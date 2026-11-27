package chain

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/transaction/chaintracker"
	"github.com/lightwebinc/bcommon/headers"
)

// SovereignHeaderJSON is the wire JSON returned by /v1/tip, /v1/root/{height}, and /v1/header/{id}.
// Matches lightwebinc/overlay-bridge/headers & bcommon/headers while also carrying full PoW header
// fields (version, prevHash, merkleRoot, time, bits, nonce, rawHex) so every downstream Hub checks
// Proof-of-Work locally against its own difficulty floor (1BSV Paper 008 §4).
type SovereignHeaderJSON struct {
	Height     uint32 `json:"height"`
	Hash       string `json:"hash,omitempty"`
	PrevHash   string `json:"prevHash,omitempty"`
	MerkleRoot string `json:"merkleRoot,omitempty"`
	Version    uint32 `json:"version,omitempty"`
	Time       uint32 `json:"time,omitempty"`
	Bits       uint32 `json:"bits,omitempty"`
	Nonce      uint32 `json:"nonce,omitempty"`
	RawHex     string `json:"rawHex,omitempty"`
	Source     string `json:"source,omitempty"`
}

// HeaderStoreHandler returns an http.Handler serving /v1/tip, /v1/root/{height}, and
// /v1/header/{heightOrHash} from this Port's verified WitnessedTracker (or memory testchain).
// A Spine Hub (or overlay-bridge) mounts this handler so sibling Leaf Hubs anchor off ONE
// PoW-verified header store instead of hammering public WoC endpoints (1BSV Paper 008 §4).
func (p *Port) HeaderStoreHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/tip", func(w http.ResponseWriter, r *http.Request) {
		h, err := p.Height(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		out := SovereignHeaderJSON{Height: h, Source: "sovereign"}
		if p.witnessed != nil {
			if hdr, hErr := p.witnessed.HeaderAt(r.Context(), h); hErr == nil && hdr != nil {
				out.Hash = hdr.Hash.String()
				out.PrevHash = hdr.Prev.String()
				out.MerkleRoot = hdr.MerkleRoot.String()
				out.Version = hdr.Version
				out.Time = hdr.Time
				out.Bits = hdr.Bits
				out.Nonce = hdr.Nonce
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/root/{height}", func(w http.ResponseWriter, r *http.Request) {
		h64, err := strconv.ParseUint(strings.TrimSpace(r.PathValue("height")), 10, 32)
		if err != nil {
			http.Error(w, "invalid height", http.StatusBadRequest)
			return
		}
		height := uint32(h64)
		out, code, hErr := p.resolveSovereignHeader(r.Context(), height)
		if hErr != nil {
			http.Error(w, hErr.Error(), code)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	mux.HandleFunc("GET /v1/header/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.PathValue("id"))
		if id == "" {
			http.Error(w, "height or hash required", http.StatusBadRequest)
			return
		}
		if h64, err := strconv.ParseUint(id, 10, 32); err == nil {
			out, code, hErr := p.resolveSovereignHeader(r.Context(), uint32(h64))
			if hErr != nil {
				http.Error(w, hErr.Error(), code)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		// Lookup by block hash inside WitnessedTracker cache.
		if p.witnessed != nil {
			p.witnessed.mu.RLock()
			for _, ce := range p.witnessed.cache {
				if strings.EqualFold(ce.hdr.Hash.String(), id) {
					out := sovereignJSONFromHeader(&ce.hdr, ce.raw[:])
					p.witnessed.mu.RUnlock()
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(out)
					return
				}
			}
			p.witnessed.mu.RUnlock()
		}
		http.Error(w, "header not held", http.StatusNotFound)
	})
	return mux
}

func (p *Port) resolveSovereignHeader(ctx context.Context, height uint32) (SovereignHeaderJSON, int, error) {
	if p.witnessed != nil {
		hdr, err := p.witnessed.HeaderAt(ctx, height)
		if err != nil {
			if errors.Is(err, headers.ErrUnknownHeight) {
				return SovereignHeaderJSON{}, http.StatusNotFound, err
			}
			return SovereignHeaderJSON{}, http.StatusBadGateway, err
		}
		raw, _ := p.witnessed.HeaderBytes(ctx, height)
		return sovereignJSONFromHeader(hdr, raw), http.StatusOK, nil
	}
	if p.testChain != nil {
		p.tcMu.Lock()
		tip := p.testChain.Height()
		p.tcMu.Unlock()
		if height > tip {
			return SovereignHeaderJSON{}, http.StatusNotFound, errors.New("height above tip")
		}
		return SovereignHeaderJSON{Height: height, Source: "memory"}, http.StatusOK, nil
	}
	return SovereignHeaderJSON{}, http.StatusServiceUnavailable, errors.New("no header store attached")
}

func sovereignJSONFromHeader(hdr *headers.Header, raw []byte) SovereignHeaderJSON {
	out := SovereignHeaderJSON{
		Height:     hdr.Height,
		Hash:       hdr.Hash.String(),
		PrevHash:   hdr.Prev.String(),
		MerkleRoot: hdr.MerkleRoot.String(),
		Version:    hdr.Version,
		Time:       hdr.Time,
		Bits:       hdr.Bits,
		Nonce:      hdr.Nonce,
		Source:     "anchor",
	}
	if len(raw) == 80 {
		out.RawHex = hex.EncodeToString(raw)
	}
	return out
}

// HeaderAnchorClient is a PoW-verifying HeaderWitnessSource + ChainTracker client that anchors
// against a Spine Hub's Sovereign Header Store (or lightwebinc/overlay-bridge's /v1/tip &
// /v1/root/{height} header server), eliminating per-Hub third-party API rate limits.
// It performs local 80-byte PoW verification on every fetched header and delegates reorg-aware
// caching to WitnessedTracker (never caching orphaned headers indefinitely).
type HeaderAnchorClient struct {
	BaseURL       string
	Client        *http.Client
	MinDifficulty float64
}

var (
	_ HeaderWitnessSource       = (*HeaderAnchorClient)(nil)
	_ chaintracker.ChainTracker = (*HeaderAnchorClient)(nil)
)

// NewHeaderAnchorClient creates a HeaderAnchorClient pointed at baseURL (e.g. https://nektar.run/hub
// or http://127.0.0.1:9178 for overlay-bridge).
func NewHeaderAnchorClient(baseURL string, cli *http.Client) *HeaderAnchorClient {
	if cli == nil {
		cli = &http.Client{Timeout: 10 * time.Second}
	}
	return &HeaderAnchorClient{
		BaseURL: strings.TrimRight(strings.TrimSpace(baseURL), "/"),
		Client:  cli,
	}
}

func (c *HeaderAnchorClient) CurrentHeight(ctx context.Context) (uint32, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/v1/tip", nil)
	if err != nil {
		return 0, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("header anchor %s/v1/tip: HTTP %d", c.BaseURL, resp.StatusCode)
	}
	var out SovereignHeaderJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return 0, err
	}
	return out.Height, nil
}

func (c *HeaderAnchorClient) HeaderAt(ctx context.Context, height uint32) (*headers.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/v1/root/%d", c.BaseURL, height), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, headers.ErrUnknownHeight
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("header anchor %s/v1/root/%d: HTTP %d", c.BaseURL, height, resp.StatusCode)
	}
	var out SovereignHeaderJSON
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return nil, err
	}
	if out.MerkleRoot == "" {
		return nil, fmt.Errorf("header anchor %s/v1/root/%d: missing merkleRoot", c.BaseURL, height)
	}
	rootH, err := chainhash.NewHashFromHex(out.MerkleRoot)
	if err != nil || rootH == nil {
		return nil, fmt.Errorf("header anchor: invalid merkleRoot %q", out.MerkleRoot)
	}
	hdr := headers.Header{
		Height:     height,
		MerkleRoot: *rootH,
		Version:    out.Version,
		Time:       out.Time,
		Bits:       out.Bits,
		Nonce:      out.Nonce,
	}
	if out.Hash != "" {
		if h, hErr := chainhash.NewHashFromHex(out.Hash); hErr == nil && h != nil {
			hdr.Hash = *h
		}
	}
	if out.PrevHash != "" {
		if ph, pErr := chainhash.NewHashFromHex(out.PrevHash); pErr == nil && ph != nil {
			hdr.Prev = *ph
		}
	}
	// Always compute and verify local 80-byte PoW before accepting any remote header JSON.
	if out.Bits != 0 || out.RawHex != "" || c.MinDifficulty > 0 {
		raw80, _, pErr := verifyHeaderLocalPoW(&hdr, height, c.MinDifficulty)
		if pErr != nil {
			return nil, fmt.Errorf("header anchor %s/v1/root/%d failed local PoW check: %w", c.BaseURL, height, pErr)
		}
		if out.RawHex != "" && !strings.EqualFold(strings.TrimSpace(out.RawHex), hex.EncodeToString(raw80[:])) {
			return nil, fmt.Errorf("header anchor %s/v1/root/%d rawHex mismatch", c.BaseURL, height)
		}
	} else {
		return nil, fmt.Errorf("header anchor %s/v1/root/%d missing PoW header fields (bits/rawHex)", c.BaseURL, height)
	}
	cp := hdr
	return &cp, nil
}

func (c *HeaderAnchorClient) IsValidRootForHeight(ctx context.Context, root *chainhash.Hash, height uint32) (bool, error) {
	if root == nil {
		return false, nil
	}
	hdr, err := c.HeaderAt(ctx, height)
	if err != nil {
		if errors.Is(err, headers.ErrUnknownHeight) {
			return false, nil
		}
		return false, err
	}
	return hdr.MerkleRoot == *root, nil
}
