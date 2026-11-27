package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestProxyStrictGroundingGateStripsHallucinationsAndEmitsReceipt(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	defer port.Close()
	mem, err := memory.New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), memory.Config{
		VerifyBaseURL: "http://127.0.0.1:18420/v1/verify",
	})
	if err != nil {
		t.Fatal(err)
	}

	r1, err := mem.Remember(ctx, memory.RememberInput{
		Shard:      "ops",
		Text:       "Chronicle activated cleanly at block height 943816.",
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Mock upstream LLM returning ONE grounded cited sentence AND ONE ungrounded hallucination!
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role": "assistant",
					"content": "Chronicle activated cleanly at block height 943816. [ops:" +
						itoa64(r1.Seq) + "]\nThe moon is made of green cheddar cheese.",
				},
			}},
		})
	}))
	defer upstream.Close()

	app, err := NewApp(mem, nil, Config{
		OpenAIUpstream: upstream.URL,
		HTTPClient:     upstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"When did Chronicle activate?"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+"/v1/proxy/openai/v1/chat/completions", bytes.NewBufferString(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nektar-Shard", "ops")
	req.Header.Set("X-Nektar-Grounding", "strict")
	req.Header.Set("X-Nektar-SPV", "required")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d", resp.StatusCode)
	}
	rcpTok := resp.Header.Get("X-Nektar-Receipt")
	if !strings.HasPrefix(rcpTok, "nektarr1.") {
		t.Fatalf("expected real nektarr1.* receipt header, got %q", rcpTok)
	}
	if resp.Header.Get("X-Nektar-Grounded") != "true" {
		t.Fatalf("expected X-Nektar-Grounded: true, got %q", resp.Header.Get("X-Nektar-Grounded"))
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	gotText := parsed.Choices[0].Message.Content
	if strings.Contains(gotText, "cheddar cheese") {
		t.Fatalf("strict proxy grounding gate FAILED to strip hallucination: %q", gotText)
	}
	if !strings.Contains(gotText, "943816") || !strings.Contains(gotText, "receipt: "+rcpTok) {
		t.Fatalf("expected grounded answer + real receipt footer, got: %q", gotText)
	}
	ver, err := mem.VerifyReceipt(ctx, rcpTok)
	if err != nil || !ver.OK || ver.Verdict != "green" {
		t.Fatalf("proxy-emitted receipt failed verification: err=%v ver=%+v", err, ver)
	}

	// Second request where model emits pure hallucination -> MUST return honest ungrounded notice with ZERO receipt!
	hallucUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-halluc",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{
					"role":    "assistant",
					"content": "Unicorns mine 9000 BTC per second on Mars.",
				},
			}},
		})
	}))
	defer hallucUpstream.Close()

	app2, err := NewApp(mem, nil, Config{
		OpenAIUpstream: hallucUpstream.URL,
		HTTPClient:     hallucUpstream.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ts2 := httptest.NewServer(app2.Handler())
	defer ts2.Close()

	req2, _ := http.NewRequestWithContext(ctx, http.MethodPost, ts2.URL+"/v1/proxy/openai/v1/chat/completions", bytes.NewBufferString(reqBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Nektar-Shard", "ops")
	req2.Header.Set("X-Nektar-Grounding", "strict")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.Header.Get("X-Nektar-Receipt") != "" || resp2.Header.Get("X-Nektar-Grounded") != "false" {
		t.Fatalf("hallucinated response MUST NOT emit receipt header: receipt=%q grounded=%q",
			resp2.Header.Get("X-Nektar-Receipt"), resp2.Header.Get("X-Nektar-Grounded"))
	}
	var parsed2 struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&parsed2)
	if strings.Contains(parsed2.Choices[0].Message.Content, "Unicorns") || strings.Contains(parsed2.Choices[0].Message.Content, "nektarr1.") {
		t.Fatalf("honesty violation on ungrounded proxy response: %q", parsed2.Choices[0].Message.Content)
	}
}

func itoa64(n int64) string {
	return json.Number(strings.TrimSpace(string(mustJSON(n)))).String()
}

func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
