package chain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/broadcaster"
	"github.com/lightwebinc/bcommon/nodeapi"
	"github.com/sony/gobreaker/v2"

	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// BroadcasterOptions configures the single streamlined BSV Broadcaster skeleton.
type BroadcasterOptions struct {
	APIKey  string
	RPCUser string
	RPCPass string
	Client  *http.Client
}

// BSVBroadcaster is Nektar's single streamlined transaction broadcasting skeleton,
// backed strictly by the official github.com/bsv-blockchain/go-sdk/transaction/broadcaster
// client with per-endpoint gobreaker circuit breakers and ARC /txs batch support.
type BSVBroadcaster struct {
	endpoints []*broadcaster.Arc
	breakers  []*gobreaker.CircuitBreaker[any]
	urls      []string
	nextIdx   atomic.Uint64
}

var _ nwallet.Broadcaster = (*BSVBroadcaster)(nil)

// rpcBroadcaster adapts a local bitcoind/Teranode RPC endpoint for regtest under the same Broadcaster interface.
type rpcBroadcaster struct {
	url string
	rpc *nodeapi.RPC
}

var _ nwallet.Broadcaster = (*rpcBroadcaster)(nil)

func (r *rpcBroadcaster) Name() string { return "rpc:" + r.url }

func (r *rpcBroadcaster) Broadcast(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("chain: nil transaction")
	}
	_, err := r.rpc.SendRawTransaction(ctx, tx.Hex())
	return err
}

func (r *rpcBroadcaster) Proof(_ context.Context, _ string) (*transaction.MerklePath, uint32, bool, error) {
	return nil, 0, false, nil
}

// ParseBroadcaster parses a single or comma-separated settlement spec into Nektar's single
// nwallet.Broadcaster skeleton plus the list of HTTP BSV Broadcaster base URLs (for /policy fee quotes).
//
// Supported specs:
//   - broadcaster:main | arcade:main                 -> official US + EU BSV Broadcaster endpoints
//   - broadcaster:testnet | arcade:test              -> official Testnet3 BSV Broadcaster endpoint
//   - broadcaster:https://host | arcade:https://host -> BSV Broadcaster instance (yours or public)
//   - arc:https://host[/v1]                          -> ARC-compatible instance (/v1 added if bare path)
//   - https://host                                   -> BSV Broadcaster base URL directly
//   - comma-separated combinations of the above      -> round-robin + failover BSVBroadcaster
//   - rpc:http://node:port                           -> local regtest RPC sendrawtransaction
func ParseBroadcaster(spec string, opts BroadcasterOptions) (nwallet.Broadcaster, []string, error) {
	norm := NormalizeSettleSpec(spec)
	parts := splitSpecs(norm)
	if len(parts) == 0 {
		return nil, nil, errors.New("chain: empty broadcaster spec")
	}

	// Local regtest RPC single endpoint.
	if len(parts) == 1 && strings.HasPrefix(parts[0], "rpc:") {
		rawURL := strings.TrimSpace(strings.TrimPrefix(parts[0], "rpc:"))
		if err := validateHTTPURL(rawURL); err != nil {
			return nil, nil, fmt.Errorf("chain: bad rpc broadcaster spec %q: %w", parts[0], err)
		}
		rpc := &nodeapi.RPC{
			URL:    rawURL,
			User:   opts.RPCUser,
			Pass:   opts.RPCPass,
			Client: opts.Client,
		}
		return &rpcBroadcaster{url: rawURL, rpc: rpc}, nil, nil
	}

	endpoints := make([]*broadcaster.Arc, 0, len(parts))
	urls := make([]string, 0, len(parts))
	seen := make(map[string]bool, len(parts))

	for _, part := range parts {
		apiURL, err := parseBroadcasterHTTPURL(part)
		if err != nil {
			return nil, nil, err
		}
		if seen[apiURL] {
			continue
		}
		seen[apiURL] = true
		arcClient := &broadcaster.Arc{
			ApiUrl: apiURL,
			ApiKey: opts.APIKey,
		}
		if opts.Client != nil {
			arcClient.Client = opts.Client
		}
		endpoints = append(endpoints, arcClient)
		urls = append(urls, apiURL)
	}
	if len(endpoints) == 0 {
		return nil, nil, fmt.Errorf("chain: no valid BSV Broadcaster endpoints in %q", spec)
	}
	breakers := make([]*gobreaker.CircuitBreaker[any], len(endpoints))
	for i, ep := range endpoints {
		breakers[i] = gobreaker.NewCircuitBreaker[any](gobreaker.Settings{
			Name:        "arc:" + ep.ApiUrl,
			MaxRequests: 2,
			Interval:    30 * time.Second,
			Timeout:     10 * time.Second,
			ReadyToTrip: func(counts gobreaker.Counts) bool {
				return counts.ConsecutiveFailures >= 5
			},
		})
	}
	return &BSVBroadcaster{
		endpoints: endpoints,
		breakers:  breakers,
		urls:      urls,
	}, urls, nil
}

func parseBroadcasterHTTPURL(item string) (string, error) {
	s := strings.TrimSpace(item)
	var rawURL string
	appendV1 := false
	switch {
	case strings.HasPrefix(s, "broadcaster:"):
		rawURL = strings.TrimPrefix(s, "broadcaster:")
	case strings.HasPrefix(s, "arcade:"):
		rawURL = strings.TrimPrefix(s, "arcade:")
	case strings.HasPrefix(s, "arc:"):
		rawURL = strings.TrimPrefix(s, "arc:")
		appendV1 = true
	case strings.HasPrefix(s, "http://") || strings.HasPrefix(s, "https://"):
		rawURL = s
	default:
		return "", fmt.Errorf("chain: unsupported broadcaster spec %q (use broadcaster:URL, arcade:URL, or rpc:URL)", item)
	}
	if err := validateHTTPURL(rawURL); err != nil {
		return "", fmt.Errorf("chain: bad broadcaster URL %q: %w", item, err)
	}
	u, _ := url.Parse(rawURL)
	if appendV1 && strings.Trim(u.Path, "/") == "" {
		u.Path = "/v1"
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func validateHTTPURL(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return err
	}
	if (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return errors.New("must be an http or https URL")
	}
	return nil
}

// Name returns a human-readable identifier for this BSV Broadcaster configuration.
func (b *BSVBroadcaster) Name() string {
	if len(b.urls) == 1 {
		return "broadcaster:" + b.urls[0]
	}
	return fmt.Sprintf("broadcaster:%s(+%d)", b.urls[0], len(b.urls)-1)
}

// Broadcast submits tx using the official go-sdk broadcaster.Arc client across configured
// endpoints (round-robin start + automatic failover on transport/timeout/429/5xx errors).
// Returns immediately once the BSV Broadcaster accepts the transaction (no blocking 15s poll).
func (b *BSVBroadcaster) Broadcast(ctx context.Context, tx *transaction.Transaction) error {
	if tx == nil {
		return errors.New("chain: nil transaction")
	}
	n := len(b.endpoints)
	if n == 0 {
		return errors.New("chain: no BSV Broadcaster endpoints configured")
	}
	start := int((b.nextIdx.Add(1) - 1) % uint64(n))

	var lastTransportErr error
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			return fmt.Errorf("%w: %w", nwallet.ErrBroadcastUnknown, ctx.Err())
		}
		idx := (start + i) % n
		ep := b.endpoints[idx]
		var cb *gobreaker.CircuitBreaker[any]
		if idx < len(b.breakers) {
			cb = b.breakers[idx]
		}
		if cb != nil && cb.State() == gobreaker.StateOpen {
			lastTransportErr = fmt.Errorf("%s: circuit breaker open", ep.ApiUrl)
			continue
		}
		var (
			success *transaction.BroadcastSuccess
			failure *transaction.BroadcastFailure
		)
		if cb != nil {
			_, cbErr := cb.Execute(func() (any, error) {
				success, failure = ep.BroadcastCtx(ctx, tx)
				if success != nil {
					return nil, nil
				}
				if failure == nil {
					return nil, errors.New("empty broadcaster response")
				}
				if isRetryableBroadcasterFailure(failure) && strings.TrimSpace(failure.Code) != "ERR_INVALID_RESPONSE" {
					return nil, fmt.Errorf("%s: %s", failure.Code, failure.Description)
				}
				return nil, nil
			})
			if errors.Is(cbErr, gobreaker.ErrOpenState) || errors.Is(cbErr, gobreaker.ErrTooManyRequests) {
				lastTransportErr = fmt.Errorf("%s: %w", ep.ApiUrl, cbErr)
				continue
			}
		} else {
			success, failure = ep.BroadcastCtx(ctx, tx)
		}
		if success != nil {
			return nil
		}
		if failure == nil {
			lastTransportErr = fmt.Errorf("%s: empty broadcaster response", ep.ApiUrl)
			continue
		}
		// Treat idempotent "already known" responses as broadcast success.
		if nwallet.IsAlreadyKnownBroadcastError(errors.New(failure.Description)) {
			return nil
		}
		// Teranode/BSV Association Arcade emits newer statuses (notably SEEN_MULTIPLE_NODES)
		// that go-sdk's ArcStatus switch maps to ERR_INVALID_RESPONSE ("unknown transaction status").
		// Resolve the authoritative status directly from GET /tx/{txid} on the same endpoint so
		// propagated transactions (especially incoming BRC-29 re-broadcasts) never stall in "sending".
		if strings.TrimSpace(failure.Code) == "ERR_INVALID_RESPONSE" {
			if st, extra, ok := b.queryEndpointStatus(ctx, ep, tx.TxID().String()); ok {
				switch strings.ToUpper(st) {
				case "SEEN_MULTIPLE_NODES", "SEEN_ON_NETWORK", "ACCEPTED_BY_NETWORK",
					"MINED", "CONFIRMED", "IMMUTABLE", "SUCCESS",
					"RECEIVED", "STORED", "QUEUED", "ANNOUNCED_TO_NETWORK", "REQUESTED_BY_NETWORK", "SENT_TO_NETWORK":
					return nil
				case "REJECTED", "DOUBLE_SPEND_ATTEMPTED", "INVALID", "MALFORMED":
					if extra == "" {
						extra = failure.Description
					}
					return fmt.Errorf("broadcaster %s refused tx %s [%s]: %s", ep.ApiUrl, tx.TxID().String(), st, extra)
				}
			}
		}
		if isRetryableBroadcasterFailure(failure) {
			lastTransportErr = fmt.Errorf("%s [%s]: %s", ep.ApiUrl, failure.Code, failure.Description)
			continue
		}
		// Deterministic tx-level refusal (4xx / REJECTED / DOUBLE_SPEND_ATTEMPTED / mismatch):
		// stop immediately without spamming failover endpoints.
		return fmt.Errorf("broadcaster %s refused tx %s [%s]: %s", ep.ApiUrl, tx.TxID().String(), failure.Code, failure.Description)
	}
	if lastTransportErr != nil {
		return fmt.Errorf("%w: all BSV Broadcaster endpoints failed (last: %v)", nwallet.ErrBroadcastUnknown, lastTransportErr)
	}
	return fmt.Errorf("%w: broadcast failed", nwallet.ErrBroadcastUnknown)
}

// BroadcastBatch submits up to len(txs) transactions in a single ARC POST /txs batch HTTP request
// (100-400x fewer HTTP roundtrips at 1,000 TPS), automatically falling back to bounded concurrent
// Broadcast calls if an endpoint does not expose /txs (HTTP 404/405).
func (b *BSVBroadcaster) BroadcastBatch(ctx context.Context, txs []*transaction.Transaction) []error {
	errs := make([]error, len(txs))
	if len(txs) == 0 {
		return errs
	}
	if len(txs) == 1 {
		errs[0] = b.Broadcast(ctx, txs[0])
		return errs
	}
	n := len(b.endpoints)
	if n == 0 {
		for i := range errs {
			errs[i] = errors.New("chain: no BSV Broadcaster endpoints configured")
		}
		return errs
	}
	type arcBatchReq struct {
		RawTx string `json:"rawTx"`
	}
	type arcBatchItemResp struct {
		Status      int    `json:"status"`
		TxID        string `json:"txid"`
		TxStatus    string `json:"txStatus"`
		Title       string `json:"title"`
		Detail      string `json:"detail"`
		ExtraInfo   string `json:"extraInfo"`
	}
	reqItems := make([]arcBatchReq, len(txs))
	for i, tx := range txs {
		if tx != nil {
			reqItems[i] = arcBatchReq{RawTx: tx.Hex()}
		}
	}
	payload, mErr := json.Marshal(reqItems)
	if mErr == nil {
		start := int((b.nextIdx.Add(1) - 1) % uint64(n))
		for attempt := 0; attempt < n; attempt++ {
			idx := (start + attempt) % n
			if idx < len(b.breakers) && b.breakers[idx].State() == gobreaker.StateOpen {
				continue
			}
			ep := b.endpoints[idx]
			reqURL := strings.TrimRight(ep.ApiUrl, "/") + "/txs"
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, bytes.NewReader(payload))
			if rErr != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept", "application/json")
			if ep.ApiKey != "" {
				req.Header.Set("Authorization", "Bearer "+ep.ApiKey)
			}
			cli := ep.Client
			if cli == nil {
				cli = http.DefaultClient
			}
			resp, dErr := cli.Do(req)
			if dErr != nil {
				continue
			}
			body, bErr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
			_ = resp.Body.Close()
			if bErr != nil {
				continue
			}
			if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
				// Endpoint only implements single /tx; break out to bounded parallel Broadcast.
				break
			}
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				var items []arcBatchItemResp
				if json.Unmarshal(body, &items) == nil && len(items) == len(txs) {
					for i, it := range items {
						st := strings.ToUpper(strings.TrimSpace(it.TxStatus))
						switch st {
						case "SEEN_MULTIPLE_NODES", "SEEN_ON_NETWORK", "ACCEPTED_BY_NETWORK",
							"MINED", "CONFIRMED", "IMMUTABLE", "SUCCESS",
							"RECEIVED", "STORED", "QUEUED", "ANNOUNCED_TO_NETWORK", "REQUESTED_BY_NETWORK", "SENT_TO_NETWORK":
							errs[i] = nil
						default:
							if it.Status >= 200 && it.Status < 300 && st == "" {
								errs[i] = nil
							} else if nwallet.IsAlreadyKnownBroadcastError(errors.New(it.Detail + " " + it.ExtraInfo)) {
								errs[i] = nil
							} else {
								errs[i] = fmt.Errorf("broadcaster %s refused batch tx %d [%s]: %s %s", ep.ApiUrl, i, st, it.Title, it.Detail)
							}
						}
					}
					return errs
				}
			}
		}
	}

	// Fallback: bounded parallel per-tx Broadcast across HTTP/2 pooled connections.
	var wg sync.WaitGroup
	sem := make(chan struct{}, 32)
	for i, tx := range txs {
		wg.Add(1)
		go func(idx int, t *transaction.Transaction) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				errs[idx] = fmt.Errorf("%w: %w", nwallet.ErrBroadcastUnknown, ctx.Err())
				return
			}
			errs[idx] = b.Broadcast(ctx, t)
		}(i, tx)
	}
	wg.Wait()
	return errs
}

type arcStatusPayload struct {
	TxID        string `json:"txid"`
	MerklePath  string `json:"merklePath"`
	BlockHeight uint32 `json:"blockHeight"`
	TxStatus    string `json:"txStatus"`
	ExtraInfo   string `json:"extraInfo"`
}

func (b *BSVBroadcaster) queryEndpointStatus(ctx context.Context, ep *broadcaster.Arc, txid string) (string, string, bool) {
	if ep == nil || txid == "" || ctx.Err() != nil {
		return "", "", false
	}
	reqURL := strings.TrimRight(ep.ApiUrl, "/") + "/tx/" + url.PathEscape(txid)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return "", "", false
	}
	req.Header.Set("Accept", "application/json")
	if ep.ApiKey != "" {
		req.Header.Set("Authorization", "Bearer "+ep.ApiKey)
	}
	cli := ep.Client
	if cli == nil {
		cli = http.DefaultClient
	}
	resp, err := cli.Do(req)
	if err != nil {
		return "", "", false
	}
	body, rErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	_ = resp.Body.Close()
	if rErr != nil || resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", false
	}
	var st arcStatusPayload
	if json.Unmarshal(body, &st) != nil {
		return "", "", false
	}
	if st.TxID != "" && !strings.EqualFold(strings.TrimSpace(st.TxID), txid) {
		return "", "", false
	}
	status := strings.TrimSpace(st.TxStatus)
	if status == "" {
		return "", "", false
	}
	return status, strings.TrimSpace(st.ExtraInfo), true
}

// Proof queries configured BSV Broadcaster endpoints (ctx-aware GET /tx/{txid}) for a mined
// BUMP MerklePath. HTTP 404 or a valid JSON status with empty merklePath counts as an
// authoritative not-mined answer so fallback Chain.Proof is never poisoned by lastErr.
func (b *BSVBroadcaster) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if ctx.Err() != nil {
		return nil, 0, false, ctx.Err()
	}
	var lastErr error
	answered := false
	for _, ep := range b.endpoints {
		if ep == nil {
			continue
		}
		if ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
		reqURL := strings.TrimRight(ep.ApiUrl, "/") + "/tx/" + url.PathEscape(txid)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
		if err != nil {
			lastErr = err
			continue
		}
		req.Header.Set("Accept", "application/json")
		if ep.ApiKey != "" {
			req.Header.Set("Authorization", "Bearer "+ep.ApiKey)
		}
		cli := ep.Client
		if cli == nil {
			cli = http.DefaultClient
		}
		resp, err := cli.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		_ = resp.Body.Close()
		if rErr != nil {
			lastErr = rErr
			continue
		}
		if resp.StatusCode == http.StatusNotFound {
			answered = true
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			lastErr = fmt.Errorf("broadcaster %s status HTTP %d for %s", ep.ApiUrl, resp.StatusCode, txid)
			continue
		}
		var st arcStatusPayload
		if jErr := json.Unmarshal(body, &st); jErr != nil {
			lastErr = fmt.Errorf("broadcaster %s decode status for %s: %w", ep.ApiUrl, txid, jErr)
			continue
		}
		answered = true
		if strings.TrimSpace(st.MerklePath) == "" {
			continue
		}
		mp, pErr := transaction.NewMerklePathFromHex(st.MerklePath)
		if pErr != nil {
			lastErr = fmt.Errorf("broadcaster %s invalid merklePath for %s: %w", ep.ApiUrl, txid, pErr)
			continue
		}
		h := st.BlockHeight
		if h == 0 {
			h = mp.BlockHeight
		}
		return mp, h, true, nil
	}
	if answered {
		return nil, 0, false, nil
	}
	return nil, 0, false, lastErr
}

func isRetryableBroadcasterFailure(f *transaction.BroadcastFailure) bool {
	if f == nil {
		return true
	}
	switch strings.TrimSpace(f.Code) {
	case "429", "500", "502", "503", "504", "ERR_INVALID_RESPONSE":
		return true
	}
	return nwallet.IsUncertainBroadcastError(errors.New(f.Description))
}
