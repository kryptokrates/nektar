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
	"github.com/bsv-blockchain/go-sdk/spv"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/lightwebinc/bcommon/publish"
)

// bcommonServices adapts Nektar's lightwebinc/bcommon chain stack (Headers/Testchain, Arcade,
// Settler, Chain/WoC) to go-wallet-toolbox's wdk.Services interface. Implementing wdk.Services
// directly lets Nektar use go-wallet-toolbox/pkg/wallet + pkg/storage WITHOUT importing
// go-wallet-toolbox/pkg/services (eliminating all Kubernetes, Teranode, libp2p, and AWS deps).
type bcommonServices struct {
	cfg            Config
	txCache        *txLRU
	ownedOutputSum func(txid string) (uint64, bool)
	isIncomingTx   func(txid string) bool
	loadTxFromDB   func(ctx context.Context, txid string) *transaction.Transaction
	loadBeefFromDB func(ctx context.Context, txid string) (*transaction.Beef, error)

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

func (s *bcommonServices) maxDepth() int {
	if s.cfg.MaxUnprovenDepth > 0 {
		return s.cfg.MaxUnprovenDepth
	}
	return DefaultMaxUnprovenDepth
}

func (s *bcommonServices) dbLoader(ctx context.Context) func(string) *transaction.Transaction {
	if s.loadTxFromDB == nil {
		return nil
	}
	return func(txid string) *transaction.Transaction {
		return s.loadTxFromDB(ctx, txid)
	}
}

func (s *bcommonServices) CurrentHeight(ctx context.Context) (uint32, error) {
	if s.cfg.Tracker == nil {
		return 0, ErrNoChain
	}
	h, err := s.cfg.Tracker.CurrentHeight(ctx)
	if err != nil {
		return 0, err
	}
	if h == 0 && !s.cfg.InMemoryStore {
		return 0, fmt.Errorf("%w: chain tracker reported height 0", ErrNoChain)
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
	loader := s.dbLoader(ctx)
	for _, id := range txids {
		tx := findAndHydrateTx(beef, id, s.txCache, loader, s.maxDepth())
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

		// Exempt verified incoming non-outgoing transactions from the receiver's SpendGuard so
		// go-wallet-toolbox BackgroundBroadcast never charges the buyer's change against the seller.
		incoming := isIncomingInternalize(ctx) || (s.isIncomingTx != nil && s.isIncomingTx(id))

		// Single post-signing SpendGuard chokepoint for outgoing spends.
		if !incoming && s.cfg.SpendGuard != nil && !isOverrideGuard(ctx) {
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
				if len(posted.TxIDResults) > 0 {
					if firstErr == nil {
						firstErr = rejErr
					}
					posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
						TxID:   id,
						Result: wdk.PostedTxIDResultError,
						Error:  rejErr,
					})
					continue
				}
				return nil, rejErr
			}
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
			case isUncertainBroadcastError(bErr):
				// Transport/timeout/5xx/TCP/Arcade-RECEIVED-timeout: outcome is UNKNOWN.
				// Never refund SpendGuard or release UTXO reservations.
				unkErr := fmt.Errorf("%w: %w", ErrBroadcastUnknown, bErr)
				s.setLastErr(id, unkErr)
				postErrSinkFromCtx(ctx).record(id, unkErr)
				if len(posted.TxIDResults) > 0 {
					if firstErr == nil {
						firstErr = unkErr
					}
					posted.TxIDResults = append(posted.TxIDResults, wdk.PostedTxID{
						TxID:   id,
						Result: wdk.PostedTxIDResultError,
						Error:  unkErr,
					})
					continue
				}
				return nil, unkErr
			default:
				// Treat broadcaster error strings (including "utxo_spent"/"double_spend"/"400")
				// strictly as observational hints: NEVER mark terminal PostedTxIDResultDoubleSpend
				// or refund SpendGuard post-submit without a verified competing BUMP.
				s.setLastErr(id, bErr)
				postErrSinkFromCtx(ctx).record(id, bErr)
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

		// Broadcast succeeded; cache hydrated tx. Only run instant post-broadcast proof lookup on
		// ephemeral testchain (where Broadcast auto-mines synchronously); skip wasted live roundtrips.
		item := wdk.PostedTxID{
			TxID:   id,
			Result: wdk.PostedTxIDResultSuccess,
		}
		if s.cfg.ProofLookup != nil {
			if mp, h, ok, _ := s.lookupProof(ctx, id); ok && mp != nil {
				tx.MerklePath = mp
				item.MerklePath = mp
				item.BlockHeight = h
				if hdr, hErr := s.ChainHeaderByHeight(ctx, h); hErr == nil && hdr != nil {
					item.BlockHash = hdr.Hash
				}
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

// lookupProof resolves a MerklePath for txid and ALWAYS verifies ComputeRoot + IsValidRootForHeight
// against the configured witnessed ChainTracker before returning ok=true.
// Fails closed when Tracker == nil and propagates tracker/upstream outages distinctly from not-mined.
func (s *bcommonServices) lookupProof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if s.cfg.Tracker == nil {
		return nil, 0, false, ErrNoChain
	}
	txHash, hErr := chainhash.NewHashFromHex(txid)
	if hErr != nil {
		return nil, 0, false, fmt.Errorf("wallet services: invalid txid %q: %w", txid, hErr)
	}
	verify := func(mp *transaction.MerklePath, h uint32) (*transaction.MerklePath, uint32, bool, error) {
		if mp == nil {
			return nil, 0, false, nil
		}
		if h == 0 {
			h = mp.BlockHeight
		}
		root, rErr := mp.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			return nil, 0, false, nil
		}
		valid, vErr := s.IsValidRootForHeight(ctx, root, h)
		if vErr != nil {
			return nil, 0, false, fmt.Errorf("wallet services: verify root for %s at height %d: %w", txid, h, vErr)
		}
		if !valid {
			return nil, 0, false, nil
		}
		return mp, h, true, nil
	}

	if s.cfg.ProofLookup != nil {
		mp, h, ok, err := s.cfg.ProofLookup(ctx, txid)
		if err != nil {
			return nil, 0, false, err
		}
		if ok && mp != nil {
			return verify(mp, h)
		}
		return nil, 0, false, nil
	}

	var lastErr error
	if s.cfg.Arcade != nil {
		mp, h, err := s.cfg.Arcade.Proof(ctx, txid)
		if err == nil && mp != nil {
			vmp, vh, ok, vErr := verify(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if ok {
				return vmp, vh, true, nil
			}
		} else if err != nil && !errors.Is(err, nodeapi.ErrNotMined) && !errors.Is(err, publish.ErrArcadeUnknown) && !errors.Is(err, publish.ErrArcadeRefused) {
			lastErr = err
		}
	}
	if s.cfg.Chain != nil {
		mp, h, err := s.cfg.Chain.Proof(ctx, txid)
		if err == nil && mp != nil {
			vmp, vh, ok, vErr := verify(mp, h)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if ok {
				return vmp, vh, true, nil
			}
		} else if err != nil && !errors.Is(err, nodeapi.ErrNotMined) {
			lastErr = err
		}
	}
	return nil, 0, false, lastErr
}

func (s *bcommonServices) MerklePath(ctx context.Context, txid string) (*wdk.MerklePathResult, error) {
	if s.cfg.Tracker == nil {
		return nil, ErrNoChain
	}
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
	rootHex := root.String()
	hdr, hdrErr := s.ChainHeaderByHeight(ctx, h)
	if hdrErr != nil && !s.cfg.InMemoryStore {
		return nil, hdrErr
	}
	blockHash := ""
	if hdr != nil && len(hdr.Hash) == 64 {
		// Prevent reorg drift between BUMP root and the fetched header at height h.
		if !s.cfg.InMemoryStore && !strings.EqualFold(hdr.MerkleRoot, rootHex) {
			return nil, fmt.Errorf("wallet services: header merkle root %s disagrees with BUMP root %s at height %d", hdr.MerkleRoot, rootHex, h)
		}
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
	if s.loadTxFromDB != nil {
		if tx := s.loadTxFromDB(ctx, txID); tx != nil {
			s.txCache.Put(txID, tx)
			return wdk.RawTxResult{TxID: txID, Name: "sqlite", RawTx: tx.Bytes()}, nil
		}
	}
	if s.cfg.Chain != nil {
		raw, err := s.cfg.Chain.TxRaw(ctx, txID)
		if err != nil {
			return wdk.RawTxResult{TxID: txID, Name: "bcommon-chain"}, err
		}
		if len(raw) > 0 {
			parsed, pErr := transaction.NewTransactionFromBytes(raw)
			if pErr != nil {
				return wdk.RawTxResult{TxID: txID, Name: "bcommon-chain"}, fmt.Errorf("wallet services: parse fetched raw tx %s: %w", txID, pErr)
			}
			// Always verify sha256d(raw) == txID before trusting or caching bytes from an indexer.
			if parsed.TxID().String() != txID {
				return wdk.RawTxResult{TxID: txID, Name: "bcommon-chain"}, fmt.Errorf("wallet services: fetched txid mismatch: got %s want %s", parsed.TxID().String(), txID)
			}
			s.txCache.Put(txID, parsed)
			return wdk.RawTxResult{TxID: txID, Name: "bcommon-chain", RawTx: raw}, nil
		}
	}
	return wdk.RawTxResult{TxID: txID, Name: "bcommon"}, nil
}

func (s *bcommonServices) GetBEEF(ctx context.Context, txID string, knownTxIDs []string) (*transaction.Beef, error) {
	// 1. Check local SQLite storage BEEF first to avoid unnecessary remote refetches.
	if s.loadBeefFromDB != nil {
		if b, err := s.loadBeefFromDB(ctx, txID); err == nil && b != nil {
			if tx := b.FindTransaction(txID); tx != nil {
				hydrateInputs(tx, b, s.txCache, s.dbLoader(ctx), 0, s.maxDepth())
				return b, nil
			}
		}
	}

	known := make(map[string]struct{}, len(knownTxIDs))
	for _, k := range knownTxIDs {
		known[k] = struct{}{}
	}
	tx, err := s.fetchHydratedTx(ctx, txID, known, make(map[string]bool), 0)
	if err != nil {
		return nil, err
	}
	beef, err := transaction.NewBeefFromTransaction(tx)
	if err != nil {
		return nil, fmt.Errorf("wallet services: assemble complete BEEF for %s: %w", txID, err)
	}
	return beef, nil
}

// fetchHydratedTx loads txID from cache, SQLite, or Chain.TxRaw (verifying sha256d == txID)
// and recursively fetches/attaches unmined ancestor transactions up to MaxUnprovenDepth back to
// header-verified mined BUMPs so 0-conf deposits assemble complete, verifiable AtomicBEEFs.
func (s *bcommonServices) fetchHydratedTx(
	ctx context.Context,
	txID string,
	known map[string]struct{},
	visiting map[string]bool,
	depth int,
) (*transaction.Transaction, error) {
	if visiting[txID] {
		return nil, fmt.Errorf("wallet services: cycle in tx ancestry at %s", txID)
	}
	if depth > s.maxDepth() {
		return nil, fmt.Errorf("%w: ancestor depth exceeds %d at %s", ErrDepthExceeded, s.maxDepth(), txID)
	}
	visiting[txID] = true
	defer delete(visiting, txID)

	tx, ok := s.txCache.Get(txID)
	if (!ok || tx == nil) && s.loadTxFromDB != nil {
		tx = s.loadTxFromDB(ctx, txID)
	}
	if tx == nil {
		if s.cfg.Chain == nil {
			return nil, fmt.Errorf("wallet services: tx %s not found locally and no chain client configured", txID)
		}
		raw, err := s.cfg.Chain.TxRaw(ctx, txID)
		if err != nil || len(raw) == 0 {
			return nil, fmt.Errorf("wallet services: fetch tx %s: %w", txID, err)
		}
		parsed, err := transaction.NewTransactionFromBytes(raw)
		if err != nil {
			return nil, fmt.Errorf("wallet services: parse tx %s: %w", txID, err)
		}
		if parsed.TxID().String() != txID {
			return nil, fmt.Errorf("wallet services: fetched txid mismatch: got %s want %s", parsed.TxID().String(), txID)
		}
		tx = parsed
	}

	if tx.MerklePath == nil && s.cfg.Tracker != nil {
		if mp, _, found, lErr := s.lookupProof(ctx, txID); lErr == nil && found && mp != nil {
			tx.MerklePath = mp
		}
	}
	if tx.MerklePath != nil {
		s.txCache.Put(txID, tx)
		return tx, nil
	}

	// Unmined transaction: hydrate every input recursively back to a mined BUMP (or known ancestor).
	for i, in := range tx.Inputs {
		if in == nil || in.SourceTXID == nil {
			return nil, fmt.Errorf("wallet services: tx %s input %d missing source txid", txID, i)
		}
		pid := in.SourceTXID.String()
		if in.SourceTransaction == nil {
			ptx, pErr := s.fetchHydratedTx(ctx, pid, known, visiting, depth+1)
			if pErr != nil {
				return nil, fmt.Errorf("wallet services: hydrate parent %s of %s: %w", pid, txID, pErr)
			}
			in.SourceTransaction = ptx
		} else if in.SourceTransaction.MerklePath == nil {
			// Ensure already-attached unmined parent is itself fully hydrated back to mined BUMPs.
			if _, pErr := s.fetchHydratedTx(ctx, pid, known, visiting, depth+1); pErr != nil {
				return nil, fmt.Errorf("wallet services: hydrate ancestor %s of %s: %w", pid, txID, pErr)
			}
		}
	}
	s.txCache.Put(txID, tx)
	return tx, nil
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
		// Consensus BIP113/BSV rule: timestamp locktime is final iff lockTime < 11-block Median Time Past.
		if s.cfg.MedianTimePast != nil {
			mtp, err := s.cfg.MedianTimePast(ctx)
			if err != nil {
				return false, fmt.Errorf("wallet services: median time past: %w", err)
			}
			return lockTime < mtp, nil
		}
		if mp, ok := s.cfg.Tracker.(spv.MedianTimePastProvider); ok {
			mtp, err := mp.MedianTimePast(ctx)
			if err != nil {
				return false, fmt.Errorf("wallet services: median time past: %w", err)
			}
			return lockTime < mtp, nil
		}
		if s.cfg.InMemoryStore {
			return int64(lockTime) < time.Now().Unix(), nil
		}
		return false, fmt.Errorf("%w: no MedianTimePast header source configured for timestamp lockTime %d", ErrNoChain, lockTime)
	}
	h, err := s.CurrentHeight(ctx)
	if err != nil {
		return false, err
	}
	return lockTime <= h, nil
}

func (s *bcommonServices) GetStatusForTxIDs(ctx context.Context, txIDs []string) (*wdk.GetStatusForTxIDsResult, error) {
	if s.cfg.Tracker == nil {
		return nil, ErrNoChain
	}
	tip, err := s.CurrentHeight(ctx)
	if err != nil {
		return nil, fmt.Errorf("wallet services: current height for status check: %w", err)
	}
	if s.cfg.Chain == nil && s.cfg.ProofLookup == nil && !s.cfg.InMemoryStore {
		return nil, fmt.Errorf("%w: no chain source configured for tx status", ErrNoChain)
	}

	results := make([]wdk.TxStatusDetail, 0, len(txIDs))
	for _, id := range txIDs {
		mp, h, ok, pErr := s.lookupProof(ctx, id)
		if pErr != nil {
			return nil, pErr
		}
		if ok && mp != nil {
			if h == 0 {
				h = mp.BlockHeight
			}
			if tip < h {
				// Witnessed tip lags BUMP height; fail closed rather than fabricating depth = 1.
				return nil, fmt.Errorf("wallet services: witnessed tip %d trails proof height %d for %s", tip, h, id)
			}
			depth := int(tip-h) + 1
			results = append(results, wdk.TxStatusDetail{
				TxID:   id,
				Depth:  &depth,
				Status: wdk.ResultStatusForTxIDMined.String(),
			})
			continue
		}

		// Never trust local RAM txCache or byte-less indexer Known JSON: fetch raw tx bytes and
		// verify sha256d(raw) == id before declaring ResultStatusForTxIDKnown.
		if s.cfg.Chain == nil {
			if s.cfg.InMemoryStore {
				results = append(results, wdk.TxStatusDetail{
					TxID:   id,
					Status: wdk.ResultStatusForTxIDNotFound.String(),
				})
				continue
			}
			return nil, fmt.Errorf("%w: cannot check mempool presence for %s without chain client", ErrNoChain, id)
		}
		raw, rErr := s.cfg.Chain.TxRaw(ctx, id)
		if rErr == nil && len(raw) > 0 {
			parsed, parseErr := transaction.NewTransactionFromBytes(raw)
			if parseErr != nil || parsed.TxID().String() != id {
				return nil, fmt.Errorf("wallet services: raw tx hash mismatch for %s", id)
			}
			zero := 0
			results = append(results, wdk.TxStatusDetail{
				TxID:   id,
				Depth:  &zero,
				Status: wdk.ResultStatusForTxIDKnown.String(),
			})
			continue
		}
		if rErr != nil {
			lower := strings.ToLower(rErr.Error())
			if !strings.Contains(lower, "404") && !strings.Contains(lower, "not found") && !strings.Contains(lower, "no such") {
				return nil, fmt.Errorf("wallet services: status lookup %s: %w", id, rErr)
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

// findAndHydrateTx locates txid in beef (or cache/SQLite) and hydrates input SourceTransaction pointers
// so Settler.Submit / testchain.Send receives a fully self-contained signed transaction.
func findAndHydrateTx(beef *transaction.Beef, txid string, cache *txLRU, loader func(string) *transaction.Transaction, maxDepth int) *transaction.Transaction {
	var tx *transaction.Transaction
	if beef != nil {
		tx = beef.FindTransaction(txid)
	}
	if tx == nil && cache != nil {
		tx, _ = cache.Get(txid)
	}
	if tx == nil && loader != nil {
		tx = loader(txid)
	}
	if tx == nil {
		return nil
	}
	hydrateInputs(tx, beef, cache, loader, 0, maxDepth)
	return tx
}

func hydrateInputs(tx *transaction.Transaction, beef *transaction.Beef, cache *txLRU, loader func(string) *transaction.Transaction, depth, maxDepth int) {
	if maxDepth <= 0 {
		maxDepth = DefaultMaxUnprovenDepth
	}
	if tx == nil || depth > maxDepth {
		return
	}
	for _, in := range tx.Inputs {
		if in == nil || in.SourceTXID == nil || in.SourceTransaction != nil {
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
		if ptx == nil && loader != nil {
			ptx = loader(pid)
		}
		if ptx != nil {
			in.SourceTransaction = ptx
			if ptx.MerklePath == nil {
				hydrateInputs(ptx, beef, cache, loader, depth+1, maxDepth)
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
