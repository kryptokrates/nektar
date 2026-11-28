package market

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v5"

	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/reputation"
)

// SalePubHex returns this node's compressed 33-byte market signing public key hex.
func (e *Engine) SalePubHex() string {
	return e.salePubHex()
}

// SignSellerDealReceipt signs r with this node's persistent market sale key (matching OfferAd.Pub).
func (e *Engine) SignSellerDealReceipt(ctx context.Context, r *DealReceipt) error {
	if e == nil || e.signer == nil {
		return errors.New("market: sale signing key unavailable")
	}
	return SignDealReceiptSeller(ctx, e.salePubHex(), e.signSaleDigest, r)
}

// SignFeedAuthHeaders produces replay-protected CBOR feed auth headers bound to (shard, subID, tsUnix, nonce, aud).
func (e *Engine) SignFeedAuthHeaders(shard, subID string, tsUnix int64, nonce, aud string) (pubHex, sigHex string, err error) {
	if e == nil || e.signer == nil {
		return "", "", errors.New("market: sale signing key unavailable")
	}
	return nmu.SignFeedAuthWithSigner(e.salePubHex(), e.signer.SignDigest, shard, subID, tsUnix, nonce, aud)
}

// SubscriptionStreamConfig configures a buyer's authenticated SSE subscription stream to a Grade S seller feed.
type SubscriptionStreamConfig struct {
	FeedURL       string
	Shard         string
	SubID         string
	SellerPub     string
	OntologyID    string
	Domains       []string
	ExpiresAtUnix int64
	SinceSeq      int64
	DealReceipt     *DealReceipt
	HTTPClient      *http.Client
	MaxRetries      int           // default 1 reconnect retry before penalizing dead feed
	RetryBackoff    time.Duration // initial jittered backoff interval (default 100ms)
	MaxRetryBackoff time.Duration // max jittered backoff cap (default 60s)
	OnEventCommit   func(eventType string, data []byte) bool
	OnSeqAdvance    func(seq int64)
	CanSlash        func() bool
	OnSlash         func(sellerPub, reason string)
}

// ErrDeadSubscriptionFeed is returned when a paid subscription feed returns 404/410/5xx or drops
// prematurely before ExpiresAtUnix (after emitting an automatic DispBP slashing attestation).
var ErrDeadSubscriptionFeed = errors.New("market: paid subscription feed dropped or unavailable before expiresAt")

// StreamShardSubscription connects to a seller's authenticated SSE Shard Feed (`GET /v1/market/feeds/{shard}`)
// with fresh +/-60s replay-protected signatures. If a paid Grade S subscription feed returns 404/410/5xx or
// drops prematurely before ExpiresAtUnix (while ctx is still active), it automatically emits a DispBP penalty
// `nektarrep1.*` attestation citing cfg.DealReceipt so dead-feed sellers lose their Grade S ranking bonus.
func (e *Engine) StreamShardSubscription(
	ctx context.Context,
	cfg SubscriptionStreamConfig,
	onEvent func(eventType string, data []byte),
) error {
	if strings.TrimSpace(cfg.FeedURL) == "" || strings.TrimSpace(cfg.Shard) == "" || strings.TrimSpace(cfg.SubID) == "" {
		return errors.New("market: feedUrl, shard, and subId are required")
	}
	hc := cfg.HTTPClient
	if hc == nil {
		netName := ""
		if e != nil && e.mem != nil && e.mem.Chain() != nil {
			netName = e.mem.Chain().Network()
		}
		hc = core.SafeHTTPClientForNetwork(netName, 0)
	}
	maxRetries := cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 1
	}
	initBackoff := cfg.RetryBackoff
	if initBackoff <= 0 {
		initBackoff = 100 * time.Millisecond
	}
	maxBackoff := cfg.MaxRetryBackoff
	if maxBackoff <= 0 {
		maxBackoff = 60 * time.Second
	}
	if maxBackoff < initBackoff {
		maxBackoff = initBackoff
	}
	bo := backoff.NewExponentialBackOff()
	bo.InitialInterval = initBackoff
	bo.MaxInterval = maxBackoff
	bo.RandomizationFactor = 0.3
	bo.Reset()
	nextWait := func() time.Duration {
		d := bo.NextBackOff()
		if d == backoff.Stop || d <= 0 {
			return initBackoff
		}
		return d
	}

	lastSeq := cfg.SinceSeq
	failures := 0

	for {
		now := time.Now().Unix()
		if cfg.ExpiresAtUnix > 0 && now >= cfg.ExpiresAtUnix {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}

		reqURL := cfg.FeedURL
		if lastSeq > 0 {
			if u, pErr := url.Parse(reqURL); pErr == nil {
				q := u.Query()
				q.Set("sinceSeq", strconv.FormatInt(lastSeq, 10))
				u.RawQuery = q.Encode()
				reqURL = u.String()
			}
		}
		nonce, nErr := nmu.NewFeedNonce()
		if nErr != nil {
			return nErr
		}
		aud := ""
		if u, pErr := url.Parse(reqURL); pErr == nil && u.Host != "" {
			aud = strings.ToLower(u.Host)
		}
		pubHex, sigHex, err := e.SignFeedAuthHeaders(cfg.Shard, cfg.SubID, now, nonce, aud)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
		req.Header.Set(nmu.HeaderNonce, nonce)
		req.Header.Set("Accept", "text/event-stream")
		req.Header.Set(nmu.HeaderSubID, cfg.SubID)
		req.Header.Set(nmu.HeaderPub, pubHex)
		req.Header.Set(nmu.HeaderSig, sigHex)
		req.Header.Set(nmu.HeaderTS, strconv.FormatInt(now, 10))

		resp, err := hc.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			failures++
			if failures > maxRetries {
				e.slashDeadSubscriptionFeed(ctx, cfg, err.Error())
				return fmt.Errorf("%w: %v", ErrDeadSubscriptionFeed, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nextWait()):
			}
			continue
		}

		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			failures++
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || failures > maxRetries {
				e.slashDeadSubscriptionFeed(ctx, cfg, fmt.Sprintf("HTTP %d", resp.StatusCode))
				return fmt.Errorf("%w: HTTP %d", ErrDeadSubscriptionFeed, resp.StatusCode)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(nextWait()):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return fmt.Errorf("market: feed HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		}
		// Successful SSE connection resets exponential backoff and transient failure counter.
		bo.Reset()
		failures = 0

		// Stream SSE frames cleanly.
		expiredCleanly := false
		scanner := bufio.NewScanner(resp.Body)
		scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
		var curEvent string
		var dataLines []string
		flushFrame := func() {
			if len(dataLines) == 0 {
				curEvent = ""
				return
			}
			evName := curEvent
			if evName == "" {
				evName = "message"
			}
			raw := []byte(strings.Join(dataLines, "\n"))
			if evName == "subscription_expired" {
				expiredCleanly = true
			}
			var seqPeek struct {
				Seq    int64 `json:"seq"`
				NewSeq int64 `json:"newSeq"`
			}
			candSeq := lastSeq
			if json.Unmarshal(raw, &seqPeek) == nil {
				if seqPeek.Seq > candSeq {
					candSeq = seqPeek.Seq
				}
				if seqPeek.NewSeq > candSeq {
					candSeq = seqPeek.NewSeq
				}
			}
			committed := true
			if cfg.OnEventCommit != nil {
				committed = cfg.OnEventCommit(evName, raw)
			}
			if onEvent != nil {
				onEvent(evName, raw)
			}
			if committed && candSeq > lastSeq {
				lastSeq = candSeq
				if cfg.OnSeqAdvance != nil {
					cfg.OnSeqAdvance(lastSeq)
				}
			}
			curEvent = ""
			dataLines = dataLines[:0]
		}

		for scanner.Scan() {
			line := scanner.Text()
			if line == "" {
				flushFrame()
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				curEvent = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			}
		}
		flushFrame()
		_ = resp.Body.Close()

		if expiredCleanly || ctx.Err() != nil {
			return ctx.Err()
		}
		if cfg.ExpiresAtUnix > 0 && time.Now().Unix() >= cfg.ExpiresAtUnix-5 {
			return nil
		}
		// Stream dropped prematurely before ExpiresAtUnix!
		failures++
		if failures > maxRetries {
			reason := fmt.Sprintf("stream closed prematurely before expiresAt=%d", cfg.ExpiresAtUnix)
			e.slashDeadSubscriptionFeed(ctx, cfg, reason)
			return fmt.Errorf("%w: %s", ErrDeadSubscriptionFeed, reason)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(nextWait()):
		}
	}
}

func (e *Engine) slashDeadSubscriptionFeed(ctx context.Context, cfg SubscriptionStreamConfig, reason string) {
	if e == nil || e.mem == nil || e.mem.DB() == nil {
		return
	}
	if cfg.CanSlash != nil && !cfg.CanSlash() {
		return
	}
	sellerPub := strings.TrimSpace(cfg.SellerPub)
	if cfg.DealReceipt != nil {
		dr := *cfg.DealReceipt
		if dr.BuyerSig == "" && e.signer != nil {
			_ = SignDealReceiptBuyer(ctx, e.salePubHex(), e.signSaleDigest, &dr)
		}
		if sellerPub == "" {
			sellerPub = dr.SellerPub
		}
		scores := reputation.EvaluatePurchasedFragment(reputation.BuyerEvalInput{
			SellerPub:         sellerPub,
			OntologyID:        cfg.OntologyID,
			SPVVerified:       true,
			MPEVerified:       true,
			HasPinnedOntology: cfg.OntologyID != "",
			FeedDroppedEarly:  true,
		})
		_, _ = e.EmitPostDealAttestation(ctx, reputation.RoleBuyerRatesSeller, dr, cfg.OntologyID, scores, cfg.Domains)
	}
	if cfg.OnSlash != nil {
		cfg.OnSlash(sellerPub, reason)
	}
}
