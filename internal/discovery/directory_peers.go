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
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/chirp"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

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
	// Guarantee post-CBOR payload fits under bstack.MaxPayload (16KB), trimming Packs if oversized!
	var body []byte
	for {
		sig, err := bstack.SignArtifact(s.announceKey, ad)
		if err != nil {
			return 0, err
		}
		body, err = json.Marshal(bstack.SignedAnnounce{Announce: ad, Sig: sig})
		if err != nil {
			return 0, err
		}
		if encoded, encErr := bstack.EncodePayload(bstack.MarketPayload{Kind: bstack.KindAnnounce, Body: body}); encErr == nil && len(encoded) <= bstack.MaxPayload {
			break
		}
		if len(ad.Packs) == 0 {
			return 0, errors.New("discovery: AnnounceAd exceeds MaxPayload even with 0 packs")
		}
		ad.Packs = ad.Packs[:len(ad.Packs)/2]
	}
	payload := bstack.MarketPayload{Kind: bstack.KindAnnounce, Body: body}
	lane := bstack.LaneOf(ad.Net, bstack.DirDomain)
	submitted, err := s.BuildAndSubmitMarketAd(ctx, s.announceKey, payload, hosts, []string{lane})
	if submitted > 0 {
		return submitted, nil
	}
	return 0, err
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
			_ = s.mem.DB().UpdateHostHealth(h, "down", time.Now().Unix())
			continue
		}
		var ans struct {
			Result []bstack.MarketEvent `json:"result"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&ans)
		resp.Body.Close()
		_ = s.mem.DB().UpdateHostHealth(h, "up", time.Now().Unix())

		nowTime := time.Now()
		nowMsCheck := nowTime.UnixMilli()
		maxExpMs := nowMsCheck + bstack.MaxLifetime.Milliseconds() + 60_000
		isLive := s.network == "main" || s.network == "testnet"
		hostSeen := make(map[string]bool)
		hostIncomplete := false
		for _, ev := range ans.Result {
			payload := ev.Payload
			if isLive && len(ev.BEEF) == 0 {
				continue // live networks require carried BEEF on directory lookups
			}
			if len(ev.BEEF) > 0 {
				var tx *transaction.Transaction
				_, parsedTx, _, pErr := guard.ParseBEEF(ev.BEEF, guard.DefaultBound)
				if pErr == nil && parsedTx != nil {
					tx = parsedTx
				} else if !isLive && pErr != nil && strings.Contains(pErr.Error(), "no inputs") && len(ev.BEEF) <= 64<<10 {
					if t0, tErr := transaction.NewTransactionFromBEEF(ev.BEEF); tErr == nil && t0 != nil && len(t0.Inputs) == 0 {
						tx = t0
					}
				}
				if tx == nil || (ev.TxID != "" && !strings.EqualFold(tx.TxID().String(), ev.TxID)) || int(ev.Vout) >= len(tx.Outputs) {
					continue
				}
				if isLive {
					vout, verifiedPayload, cErr := bstack.VerifyMarketCarrier(tx, s.network, ev.Topic, nowTime)
					if cErr != nil || vout != ev.Vout {
						continue
					}
					if s.mem == nil || s.mem.Chain() == nil || s.mem.Chain().Tracker() == nil {
						hostIncomplete = true
						continue
					}
					verdict, vErr := verify.Check(ctx, tx, s.mem.Chain().Tracker())
					if verdict == verify.Transport {
						// Tracker outage is an unknown verification state, never peer absence or forgery.
						hostIncomplete = true
						continue
					}
					if verdict != verify.Passed || vErr != nil {
						continue
					}
					payload = verifiedPayload
				} else {
					extracted, ok, _ := bstack.ExtractNkt1Payload(tx.Outputs[ev.Vout].LockingScript)
					if !ok {
						continue
					}
					payload = extracted
				}
			}
			if payload.Kind != bstack.KindAnnounce {
				continue
			}
			var sa bstack.SignedAnnounce
			if err := json.Unmarshal(payload.Body, &sa); err != nil {
				continue
			}
			// Reject cross-network replay, missing/expired Exp, or unbounded Exp.
			if sa.Announce.Net != s.network || sa.Announce.Exp <= nowMsCheck || sa.Announce.Exp > maxExpMs {
				continue
			}
			if err := bstack.VerifyArtifact(sa.Announce.Pub, sa.Sig, sa.Announce); err != nil {
				continue // reject spoofed/invalid announce!
			}
			hostSeen[sa.Announce.Pub] = true
			existing, ok := byPub[sa.Announce.Pub]
			if !ok || sa.Announce.Exp > existing.Exp {
				seenHosts := []string{h}
				if ok {
					seenHosts = appendUnique(existing.SeenOnHosts, h)
				}
				byPub[sa.Announce.Pub] = &PeerInfo{
					Pub:               sa.Announce.Pub,
					Handle:            sa.Announce.Handle,
					PublicURL:         sa.Announce.PublicURL,
					BboxOffice:        sa.Announce.BboxOffice,
					Hosts:             sa.Announce.Hosts,
					Domains:           sa.Announce.Domains,
					SellDomains:       sa.Announce.SellDomains,
					BountyDomains:     sa.Announce.BountyDomains,
					Packs:             sa.Announce.Packs,
					IsHub:             sa.Announce.IsHub,
					HubRole:           sa.Announce.HubRole,
					HubPayIdentityKey: sa.Announce.HubPayIdentityKey,
					HubTermsURL:       sa.Announce.HubTermsURL,
					HubFees:           sa.Announce.HubFees,
					Exp:               sa.Announce.Exp,
					SeenOnHosts:       seenHosts,
				}
			} else {
				existing.SeenOnHosts = appendUnique(existing.SeenOnHosts, h)
			}
		}
		if !hostIncomplete {
			perHostSeen[h] = hostSeen
		}
	}

	// Detect withholding across healthy multi-host quorums and quarantine disputed peers
	// seen on fewer than strict majority (>50%) of responding hosts.
	var warnings []WithholdingWarning
	quorum := 1
	if len(perHostSeen) >= 2 {
		quorum = len(perHostSeen)/2 + 1
		for pub := range byPub {
			for h, seen := range perHostSeen {
				if !seen[pub] {
					warnings = append(warnings, WithholdingWarning{Host: h, MissingPub: pub})
				}
			}
		}
	}

	nowMs := time.Now().UnixMilli()
	out := make([]PeerInfo, 0, len(byPub))
	s.mu.Lock()
	// Evict any expired peers from the local cache.
	for pub, old := range s.peers {
		if old.Exp > 0 && old.Exp < nowMs {
			delete(s.peers, pub)
		}
	}
	for pub, p := range byPub {
		if p.Exp > 0 && p.Exp < nowMs {
			continue
		}
		if len(p.SeenOnHosts) < quorum {
			// Disputed peer omitted by a quorum of sibling hosts: quarantine rather than blindly unioning.
			delete(s.peers, pub)
			continue
		}
		s.peers[pub] = *p
		out = append(out, *p)
	}
	s.withholdings = warnings
	s.mu.Unlock()
	return out, nil
}

// Peers returns all currently known unexpired verified peers.
func (s *Service) Peers() []PeerInfo {
	nowMs := time.Now().UnixMilli()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]PeerInfo, 0, len(s.peers))
	for _, p := range s.peers {
		if p.Exp > 0 && p.Exp < nowMs {
			continue
		}
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
	base := normalizeHostURL(peerBaseURL)
	if base == "" {
		return nil, errors.New("discovery: invalid peerBaseURL")
	}
	if _, err := core.ValidateOutboundURL(base, core.SafeHTTPOpts{
		AllowPrivate: core.AllowPrivateOutboundForNetwork(s.network),
		RequireHTTPS: s.network == "main",
	}); err != nil {
		return nil, fmt.Errorf("discovery: blocked remote chirp host: %w", err)
	}
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
		b, err := io.ReadAll(io.LimitReader(resp.Body, bstack.MaxChirpObjectBytes+1))
		if err != nil {
			return nil, err
		}
		if len(b) > bstack.MaxChirpObjectBytes {
			return nil, fmt.Errorf("discovery: remote chirp object exceeds max %d bytes", bstack.MaxChirpObjectBytes)
		}
		if sha256.Sum256(b) != h {
			return nil, errors.New("discovery: remote chirp object hash mismatch")
		}
		_ = s.mem.Objects().PutObject(h, b)
		held[h] = b
		return b, nil
	}, bstack.DefaultChirpLimits())
	if err != nil {
		return nil, fmt.Errorf("discovery: verify remote chirp closure: %w", err)
	}
	var out []byte
	for _, ref := range closure.Blobs {
		b, ok := held[ref.Hash]
		if !ok {
			return nil, errors.New("discovery: missing blob in verified closure")
		}
		if len(out)+len(b) > bstack.MaxChirpTotalBytes {
			return nil, fmt.Errorf("discovery: reassembled remote chirp exceeds max %d bytes", bstack.MaxChirpTotalBytes)
		}
		out = append(out, b...)
	}
	return out, nil
}
