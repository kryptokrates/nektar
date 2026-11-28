package discovery

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const (
	// DefaultFundingTreeWidth matches 1BSV Paper 011 §9 (16 equal-valued outputs per mined tree:
	// 1 miner fee funds 16 carriers, and every carrier inherits the tree's mined BUMP).
	DefaultFundingTreeWidth = 16
	// DefaultFundingTreeAheadThreshold pre-mints the next 16-output tree when <=8 outputs remain
	// so the Ahead tree has ample time to mine before Active exhausts.
	DefaultFundingTreeAheadThreshold = DefaultFundingTreeWidth / 2
	maxRetiredFundingTrees           = 8
	retiredFundingTreeMaxAgeSec      = 72 * 3600
	carrierTreeKVKey                 = "carrier_funding_tree_v1"
)

// FundingTreeRecord persists one 16-output mined carrier funding tree and its allocation cursor
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

// CarrierTreeManager manages M=16 carrier funding trees with background Ahead pre-minting and
// mined kill-switch sweeps (carrier.Sweep) for cryptographic ad retraction.
type CarrierTreeManager struct {
	mu        sync.Mutex
	mintMu    sync.Mutex
	port      *chain.Port
	db        *store.DB
	signer    *nwallet.MarketSigner
	width     int
	aheadAt   int
	state     FundingTreeState
	minting   bool
	stopCh    chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup
}

// NewCarrierTreeManager opens or initializes a persistent M=16 carrier funding tree manager.
func NewCarrierTreeManager(port *chain.Port, db *store.DB, signer *nwallet.MarketSigner) *CarrierTreeManager {
	m := &CarrierTreeManager{
		port:    port,
		db:      db,
		signer:  signer,
		width:   DefaultFundingTreeWidth,
		aheadAt: DefaultFundingTreeAheadThreshold,
		stopCh:  make(chan struct{}),
	}
	if db != nil {
		_, _ = db.GetJSON(carrierTreeKVKey, &m.state)
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

func (m *CarrierTreeManager) saveLocked() {
	if m.db == nil {
		return
	}
	_ = m.db.SetJSON(carrierTreeKVKey, m.state)
}

func (m *CarrierTreeManager) retireActiveLocked() {
	if m.state.Active == nil {
		return
	}
	old := m.state.Active
	now := time.Now().Unix()
	filtered := make([]*FundingTreeRecord, 0, len(m.state.Retired)+1)
	for _, r := range m.state.Retired {
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
	m.state.Retired = filtered
}

func (m *CarrierTreeManager) allTreesLocked() []*FundingTreeRecord {
	out := make([]*FundingTreeRecord, 0, 3+len(m.state.Retired))
	if m.state.Active != nil {
		out = append(out, m.state.Active)
	}
	if m.state.Ahead != nil {
		out = append(out, m.state.Ahead)
	}
	if m.state.Pending != nil {
		out = append(out, m.state.Pending)
	}
	out = append(out, m.state.Retired...)
	return out
}

// WarmStock pre-mints Active (and Ahead) carrier funding trees on boot so the first MintCarrier
// does not stall waiting for funding-tree construction or mining.
func (m *CarrierTreeManager) WarmStock(ctx context.Context) error {
	if m.port == nil || m.signer == nil {
		return errors.New("discovery: carrier tree requires chain port and signing key")
	}
	protoWallet, err := m.signer.ProtoWallet()
	if err != nil {
		return fmt.Errorf("discovery: init carrier wallet: %w", err)
	}
	if err := m.ensureActiveTree(ctx, protoWallet); err != nil {
		return err
	}
	if err := m.ensureAheadTree(ctx, protoWallet); err != nil {
		log.Printf("discovery: carrier tree WarmStock ahead pre-mint failed: %v", err)
	}
	return nil
}

// OnTxMined asynchronously attaches a verified BUMP to any locally held Active, Ahead, or Retired
// funding tree matching txid, so MintCarrier never hits the network on the hot path.
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
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for _, rec := range m.allTreesLocked() {
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
		m.saveLocked()
	}
}

// HandleReorg strips any cached BUMP at block height >= forkHeight from Active, Ahead, and Retired
// funding trees so orphaned proofs are never re-emitted on subsequent carriers.
func (m *CarrierTreeManager) HandleReorg(forkHeight uint32) {
	if m == nil || forkHeight == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	changed := false
	for _, rec := range m.allTreesLocked() {
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
		m.saveLocked()
	}
}

// CarrierTree returns (lazily initializing) the Service's persistent M=16 funding tree manager.
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

// State returns a snapshot of the active & ahead M=16 funding trees (for diagnostics & tests).
func (m *CarrierTreeManager) State() FundingTreeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}
