// banner.ts — crisp human-readable Startup Card printed to stderr when nektar
// boots in HTTP/server mode. Gives immediate clarity: Dashboard pairing link,
// MCP endpoint, L0 Universal Proxy URL, fee address, and the single next command.
import { writeErr } from '@nektar/core'

export interface StartupBannerOpts {
  url: string
  network: string
  bootMs: number
  pairCode?: string
  feeAddress?: string
  balanceSats?: number
  automationOn: boolean
  containerMode: boolean
  generatedTokens?: boolean
}

export function formatStartupBanner(opts: StartupBannerOpts): string {
  const base = opts.url.replace(/\/+$/, '')
  const dashUrl = opts.pairCode ? `${base}/pair?code=${opts.pairCode}` : base
  const sec = (opts.bootMs / 1000).toFixed(opts.bootMs < 10_000 ? 1 : 0)
  const lines: string[] = [
    '',
    `╭─ nektar ready (${opts.network} · ${sec}s${opts.automationOn ? ' · market on' : ''})`,
    `│ Dashboard : ${dashUrl}`,
    `│ MCP (HTTP): ${base}/mcp`,
    `│ LLM Proxy : ${base}/v1/proxy/openai/v1   (any OpenAI/Anthropic agent)`
  ]
  if (opts.feeAddress) {
    const bal = opts.balanceSats ?? 0
    const hint = bal === 0 && opts.network === 'testnet' ? ' — 1-click faucet in Dashboard' : ''
    lines.push(`│ Fee Key   : ${opts.feeAddress} (${bal} sats)${hint}`)
  }
  if (opts.generatedTokens) {
    lines.push(`│ Auth      : auto-generated token saved in data dir (use pairing link above)`)
  }
  lines.push(
    opts.containerMode
      ? `│ Dock Apps : nektar dock   (or set OPENAI_BASE_URL=${base}/v1/proxy/openai/v1)`
      : `│ Dock Apps : nektar dock   (auto-wires Claude, Cursor, Hermes, OpenClaw, Codex…)`
  )
  lines.push(`╰─`)
  lines.push('')
  return lines.join('\n')
}

export function printStartupBanner(opts: StartupBannerOpts, write: (s: string) => void = writeErr): void {
  write(formatStartupBanner(opts))
}
