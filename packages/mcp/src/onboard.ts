// onboard.ts — browser onboarding endpoints behind /v1/onboard/*:
//   POST /v1/onboard/faucet        — 1-click testnet faucet drip + auto-import
//   POST /v1/onboard/policy-preset — operator-gated starter policy approval
//   GET  /v1/onboard/hub-bundle    — pre-filled Hub CLI + Cloud-Init + Coolify config
import { mkdirSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { join, resolve } from 'node:path'
import { nektarEnv, type EnvSource } from '@nektar/core'
import { policyApprovalError, AutomationStore } from '@nektar/automation'
import type { NektarMemory } from './memory.js'
import { starterPolicy } from './policy-cli.js'

const DEFAULT_FAUCET_URL = 'https://nektar.run/v1/faucet'

export interface OnboardContext {
  mem: NektarMemory
  ops?: {
    wallet?: () => Promise<unknown>
    importFunding?: (txid: string) => Promise<unknown>
  }
  env?: EnvSource
  fetchImpl?: typeof fetch
  isOperator: (req: IncomingMessage) => boolean
}

export function buildHubBundle(opts: { domain?: string; network?: string; mode?: string }): {
  network: string
  mode: string
  domain: string
  cliCommand: string
  vpsOneLiner: string
  cloudInitYaml: string
  coolifyTemplateUrl: string
  helmCommand: string
} {
  const network = opts.network === 'main' ? 'main' : 'testnet'
  const mode = opts.mode === 'tunnel' ? 'tunnel' : opts.mode === 'auto-ip' ? 'auto-ip' : 'domain'
  const cleanDomain = (opts.domain ?? '')
    .trim()
    .replace(/^https?:\/\//i, '')
    .replace(/\/.*$/, '')
  const domain = cleanDomain || (mode === 'auto-ip' ? '<auto-detected-ip>.sslip.io' : 'hub.example.com')
  const mainFlag = network === 'main' ? ' --network main --confirm I_UNDERSTAND' : ''
  const cliCommand =
    mode === 'tunnel'
      ? `nektar hub up --tunnel${mainFlag}`
      : mode === 'auto-ip'
        ? `nektar hub up --auto-ip${mainFlag}`
        : `nektar hub up --domain ${domain}${mainFlag}`

  const envPrefix = mode === 'tunnel' ? `NEKTAR_HUB_MODE=tunnel` : mode === 'auto-ip' ? `NEKTAR_AUTO_IP=1` : `NEKTAR_PUBLIC_URL=https://${domain}`
  const mainEnv = network === 'main' ? ` NEKTAR_NETWORK=main NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` : ''
  const vpsOneLiner = `curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | ${envPrefix}${mainEnv} bash`

  const cloudInitYaml = `#cloud-config
# Paste into Hetzner Cloud / DigitalOcean / Vultr / AWS User Data.
# Boots Docker + GHCR nektar hub + Caddy Auto-TLS in ~90 seconds.
package_update: true
packages:
  - curl
  - ca-certificates
runcmd:
  - |
    curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/install.sh | ${envPrefix}${mainEnv} bash
`

  return {
    network,
    mode,
    domain,
    cliCommand,
    vpsOneLiner,
    cloudInitYaml,
    coolifyTemplateUrl: 'https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub/templates/coolify.compose.yaml',
    helmCommand: `helm install nektar-hub oci://ghcr.io/kryptokrates/charts/nektar-hub --set publicUrl=https://${domain}`
  }
}

export async function handleOnboardRoute(
  req: IncomingMessage,
  res: ServerResponse,
  url: URL,
  readBody: () => Promise<unknown>,
  ctx: OnboardContext
): Promise<boolean> {
  if (!url.pathname.startsWith('/v1/onboard/')) return false
  const env = ctx.env ?? process.env
  const fetchImpl = ctx.fetchImpl ?? fetch
  const send = (code: number, body: unknown) => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body))
  }

  // 1. 1-click testnet faucet drip + automatic fundingImport
  if (req.method === 'POST' && url.pathname === '/v1/onboard/faucet') {
    const net = nektarEnv('NEKTAR_NETWORK', env) ?? 'testnet'
    if (net === 'main') {
      send(403, { error: 'faucet is testnet-only — fund mainnet manually' })
      return true
    }
    const w = (await ctx.ops?.wallet?.()) as { address?: string; balanceSats?: number } | undefined
    const address = w?.address
    if (!address) {
      send(400, { error: 'no fee address available on this runtime' })
      return true
    }
    const faucetUrl = nektarEnv('NEKTAR_FAUCET_URL', env) ?? DEFAULT_FAUCET_URL
    try {
      const r = await fetchImpl(faucetUrl, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ address }),
        signal: AbortSignal.timeout(15_000)
      })
      const body = (await r.json().catch(() => ({}))) as { txid?: string; sats?: number; error?: string }
      if (!r.ok || !body.txid) {
        send(r.status === 429 ? 429 : 502, { error: body.error ?? `faucet returned HTTP ${r.status}`, address })
        return true
      }
      let imported = false
      let importError: string | undefined
      if (ctx.ops?.importFunding) {
        try {
          await ctx.ops.importFunding(body.txid)
          imported = true
        } catch (e) {
          importError = (e as Error).message
        }
      }
      ctx.mem.events.emit('change', 'funding')
      send(200, { ok: true, address, txid: body.txid, sats: body.sats ?? 0, imported, ...(importError ? { importError } : {}) })
    } catch (e) {
      send(502, { error: `faucet unreachable: ${(e as Error).message}`, address })
    }
    return true
  }

  // 2. Operator-gated starter policy preset approval
  if (req.method === 'POST' && url.pathname === '/v1/onboard/policy-preset') {
    if (!ctx.isOperator(req)) {
      send(403, { error: 'operator authentication required (NEKTAR_OPERATOR_TOKEN or operator pairing session)' })
      return true
    }
    const origin = req.headers.origin
    if (origin && !/^https?:\/\/(localhost|127\.0\.0\.1)(:\d+)?$/i.test(origin) && origin !== nektarEnv('NEKTAR_VERIFY_BASE', env)) {
      send(403, { error: 'bad origin' })
      return true
    }
    const b = (await readBody().catch(() => ({}))) as {
      preset?: unknown
      domains?: unknown
      shards?: unknown
      maxSats?: unknown
      confirm?: unknown
    }
    const preset = b.preset
    if (preset !== 'buyer' && preset !== 'seller' && preset !== 'both') {
      send(400, { error: 'preset must be buyer, seller, or both' })
      return true
    }
    const netRaw = nektarEnv('NEKTAR_NETWORK', env) ?? 'testnet'
    const network: 'regtest' | 'testnet' | 'main' = netRaw === 'main' ? 'main' : netRaw === 'regtest' ? 'regtest' : 'testnet'
    if (network === 'main' && b.confirm !== 'I_UNDERSTAND') {
      send(400, { error: 'mainnet policy approval requires confirm: "I_UNDERSTAND"' })
      return true
    }
    const agentPub = await ctx.mem.operatorSigner().pubKeyHex()
    const domains = Array.isArray(b.domains)
      ? b.domains.filter((x): x is string => typeof x === 'string' && x.trim() !== '').map((x) => x.trim().toLowerCase())
      : undefined
    const shards = Array.isArray(b.shards) ? b.shards.filter((x): x is string => typeof x === 'string' && x.trim() !== '').map((x) => x.trim()) : undefined
    const maxSats = typeof b.maxSats === 'number' && Number.isSafeInteger(b.maxSats) && b.maxSats > 0 ? b.maxSats : undefined
    const pol = starterPolicy({ preset, network, agentPub, domains, shards, maxSats })
    const err = policyApprovalError(pol)
    if (err) {
      send(400, { error: `invalid policy: ${err}` })
      return true
    }
    let hash: string
    if (ctx.mem.automation?.store) {
      hash = await ctx.mem.automation.store.approvePolicy(pol, 'dashboard:onboard')
    } else {
      const dataDir = resolve(nektarEnv('NEKTAR_DATA_DIR', env) ?? '.nektar')
      mkdirSync(dataDir, { recursive: true })
      const knexMod = (await import('knex')).default
      const kx = knexMod({ client: 'sqlite3', connection: { filename: join(dataDir, 'automation.sqlite') }, useNullAsDefault: true })
      try {
        const store = new AutomationStore(kx)
        await store.init()
        hash = await store.approvePolicy(pol, 'dashboard:onboard')
      } finally {
        await kx.destroy()
      }
    }
    ctx.mem.events.emit('change', 'automation')
    send(200, { ok: true, policyHash: hash, policyId: pol.policyId, roles: pol.roles, network })
    return true
  }

  // 3. Pre-filled Hub deployment bundle generator
  if (req.method === 'GET' && url.pathname === '/v1/onboard/hub-bundle') {
    const bundle = buildHubBundle({
      domain: url.searchParams.get('domain') ?? undefined,
      network: url.searchParams.get('network') ?? nektarEnv('NEKTAR_NETWORK', env) ?? 'testnet',
      mode: url.searchParams.get('mode') ?? undefined
    })
    send(200, bundle)
    return true
  }

  return false
}
