package jev

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientOpenWeightDialectsAndNoAPIKey(t *testing.T) {
	ctx := context.Background()

	// 1. Default open-weight System One dialect (`noul` + array `probabilities`), ZERO Authorization header!
	var calls atomic.Int32
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("expected keyless open-weight request (no Authorization header), got %q", auth)
		}
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		var got Request
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Model != DefaultModel {
			t.Errorf("expected default open-weight model %q, got %q", DefaultModel, got.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "kev-0.8b",
			"request_id": "req-1",
			"answers": {
				"best": {"type": "choice", "choice": "o1", "confidence": 0.91, "probabilities": {"none": 0.02, "o0": 0.07, "o1": 0.91}},
				"rel":  {"type": "score", "score": 3.4, "confidence": 0.88, "probabilities": [0.0, 0.0, 0.05, 0.5, 0.45]},
				"fit":  {"type": "noul", "noul": 0.95}
			},
			"usage": {"input_tokens": 180, "output_tokens": 0}
		}`))
	}))
	defer localSrv.Close()

	// Loopback open-weight sidecar MUST work even when Network is "main"!
	cl, err := NewClient(Config{
		BaseURL: localSrv.URL,
		Network: "main",
	})
	if err != nil {
		t.Fatalf("NewClient (loopback on main): %v", err)
	}
	if cl.Provider() != ProviderOpenWeight {
		t.Fatalf("expected provider %q, got %q", ProviderOpenWeight, cl.Provider())
	}
	req := Request{
		State: map[string]any{"want": "fix teranode spend"},
		Questions: map[string]Question{
			"best": {Type: TypeChoice, Instructions: "pick best", Criteria: map[string]string{"none": "none", "o0": "a", "o1": "b"}},
			"rel":  {Type: TypeScore, Instructions: "rate", Criteria: []string{"0", "1", "2", "3", "4"}},
			"fit":  {Type: TypeNoul, Instructions: "fits policy?"},
		},
	}
	resp, err := cl.Evaluate(ctx, req)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if resp.Answers["best"].Choice != "o1" {
		t.Fatalf("expected choice o1, got %+v", resp.Answers["best"])
	}
	if p, ok := resp.Answers["fit"].NoulProb(); !ok || p != 0.95 {
		t.Fatalf("expected noul 0.95, got %v (ok=%v)", p, ok)
	}
	if sc, ok := resp.Answers["rel"].ScoreValue(); !ok || sc != 3.4 {
		t.Fatalf("expected score 3.4, got %v (ok=%v)", sc, ok)
	}
	if len(resp.Answers["rel"].Probabilities) != 5 || resp.Answers["rel"].Probabilities["4"] != 0.45 {
		t.Fatalf("expected array probabilities normalized to map, got %+v", resp.Answers["rel"].Probabilities)
	}

	// Identical call hits TTL cache (0 additional HTTP requests).
	resp2, err := cl.Evaluate(ctx, req)
	if err != nil || resp2.Answers["best"].Choice != "o1" {
		t.Fatalf("cached Evaluate failed: resp=%+v err=%v", resp2, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected cache hit (1 upstream call), got %d", calls.Load())
	}

	// 2. Alternate open-weight response dialect using `probability` on noul + custom model override.
	altSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var got Request
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Model != "laya-multilingual" {
			t.Errorf("expected model laya-multilingual, got %q", got.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "laya-multilingual",
			"answers": {
				"safe": {"type": "noul", "probability": 0.87, "confidence": 0.9}
			},
			"usage": {"input_tokens": 90, "output_tokens": 0}
		}`))
	}))
	defer altSrv.Close()

	altCl, err := NewClient(Config{
		BaseURL: altSrv.URL + "/v1",
		Model:   "laya-multilingual",
		Network: "testnet",
	})
	if err != nil {
		t.Fatalf("NewClient alt: %v", err)
	}
	altResp, err := altCl.Evaluate(ctx, Request{
		State: "test",
		Questions: map[string]Question{
			"safe": {Type: TypeNoul, Instructions: "safe?"},
		},
	})
	if err != nil {
		t.Fatalf("alt Evaluate: %v", err)
	}
	if p, ok := altResp.Answers["safe"].NoulProb(); !ok || p != 0.87 {
		t.Fatalf("expected normalized noul prob 0.87, got %v (ok=%v)", p, ok)
	}

	// 3. Default Config{} (zero env/args) points directly to local open-weight 127.0.0.1:8000/v1/systemone.
	defCl, err := NewClient(Config{Network: "main"})
	if err != nil {
		t.Fatalf("NewClient default: %v", err)
	}
	if defCl.Health().Endpoint != DefaultBaseURL || defCl.Model() != DefaultModel {
		t.Fatalf("unexpected defaults: endpoint=%s model=%s", defCl.Health().Endpoint, defCl.Model())
	}
}

func TestClientValidationHostedRefusalCircuitBreakerAndSSRF(t *testing.T) {
	ctx := context.Background()

	// Hosted third-party Jev cloud endpoints MUST be rejected outright!
	for _, blocked := range []string{
		"https://api.typesafe.ai/v1/systemone",
		"https://openrouter.ai/api/alpha/decisions",
	} {
		if _, err := NewClient(Config{BaseURL: blocked, Network: "main"}); !errors.Is(err, ErrHostedProviderDisallowed) {
			t.Fatalf("expected ErrHostedProviderDisallowed for %s, got %v", blocked, err)
		}
	}

	// Out-of-set choice must be rejected.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "kev-0.8b",
			"answers": {"pick": {"type": "choice", "choice": "invented_option", "confidence": 0.99}},
			"usage": {"input_tokens": 50}
		}`))
	}))
	defer badSrv.Close()

	clBad, err := NewClient(Config{BaseURL: badSrv.URL, Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := clBad.Evaluate(ctx, Request{
		State: "s",
		Questions: map[string]Question{
			"pick": {Type: TypeChoice, Instructions: "pick", Criteria: map[string]string{"a": "opt a", "b": "opt b"}},
		},
	}); err == nil {
		t.Fatal("expected out-of-set choice to fail validation")
	}

	// Host allowlist enforcement (policy egress pinning).
	clBad.SetAllowedHosts([]string{"jev.hub.example"})
	if _, err := clBad.Evaluate(ctx, Request{
		State:     "s",
		Questions: map[string]Question{"q": {Type: TypeNoul, Instructions: "?"}},
	}); !errors.Is(err, ErrHostNotAllowed) {
		t.Fatalf("expected ErrHostNotAllowed, got %v", err)
	}

	// Circuit breaker opens after threshold 5xx failures and recovers after cooldown.
	now := time.Date(2026, 11, 25, 12, 0, 0, 0, time.UTC)
	failMode := true
	flakySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if failMode {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`upstream down`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"model":"kev-0.8b","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":20}}`))
	}))
	defer flakySrv.Close()

	clFlaky, err := NewClient(Config{
		BaseURL:          flakySrv.URL,
		Network:          "memory",
		CircuitThreshold: 2,
		CircuitCooldown:  15 * time.Second,
		Now:              func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	qReq := Request{State: "x", Questions: map[string]Question{"q": {Type: TypeNoul, Instructions: "?"}}}
	_, _ = clFlaky.Evaluate(ctx, qReq)
	_, _ = clFlaky.Evaluate(ctx, qReq)
	if _, err := clFlaky.Evaluate(ctx, qReq); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen on 3rd call, got %v", err)
	}
	if !clFlaky.Health().CircuitOpen {
		t.Fatal("expected Health().CircuitOpen == true")
	}
	// Advance clock past cooldown and recover.
	now = now.Add(20 * time.Second)
	failMode = false
	if _, err := clFlaky.Evaluate(ctx, qReq); err != nil {
		t.Fatalf("expected recovery after cooldown, got %v", err)
	}
	if clFlaky.Health().CircuitOpen {
		t.Fatal("expected circuit breaker closed after recovery")
	}

	// Mainnet still blocks non-HTTPS remote endpoints AND private/cloud-metadata IPs!
	for _, bad := range []string{
		"http://jev.remote-hub.example/v1/systemone",
		"https://169.254.169.254/v1/systemone",
		"http://10.0.0.15:8000/v1/systemone",
	} {
		if _, err := NewClient(Config{BaseURL: bad, Network: "main"}); err == nil {
			t.Fatalf("expected mainnet client to block %s", bad)
		}
	}
}
