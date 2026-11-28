package server

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
)

var capsuleSlugRe = regexp.MustCompile(`[^a-z0-9-]+`)

// BuyCapsuleInput drives the 1-call `buyCapsule` verb across MCP, REST, A2A, and CLI (`nektar buy`).
type BuyCapsuleInput struct {
	Question         string   `json:"question,omitempty"`
	Query            string   `json:"query,omitempty"`
	Domains          []string `json:"domains,omitempty"`
	Domain           string   `json:"domain,omitempty"`
	MaxSats          uint64   `json:"maxSats,omitempty"`
	MinGrade         string   `json:"minGrade,omitempty"`
	OntologyID       string   `json:"ontologyId,omitempty"`
	TargetShard      string   `json:"targetShard,omitempty"`
	Repo             string   `json:"repo,omitempty"`
	PostBounty       *bool    `json:"postBounty,omitempty"`
	BountyTTLSeconds int64    `json:"bountyTtlSeconds,omitempty"`
}

// SellCapsuleInput drives the 1-call `sellCapsule` verb across MCP, REST, A2A, and CLI (`nektar sell`).
type SellCapsuleInput struct {
	Text        string   `json:"text,omitempty"`
	Data        string   `json:"data,omitempty"`
	File        string   `json:"file,omitempty"`
	Shard       string   `json:"shard,omitempty"`
	Repo        string   `json:"repo,omitempty"`
	Path        string   `json:"path,omitempty"`
	Domains     []string `json:"domains,omitempty"`
	Domain      string   `json:"domain,omitempty"`
	PriceSats   uint64   `json:"priceSats,omitempty"`
	Description string   `json:"description,omitempty"`
	Desc        string   `json:"desc,omitempty"`
	OntologyID  string   `json:"ontologyId,omitempty"`
	ExportPath  string   `json:"exportPath,omitempty"`
	BountyID    string   `json:"bountyId,omitempty"`
	HubURL      string   `json:"hubUrl,omitempty"`
	Seq         int64    `json:"seq,omitempty"`
}

// ImportCapsuleInput drives the 1-call `importCapsule` verb across MCP, REST, A2A, and CLI (`nektar import`).
type ImportCapsuleInput struct {
	Source         string `json:"source,omitempty"`
	CapsuleBase64  string `json:"capsuleBase64,omitempty"`
	Repo           string `json:"repo,omitempty"`
	Shard          string `json:"shard,omitempty"`
	Branch         string `json:"branch,omitempty"`
	CreatorHandle  string `json:"creatorHandle,omitempty"`
	JoinCreatorHub *bool  `json:"joinCreatorHub,omitempty"`
}

func normalizeCapsuleDomains(domains []string, single string) []string {
	var out []string
	seen := make(map[string]bool)
	add := func(raw string) {
		for _, part := range strings.Split(raw, ",") {
			d := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(part), "#"))
			if d != "" && !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	for _, d := range domains {
		add(d)
	}
	if single != "" {
		add(single)
	}
	if len(out) == 0 {
		return []string{"ops"}
	}
	return out
}

func deriveCapsuleRepoName(primaryDomain, text string) string {
	slug := capsuleSlugRe.ReplaceAllString(strings.ToLower(strings.TrimSpace(primaryDomain)), "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "ops"
	}
	sum := sha256.Sum256([]byte(text))
	return fmt.Sprintf("capsules/%s-%s", slug, hex.EncodeToString(sum[:])[:8])
}

func (tb *Toolbox) ensureAutoTradingPolicy(allowMainnetSeller bool, wantBuyer ...bool) {
	if tb == nil || tb.mem == nil || tb.mkt == nil || tb.mem.DB() == nil {
		return
	}
	needBuyer := len(wantBuyer) > 0 && wantBuyer[0]
	hasActive := false
	hasBuyer := false
	pols, err := tb.mem.DB().ListOperatorPolicies()
	if err == nil {
		for _, p := range pols {
			if p.Active {
				hasActive = true
				for _, r := range p.Roles {
					if strings.EqualFold(r, "buyer") {
						hasBuyer = true
					}
				}
			}
		}
	}
	net := ""
	if tb.mem.Chain() != nil {
		net = tb.mem.Chain().Network()
	}
	enableBoundedAutonomy := func() {
		if pol := tb.mkt.Policy(); pol != nil {
			_ = pol.SetAutonomyConfig(market.AutonomyConfig{
				AutoWantOnMiss:        true,
				AutoWantMissThreshold: 3,
				AutoWantMaxPerDay:     3,
				AutoWantBudgetSats:    250,
			})
		}
	}
	if hasActive {
		// On Mainnet, auto-upgrade seller-only nodes to bounded "both" once funded and explicitly invoking buyCapsule.
		if net == "main" && needBuyer && !hasBuyer && tb.mem.Chain() != nil &&
			(tb.mem.Chain().BalanceSats() > 0 || tb.mem.Chain().WalletMode() == "client") {
			if prof, ok := market.PolicyProfileDefaults("standard"); ok {
				ai := prof.AIMatching
				zc := prof.ZeroConfMax
				_, _ = market.ApprovePolicyCustom(tb.mem.DB(), tb.mkt.Policy(), "both", nil, &market.PolicyLimitsOverride{
					MaxPerDeal:  prof.MaxPerDeal,
					MaxPerDay:   prof.MaxPerDay,
					ZeroConfMax: &zc,
				}, "auto-funded-buyer", &ai)
				enableBoundedAutonomy()
			}
		}
		return
	}
	switch net {
	case "testnet", "memory", "regtest":
		if prof, ok := market.PolicyProfileDefaults("easy"); ok {
			ai := prof.AIMatching
			zc := prof.ZeroConfMax
			_, _ = market.ApprovePolicyCustom(tb.mem.DB(), tb.mkt.Policy(), "both", nil, &market.PolicyLimitsOverride{
				MaxPerDeal:  prof.MaxPerDeal,
				MaxPerDay:   prof.MaxPerDay,
				ZeroConfMax: &zc,
			}, "auto-onboard", &ai)
			enableBoundedAutonomy()
		}
	case "main":
		canBuyMain := needBuyer && tb.mem.Chain() != nil && (tb.mem.Chain().BalanceSats() > 0 || tb.mem.Chain().WalletMode() == "client")
		if canBuyMain || allowMainnetSeller {
			if prof, ok := market.PolicyProfileDefaults("standard"); ok {
				ai := prof.AIMatching
				zc := prof.ZeroConfMax
				preset := "seller"
				by := "auto-seller"
				if canBuyMain {
					preset = "both"
					by = "auto-funded-buyer"
				}
				_, _ = market.ApprovePolicyCustom(tb.mem.DB(), tb.mkt.Policy(), preset, nil, &market.PolicyLimitsOverride{
					MaxPerDeal:  prof.MaxPerDeal,
					MaxPerDay:   prof.MaxPerDay,
					ZeroConfMax: &zc,
				}, by, &ai)
				if canBuyMain {
					enableBoundedAutonomy()
				}
			}
		}
	}
}

func isZeroOffersOutcome(dealErr string, callErr error) bool {
	s := strings.ToLower(dealErr)
	if callErr != nil {
		s += " " + strings.ToLower(callErr.Error())
	}
	return strings.Contains(s, "no verified offers") ||
		strings.Contains(s, "no matching offers") ||
		strings.Contains(s, "no offers") ||
		strings.Contains(s, "0 offers")
}

// ExecBuyCapsule searches active Hubs, settles atomically with SPV verification, and — when 0 offers
// match immediately — automatically posts a bounded 0%-CPU StandingOrder satoshi bounty on the Hub.
func (tb *Toolbox) ExecBuyCapsule(ctx context.Context, in BuyCapsuleInput) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached (start your node with `nektar up`)")
	}
	question := strings.TrimSpace(in.Question)
	if question == "" {
		question = strings.TrimSpace(in.Query)
	}
	if question == "" {
		return nil, errors.New("buyCapsule: provide `question` (what verified capsule or runbook you want)")
	}
	doms := normalizeCapsuleDomains(in.Domains, in.Domain)
	targetShard := strings.TrimSpace(in.TargetShard)
	if targetShard == "" {
		targetShard = strings.TrimSpace(in.Repo)
	}
	maxSats := in.MaxSats
	if maxSats == 0 {
		maxSats = 100
	}
	postBounty := true
	if in.PostBounty != nil {
		postBounty = *in.PostBounty
	}
	ttlSec := in.BountyTTLSeconds
	if ttlSec <= 0 {
		ttlSec = 86400
	}

	tb.ensureAutoTradingPolicy(false, true)

	collectTimeout := time.Duration(0)
	if postBounty && tb.mem != nil && tb.mem.Chain() != nil && (tb.mem.Chain().Network() == "main" || tb.mem.Chain().Network() == "testnet") {
		// Fast initial spot probe (4s) before posting a durable 0%-CPU StandingOrder bounty so interactive
		// Claude/Hermes/Codex sessions never block 45s on an empty topic!
		collectTimeout = 4 * time.Second
	}

	deal, err := tb.broker.Want(ctx, market.WantRequest{
		Domains:        doms,
		Question:       question,
		OntologyID:     strings.TrimSpace(in.OntologyID),
		MaxSats:        maxSats,
		MinGrade:       strings.TrimSpace(in.MinGrade),
		TargetShard:    targetShard,
		CollectTimeout: collectTimeout,
	})
	if err == nil && deal != nil && (deal.Status == "settled" || deal.Status == "awaiting_confirmation" || deal.Status == "paid_awaiting_reveal") {
		return map[string]any{
			"ok":            true,
			"mode":          "instant_fill",
			"dealId":        deal.DealID,
			"status":        deal.Status,
			"shard":         deal.Shard,
			"priceSats":     deal.PriceSats,
			"paymentTxid":   deal.PaymentTxID,
			"deliveredText": deal.DeliveredText,
			"deal":          deal,
			"truthNote":     reasoning.TruthNote,
		}, nil
	}

	dealErr := ""
	if deal != nil {
		dealErr = deal.Error
	}
	if postBounty && (deal != nil && deal.Status == "expired" || isZeroOffersOutcome(dealErr, err)) {
		bountyOrder, bErr := tb.broker.StartStandingOrder(ctx, market.StandingOrderInput{
			Domains:     doms,
			Query:       question,
			OntologyID:  strings.TrimSpace(in.OntologyID),
			MaxSats:     maxSats,
			MinGrade:    strings.TrimSpace(in.MinGrade),
			TargetShard: targetShard,
			TTLSeconds:  ttlSec,
			MaxFills:    1,
		})
		if bErr == nil {
			return map[string]any{
				"ok":          true,
				"mode":        "bounty_posted",
				"domains":     doms,
				"question":    question,
				"maxSats":     maxSats,
				"ttlSeconds":  ttlSec,
				"bountyOrder": bountyOrder,
				"message":     "No immediate capsule matched — posted a live 0%-CPU StandingOrder satoshi bounty on your active Hub(s). As soon as a seller fulfills it, Nektar will SPV-verify and import the capsule automatically.",
				"truthNote":   reasoning.TruthNote,
			}, nil
		}
	}

	if err != nil {
		return nil, err
	}
	if deal != nil && deal.Error != "" {
		return map[string]any{
			"ok":        false,
			"mode":      deal.Status,
			"deal":      deal,
			"error":     deal.Error,
			"truthNote": reasoning.TruthNote,
		}, nil
	}
	return map[string]any{"ok": false, "deal": deal, "truthNote": reasoning.TruthNote}, nil
}

// ExecSellCapsule runs the 1-call Hard-Side pipeline: fail-closed PII/secret scrub -> Memory Repo commit ->
// anchor -> auto-arm seller policy -> list & announce OfferAd on active Hubs -> export .nkt metadata/file.
func (tb *Toolbox) ExecSellCapsule(ctx context.Context, in SellCapsuleInput) (any, error) {
	if tb.broker == nil {
		return nil, errors.New("market broker is not attached (start your node with `nektar up`)")
	}
	cc := CallerContextFor(ctx, policy.Principal{})
	allowHostFS := !cc.FromHTTP || cc.IsOperator

	doms := normalizeCapsuleDomains(in.Domains, in.Domain)
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(in.Shard)
	}
	txt := strings.TrimSpace(in.Text)
	if txt == "" {
		txt = strings.TrimSpace(in.Data)
	}

	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}

	if fileArg := strings.TrimSpace(in.File); fileArg != "" {
		if !allowHostFS {
			return nil, errors.New("sellCapsule: operator token required to read local files over HTTP")
		}
		rawBytes, rErr := os.ReadFile(fileArg)
		if rErr != nil {
			return nil, fmt.Errorf("sellCapsule: read %s: %w", fileArg, rErr)
		}
		if strings.HasSuffix(strings.ToLower(fileArg), ".nkt") {
			impRes, iErr := tb.mem.ImportCapsuleAuthorized(ctx, rawBytes, "", repo, "", true, principalPtr)
			if iErr != nil {
				return nil, fmt.Errorf("sellCapsule: import .nkt %s: %w", fileArg, iErr)
			}
			if repo == "" {
				repo = impRes.Repo
			}
		} else {
			txt = strings.TrimSpace(string(rawBytes))
			if in.Path == "" {
				in.Path = filepath.Base(fileArg)
			}
		}
	}

	var committedSeq int64
	var anchorTxID string
	var redactedCount int
	if txt != "" {
		// Fail-closed secret & PII gate BEFORE committing any sellable text!
		sr := distill.Scrub(txt)
		if sr.Verdict == distill.ScrubBlocked {
			rule := "secret-detected"
			if len(sr.BlockedRules) > 0 {
				rule = sr.BlockedRules[0]
			}
			return nil, fmt.Errorf("sellCapsule blocked fail-closed by secret scrubber (%s) — remove credentials/keys before publishing", rule)
		}
		psr := policy.Scrub(sr.SanitizedText)
		if psr.Verdict == policy.ScrubBlocked {
			rule := "secret-detected"
			if len(psr.BlockedRules) > 0 {
				rule = psr.BlockedRules[0]
			}
			return nil, fmt.Errorf("sellCapsule blocked fail-closed by policy scrubber (%s) — remove credentials/keys before publishing", rule)
		}
		cleanText := psr.SanitizedText
		if cleanText == "" {
			cleanText = sr.SanitizedText
		}
		redactedCount = len(sr.Redactions) + len(psr.Redactions)

		if repo == "" {
			repo = deriveCapsuleRepoName(doms[0], cleanText)
		}
		docPath := strings.TrimSpace(in.Path)
		if docPath == "" {
			docPath = "CAPSULE.md"
		}
		canAnchor := tb.mem.Chain().Network() == "memory" || tb.mem.Chain().BalanceSats() > 0 || tb.mem.Chain().WalletMode() == "client"
		remRes, rErr := tb.mem.Remember(ctx, memory.RememberInput{
			Shard:         repo,
			Path:          docPath,
			Text:          cleanText,
			Class:         doms[0],
			Source:        "capsule:sell",
			CommitMessage: firstNonEmpty(in.Description, in.Desc, "Publish verified capsule ("+doms[0]+")"),
			Public:        true,
			Distilled:     true,
			Ontology:      strings.TrimSpace(in.OntologyID),
			Principal:     principalPtr,
			AutoAnchor:    canAnchor && (!cc.FromHTTP || cc.IsOperator),
		})
		if rErr != nil {
			return nil, rErr
		}
		committedSeq = remRes.Seq
		anchorTxID = remRes.AnchorTxID
	}

	if repo == "" {
		return nil, errors.New("sellCapsule: provide `text` (inline runbook/facts to package & sell) or an existing `repo`/`shard`")
	}

	if anchorTxID == "" && (!cc.FromHTTP || cc.IsOperator) && (tb.mem.Chain().Network() == "memory" || tb.mem.Chain().BalanceSats() > 0 || tb.mem.Chain().WalletMode() == "client") {
		if aRes, aErr := tb.mem.AnchorShard(ctx, repo); aErr == nil && aRes != nil {
			anchorTxID = aRes.TxID
		}
	}

	tb.ensureAutoTradingPolicy(allowHostFS)

	if strings.TrimSpace(in.BountyID) != "" {
		seq := in.Seq
		if seq <= 0 {
			seq = committedSeq
		}
		return tb.broker.SellToHub(ctx, in.HubURL, in.BountyID, repo, seq)
	}

	price := in.PriceSats
	if price == 0 {
		price = 50
	}
	desc := firstNonEmpty(in.Description, in.Desc, fmt.Sprintf("Verified %s capsule (%s)", doms[0], repo))
	armRes, err := tb.broker.ArmSell(ctx, market.SellInput{
		Shard:       repo,
		Domains:     doms,
		PriceSats:   price,
		Description: desc,
		OntologyID:  strings.TrimSpace(in.OntologyID),
		AllowRaw:    true,
	})
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"ok":             true,
		"repo":           repo,
		"shard":          repo,
		"domains":        armRes.Domains,
		"priceSats":      armRes.PriceSats,
		"quotingReady":   armRes.QuotingReady,
		"blockedReasons": armRes.BlockedReasons,
		"redactedPii":    redactedCount,
		"listing":        armRes,
	}
	if committedSeq > 0 {
		out["seq"] = committedSeq
	}
	if anchorTxID != "" {
		out["anchorTxid"] = anchorTxID
	}

	// Build creator attribution & portable .nkt capsule closure for GitHub / PR / X sharing!
	handle := strings.TrimSpace(os.Getenv("NEKTAR_HANDLE"))
	var activeHubs []string
	if tb.disc != nil {
		activeHubs = tb.disc.ActiveHosts()
	}
	if handle == "" && tb.mem != nil && tb.mem.Chain() != nil && len(activeHubs) > 0 {
		if u, uErr := url.Parse(activeHubs[0]); uErr == nil && u.Host != "" {
			handle = fmt.Sprintf("nektar://pub/%s@%s", tb.mem.Chain().IdentityPubHex(), u.Host)
		}
	}
	if handle != "" {
		out["creatorHandle"] = handle
	}
	if len(activeHubs) > 0 {
		out["announcedHubs"] = activeHubs
	}

	if capRes, _, rawNKT, cErr := tb.mem.ExportCapsuleAuthorized(ctx, repo, "", "", cc.Principal, cc.DisallowBorg); cErr == nil && capRes != nil {
		out["capsule"] = map[string]any{
			"chirpUrl":    capRes.ChirpURL,
			"headCommit":  capRes.HeadCommit,
			"sizeBytes":   capRes.SizeBytes,
			"anchorCount": capRes.AnchorCount,
		}
		if exportDst := strings.TrimSpace(in.ExportPath); exportDst != "" {
			if !allowHostFS {
				return nil, errors.New("sellCapsule: operator token required to write exportPath on host")
			}
			if mkErr := os.MkdirAll(filepath.Dir(exportDst), 0o750); mkErr != nil {
				return nil, mkErr
			}
			if wErr := os.WriteFile(exportDst, rawNKT, 0o644); wErr != nil {
				return nil, wErr
			}
			out["exportedFile"] = exportDst
			out["shareCommand"] = fmt.Sprintf("nektar import %s", exportDst)
		}
	}
	return out, nil
}

// ExecImportCapsule verifies and imports a portable .nkt Memory Capsule from a local path, https:// URL,
// chirp:// URI, or inline base64 payload, and bridges the user to the creator's LightBSV handle / Hub.
func (tb *Toolbox) ExecImportCapsule(ctx context.Context, in ImportCapsuleInput) (any, error) {
	cc := CallerContextFor(ctx, policy.Principal{})
	allowHostFS := !cc.FromHTTP || cc.IsOperator
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}

	targetRepo := firstNonEmpty(in.Repo, in.Shard)
	src := strings.TrimSpace(in.Source)

	var raw []byte
	var chirpURL string
	var sourceOrigin string

	switch {
	case strings.TrimSpace(in.CapsuleBase64) != "":
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.CapsuleBase64))
		if err != nil {
			return nil, fmt.Errorf("importCapsule: invalid capsuleBase64: %w", err)
		}
		raw = dec
	case strings.HasPrefix(strings.ToLower(src), "chirp://"):
		chirpURL = src
	case strings.HasPrefix(strings.ToLower(src), "http://") || strings.HasPrefix(strings.ToLower(src), "https://"):
		if u, uErr := url.Parse(src); uErr == nil && u.Scheme != "" && u.Host != "" {
			sourceOrigin = u.Scheme + "://" + u.Host
		}
		hc := tb.httpClient
		if hc == nil {
			hc = &http.Client{Timeout: 20 * time.Second}
		}
		req, rErr := http.NewRequestWithContext(ctx, http.MethodGet, src, nil)
		if rErr != nil {
			return nil, fmt.Errorf("importCapsule: invalid URL %q: %w", src, rErr)
		}
		resp, dErr := hc.Do(req)
		if dErr != nil {
			return nil, fmt.Errorf("importCapsule: fetch %s: %w", src, dErr)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("importCapsule: fetch %s returned HTTP %d", src, resp.StatusCode)
		}
		body, bErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		if bErr != nil {
			return nil, fmt.Errorf("importCapsule: read %s: %w", src, bErr)
		}
		raw = body
	case src != "":
		if !allowHostFS {
			return nil, errors.New("importCapsule: operator token required to read local file paths over HTTP")
		}
		body, rErr := os.ReadFile(src)
		if rErr != nil {
			return nil, fmt.Errorf("importCapsule: read %s: %w", src, rErr)
		}
		raw = body
	default:
		return nil, errors.New("importCapsule: provide `source` (.nkt file path, https:// URL, or chirp:// URI) or `capsuleBase64`")
	}

	impRes, err := tb.mem.ImportCapsuleAuthorized(ctx, raw, chirpURL, targetRepo, in.Branch, true, principalPtr)
	if err != nil {
		return nil, err
	}

	out := map[string]any{
		"ok":        true,
		"imported":  impRes,
		"repo":      impRes.Repo,
		"truthNote": reasoning.TruthNote,
	}

	// Viral Cold-Start Bridge: discover creator's Hub from creatorHandle or the HTTPS origin hosting the .nkt!
	var candidateHub string
	if handle := strings.TrimSpace(in.CreatorHandle); handle != "" && tb.disc != nil {
		if peer, ok := tb.disc.ResolveHandle(handle); ok && len(peer.Hosts) > 0 {
			candidateHub = peer.Hosts[0]
			out["creator"] = map[string]any{"handle": handle, "pub": peer.Pub, "hosts": peer.Hosts}
		} else if idx := strings.LastIndex(handle, "@"); idx > 0 && idx+1 < len(handle) {
			candidateHub = "https://" + handle[idx+1:]
		}
	} else if sourceOrigin != "" {
		candidateHub = sourceOrigin
	}

	if candidateHub != "" && tb.disc != nil {
		if in.JoinCreatorHub != nil && *in.JoinCreatorHub && allowHostFS {
			if joinedURL, _, jErr := tb.disc.JoinHost(ctx, candidateHub); jErr == nil {
				out["joinedHub"] = joinedURL
			}
		} else {
			out["suggestedHubJoin"] = fmt.Sprintf("nektar peers join %s", candidateHub)
		}
	}
	return out, nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if s := strings.TrimSpace(v); s != "" {
			return s
		}
	}
	return ""
}
