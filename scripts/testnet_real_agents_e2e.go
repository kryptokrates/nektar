//go:build ignore

// Pure-Go heterogeneous real-world agent harness drivers (zero Node.js, zero external deps):
//   - beta     : REAL Claude Code / Codex CLI stdio MCP client (`nektar mcp`) + native `nektar hook`
//   - gamma    : REAL Cursor / OpenAI / Anthropic Streamable HTTP MCP (`/mcp`) + L0 Transparent LLM Proxy
//   - delta    : REAL NAT-only A2A Agent Protocol (`/.well-known/agent.json` + JSON-RPC 2.0 `POST /a2a`)
//   - mock-llm : Local mock upstream LLM server verifying L0 memory injection
package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

func die(msg string) {
	fmt.Fprintln(os.Stderr, msg)
	os.Exit(1)
}

func main() {
	if len(os.Args) < 3 {
		die("usage: go run scripts/testnet_real_agents_e2e.go <mock-llm|beta|gamma|delta> <phase|port> [args...]")
	}
	role := os.Args[1]
	phase := os.Args[2]
	rest := os.Args[3:]

	var err error
	switch role {
	case "mock-llm":
		err = runMockLLM(phase)
	case "beta":
		if len(rest) < 3 {
			die("beta requires <exePath> <dataDir> <runTag>")
		}
		err = runBetaCodexMCP(phase, rest[0], rest[1], rest[2])
	case "gamma":
		if len(rest) < 3 {
			die("gamma requires <baseUrl> <runTag> <peerPubHex>")
		}
		err = runGammaHTTPMCPAndProxy(phase, rest[0], rest[1], rest[2])
	case "delta":
		if len(rest) < 2 {
			die("delta requires <baseUrl> <runTag>")
		}
		err = runDeltaA2ANAT(phase, rest[0], rest[1])
	default:
		die("unknown role: " + role)
	}
	if err != nil {
		die(err.Error())
	}
}

type stdioMCPClient struct {
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	mu      sync.Mutex
	nextID  int
	pending map[int]chan map[string]any
	stderr  bytes.Buffer
}

func newStdioMCPClient(exePath, dataDir string) (*stdioMCPClient, error) {
	cmd := exec.Command(exePath, "mcp", "-data-dir", dataDir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	c := &stdioMCPClient{
		cmd:     cmd,
		stdin:   stdin,
		nextID:  1,
		pending: make(map[int]chan map[string]any),
	}
	cmd.Stderr = &c.stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
		for sc.Scan() {
			line := strings.TrimSpace(sc.Text())
			if line == "" {
				continue
			}
			var msg map[string]any
			if json.Unmarshal([]byte(line), &msg) != nil {
				continue
			}
			if idf, ok := msg["id"].(float64); ok {
				id := int(idf)
				c.mu.Lock()
				ch := c.pending[id]
				delete(c.pending, id)
				c.mu.Unlock()
				if ch != nil {
					ch <- msg
				}
			}
		}
	}()
	return c, nil
}

func (c *stdioMCPClient) request(method string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	id := c.nextID
	c.nextID++
	ch := make(chan map[string]any, 1)
	c.pending[id] = ch
	c.mu.Unlock()

	if params == nil {
		params = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      id,
		"method":  method,
		"params":  params,
	})
	if _, err := c.stdin.Write(append(raw, '\n')); err != nil {
		return nil, err
	}
	select {
	case msg := <-ch:
		if e, ok := msg["error"]; ok && e != nil {
			b, _ := json.Marshal(e)
			return nil, fmt.Errorf("MCP error %s: %s", method, b)
		}
		res, _ := msg["result"].(map[string]any)
		return res, nil
	case <-time.After(45 * time.Second):
		return nil, fmt.Errorf("MCP stdio timeout on %s; stderr=%s", method, c.stderr.String())
	}
}

func (c *stdioMCPClient) callTool(name string, args map[string]any) (map[string]any, error) {
	res, err := c.request("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	if isErr, _ := res["isError"].(bool); isErr {
		b, _ := json.Marshal(res)
		return nil, fmt.Errorf("MCP tool %s error: %s", name, b)
	}
	return parseToolText(res)
}

func (c *stdioMCPClient) close() {
	_ = c.stdin.Close()
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	_ = c.cmd.Wait()
}

func parseToolText(res map[string]any) (map[string]any, error) {
	txt := "{}"
	if content, ok := res["content"].([]any); ok && len(content) > 0 {
		if first, ok := content[0].(map[string]any); ok {
			if s, ok := first["text"].(string); ok && s != "" {
				txt = s
			}
		}
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(txt), &out); err != nil {
		return nil, fmt.Errorf("parse tool JSON %q: %w", txt, err)
	}
	return out, nil
}

func runHook(exePath string, args []string, stdin string) (string, error) {
	cmd := exec.Command(exePath, args...)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var out, errBuf bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errBuf
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("hook %v failed: %v (%s %s)", args, err, out.String(), errBuf.String())
	}
	return out.String(), nil
}

func runBetaCodexMCP(phase, exePath, dataDir, runTag string) error {
	mcp, err := newStdioMCPClient(exePath, dataDir)
	if err != nil {
		return err
	}
	defer mcp.close()

	initRes, err := mcp.request("initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "codex-claude-live-e2e", "version": "1.0.0"},
	})
	if err != nil {
		return err
	}
	toolsRes, err := mcp.request("tools/list", nil)
	if err != nil {
		return err
	}
	rawTools, _ := toolsRes["tools"].([]any)
	has := map[string]bool{}
	for _, t := range rawTools {
		if m, ok := t.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				has[n] = true
			}
		}
	}
	if !has["want"] || !has["sell"] || !has["recall"] {
		return fmt.Errorf("Codex/Claude MCP tools/list missing expected tools")
	}

	if phase == "setup" {
		ss, err := runHook(exePath, []string{"hook", "session-start", "-data-dir", dataDir}, "")
		if err != nil || !strings.Contains(ss, "welcome") {
			return fmt.Errorf("Beta native hook session-start failed: %v %s", err, ss)
		}
		r1, err := mcp.callTool("remember", map[string]any{
			"shard":      "codex-ops",
			"text":       fmt.Sprintf("CodexBeta[%s]: Prod Kubernetes etcd snapshots rotate every 15m using zstd-3 compression.", runTag),
			"source":     "codex:beta",
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		s1, err := mcp.callTool("sell", map[string]any{
			"shard":       "codex-ops",
			"domains":     []string{"ops"},
			"priceSats":   55,
			"description": "Codex Beta cheaper ops runbook",
		})
		if err != nil {
			return err
		}
		r2, err := mcp.callTool("remember", map[string]any{
			"shard":      "codex-bsv",
			"text":       fmt.Sprintf("CodexBeta[%s]: BSV Broadcaster testnet accepts EF transactions at 100 sat/kB.", runTag),
			"source":     "codex:beta",
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		s2, err := mcp.callTool("sell", map[string]any{
			"shard":       "codex-bsv",
			"domains":     []string{"bsv"},
			"priceSats":   65,
			"description": "Codex Beta BSV Arcade policy",
		})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"ok":                 true,
			"serverInfo":         initRes["serverInfo"],
			"mcpToolsCount":      len(rawTools),
			"hookSessionStartOK": true,
			"opsAnchorTxid":      r1["anchorTxid"],
			"bsvAnchorTxid":      r2["anchorTxid"],
			"listings":           []any{s1["shard"], s2["shard"]},
		})
	}

	if phase == "trade" {
		t0 := time.Now()
		deal, err := mcp.callTool("want", map[string]any{
			"domains":     []string{"nat"},
			"question":    "NAT-only A2A agents bbox relay",
			"maxSats":     150,
			"targetShard": "bought/nat",
		})
		if err != nil {
			return err
		}
		totalMs := time.Since(t0).Milliseconds()
		if deal["status"] != "settled" || !strings.Contains(fmt.Sprint(deal["deliveredText"]), "bbox") {
			b, _ := json.Marshal(deal)
			return fmt.Errorf("Codex Beta stdio MCP want failed: %s", b)
		}
		pref, err := runHook(exePath, []string{"hook", "prefetch", "-data-dir", dataDir}, `{"prompt":"bbox"}`)
		if err != nil || !strings.Contains(pref, "bbox") {
			return fmt.Errorf("Beta native hook prefetch failed: %v %s", err, pref)
		}
		rec, err := mcp.callTool("recall", map[string]any{"shard": "bought/nat"})
		if err != nil {
			return err
		}
		items, _ := rec["items"].([]any)
		if len(items) == 0 {
			return errors.New("recall bought/nat returned empty")
		}
		item, _ := items[len(items)-1].(map[string]any)
		ga, err := mcp.callTool("groundedAnswer", map[string]any{
			"claims":     []any{map[string]any{"text": item["text"], "cites": []any{map[string]any{"shard": item["shard"], "seq": item["seq"]}}}},
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		vr, err := runHook(exePath, []string{"hook", "verify-receipt", "-data-dir", dataDir}, fmt.Sprint(ga["footer"]))
		if err != nil {
			return err
		}
		var parsedHook struct {
			Verified []struct {
				OK      bool   `json:"ok"`
				Verdict string `json:"verdict"`
			} `json:"verified"`
		}
		if err := json.Unmarshal([]byte(vr), &parsedHook); err != nil || len(parsedHook.Verified) == 0 || !parsedHook.Verified[0].OK {
			return fmt.Errorf("Beta native hook verify-receipt failed: %s", vr)
		}
		return printJSON(map[string]any{
			"ok": true,
			"deal": map[string]any{
				"status":        deal["status"],
				"priceSats":     deal["priceSats"],
				"firstOfferMs":  deal["firstOfferMs"],
				"totalMs":       totalMs,
				"paymentTxid":   deal["paymentTxid"],
				"deliveredText": deal["deliveredText"],
			},
			"hookPrefetchOK": true,
			"receiptToken":   ga["receiptToken"],
			"hookVerdict":    parsedHook.Verified[0].Verdict,
		})
	}
	return fmt.Errorf("unknown beta phase %q", phase)
}

func streamableMCPCall(baseURL, method string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("Streamable HTTP MCP HTTP %d: %s", resp.StatusCode, body)
	}
	payload := strings.TrimSpace(string(body))
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		for _, l := range strings.Split(payload, "\n") {
			if strings.HasPrefix(l, "data: ") {
				payload = strings.TrimSpace(strings.TrimPrefix(l, "data: "))
				break
			}
		}
	}
	var msg map[string]any
	if err := json.Unmarshal([]byte(payload), &msg); err != nil {
		return nil, err
	}
	if e, ok := msg["error"]; ok && e != nil {
		b, _ := json.Marshal(e)
		return nil, fmt.Errorf("Streamable HTTP MCP error: %s", b)
	}
	res, _ := msg["result"].(map[string]any)
	return res, nil
}

func streamableTool(baseURL, name string, args map[string]any) (map[string]any, error) {
	res, err := streamableMCPCall(baseURL, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return nil, err
	}
	if isErr, _ := res["isError"].(bool); isErr {
		b, _ := json.Marshal(res)
		return nil, fmt.Errorf("Streamable MCP tool %s failed: %s", name, b)
	}
	return parseToolText(res)
}

func runGammaHTTPMCPAndProxy(phase, baseURL, runTag, peerPubHex string) error {
	if _, err := streamableMCPCall(baseURL, "initialize", map[string]any{
		"protocolVersion": "2024-11-05",
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "cursor-openai-l0-e2e", "version": "1.0.0"},
	}); err != nil {
		return err
	}
	if phase == "setup" {
		r, err := streamableTool(baseURL, "remember", map[string]any{
			"shard":      "gamma-agents",
			"text":       fmt.Sprintf("GammaHTTPMCP[%s]: MCP Streamable HTTP multiplexes concurrent AI agents cleanly over one daemon.", runTag),
			"source":     "cursor:gamma",
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		s, err := streamableTool(baseURL, "sell", map[string]any{
			"shard":       "gamma-agents",
			"domains":     []string{"agents"},
			"priceSats":   60,
			"description": "Gamma Streamable HTTP MCP architecture",
		})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "anchorTxid": r["anchorTxid"], "listing": s["shard"]})
	}
	if phase == "trade" {
		t0 := time.Now()
		deal, err := streamableTool(baseURL, "want", map[string]any{
			"domains":     []string{"ops"},
			"question":    "Kubernetes etcd snapshots rotation",
			"maxSats":     150,
			"targetShard": "bought/ops",
		})
		if err != nil {
			return err
		}
		totalMs := time.Since(t0).Milliseconds()
		price, _ := deal["priceSats"].(float64)
		if deal["status"] != "settled" || int(price) != 55 || !strings.Contains(fmt.Sprint(deal["deliveredText"]), "zstd-3") {
			b, _ := json.Marshal(deal)
			return fmt.Errorf("Gamma competitive want expected Beta (55 sats, zstd-3), got: %s", b)
		}
		oaJSON, err := postJSONWithHeader(baseURL+"/v1/proxy/openai/v1/chat/completions", "bought/ops", map[string]any{
			"model":    "gpt-4o",
			"messages": []any{map[string]any{"role": "user", "content": "How do etcd snapshots rotate?"}},
		})
		if err != nil {
			return err
		}
		anJSON, err := postJSONWithHeader(baseURL+"/v1/proxy/anthropic/v1/messages", "bought/ops", map[string]any{
			"model":    "claude-3-7-sonnet",
			"messages": []any{map[string]any{"role": "user", "content": "How do etcd snapshots rotate?"}},
		})
		if err != nil {
			return err
		}
		if okOA, _ := oaJSON["injectedHasBoughtFact"].(bool); !okOA {
			return fmt.Errorf("OpenAI L0 Proxy did not inject fact: %v", oaJSON)
		}
		if okAN, _ := anJSON["injectedHasBoughtFact"].(bool); !okAN {
			return fmt.Errorf("Anthropic L0 Proxy did not inject fact: %v", anJSON)
		}

		st, err := streamableTool(baseURL, "status", map[string]any{})
		if err != nil {
			return err
		}
		gid := "borg-" + runTag
		if _, err := streamableTool(baseURL, "borgGroup", map[string]any{"op": "create", "groupId": gid, "name": "Gamma Sec", "pubHex": st["identityPub"]}); err != nil {
			return err
		}
		if _, err := streamableTool(baseURL, "borgGroup", map[string]any{"op": "add", "groupId": gid, "pubHex": peerPubHex, "role": "member"}); err != nil {
			return err
		}
		if _, err := streamableTool(baseURL, "remember", map[string]any{"shard": "sec-" + runTag, "text": "Epoch1 secret", "borgGroup": gid, "callerPub": st["identityPub"]}); err != nil {
			return err
		}
		rev, err := streamableTool(baseURL, "borgGroup", map[string]any{"op": "revoke", "groupId": gid, "pubHex": peerPubHex})
		if err != nil {
			return err
		}
		if ep, _ := rev["newEpoch"].(float64); int(ep) != 2 {
			return fmt.Errorf("expected Borg epoch 2, got %v", rev)
		}
		return printJSON(map[string]any{
			"ok": true,
			"deal": map[string]any{
				"status":        deal["status"],
				"priceSats":     deal["priceSats"],
				"firstOfferMs":  deal["firstOfferMs"],
				"totalMs":       totalMs,
				"paymentTxid":   deal["paymentTxid"],
				"deliveredText": deal["deliveredText"],
			},
			"l0OpenAIProxyInjected":    true,
			"l0AnthropicProxyInjected": true,
			"borgRevokedEpoch":         rev["newEpoch"],
		})
	}
	return fmt.Errorf("unknown gamma phase %q", phase)
}

func a2aCall(baseURL, method string, params map[string]any) (map[string]any, error) {
	if params == nil {
		params = map[string]any{}
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	resp, err := http.Post(baseURL+"/a2a", "application/json", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var msg map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&msg); err != nil {
		return nil, err
	}
	if e, ok := msg["error"]; ok && e != nil {
		b, _ := json.Marshal(e)
		return nil, fmt.Errorf("A2A %s error: %s", method, b)
	}
	res, _ := msg["result"].(map[string]any)
	return res, nil
}

func runDeltaA2ANAT(phase, baseURL, runTag string) error {
	resp, err := http.Get(baseURL + "/.well-known/agent.json")
	if err != nil {
		return err
	}
	var card map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&card)
	resp.Body.Close()
	caps, _ := card["capabilities"].(map[string]any)
	if card["name"] != "nektar" || caps["paidKnowledgeDisclosure"] != true {
		return fmt.Errorf("Delta A2A card invalid: %v", card)
	}
	if phase == "setup" {
		r, err := a2aCall(baseURL, "memory/remember", map[string]any{
			"shard":      "a2a-nat",
			"text":       fmt.Sprintf("DeltaA2A[%s]: NAT-only A2A agents settle BRC-369 trades over Hub bbox relay with zero open ports.", runTag),
			"source":     "a2a:delta-nat",
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		s, err := a2aCall(baseURL, "market/sell", map[string]any{
			"shard":       "a2a-nat",
			"domains":     []string{"nat"},
			"priceSats":   50,
			"description": "Delta A2A NAT-relayed knowledge",
		})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "a2aCardName": card["name"], "anchorTxid": r["anchorTxid"], "listing": s["shard"]})
	}
	if phase == "trade" {
		peersRes, err := a2aCall(baseURL, "market/peers", map[string]any{"refresh": true})
		if err != nil {
			return err
		}
		peersList, _ := peersRes["peers"].([]any)
		bountiesList, _ := peersRes["bounties"].([]any)
		if len(peersList) < 5 {
			return fmt.Errorf("Delta A2A expected >=5 peers on dir lane, got %d", len(peersList))
		}
		t0 := time.Now()
		deal, err := a2aCall(baseURL, "market/want", map[string]any{
			"domains":     []string{"bsv"},
			"question":    "BSV Broadcaster testnet EF 100 sat/kB",
			"maxSats":     150,
			"targetShard": "bought/bsv",
		})
		if err != nil {
			return err
		}
		totalMs := time.Since(t0).Milliseconds()
		if deal["status"] != "settled" || !strings.Contains(fmt.Sprint(deal["deliveredText"]), "100 sat/kB") {
			b, _ := json.Marshal(deal)
			return fmt.Errorf("Delta A2A want failed: %s", b)
		}
		rec, err := a2aCall(baseURL, "memory/recall", map[string]any{"shard": "bought/bsv"})
		if err != nil {
			return err
		}
		items, _ := rec["items"].([]any)
		item, _ := items[len(items)-1].(map[string]any)
		ga, err := a2aCall(baseURL, "memory/answer", map[string]any{
			"claims":     []any{map[string]any{"text": item["text"], "cites": []any{map[string]any{"shard": item["shard"], "seq": item["seq"]}}}},
			"autoAnchor": true,
		})
		if err != nil {
			return err
		}
		ver, err := a2aCall(baseURL, "memory/verify", map[string]any{"token": ga["receiptToken"]})
		if err != nil || ver["ok"] != true {
			return fmt.Errorf("Delta A2A receipt verify failed: %v %v", ver, err)
		}
		return printJSON(map[string]any{
			"ok":                 true,
			"peersDiscovered":    len(peersList),
			"bountiesDiscovered": len(bountiesList),
			"deal": map[string]any{
				"status":        deal["status"],
				"priceSats":     deal["priceSats"],
				"firstOfferMs":  deal["firstOfferMs"],
				"totalMs":       totalMs,
				"paymentTxid":   deal["paymentTxid"],
				"deliveredText": deal["deliveredText"],
			},
			"receiptToken":   ga["receiptToken"],
			"receiptVerdict": ver["verdict"],
		})
	}
	return fmt.Errorf("unknown delta phase %q", phase)
}

func runMockLLM(port string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		hasFact := strings.Contains(body, "Nektar Verified Memory") && strings.Contains(body, "zstd-3")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":                    "mock-llm",
			"path":                  r.URL.Path,
			"injectedHasBoughtFact": hasFact,
		})
	})
	fmt.Printf("MOCK_LLM_READY:%s\n", port)
	return http.ListenAndServe("127.0.0.1:"+port, mux)
}

func postJSONWithHeader(urlStr, shard string, body map[string]any) (map[string]any, error) {
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, urlStr, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Nektar-Shard", shard)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return out, nil
}

func printJSON(v any) error {
	enc := json.NewEncoder(os.Stdout)
	return enc.Encode(v)
}
