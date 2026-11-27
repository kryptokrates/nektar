package chain

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkwallet "github.com/bsv-blockchain/go-sdk/wallet"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/contracts"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// CovenantSpendRequest describes spending a Rúnar covenant UTXO (Input 0) with optional
// automatic BRC-100 wallet fuel + change handling and mandatory Chronicle Script VM verification.
type CovenantSpendRequest struct {
	InputTx               *transaction.Transaction
	InputBEEF             []byte
	InputVout             uint32
	Outputs               []*transaction.TransactionOutput
	LockTime              uint32
	Sequence              *uint32 // nil => 0xfffffffe if LockTime > 0 else 0xffffffff
	UnlockingScriptLength uint32  // fee-sizing hint when FuelFromWallet == true
	FuelFromWallet        bool    // true => add wallet BRC-29 fuel + change via CreateAction/SignAction; false => self-funded 1-input tx
	Description           string
	Labels                []string
	OutputSpec            *nwallet.OutputTagSpec // optional BRC-100 asset basket + tags for covenant output(s)
	// BuildUnlock is invoked with the assembled transaction (including wallet fuel inputs & change output if any)
	// and must return Input 0's complete unlocking script.
	BuildUnlock func(tx *transaction.Transaction, changePKH []byte, changeSats uint64) (*script.Script, error)
}

// CovenantSpendResult holds the Chronicle-verified signed covenant spending transaction and AtomicBEEF.
type CovenantSpendResult struct {
	Tx               *transaction.Transaction
	BEEF             []byte
	FeeSats          uint64
	SpendSats        uint64
	ChangePKH        []byte
	ChangeSats       uint64
	AlreadyBroadcast bool
}

// BuildCovenantSpendTx builds, unlocks, and verifies (via post-Chronicle Bitcoin Script VM)
// a transaction spending a Rúnar covenant UTXO as Input 0.
// When FuelFromWallet is true, it uses BRC-100 CreateAction(SignAndProcess:false) + SignAction(NoSend:true)
// so wallet UTXOs are atomically reserved and released on any failure, and Broadcast() submits through SpendGuard.
func (p *Port) BuildCovenantSpendTx(ctx context.Context, req CovenantSpendRequest) (*CovenantSpendResult, error) {
	if req.InputTx == nil {
		return nil, errors.New("chain: BuildCovenantSpendTx requires InputTx")
	}
	if int(req.InputVout) >= len(req.InputTx.Outputs) || req.InputTx.Outputs[req.InputVout] == nil || req.InputTx.Outputs[req.InputVout].LockingScript == nil {
		return nil, fmt.Errorf("chain: BuildCovenantSpendTx invalid input vout %d", req.InputVout)
	}
	if len(req.Outputs) == 0 {
		return nil, errors.New("chain: BuildCovenantSpendTx requires at least one output")
	}
	if req.BuildUnlock == nil {
		return nil, errors.New("chain: BuildCovenantSpendTx requires BuildUnlock callback")
	}

	seq := uint32(0xffffffff)
	if req.LockTime > 0 {
		seq = 0xfffffffe
	}
	if req.Sequence != nil {
		seq = *req.Sequence
	}
	prevOut := req.InputTx.Outputs[req.InputVout]

	// Mode A: Self-funded 1-input covenant tx (DisclosureSale.Buy/Refund, NektarAgentVault.*).
	if !req.FuelFromWallet {
		tx := transaction.NewTransaction()
		tx.LockTime = req.LockTime
		tx.AddInput(&transaction.TransactionInput{
			SourceTXID:        req.InputTx.TxID(),
			SourceTxOutIndex:  req.InputVout,
			SourceTransaction: req.InputTx,
			SequenceNumber:    seq,
		})
		var outTotal uint64
		for _, o := range req.Outputs {
			tx.AddOutput(o)
			outTotal += o.Satoshis
		}
		if prevOut.Satoshis < outTotal {
			return nil, fmt.Errorf("chain: self-funded covenant input %d < output sum %d", prevOut.Satoshis, outTotal)
		}
		unlock, err := req.BuildUnlock(tx, make([]byte, 20), 0)
		if err != nil {
			return nil, err
		}
		tx.Inputs[0].UnlockingScript = unlock
		if err := contracts.VerifyInputScriptChronicleCtx(ctx, tx, 0, prevOut); err != nil {
			return nil, err
		}
		beef, err := SafeAtomicBEEF(tx)
		if err != nil {
			return nil, fmt.Errorf("chain: encode covenant AtomicBEEF: %w", err)
		}
		fee := prevOut.Satoshis - outTotal
		return &CovenantSpendResult{
			Tx:        tx,
			BEEF:      beef,
			FeeSats:   fee,
			SpendSats: outTotal + fee,
		}, nil
	}

	// Mode B: Wallet-fueled multi-input covenant tx (NektarAnchor.Append / NektarAnchor.Rotate).
	if p.wallet == nil {
		return nil, errors.New("chain: wallet not initialized for fueled covenant spend")
	}
	inputBeef := req.InputBEEF
	if len(inputBeef) == 0 {
		var err error
		inputBeef, err = SafeAtomicBEEF(req.InputTx)
		if err != nil {
			return nil, fmt.Errorf("chain: encode InputBEEF for covenant spend: %w", err)
		}
	}
	unlockLen := req.UnlockingScriptLength
	if unlockLen == 0 {
		unlockLen = 5120
	}
	baseDesc := req.Description
	if strings.TrimSpace(baseDesc) == "" {
		baseDesc = nwallet.ActionCovenantSpend().Description
	}
	desc, labels := decorateActionWithContext(ctx, baseDesc, append([]string{"covenant"}, req.Labels...))
	wOuts := make([]sdkwallet.CreateActionOutput, len(req.Outputs))
	var outTotal uint64
	for i, o := range req.Outputs {
		if o == nil || o.LockingScript == nil {
			return nil, fmt.Errorf("chain: covenant output %d has nil locking script", i)
		}
		outTotal += o.Satoshis
		wOuts[i] = sdkwallet.CreateActionOutput{
			LockingScript:     append([]byte(nil), (*o.LockingScript)...),
			Satoshis:          o.Satoshis,
			OutputDescription: desc,
		}
	}
	applyOutputTagSpec(wOuts, req.OutputSpec)

	signAndProc := false
	randomize := false
	noSend := p.engine != nil
	acceptDelayed := false
	var lockTimePtr *uint32
	if req.LockTime > 0 {
		lt := req.LockTime
		lockTimePtr = &lt
	}
	var resID string
	var estFee uint64
	if p.engine == nil {
		// Client mode broadcasts directly inside SignAction: atomically reserve SpendGuard budget BEFORE CreateAction.
		var rErr error
		resID, estFee, rErr = p.reserveClientSpend(ctx, outTotal, len(wOuts), false)
		if rErr != nil {
			return nil, rErr
		}
	}
	cancelRes := func() {
		if resID != "" && p.guard != nil {
			p.guard.CancelReservation(resID)
			resID = ""
		}
	}
	created, err := p.wallet.CreateAction(ctx, sdkwallet.CreateActionArgs{
		Description: desc,
		Labels:      labels,
		InputBEEF:   inputBeef,
		Inputs: []sdkwallet.CreateActionInput{{
			Outpoint:              transaction.Outpoint{Txid: *req.InputTx.TxID(), Index: req.InputVout},
			InputDescription:      desc,
			UnlockingScriptLength: unlockLen,
			SequenceNumber:        &seq,
		}},
		Outputs:  wOuts,
		LockTime: lockTimePtr,
		Options: &sdkwallet.CreateActionOptions{
			SignAndProcess:         &signAndProc,
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
			RandomizeOutputs:       &randomize,
		},
	}, Originator)
	if err != nil {
		cancelRes()
		return nil, fmt.Errorf("chain: covenant CreateAction: %w", err)
	}
	if created == nil || created.SignableTransaction == nil {
		cancelRes()
		return nil, errors.New("chain: covenant CreateAction did not return SignableTransaction")
	}
	abortAction := func() {
		_, _ = p.wallet.AbortAction(ctx, sdkwallet.AbortActionArgs{Reference: created.SignableTransaction.Reference}, Originator)
	}

	signableTx, err := transaction.NewTransactionFromBEEF(created.SignableTransaction.Tx)
	if err != nil {
		cancelRes()
		abortAction()
		return nil, fmt.Errorf("chain: parse signable covenant tx: %w", err)
	}
	if len(signableTx.Inputs) == 0 {
		cancelRes()
		abortAction()
		return nil, errors.New("chain: signable covenant tx has no inputs")
	}
	// Ensure Input 0 has its SourceTransaction attached for preimage & Script VM execution.
	if signableTx.Inputs[0].SourceTransaction == nil {
		signableTx.Inputs[0].SourceTransaction = req.InputTx
	}

	// Extract optional wallet change output placed after explicit outputs (RandomizeOutputs=false).
	changePKH := make([]byte, 20)
	var changeSats uint64
	extraOuts := len(signableTx.Outputs) - len(req.Outputs)
	if extraOuts > 1 {
		cancelRes()
		abortAction()
		return nil, fmt.Errorf("chain: covenant CreateAction produced %d change outputs (stateful covenants support at most 1)", extraOuts)
	}
	if extraOuts == 1 {
		chgOut := signableTx.Outputs[len(req.Outputs)]
		if chgOut == nil || chgOut.LockingScript == nil {
			cancelRes()
			abortAction()
			return nil, errors.New("chain: covenant change output missing locking script")
		}
		pkh, ok := contracts.ExtractP2PKH(*chgOut.LockingScript)
		if !ok {
			cancelRes()
			abortAction()
			return nil, errors.New("chain: wallet change output is not standard P2PKH")
		}
		changePKH = pkh
		changeSats = chgOut.Satoshis
	}

	unlock, err := req.BuildUnlock(signableTx, changePKH, changeSats)
	if err != nil {
		cancelRes()
		abortAction()
		return nil, fmt.Errorf("chain: build covenant unlock: %w", err)
	}
	signableTx.Inputs[0].UnlockingScript = unlock

	// Pre-sign fail-closed Chronicle Script VM gate on Input 0 BEFORE finalizing SignAction!
	if err := contracts.VerifyInputScriptChronicleCtx(ctx, signableTx, 0, prevOut); err != nil {
		cancelRes()
		abortAction()
		return nil, fmt.Errorf("chain: pre-broadcast Chronicle Script VM rejected covenant spend: %w", err)
	}

	signed, err := p.wallet.SignAction(ctx, sdkwallet.SignActionArgs{
		Reference: created.SignableTransaction.Reference,
		Spends: map[uint32]sdkwallet.SignActionSpend{
			0: {
				UnlockingScript: append([]byte(nil), (*unlock)...),
				SequenceNumber:  &seq,
			},
		},
		Options: &sdkwallet.SignActionOptions{
			NoSend:                 &noSend,
			AcceptDelayedBroadcast: &acceptDelayed,
		},
	}, Originator)
	if err != nil {
		if p.engine == nil && nwallet.IsUncertainBroadcastError(err) {
			// Remote wallet may already have broadcast: keep SpendGuard reservation intact and never AbortAction.
			return nil, fmt.Errorf("chain: covenant SignAction: %w: %w", nwallet.ErrBroadcastUnknown, err)
		}
		cancelRes()
		abortAction()
		return nil, fmt.Errorf("chain: covenant SignAction: %w", err)
	}

	txid := signed.Txid.String()
	beef := signed.Tx
	var finalTx *transaction.Transaction
	var parseErr error
	if len(beef) > 0 {
		_, finalTx, _, parseErr = guard.ParseBEEF(beef, guard.DefaultBound)
	}
	if p.engine == nil {
		p.rememberClientTxBytes(txid, beef, finalTx)
		// Unconditionally commit client broadcast spend BEFORE returning any parse error so spentToday never under-counts.
		feeSats, cErr := p.commitClientSpend(resID, txid, outTotal, estFee, finalTx, false)
		if cErr != nil {
			return nil, cErr
		}
		if parseErr != nil {
			return nil, fmt.Errorf("chain: parse signed covenant AtomicBEEF: %w", parseErr)
		}
		if finalTx == nil {
			return nil, fmt.Errorf("chain: SignAction did not return parseable tx for %s", txid)
		}
		if len(beef) == 0 {
			beef, err = SafeAtomicBEEF(finalTx)
			if err != nil {
				return nil, err
			}
		}
		return &CovenantSpendResult{
			Tx:               finalTx,
			BEEF:             beef,
			FeeSats:          feeSats,
			SpendSats:        outTotal + feeSats,
			ChangePKH:        changePKH,
			ChangeSats:       changeSats,
			AlreadyBroadcast: !noSend,
		}, nil
	}

	if parseErr != nil {
		_ = p.engine.AbortNoSendByTxID(ctx, txid)
		return nil, fmt.Errorf("chain: parse signed covenant AtomicBEEF: %w", parseErr)
	}
	if finalTx == nil {
		finalTx, err = p.engine.LoadParsedTx(ctx, txid)
		if err != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, err
		}
	}
	if finalTx == nil {
		return nil, fmt.Errorf("chain: SignAction did not return parseable tx for %s", txid)
	}
	if len(beef) == 0 {
		beef, err = SafeAtomicBEEF(finalTx)
		if err != nil {
			_ = p.engine.AbortNoSendByTxID(ctx, txid)
			return nil, err
		}
	}

	feeSats := nwallet.ComputeTxFee(finalTx)
	if feeSats == 0 {
		feeSats = p.engine.TxFeeSats(ctx, txid)
	}
	p.mu.Lock()
	p.pendingNoSend[txid] = txid
	p.mu.Unlock()

	return &CovenantSpendResult{
		Tx:               finalTx,
		BEEF:             beef,
		FeeSats:          feeSats,
		SpendSats:        outTotal + feeSats,
		ChangePKH:        changePKH,
		ChangeSats:       changeSats,
		AlreadyBroadcast: !noSend,
	}, nil
}
