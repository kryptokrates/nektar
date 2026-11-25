package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/bwallet"
)

func (p *Port) beefDir() string {
	return filepath.Join(p.dataDir, "wallet", "txbeef")
}

// SafeAtomicBEEF encodes tx as a strict BRC-95 AtomicBEEF (allowPartial=false), falling back
// to allowPartial=true only when spending an external unconfirmed faucet output whose remote
// ancestor BUMP has not yet mined.
func SafeAtomicBEEF(tx *transaction.Transaction) ([]byte, error) {
	if tx == nil {
		return nil, errors.New("chain: nil transaction")
	}
	if b, err := tx.AtomicBEEF(false); err == nil {
		return b, nil
	}
	return tx.AtomicBEEF(true)
}

// saveTxBEEF persists a transaction's AtomicBEEF bytes on disk so subsequent 0-conf spends
// can hydrate full ancestor proofs all the way back to a confirmed grandparent BUMP.
func (p *Port) saveTxBEEF(tx *transaction.Transaction) {
	if tx == nil || p.dataDir == "" {
		return
	}
	beef, err := SafeAtomicBEEF(tx)
	if err != nil || len(beef) == 0 {
		return
	}
	dir := p.beefDir()
	_ = os.MkdirAll(dir, 0o700)
	path := filepath.Join(dir, tx.TxID().String()+".beef")
	_ = os.WriteFile(path, beef, 0o600)
}

// loadTxWithAncestors reconstructs coin's parent transaction WITH either its mined MerklePath
// OR its cached/remote AtomicBEEF ancestor chain (so child tx.AtomicBEEF(false) succeeds even at 0-conf!).
func (p *Port) loadTxWithAncestors(ctx context.Context, coin bwallet.Output) (*transaction.Transaction, error) {
	// 1. Check in-memory tx cache first (already carries full parent pointers / MerklePath!).
	p.mu.Lock()
	cached := p.txs[coin.TxID]
	p.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	// 2. Check persisted txbeef/<txid>.beef on disk.
	if p.dataDir != "" {
		if rawBeef, err := os.ReadFile(filepath.Join(p.beefDir(), coin.TxID+".beef")); err == nil && len(rawBeef) > 0 {
			if tx, err := transaction.NewTransactionFromBEEF(rawBeef); err == nil {
				return tx, nil
			}
		}
	}
	// 3. Parse from coin.Raw + attach coin.Bump if present.
	if coin.Raw != "" {
		srcTx, err := transaction.NewTransactionFromHex(coin.Raw)
		if err != nil {
			return nil, err
		}
		if coin.Bump != "" {
			if bumpBytes, bErr := hex.DecodeString(coin.Bump); bErr == nil {
				if mp, mErr := transaction.NewMerklePathFromBinary(bumpBytes); mErr == nil {
					srcTx.MerklePath = mp
				}
			}
		}
		if srcTx.MerklePath != nil || p.woc == nil {
			return srcTx, nil
		}
		// Unproven coin without cached BEEF: try fetching full ancestor BEEF from WhatsOnChain!
		if beefBytes, bErr := p.woc.TxBEEF(ctx, coin.TxID); bErr == nil && len(beefBytes) > 0 {
			if fullTx, tErr := transaction.NewTransactionFromBEEF(beefBytes); tErr == nil {
				p.saveTxBEEF(fullTx)
				return fullTx, nil
			}
		}
		return srcTx, nil
	}
	// 4. Last resort: fetch from WoC.
	if p.woc != nil {
		if beefBytes, bErr := p.woc.TxBEEF(ctx, coin.TxID); bErr == nil && len(beefBytes) > 0 {
			if fullTx, tErr := transaction.NewTransactionFromBEEF(beefBytes); tErr == nil {
				p.saveTxBEEF(fullTx)
				return fullTx, nil
			}
		}
		if rawBytes, rErr := p.woc.TxRaw(ctx, coin.TxID); rErr == nil && len(rawBytes) > 0 {
			return transaction.NewTransactionFromBytes(rawBytes)
		}
	}
	return nil, errors.New("parent transaction raw/BEEF unavailable")
}

// AdoptReceived credits any outputs in a counterparty's verified AtomicBEEF payment tx that pay
// this wallet's FundScript() directly into bwallet.Pool (and propagates to settler if not yet known).
func (p *Port) AdoptReceived(ctx context.Context, tx *transaction.Transaction) (uint64, error) {
	if tx == nil || p.embedded == nil || p.embedded.Pool == nil {
		return 0, nil
	}
	fundScript, err := p.signer.FundScript()
	if err != nil {
		return 0, err
	}
	fundHex := fundScript.String()
	txid := tx.TxID().String()

	// Ensure transaction is propagated on live networks if settler hasn't seen it yet.
	if p.testChain == nil && p.settler != nil {
		known := false
		if p.nodeChain != nil {
			if k, kErr := p.nodeChain.Known(ctx, txid); kErr == nil && k {
				known = true
			}
		}
		if !known {
			_ = p.settler.Submit(ctx, tx)
		}
	}

	p.saveTxBEEF(tx)
	p.mu.Lock()
	p.txs[txid] = tx
	p.mu.Unlock()

	var bumpHex string
	var height uint32
	if tx.MerklePath != nil {
		bumpHex = hex.EncodeToString(tx.MerklePath.Bytes())
		height = tx.MerklePath.BlockHeight
	}
	var credited uint64
	for vout, out := range tx.Outputs {
		if out.LockingScript != nil && out.LockingScript.String() == fundHex && out.Satoshis > 0 {
			_, err := p.embedded.Pool.Add(bwallet.Output{
				TxID:          txid,
				Vout:          uint32(vout),
				Satoshis:      out.Satoshis,
				LockingScript: fundHex,
				Height:        height,
				Raw:           tx.Hex(),
				Bump:          bumpHex,
				Unproven:      tx.MerklePath == nil,
			})
			if err != nil {
				return credited, err
			}
			credited += out.Satoshis
		}
	}
	return credited, nil
}

type wocUnspentAllResp struct {
	Result []struct {
		Height   uint32 `json:"height"`
		TxPos    uint32 `json:"tx_pos"`
		TxHash   string `json:"tx_hash"`
		Value    uint64 `json:"value"`
		IsSpent  bool   `json:"isSpentInMempoolTx"`
	} `json:"result"`
}

// SyncPool queries WhatsOnChain (testnet/main) for all unspent outputs paying FundAddress(),
// imports new confirmed & unconfirmed UTXOs with raw tx + BUMP/BEEF into bwallet.Pool,
// upgrades existing Unproven entries once mined, and prunes spent coins.
func (p *Port) SyncPool(ctx context.Context) (added int, proved int, err error) {
	if p.embedded == nil || p.embedded.Pool == nil {
		return 0, 0, nil
	}
	if p.testChain != nil || p.woc == nil {
		return 0, 0, nil
	}
	addr, err := p.signer.FundAddress(p.mainnet)
	if err != nil {
		return 0, 0, err
	}
	fundScript, err := p.signer.FundScript()
	if err != nil {
		return 0, 0, err
	}
	fundHex := fundScript.String()

	wocNet := "test"
	if p.mainnet {
		wocNet = "main"
	}
	urlStr := fmt.Sprintf("https://api.whatsonchain.com/v1/bsv/%s/address/%s/unspent/all", wocNet, addr)
	var body []byte
	for attempt := range 3 {
		req, rErr := http.NewRequestWithContext(ctx, http.MethodGet, urlStr, nil)
		if rErr != nil {
			return 0, 0, rErr
		}
		if p.woc.Key != "" {
			req.Header.Set("Authorization", p.woc.Key)
		}
		resp, dErr := p.httpClient.Do(req)
		if dErr != nil {
			if attempt == 2 {
				return 0, 0, fmt.Errorf("chain sync: woc unspent: %w", dErr)
			}
			time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
			continue
		}
		body, _ = io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			time.Sleep(time.Duration(attempt+1) * 800 * time.Millisecond)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return 0, 0, fmt.Errorf("chain sync: woc HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
		}
		break
	}

	var parsed wocUnspentAllResp
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, 0, fmt.Errorf("chain sync: decode woc unspent: %w", err)
	}

	existingByOutpoint := make(map[string]bwallet.Output)
	for _, o := range p.embedded.Pool.Outputs() {
		existingByOutpoint[o.Outpoint()] = o
	}
	liveOutpoints := make(map[string]bool, len(parsed.Result))

	for _, u := range parsed.Result {
		if u.IsSpent || u.Value == 0 || u.TxHash == "" {
			continue
		}
		op := fmt.Sprintf("%s:%d", u.TxHash, u.TxPos)
		p.mu.Lock()
		_, alreadySpentByUs := p.spentOutpoints[op]
		p.mu.Unlock()
		if alreadySpentByUs {
			// Ignore lagging WoC unspent entry for an outpoint we already broadcast a spend for!
			continue
		}
		liveOutpoints[op] = true

		if cur, ok := existingByOutpoint[op]; ok {
			// Upgrade Unproven coin as soon as it mines!
			if cur.Unproven && u.Height > 0 {
				if mp, h, pErr := p.woc.Proof(ctx, u.TxHash); pErr == nil && mp != nil {
					if n, _ := p.embedded.Pool.Prove(u.TxHash, hex.EncodeToString(mp.Bytes()), h); n > 0 {
						proved += n
					}
				}
			}
			continue
		}

		// New UTXO: fetch raw tx + optional MerklePath/BEEF and verify txid!
		var rawHex, bumpHex string
		var height uint32
		if beefBytes, bErr := p.woc.TxBEEF(ctx, u.TxHash); bErr == nil && len(beefBytes) > 0 {
			if tx, tErr := transaction.NewTransactionFromBEEF(beefBytes); tErr == nil && tx.TxID().String() == u.TxHash {
				rawHex = tx.Hex()
				p.saveTxBEEF(tx)
				p.mu.Lock()
				p.txs[u.TxHash] = tx
				p.mu.Unlock()
				if tx.MerklePath != nil {
					bumpHex = hex.EncodeToString(tx.MerklePath.Bytes())
					height = tx.MerklePath.BlockHeight
				}
			}
		}
		if rawHex == "" {
			rawBytes, rErr := p.woc.TxRaw(ctx, u.TxHash)
			if rErr != nil {
				continue
			}
			tx, tErr := transaction.NewTransactionFromBytes(rawBytes)
			if tErr != nil || tx.TxID().String() != u.TxHash {
				continue
			}
			rawHex = tx.Hex()
			if u.Height > 0 {
				if mp, h, pErr := p.woc.Proof(ctx, u.TxHash); pErr == nil && mp != nil {
					tx.MerklePath = mp
					bumpHex = hex.EncodeToString(mp.Bytes())
					height = h
					p.saveTxBEEF(tx)
				}
			}
			p.mu.Lock()
			p.txs[u.TxHash] = tx
			p.mu.Unlock()
		}
		if height == 0 && u.Height > 0 {
			height = u.Height
		}
		// Re-check spentOutpoints & in-flight reserved coins right before Pool.Add to eliminate TOCTOU races
		// when a spend broadcasts while SyncPool is fetching TxRaw/TxBEEF!
		p.mu.Lock()
		_, spentNow := p.spentOutpoints[op]
		reservedNow := false
		for _, rCoin := range p.reserved {
			if rCoin.Outpoint() == op {
				reservedNow = true
				break
			}
		}
		p.mu.Unlock()
		if spentNow || reservedNow {
			continue
		}
		n, aErr := p.embedded.Pool.Add(bwallet.Output{
			TxID:          u.TxHash,
			Vout:          u.TxPos,
			Satoshis:      u.Value,
			LockingScript: fundHex,
			Height:        height,
			Raw:           rawHex,
			Bump:          bumpHex,
			Unproven:      bumpHex == "",
		})
		if aErr == nil && n > 0 {
			added += n
		}
	}

	// Prune any pool coins confirmed spent elsewhere on chain.
	for op, cur := range existingByOutpoint {
		if liveOutpoints[op] {
			continue
		}
		if spender, sErr := p.woc.Spender(ctx, cur.TxID, cur.Vout); sErr == nil && spender != "" {
			_, _ = p.embedded.Pool.Remove(cur)
		}
	}
	return added, proved, nil
}
