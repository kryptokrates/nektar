package wallet

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"
	"github.com/bsv-blockchain/go-sdk/transaction/template/p2pkh"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	dsecp "github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/faststore"
)

const (
	fastLaneShardCount = 64
	fastLaneDustLimit  = 100 // matches DefaultNetworkFees.Dust
)

var (
	// ErrFastLaneUnavailable signals that FastLanePool has no eligible UTXO and caller should fall back to go-wallet-toolbox.
	ErrFastLaneUnavailable = errors.New("wallet: fast-lane pool has no eligible UTXO")
	errFastLaneUnavailable = ErrFastLaneUnavailable
)

// FastLaneUTXO represents a spendable P2PKH UTXO controlled by the embedded wallet's
// BRC-43 funding key (`e.fundKey`), carried with its parent Transaction/BUMP for SPV AtomicBEEF.
type FastLaneUTXO struct {
	TxID          string                   `json:"txid"`
	Vout          uint32                   `json:"vout"`
	Satoshis      uint64                   `json:"sats"`
	UnprovenDepth int                      `json:"depth"`
	SourceTx      *transaction.Transaction `json:"-"`
}

type fastLaneDiskUTXO struct {
	TxID          string `json:"txid"`
	Vout          uint32 `json:"vout"`
	Satoshis      uint64 `json:"sats"`
	UnprovenDepth int    `json:"depth"`
	BEEF          []byte `json:"beef,omitempty"`
}

type fastLaneIncomingRecord struct {
	TxID      string `json:"txid"`
	Vout      uint32 `json:"vout"`
	Satoshis  uint64 `json:"sats"`
	Prefix    string `json:"prefix"`
	Suffix    string `json:"suffix"`
	SenderPub string `json:"senderPub"`
	TS        int64  `json:"ts"`
}

type fastLaneShard struct {
	mu    sync.Mutex
	utxos []FastLaneUTXO
}

// FastLanePool is a 64-shard, zero-GORM, pure-Go BRC-29 payment engine backed by Pebble WAL group-commit.
// Raw private keys remain strictly sealed inside internal/wallet.
type FastLanePool struct {
	e          *Engine
	fs         *faststore.DB
	ownsFS     bool
	rootFast   *dsecp.PrivateKey
	fundFast   *dsecp.PrivateKey
	fundPub33  [33]byte
	fundScript []byte
	idPubHex   string
	feePerKB   uint64
	maxDepth   int
	mainnet    bool

	shards     [fastLaneShardCount]fastLaneShard
	rr         atomic.Uint64
	totalCount atomic.Int64
	totalSats  atomic.Uint64

	prepMu   sync.Mutex
	prepared map[string]*preparedFastTx

	closeMu sync.Mutex
	closed  bool
}

type preparedFastTx struct {
	txid      string
	tx        *transaction.Transaction
	rawBEEF   []byte
	spentUTXO FastLaneUTXO
	shardIdx  int
	hasChange bool
	nextUTXO  FastLaneUTXO
	feeSats   uint64
	spendSats uint64
}

func newFastLanePool(e *Engine, fs *faststore.DB, ownsFS bool) *FastLanePool {
	if e == nil || fs == nil || e.rootKey == nil || e.fundKey == nil {
		return nil
	}
	rootFast := makeFastPriv(e.rootKey)
	fundFast := makeFastPriv(e.fundKey)
	var pub33 [33]byte
	copy(pub33[:], fundFast.PubKey().SerializeCompressed())
	lockScript := core.P2PKHLockScript(core.Hash160(pub33[:]))
	feePerKB := uint64(100)
	if e.cfg.FeeSource != nil {
		fCtx, cancel := context.WithTimeout(context.Background(), time.Second)
		if f, err := e.cfg.FeeSource.Fees(fCtx); err == nil && !f.Rate.IsZero() && f.Rate.Bytes > 0 {
			feePerKB = (f.Rate.Sats*1000 + f.Rate.Bytes - 1) / f.Rate.Bytes
		}
		cancel()
	}
	fl := &FastLanePool{
		e:          e,
		fs:         fs,
		ownsFS:     ownsFS,
		rootFast:   rootFast,
		fundFast:   fundFast,
		fundPub33:  pub33,
		fundScript: lockScript,
		idPubHex:   e.IdentityPubHex(),
		feePerKB:   feePerKB,
		maxDepth:   e.cfg.MaxUnprovenDepth,
		mainnet:    e.cfg.Mainnet,
		prepared:   make(map[string]*preparedFastTx, 256),
	}
	fl.loadFromWAL()
	return fl
}

func (fl *FastLanePool) loadFromWAL() {
	if fl == nil || fl.fs == nil {
		return
	}
	var idx uint64
	_ = fl.fs.ScanPrefix([]byte("fl:utxo:"), func(_, val []byte) bool {
		var rec fastLaneDiskUTXO
		if json.Unmarshal(val, &rec) != nil || rec.TxID == "" || rec.Satoshis == 0 {
			return true
		}
		var srcTx *transaction.Transaction
		if len(rec.BEEF) > 0 {
			if _, parsed, _, err := guard.ParseBEEF(rec.BEEF, guard.DefaultBound); err == nil && parsed != nil {
				srcTx = parsed
			}
		}
		if srcTx == nil {
			if raw, err := fl.fs.Get([]byte("fl:tx:" + rec.TxID)); err == nil && len(raw) > 0 {
				if _, parsed, _, err := guard.ParseBEEF(raw, guard.DefaultBound); err == nil && parsed != nil {
					srcTx = parsed
				}
			}
		}
		if srcTx == nil {
			return true
		}
		sh := &fl.shards[idx&(fastLaneShardCount-1)]
		idx++
		sh.mu.Lock()
		sh.utxos = append(sh.utxos, FastLaneUTXO{
			TxID:          rec.TxID,
			Vout:          rec.Vout,
			Satoshis:      rec.Satoshis,
			UnprovenDepth: rec.UnprovenDepth,
			SourceTx:      srcTx,
		})
		sh.mu.Unlock()
		fl.totalCount.Add(1)
		fl.totalSats.Add(rec.Satoshis)
		return true
	})
}

// Close zeroes cached secp256k1 private scalars and closes the underlying Pebble store if owned.
func (fl *FastLanePool) Close() error {
	if fl == nil {
		return nil
	}
	fl.closeMu.Lock()
	defer fl.closeMu.Unlock()
	if fl.closed {
		return nil
	}
	fl.closed = true
	if fl.rootFast != nil {
		fl.rootFast.Zero()
	}
	if fl.fundFast != nil {
		fl.fundFast.Zero()
	}
	if fl.ownsFS && fl.fs != nil {
		return fl.fs.Close()
	}
	return nil
}

// AvailableUTXOs returns the number of spendable UTXOs currently loaded in the 64-shard pool.
func (fl *FastLanePool) AvailableUTXOs() int {
	if fl == nil {
		return 0
	}
	return int(fl.totalCount.Load())
}

// AvailableSats returns total satoshis currently loaded in the 64-shard pool.
func (fl *FastLanePool) AvailableSats() uint64 {
	if fl == nil {
		return 0
	}
	return fl.totalSats.Load()
}

// SeedUTXOs durably persists utxos into Pebble WAL and distributes them evenly across all 64 shards.
func (fl *FastLanePool) SeedUTXOs(utxos []FastLaneUTXO) error {
	if fl == nil || len(utxos) == 0 {
		return nil
	}
	batch := fl.fs.NewBatch()
	seenTx := make(map[string]bool)
	for _, u := range utxos {
		if u.TxID == "" || u.Satoshis == 0 || u.SourceTx == nil {
			continue
		}
		if !seenTx[u.TxID] {
			if raw, err := SafeAtomicBEEF(u.SourceTx); err == nil && len(raw) > 0 {
				_ = batch.Set([]byte("fl:tx:"+u.TxID), raw, nil)
				if fl.e != nil && fl.e.txCache != nil {
					fl.e.txCache.Put(u.TxID, u.SourceTx)
				}
			}
			seenTx[u.TxID] = true
		}
		recBytes, _ := json.Marshal(fastLaneDiskUTXO{
			TxID:          u.TxID,
			Vout:          u.Vout,
			Satoshis:      u.Satoshis,
			UnprovenDepth: u.UnprovenDepth,
		})
		_ = batch.Set(fastLaneUTXOKey(u.TxID, u.Vout), recBytes, nil)
	}
	if err := fl.fs.CommitBatch(batch); err != nil {
		return fmt.Errorf("wallet: commit seeded fast-lane UTXOs: %w", err)
	}
	for _, u := range utxos {
		if u.TxID == "" || u.Satoshis == 0 || u.SourceTx == nil {
			continue
		}
		slot := fl.rr.Add(1) & (fastLaneShardCount - 1)
		sh := &fl.shards[slot]
		sh.mu.Lock()
		sh.utxos = append(sh.utxos, u)
		sh.mu.Unlock()
		fl.totalCount.Add(1)
		fl.totalSats.Add(u.Satoshis)
	}
	return nil
}

// ProvisionFastLane splits available wallet funds into numLanes independent P2PKH UTXOs paying
// the wallet's BRC-43 funding script in ONE transaction and loads them into the 64-shard FastLanePool.
func (e *Engine) ProvisionFastLane(ctx context.Context, numLanes int, satsPerLane uint64) (int, error) {
	if e == nil || e.fastLane == nil {
		return 0, errors.New("wallet: fast-lane pool not enabled")
	}
	if numLanes <= 0 {
		numLanes = 64
	}
	if numLanes > 512 {
		numLanes = 512
	}
	if satsPerLane < 1000 {
		satsPerLane = 10_000
	}
	outs := make([]sdkwallet.CreateActionOutput, numLanes)
	for i := 0; i < numLanes; i++ {
		outs[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*e.fundScript)...),
			Satoshis:          satsPerLane,
			OutputDescription: "fastlane fuel UTXO",
		}
	}
	randomize := false
	acceptDelayed := false
	caRes, err := e.createActionGuarded(ctx, sdkwallet.CreateActionArgs{
		Description: fmt.Sprintf("nektar fastlane fanout x%d", numLanes),
		Labels:      []string{"fastlane", "fuel"},
		Outputs:     outs,
		Options: &sdkwallet.CreateActionOptions{
			RandomizeOutputs:       &randomize,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, e.cfg.Originator, true, true)
	if err != nil {
		return 0, err
	}
	var parsed *transaction.Transaction
	if len(caRes.Tx) > 0 {
		_, parsed, _, _ = guard.ParseBEEF(caRes.Tx, guard.DefaultBound)
	}
	if parsed == nil {
		parsed, _ = e.LoadParsedTx(ctx, caRes.Txid.String())
	}
	if parsed == nil {
		return 0, errors.New("wallet: fast-lane fanout returned unparseable tx")
	}
	depth := 1
	if parsed.MerklePath != nil {
		depth = 0
	}
	txid := parsed.TxID().String()
	utxos := make([]FastLaneUTXO, 0, numLanes)
	for vout, o := range parsed.Outputs {
		if o.LockingScript != nil && string(*o.LockingScript) == string(*e.fundScript) && o.Satoshis == satsPerLane {
			utxos = append(utxos, FastLaneUTXO{
				TxID:          txid,
				Vout:          uint32(vout),
				Satoshis:      o.Satoshis,
				UnprovenDepth: depth,
				SourceTx:      parsed,
			})
		}
	}
	if err := e.fastLane.SeedUTXOs(utxos); err != nil {
		return 0, err
	}
	return len(utxos), nil
}

// SeedFastLaneUTXOs directly seeds spendable UTXOs into the Engine's 64-shard FastLanePool.
func (e *Engine) SeedFastLaneUTXOs(utxos []FastLaneUTXO) error {
	if e == nil || e.fastLane == nil {
		return errors.New("wallet: fast-lane pool not enabled")
	}
	return e.fastLane.SeedUTXOs(utxos)
}

// FastLane returns the Engine's Pebble-backed FastLanePool if enabled.
func (e *Engine) FastLane() *FastLanePool {
	if e == nil {
		return nil
	}
	return e.fastLane
}

func (fl *FastLanePool) popEligible(minSats uint64) (FastLaneUTXO, int, bool) {
	start := fl.rr.Add(1) & (fastLaneShardCount - 1)
	maxD := fl.maxDepth
	for step := uint64(0); step < fastLaneShardCount; step++ {
		idx := int((start + step) & (fastLaneShardCount - 1))
		sh := &fl.shards[idx]
		sh.mu.Lock()
		for i, u := range sh.utxos {
			if u.Satoshis >= minSats && (maxD <= 0 || u.UnprovenDepth < maxD) {
				// Pop FIFO (keep shallowest-depth UTXOs rotating evenly across the lane).
				copy(sh.utxos[i:], sh.utxos[i+1:])
				sh.utxos = sh.utxos[:len(sh.utxos)-1]
				sh.mu.Unlock()
				fl.totalCount.Add(-1)
				fl.totalSats.Add(^u.Satoshis + 1)
				return u, idx, true
			}
		}
		sh.mu.Unlock()
	}
	return FastLaneUTXO{}, -1, false
}

func (fl *FastLanePool) pushBack(shardIdx int, u FastLaneUTXO) {
	sh := &fl.shards[shardIdx]
	sh.mu.Lock()
	sh.utxos = append(sh.utxos, u)
	sh.mu.Unlock()
	fl.totalCount.Add(1)
	fl.totalSats.Add(u.Satoshis)
}

// SendFast executes a standard single-output BRC-29 or P2PKH payment with zero GORM/SQLite locking:
// ~35us BRC-29 ECDH + ~25us pure-Go secp256k1/v4 signing + 1 group-committed Pebble WAL batch before broadcast.
func (fl *FastLanePool) SendFast(ctx context.Context, args SendArgs) (*SendResult, error) {
	if fl == nil || args.NoSend || args.Satoshis == 0 {
		return nil, errFastLaneUnavailable
	}
	if fl.totalCount.Load() <= 0 {
		return nil, errFastLaneUnavailable
	}
	if !fl.e.svc.hasBroadcaster() {
		return nil, ErrNoSettler
	}

	// 1. Derive locking script (~35us for BRC-29 via secp256k1/v4).
	var lockBytes []byte
	var prefixB64, suffixB64, senderHex string
	if args.ToAddress != "" {
		addr, err := script.NewAddressFromString(strings.TrimSpace(args.ToAddress))
		if err != nil {
			return nil, fmt.Errorf("%w: invalid address %q: %v", ErrInvalidArgs, args.ToAddress, err)
		}
		ls, err := p2pkh.Lock(addr)
		if err != nil {
			return nil, err
		}
		lockBytes = *ls
	} else {
		prefixB64 = strings.TrimSpace(args.DerivationPrefix)
		suffixB64 = strings.TrimSpace(args.DerivationSuffix)
		if prefixB64 == "" {
			var preRaw [16]byte
			_, _ = rand.Read(preRaw[:])
			prefixB64 = base64.StdEncoding.EncodeToString(preRaw[:])
		}
		if suffixB64 == "" {
			var sufRaw [16]byte
			_, _ = rand.Read(sufRaw[:])
			suffixB64 = base64.StdEncoding.EncodeToString(sufRaw[:])
		}
		senderHex = fl.idPubHex
		var err error
		lockBytes, err = core.FastDeriveBRC29LockingScript(fl.rootFast, args.ToIdentityKey, prefixB64, suffixB64)
		if err != nil {
			return nil, fmt.Errorf("%w: derive BRC-29 lock script: %v", ErrInvalidArgs, err)
		}
	}

	// Estimate 1-in 2-out P2PKH tx size (~226 bytes -> ~23 sats at 100 sat/kB).
	estFee := (230*fl.feePerKB + 999) / 1000
	if estFee < 1 {
		estFee = 1
	}

	// Read-only SpendGuard pre-check before reserving a UTXO.
	if !args.OverrideGuard && fl.e.cfg.SpendGuardCheck != nil {
		if err := fl.e.cfg.SpendGuardCheck(estFee, args.Satoshis+estFee); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}

	// 2. Pop 1 spendable UTXO from sharded pool (zero contention across 64 lanes).
	utxo, shardIdx, ok := fl.popEligible(args.Satoshis + estFee)
	if !ok {
		return nil, errFastLaneUnavailable
	}

	// 3. Build standard BSV 1-in, 2-out (payment + change) transaction.
	srcHash, err := chainhash.NewHashFromHex(utxo.TxID)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, err
	}
	tx := transaction.NewTransaction()
	tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
		SourceTXID:        srcHash,
		SourceTxOutIndex:  utxo.Vout,
		SourceTransaction: utxo.SourceTx,
		SequenceNumber:    transaction.DefaultSequenceNumber,
	})
	payScript := script.Script(lockBytes)
	tx.AddOutput(&transaction.TransactionOutput{
		Satoshis:      args.Satoshis,
		LockingScript: &payScript,
	})

	feeSats := estFee
	hasChange := false
	var changeSats uint64
	if utxo.Satoshis >= args.Satoshis+feeSats+fastLaneDustLimit {
		changeSats = utxo.Satoshis - args.Satoshis - feeSats
		chgScript := script.Script(append([]byte(nil), fl.fundScript...))
		tx.AddOutput(&transaction.TransactionOutput{
			Satoshis:      changeSats,
			LockingScript: &chgScript,
		})
		hasChange = true
	} else {
		feeSats = utxo.Satoshis - args.Satoshis
	}

	// 4. Sign input 0 with BIP-143 FORKID + pure-Go secp256k1/v4 (~25us).
	sigHash, err := tx.CalcInputSignatureHash(0, sighash.AllForkID)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane sighash: %w", err)
	}
	derSig, err := core.FastSignWithPriv(fl.fundFast, sigHash)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane sign: %w", err)
	}
	sigWithFlag := make([]byte, len(derSig)+1)
	copy(sigWithFlag, derSig)
	sigWithFlag[len(derSig)] = byte(sighash.AllForkID)

	unlockBuf := make([]byte, 0, 1+len(sigWithFlag)+1+33)
	unlockBuf = append(unlockBuf, byte(len(sigWithFlag)))
	unlockBuf = append(unlockBuf, sigWithFlag...)
	unlockBuf = append(unlockBuf, 33)
	unlockBuf = append(unlockBuf, fl.fundPub33[:]...)
	unlockScript := script.Script(unlockBuf)
	tx.Inputs[0].UnlockingScript = &unlockScript

	txid := tx.TxID().String()
	rawBEEF, err := tx.AtomicBEEF(false)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane AtomicBEEF: %w", err)
	}

	// 5. Enforce SpendGuard synchronously BEFORE committing/broadcasting!
	if !args.OverrideGuard && fl.e.cfg.SpendGuard != nil {
		if gErr := fl.e.cfg.SpendGuard(txid, feeSats, args.Satoshis+feeSats, false); gErr != nil {
			fl.pushBack(shardIdx, utxo)
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, gErr)
		}
	}

	// 6. Commit spent UTXO deletion + change UTXO + raw AtomicBEEF in ONE Pebble group-committed WAL batch BEFORE broadcast!
	batch := fl.fs.NewBatch()
	_ = batch.Delete(fastLaneUTXOKey(utxo.TxID, utxo.Vout), nil)
	_ = batch.Set([]byte("fl:tx:"+txid), rawBEEF, nil)
	var nextUTXO FastLaneUTXO
	if hasChange {
		nextUTXO = FastLaneUTXO{
			TxID:          txid,
			Vout:          1,
			Satoshis:      changeSats,
			UnprovenDepth: utxo.UnprovenDepth + 1,
			SourceTx:      tx,
		}
		chgRec, _ := json.Marshal(fastLaneDiskUTXO{
			TxID:          txid,
			Vout:          1,
			Satoshis:      changeSats,
			UnprovenDepth: nextUTXO.UnprovenDepth,
		})
		_ = batch.Set(fastLaneUTXOKey(txid, 1), chgRec, nil)
	}
	if err := fl.fs.CommitBatch(batch); err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane WAL commit: %w", err)
	}

	if hasChange {
		fl.pushBack(shardIdx, nextUTXO)
	}
	if fl.e.txCache != nil {
		fl.e.txCache.Put(txid, tx)
	}

	// 7. Broadcast transaction (async pool when WithAsyncBroadcast(ctx) + Broadcaster, else direct).
	if err := fl.broadcastTx(ctx, tx, txid); err != nil {
		return nil, err
	}

	return &SendResult{
		TxID:              txid,
		Satoshis:          args.Satoshis,
		FeeSats:           feeSats,
		InputCount:        1,
		BEEF:              rawBEEF,
		BEEFHex:           hex.EncodeToString(rawBEEF),
		Tx:                tx,
		DerivationPrefix:  prefixB64,
		DerivationSuffix:  suffixB64,
		SenderIdentityKey: senderHex,
		OutputIndex:       0,
	}, nil
}

// PrepareFastBRC29 builds and signs a BRC-29 payment tx from the 64-shard FastLanePool WITHOUT
// broadcasting yet, so durable callers (BuyerBroadcastPayment) can persist txid + BEEF + nonces
// atomically BEFORE BroadcastPrepared() runs SpendGuard + WAL commit + broadcast.
func (fl *FastLanePool) PrepareFastBRC29(ctx context.Context, toIdentityKey string, sats uint64) (*SendResult, error) {
	if fl == nil || sats == 0 {
		return nil, ErrFastLaneUnavailable
	}
	if fl.totalCount.Load() <= 0 {
		return nil, ErrFastLaneUnavailable
	}
	if !fl.e.svc.hasBroadcaster() {
		return nil, ErrNoSettler
	}
	var preRaw, sufRaw [16]byte
	_, _ = rand.Read(preRaw[:])
	_, _ = rand.Read(sufRaw[:])
	prefixB64 := base64.StdEncoding.EncodeToString(preRaw[:])
	suffixB64 := base64.StdEncoding.EncodeToString(sufRaw[:])
	lockBytes, err := core.FastDeriveBRC29LockingScript(fl.rootFast, toIdentityKey, prefixB64, suffixB64)
	if err != nil {
		return nil, fmt.Errorf("%w: derive BRC-29 lock script: %v", ErrInvalidArgs, err)
	}
	estFee := (230*fl.feePerKB + 999) / 1000
	if estFee < 1 {
		estFee = 1
	}
	if fl.e.cfg.SpendGuardCheck != nil {
		if err := fl.e.cfg.SpendGuardCheck(estFee, sats+estFee); err != nil {
			return nil, fmt.Errorf("%w: %w", ErrSpendRejected, err)
		}
	}
	utxo, shardIdx, ok := fl.popEligible(sats + estFee)
	if !ok {
		return nil, ErrFastLaneUnavailable
	}
	srcHash, err := chainhash.NewHashFromHex(utxo.TxID)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, err
	}
	tx := transaction.NewTransaction()
	tx.Inputs = append(tx.Inputs, &transaction.TransactionInput{
		SourceTXID:        srcHash,
		SourceTxOutIndex:  utxo.Vout,
		SourceTransaction: utxo.SourceTx,
		SequenceNumber:    transaction.DefaultSequenceNumber,
	})
	payScript := script.Script(lockBytes)
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: sats, LockingScript: &payScript})

	feeSats := estFee
	hasChange := false
	var changeSats uint64
	if utxo.Satoshis >= sats+feeSats+fastLaneDustLimit {
		changeSats = utxo.Satoshis - sats - feeSats
		chgScript := script.Script(append([]byte(nil), fl.fundScript...))
		tx.AddOutput(&transaction.TransactionOutput{Satoshis: changeSats, LockingScript: &chgScript})
		hasChange = true
	} else {
		feeSats = utxo.Satoshis - sats
	}

	sigHash, err := tx.CalcInputSignatureHash(0, sighash.AllForkID)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane sighash: %w", err)
	}
	derSig, err := core.FastSignWithPriv(fl.fundFast, sigHash)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane sign: %w", err)
	}
	sigWithFlag := make([]byte, len(derSig)+1)
	copy(sigWithFlag, derSig)
	sigWithFlag[len(derSig)] = byte(sighash.AllForkID)

	unlockBuf := make([]byte, 0, 1+len(sigWithFlag)+1+33)
	unlockBuf = append(unlockBuf, byte(len(sigWithFlag)))
	unlockBuf = append(unlockBuf, sigWithFlag...)
	unlockBuf = append(unlockBuf, 33)
	unlockBuf = append(unlockBuf, fl.fundPub33[:]...)
	unlockScript := script.Script(unlockBuf)
	tx.Inputs[0].UnlockingScript = &unlockScript

	txid := tx.TxID().String()
	rawBEEF, err := tx.AtomicBEEF(false)
	if err != nil {
		fl.pushBack(shardIdx, utxo)
		return nil, fmt.Errorf("wallet: fast-lane AtomicBEEF: %w", err)
	}
	var nextUTXO FastLaneUTXO
	if hasChange {
		nextUTXO = FastLaneUTXO{
			TxID:          txid,
			Vout:          1,
			Satoshis:      changeSats,
			UnprovenDepth: utxo.UnprovenDepth + 1,
			SourceTx:      tx,
		}
	}
	fl.prepMu.Lock()
	fl.prepared[txid] = &preparedFastTx{
		txid:      txid,
		tx:        tx,
		rawBEEF:   rawBEEF,
		spentUTXO: utxo,
		shardIdx:  shardIdx,
		hasChange: hasChange,
		nextUTXO:  nextUTXO,
		feeSats:   feeSats,
		spendSats: sats + feeSats,
	}
	fl.prepMu.Unlock()

	return &SendResult{
		TxID:              txid,
		Satoshis:          sats,
		FeeSats:           feeSats,
		InputCount:        1,
		BEEF:              rawBEEF,
		BEEFHex:           hex.EncodeToString(rawBEEF),
		Tx:                tx,
		DerivationPrefix:  prefixB64,
		DerivationSuffix:  suffixB64,
		SenderIdentityKey: fl.idPubHex,
		OutputIndex:       0,
	}, nil
}

// HasPrepared reports whether txid is awaiting BroadcastPrepared.
func (fl *FastLanePool) HasPrepared(txid string) bool {
	if fl == nil || txid == "" {
		return false
	}
	fl.prepMu.Lock()
	_, ok := fl.prepared[txid]
	fl.prepMu.Unlock()
	return ok
}

// BroadcastPrepared enforces SpendGuard, commits UTXO consumption + change + BEEF to Pebble WAL,
// returns the change UTXO to its shard, and broadcasts the signed tx (zero GORM/SQLite).
func (fl *FastLanePool) BroadcastPrepared(ctx context.Context, txid string) error {
	if fl == nil {
		return errors.New("wallet: nil fast-lane pool")
	}
	fl.prepMu.Lock()
	p, ok := fl.prepared[txid]
	if ok {
		delete(fl.prepared, txid)
	}
	fl.prepMu.Unlock()
	if !ok || p == nil {
		return fmt.Errorf("wallet: prepared fast-lane tx %s not found", txid)
	}
	if fl.e.cfg.SpendGuard != nil && !isOverrideGuard(ctx) {
		if gErr := fl.e.cfg.SpendGuard(txid, p.feeSats, p.spendSats, false); gErr != nil {
			fl.pushBack(p.shardIdx, p.spentUTXO)
			return fmt.Errorf("%w: %w", ErrSpendRejected, gErr)
		}
	}
	batch := fl.fs.NewBatch()
	_ = batch.Delete(fastLaneUTXOKey(p.spentUTXO.TxID, p.spentUTXO.Vout), nil)
	_ = batch.Set([]byte("fl:tx:"+txid), p.rawBEEF, nil)
	if p.hasChange {
		chgRec, _ := json.Marshal(fastLaneDiskUTXO{
			TxID:          txid,
			Vout:          1,
			Satoshis:      p.nextUTXO.Satoshis,
			UnprovenDepth: p.nextUTXO.UnprovenDepth,
		})
		_ = batch.Set(fastLaneUTXOKey(txid, 1), chgRec, nil)
	}
	if err := fl.fs.CommitBatch(batch); err != nil {
		fl.pushBack(p.shardIdx, p.spentUTXO)
		return fmt.Errorf("wallet: fast-lane WAL commit: %w", err)
	}
	if p.hasChange {
		fl.pushBack(p.shardIdx, p.nextUTXO)
	}
	if fl.e.txCache != nil {
		fl.e.txCache.Put(txid, p.tx)
	}
	return fl.broadcastTx(ctx, p.tx, txid)
}

// AbortPrepared restores a prepared fast-lane UTXO back into its shard when pre-broadcast persistence fails.
func (fl *FastLanePool) AbortPrepared(txid string) {
	if fl == nil || txid == "" {
		return
	}
	fl.prepMu.Lock()
	p, ok := fl.prepared[txid]
	if ok {
		delete(fl.prepared, txid)
	}
	fl.prepMu.Unlock()
	if ok && p != nil {
		fl.pushBack(p.shardIdx, p.spentUTXO)
	}
}

func (fl *FastLanePool) broadcastTx(ctx context.Context, tx *transaction.Transaction, txid string) error {
	svc := fl.e.svc
	if isAsyncBroadcast(ctx) && svc.cfg.Broadcast == nil && svc.cfg.Broadcaster != nil {
		txCopy := tx
		go func() {
			outgoingBroadcastSem <- struct{}{}
			defer func() { <-outgoingBroadcastSem }()
			bCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			if bErr := svc.cfg.Broadcaster.Broadcast(bCtx, txCopy); bErr != nil && !IsAlreadyKnownBroadcastError(bErr) {
				svc.setLastErr(txid, bErr)
			}
		}()
		return nil
	}
	var bErr error
	if svc.cfg.Broadcast != nil {
		bErr = svc.cfg.Broadcast(ctx, tx)
	} else {
		bErr = svc.cfg.Broadcaster.Broadcast(ctx, tx)
	}
	if bErr != nil && !IsAlreadyKnownBroadcastError(bErr) {
		if IsUncertainBroadcastError(bErr) {
			return fmt.Errorf("%w: %w", ErrBroadcastUnknown, bErr)
		}
		return bErr
	}
	return nil
}

// RecordIncomingBRC29 durably commits an already-SPV-verified Seller incoming BRC-29 payment
// into Pebble WAL in one group-committed batch (~50us, zero GORM/SQLite write lock).
func (fl *FastLanePool) RecordIncomingBRC29(tx *transaction.Transaction, vout uint32, prefix, suffix, senderPub string, rawBEEF []byte) error {
	if fl == nil || tx == nil {
		return nil
	}
	txid := tx.TxID().String()
	var sats uint64
	if int(vout) < len(tx.Outputs) {
		sats = tx.Outputs[vout].Satoshis
	}
	recBytes, _ := json.Marshal(fastLaneIncomingRecord{
		TxID:      txid,
		Vout:      vout,
		Satoshis:  sats,
		Prefix:    prefix,
		Suffix:    suffix,
		SenderPub: senderPub,
		TS:        time.Now().Unix(),
	})
	batch := fl.fs.NewBatch()
	_ = batch.Set(fastLaneIncomingKey(txid, vout), recBytes, nil)
	if len(rawBEEF) > 0 {
		_ = batch.Set([]byte("fl:tx:"+txid), rawBEEF, nil)
	}
	if err := fl.fs.CommitBatch(batch); err != nil {
		return err
	}
	if fl.e != nil && fl.e.txCache != nil {
		fl.e.txCache.Put(txid, tx)
	}
	return nil
}

// LoadTx recovers a signed transaction from Pebble WAL if not in txCache.
func (fl *FastLanePool) LoadTx(txid string) (*transaction.Transaction, bool) {
	if fl == nil || fl.fs == nil || txid == "" {
		return nil, false
	}
	raw, err := fl.fs.Get([]byte("fl:tx:" + txid))
	if err != nil || len(raw) == 0 {
		return nil, false
	}
	_, parsed, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil || parsed == nil {
		return nil, false
	}
	return parsed, true
}

// DeriveBRC29ReceiveScriptFast computes the Seller's expected 25-byte BRC-29 P2PKH locking script
// for an incoming payment from senderPubHex using cached ECDH + secp256k1/v4 (~12us, zero math/big!).
func (fl *FastLanePool) DeriveBRC29ReceiveScriptFast(senderPubHex, prefix, suffix string) ([]byte, error) {
	if fl == nil || fl.rootFast == nil {
		return nil, errors.New("wallet: fast-lane pool not initialized")
	}
	return core.FastDeriveBRC29LockingScriptForRecipient(fl.rootFast, senderPubHex, prefix, suffix)
}

func fastLaneUTXOKey(txid string, vout uint32) []byte {
	key := make([]byte, 0, 8+len(txid)+5)
	key = append(key, "fl:utxo:"...)
	key = append(key, txid...)
	key = append(key, ':')
	var vbuf [4]byte
	binary.BigEndian.PutUint32(vbuf[:], vout)
	return append(key, vbuf[:]...)
}

func fastLaneIncomingKey(txid string, vout uint32) []byte {
	key := make([]byte, 0, 6+len(txid)+5)
	key = append(key, "fl:in:"...)
	key = append(key, txid...)
	key = append(key, ':')
	var vbuf [4]byte
	binary.BigEndian.PutUint32(vbuf[:], vout)
	return append(key, vbuf[:]...)
}
