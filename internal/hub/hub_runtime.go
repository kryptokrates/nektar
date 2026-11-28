package hub

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/cbor"
	"github.com/lightwebinc/bcommon/module"

	"github.com/kryptokrates/nektar/internal/core"
)

// BountySubmission is a signed agent submission selling verified knowledge to the Hub bounty pool.
type BountySubmission struct {
	BountyID      string          `json:"bountyId"`
	SellerPub     string          `json:"sellerPub"`
	ProvenancePub string          `json:"provenancePub,omitempty"` // BRC-43 provenance pubkey matching Provenance signature & OwnerSig
	SellerAddress string          `json:"sellerAddress"`
	Shard         string          `json:"shard"`
	Seq           int64           `json:"seq"`
	HDP           string          `json:"hdp"`
	MPERoot       string          `json:"mpeRoot,omitempty"`
	ContentSHA256 string          `json:"contentSha256,omitempty"`
	CommitHash    string          `json:"commitHash,omitempty"`
	TransitionHash string         `json:"transitionHash,omitempty"`
	CiphertextHex string          `json:"ciphertextHex,omitempty"`
	Text          string          `json:"text"`
	ProvenanceHex string          `json:"provenanceHex,omitempty"`
	ProofBundle   json.RawMessage `json:"proofBundle,omitempty"`
	TS            int64           `json:"ts,omitempty"`
	Sig           string          `json:"sig,omitempty"`
	OwnerSig      string          `json:"ownerSig,omitempty"`
}

// BountySignable is the canonical CBOR-signed payload for BountySubmission.
type BountySignable struct {
	BountyID      string `json:"bountyId"`
	HDP           string `json:"hdp"`
	SellerPub     string `json:"sellerPub"`
	ProvenancePub string `json:"provenancePub"`
	SellerAddress string `json:"sellerAddress"`
	TS            int64  `json:"ts"`
}

func (b BountySignable) DomainTag() []byte { return core.TagBountySubmitV1 }

func (b BountySignable) EncodeCanonicalCBOR() ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: b.BountyID},
		{Key: uint64(2), Val: b.HDP},
		{Key: uint64(3), Val: b.SellerPub},
		{Key: uint64(4), Val: b.ProvenancePub},
		{Key: uint64(5), Val: b.SellerAddress},
		{Key: uint64(6), Val: b.TS},
	})
}

// OwnerSigDigest returns the domain-separated DSHA256 digest signed by ProvenancePub for OwnerSig.
func (b BountySignable) OwnerSigDigest() ([]byte, error) {
	raw, err := b.EncodeCanonicalCBOR()
	if err != nil {
		return nil, err
	}
	return core.DSHA256(core.Concat(core.TagBountySubmitV1, raw)), nil
}

// Signable returns the canonical signable payload for BountySubmission (using canonical lowercase HDP).
func (s BountySubmission) Signable(canonicalHDP string) BountySignable {
	return BountySignable{
		BountyID:      s.BountyID,
		HDP:           canonicalHDP,
		SellerPub:     s.SellerPub,
		ProvenancePub: s.ProvenancePub,
		SellerAddress: s.SellerAddress,
		TS:            s.TS,
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
	maxHubSSESubs       = 4096
	maxRateBucketsShard = 64
	maxRateBuckets      = 64 * maxRateBucketsShard
	maxNoncesPerShard   = 32768 // 64 shards * 32,768 = 2,097,152 active nonces (~3,495 req/s over 600s window)
	bboxSkewSeconds     = 300
)

type hubNonceEntry struct {
	key string
	ts  int64
}

type hubRateNonceShard struct {
	mu         sync.Mutex
	rates      map[string]*rateBucket
	nonces     map[string]int64
	nonceOrder []hubNonceEntry
	nonceHead  int
}

func hubShardIdx(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h & 63
}

func (n *Node) maintenanceLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-n.stopCh:
			return
		case <-ticker.C:
			nowMs := time.Now().UnixMilli()
			_, _ = n.store.PruneExpired(nowMs)
			_, _ = n.store.PruneBbox(nowMs-int64(7*24*time.Hour/time.Millisecond), 100_000)
			if n.marketLS != nil {
				_ = n.marketLS.PruneExpired(nowMs)
			}
			// STATE-9: sweep unmined fast-accepted fee payments that exceeded 2x AcceptWindow without mining.
			winSec := int64(2 * n.fees.AcceptWindow.Seconds())
			if winSec <= 0 {
				winSec = 7200
			}
			_, _ = n.SweepUnsettleableFees(winSec)
			// Evict stale per-pubkey rate-limit buckets per shard without touching Node.mu.
			now := time.Now()
			for i := range n.rateShards {
				sh := &n.rateShards[i]
				sh.mu.Lock()
				for pub, b := range sh.rates {
					if now.Sub(b.windowStart) >= 2*time.Minute {
						delete(sh.rates, pub)
					}
				}
				sh.mu.Unlock()
			}
		}
	}
}

// AcquireDownstreamTopics increments reference counts for topics requested by a downstream Leaf Hub or Agent
// and returns an idempotent release closure. When the union of active topics changes, it wakes upstream
// Spine plane relays so Leaf Hubs subscribe strictly to wanted slices (1BSV / BRC-223 union rule).
func (n *Node) AcquireDownstreamTopics(topics []string) func() {
	if n == nil || len(topics) == 0 {
		return func() {}
	}
	seen := make(map[string]bool, len(topics))
	clean := make([]string, 0, len(topics))
	for _, t := range topics {
		if t = strings.TrimSpace(t); t != "" && !seen[t] {
			seen[t] = true
			clean = append(clean, t)
		}
	}
	if len(clean) == 0 {
		return func() {}
	}
	changed := false
	n.downInterestMu.Lock()
	if n.downTopicRefs == nil {
		n.downTopicRefs = make(map[string]int, 16)
	}
	for _, t := range clean {
		if n.downTopicRefs[t] == 0 {
			changed = true
		}
		n.downTopicRefs[t]++
	}
	n.downInterestMu.Unlock()
	if changed {
		n.notifyDownstreamInterestChanged()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			dropped := false
			n.downInterestMu.Lock()
			for _, t := range clean {
				if cur := n.downTopicRefs[t]; cur <= 1 {
					if cur == 1 {
						dropped = true
					}
					delete(n.downTopicRefs, t)
				} else {
					n.downTopicRefs[t] = cur - 1
				}
			}
			n.downInterestMu.Unlock()
			if dropped {
				n.notifyDownstreamInterestChanged()
			}
		})
	}
}

// ActiveDownstreamTopics returns the sorted union of topics currently requested by active downstream subscribers.
func (n *Node) ActiveDownstreamTopics() []string {
	if n == nil {
		return nil
	}
	n.downInterestMu.Lock()
	out := make([]string, 0, len(n.downTopicRefs))
	for t, c := range n.downTopicRefs {
		if c > 0 {
			out = append(out, t)
		}
	}
	n.downInterestMu.Unlock()
	sort.Strings(out)
	return out
}

func (n *Node) notifyDownstreamInterestChanged() {
	if n == nil || n.downInterestWake == nil {
		return
	}
	select {
	case n.downInterestWake <- struct{}{}:
	default:
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

// OnBountyPayoutMined promotes a broadcast bounty or work claim to 'paid' once its payout tx mines at settleDepth (STATE-10).
func (n *Node) OnBountyPayoutMined(txid string) error {
	err1 := n.store.FinalizeBountyClaimByTxID(txid)
	err2 := n.store.FinalizeWorkClaimByTxID(txid)
	if err1 != nil {
		return err1
	}
	return err2
}

// SweepUnsettleableFees marks expired unmined fast fee payments as 'lost' and flags their payers (STATE-9).
func (n *Node) SweepUnsettleableFees(maxAgeSec int64) (int, error) {
	stale, err := n.store.SweepExpiredFastFees(maxAgeSec)
	if err != nil {
		return 0, err
	}
	for _, e := range stale {
		if e.PayerPub != "" {
			_ = n.store.FlagPayer(e.PayerPub, e.TxID, "fast fee payment failed to mine within acceptance window")
		}
	}
	return len(stale), nil
}

func (n *Node) mountModule(m module.Module) {
	for k, tm := range m.Topics {
		n.topics[k] = tm
		if n.oeEngine != nil {
			n.oeEngine.RegisterTopicManager(k, n.wrapEngineTopicManager(tm))
		}
	}
	for k, ls := range m.Lookups {
		n.lookups[k] = ls
		if n.oeEngine != nil {
			n.oeEngine.RegisterLookupService(k, n.wrapEngineLookupService(ls))
		}
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
	limit := n.maxPerMin
	if limit <= 0 {
		return true
	}
	sh := &n.rateShards[hubShardIdx(pub)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.rates == nil {
		sh.rates = make(map[string]*rateBucket, 32)
	}
	now := time.Now()
	b, ok := sh.rates[pub]
	if ok && now.Sub(b.windowStart) < time.Minute {
		if b.count >= limit {
			return false
		}
		b.count++
		return true
	}
	if !ok && len(sh.rates) >= maxRateBucketsShard {
		for k, v := range sh.rates {
			if now.Sub(v.windowStart) >= time.Minute {
				delete(sh.rates, k)
			}
		}
		if len(sh.rates) >= maxRateBucketsShard {
			var oldestKey string
			var oldestStart time.Time
			for k, v := range sh.rates {
				if oldestKey == "" || v.windowStart.Before(oldestStart) {
					oldestKey = k
					oldestStart = v.windowStart
				}
			}
			if oldestKey != "" {
				delete(sh.rates, oldestKey)
			}
		}
	}
	sh.rates[pub] = &rateBucket{windowStart: now, count: 1}
	return true
}

func (n *Node) hasBboxNonce(scope, nonce string) bool {
	if strings.TrimSpace(nonce) == "" {
		return true
	}
	key := scope + ":" + nonce
	sh := &n.rateShards[hubShardIdx(key)]
	sh.mu.Lock()
	_, exists := sh.nonces[key]
	sh.mu.Unlock()
	return exists
}

func (n *Node) claimBboxNonce(scope, nonce string, now int64) bool {
	if strings.TrimSpace(nonce) == "" {
		return false
	}
	key := scope + ":" + nonce
	sh := &n.rateShards[hubShardIdx(key)]
	sh.mu.Lock()
	defer sh.mu.Unlock()
	if sh.nonces == nil {
		sh.nonces = make(map[string]int64, 128)
	}
	cutoff := now - bboxSkewSeconds*2
	for sh.nonceHead < len(sh.nonceOrder) && sh.nonceOrder[sh.nonceHead].ts < cutoff {
		old := sh.nonceOrder[sh.nonceHead]
		sh.nonceOrder[sh.nonceHead] = hubNonceEntry{}
		sh.nonceHead++
		if curTS, ok := sh.nonces[old.key]; ok && curTS < cutoff {
			delete(sh.nonces, old.key)
		}
	}
	for len(sh.nonces) >= maxNoncesPerShard && sh.nonceHead < len(sh.nonceOrder) {
		old := sh.nonceOrder[sh.nonceHead]
		sh.nonceOrder[sh.nonceHead] = hubNonceEntry{}
		sh.nonceHead++
		delete(sh.nonces, old.key)
	}
	if sh.nonceHead > 0 && sh.nonceHead*2 >= len(sh.nonceOrder) {
		rem := copy(sh.nonceOrder, sh.nonceOrder[sh.nonceHead:])
		sh.nonceOrder = sh.nonceOrder[:rem]
		sh.nonceHead = 0
	}
	if ts, exists := sh.nonces[key]; exists && ts >= cutoff {
		return false
	}
	sh.nonces[key] = now
	sh.nonceOrder = append(sh.nonceOrder, hubNonceEntry{key: key, ts: now})
	return true
}
