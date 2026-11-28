package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
)

// Objects returns the Hub's content-addressed BRC-167 CHIRP store backing `/chirp/v1/objects/{hash}`.
func (n *Node) Objects() bstack.ObjectStore {
	if n == nil {
		return nil
	}
	return n.objects
}

func (n *Node) handleChirpPut(w http.ResponseWriter, r *http.Request) {
	if n.objects == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub chirp object store unavailable"})
		return
	}
	hashHex := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
	rawHash, err := hex.DecodeString(hashHex)
	if err != nil || len(rawHash) != 32 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "valid 64-hex sha256 object hash required"})
		return
	}
	var want [32]byte
	copy(want[:], rawHash)

	rateKey := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Nektar-Pub")))
	if rateKey == "" {
		rateKey = "ip:" + strings.TrimSpace(r.RemoteAddr)
	}
	if !n.allowPubRate(rateKey) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, bstack.MaxChirpObjectBytes+1))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if len(body) > bstack.MaxChirpObjectBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "chirp object exceeds maximum size"})
		return
	}
	if sha256.Sum256(body) != want {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "chirp object sha256 mismatch"})
		return
	}
	if err := n.objects.PutObject(want, body); err != nil {
		writeJSON(w, http.StatusInsufficientStorage, map[string]string{"error": err.Error()})
		return
	}

	// Leaf -> Spine content-addressed staging: mirror uploaded CHIRP objects UP to configured Spine(s)
	// before returning 201 Created so Buyers behind sibling Leaf Hubs pull them with zero race window!
	if r.Header.Get("X-Nektar-Chirp-Relayed") == "" && !n.propagationOff && n.fees.Role != RoleSpine {
		n.mu.RLock()
		spines := append([]string(nil), n.upstreamSpines...)
		n.mu.RUnlock()
		for _, sp := range spines {
			uCtx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			uReq, rErr := http.NewRequestWithContext(uCtx, http.MethodPut, strings.TrimRight(sp, "/")+"/chirp/v1/objects/"+hashHex, bytes.NewReader(body))
			if rErr == nil {
				uReq.Header.Set("Content-Type", "application/octet-stream")
				uReq.Header.Set("X-Nektar-Chirp-Relayed", "1")
				if uResp, dErr := n.httpClient.Do(uReq); dErr == nil && uResp != nil {
					uResp.Body.Close()
				}
			}
			cancel()
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"ok":     true,
		"sha256": hashHex,
		"size":   len(body),
	})
}

func (n *Node) handleChirpGet(w http.ResponseWriter, r *http.Request) {
	if n.objects == nil {
		http.NotFound(w, r)
		return
	}
	hashHex := strings.ToLower(strings.TrimSpace(r.PathValue("hash")))
	rawHash, err := hex.DecodeString(hashHex)
	if err != nil || len(rawHash) != 32 {
		http.Error(w, "invalid 64-hex sha256 object hash", http.StatusBadRequest)
		return
	}
	var want [32]byte
	copy(want[:], rawHash)

	data, err := n.objects.GetObject(want)
	if err != nil {
		// Leaf <- Spine pull-through cache on local miss (content-addressed SHA-256 verified before caching!).
		n.mu.RLock()
		spines := append([]string(nil), n.upstreamSpines...)
		n.mu.RUnlock()
		for _, sp := range spines {
			fCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
			uReq, rErr := http.NewRequestWithContext(fCtx, http.MethodGet, strings.TrimRight(sp, "/")+"/chirp/v1/objects/"+hashHex, nil)
			if rErr != nil {
				cancel()
				continue
			}
			uResp, dErr := n.httpClient.Do(uReq)
			if dErr != nil {
				cancel()
				continue
			}
			if uResp.StatusCode != http.StatusOK {
				uResp.Body.Close()
				cancel()
				continue
			}
			fetched, rBodyErr := io.ReadAll(io.LimitReader(uResp.Body, bstack.MaxChirpObjectBytes+1))
			uResp.Body.Close()
			cancel()
			if rBodyErr == nil && len(fetched) <= bstack.MaxChirpObjectBytes && sha256.Sum256(fetched) == want {
				_ = n.objects.PutObject(want, fetched)
				data = fetched
				err = nil
				break
			}
		}
	}
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	w.Header().Set("ETag", `"`+hashHex+`"`)
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = w.Write(data)
	}
}
