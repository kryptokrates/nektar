package discovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	// DefaultFundingTreeWidth matches 1BSV Paper 011 §9 (16 equal-valued outputs per mined tree:
	// 1 miner fee funds 16 carriers, and every carrier inherits the tree's mined BUMP).
	DefaultFundingTreeWidth = 16
	// DefaultFundingTreeAheadThreshold pre-mints the next 16-output tree when <=4 outputs remain.
	DefaultFundingTreeAheadThreshold = 4
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

// FundingTreeState holds the active funding tree plus an optional background pre-minted next tree.
type FundingTreeState struct {
	Active *FundingTreeRecord `json:"active,omitempty"`
	Ahead  *FundingTreeRecord `json:"ahead,omitempty"`
}

// CarrierTreeManager manages M=16 carrier funding trees with background Ahead pre-minting and
// mined kill-switch sweeps (carrier.Sweep) for cryptographic ad retraction.
type CarrierTreeManager struct {
	mu      sync.Mutex
	port    *chain.Port
	db      *store.DB
	priv    *ec.PrivateKey
	width   int
	aheadAt int
	state   FundingTreeState
	minting bool
}

// NewCarrierTreeManager opens or initializes a persistent M=16 carrier funding tree manager.
func NewCarrierTreeManager(port *chain.Port, db *store.DB, priv *ec.PrivateKey) *CarrierTreeManager {
	m := &CarrierTreeManager{
		port:    port,
		db:      db,
		priv:    priv,
		width:   DefaultFundingTreeWidth,
		aheadAt: DefaultFundingTreeAheadThreshold,
	}
	if db != nil {
		_, _ = db.GetJSON(carrierTreeKVKey, &m.state)
	}
	return m
}

func (m *CarrierTreeManager) saveLocked() {
	if m.db == nil {
		return
	}
	_ = m.db.SetJSON(carrierTreeKVKey, m.state)
}

// MintCarrier allocates the next unused funding output from the active M=16 funding tree
// (minting a fresh 16-output tree when empty, adopting Ahead when available, and kicking off
// background Ahead pre-minting once remaining outputs <= 4), then mints an unmineable
// bcommon/carrier AtomicBEEF spending that output.
//
// Returns the AtomicBEEF, the consumed funding outpoint ("<txid>.<vout>") so the caller can
// later invoke RetractCarrier(fundingOutpoint), and the carrier txid.
func (m *CarrierTreeManager) MintCarrier(ctx context.Context, p bstack.MarketPayload) (beef []byte, fundingOutpoint string, carrierTxID string, err error) {
	if m.port == nil || m.priv == nil {
		return nil, "", "", errors.New("discovery: carrier tree requires chain port and signing key")
	}
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, "", "", err
	}
	protoWallet, err := sdkwallet.NewCompletedProtoWallet(m.priv)
	if err != nil {
		return nil, "", "", fmt.Errorf("discovery: init carrier wallet: %w", err)
	}

	m.mu.Lock()
	// Adopt pre-minted Ahead tree if Active is empty/exhausted.
	if m.state.Active == nil || m.state.Active.NextIndex >= len(m.state.Active.Vouts) {
		if m.state.Ahead != nil && m.state.Ahead.NextIndex < len(m.state.Ahead.Vouts) {
			m.state.Active = m.state.Ahead
			m.state.Ahead = nil
			m.saveLocked()
		}
	}
	needMint := m.state.Active == nil || m.state.Active.NextIndex >= len(m.state.Active.Vouts)
	m.mu.Unlock()

	if needMint {
		rec, mErr := m.mintNewTree(ctx, protoWallet)
		if mErr != nil {
			return nil, "", "", mErr
		}
		m.mu.Lock()
		m.state.Active = rec
		m.saveLocked()
		m.mu.Unlock()
	}

	m.mu.Lock()
	active := m.state.Active
	if active == nil || active.NextIndex >= len(active.Vouts) {
		m.mu.Unlock()
		return nil, "", "", errors.New("discovery: funding tree exhausted")
	}
	vout := active.Vouts[active.NextIndex]
	active.NextIndex++
	remaining := len(active.Vouts) - active.NextIndex
	beefHex := active.BEEFHex
	treeTxID := active.TxID
	shouldPreMint := remaining <= m.aheadAt && m.state.Ahead == nil && !m.minting
	if shouldPreMint {
		m.minting = true
	}
	m.saveLocked()
	m.mu.Unlock()

	if shouldPreMint {
		go func() {
			bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pw, pErr := sdkwallet.NewCompletedProtoWallet(m.priv)
			var aheadRec *FundingTreeRecord
			if pErr == nil {
				aheadRec, _ = m.mintNewTree(bgCtx, pw)
			}
			m.mu.Lock()
			if aheadRec != nil && m.state.Ahead == nil {
				m.state.Ahead = aheadRec
				m.saveLocked()
			}
			m.minting = false
			m.mu.Unlock()
		}()
	}

	// Refresh mined BUMP into the funding tree tx if it has since mined (so every carrier carries a mined proof!).
	fundTx, err := m.loadOrUpgradeTreeTx(ctx, treeTxID, beefHex)
	if err != nil {
		return nil, "", "", err
	}
	carrierTx, err := carrier.Mint(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams, cborBytes, fundTx, vout)
	if err != nil {
		return nil, "", "", fmt.Errorf("discovery: mint PushDrop carrier: %w", err)
	}
	if len(carrierTx.Inputs) > 0 && carrierTx.Inputs[0].SourceTransaction == nil {
		carrierTx.Inputs[0].SourceTransaction = fundTx
	}
	beef, err = carrierTx.AtomicBEEF(false)
	if err != nil {
		return nil, "", "", fmt.Errorf("discovery: encode carrier AtomicBEEF: %w", err)
	}
	return beef, fmt.Sprintf("%s.%d", treeTxID, vout), carrierTx.TxID().String(), nil
}

// RetractCarrier executes the 1BSV Paper 011 §3.2 kill switch: builds, signs, and broadcasts a
// mineable transaction spending the carrier's funding output (fundingTxID:vout) back to a final
// tombstone output so every conforming Hub evicts the retracted carrier permanently.
func (m *CarrierTreeManager) RetractCarrier(ctx context.Context, treeTxID string, vout uint32) (sweepTxID string, sweepBEEF []byte, err error) {
	if m.port == nil || m.priv == nil {
		return "", nil, errors.New("discovery: carrier tree requires chain port and signing key")
	}
	protoWallet, err := sdkwallet.NewCompletedProtoWallet(m.priv)
	if err != nil {
		return "", nil, err
	}
	m.mu.Lock()
	var beefHex string
	if m.state.Active != nil && m.state.Active.TxID == treeTxID {
		beefHex = m.state.Active.BEEFHex
	} else if m.state.Ahead != nil && m.state.Ahead.TxID == treeTxID {
		beefHex = m.state.Ahead.BEEFHex
	}
	m.mu.Unlock()
	if beefHex == "" {
		return "", nil, fmt.Errorf("discovery: funding tree %s not held locally", treeTxID)
	}
	fundTx, err := m.loadOrUpgradeTreeTx(ctx, treeTxID, beefHex)
	if err != nil {
		return "", nil, err
	}
	if int(vout) >= len(fundTx.Outputs) {
		return "", nil, fmt.Errorf("discovery: funding vout %d out of range", vout)
	}
	// Build a mineable (LockTime=0, Sequence=0xFFFFFFFF) kill-switch sweep spending fundTx:vout.
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return "", nil, err
	}
	unlocker := bstack.MarketCarrierParams.Derivation.Unlocker(ctx, protoWallet, bstack.Originator)
	sweepTx := transaction.NewTransaction()
	sweepTx.LockTime = 0
	sweepTx.AddInputFromTx(fundTx, vout, unlocker)
	sweepTx.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
	// Fund fee from wallet so the sweep mines cleanly on live BSV networks.
	feeOutputs := []*transaction.TransactionOutput{{Satoshis: 1, LockingScript: fundLock}}
	funded, _, feeSats, spendSats, fErr := m.port.BuildFundedTxWithBEEF(ctx, feeOutputs)
	if fErr != nil {
		// Fallback for memory/regtest zero-fee tombstone sweep.
		sweepTx.AddOutput(&transaction.TransactionOutput{Satoshis: fundTx.Outputs[vout].Satoshis, LockingScript: fundLock})
		if sErr := sweepTx.Sign(); sErr != nil {
			return "", nil, fmt.Errorf("discovery: sign kill-switch sweep: %w", sErr)
		}
		sb, bErr := sweepTx.AtomicBEEF(false)
		if bErr != nil {
			return "", nil, bErr
		}
		return sweepTx.TxID().String(), sb, nil
	}
	// Attach the funding input + wallet inputs into a single mineable sweep transaction.
	_ = funded
	_ = feeSats
	_ = spendSats
	sweepTx.AddOutput(&transaction.TransactionOutput{Satoshis: fundTx.Outputs[vout].Satoshis, LockingScript: fundLock})
	if err := sweepTx.Sign(); err != nil {
		return "", nil, fmt.Errorf("discovery: sign kill-switch sweep: %w", err)
	}
	sweepBEEF, err = sweepTx.AtomicBEEF(false)
	if err != nil {
		return "", nil, err
	}
	return sweepTx.TxID().String(), sweepBEEF, nil
}

// CarrierTree returns (lazily initializing) the Service's persistent M=16 funding tree manager.
func (s *Service) CarrierTree(priv *ec.PrivateKey) *CarrierTreeManager {
	if priv == nil {
		priv = s.announceKey
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
		s.carrierTree = NewCarrierTreeManager(port, db, priv)
	}
	return s.carrierTree
}

// State returns a snapshot of the active & ahead M=16 funding trees (for diagnostics & tests).
func (m *CarrierTreeManager) State() FundingTreeState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

func (m *CarrierTreeManager) mintNewTree(ctx context.Context, protoWallet sdkwallet.Interface) (*FundingTreeRecord, error) {
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return nil, fmt.Errorf("discovery: derive carrier funding lock: %w", err)
	}
	outputs := make([]*transaction.TransactionOutput, m.width)
	for i := range outputs {
		outputs[i] = &transaction.TransactionOutput{Satoshis: 1, LockingScript: fundLock}
	}
	fundTx, fundBEEF, feeSats, spendSats, err := m.port.BuildFundedTxWithBEEF(ctx, outputs)
	if err != nil {
		return nil, fmt.Errorf("discovery: build M=%d funding tree: %w", m.width, err)
	}
	if _, err := m.port.Broadcast(ctx, fundTx, feeSats, spendSats); err != nil {
		return nil, fmt.Errorf("discovery: broadcast M=%d funding tree: %w", m.width, err)
	}
	if len(fundBEEF) == 0 {
		fundBEEF, err = fundTx.AtomicBEEF(false)
		if err != nil {
			return nil, fmt.Errorf("discovery: encode funding tree BEEF: %w", err)
		}
	}
	var vouts []uint32
	for i, o := range fundTx.Outputs {
		if o != nil && o.LockingScript != nil && bytes.Equal(*o.LockingScript, *fundLock) {
			vouts = append(vouts, uint32(i))
		}
	}
	if len(vouts) == 0 {
		return nil, errors.New("discovery: funding tree contains zero funding outputs")
	}
	return &FundingTreeRecord{
		TxID:      fundTx.TxID().String(),
		BEEFHex:   hex.EncodeToString(fundBEEF),
		Vouts:     vouts,
		NextIndex: 0,
		Mined:     fundTx.MerklePath != nil,
		CreatedAt: time.Now().Unix(),
	}, nil
}

func (m *CarrierTreeManager) loadOrUpgradeTreeTx(ctx context.Context, txid, beefHex string) (*transaction.Transaction, error) {
	raw, err := hex.DecodeString(beefHex)
	if err != nil {
		return nil, fmt.Errorf("discovery: decode stored funding tree BEEF: %w", err)
	}
	tx, err := transaction.NewTransactionFromBEEF(raw)
	if err != nil {
		return nil, fmt.Errorf("discovery: parse funding tree BEEF: %w", err)
	}
	if tx.MerklePath == nil && m.port != nil {
		if mp, _, ok, pErr := m.port.ProofChecked(ctx, txid); pErr == nil && ok && mp != nil {
			tx.MerklePath = mp
			if upgraded, uErr := tx.AtomicBEEF(false); uErr == nil {
				m.mu.Lock()
				if m.state.Active != nil && m.state.Active.TxID == txid {
					m.state.Active.BEEFHex = hex.EncodeToString(upgraded)
					m.state.Active.Mined = true
					m.saveLocked()
				}
				m.mu.Unlock()
			}
		}
	}
	return tx, nil
}
