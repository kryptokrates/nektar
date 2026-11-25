package market

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"
)

// RestateInvocation summarizes a live Restate invocation queried from sys_invocation.
type RestateInvocation struct {
	ID             string `json:"id"`
	Target         string `json:"target"`
	Status         string `json:"status"`
	RetryCount     int64  `json:"retryCount"`
	NextRetryAt    int64  `json:"nextRetryAt,omitempty"`
	ModifiedAt     int64  `json:"modifiedAt,omitempty"`
	LastFailureErr string `json:"lastFailureError,omitempty"`
}

// RestateClient is the pure-Go Restate Ingress (`sdk-go/ingress`) + Admin HTTP client
// driving Nektar's active durable execution plane.
type RestateClient struct {
	IngressURL    string
	AdminURL      string
	InternalToken string
	HTTP          *http.Client
	Ingress       *ingress.Client
}

// NewRestateClient constructs a Restate client backed by official restatedev/sdk-go/ingress
// pointing at ingressURL (e.g. http://127.0.0.1:8111) and adminURL (e.g. http://127.0.0.1:9070).
func NewRestateClient(ingressURL, adminURL string) *RestateClient {
	return NewRestateClientWithToken(ingressURL, adminURL, "")
}

// NewRestateClientWithToken constructs a RestateClient sending an optional internal token on NektarRuntime calls.
func NewRestateClientWithToken(ingressURL, adminURL, internalToken string) *RestateClient {
	if ingressURL == "" {
		ingressURL = "http://127.0.0.1:8080"
	}
	if adminURL == "" {
		adminURL = "http://127.0.0.1:9070"
	}
	ingressURL = strings.TrimRight(ingressURL, "/")
	adminURL = strings.TrimRight(adminURL, "/")
	hc := &http.Client{Timeout: 30 * time.Second}
	ic := ingress.NewClient(ingressURL, ingress.WithHttpClient(hc))
	return &RestateClient{
		IngressURL:    ingressURL,
		AdminURL:      adminURL,
		InternalToken: strings.TrimSpace(internalToken),
		HTTP:          hc,
		Ingress:       ic,
	}
}

func (c *RestateClient) runtimeHeaders() map[string]string {
	if c.InternalToken == "" {
		return nil
	}
	return map[string]string{internalTokenHeader: c.InternalToken}
}

// HealthCheck probes GET /health on the Restate Admin API.
func (c *RestateClient) HealthCheck(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/health", nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("restate admin /health HTTP %d", resp.StatusCode)
	}
	return nil
}

// expectedManifest lists every service + handler Nektar v0.4 requires on a registered deployment.
var expectedManifest = map[string][]string{
	"BuyerDeal":     {"deliverKey", "run", "status"},
	"SellerDeal":    {"confirmPayment", "deliverPayment", "run", "status"},
	"Custody":       {"balance", "release", "reserve"},
	"Tx":            {"emit", "state"},
	"AnchorCadence": {"status", "trigger"},
	"NektarRuntime": {"anchorStatus", "emitTx", "triggerAnchor", "txState"},
}

type adminDeployment struct {
	ID       string `json:"id"`
	URI      string `json:"uri"`
	Services []struct {
		Name     string `json:"name"`
		Handlers []struct {
			Name string `json:"name"`
		} `json:"handlers"`
	} `json:"services"`
}

func deploymentMatchesExpected(dep adminDeployment) bool {
	if len(dep.Services) < len(expectedManifest) {
		return false
	}
	got := make(map[string]map[string]bool, len(dep.Services))
	for _, s := range dep.Services {
		hmap := make(map[string]bool, len(s.Handlers))
		for _, h := range s.Handlers {
			hmap[h.Name] = true
		}
		got[s.Name] = hmap
	}
	for svc, wantHandlers := range expectedManifest {
		hmap, ok := got[svc]
		if !ok {
			return false
		}
		// Some older mock servers omit handler arrays; accept when handlers array is present and complete.
		if len(hmap) > 0 {
			for _, wh := range wantHandlers {
				if !hmap[wh] {
					return false
				}
			}
		}
	}
	return true
}

// EnsureDeployment checks GET /deployments first, compares the full service+handler manifest,
// and registers endpointURI when missing or drifted (never blindly calling force:true on every restart).
func (c *RestateClient) EnsureDeployment(ctx context.Context, endpointURI string, force bool) (changed bool, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.AdminURL+"/deployments", nil)
	if err == nil {
		if resp, dErr := c.HTTP.Do(req); dErr == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var parsed struct {
					Deployments []adminDeployment `json:"deployments"`
				}
				if json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&parsed) == nil {
					target := strings.TrimRight(endpointURI, "/")
					for _, dep := range parsed.Deployments {
						if strings.TrimRight(dep.URI, "/") == target && deploymentMatchesExpected(dep) {
							if !force {
								return false, nil
							}
						}
					}
					// If URI exists with drifted manifest, force-update it!
					for _, dep := range parsed.Deployments {
						if strings.TrimRight(dep.URI, "/") == target && !deploymentMatchesExpected(dep) {
							force = true
							break
						}
					}
				}
			}
		}
	}
	if err := c.RegisterDeployment(ctx, endpointURI, force); err != nil {
		return false, err
	}
	return true, nil
}

// RegisterDeployment registers this Go process's Restate service endpoint URI with the Restate Admin API.
func (c *RestateClient) RegisterDeployment(ctx context.Context, endpointURI string, force bool) error {
	body, _ := json.Marshal(map[string]any{
		"uri":   endpointURI,
		"force": force,
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.AdminURL+"/deployments", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: register deployment %s: %w", endpointURI, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: register deployment HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// StartBuyerDealAsync submits a BuyerDeal workflow asynchronously via sdk-go/ingress (/send) and returns the invocation ID.
// Restate Workflows are inherently idempotent by workflowID (wantID), so no idempotency-key header is attached.
func (c *RestateClient) StartBuyerDealAsync(ctx context.Context, wantID string, in BuyerDealInput) (string, error) {
	sr, err := ingress.WorkflowSend[BuyerDealInput](c.Ingress, "BuyerDeal", wantID, "run").
		Send(ctx, in)
	if err != nil {
		return "", fmt.Errorf("restate: start BuyerDeal %s: %w", wantID, err)
	}
	return sr.Id(), nil
}

// StartBuyerDeal starts a durable BuyerDeal workflow asynchronously and attaches to its result until completion or ctx expiry.
func (c *RestateClient) StartBuyerDeal(ctx context.Context, wantID string, in BuyerDealInput) (*DealState, error) {
	if _, err := c.StartBuyerDealAsync(ctx, wantID, in); err != nil {
		return nil, err
	}
	return c.AttachBuyerDeal(ctx, wantID)
}

// AttachBuyerDeal attaches to an already-started BuyerDeal workflow via sdk-go/ingress WorkflowHandle.
func (c *RestateClient) AttachBuyerDeal(ctx context.Context, wantID string) (*DealState, error) {
	out, err := ingress.WorkflowHandle[DealState](c.Ingress, "BuyerDeal", wantID).Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("restate: attach BuyerDeal %s: %w", wantID, err)
	}
	return &out, nil
}

// BuyerDealStatus queries the shared status handler of a BuyerDeal workflow.
func (c *RestateClient) BuyerDealStatus(ctx context.Context, wantID string) (*DealState, error) {
	out, err := ingress.Workflow[restate.Void, DealState](c.Ingress, "BuyerDeal", wantID, "status").
		Request(ctx, restate.Void{})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverBuyerKey delivers the revealed BRC-369 content scalar with a content-derived idempotency key.
func (c *RestateClient) DeliverBuyerKey(ctx context.Context, wantID string, sig ClaimSignal) (bool, error) {
	idem := "deliver-key:" + wantID + ":" + sig.ClaimTxID
	if sig.ClaimTxID == "" && len(sig.ContentKeyHex) >= 12 {
		idem = "deliver-key:" + wantID + ":" + sig.ContentKeyHex[:12]
	}
	out, err := ingress.Workflow[ClaimSignal, bool](c.Ingress, "BuyerDeal", wantID, "deliverKey").
		Request(ctx, sig, restate.WithIdempotencyKey(idem))
	if err != nil {
		return false, err
	}
	return out, nil
}

// StartSellerDealAsync submits a SellerDeal workflow asynchronously via sdk-go/ingress (/send) so the producer never blocks!
// Restate Workflows are inherently idempotent by workflowID (offerID), so no idempotency-key header is attached.
func (c *RestateClient) StartSellerDealAsync(ctx context.Context, offerID string, in SellerDealInput) (string, error) {
	sr, err := ingress.WorkflowSend[SellerDealInput](c.Ingress, "SellerDeal", offerID, "run").
		Send(ctx, in)
	if err != nil {
		return "", fmt.Errorf("restate: start SellerDeal %s: %w", offerID, err)
	}
	return sr.Id(), nil
}

// AttachSellerDeal attaches to a SellerDeal workflow result.
func (c *RestateClient) AttachSellerDeal(ctx context.Context, offerID string) (*DealState, error) {
	out, err := ingress.WorkflowHandle[DealState](c.Ingress, "SellerDeal", offerID).Attach(ctx)
	if err != nil {
		return nil, fmt.Errorf("restate: attach SellerDeal %s: %w", offerID, err)
	}
	return &out, nil
}

// SellerDealStatus queries the shared status handler of a SellerDeal workflow.
func (c *RestateClient) SellerDealStatus(ctx context.Context, offerID string) (*DealState, error) {
	out, err := ingress.Workflow[restate.Void, DealState](c.Ingress, "SellerDeal", offerID, "status").
		Request(ctx, restate.Void{})
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// DeliverSellerPayment notifies SellerDeal of buyer payment with a confirmation-aware idempotency key (R9).
func (c *RestateClient) DeliverSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	idem := fmt.Sprintf("deliver-pay:%s:%s:%v", offerID, sig.TxID, sig.Confirmed)
	out, err := ingress.Workflow[PaymentSignal, bool](c.Ingress, "SellerDeal", offerID, "deliverPayment").
		Request(ctx, sig, restate.WithIdempotencyKey(idem))
	if err != nil {
		return false, err
	}
	return out, nil
}

// ConfirmSellerPayment notifies a SellerDeal waiting in payment_pending that the tx has mined!
func (c *RestateClient) ConfirmSellerPayment(ctx context.Context, offerID string, sig PaymentSignal) (bool, error) {
	sig.Confirmed = true
	idem := fmt.Sprintf("confirm-pay:%s:%s", offerID, sig.TxID)
	out, err := ingress.Workflow[PaymentSignal, bool](c.Ingress, "SellerDeal", offerID, "confirmPayment").
		Request(ctx, sig, restate.WithIdempotencyKey(idem))
	if err != nil {
		return false, err
	}
	return out, nil
}

// EmitTxEvent fires a non-blocking durable transaction lifecycle event via NektarRuntime -> private Tx VO (R1, R9).
func (c *RestateClient) EmitTxEvent(ctx context.Context, txid string, ev TxEventInput) (string, error) {
	if ev.ObservedAt == 0 {
		ev.ObservedAt = time.Now().Unix()
	}
	opts := []ingress.SendOption{
		restate.WithIdempotencyKey("rt:" + txEventIdemKey(txid, ev)),
	}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.ServiceSend[RuntimeTxEmitInput](c.Ingress, "NektarRuntime", "emitTx").
		Send(ctx, RuntimeTxEmitInput{TxID: txid, Event: ev}, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: emitTx %s: %w", txid, err)
	}
	return sr.Id(), nil
}

// GetTxState reads the durable state of txid via NektarRuntime -> private Tx VO.
func (c *RestateClient) GetTxState(ctx context.Context, txid string) (*TxState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[string, TxState](c.Ingress, "NektarRuntime", "txState").
		Request(ctx, txid, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// TriggerAnchorCadence pokes the private AnchorCadence VO asynchronously with a quantized slot idempotency key (R6, R9).
func (c *RestateClient) TriggerAnchorCadence(ctx context.Context, shard string, intervalSec int64) (string, error) {
	var slot int64
	if intervalSec > 0 {
		slot = (time.Now().Unix() / intervalSec) * intervalSec
	} else {
		// Short 5-second dedup bucket for immediate on-remember pokes!
		slot = (time.Now().Unix() / 5) * 5
	}
	opts := []ingress.SendOption{
		restate.WithIdempotencyKey(fmt.Sprintf("rt-anchor:%s:%d", shard, slot)),
	}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.ServiceSend[RuntimeAnchorTriggerInput](c.Ingress, "NektarRuntime", "triggerAnchor").
		Send(ctx, RuntimeAnchorTriggerInput{Shard: shard, IntervalSeconds: intervalSec, Slot: slot}, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: triggerAnchor %s: %w", shard, err)
	}
	return sr.Id(), nil
}

// GetAnchorCadenceStatus reads the durable AnchorCadence state for shard via NektarRuntime.
func (c *RestateClient) GetAnchorCadenceStatus(ctx context.Context, shard string) (*AnchorCadenceState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[string, AnchorCadenceState](c.Ingress, "NektarRuntime", "anchorStatus").
		Request(ctx, shard, opts...)
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// ResolveAwakeable resolves a suspended Restate Awakeable by ID via Ingress /restate/awakeables/{id}/resolve.
func (c *RestateClient) ResolveAwakeable(ctx context.Context, awakeableID string, payload any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	u := c.IngressURL + "/restate/awakeables/" + url.PathEscape(awakeableID) + "/resolve"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(raw))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: resolve awakeable %s: %w", awakeableID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: resolve awakeable HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// RejectAwakeable rejects a suspended Restate Awakeable by ID via Ingress /restate/awakeables/{id}/reject.
func (c *RestateClient) RejectAwakeable(ctx context.Context, awakeableID, reason string) error {
	u := c.IngressURL + "/restate/awakeables/" + url.PathEscape(awakeableID) + "/reject"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(reason))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "text/plain")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: reject awakeable %s: %w", awakeableID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: reject awakeable HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// QueryInvocations queries live Restate invocations from the Admin SQL API (`POST /query` on `sys_invocation`)
// so the Dashboard reports real executor telemetry instead of faking SQLite deals as Restate journal (F26).
func (c *RestateClient) QueryInvocations(ctx context.Context, limit int) ([]RestateInvocation, error) {
	if limit <= 0 {
		limit = 25
	}
	qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]any{
		"query": fmt.Sprintf(
			"SELECT id, target, status, retry_count, next_retry_at, modified_at, last_failure FROM sys_invocation ORDER BY modified_at DESC LIMIT %d",
			limit,
		),
	})
	req, err := http.NewRequestWithContext(qCtx, http.MethodPost, c.AdminURL+"/query", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("restate /query HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	var parsed struct {
		Rows []map[string]any `json:"rows"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&parsed); err != nil {
		return nil, err
	}
	out := make([]RestateInvocation, 0, len(parsed.Rows))
	for _, row := range parsed.Rows {
		inv := RestateInvocation{
			ID:             fmtAnyStr(row["id"]),
			Target:         fmtAnyStr(row["target"]),
			Status:         fmtAnyStr(row["status"]),
			RetryCount:     fmtAnyInt64(row["retry_count"]),
			NextRetryAt:    fmtAnyTimestampMs(row["next_retry_at"]),
			ModifiedAt:     fmtAnyTimestampMs(row["modified_at"]),
			LastFailureErr: fmtAnyStr(row["last_failure"]),
		}
		out = append(out, inv)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModifiedAt > out[j].ModifiedAt })
	return out, nil
}

func fmtAnyStr(v any) string {
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprintf("%v", v)
}

func fmtAnyInt64(v any) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int64:
		return n
	case int:
		return int64(n)
	default:
		return 0
	}
}

func fmtAnyTimestampMs(v any) int64 {
	switch t := v.(type) {
	case float64:
		return int64(t)
	case int64:
		return t
	case string:
		if ts, err := time.Parse(time.RFC3339Nano, t); err == nil {
			return ts.UnixMilli()
		}
		if ts, err := time.Parse("2006-01-02 15:04:05.999999999", t); err == nil {
			return ts.UnixMilli()
		}
	}
	return 0
}
