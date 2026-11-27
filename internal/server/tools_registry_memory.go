package server

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/lattice"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

// RememberToolInput is the canonical input schema shared by MCP, REST /v1/tools/remember, and A2A.
type RememberToolInput struct {
	Shard         string                `json:"shard,omitempty"`
	Repo          string                `json:"repo,omitempty"`
	Branch        string                `json:"branch,omitempty"`
	Path          string                `json:"path,omitempty"`
	MediaType     string                `json:"mediaType,omitempty"`
	Text          string                `json:"text,omitempty"`
	Data          string                `json:"data,omitempty"`
	BlobBase64    string                `json:"blobBase64,omitempty"`
	ChirpURL      string                `json:"chirpUrl,omitempty"`
	CommitMessage string                `json:"commitMessage,omitempty"`
	Source        string                `json:"source,omitempty"`
	Level          int                   `json:"level,omitempty"`
	Class          string                `json:"class,omitempty"`
	Public         bool                  `json:"public,omitempty"`
	Ontology       string                `json:"ontology,omitempty"`
	OntologyStrict bool                  `json:"ontologyStrict,omitempty"`
	BorgGroup      string                `json:"borgGroup,omitempty"`
	Modality       *core.Modality        `json:"modality,omitempty"`
	Refines        []core.RefinementEdge `json:"refines,omitempty"`
	Triples        []reasoning.Triple    `json:"triples,omitempty"`
	CallerPub      string                `json:"callerPub,omitempty"`
	Supersedes     int64                 `json:"supersedes,omitempty"`
	AutoAnchor     bool                  `json:"autoAnchor,omitempty"`
	FreshnessClass string                `json:"freshnessClass,omitempty"`
	ValidUntilUnix int64                 `json:"validUntilUnix,omitempty"`
	SubjectVersion string                `json:"subjectVersion,omitempty"`
	UsageRights    string                `json:"usageRights,omitempty"`
	Extra          map[string]string     `json:"extra,omitempty"`
}

// CommitOntologyInput commits DL-Lite / OWL 2 RL / deontic axioms into the __ontology__ system shard.
type CommitOntologyInput struct {
	Axioms     []reasoning.OntologyAxiom `json:"axioms"`
	AutoAnchor bool                      `json:"autoAnchor,omitempty"`
}

// LineageToolInput queries the verified cryptographic Knowledge DAG rooted at (shard, seq).
type LineageToolInput struct {
	Shard string `json:"shard"`
	Seq   int64  `json:"seq"`
}

// ProvisionClearanceInput derives a downward-only CLKA clearance bundle for (shard, maxLevel, domains).
type ProvisionClearanceInput struct {
	Shard           string               `json:"shard"`
	MaxLevel        int                  `json:"maxLevel"`
	Domains         []string             `json:"domains,omitempty"`
	DomainEdges     []lattice.DomainEdge `json:"domainEdges,omitempty"`
	RecipientPubHex string               `json:"recipientPubHex,omitempty"`
}

// RotateLatticeInput bumps a shard's CLKA epoch for forward secrecy.
type RotateLatticeInput struct {
	Shard string `json:"shard"`
}

// CompactProveInput constructs an O(log N) MMR CompactProofBundle against the head anchor UTXO.
type CompactProveInput struct {
	Shard string `json:"shard"`
	Seq   int64  `json:"seq"`
}

// ConsistencyProofInput constructs an O(log N) MMR append-only consistency proof between batch fromN and toN.
type ConsistencyProofInput struct {
	Shard string `json:"shard"`
	FromN int64  `json:"fromN"`
	ToN   int64  `json:"toN"`
}

// WantToolInput is the canonical input schema shared by MCP, REST /v1/tools/want, and A2A.
type WantToolInput struct {
	Domains            []string `json:"domains,omitempty"`
	Domain             string   `json:"domain,omitempty"`
	Question           string   `json:"question,omitempty"`
	Query              string   `json:"query,omitempty"`
	OntologyID         string   `json:"ontologyId,omitempty"`
	MaxSats            uint64   `json:"maxSats,omitempty"`
	MinGrade           string   `json:"minGrade,omitempty"`
	MaxSatsPer1kUnits  uint64   `json:"maxSatsPer1kUnits,omitempty"`
	FreshnessClass     string   `json:"freshnessClass,omitempty"`
	RequiredRights     string   `json:"requiredRights,omitempty"`
	PricingMode        string   `json:"pricingMode,omitempty"`
	TargetShard        string   `json:"targetShard,omitempty"`
	StandingTTLSeconds int64    `json:"standingTtlSeconds,omitempty"`
}

// PackToolInput is the canonical input schema for exportPack and importPack.
type PackToolInput struct {
	Shard       string `json:"shard,omitempty"`
	ChirpURL    string `json:"chirpUrl,omitempty"`
	TargetShard string `json:"targetShard,omitempty"`
	PeerURL     string `json:"peerUrl,omitempty"`
}

// AuthorizeToolInput is the canonical input schema for the authorize tool.
type AuthorizeToolInput struct {
	Principal policy.Principal `json:"principal"`
	Action    string           `json:"action"`
	Resource  policy.Resource  `json:"resource"`
}

// ExecRemember commits a provenance-signed memory fragment/file, preserving Hermes/Claude/REST aliases.
func (tb *Toolbox) ExecRemember(ctx context.Context, raw RememberToolInput) (*memory.RememberResult, error) {
	var blob []byte
	if strings.TrimSpace(raw.BlobBase64) != "" {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(raw.BlobBase64))
		if err != nil {
			return nil, fmt.Errorf("invalid blobBase64: %w", err)
		}
		blob = dec
	}
	shard := raw.Shard
	if shard == "" {
		shard = raw.Repo
	}
	txt := raw.Text
	if txt == "" && raw.Data != "" {
		txt = raw.Data // Hermes NektarMemoryProvider uses `data`
	}
	in := memory.RememberInput{
		Shard:          shard,
		Branch:         raw.Branch,
		Path:           raw.Path,
		MediaType:      raw.MediaType,
		Text:           txt,
		Blob:           blob,
		ChirpURL:       raw.ChirpURL,
		CommitMessage:  raw.CommitMessage,
		Source:         raw.Source,
		Level:          raw.Level,
		Class:          raw.Class,
		Public:         raw.Public,
		Ontology:       raw.Ontology,
		OntologyStrict: raw.OntologyStrict,
		BorgGroup:      raw.BorgGroup,
		Modality:       raw.Modality,
		Refines:        raw.Refines,
		Triples:        raw.Triples,
		CallerPub:      raw.CallerPub,
		Supersedes:     raw.Supersedes,
		AutoAnchor:     raw.AutoAnchor,
		FreshnessClass: raw.FreshnessClass,
		ValidUntilUnix: raw.ValidUntilUnix,
		SubjectVersion: raw.SubjectVersion,
		UsageRights:    raw.UsageRights,
		Extra:          raw.Extra,
	}
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		p := cc.Principal
		in.Principal = &p
		in.CallerPub = ""
		if cc.DisallowBorg && in.BorgGroup != "" {
			return nil, errors.New("operator token required for Borg group writes")
		}
		if !cc.IsOperator && in.AutoAnchor {
			return nil, errors.New("operator token required for autoAnchor")
		}
	}
	return tb.mem.Remember(ctx, in)
}

func (tb *Toolbox) callRememberTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var wire struct {
		Shard          string                `json:"shard,omitempty"`
		Repo           string                `json:"repo,omitempty"`
		Branch         string                `json:"branch,omitempty"`
		Path           string                `json:"path,omitempty"`
		MediaType      string                `json:"mediaType,omitempty"`
		Text           string                `json:"text,omitempty"`
		Data           string                `json:"data,omitempty"`
		BlobBase64     string                `json:"blobBase64,omitempty"`
		ChirpURL       string                `json:"chirpUrl,omitempty"`
		CommitMessage  string                `json:"commitMessage,omitempty"`
		Source         string                `json:"source,omitempty"`
		Level          int                   `json:"level,omitempty"`
		Class          string                `json:"class,omitempty"`
		Public         bool                  `json:"public,omitempty"`
		Ontology       string                `json:"ontology,omitempty"`
		OntologyStrict bool                  `json:"ontologyStrict,omitempty"`
		BorgGroup      string                `json:"borgGroup,omitempty"`
		Modality       *core.Modality        `json:"modality,omitempty"`
		Refines        []core.RefinementEdge `json:"refines,omitempty"`
		Triples        []reasoning.Triple    `json:"triples,omitempty"`
		CallerPub      string                `json:"callerPub,omitempty"`
		Supersedes     json.RawMessage       `json:"supersedes,omitempty"`
		AutoAnchor     bool                  `json:"autoAnchor,omitempty"`
		FreshnessClass string                `json:"freshnessClass,omitempty"`
		ValidUntilUnix int64                 `json:"validUntilUnix,omitempty"`
		SubjectVersion string                `json:"subjectVersion,omitempty"`
		UsageRights    string                `json:"usageRights,omitempty"`
		Extra          map[string]string     `json:"extra,omitempty"`
	}
	if err := json.Unmarshal(rawArgs, &wire); err != nil {
		return nil, err
	}
	in := RememberToolInput{
		Shard:          wire.Shard,
		Repo:           wire.Repo,
		Branch:         wire.Branch,
		Path:           wire.Path,
		MediaType:      wire.MediaType,
		Text:           wire.Text,
		Data:           wire.Data,
		BlobBase64:     wire.BlobBase64,
		ChirpURL:       wire.ChirpURL,
		CommitMessage:  wire.CommitMessage,
		Source:         wire.Source,
		Level:          wire.Level,
		Class:          wire.Class,
		Public:         wire.Public,
		Ontology:       wire.Ontology,
		OntologyStrict: wire.OntologyStrict,
		BorgGroup:      wire.BorgGroup,
		Modality:       wire.Modality,
		Refines:        wire.Refines,
		Triples:        wire.Triples,
		CallerPub:      wire.CallerPub,
		AutoAnchor:     wire.AutoAnchor,
		FreshnessClass: wire.FreshnessClass,
		ValidUntilUnix: wire.ValidUntilUnix,
		SubjectVersion: wire.SubjectVersion,
		UsageRights:    wire.UsageRights,
		Extra:          wire.Extra,
	}
	if len(wire.Supersedes) > 0 && string(wire.Supersedes) != "null" {
		var seq int64
		if err := json.Unmarshal(wire.Supersedes, &seq); err == nil {
			in.Supersedes = seq
		} else {
			var obj struct {
				Shard string `json:"shard"`
				Seq   int64  `json:"seq"`
			}
			if err := json.Unmarshal(wire.Supersedes, &obj); err == nil {
				in.Supersedes = obj.Seq
				if in.Shard == "" && obj.Shard != "" {
					in.Shard = obj.Shard
				}
			}
		}
	}
	return tb.ExecRemember(ctx, in)
}

func (tb *Toolbox) callRecallTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in memory.RecallInput
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecRecall(ctx, in)
}

// ExecForget shreds local plaintext/ciphertext of a fragment with Cedar authorization.
func (tb *Toolbox) ExecForget(ctx context.Context, shard string, seq int64) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		if err := tb.mem.ForgetAuthorized(ctx, shard, seq, cc.Principal); err != nil {
			return nil, err
		}
	} else if err := tb.mem.Forget(ctx, shard, seq); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "shard": shard, "seq": seq}, nil
}

func (tb *Toolbox) callForgetTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in struct {
		Shard string `json:"shard"`
		Seq   int64  `json:"seq"`
	}
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecForget(ctx, in.Shard, in.Seq)
}

// ExecGroundedAnswer verifies claims against Cedar-authorized fragments and emits a receipt.
func (tb *Toolbox) ExecGroundedAnswer(ctx context.Context, in memory.GroundedAnswerInput) (*memory.GroundedAnswerResult, error) {
	cc := CallerContextFor(ctx, in.Principal)
	if cc.FromHTTP {
		in.Principal = cc.Principal
		in.CallerPub = ""
		in.DisallowBorg = cc.DisallowBorg
	}
	return tb.mem.GroundedAnswer(ctx, in)
}

func (tb *Toolbox) callGroundedAnswerTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in memory.GroundedAnswerInput
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecGroundedAnswer(ctx, in)
}

// ExecVerifyReceipt verifies a nektarr1.* receipt token.
func (tb *Toolbox) ExecVerifyReceipt(ctx context.Context, token string) (*memory.ReceiptVerification, error) {
	return tb.mem.VerifyReceipt(ctx, token)
}

func (tb *Toolbox) callVerifyReceiptTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in struct {
		Token string `json:"token"`
	}
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecVerifyReceipt(ctx, in.Token)
}

// ExecWantTool normalizes domain/domains and question/query aliases before executing ExecWant.
func (tb *Toolbox) ExecWantTool(ctx context.Context, in WantToolInput) (any, error) {
	doms := in.Domains
	if len(doms) == 0 && in.Domain != "" {
		doms = []string{in.Domain}
	}
	q := in.Question
	if q == "" {
		q = in.Query
	}
	return tb.ExecWant(ctx, market.WantRequest{
		Domains:            doms,
		Question:           q,
		OntologyID:         in.OntologyID,
		MaxSats:            in.MaxSats,
		MinGrade:           in.MinGrade,
		MaxSatsPer1kUnits:  in.MaxSatsPer1kUnits,
		FreshnessClass:     in.FreshnessClass,
		RequiredRights:     in.RequiredRights,
		PricingMode:        in.PricingMode,
		TargetShard:        in.TargetShard,
		StandingTTLSeconds: in.StandingTTLSeconds,
	})
}

func (tb *Toolbox) callWantTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in WantToolInput
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecWantTool(ctx, in)
}

// ExecAnchor anchors all unanchored fragments in a shard (defaulting empty shard to "default").
func (tb *Toolbox) ExecAnchor(ctx context.Context, shard string) (any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP && !cc.IsOperator {
		return nil, errors.New("operator token required for anchor")
	}
	if strings.TrimSpace(shard) == "" {
		shard = "default"
	}
	a, err := tb.mem.AnchorShard(ctx, shard)
	if err != nil {
		return nil, err
	}
	if a == nil {
		return map[string]any{"ok": true, "anchored": false, "reason": "nothing unanchored"}, nil
	}
	return a, nil
}

func (tb *Toolbox) callAnchorTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in struct {
		Shard string `json:"shard"`
	}
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecAnchor(ctx, in.Shard)
}

// ExecProve builds and verifies an SPV ProofBundle for a Cedar-authorized fragment.
func (tb *Toolbox) ExecProve(ctx context.Context, shard string, seq int64) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		bundle, vr, err := tb.mem.ProveAuthorized(ctx, shard, seq, cc.Principal)
		if err != nil {
			return nil, err
		}
		return map[string]any{"bundle": bundle, "verify": vr}, nil
	}
	bundle, vr, err := tb.mem.Prove(ctx, shard, seq)
	if err != nil {
		return nil, err
	}
	return map[string]any{"bundle": bundle, "verify": vr}, nil
}

func (tb *Toolbox) callProveTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in struct {
		Shard string `json:"shard"`
		Seq   int64  `json:"seq"`
	}
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecProve(ctx, in.Shard, in.Seq)
}

// ExecExportPack exports a Cedar-authorized shard pack closure.
func (tb *Toolbox) ExecExportPack(ctx context.Context, shard string) (*memory.ShardPackResult, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		return tb.mem.ExportShardPackAuthorized(ctx, shard, cc.Principal, "disclose", cc.DisallowBorg)
	}
	return tb.mem.ExportShardPack(ctx, shard)
}

func (tb *Toolbox) callExportPackTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in struct {
		Shard string `json:"shard"`
	}
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecExportPack(ctx, in.Shard)
}

// ExecImportPack imports a local or remote BRC-167 CHIRP shard pack closure.
func (tb *Toolbox) ExecImportPack(ctx context.Context, in PackToolInput) (map[string]any, error) {
	if in.PeerURL != "" && tb.disc != nil {
		raw, err := tb.disc.FetchRemoteChirp(ctx, in.PeerURL, in.ChirpURL)
		if err != nil {
			return nil, err
		}
		n, sh, err := tb.mem.ImportShardPackBytes(raw, in.TargetShard)
		if err != nil {
			return nil, err
		}
		return map[string]any{"imported": n, "shard": sh}, nil
	}
	n, err := tb.mem.ImportShardPack(ctx, in.ChirpURL, in.TargetShard)
	if err != nil {
		return nil, err
	}
	return map[string]any{"imported": n, "shard": in.TargetShard}, nil
}

func (tb *Toolbox) callImportPackTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in PackToolInput
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecImportPack(ctx, in)
}

// ExecAuthorize evaluates the active Cedar policy set and returns a signed decision receipt.
func (tb *Toolbox) ExecAuthorize(ctx context.Context, in AuthorizeToolInput) (map[string]any, error) {
	pc := tb.mem.PolicyCommit()
	dec, err := policy.Authorize(pc.Text, in.Principal, in.Action, in.Resource)
	if err != nil {
		return nil, err
	}
	rcp := policy.MakeReceipt(in.Principal, in.Action, in.Resource, pc, dec.Allow)
	if tb.mem.Chain() != nil {
		if pub, pErr := tb.mem.Chain().SigningPubKey(ctx); pErr == nil && pub != nil {
			_ = policy.SignDecisionReceipt(hex.EncodeToString(pub.Compressed()), func(digest []byte) ([]byte, error) {
				return tb.mem.Chain().SignDigest(ctx, digest)
			}, &rcp)
		}
	}
	return map[string]any{"decision": dec, "receipt": rcp}, nil
}

func (tb *Toolbox) callAuthorizeTool(ctx context.Context, rawArgs json.RawMessage) (any, error) {
	var in AuthorizeToolInput
	if err := json.Unmarshal(rawArgs, &in); err != nil {
		return nil, err
	}
	return tb.ExecAuthorize(ctx, in)
}

// ExecRecall supports omitted shard (returning shards + items + fragments + count for Dashboard & agents).
func (tb *Toolbox) ExecRecall(ctx context.Context, in memory.RecallInput) (map[string]any, error) {
	cc := CallerContextFor(ctx, in.Principal)
	if cc.FromHTTP {
		in.Principal = cc.Principal
		in.CallerPub = ""
		in.DisallowBorg = cc.DisallowBorg
	}
	shards, items, err := tb.mem.RecallAllShards(ctx, in)
	if err != nil {
		return nil, err
	}
	if items == nil {
		items = []memory.RecalledFragment{}
	}
	if shards == nil {
		shards = []string{}
	}
	return map[string]any{
		"shards":    shards,
		"items":     items,
		"fragments": items,
		"count":     len(items),
	}, nil
}

type BorgGroupInput struct {
	Op        string            `json:"op"`
	GroupID   string            `json:"groupId"`
	Name      string            `json:"name,omitempty"`
	PubHex    string            `json:"pubHex,omitempty"`
	Role      string            `json:"role,omitempty"`
	Principal *policy.Principal `json:"principal,omitempty"`
}

// ExecBorgGroupCtx enforces operator authentication before mutating or inspecting Borg groups.
func (tb *Toolbox) ExecBorgGroupCtx(ctx context.Context, in BorgGroupInput) (any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if !cc.IsOperator {
		return nil, errors.New("operator token required for borgGroup")
	}
	if cc.FromHTTP {
		p := cc.Principal
		in.Principal = &p
	}
	princ := policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	if in.Principal != nil && in.Principal.ID != "" {
		princ = *in.Principal
	}
	switch in.Op {
	case "create":
		owner := in.PubHex
		if owner == "" {
			owner = tb.mem.Chain().IdentityPubHex()
		}
		return tb.mem.Groups().CreateGroup(in.GroupID, in.Name, owner)
	case "add":
		if dec, err := tb.mem.AuthorizeAction(princ, "rotate", policy.Resource{Type: "Fragment", ID: "group:" + in.GroupID, Shard: in.GroupID, Level: 0}); err != nil || !dec.Allow {
			return nil, fmt.Errorf("cedar policy denied rotate on group %q", in.GroupID)
		}
		if err := tb.mem.Groups().AddMember(in.GroupID, in.PubHex, in.Role); err != nil {
			return nil, err
		}
		g, _ := tb.mem.Groups().GetGroup(in.GroupID)
		return g, nil
	case "revoke":
		if dec, err := tb.mem.AuthorizeAction(princ, "revoke", policy.Resource{Type: "Fragment", ID: "group:" + in.GroupID, Shard: in.GroupID, Level: 0}); err != nil || !dec.Allow {
			return nil, fmt.Errorf("cedar policy denied revoke on group %q", in.GroupID)
		}
		ep, err := tb.mem.Groups().RevokeMember(in.GroupID, in.PubHex)
		if err != nil {
			return nil, err
		}
		return map[string]any{"groupId": in.GroupID, "revoked": in.PubHex, "newEpoch": ep}, nil
	default:
		g, ok := tb.mem.Groups().GetGroup(in.GroupID)
		if !ok {
			return nil, fmt.Errorf("unknown group %q", in.GroupID)
		}
		return g, nil
	}
}

// ExecCommitOntology compiles and commits DL-Lite / OWL 2 RL / deontic axioms into __ontology__.
func (tb *Toolbox) ExecCommitOntology(ctx context.Context, in CommitOntologyInput) (*memory.OntologyCommitResult, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP && !cc.IsOperator {
		return nil, errors.New("operator token required for commitOntology")
	}
	return tb.mem.CommitOntologyAxioms(ctx, in.Axioms, in.AutoAnchor)
}

// ExecLineage returns the verified cryptographic Knowledge DAG for (shard, seq).
func (tb *Toolbox) ExecLineage(ctx context.Context, in LineageToolInput) (*memory.LineageReport, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		return tb.mem.LineageAuthorized(ctx, in.Shard, in.Seq, cc.Principal)
	}
	return tb.mem.Lineage(ctx, in.Shard, in.Seq)
}

// ExecProvisionClearance derives a downward-only CLKA clearance bundle (+ manifest) for a shard,
// optionally setting the shard's ontological domain hierarchy and/or sealing the bundle via ECDH.
func (tb *Toolbox) ExecProvisionClearance(ctx context.Context, in ProvisionClearanceInput) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP && !cc.IsOperator {
		return nil, errors.New("operator token required for provisionClearance")
	}
	shard := strings.TrimSpace(in.Shard)
	if shard == "" {
		shard = "default"
	}
	if err := memory.ValidateShardName(shard, false); err != nil {
		return nil, err
	}
	if len(in.DomainEdges) > 0 {
		if err := tb.mem.Lattice().SetDomainHierarchy(shard, in.DomainEdges); err != nil {
			return nil, err
		}
	}
	clr, man, err := tb.mem.Lattice().ProvisionClearance(ctx, shard, in.MaxLevel, in.Domains)
	if err != nil {
		return nil, err
	}
	out := map[string]any{
		"shard":    shard,
		"epoch":    clr.Epoch,
		"manifest": man,
	}
	if recPub := strings.TrimSpace(in.RecipientPubHex); recPub != "" {
		sealed, sErr := lattice.SealClearanceForRecipient(recPub, clr)
		if sErr != nil {
			return nil, sErr
		}
		out["sealedClearance"] = sealed
	} else {
		out["clearance"] = clr
	}
	return out, nil
}

// ExecRotateLattice bumps a shard's CLKA encryption epoch for forward secrecy.
func (tb *Toolbox) ExecRotateLattice(ctx context.Context, in RotateLatticeInput) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP && !cc.IsOperator {
		return nil, errors.New("operator token required for rotateLattice")
	}
	shard := strings.TrimSpace(in.Shard)
	if shard == "" {
		shard = "default"
	}
	if err := memory.ValidateShardName(shard, false); err != nil {
		return nil, err
	}
	ep, err := tb.mem.Lattice().RotateEpoch(shard)
	if err != nil {
		return nil, err
	}
	return map[string]any{"shard": shard, "epoch": ep}, nil
}

// ExecCompactProve builds and SPV-verifies an O(log N) MMR CompactProofBundle.
func (tb *Toolbox) ExecCompactProve(ctx context.Context, in CompactProveInput) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP {
		bundle, vr, err := tb.mem.ProveCompactAuthorized(ctx, in.Shard, in.Seq, cc.Principal)
		if err != nil {
			return nil, err
		}
		return map[string]any{"bundle": bundle, "verify": vr}, nil
	}
	bundle, vr, err := tb.mem.ProveCompact(ctx, in.Shard, in.Seq)
	if err != nil {
		return nil, err
	}
	return map[string]any{"bundle": bundle, "verify": vr}, nil
}

// ExecConsistencyProof builds an O(log N) MMR consistency proof between anchor batches fromN and toN.
func (tb *Toolbox) ExecConsistencyProof(ctx context.Context, in ConsistencyProofInput) (map[string]any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	if cc.FromHTTP && !cc.IsOperator && memory.IsSystemShard(in.Shard) {
		return nil, errors.New("operator token required for system shard consistency proof")
	}
	cp, err := tb.mem.ConsistencyProof(in.Shard, in.FromN, in.ToN)
	if err != nil {
		return nil, err
	}
	return map[string]any{"shard": in.Shard, "proof": cp}, nil
}
