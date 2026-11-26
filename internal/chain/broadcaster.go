package chain

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/bsv-blockchain/go-sdk/transaction/broadcaster"
	"github.com/lightwebinc/bcommon/nodeapi"

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
// client (with multi-endpoint round-robin + failover across BSV Association BSV Broadcaster
// or self-hosted bsv-blockchain/broadcaster instances).
type BSVBroadcaster struct {
	endpoints []*broadcaster.Arc
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
	return &BSVBroadcaster{
		endpoints: endpoints,
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

// URLs returns the configured BSV Broadcaster base URLs.
func (b *BSVBroadcaster) URLs() []string {
	out := make([]string, len(b.urls))
	copy(out, b.urls)
	return out
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
		ep := b.endpoints[(start+i)%n]
		success, failure := ep.BroadcastCtx(ctx, tx)
		if success != nil {
			return nil
		}
		if failure == nil {
			lastTransportErr = fmt.Errorf("%s: empty broadcaster response", ep.ApiUrl)
			continue
		}
		// Treat idempotent "already known" responses as broadcast success.
		descLower := strings.ToLower(failure.Description)
		if strings.Contains(descLower, "already in mempool") ||
			strings.Contains(descLower, "already known") ||
			strings.Contains(descLower, "txn-already-known") {
			return nil
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

// Proof queries configured BSV Broadcaster endpoints (GET /tx/{txid}) via go-sdk broadcaster.Arc.Status
// for a mined BUMP MerklePath. Returns (nil, 0, false, nil) when the tx is unmined or unknown.
func (b *BSVBroadcaster) Proof(ctx context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
	if ctx.Err() != nil {
		return nil, 0, false, ctx.Err()
	}
	var lastErr error
	answered := false
	for _, ep := range b.endpoints {
		if ctx.Err() != nil {
			return nil, 0, false, ctx.Err()
		}
		resp, err := ep.Status(txid)
		if err != nil {
			// 404 or non-JSON unknown responses are treated as not-found on this endpoint;
			// record error only if every endpoint fails to answer cleanly.
			lastErr = err
			continue
		}
		answered = true
		if resp == nil || strings.TrimSpace(resp.MerklePath) == "" {
			continue
		}
		mp, pErr := transaction.NewMerklePathFromHex(resp.MerklePath)
		if pErr != nil {
			lastErr = fmt.Errorf("broadcaster %s invalid merklePath for %s: %w", ep.ApiUrl, txid, pErr)
			continue
		}
		h := resp.BlockHeight
		if h == 0 {
			h = mp.BlockHeight
		}
		return mp, h, true, nil
	}
	if answered {
		return nil, 0, false, nil
	}
	// If every endpoint returned a 404/non-JSON body rather than a real network outage, don't block fallback to Chain.Proof.
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
	low := strings.ToLower(f.Description)
	for _, tok := range []string{
		"timeout", "deadline exceeded", "eof", "connection reset", "connection refused",
		"broken pipe", "temporary failure", "too many requests", "service unavailable",
		"bad gateway", "gateway timeout", "context canceled",
	} {
		if strings.Contains(low, tok) {
			return true
		}
	}
	return false
}
