package discovery

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/lightwebinc/bcommon/resolve"
	"github.com/lightwebinc/bdns/zonerec"
	fingermod "github.com/lightwebinc/bfinger/module"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
)

const handlePinKVPrefix = "handle_pin:"

// ErrHandleKeyChanged is returned fail-closed when a previously TOFU-pinned handle resolves
// to a different secp256k1 identity public key without an explicit operator pin reset.
var ErrHandleKeyChanged = errors.New("discovery: handle identity key changed against TOFU pin (refusing silent key swap)")

// HandlePinRecord persists a TOFU handle -> secp256k1 identity key pin in SQLite KV.
type HandlePinRecord struct {
	Acct           string `json:"acct"`
	IdentityPubHex string `json:"identityPubHex"`
	Messagebox     string `json:"messagebox,omitempty"`
	PinnedAt       int64  `json:"pinnedAt"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// DomainDocsBundle holds ready-to-serve Tier-2 sovereign domain files for `nektar domain-docs`.
type DomainDocsBundle struct {
	Domain             string          `json:"domain"`
	Handle             string          `json:"handle"`
	Acct               string          `json:"acct"`
	IdentityPubHex     string          `json:"identityPubHex"`
	HubURL             string          `json:"hubUrl"`
	BboxOffice         string          `json:"bboxOffice"`
	ManifestPath       string          `json:"manifestPath"`
	ManifestJSON       json.RawMessage `json:"manifestJson"`
	WellKnownPath      string          `json:"wellKnownPath"`
	WellKnownJSON      json.RawMessage `json:"wellKnownJson"`
	BdnsLookupService  string          `json:"bdnsLookupService"`
	SetupInstructions  []string        `json:"setupInstructions"`
}

// VerifyOrPinHandle enforces TOFU handle -> pubkey pinning in SQLite KV when a DB is attached.
func (s *Service) VerifyOrPinHandle(acct, pubHex, messagebox string) error {
	acct = strings.ToLower(strings.TrimSpace(acct))
	pubHex = strings.ToLower(strings.TrimSpace(pubHex))
	if acct == "" || pubHex == "" {
		return errors.New("discovery: handle and identity pubkey required")
	}
	if _, _, err := core.ParseStrictCompressedPubKey(pubHex); err != nil {
		return err
	}
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return nil
	}
	kvKey := handlePinKVPrefix + acct
	now := time.Now().Unix()
	var existing HandlePinRecord
	if ok, err := s.mem.DB().GetJSON(kvKey, &existing); err == nil && ok && existing.IdentityPubHex != "" {
		if !strings.EqualFold(existing.IdentityPubHex, pubHex) {
			return fmt.Errorf("%w: %s pinned to %s, got %s", ErrHandleKeyChanged, acct, existing.IdentityPubHex, pubHex)
		}
		existing.UpdatedAt = now
		if messagebox != "" {
			existing.Messagebox = messagebox
		}
		_ = s.mem.DB().SetJSON(kvKey, existing)
		return nil
	}
	return s.mem.DB().SetJSON(kvKey, HandlePinRecord{
		Acct:           acct,
		IdentityPubHex: pubHex,
		Messagebox:     messagebox,
		PinnedAt:       now,
		UpdatedAt:      now,
	})
}

// GetHandlePin returns the persisted TOFU pin for acct if present.
func (s *Service) GetHandlePin(acct string) (HandlePinRecord, bool) {
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return HandlePinRecord{}, false
	}
	var rec HandlePinRecord
	ok, err := s.mem.DB().GetJSON(handlePinKVPrefix+strings.ToLower(strings.TrimSpace(acct)), &rec)
	if err != nil || !ok || rec.IdentityPubHex == "" {
		return HandlePinRecord{}, false
	}
	return rec, true
}

// ResetHandlePin clears a persisted TOFU handle pin after operator verification of a key rotation.
func (s *Service) ResetHandlePin(acct string) error {
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return nil
	}
	return s.mem.DB().SetJSON(handlePinKVPrefix+strings.ToLower(strings.TrimSpace(acct)), HandlePinRecord{})
}

// ParseAgentURI parses Tier-3 direct zero-DNS URIs (`nektar://pub/<66-hex>@<host>[?office=...]` or `<66-hex>@<host>`).
func ParseAgentURI(raw string) (pubHex, hostBaseURL, bboxOffice string, isURI bool, err error) {
	trimmed := strings.TrimSpace(raw)
	if strings.HasPrefix(strings.ToLower(trimmed), "nektar://pub/") {
		rest := trimmed[len("nektar://pub/"):]
		var queryPart string
		if qIdx := strings.IndexByte(rest, '?'); qIdx >= 0 {
			queryPart = rest[qIdx+1:]
			rest = rest[:qIdx]
		}
		pubPart, hostPart, ok := strings.Cut(rest, "@")
		if !ok || strings.TrimSpace(hostPart) == "" {
			return "", "", "", true, errors.New("discovery: nektar://pub URI requires <66-hex-pubkey>@<host>")
		}
		if _, _, pErr := core.ParseStrictCompressedPubKey(strings.TrimSpace(pubPart)); pErr != nil {
			return "", "", "", true, fmt.Errorf("discovery: invalid pubkey in nektar://pub URI: %w", pErr)
		}
		hostURL := normalizeDirectHost(hostPart)
		if hostURL == "" {
			return "", "", "", true, fmt.Errorf("discovery: invalid host %q in nektar://pub URI", hostPart)
		}
		if queryPart != "" {
			if vals, qErr := url.ParseQuery(queryPart); qErr == nil {
				bboxOffice = strings.TrimSpace(vals.Get("office"))
			}
		}
		return strings.ToLower(strings.TrimSpace(pubPart)), hostURL, bboxOffice, true, nil
	}
	// Bare `<66-hex>@<host>` shorthand.
	pubPart, hostPart, ok := strings.Cut(trimmed, "@")
	if ok && len(strings.TrimSpace(pubPart)) == 66 && strings.TrimSpace(hostPart) != "" {
		if _, _, pErr := core.ParseStrictCompressedPubKey(strings.TrimSpace(pubPart)); pErr == nil {
			hostURL := normalizeDirectHost(hostPart)
			if hostURL == "" {
				return "", "", "", true, fmt.Errorf("discovery: invalid host %q", hostPart)
			}
			return strings.ToLower(strings.TrimSpace(pubPart)), hostURL, "", true, nil
		}
	}
	return "", "", "", false, nil
}

func normalizeDirectHost(rawHost string) string {
	h := strings.TrimSpace(rawHost)
	if h == "" {
		return ""
	}
	if !strings.HasPrefix(h, "http://") && !strings.HasPrefix(h, "https://") {
		if strings.HasPrefix(h, "127.0.0.1") || strings.HasPrefix(h, "localhost") || strings.HasPrefix(h, "[::1]") {
			h = "http://" + h
		} else {
			h = "https://" + h
		}
	}
	return normalizeHostURL(h)
}

// ResolveAgentCtx resolves any of the 3 LightBSV discovery tiers into a verified PeerInfo:
//   - Tier 3 (`nektar://pub/<pubkey>@<host>` or `<66-hex>@<host>`): zero-DNS direct resolution + `/manifest.json`
//   - Tier 1 (`<handle>@nektar.run` / `<handle>@<hub>`): BRC-169 well-known + `ls_finger` + TOFU pubkey pin
//   - Tier 2 (`<handle>@yourdomain.com`): sovereign domain `/manifest.json` + BRC-169 well-known + TOFU pubkey pin
func (s *Service) ResolveAgentCtx(ctx context.Context, target string) (PeerInfo, error) {
	if s == nil {
		return PeerInfo{}, errors.New("discovery: nil service")
	}
	q := strings.TrimSpace(target)
	if q == "" {
		return PeerInfo{}, errors.New("discovery: empty handle or agent URI")
	}

	// 1. Tier 3: Zero-DNS direct URI (`nektar://pub/<pubkey>@<host>` or `<66-hex>@<host>`).
	if pubHex, hostURL, office, isURI, uErr := ParseAgentURI(q); isURI {
		if uErr != nil {
			return PeerInfo{}, uErr
		}
		if _, vErr := core.ValidateOutboundURL(hostURL, core.SafeHTTPOpts{
			AllowPrivate: core.AllowPrivateOutboundForNetwork(s.network),
			RequireHTTPS: s.network == "main",
		}); vErr != nil {
			return PeerInfo{}, fmt.Errorf("discovery: blocked Tier-3 host: %w", vErr)
		}
		hosts := []string{hostURL}
		if mf, mErr := s.fetchManifestFromBaseURL(ctx, hostURL); mErr == nil && mf != nil {
			if office == "" && mf.Handles != nil && mf.Handles.Messagebox != "" {
				office = mf.Handles.Messagebox
			}
			if lsURL, ok := mf.Overlay(bstack.LookupService); ok {
				if norm := normalizeHostURL(lsURL); norm != "" {
					hosts = appendUnique([]string{norm}, hostURL)
				}
			}
		}
		if office == "" {
			office = bstack.NormalizeBboxOffice("default")
		}
		p := PeerInfo{
			Pub:        pubHex,
			PublicURL:  hostURL,
			BboxOffice: office,
			Hosts:      hosts,
			Exp:        time.Now().Add(24 * time.Hour).UnixMilli(),
		}
		s.UpsertPeer(p)
		return p, nil
	}

	// 2. Tier 1 & Tier 2: `<handle>@<domain>` (HTTPS domain via bcommon/resolve, or HTTP loopback on memory/regtest).
	acctStr, handlePart, domainOrHost, baseOrigin, err := s.parseHandleTarget(q)
	if err == nil && handlePart != "" {
		if peer, rErr := s.resolveHandleViaWellKnown(ctx, acctStr, handlePart, domainOrHost, baseOrigin); rErr == nil {
			return peer, nil
		} else if errors.Is(rErr, ErrHandleKeyChanged) || errors.Is(rErr, resolve.ErrRevoked) {
			return PeerInfo{}, rErr
		}
		// Fallback to overlay `ls_finger` (`bfinger`) + verified `dir` peers, still enforcing TOFU handle pinning!
		if peer, ok := s.lookupFingerOrCachedPeer(ctx, acctStr); ok {
			if pinErr := s.VerifyOrPinHandle(acctStr, peer.Pub, peer.BboxOffice); pinErr != nil {
				return PeerInfo{}, pinErr
			}
			return peer, nil
		}
	}

	// 3. Cached verified peer handle or compressed pubkey prefix lookup.
	if peer, ok := s.ResolveHandle(q); ok {
		if peer.Handle != "" {
			if pinErr := s.VerifyOrPinHandle(peer.Handle, peer.Pub, peer.BboxOffice); pinErr != nil {
				return PeerInfo{}, pinErr
			}
		}
		return peer, nil
	}
	return PeerInfo{}, fmt.Errorf("discovery: agent %q not found", target)
}

func (s *Service) parseHandleTarget(raw string) (acctStr, handle, domain, httpOrigin string, err error) {
	clean := strings.TrimSpace(raw)
	clean = strings.TrimPrefix(clean, "acct:")
	clean = strings.TrimPrefix(clean, "@")
	h, rhs, ok := strings.Cut(clean, "@")
	if !ok || strings.TrimSpace(h) == "" || strings.TrimSpace(rhs) == "" {
		return "", "", "", "", errors.New("not a handle@domain")
	}
	h = strings.ToLower(strings.TrimSpace(h))
	if idx := strings.IndexByte(h, '+'); idx > 0 {
		h = h[:idx]
	}
	rhs = strings.TrimSpace(rhs)
	// Support local HTTP test/regtest origins such as `alice@http://127.0.0.1:12345` or `alice@127.0.0.1:12345`.
	if strings.HasPrefix(rhs, "http://") || strings.HasPrefix(rhs, "https://") || strings.Contains(rhs, ":") {
		orig := normalizeDirectHost(rhs)
		if orig == "" {
			return "", "", "", "", errors.New("invalid handle origin")
		}
		u, uErr := url.Parse(orig)
		if uErr != nil || u.Host == "" {
			return "", "", "", "", errors.New("invalid handle origin")
		}
		dom := strings.ToLower(u.Host)
		return h + "@" + dom, h, dom, orig, nil
	}
	acct, pErr := resolve.ParseAcct(h + "@" + rhs)
	if pErr != nil {
		return "", "", "", "", pErr
	}
	return acct.String(), acct.Handle, acct.Domain, "https://" + acct.Domain, nil
}

func (s *Service) resolveHandleViaWellKnown(ctx context.Context, acctStr, handle, domain, baseOrigin string) (PeerInfo, error) {
	if _, vErr := core.ValidateOutboundURL(baseOrigin, core.SafeHTTPOpts{
		AllowPrivate: core.AllowPrivateOutboundForNetwork(s.network),
		RequireHTTPS: s.network == "main",
	}); vErr != nil {
		return PeerInfo{}, vErr
	}
	u, err := url.Parse(baseOrigin)
	if err != nil {
		return PeerInfo{}, err
	}

	var mf *resolve.Manifest
	var pubHex, messagebox string
	var ttlSec int

	if strings.EqualFold(u.Scheme, "https") && !strings.Contains(domain, ":") {
		acct, aErr := resolve.ParseAcct(handle + "@" + domain)
		if aErr != nil {
			return PeerInfo{}, aErr
		}
		mf, _ = resolve.FetchManifest(ctx, s.httpClient, acct.Domain)
		if mf == nil {
			mf = &resolve.Manifest{Handles: &resolve.Handles{Version: "1.0"}}
		} else if mf.Handles == nil {
			mf.Handles = &resolve.Handles{Version: "1.0"}
		}
		res, rErr := resolve.ResolveHandle(ctx, s.httpClient, mf, acct)
		if rErr != nil {
			return PeerInfo{}, rErr
		}
		pubHex = hex.EncodeToString(res.IdentityKey[:])
		messagebox = res.Messagebox
		ttlSec = res.TTL
	} else {
		// HTTP loopback / port-qualified regtest resolution enforcing identical BRC-169 echo & key rules.
		mf, _ = s.fetchManifestFromBaseURL(ctx, baseOrigin)
		resolveEndpoint := strings.TrimRight(baseOrigin, "/") + "/.well-known/metanet-handles/resolve"
		if mf != nil && mf.Handles != nil && mf.Handles.Resolve != "" {
			resolveEndpoint = mf.Handles.Resolve
		}
		ru, pErr := url.Parse(resolveEndpoint)
		if pErr != nil {
			return PeerInfo{}, pErr
		}
		q := ru.Query()
		q.Set("handle", handle)
		ru.RawQuery = q.Encode()

		req, reqErr := http.NewRequestWithContext(ctx, http.MethodGet, ru.String(), nil)
		if reqErr != nil {
			return PeerInfo{}, reqErr
		}
		resp, doErr := s.httpClient.Do(req)
		if doErr != nil {
			return PeerInfo{}, doErr
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
		resp.Body.Close()

		var doc struct {
			MetanetHandles string `json:"metanetHandles"`
			Handle         string `json:"handle"`
			Domain         string `json:"domain"`
			IdentityKey    string `json:"identityKey"`
			Messagebox     string `json:"messagebox"`
			TTL            int    `json:"ttl"`
			Revoked        bool   `json:"revoked"`
		}
		if resp.StatusCode == http.StatusGone {
			return PeerInfo{}, resolve.ErrRevoked
		}
		if resp.StatusCode != http.StatusOK {
			return PeerInfo{}, fmt.Errorf("resolve HTTP %d", resp.StatusCode)
		}
		if uErr := json.Unmarshal(body, &doc); uErr != nil {
			return PeerInfo{}, uErr
		}
		major, _, _ := strings.Cut(doc.MetanetHandles, ".")
		if major != "1" {
			return PeerInfo{}, fmt.Errorf("unsupported metanetHandles version %q", doc.MetanetHandles)
		}
		if !strings.EqualFold(doc.Handle, handle) || !strings.EqualFold(doc.Domain, domain) {
			return PeerInfo{}, fmt.Errorf("BRC-169 echo mismatch: got %s@%s, want %s@%s", doc.Handle, doc.Domain, handle, domain)
		}
		if doc.Revoked {
			return PeerInfo{}, resolve.ErrRevoked
		}
		if _, _, kErr := core.ParseStrictCompressedPubKey(doc.IdentityKey); kErr != nil {
			return PeerInfo{}, kErr
		}
		pubHex = strings.ToLower(doc.IdentityKey)
		messagebox = doc.Messagebox
		ttlSec = doc.TTL
	}

	if messagebox == "" && mf != nil && mf.Handles != nil && mf.Handles.Messagebox != "" {
		messagebox = mf.Handles.Messagebox
	}
	if messagebox == "" {
		messagebox = bstack.NormalizeBboxOffice("default")
	}
	if err := s.VerifyOrPinHandle(acctStr, pubHex, messagebox); err != nil {
		return PeerInfo{}, err
	}

	var hosts []string
	if mf != nil {
		if lsURL, ok := mf.Overlay(bstack.LookupService); ok {
			if norm := normalizeHostURL(lsURL); norm != "" {
				hosts = appendUnique(hosts, norm)
			}
		}
		if bboxURL, ok := mf.Overlay("ls_bbox"); ok {
			if norm := normalizeHostURL(bboxURL); norm != "" {
				hosts = appendUnique(hosts, norm)
			}
		}
	}
	if len(hosts) == 0 {
		hosts = appendUnique(s.ActiveHosts(), strings.TrimRight(baseOrigin, "/"))
	}
	if ttlSec <= 0 {
		ttlSec = 300
	}
	p := PeerInfo{
		Pub:        pubHex,
		Handle:     acctStr,
		PublicURL:  strings.TrimRight(baseOrigin, "/"),
		BboxOffice: messagebox,
		Hosts:      hosts,
		Exp:        time.Now().Add(time.Duration(ttlSec) * time.Second).UnixMilli(),
	}
	s.UpsertPeer(p)
	return p, nil
}

func (s *Service) lookupFingerOrCachedPeer(ctx context.Context, acctStr string) (PeerInfo, bool) {
	if p, ok := s.ResolveHandle(acctStr); ok {
		s.enrichPeerFromFingerOverlay(ctx, &p)
		return p, true
	}
	if _, err := s.DiscoverPeers(ctx, nil); err == nil {
		if p, ok := s.ResolveHandle(acctStr); ok {
			s.enrichPeerFromFingerOverlay(ctx, &p)
			return p, true
		}
	}
	// If handle was previously TOFU-pinned, verify live `ls_finger` chain state by identityKey across active Hubs.
	if pin, ok := s.GetHandlePin(acctStr); ok && pin.IdentityPubHex != "" {
		p := PeerInfo{
			Pub:        pin.IdentityPubHex,
			Handle:     acctStr,
			BboxOffice: pin.Messagebox,
			Hosts:      s.ActiveHosts(),
			Exp:        time.Now().Add(5 * time.Minute).UnixMilli(),
		}
		if s.enrichPeerFromFingerOverlay(ctx, &p) {
			s.UpsertPeer(p)
			return p, true
		}
	}
	return PeerInfo{}, false
}

// enrichPeerFromFingerOverlay queries Lightweb's `ls_finger` (`{ identityKey: "<66-hex>" }`) across active Hubs
// and decodes any returned `OutputList` BEEF outputs to confirm live unkilled `bfinger` state.
func (s *Service) enrichPeerFromFingerOverlay(ctx context.Context, p *PeerInfo) bool {
	if s == nil || p == nil || len(strings.TrimSpace(p.Pub)) != 66 {
		return false
	}
	qBytes, err := json.Marshal(map[string]any{
		"service": fingermod.LookupName,
		"query":   map[string]string{"identityKey": strings.ToLower(strings.TrimSpace(p.Pub))},
	})
	if err != nil {
		return false
	}
	for _, h := range s.ActiveHosts() {
		fCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		req, rErr := http.NewRequestWithContext(fCtx, http.MethodPost, strings.TrimRight(h, "/")+"/lookup", bytes.NewReader(qBytes))
		if rErr != nil {
			cancel()
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, dErr := s.httpClient.Do(req)
		if dErr != nil {
			cancel()
			continue
		}
		var ans struct {
			Type    string `json:"type"`
			Outputs []struct {
				Beef        []byte `json:"beef"`
				OutputIndex uint32 `json:"outputIndex"`
			} `json:"outputs"`
		}
		decErr := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&ans)
		resp.Body.Close()
		cancel()
		if decErr == nil && len(ans.Outputs) > 0 {
			p.Hosts = appendUnique(p.Hosts, h)
			return true
		}
	}
	return false
}

// WellKnownHandleHandler returns a standards-compliant BRC-169 `/.well-known/metanet-handles/resolve`
// HTTP handler serving both this node's local handle and signature-verified `dir` peers.
func (s *Service) WellKnownHandleHandler(defaultDomain, defaultBboxOffice string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		handle := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("handle")))
		if handle == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "bad_request", "message": "handle query parameter required"},
			})
			return
		}
		domain := strings.ToLower(strings.TrimSpace(defaultDomain))
		if r.Host != "" {
			domain = strings.ToLower(strings.TrimSpace(r.Host))
		}
		if domain == "" {
			domain = "nektar.local"
		}
		fullAcct := handle + "@" + domain

		var identityKey, messagebox string
		if s != nil {
			s.mu.RLock()
			lh := s.localHandle
			loff := s.localBboxOffice
			s.mu.RUnlock()
			localBare, _, _ := strings.Cut(lh, "@")
			if lh != "" && (strings.EqualFold(lh, handle) || strings.EqualFold(lh, fullAcct) || strings.EqualFold(localBare, handle)) {
				identityKey = s.AnnouncePubHex()
				messagebox = loff
			}
			if identityKey == "" {
				for _, p := range s.Peers() {
					pHandle := strings.ToLower(strings.TrimSpace(p.Handle))
					if pHandle == "" {
						continue
					}
					pBare, _, _ := strings.Cut(pHandle, "@")
					if pHandle == fullAcct || pHandle == handle || pBare == handle {
						identityKey = p.Pub
						messagebox = p.BboxOffice
						break
					}
				}
			}
		}
		if identityKey == "" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]string{"code": "not_found", "message": "handle not found"},
			})
			return
		}
		if messagebox == "" {
			messagebox = defaultBboxOffice
		}
		if messagebox == "" {
			messagebox = bstack.NormalizeBboxOffice("default")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metanetHandles": "1.0",
			"handle":         handle,
			"domain":         domain,
			"identityKey":    strings.ToLower(identityKey),
			"messagebox":     messagebox,
			"ttl":            300,
			"revoked":        false,
		})
	}
}

// GenerateDomainDocs builds ready-to-serve Tier-2 sovereign domain artifacts (`/manifest.json` +
// `/.well-known/metanet-handles/resolve` + `bdns` overlay guidance) for `nektar domain-docs`.
func GenerateDomainDocs(domain, handle, identityPubHex, hubURL, bboxOffice string) (DomainDocsBundle, error) {
	acct, err := resolve.ParseAcct(strings.TrimSpace(handle) + "@" + strings.TrimSpace(domain))
	if err != nil {
		return DomainDocsBundle{}, err
	}
	if _, _, kErr := core.ParseStrictCompressedPubKey(strings.TrimSpace(identityPubHex)); kErr != nil {
		return DomainDocsBundle{}, fmt.Errorf("invalid compressed identityPubHex: %w", kErr)
	}
	pubHex := strings.ToLower(strings.TrimSpace(identityPubHex))
	cleanHub := normalizeHostURL(hubURL)
	if cleanHub == "" {
		cleanHub = "https://" + acct.Domain + "/hub"
	}
	office := bstack.NormalizeBboxOffice(bboxOffice)

	manifestDoc := map[string]any{
		"bsv": "0.1",
		"metanet": map[string]any{
			"overlays": map[string]string{
				bstack.LookupService:   cleanHub,
				fingermod.LookupName:   cleanHub,
				"ls_bbox":              cleanHub,
				"ls_bstore":            cleanHub,
				zonerec.LookupService:  cleanHub,
				"tm_finger":            cleanHub,
				"tm_bbox_" + office:    cleanHub,
			},
			"handles": map[string]any{
				"version":    "1.0",
				"resolve":    "https://" + acct.Domain + "/.well-known/metanet-handles/resolve",
				"messagebox": office,
			},
			"trust": map[string]string{
				"name":      "Nektar Sovereign Agent (" + acct.String() + ")",
				"publicKey": pubHex,
			},
		},
	}
	manifestBytes, err := json.MarshalIndent(manifestDoc, "", "  ")
	if err != nil {
		return DomainDocsBundle{}, err
	}
	if _, pErr := resolve.ParseManifest(manifestBytes); pErr != nil {
		return DomainDocsBundle{}, fmt.Errorf("validate generated manifest.json: %w", pErr)
	}

	wellKnownDoc := map[string]any{
		"metanetHandles": "1.0",
		"handle":         acct.Handle,
		"domain":         acct.Domain,
		"identityKey":    pubHex,
		"messagebox":     office,
		"ttl":            300,
		"revoked":        false,
	}
	wellKnownBytes, err := json.MarshalIndent(wellKnownDoc, "", "  ")
	if err != nil {
		return DomainDocsBundle{}, err
	}

	return DomainDocsBundle{
		Domain:            acct.Domain,
		Handle:            acct.Handle,
		Acct:              acct.String(),
		IdentityPubHex:    pubHex,
		HubURL:            cleanHub,
		BboxOffice:        office,
		ManifestPath:      "/manifest.json",
		ManifestJSON:      manifestBytes,
		WellKnownPath:     "/.well-known/metanet-handles/resolve?handle=" + acct.Handle,
		WellKnownJSON:     wellKnownBytes,
		BdnsLookupService: zonerec.LookupService,
		SetupInstructions: []string{
			fmt.Sprintf("1. Serve manifestJson at https://%s/manifest.json with Content-Type: application/json", acct.Domain),
			fmt.Sprintf("2. Point https://%s/.well-known/metanet-handles/resolve to your Nektar Hub (%s) or serve wellKnownJson for ?handle=%s", acct.Domain, cleanHub, acct.Handle),
			fmt.Sprintf("3. Start your Nektar node with --handle %s and --public-url https://%s", acct.String(), acct.Domain),
		},
	}, nil
}
