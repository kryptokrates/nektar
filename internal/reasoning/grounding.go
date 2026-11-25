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
	unique := len(seen)
	if unique == 0 {
		return false
	}
	return matched*2 >= unique && matched >= 1
}

// Ground verifies every claim against verified cited fragments via:
//  1. exact normalized quote substring,
//  2. Google Mangle Datalog entailment (when Claim.Literal is provided; strictly parsed ground atoms only), or
//  3. quote-backed synthesis (meaningful verbatim quotes with lexical overlap against Claim.Text).
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
		// 2. Mangle Datalog entailment (strictly parsed ground atoms; never allows rule injection from fragments).
		if len(verifiedTexts) > 0 && c.Literal != "" {
			var facts []string
			for _, vt := range verifiedTexts {
				for _, line := range strings.Split(vt, "\n") {
					if atom, ok := ParseGroundFact(line); ok {
						facts = append(facts, atom.String())
					}
				}
			}
			if len(facts) > 0 {
				entailed, err := Entails(facts, rules, c.Literal)
				if err == nil && entailed {
					gc := GroundedClaim{Claim: c, Grounded: true, Reason: "grounded:entailed"}
					out = append(out, gc)
					kept = append(kept, c)
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
				ok, txt := fetch(FragmentRef{Shard: q.Shard, Seq: q.Seq})
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
	IssuerPub           string          `json:"issuerPub,omitempty"`
	IssuerSig           string          `json:"issuerSig,omitempty"`
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

// SignReceipt signs r with key under core.TagReceiptSig, populating IssuerPub and IssuerSig.
func SignReceipt(key *ec.PrivateKey, r *Receipt) error {
	if key == nil || r == nil {
		return errors.New("reasoning: nil key or receipt")
	}
	r.IssuerPub = hex.EncodeToString(key.PubKey().Compressed())
	digest, err := ReceiptSignDigest(*r)
	if err != nil {
		return err
	}
	sig, err := key.Sign(digest)
	if err != nil {
		return err
	}
	r.IssuerSig = hex.EncodeToString(sig.Serialize())
	return nil
}

// VerifyReceiptSignature validates IssuerSig against IssuerPub when present on r.
func VerifyReceiptSignature(r Receipt) error {
	if r.IssuerPub == "" && r.IssuerSig == "" {
		return nil
	}
	if r.IssuerPub == "" || r.IssuerSig == "" {
		return errors.New("reasoning: incomplete receipt issuer signature")
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

// MakeDelegation creates a §16 delegation signature over length-framed (delegator, decision, answer) under TagDelegationV2.
func MakeDelegation(key *ec.PrivateKey, decision, answer string) (Delegation, error) {
	delegator := hex.EncodeToString(key.PubKey().Compressed())
	msg := delegationDigestV2(delegator, decision, answer)
	sig, err := key.Sign(msg)
	if err != nil {
		return Delegation{}, err
	}
	return Delegation{
		Version:   2,
		Delegator: delegator,
		Decision:  decision,
		Signature: hex.EncodeToString(sig.Serialize()),
	}, nil
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
		var msg []byte
		if d.Version >= 2 {
			msg = delegationDigestV2(d.Delegator, d.Decision, r.Answer)
		} else {
			msg = core.DSHA256(core.Concat(core.TagDelegation, []byte(d.Delegator), []byte(d.Decision), []byte(r.Answer)))
		}
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

func frameStringSlice(items []string) []byte {
	parts := make([][]byte, 0, 1+len(items))
	parts = append(parts, core.U32LE(uint32(len(items))))
	for _, s := range items {
		parts = append(parts, core.Field([]byte(s)))
	}
	return core.Concat(parts...)
}

func vertexHash(v Vertex) string {
	buf := core.Concat(
		[]byte("nektar:aorg-vertex:v2"),
		core.Field([]byte(v.Kind)),
		core.Field([]byte(v.Phi)),
		core.U64LE(uint64(v.LogPos)),
		core.U32LE(v.Height),
		core.Field([]byte(v.Refines)),
		frameStringSlice(v.Sigma),
		frameStringSlice(v.Context),
	)
	return hex.EncodeToString(core.DSHA256(buf))
}

func edgeHash(from, to, ruleTag string) string {
	buf := core.Concat(
		[]byte("nektar:aorg-edge:v2"),
		core.Field([]byte(from)),
		core.Field([]byte(to)),
		core.Field([]byte(ruleTag)),
	)
	return hex.EncodeToString(core.DSHA256(buf))
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
		eHash := edgeHash(p, v.H, ruleTag)
		g.edges = append(g.edges, Edge{From: p, To: v.H, Rule: ruleTag, E: eHash})
	}
	return v, nil
}

// AORGSnapshot serializes an AORGraph for durable SQLite persistence across restarts.
type AORGSnapshot struct {
	Vertices  map[string]Vertex `json:"vertices"`
	Edges     []Edge            `json:"edges"`
	InvalidAt map[string]int64  `json:"invalidAt"`
	Rules     []string          `json:"rules"`
}

// AddRules appends Datalog rules to the graph without wiping existing vertices or edges.
func (g *AORGraph) AddRules(rules ...string) {
	g.mu.Lock()
	g.rules = append(g.rules, rules...)
	g.mu.Unlock()
}

// Snapshot returns a deep copy of the graph suitable for JSON persistence.
func (g *AORGraph) Snapshot() AORGSnapshot {
	g.mu.RLock()
	defer g.mu.RUnlock()
	verts := make(map[string]Vertex, len(g.vertices))
	for k, v := range g.vertices {
		verts[k] = v
	}
	inv := make(map[string]int64, len(g.invalidAt))
	for k, v := range g.invalidAt {
		inv[k] = v
	}
	return AORGSnapshot{
		Vertices:  verts,
		Edges:     append([]Edge(nil), g.edges...),
		InvalidAt: inv,
		Rules:     append([]string(nil), g.rules...),
	}
}

// Restore loads a persisted AORGSnapshot into g.
func (g *AORGraph) Restore(snap AORGSnapshot) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if snap.Vertices != nil {
		g.vertices = make(map[string]Vertex, len(snap.Vertices))
		for k, v := range snap.Vertices {
			g.vertices[k] = v
		}
	}
	g.edges = append([]Edge(nil), snap.Edges...)
	if snap.InvalidAt != nil {
		g.invalidAt = make(map[string]int64, len(snap.InvalidAt))
		for k, v := range snap.InvalidAt {
			g.invalidAt[k] = v
		}
	}
	if len(snap.Rules) > 0 {
		g.rules = append([]string(nil), snap.Rules...)
	}
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
