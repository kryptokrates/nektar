package reasoning

import (
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

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
	Grounded bool   `json:"grounded"`
	Reason   string `json:"reason"`
}

// GroundingReport summarizes claim-by-claim grounding.
type GroundingReport struct {
	Claims     []GroundedClaim `json:"claims"`
	Kept       []Claim         `json:"kept"`
	Flagged    []GroundedClaim `json:"flagged"`
	Mode       string          `json:"mode"` // flag | remove
	ReportHash string          `json:"reportHash"`
}

// FragmentFetcher resolves (shard, seq) to verified plaintext text.
type FragmentFetcher func(ref FragmentRef) (verified bool, text string)

func normText(s string) string {
	return strings.ToLower(strings.Join(strings.Fields(s), " "))
}

// Ground verifies every claim against verified cited fragments via:
//  1. exact normalized quote substring,
//  2. Google Mangle Datalog entailment (when Claim.Literal is provided), or
//  3. quote-backed synthesis (verbatim substrings >= 3 chars).
func Ground(claims []Claim, fetch FragmentFetcher, mode string, rules []string) (*GroundingReport, error) {
	if mode == "" {
		mode = "remove"
	}
	out := make([]GroundedClaim, 0, len(claims))
	var kept []Claim
	var flagged []GroundedClaim

	for _, c := range claims {
		if len(c.Cites) == 0 && len(c.Quotes) == 0 {
			gc := GroundedClaim{Claim: c, Grounded: false, Reason: "uncited"}
			out = append(out, gc)
			flagged = append(flagged, gc)
			continue
		}
		var verifiedTexts []string
		for _, ref := range c.Cites {
			ok, txt := fetch(ref)
			if ok && txt != "" {
				verifiedTexts = append(verifiedTexts, txt)
			}
		}
		// 1. Exact substring quote.
		nt := normText(c.Text)
		quoted := false
		for _, vt := range verifiedTexts {
			if nt != "" && strings.Contains(normText(vt), nt) {
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
		// 2. Mangle Datalog entailment.
		if len(verifiedTexts) > 0 && c.Literal != "" {
			var facts []string
			for _, vt := range verifiedTexts {
				for _, line := range strings.Split(vt, "\n") {
					line = strings.TrimSpace(line)
					if strings.Contains(line, "(") && strings.Contains(line, ")") {
						facts = append(facts, line)
					}
				}
			}
			entailed, err := Entails(facts, rules, c.Literal)
			if err == nil && entailed {
				gc := GroundedClaim{Claim: c, Grounded: true, Reason: "grounded:entailed"}
				out = append(out, gc)
				kept = append(kept, c)
				continue
			}
		}
		// 3. Quote-backed synthesis.
		if len(c.Quotes) > 0 {
			allVer := true
			allMatch := true
			for _, q := range c.Quotes {
				ok, txt := fetch(FragmentRef{Shard: q.Shard, Seq: q.Seq})
				if !ok || txt == "" {
					allVer = false
					break
				}
				nq := normText(q.Quote)
				if len(nq) < 3 || !strings.Contains(normText(txt), nq) {
					allMatch = false
				}
			}
			if !allVer {
				gc := GroundedClaim{Claim: c, Grounded: false, Reason: "citation-unverifiable"}
				out = append(out, gc)
				flagged = append(flagged, gc)
				continue
			}
			if allMatch {
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
		if len(verifiedTexts) == 0 {
			reason = "citation-unverifiable"
		}
		gc := GroundedClaim{Claim: c, Grounded: false, Reason: reason}
		out = append(out, gc)
		flagged = append(flagged, gc)
	}

	if mode == "flag" {
		kept = claims
	}
	reportHash, err := core.HashOf(map[string]any{"mode": mode, "claims": out})
	if err != nil {
		return nil, err
	}
	return &GroundingReport{
		Claims:     out,
		Kept:       kept,
		Flagged:    flagged,
		Mode:       mode,
		ReportHash: reportHash,
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
}

// Receipt is the committed, auditable answer artifact hashed into nektarr1.<token>.
type Receipt struct {
	Answer              string          `json:"answer"`
	FragmentRefs        []FragmentRef   `json:"fragmentRefs"`
	CheckResult         string          `json:"checkResult"` // verified | unverifiable | failed
	ModelID             ModelIdentity   `json:"modelId"`
	Timestamp           int64           `json:"timestamp"`
	GroundingReportHash string          `json:"groundingReportHash,omitempty"`
	GroundingTier       string          `json:"groundingTier,omitempty"`
	TruthNote           string          `json:"truthNote"`
	Delegations         []Delegation    `json:"delegations,omitempty"`
	Payment             *PaymentSummary `json:"payment,omitempty"`
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

// MakeDelegation creates a §16 delegation signature over (delegator || decision || answer).
func MakeDelegation(key *ec.PrivateKey, decision, answer string) (Delegation, error) {
	delegator := hex.EncodeToString(key.PubKey().Compressed())
	msg := core.DSHA256(core.Concat(core.TagDelegation, []byte(delegator), []byte(decision), []byte(answer)))
	sig, err := key.Sign(msg)
	if err != nil {
		return Delegation{}, err
	}
	return Delegation{
		Delegator: delegator,
		Decision:  decision,
		Signature: hex.EncodeToString(sig.Serialize()),
	}, nil
}

// VerifyDelegations checks every delegation signature on a receipt.
func VerifyDelegations(r Receipt) error {
	for _, d := range r.Delegations {
		pubBytes, err := hex.DecodeString(d.Delegator)
		if err != nil {
			return err
		}
		pub, err := ec.ParsePubKey(pubBytes)
		if err != nil {
			return err
		}
		sigBytes, err := hex.DecodeString(d.Signature)
		if err != nil {
			return err
		}
		sig, err := ec.ParseDERSignature(sigBytes)
		if err != nil {
			return err
		}
		msg := core.DSHA256(core.Concat(core.TagDelegation, []byte(d.Delegator), []byte(d.Decision), []byte(r.Answer)))
		if !sig.Verify(msg, pub) {
			return fmt.Errorf("reasoning: invalid delegation signature for %q", d.Decision)
		}
	}
	return nil
}

// AORG — Append-Only Reasoning Graph (bi-temporal, acyclic, Mangle-justified).

type Vertex struct {
	Kind    string   `json:"kind"` // episode | axiom | empirical | derived | refinement
	Phi     string   `json:"phi"`
	LogPos  int64    `json:"logPos"`
	Height  uint32   `json:"height,omitempty"`
	Context []string `json:"context"`
	Sigma   []string `json:"sigma,omitempty"`
	H       string   `json:"h"`
	Refines string   `json:"refines,omitempty"`
}

type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
	Rule string `json:"rule"`
	E    string `json:"e"`
}

type AORGraph struct {
	mu        sync.RWMutex
	vertices  map[string]Vertex
	edges     []Edge
	invalidAt map[string]int64
	rules     []string
}

func NewAORGraph(rules []string) *AORGraph {
	return &AORGraph{
		vertices:  make(map[string]Vertex),
		invalidAt: make(map[string]int64),
		rules:     append([]string(nil), rules...),
	}
}

func vertexHash(v Vertex) string {
	s := fmt.Sprintf("%s|%s|%d:%d|%s", v.Phi, strings.Join(v.Sigma, ","), v.LogPos, v.Height, strings.Join(v.Context, ","))
	return hex.EncodeToString(core.DSHA256([]byte(s)))
}

func (g *AORGraph) ancestorsLocked(h string, out map[string]bool) {
	if out[h] {
		return
	}
	out[h] = true
	for _, e := range g.edges {
		if e.To == h {
			g.ancestorsLocked(e.From, out)
		}
	}
}

// Append enforces acyclicity, Mangle justifiability (for derived vertices),
// episode traceability, and bi-temporal refinement invalidation.
func (g *AORGraph) Append(v Vertex, premises []string, ruleTag string) (Vertex, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, p := range premises {
		if _, ok := g.vertices[p]; !ok {
			return Vertex{}, fmt.Errorf("aorg: unknown premise %s", p)
		}
	}
	v.H = vertexHash(v)
	if _, dup := g.vertices[v.H]; dup {
		return Vertex{}, errors.New("aorg: duplicate vertex")
	}
	if v.Kind == "derived" {
		premFacts := make([]string, 0, len(premises))
		closure := make(map[string]bool)
		for _, p := range premises {
			premFacts = append(premFacts, g.vertices[p].Phi)
			g.ancestorsLocked(p, closure)
		}
		hasEpisode := false
		for h := range closure {
			if g.vertices[h].Kind == "episode" {
				hasEpisode = true
				break
			}
		}
		if !hasEpisode {
			return Vertex{}, errors.New("aorg: traceability violated (no episode ancestor)")
		}
		ok, err := Entails(premFacts, g.rules, v.Phi)
		if err != nil || !ok {
			return Vertex{}, fmt.Errorf("aorg: Mangle rule set does not justify %q (err=%v)", v.Phi, err)
		}
	}
	if v.Kind == "refinement" {
		if v.Refines == "" {
			return Vertex{}, errors.New("aorg: refinement missing refines target")
		}
		g.invalidAt[v.Refines] = v.LogPos
	}
	g.vertices[v.H] = v
	if ruleTag == "" {
		ruleTag = "assert"
	}
	for _, p := range premises {
		eHash := hex.EncodeToString(core.DSHA256([]byte(p + "|" + v.H + "|" + ruleTag)))
		g.edges = append(g.edges, Edge{From: p, To: v.H, Rule: ruleTag, E: eHash})
	}
	return v, nil
}

// CurrentView returns all vertices valid at logical time atLogPos (bi-temporal projection).
func (g *AORGraph) CurrentView(atLogPos int64) []Vertex {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Vertex, 0, len(g.vertices))
	for _, v := range g.vertices {
		if v.LogPos > atLogPos {
			continue
		}
		if inv, has := g.invalidAt[v.H]; has && inv <= atLogPos {
			continue
		}
		out = append(out, v)
	}
	return out
}
