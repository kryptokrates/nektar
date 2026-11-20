#!/usr/bin/env node
// nektar MCP server.
//   stdio (default):      node dist/src/bin.js
//   Streamable HTTP+REST:  NEKTAR_HTTP_PORT=8080 node dist/src/bin.js
// Backend via NEKTAR_NETWORK=regtest (default, local Teranode stack) | testnet | main
// (persistent, see runtime.ts). Diagnostics go to stderr — stdout is the MCP channel.
import { nektarEnv, setNektarEnv, marketHostsFromEnv, mkLog } from '@nektar/core'
import { openRuntime, configFromEnv } from './runtime.js'
import { serveStdio } from './mcp-server.js'
import { createHttpApi } from './http.js'
import { existsSync, readFileSync } from 'node:fs'

// --env <regtest|testnet|main> + optional .env beside cwd → minimal-typing boot.
// .env lines are applied only where the var isn't already set (shell wins).
const nlog = mkLog('nektar')
const argv = process.argv.slice(2)
const envFlag = argv.includes('--env') ? argv[argv.indexOf('--env') + 1] : undefined
if (existsSync('.env')) {
  for (const line of readFileSync('.env', 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m && nektarEnv(m[1]) === undefined) setNektarEnv(m[1], m[2].trim())
  }
}
if (envFlag) {
  if (!/^(regtest|testnet|main)$/.test(envFlag)) {
    nlog(`--env must be regtest|testnet|main, got "${envFlag}"`)
    process.exit(2)
  }
  setNektarEnv('NEKTAR_NETWORK', envFlag)
}
// NEKTAR_PROFILE — one name fills the conventional defaults for a role.
// The profile only sets variables that are still unset: .env entries,
// exported vars and the flags above always win. An unknown name is a
// loud refusal — silently booting the wrong shape is how agents end up
// on mainnet with regtest assumptions (or the reverse).
const PROFILES: Record<string, Record<string, string>> = {
  'regtest-dev': {
    NEKTAR_NETWORK: 'regtest',
    NEKTAR_HTTP_PORT: '8080',
    NEKTAR_AUTOMATION: '1',
    NEKTAR_DATA_DIR: '.nektar'
  },
  'testnet-agent': {
    NEKTAR_NETWORK: 'testnet',
    NEKTAR_HTTP_PORT: '8080',
    NEKTAR_AUTOMATION: '1',
    NEKTAR_WALLET: 'toolbox'
  },
  'mainnet-agent': {
    NEKTAR_NETWORK: 'main',
    NEKTAR_HTTP_PORT: '8080',
    NEKTAR_AUTOMATION: '1',
    NEKTAR_AUTOMATION_STRICT: '1',
    NEKTAR_WALLET: 'toolbox'
  }
}
// the names people actually type — an explicit table, not fuzzy matching:
// a guessed profile would silently boot the wrong shape, which is exactly
// what the loud refusal below exists to prevent
const PROFILE_ALIASES: Record<string, string> = {
  regtest: 'regtest-dev',
  dev: 'regtest-dev',
  local: 'regtest-dev',
  testnet: 'testnet-agent',
  main: 'mainnet-agent',
  mainnet: 'mainnet-agent'
}
const profile = nektarEnv('NEKTAR_PROFILE')
if (profile !== undefined) {
  const resolved = PROFILE_ALIASES[profile] ?? profile
  const defs = PROFILES[resolved]
  if (defs === undefined) {
    nlog(`NEKTAR_PROFILE must be one of ${Object.keys(PROFILES).join('|')}, got "${profile}"`)
    process.exit(2)
  }
  if (resolved !== profile) nlog(`profile "${profile}" → "${resolved}"`)
  for (const [k, v] of Object.entries(defs)) if (nektarEnv(k) === undefined) setNektarEnv(k, v)
}
const portFlag = argv.includes('--port') ? argv[argv.indexOf('--port') + 1] : undefined
if (portFlag) setNektarEnv('NEKTAR_HTTP_PORT', portFlag)
const hostFlag = argv.includes('--host') ? argv[argv.indexOf('--host') + 1] : undefined
if (hostFlag) setNektarEnv('NEKTAR_HTTP_HOST', hostFlag)

const rt = await openRuntime(configFromEnv())
nlog(`network=${rt.network}`)
const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
if (paymentsEnabled()) {
  requireOperatorWallet()
  nlog('payments on')
}
// the effective market-host set — NEKTAR_MARKET_HOSTS when set, else the
// baked-in seed hosts for this network (the zero-config join's Layer 0)
const mh = marketHostsFromEnv()
const { callTool } = await import('./server.js')
// zero-config join, Layer 2 — seed packs from operator announces on the
// dir lane. Independent of NEKTAR_AUTOMATION: the path is host reads +
// re-verified pack imports, no lane writes. Fire-and-forget — a dead seed
// degrades to an empty start, never a boot failure.
const seedDomains = (nektarEnv('NEKTAR_SEED_DOMAINS') ?? '')
  .split(',')
  .map((s) => s.trim().toLowerCase())
  .filter(Boolean)
if (seedDomains.length > 0 && mh.length > 0) {
  const { seedFromDirectory } = await import('./seed.js')
  void seedFromDirectory({
    mem: rt.mem,
    call: (t, a) => callTool(rt.mem, t, a, { ops: rt.ops }),
    hosts: mh,
    net: rt.network,
    domains: seedDomains.filter((d) => d !== '*'),
    all: seedDomains.includes('*'),
    allowHttp: rt.network === 'regtest' || nektarEnv('NEKTAR_MARKET_LOCAL_HOSTS') === '1',
    log: nlog
  })
    .then((r) =>
      nlog(`seed: ${r.imported} imported, ${r.skipped} already present, ${r.failed} failed from ${r.announces} announce(s) (+${r.hostsAdded} host(s))`)
    )
    .catch((e) => nlog(`seed: ${(e as Error).message}`))
}
if (/^(1|true|yes)$/i.test(nektarEnv('NEKTAR_AUTOMATION') ?? '')) {
  const { openAutomation } = await import('@nektar/automation')
  // unattended mode is honored ONLY behind a valid conformance token —
  // it implies the strict gate so a red preflight refuses to start
  const unattended = /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_UNATTENDED') ?? '')
  const strict = unattended || /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_AUTOMATION_STRICT') ?? '')
  // the transport knows the broadcast endpoint — an HTTP answer proves the
  // socket where a txFacts 'pending' cannot
  const broadcasterProbe = async (): Promise<boolean> => {
    try {
      if (rt.network === 'regtest') {
        const url = nektarEnv('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx'
        await fetch(url, { method: 'POST', body: '', signal: AbortSignal.timeout(3_000) })
        return true // any HTTP answer = endpoint up
      }
      const base = nektarEnv('NEKTAR_BROADCASTER_URL') ?? 'https://arcade.gorillapool.io'
      await fetch(`${base}/tx/${'00'.repeat(32)}`, { signal: AbortSignal.timeout(5_000) })
      return true
    } catch {
      return false
    }
  }
  rt.mem.automation = await openAutomation(rt.mem, {
    network: rt.network,
    dataDir: nektarEnv('NEKTAR_DATA_DIR') ?? '.nektar',
    marketHosts: mh,
    localHosts: nektarEnv('NEKTAR_MARKET_LOCAL_HOSTS') === '1',
    boxOffice: nektarEnv('NEKTAR_BBOX_OFFICE') ?? undefined,
    logStream: nektarEnv('NEKTAR_LOG_STREAM') ?? undefined,
    strict,
    unattended,
    broadcasterProbe,
    // the guarded broadcaster's day ledger — the doctor's 'budget' check
    // reads today's committed spend + caps and splits upkeep vs deal sats
    spendProbe: async () => {
      const w = (await rt.ops?.wallet?.()) as { spentTodaySats?: number; caps?: { maxPerDay?: number; maxFeePerTx?: number } } | undefined
      if (w === undefined) return undefined
      return {
        spentTodaySats: w.spentTodaySats ?? 0,
        maxPerDay: w.caps?.maxPerDay ?? Number.MAX_SAFE_INTEGER,
        maxFeePerTx: w.caps?.maxFeePerTx ?? Number.MAX_SAFE_INTEGER,
        costs: (rt.ops?.costs?.() ?? []) as Array<{ txid: string; spend: number; at: string }>
      }
    },
    publishFinger: /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_BFINGER_PUBLISH') ?? ''),
    // NEKTAR_ANNOUNCE=1 — publish the directory card (hosts + offered seed
    // packs) on the dir lane so fresh instances can enumerate this operator
    announce: /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_ANNOUNCE') ?? ''),
    watchdogMs: nektarEnv('NEKTAR_WATCHDOG_MS') ? Number(nektarEnv('NEKTAR_WATCHDOG_MS')) : undefined,
    intervalMs: Number(nektarEnv('NEKTAR_AUTOMATION_INTERVAL_MS') ?? 1000),
    watchHosts: (nektarEnv('NEKTAR_WATCH_URLS') ?? '')
      .split(',')
      .map((u) => u.trim())
      .filter((u) => u !== ''),
    // our own shim's public URL — goes into the bfinger record so peers can
    // /notify our long-polls on outbound sends (needs NEKTAR_BFINGER_PUBLISH)
    watchPublic: nektarEnv('NEKTAR_WATCH_PUBLIC') !== undefined && nektarEnv('NEKTAR_WATCH_PUBLIC') !== '' ? nektarEnv('NEKTAR_WATCH_PUBLIC') : undefined,
    // our own push base URL — goes into the bfinger record so peers POST
    // carriers to /v1/lane instead of waiting for our next host read
    pushPublic: nektarEnv('NEKTAR_PUSH_PUBLIC') !== undefined && nektarEnv('NEKTAR_PUSH_PUBLIC') !== '' ? nektarEnv('NEKTAR_PUSH_PUBLIC') : undefined,
    // per-read failure ceiling on host lookups — a host slower than this is
    // unreachable for the pass, never a wait on the hot path
    laneReadTimeoutMs: nektarEnv('NEKTAR_LANE_READ_TIMEOUT_MS') ? Number(nektarEnv('NEKTAR_LANE_READ_TIMEOUT_MS')) : undefined,
    // hot-path cadence while any deal is live — wakes and inbound items hold
    // it; silence falls back to intervalMs
    hotMs: nektarEnv('NEKTAR_HOT_INTERVAL_MS') ? Number(nektarEnv('NEKTAR_HOT_INTERVAL_MS')) : undefined,
    // the smallest collecting window before an acceptable offer may
    // early-select — a cheaper offer inside the window still wins; collectMs
    // remains the outer ceiling
    minCollectMs: nektarEnv('NEKTAR_MIN_COLLECT_MS') ? Number(nektarEnv('NEKTAR_MIN_COLLECT_MS')) : undefined,
    // opt-in: the collect window stays open while offers keep arriving and
    // closes collectQuietMs after the last one (collectMs stays the ceiling)
    collectQuietMs: nektarEnv('NEKTAR_COLLECT_QUIET_MS') ? Number(nektarEnv('NEKTAR_COLLECT_QUIET_MS')) : undefined,
    // extra want lifetime beyond the collection window — the buyer republishes
    // a still-live want at each deadline instead of expiring; a seller under
    // load that missed the first window can still answer inside exp
    wantExtendMs: nektarEnv('NEKTAR_WANT_EXTEND_MS') ? Number(nektarEnv('NEKTAR_WANT_EXTEND_MS')) : undefined,
    // a deliver leg that never settles retries transport-classed instead of
    // parking its outbox worker — bound covers the worst legit two-leg send
    deliverBoundMs: nektarEnv('NEKTAR_DELIVER_BOUND_MS') ? Number(nektarEnv('NEKTAR_DELIVER_BOUND_MS')) : undefined,
    // upkeep keeps this many medium self-pay outputs on hand so a tree mint
    // and a deal escrow never contend for one dominant UTXO
    reserveOuts: nektarEnv('NEKTAR_RESERVE_OUTS') ? Number(nektarEnv('NEKTAR_RESERVE_OUTS')) : undefined,
    reserveOutSats: nektarEnv('NEKTAR_RESERVE_OUT_SATS') ? Number(nektarEnv('NEKTAR_RESERVE_OUT_SATS')) : undefined,
    // change outputs a mint fans its remainder across — depth-1 ancestry for
    // the next few spends instead of a serial mint-to-mint chain
    changeFanout: nektarEnv('NEKTAR_CHANGE_FANOUT') ? Number(nektarEnv('NEKTAR_CHANGE_FANOUT')) : undefined,
    // a settlement proof must sit this deep before a completion settles —
    // the same finality depth the memory applies to anchor queries
    settleDepth: rt.mem.finalityDepth,
    // chains without a status push (polled broadcasters) mark
    // funding trees proven only here — let ops tune the cadence
    upkeepMs: nektarEnv('NEKTAR_AUTOMATION_UPKEEP_MS') ? Number(nektarEnv('NEKTAR_AUTOMATION_UPKEEP_MS')) : undefined,
    call: (tool, args) => callTool(rt.mem, tool, args, { ops: rt.ops }),
    opts: {
      ...(rt.network === 'main' ? { saleMaxFee: 500 } : {}),
      ...(nektarEnv('NEKTAR_OFFER_TTL_BLOCKS') ? { offerTtlBlocks: Number(nektarEnv('NEKTAR_OFFER_TTL_BLOCKS')) } : {}),
      ...(nektarEnv('NEKTAR_REFUND_BLOCKS') ? { refundBlocks: Number(nektarEnv('NEKTAR_REFUND_BLOCKS')) } : {}),
      // the direct rail — NEKTAR_DIRECT_MAX_SATS caps buyer-side outright
      // payments (payment-final, no refund); NEKTAR_DIRECT_ACCEPT=1 advertises
      // it on our quotes. Both off by default: escrow stays the default rail.
      ...(nektarEnv('NEKTAR_DIRECT_MAX_SATS') ? { directMaxSats: Number(nektarEnv('NEKTAR_DIRECT_MAX_SATS')) } : {}),
      ...(nektarEnv('NEKTAR_DIRECT_ACCEPT') === '1' ? { directAccept: true } : {}),
      // NEKTAR_DIRECT_ACCEPT_MAX_SATS caps which quotes advertise the direct
      // rail — disclosure ships against an unproven payment, so this bounds
      // the seller's no-recourse exposure (unset = any price)
      ...(nektarEnv('NEKTAR_DIRECT_ACCEPT_MAX_SATS') ? { directAcceptMaxSats: Number(nektarEnv('NEKTAR_DIRECT_ACCEPT_MAX_SATS')) } : {}),
      // NEKTAR_AUTO_USEFUL=1 — a verified disclosure closes the deal as
      // `useful` without waiting on an operator markUsefulness call
      ...(nektarEnv('NEKTAR_AUTO_USEFUL') === '1' ? { autoUseful: true } : {})
    },
    log: nlog
  })
  nlog(`automation on — ${mh.length} market host(s); policies and approvals via 'pnpm automation'`)
  if (mh.length === 0) nlog('automation: no market hosts (set NEKTAR_MARKET_HOSTS) — wants and offers cannot be published')
}
const port = nektarEnv('NEKTAR_HTTP_PORT')
let srv: ReturnType<typeof createHttpApi> | undefined
if (port) {
  srv = createHttpApi(rt.mem, rt.ops)
  const host = nektarEnv('NEKTAR_HTTP_HOST') ?? '127.0.0.1'
  // fail-closed: a non-loopback bind with no bearer exposes every tool and
  // operator route unauthenticated — refuse unless the operator explicitly
  // opts into open serving
  const loopbackBind = host === '127.0.0.1' || host === 'localhost' || host === '::1'
  if (!loopbackBind && nektarEnv('NEKTAR_HTTP_TOKEN') === undefined && !nektarEnv('NEKTAR_INSECURE_NO_AUTH'))
    throw new Error(
      `refusing to bind ${host}:${port} without NEKTAR_HTTP_TOKEN — the tool surface would be unauthenticated (set NEKTAR_INSECURE_NO_AUTH=1 to override)`
    )
  // a crashing-then-respawned agent briefly loses its port to the dying
  // process (WER holds it on Windows); retry the bind on a bounded ceiling
  // instead of letting an unhandled 'error' event kill the whole agent
  let tries = 0
  const server = srv
  server.on('error', (err: NodeJS.ErrnoException) => {
    if (err.code === 'EADDRINUSE' && tries < 15) {
      tries++
      setTimeout(() => server.listen(Number(port), host), 1000)
    } else {
      throw err
    }
  })
  server.listen(Number(port), host, () => nlog(`HTTP + MCP on :${port} (/v1, /mcp)`))
}
let stopping = false
const stop = async () => {
  if (stopping) return
  stopping = true
  // the whole teardown is bounded: a wedged close must not hang the
  // process — on Windows a lingering socket keeps the port hostage for
  // the respawned agent, and open sockets at process.exit trip the libuv
  // teardown assert (0xC0000409). Force-exit past the deadline.
  setTimeout(() => {
    nlog('stop deadline exceeded — forcing exit')
    process.exit(1)
  }, 20_000).unref()
  if (srv !== undefined) {
    srv.close() // stop accepting; in-flight requests finish
    srv.closeIdleConnections() // sever idle keep-alives so the socket count drains
  }
  await rt.mem.automation?.stop().catch(() => {})
  await rt.close().catch(() => {})
  process.exit(0)
}
process.on('SIGINT', () => {
  void stop()
})
process.on('SIGTERM', () => {
  void stop()
})
if (!port) {
  await serveStdio(rt.mem, rt.ops)
  // the MCP client went away → exit (otherwise `docker run -i` containers leak
  // and several instances end up sharing one data volume)
  process.stdin.on('end', () => {
    void stop()
  })
  process.stdin.on('close', () => {
    void stop()
  })
}
