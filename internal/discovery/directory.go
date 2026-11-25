package discovery

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/chirp"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

// DefaultSeedHosts returns Layer-0 bootstrap seed hubs for each network.
// Explicit NEKTAR_MARKET_HOSTS always overrides; hosts are untrusted relays (every artifact is signature-verified).
func DefaultSeedHosts(network string) []string {
	switch network {
	case "testnet":
		return []string{"https://nektar.run/hub"}
	case "main":
		return []string{"https://nektar.run/hub"}
	default:
		return nil
	}
}

// PeerInfo summarizes one signature-verified operator/agent/hub discovered on the `dir` lane.
type PeerInfo struct {
	Pub           string           `json:"pub"`
	Handle        string           `json:"handle,omitempty"`
	PublicURL     string           `json:"publicUrl,omitempty"`
	BboxOffice    string           `json:"bboxOffice,omitempty"`
	Hosts         []string         `json:"hosts"`
	Domains       []string         `json:"domains,omitempty"`
	SellDomains   []string         `json:"sellDomains,omitempty"`
	BountyDomains []string         `json:"bountyDomains,omitempty"`
	Packs         []bstack.PackRef `json:"packs,omitempty"`
	IsHub         bool             `json:"isHub,omitempty"`
	Exp           int64            `json:"exp"`
	SeenOnHosts   []string         `json:"seenOnHosts"`
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
	peers        map[string]PeerInfo
	withholdings []WithholdingWarning
}

// New constructs a discovery Service bound to mem's SQLite store & network.
func New(mem *memory.Service, envHosts []string, hc *http.Client) *Service {
	if hc == nil {
		hc = &http.Client{Timeout: 12 * time.Second}
	}
	ak, _ := ec.NewPrivateKey()
	cleanEnv := normalizeHosts(envHosts)
	return &Service{
		mem:         mem,
		network:     mem.Chain().Network(),
		envHosts:    cleanEnv,
		announceKey: ak,
		httpClient:  hc,
		peers:       make(map[string]PeerInfo),
	}
}

// AnnouncePubHex returns the compressed pubkey signing this node's AnnounceAd records.
func (s *Service) AnnouncePubHex() string {
	return hex.EncodeToString(s.announceKey.PubKey().Compressed())
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
			} else {
				probeURL = normalizeHostURL(wellKnown.Hub)
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

// PublishAnnounce signs an AnnounceAd, wraps it in a canonical CBOR nkt1 AtomicBEEF, and submits
// it to the reserved `dir` nibble lane across all active hosts.
func (s *Service) PublishAnnounce(ctx context.Context, ad bstack.AnnounceAd) (int, error) {
	hosts := s.ActiveHosts()
	if len(hosts) == 0 {
		return 0, nil
	}
	if ad.Pub == "" {
		ad.Pub = s.AnnouncePubHex()
	}
	if ad.Net == "" {
		ad.Net = s.network
	}
	if len(ad.Hosts) == 0 {
		ad.Hosts = hosts
	}
	if ad.Exp == 0 {
		ad.Exp = time.Now().Add(72 * time.Hour).UnixMilli()
	}
	sig, err := bstack.SignArtifact(s.announceKey, ad)
	if err != nil {
		return 0, err
	}
	body, err := json.Marshal(bstack.SignedAnnounce{Announce: ad, Sig: sig})
	if err != nil {
		return 0, err
	}
	beef, err := BuildMarketBEEF(bstack.MarketPayload{Kind: bstack.KindAnnounce, Body: body})
	if err != nil {
		return 0, err
	}
	lane := bstack.LaneOf(ad.Net, bstack.DirDomain)
	submitted := 0
	for _, h := range hosts {
		if err := s.SubmitBEEF(ctx, h, beef, []string{lane}); err == nil {
			submitted++
		}
	}
	return submitted, nil
}

// DiscoverPeers quorum-reads the signed `dir` lane across hosts, verifies every signature,
// detects host withholding, and caches discovered peers locally.
func (s *Service) DiscoverPeers(ctx context.Context, hosts []string) ([]PeerInfo, error) {
	if len(hosts) == 0 {
		hosts = s.ActiveHosts()
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	qBytes, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   bstack.MarketQuery{Kind: bstack.KindAnnounce, Domain: bstack.DirDomain, Limit: 200},
	})

	byPub := make(map[string]*PeerInfo)
	perHostSeen := make(map[string]map[string]bool)

	for _, h := range hosts {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, h+"/lookup", bytes.NewReader(qBytes))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := s.httpClient.Do(req)
		if err != nil || resp.StatusCode != http.StatusOK {
			if resp != nil {
				resp.Body.Close()
			}
			_ = s.mem.DB().UpsertHost(store.HostRecord{URL: h, State: "down", Source: "joined", LastSeen: time.Now().Unix()})
			continue
		}
		var ans struct {
			Result []bstack.MarketEvent `json:"result"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ans)
		resp.Body.Close()
		_ = s.mem.DB().UpsertHost(store.HostRecord{URL: h, State: "up", Source: "joined", LastSeen: time.Now().Unix()})

		perHostSeen[h] = make(map[string]bool)
		for _, ev := range ans.Result {
			if ev.Payload.Kind != bstack.KindAnnounce {
				continue
			}
			var sa bstack.SignedAnnounce
			if err := json.Unmarshal(ev.Payload.Body, &sa); err != nil {
				continue
			}
			if err := bstack.VerifyArtifact(sa.Announce.Pub, sa.Sig, sa.Announce); err != nil {
				continue // reject spoofed/invalid announce!
			}
			perHostSeen[h][sa.Announce.Pub] = true
			existing, ok := byPub[sa.Announce.Pub]
			if !ok || sa.Announce.Exp > existing.Exp {
				seenHosts := []string{h}
				if ok {
					seenHosts = appendUnique(existing.SeenOnHosts, h)
				}
				byPub[sa.Announce.Pub] = &PeerInfo{
					Pub:           sa.Announce.Pub,
					Handle:        sa.Announce.Handle,
					PublicURL:     sa.Announce.PublicURL,
					BboxOffice:    sa.Announce.BboxOffice,
					Hosts:         sa.Announce.Hosts,
					Domains:       sa.Announce.Domains,
					SellDomains:   sa.Announce.SellDomains,
					BountyDomains: sa.Announce.BountyDomains,
					Packs:         sa.Announce.Packs,
					IsHub:         sa.Announce.IsHub,
					Exp:           sa.Announce.Exp,
					SeenOnHosts:   seenHosts,
				}
			} else {
				existing.SeenOnHosts = appendUnique(existing.SeenOnHosts, h)
			}
		}
	}

	// Detect withholding across healthy multi-host quorums.
	var warnings []WithholdingWarning
	if len(perHostSeen) >= 2 {
		for pub := range byPub {
			for h, seen := range perHostSeen {
				if !seen[pub] {
					warnings = append(warnings, WithholdingWarning{Host: h, MissingPub: pub})
				}
			}
		}
	}

	out := make([]PeerInfo, 0, len(byPub))
	s.mu.Lock()
	for pub, p := range byPub {
		s.peers[pub] = *p
		out = append(out, *p)
	}
	s.withholdings = warnings
	s.mu.Unlock()
	return out, nil
}

// Peers returns all currently known verified peers.
func (s *Service) Peers() []PeerInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PeerInfo, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	return out
}

// ResolveHandle looks up a peer by its bfinger handle (e.g. "alice@nektar.run") or pubkey prefix.
func (s *Service) ResolveHandle(handleOrPub string) (PeerInfo, bool) {
	q := strings.ToLower(strings.TrimSpace(handleOrPub))
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, p := range s.peers {
		if strings.ToLower(p.Handle) == q || strings.HasPrefix(strings.ToLower(p.Pub), q) {
			return p, true
		}
	}
	return PeerInfo{}, false
}

// SeedFromNetwork (Layer 2) discovers announced packs matching seedDomains ("*" = all),
// downloads & verifies their BRC-167 CHIRP closures from the announcing peer's /v1/chirp/{hash},
// and imports verified fragments locally so cold-started agents never start empty.
func (s *Service) SeedFromNetwork(ctx context.Context, seedDomains []string) (int, error) {
	peers, err := s.DiscoverPeers(ctx, nil)
	if err != nil {
		return 0, err
	}
	wantAll := false
	wantDom := make(map[string]bool)
	for _, d := range seedDomains {
		d = strings.ToLower(strings.TrimSpace(d))
		if d == "*" {
			wantAll = true
		} else if d != "" {
			wantDom[d] = true
		}
	}
	if !wantAll && len(wantDom) == 0 {
		return 0, nil
	}

	totalImported := 0
	for _, peer := range peers {
		if peer.PublicURL == "" || len(peer.Packs) == 0 {
			continue
		}
		for _, pk := range peer.Packs {
			if !wantAll && !packMatches(pk.Domains, wantDom) {
				continue
			}
			raw, err := s.FetchRemoteChirp(ctx, peer.PublicURL, pk.ChirpURL)
			if err != nil {
				continue
			}
			n, _, err := s.mem.ImportShardPackBytes(raw, pk.Shard)
			if err == nil {
				totalImported += n
			}
		}
	}
	return totalImported, nil
}

// FetchRemoteChirp resolves a chirp:// URL from a remote Nektar node's GET /v1/chirp/{hash},
// verifying every node & blob cryptographically via lightwebinc/bcommon/chirp.Verify!
func (s *Service) FetchRemoteChirp(ctx context.Context, peerBaseURL, chirpURL string) ([]byte, error) {
	rootHash, err := chirp.ParseURL(chirpURL)
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(peerBaseURL, "/")
	held := make(map[[32]byte][]byte)
	closure, err := chirp.Verify(rootHash, func(h [32]byte) ([]byte, error) {
		if b, err := s.mem.Objects().GetObject(h); err == nil {
			held[h] = b
			return b, nil
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v1/chirp/"+hex.EncodeToString(h[:]), nil)
		if err != nil {
			return nil, err
		}
		resp, err := s.httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("discovery: remote chirp HTTP %d", resp.StatusCode)
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		if err != nil {
			return nil, err
		}
		if sha256.Sum256(b) != h {
			return nil, errors.New("discovery: remote chirp object hash mismatch")
		}
		_ = s.mem.Objects().PutObject(h, b)
		held[h] = b
		return b, nil
	}, chirp.Limits{})
	if err != nil {
		return nil, fmt.Errorf("discovery: verify remote chirp closure: %w", err)
	}
	var out []byte
	for _, ref := range closure.Blobs {
		b, ok := held[ref.Hash]
		if !ok {
			return nil, errors.New("discovery: missing blob in verified closure")
		}
		out = append(out, b...)
	}
	return out, nil
}

// BuildMarketBEEF encodes a MarketPayload into an OP_FALSE OP_RETURN AtomicBEEF transaction for Hub /submit.
func BuildMarketBEEF(p bstack.MarketPayload) ([]byte, error) {
	cborBytes, err := bstack.EncodePayload(p)
	if err != nil {
		return nil, err
	}
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	return tx.AtomicBEEF(false)
}

// SubmitBEEF posts an AtomicBEEF to a Hub's /submit endpoint for topics.
func (s *Service) SubmitBEEF(ctx context.Context, hubURL string, beef []byte, topics []string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(hubURL, "/")+"/submit", bytes.NewReader(beef))
	if err != nil {
		return err
	}
	tHdr, _ := json.Marshal(topics)
	req.Header.Set("X-Topics", string(tHdr))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("hub submit HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
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
	return strings.TrimRight(u, "/")
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
