package discovery

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// DefaultSeedHosts returns Layer-0 bootstrap seed hubs for each network.
// Explicit NEKTAR_MARKET_HOSTS always overrides; hosts are untrusted relays (every artifact is signature-verified).
func DefaultSeedHosts(network string) []string {
	switch network {
	case "testnet":
		return []string{"https://testnet.nektar.run/hub"}
	case "main":
		return []string{"https://nektar.run/hub"}
	default:
		return nil
	}
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
	mu           sync.RWMutex
	mem          *memory.Service
	network      string
	envHosts     []string
	announceKey  *ec.PrivateKey
	httpClient   *http.Client
	termsCache   *HubTermsCache
	feeApprover  HubFeeApprover
	feeRecorder  HubFeeRecorder
	carrierTree  *CarrierTreeManager
	peers        map[string]PeerInfo
	withholdings []WithholdingWarning
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
	var ak *ec.PrivateKey
	if mem != nil {
		if raw, err := mem.LoadOrCreateProtectedKey("node_market_key_v1", func() ([]byte, error) {
			k, err := ec.NewPrivateKey()
			if err != nil {
				return nil, err
			}
			return k.Serialize(), nil
		}); err == nil && len(raw) == 32 {
			ak, _ = ec.PrivateKeyFromBytes(raw)
		}
	}
	if ak == nil {
		ak, _ = ec.NewPrivateKey()
	}
	cleanEnv := normalizeHosts(envHosts)
	return &Service{
		mem:         mem,
		network:     netName,
		envHosts:    cleanEnv,
		announceKey: ak,
		httpClient:  hc,
		termsCache:  NewHubTermsCache(),
		peers:       make(map[string]PeerInfo),
	}
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

// AnnounceKey returns this node's announce/directory signing key (also used for hub egress auth).
func (s *Service) AnnounceKey() *ec.PrivateKey {
	return s.announceKey
}

// AnnouncePubHex returns the compressed pubkey signing this node's AnnounceAd records.
func (s *Service) AnnouncePubHex() string {
	return hex.EncodeToString(s.announceKey.PubKey().Compressed())
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
	// Try probing as direct Hub (/v1/health) OR as Nektar node (/.well-known/nektar -> /hub).
	probeURL := base
	var wellKnown struct {
		Hosts []string `json:"hosts"`
		Hub   string   `json:"hub"`
	}
	if resp, rErr := s.getJSON(ctx, base+"/.well-known/nektar", &wellKnown); rErr == nil && resp == http.StatusOK {
		if wellKnown.Hub != "" {
			if strings.HasPrefix(wellKnown.Hub, "/") {
				probeURL = base + strings.TrimRight(wellKnown.Hub, "/")
			} else if candidate := normalizeHostURL(wellKnown.Hub); candidate != "" && sameOriginURL(base, candidate) {
				probeURL = candidate
			}
		} else {
			// Check if /hub/v1/health answers on this node.
			var hCheck map[string]any
			if code, _ := s.getJSON(ctx, base+"/hub/v1/health", &hCheck); code == http.StatusOK {
				probeURL = base + "/hub"
			}
		}
		discovered = append(discovered, normalizeHosts(wellKnown.Hosts)...)
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
