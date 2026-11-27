package reputation

import (
	"sort"
	"strings"
)

const (
	eigenIterations = 16
	eigenScale      = int64(1_000_000)
	eigenAlphaBP    = int64(8500) // 0.85 damping toward local trust graph; 0.15 return to self pre-trust
)

// fracHalfLifeTableBP holds round(10000 * 2^(-i/16)) for i = 0..16.
// Pure deterministic integer math — zero floating-point variance across architectures.
var fracHalfLifeTableBP = [17]int64{
	10000, 9576, 9170, 8781, 8409, 8052, 7711, 7384,
	7071, 6771, 6484, 6209, 5946, 5694, 5453, 5221, 5000,
}

// DecayWeightBP computes a deterministic integer exponential decay weight in [0, 10000]
// with half-life halfLifeSec: weight(age) = 10000 * 2^(-age / halfLifeSec).
func DecayWeightBP(ageSeconds, halfLifeSec int64) int64 {
	if ageSeconds <= 0 {
		return MaxBP
	}
	if halfLifeSec <= 0 {
		halfLifeSec = DefaultHalfLifeSeconds
	}
	fullHalves := ageSeconds / halfLifeSec
	if fullHalves >= 14 {
		// 10000 / 2^14 < 1 bp
		return 0
	}
	rem := ageSeconds % halfLifeSec
	// Bucket index in [0, 15] plus linear interpolation to next bucket for <1 bp precision.
	pos16 := (rem * 16) / halfLifeSec
	if pos16 > 15 {
		pos16 = 15
	}
	bucketStart := (pos16 * halfLifeSec) / 16
	bucketLen := halfLifeSec / 16
	if bucketLen <= 0 {
		bucketLen = 1
	}
	hi := fracHalfLifeTableBP[pos16]
	lo := fracHalfLifeTableBP[pos16+1]
	fracWeight := hi - ((hi - lo) * (rem - bucketStart) / bucketLen)
	w := fracWeight >> uint(fullHalves)
	if w < 0 {
		return 0
	}
	if w > MaxBP {
		return MaxBP
	}
	return w
}

// ComputeOntologyVectors computes deterministic Personalized EigenTrust weights and
// trust-weighted 6D reputation vectors for every subject in ontologyID.
//
// Sybil Guarantee:
//   - The pre-trust vector p is supported STRICTLY on peers that selfPub has directly
//     rated positively in verified bilateral deals plus explicit operator-pinned peers.
//   - Disconnected sockpuppet clusters (even 100+ peers rating each other 10,000 bps)
//     are mathematically unreachable from p and receive EigenTrustBP == 0 AND contribute
//     0 weight to any peer's 6D score.
func ComputeOntologyVectors(
	selfPub string,
	ontologyID string,
	attestations []Attestation,
	pinnedPeers map[string]bool,
	nowUnix int64,
	halfLifeSec int64,
) map[string]Vector6D {
	self := strings.ToLower(strings.TrimSpace(selfPub))
	targetOnt := NormalizeOntologyID(ontologyID)
	if halfLifeSec <= 0 {
		halfLifeSec = DefaultHalfLifeSeconds
	}

	// 1. Filter valid attestations for targetOnt and collect unique peers in deterministic order.
	var rel []Attestation
	peerSet := make(map[string]bool)
	if self != "" {
		peerSet[self] = true
	}
	for p, ok := range pinnedPeers {
		if ok {
			lp := strings.ToLower(strings.TrimSpace(p))
			if lp != "" && lp != self {
				peerSet[lp] = true
			}
		}
	}
	for _, a := range attestations {
		if targetOnt != GlobalOntologyID && NormalizeOntologyID(a.OntologyID) != targetOnt {
			continue
		}
		iss := strings.ToLower(strings.TrimSpace(a.IssuerPub))
		sub := strings.ToLower(strings.TrimSpace(a.SubjectPub))
		if iss == "" || sub == "" || iss == sub {
			continue
		}
		a.IssuerPub = iss
		a.SubjectPub = sub
		a.OntologyID = NormalizeOntologyID(a.OntologyID)
		a.Scores.CompositeBP = ComputeComposite6D(
			a.Scores.AccBP, a.Scores.ConsBP, a.Scores.DispBP,
			a.Scores.PredBP, a.Scores.CollabBP, a.Scores.ProvBP,
		)
		rel = append(rel, a)
		peerSet[iss] = true
		peerSet[sub] = true
	}

	peers := make([]string, 0, len(peerSet))
	for p := range peerSet {
		peers = append(peers, p)
	}
	sort.Strings(peers)
	if len(peers) == 0 {
		return map[string]Vector6D{}
	}
	idx := make(map[string]int, len(peers))
	for i, p := range peers {
		idx[p] = i
	}
	n := len(peers)

	// 2. Build time-decayed directed positive trust mass s[i][j] from issuer i -> subject j.
	// Only positive ratings above neutral floor (CompositeBP > 4000 and DispBP >= 4000) carry trust flow.
	rawPos := make([]map[int]int64, n)
	for _, a := range rel {
		age := nowUnix - a.Timestamp
		if age < 0 {
			age = 0
		}
		decay := DecayWeightBP(age, halfLifeSec)
		if decay == 0 {
			continue
		}
		pos := a.Scores.CompositeBP - 4000
		if pos <= 0 || a.Scores.DispBP < 4000 {
			continue
		}
		w := (pos * decay) / MaxBP
		if w <= 0 {
			continue
		}
		i := idx[a.IssuerPub]
		j := idx[a.SubjectPub]
		if rawPos[i] == nil {
			rawPos[i] = make(map[int]int64)
		}
		rawPos[i][j] += w
	}

	// 3. Build personalized pre-trust vector p (fixed-point sum == eigenScale) rooted strictly in selfPub + pinnedPeers.
	preRaw := make([]int64, n)
	var preSum int64
	if selfIdx, ok := idx[self]; ok {
		for j, w := range rawPos[selfIdx] {
			if j != selfIdx && w > 0 {
				preRaw[j] += w
				preSum += w
			}
		}
	}
	for p, ok := range pinnedPeers {
		if !ok {
			continue
		}
		lp := strings.ToLower(strings.TrimSpace(p))
		if j, exists := idx[lp]; exists && lp != self {
			// Pinned operator peer receives anchor pre-trust mass equivalent to a fresh high-trust deal.
			const pinnedSeed int64 = 6000
			preRaw[j] += pinnedSeed
			preSum += pinnedSeed
		}
	}

	preTrust := make([]int64, n)
	if preSum > 0 {
		for i := 0; i < n; i++ {
			if preRaw[i] > 0 {
				preTrust[i] = (preRaw[i] * eigenScale) / preSum
			}
		}
	}

	// 4. Row-normalize C[i][j] in fixed-point eigenScale.
	// Rows with zero outgoing positive edges stay empty (their mass returns to p, never to strangers).
	cMat := make([]map[int]int64, n)
	for i := 0; i < n; i++ {
		if len(rawPos[i]) == 0 {
			continue
		}
		var rowSum int64
		for _, w := range rawPos[i] {
			rowSum += w
		}
		if rowSum <= 0 {
			continue
		}
		cMat[i] = make(map[int]int64, len(rawPos[i]))
		for j, w := range rawPos[i] {
			cMat[i][j] = (w * eigenScale) / rowSum
		}
	}

	// 5. Power-iterate Personalized EigenTrust t^(k+1) = alpha * (t^(k) * C) + (1 - alpha) * p
	//    (with dangling-node mass returning strictly to p!).
	tVec := make([]int64, n)
	copy(tVec, preTrust)
	if preSum > 0 {
		for iter := 0; iter < eigenIterations; iter++ {
			next := make([]int64, n)
			var transferred int64
			for i := 0; i < n; i++ {
				ti := tVec[i]
				if ti <= 0 || len(cMat[i]) == 0 {
					continue
				}
				for j, cij := range cMat[i] {
					flow := (ti * cij) / eigenScale
					if flow > 0 {
						next[j] += flow
						transferred += flow
					}
				}
			}
			// Combine damped graph walk with personalized restart vector p (including dangling mass).
			dangling := eigenScale - transferred
			if dangling < 0 {
				dangling = 0
			}
			for j := 0; j < n; j++ {
				walk := (eigenAlphaBP * next[j]) / MaxBP
				restart := ((MaxBP - eigenAlphaBP) * preTrust[j]) / MaxBP
				danglingReturn := (eigenAlphaBP * ((dangling * preTrust[j]) / eigenScale)) / MaxBP
				tVec[j] = walk + restart + danglingReturn
			}
		}
	}

	// 6. Normalize EigenTrustBP into [0, 10000] relative to max trusted peer (strictly 0 when tVec[j] == 0).
	var maxT int64
	for i := 0; i < n; i++ {
		if peers[i] == self {
			continue
		}
		if tVec[i] > maxT {
			maxT = tVec[i]
		}
	}
	eigenBP := make([]int64, n)
	if maxT > 0 {
		for i := 0; i < n; i++ {
			if peers[i] == self {
				eigenBP[i] = MaxBP
				continue
			}
			if tVec[i] > 0 {
				v := (tVec[i] * MaxBP) / maxT
				if v == 0 {
					v = 1
				}
				eigenBP[i] = ClampBP(v)
			}
		}
	} else if selfIdx, ok := idx[self]; ok {
		eigenBP[selfIdx] = MaxBP
	}

	// 7. Compute trust-weighted, time-decayed 6D vectors per subject.
	// Direct attestations from selfPub carry full weight 10000; third-party attestations carry
	// weight == issuer's EigenTrustBP (so Sybil clusters with EigenTrustBP == 0 carry ZERO weight).
	type accum struct {
		accNum, consNum, dispNum, predNum, collabNum, provNum int64
		weightSum                                             int64
		directDeals                                           int64
		attestCount                                           int64
		latestTS                                              int64
	}
	bySubject := make(map[string]*accum)
	for _, a := range rel {
		age := nowUnix - a.Timestamp
		if age < 0 {
			age = 0
		}
		decay := DecayWeightBP(age, halfLifeSec)
		if decay == 0 {
			continue
		}
		ac := bySubject[a.SubjectPub]
		if ac == nil {
			ac = &accum{}
			bySubject[a.SubjectPub] = ac
		}
		ac.attestCount++
		if a.Timestamp > ac.latestTS {
			ac.latestTS = a.Timestamp
		}
		var issuerWeight int64
		if a.IssuerPub == self {
			issuerWeight = MaxBP
			ac.directDeals++
		} else if iIdx, ok := idx[a.IssuerPub]; ok {
			issuerWeight = eigenBP[iIdx]
		}
		w := (issuerWeight * decay) / MaxBP
		if w <= 0 {
			continue
		}
		ac.accNum += a.Scores.AccBP * w
		ac.consNum += a.Scores.ConsBP * w
		ac.dispNum += a.Scores.DispBP * w
		ac.predNum += a.Scores.PredBP * w
		ac.collabNum += a.Scores.CollabBP * w
		ac.provNum += a.Scores.ProvBP * w
		ac.weightSum += w
	}

	out := make(map[string]Vector6D, len(bySubject))
	for sub, ac := range bySubject {
		eBP := int64(0)
		if j, ok := idx[sub]; ok {
			eBP = eigenBP[j]
		}
		v := Vector6D{
			SubjectPub:   sub,
			OntologyID:   targetOnt,
			EigenTrustBP: eBP,
			DirectDeals:  ac.directDeals,
			AttestCount:  ac.attestCount,
			UpdatedAt:    ac.latestTS,
		}
		if ac.weightSum > 0 {
			v.AccBP = ClampBP(ac.accNum / ac.weightSum)
			v.ConsBP = ClampBP(ac.consNum / ac.weightSum)
			v.DispBP = ClampBP(ac.dispNum / ac.weightSum)
			v.PredBP = ClampBP(ac.predNum / ac.weightSum)
			v.CollabBP = ClampBP(ac.collabNum / ac.weightSum)
			v.ProvBP = ClampBP(ac.provNum / ac.weightSum)
			v.CompositeBP = ComputeComposite6D(v.AccBP, v.ConsBP, v.DispBP, v.PredBP, v.CollabBP, v.ProvBP)
		}
		out[sub] = v
	}
	// Also emit entries for operator-pinned peers that have no attestations yet so EigenTrustBP is visible.
	for p, ok := range pinnedPeers {
		if !ok {
			continue
		}
		lp := strings.ToLower(strings.TrimSpace(p))
		if lp == "" || lp == self {
			continue
		}
		if _, exists := out[lp]; !exists {
			eBP := int64(0)
			if j, existsIdx := idx[lp]; existsIdx {
				eBP = eigenBP[j]
			}
			out[lp] = Vector6D{
				SubjectPub:   lp,
				OntologyID:   targetOnt,
				EigenTrustBP: eBP,
				UpdatedAt:    nowUnix,
			}
		}
	}
	return out
}
