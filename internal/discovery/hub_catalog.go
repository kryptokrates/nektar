package discovery

import (
	"context"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/store"
)

// HubFeeSummary captures exact satoshi relay & egress fees advertised by a Hub's /v1/terms.
type HubFeeSummary struct {
	WantSats         uint64 `json:"wantSats"`
	OfferSats        uint64 `json:"offerSats"`
	BboxSats         uint64 `json:"bboxSats"`
	EgressSatsPer100 uint64 `json:"egressSatsPer100"`
	MinTopupSats     uint64 `json:"minTopupSats"`
}

// HubCatalogEntry summarizes one known, seed, joined, or peer-discovered Overlay Hub
// with live LightBSV-style reachability, latency, role, exact satoshi fees, federation links, and topics.
type HubCatalogEntry struct {
	URL             string         `json:"url"`
	Name            string         `json:"name"`
	Source          string         `json:"source"` // "seed" | "joined" | "discovered" | "blocked"
	Connected       bool           `json:"connected"`
	Reachable       bool           `json:"reachable"`
	LatencyMs       int64          `json:"latencyMs"`
	Network         string         `json:"network,omitempty"`
	NetworkMismatch bool           `json:"networkMismatch,omitempty"`
	Role            string         `json:"role,omitempty"`      // "spine" | "leaf"
	FeePreset       string         `json:"feePreset,omitempty"` // "free" | "hybrid" | "lightbsv_egress"
	Fees            *HubFeeSummary `json:"fees,omitempty"`
	Topics          []string       `json:"topics,omitempty"`
	LinkedHubs      []string       `json:"linkedHubs,omitempty"`
	PeersCount      int            `json:"peersCount,omitempty"`
	Error           string         `json:"error,omitempty"`
}

const (
	hubCatalogCacheTTL = 15 * time.Second
	hubProbeTimeout    = 2500 * time.Millisecond
)

type hubCatalogCache struct {
	mu      sync.Mutex
	entries []HubCatalogEntry
	at      time.Time
}

func (s *Service) getCatalogCache() *hubCatalogCache {
	return &s.catalogCache
}

func hostDisplayName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(rawURL, "https://"), "http://")
	}
	return u.Host
}

// ProbeHubCatalog gathers all default seed hubs, env/joined/blocked hosts, and
// dir-lane discovered community hubs (IsHub==true), probes their live health & terms
// in parallel, and returns a sorted catalog for the browser Hubs page.
func (s *Service) ProbeHubCatalog(ctx context.Context, forceRefresh bool) []HubCatalogEntry {
	if s == nil {
		return nil
	}
	cc := s.getCatalogCache()
	if !forceRefresh {
		cc.mu.Lock()
		if len(cc.entries) > 0 && time.Since(cc.at) < hubCatalogCacheTTL {
			cp := make([]HubCatalogEntry, len(cc.entries))
			copy(cp, cc.entries)
			cc.mu.Unlock()
			return cp
		}
		cc.mu.Unlock()
	}

	activeSet := make(map[string]bool)
	for _, h := range s.ActiveHosts() {
		activeSet[normalizeHostURL(h)] = true
	}
	blockedSet := make(map[string]bool)
	joinedSet := make(map[string]bool)
	if s.mem != nil && s.mem.DB() != nil {
		if persisted, err := s.mem.DB().ListHosts(); err == nil {
			for _, h := range persisted {
				u := normalizeHostURL(h.URL)
				if u == "" {
					continue
				}
				if h.Blocked {
					blockedSet[u] = true
				} else {
					joinedSet[u] = true
				}
			}
		}
	}

	byURL := make(map[string]*HubCatalogEntry)
	addCandidate := func(rawURL, name, source, role string, topics []string) *HubCatalogEntry {
		u := normalizeHostURL(rawURL)
		if u == "" {
			return nil
		}
		if cur, ok := byURL[u]; ok {
			if cur.Role == "" && role != "" {
				cur.Role = role
			}
			if len(cur.Topics) == 0 && len(topics) > 0 {
				cur.Topics = append([]string(nil), topics...)
			}
			return cur
		}
		if name == "" {
			name = hostDisplayName(u)
		}
		src := source
		if blockedSet[u] {
			src = "blocked"
		} else if joinedSet[u] && src == "discovered" {
			src = "joined"
		}
		ent := &HubCatalogEntry{
			URL:       u,
			Name:      name,
			Source:    src,
			Connected: activeSet[u],
			Role:      role,
			Topics:    append([]string(nil), topics...),
		}
		byURL[u] = ent
		return ent
	}

	// 1. Default curated seed fleet for the active network (and show live testnet fleet
	//    preview in memory mode when no custom local hubs are configured yet).
	specs := DefaultSeedSpecs(s.network)
	previewOnlyRemote := false
	if len(specs) == 0 && s.network == "memory" && len(s.envHosts) == 0 && len(joinedSet) == 0 {
		specs = DefaultSeedSpecs("testnet")
		previewOnlyRemote = true
	}
	for _, sp := range specs {
		ent := addCandidate(sp.URL, sp.Name, "seed", sp.Role, sp.Topics)
		if previewOnlyRemote && ent != nil {
			ent.Network = "testnet"
		}
	}

	// 2. Explicit env hosts & persisted joined/blocked hosts.
	for _, h := range s.envHosts {
		addCandidate(h, "", "seed", "", nil)
	}
	for u := range joinedSet {
		addCandidate(u, "", "joined", "", nil)
	}
	for u := range blockedSet {
		addCandidate(u, "", "blocked", "", nil)
	}

	// 3. Community hubs announced on the signed dir lane (IsHub == true) + peer counts per hub.
	peersPerHub := make(map[string]int)
	for _, p := range s.Peers() {
		for _, sh := range p.SeenOnHosts {
			peersPerHub[normalizeHostURL(sh)]++
		}
		if p.IsHub {
			hubURL := strings.TrimSpace(p.PublicURL)
			if hubURL != "" {
				if !strings.HasSuffix(strings.TrimRight(hubURL, "/"), "/hub") {
					hubURL = strings.TrimRight(hubURL, "/") + "/hub"
				}
				ent := addCandidate(hubURL, hostDisplayName(hubURL), "discovered", p.HubRole, p.SellDomains)
				if ent != nil && p.HubFees != nil && ent.FeePreset == "" {
					switch {
					case p.HubFees.SubmitWantSats == 0 && p.HubFees.SubmitOfferSats == 0 && p.HubFees.EgressSatsPer100Events == 0:
						ent.FeePreset = "free"
					case p.HubFees.SubmitWantSats == 0 && p.HubFees.SubmitOfferSats == 0 && p.HubFees.EgressSatsPer100Events > 0:
						ent.FeePreset = "lightbsv_egress"
					default:
						ent.FeePreset = "hybrid"
					}
				}
			}
		}
	}

	// Probe all candidates concurrently (skip live internet calls in offline memory preview unless explicit local/joined).
	entries := make([]*HubCatalogEntry, 0, len(byURL))
	for _, ent := range byURL {
		ent.PeersCount = peersPerHub[ent.URL]
		entries = append(entries, ent)
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for _, ent := range entries {
		if previewOnlyRemote && ent.Source == "seed" {
			ent.NetworkMismatch = true
			continue
		}
		wg.Add(1)
		go func(e *HubCatalogEntry) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			s.probeSingleHub(ctx, e)
		}(ent)
	}
	wg.Wait()

	out := make([]HubCatalogEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Connected != out[j].Connected {
			return out[i].Connected
		}
		if out[i].Reachable != out[j].Reachable {
			return out[i].Reachable
		}
		if (out[i].Role == "spine") != (out[j].Role == "spine") {
			return out[i].Role == "spine"
		}
		if out[i].LatencyMs > 0 && out[j].LatencyMs > 0 && out[i].LatencyMs != out[j].LatencyMs {
			return out[i].LatencyMs < out[j].LatencyMs
		}
		return out[i].Name < out[j].Name
	})

	cc.mu.Lock()
	cc.entries = make([]HubCatalogEntry, len(out))
	copy(cc.entries, out)
	cc.at = time.Now()
	cc.mu.Unlock()
	return out
}

func (s *Service) probeSingleHub(ctx context.Context, e *HubCatalogEntry) {
	pCtx, cancel := context.WithTimeout(ctx, hubProbeTimeout)
	defer cancel()

	t0 := time.Now()
	hubBase := strings.TrimRight(e.URL, "/")
	rootBase := strings.TrimSuffix(hubBase, "/hub")

	// 1. Fast /v1/health check (measures real RTT latency)
	var health struct {
		OK      bool   `json:"ok"`
		Network string `json:"network"`
	}
	code, err := s.getJSON(pCtx, hubBase+"/v1/health", &health)
	if err == nil && code == http.StatusOK && health.OK {
		e.Reachable = true
		e.LatencyMs = max(int64(1), time.Since(t0).Milliseconds())
		if health.Network != "" {
			e.Network = health.Network
		}
		if s.mem != nil && s.mem.DB() != nil && e.Connected {
			_ = s.mem.DB().UpdateHostHealth(e.URL, "up", time.Now().Unix())
		}
	} else {
		e.Reachable = false
		if err != nil {
			e.Error = err.Error()
		} else if code != 0 {
			e.Error = http.StatusText(code)
		}
		if s.mem != nil && s.mem.DB() != nil && e.Connected {
			_ = s.mem.DB().UpdateHostHealth(e.URL, "down", time.Now().Unix())
		}
		return
	}

	// 2. /v1/terms for role, exact satoshi fees, egress pricing & network metadata
	var terms struct {
		Network   string            `json:"network"`
		HubRole   string            `json:"hubRole"`
		FeePreset string            `json:"feePreset"`
		Fees      map[string]uint64 `json:"fees"`
		Egress    struct {
			SatsPer100Events uint64 `json:"satsPer100Events"`
			MinTopupSats     uint64 `json:"minTopupSats"`
		} `json:"egress"`
	}
	if tCode, tErr := s.getJSON(pCtx, hubBase+"/v1/terms", &terms); tErr == nil && tCode == http.StatusOK {
		if terms.Network != "" {
			e.Network = terms.Network
		}
		if terms.HubRole != "" {
			e.Role = terms.HubRole
		}
		if terms.FeePreset != "" {
			e.FeePreset = terms.FeePreset
		}
		fs := &HubFeeSummary{
			WantSats:         terms.Fees["submit:want"],
			OfferSats:        terms.Fees["submit:offer"],
			BboxSats:         terms.Fees["bbox:send"],
			EgressSatsPer100: terms.Egress.SatsPer100Events,
			MinTopupSats:     terms.Egress.MinTopupSats,
		}
		e.Fees = fs
		if e.FeePreset == "" {
			switch {
			case fs.WantSats == 0 && fs.OfferSats == 0 && fs.BboxSats == 0 && fs.EgressSatsPer100 == 0:
				e.FeePreset = "free"
			case fs.WantSats == 0 && fs.OfferSats == 0 && fs.EgressSatsPer100 > 0:
				e.FeePreset = "lightbsv_egress"
			default:
				e.FeePreset = "hybrid"
			}
		}
	}

	// 3. /.well-known/nektar for federated peer hub links (+ network fallback)
	if rootBase != "" && rootBase != hubBase {
		var wk struct {
			Network string   `json:"network"`
			Hosts   []string `json:"hosts"`
		}
		if wCode, wErr := s.getJSON(pCtx, rootBase+"/.well-known/nektar", &wk); wErr == nil && wCode == http.StatusOK {
			if e.Network == "" && wk.Network != "" {
				e.Network = wk.Network
			}
			for _, lh := range normalizeHosts(wk.Hosts) {
				if lh != e.URL {
					e.LinkedHubs = appendUnique(e.LinkedHubs, lh)
				}
			}
		}
	}

	// Default Spine link for the curated hub1..hub5.nektar.run fleet leaves -> nektar.run Spine.
	if strings.HasSuffix(e.Name, ".nektar.run") && e.Name != "nektar.run" {
		e.LinkedHubs = appendUnique(e.LinkedHubs, "https://nektar.run/hub")
	}

	if e.Network != "" && s.network != "" && !strings.EqualFold(e.Network, s.network) {
		e.NetworkMismatch = true
	}
}

// ConnectAllReachable probes all known hubs in parallel and connects to every reachable
// hub matching this node's BSV network (LightBSV 1-click multi-hub mesh bootstrap).
func (s *Service) ConnectAllReachable(ctx context.Context) (connected []string, skipped []string, catalog []HubCatalogEntry) {
	if s == nil {
		return nil, nil, nil
	}
	probed := s.ProbeHubCatalog(ctx, true)
	for i := range probed {
		e := &probed[i]
		if !e.Reachable || e.NetworkMismatch {
			skipped = append(skipped, e.URL)
			continue
		}
		if s.mem != nil && s.mem.DB() != nil {
			_ = s.mem.DB().UpsertHost(store.HostRecord{
				URL:      e.URL,
				State:    "up",
				Source:   "joined",
				Blocked:  false,
				LastSeen: time.Now().Unix(),
			})
		}
		e.Connected = true
		if e.Source == "blocked" {
			e.Source = "joined"
		}
		connected = append(connected, e.URL)
	}
	// Refresh cached catalog with updated Connected flags.
	cc := s.getCatalogCache()
	cc.mu.Lock()
	cc.entries = make([]HubCatalogEntry, len(probed))
	copy(cc.entries, probed)
	cc.at = time.Now()
	cc.mu.Unlock()
	return connected, skipped, probed
}
