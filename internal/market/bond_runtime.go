package market

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

const checkpointBondKVKey = "checkpoint_bond_utxo"

var checkpointBondMu sync.Mutex

// LiveBondRecord persists this node's active NektarCheckpointBond singleton UTXO in SQLite KV.
type LiveBondRecord struct {
	TxID      string                        `json:"txid"`
	Vout      uint32                        `json:"vout"`
	Satoshis  uint64                        `json:"satoshis"`
	BEEFHex   string                        `json:"beefHex"`
	State     contracts.CheckpointBondState `json:"state"`
	UpdatedAt int64                         `json:"updatedAt"`
}

// Outpoint returns canonical "txid:vout" notation for OfferAd / AnnounceAd advertisement.
func (r LiveBondRecord) Outpoint() string {
	if r.TxID == "" {
		return ""
	}
	return fmt.Sprintf("%s:%d", r.TxID, r.Vout)
}

// LoadActiveBond loads the persisted NektarCheckpointBond record and parses its backing tx.
func LoadActiveBond(db *store.DB) (*LiveBondRecord, *transaction.Transaction, error) {
	if db == nil {
		return nil, nil, nil
	}
	var rec LiveBondRecord
	ok, err := db.GetJSON(checkpointBondKVKey, &rec)
	if err != nil || !ok || rec.TxID == "" {
		return nil, nil, err
	}
	beef, err := hex.DecodeString(rec.BEEFHex)
	if err != nil || len(beef) == 0 {
		return &rec, nil, fmt.Errorf("market: corrupt bond beefHex for %s", rec.TxID)
	}
	_, tx, _, err := guard.ParseBEEF(beef, guard.DefaultBound)
	if err != nil || tx == nil {
		return &rec, nil, fmt.Errorf("market: parse bond AtomicBEEF: %w", err)
	}
	if int(rec.Vout) >= len(tx.Outputs) || tx.Outputs[rec.Vout] == nil || tx.Outputs[rec.Vout].LockingScript == nil {
		return &rec, nil, fmt.Errorf("market: bond tx %s missing output %d", rec.TxID, rec.Vout)
	}
	if decoded, dErr := contracts.DecodeCheckpointBond(*tx.Outputs[rec.Vout].LockingScript); dErr == nil && decoded != nil {
		rec.State = *decoded
		rec.Satoshis = tx.Outputs[rec.Vout].Satoshis
	}
	return &rec, tx, nil
}

// SaveActiveBond persists the active NektarCheckpointBond pointer in SQLite KV.
func SaveActiveBond(db *store.DB, rec LiveBondRecord) error {
	if db == nil {
		return errors.New("market: nil store DB")
	}
	rec.UpdatedAt = time.Now().Unix()
	return db.SetJSON(checkpointBondKVKey, rec)
}

// ClearActiveBond removes the active bond pointer after Close() or Seize().
func ClearActiveBond(db *store.DB) error {
	if db == nil {
		return nil
	}
	return db.SetJSON(checkpointBondKVKey, LiveBondRecord{})
}

// OpenCheckpointBond locks depositSats into a slashable on-chain NektarCheckpointBond UTXO
// bound to this node's BRC-43 FundPKH with epochLen blocks per checkpoint and releaseBlocks total term.
func OpenCheckpointBond(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	depositSats uint64,
	epochLen uint64,
	releaseBlocks uint64,
	maxFee uint64,
) (*LiveBondRecord, error) {
	if db == nil || port == nil {
		return nil, errors.New("market: OpenCheckpointBond requires db and chain port")
	}
	if depositSats < 1000 {
		return nil, errors.New("market: bond deposit must be >= 1000 sats")
	}
	if epochLen == 0 {
		epochLen = 144
	}
	if releaseBlocks < epochLen {
		releaseBlocks = epochLen * 2
	}
	if maxFee == 0 {
		maxFee = 500
	}
	opPKH, err := port.FundPKH(ctx)
	if err != nil {
		return nil, fmt.Errorf("market: derive bond operator PKH: %w", err)
	}
	curH, err := port.Height(ctx)
	if err != nil {
		return nil, fmt.Errorf("market: read chain height for bond open: %w", err)
	}
	st := contracts.CheckpointBondState{
		OperatorPKH:       opPKH,
		BondReleaseHeight: uint64(curH) + releaseBlocks,
		MaxFee:            maxFee,
		EpochLen:          epochLen,
		N:                 0,
		LastRoot:          nil,
		Deadline:          uint64(curH) + epochLen,
		Seized:            false,
	}
	lockScript, err := contracts.EncodeCheckpointBondLockingScript(st)
	if err != nil {
		return nil, err
	}
	tx, beef, fee, spend, err := port.BuildFundedTxWithBEEF(ctx, []*transaction.TransactionOutput{{
		Satoshis:      depositSats,
		LockingScript: lockScript,
	}})
	if err != nil {
		return nil, fmt.Errorf("market: build bond open tx: %w", err)
	}
	txid, err := port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		return nil, fmt.Errorf("market: broadcast bond open tx: %w", err)
	}
	_ = port.AdoptChange(tx)
	if fresh, bErr := chain.SafeAtomicBEEF(tx); bErr == nil && len(fresh) > 0 {
		beef = fresh
	}
	rec := LiveBondRecord{
		TxID:      txid,
		Vout:      0,
		Satoshis:  depositSats,
		BEEFHex:   hex.EncodeToString(beef),
		State:     st,
		UpdatedAt: time.Now().Unix(),
	}
	checkpointBondMu.Lock()
	err = SaveActiveBond(db, rec)
	checkpointBondMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// CommitActiveBond submits an on-chain NektarCheckpointBond.Commit() tx proving liveness and
// rolling Deadline forward by EpochLen before the bond becomes slashable.
func CommitActiveBond(ctx context.Context, db *store.DB, port *chain.Port, newRoot []byte) (*LiveBondRecord, error) {
	if db == nil || port == nil {
		return nil, errors.New("market: CommitActiveBond requires db and chain port")
	}
	checkpointBondMu.Lock()
	defer checkpointBondMu.Unlock()

	rec, prevTx, err := LoadActiveBond(db)
	if err != nil {
		return nil, err
	}
	if rec == nil || prevTx == nil {
		return nil, errors.New("market: no active NektarCheckpointBond deployed")
	}
	curH, err := port.Height(ctx)
	if err != nil {
		return nil, err
	}
	if len(newRoot) == 0 {
		newRoot = core.SHA256(core.Concat([]byte("nektar:bond-checkpoint:v1"), core.U64LE(uint64(curH)), core.U64LE(rec.State.N+1)))
	}
	commitTx, nextState, err := contracts.BuildBondCommitTx(ctx, prevTx, rec.Vout, newRoot, curH, 0, port.SignCovenantSighash)
	if err != nil {
		return nil, err
	}
	txid, err := port.SubmitCovenantTx(ctx, commitTx)
	if err != nil {
		return nil, err
	}
	beef, err := chain.SafeAtomicBEEF(commitTx)
	if err != nil {
		return nil, err
	}
	updated := LiveBondRecord{
		TxID:      txid,
		Vout:      0,
		Satoshis:  commitTx.Outputs[0].Satoshis,
		BEEFHex:   hex.EncodeToString(beef),
		State:     *nextState,
		UpdatedAt: time.Now().Unix(),
	}
	if err := SaveActiveBond(db, updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// SeizePeerBond executes a permissionless NektarCheckpointBond.Seize() slash on any delinquent
// peer/hub's expired bond BEEF, routing slashed satoshis to payToPKH (defaults to port.FundPKH).
func SeizePeerBond(ctx context.Context, port *chain.Port, bondBEEF []byte, vout uint32, payToPKH []byte) (string, uint64, error) {
	if port == nil {
		return "", 0, errors.New("market: SeizePeerBond requires chain port")
	}
	if len(payToPKH) == 0 {
		var err error
		payToPKH, err = port.FundPKH(ctx)
		if err != nil {
			return "", 0, err
		}
	}
	payToLock, err := contracts.P2PKHLockingScript(payToPKH)
	if err != nil {
		return "", 0, err
	}
	_, bondTx, _, err := guard.ParseBEEF(bondBEEF, guard.DefaultBound)
	if err != nil || bondTx == nil {
		return "", 0, fmt.Errorf("market: parse target bond BEEF: %w", err)
	}
	curH, err := port.Height(ctx)
	if err != nil {
		return "", 0, err
	}
	seizeTx, _, err := contracts.BuildBondSeizeTx(bondTx, vout, payToLock, curH, 0)
	if err != nil {
		return "", 0, err
	}
	txid, err := port.SubmitCovenantTx(ctx, seizeTx)
	if err != nil {
		return "", 0, err
	}
	_, _ = port.AdoptReceived(ctx, seizeTx)
	return txid, seizeTx.Outputs[0].Satoshis, nil
}

// CloseActiveBond reclaims the operator's un-seized bond back to OperatorPKH once BondReleaseHeight is reached.
func CloseActiveBond(ctx context.Context, db *store.DB, port *chain.Port) (string, uint64, error) {
	if db == nil || port == nil {
		return "", 0, errors.New("market: CloseActiveBond requires db and chain port")
	}
	checkpointBondMu.Lock()
	defer checkpointBondMu.Unlock()

	rec, prevTx, err := LoadActiveBond(db)
	if err != nil {
		return "", 0, err
	}
	if rec == nil || prevTx == nil {
		return "", 0, errors.New("market: no active NektarCheckpointBond deployed")
	}
	curH, err := port.Height(ctx)
	if err != nil {
		return "", 0, err
	}
	closeTx, err := contracts.BuildBondCloseTx(ctx, prevTx, rec.Vout, curH, 0, port.SignCovenantSighash)
	if err != nil {
		return "", 0, err
	}
	txid, err := port.SubmitCovenantTx(ctx, closeTx)
	if err != nil {
		return "", 0, err
	}
	_, _ = port.AdoptReceived(ctx, closeTx)
	_ = ClearActiveBond(db)
	return txid, closeTx.Outputs[0].Satoshis, nil
}

// VerifyOfferBondSats SPV-verifies an OfferAd's advertised NektarCheckpointBond (BondOutpoint + BondBEEFHex),
// binding OperatorPKH to the offer's PayAddress (preventing stolen/copied bond spoofing) and checking
// that the bond is active (!Seized), un-expired, and valid against port's ChainTracker!
// Returns verified bond satoshis (0 on any missing, invalid, seized, expired, or unowned bond).
func VerifyOfferBondSats(ctx context.Context, offer bstack.OfferAd, port *chain.Port) uint64 {
	if strings.TrimSpace(offer.BondOutpoint) == "" || strings.TrimSpace(offer.BondBEEFHex) == "" {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(offer.BondOutpoint), ":")
	if len(parts) != 2 || len(parts[0]) != 64 {
		return 0
	}
	vout64, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		return 0
	}
	beef, err := hex.DecodeString(strings.TrimSpace(offer.BondBEEFHex))
	if err != nil || len(beef) == 0 {
		return 0
	}
	var expectedOpPKH []byte
	if offer.PayAddress != "" {
		if addr, aErr := script.NewAddressFromString(strings.TrimSpace(offer.PayAddress)); aErr == nil && len(addr.PublicKeyHash) == 20 {
			expectedOpPKH = append([]byte(nil), addr.PublicKeyHash...)
		}
	}
	checkHeaders := port != nil && port.Network() != "memory"
	if port == nil {
		vr := contracts.VerifyCheckpointBondBEEF(ctx, beef, uint32(vout64), expectedOpPKH, nil, false)
		if !vr.OK || !strings.EqualFold(vr.TxID, parts[0]) {
			return 0
		}
		return vr.BondSats
	}
	vr := contracts.VerifyCheckpointBondBEEF(ctx, beef, uint32(vout64), expectedOpPKH, port.Tracker(), checkHeaders)
	if !vr.OK || !strings.EqualFold(vr.TxID, parts[0]) {
		return 0
	}
	// In memory mode also enforce deadline check against local tip height.
	if !checkHeaders && vr.State != nil {
		if tip, tErr := port.Height(ctx); tErr == nil && uint64(tip) >= vr.State.Deadline {
			return 0
		}
	}
	return vr.BondSats
}
