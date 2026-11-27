package market

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const agentVaultKVKey = "agent_vault_utxo"

var agentVaultMu sync.Mutex

// LiveVaultRecord persists the current unspent NektarAgentVault singleton UTXO pointer in SQLite KV.
type LiveVaultRecord struct {
	TxID      string                    `json:"txid"`
	Vout      uint32                    `json:"vout"`
	Satoshis  uint64                    `json:"satoshis"`
	BEEFHex   string                    `json:"beefHex"`
	State     contracts.AgentVaultState `json:"state"`
	UpdatedAt int64                     `json:"updatedAt"`
}

// ParsePubKeyOrPKH parses either a 33-byte compressed secp256k1 pubkey hex (returning Hash160)
// or a 20-byte PKH hex directly.
func ParsePubKeyOrPKH(hexStr string) ([]byte, error) {
	clean := strings.TrimPrefix(strings.TrimSpace(hexStr), "0x")
	raw, err := hex.DecodeString(clean)
	if err != nil {
		return nil, fmt.Errorf("market: invalid pubkey/pkh hex: %w", err)
	}
	switch len(raw) {
	case 20:
		return raw, nil
	case 33:
		if _, _, pErr := core.ParseStrictCompressedPubKey(clean); pErr != nil {
			return nil, fmt.Errorf("market: invalid compressed pubkey: %w", pErr)
		}
		return core.Hash160(raw), nil
	default:
		return nil, fmt.Errorf("market: expected 33-byte pubkey hex or 20-byte PKH hex, got %d bytes", len(raw))
	}
}

// LoadActiveVault loads the persisted live NektarAgentVault record and parses its backing transaction.
func LoadActiveVault(db *store.DB) (*LiveVaultRecord, *transaction.Transaction, error) {
	if db == nil {
		return nil, nil, nil
	}
	var rec LiveVaultRecord
	ok, err := db.GetJSON(agentVaultKVKey, &rec)
	if err != nil || !ok || rec.TxID == "" {
		return nil, nil, err
	}
	beef, err := hex.DecodeString(rec.BEEFHex)
	if err != nil || len(beef) == 0 {
		return &rec, nil, fmt.Errorf("market: corrupt vault beefHex for %s", rec.TxID)
	}
	_, tx, _, err := guard.ParseBEEF(beef, guard.DefaultBound)
	if err != nil || tx == nil {
		return &rec, nil, fmt.Errorf("market: parse vault AtomicBEEF: %w", err)
	}
	if int(rec.Vout) >= len(tx.Outputs) || tx.Outputs[rec.Vout] == nil || tx.Outputs[rec.Vout].LockingScript == nil {
		return &rec, nil, fmt.Errorf("market: vault tx %s missing output %d", rec.TxID, rec.Vout)
	}
	if decoded, dErr := contracts.DecodeAgentVault(*tx.Outputs[rec.Vout].LockingScript); dErr == nil && decoded != nil {
		rec.State = *decoded
		rec.Satoshis = tx.Outputs[rec.Vout].Satoshis
	}
	return &rec, tx, nil
}

// SaveActiveVault persists the active NektarAgentVault singleton UTXO pointer in SQLite KV.
func SaveActiveVault(db *store.DB, rec LiveVaultRecord) error {
	if db == nil {
		return errors.New("market: nil store DB")
	}
	rec.UpdatedAt = time.Now().Unix()
	return db.SetJSON(agentVaultKVKey, rec)
}

// ClearActiveVault removes the active vault pointer (e.g. after OperatorSweep closes the vault).
func ClearActiveVault(db *store.DB) error {
	if db == nil {
		return nil
	}
	return db.SetJSON(agentVaultKVKey, LiveVaultRecord{})
}

// CompactVaultWithBUMP attaches a verified mined MerklePath to the persisted active vault UTXO
// and re-encodes its AtomicBEEF so pruned ancestors drop out once the vault tx mines.
func CompactVaultWithBUMP(ctx context.Context, db *store.DB, port *chain.Port, txid string, mp *transaction.MerklePath) {
	if db == nil || txid == "" || mp == nil {
		return
	}
	if port != nil && port.Network() != "memory" && port.Tracker() != nil {
		txH, err := chainhash.NewHashFromHex(txid)
		if err != nil || txH == nil {
			return
		}
		root, err := mp.ComputeRoot(txH)
		if err != nil || root == nil {
			return
		}
		ok, err := port.Tracker().IsValidRootForHeight(ctx, root, mp.BlockHeight)
		if err != nil || !ok {
			return
		}
	}
	agentVaultMu.Lock()
	defer agentVaultMu.Unlock()
	rec, tx, err := LoadActiveVault(db)
	if err != nil || rec == nil || tx == nil || !strings.EqualFold(rec.TxID, txid) {
		return
	}
	tx.MerklePath = mp
	compacted, err := chain.SafeAtomicBEEF(tx)
	if err != nil || len(compacted) == 0 {
		return
	}
	rec.BEEFHex = hex.EncodeToString(compacted)
	_ = SaveActiveVault(db, *rec)
}

// DeployAgentVault funds and broadcasts a fresh two-key consensus-enforced NektarAgentVault UTXO,
// separating cold operator authority (operatorPubOrPKHHex) from the local hot agent key (port.FundPKH).
func DeployAgentVault(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	operatorPubOrPKHHex string,
	depositSats uint64,
	perTxCap uint64,
	windowCap uint64,
	windowBlocks uint64,
	maxFee uint64,
) (*LiveVaultRecord, error) {
	if db == nil || port == nil {
		return nil, errors.New("market: DeployAgentVault requires db and chain port")
	}
	if depositSats < 1000 {
		return nil, errors.New("market: vault deposit must be >= 1000 sats")
	}
	opPKH, err := ParsePubKeyOrPKH(operatorPubOrPKHHex)
	if err != nil {
		return nil, fmt.Errorf("market: vault operator key: %w", err)
	}
	agentPKH, err := port.FundPKH(ctx)
	if err != nil {
		return nil, fmt.Errorf("market: derive agent fund PKH: %w", err)
	}
	if windowBlocks == 0 {
		windowBlocks = 144
	}
	if maxFee == 0 {
		maxFee = 600
	}
	h, hErr := port.Height(ctx)
	if hErr != nil {
		return nil, fmt.Errorf("market: fetch block height for vault windowStart: %w", hErr)
	}
	if h >= 500_000_000 || (port.Network() != "memory" && h == 0) {
		return nil, fmt.Errorf("market: invalid block height %d for vault windowStart", h)
	}
	startHeight := uint64(h)
	st := contracts.AgentVaultState{
		OperatorPKH:   opPKH,
		AgentPKH:      agentPKH,
		PerTxCap:      perTxCap,
		WindowCap:     windowCap,
		WindowBlocks:  windowBlocks,
		MaxFee:        maxFee,
		SpentInWindow: 0,
		WindowStart:   startHeight,
		Paused:        false,
	}
	lockScript, err := contracts.EncodeAgentVaultLockingScript(st)
	if err != nil {
		return nil, err
	}
	meta := nwallet.ActionDeployVault(depositSats)
	vaultSpec := &nwallet.OutputTagSpec{
		Basket:             nwallet.BasketVaults,
		Tags:               []string{"vault", "type:agent-vault"},
		CustomInstructions: fmt.Sprintf(`{"v":1,"kind":"agent-vault","sats":%d,"perTxCap":%d,"windowCap":%d,"windowBlocks":%d}`, depositSats, perTxCap, windowCap, windowBlocks),
	}
	tx, beef, fee, spend, err := port.BuildFundedTxWithBasket(ctx, []*transaction.TransactionOutput{{
		Satoshis:      depositSats,
		LockingScript: lockScript,
	}}, meta.Description, meta.Labels, vaultSpec)
	if err != nil {
		return nil, fmt.Errorf("market: build vault deploy tx: %w", err)
	}
	txid, err := port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		return nil, fmt.Errorf("market: broadcast vault deploy tx: %w", err)
	}
	if fresh, bErr := chain.SafeAtomicBEEF(tx); bErr == nil && len(fresh) > 0 {
		beef = fresh
	}
	rec := LiveVaultRecord{
		TxID:      txid,
		Vout:      0,
		Satoshis:  depositSats,
		BEEFHex:   hex.EncodeToString(beef),
		State:     st,
		UpdatedAt: time.Now().Unix(),
	}
	agentVaultMu.Lock()
	err = SaveActiveVault(db, rec)
	agentVaultMu.Unlock()
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

// SpendActiveVault executes NektarAgentVault.AgentSpend() directly into destScript (which can be
// a DisclosureSale locking script for 1-tx atomic vault->escrow composition OR a P2PKH script!),
// passing Chronicle Script VM verification and SpendGuard before broadcast, and advances the
// persisted vault UTXO pointer atomically to Output 0 of the resulting transaction.
func SpendActiveVault(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	destScript *script.Script,
	payAmount uint64,
) (tx *transaction.Transaction, beef []byte, outIdx uint32, feeSats uint64, nextRec *LiveVaultRecord, err error) {
	if db == nil || port == nil {
		return nil, nil, 0, 0, nil, errors.New("market: SpendActiveVault requires db and chain port")
	}
	agentVaultMu.Lock()
	defer agentVaultMu.Unlock()

	rec, prevTx, err := LoadActiveVault(db)
	if err != nil {
		return nil, nil, 0, 0, nil, err
	}
	if rec == nil || prevTx == nil {
		return nil, nil, 0, 0, nil, errors.New("market: no active NektarAgentVault deployed")
	}
	h, err := port.Height(ctx)
	if err != nil {
		return nil, nil, 0, 0, nil, fmt.Errorf("market: read witnessed chain height for vault spend: %w", err)
	}
	if uint64(h) < rec.State.WindowStart {
		h = uint32(rec.State.WindowStart)
	}
	feeSats = min(rec.State.MaxFee, uint64(450))
	spendTx, nextState, err := contracts.BuildAgentVaultSpendTx(
		ctx, prevTx, rec.Vout, destScript, payAmount, h, feeSats, port.SignCovenantSighash,
	)
	if err != nil {
		return nil, nil, 0, 0, nil, err
	}
	txid := spendTx.TxID().String()
	beef, err = chain.SafeAtomicBEEF(spendTx)
	if err != nil {
		return nil, nil, 0, 0, nil, fmt.Errorf("market: encode vault spend AtomicBEEF: %w", err)
	}
	if sg := port.SpendGuard(); sg != nil {
		if gErr := sg.CheckAndRecord(txid, feeSats, payAmount+feeSats); gErr != nil {
			return nil, nil, 0, 0, nil, gErr
		}
	}
	if _, bErr := port.SubmitCovenantTx(ctx, spendTx); bErr != nil {
		if sg := port.SpendGuard(); sg != nil && port.Network() == "memory" {
			_ = sg.Refund(txid)
		}
		if errors.Is(bErr, nwallet.ErrBroadcastUnknown) {
			return spendTx, beef, 1, feeSats, nil, bErr
		}
		return nil, nil, 0, 0, nil, bErr
	}
	if freshBEEF, fErr := chain.SafeAtomicBEEF(spendTx); fErr == nil && len(freshBEEF) > 0 {
		beef = freshBEEF
	}
	updated := LiveVaultRecord{
		TxID:      txid,
		Vout:      0,
		Satoshis:  spendTx.Outputs[0].Satoshis,
		BEEFHex:   hex.EncodeToString(beef),
		State:     *nextState,
		UpdatedAt: time.Now().Unix(),
	}
	if err := SaveActiveVault(db, updated); err != nil {
		return spendTx, beef, 1, feeSats, nil, err
	}
	return spendTx, beef, 1, feeSats, &updated, nil
}

// ApplyOperatorVaultControl executes a cold-operator NektarAgentVault.OperatorControl() tx
// (pause/resume, hot-agent key rotation, or cap tuning) and advances the persisted vault UTXO.
func ApplyOperatorVaultControl(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	upd contracts.OperatorControlUpdate,
	operatorSigner contracts.CovenantSighashSigner,
) (*LiveVaultRecord, error) {
	if db == nil || port == nil {
		return nil, errors.New("market: ApplyOperatorVaultControl requires db and chain port")
	}
	agentVaultMu.Lock()
	defer agentVaultMu.Unlock()

	rec, prevTx, err := LoadActiveVault(db)
	if err != nil {
		return nil, err
	}
	if rec == nil || prevTx == nil {
		return nil, errors.New("market: no active NektarAgentVault deployed")
	}
	ctrlTx, nextState, err := contracts.BuildAgentVaultOperatorControlTx(ctx, prevTx, rec.Vout, upd, operatorSigner)
	if err != nil {
		return nil, err
	}
	txid, err := port.SubmitCovenantTx(ctx, ctrlTx)
	if err != nil {
		return nil, err
	}
	beef, err := chain.SafeAtomicBEEF(ctrlTx)
	if err != nil {
		return nil, err
	}
	updated := LiveVaultRecord{
		TxID:      txid,
		Vout:      0,
		Satoshis:  ctrlTx.Outputs[0].Satoshis,
		BEEFHex:   hex.EncodeToString(beef),
		State:     *nextState,
		UpdatedAt: time.Now().Unix(),
	}
	if err := SaveActiveVault(db, updated); err != nil {
		return nil, err
	}
	return &updated, nil
}

// SweepActiveVault executes a cold-operator terminal NektarAgentVault.OperatorSweep() tx,
// sweeping all remaining vault satoshis to destPKH and clearing the active vault pointer.
func SweepActiveVault(
	ctx context.Context,
	db *store.DB,
	port *chain.Port,
	destPKH []byte,
	operatorSigner contracts.CovenantSighashSigner,
) (txid string, sweptSats uint64, err error) {
	if db == nil || port == nil {
		return "", 0, errors.New("market: SweepActiveVault requires db and chain port")
	}
	agentVaultMu.Lock()
	defer agentVaultMu.Unlock()

	rec, prevTx, err := LoadActiveVault(db)
	if err != nil {
		return "", 0, err
	}
	if rec == nil || prevTx == nil {
		return "", 0, errors.New("market: no active NektarAgentVault deployed")
	}
	sweepTx, err := contracts.BuildAgentVaultSweepTx(ctx, prevTx, rec.Vout, destPKH, 0, operatorSigner)
	if err != nil {
		return "", 0, err
	}
	txid, err = port.SubmitCovenantTx(ctx, sweepTx)
	if err != nil {
		return "", 0, err
	}
	sweptSats = sweepTx.Outputs[0].Satoshis
	_ = ClearActiveVault(db)
	return txid, sweptSats, nil
}
