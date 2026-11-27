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

// expectedManifest lists every service + handler Nektar v0.1 requires on a registered deployment.
var expectedManifest = map[string][]string{
	"BuyerDeal":      {"deliverKey", "run", "status"},
	"SellerDeal":     {"confirmPayment", "deliverPayment", "run", "status"},
	"Custody":        {"balance", "release", "reserve"},
	"Tx":             {"emit", "state"},
	"AnchorCadence":  {"status", "trigger"},
	"NektarRuntime":  {"anchorStatus", "emitTx", "triggerAnchor", "txState"},
	"StandingOrder":  {"cancel", "onOffer", "run", "status", "steer"},
	"EscrowGuardian": {"markClaimed", "run", "status"},
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

// ResumeInvocation resumes a paused Restate invocation via Admin API PATCH /invocations/{id}/resume.
// Pairs with PauseOnMaxAttempts() so transient outages (e.g. closed BSV Desktop) never destroy in-flight deals.
func (c *RestateClient) ResumeInvocation(ctx context.Context, invocationID string) error {
	u := c.AdminURL + "/invocations/" + url.PathEscape(strings.TrimSpace(invocationID)) + "/resume"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: resume invocation %s: %w", invocationID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: resume invocation HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// RestartInvocationAsNew restarts a paused/failed Restate invocation as a fresh invocation via Admin API
// PATCH /invocations/{id}/restart-as-new and returns the new invocation ID if provided.
func (c *RestateClient) RestartInvocationAsNew(ctx context.Context, invocationID string) (string, error) {
	u := c.AdminURL + "/invocations/" + url.PathEscape(strings.TrimSpace(invocationID)) + "/restart-as-new"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("restate: restart invocation %s: %w", invocationID, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("restate: restart invocation HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var parsed struct {
		NewInvocationID string `json:"new_invocation_id"`
		ID              string `json:"id"`
	}
	_ = json.Unmarshal(raw, &parsed)
	if parsed.NewInvocationID != "" {
		return parsed.NewInvocationID, nil
	}
	return parsed.ID, nil
}

// KillInvocation terminates a stuck invocation via Restate Admin API PATCH /invocations/{id}/kill.
func (c *RestateClient) KillInvocation(ctx context.Context, invocationID string) error {
	u := c.AdminURL + "/invocations/" + url.PathEscape(strings.TrimSpace(invocationID)) + "/kill"
	req, err := http.NewRequestWithContext(ctx, http.MethodPatch, u, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("restate: kill invocation %s: %w", invocationID, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return fmt.Errorf("restate: kill invocation HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
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

// TriggerAnchorCadence pokes the private AnchorCadence VO asynchronously with a short 5-second
// ingress dedup bucket PLUS a coalesced 5-second trailing-edge slot so rapid bursts coalesce
// without dropping tail writes that arrive right after a pass drains ListUnanchored!
func (c *RestateClient) TriggerAnchorCadence(ctx context.Context, shard string, intervalSec int64) (string, error) {
	dedupSlot := (time.Now().Unix() / 5) * 5
	opts := []ingress.SendOption{
		restate.WithIdempotencyKey(fmt.Sprintf("rt-anchor:%s:%d", shard, dedupSlot)),
	}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	sr, err := ingress.ServiceSend[RuntimeAnchorTriggerInput](c.Ingress, "NektarRuntime", "triggerAnchor").
		Send(ctx, RuntimeAnchorTriggerInput{Shard: shard, IntervalSeconds: intervalSec, Slot: dedupSlot}, opts...)
	if err != nil {
		return "", fmt.Errorf("restate: triggerAnchor %s: %w", shard, err)
	}
	// Schedule one coalesced trailing-edge check in the next 5s bucket to catch writes that raced an active pass.
	nextSlot := dedupSlot + 5
	trailOpts := []ingress.SendOption{
		restate.WithIdempotencyKey(fmt.Sprintf("rt-anchor:%s:%d", shard, nextSlot)),
		restate.WithDelay(5 * time.Second),
	}
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		trailOpts = append(trailOpts, restate.WithHeaders(hdrs))
	}
	_, _ = ingress.ServiceSend[RuntimeAnchorTriggerInput](c.Ingress, "NektarRuntime", "triggerAnchor").
		Send(ctx, RuntimeAnchorTriggerInput{Shard: shard, IntervalSeconds: intervalSec, Slot: nextSlot}, trailOpts...)
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

// QueryInvocations queries live Restate invocations from the Admin SQL API (`POST /query` on `sys_invocation`)
// so the Dashboard reports real executor telemetry instead of faking SQLite deals as Restate journal (F26).
func (c *RestateClient) QueryInvocations(ctx context.Context, limit int) ([]RestateInvocation, error) {
	if limit <= 0 {
		limit = 25
	}
	if limit > 200 {
		limit = 200
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
