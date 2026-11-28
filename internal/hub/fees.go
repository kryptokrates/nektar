package hub

import (
	"strconv"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// HubRole defines a Hub's position in Nektar's Spine-and-Leaf topology (adapted from @LightBSV's
// deterministic sharded plumbing):
//   - RoleSpine: backbone hub carrying sharded slices; peers settlement-free ONLY with explicitly
//     configured bilateral Spine peers (NEKTAR_PEER_HUBS + SpinePeerPubs), accepts POST /submit UP
//     from Leaf hubs and direct agents, and meters GET /events & GET /sync DOWN.
//   - RoleLeaf: edge proxy hub serving local agents; forwards local ads UP to configured Spine(s)
//     via POST /submit and subscribes DOWN to interested domain slices (?domains=) paying Spine egress.
type HubRole string

const (
	RoleLeaf  HubRole = "leaf"
	RoleSpine HubRole = "spine"

	PresetFree           = "free"
	PresetLightBSVEgress = "lightbsv_egress"
	PresetHybrid         = "hybrid"

	ActionSubmitWant     = "submit:want"
	ActionSubmitOffer    = "submit:offer"
	ActionSubmitAnnounce = "submit:announce"
	ActionBboxSend       = "bbox:send"
	ActionEgressTopup    = "egress:topup"
	ActionLookupPrefix   = "lookup:"

	defaultPrefixTTL        = 10 * time.Minute
	defaultAcceptThreshold  = uint64(5000)
	defaultAcceptPayerLimit = uint64(500_000)
	defaultAcceptTotalLimit = uint64(50_000_000)
	defaultAcceptWindow     = time.Hour
	defaultMinTopupSats     = uint64(20)
)

// FeeSchedule configures a Hub operator's 4-valve Spine-and-Leaf monetization policy.
// All satoshi prices default to 0 (free) unless explicitly configured or a preset is enabled.
type FeeSchedule struct {
	Role                   HubRole           `json:"role"`
	Preset                 string            `json:"preset,omitempty"`
	SubmitWantSats         uint64            `json:"submitWantSats"`
	SubmitOfferSats        uint64            `json:"submitOfferSats"`
	SubmitAnnounceSats     uint64            `json:"submitAnnounceSats"`
	BboxSendSats           uint64            `json:"bboxSendSats"`
	EgressSatsPer100Events uint64            `json:"egressSatsPer100Events"`
	EgressMinTopupSats     uint64            `json:"egressMinTopupSats"`
	LookupPrices           map[string]uint64 `json:"lookupPrices,omitempty"` // e.g. {"history": 5, "export": 20}
	SpinePeerPubs          []string          `json:"spinePeerPubs,omitempty"`
	AcceptThresholdSats    uint64            `json:"acceptThresholdSats"`
	AcceptPayerLimitSats   uint64            `json:"acceptPayerLimitSats"`
	AcceptTotalLimitSats   uint64            `json:"acceptTotalLimitSats"`
	AcceptWindow           time.Duration     `json:"-"`
}

// DefaultFeeSchedule returns canonical fee presets ("free" | "lightbsv_egress" | "hybrid").
func DefaultFeeSchedule(preset string) FeeSchedule {
	switch strings.ToLower(strings.TrimSpace(preset)) {
	case PresetLightBSVEgress:
		return FeeSchedule{
			Role:                   RoleLeaf,
			Preset:                 PresetLightBSVEgress,
			SubmitWantSats:         0,
			SubmitOfferSats:        0,
			SubmitAnnounceSats:     0,
			BboxSendSats:           2,
			EgressSatsPer100Events: 10,
			EgressMinTopupSats:     defaultMinTopupSats,
			LookupPrices:           map[string]uint64{"history": 5, "export": 20},
			AcceptThresholdSats:    defaultAcceptThreshold,
			AcceptPayerLimitSats:   defaultAcceptPayerLimit,
			AcceptTotalLimitSats:   defaultAcceptTotalLimit,
			AcceptWindow:           defaultAcceptWindow,
		}
	case PresetHybrid:
		return FeeSchedule{
			Role:                   RoleLeaf,
			Preset:                 PresetHybrid,
			SubmitWantSats:         5,
			SubmitOfferSats:        2,
			SubmitAnnounceSats:     0,
			BboxSendSats:           2,
			EgressSatsPer100Events: 10,
			EgressMinTopupSats:     defaultMinTopupSats,
			LookupPrices:           map[string]uint64{"history": 5, "export": 20},
			AcceptThresholdSats:    defaultAcceptThreshold,
			AcceptPayerLimitSats:   defaultAcceptPayerLimit,
			AcceptTotalLimitSats:   defaultAcceptTotalLimit,
			AcceptWindow:           defaultAcceptWindow,
		}
	default:
		return FeeSchedule{
			Role:                 RoleLeaf,
			Preset:               PresetFree,
			EgressMinTopupSats:   defaultMinTopupSats,
			AcceptThresholdSats:  defaultAcceptThreshold,
			AcceptPayerLimitSats: defaultAcceptPayerLimit,
			AcceptTotalLimitSats: defaultAcceptTotalLimit,
			AcceptWindow:         defaultAcceptWindow,
		}
	}
}

// LoadFeeSchedule builds a FeeSchedule from environment variables via getenv.
// Defaults to free (0 sats) unless NEKTAR_HUB_FEE_PRESET or explicit fee env vars are set.
func LoadFeeSchedule(getenv func(string) string) FeeSchedule {
	if getenv == nil {
		return DefaultFeeSchedule(PresetFree)
	}
	fs := DefaultFeeSchedule(getenv("NEKTAR_HUB_FEE_PRESET"))
	if role := strings.ToLower(strings.TrimSpace(getenv("NEKTAR_HUB_ROLE"))); role == string(RoleSpine) || role == string(RoleLeaf) {
		fs.Role = HubRole(role)
	}
	parseU64 := func(key string, dst *uint64) {
		if raw := strings.TrimSpace(getenv(key)); raw != "" {
			if v, err := strconv.ParseUint(raw, 10, 64); err == nil {
				*dst = v
			}
		}
	}
	parseU64("NEKTAR_HUB_SUBMIT_WANT_SATS", &fs.SubmitWantSats)
	parseU64("NEKTAR_HUB_SUBMIT_OFFER_SATS", &fs.SubmitOfferSats)
	parseU64("NEKTAR_HUB_SUBMIT_ANNOUNCE_SATS", &fs.SubmitAnnounceSats)
	parseU64("NEKTAR_HUB_BBOX_SEND_SATS", &fs.BboxSendSats)
	parseU64("NEKTAR_HUB_EGRESS_SATS_PER_100", &fs.EgressSatsPer100Events)
	parseU64("NEKTAR_HUB_EGRESS_MIN_TOPUP_SATS", &fs.EgressMinTopupSats)
	parseU64("NEKTAR_HUB_ACCEPT_THRESHOLD_SATS", &fs.AcceptThresholdSats)
	parseU64("NEKTAR_HUB_ACCEPT_PAYER_LIMIT_SATS", &fs.AcceptPayerLimitSats)
	parseU64("NEKTAR_HUB_ACCEPT_TOTAL_LIMIT_SATS", &fs.AcceptTotalLimitSats)
	if raw := strings.TrimSpace(getenv("NEKTAR_HUB_LOOKUP_PRICES")); raw != "" {
		fs.LookupPrices = ParseLookupPrices(raw)
	}
	if raw := strings.TrimSpace(getenv("NEKTAR_SPINE_PEER_PUBS")); raw != "" {
		for _, p := range strings.Split(raw, ",") {
			if p = strings.ToLower(strings.TrimSpace(p)); p != "" {
				fs.SpinePeerPubs = append(fs.SpinePeerPubs, p)
			}
		}
	}
	fs.Normalize()
	return fs
}

// ParseLookupPrices parses a Lightweb-style price list like "history=5,export=20".
func ParseLookupPrices(raw string) map[string]uint64 {
	out := make(map[string]uint64)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(kv[0]))
		v, err := strconv.ParseUint(strings.TrimSpace(kv[1]), 10, 64)
		if err == nil && k != "" && !strings.Contains(k, "|") && v > 0 {
			out[k] = v
		}
	}
	return out
}

// Normalize fills safe defaults for missing role, exposure caps, and top-up minimums.
func (f *FeeSchedule) Normalize() {
	if f.Role != RoleSpine && f.Role != RoleLeaf {
		f.Role = RoleLeaf
	}
	if f.EgressMinTopupSats == 0 {
		f.EgressMinTopupSats = defaultMinTopupSats
	}
	if f.AcceptThresholdSats == 0 {
		f.AcceptThresholdSats = defaultAcceptThreshold
	}
	if f.AcceptPayerLimitSats == 0 {
		f.AcceptPayerLimitSats = defaultAcceptPayerLimit
	}
	if f.AcceptTotalLimitSats == 0 {
		f.AcceptTotalLimitSats = defaultAcceptTotalLimit
	}
	if f.AcceptWindow <= 0 {
		f.AcceptWindow = defaultAcceptWindow
	}
}

// HasAnyFees reports whether any of the 4 Hub fee valves has a non-zero price.
func (f FeeSchedule) HasAnyFees() bool {
	if f.SubmitWantSats > 0 || f.SubmitOfferSats > 0 || f.SubmitAnnounceSats > 0 || f.BboxSendSats > 0 || f.EgressSatsPer100Events > 0 {
		return true
	}
	for _, p := range f.LookupPrices {
		if p > 0 {
			return true
		}
	}
	return false
}

// FeeForKind returns the required ingress fee (satoshis) and canonical action name for a MarketPayload kind.
func (f FeeSchedule) FeeForKind(kind bstack.ArtifactKind) (uint64, string) {
	switch kind {
	case bstack.KindWant:
		return f.SubmitWantSats, ActionSubmitWant
	case bstack.KindOffer:
		return f.SubmitOfferSats, ActionSubmitOffer
	case bstack.KindAnnounce:
		return f.SubmitAnnounceSats, ActionSubmitAnnounce
	default:
		return 0, ""
	}
}

// EgressMilliSatsPerEvent returns the exact milli-satoshi cost per delivered market event (1000 msats = 1 sat).
func (f FeeSchedule) EgressMilliSatsPerEvent() int64 {
	if f.EgressSatsPer100Events == 0 {
		return 0
	}
	// satsPer100 * 1000 / 100 == satsPer100 * 10 milli-sats per event.
	return int64(f.EgressSatsPer100Events) * 10
}

// AdvertSummary returns the compact wire advertisement embedded in AnnounceAd.HubFees.
func (f FeeSchedule) AdvertSummary() *bstack.HubFeeAdvert {
	return &bstack.HubFeeAdvert{
		SubmitWantSats:         f.SubmitWantSats,
		SubmitOfferSats:        f.SubmitOfferSats,
		SubmitAnnounceSats:     f.SubmitAnnounceSats,
		BboxSendSats:           f.BboxSendSats,
		EgressSatsPer100Events: f.EgressSatsPer100Events,
		EgressMinTopupSats:     f.EgressMinTopupSats,
	}
}
