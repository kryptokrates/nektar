package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/server"
)

func callLiveAPI(info *server.DaemonInfo, method, path string, body any) (any, error) {
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, strings.TrimRight(info.URL, "/")+path, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	tok := info.AuthToken
	if tok == "" {
		tok = info.Token
	}
	if opTok := strings.TrimSpace(os.Getenv("NEKTAR_OPERATOR_TOKEN")); opTok != "" {
		tok = opTok
	} else if rawOp, rErr := os.ReadFile(filepath.Join(info.DataDir, "operator-token")); rErr == nil {
		if s := strings.TrimSpace(string(rawOp)); s != "" {
			tok = s
		}
	}
	if tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := (&http.Client{Timeout: 20 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("daemon HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	var out any
	_ = json.Unmarshal(raw, &out)
	return out, nil
}

func callLiveTool(info *server.DaemonInfo, tool string, args any) (any, error) {
	return callLiveAPI(info, http.MethodPost, "/v1/tools/"+tool, args)
}

func cmdDoctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override (testnet|main|memory)")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	_ = fs.Parse(args)
	if info, ok := detectDaemon(*dataDir, *netName); ok {
		out, err := callLiveTool(info, "doctor", map[string]any{})
		if err == nil {
			redacted := *info
			redacted.AuthToken = ""
			redacted.Token = ""
			if *asJSON {
				return printJSON(map[string]any{"daemon": &redacted, "diagnosis": out})
			}
			printDoctorHuman(&redacted, out)
			return nil
		}
	}
	effectiveNet := strings.TrimSpace(*netName)
	if effectiveNet == "" {
		effectiveNet = envOr("NEKTAR_NETWORK", "testnet")
	}
	dir := strings.TrimSpace(*dataDir)
	if dir == "" {
		dir = defaultDataDirForNetwork(effectiveNet)
	}
	dbPath := filepath.Join(dir, "nektar.sqlite")
	if effectiveNet != "memory" {
		if _, err := os.Stat(dbPath); os.IsNotExist(err) {
			uninit := map[string]any{
				"ok":        false,
				"verdict":   "amber",
				"running":   false,
				"network":   effectiveNet,
				"dataDir":   dir,
				"checks":    []map[string]string{{"name": "daemon", "status": "amber", "detail": fmt.Sprintf("no running daemon and no database yet at %s", dir)}},
				"nextSteps": []string{"Start your node with `nektar up` (or `nektar up --network main --confirm I_UNDERSTAND`)"},
			}
			if *asJSON {
				return printJSON(uninit)
			}
			printDoctorHuman(nil, uninit)
			return nil
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
	diag := tb.ExecDoctor(ctx)
	if *asJSON {
		return printJSON(diag)
	}
	printDoctorHuman(nil, diag)
	return nil
}

func printDoctorHuman(daemon *server.DaemonInfo, raw any) {
	m, ok := raw.(map[string]any)
	if !ok {
		_ = printJSON(raw)
		return
	}
	verdict, _ := m["verdict"].(string)
	if verdict == "" {
		verdict = "unknown"
	}
	netName, _ := m["network"].(string)
	var b strings.Builder
	if daemon != nil {
		fmt.Fprintf(&b, "nektar doctor [%s] — daemon UP at %s (%s · pid %d)\n", strings.ToUpper(verdict), daemon.URL, daemon.Network, daemon.PID)
	} else {
		fmt.Fprintf(&b, "nektar doctor [%s] — daemon STOPPED (offline check · %s)\n", strings.ToUpper(verdict), netName)
	}
	if checks, ok := m["checks"].([]any); ok {
		for _, c := range checks {
			cm, _ := c.(map[string]any)
			st := strings.ToUpper(fmt.Sprint(cm["status"]))
			fmt.Fprintf(&b, "  [%-5s] %-20s %s\n", st, fmt.Sprint(cm["name"]), fmt.Sprint(cm["detail"]))
		}
	} else if checks, ok := m["checks"].([]server.DoctorCheck); ok {
		for _, c := range checks {
			fmt.Fprintf(&b, "  [%-5s] %-20s %s\n", strings.ToUpper(c.Status), c.Name, c.Detail)
		}
	} else if checks, ok := m["checks"].([]map[string]string); ok {
		for _, c := range checks {
			fmt.Fprintf(&b, "  [%-5s] %-20s %s\n", strings.ToUpper(c["status"]), c["name"], c["detail"])
		}
	}
	if steps, ok := m["nextSteps"].([]any); ok && len(steps) > 0 {
		b.WriteString("\nNext:\n")
		for _, s := range steps {
			fmt.Fprintf(&b, "  • %s\n", fmt.Sprint(s))
		}
	} else if steps, ok := m["nextSteps"].([]string); ok && len(steps) > 0 {
		b.WriteString("\nNext:\n")
		for _, s := range steps {
			fmt.Fprintf(&b, "  • %s\n", s)
		}
	}
	fmt.Print(formatConsoleText(b.String()))
}

func cmdPeers(args []string) error {
	if len(args) > 0 && (args[0] == "join" || args[0] == "leave") {
		sub := args[0]
		fs := flag.NewFlagSet("peers "+sub, flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		asJSON := fs.Bool("json", false, "Machine-readable JSON output")
		_ = fs.Parse(args[1:])
		if fs.NArg() == 0 {
			return fmt.Errorf("usage: nektar peers %s <domain-or-hub-url>", sub)
		}
		target := strings.TrimSpace(fs.Arg(0))
		payload := map[string]any{"refresh": true}
		if sub == "join" {
			payload["joinHub"] = target
		} else {
			payload["leaveHub"] = target
		}
		if info, ok := detectDaemon(*dataDir, ""); ok {
			out, err := callLiveTool(info, "peers", payload)
			if err != nil {
				return err
			}
			if *asJSON {
				return printJSON(out)
			}
			if sub == "join" {
				if m, ok := out.(map[string]any); ok {
					fmt.Printf("✓ Joined LightBSV Hub: %v\n", m["joinedHub"])
				}
			} else {
				fmt.Printf("✓ Disconnected Hub: %s\n", target)
			}
			return nil
		}
		ctx := context.Background()
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
		var in server.PeersToolInput
		in.Refresh = true
		if sub == "join" {
			in.JoinHub = target
		} else {
			in.LeaveHub = target
		}
		out, err := tb.ExecPeersWithInput(ctx, in)
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out)
		}
		fmt.Printf("✓ Updated Hub list (%s %s)\n", sub, target)
		return nil
	}

	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	_ = fs.Parse(args)
	var raw any
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "peers", map[string]any{"refresh": true})
		if err == nil {
			raw = out
		}
	}
	if raw == nil {
		ctx := context.Background()
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
		raw = tb.ExecPeers(ctx, true)
	}
	if *asJSON {
		return printJSON(raw)
	}
	printPeersHuman(raw)
	return nil
}

func printPeersHuman(raw any) {
	b, err := json.Marshal(raw)
	if err != nil {
		_ = printJSON(raw)
		return
	}
	var doc struct {
		Hosts []string `json:"hosts"`
		Hubs  []struct {
			URL       string   `json:"url"`
			Name      string   `json:"name"`
			Source    string   `json:"source"`
			Connected bool     `json:"connected"`
			Reachable bool     `json:"reachable"`
			LatencyMs int64    `json:"latencyMs"`
			Role      string   `json:"role"`
			FeePreset string   `json:"feePreset"`
			Topics    []string `json:"topics"`
		} `json:"hubs"`
		Peers []struct {
			Pub         string   `json:"pub"`
			Handle      string   `json:"handle"`
			IsHub       bool     `json:"isHub"`
			SellDomains []string `json:"sellDomains"`
		} `json:"peers"`
		Bounties []struct {
			ID         string `json:"id"`
			Domain     string `json:"domain"`
			Question   string `json:"question"`
			RewardSats uint64 `json:"rewardSats"`
		} `json:"bounties"`
	}
	if json.Unmarshal(b, &doc) != nil {
		_ = printJSON(raw)
		return
	}
	var out strings.Builder
	out.WriteString("LIGHTBSV HUBS & DIRECTORY (opt-in via `nektar peers join <domain-or-url>`)\n")
	if len(doc.Hubs) == 0 && len(doc.Hosts) == 0 {
		out.WriteString("  (no Hubs configured — run `nektar hub up` or `nektar peers join https://nektar.run/hub`)\n")
	} else {
		for _, h := range doc.Hubs {
			state := "discovered"
			if h.Connected {
				state = "CONNECTED"
			} else if !h.Reachable {
				state = "offline"
			}
			role := h.Role
			if role == "" {
				role = "leaf"
			}
			fee := h.FeePreset
			if fee == "" {
				fee = "free"
			}
			fmt.Fprintf(&out, "  [%-10s] %-26s role=%-5s fees=%-7s rtt=%dms  (%s)\n",
				state, h.Name, role, fee, h.LatencyMs, h.URL)
		}
	}
	out.WriteString("\nVERIFIED PEERS & HANDLES\n")
	if len(doc.Peers) == 0 {
		out.WriteString("  (0 peers cached yet — publish a capsule with `nektar sell` to announce your handle)\n")
	} else {
		for _, p := range doc.Peers {
			label := p.Handle
			if label == "" && len(p.Pub) >= 12 {
				label = p.Pub[:12] + "…"
			}
			fmt.Fprintf(&out, "  • %-28s sell=%v hub=%v\n", label, p.SellDomains, p.IsHub)
		}
	}
	out.WriteString("\nOPEN SATOSHI BOUNTIES (`nektar earn`)\n")
	if len(doc.Bounties) == 0 {
		out.WriteString("  (no open Hub bounties right now)\n")
	} else {
		for _, bo := range doc.Bounties {
			fmt.Fprintf(&out, "  • [%d sats] #%-11s %s (id=%s)\n", bo.RewardSats, bo.Domain, bo.Question, bo.ID)
		}
	}
	fmt.Print(formatConsoleText(out.String()))
}

func cmdBuy(args []string) error {
	fs := flag.NewFlagSet("buy", flag.ExitOnError)
	domains := fs.String("domains", "ops", "Comma-separated capsule domains (e.g. ops,debugging,mcp,api-quirks,bsv)")
	question := fs.String("question", "", "Capsule or runbook question to buy")
	maxSats := fs.Uint64("max", 100, "Max satoshis to pay")
	minGrade := fs.String("min-grade", "", "Minimum NMU-1 grade (S|A|B|C)")
	targetShard := fs.String("shard", "", "Target local repo/shard to import into")
	noBounty := fs.Bool("no-bounty", false, "Do not auto-post a StandingOrder bounty if 0 immediate offers match")
	bountyTTL := fs.Int64("bounty-ttl", 86400, "StandingOrder bounty TTL in seconds when 0 offers match")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	q := strings.TrimSpace(*question)
	if q == "" && fs.NArg() > 0 {
		q = strings.TrimSpace(strings.Join(fs.Args(), " "))
	}
	if q == "" {
		return errors.New("provide a question: `nektar buy -domains ops -question \"...\"` (or `nektar buy \"...\"`)")
	}
	postBounty := !*noBounty
	info, ok := detectDaemon(*dataDir, "")
	if !ok {
		return errors.New("no live nektar daemon found — start one with `nektar up` first")
	}
	out, err := callLiveTool(info, "buyCapsule", map[string]any{
		"domains":          strings.Split(*domains, ","),
		"question":         q,
		"maxSats":          *maxSats,
		"minGrade":         strings.TrimSpace(*minGrade),
		"targetShard":      strings.TrimSpace(*targetShard),
		"postBounty":       postBounty,
		"bountyTtlSeconds": *bountyTTL,
	})
	if err != nil {
		return err
	}
	return printJSON(out)
}

func cmdImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ExitOnError)
	source := fs.String("source", "", "Path to .nkt file, https:// URL, or chirp:// URI")
	repo := fs.String("repo", "", "Optional target local Memory Repo name")
	branch := fs.String("branch", "", "Optional target branch (default: main)")
	creator := fs.String("from", "", "Optional creator LightBSV handle (<handle>@domain)")
	joinHub := fs.Bool("join-hub", false, "Automatically join the creator's Hub if reachable")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	src := strings.TrimSpace(*source)
	if src == "" && fs.NArg() > 0 {
		src = strings.TrimSpace(fs.Arg(0))
	}
	if src == "" {
		return errors.New("usage: nektar import <path-or-https-url.nkt> [--repo <name>] [--join-hub]")
	}
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "importCapsule", map[string]any{
			"source":         src,
			"repo":           strings.TrimSpace(*repo),
			"branch":         strings.TrimSpace(*branch),
			"creatorHandle":  strings.TrimSpace(*creator),
			"joinCreatorHub": *joinHub,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
	out, err := tb.ExecImportCapsule(ctx, server.ImportCapsuleInput{
		Source:         src,
		Repo:           strings.TrimSpace(*repo),
		Branch:         strings.TrimSpace(*branch),
		CreatorHandle:  strings.TrimSpace(*creator),
		JoinCreatorHub: joinHub,
	})
	if err != nil {
		return err
	}
	return printJSON(out)
}

func cmdWant(args []string) error {
	fs := flag.NewFlagSet("want", flag.ExitOnError)
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	question := fs.String("question", "", "Knowledge question to buy")
	ontologyID := fs.String("ontology", "", "Optional 64-hex OntologyID pin")
	maxSats := fs.Uint64("max", 200, "Max satoshis to pay")
	minGrade := fs.String("min-grade", "", "Minimum NMU-1 commodity grade (S|A|B|C)")
	maxSatsPer1k := fs.Uint64("max-sats-per-1k", 0, "Max normalized unit price in sats per 1000 verified triples (milli-sats/triple)")
	freshness := fs.String("freshness", "", "Required freshness class (ephemeral|versioned|evergreen)")
	rights := fs.String("rights", "", "Required usage rights (inference_only|distill_resellable)")
	mode := fs.String("mode", "", "Preferred pricing contract mode (spot|per_query|subscription)")
	targetShard := fs.String("shard", "", "Target shard to store purchased knowledge")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	if strings.TrimSpace(*question) == "" {
		return errors.New("provide -question \"...\"")
	}
	domList := []string{}
	for _, d := range strings.Split(*domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domList = append(domList, d)
		}
	}
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "want", map[string]any{
			"domains":           domList,
			"question":          *question,
			"ontologyId":        strings.TrimSpace(*ontologyID),
			"maxSats":           *maxSats,
			"minGrade":          strings.TrimSpace(*minGrade),
			"maxSatsPer1kUnits": *maxSatsPer1k,
			"freshnessClass":    strings.TrimSpace(*freshness),
			"requiredRights":    strings.TrimSpace(*rights),
			"pricingMode":       strings.TrimSpace(*mode),
			"targetShard":       *targetShard,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar serve` first")
}

func cmdSell(args []string) error {
	fs := flag.NewFlagSet("sell", flag.ExitOnError)
	textFlag := fs.String("text", "", "Inline capsule text to PII-scrub, commit, anchor, and list in 1 step")
	fileFlag := fs.String("file", "", "Optional .md/.txt/.nkt file to scrub, commit/import, anchor, and list in 1 step")
	exportFlag := fs.String("export", "", "Optional output path to write a shareable .nkt capsule file")
	shard := fs.String("shard", "default", "Local shard/repo to sell")
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	price := fs.Uint64("price", 50, "Spot price in satoshis (0 disables spot when -query-price or -sub-price is set)")
	perUnit := fs.Bool("per-unit", false, "Interpret -price as sats per verified triple (unit pricing)")
	queryPrice := fs.Uint64("query-price", 0, "Enable NMU-1 per_query BRC-105 HTTP 402 recall at this satoshi price")
	subPrice := fs.Uint64("sub-price", 0, "Enable NMU-1 subscription SSE feed (Grade S SLA) at this satoshi price")
	subDays := fs.Int64("sub-days", 30, "Subscription window length in days when -sub-price > 0")
	freshness := fs.String("freshness", "", "Default NMU-1 freshness class (ephemeral|versioned|evergreen)")
	rights := fs.String("rights", "", "NMU-1 usage rights (inference_only|distill_resellable)")
	minGrade := fs.String("min-grade", "", "Optional advertised grade cap/floor (S|A|B|C)")
	desc := fs.String("desc", "", "Short listing description")
	ontologyID := fs.String("ontology", "", "Optional pinned OntologyID")
	allowRaw := fs.Bool("allow-raw", false, "Explicitly permit selling non-distilled raw fragments (still scrubbed for secrets)")
	dynamic := fs.Bool("dynamic", false, "Enable adaptive token-arbitrage dynamic pricing")
	floor := fs.Uint64("floor", 0, "Optional dynamic price floor in satoshis")
	ceiling := fs.Uint64("ceiling", 0, "Optional dynamic price ceiling in satoshis")
	bountyID := fs.String("bounty", "", "Optional Hub bounty ID to sell directly TO the Hub")
	hubURL := fs.String("hub", "", "Optional Hub URL when submitting a bounty")
	seq := fs.Int64("seq", 0, "Optional fragment seq when submitting a Hub bounty")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	visitedShard := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "shard" {
			visitedShard = true
		}
	})

	domList := []string{}
	for _, d := range strings.Split(*domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domList = append(domList, d)
		}
	}
	if strings.TrimSpace(*textFlag) != "" || strings.TrimSpace(*fileFlag) != "" || strings.TrimSpace(*exportFlag) != "" {
		info, ok := detectDaemon(*dataDir, "")
		if !ok {
			return errors.New("no live nektar daemon found — start one with `nektar up` first")
		}
		targetShard := *shard
		if !visitedShard {
			targetShard = ""
		}
		out, err := callLiveTool(info, "sellCapsule", map[string]any{
			"text":        *textFlag,
			"file":        *fileFlag,
			"shard":       targetShard,
			"domains":     domList,
			"priceSats":   *price,
			"description": *desc,
			"ontologyId":  strings.TrimSpace(*ontologyID),
			"exportPath":  strings.TrimSpace(*exportFlag),
			"bountyId":    *bountyID,
			"hubUrl":      *hubURL,
			"seq":         *seq,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	var subDurSec int64
	if *subPrice > 0 && *subDays > 0 {
		subDurSec = *subDays * 86400
	}
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "sell", map[string]any{
			"shard":                   *shard,
			"domains":                 domList,
			"priceSats":               *price,
			"pricePerUnit":            *perUnit,
			"queryPriceSats":          *queryPrice,
			"subscriptionPriceSats":   *subPrice,
			"subscriptionDurationSec": subDurSec,
			"freshnessClass":          strings.TrimSpace(*freshness),
			"usageRights":             strings.TrimSpace(*rights),
			"minAdvertisedGrade":      strings.TrimSpace(*minGrade),
			"description":             *desc,
			"ontologyId":              strings.TrimSpace(*ontologyID),
			"allowRaw":                *allowRaw,
			"dynamicPricing":          *dynamic,
			"floorSats":               *floor,
			"ceilingSats":             *ceiling,
			"bountyId":                *bountyID,
			"hubUrl":                  *hubURL,
			"seq":                     *seq,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar serve` first")
}

func cmdTicker(args []string) error {
	fs := flag.NewFlagSet("ticker", flag.ExitOnError)
	ontologyID := fs.String("ontology", "", "Filter by 64-hex OntologyID (or 'unpinned')")
	grade := fs.String("grade", "", "Filter by NMU-1 grade (S|A|B|C)")
	mode := fs.String("mode", "", "Filter by pricing mode (spot|per_query|subscription)")
	domain := fs.String("domain", "", "Filter by market domain")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	_ = fs.Parse(args)

	if info, ok := detectDaemon(*dataDir, *netName); ok {
		q := url.Values{}
		if v := strings.TrimSpace(*ontologyID); v != "" {
			q.Set("ontologyId", v)
		}
		if v := strings.TrimSpace(*grade); v != "" {
			q.Set("grade", v)
		}
		if v := strings.TrimSpace(*mode); v != "" {
			q.Set("pricingMode", v)
		}
		if v := strings.TrimSpace(*domain); v != "" {
			q.Set("domain", v)
		}
		path := "/v1/market/ticker"
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
		out, err := callLiveAPI(info, http.MethodGet, path, nil)
		if err == nil {
			if *asJSON {
				return printJSON(out)
			}
			var parsed server.CommodityTickerResponse
			if b, mErr := json.Marshal(out); mErr == nil && json.Unmarshal(b, &parsed) == nil {
				printTickerHuman(parsed)
				return nil
			}
			return printJSON(out)
		}
	}

	ctx := context.Background()
	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	resp := server.BuildCommodityTickerFrom(ctx, rt.mem, rt.disc, nil, "", server.TickerFilter{
		OntologyID:  strings.TrimSpace(*ontologyID),
		Grade:       strings.TrimSpace(*grade),
		PricingMode: strings.TrimSpace(*mode),
		Domain:      strings.TrimSpace(*domain),
	})
	if *asJSON {
		return printJSON(resp)
	}
	printTickerHuman(resp)
	return nil
}

func printTickerHuman(resp server.CommodityTickerResponse) {
	var b strings.Builder
	b.WriteString("NEKTAR NMU-1 COMMODITY TICKER (Standardized Knowledge Unit Quotes)\n")
	if len(resp.Rows) == 0 {
		b.WriteString("  (no active NMU-1 quotes found — list a shard with `nektar sell` or join a Hub)\n")
	} else {
		fmt.Fprintf(&b, "  %-16s %-5s %-12s %12s %12s %9s %7s %8s %9s\n",
			"ONTOLOGY", "GRADE", "MODE", "MIN s/triple", "MED s/triple", "MIN ASK", "OFFERS", "UNITS", "BOND/FEED")
		for _, r := range resp.Rows {
			ont := r.OntologyID
			if len(ont) > 14 && ont != "unpinned" {
				ont = ont[:8] + ".." + ont[len(ont)-4:]
			}
			fmt.Fprintf(&b, "  %-16s %-5s %-12s %12s %12s %6d sat %7d %8d %4d/%-4d\n",
				ont, r.Grade, r.PricingMode, r.MinSatsPerTriple, r.MedianSatsPerTriple,
				r.MinAskSats, r.ActiveOffers, r.TotalUnitsAvailable, r.BondedSellers, r.LiveFeeds)
		}
	}
	if resp.AdvisoryNote != "" {
		fmt.Fprintf(&b, "\nNote: %s\n", resp.AdvisoryNote)
	}
	fmt.Print(formatConsoleText(b.String()))
}

func cmdDistill(args []string) error {
	fs := flag.NewFlagSet("distill", flag.ExitOnError)
	shards := fs.String("shards", "", "Optional comma-separated working shards to distill")
	maxBatch := fs.Int("max-batch", 32, "Maximum fragments to process in this pass")
	autoAnchor := fs.Bool("anchor", true, "Anchor newly distilled market-* shards when wallet is funded")
	autoList := fs.Bool("list", false, "Auto-list newly distilled market-* shards on the market")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	var shardList []string
	for _, s := range strings.Split(*shards, ",") {
		if s = strings.TrimSpace(s); s != "" {
			shardList = append(shardList, s)
		}
	}
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveAPI(info, http.MethodPost, "/v1/distill/run", map[string]any{
			"shards":     shardList,
			"maxBatch":   *maxBatch,
			"autoAnchor": *autoAnchor,
			"autoList":   *autoList,
			"force":      true,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar up` first")
}

func cmdPolicy(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: nektar policy [approve --preset buyer|seller|both | list | revoke <policyId>]")
	}
	sub := args[0]
	rest := args[1:]
	ctx := context.Background()

	switch sub {
	case "approve":
		fs := flag.NewFlagSet("policy approve", flag.ExitOnError)
		preset := fs.String("preset", "both", "Policy preset: buyer | seller | both")
		domains := fs.String("domains", "", "Optional comma-separated allowed domains")
		confirm := fs.String("confirm", os.Getenv("NEKTAR_MAINNET_CONFIRM"), "Required I_UNDERSTAND on mainnet")
		aiMode := fs.String("ai-mode", "", "Optional Jev AI matching mode: off | advisory | gated | strict")
		aiPrivacy := fs.String("ai-privacy", "", "Optional seller privacy scope: metadata_only | disclosable_preview")
		aiHosts := fs.String("ai-hosts", "", "Optional comma-separated allowed AI egress hostnames")
		aiIntent := fs.String("ai-intent", "", "Optional semantic scope description for Jev policy-fit rubric")
		aiProhibit := fs.String("ai-prohibit", "", "Optional comma-separated prohibited topics for Jev matching")
		aiMinRel := fs.Int64("ai-min-relevance", 0, "Optional minimum relevance floor in basis points (0..10000)")
		aiEscConf := fs.Int64("ai-escalate-confidence", 0, "Optional confidence floor in bps below which deals escalate for approval")
		aiCedarFile := fs.String("ai-cedar-file", "", "Optional path to Cedar post-match market policy file")
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)

		var doms []string
		for _, d := range strings.Split(*domains, ",") {
			if d = strings.TrimSpace(d); d != "" {
				doms = append(doms, d)
			}
		}
		var aiPol *market.AIMatchingPolicy
		var hosts, prohib []string
		for _, h := range strings.Split(*aiHosts, ",") {
			if h = strings.TrimSpace(h); h != "" {
				hosts = append(hosts, h)
			}
		}
		for _, p := range strings.Split(*aiProhibit, ",") {
			if p = strings.TrimSpace(p); p != "" {
				prohib = append(prohib, p)
			}
		}
		var cedarText string
		if strings.TrimSpace(*aiCedarFile) != "" {
			b, rErr := os.ReadFile(strings.TrimSpace(*aiCedarFile))
			if rErr != nil {
				return fmt.Errorf("read --ai-cedar-file: %w", rErr)
			}
			cedarText = string(b)
		}
		candAI := &market.AIMatchingPolicy{
			Mode:                 strings.TrimSpace(*aiMode),
			AllowedHosts:         hosts,
			SellerPrivacyScope:   strings.TrimSpace(*aiPrivacy),
			MinSellerRelevanceBP: *aiMinRel,
			MinBuyerRelevanceBP:  *aiMinRel,
			EscalateConfidenceBP: *aiEscConf,
			SemanticIntent:       strings.TrimSpace(*aiIntent),
			ProhibitedTopics:     prohib,
			CedarMarketPolicy:    cedarText,
		}
		if !candAI.IsEmpty() {
			if err := candAI.Validate(); err != nil {
				return err
			}
			aiPol = candAI
		}

		if info, ok := detectDaemon(*dataDir, ""); ok {
			payload := map[string]any{
				"preset":  *preset,
				"domains": doms,
				"confirm": *confirm,
			}
			if aiPol != nil {
				payload["aiMatching"] = aiPol
			}
			out, err := callLiveAPI(info, http.MethodPost, "/v1/onboard/policy-preset", payload)
			if err != nil {
				return err
			}
			return printJSON(out)
		}

		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		if rt.mem.Chain().Mainnet() && *confirm != "I_UNDERSTAND" {
			return errors.New("mainnet policy approval requires --confirm I_UNDERSTAND")
		}
		rec, err := market.ApprovePolicyPresetWithAI(rt.mem.DB(), rt.mkt.Policy(), *preset, doms, "cli", aiPol)
		if err != nil {
			return err
		}
		_, _, _ = market.CommitPolicyAudit(ctx, rt.mem, rec, false)
		return printJSON(rec)

	case "list":
		fs := flag.NewFlagSet("policy list", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest)
		if info, ok := detectDaemon(*dataDir, ""); ok {
			if out, err := callLiveAPI(info, http.MethodGet, "/v1/policies", nil); err == nil {
				return printJSON(out)
			}
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		list, err := rt.mem.DB().ListOperatorPolicies()
		if err != nil {
			return err
		}
		return printJSON(list)

	case "revoke":
		if len(rest) == 0 {
			return errors.New("usage: nektar policy revoke <policyId>")
		}
		policyID := rest[0]
		fs := flag.NewFlagSet("policy revoke", flag.ExitOnError)
		dataDir := fs.String("data-dir", "", "Data directory override")
		_ = fs.Parse(rest[1:])
		if info, ok := detectDaemon(*dataDir, ""); ok {
			if out, err := callLiveAPI(info, http.MethodDelete, "/v1/policies/"+policyID, nil); err == nil {
				return printJSON(out)
			}
		}
		rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
		if err != nil {
			return err
		}
		defer rt.closeFn()
		if err := rt.mem.DB().RevokeOperatorPolicy(policyID); err != nil {
			return err
		}
		return printJSON(map[string]any{"revoked": policyID})

	default:
		return fmt.Errorf("unknown policy subcommand %q (use approve, list, or revoke)", sub)
	}
}

func cmdOpen(args []string) error {
	fs := flag.NewFlagSet("open", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	startFlag := fs.Bool("start", false, "Auto-start the background daemon if not running")
	_ = fs.Parse(args)
	info, ok := detectDaemon(*dataDir, *netName)
	if !ok || info == nil {
		if !*startFlag {
			return errors.New("no running nektar daemon found — start one with `nektar up` (or run `nektar open --start`)")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		started, err := ensureBackgroundDaemon(ctx, backgroundDaemonOpts{
			Network: *netName,
			Confirm: os.Getenv("NEKTAR_MAINNET_CONFIRM"),
			DataDir: *dataDir,
		})
		if err != nil {
			return err
		}
		info = started
	}
	u := strings.TrimRight(info.URL, "/")
	if out, err := callLiveAPI(info, http.MethodPost, "/v1/auth/pair-code", map[string]any{}); err == nil {
		if m, ok := out.(map[string]any); ok {
			if code, _ := m["code"].(string); code != "" {
				u = u + "/pair?code=" + code
			}
		}
	}
	fmt.Printf("Opening Nektar Dashboard: %s\n", u)
	return openBrowserURL(u)
}

func openBrowserURL(u string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", u)
	case "darwin":
		cmd = exec.Command("open", u)
	default:
		cmd = exec.Command("xdg-open", u)
	}
	return cmd.Start()
}
