package contracts

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/core"
)

// CheckpointBondState holds the full on-chain consensus state of a NektarCheckpointBond UTXO.
type CheckpointBondState struct {
	OperatorPKH       []byte `json:"operatorPkh"`       // 20B hash160(operator pubkey)
	BondReleaseHeight uint64 `json:"bondReleaseHeight"` // block height after which operator may Close()
	MaxFee            uint64 `json:"maxFee"`            // max miner fee per transition
	EpochLen          uint64 `json:"epochLen"`          // blocks per checkpoint SLA epoch
	N                 uint64 `json:"n"`                 // completed checkpoint count
	LastRoot          []byte `json:"lastRoot"`          // latest committed root (empty at genesis, 32B after Commit)
	Deadline          uint64 `json:"deadline"`          // next checkpoint deadline block height (<500_000_000)
	Seized            bool   `json:"seized"`            // true once slashed via Seize()
}

// Validate enforces NektarCheckpointBond consensus bounds fail-closed.
func (s CheckpointBondState) Validate() error {
	if len(s.OperatorPKH) != 20 {
		return fmt.Errorf("contracts: bond operatorPkh must be 20 bytes, got %d", len(s.OperatorPKH))
	}
	if s.BondReleaseHeight == 0 || s.BondReleaseHeight >= 500_000_000 {
		return fmt.Errorf("contracts: bondReleaseHeight %d must be in block-height domain (1..499999999)", s.BondReleaseHeight)
	}
	if s.Deadline == 0 || s.Deadline >= 500_000_000 {
		return fmt.Errorf("contracts: bond deadline %d must be in block-height domain (1..499999999)", s.Deadline)
	}
	if s.EpochLen == 0 || s.MaxFee == 0 {
		return errors.New("contracts: bond epochLen and maxFee must be > 0")
	}
	if len(s.LastRoot) > 75 {
		return errors.New("contracts: bond lastRoot exceeds 75 bytes")
	}
	return nil
}

func (s CheckpointBondState) toStateMap() map[string]any {
	seized := int64(0)
	if s.Seized {
		seized = 1
	}
	lastRoot := s.LastRoot
	if lastRoot == nil {
		lastRoot = []byte{}
	}
	return map[string]any{
		"operatorPkh":       s.OperatorPKH,
		"bondReleaseHeight": int64(s.BondReleaseHeight),
		"maxFee":            int64(s.MaxFee),
		"epochLen":          int64(s.EpochLen),
		"n":                 int64(s.N),
		"lastRoot":          lastRoot,
		"deadline":          int64(s.Deadline),
		"seized":            seized,
	}
}

// EncodeCheckpointBondLockingScript builds the L1-spendable NektarCheckpointBond locking script
// (<patched NektarCheckpointBond code> OP_RETURN <serialized state>).
func EncodeCheckpointBondLockingScript(st CheckpointBondState) (*script.Script, error) {
	if err := st.Validate(); err != nil {
		return nil, err
	}
	art, err := LoadFullArtifact("NektarCheckpointBond")
	if err != nil {
		return nil, err
	}
	return BuildStatefulLockingScript(art, nil, st.toStateMap())
}

// DecodeCheckpointBond validates scriptBytes against the embedded NektarCheckpointBond code prefix
// and decodes its OP_RETURN state payload.
func DecodeCheckpointBond(scriptBytes []byte) (*CheckpointBondState, error) {
	art, err := LoadFullArtifact("NektarCheckpointBond")
	if err != nil {
		return nil, err
	}
	codePart, err := BuildCodePart(art, nil)
	if err != nil {
		return nil, err
	}
	if len(scriptBytes) <= len(codePart) || !bytes.Equal(scriptBytes[:len(codePart)], codePart) || scriptBytes[len(codePart)] != 0x6a {
		return nil, errors.New("contracts: locking script code does not match NektarCheckpointBond artifact")
	}
	fields, err := DeserializeStateFields(art.StateFields, scriptBytes[len(codePart)+1:])
	if err != nil {
		return nil, err
	}
	st := &CheckpointBondState{
		OperatorPKH:       fields["operatorPkh"].([]byte),
		BondReleaseHeight: uint64(fields["bondReleaseHeight"].(int64)),
		MaxFee:            uint64(fields["maxFee"].(int64)),
		EpochLen:          uint64(fields["epochLen"].(int64)),
		N:                 uint64(fields["n"].(int64)),
		LastRoot:          fields["lastRoot"].([]byte),
		Deadline:          uint64(fields["deadline"].(int64)),
		Seized:            fields["seized"].(int64) != 0,
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	return st, nil
}

// VerifiedBondSummary reports the SPV-verified status of a peer/hub's advertised NektarCheckpointBond UTXO.
type VerifiedBondSummary struct {
	OK          bool                 `json:"ok"`
	Settled     bool                 `json:"settled"`
	TxID        string               `json:"txid"`
	Vout        uint32               `json:"vout"`
	BondSats    uint64               `json:"bondSats"`
	BlockHeight uint32               `json:"blockHeight,omitempty"`
	State       *CheckpointBondState `json:"state,omitempty"`
	Reason      string               `json:"reason,omitempty"`
}

// VerifyCheckpointBondBEEF validates a seller/hub's advertised NektarCheckpointBond BEEF:
// parses BEEF, decodes and verifies the NektarCheckpointBond script + state, checks active (!Seized),
// optionally binds OperatorPKH, and verifies the BUMP against tracker when checkHeaders is true.
func VerifyCheckpointBondBEEF(
	ctx context.Context,
	beefBytes []byte,
	vout uint32,
	expectedOperatorPKH []byte,
	tracker chaintracker.ChainTracker,
	checkHeaders bool,
) VerifiedBondSummary {
	if len(beefBytes) == 0 {
		return VerifiedBondSummary{OK: false, Reason: "empty bond BEEF"}
	}
	_, tx, txHash, err := guard.ParseBEEF(beefBytes, guard.DefaultBound)
	if err != nil || tx == nil || txHash == nil {
		return VerifiedBondSummary{OK: false, Reason: "malformed bond BEEF"}
	}
	if int(vout) >= len(tx.Outputs) || tx.Outputs[vout] == nil || tx.Outputs[vout].LockingScript == nil {
		return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "bond vout out of range"}
	}
	out := tx.Outputs[vout]
	st, err := DecodeCheckpointBond(*out.LockingScript)
	if err != nil {
		return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: err.Error()}
	}
	if st.Seized {
		return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, BondSats: out.Satoshis, State: st, Reason: "bond already seized (slashed)"}
	}
	if len(expectedOperatorPKH) == 20 && !bytes.Equal(st.OperatorPKH, expectedOperatorPKH) {
		return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, BondSats: out.Satoshis, State: st, Reason: "bond operatorPkh mismatch"}
	}

	settled := false
	var bh uint32
	if tx.MerklePath != nil {
		if len(tx.MerklePath.Path) > core.MaxMerkleProofDepth {
			return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "BUMP depth exceeds cap"}
		}
		root, rErr := tx.MerklePath.ComputeRoot(txHash)
		if rErr != nil || root == nil {
			return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "BUMP root computation failed"}
		}
		bh = tx.MerklePath.BlockHeight
		if checkHeaders {
			if tracker == nil {
				return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "no chain tracker configured"}
			}
			valid, vErr := tracker.IsValidRootForHeight(ctx, root, bh)
			if vErr != nil {
				return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "TrackerUnavailable"}
			}
			if !valid {
				return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "BUMP root invalid in header chain"}
			}
			// Check that bond is not already past its slashable deadline at current tip (fail closed on tracker error).
			tip, tErr := tracker.CurrentHeight(ctx)
			if tErr != nil {
				return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, Reason: "TrackerUnavailable"}
			}
			if uint64(tip) >= st.Deadline {
				return VerifiedBondSummary{OK: false, TxID: txHash.String(), Vout: vout, BondSats: out.Satoshis, BlockHeight: bh, State: st, Reason: "bond deadline expired (slashable)"}
			}
		}
		settled = true
	} else if checkHeaders {
		return VerifiedBondSummary{OK: false, Settled: false, TxID: txHash.String(), Vout: vout, BondSats: out.Satoshis, State: st, Reason: "unmined bond tx (no BUMP)"}
	}

	return VerifiedBondSummary{
		OK:          true,
		Settled:     settled,
		TxID:        txHash.String(),
		Vout:        vout,
		BondSats:    out.Satoshis,
		BlockHeight: bh,
		State:       st,
	}
}
