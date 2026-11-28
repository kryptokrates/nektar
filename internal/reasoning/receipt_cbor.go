package reasoning

import (
	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

func fragmentRefToCBOR(ref FragmentRef) cbor.Map {
	return cbor.Map{
		{Key: uint64(1), Val: ref.Shard},
		{Key: uint64(2), Val: ref.Seq},
	}
}

func fragmentRefsToCBOR(refs []FragmentRef) []cbor.Value {
	arr := make([]cbor.Value, len(refs))
	for i, ref := range refs {
		arr[i] = fragmentRefToCBOR(ref)
	}
	return arr
}

func derivationStepToCBOR(d DerivationStep) cbor.Map {
	dm := cbor.Map{
		{Key: uint64(1), Val: d.DerivedAtom},
		{Key: uint64(3), Val: d.RuleHash},
		{Key: uint64(4), Val: d.RuleText},
	}
	if d.RuleRef != nil {
		dm = append(dm, cbor.Pair{Key: uint64(2), Val: fragmentRefToCBOR(*d.RuleRef)})
	}
	if len(d.Bindings) > 0 {
		dm = append(dm, cbor.Pair{Key: uint64(5), Val: core.CBORStringMapVal(d.Bindings)})
	}
	if len(d.Premises) > 0 {
		ps := make([]cbor.Value, len(d.Premises))
		for i, p := range d.Premises {
			pm := cbor.Map{
				{Key: uint64(1), Val: p.Atom},
			}
			if p.FactRef != nil {
				pm = append(pm, cbor.Pair{Key: uint64(2), Val: fragmentRefToCBOR(*p.FactRef)})
			}
			if p.FactHDP != "" {
				pm = append(pm, cbor.Pair{Key: uint64(3), Val: p.FactHDP})
			}
			if p.Negated {
				pm = append(pm, cbor.Pair{Key: uint64(4), Val: true})
			}
			if p.Step != nil {
				pm = append(pm, cbor.Pair{Key: uint64(5), Val: derivationStepToCBOR(*p.Step)})
			}
			ps[i] = pm
		}
		dm = append(dm, cbor.Pair{Key: uint64(6), Val: ps})
	}
	return dm
}

// EncodeReceiptCBOR encodes r into canonical RFC 8949 CDE CBOR for deterministic hashing and signing.
func EncodeReceiptCBOR(r Receipt) ([]byte, error) {
	if r.TruthNote == "" {
		r.TruthNote = TruthNote
	}
	mid := cbor.Map{
		{Key: uint64(1), Val: r.ModelID.Kind},
	}
	if r.ModelID.Provider != "" {
		mid = append(mid, cbor.Pair{Key: uint64(2), Val: r.ModelID.Provider})
	}
	if r.ModelID.Release != "" {
		mid = append(mid, cbor.Pair{Key: uint64(3), Val: r.ModelID.Release})
	}
	if r.ModelID.WeightsSHA256 != "" {
		mid = append(mid, cbor.Pair{Key: uint64(4), Val: r.ModelID.WeightsSHA256})
	}
	if r.ModelID.Name != "" {
		mid = append(mid, cbor.Pair{Key: uint64(5), Val: r.ModelID.Name})
	}

	m := cbor.Map{
		{Key: uint64(1), Val: r.Answer},
		{Key: uint64(2), Val: fragmentRefsToCBOR(r.FragmentRefs)},
		{Key: uint64(3), Val: r.CheckResult},
		{Key: uint64(4), Val: mid},
		{Key: uint64(5), Val: r.Timestamp},
		{Key: uint64(13), Val: r.TruthNote},
	}
	if r.GroundingReportHash != "" {
		m = append(m, cbor.Pair{Key: uint64(6), Val: r.GroundingReportHash})
	}
	if r.GroundingTier != "" {
		m = append(m, cbor.Pair{Key: uint64(7), Val: r.GroundingTier})
	}
	if r.PolicyCommitHash != "" {
		m = append(m, cbor.Pair{Key: uint64(8), Val: r.PolicyCommitHash})
	}
	if r.PolicyRef != nil {
		m = append(m, cbor.Pair{Key: uint64(9), Val: fragmentRefToCBOR(*r.PolicyRef)})
	}
	if r.OntologyCommitHash != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: r.OntologyCommitHash})
	}
	if len(r.RuleRefs) > 0 {
		m = append(m, cbor.Pair{Key: uint64(11), Val: fragmentRefsToCBOR(r.RuleRefs)})
	}
	if len(r.Derivations) > 0 {
		ds := make([]cbor.Value, len(r.Derivations))
		for i, d := range r.Derivations {
			ds[i] = derivationStepToCBOR(d)
		}
		m = append(m, cbor.Pair{Key: uint64(12), Val: ds})
	}
	if len(r.Delegations) > 0 {
		dels := make([]cbor.Value, len(r.Delegations))
		for i, d := range r.Delegations {
			dels[i] = cbor.Map{
				{Key: uint64(1), Val: int64(d.Version)},
				{Key: uint64(2), Val: d.Delegator},
				{Key: uint64(3), Val: d.Decision},
				{Key: uint64(4), Val: d.Signature},
			}
		}
		m = append(m, cbor.Pair{Key: uint64(14), Val: dels})
	}
	if r.Payment != nil {
		pm := cbor.Map{
			{Key: uint64(1), Val: r.Payment.TxID},
			{Key: uint64(2), Val: r.Payment.AmountSats},
			{Key: uint64(3), Val: r.Payment.PayTo},
			{Key: uint64(4), Val: r.Payment.InvoiceID},
		}
		if r.Payment.BEEFHex != "" {
			pm = append(pm, cbor.Pair{Key: uint64(5), Val: r.Payment.BEEFHex})
		}
		m = append(m, cbor.Pair{Key: uint64(15), Val: pm})
	}
	if r.IssuerPub != "" {
		m = append(m, cbor.Pair{Key: uint64(16), Val: r.IssuerPub})
	}
	if r.IssuerSig != "" {
		m = append(m, cbor.Pair{Key: uint64(17), Val: r.IssuerSig})
	}
	return core.EncodeCBOR(m)
}
