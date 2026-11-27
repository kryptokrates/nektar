package reputation

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/core"
)

type testIdentity struct {
	priv *ec.PrivateKey
	pub  string
}

func newTestIdentity(t *testing.T) testIdentity {
	t.Helper()
	priv, err := ec.NewPrivateKey()
	if err != nil {
		t.Fatalf("NewPrivateKey: %v", err)
	}
	return testIdentity{
		priv: priv,
		pub:  strings.ToLower(hex.EncodeToString(priv.PubKey().Compressed())),
	}
}

func (id testIdentity) signDigest(digest []byte) ([]byte, error) {
	sig, err := id.priv.Sign(digest)
	if err != nil {
		return nil, err
	}
	return sig.Serialize(), nil
}

func fakeDealHash(label string) string {
	return hex.EncodeToString(core.DSHA256([]byte("test-deal:" + label)))
}

func TestAttestationRoundTripAndBilateralReceiptEnforcement(t *testing.T) {
	buyer := newTestIdentity(t)
	seller := newTestIdentity(t)
	attacker := newTestIdentity(t)

	ontID := hex.EncodeToString(core.SHA256([]byte("ont-cve")))
	dealHash := fakeDealHash("deal-1")
	now := time.Now().Unix()

	att := Attestation{
		Version:          1,
		Role:             RoleBuyerRatesSeller,
		IssuerPub:        buyer.pub,
		SubjectPub:       seller.pub,
		OntologyID:       ontID,
		DealID:           "deal-1",
		DealReceiptHash:  dealHash,
		DealReceiptToken: "nektardr1.valid-token-1",
		Scores: Vector6D{
			AccBP:    9200,
			ConsBP:   8800,
			DispBP:   10000,
			PredBP:   9000,
			CollabBP: 8500,
			ProvBP:   9500,
		},
		Timestamp: now,
	}
	if err := SignAttestation(&att, buyer.pub, buyer.signDigest); err != nil {
		t.Fatalf("SignAttestation: %v", err)
	}

	tok, err := EncodeAttestationToken(att)
	if err != nil {
		t.Fatalf("EncodeAttestationToken: %v", err)
	}
	if !strings.HasPrefix(tok, TokenPrefix) {
		t.Fatalf("expected prefix %q, got %q", TokenPrefix, tok)
	}

	decoded, err := DecodeAttestationToken(tok)
	if err != nil {
		t.Fatalf("DecodeAttestationToken: %v", err)
	}
	if decoded.AttestHash != att.AttestHash {
		t.Fatalf("attestHash mismatch: %s != %s", decoded.AttestHash, att.AttestHash)
	}

	verifier := func(token string) (*DealReceiptBinding, error) {
		if token != "nektardr1.valid-token-1" {
			return nil, fmt.Errorf("unknown receipt token")
		}
		return &DealReceiptBinding{
			DealID:      "deal-1",
			ReceiptHash: dealHash,
			BuyerPub:    buyer.pub,
			SellerPub:   seller.pub,
			OntologyID:  ontID,
			PriceSats:   50,
			SettledAt:   now,
		}, nil
	}

	// 1. Valid bilateral verification succeeds.
	if _, err := VerifyAttestation(decoded, verifier, nil); err != nil {
		t.Fatalf("VerifyAttestation valid: %v", err)
	}

	// 2. Unbound attestation (no verifier & no knownBinding) MUST fail closed.
	if _, err := VerifyAttestation(decoded, nil, nil); err == nil {
		t.Fatal("expected fail-closed error when deal receipt is unverified")
	}

	// 3. Forged attestation by attacker trying to rate seller using buyer's deal receipt MUST fail!
	forged := att
	forged.IssuerPub = attacker.pub
	if err := SignAttestation(&forged, attacker.pub, attacker.signDigest); err != nil {
		t.Fatalf("SignAttestation forged: %v", err)
	}
	if _, err := VerifyAttestation(&forged, verifier, nil); err == nil {
		t.Fatal("expected forged attestation from non-party issuer to fail verification")
	}

	// 4. Self-rating MUST fail.
	selfAtt := att
	selfAtt.SubjectPub = buyer.pub
	if err := SignAttestation(&selfAtt, buyer.pub, buyer.signDigest); err == nil {
		t.Fatal("expected self-rating to be rejected")
	}
}

func TestPersonalizedEigenTrust_100SybilSockpuppetsZeroWeight(t *testing.T) {
	self := newTestIdentity(t)
	honestA := newTestIdentity(t)
	honestB := newTestIdentity(t)
	ontID := hex.EncodeToString(core.SHA256([]byte("ont-golang")))
	now := time.Now().Unix()

	// Create 100 Sybil sockpuppets that form a dense mutual 10,000-bps rating ring!
	sybils := make([]testIdentity, 100)
	for i := range sybils {
		sybils[i] = newTestIdentity(t)
	}

	var atts []Attestation
	// Direct deal: self -> honestA (high trust).
	atts = append(atts, Attestation{
		Version: 1, Role: RoleBuyerRatesSeller, IssuerPub: self.pub, SubjectPub: honestA.pub,
		OntologyID: ontID, DealID: "d-self-a", DealReceiptHash: fakeDealHash("d-self-a"),
		Scores:    Vector6D{AccBP: 9500, ConsBP: 9000, DispBP: 10000, PredBP: 9000, CollabBP: 9000, ProvBP: 9500},
		Timestamp: now - 3600,
	})
	// Transitive honest recommendation: honestA -> honestB (high trust).
	atts = append(atts, Attestation{
		Version: 1, Role: RoleBuyerRatesSeller, IssuerPub: honestA.pub, SubjectPub: honestB.pub,
		OntologyID: ontID, DealID: "d-a-b", DealReceiptHash: fakeDealHash("d-a-b"),
		Scores:    Vector6D{AccBP: 9000, ConsBP: 8800, DispBP: 10000, PredBP: 8500, CollabBP: 8500, ProvBP: 9000},
		Timestamp: now - 7200,
	})

	// 100 sockpuppets rate each other 10,000 bps AND try to smear honestB with 0 bps!
	for i := 0; i < len(sybils); i++ {
		next := (i + 1) % len(sybils)
		next2 := (i + 2) % len(sybils)
		for _, target := range []string{sybils[next].pub, sybils[next2].pub} {
			atts = append(atts, Attestation{
				Version: 1, Role: RoleBuyerRatesSeller, IssuerPub: sybils[i].pub, SubjectPub: target,
				OntologyID: ontID, DealID: fmt.Sprintf("syb-%d-%s", i, target[:8]),
				DealReceiptHash: fakeDealHash(fmt.Sprintf("syb-%d-%s", i, target[:8])),
				Scores:          Vector6D{AccBP: 10000, ConsBP: 10000, DispBP: 10000, PredBP: 10000, CollabBP: 10000, ProvBP: 10000},
				Timestamp:       now - 60,
			})
		}
		// Smear attempt on honestB!
		atts = append(atts, Attestation{
			Version: 1, Role: RoleBuyerRatesSeller, IssuerPub: sybils[i].pub, SubjectPub: honestB.pub,
			OntologyID: ontID, DealID: fmt.Sprintf("smear-%d", i),
			DealReceiptHash: fakeDealHash(fmt.Sprintf("smear-%d", i)),
			Scores:          Vector6D{AccBP: 0, ConsBP: 0, DispBP: 0, PredBP: 0, CollabBP: 0, ProvBP: 0},
			Timestamp:       now - 30,
		})
	}

	vecs := ComputeOntologyVectors(self.pub, ontID, atts, nil, now, DefaultHalfLifeSeconds)

	// 1. Every single Sybil sockpuppet MUST receive strictly 0 EigenTrustBP and 0 CompositeBP.
	for i, s := range sybils {
		v := vecs[s.pub]
		if v.EigenTrustBP != 0 {
			t.Fatalf("sybil[%d] leaked non-zero EigenTrustBP=%d", i, v.EigenTrustBP)
		}
		if v.CompositeBP != 0 {
			t.Fatalf("sybil[%d] leaked non-zero CompositeBP=%d", i, v.CompositeBP)
		}
	}

	// 2. honestA (direct) and honestB (1-hop transitive) both receive strong EigenTrust and unaffected 6D scores!
	va := vecs[honestA.pub]
	vb := vecs[honestB.pub]
	if va.EigenTrustBP < 5000 {
		t.Fatalf("expected honestA strong EigenTrustBP, got %d", va.EigenTrustBP)
	}
	if vb.EigenTrustBP < 2000 {
		t.Fatalf("expected honestB transitive EigenTrustBP >= 2000, got %d", vb.EigenTrustBP)
	}
	if vb.AccBP != 9000 {
		t.Fatalf("expected 100 sybil smear attacks to have ZERO effect on honestB AccBP=9000, got %d", vb.AccBP)
	}

	// 3. Mangle stratified Datalog must classify honestA & honestB as trusted_seller, and every Sybil as sybil_or_untrusted!
	allVecs := make([]Vector6D, 0, len(vecs))
	for _, v := range vecs {
		allVecs = append(allVecs, v)
	}
	eval, err := EvaluateMangleTrust(ontID, allVecs, nil, nil, nil, nil, TrustThresholds{})
	if err != nil {
		t.Fatalf("EvaluateMangleTrust: %v", err)
	}
	if !eval.TrustedSellers[honestA.pub] || !eval.TrustedSellers[honestB.pub] {
		t.Fatalf("expected honestA & honestB trusted_seller, got %+v", eval.TrustedSellers)
	}
	for i, s := range sybils[:5] {
		if eval.TrustedSellers[s.pub] {
			t.Fatalf("sybil[%d] must NEVER be trusted_seller", i)
		}
		if !eval.SybilOrUntrusted[s.pub] {
			t.Fatalf("sybil[%d] must be classified sybil_or_untrusted", i)
		}
	}
}

func TestTimeDecayContradictionBondSlashAndBilateralPriorityBuyer(t *testing.T) {
	// 1. Deterministic half-life decay check.
	if got := DecayWeightBP(0, DefaultHalfLifeSeconds); got != 10000 {
		t.Fatalf("age 0 decay = %d, want 10000", got)
	}
	if got := DecayWeightBP(DefaultHalfLifeSeconds, DefaultHalfLifeSeconds); got != 5000 {
		t.Fatalf("1 half-life decay = %d, want 5000", got)
	}
	if got := DecayWeightBP(2*DefaultHalfLifeSeconds, DefaultHalfLifeSeconds); got != 2500 {
		t.Fatalf("2 half-life decay = %d, want 2500", got)
	}

	// 2. Contradiction + bond slash downgrade.
	clean := EvaluatePurchasedFragment(BuyerEvalInput{
		SellerPub: "02" + strings.Repeat("aa", 32), OntologyID: "ont1",
		SPVVerified: true, MPEVerified: true, HasEvidenceSpans: true,
		HasPinnedOntology: true, SellerBonded: true, GroundedTriples: 3, DeliveryLatencyMs: 800,
	})
	if clean.AccBP < 9000 || clean.DispBP != 10000 || clean.ProvBP != 10000 {
		t.Fatalf("unexpected clean eval: %+v", clean)
	}

	contradicted := EvaluatePurchasedFragment(BuyerEvalInput{
		SellerPub: "02" + strings.Repeat("bb", 32), OntologyID: "ont1",
		SPVVerified: true, MPEVerified: true, ContradictionDetected: true, SellerBondSlashed: true,
	})
	if contradicted.AccBP > 1500 || contradicted.DispBP > 1000 {
		t.Fatalf("expected sharp contradiction + slash downgrade, got %+v", contradicted)
	}

	// Task outcome hook adjusts r_pred.
	afterPass := ApplyTaskOutcome(clean, 0)
	afterFail := ApplyTaskOutcome(clean, 1)
	if afterPass.PredBP != 9500 || afterFail.PredBP != 2000 {
		t.Fatalf("ApplyTaskOutcome unexpected: pass=%d fail=%d", afterPass.PredBP, afterFail.PredBP)
	}

	// 3. Mangle: contradiction revokes trusted_seller even if bonded; clean cold-start bonded seller is trusted;
	// high r_collab buyer becomes priority_buyer!
	sellerClean := "02" + strings.Repeat("11", 32)
	sellerBad := "02" + strings.Repeat("22", 32)
	buyerGood := "02" + strings.Repeat("33", 32)
	buyerScore := EvaluateBuyerCollaboration(SellerEvalInput{
		BuyerPub: buyerGood, OntologyID: "ont1",
		HasPinnedOntology: true, CleanFirstPay: true,
	})
	if buyerScore.CollabBP < 9000 {
		t.Fatalf("expected high buyer CollabBP, got %d", buyerScore.CollabBP)
	}

	eval, err := EvaluateMangleTrust(
		"ont1",
		[]Vector6D{buyerScore},
		[]PeerBondInfo{
			{Pub: sellerClean, BondSats: 25000, Status: "active"},
			{Pub: sellerBad, BondSats: 50000, Status: "active"},
		},
		[]ContradictionRecord{{PeerPub: sellerBad, OntologyID: "ont1", Seq: 7}},
		nil, nil, TrustThresholds{},
	)
	if err != nil {
		t.Fatalf("EvaluateMangleTrust: %v", err)
	}
	if !eval.TrustedSellers[sellerClean] {
		t.Fatal("expected clean bonded seller to be trusted_seller")
	}
	if eval.TrustedSellers[sellerBad] {
		t.Fatal("expected contradicted bonded seller to be disqualified from trusted_seller")
	}
	if !eval.PriorityBuyers[buyerGood] {
		t.Fatal("expected high-collab buyer to be classified priority_buyer")
	}
}
