package market

import (
	"bytes"
	"context"
	crand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thejerf/suture/v4"

	"github.com/kryptokrates/nektar/internal/autonomy"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/store"
)

// SubscriptionSupervisorConfig configures persistent Grade S SSE subscription supervision.
type SubscriptionSupervisorConfig struct {
	Watchdog        *autonomy.Watchdog
	HTTPClient      *http.Client
	MaxRetries      int           // default 3 reconnect retries with jittered backoff before dead-feed slash
	RetryBackoff    time.Duration // default 2s initial backoff
	MaxRetryBackoff time.Duration // default 60s max backoff
	ExpirySweep     time.Duration // default 60s
	OnUpdate        func(event string, subID string)
}

// SubscriptionSupervisorStatus captures live runtime health of supervised buyer Grade S feeds.
type SubscriptionSupervisorStatus struct {
	Running      bool     `json:"running"`
	ActiveCount  int      `json:"activeCount"`
	ActiveSubIDs []string `json:"activeSubIds,omitempty"`
	EventsTotal  int64    `json:"eventsTotal"`
	SlashesTotal int64    `json:"slashesTotal"`
}

// SubscribePeerRequest configures a paid buyer-side Grade S subscription purchase via POST /v1/market/subscriptions.
type SubscribePeerRequest struct {
	PeerBaseURL string
	SellerPub   string
	Shard       string
	TargetShard string
	OntologyID  string
	Domains     []string
	MaxSats     uint64
}

type supervisedFeedEntry struct {
	sub   store.ShardSubscription
	token suture.ServiceToken
}

// SubscriptionSupervisor manages one isolated suture/v4 worker per active unexpired buyer subscription,
// automatically resuming from persisted SQLite last_seq across daemon restarts and laptop sleep,
// applying fragment_distilled / fragment_superseded updates locally, and slashing dead feeds.
type SubscriptionSupervisor struct {
	mem    *memory.Service
	engine *Engine
	cfg    SubscriptionSupervisorConfig

	mu      sync.Mutex
	sup     *suture.Supervisor
	running bool
	cancel  context.CancelFunc
	doneCh  <-chan error
	wg      sync.WaitGroup
	entries map[string]*supervisedFeedEntry

	eventsTotal  atomic.Int64
	slashesTotal atomic.Int64
}

// NewSubscriptionSupervisor constructs a persistent Grade S feed supervisor.
func NewSubscriptionSupervisor(mem *memory.Service, engine *Engine, cfg SubscriptionSupervisorConfig) *SubscriptionSupervisor {
	if cfg.MaxRetries <= 0 {
		cfg.MaxRetries = 3
	}
	if cfg.RetryBackoff <= 0 {
		cfg.RetryBackoff = 2 * time.Second
	}
	if cfg.MaxRetryBackoff <= 0 {
		cfg.MaxRetryBackoff = 60 * time.Second
	}
	if cfg.ExpirySweep <= 0 {
		cfg.ExpirySweep = 60 * time.Second
	}
	if cfg.HTTPClient == nil {
		netName := ""
		if mem != nil && mem.Chain() != nil {
			netName = mem.Chain().Network()
		}
		cfg.HTTPClient = core.SafeHTTPClientForNetwork(netName, 0)
	}
	return &SubscriptionSupervisor{
		mem:     mem,
		engine:  engine,
		cfg:     cfg,
		entries: make(map[string]*supervisedFeedEntry),
	}
}

// SetWatchdog updates the attached autonomy Watchdog reference.
func (s *SubscriptionSupervisor) SetWatchdog(w *autonomy.Watchdog) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.cfg.Watchdog = w
	s.mu.Unlock()
}

// Start boots the suture/v4 supervision tree, reloads every unexpired buyer subscription from SQLite,
// and starts periodic expiry cleanup. Idempotent across calls.
func (s *SubscriptionSupervisor) Start(ctx context.Context) error {
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return nil
	}
	s.mu.Lock()
	if s.running {
		s.mu.Unlock()
		return nil
	}
	runCtx, cancel := context.WithCancel(ctx)
	sup := suture.New("nektar-grade-s-feeds", suture.Spec{
		EventHook:        func(suture.Event) {},
		FailureThreshold: 10,
		FailureBackoff:   3 * time.Second,
	})
	s.sup = sup
	s.cancel = cancel
	s.doneCh = sup.ServeBackground(runCtx)
	s.running = true
	s.mu.Unlock()

	if err := s.Reload(runCtx); err != nil {
		return err
	}

	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		ticker := time.NewTicker(s.cfg.ExpirySweep)
		defer ticker.Stop()
		for {
			select {
			case <-runCtx.Done():
				return
			case <-ticker.C:
				_, _ = s.mem.DB().DeactivateExpiredSubscriptions(time.Now().Unix())
				_ = s.Reload(runCtx)
			}
		}
	}()
	return nil
}

// Stop shuts down all active SSE subscription workers cleanly and waits for goroutines to exit.
func (s *SubscriptionSupervisor) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	cancel := s.cancel
	doneCh := s.doneCh
	s.running = false
	s.cancel = nil
	s.sup = nil
	clear(s.entries)
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if doneCh != nil {
		select {
		case <-doneCh:
		case <-time.After(3 * time.Second):
		}
	}
	s.wg.Wait()
}

// Reload loads all unexpired active buyer subscriptions from SQLite and ensures each has a live worker.
func (s *SubscriptionSupervisor) Reload(ctx context.Context) error {
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return nil
	}
	now := time.Now().Unix()
	_, _ = s.mem.DB().DeactivateExpiredSubscriptions(now)
	subs, err := s.mem.DB().ListActiveBuyerSubscriptions(now)
	if err != nil {
		return err
	}
	for _, sub := range subs {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := s.attachLockedOrSpawn(sub); err != nil {
			continue
		}
	}
	return nil
}

// Ensure persists a buyer subscription to SQLite and attaches a supervised suture worker if running.
func (s *SubscriptionSupervisor) Ensure(sub store.ShardSubscription) error {
	if s == nil || s.mem == nil || s.mem.DB() == nil {
		return errors.New("market: subscription supervisor not initialized")
	}
	sub.Role = "buyer"
	if strings.TrimSpace(sub.TargetShard) == "" {
		sub.TargetShard = "sub-" + sub.Shard
	}
	if err := s.mem.DB().UpsertBuyerSubscription(sub); err != nil {
		return err
	}
	if !sub.Active || sub.Slashed || (sub.ExpiresAt > 0 && time.Now().Unix() >= sub.ExpiresAt) {
		return nil
	}
	return s.attachLockedOrSpawn(sub)
}

func (s *SubscriptionSupervisor) attachLockedOrSpawn(sub store.ShardSubscription) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.running || s.sup == nil {
		return nil
	}
	if _, exists := s.entries[sub.SubID]; exists {
		return nil
	}
	svc := &gradeSFeedWorker{
		parent: s,
		sub:    sub,
	}
	tok := s.sup.Add(svc)
	s.entries[sub.SubID] = &supervisedFeedEntry{sub: sub, token: tok}
	return nil
}

func (s *SubscriptionSupervisor) removeEntry(subID string) {
	s.mu.Lock()
	delete(s.entries, subID)
	s.mu.Unlock()
}

// Status returns a consistent telemetry snapshot of currently supervised buyer feeds.
func (s *SubscriptionSupervisor) Status() SubscriptionSupervisorStatus {
	if s == nil {
		return SubscriptionSupervisorStatus{}
	}
	s.mu.Lock()
	running := s.running
	ids := make([]string, 0, len(s.entries))
	for id := range s.entries {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	sort.Strings(ids)
	return SubscriptionSupervisorStatus{
		Running:      running,
		ActiveCount:  len(ids),
		ActiveSubIDs: ids,
		EventsTotal:  s.eventsTotal.Load(),
		SlashesTotal: s.slashesTotal.Load(),
	}
}

// SubscribePeer purchases a paid Grade S subscription over BRC-105 HTTP 402, verifies the seller's
// DealReceipt, persists the buyer subscription row, and immediately attaches it to the supervisor.
func (s *SubscriptionSupervisor) SubscribePeer(ctx context.Context, req SubscribePeerRequest) (*store.ShardSubscription, error) {
	if s == nil || s.mem == nil || s.engine == nil {
		return nil, errors.New("market: subscription supervisor not initialized")
	}
	if w := s.watchdog(); w != nil {
		if err := w.AllowAutonomousSpend(); err != nil {
			return nil, err
		}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(req.PeerBaseURL), "/")
	shard := strings.TrimSpace(req.Shard)
	if baseURL == "" || shard == "" {
		return nil, errors.New("market: peerBaseUrl and shard are required")
	}
	if s.engine.Policy() != nil {
		if kill, _, _, _, _, _, _ := s.engine.Policy().Snapshot(); kill {
			return nil, errors.New("market: kill switch active")
		}
	}

	buyerPub := s.engine.SalePubHex()
	payload, err := json.Marshal(map[string]any{
		"shard":    shard,
		"buyerPub": buyerPub,
	})
	if err != nil {
		return nil, err
	}

	hc := core.SafeHTTPClientForNetwork(s.mem.Chain().Network(), 15*time.Second)
	subEndpoint := baseURL + "/v1/market/subscriptions"

	doPost := func(payHdr string) (*http.Response, error) {
		httpReq, rErr := http.NewRequestWithContext(ctx, http.MethodPost, subEndpoint, bytes.NewReader(payload))
		if rErr != nil {
			return nil, rErr
		}
		httpReq.Header.Set("Content-Type", "application/json")
		if payHdr != "" {
			httpReq.Header.Set("X-BSV-Payment", payHdr)
		}
		return hc.Do(httpReq)
	}

	resp, err := doPost("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusPaymentRequired {
		requiredSats, payeePub, derivPrefix, challengeSellerPub, parseErr := parseBRC105Challenge(resp)
		_ = resp.Body.Close()
		if parseErr != nil {
			return nil, parseErr
		}
		if req.SellerPub != "" && challengeSellerPub != "" && !strings.EqualFold(challengeSellerPub, req.SellerPub) {
			return nil, fmt.Errorf("market: 402 sellerPub mismatch (expected %s, got %s)", req.SellerPub, challengeSellerPub)
		}
		if req.MaxSats > 0 && requiredSats > req.MaxSats {
			return nil, fmt.Errorf("market: subscription price %d sats exceeds cap %d sats", requiredSats, req.MaxSats)
		}
		if s.engine.Policy() != nil {
			if err := s.engine.Policy().CanSpendForDomains(payeePub, req.Domains, requiredSats); err != nil {
				return nil, err
			}
		}
		payHdr, _, payErr := discovery.BuildStandaloneHubPayment(ctx, s.mem.Chain(), payeePub, derivPrefix, "market_subscription:"+shard, requiredSats)
		if payErr != nil {
			if w := s.watchdog(); w != nil {
				w.RecordSpendOutcome(payErr)
			}
			return nil, payErr
		}
		resp, err = doPost(payHdr)
		if err != nil {
			if w := s.watchdog(); w != nil {
				w.RecordSpendOutcome(err)
			}
			return nil, err
		}
	}
	defer resp.Body.Close()
	bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	if resp.StatusCode != http.StatusOK {
		err := fmt.Errorf("market: subscribe HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		if w := s.watchdog(); w != nil {
			w.RecordSpendOutcome(err)
		}
		return nil, err
	}

	var out struct {
		OK          bool         `json:"ok"`
		SubID       string       `json:"subId"`
		Shard       string       `json:"shard"`
		FeedURL     string       `json:"feedUrl"`
		StartedAt   int64        `json:"startedAt"`
		ExpiresAt   int64        `json:"expiresAt"`
		UsageRights string       `json:"usageRights"`
		PaidSats    uint64       `json:"paidSats"`
		PaymentTxID string       `json:"paymentTxid"`
		DealReceipt *DealReceipt `json:"dealReceipt"`
	}
	if err := json.Unmarshal(bodyBytes, &out); err != nil {
		return nil, fmt.Errorf("market: decode subscribe response: %w", err)
	}
	if !out.OK || out.SubID == "" || out.FeedURL == "" {
		return nil, errors.New("market: incomplete subscribe response")
	}

	var receiptJSON []byte
	sellerPub := strings.ToLower(strings.TrimSpace(req.SellerPub))
	if out.DealReceipt != nil {
		if vr := VerifyDealReceipt(*out.DealReceipt, false); !vr.OK {
			return nil, fmt.Errorf("market: invalid seller subscription deal receipt: %s", vr.Error)
		}
		if sellerPub == "" {
			sellerPub = strings.ToLower(strings.TrimSpace(out.DealReceipt.SellerPub))
		}
		receiptJSON, _ = json.Marshal(out.DealReceipt)
	}

	targetShard := strings.TrimSpace(req.TargetShard)
	if targetShard == "" {
		targetShard = "sub-" + shard
	}
	sub := store.ShardSubscription{
		SubID:           out.SubID,
		Role:            "buyer",
		Shard:           shard,
		BuyerPub:        buyerPub,
		SellerPub:       sellerPub,
		PricingMode:     string(nmu.ModeSubscription),
		PriceSats:       out.PaidSats,
		PaymentTxID:     out.PaymentTxID,
		UsageRights:     out.UsageRights,
		StartedAt:       out.StartedAt,
		ExpiresAt:       out.ExpiresAt,
		LastSeq:         0,
		Active:          true,
		FeedURL:         out.FeedURL,
		OntologyID:      strings.TrimSpace(req.OntologyID),
		Domains:         append([]string(nil), req.Domains...),
		TargetShard:     targetShard,
		DealReceiptJSON: receiptJSON,
	}
	if err := s.Ensure(sub); err != nil {
		return nil, err
	}
	if s.engine.Policy() != nil {
		s.engine.Policy().RecordSpend(out.PaidSats)
	} else {
		_, _ = s.mem.DB().AddDailySpend("market_daily_spend", out.PaidSats)
	}
	if w := s.watchdog(); w != nil {
		w.RecordSpendOutcome(nil)
	}
	return &sub, nil
}

func (s *SubscriptionSupervisor) watchdog() *autonomy.Watchdog {
	s.mu.Lock()
	w := s.cfg.Watchdog
	s.mu.Unlock()
	return w
}

type gradeSFeedWorker struct {
	parent *SubscriptionSupervisor
	sub    store.ShardSubscription
}

func (w *gradeSFeedWorker) String() string {
	return "grade-s-feed:" + w.sub.SubID
}

func (w *gradeSFeedWorker) Serve(ctx context.Context) error {
	defer func() {
		// Remove active registration when terminating permanently.
	}()
	db := w.parent.mem.DB()
	// Always reload latest cursor & active status from SQLite before connecting (handles restart/sleep!).
	latest, err := db.GetShardSubscription(w.sub.SubID)
	if err != nil {
		return err
	}
	if latest == nil || !latest.Active || latest.Slashed || (latest.ExpiresAt > 0 && time.Now().Unix() >= latest.ExpiresAt) {
		if latest != nil && latest.Active && latest.ExpiresAt > 0 && time.Now().Unix() >= latest.ExpiresAt {
			_ = db.RevokeShardSubscription(latest.SubID)
		}
		w.parent.removeEntry(w.sub.SubID)
		return suture.ErrDoNotRestart
	}
	sub := *latest
	var dr *DealReceipt
	if len(sub.DealReceiptJSON) > 0 {
		var parsed DealReceipt
		if json.Unmarshal(sub.DealReceiptJSON, &parsed) == nil {
			dr = &parsed
		}
	}

	targetShard := strings.TrimSpace(sub.TargetShard)
	if targetShard == "" {
		targetShard = "sub-" + sub.Shard
	}

	streamCfg := SubscriptionStreamConfig{
		FeedURL:         sub.FeedURL,
		Shard:           sub.Shard,
		SubID:           sub.SubID,
		SellerPub:       sub.SellerPub,
		OntologyID:      sub.OntologyID,
		Domains:         sub.Domains,
		ExpiresAtUnix:   sub.ExpiresAt,
		SinceSeq:        sub.LastSeq,
		DealReceipt:     dr,
		HTTPClient:      w.parent.cfg.HTTPClient,
		MaxRetries:      w.parent.cfg.MaxRetries,
		RetryBackoff:    w.parent.cfg.RetryBackoff,
		MaxRetryBackoff: w.parent.cfg.MaxRetryBackoff,
		OnSeqAdvance: func(seq int64) {
			_ = db.UpdateSubscriptionCursor(sub.SubID, seq, time.Now().Unix())
		},
		OnSlash: func(sellerPub, reason string) {
			w.parent.slashesTotal.Add(1)
			_ = db.MarkSubscriptionSlashed(sub.SubID)
			if wd := w.parent.watchdog(); wd != nil {
				wd.RecordSlash(sellerPub, reason)
			}
			if w.parent.cfg.OnUpdate != nil {
				w.parent.cfg.OnUpdate("subscription_slashed", sub.SubID)
			}
		},
	}

	runErr := w.parent.engine.StreamShardSubscription(ctx, streamCfg, func(evName string, raw []byte) {
		w.handleEvent(ctx, sub, targetShard, evName, raw)
	})

	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(runErr, ErrDeadSubscriptionFeed) {
		_ = db.MarkSubscriptionSlashed(sub.SubID)
		w.parent.removeEntry(sub.SubID)
		return suture.ErrDoNotRestart
	}
	if runErr == nil || (sub.ExpiresAt > 0 && time.Now().Unix() >= sub.ExpiresAt-5) {
		_ = db.RevokeShardSubscription(sub.SubID)
		w.parent.removeEntry(sub.SubID)
		return suture.ErrDoNotRestart
	}
	return runErr
}

func (w *gradeSFeedWorker) seqMapKey(subID string, sourceSeq int64) string {
	return fmt.Sprintf("sub_seq_map:%s:%d", subID, sourceSeq)
}

func (w *gradeSFeedWorker) handleEvent(ctx context.Context, sub store.ShardSubscription, targetShard, evName string, raw []byte) {
	db := w.parent.mem.DB()
	switch evName {
	case "fragment_distilled":
		var ev struct {
			Shard       string    `json:"shard"`
			Seq         int64     `json:"seq"`
			Text        string    `json:"text"`
			HDP         string    `json:"hdp"`
			Leaf        string    `json:"leaf"`
			OntologyID  string    `json:"ontologyId"`
			EdgesRoot   string    `json:"edgesRoot"`
			NMU         *nmu.Spec `json:"nmu"`
			UsageRights string    `json:"usageRights"`
			CreatedAt   int64     `json:"createdAt"`
		}
		if json.Unmarshal(raw, &ev) != nil || strings.TrimSpace(ev.Text) == "" || ev.Seq <= 0 {
			return
		}
		// Deduplicate across reconnect/restart via persisted sourceSeq -> localSeq mapping.
		var existingLocalSeq int64
		if found, _ := db.GetJSON(w.seqMapKey(sub.SubID, ev.Seq), &existingLocalSeq); found && existingLocalSeq > 0 {
			_ = db.UpdateSubscriptionCursor(sub.SubID, ev.Seq, time.Now().Unix())
			return
		}
		if sr := distill.Scrub(ev.Text); sr.Verdict == distill.ScrubBlocked {
			_ = db.UpdateSubscriptionCursor(sub.SubID, ev.Seq, time.Now().Unix())
			return
		}
		rights := strings.TrimSpace(ev.UsageRights)
		if rights == "" {
			rights = sub.UsageRights
		}
		if rights == "" {
			rights = string(nmu.RightsInferenceOnly)
		}
		res, err := w.parent.mem.Remember(ctx, memory.RememberInput{
			Shard:       targetShard,
			Text:        ev.Text,
			Source:      "grade-s-feed:" + sub.SubID,
			Tainted:     true,
			Ontology:    ev.OntologyID,
			NMU:         ev.NMU,
			UsageRights: rights,
			Extra: map[string]string{
				"source_sub":   sub.SubID,
				"seller_pub":   sub.SellerPub,
				"source_shard": ev.Shard,
				"source_seq":   strconv.FormatInt(ev.Seq, 10),
				"source_leaf":  ev.Leaf,
			},
		})
		if err != nil {
			return
		}
		_ = db.SetJSON(w.seqMapKey(sub.SubID, ev.Seq), res.Seq)
		_ = db.UpdateSubscriptionCursor(sub.SubID, ev.Seq, time.Now().Unix())
		w.parent.eventsTotal.Add(1)
		if w.parent.cfg.OnUpdate != nil {
			w.parent.cfg.OnUpdate("subscription_fragment", sub.SubID)
		}

	case "fragment_superseded":
		var ev memory.SupersessionEvent
		if json.Unmarshal(raw, &ev) != nil || ev.SupersededSeq <= 0 {
			return
		}
		var targetLocalSeq int64
		if found, _ := db.GetJSON(w.seqMapKey(sub.SubID, ev.SupersededSeq), &targetLocalSeq); found && targetLocalSeq > 0 {
			reason := strings.TrimSpace(ev.Reason)
			if reason == "" {
				reason = fmt.Sprintf("superseded by seller seq #%d", ev.NewSeq)
			}
			_, _ = w.parent.mem.Remember(ctx, memory.RememberInput{
				Shard:       targetShard,
				Text:        fmt.Sprintf("[Grade S supersession notice] source %s#%d superseded (%s)", ev.Shard, ev.SupersededSeq, reason),
				Source:      "grade-s-supersession:" + sub.SubID,
				Supersedes:  targetLocalSeq,
				Tainted:     true,
				Ontology:    ev.OntologyID,
				UsageRights: sub.UsageRights,
				Extra: map[string]string{
					"source_sub":     sub.SubID,
					"superseded_seq": strconv.FormatInt(ev.SupersededSeq, 10),
					"new_seq":        strconv.FormatInt(ev.NewSeq, 10),
				},
			})
		}
		if ev.NewSeq > 0 {
			_ = db.UpdateSubscriptionCursor(sub.SubID, ev.NewSeq, time.Now().Unix())
		}
		w.parent.eventsTotal.Add(1)
		if w.parent.cfg.OnUpdate != nil {
			w.parent.cfg.OnUpdate("subscription_superseded", sub.SubID)
		}
	}
}

func parseBRC105Challenge(resp *http.Response) (amountSats uint64, payeePub, derivPrefix, sellerPub string, err error) {
	if resp == nil {
		return 0, "", "", "", errors.New("market: nil 402 response")
	}
	var body struct {
		AmountSats       uint64 `json:"amountSats"`
		IdentityKey      string `json:"identityKey"`
		DerivationPrefix string `json:"derivationPrefix"`
		SellerPub        string `json:"sellerPub"`
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	_ = json.Unmarshal(raw, &body)

	amountSats = body.AmountSats
	if h := strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Satoshis-Required")); h != "" {
		if v, pErr := strconv.ParseUint(h, 10, 64); pErr == nil && v > 0 {
			amountSats = v
		}
	}
	payeePub = strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Identity"))
	if payeePub == "" {
		payeePub = strings.TrimSpace(body.IdentityKey)
	}
	derivPrefix = strings.TrimSpace(resp.Header.Get("X-BSV-Payment-Derivation-Prefix"))
	if derivPrefix == "" {
		derivPrefix = strings.TrimSpace(body.DerivationPrefix)
	}
	if derivPrefix == "" {
		var rnd [16]byte
		if _, rErr := crand.Read(rnd[:]); rErr != nil {
			return 0, "", "", "", fmt.Errorf("market: generate derivation prefix: %w", rErr)
		}
		derivPrefix = base64.StdEncoding.EncodeToString(rnd[:])
	}
	sellerPub = strings.ToLower(strings.TrimSpace(body.SellerPub))
	if amountSats == 0 || payeePub == "" {
		return 0, "", "", "", fmt.Errorf("market: incomplete BRC-105 402 challenge (sats=%d payee=%v)", amountSats, payeePub != "")
	}
	return amountSats, strings.ToLower(payeePub), derivPrefix, sellerPub, nil
}
