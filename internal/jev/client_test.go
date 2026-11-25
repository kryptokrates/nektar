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

func TestClientTypeSafeAndOpenRouterDialects(t *testing.T) {
	ctx := context.Background()

	// 1. TypeSafe direct dialect: `noul` field + array `probabilities` on score.
	var calls atomic.Int32
	tsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Authorization") != "Bearer ts-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/systemone" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-1.14.0",
			"request_id": "req-1",
			"answers": {
				"best": {"type": "choice", "choice": "o1", "confidence": 0.91, "probabilities": {"none": 0.02, "o0": 0.07, "o1": 0.91}},
				"rel":  {"type": "score", "score": 3.4, "confidence": 0.88, "probabilities": [0.0, 0.0, 0.05, 0.5, 0.45]},
				"fit":  {"type": "noul", "noul": 0.95}
			},
			"usage": {"input_tokens": 180, "output_tokens": 0}
		}`))
	}))
	defer tsSrv.Close()

	cl, err := NewClient(Config{
		Provider: ProviderTypeSafe,
		BaseURL:  tsSrv.URL,
		APIKey:   "ts-secret",
		Network:  "memory",
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
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

	// 2. OpenRouter dialect: `probability` field on noul + `/api/alpha/decisions`.
	orSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/alpha/decisions" {
			t.Errorf("unexpected openrouter path %q", r.URL.Path)
		}
		var got Request
		_ = json.NewDecoder(r.Body).Decode(&got)
		if got.Model != DefaultOpenRouterModel {
			t.Errorf("expected default openrouter model %q, got %q", DefaultOpenRouterModel, got.Model)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "typesafe/jev-1.14-20261014",
			"provider": "TypeSafe",
			"answers": {
				"safe": {"type": "noul", "probability": 0.87, "confidence": 0.9}
			},
			"usage": {"input_tokens": 90, "output_tokens": 0}
		}`))
	}))
	defer orSrv.Close()

	orCl, err := NewClient(Config{
		Provider: ProviderOpenRouter,
		BaseURL:  orSrv.URL,
		APIKey:   "or-secret",
		Network:  "memory",
	})
	if err != nil {
		t.Fatalf("NewClient openrouter: %v", err)
	}
	orResp, err := orCl.Evaluate(ctx, Request{
		State: "test",
		Questions: map[string]Question{
			"safe": {Type: TypeNoul, Instructions: "safe?"},
		},
	})
	if err != nil {
		t.Fatalf("OpenRouter Evaluate: %v", err)
	}
	if p, ok := orResp.Answers["safe"].NoulProb(); !ok || p != 0.87 {
		t.Fatalf("expected normalized OpenRouter noul prob 0.87, got %v (ok=%v)", p, ok)
	}
}

func TestClientValidationOutOfSetCircuitBreakerAndHostAllowlist(t *testing.T) {
	ctx := context.Background()

	// Out-of-set choice must be rejected.
	badSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"model": "jev-latest",
			"answers": {"pick": {"type": "choice", "choice": "invented_option", "confidence": 0.99}},
			"usage": {"input_tokens": 50}
		}`))
	}))
	defer badSrv.Close()

	clBad, err := NewClient(Config{BaseURL: badSrv.URL, APIKey: "k", Network: "memory"})
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
	clBad.SetAllowedHosts([]string{"api.typesafe.ai"})
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
		_, _ = w.Write([]byte(`{"model":"jev-latest","answers":{"q":{"type":"noul","noul":0.8}},"usage":{"input_tokens":20}}`))
	}))
	defer flakySrv.Close()

	clFlaky, err := NewClient(Config{
		BaseURL:          flakySrv.URL,
		APIKey:           "k",
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

	// Mainnet must refuse http:// and private loopback endpoints!
	if _, err := NewClient(Config{
		BaseURL: "http://127.0.0.1:9999/v1/systemone",
		APIKey:  "k",
		Network: "main",
	}); err == nil {
		t.Fatal("expected mainnet Jev client to block non-HTTPS / loopback endpoint")
	}
}
