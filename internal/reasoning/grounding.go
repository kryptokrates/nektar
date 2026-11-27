package reasoning

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/core"
)

const (
	TruthNote          = "Proofs show a cited record was committed, is unaltered and existed by its block time; they do not show the record is true."
	ReceiptTokenPrefix = "nektarr1."
)

// FragmentRef identifies a cited record by (shard, seq).
type FragmentRef struct {
	Shard string `json:"shard"`
	Seq   int64  `json:"seq"`
}

// QuoteCite names a verbatim substring inside a verified fragment.
type QuoteCite struct {
	Quote string `json:"quote"`
	Shard string `json:"shard"`
	Seq   int64  `json:"seq"`
}

// Claim is one atomic assertion in a model's answer.
type Claim struct {
	Text    string        `json:"text"`
	Literal string        `json:"literal,omitempty"` // optional Mangle Datalog goal atom
	Cites   []FragmentRef `json:"cites"`
	Quotes  []QuoteCite   `json:"quotes,omitempty"`
}

// GroundedClaim is the grounding verdict for one Claim.
type GroundedClaim struct {
	Claim
	Grounded   bool            `json:"grounded"`
	Reason     string          `json:"reason"`
	Derivation *DerivationStep `json:"derivation,omitempty"`
}

// GroundingReport summarizes claim-by-claim grounding.
type GroundingReport struct {
	Claims      []GroundedClaim  `json:"claims"`
	Kept        []Claim          `json:"kept"`
	Flagged     []GroundedClaim  `json:"flagged"`
	Derivations []DerivationStep `json:"derivations,omitempty"`
	Mode        string           `json:"mode"` // flag | remove
	ReportHash  string           `json:"reportHash"`
}

// FragmentFetcher resolves (shard, seq) to verified plaintext text.
type FragmentFetcher func(ref FragmentRef) (verified bool, text string)

// DetailedFragmentFetcher resolves (shard, seq) to verified plaintext text AND its committed HDP hex.
type DetailedFragmentFetcher func(ref FragmentRef) (verified bool, text string, hdpHex string)

func normText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

var quoteStopwords = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "by": true, "for": true, "from": true, "has": true, "have": true,
	"in": true, "is": true, "it": true, "its": true, "of": true, "on": true,
	"or": true, "that": true, "the": true, "this": true, "to": true, "was": true,
	"were": true, "will": true, "with": true,
}

func contentTokens(s string) []string {
	var out []string
	var cur []rune
	flush := func() {
		if len(cur) > 0 {
			tok := strings.ToLower(string(cur))
			if len(tok) >= 2 && !quoteStopwords[tok] {
				out = append(out, tok)
			}
			cur = cur[:0]
		}
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' {
			cur = append(cur, r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// isMeaningfulQuote requires either >=12 normalized chars or >=2 non-stopword content tokens
// so trivial substrings like "the" or "2025" cannot rubber-stamp unrelated claims.
func isMeaningfulQuote(nq string) bool {
	if len(nq) >= 12 {
		return len(contentTokens(nq)) >= 1
	}
	return len(contentTokens(nq)) >= 2
}

// claimSupportedByQuotes checks that Claim.Text shares meaningful lexical overlap with
// the verified verbatim quotes (at least 50% of unique claim content tokens, min 1).
func claimSupportedByQuotes(claimText string, verifiedQuotes []string) bool {
	qSet := make(map[string]bool)
	for _, q := range verifiedQuotes {
		for _, tok := range contentTokens(q) {
			qSet[tok] = true
		}
	}
	if len(qSet) == 0 {
		return false
	}
	cToks := contentTokens(claimText)
	if len(cToks) == 0 {
		return false
	}
	seen := make(map[string]bool, len(cToks))
	matched := 0
	for _, tok := range cToks {
		if seen[tok] {
			continue
		}
		seen[tok] = true
		if qSet[tok] {
			matched++
		}
	}
	return matched*2 >= len(seen)
}

// Ground verifies every claim against verified cited fragments via:
//  1. exact normalized quote substring,
//  2. Google Mangle Datalog entailment (when Claim.Literal is provided; strictly parsed ground atoms only), or
//  3. quote-backed synthesis (meaningful verbatim quotes with lexical overlap against Claim.Text).
func Ground(claims []Claim, fetch FragmentFetcher, mode string, rules []string) (*GroundingReport, error) {
	crules := make([]CommittedRule, 0, len(rules))
	for _, r := range rules {
		if s := strings.TrimSpace(r); s != "" {
			crules = append(crules, CommittedRule{RuleText: s, RuleHash: ComputeRuleHash(s)})
		}
	}
	return GroundWithProofs(claims, func(ref FragmentRef) (bool, string, string) {
		ok, txt := fetch(ref)
		return ok, txt, ""
	}, mode, crules)
}

// GroundWithProofs verifies every claim and attaches minimal, offline-verifiable DerivationStep
// proof trees whenever Mangle/ontology entailment justifies a claim (Paper §8).
func GroundWithProofs(claims []Claim, fetch DetailedFragmentFetcher, mode string, rules []CommittedRule) (*GroundingReport, error) {
	if mode == "" {
		mode = "remove"
	}
	out := make([]GroundedClaim, 0, len(claims))
	var kept []Claim
	var flagged []GroundedClaim
	var derivations []DerivationStep

	for _, c := range claims {
		if len(c.Cites) == 0 && len(c.Quotes) == 0 {
			gc := GroundedClaim{Claim: c, Grounded: false, Reason: "uncited"}
			out = append(out, gc)
			flagged = append(flagged, gc)
			continue
		}
		type fetchedFrag struct {
			ref  FragmentRef
			text string
			hdp  string
		}
		var verifiedFrags []fetchedFrag
		for _, ref := range c.Cites {
			ok, txt, hdp := fetch(ref)
			if ok && txt != "" {
				verifiedFrags = append(verifiedFrags, fetchedFrag{ref: ref, text: txt, hdp: hdp})
			}
		}
		// 1. Exact substring quote.
		nt := normText(c.Text)
		quoted := false
		for _, vf := range verifiedFrags {
			if nt != "" && strings.Contains(normText(vf.text), nt) {
				quoted = true
				break
			}
		}
		if quoted {
			gc := GroundedClaim{Claim: c, Grounded: true, Reason: "grounded:quote"}
			out = append(out, gc)
			kept = append(kept, c)
			continue
		}
		// 2. Proof-carrying Mangle Datalog + Triple entailment (strictly parsed ground atoms; never allows rule injection).
		if len(verifiedFrags) > 0 && c.Literal != "" {
			var facts []CommittedFact
			for _, vf := range verifiedFrags {
				for _, line := range strings.Split(vf.text, "\n") {
					if atom, ok := ParseGroundFact(line); ok {
						facts = append(facts, CommittedFact{Atom: atom.String(), Ref: vf.ref, HDP: vf.hdp})
					} else if tr, ok := ParseNTripleLine(line); ok {
						if atoms, aErr := tr.ToMangleAtoms(); aErr == nil {
							for _, a := range atoms {
								facts = append(facts, CommittedFact{Atom: a, Ref: vf.ref, HDP: vf.hdp})
							}
						}
					}
				}
			}
			if len(facts) > 0 {
				step, entailed, err := EntailsWithProof(facts, rules, c.Literal)
				if err == nil && entailed {
					gc := GroundedClaim{Claim: c, Grounded: true, Reason: "grounded:entailed", Derivation: step}
					out = append(out, gc)
					kept = append(kept, c)
					if step != nil {
						derivations = append(derivations, *step)
					}
					continue
				}
			}
		}
		// 3. Quote-backed synthesis (meaningful quote + lexical overlap with Claim.Text).
		if len(c.Quotes) > 0 {
			allVer := true
			allMatch := true
			verifiedQuotes := make([]string, 0, len(c.Quotes))
			for _, q := range c.Quotes {
				ok, txt, _ := fetch(FragmentRef{Shard: q.Shard, Seq: q.Seq})
				if !ok || txt == "" {
					allVer = false
					break
				}
				nq := normText(q.Quote)
				if !isMeaningfulQuote(nq) || !strings.Contains(normText(txt), nq) {
					allMatch = false
					continue
				}
				verifiedQuotes = append(verifiedQuotes, nq)
			}
			if !allVer {
				gc := GroundedClaim{Claim: c, Grounded: false, Reason: "citation-unverifiable"}
				out = append(out, gc)
				flagged = append(flagged, gc)
				continue
			}
			if allMatch && claimSupportedByQuotes(c.Text, verifiedQuotes) {
				gc := GroundedClaim{Claim: c, Grounded: true, Reason: "grounded:quote-backed"}
				out = append(out, gc)
				kept = append(kept, c)
				continue
			}
			gc := GroundedClaim{Claim: c, Grounded: false, Reason: "quote-mismatch"}
			out = append(out, gc)
			flagged = append(flagged, gc)
			continue
		}
		reason := "not-entailed"
		if len(verifiedFrags) == 0 {
			reason = "citation-unverifiable"
		}
		gc := GroundedClaim{Claim: c, Grounded: false, Reason: reason}
		out = append(out, gc)
		flagged = append(flagged, gc)
	}

	reportHash, err := core.HashOf(map[string]any{"mode": "remove", "claims": out})
	if err != nil {
		return nil, err
	}
	return &GroundingReport{
		Claims:      out,
		Kept:        kept,
		Flagged:     flagged,
		Derivations: derivations,
		Mode:        "remove",
		ReportHash:  reportHash,
	}, nil
}

// ModelIdentity declares (not zkML-proves) the model that produced an answer.
type ModelIdentity struct {
	Kind          string `json:"kind"` // api | local
	Provider      string `json:"provider,omitempty"`
	Release       string `json:"release,omitempty"`
	WeightsSHA256 string `json:"weightsSha256,omitempty"`
	Name          string `json:"name,omitempty"`
}

// Delegation signs sub-agent authorship over a sub-decision bound to the final answer.
type Delegation struct {
	Version   int    `json:"v,omitempty"` // 2 = TagDelegationV2 framed; 0/1 = legacy v1
	Delegator string `json:"delegator"`
	Decision  string `json:"decision"`
	Signature string `json:"signature"`
}

// PaymentSummary records an auditable BSV settlement attached to a paid answer.
type PaymentSummary struct {
	TxID       string `json:"txid"`
	AmountSats uint64 `json:"amountSats"`
	PayTo      string `json:"payTo"`
	InvoiceID  string `json:"invoiceId"`
	BEEFHex    string `json:"beefHex,omitempty"`
}

// Receipt is the committed, auditable answer artifact hashed into nektarr1.<token>,
// binding cited ABox fragments, committed Cedar policy, committed __ontology__ TBox rules,
// and offline-verifiable DerivationStep proof witnesses.
type Receipt struct {
	Answer              string           `json:"answer"`
	FragmentRefs        []FragmentRef    `json:"fragmentRefs"`
	CheckResult         string           `json:"checkResult"` // verified | unverifiable | failed
	ModelID             ModelIdentity    `json:"modelId"`
	Timestamp           int64            `json:"timestamp"`
	GroundingReportHash string           `json:"groundingReportHash,omitempty"`
	GroundingTier       string           `json:"groundingTier,omitempty"`
	PolicyCommitHash    string           `json:"policyCommitHash,omitempty"`
	PolicyRef           *FragmentRef     `json:"policyRef,omitempty"`
	OntologyCommitHash  string           `json:"ontologyCommitHash,omitempty"`
	RuleRefs            []FragmentRef    `json:"ruleRefs,omitempty"`
	Derivations         []DerivationStep `json:"derivations,omitempty"`
	TruthNote           string           `json:"truthNote"`
	Delegations         []Delegation     `json:"delegations,omitempty"`
	Payment             *PaymentSummary  `json:"payment,omitempty"`
	IssuerPub           string           `json:"issuerPub,omitempty"`
	IssuerSig           string           `json:"issuerSig,omitempty"`
}

// ReceiptSignDigest computes the domain-separated digest signed by the issuing agent (TagReceiptSig).
func ReceiptSignDigest(r Receipt) ([]byte, error) {
	if r.TruthNote == "" {
		r.TruthNote = TruthNote
	}
	r.IssuerSig = ""
	canon, err := core.CanonicalJSON(r)
	if err != nil {
		return nil, err
	}
	return core.DSHA256(core.Concat(core.TagReceiptSig, canon)), nil
}

// VerifyReceiptSignature strictly validates IssuerSig against IssuerPub on r (unsigned receipts are rejected).
func VerifyReceiptSignature(r Receipt) error {
	if r.IssuerPub == "" || r.IssuerSig == "" {
		return errors.New("reasoning: receipt missing required issuer signature")
	}
	pub, _, err := core.ParseStrictCompressedPubKey(r.IssuerPub)
	if err != nil {
		return fmt.Errorf("reasoning: invalid receipt issuerPub: %w", err)
	}
	sig, err := core.ParseStrictLowSDER(r.IssuerSig)
	if err != nil {
		return fmt.Errorf("reasoning: invalid receipt issuerSig: %w", err)
	}
	digest, err := ReceiptSignDigest(r)
	if err != nil {
		return err
	}
	if !sig.Verify(digest, pub) {
		return errors.New("reasoning: invalid receipt issuer signature")
	}
	return nil
}

// ReceiptHash computes the canonical hash of r.
func ReceiptHash(r Receipt) (string, error) {
	if r.TruthNote == "" {
		r.TruthNote = TruthNote
	}
	return core.HashOf(r)
}

// ReceiptToken encodes a 32-byte hex receipt hash into `nektarr1.<base64url>`.
func ReceiptToken(receiptHashHex string) (string, error) {
	b, err := hex.DecodeString(receiptHashHex)
	if err != nil || len(b) != 32 {
		return "", errors.New("reasoning: receipt hash must be 32-byte hex")
	}
	return ReceiptTokenPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

// ParseReceiptToken decodes `nektarr1.<b64url>` or a bare 64-hex hash.
func ParseReceiptToken(tok string) (string, error) {
	t := strings.TrimSpace(tok)
	if len(t) == 64 {
		if _, err := hex.DecodeString(t); err == nil {
			return strings.ToLower(t), nil
		}
	}
	if !strings.HasPrefix(t, ReceiptTokenPrefix) {
		return "", fmt.Errorf("reasoning: invalid receipt token prefix")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(t, ReceiptTokenPrefix))
	if err != nil || len(raw) != 32 {
		return "", fmt.Errorf("reasoning: invalid receipt token encoding")
	}
	return hex.EncodeToString(raw), nil
}

func delegationDigestV2(delegator, decision, answer string) []byte {
	return core.DSHA256(core.Concat(
		core.TagDelegationV2,
		core.Field([]byte(delegator)),
		core.Field([]byte(decision)),
		core.Field([]byte(answer)),
	))
}

// VerifyDelegations checks every delegation signature on a receipt using strict compressed pubkey and low-S DER rules.
func VerifyDelegations(r Receipt) error {
	for _, d := range r.Delegations {
		pub, _, err := core.ParseStrictCompressedPubKey(d.Delegator)
		if err != nil {
			return err
		}
		sig, err := core.ParseStrictLowSDER(d.Signature)
		if err != nil {
			return err
		}
		if d.Version < 2 {
			return fmt.Errorf("reasoning: unsupported legacy delegation version %d (v2 required)", d.Version)
		}
		msg := delegationDigestV2(d.Delegator, d.Decision, r.Answer)
		if !sig.Verify(msg, pub) {
			return fmt.Errorf("reasoning: invalid delegation signature for %q", d.Decision)
		}
	}
	return nil
}
