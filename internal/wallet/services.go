package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
)

// bcommonServices adapts Nektar's lightwebinc/bcommon chain stack (Headers/Testchain, Arcade,
// Settler, Chain/WoC) to go-wallet-toolbox's wdk.Services interface. Implementing wdk.Services
// directly lets Nektar use go-wallet-toolbox/pkg/wallet + pkg/storage WITHOUT importing
// go-wallet-toolbox/pkg/services (eliminating all Kubernetes, Teranode, libp2p, and AWS deps).
type bcommonServices struct {
	cfg            Config
	txCache        *txLRU
	ownedOutputSum func(txid string) (uint64, bool)

	errMu        sync.Mutex
	lastErr      map[string]error
	plannedSpend map[string]uint64
}

var _ wdk.Services = (*bcommonServices)(nil)

func newBcommonServices(cfg Config, cache *txLRU) *bcommonServices {
	return &bcommonServices{
		cfg:          cfg,
		txCache:      cache,
		lastErr:      make(map[string]error),
		plannedSpend: make(map[string]uint64),
	}
}

func (s *bcommonServices) setPlannedSpend(txid string, externalSats uint64) {
	if txid == "" {
		return
	}
	s.errMu.Lock()
	if len(s.plannedSpend) > 512 {
		clear(s.plannedSpend)
	}
	s.plannedSpend[txid] = externalSats
	s.errMu.Unlock()
}

func (s *bcommonServices) lookupPlannedSpend(txid string) (uint64, bool) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	v, ok := s.plannedSpend[txid]
	return v, ok
}

func (s *bcommonServices) setLastErr(txid string, err error) {
	if txid == "" || err == nil {
		return
	}
	s.errMu.Lock()
	if len(s.lastErr) > 256 {
		clear(s.lastErr)
	}
	s.lastErr[txid] = err
	s.errMu.Unlock()
}

func (s *bcommonServices) takeLastErr(txid string) error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	err := s.lastErr[txid]
	delete(s.lastErr, txid)
	return err
}

func (s *bcommonServices) hasBroadcaster() bool {
	return s.cfg.Broadcast != nil || s.cfg.Settler != nil
}

func (s *bcommonServices) CurrentHeight(ctx context.Context) (uint32, error) {
	if s.cfg.Tracker == nil {
		if !s.cfg.InMemoryStore {
			return 0, ErrNoChain
		}
		return 1, nil
	}
	h, err := s.cfg.Tracker.CurrentHeight(ctx)
	if err != nil {
		return 0, err
	}
	if h == 0 {
		h = 1
	}
	return h, nil
}

func (s *bcommonServices) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if s.cfg.Tracker == nil {
		return false, ErrNoChain
	}
	return s.cfg.Tracker.IsValidRootForHeight(ctx, root, height)
}

func (s *bcommonServices) ChainHeaderByHeight(ctx context.Context, height uint32) (*wdk.ChainBlockHeader, error) {
	if s.cfg.HeaderBytes != nil {
		raw, err := s.cfg.HeaderBytes(ctx, height)
		if err != nil {
			if !s.cfg.InMemoryStore {
				return nil, fmt.Errorf("wallet services: fetch header %d: %w", height, err)
			}
		} else if len(raw) == wdk.BlockHeaderLength {
			base, pErr := wdk.ChainBaseBlockHeaderFromBytes(raw)
			if pErr != nil {
				if !s.cfg.InMemoryStore {
					return nil, fmt.Errorf("wallet services: parse header %d: %w", height, pErr)
				}
			} else {
				hash, hErr := base.CalculateHash()
				if hErr == nil {
					return &wdk.ChainBlockHeader{
						ChainBaseBlockHeader: *base,
						Height:               uint(height),
						Hash:                 hash.String(),
					}, nil
				}
			}
		} else if !s.cfg.InMemoryStore {
			return nil, fmt.Errorf("wallet services: header %d invalid length %d", height, len(raw))
		}
	}
	if s.cfg.InMemoryStore {
		// Deterministic synthetic header fallback strictly for ephemeral in-memory testchain.
		return syntheticChainBlockHeader(height), nil
	}
	return nil, fmt.Errorf("%w: no raw block header source configured for height %d", ErrNoChain, height)
}

func (s *bcommonServices) FindChainTipHeader(ctx context.Context) (*wdk.ChainBlockHeader, error) {
	h, err := s.CurrentHeight(ctx)
	if err != nil {
		return nil, err
	}
	return s.ChainHeaderByHeight(ctx, h)
}

func (s *bcommonServices) PostFromBEEF(ctx context.Context, beef *transaction.Beef, txids []string) (wdk.PostFromBeefResult, error) {
	if !s.hasBroadcaster() {
		return nil, ErrNoSettler
	}
	name := "bcommon"
	if s.cfg.Settler != nil {
		name = s.cfg.Settler.Name()
	} else if s.cfg.Broadcast != nil {
		name = "testchain"
	}

	posted := &wdk.PostedBEEF{
		TxIDResults: make([]wdk.PostedTxID, 0, len(txids)),
	}
	var firstErr error
	for _, id := range txids {
		tx := findAndHydrateTx(beef, id, s.txCache)
		if tx == nil {
			err := fmt.Errorf("wallet services: tx %s missing from BEEF", id)
			if firstErr == nil {
				firstErr = err
			}
			posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
				TxID:   id,
				Result: wdk.PostedTxIDResultError,
				Error:  err,
			})
			continue
		}

		// Single post-signing SpendGuard chokepoint: enforce exact actual fee + external net spend
		// across EVERY broadcast path (CreateAction, SignAction, SendNoSendTx, FanOutFuel, Sweep, Consolidate).
		guarded := false
		if s.cfg.SpendGuard != nil && !isOverrideGuard(ctx) {
			feeSats := computeTxFee(tx)
			var outSum uint64
			for _, o := range tx.Outputs {
				outSum += o.Satoshis
			}
			var externalOut uint64
			hint := externalSpendFromCtx(ctx)
			switch {
			case hint.set && hint.sweepAll:
				externalOut = outSum
			case hint.set:
				externalOut = hint.sats
			default:
				if ownedSum, ok := func() (uint64, bool) {
					if s.ownedOutputSum != nil {
						return s.ownedOutputSum(id)
					}
					return 0, false
				}(); ok {
					if outSum > ownedSum {
						externalOut = outSum - ownedSum
					}
				} else if planned, ok := s.lookupPlannedSpend(id); ok {
					externalOut = planned
				} else {
					// Fail-closed: unknown direct broadcast path counts full output sum.
					externalOut = outSum
				}
			}
			spendSats := externalOut + feeSats
			if gErr := s.cfg.SpendGuard(id, feeSats, spendSats, false); gErr != nil {
				rejErr := fmt.Errorf("%w: %w", ErrSpendRejected, gErr)
				s.setLastErr(id, rejErr)
				postErrSinkFromCtx(ctx).record(id, rejErr)
				return nil, rejErr
			}
			guarded = true
		}

		var bErr error
		if s.cfg.Broadcast != nil {
			bErr = s.cfg.Broadcast(ctx, tx)
		} else {
			bErr = s.cfg.Settler.Submit(ctx, tx)
		}

		if bErr != nil {
			lower := strings.ToLower(bErr.Error())
			switch {
			case strings.Contains(lower, "already in mempool") || strings.Contains(lower, "already known") || strings.Contains(lower, "txn-already-known"):
				s.txCache.Put(id, tx)
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:         id,
					Result:       wdk.PostedTxIDResultAlreadyKnown,
					AlreadyKnown: true,
				})
			case strings.Contains(lower, "utxo_spent") || strings.Contains(lower, "spent by another transaction") || strings.Contains(lower, "double_spend") || strings.Contains(lower, "double spend"):
				if guarded && s.cfg.SpendGuardRefund != nil {
					s.cfg.SpendGuardRefund(id)
				}
				s.setLastErr(id, bErr)
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:        id,
					Result:      wdk.PostedTxIDResultDoubleSpend,
					DoubleSpend: true,
					Error:       bErr,
				})
			case strings.Contains(lower, "missing inputs") || strings.Contains(lower, "missing-inputs"):
				if guarded && s.cfg.SpendGuardRefund != nil {
					s.cfg.SpendGuardRefund(id)
				}
				s.setLastErr(id, bErr)
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:   id,
					Result: wdk.PostedTxIDResultMissingInputs,
					Error:  bErr,
				})
			case isUncertainBroadcastError(bErr):
				// Transport/timeout/5xx: broadcast outcome is UNKNOWN. Do NOT refund SpendGuard and
				// return ErrBroadcastUnknown so callers preserve UTXO reservations instead of aborting!
				unkErr := fmt.Errorf("%w: %w", ErrBroadcastUnknown, bErr)
				s.setLastErr(id, unkErr)
				postErrSinkFromCtx(ctx).record(id, unkErr)
				return nil, unkErr
			default:
				if guarded && s.cfg.SpendGuardRefund != nil && isDefiniteRejectError(lower) {
					s.cfg.SpendGuardRefund(id)
				}
				s.setLastErr(id, bErr)
				if firstErr == nil {
					firstErr = bErr
				}
				posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
					TxID:   id,
					Result: wdk.PostedTxIDResultError,
					Error:  bErr,
				})
			}
			continue
		}

		// Broadcast succeeded; cache hydrated tx and attach instant proof when available (e.g. testchain).
		item := wdk.PostedTxID{
			TxID:   id,
			Result: wdk.PostedTxIDResultSuccess,
		}
		if mp, h, ok, _ := s.lookupProof(ctx, id); ok && mp != nil {
			tx.MerklePath = mp
			item.MerklePath = mp
			item.BlockHeight = h
			if hdr, hErr := s.ChainHeaderByHeight(ctx, h); hErr == nil && hdr != nil {
				item.BlockHash = hdr.Hash
			}
		}
		s.txCache.Put(id, tx)
		posted.TxIDResults = append(posted.TxIDResults, item)
	}

	return wdk.PostFromBeefResult{
		{
			Name:             name,
			PostedBEEFResult: posted,
			Error:            firstErr,
		},
	}, nil
}

func (s *bcommonServices) lookupProof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if s.cfg.ProofLookup != nil {
		return s.cfg.ProofLookup(ctx, txid)
	}
	if s.cfg.Arcade != nil {
		if mp, h, err := s.cfg.Arcade.Proof(ctx, txid); err == nil && mp != nil {
			return mp, h, true, nil
		}
	}
	if s.cfg.Chain != nil {
		if mp, h, err := s.cfg.Chain.Proof(ctx, txid); err == nil && mp != nil {
			return mp, h, true, nil
		}
	}
	return nil, 0, false, nil
}

func (s *bcommonServices) MerklePath(ctx context.Context, txid string) (*wdk.MerklePathResult, error) {
	mp, h, ok, err := s.lookupProof(ctx, txid)
	if err != nil {
		return nil, err
	}
	if !ok || mp == nil {
		return &wdk.MerklePathResult{Name: "bcommon"}, nil
	}
	if h == 0 {
		h = mp.BlockHeight
	}
	txHash, hErr := chainhash.NewHashFromHex(txid)
	if hErr != nil {
		return nil, fmt.Errorf("wallet services: invalid txid %q: %w", txid, hErr)
	}
	root, rErr := mp.ComputeRoot(txHash)
	if rErr != nil || root == nil {
		return nil, fmt.Errorf("wallet services: compute merkle root for %s: %w", txid, rErr)
	}
	// Fail-closed SPV verification: validate computed Merkle root against block headers!
	if s.cfg.Tracker != nil {
		valid, vErr := s.IsValidRootForHeight(ctx, root, h)
		if vErr != nil {
			return nil, fmt.Errorf("wallet services: verify root for %s at height %d: %w", txid, h, vErr)
		}
		if !valid {
			return nil, fmt.Errorf("wallet services: invalid merkle root %s for %s at height %d", root.String(), txid, h)
		}
	}
	rootHex := root.String()
	hdr, hdrErr := s.ChainHeaderByHeight(ctx, h)
	if hdrErr != nil && !s.cfg.InMemoryStore {
		return nil, hdrErr
	}
	blockHash := ""
	if hdr != nil && len(hdr.Hash) == 64 {
		blockHash = hdr.Hash
	} else if s.cfg.InMemoryStore {
		blockHash = syntheticHashHex("block", h)
	} else {
		return nil, fmt.Errorf("wallet services: missing verified block hash at height %d", h)
	}
	return &wdk.MerklePathResult{
		Name:       "bcommon",
		MerklePath: mp,
		BlockHeader: &wdk.MerklePathBlockHeader{
			Height:     h,
			MerkleRoot: rootHex,
			Hash:       blockHash,
		},
	}, nil
}

func (s *bcommonServices) RawTx(ctx context.Context, txID string) (wdk.RawTxResult, error) {
	if tx, ok := s.txCache.Get(txID); ok && tx != nil {
		return wdk.RawTxResult{TxID: txID, Name: "cache", RawTx: tx.Bytes()}, nil
	}
	if s.cfg.Chain != nil {
		if raw, err := s.cfg.Chain.TxRaw(ctx, txID); err == nil && len(raw) > 0 {
			if tx, pErr := transaction.NewTransactionFromBytes(raw); pErr == nil {
				s.txCache.Put(txID, tx)
			}
			return wdk.RawTxResult{TxID: txID, Name: "bcommon-chain", RawTx: raw}, nil
		}
	}
	return wdk.RawTxResult{TxID: txID, Name: "bcommon"}, nil
}

func (s *bcommonServices) GetBEEF(ctx context.Context, txID string, knownTxIDs []string) (*transaction.Beef, error) {
	known := make(map[string]struct{}, len(knownTxIDs))
	for _, k := range knownTxIDs {
		known[k] = struct{}{}
	}
	tx, ok := s.txCache.Get(txID)
	if !ok || tx == nil {
		if s.cfg.Chain == nil {
			return nil, fmt.Errorf("wallet services: tx %s not found locally and no chain client configured", txID)
		}
		raw, err := s.cfg.Chain.TxRaw(ctx, txID)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("wallet services: fetch tx %s: %w", txID, err)
		}
		tx, err = transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("wallet services: parse tx %s: %w", txID, err)
		}
	}
	if tx.MerklePath == nil {
		if mp, _, found, _ := s.lookupProof(ctx, txID); found && mp != nil {
			tx.MerklePath = mp
		}
	}
	beef, err := transaction.NewBeefFromTransaction(tx)
	if err != nil {
		// Fall back to direct beef assembly with txid-only known ancestors if needed.
		b := transaction.NewBeef()
		if _, mErr := b.MergeTransaction(tx); mErr != nil {
			return nil, err
		}
		return b, nil
	}
	for k := range known {
		_ = k
	}
	return beef, nil
}

func (s *bcommonServices) NLockTimeIsFinal(ctx context.Context, txOrLockTime any) (bool, error) {
	const locktimeThreshold = uint32(500_000_000)
	var lockTime uint32
	switch v := txOrLockTime.(type) {
	case *transaction.Transaction:
		if v == nil {
			return true, nil
		}
		allFinal := true
		for _, in := range v.Inputs {
			if in.SequenceNumber != 0xffffffff {
				allFinal = false
				break
			}
		}
		if allFinal {
			return true, nil
		}
		lockTime = v.LockTime
	case uint32:
		lockTime = v
	case int:
		if v <= 0 {
			return true, nil
		}
		lockTime = uint32(v)
	default:
		return true, nil
	}
	if lockTime == 0 {
		return true, nil
	}
	if lockTime >= locktimeThreshold {
		return int64(lockTime) <= time.Now().Unix(), nil
	}
	h, err := s.CurrentHeight(ctx)
	if err != nil {
		return false, err
	}
	return lockTime <= h, nil
}

func (s *bcommonServices) GetStatusForTxIDs(ctx context.Context, txIDs []string) (*wdk.GetStatusForTxIDsResult, error) {
	tip, _ := s.CurrentHeight(ctx)
	results := make([]wdk.TxStatusDetail, 0, len(txIDs))
	for _, id := range txIDs {
		if mp, h, ok, _ := s.lookupProof(ctx, id); ok && mp != nil {
			if h == 0 {
				h = mp.BlockHeight
			}
			// Verify proof root before declaring mined.
			validProof := true
			if s.cfg.Tracker != nil {
				if txHash, hErr := chainhash.NewHashFromHex(id); hErr == nil {
					if root, rErr := mp.ComputeRoot(txHash); rErr != nil || root == nil {
						validProof = false
					} else if v, vErr := s.IsValidRootForHeight(ctx, root, h); vErr != nil || !v {
						validProof = false
					}
				} else {
					validProof = false
				}
			}
			if validProof {
				depth := 1
				if tip >= h {
					depth = int(tip-h) + 1
				}
				results = append(results, wdk.TxStatusDetail{
					TxID:   id,
					Depth:  &depth,
					Status: wdk.ResultStatusForTxIDMined.String(),
				})
				continue
			}
		}
		// Never treat local RAM txCache as authoritative mempool proof — query external chain only!
		if s.cfg.Chain != nil {
			if ks, ok := s.cfg.Chain.(interface {
				Known(context.Context, string) (bool, error)
			}); ok {
				if known, kErr := ks.Known(ctx, id); kErr == nil && known {
					zero := 0
					results = append(results, wdk.TxStatusDetail{
						TxID:   id,
						Depth:  &zero,
						Status: wdk.ResultStatusForTxIDKnown.String(),
					})
					continue
				}
			}
			if raw, rErr := s.cfg.Chain.TxRaw(ctx, id); rErr == nil && len(raw) > 0 {
				zero := 0
				results = append(results, wdk.TxStatusDetail{
					TxID:   id,
					Depth:  &zero,
					Status: wdk.ResultStatusForTxIDKnown.String(),
				})
				continue
			}
		}
		results = append(results, wdk.TxStatusDetail{
			TxID:   id,
			Status: wdk.ResultStatusForTxIDNotFound.String(),
		})
	}
	return &wdk.GetStatusForTxIDsResult{
		Name:    "bcommon",
		Status:  wdk.GetStatusSuccess,
		Results: results,
	}, nil
}

// findAndHydrateTx locates txid in beef (or cache) and hydrates input SourceTransaction pointers
// so Settler.Submit / testchain.Send receives a fully self-contained signed transaction.
func findAndHydrateTx(beef *transaction.Beef, txid string, cache *txLRU) *transaction.Transaction {
	var tx *transaction.Transaction
	if beef != nil {
		tx = beef.FindTransaction(txid)
	}
	if tx == nil && cache != nil {
		tx, _ = cache.Get(txid)
	}
	if tx == nil {
		return nil
	}
	hydrateInputs(tx, beef, cache, 0)
	return tx
}

func hydrateInputs(tx *transaction.Transaction, beef *transaction.Beef, cache *txLRU, depth int) {
	if tx == nil || depth > 32 {
		return
	}
	for _, in := range tx.Inputs {
		if in.SourceTXID == nil || in.SourceTransaction != nil {
			continue
		}
		pid := in.SourceTXID.String()
		var ptx *transaction.Transaction
		if beef != nil {
			ptx = beef.FindTransaction(pid)
		}
		if ptx == nil && cache != nil {
			ptx, _ = cache.Get(pid)
		}
		if ptx != nil {
			in.SourceTransaction = ptx
			if ptx.MerklePath == nil {
				hydrateInputs(ptx, beef, cache, depth+1)
			}
		}
	}
}

func syntheticChainBlockHeader(height uint32) *wdk.ChainBlockHeader {
	var prev uint32
	if height > 0 {
		prev = height - 1
	}
	return &wdk.ChainBlockHeader{
		ChainBaseBlockHeader: wdk.ChainBaseBlockHeader{
			Version:      1,
			PreviousHash: syntheticHashHex("block", prev),
			MerkleRoot:   syntheticHashHex("root", height),
			Time:         uint32(time.Now().Unix()),
			Bits:         0x1d00ffff,
			Nonce:        height,
		},
		Height: uint(height),
		Hash:   syntheticHashHex("block", height),
	}
}

func syntheticHashHex(tag string, height uint32) string {
	var buf [16]byte
	copy(buf[:], tag)
	binary.LittleEndian.PutUint32(buf[12:], height)
	sum := sha256.Sum256(buf[:])
	return hex.EncodeToString(sum[:])
}

// isNotEnoughFunds reports whether err indicates insufficient wallet funds.
func isNotEnoughFunds(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrInsufficient) || errors.Is(err, wdk.ErrNotEnoughFunds) {
		return true
	}
	low := strings.ToLower(err.Error())
	return strings.Contains(low, "not enough funds") || strings.Contains(low, "insufficient")
}

// isUncertainBroadcastError reports whether err is a timeout, canceled context, network transport
// failure, or 5xx response where the transaction MAY have been accepted by the network.
func isUncertainBroadcastError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrBroadcastUnknown) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return true
	}
	low := strings.ToLower(err.Error())
	for _, tok := range []string{
		"timeout", "deadline exceeded", "eof", "connection reset", "connection refused",
		"broken pipe", "http 500", "http 502", "http 503", "http 504", "status 500", "status 502", "status 503", "status 504",
		"temporary failure", "context canceled",
	} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}

func isDefiniteRejectError(lower string) bool {
	for _, tok := range []string{
		"bad-txns", "insufficient fee", "min relay fee", "dust", "txn-mempool-conflict",
		"mandatory-script-verify-flag", "non-mandatory-script-verify-flag", "http 400", "http 422", "http 46",
		"status 400", "status 422", "status 46", "rejected", "invalid", "script",
	} {
		if strings.Contains(lower, tok) {
			return true
		}
	}
	return false
}
