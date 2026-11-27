package reputation

// BuyerEvalInput captures deterministic post-purchase signals observed by a buyer after settling a deal.
type BuyerEvalInput struct {
	SellerPub             string
	OntologyID            string
	SPVVerified           bool
	MPEVerified           bool
	HasEvidenceSpans      bool
	HasPinnedOntology     bool
	SellerBonded          bool
	SellerBondSlashed     bool
	ContradictionDetected bool
	PriorContradictions   int
	PriorCleanDeals       int
	RefundOrTimeout       bool
	DeliveryLatencyMs     int64
	GroundedTriples       int
	GradeMismatch         bool  // NMU-1 post-reveal grade/unit shortfall or expiry violation
	SlashAccCapBP         int64 // optional cap for AccBP when GradeMismatch is true (default 2000)
	SlashDispCapBP        int64 // optional cap for DispBP when GradeMismatch is true (default 2500)
	SlashProvCapBP        int64 // optional cap for ProvBP when GradeMismatch is true (default 2500)
	FeedDroppedEarly      bool  // Grade S subscription feed returned 404/410/5xx or dropped before expires_at
}

// EvaluatePurchasedFragment deterministically scores a seller across all 6 reputation dimensions
// based solely on locally verifiable post-purchase cryptographic, Mangle, and settlement evidence.
func EvaluatePurchasedFragment(in BuyerEvalInput) Vector6D {
	// 1. Accuracy (r_acc): Mangle consistency + MPE/SPV integrity.
	acc := int64(5000)
	if in.SPVVerified && in.MPEVerified {
		acc = 8500
		if in.GroundedTriples > 0 {
			acc = 9500
		}
	} else if in.SPVVerified || in.MPEVerified {
		acc = 6500
	}
	if in.ContradictionDetected {
		acc = 1000
	}

	// 2. Consistency (r_cons): historical contradiction rate across AORG revisions.
	cons := int64(8500)
	if in.PriorCleanDeals >= 3 {
		cons = 9200
	}
	if in.ContradictionDetected {
		cons -= 4000
	}
	cons -= int64(in.PriorContradictions) * 1500
	cons = ClampBP(cons)
	if cons < 500 && in.ContradictionDetected {
		cons = 500
	}

	// 3. Dispute hygiene (r_disp): atomic settlement vs refund/escrow timeout or bond slash.
	disp := int64(10000)
	if in.DeliveryLatencyMs > 15000 {
		disp = 7500
	}
	if in.RefundOrTimeout {
		disp = 1500
	}
	if in.SellerBondSlashed {
		disp = 500
	}

	// 4. Predictive utility (r_pred): neutral prior until downstream task outcome hook reports pass/fail.
	pred := int64(7000)
	if in.ContradictionDetected || in.RefundOrTimeout {
		pred = 2000
	}

	// 5. Collaboration quality (r_collab): responsiveness & clean schema delivery.
	collab := int64(8500)
	if in.DeliveryLatencyMs > 0 && in.DeliveryLatencyMs <= 3000 {
		collab = 9500
	} else if in.DeliveryLatencyMs > 15000 {
		collab = 5500
	}
	if in.RefundOrTimeout {
		collab = 2000
	}

	// 6. Provenance completeness (r_prov): evidence spans, SPV anchor, pinned ontology, checkpoint bond.
	prov := int64(4000)
	if in.HasEvidenceSpans {
		prov += 2000
	}
	if in.SPVVerified {
		prov += 2000
	}
	if in.HasPinnedOntology {
		prov += 1000
	}
	if in.SellerBonded && !in.SellerBondSlashed {
		prov += 1000
	}
	prov = ClampBP(prov)

	// NMU-1 post-reveal grade inflation slashing: cap AccBP, DispBP, ProvBP, and CollabBP
	// so dishonest sellers lose Grade S/A ranking bonuses and trigger Mangle trust exclusion.
	if in.GradeMismatch {
		accCap := in.SlashAccCapBP
		if accCap <= 0 {
			accCap = 2000
		}
		dispCap := in.SlashDispCapBP
		if dispCap <= 0 {
			dispCap = 2500
		}
		provCap := in.SlashProvCapBP
		if provCap <= 0 {
			provCap = 2500
		}
		if acc > accCap {
			acc = accCap
		}
		if disp > dispCap {
			disp = dispCap
		}
		if prov > provCap {
			prov = provCap
		}
		if collab > 3000 {
			collab = 3000
		}
		if pred > 3000 {
			pred = 3000
		}
	}
	// Grade S dead-feed penalty when a paid subscription stream drops/404s before expires_at.
	if in.FeedDroppedEarly {
		if disp > 2500 {
			disp = 2500
		}
		if collab > 3500 {
			collab = 3500
		}
	}

	return Vector6D{
		SubjectPub:  in.SellerPub,
		OntologyID:  NormalizeOntologyID(in.OntologyID),
		AccBP:       ClampBP(acc),
		ConsBP:      ClampBP(cons),
		DispBP:      ClampBP(disp),
		PredBP:      ClampBP(pred),
		CollabBP:    ClampBP(collab),
		ProvBP:      ClampBP(prov),
		CompositeBP: ComputeComposite6D(acc, cons, disp, pred, collab, prov),
	}
}

// SellerEvalInput captures deterministic bilateral signals observed by a seller about a buyer.
type SellerEvalInput struct {
	BuyerPub           string
	OntologyID         string
	HasPinnedOntology  bool
	CleanFirstPay      bool
	PaymentTimedOut    bool
	SpuriousDispute    bool
	MalformedWantQuery bool
}

// EvaluateBuyerCollaboration scores a buyer bilaterally (`seller_rates_buyer`), focusing on
// r_collab (structured ontology-pinned queries & prompt BRC-29 settlement) and r_disp.
func EvaluateBuyerCollaboration(in SellerEvalInput) Vector6D {
	collab := int64(5500)
	if in.HasPinnedOntology {
		collab += 2000
	}
	if in.CleanFirstPay {
		collab += 1500
	}
	if !in.PaymentTimedOut && !in.SpuriousDispute {
		collab += 1000
	}
	if in.MalformedWantQuery {
		collab -= 3000
	}
	if in.PaymentTimedOut {
		collab -= 3500
	}
	if in.SpuriousDispute {
		collab -= 4500
	}
	collab = ClampBP(collab)

	disp := int64(10000)
	if in.PaymentTimedOut {
		disp = 3500
	}
	if in.SpuriousDispute {
		disp = 1000
	}

	acc := int64(8000)
	if in.MalformedWantQuery {
		acc = 3000
	}
	cons := int64(8000)
	pred := int64(7500)
	prov := int64(7500)
	if in.HasPinnedOntology {
		prov = 9000
	}

	return Vector6D{
		SubjectPub:  in.BuyerPub,
		OntologyID:  NormalizeOntologyID(in.OntologyID),
		AccBP:       ClampBP(acc),
		ConsBP:      ClampBP(cons),
		DispBP:      ClampBP(disp),
		PredBP:      ClampBP(pred),
		CollabBP:    ClampBP(collab),
		ProvBP:      ClampBP(prov),
		CompositeBP: ComputeComposite6D(acc, cons, disp, pred, collab, prov),
	}
}

// ApplyTaskOutcome updates r_pred (and recalculates CompositeBP) after a developer/agent
// reports whether the purchased fragment helped a task succeed (`exitCode == 0`) or fail.
func ApplyTaskOutcome(prev Vector6D, exitCode int) Vector6D {
	out := prev
	if exitCode == 0 {
		out.PredBP = 9500
	} else {
		out.PredBP = 2000
	}
	out.CompositeBP = ComputeComposite6D(out.AccBP, out.ConsBP, out.DispBP, out.PredBP, out.CollabBP, out.ProvBP)
	return out
}
