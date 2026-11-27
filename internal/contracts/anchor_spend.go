package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sighash "github.com/bsv-blockchain/go-sdk/transaction/sighash"

	"github.com/kryptokrates/nektar/internal/core"
)

// CovenantSighashSigner signs a 32-byte BIP-143 sighash for the key matching targetPKH,
// returning (lowSDERWithAllForkID, compressedPubKey33, error).
// Keeps all raw private keys strictly inside wallet custody boundaries.
type CovenantSighashSigner func(ctx context.Context, targetPKH []byte, sighash32 []byte) (sigWithFlag []byte, pubCompressed []byte, err error)

// NextAnchorState computes the deterministic successor AnchorState for NektarAnchor.Append(),
// advancing R', StateHash (Q_n), and MmrRoot identically to the on-chain Rúnar script.
func NextAnchorState(prev AnchorState, newR []byte, newSize uint64, batchMetaHash, newMmrRoot []byte) (AnchorState, error) {
	if len(newR) != 32 {
		return AnchorState{}, errors.New("contracts: NextAnchorState requires 32-byte newR")
	}
	if len(prev.Rp) != 32 || len(prev.StateHash) != 32 {
		return AnchorState{}, errors.New("contracts: NextAnchorState requires 32-byte prev Rp and StateHash")
	}
	if len(batchMetaHash) != 32 || len(newMmrRoot) != 32 {
		return AnchorState{}, errors.New("contracts: NextAnchorState requires 32-byte batchMetaHash and newMmrRoot")
	}
	tag := prev.Tag
	if len(tag) == 0 {
		tag = core.TagRPrime
	}
	nextRp := core.ChainRpWithTag(tag, prev.Rp, newR, newSize)
	nextState := core.ChainStateQ(prev.StateHash, newR, batchMetaHash, newMmrRoot)
	return AnchorState{
		Shard:     prev.Shard,
		OwnerPub:  prev.OwnerPub,
		OwnerPKH:  append([]byte(nil), prev.OwnerPKH...),
		N:         prev.N + 1,
		Size:      newSize,
		R:         append([]byte(nil), newR...),
		Rp:        nextRp,
		StateHash: nextState,
		MmrRoot:   append([]byte(nil), newMmrRoot...),
		Tag:       append([]byte(nil), tag...),
		Metanet:   prev.Metanet,
	}, nil
}

// RotatedAnchorState computes the deterministic successor AnchorState for NektarAnchor.Rotate().
func RotatedAnchorState(prev AnchorState, newOwnerPKH []byte) (AnchorState, error) {
	if len(newOwnerPKH) != 20 {
		return AnchorState{}, errors.New("contracts: RotatedAnchorState requires 20-byte newOwnerPKH")
	}
	next := prev
	next.OwnerPKH = append([]byte(nil), newOwnerPKH...)
	next.R = append([]byte(nil), prev.R...)
	next.Rp = append([]byte(nil), prev.Rp...)
	next.StateHash = append([]byte(nil), prev.StateHash...)
	next.MmrRoot = append([]byte(nil), prev.MmrRoot...)
	next.Tag = append([]byte(nil), prev.Tag...)
	return next, nil
}

// BuildAnchorAppendUnlock constructs the OP_PUSH_TX + ECDSA unlocking script for NektarAnchor.Append()
// on tx.Inputs[inputIdx], spending prevOut (whose LockingScript must be a spendable NektarAnchor).
// changePKH must be 20 bytes (zeroed when changeAmount == 0).
func BuildAnchorAppendUnlock(
	ctx context.Context,
	tx *transaction.Transaction,
	inputIdx int,
	prevOut *transaction.TransactionOutput,
	newR []byte,
	newSize uint64,
	batchMetaHash []byte,
	newMmrRoot []byte,
	changePKH []byte,
	changeAmount uint64,
	signer CovenantSighashSigner,
) (*script.Script, error) {
	if prevOut == nil || prevOut.LockingScript == nil {
		return nil, errors.New("contracts: Append requires prevOut locking script")
	}
	if len(newR) != 32 || len(batchMetaHash) != 32 || len(newMmrRoot) != 32 {
		return nil, errors.New("contracts: Append requires 32-byte newR, batchMetaHash, and newMmrRoot")
	}
	if signer == nil {
		return nil, errors.New("contracts: Append requires signer")
	}
	prevScript := []byte(*prevOut.LockingScript)
	if !IsSpendableNektarAnchorScript(prevScript) {
		return nil, errors.New("contracts: previous anchor locking script is not spendable NektarAnchor")
	}
	prevState, err := DecodeNektarAnchor(prevScript)
	if err != nil {
		return nil, err
	}
	art, err := LoadFullArtifact("NektarAnchor")
	if err != nil {
		return nil, err
	}
	codePart, err := loadPatchedAnchorCode()
	if err != nil {
		return nil, err
	}
	if len(changePKH) == 0 {
		changePKH = make([]byte, 20)
	}
	if len(changePKH) != 20 {
		return nil, fmt.Errorf("contracts: Append changePKH must be 20 bytes, got %d", len(changePKH))
	}
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 0)
	preimage, err := ComputeOpPushTxPreimage(tx, inputIdx, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, fmt.Errorf("contracts: Append preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := signer(ctx, prevState.OwnerPKH, sigHash)
	if err != nil {
		return nil, fmt.Errorf("contracts: Append sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OwnerPKH) {
		return nil, errors.New("contracts: Append signer pubkey does not match anchor ownerPkh")
	}
	return BuildMethodUnlockingScript(art, "append", codePart, []any{
		sigWithFlag,
		pubCompressed,
		newR,
		newSize,
		batchMetaHash,
		newMmrRoot,
		changePKH,
		changeAmount,
		preimage,
	})
}

// BuildAnchorRotateUnlock constructs the OP_PUSH_TX + ECDSA unlocking script for NektarAnchor.Rotate()
// on tx.Inputs[inputIdx], transferring shard anchor ownership from prevState.OwnerPKH to newOwnerPKH.
func BuildAnchorRotateUnlock(
	ctx context.Context,
	tx *transaction.Transaction,
	inputIdx int,
	prevOut *transaction.TransactionOutput,
	newOwnerPKH []byte,
	changePKH []byte,
	changeAmount uint64,
	signer CovenantSighashSigner,
) (*script.Script, error) {
	if prevOut == nil || prevOut.LockingScript == nil {
		return nil, errors.New("contracts: Rotate requires prevOut locking script")
	}
	if len(newOwnerPKH) != 20 {
		return nil, errors.New("contracts: Rotate requires 20-byte newOwnerPKH")
	}
	if signer == nil {
		return nil, errors.New("contracts: Rotate requires signer")
	}
	prevScript := []byte(*prevOut.LockingScript)
	if !IsSpendableNektarAnchorScript(prevScript) {
		return nil, errors.New("contracts: previous anchor locking script is not spendable NektarAnchor")
	}
	prevState, err := DecodeNektarAnchor(prevScript)
	if err != nil {
		return nil, err
	}
	art, err := LoadFullArtifact("NektarAnchor")
	if err != nil {
		return nil, err
	}
	codePart, err := loadPatchedAnchorCode()
	if err != nil {
		return nil, err
	}
	if len(changePKH) == 0 {
		changePKH = make([]byte, 20)
	}
	if len(changePKH) != 20 {
		return nil, fmt.Errorf("contracts: Rotate changePKH must be 20 bytes, got %d", len(changePKH))
	}
	codeSepIdx := ResolveMethodCodeSepOffset(art, prevScript, 1)
	preimage, err := ComputeOpPushTxPreimage(tx, inputIdx, prevScript, prevOut.Satoshis, codeSepIdx, sighash.AllForkID)
	if err != nil {
		return nil, fmt.Errorf("contracts: Rotate preimage: %w", err)
	}
	sigHash := core.DSHA256(preimage)
	sigWithFlag, pubCompressed, err := signer(ctx, prevState.OwnerPKH, sigHash)
	if err != nil {
		return nil, fmt.Errorf("contracts: Rotate sign: %w", err)
	}
	if len(pubCompressed) != 33 || !bytes.Equal(core.Hash160(pubCompressed), prevState.OwnerPKH) {
		return nil, errors.New("contracts: Rotate signer pubkey does not match anchor ownerPkh")
	}
	return BuildMethodUnlockingScript(art, "rotate", codePart, []any{
		sigWithFlag,
		pubCompressed,
		newOwnerPKH,
		changePKH,
		changeAmount,
		preimage,
	})
}

// VerifyAnchorUTXOLineage checks that nextTx spends prevTx:0 via input 0, preserves shard identity,
// advances NektarAnchor state monotonically (Append: n+1, R' hash chain, StateHash Q_n, and MmrRoot,
// OR Rotate: same n/R/Rp/StateHash/MmrRoot/size with rotated OwnerPKH), AND passes full post-Chronicle
// Bitcoin Script VM execution on input 0.
func VerifyAnchorUTXOLineage(prevTx, nextTx *transaction.Transaction) error {
	if prevTx == nil || nextTx == nil {
		return errors.New("contracts: VerifyAnchorUTXOLineage requires non-nil prevTx and nextTx")
	}
	if len(prevTx.Outputs) == 0 || prevTx.Outputs[0] == nil || prevTx.Outputs[0].LockingScript == nil {
		return errors.New("contracts: prevTx missing output 0")
	}
	if len(nextTx.Inputs) == 0 || nextTx.Inputs[0] == nil || nextTx.Inputs[0].SourceTXID == nil {
		return errors.New("contracts: nextTx missing input 0")
	}
	if len(nextTx.Outputs) == 0 || nextTx.Outputs[0] == nil || nextTx.Outputs[0].LockingScript == nil {
		return errors.New("contracts: nextTx missing output 0")
	}
	if !nextTx.Inputs[0].SourceTXID.IsEqual(prevTx.TxID()) || nextTx.Inputs[0].SourceTxOutIndex != 0 {
		return fmt.Errorf("contracts: anchor lineage broken: nextTx input 0 spends %s:%d, expected %s:0",
			nextTx.Inputs[0].SourceTXID.String(), nextTx.Inputs[0].SourceTxOutIndex, prevTx.TxID().String())
	}
	prevSt, err := DecodeNektarAnchor(*prevTx.Outputs[0].LockingScript)
	if err != nil {
		return fmt.Errorf("contracts: decode prev anchor: %w", err)
	}
	nextSt, err := DecodeNektarAnchor(*nextTx.Outputs[0].LockingScript)
	if err != nil {
		return fmt.Errorf("contracts: decode next anchor: %w", err)
	}
	if prevSt.Shard != nextSt.Shard || prevSt.OwnerPub != nextSt.OwnerPub {
		return errors.New("contracts: anchor lineage mutated immutable genesis shard/ownerPub")
	}
	if nextSt.N == prevSt.N+1 {
		wantRp := core.ChainRpWithTag(prevSt.Tag, prevSt.Rp, nextSt.R, nextSt.Size)
		if !bytes.Equal(nextSt.Rp, wantRp) {
			return errors.New("contracts: anchor Append lineage R' mismatch")
		}
		if !bytes.Equal(prevSt.OwnerPKH, nextSt.OwnerPKH) {
			return errors.New("contracts: anchor Append mutated ownerPkh")
		}
	} else if nextSt.N == prevSt.N {
		if !bytes.Equal(prevSt.R, nextSt.R) || !bytes.Equal(prevSt.Rp, nextSt.Rp) ||
			!bytes.Equal(prevSt.StateHash, nextSt.StateHash) || !bytes.Equal(prevSt.MmrRoot, nextSt.MmrRoot) ||
			prevSt.Size != nextSt.Size {
			return errors.New("contracts: anchor Rotate mutated R/Rp/StateHash/MmrRoot/size")
		}
	} else {
		return fmt.Errorf("contracts: anchor lineage non-monotonic n: prev=%d next=%d", prevSt.N, nextSt.N)
	}
	return VerifyInputScriptChronicleConsensus(nextTx, 0, prevTx.Outputs[0])
}
