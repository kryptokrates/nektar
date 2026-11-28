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

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/chirp"
	"github.com/lightwebinc/bcommon/guard"
	"github.com/lightwebinc/bcommon/verify"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
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
	var payload bstack.MarketPayload
	for {
		sig, err := bstack.SignArtifactWithSigner(s.signer, ad)
		if err != nil {
			return 0, err
		}
		payload, err = bstack.NewAnnouncePayload(bstack.SignedAnnounce{Announce: ad, Sig: sig})
		if err != nil {
			return 0, err
		}
		if encoded, encErr := bstack.EncodePayload(payload); encErr == nil && len(encoded) <= bstack.MaxPayload {
			break
		}
		if len(ad.Packs) == 0 {
			return 0, errors.New("discovery: AnnounceAd exceeds MaxPayload even with 0 packs")
		}
		ad.Packs = ad.Packs[:len(ad.Packs)/2]
	}
	lane := bstack.LaneOf(ad.Net, bstack.DirDomain)
	submitted, err := s.BuildAndSubmitMarketAd(ctx, s.signer, payload, hosts, []string{lane})
	if submitted > 0 {
		return submitted, nil
	}
	return 0, err
}

// DiscoverPeers quorum-reads the signed `dir` lane across hosts in parallel, deduplicates BEEF/SPV
// verification across sibling hosts, verifies every signature, detects host withholding, and caches discovered peers locally.
func (s *Service) DiscoverPeers(ctx context.Context, hosts []string) ([]PeerInfo, error) {
	if len(hosts) == 0 {
		hosts = s.ActiveHosts()
	}
	if len(hosts) == 0 {
		return nil, nil
	}
	qBytes, _ := json.Marshal(map[string]any{
		"service": bstack.LookupService,
		"query":   bstack.MarketQuery{Kind: bstack.KindAnnounce, Domain: bstack.DirDomain, Limit: 1000},
	})

	type hostFetchResult struct {
		host   string
		events []bstack.MarketEvent
		ok     bool
	}
	fetched := make([]hostFetchResult, len(hosts))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for idx, h := range hosts {
		wg.Add(1)
		go func(i int, hostURL string) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, hostURL+"/lookup", bytes.NewReader(qBytes))
			if err != nil {
				return
			}
			req.Header.Set("Content-Type", "application/json")
			resp, err := s.httpClient.Do(req)
			if err != nil || resp.StatusCode != http.StatusOK {
				if resp != nil {
					resp.Body.Close()
				}
				_ = s.mem.DB().UpdateHostHealth(hostURL, "down", time.Now().Unix())
				return
			}
			var ans struct {
				Result []bstack.MarketEvent `json:"result"`
			}
			_ = json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&ans)
			resp.Body.Close()
			_ = s.mem.DB().UpdateHostHealth(hostURL, "up", time.Now().Unix())
			fetched[i] = hostFetchResult{host: hostURL, events: ans.Result, ok: true}
		}(idx, h)
	}
	wg.Wait()

	byPub := make(map[string]*PeerInfo)
	perHostSeen := make(map[string]map[string]bool)

	type verifiedAnn struct {
		ann       bstack.AnnounceAd
		transport bool
		valid     bool
	}
	verifiedCache := make(map[[32]byte]verifiedAnn)

	nowTime := time.Now()
	nowMsCheck := nowTime.UnixMilli()
	maxExpMs := nowMsCheck + bstack.MaxLifetime.Milliseconds() + 60_000
	isLive := s.network == "main" || s.network == "testnet"

	for _, fr := range fetched {
		if !fr.ok {
			continue
		}
		h := fr.host
		hostSeen := make(map[string]bool)
		hostIncomplete := false
		for _, ev := range fr.events {
			if isLive && len(ev.BEEF) == 0 {
				continue // live networks require carried BEEF on directory lookups
			}
			var cacheKey [32]byte
			hasCacheKey := false
			if len(ev.BEEF) > 0 {
				hSum := sha256.New()
				_, _ = hSum.Write(ev.BEEF)
				_, _ = hSum.Write([]byte{':'})
				_, _ = hSum.Write([]byte(ev.Topic))
				_, _ = fmt.Fprintf(hSum, ":%d", ev.Vout)
				copy(cacheKey[:], hSum.Sum(nil))
				hasCacheKey = true
				if cached, ok := verifiedCache[cacheKey]; ok {
					if cached.transport {
						hostIncomplete = true
						continue
					}
					if !cached.valid {
						continue
					}
					hostSeen[cached.ann.Pub] = true
					existing, eok := byPub[cached.ann.Pub]
					if !eok || cached.ann.Exp > existing.Exp {
						seenHosts := []string{h}
						if eok {
							seenHosts = appendUnique(existing.SeenOnHosts, h)
						}
						byPub[cached.ann.Pub] = &PeerInfo{
							Pub:               cached.ann.Pub,
							Handle:            cached.ann.Handle,
							PublicURL:         cached.ann.PublicURL,
							BboxOffice:        cached.ann.BboxOffice,
							Hosts:             cached.ann.Hosts,
							Domains:           cached.ann.Domains,
							SellDomains:       cached.ann.SellDomains,
							BountyDomains:     cached.ann.BountyDomains,
							Packs:             cached.ann.Packs,
							IsHub:             cached.ann.IsHub,
							HubRole:           cached.ann.HubRole,
							HubPayIdentityKey: cached.ann.HubPayIdentityKey,
							HubTermsURL:       cached.ann.HubTermsURL,
							HubFees:           cached.ann.HubFees,
							Exp:               cached.ann.Exp,
							SeenOnHosts:       seenHosts,
						}
					} else {
						existing.SeenOnHosts = appendUnique(existing.SeenOnHosts, h)
					}
					continue
				}
			}

			payload := ev.Payload
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
					if hasCacheKey {
						verifiedCache[cacheKey] = verifiedAnn{}
					}
					continue
				}
				if isLive {
					vout, verifiedPayload, cErr := bstack.VerifyMarketCarrier(tx, s.network, ev.Topic, nowTime)
					if cErr != nil || vout != ev.Vout {
						if hasCacheKey {
							verifiedCache[cacheKey] = verifiedAnn{}
						}
						continue
					}
					if s.mem == nil || s.mem.Chain() == nil || s.mem.Chain().Tracker() == nil {
						hostIncomplete = true
						if hasCacheKey {
							verifiedCache[cacheKey] = verifiedAnn{transport: true}
						}
						continue
					}
					verdict, vErr := verify.Check(ctx, tx, s.mem.Chain().Tracker())
					if verdict == verify.Transport {
						// Tracker outage is an unknown verification state, never peer absence or forgery.
						hostIncomplete = true
						if hasCacheKey {
							verifiedCache[cacheKey] = verifiedAnn{transport: true}
						}
						continue
					}
					if verdict != verify.Passed || vErr != nil {
						if hasCacheKey {
							verifiedCache[cacheKey] = verifiedAnn{}
						}
						continue
					}
					payload = verifiedPayload
				} else {
					extracted, ok, _ := bstack.ExtractNkt1Payload(tx.Outputs[ev.Vout].LockingScript)
					if !ok {
						if hasCacheKey {
							verifiedCache[cacheKey] = verifiedAnn{}
						}
						continue
					}
					payload = extracted
				}
			}
			if payload.Kind != bstack.KindAnnounce {
				if hasCacheKey {
					verifiedCache[cacheKey] = verifiedAnn{}
				}
				continue
			}
			sa, err := bstack.DecodeAnnouncePayload(payload.Body)
			if err != nil {
				if hasCacheKey {
					verifiedCache[cacheKey] = verifiedAnn{}
				}
				continue
			}
			// Reject cross-network replay, missing/expired Exp, or unbounded Exp.
			if sa.Announce.Net != s.network || sa.Announce.Exp <= nowMsCheck || sa.Announce.Exp > maxExpMs {
				if hasCacheKey {
					verifiedCache[cacheKey] = verifiedAnn{}
				}
				continue
			}
			if err := bstack.VerifyArtifactRaw(sa.Announce.Pub, sa.Sig, sa.Announce.DomainTag(), sa.RawCBOR); err != nil {
				if hasCacheKey {
					verifiedCache[cacheKey] = verifiedAnn{}
				}
				continue // reject spoofed/invalid announce!
			}
			if hasCacheKey {
				verifiedCache[cacheKey] = verifiedAnn{ann: sa.Announce, valid: true}
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
// Peers returns all non-expired verified peers currently cached in memory.
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

// UpsertPeer registers or refreshes a verified peer entry in memory (used by direct peer sync and tests).
func (s *Service) UpsertPeer(p PeerInfo) {
	if s == nil || strings.TrimSpace(p.Pub) == "" {
		return
	}
	pub := strings.ToLower(strings.TrimSpace(p.Pub))
	p.Pub = pub
	s.mu.Lock()
	if s.peers == nil {
		s.peers = make(map[string]PeerInfo)
	}
	s.peers[pub] = p
	s.mu.Unlock()
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
			// Chase any external CHIRP blobs referenced by thin network capsules concurrently BEFORE fail-closed VerifyCapsule.
			if memrepo.IsCapsuleBytes(raw) {
				if capPeek, pErr := memrepo.DecodeCapsule(raw); pErr == nil && capPeek != nil {
					var missingURLs []string
					seenURL := make(map[string]bool)
					for _, b := range capPeek.Blobs {
						if b.Redacted || b.Encrypted || len(b.Inline) > 0 || b.ChirpURL == "" || seenURL[b.ChirpURL] {
							continue
						}
						if _, rErr := bstack.ResolveChirp(s.mem.Objects(), b.ChirpURL); rErr != nil {
							seenURL[b.ChirpURL] = true
							missingURLs = append(missingURLs, b.ChirpURL)
						}
					}
					if len(missingURLs) > 0 {
						sem := make(chan struct{}, 16)
						var bwg sync.WaitGroup
						pURL := peer.PublicURL
						for _, cu := range missingURLs {
							bwg.Add(1)
							go func(u string) {
								defer bwg.Done()
								select {
								case sem <- struct{}{}:
									defer func() { <-sem }()
								case <-ctx.Done():
									return
								}
								_, _ = s.FetchRemoteChirp(ctx, pURL, u)
							}(cu)
						}
						bwg.Wait()
					}
				}
			}
			n, _, err := s.mem.ImportShardPackBytesCtx(ctx, raw, pk.Shard)
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
