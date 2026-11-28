package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const (
	// DefaultFundingTreeWidth pre-mints 64 equal-valued 1-sat outputs per tree (1BSV Paper 011 §9 scaled
	// 4x across 4 stripes = 256 pre-minted PushDrop carrier slots with 1 miner fee per 64 carriers).
	DefaultFundingTreeWidth = 64
	// DefaultFundingTreeAheadThreshold pre-mints the next tree at <=25% remaining slots (16 outputs)
	// so Ahead trees warm asynchronously off the critical path before Active exhausts.
	DefaultFundingTreeAheadThreshold = DefaultFundingTreeWidth / 4
	carrierTreeStripeCount           = 4
	maxRetiredFundingTrees           = 8
	retiredFundingTreeMaxAgeSec      = 72 * 3600
	carrierTreeKVKey                 = "carrier_funding_tree_v1"
)

// FundingTreeRecord persists one mined carrier funding tree and its allocation cursor
// (aligned with 1BSV Paper 011 §3.2 & bcommon/producer.Trees).
type FundingTreeRecord struct {
	TxID      string   `json:"txid"`
	BEEFHex   string   `json:"beefHex"`
	Vouts     []uint32 `json:"vouts"`
	NextIndex int      `json:"nextIndex"`
	Mined     bool     `json:"mined"`
	CreatedAt int64    `json:"createdAt"`
}

// FundingTreeState holds the active funding tree, an optional background pre-minted next tree,
// and recently rotated trees retained so RetractCarrier can always sweep live carriers.
type FundingTreeState struct {
	Active  *FundingTreeRecord   `json:"active,omitempty"`
	Ahead   *FundingTreeRecord   `json:"ahead,omitempty"`
	Pending *FundingTreeRecord   `json:"pending,omitempty"`
	Retired []*FundingTreeRecord `json:"retired,omitempty"`
}

type carrierTreeStripe struct {
	mu      sync.Mutex
	mintMu  sync.Mutex
	kvKey   string
	state   FundingTreeState
	minting bool
}

// CarrierTreeManager manages 4 striped M=64 carrier funding trees (256 slots total; adaptive M=16
// on tiny <500-sat balances) with background Ahead pre-minting and mined kill-switch sweeps (carrier.Sweep).
type CarrierTreeManager struct {
	stripes   [carrierTreeStripeCount]carrierTreeStripe
	stripeRR  atomic.Uint64
	port      *chain.Port
	db        *store.DB
	signer    *nwallet.MarketSigner
	width     int
	aheadAt   int
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewCarrierTreeManager opens or initializes a persistent 4x64 striped carrier funding tree manager.
func NewCarrierTreeManager(port *chain.Port, db *store.DB, signer *nwallet.MarketSigner) *CarrierTreeManager {
	m := &CarrierTreeManager{
		port:    port,
		db:      db,
		signer:  signer,
		width:   DefaultFundingTreeWidth,
		aheadAt: DefaultFundingTreeAheadThreshold,
		stopCh:  make(chan struct{}),
	}
	for i := 0; i < carrierTreeStripeCount; i++ {
		kvKey := carrierTreeKVKey
		if i > 0 {
			kvKey = fmt.Sprintf("%s:%d", carrierTreeKVKey, i)
		}
		m.stripes[i].kvKey = kvKey
		if db != nil {
			_, _ = db.GetJSON(kvKey, &m.stripes[i].state)
		}
	}
	return m
}

// Close stops background pre-minting goroutines and waits for them to exit cleanly.
func (m *CarrierTreeManager) Close() {
	if m == nil {
		return
	}
	m.closeOnce.Do(func() {
		if m.stopCh != nil {
			close(m.stopCh)
		}
	})
	m.wg.Wait()
}

func (m *CarrierTreeManager) saveStripeLocked(st *carrierTreeStripe) {
	if m.db == nil || st == nil {
		return
	}
	_ = m.db.SetJSON(st.kvKey, st.state)
}

func retireStripeActiveLocked(st *carrierTreeStripe) {
	if st == nil || st.state.Active == nil {
		return
	}
	old := st.state.Active
	now := time.Now().Unix()
	filtered := make([]*FundingTreeRecord, 0, len(st.state.Retired)+1)
	for _, r := range st.state.Retired {
		if r == nil || r.TxID == "" || r.TxID == old.TxID {
			continue
		}
		if r.CreatedAt > 0 && now-r.CreatedAt > retiredFundingTreeMaxAgeSec {
			continue
		}
		filtered = append(filtered, r)
	}
	filtered = append(filtered, old)
	if len(filtered) > maxRetiredFundingTrees {
		filtered = filtered[len(filtered)-maxRetiredFundingTrees:]
	}
	st.state.Retired = filtered
}

func allStripeTreesLocked(st *carrierTreeStripe) []*FundingTreeRecord {
	if st == nil {
		return nil
	}
	out := make([]*FundingTreeRecord, 0, 3+len(st.state.Retired))
	if st.state.Active != nil {
		out = append(out, st.state.Active)
	}
	if st.state.Ahead != nil {
		out = append(out, st.state.Ahead)
	}
	if st.state.Pending != nil {
		out = append(out, st.state.Pending)
	}
	out = append(out, st.state.Retired...)
	return out
}

// WarmStock pre-mints Active (and Ahead) carrier funding trees on stripe 0 on boot so the first
// MintCarrier does not stall waiting for funding-tree construction or mining.
func (m *CarrierTreeManager) WarmStock(ctx context.Context) error {
	if m.port == nil || m.signer == nil {
		return errors.New("discovery: carrier tree requires chain port and signing key")
	}
	protoWallet, err := m.signer.ProtoWallet()
	if err != nil {
		return fmt.Errorf("discovery: init carrier wallet: %w", err)
	}
	if err := m.ensureActiveTree(ctx, &m.stripes[0], protoWallet); err != nil {
		return err
	}
	if err := m.ensureAheadTree(ctx, &m.stripes[0], protoWallet); err != nil {
		log.Printf("discovery: carrier tree WarmStock ahead pre-mint failed: %v", err)
	}
	return nil
}

// OnTxMined asynchronously attaches a verified BUMP to any locally held Active, Ahead, or Retired
// funding tree matching txid across all stripes, so MintCarrier never hits the network on the hot path.
func (m *CarrierTreeManager) OnTxMined(txid string, height uint32, mp *transaction.MerklePath) {
	if m == nil || txid == "" || mp == nil {
		return
	}
	if m.port != nil && m.port.Tracker() != nil {
		txHash, err := chainhash.NewHashFromHex(txid)
		if err != nil {
			return
		}
		root, err := mp.ComputeRoot(txHash)
		if err != nil || root == nil {
			return
		}
		ok, err := m.port.Tracker().IsValidRootForHeight(context.Background(), root, height)
		if err != nil || !ok {
			return
		}
	}
	for i := 0; i < carrierTreeStripeCount; i++ {
		st := &m.stripes[i]
		st.mu.Lock()
		changed := false
		for _, rec := range allStripeTreesLocked(st) {
			if rec == nil || rec.TxID != txid {
				continue
			}
			raw, err := hex.DecodeString(rec.BEEFHex)
			if err != nil {
				continue
			}
			_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
			if err != nil || tx == nil {
				continue
			}
			tx.MerklePath = mp
			upgraded, err := tx.AtomicBEEF(false)
			if err != nil {
				continue
			}
			rec.BEEFHex = hex.EncodeToString(upgraded)
			rec.Mined = true
			changed = true
		}
		if changed {
			m.saveStripeLocked(st)
		}
		st.mu.Unlock()
	}
}

// HandleReorg strips any cached BUMP at block height >= forkHeight from Active, Ahead, and Retired
// funding trees across all stripes so orphaned proofs are never re-emitted on subsequent carriers.
func (m *CarrierTreeManager) HandleReorg(forkHeight uint32) {
	if m == nil || forkHeight == 0 {
		return
	}
	for i := 0; i < carrierTreeStripeCount; i++ {
		st := &m.stripes[i]
		st.mu.Lock()
		changed := false
		for _, rec := range allStripeTreesLocked(st) {
			if rec == nil || rec.BEEFHex == "" {
				continue
			}
			raw, err := hex.DecodeString(rec.BEEFHex)
			if err != nil {
				continue
			}
			_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
			if err != nil || tx == nil || tx.MerklePath == nil {
				continue
			}
			if tx.MerklePath.BlockHeight >= forkHeight {
				tx.MerklePath = nil
				if downgraded, dErr := tx.AtomicBEEF(false); dErr == nil {
					rec.BEEFHex = hex.EncodeToString(downgraded)
				}
				rec.Mined = false
				changed = true
			}
		}
		if changed {
			m.saveStripeLocked(st)
		}
		st.mu.Unlock()
	}
}

// CarrierTree returns (lazily initializing) the Service's persistent 4x64 funding tree manager.
func (s *Service) CarrierTree(signer *nwallet.MarketSigner) *CarrierTreeManager {
	if signer == nil {
		signer = s.signer
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.carrierTree == nil {
		var port *chain.Port
		var db *store.DB
		if s.mem != nil {
			port = s.mem.Chain()
			db = s.mem.DB()
		}
		s.carrierTree = NewCarrierTreeManager(port, db, signer)
	}
	return s.carrierTree
}

// State returns a snapshot of stripe 0's active & ahead funding trees (for diagnostics & tests).
func (m *CarrierTreeManager) State() FundingTreeState {
	m.stripes[0].mu.Lock()
	defer m.stripes[0].mu.Unlock()
	return m.stripes[0].state
}
