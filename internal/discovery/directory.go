package discovery

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/resolve"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// SeedHubSpec describes one curated Layer-0 bootstrap hub in the nektar.run fleet.
type SeedHubSpec struct {
	URL    string
	Name   string
	Role   string
	Topics []string
}

// DefaultSeedSpecs returns curated Layer-0 bootstrap hubs and human metadata for each network.
// Explicit NEKTAR_MARKET_HOSTS always overrides; hubs are untrusted relays (every artifact is signature-verified).
func DefaultSeedSpecs(network string) []SeedHubSpec {
	switch network {
	case "testnet":
		return []SeedHubSpec{
			{URL: "https://testnet.nektar.run/hub", Name: "testnet.nektar.run", Role: "spine", Topics: []string{"general", "ops", "bsv"}},
			{URL: "https://hub1.nektar.run/hub", Name: "hub1.nektar.run", Role: "leaf", Topics: []string{"ops", "nektar", "agents"}},
			{URL: "https://hub2.nektar.run/hub", Name: "hub2.nektar.run", Role: "leaf", Topics: []string{"bsv", "spv", "runar"}},
			{URL: "https://hub3.nektar.run/hub", Name: "hub3.nektar.run", Role: "leaf", Topics: []string{"travel", "deals", "prosumer"}},
			{URL: "https://hub4.nektar.run/hub", Name: "hub4.nektar.run", Role: "leaf", Topics: []string{"research", "workflows", "ai"}},
			{URL: "https://hub5.nektar.run/hub", Name: "hub5.nektar.run", Role: "leaf", Topics: []string{"devops", "cloud", "security"}},
		}
	case "main":
		return []SeedHubSpec{
			{URL: "https://nektar.run/hub", Name: "nektar.run", Role: "spine", Topics: []string{"general", "bsv", "agents"}},
		}
	default:
		return nil
	}
}

// DefaultSeedHosts returns Layer-0 bootstrap seed hubs for each network.
// Explicit NEKTAR_MARKET_HOSTS always overrides; hosts are untrusted relays (every artifact is signature-verified).
func DefaultSeedHosts(network string) []string {
	specs := DefaultSeedSpecs(network)
	if len(specs) == 0 {
		return nil
	}
	out := make([]string, 0, len(specs))
	for _, sp := range specs {
		out = append(out, sp.URL)
	}
	return out
}

// PeerInfo summarizes one signature-verified operator/agent/hub discovered on the `dir` lane.
type PeerInfo struct {
	Pub               string               `json:"pub"`
	Handle            string               `json:"handle,omitempty"`
	PublicURL         string               `json:"publicUrl,omitempty"`
	BboxOffice        string               `json:"bboxOffice,omitempty"`
	Hosts             []string             `json:"hosts"`
	Domains           []string             `json:"domains,omitempty"`
	SellDomains       []string             `json:"sellDomains,omitempty"`
	BountyDomains     []string             `json:"bountyDomains,omitempty"`
	Packs             []bstack.PackRef     `json:"packs,omitempty"`
	IsHub             bool                 `json:"isHub,omitempty"`
	HubRole           string               `json:"hubRole,omitempty"`
	HubPayIdentityKey string               `json:"hubPayIdentityKey,omitempty"`
	HubTermsURL       string               `json:"hubTermsUrl,omitempty"`
	HubFees           *bstack.HubFeeAdvert `json:"hubFees,omitempty"`
	Exp               int64                `json:"exp"`
	SeenOnHosts       []string             `json:"seenOnHosts"`
}

// WithholdingWarning records when a queried host omitted a valid peer returned by a sibling host.
type WithholdingWarning struct {
	Host       string `json:"host"`
	MissingPub string `json:"missingPub"`
}

// Service manages Layer 0 (seeds/hosts), Layer 1 (dir lane AnnounceAd + bfinger handles),
// and Layer 2 (market-discovered CHIRP pack auto-seeding).
type Service struct {
	mu              sync.RWMutex
	mem             *memory.Service
	network         string
	envHosts        []string
	signer          *nwallet.MarketSigner
	keyInitErr      error
	httpClient      *http.Client
	termsCache      *HubTermsCache
	laneRouter      *hubLaneRouter
	catalogCache    hubCatalogCache
	topupMu         sync.Mutex
	topupLocks      map[string]*sync.Mutex
	feeApprover     HubFeeApprover
	feeRecorder     HubFeeRecorder
	carrierTree     *CarrierTreeManager
	peers           map[string]PeerInfo
	withholdings    []WithholdingWarning
	localHandle     string
	localPublicURL  string
	localBboxOffice string
}

// New constructs a discovery Service bound to mem's SQLite store & network.
func New(mem *memory.Service, envHosts []string, hc *http.Client) *Service {
	netName := ""
	if mem != nil && mem.Chain() != nil {
		netName = mem.Chain().Network()
	}
	if hc == nil {
		hc = core.SafeHTTPClientForNetwork(netName, 30*time.Second)
	}
	var signer *nwallet.MarketSigner
	var keyErr error
	if mem != nil {
		raw, err := mem.LoadOrCreateProtectedKey("node_market_key_v1", nwallet.GenerateProtectedKeyBytes)
		if err != nil {
			keyErr = err
		} else {
			signer, keyErr = nwallet.NewMarketSignerFromBytes(raw)
		}
	} else {
		signer, keyErr = nwallet.NewEphemeralMarketSigner()
	}
	cleanEnv := normalizeHosts(envHosts)
	return &Service{
		mem:        mem,
		network:    netName,
		envHosts:   cleanEnv,
		signer:     signer,
		keyInitErr: keyErr,
		httpClient: hc,
		termsCache: NewHubTermsCache(),
		laneRouter: newHubLaneRouter(),
		topupLocks: make(map[string]*sync.Mutex),
		peers:      make(map[string]PeerInfo),
	}
}

// SetHubTopics pre-populates the lane router's mounted-topic map for hostURL (avoids initial /health probe).
func (s *Service) SetHubTopics(hostURL string, topics []string) {
	if s != nil && s.laneRouter != nil {
		s.laneRouter.SetHostTopics(hostURL, topics)
	}
}

// SetLocalHandle configures this node's own BRC-169 handle metadata served by WellKnownHandleHandler.
func (s *Service) SetLocalHandle(handle, publicURL, bboxOffice string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.localHandle = strings.ToLower(strings.TrimSpace(handle))
	s.localPublicURL = strings.TrimRight(strings.TrimSpace(publicURL), "/")
	s.localBboxOffice = strings.TrimSpace(bboxOffice)
	s.mu.Unlock()
}

// SetHubFeePolicy wires operator hub-fee caps and daily spend recording into discovery's BRC-105 client.
func (s *Service) SetHubFeePolicy(approver HubFeeApprover, recorder HubFeeRecorder) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.feeApprover = approver
	s.feeRecorder = recorder
	s.mu.Unlock()
}

// AnnounceKey returns this node's encapsulated market/directory signer (also used for hub egress auth).
func (s *Service) AnnounceKey() *nwallet.MarketSigner {
	if s == nil {
		return nil
	}
	return s.signer
}

// AnnouncePubHex returns the compressed pubkey signing this node's AnnounceAd records.
func (s *Service) AnnouncePubHex() string {
	if s == nil || s.signer == nil {
		return ""
	}
	return s.signer.PubHex()
}

// Withholdings returns any multi-host directory withholding warnings detected during quorum reads.
func (s *Service) Withholdings() []WithholdingWarning {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]WithholdingWarning(nil), s.withholdings...)
}

// ActiveHosts merges env/seed hosts + persisted joined hosts minus explicitly blocked hosts.
func (s *Service) ActiveHosts() []string {
	persisted, _ := s.mem.DB().ListHosts()
	blocked := make(map[string]bool)
	seen := make(map[string]bool)
	var out []string
	for _, h := range persisted {
		u := normalizeHostURL(h.URL)
		if h.Blocked {
			blocked[u] = true
		}
	}
	add := func(u string) {
		u = normalizeHostURL(u)
		if u == "" || blocked[u] || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	base := s.envHosts
	if len(base) == 0 {
		base = DefaultSeedHosts(s.network)
	}
	for _, u := range base {
		add(u)
	}
	for _, h := range persisted {
		if !h.Blocked {
			add(h.URL)
		}
	}
	return out
}

// HostLists returns the env/seeds, saved joined, and blocked host lists for the Dashboard Settings tab.
func (s *Service) HostLists() (envList, added, blocked []string) {
	base := s.envHosts
	if len(base) == 0 {
		base = DefaultSeedHosts(s.network)
	}
	envList = append([]string(nil), base...)
	persisted, _ := s.mem.DB().ListHosts()
	for _, h := range persisted {
		if h.Blocked {
			blocked = append(blocked, h.URL)
		} else if h.Source == "joined" || h.Source == "dir" {
			added = append(added, h.URL)
		}
	}
	return envList, added, blocked
}

// JoinHost probes rawURL (/.well-known/nektar or /v1/health), persists it as an active Hub host,
// and crawls any peer hosts it advertises (supporting the Dashboard paste-anything join flow!).
func (s *Service) JoinHost(ctx context.Context, rawURL string) (hubURL string, discovered []string, err error) {
	base := normalizeHostURL(rawURL)
	if base == "" {
		return "", nil, errors.New("discovery: invalid host URL")
	}
	probeURL := base
	// Try probing BRC-180 /manifest.json first so pasting any LightBSV sovereign domain or Hub origin works seamlessly!
	if mf, mErr := s.fetchManifestFromBaseURL(ctx, base); mErr == nil && mf != nil {
		if lsURL, ok := mf.Overlay(bstack.LookupService); ok {
			if cand := normalizeHostURL(lsURL); cand != "" {
				probeURL = cand
			}
		}
		for _, ovURL := range mf.Overlays {
			if cand := normalizeHostURL(ovURL); cand != "" {
				discovered = appendUnique(discovered, cand)
			}
		}
	}
	// Also probe as direct Hub (/v1/health) OR as Nektar node (/.well-known/nektar -> /hub) on both base and probeURL.
	for _, wkOrigin := range appendUnique([]string{base}, probeURL) {
		var wellKnown struct {
			Hosts []string `json:"hosts"`
			Hub   string   `json:"hub"`
		}
		if resp, rErr := s.getJSON(ctx, wkOrigin+"/.well-known/nektar", &wellKnown); rErr == nil && resp == http.StatusOK {
			if wellKnown.Hub != "" {
				if strings.HasPrefix(wellKnown.Hub, "/") {
					probeURL = wkOrigin + strings.TrimRight(wellKnown.Hub, "/")
				} else if candidate := normalizeHostURL(wellKnown.Hub); candidate != "" && sameOriginURL(wkOrigin, candidate) {
					probeURL = candidate
				}
			} else {
				var hCheck map[string]any
				if code, _ := s.getJSON(ctx, wkOrigin+"/hub/v1/health", &hCheck); code == http.StatusOK {
					probeURL = wkOrigin + "/hub"
				}
			}
			for _, h := range normalizeHosts(wellKnown.Hosts) {
				discovered = appendUnique(discovered, h)
			}
			break
		}
	}

	var health struct {
		OK bool `json:"ok"`
	}
	code, hErr := s.getJSON(ctx, probeURL+"/v1/health", &health)
	if hErr != nil || code != http.StatusOK || !health.OK {
		return "", nil, fmt.Errorf("discovery: host %s health check failed (status=%d err=%v)", probeURL, code, hErr)
	}
	if err := s.mem.DB().UpsertHost(store.HostRecord{
		URL:      probeURL,
		State:    "up",
		Source:   "joined",
		Blocked:  false,
		LastSeen: time.Now().Unix(),
	}); err != nil {
		return "", nil, err
	}

	// Crawl dir lane on the newly joined hub to discover additional peer hosts!
	if peers, _ := s.DiscoverPeers(ctx, []string{probeURL}); len(peers) > 0 {
		seen := map[string]bool{probeURL: true}
		for _, d := range discovered {
			seen[d] = true
		}
		for _, p := range peers {
			for _, h := range p.Hosts {
				h = normalizeHostURL(h)
				if h != "" && !seen[h] {
					seen[h] = true
					discovered = append(discovered, h)
				}
			}
		}
	}
	return probeURL, discovered, nil
}

// LeaveHost removes a joined host or blocks an env/seed host so disconnect persists across restarts.
func (s *Service) LeaveHost(rawURL string) error {
	u := normalizeHostURL(rawURL)
	if u == "" {
		return errors.New("discovery: invalid host URL")
	}
	return s.mem.DB().BlockHost(u)
}

func (s *Service) fetchManifestFromBaseURL(ctx context.Context, base string) (*resolve.Manifest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/manifest.json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("manifest HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if err != nil {
		return nil, err
	}
	return resolve.ParseManifest(body)
}

func (s *Service) getJSON(ctx context.Context, u string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if out != nil && resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(out)
	}
	return resp.StatusCode, nil
}

func normalizeHostURL(raw string) string {
	u := strings.TrimSpace(raw)
	if u == "" {
		return ""
	}
	if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
		u = "https://" + u
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return ""
	}
	parsed.Scheme = scheme
	return strings.TrimRight(parsed.String(), "/")
}

func sameOriginURL(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host)
}

func normalizeHosts(in []string) []string {
	var out []string
	seen := make(map[string]bool)
	for _, h := range in {
		u := normalizeHostURL(h)
		if u != "" && !seen[u] {
			seen[u] = true
			out = append(out, u)
		}
	}
	return out
}

func appendUnique(list []string, item string) []string {
	for _, v := range list {
		if v == item {
			return list
		}
	}
	return append(list, item)
}

func packMatches(doms []string, want map[string]bool) bool {
	for _, d := range doms {
		if want[strings.ToLower(strings.TrimSpace(d))] {
			return true
		}
	}
	return false
}
