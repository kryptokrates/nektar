package spendpolicy

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/store"
)

// ComputePolicyHash computes the single canonical SHA-256 commitment hash for an OperatorPolicyRecord (RCA-3).
func ComputePolicyHash(roles, domains []string, maxPerDeal, maxPerDay, zeroConfMax uint64, aiPol *AIMatchingPolicy) (string, error) {
	hashInput := map[string]any{
		"roles":       roles,
		"domains":     domains,
		"maxPerDeal":  maxPerDeal,
		"maxPerDay":   maxPerDay,
		"zeroConfMax": zeroConfMax,
	}
	if !aiPol.IsEmpty() {
		hashInput["aiMatching"] = aiPol
	}
	canon, err := core.CanonicalJSON(hashInput)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(core.SHA256(canon)), nil
}

// ApprovePolicyPreset creates, hashes, persists, and activates a safe operator policy preset ("buyer" | "seller" | "both").
func ApprovePolicyPreset(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string) (store.OperatorPolicyRecord, error) {
	return ApprovePolicyPresetWithAI(db, pol, preset, domains, approvedBy, nil)
}

// OperatorPolicyDigest computes the domain-separated, length-framed digest signed by the Operator key.
func OperatorPolicyDigest(rec store.OperatorPolicyRecord) []byte {
	roles := append([]string(nil), rec.Roles...)
	sort.Strings(roles)
	doms := append([]string(nil), rec.Domains...)
	sort.Strings(doms)
	return core.DSHA256(core.Concat(
		[]byte("nektar:operator-policy:v2"),
		core.Field([]byte(rec.PolicyID)),
		core.Field([]byte(rec.PolicyHash)),
		core.Field([]byte(strings.Join(roles, ","))),
		core.Field([]byte(strings.Join(doms, ","))),
		core.U64LE(rec.MaxPerDeal),
		core.U64LE(rec.MaxPerDay),
		core.U64LE(rec.ZeroConfMax),
		core.Field([]byte(rec.AIPolicyJSON)),
		core.Field([]byte(rec.ApprovedBy)),
		core.U64LE(uint64(rec.ApprovedAt)),
	))
}

// SignOperatorPolicy signs rec with the operator key via signDigest (preserving zero raw key exposure).
func SignOperatorPolicy(ctx context.Context, operatorPub string, signDigest func(context.Context, []byte) ([]byte, error), rec *store.OperatorPolicyRecord) error {
	if rec == nil || operatorPub == "" || signDigest == nil {
		return errors.New("market: missing policy record, operatorPub, or signer")
	}
	if _, _, err := core.ParseStrictCompressedPubKey(operatorPub); err != nil {
		return fmt.Errorf("market: invalid operatorPub: %w", err)
	}
	rec.OperatorPub = strings.ToLower(strings.TrimSpace(operatorPub))
	sig, err := signDigest(ctx, OperatorPolicyDigest(*rec))
	if err != nil {
		return err
	}
	rec.OperatorSig = hex.EncodeToString(sig)
	return nil
}

// VerifyOperatorPolicy verifies rec's canonical PolicyHash and cryptographic OperatorSig.
func VerifyOperatorPolicy(rec store.OperatorPolicyRecord, pinnedOperatorPub string) error {
	var parsedAI *AIMatchingPolicy
	if strings.TrimSpace(rec.AIPolicyJSON) != "" {
		var ai AIMatchingPolicy
		if err := json.Unmarshal([]byte(rec.AIPolicyJSON), &ai); err != nil {
			return fmt.Errorf("market: corrupt aiPolicyJson on policy %s: %w", rec.PolicyID, err)
		}
		parsedAI = &ai
	}
	wantHash, err := ComputePolicyHash(rec.Roles, rec.Domains, rec.MaxPerDeal, rec.MaxPerDay, rec.ZeroConfMax, parsedAI)
	if err != nil {
		return err
	}
	if rec.PolicyHash != "" && !strings.EqualFold(rec.PolicyHash, wantHash) {
		return fmt.Errorf("market: policy %s hash mismatch (tampered SQLite row)", rec.PolicyID)
	}
	pinned := strings.ToLower(strings.TrimSpace(pinnedOperatorPub))
	if pinned != "" {
		if !strings.EqualFold(strings.TrimSpace(rec.OperatorPub), pinned) {
			return fmt.Errorf("market: policy %s operatorPub %q != pinned %q", rec.PolicyID, rec.OperatorPub, pinned)
		}
		if strings.TrimSpace(rec.OperatorSig) == "" {
			return fmt.Errorf("market: policy %s missing required operatorSig under pinned operator key", rec.PolicyID)
		}
	}
	if strings.TrimSpace(rec.OperatorSig) != "" {
		pub, _, err := core.ParseStrictCompressedPubKey(rec.OperatorPub)
		if err != nil {
			return fmt.Errorf("market: policy %s invalid operatorPub: %w", rec.PolicyID, err)
		}
		sig, err := core.ParseStrictLowSDER(rec.OperatorSig)
		if err != nil {
			return fmt.Errorf("market: policy %s invalid operatorSig: %w", rec.PolicyID, err)
		}
		if !sig.Verify(OperatorPolicyDigest(rec), pub) {
			return fmt.Errorf("market: policy %s operatorSig verification failed", rec.PolicyID)
		}
	}
	return nil
}

// ApprovePolicyPresetSigned creates, cryptographically signs with operatorPub, persists, and activates a policy preset.
func ApprovePolicyPresetSigned(
	ctx context.Context,
	db *store.DB,
	pol *Policy,
	preset string,
	domains []string,
	approvedBy string,
	aiPol *AIMatchingPolicy,
	operatorPub string,
	signDigest func(context.Context, []byte) ([]byte, error),
) (store.OperatorPolicyRecord, error) {
	rec, err := buildOperatorPolicyRecord(preset, domains, approvedBy, aiPol)
	if err != nil {
		return store.OperatorPolicyRecord{}, err
	}
	if operatorPub != "" && signDigest != nil {
		if err := SignOperatorPolicy(ctx, operatorPub, signDigest, &rec); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	}
	if db != nil {
		if err := db.SaveOperatorPolicy(rec); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	}
	if pol != nil && db != nil {
		if err := SyncPolicyFromDB(db, pol); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	} else if pol != nil {
		applyPolicyRecord(pol, rec)
	}
	return rec, nil
}

// PolicyProfile defines canonical Easy / Standard / Secure policy defaults.
type PolicyProfile struct {
	Name        string
	Preset      string // "buyer" | "seller" | "both"
	MaxPerDeal  uint64
	MaxPerDay   uint64
	ZeroConfMax uint64
	AIMatching  AIMatchingPolicy
}

// PolicyProfileDefaults returns canonical defaults for "easy", "standard", or "secure".
func PolicyProfileDefaults(tier string) (PolicyProfile, bool) {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "easy":
		return PolicyProfile{
			Name:        "easy",
			Preset:      "both",
			MaxPerDeal:  5_000,
			MaxPerDay:   50_000,
			ZeroConfMax: 2_500,
			AIMatching: AIMatchingPolicy{
				Mode:                 AIModeGated,
				SellerPrivacyScope:   PrivacyDisclosablePreview,
				MinConfidenceBP:      8000,
				EscalateConfidenceBP: 8200,
				AutoApproveMaxSats:   1_000,
			},
		}, true
	case "standard":
		return PolicyProfile{
			Name:        "standard",
			Preset:      "both",
			MaxPerDeal:  2_000,
			MaxPerDay:   20_000,
			ZeroConfMax: 1_000,
			AIMatching: AIMatchingPolicy{
				Mode:                 AIModeGated,
				SellerPrivacyScope:   PrivacyDisclosablePreview,
				MinConfidenceBP:      8500,
				EscalateConfidenceBP: 8800,
				AutoApproveMaxSats:   500,
			},
		}, true
	case "secure":
		return PolicyProfile{
			Name:        "secure",
			Preset:      "buyer",
			MaxPerDeal:  250,
			MaxPerDay:   2_000,
			ZeroConfMax: 0,
			AIMatching: AIMatchingPolicy{
				Mode:                 AIModeGated,
				SellerPrivacyScope:   PrivacyMetadataOnly,
				MinConfidenceBP:      9200,
				EscalateConfidenceBP: 9500,
				AutoApproveMaxSats:   100,
			},
		}, true
	default:
		return PolicyProfile{}, false
	}
}

// PolicyLimitsOverride carries optional custom satoshi limits when approving a policy.
type PolicyLimitsOverride struct {
	MaxPerDeal  uint64
	MaxPerDay   uint64
	ZeroConfMax *uint64
}

// ApprovePolicyCustom creates, validates, hashes, persists, and activates an operator policy.
func ApprovePolicyCustom(
	db *store.DB,
	pol *Policy,
	preset string,
	domains []string,
	limits *PolicyLimitsOverride,
	approvedBy string,
	aiPol *AIMatchingPolicy,
) (store.OperatorPolicyRecord, error) {
	rec, err := buildOperatorPolicyRecord(preset, domains, approvedBy, aiPol)
	if err != nil {
		return store.OperatorPolicyRecord{}, err
	}
	if limits != nil {
		if limits.MaxPerDeal > 0 {
			rec.MaxPerDeal = limits.MaxPerDeal
		}
		if limits.MaxPerDay > 0 {
			rec.MaxPerDay = limits.MaxPerDay
		}
		if limits.ZeroConfMax != nil {
			rec.ZeroConfMax = *limits.ZeroConfMax
		}
		if rec.MaxPerDeal > rec.MaxPerDay {
			return store.OperatorPolicyRecord{}, errors.New("market: maxPerDeal cannot exceed maxPerDay")
		}
		h, cErr := ComputePolicyHash(rec.Roles, rec.Domains, rec.MaxPerDeal, rec.MaxPerDay, rec.ZeroConfMax, aiPol)
		if cErr != nil {
			return store.OperatorPolicyRecord{}, cErr
		}
		rec.PolicyHash = h
	}
	if db != nil {
		if err := db.SaveOperatorPolicy(rec); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	}
	if pol != nil && db != nil {
		if err := SyncPolicyFromDB(db, pol); err != nil {
			return store.OperatorPolicyRecord{}, err
		}
	} else if pol != nil {
		applyPolicyRecord(pol, rec)
	}
	return rec, nil
}

// ApprovePolicyPresetWithAI creates, validates, hashes, persists, and activates an operator policy preset.
func ApprovePolicyPresetWithAI(db *store.DB, pol *Policy, preset string, domains []string, approvedBy string, aiPol *AIMatchingPolicy) (store.OperatorPolicyRecord, error) {
	return ApprovePolicyPresetSigned(context.Background(), db, pol, preset, domains, approvedBy, aiPol, "", nil)
}

func buildOperatorPolicyRecord(preset string, domains []string, approvedBy string, aiPol *AIMatchingPolicy) (store.OperatorPolicyRecord, error) {
	if err := aiPol.Validate(); err != nil {
		return store.OperatorPolicyRecord{}, err
	}
	preset = strings.ToLower(strings.TrimSpace(preset))
	var roles []string
	switch preset {
	case "buyer":
		roles = []string{"buyer"}
	case "seller":
		roles = []string{"seller"}
	case "both":
		roles = []string{"buyer", "seller"}
	default:
		return store.OperatorPolicyRecord{}, fmt.Errorf("market: unknown policy preset %q (use buyer, seller, or both)", preset)
	}
	if approvedBy == "" {
		approvedBy = "operator"
	}
	var idBytes [6]byte
	if _, err := crand.Read(idBytes[:]); err != nil {
		return store.OperatorPolicyRecord{}, fmt.Errorf("market: csprng failed: %w", err)
	}
	var aiJSON string
	if !aiPol.IsEmpty() {
		raw, err := core.CanonicalJSON(aiPol)
		if err != nil {
			return store.OperatorPolicyRecord{}, err
		}
		aiJSON = string(raw)
	}
	rec := store.OperatorPolicyRecord{
		PolicyID:     fmt.Sprintf("pol-%s-%s", preset, hex.EncodeToString(idBytes[:])),
		Roles:        roles,
		Domains:      domains,
		MaxPerDeal:   2_000,
		MaxPerDay:    20_000,
		ZeroConfMax:  1_000,
		AIPolicyJSON: aiJSON,
		Active:       true,
		ApprovedBy:   approvedBy,
		ApprovedAt:   time.Now().Unix(),
	}
	h, err := ComputePolicyHash(rec.Roles, rec.Domains, rec.MaxPerDeal, rec.MaxPerDay, rec.ZeroConfMax, aiPol)
	if err != nil {
		return store.OperatorPolicyRecord{}, err
	}
	rec.PolicyHash = h
	return rec, nil
}

func applyPolicyRecord(pol *Policy, rec store.OperatorPolicyRecord) {
	pol.mu.Lock()
	defer pol.mu.Unlock()
	for _, r := range rec.Roles {
		if r == "buyer" {
			pol.BuyerApproved = true
		}
		if r == "seller" {
			pol.SellerApproved = true
		}
	}
	if rec.MaxPerDeal > 0 {
		pol.MaxSatsPerDeal = rec.MaxPerDeal
	}
	if rec.MaxPerDay > 0 {
		pol.MaxSatsPerDay = rec.MaxPerDay
	}
	if rec.ZeroConfMax > 0 {
		pol.ZeroConfMaxSats = rec.ZeroConfMax
	}
	if len(rec.Domains) > 0 {
		if pol.AllowedDomains == nil {
			pol.AllowedDomains = make(map[string]bool)
		}
		for _, d := range rec.Domains {
			pol.AllowedDomains[strings.ToLower(strings.TrimSpace(d))] = true
		}
	}
	if strings.TrimSpace(rec.AIPolicyJSON) != "" {
		var ai AIMatchingPolicy
		if json.Unmarshal([]byte(rec.AIPolicyJSON), &ai) == nil {
			pol.AIMatching = &ai
		}
	}
}

// SyncPolicyFromDB loads active persisted operator_policies into pol (fail-closed on DB errors).
func SyncPolicyFromDB(db *store.DB, pol *Policy) error {
	if db == nil || pol == nil {
		return nil
	}
	list, err := db.ListOperatorPolicies()
	if err != nil {
		return err
	}
	pol.mu.Lock()
	pol.db = db
	pol.mu.Unlock()
	return applyOperatorPoliciesList(pol, list)
}

func applyOperatorPoliciesList(pol *Policy, list []store.OperatorPolicyRecord) error {
	pol.mu.Lock()
	defer pol.mu.Unlock()
	pol.syncedFromDB = true
	pol.BuyerApproved = false
	pol.SellerApproved = false
	pinnedOp := pol.pinnedOperatorPub
	pol.activePolicyHash = ""
	pol.tamperedPolicies = 0
	var maxDeal, maxDay, zeroConf uint64
	activeCount := 0
	doms := make(map[string]bool)
	var latestAI *AIMatchingPolicy
	for _, rec := range list {
		if !rec.Active {
			continue
		}
		if err := VerifyOperatorPolicy(rec, pinnedOp); err != nil {
			pol.tamperedPolicies++
			continue
		}
		activeCount++
		if pol.activePolicyHash == "" {
			pol.activePolicyHash = rec.PolicyHash
		}
		for _, r := range rec.Roles {
			if r == "buyer" {
				pol.BuyerApproved = true
			}
			if r == "seller" {
				pol.SellerApproved = true
			}
		}
		if rec.MaxPerDeal > maxDeal {
			maxDeal = rec.MaxPerDeal
		}
		if rec.MaxPerDay > maxDay {
			maxDay = rec.MaxPerDay
		}
		if rec.ZeroConfMax > zeroConf {
			zeroConf = rec.ZeroConfMax
		}
		for _, d := range rec.Domains {
			doms[strings.ToLower(strings.TrimSpace(d))] = true
		}
		if latestAI == nil && strings.TrimSpace(rec.AIPolicyJSON) != "" {
			var ai AIMatchingPolicy
			if json.Unmarshal([]byte(rec.AIPolicyJSON), &ai) == nil {
				latestAI = &ai
			}
		}
	}
	if activeCount > 0 {
		pol.MaxSatsPerDeal = maxDeal
		pol.MaxSatsPerDay = maxDay
		pol.ZeroConfMaxSats = zeroConf
		pol.AllowedDomains = doms
		pol.AIMatching = latestAI
	} else if len(list) > 0 || pinnedOp != "" || pol.tamperedPolicies > 0 {
		// Fail closed: when operator policies exist in SQLite (all revoked or tampered) or an
		// operator key is pinned with zero valid signed policies, spending caps drop to 0.
		pol.MaxSatsPerDeal = 0
		pol.MaxSatsPerDay = 0
		pol.ZeroConfMaxSats = 0
		pol.AllowedDomains = nil
		pol.AIMatching = nil
	} else {
		def := DefaultPolicy()
		pol.MaxSatsPerDeal = def.MaxSatsPerDeal
		pol.MaxSatsPerDay = def.MaxSatsPerDay
		pol.ZeroConfMaxSats = def.ZeroConfMaxSats
		pol.AllowedDomains = nil
		pol.AIMatching = nil
	}
	return nil
}
