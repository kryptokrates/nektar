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
	if e == nil || e.saleKey == nil {
		return errors.New("market: sale signing key unavailable")
	}
	return SignDealReceiptSeller(ctx, e.salePubHex(), e.signSaleDigest, r)
}

// SignFeedAuthHeaders produces replay-protected GET|/v1/market/feeds/{shard}|{subId}|{ts} auth headers.
func (e *Engine) SignFeedAuthHeaders(shard, subID string, tsUnix int64) (pubHex, sigHex string, err error) {
	if e == nil || e.saleKey == nil {
		return "", "", errors.New("market: sale signing key unavailable")
	}
	return nmu.SignFeedAuth(e.saleKey, shard, subID, tsUnix)
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
	DealReceipt   *DealReceipt
	HTTPClient    *http.Client
	MaxRetries    int           // default 1 reconnect retry before penalizing dead feed
	RetryBackoff  time.Duration // default 100ms
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
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = 100 * time.Millisecond
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
		pubHex, sigHex, err := e.SignFeedAuthHeaders(cfg.Shard, cfg.SubID, now)
		if err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			return err
		}
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
				e.slashDeadSubscriptionFeed(ctx, cfg)
				return fmt.Errorf("%w: %v", ErrDeadSubscriptionFeed, err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}

		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || resp.StatusCode >= 500 {
			_ = resp.Body.Close()
			failures++
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone || failures > maxRetries {
				e.slashDeadSubscriptionFeed(ctx, cfg)
				return fmt.Errorf("%w: HTTP %d", ErrDeadSubscriptionFeed, resp.StatusCode)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(backoff):
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			bodyBytes, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			_ = resp.Body.Close()
			return fmt.Errorf("market: feed HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(bodyBytes)))
		}

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
			if json.Unmarshal(raw, &seqPeek) == nil {
				if seqPeek.Seq > lastSeq {
					lastSeq = seqPeek.Seq
				}
				if seqPeek.NewSeq > lastSeq {
					lastSeq = seqPeek.NewSeq
				}
			}
			if onEvent != nil {
				onEvent(evName, raw)
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
			e.slashDeadSubscriptionFeed(ctx, cfg)
			return fmt.Errorf("%w: stream closed prematurely before expiresAt=%d", ErrDeadSubscriptionFeed, cfg.ExpiresAtUnix)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
	}
}

func (e *Engine) slashDeadSubscriptionFeed(ctx context.Context, cfg SubscriptionStreamConfig) {
	if e == nil || e.mem == nil || e.mem.DB() == nil || cfg.DealReceipt == nil {
		return
	}
	dr := *cfg.DealReceipt
	if dr.BuyerSig == "" && e.saleKey != nil {
		_ = SignDealReceiptBuyer(ctx, e.salePubHex(), e.signSaleDigest, &dr)
	}
	sellerPub := strings.TrimSpace(cfg.SellerPub)
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
