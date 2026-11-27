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
	fs := flag.NewFlagSet("peers", flag.ExitOnError)
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "peers", map[string]any{"refresh": true})
		if err == nil {
			return printJSON(out)
		}
	}
	ctx := context.Background()
	rt, err := openRuntime(ctx, "", *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	tb := server.NewToolbox(rt.mem, rt.mkt, nil, rt.disc, nil, "operator")
	return printJSON(tb.ExecPeers(ctx, true))
}

func cmdWant(args []string) error {
	fs := flag.NewFlagSet("want", flag.ExitOnError)
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	question := fs.String("question", "", "Knowledge question to buy")
	ontologyID := fs.String("ontology", "", "Optional 64-hex OntologyID pin")
	maxSats := fs.Uint64("max", 200, "Max satoshis to pay")
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
			"domains":     domList,
			"question":    *question,
			"ontologyId":  strings.TrimSpace(*ontologyID),
			"maxSats":     *maxSats,
			"targetShard": *targetShard,
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
	shard := fs.String("shard", "default", "Local shard to sell")
	domains := fs.String("domains", "ops", "Comma-separated market domains")
	price := fs.Uint64("price", 100, "Price in satoshis")
	desc := fs.String("desc", "", "Short listing description")
	bountyID := fs.String("bounty", "", "Optional Hub bounty ID to sell directly TO the Hub")
	hubURL := fs.String("hub", "", "Optional Hub URL when submitting a bounty")
	seq := fs.Int64("seq", 0, "Optional fragment seq when submitting a Hub bounty")
	dataDir := fs.String("data-dir", "", "Data directory override")
	_ = fs.Parse(args)

	domList := []string{}
	for _, d := range strings.Split(*domains, ",") {
		if d = strings.TrimSpace(d); d != "" {
			domList = append(domList, d)
		}
	}
	if info, ok := detectDaemon(*dataDir, ""); ok {
		out, err := callLiveTool(info, "sell", map[string]any{
			"shard":       *shard,
			"domains":     domList,
			"priceSats":   *price,
			"description": *desc,
			"bountyId":    *bountyID,
			"hubUrl":      *hubURL,
			"seq":         *seq,
		})
		if err != nil {
			return err
		}
		return printJSON(out)
	}
	return errors.New("no live nektar daemon found — start one with `nektar serve` first")
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
