package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/server"
)

func cmdMCP(args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	netName := fs.String("network", "", "Network override")
	dataDir := fs.String("data-dir", "", "Data directory override")
	direct := fs.Bool("direct", false, "Force in-process stdio MCP instead of bridging to a running daemon")
	_ = fs.Parse(args)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := *dataDir
	if dir == "" {
		dir = defaultDataDirForNetwork(*netName)
	}
	// One brain, many mouths: transparently bridge stdio -> live daemon `/mcp` across Mainnet or Testnet!
	if !*direct {
		if info, ok := detectDaemon(*dataDir, *netName); ok {
			return server.RunStdioBridge(ctx, info.URL, info.AuthToken, os.Stdin, os.Stdout)
		}
		// Auto-spawn a shared background daemon on demand so IDE/agent windows never collide on instance.lock!
		if *netName != "memory" && os.Getenv("NEKTAR_MCP_NO_AUTOSTART") != "1" && os.Getenv("NEKTAR_EPHEMERAL") != "1" {
			if info, err := ensureBackgroundDaemon(ctx, backgroundDaemonOpts{
				Network: *netName,
				Confirm: os.Getenv("NEKTAR_MAINNET_CONFIRM"),
				DataDir: *dataDir,
				Quiet:   true,
			}); err == nil && info != nil {
				return server.RunStdioBridge(ctx, info.URL, info.AuthToken, os.Stdin, os.Stdout)
			}
		}
	}

	if *netName != "memory" {
		releaseLock, lockErr := acquireInstanceLock(dir, "mcp-stdio")
		if lockErr != nil {
			return lockErr
		}
		defer releaseLock()
	}

	rt, err := openRuntime(ctx, *netName, *dataDir, DefaultLocalPort)
	if err != nil {
		return err
	}
	defer rt.closeFn()
	_, _ = rt.mem.ReconcilePendingAnchors(ctx)
	_, _ = rt.mem.EnsureWelcome(ctx)
	rt.mem.Chain().StartBackground(ctx)
	stopRefresher := startAnchorProofRefresher(ctx, rt.mem)
	defer stopRefresher()

	broker := market.NewBroker(rt.mem, rt.mkt, rt.disc, market.BrokerConfig{
		Matcher: buildJevMatcherFromEnv(rt.mem.Chain().Network()),
	})
	tb := server.NewToolbox(rt.mem, rt.mkt, broker, rt.disc, nil, envOr("NEKTAR_TOOL_PROFILE", "agent"))
	srv := server.NewMCPServerWithToolbox(tb)
	return srv.Run(ctx, &mcp.StdioTransport{})
}
