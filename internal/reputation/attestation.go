package reputation

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

// Attestation is a canonical RFC 8949 CBOR bilateral reputation attestation (`nektarrep1.*`),
// cryptographically bound to a verified bilateral deal receipt (`nektardr1.*`).
type Attestation struct {
	Version          uint64   `json:"v"`
	Role             string   `json:"role"` // buyer_rates_seller | seller_rates_buyer
	IssuerPub        string   `json:"issuerPub"`
	SubjectPub       string   `json:"subjectPub"`
	OntologyID       string   `json:"ontologyId"`
	DealID           string   `json:"dealId"`
	DealReceiptHash  string   `json:"dealReceiptHash"`
	DealReceiptToken string   `json:"dealReceiptToken,omitempty"`
	Scores           Vector6D `json:"scores"`
	EvidenceHash     string   `json:"evidenceHash,omitempty"`
	AdvisoryNote     string   `json:"advisoryNote"`
	Timestamp        int64    `json:"ts"`
	SigHex           string   `json:"sig,omitempty"`
	AttestHash       string   `json:"attestHash,omitempty"`
}

func validateAttestationBody(a *Attestation) error {
	if a == nil {
		return errors.New("reputation: nil attestation")
	}
	if a.Version == 0 {
		a.Version = 1
	}
	if a.Version != 1 {
		return fmt.Errorf("reputation: unsupported attestation version %d", a.Version)
	}
	if a.Role != RoleBuyerRatesSeller && a.Role != RoleSellerRatesBuyer {
		return fmt.Errorf("reputation: invalid role %q", a.Role)
	}
	a.IssuerPub = strings.ToLower(strings.TrimSpace(a.IssuerPub))
	a.SubjectPub = strings.ToLower(strings.TrimSpace(a.SubjectPub))
	if _, _, err := core.ParseStrictCompressedPubKey(a.IssuerPub); err != nil {
		return fmt.Errorf("reputation: invalid issuerPub: %w", err)
	}
	if _, _, err := core.ParseStrictCompressedPubKey(a.SubjectPub); err != nil {
		return fmt.Errorf("reputation: invalid subjectPub: %w", err)
	}
	if strings.EqualFold(a.IssuerPub, a.SubjectPub) {
		return errors.New("reputation: self-rating is forbidden (issuerPub == subjectPub)")
	}
	a.OntologyID = NormalizeOntologyID(a.OntologyID)
	a.DealID = strings.TrimSpace(a.DealID)
	a.DealReceiptHash = strings.ToLower(strings.TrimSpace(a.DealReceiptHash))
	if a.DealID == "" {
		return errors.New("reputation: dealId is required")
	}
	if len(a.DealReceiptHash) != 64 {
		return errors.New("reputation: dealReceiptHash must be 64-char hex")
	}
	if _, err := hex.DecodeString(a.DealReceiptHash); err != nil {
		return fmt.Errorf("reputation: invalid dealReceiptHash hex: %w", err)
	}
	for name, val := range map[string]int64{
		"accBp":    a.Scores.AccBP,
		"consBp":   a.Scores.ConsBP,
		"dispBp":   a.Scores.DispBP,
		"predBp":   a.Scores.PredBP,
		"collabBp": a.Scores.CollabBP,
		"provBp":   a.Scores.ProvBP,
	} {
		if val < 0 || val > MaxBP {
			return fmt.Errorf("reputation: score %s=%d out of [0, %d]", name, val, MaxBP)
		}
	}
	a.Scores.CompositeBP = ComputeComposite6D(
		a.Scores.AccBP, a.Scores.ConsBP, a.Scores.DispBP,
		a.Scores.PredBP, a.Scores.CollabBP, a.Scores.ProvBP,
	)
	a.Scores.SubjectPub = a.SubjectPub
	a.Scores.OntologyID = a.OntologyID
	if strings.TrimSpace(a.AdvisoryNote) == "" {
		a.AdvisoryNote = AdvisoryReputationNote
	}
	if a.Timestamp <= 0 {
		return errors.New("reputation: timestamp must be > 0")
	}
	a.EvidenceHash = strings.ToLower(strings.TrimSpace(a.EvidenceHash))
	if a.EvidenceHash != "" {
		if len(a.EvidenceHash) != 64 {
			return errors.New("reputation: evidenceHash must be 64-char hex when set")
		}
		if _, err := hex.DecodeString(a.EvidenceHash); err != nil {
			return fmt.Errorf("reputation: invalid evidenceHash hex: %w", err)
		}
	}
	return nil
}

func attestationBodyPairs(a Attestation) cbor.Map {
	return cbor.Map{
		{Key: uint64(1), Val: a.Version},
		{Key: uint64(2), Val: a.Role},
		{Key: uint64(3), Val: a.IssuerPub},
		{Key: uint64(4), Val: a.SubjectPub},
		{Key: uint64(5), Val: NormalizeOntologyID(a.OntologyID)},
		{Key: uint64(6), Val: a.DealID},
		{Key: uint64(7), Val: a.DealReceiptHash},
		{Key: uint64(8), Val: uint64(a.Scores.AccBP)},
		{Key: uint64(9), Val: uint64(a.Scores.ConsBP)},
		{Key: uint64(10), Val: uint64(a.Scores.DispBP)},
		{Key: uint64(11), Val: uint64(a.Scores.PredBP)},
		{Key: uint64(12), Val: uint64(a.Scores.CollabBP)},
		{Key: uint64(13), Val: uint64(a.Scores.ProvBP)},
		{Key: uint64(14), Val: a.EvidenceHash},
		{Key: uint64(15), Val: a.AdvisoryNote},
		{Key: uint64(16), Val: uint64(a.Timestamp)},
	}
}

// EncodeAttestationBodyCBOR encodes the unsigned canonical RFC 8949 CBOR attestation body.
func EncodeAttestationBodyCBOR(a Attestation) ([]byte, error) {
	if err := validateAttestationBody(&a); err != nil {
		return nil, err
	}
	return core.EncodeCBOR(attestationBodyPairs(a))
}

// AttestationDigest returns DSHA256(TagRepAttestV1 || CanonicalCBOR(body)).
func AttestationDigest(a Attestation) ([]byte, error) {
	if err := validateAttestationBody(&a); err != nil {
		return nil, err
	}
	return core.HashCBOR(core.TagRepAttestV1, attestationBodyPairs(a))
}

// SignAttestation canonicalizes a, computes its CBOR digest, and signs it via the wallet digest signer.
func SignAttestation(a *Attestation, issuerPubHex string, signDigest func(digest []byte) ([]byte, error)) error {
	if a == nil || signDigest == nil {
		return errors.New("reputation: attestation and signer are required")
	}
	if a.Version == 0 {
		a.Version = 1
	}
	if issuerPubHex != "" {
		a.IssuerPub = issuerPubHex
	}
	if strings.TrimSpace(a.AdvisoryNote) == "" {
		a.AdvisoryNote = AdvisoryReputationNote
	}
	if err := validateAttestationBody(a); err != nil {
		return err
	}
	digest, err := AttestationDigest(*a)
	if err != nil {
		return err
	}
	sig, err := signDigest(digest)
	if err != nil {
		return fmt.Errorf("reputation: sign attestation: %w", err)
	}
	a.SigHex = hex.EncodeToString(sig)
	a.AttestHash = hex.EncodeToString(digest)
	return nil
}

// EncodeAttestationToken serializes a signed Attestation into a compact `nektarrep1.<base64url>` canonical CBOR token.
func EncodeAttestationToken(a Attestation) (string, error) {
	if err := validateAttestationBody(&a); err != nil {
		return "", err
	}
	sigBytes, err := hex.DecodeString(strings.TrimSpace(a.SigHex))
	if err != nil || len(sigBytes) == 0 {
		return "", errors.New("reputation: signed attestation requires valid sig hex")
	}
	m := attestationBodyPairs(a)
	m = append(m, cbor.Pair{Key: uint64(17), Val: sigBytes})
	if tok := strings.TrimSpace(a.DealReceiptToken); tok != "" {
		m = append(m, cbor.Pair{Key: uint64(18), Val: tok})
	}
	raw, err := core.EncodeCBOR(m)
	if err != nil {
		return "", err
	}
	return TokenPrefix + base64.RawURLEncoding.EncodeToString(raw), nil
}

// DecodeAttestationToken strictly decodes a `nektarrep1.<base64url>` canonical CBOR token.
func DecodeAttestationToken(token string) (*Attestation, error) {
	tok := strings.TrimSpace(token)
	if !strings.HasPrefix(tok, TokenPrefix) {
		return nil, fmt.Errorf("reputation: token missing %q prefix", TokenPrefix)
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(tok, TokenPrefix))
	if err != nil {
		return nil, fmt.Errorf("reputation: decode token base64: %w", err)
	}
	val, err := core.DecodeCBOR(raw)
	if err != nil {
		return nil, fmt.Errorf("reputation: decode canonical cbor: %w", err)
	}
	um, err := core.CBORMapToUintMap(val)
	if err != nil {
		return nil, fmt.Errorf("reputation: attestation map: %w", err)
	}
	for k := range um {
		if k < 1 || k > 18 {
			return nil, fmt.Errorf("reputation: unexpected cbor key %d", k)
		}
	}
	getStr := func(k uint64) (string, error) {
		v, ok := um[k]
		if !ok {
			return "", nil
		}
		s, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("reputation: key %d expected text", k)
		}
		return s, nil
	}
	getUint := func(k uint64) (uint64, error) {
		v, ok := um[k]
		if !ok {
			return 0, nil
		}
		u, ok := v.(uint64)
		if !ok {
			return 0, fmt.Errorf("reputation: key %d expected uint64", k)
		}
		return u, nil
	}

	ver, err := getUint(1)
	if err != nil {
		return nil, err
	}
	role, err := getStr(2)
	if err != nil {
		return nil, err
	}
	issuer, err := getStr(3)
	if err != nil {
		return nil, err
	}
	subject, err := getStr(4)
	if err != nil {
		return nil, err
	}
	ontID, err := getStr(5)
	if err != nil {
		return nil, err
	}
	dealID, err := getStr(6)
	if err != nil {
		return nil, err
	}
	drHash, err := getStr(7)
	if err != nil {
		return nil, err
	}
	acc, err := getUint(8)
	if err != nil {
		return nil, err
	}
	cons, err := getUint(9)
	if err != nil {
		return nil, err
	}
	disp, err := getUint(10)
	if err != nil {
		return nil, err
	}
	pred, err := getUint(11)
	if err != nil {
		return nil, err
	}
	collab, err := getUint(12)
	if err != nil {
		return nil, err
	}
	prov, err := getUint(13)
	if err != nil {
		return nil, err
	}
	evHash, err := getStr(14)
	if err != nil {
		return nil, err
	}
	note, err := getStr(15)
	if err != nil {
		return nil, err
	}
	ts, err := getUint(16)
	if err != nil {
		return nil, err
	}
	sigVal, ok := um[17]
	if !ok {
		return nil, errors.New("reputation: missing signature field")
	}
	sigBytes, ok := sigVal.([]byte)
	if !ok || len(sigBytes) == 0 {
		return nil, errors.New("reputation: key 17 expected non-empty signature bytes")
	}
	drTok, err := getStr(18)
	if err != nil {
		return nil, err
	}

	a := &Attestation{
		Version:          ver,
		Role:             role,
		IssuerPub:        issuer,
		SubjectPub:       subject,
		OntologyID:       ontID,
		DealID:           dealID,
		DealReceiptHash:  drHash,
		DealReceiptToken: drTok,
		Scores: Vector6D{
			SubjectPub: subject,
			OntologyID: ontID,
			AccBP:      int64(acc),
			ConsBP:     int64(cons),
			DispBP:     int64(disp),
			PredBP:     int64(pred),
			CollabBP:   int64(collab),
			ProvBP:     int64(prov),
			UpdatedAt:  int64(ts),
		},
		EvidenceHash: evHash,
		AdvisoryNote: note,
		Timestamp:    int64(ts),
		SigHex:       hex.EncodeToString(sigBytes),
	}
	if err := validateAttestationBody(a); err != nil {
		return nil, err
	}
	digest, err := AttestationDigest(*a)
	if err != nil {
		return nil, err
	}
	a.AttestHash = hex.EncodeToString(digest)
	return a, nil
}

// VerifyAttestation cryptographically verifies an attestation's signature, schema invariants,
// anti-self-rating invariant, and bilateral deal receipt binding (fail-closed if unbound).
// Callers may supply either knownBinding (from a locally verified settled deal) or verifyDeal
// (to verify the embedded nektardr1.* token offline).
func VerifyAttestation(a *Attestation, verifyDeal DealReceiptVerifier, knownBinding *DealReceiptBinding) (*DealReceiptBinding, error) {
	if err := validateAttestationBody(a); err != nil {
		return nil, err
	}
	if strings.TrimSpace(a.AdvisoryNote) == "" {
		return nil, errors.New("reputation: missing honesty advisory note")
	}
	pub, _, err := core.ParseStrictCompressedPubKey(a.IssuerPub)
	if err != nil {
		return nil, fmt.Errorf("reputation: invalid issuerPub: %w", err)
	}
	sig, err := core.ParseStrictLowSDER(a.SigHex)
	if err != nil {
		return nil, fmt.Errorf("reputation: invalid low-S DER signature: %w", err)
	}
	digest, err := AttestationDigest(*a)
	if err != nil {
		return nil, err
	}
	if !sig.Verify(digest, pub) {
		return nil, errors.New("reputation: attestation signature verification failed")
	}
	a.AttestHash = hex.EncodeToString(digest)

	var binding *DealReceiptBinding
	if knownBinding != nil {
		cp := *knownBinding
		binding = &cp
	} else if verifyDeal != nil && strings.TrimSpace(a.DealReceiptToken) != "" {
		b, bErr := verifyDeal(a.DealReceiptToken)
		if bErr != nil {
			return nil, fmt.Errorf("reputation: bilateral deal receipt invalid: %w", bErr)
		}
		binding = b
	} else {
		return nil, errors.New("reputation: attestation rejected — no verified bilateral deal receipt (nektardr1.*) binding")
	}

	if binding.DealID == "" || !strings.EqualFold(binding.DealID, a.DealID) {
		return nil, fmt.Errorf("reputation: dealId mismatch (%q != %q)", a.DealID, binding.DealID)
	}
	if binding.ReceiptHash == "" || !strings.EqualFold(binding.ReceiptHash, a.DealReceiptHash) {
		return nil, fmt.Errorf("reputation: dealReceiptHash mismatch (%s != %s)", a.DealReceiptHash, binding.ReceiptHash)
	}
	switch a.Role {
	case RoleBuyerRatesSeller:
		if !strings.EqualFold(a.IssuerPub, binding.BuyerPub) || !strings.EqualFold(a.SubjectPub, binding.SellerPub) {
			return nil, errors.New("reputation: buyer_rates_seller issuer/subject do not match bilateral deal receipt")
		}
	case RoleSellerRatesBuyer:
		if !strings.EqualFold(a.IssuerPub, binding.SellerPub) || !strings.EqualFold(a.SubjectPub, binding.BuyerPub) {
			return nil, errors.New("reputation: seller_rates_buyer issuer/subject do not match bilateral deal receipt")
		}
	default:
		return nil, fmt.Errorf("reputation: unsupported role %q", a.Role)
	}
	if bOnt := NormalizeOntologyID(binding.OntologyID); bOnt != GlobalOntologyID && a.OntologyID != GlobalOntologyID && bOnt != a.OntologyID {
		return nil, fmt.Errorf("reputation: ontologyId mismatch (%s != %s)", a.OntologyID, bOnt)
	}
	return binding, nil
}
