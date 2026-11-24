// cli-dispatch.ts — subcommand router + stdio-to-HTTP bridge helper for the
// nektar binary (`policy`, `dock`, `hook`, `status`). Keeps bin.ts fanout well
// below the architecture ratchet while exposing every no-checkout operator and
// docking command from one binary.
import { discoverDaemon } from './daemon.js'

export async function runSubcommand(argv: string[]): Promise<number | undefined> {
  const sub = argv[0]
  if (sub === 'policy') {
    const { runPolicyCli } = await import('./policy-cli.js')
    return await runPolicyCli(argv.slice(1))
  }
  if (sub === 'dock') {
    const { runDockCli } = await import('./dock-cli.js')
    return await runDockCli(argv.slice(1))
  }
  if (sub === 'hook') {
    const { runHookCli } = await import('./hooks-cli.js')
    return await runHookCli(argv.slice(1))
  }
  if (sub === 'status') {
    const { runStatusCli } = await import('./status-cli.js')
    return await runStatusCli(argv.slice(1))
  }
  return undefined
}

export async function tryAttachStdioBridge(opts: { dataDir: string; log: (line: string) => void }): Promise<boolean> {
  const existing = await discoverDaemon({ dataDir: opts.dataDir, timeoutMs: 500 })
  if (existing === undefined) return false
  const { serveStdioBridge } = await import('./mcp-bridge.js')
  const bridge = await serveStdioBridge({
    mcpUrl: existing.mcpUrl,
    token: existing.token,
    log: opts.log,
    onClose: () => process.exit(0)
  })
  process.on('SIGINT', () => void bridge.close())
  process.on('SIGTERM', () => void bridge.close())
  return true
}
