// mainnet-check.ts — zero-sat dry-run mainnet readiness verifier (`nektar check-mainnet`).
// Probes security/custody config, live mainnet Chaintracks + indexer witness
// agreement, broadcaster /policy fee rate vs SpendGuard fee caps, overlay host
// reachability, and local cache/DB health without moving any sats.
import { existsSync, readFileSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { ioSignal, nektarEnv, nektarEnvFlag, nektarEnvNum, writeOut, type EnvSource } from '@nektar/core'
import { ChaintracksTracker, IndexTracker, fetchBroadcasterFeeRate } from '@nektar/chain'
import { marketHosts } from './hosts.js'
import { probeWalletSubstrate } from './rtsupport.js'

interface MainnetCheckItem {
  name: string
  status: 'pass' | 'warn' | 'fail'
  detail: string
  fix?: string
}

export interface MainnetCheckResult {
  ok: boolean
  checks: MainnetCheckItem[]
}

export interface MainnetCheckOpts {
  env?: EnvSource
  fetchImpl?: typeof fetch
  dataDir?: string
  out?: (s: string) => void
}

const MAINNET_BROADCASTERS = ['https://arcade.gorillapool.io', 'https://arcade-v2-us-1.bsvblockchain.tech']
const MAINNET_CHAINTRACKS = 'https://arcade-v2-us-1.bsvblockchain.tech/chaintracks/v2'
const MAINNET_INDEXERS = ['https://bananablocks.com/api/v1', 'https://api.whatsonchain.com/v1/bsv/main']

export async function runMainnetCheck(opts: MainnetCheckOpts = {}): Promise<MainnetCheckResult> {
  const env = opts.env ?? process.env
  const fetchImpl = opts.fetchImpl ?? fetch
  const dataDir = resolve(opts.dataDir ?? nektarEnv('NEKTAR_DATA_DIR', env) ?? '.nektar')
  const checks: MainnetCheckItem[] = []

  // 1. Explicit mainnet safety opt-in
  const confirm = nektarEnv('NEKTAR_MAINNET_CONFIRM', env)
  checks.push(
    confirm === 'I_UNDERSTAND'
      ? { name: 'mainnet-confirm', status: 'pass', detail: 'NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND set' }
      : {
          name: 'mainnet-confirm',
          status: 'fail',
          detail: 'NEKTAR_MAINNET_CONFIRM is not set to I_UNDERSTAND',
          fix: 'export NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND before booting mainnet'
        }
  )

  // 2. Custody & key encryption at rest
  const walletMode = nektarEnv('NEKTAR_WALLET', env) ?? 'toolbox'
  if (walletMode === 'client') {
    const sub = await probeWalletSubstrate(fetchImpl)
    if (sub === 'live') {
      checks.push({ name: 'custody', status: 'pass', detail: 'NEKTAR_WALLET=client — BRC-100 substrate live on localhost:3321 (zero local private keys)' })
    } else {
      checks.push({
        name: 'custody',
        status: 'fail',
        detail: sub === 'locked' ? 'NEKTAR_WALLET=client — BSV Desktop vault is LOCKED' : 'NEKTAR_WALLET=client — no BRC-100 substrate on localhost:3321',
        fix: sub === 'locked' ? 'unlock BSV Desktop before starting nektar' : 'start BSV Desktop unlocked, or switch to NEKTAR_WALLET=toolbox'
      })
    }
  } else if (walletMode === 'toolbox') {
    const hasPass = nektarEnv('NEKTAR_KEY_PASSPHRASE', env) !== undefined || nektarEnv('NEKTAR_KEY_PASSPHRASE_FILE', env) !== undefined
    checks.push(
      hasPass
        ? { name: 'custody', status: 'pass', detail: 'NEKTAR_WALLET=toolbox — passphrase configured for encrypted mainnet keys at rest' }
        : {
            name: 'custody',
            status: 'fail',
            detail: 'NEKTAR_WALLET=toolbox on mainnet requires NEKTAR_KEY_PASSPHRASE (or _FILE)',
            fix: 'set NEKTAR_KEY_PASSPHRASE (or NEKTAR_KEY_PASSPHRASE_FILE), or switch to NEKTAR_WALLET=client'
          }
    )
  } else {
    checks.push({
      name: 'custody',
      status: 'fail',
      detail: `unsupported NEKTAR_WALLET="${walletMode}"`,
      fix: 'set NEKTAR_WALLET=toolbox or NEKTAR_WALLET=client'
    })
  }

  // 3. Callback & HTTP auth tokens
  const cbUrl = nektarEnv('NEKTAR_CALLBACK_URL', env)
  const cbTok = nektarEnv('NEKTAR_CALLBACK_TOKEN', env)
  if (cbUrl !== undefined && cbTok === undefined) {
    checks.push({
      name: 'callback-token',
      status: 'fail',
      detail: 'NEKTAR_CALLBACK_URL is set without NEKTAR_CALLBACK_TOKEN (live callbacks return 403)',
      fix: 'set NEKTAR_CALLBACK_TOKEN to a strong random hex secret'
    })
  } else {
    checks.push({
      name: 'callback-token',
      status: 'pass',
      detail: cbTok !== undefined ? 'NEKTAR_CALLBACK_TOKEN configured' : 'no webhook URL set — outbound SSE stream uses auto-persisted token'
    })
  }

  const httpHost = nektarEnv('NEKTAR_HTTP_HOST', env) ?? '127.0.0.1'
  const loopback = httpHost === '127.0.0.1' || httpHost === 'localhost' || httpHost === '::1'
  if (!loopback && nektarEnv('NEKTAR_HTTP_TOKEN', env) === undefined) {
    checks.push({
      name: 'http-auth',
      status: 'fail',
      detail: `NEKTAR_HTTP_HOST=${httpHost} is non-loopback without NEKTAR_HTTP_TOKEN`,
      fix: 'set NEKTAR_HTTP_TOKEN or bind NEKTAR_HTTP_HOST=127.0.0.1'
    })
  } else {
    checks.push({ name: 'http-auth', status: 'pass', detail: loopback ? `loopback bind (${httpHost})` : `non-loopback bind (${httpHost}) with bearer token` })
  }

  // 4. Live mainnet header sources (2-of-2 primary Chaintracks + witness indexer)
  const ctUrl = nektarEnv('NEKTAR_CHAINTRACKS_URL', env) ?? MAINNET_CHAINTRACKS
  const idxUrl = nektarEnv('NEKTAR_INDEXER_URL', env) ?? nektarEnv('NEKTAR_WOC_URL', env) ?? MAINNET_INDEXERS[0]
  const idxKey = nektarEnv('NEKTAR_INDEXER_API_KEY', env) ?? nektarEnv('NEKTAR_WOC_API_KEY', env)
  const requirePow = nektarEnvFlag('NEKTAR_HEADER_POW', env) !== false
  const mc = {
    broadcasterUrl: nektarEnv('NEKTAR_BROADCASTER_URL', env) ?? MAINNET_BROADCASTERS[0],
    broadcasterUrls: [
      ...(nektarEnv('NEKTAR_BROADCASTER_URLS', env)
        ?.split(',')
        .map((s) => s.trim().replace(/\/+$/, ''))
        .filter(Boolean) ?? []),
      ...MAINNET_BROADCASTERS
    ],
    broadcasterApiKey: nektarEnv('NEKTAR_BROADCASTER_API_KEY', env),
    indexerUrl: idxUrl,
    indexerUrls: MAINNET_INDEXERS.filter((u) => u !== idxUrl),
    indexerApiKey: idxKey,
    fetchImpl
  }
  try {
    const ct = new ChaintracksTracker(mc, ctUrl, undefined, { requirePow })
    const ix = new IndexTracker(mc, { requirePow })
    const [h1, h2] = await Promise.all([ct.currentHeight().catch(() => undefined), ix.currentHeight().catch(() => undefined)])
    if (h1 !== undefined && h2 !== undefined) {
      const diff = Math.abs(h1 - h2)
      checks.push(
        diff <= 2
          ? { name: 'headers', status: 'pass', detail: `2-of-2 header sources live (Chaintracks=${h1}, Indexer=${h2})` }
          : {
              name: 'headers',
              status: 'warn',
              detail: `header tip drift: Chaintracks=${h1} vs Indexer=${h2} (diff=${diff})`,
              fix: 'verify NEKTAR_CHAINTRACKS_URL and NEKTAR_INDEXER_URL point to BSV mainnet'
            }
      )
    } else {
      checks.push({
        name: 'headers',
        status: 'fail',
        detail: `header corroboration incomplete (Chaintracks=${h1 ?? 'down'}, Indexer=${h2 ?? 'down'})`,
        fix: 'strict 2-of-2 SPV requires both primary Chaintracks and witness indexer reachable'
      })
    }
  } catch (e) {
    checks.push({ name: 'headers', status: 'fail', detail: `header probe failed: ${(e as Error).message}` })
  }

  // 5. Live mainnet broadcaster fee quote vs SpendGuard caps
  const maxFeePerTx = nektarEnvNum('NEKTAR_MAX_FEE_SATS_PER_TX', env) ?? 1000
  try {
    const liveRate = await fetchBroadcasterFeeRate(mc)
    const minCovenantFee = Math.ceil(4 * Math.max(100, liveRate))
    if (maxFeePerTx < minCovenantFee) {
      checks.push({
        name: 'broadcaster-fees',
        status: 'fail',
        detail: `live fee rate=${liveRate} sat/kB (~${minCovenantFee} sats for 4kB covenant), exceeds NEKTAR_MAX_FEE_SATS_PER_TX=${maxFeePerTx}`,
        fix: `raise NEKTAR_MAX_FEE_SATS_PER_TX to at least ${Math.max(1000, minCovenantFee * 2)}`
      })
    } else {
      checks.push({
        name: 'broadcaster-fees',
        status: 'pass',
        detail: `broadcaster /policy live (${liveRate} sat/kB; NEKTAR_MAX_FEE_SATS_PER_TX=${maxFeePerTx} covers covenant txs)`
      })
    }
  } catch (e) {
    checks.push({
      name: 'broadcaster-fees',
      status: 'fail',
      detail: `broadcaster /policy unreachable: ${(e as Error).message}`,
      fix: 'check NEKTAR_BROADCASTER_URL / NEKTAR_BROADCASTER_URLS connectivity'
    })
  }

  // 6. Overlay hosts (required for multi-agent market mode; optional for solo memory)
  const automationOn = nektarEnvFlag('NEKTAR_AUTOMATION', env) === true
  const hosts = marketHosts({ ...env, NEKTAR_NETWORK: 'main' })
  if (hosts.length === 0) {
    checks.push({
      name: 'market-hosts',
      status: automationOn ? 'warn' : 'pass',
      detail: automationOn
        ? 'NEKTAR_AUTOMATION=1 with 0 mainnet market hosts configured (solo/single-player only)'
        : 'solo memory mode (no overlay hosts required)',
      ...(automationOn ? { fix: 'set NEKTAR_MARKET_HOSTS=https://host-a,https://host-b to enable multi-agent deals' } : {})
    })
  } else {
    const results = await Promise.all(
      hosts.map(async (h) => {
        try {
          const r = await fetchImpl(`${h}/lookup`, {
            method: 'POST',
            headers: { 'content-type': 'application/json' },
            body: JSON.stringify({ service: 'ls_nektar', query: { wants: { net: 'main', domain: '_preflight', limit: 1 } } }),
            signal: ioSignal(5_000)
          })
          return { h, ok: r.status < 500, detail: `HTTP ${r.status}` }
        } catch (e) {
          return { h, ok: false, detail: (e as Error).message }
        }
      })
    )
    const down = results.filter((x) => !x.ok)
    checks.push(
      down.length === 0
        ? { name: 'market-hosts', status: 'pass', detail: `${hosts.length}/${hosts.length} mainnet overlay host(s) answered` }
        : {
            name: 'market-hosts',
            status: 'fail',
            detail: `${down.length}/${hosts.length} host(s) unreachable: ${down.map((d) => `${d.h} (${d.detail})`).join(', ')}`,
            fix: 'restore unreachable hosts or update NEKTAR_MARKET_HOSTS'
          }
    )
  }

  // 7. Local state health (header cache + lanes.sqlite size)
  const hdrFile = join(dataDir, 'headers-cache.json')
  let hdrCount = 0
  if (existsSync(hdrFile)) {
    try {
      const parsed = JSON.parse(readFileSync(hdrFile, 'utf8')) as unknown
      if (Array.isArray(parsed)) hdrCount = parsed.length
    } catch {
      /* corrupt cache */
    }
  }
  const lanesFile = join(dataDir, 'lanes.sqlite')
  const lanesMb = existsSync(lanesFile) ? statSync(lanesFile).size / (1024 * 1024) : 0
  checks.push(
    lanesMb > 25
      ? {
          name: 'local-state',
          status: 'warn',
          detail: `dataDir=${dataDir} (headersCache=${hdrCount}, lanes.sqlite=${lanesMb.toFixed(1)} MB)`,
          fix: 'lanes.sqlite exceeds 25 MB — daily prune will compact exhausted trees on next upkeep'
        }
      : {
          name: 'local-state',
          status: 'pass',
          detail: `dataDir=${dataDir} (headersCache=${hdrCount}, lanes.sqlite=${lanesMb.toFixed(1)} MB)`
        }
  )

  return { ok: checks.every((c) => c.status !== 'fail'), checks }
}

export async function runMainnetCheckCli(argv: string[], opts: MainnetCheckOpts = {}): Promise<number> {
  const out = opts.out ?? writeOut
  const asJson = argv.includes('--json')
  const res = await runMainnetCheck(opts)
  if (asJson) {
    out(JSON.stringify(res, null, 2) + '\n')
  } else {
    out(`nektar check-mainnet: ${res.ok ? 'READY' : 'NOT READY'}\n`)
    for (const c of res.checks) {
      const tag = c.status === 'pass' ? 'PASS' : c.status === 'warn' ? 'WARN' : 'FAIL'
      out(`  [${tag}] ${c.name}: ${c.detail}\n`)
      if (c.fix) out(`         fix: ${c.fix}\n`)
    }
  }
  return res.ok ? 0 : 1
}
