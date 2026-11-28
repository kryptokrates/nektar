package wallet

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-wallet-toolbox/pkg/wdk"
	"github.com/lightwebinc/bcommon/nodeapi"
)

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

	// 0. Check locally held verified proof FIRST (avoid unnecessary remote refetches).
	if cachedTx, ok := s.txCache.Get(txid); ok && cachedTx != nil && cachedTx.MerklePath != nil {
		vmp, vh, valid, vErr := verify(cachedTx.MerklePath, cachedTx.MerklePath.BlockHeight)
		if vErr != nil {
			return nil, 0, false, vErr
		}
		if valid {
			return vmp, vh, true, nil
		}
	}
	if s.loadTxFromDB != nil {
		if dbTx := s.loadTxFromDB(ctx, txid); dbTx != nil && dbTx.MerklePath != nil {
			vmp, vh, valid, vErr := verify(dbTx.MerklePath, dbTx.MerklePath.BlockHeight)
			if vErr != nil {
				return nil, 0, false, vErr
			}
			if valid {
				s.txCache.Put(txid, dbTx)
				return vmp, vh, true, nil
			}
		}
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
	if s.cfg.Broadcaster != nil {
		mp, h, found, err := s.cfg.Broadcaster.Proof(ctx, txid)
		if err == nil && found && mp != nil {
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
		} else if err == nil || errors.Is(err, nodeapi.ErrNotMined) {
			// Authoritative not-mined answer from Chain.Proof clears any prior broadcaster outage error.
			lastErr = nil
		} else {
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

	tx, err := s.fetchHydratedTx(ctx, txID, make(map[string]bool), 0)
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
		mp, _, found, lErr := s.lookupProof(ctx, txID)
		if lErr != nil {
			return nil, fmt.Errorf("wallet services: lookup proof %s: %w", txID, lErr)
		}
		if found && mp != nil {
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
			ptx, pErr := s.fetchHydratedTx(ctx, pid, visiting, depth+1)
			if pErr != nil {
				return nil, fmt.Errorf("wallet services: hydrate parent %s of %s: %w", pid, txID, pErr)
			}
			in.SourceTransaction = ptx
		} else if in.SourceTransaction.MerklePath == nil {
			// Ensure already-attached unmined parent is itself fully hydrated back to mined BUMPs.
			ptx, pErr := s.fetchHydratedTx(ctx, pid, visiting, depth+1)
			if pErr != nil {
				return nil, fmt.Errorf("wallet services: hydrate ancestor %s of %s: %w", pid, txID, pErr)
			}
			in.SourceTransaction = ptx
		}
	}
	s.txCache.Put(txID, tx)
	return tx, nil
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

	type slotResult struct {
		detail wdk.TxStatusDetail
		hasDet bool
		err    error
	}
	slots := make([]slotResult, len(txIDs))
	workers := min(16, max(1, len(txIDs)))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for idx, id := range txIDs {
		if ctx.Err() != nil {
			slots[idx] = slotResult{err: ctx.Err()}
			break
		}
		wg.Add(1)
		go func(i int, txID string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				slots[i] = slotResult{err: ctx.Err()}
				return
			}
			mp, h, ok, pErr := s.lookupProof(ctx, txID)
			if pErr != nil {
				slots[i] = slotResult{err: pErr}
				return
			}
			if ok && mp != nil {
				if h == 0 {
					h = mp.BlockHeight
				}
				if tip < h {
					slots[i] = slotResult{err: fmt.Errorf("wallet services: witnessed tip %d trails proof height %d for %s", tip, h, txID)}
					return
				}
				depth := int(tip-h) + 1
				slots[i] = slotResult{
					hasDet: true,
					detail: wdk.TxStatusDetail{
						TxID:   txID,
						Depth:  &depth,
						Status: wdk.ResultStatusForTxIDMined.String(),
					},
				}
				return
			}
			// Never trust local RAM txCache or byte-less indexer Known JSON: fetch raw tx bytes and
			// verify sha256d(raw) == id before declaring ResultStatusForTxIDKnown.
			if s.cfg.Chain == nil {
				if s.cfg.InMemoryStore {
					slots[i] = slotResult{
						hasDet: true,
						detail: wdk.TxStatusDetail{
							TxID:   txID,
							Status: wdk.ResultStatusForTxIDNotFound.String(),
						},
					}
					return
				}
				slots[i] = slotResult{err: fmt.Errorf("%w: cannot check mempool presence for %s without chain client", ErrNoChain, txID)}
				return
			}
			raw, rErr := s.cfg.Chain.TxRaw(ctx, txID)
			if rErr == nil && len(raw) > 0 {
				parsed, parseErr := transaction.NewTransactionFromBytes(raw)
				if parseErr != nil || parsed.TxID().String() != txID {
					slots[i] = slotResult{err: fmt.Errorf("wallet services: raw tx hash mismatch for %s", txID)}
					return
				}
				zero := 0
				slots[i] = slotResult{
					hasDet: true,
					detail: wdk.TxStatusDetail{
						TxID:   txID,
						Depth:  &zero,
						Status: wdk.ResultStatusForTxIDKnown.String(),
					},
				}
				return
			}
			// Never treat single-host silence, 404, or empty raw bytes as proof of non-existence on live networks.
			if s.cfg.InMemoryStore {
				slots[i] = slotResult{
					hasDet: true,
					detail: wdk.TxStatusDetail{
						TxID:   txID,
						Status: wdk.ResultStatusForTxIDNotFound.String(),
					},
				}
				return
			}
			if rErr != nil {
				slots[i] = slotResult{err: fmt.Errorf("wallet services: status lookup %s unconfirmed by single host: %w", txID, rErr)}
			} else {
				slots[i] = slotResult{err: fmt.Errorf("wallet services: status lookup %s returned empty raw tx", txID)}
			}
		}(idx, id)
	}
	wg.Wait()

	results := make([]wdk.TxStatusDetail, 0, len(txIDs))
	var firstErr error
	for _, sl := range slots {
		if sl.hasDet {
			results = append(results, sl.detail)
		} else if sl.err != nil && firstErr == nil {
			firstErr = sl.err
		}
	}
	if len(results) == 0 && firstErr != nil {
		return nil, firstErr
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
