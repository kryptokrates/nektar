package reputation

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"codeberg.org/TauCeti/mangle-go/parse"

	"github.com/kryptokrates/nektar/internal/reasoning"
)

// EvaluateMangleTrust compiles peer 6D reputation vectors, active NektarCheckpointBond facts,
// Mangle contradiction records, and operator pin/block overrides into a deterministic stratified
// Google Mangle Datalog program and derives trusted_seller/2, priority_buyer/2, and sybil_or_untrusted/2.
func EvaluateMangleTrust(
	ontologyID string,
	vectors []Vector6D,
	bonds []PeerBondInfo,
	contradictions []ContradictionRecord,
	operatorPreferred []string,
	operatorBlocked []string,
	thresholds TrustThresholds,
) (*TrustEvaluation, error) {
	ont := NormalizeOntologyID(ontologyID)
	th := thresholds.Normalized()

	var b strings.Builder
	// Explicit declarations so empty relations analyze cleanly under mangle-go.
	b.WriteString("Decl target_ontology(Ont).\n")
	b.WriteString("Decl peer_rep(P, Ont, Acc, Cons, Disp, Pred, Collab, Prov, Eigen, Comp).\n")
	b.WriteString("Decl peer_bond(P, BondSats, Status).\n")
	b.WriteString("Decl peer_contradiction(P, Ont, Seq).\n")
	b.WriteString("Decl operator_preferred(P).\n")
	b.WriteString("Decl operator_blocked(P).\n")
	b.WriteString("Decl peer_has_contradiction(P, Ont).\n")
	b.WriteString("Decl peer_has_rep(P, Ont).\n")
	b.WriteString("Decl trusted_seller(P, Ont).\n")
	b.WriteString("Decl priority_buyer(P, Ont).\n")
	b.WriteString("Decl sybil_or_untrusted(P, Ont).\n")

	fmt.Fprintf(&b, "target_ontology(%s).\n", strconv.Quote(ont))

	// Sort inputs deterministically.
	vecs := append([]Vector6D(nil), vectors...)
	sort.Slice(vecs, func(i, j int) bool {
		if vecs[i].SubjectPub != vecs[j].SubjectPub {
			return vecs[i].SubjectPub < vecs[j].SubjectPub
		}
		return vecs[i].OntologyID < vecs[j].OntologyID
	})
	for _, v := range vecs {
		pub := strings.ToLower(strings.TrimSpace(v.SubjectPub))
		if pub == "" {
			continue
		}
		vOnt := NormalizeOntologyID(v.OntologyID)
		fmt.Fprintf(&b, "peer_rep(%s, %s, %d, %d, %d, %d, %d, %d, %d, %d).\n",
			strconv.Quote(pub), strconv.Quote(vOnt),
			ClampBP(v.AccBP), ClampBP(v.ConsBP), ClampBP(v.DispBP),
			ClampBP(v.PredBP), ClampBP(v.CollabBP), ClampBP(v.ProvBP),
			ClampBP(v.EigenTrustBP), ClampBP(v.CompositeBP),
		)
	}

	bList := append([]PeerBondInfo(nil), bonds...)
	sort.Slice(bList, func(i, j int) bool { return bList[i].Pub < bList[j].Pub })
	for _, pb := range bList {
		pub := strings.ToLower(strings.TrimSpace(pb.Pub))
		if pub == "" {
			continue
		}
		st := strings.ToLower(strings.TrimSpace(pb.Status))
		if st == "" {
			st = "none"
		}
		fmt.Fprintf(&b, "peer_bond(%s, %d, %s).\n", strconv.Quote(pub), pb.BondSats, strconv.Quote(st))
	}

	for _, c := range contradictions {
		pub := strings.ToLower(strings.TrimSpace(c.PeerPub))
		if pub == "" {
			continue
		}
		fmt.Fprintf(&b, "peer_contradiction(%s, %s, %d).\n",
			strconv.Quote(pub), strconv.Quote(NormalizeOntologyID(c.OntologyID)), c.Seq,
		)
	}
	for _, p := range operatorPreferred {
		lp := strings.ToLower(strings.TrimSpace(p))
		if lp != "" {
			fmt.Fprintf(&b, "operator_preferred(%s).\n", strconv.Quote(lp))
		}
	}
	for _, p := range operatorBlocked {
		lp := strings.ToLower(strings.TrimSpace(p))
		if lp != "" {
			fmt.Fprintf(&b, "operator_blocked(%s).\n", strconv.Quote(lp))
		}
	}

	// Helper projections for range-restricted stratified negation.
	b.WriteString("peer_has_contradiction(P, Ont) :- peer_contradiction(P, Ont, _).\n")
	b.WriteString("peer_has_rep(P, Ont) :- peer_rep(P, Ont, _, _, _, _, _, _, _, _).\n")

	// 1. Operator-pinned peer (unless explicitly blocked).
	b.WriteString("trusted_seller(P, Ont) :- operator_preferred(P), target_ontology(Ont), !operator_blocked(P).\n")

	// 2. Reputation-trusted seller via 6D thresholds + personalized EigenTrust (contradiction-free, unblocked).
	fmt.Fprintf(&b,
		"trusted_seller(P, Ont) :- peer_rep(P, Ont, Acc, _, Disp, _, _, _, Eigen, Comp), Acc >= %d, Disp >= %d, Eigen >= %d, Comp >= %d, !peer_has_contradiction(P, Ont), !operator_blocked(P).\n",
		th.MinAccuracyBP, th.MinDisputeBP, th.MinEigenTrustBP, th.MinCompositeBP,
	)

	// 3. Active NektarCheckpointBond seller with clean dispute history (or cold-start bonded seller with no prior rep).
	fmt.Fprintf(&b,
		"trusted_seller(P, Ont) :- peer_bond(P, BondSats, \"active\"), BondSats >= %d, peer_rep(P, Ont, _, _, Disp, _, _, _, _, _), Disp >= %d, !peer_has_contradiction(P, Ont), !operator_blocked(P).\n",
		th.MinBondSats, th.MinDisputeBP,
	)
	fmt.Fprintf(&b,
		"trusted_seller(P, Ont) :- target_ontology(Ont), peer_bond(P, BondSats, \"active\"), BondSats >= %d, !peer_has_rep(P, Ont), !peer_has_contradiction(P, Ont), !operator_blocked(P).\n",
		th.MinBondSats,
	)

	// 4. Priority buyer based on bilateral seller_rates_buyer collaboration score + dispute hygiene.
	fmt.Fprintf(&b,
		"priority_buyer(P, Ont) :- peer_rep(P, Ont, _, _, Disp, _, Collab, _, _, _), Collab >= %d, Disp >= %d, !operator_blocked(P).\n",
		th.PriorityCollabBP, th.MinDisputeBP,
	)

	// 5. Sybil or untrusted classification (EigenTrust == 0 and not trusted_seller, OR operator blocked).
	b.WriteString("sybil_or_untrusted(P, Ont) :- peer_rep(P, Ont, _, _, _, _, _, _, 0, _), !trusted_seller(P, Ont).\n")
	b.WriteString("sybil_or_untrusted(P, Ont) :- operator_blocked(P), target_ontology(Ont).\n")

	derived, err := reasoning.EvalDatalog(b.String())
	if err != nil {
		return nil, fmt.Errorf("reputation: mangle trust eval: %w", err)
	}

	eval := &TrustEvaluation{
		OntologyID:       ont,
		TrustedSellers:   make(map[string]bool),
		PriorityBuyers:   make(map[string]bool),
		SybilOrUntrusted: make(map[string]bool),
	}
	for factStr := range derived {
		atom, pErr := parse.Atom(factStr)
		if pErr != nil || len(atom.Args) != 2 {
			continue
		}
		pArg, err1 := strconv.Unquote(atom.Args[0].String())
		oArg, err2 := strconv.Unquote(atom.Args[1].String())
		if err1 != nil || err2 != nil || oArg != ont {
			continue
		}
		switch atom.Predicate.Symbol {
		case "trusted_seller":
			eval.TrustedSellers[pArg] = true
		case "priority_buyer":
			eval.PriorityBuyers[pArg] = true
		case "sybil_or_untrusted":
			eval.SybilOrUntrusted[pArg] = true
		}
	}
	return eval, nil
}
