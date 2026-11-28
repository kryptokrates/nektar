package discovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/carrier"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func (m *CarrierTreeManager) pickStripe() *carrierTreeStripe {
	// Prefer any stripe that already has ready slots before minting a new stripe tree.
	start := int((m.stripeRR.Add(1) - 1) % carrierTreeStripeCount)
	for step := 0; step < carrierTreeStripeCount; step++ {
		st := &m.stripes[(start+step)%carrierTreeStripeCount]
		st.mu.Lock()
		hasReady := (st.state.Active != nil && st.state.Active.NextIndex < len(st.state.Active.Vouts)) ||
			(st.state.Ahead != nil && st.state.Ahead.NextIndex < len(st.state.Ahead.Vouts))
		st.mu.Unlock()
		if hasReady {
			return st
		}
	}
	return &m.stripes[start]
}

// MintCarrier allocates the next unused funding output from a striped M=64 funding tree
// (minting a fresh tree when empty, adopting Ahead when available, and kicking off background
// Ahead pre-minting once remaining outputs <= aheadAt), then mints an unmineable bcommon/carrier
// AtomicBEEF spending that output.
func (m *CarrierTreeManager) MintCarrier(ctx context.Context, p bstack.MarketPayload) (beef []byte, fundingOutpoint string, carrierTxID string, err error) {
	if m.port == nil || m.signer == nil {
		return nil, "", "", errors.New("discovery: carrier tree requires chain port and signing key")
	}
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, "", "", err
	}
	protoWallet, err := m.signer.ProtoWallet()
	if err != nil {
		return nil, "", "", fmt.Errorf("discovery: init carrier wallet: %w", err)
	}

	st := m.pickStripe()
	var (
		vout          uint32
		beefHex       string
		treeTxID      string
		shouldPreMint bool
	)
	for attempt := 0; attempt < 4; attempt++ {
		st.mu.Lock()
		if st.state.Active == nil || st.state.Active.NextIndex >= len(st.state.Active.Vouts) {
			if st.state.Ahead != nil && st.state.Ahead.NextIndex < len(st.state.Ahead.Vouts) {
				retireStripeActiveLocked(st)
				st.state.Active = st.state.Ahead
				st.state.Ahead = nil
				m.saveStripeLocked(st)
			}
		}
		if st.state.Active != nil && st.state.Active.NextIndex < len(st.state.Active.Vouts) {
			active := st.state.Active
			vout = active.Vouts[active.NextIndex]
			active.NextIndex++
			remaining := len(active.Vouts) - active.NextIndex
			beefHex = active.BEEFHex
			treeTxID = active.TxID
			aheadThresh := max(2, len(active.Vouts)/4)
			if m.aheadAt > 0 && m.aheadAt < len(active.Vouts) {
				aheadThresh = m.aheadAt
			}
			shouldPreMint = remaining <= aheadThresh && st.state.Ahead == nil && !st.minting
			if shouldPreMint {
				st.minting = true
			}
			m.saveStripeLocked(st)
			st.mu.Unlock()
			break
		}
		st.mu.Unlock()

		if mErr := m.ensureActiveTree(ctx, st, protoWallet); mErr != nil {
			return nil, "", "", mErr
		}
	}
	if treeTxID == "" {
		return nil, "", "", errors.New("discovery: funding tree exhausted")
	}

	if shouldPreMint {
		stripeRef := st
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			bgCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			go func() {
				select {
				case <-m.stopCh:
					cancel()
				case <-bgCtx.Done():
				}
			}()
			pw, pErr := m.signer.ProtoWallet()
			if pErr != nil {
				log.Printf("discovery: carrier tree ahead wallet init failed: %v", pErr)
			} else if mErr := m.ensureAheadTree(bgCtx, stripeRef, pw); mErr != nil {
				log.Printf("discovery: carrier tree ahead pre-mint failed: %v", mErr)
			}
			stripeRef.mu.Lock()
			stripeRef.minting = false
			stripeRef.mu.Unlock()
		}()
	}

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

// RetractCarrier executes the 1BSV Paper 011 §3.2 kill switch across any stripe holding treeTxID.
func (m *CarrierTreeManager) RetractCarrier(ctx context.Context, treeTxID string, vout uint32) (sweepTxID string, sweepBEEF []byte, err error) {
	if m.port == nil || m.signer == nil {
		return "", nil, errors.New("discovery: carrier tree requires chain port and signing key")
	}
	protoWallet, err := m.signer.ProtoWallet()
	if err != nil {
		return "", nil, err
	}
	var beefHex string
	for i := 0; i < carrierTreeStripeCount && beefHex == ""; i++ {
		st := &m.stripes[i]
		st.mu.Lock()
		for _, rec := range allStripeTreesLocked(st) {
			if rec != nil && rec.TxID == treeTxID {
				beefHex = rec.BEEFHex
				break
			}
		}
		st.mu.Unlock()
	}
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
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return "", nil, err
	}
	unlocker := bstack.MarketCarrierParams.Derivation.Unlocker(ctx, protoWallet, bstack.Originator)
	sweepTx := transaction.NewTransaction()
	sweepTx.LockTime = 0
	sweepTx.AddInputFromTx(fundTx, vout, unlocker)
	sweepTx.Inputs[0].SequenceNumber = transaction.MaxTxInSequenceNum
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

func (m *CarrierTreeManager) ensureActiveTree(ctx context.Context, st *carrierTreeStripe, protoWallet sdkwallet.Interface) error {
	st.mintMu.Lock()
	defer st.mintMu.Unlock()

	st.mu.Lock()
	if st.state.Active != nil && st.state.Active.NextIndex < len(st.state.Active.Vouts) {
		st.mu.Unlock()
		return nil
	}
	if st.state.Ahead != nil && st.state.Ahead.NextIndex < len(st.state.Ahead.Vouts) {
		retireStripeActiveLocked(st)
		st.state.Active = st.state.Ahead
		st.state.Ahead = nil
		m.saveStripeLocked(st)
		st.mu.Unlock()
		return nil
	}
	pending := st.state.Pending
	st.mu.Unlock()

	if pending != nil {
		if err := m.reconcilePendingTree(ctx, st, pending, false); err == nil {
			return nil
		}
	}
	rec, err := m.mintNewTree(ctx, st, protoWallet)
	if err != nil {
		return err
	}
	st.mu.Lock()
	retireStripeActiveLocked(st)
	st.state.Active = rec
	st.state.Pending = nil
	m.saveStripeLocked(st)
	st.mu.Unlock()
	return nil
}

func (m *CarrierTreeManager) ensureAheadTree(ctx context.Context, st *carrierTreeStripe, protoWallet sdkwallet.Interface) error {
	st.mintMu.Lock()
	defer st.mintMu.Unlock()

	st.mu.Lock()
	if st.state.Ahead != nil {
		st.mu.Unlock()
		return nil
	}
	pending := st.state.Pending
	st.mu.Unlock()

	if pending != nil {
		if err := m.reconcilePendingTree(ctx, st, pending, true); err == nil {
			return nil
		}
	}
	rec, err := m.mintNewTree(ctx, st, protoWallet)
	if err != nil {
		return err
	}
	st.mu.Lock()
	if st.state.Ahead == nil {
		st.state.Ahead = rec
	}
	st.state.Pending = nil
	m.saveStripeLocked(st)
	st.mu.Unlock()
	return nil
}

func (m *CarrierTreeManager) reconcilePendingTree(ctx context.Context, st *carrierTreeStripe, pending *FundingTreeRecord, forAhead bool) error {
	if pending == nil || pending.TxID == "" {
		return errors.New("empty pending tree")
	}
	mined, wStatus, _, _, statusErr := m.port.TxBroadcastStatus(ctx, pending.TxID)
	if statusErr != nil && !mined && wStatus != "nosend" {
		return statusErr
	}
	if !mined {
		if fundTx, err := m.loadOrUpgradeTreeTx(ctx, pending.TxID, pending.BEEFHex); err == nil && fundTx != nil {
			switch wStatus {
			case "nosend":
				if _, bErr := m.port.Broadcast(ctx, fundTx, 0, uint64(len(pending.Vouts))); bErr != nil {
					if !errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
						st.mu.Lock()
						if st.state.Pending != nil && st.state.Pending.TxID == pending.TxID {
							st.state.Pending = nil
							m.saveStripeLocked(st)
						}
						st.mu.Unlock()
					}
					return bErr
				}
			case "submission_unknown", "sending", "unproven":
				_ = m.port.RelayExternalTx(ctx, fundTx)
			}
		}
	} else {
		pending.Mined = true
	}
	st.mu.Lock()
	if forAhead {
		if st.state.Ahead == nil {
			st.state.Ahead = pending
		}
	} else {
		retireStripeActiveLocked(st)
		st.state.Active = pending
	}
	st.state.Pending = nil
	m.saveStripeLocked(st)
	st.mu.Unlock()
	return nil
}

func (m *CarrierTreeManager) mintNewTree(ctx context.Context, st *carrierTreeStripe, protoWallet sdkwallet.Interface) (*FundingTreeRecord, error) {
	fundLock, err := carrier.FundingLock(ctx, protoWallet, bstack.Originator, bstack.MarketCarrierParams)
	if err != nil {
		return nil, fmt.Errorf("discovery: derive carrier funding lock: %w", err)
	}
	effWidth := m.width
	if effWidth <= 0 {
		effWidth = DefaultFundingTreeWidth
	}
	if bal := m.port.BalanceSats(); bal > 0 && bal < 500 && effWidth > 16 {
		effWidth = 16
	}
	outputs := make([]*transaction.TransactionOutput, effWidth)
	for i := range outputs {
		outputs[i] = &transaction.TransactionOutput{Satoshis: 1, LockingScript: fundLock}
	}
	treeMeta := nwallet.ActionFundCarrierTree(effWidth)
	treeSpec := &nwallet.OutputTagSpec{
		Basket:             nwallet.BasketCarriers,
		Tags:               []string{"carrier", "slot"},
		CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"carrier-slot","width":%d}`, effWidth),
		ApplyToAll:         true,
	}
	fundTx, fundBEEF, feeSats, spendSats, err := m.port.BuildFundedTxWithBasket(ctx, outputs, treeMeta.Description, treeMeta.Labels, treeSpec)
	if err != nil {
		return nil, fmt.Errorf("discovery: build M=%d funding tree: %w", effWidth, err)
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
	rec := &FundingTreeRecord{
		TxID:      fundTx.TxID().String(),
		BEEFHex:   hex.EncodeToString(fundBEEF),
		Vouts:     vouts,
		NextIndex: 0,
		CreatedAt: time.Now().Unix(),
	}
	// Persist Pending BEFORE broadcast so a crash or ErrBroadcastUnknown never strands a funded tree.
	st.mu.Lock()
	st.state.Pending = rec
	m.saveStripeLocked(st)
	st.mu.Unlock()

	if _, bErr := m.port.Broadcast(ctx, fundTx, feeSats, spendSats); bErr != nil {
		if !errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
			st.mu.Lock()
			if st.state.Pending != nil && st.state.Pending.TxID == rec.TxID {
				st.state.Pending = nil
				m.saveStripeLocked(st)
			}
			st.mu.Unlock()
		}
		return nil, fmt.Errorf("discovery: broadcast M=%d funding tree: %w", effWidth, bErr)
	}
	if freshBEEF, fErr := fundTx.AtomicBEEF(false); fErr == nil && len(freshBEEF) > 0 {
		rec.BEEFHex = hex.EncodeToString(freshBEEF)
	}
	if fundTx.MerklePath != nil {
		if m.port.Tracker() == nil {
			fundTx.MerklePath = nil
		} else {
			if root, rErr := fundTx.MerklePath.ComputeRoot(fundTx.TxID()); rErr == nil && root != nil {
				if ok, vErr := m.port.Tracker().IsValidRootForHeight(ctx, root, fundTx.MerklePath.BlockHeight); vErr == nil && ok {
					rec.Mined = true
				} else {
					fundTx.MerklePath = nil
				}
			} else {
				fundTx.MerklePath = nil
			}
		}
	}
	return rec, nil
}

func (m *CarrierTreeManager) loadOrUpgradeTreeTx(ctx context.Context, txid, beefHex string) (*transaction.Transaction, error) {
	raw, err := hex.DecodeString(beefHex)
	if err != nil {
		return nil, fmt.Errorf("discovery: decode stored funding tree BEEF: %w", err)
	}
	_, tx, _, err := guard.ParseBEEF(raw, guard.DefaultBound)
	if err != nil || tx == nil {
		if err == nil {
			err = errors.New("empty funding tree transaction")
		}
		return nil, fmt.Errorf("discovery: parse funding tree BEEF: %w", err)
	}
	// Verify any persisted BUMP against witnessed headers; strip unverified/orphaned BUMPs.
	// Mined BUMP attachment happens asynchronously via OnTxMined so MintCarrier never stalls on network I/O.
	if tx.MerklePath != nil && m.port != nil && m.port.Tracker() != nil {
		root, rErr := tx.MerklePath.ComputeRoot(tx.TxID())
		if rErr != nil || root == nil {
			tx.MerklePath = nil
		} else if ok, vErr := m.port.Tracker().IsValidRootForHeight(ctx, root, tx.MerklePath.BlockHeight); vErr == nil && !ok {
			tx.MerklePath = nil
		}
	}
	return tx, nil
}
