package hub

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/module"
)

// BountySubmission is a signed agent submission selling verified knowledge to the Hub bounty pool.
type BountySubmission struct {
	BountyID      string          `json:"bountyId"`
	SellerPub     string          `json:"sellerPub"`
	ProvenancePub string          `json:"provenancePub,omitempty"` // BRC-43 provenance pubkey matching Provenance signature & OwnerSig
	SellerAddress string          `json:"sellerAddress"`
	Shard         string          `json:"shard"`
	Seq           int64           `json:"seq"`
	HDP            string          `json:"hdp"`
	MPERoot        string          `json:"mpeRoot,omitempty"`
	ContentSHA256  string          `json:"contentSha256,omitempty"`
	CommitHash     string          `json:"commitHash,omitempty"`
	TransitionHash string          `json:"transitionHash,omitempty"`
	CiphertextHex  string          `json:"ciphertextHex,omitempty"`
	Text           string          `json:"text"`
	ProvenanceHex string          `json:"provenanceHex,omitempty"`
	ProofBundle   json.RawMessage `json:"proofBundle,omitempty"`
	TS            int64           `json:"ts,omitempty"`
	Sig           string          `json:"sig,omitempty"`
	OwnerSig      string          `json:"ownerSig,omitempty"`
}

// Signable returns the canonical signable payload for BountySubmission (using canonical lowercase HDP).
func (s BountySubmission) Signable(canonicalHDP string) map[string]any {
	return map[string]any{
		"action":        "bounty_submit",
		"bountyId":      s.BountyID,
		"hdp":           canonicalHDP,
		"sellerPub":     s.SellerPub,
		"provenancePub": s.ProvenancePub,
		"sellerAddress": s.SellerAddress,
		"ts":            s.TS,
	}
}

// BountyPayoutResult reports the Hub's verified payment to a contributing agent.
// BEEFHex carries the strict AtomicBEEF payout transaction so recipients can SPV-verify and internalize immediately.
type BountyPayoutResult struct {
	Accepted   bool   `json:"accepted"`
	BountyID   string `json:"bountyId"`
	RewardSats uint64 `json:"rewardSats"`
	TxID       string `json:"txid,omitempty"`
	BEEFHex    string `json:"beefHex,omitempty"`
	Status     string `json:"status,omitempty"` // broadcast | paid | pending
	Shard      string `json:"shard,omitempty"`
	Seq        int64  `json:"seq,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// BountyHandler validates and pays an incoming BountySubmission using the Hub's memory + SpendGuard wallet.
type BountyHandler func(ctx context.Context, b Bounty, sub BountySubmission) (*BountyPayoutResult, error)

type simpleHost struct {
	mu       sync.Mutex
	counters map[string]float64
	gauges   map[string]func() float64
}

func newSimpleHost() *simpleHost {
	return &simpleHost{
		counters: make(map[string]float64),
		gauges:   make(map[string]func() float64),
	}
}

func (h *simpleHost) Log(_ string, _ map[string]any) {}
func (h *simpleHost) Metrics() module.Metrics        { return h }
func (h *simpleHost) Inc(name string, _ map[string]string, by float64) {
	h.mu.Lock()
	h.counters[name] += by
	h.mu.Unlock()
}
func (h *simpleHost) Preset(name string, _ map[string]string) {
	h.mu.Lock()
	if _, ok := h.counters[name]; !ok {
		h.counters[name] = 0
	}
	h.mu.Unlock()
}
func (h *simpleHost) Gauge(name string, fn func() float64) {
	h.mu.Lock()
	h.gauges[name] = fn
	h.mu.Unlock()
}

type rateBucket struct {
	windowStart time.Time
	count       int
}

const (
	maxHubSSESubs   = 256
	maxRateBuckets  = 4096
	maxBboxNonces   = 16384
	bboxSkewSeconds = 300
)

func (n *Node) maintenanceLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			nowMs := time.Now().UnixMilli()
			_, _ = n.store.PruneExpired(nowMs)
			_, _ = n.store.PruneBbox(nowMs-int64(7*24*time.Hour/time.Millisecond), 10_000)
			if n.marketLS != nil {
				_ = n.marketLS.PruneExpired(nowMs)
			}
			// Evict stale per-pubkey rate-limit buckets.
			n.mu.Lock()
			now := time.Now()
			for pub, b := range n.rates {
				if now.Sub(b.windowStart) >= 2*time.Minute {
					delete(n.rates, pub)
				}
			}
			n.mu.Unlock()
		}
	}
}

func (n *Node) Store() *Store { return n.store }

func (n *Node) SetBountyHandler(h BountyHandler) {
	n.mu.Lock()
	n.bountyHandler = h
	n.mu.Unlock()
}

// SetUpstreamSubmitter configures the Leaf -> Spine upstream BEEF push function.
func (n *Node) SetUpstreamSubmitter(fn func(ctx context.Context, spineURL string, beef []byte, topics []string) error) {
	n.mu.Lock()
	n.upstreamSubmitter = fn
	n.mu.Unlock()
}

// SetUpstreamSpines registers Spine Hub URLs that a Leaf Hub forwards local ads UP to.
func (n *Node) SetUpstreamSpines(spines []string) {
	n.mu.Lock()
	seen := make(map[string]bool)
	var clean []string
	for _, s := range spines {
		if s = strings.TrimRight(strings.TrimSpace(s), "/"); s != "" && !seen[s] {
			seen[s] = true
			clean = append(clean, s)
		}
	}
	n.upstreamSpines = clean
	n.mu.Unlock()
}

// SetEgressAuth wires outgoing Spine-and-Leaf egress authentication and credit auto-topup.
func (n *Node) SetEgressAuth(
	signer func(action, aud string, cursor int64, domainsCSV string) (pub, ts, nonce, sig string, err error),
	topup func(ctx context.Context, spineURL string, minTopupSats uint64) error,
) {
	n.mu.Lock()
	n.egressAuthSigner = signer
	n.onEgressTopup = topup
	n.mu.Unlock()
}

// AddSpinePeerPub whitelists a bilateral Spine peer's compressed pubkey for settlement-free peering.
func (n *Node) AddSpinePeerPub(pubHex string) {
	if pubHex = strings.ToLower(strings.TrimSpace(pubHex)); pubHex != "" {
		n.mu.Lock()
		n.spinePeers[pubHex] = true
		n.mu.Unlock()
	}
}

// OnBountyPayoutMined promotes a broadcast bounty claim to 'paid' once its payout tx mines at settleDepth.
func (n *Node) OnBountyPayoutMined(txid string) error {
	return n.store.FinalizeBountyClaimByTxID(txid)
}

func (n *Node) mountModule(m module.Module) {
	for k, tm := range m.Topics {
		n.topics[k] = tm
	}
	for k, ls := range m.Lookups {
		n.lookups[k] = ls
	}
}

func (n *Node) Topics() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.topics))
	for k := range n.topics {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *Node) Lookups() []string {
	n.mu.RLock()
	defer n.mu.RUnlock()
	out := make([]string, 0, len(n.lookups))
	for k := range n.lookups {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (n *Node) allowPubRate(pub string) bool {
	if pub == "" {
		return true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	now := time.Now()
	b, ok := n.rates[pub]
	if ok && now.Sub(b.windowStart) < time.Minute {
		if b.count >= n.maxPerMin {
			return false
		}
		b.count++
		return true
	}
	if !ok && len(n.rates) >= maxRateBuckets {
		for k, v := range n.rates {
			if now.Sub(v.windowStart) >= time.Minute {
				delete(n.rates, k)
			}
		}
		if len(n.rates) >= maxRateBuckets {
			var oldestKey string
			var oldestStart time.Time
			for k, v := range n.rates {
				if oldestKey == "" || v.windowStart.Before(oldestStart) {
					oldestKey = k
					oldestStart = v.windowStart
				}
			}
			if oldestKey != "" {
				delete(n.rates, oldestKey)
			}
		}
	}
	n.rates[pub] = &rateBucket{windowStart: now, count: 1}
	return true
}

func (n *Node) hasBboxNonce(scope, nonce string) bool {
	if strings.TrimSpace(nonce) == "" {
		return true
	}
	key := scope + ":" + nonce
	n.mu.RLock()
	_, exists := n.bboxNonces[key]
	n.mu.RUnlock()
	return exists
}

func (n *Node) claimBboxNonce(scope, nonce string, now int64) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	key := scope + ":" + nonce
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.bboxNonces) >= maxBboxNonces {
		cutoff := now - bboxSkewSeconds*2
		for k, ts := range n.bboxNonces {
			if ts < cutoff {
				delete(n.bboxNonces, k)
			}
		}
		if len(n.bboxNonces) >= maxBboxNonces {
			return false
		}
	}
	if _, exists := n.bboxNonces[key]; exists {
		return false
	}
	n.bboxNonces[key] = now
	return true
}
