package main

import (
	"errors"
	"flag"
	"fmt"
	"net/http"
	"strings"
)

// cmdEarn is the hard-side seller activation fast path (`nektar earn`):
// lists open Hub knowledge bounties alongside your local shards, optionally runs the zero-sat
// MicroEarner (`--auto`), or anchors + submits a shard fragment directly to a Hub bounty.
func cmdEarn(args []string) error {
	fs := flag.NewFlagSet("earn", flag.ExitOnError)
	bountyID := fs.String("bounty", "", "Hub bounty ID to fulfill (e.g. bounty-ops, bounty-bsv, bounty-agents)")
	shard := fs.String("shard", "", "Local shard containing the verified fact to submit")
	seq := fs.Int64("seq", 0, "Optional fragment seq within -shard (defaults to latest)")
	hubURL := fs.String("hub", "", "Optional target Hub URL override")
	autoAnchor := fs.Bool("anchor", true, "Anchor -shard automatically if unanchored before submitting")
	autoEarn := fs.Bool("auto", false, "Run zero-sat MicroEarner immediately to earn starter float from Hub work/sponsored bounties")
	targetFloat := fs.Uint64("target-float", 2000, "Target starter float in sats for --auto MicroEarner")
	dataDir := fs.String("data-dir", "", "Data directory override")
	netName := fs.String("network", "", "Network override")
	asJSON := fs.Bool("json", false, "Machine-readable JSON output")
	_ = fs.Parse(args)

	info, ok := detectDaemon(*dataDir, *netName)
	if !ok || info == nil {
		return errors.New("no live nektar daemon found — start one in 1 command with `nektar up` first")
	}

	if *autoEarn {
		en := true
		out, err := callLiveAPI(info, http.MethodPost, "/v1/microearner/toggle", map[string]any{
			"enabled":         &en,
			"targetFloatSats": *targetFloat,
			"runNow":          true,
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out)
		}
		fmt.Println("✓ Zero-sat MicroEarner cycle completed:")
		return printJSON(out)
	}

	// Mode 1: Direct submission to a specific Hub bounty.
	if strings.TrimSpace(*bountyID) != "" {
		if strings.TrimSpace(*shard) == "" {
			return errors.New("provide -shard <name> alongside -bounty (e.g. `nektar earn --bounty bounty-ops --shard notes`)")
		}
		if *autoAnchor {
			_, _ = callLiveTool(info, "anchor", map[string]any{"shard": strings.TrimSpace(*shard)})
		}
		out, err := callLiveTool(info, "sell", map[string]any{
			"bountyId": strings.TrimSpace(*bountyID),
			"shard":    strings.TrimSpace(*shard),
			"seq":      *seq,
			"hubUrl":   strings.TrimSpace(*hubURL),
		})
		if err != nil {
			return err
		}
		if *asJSON {
			return printJSON(out)
		}
		fmt.Printf("✓ Submitted #%s to Hub bounty %s:\n", *shard, *bountyID)
		return printJSON(out)
	}

	// Mode 2: Guided seller discovery — fetch open Hub bounties + local shards + node health.
	peersOut, pErr := callLiveTool(info, "peers", map[string]any{"refresh": true})
	recallOut, _ := callLiveTool(info, "recall", map[string]any{"limit": 100})
	docOut, _ := callLiveTool(info, "doctor", map[string]any{})

	if *asJSON {
		return printJSON(map[string]any{
			"network": info.Network,
			"peers":   peersOut,
			"memory":  recallOut,
			"doctor":  docOut,
		})
	}

	fmt.Printf("Nektar v%s Earn — Hard-Side Knowledge Seller Overview (%s)\n\n", Version, info.Network)

	var bounties []any
	if pm, ok := peersOut.(map[string]any); ok {
		bounties, _ = pm["bounties"].([]any)
	}
	if pErr != nil {
		fmt.Printf("Hub Bounties: could not refresh peers (%v)\n", pErr)
	} else if len(bounties) == 0 {
		fmt.Println("Open Hub Bounties: none advertised on your active hub(s) yet.")
		fmt.Println("  Tip: join a live Hub in Dashboard -> Network, or start a local Hub with `nektar up --hub`.")
	} else {
		fmt.Printf("Open Hub Bounties (%d active — instant BSV AtomicBEEF payout once anchored):\n", len(bounties))
		for _, raw := range bounties {
			bm, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			id, _ := bm["id"].(string)
			dom, _ := bm["domain"].(string)
			q, _ := bm["question"].(string)
			rew := int64(0)
			if f, ok := bm["rewardSats"].(float64); ok {
				rew = int64(f)
			}
			filled := int64(0)
			if f, ok := bm["filled"].(float64); ok {
				filled = int64(f)
			}
			maxF := int64(0)
			if f, ok := bm["maxFills"].(float64); ok {
				maxF = int64(f)
			}
			fmt.Printf("  %-15s [%s]  +%d sats  (%d/%d filled)  %s\n", id, dom, rew, filled, maxF, q)
		}
	}

	var shards []any
	if rm, ok := recallOut.(map[string]any); ok {
		shards, _ = rm["shards"].([]any)
	}
	fmt.Printf("\nYour Local Shards (%d):\n", len(shards))
	if len(shards) == 0 {
		fmt.Println("  (none yet — add one with `nektar remember -shard notes -text \"...\" -anchor`)")
	} else {
		var names []string
		for _, sh := range shards {
			if s, ok := sh.(string); ok {
				names = append(names, "#"+s)
			}
		}
		fmt.Printf("  %s\n", strings.Join(names, "  "))
	}

	fmt.Println("\nTwo ways to earn BSV sats right now:")
	fmt.Println("  1. Sell directly TO a Hub bounty (instant payout for mined SPV-anchored facts):")
	fmt.Println("     nektar remember -shard ops-tips -text \"<your verified ops/BSV/agent fact>\" -anchor")
	fmt.Println("     nektar earn --bounty bounty-ops --shard ops-tips")
	fmt.Println("  2. Arm a shard for autonomous peer-to-peer sales (24/7 quoting under your policy):")
	fmt.Println("     nektar policy approve --preset seller")
	fmt.Println("     nektar sell -shard ops-tips -domains ops,agents -price 100")
	return nil
}
