#!/usr/bin/env node
// tools/codex-deal.mjs — one real deal between two real Codex CLI sessions.
// Each session drives its own local nektar instance over the /mcp Streamable-
// HTTP transport (the exact endpoint Codex wires to). One direction:
// seller A sells a canary fact → buyer B posts a want → escrow → disclosure
// → settlement, all on the live testnet host plane (nektar.run) with the
// box's watch shims reached through an SSH local forward.
//
//   node tools/codex-deal.mjs [--seller alice] [--buyer bob] [--price 200]
//        [--domain codexe2e] [--no-watch] [--timeout 12] [--keep]
//
// prereqs: pnpm -r build; `codex` CLI logged in (ChatGPT or key — auth.json is
// copied into each per-session CODEX_HOME); funded keys in
// .state/testnet-agents/agent-keys.json. The market + watch URLs are the
// public nektar.run surface — the same path any external operator takes.
import { spawn, execFile, execFileSync } from 'node:child_process'
import { existsSync, mkdirSync, readFileSync, writeFileSync, rmSync, cpSync, openSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'
import { homedir } from 'node:os'
import net from 'node:net'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const argv = process.argv.slice(2)
const opt = (k, d) => (argv.includes(k) ? argv[argv.indexOf(k) + 1] : d)
const SELLER = opt('--seller', 'alice')
const BUYER = opt('--buyer', 'bob')
const PRICE = Number(opt('--price', 200))
const DOMAIN = opt('--domain', 'codexe2e')
const SHARD = opt('--shard', 'codexdeal')
const TIMEOUT_MIN = Number(opt('--timeout', 12))
const NO_WATCH = argv.includes('--no-watch')
const DISCOVERY = argv.includes('--discovery')
const KEEP = argv.includes('--keep')
const FUND_TXID = opt('--fund-tx', '')
const CANARY = `canary-${Math.random().toString(36).slice(2, 8).toUpperCase()}`
const FACT = `codex e2e canary: the magic token is ${CANARY}`
const QUESTION = 'what is the codex e2e magic token?'

const MARKET_HOSTS = 'https://nektar.run/hosts/a,https://nektar.run/hosts/b'
const WATCH_PUBLIC = 'https://nektar.run/watch/a,https://nektar.run/watch/b'

const dir = join(root, '.state', 'codex-deal')
const die = (m) => {
  console.error(`codex-deal: ${m}`)
  process.exit(1)
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))
const procs = []
let dying = false
const cleanup = () => {
  dying = true
  for (const p of procs) {
    try {
      p.kill('SIGTERM')
    } catch {}
  }
}
process.on('exit', cleanup)
process.on('SIGINT', () => process.exit(130))

const keysFile = join(root, '.state', 'testnet-agents', 'agent-keys.json')
if (!existsSync(keysFile)) die(`${keysFile} missing — no funded agent keys`)
const keys = JSON.parse(readFileSync(keysFile, 'utf8'))
if (!keys[SELLER] || !keys[BUYER]) die(`keys for ${SELLER}/${BUYER} not in agent-keys.json (${Object.keys(keys).join(', ')})`)

const requireMcp = createRequire(join(root, 'packages/mcp/package.json'))
const { PrivateKey } = requireMcp('@bsv/sdk')
const pubOf = (wif) => PrivateKey.fromWif(wif).toPublicKey().toString()

const post = async (port, tool, args) => {
  const r = await fetch(`http://127.0.0.1:${port}/v1/tools/${tool}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(args ?? {})
  })
  return r.json()
}
const up = async (port) => {
  try {
    const r = await fetch(`http://127.0.0.1:${port}/v1/tools`, { signal: AbortSignal.timeout(2000) })
    return r.ok
  } catch {
    return false
  }
}
const portFree = (port) =>
  new Promise((r) => {
    const s = net
      .createServer()
      .once('error', () => r(false))
      .once('listening', () => s.close(() => r(true)))
    s.listen(port, '127.0.0.1')
  })

const spawnLogged = (name, cmd, args, opts) => {
  const log = join(dir, `${name}.log`)
  const out = openSync(log, 'a')
  const p = spawn(cmd, args, { stdio: ['ignore', out, out], windowsHide: true, ...opts })
  procs.push(p)
  p.on('exit', (c) => console.log(`[${name}] exited ${c}`))
  return p
}

// per-session CODEX_HOME: isolated config (no inherited hooks), shared auth
const codexHome = (name, port) => {
  const h = join(dir, `codex-home-${name}`)
  mkdirSync(h, { recursive: true })
  const auth = join(homedir(), '.codex', 'auth.json')
  if (existsSync(auth)) cpSync(auth, join(h, 'auth.json'))
  writeFileSync(
    join(h, 'config.toml'),
    ['sandbox_mode = "danger-full-access"', 'approval_policy = "never"', '', '[mcp_servers.nektar]', `url = "http://127.0.0.1:${port}/mcp"`, ''].join('\n')
  )
  return h
}

const codexExec = (name, port, prompt) =>
  new Promise((res) => {
    const home = codexHome(name, port)
    const cwd = join(dir, `sess-${name}`)
    mkdirSync(cwd, { recursive: true })
    // prompt via stdin (`exec -`) — avoids every quoting hazard on Windows
    const p = spawn('codex', ['exec', '--skip-git-repo-check', '-'], {
      shell: true,
      env: { ...process.env, CODEX_HOME: home },
      cwd,
      windowsHide: true
    })
    let out = ''
    p.stdout.on('data', (d) => {
      out += d
      process.stdout.write(`[${name}] ${d}`)
    })
    p.stderr.on('data', (d) => process.stderr.write(`[${name}!] ${d}`))
    p.on('exit', (c) => res({ code: c, out }))
    p.stdin.write(prompt)
    p.stdin.end()
    procs.push(p)
  })

const run = async (cmd, args) =>
  new Promise((res, rej) => execFile(cmd, args, { cwd: root }, (e, so, se) => (e ? rej(new Error(`${cmd}: ${e.message}\n${se}`)) : res(so))))

// ---------- layout ----------
// kill stale harnesses/agents/codex from previous runs BEFORE wiping state —
// an orphan holding a tmpdir key lock, a port, or an open sqlite file either
// starves its replacement silently or EPERMs the wipe below
try {
  execFileSync('powershell', [
    '-NoProfile',
    '-Command',
    `Get-CimInstance Win32_Process | Where-Object { $_.ProcessId -ne ${process.pid} -and $_.Name -notlike 'powershell*' -and $_.Name -notlike 'bash*' -and ($_.CommandLine -like '*codex-deal.mjs*' -or $_.CommandLine -like '*mcp*bin.js*' -or $_.CommandLine -like '*sess-seller*' -or $_.CommandLine -like '*sess-buyer*') } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`
  ])
} catch {}
// file handles release seconds behind the kill on Windows — sqlite WAL,
// log fds, the dead process's bash wrapper all drain asynchronously
await sleep(4000)
rmSync(dir, { recursive: true, force: true })
mkdirSync(dir, { recursive: true })
for (const p of [8091, 8092]) if (!(await portFree(p))) die(`port ${p} already bound`)

// ---------- 2. the two instances ----------
const agentEnv = (port, dataDir, wif) => ({
  ...process.env,
  NEKTAR_NETWORK: 'testnet',
  NEKTAR_HTTP_PORT: String(port),
  NEKTAR_HTTP_HOST: '127.0.0.1',
  NEKTAR_DATA_DIR: dataDir,
  // a --price over the 1,000-sat zero-conf cap parks the demo sale until the
  // payment mines — the demo exists to show zero-conf delivery, so the cap
  // lifts to exactly the demo price
  NEKTAR_ZEROCONF_MAX_SATS: String(Math.max(PRICE, 1000)),
  NEKTAR_AUTOMATION: '1',
  NEKTAR_PAID_DISCLOSURE: '1',
  NEKTAR_WALLET: 'toolbox',
  NEKTAR_FUND_WIF: wif,
  // day-one fuse, raised for a disposable test run — tree bootstrap + escrow
  // + parallel legs in one session sail past the 20k default
  NEKTAR_MAX_SPEND_SATS_PER_DAY: '200000',
  NEKTAR_MARKET_HOSTS: MARKET_HOSTS,
  // deterministic funding: import the funding tx at boot and hold startup
  // until the book has sats — without this the first wants publish with an
  // empty reserve and die unfunded on the host plane
  ...(FUND_TXID ? { NEKTAR_FUND_TXIDS: FUND_TXID, NEKTAR_WAIT_FUNDING: '1' } : {}),
  // address-index discovery: these keys have accumulated history on-chain —
  // importing the whole spendable set keeps tree mints and the deal escrow
  // from racing a single coin (the observed funding_starved oscillation)
  // indexer discovery is opt-IN here — hex imports carry funding, and the
  // bananablocks rate limit (HTTP 429) turns discovery into pure latency
  ...(DISCOVERY ? { NEKTAR_INDEXER_DISCOVERY: '1' } : {}),
  ...(NO_WATCH ? {} : { NEKTAR_WATCH_URLS: WATCH_PUBLIC })
})
const bin = join(root, 'packages/mcp/dist/src/bin.js')
if (!existsSync(bin)) die('packages/mcp not built — run pnpm -r build first')
const dirA = join(dir, 'a')
const dirB = join(dir, 'b')
mkdirSync(dirA, { recursive: true })
mkdirSync(dirB, { recursive: true })
// cwd = the agent's own dir so bin.js never picks up the repo .env. Agents
// auto-respawn on exit: Windows/Node 24 kills long-running socket-holders
// with 0xC0000409 (AGENTS.md), and the instance's real resume path — dataDir
// state picks the op up where it left off — is exactly what should run.
const spawnAgent = (name, port, dirA_, wif) => {
  let gen = 0
  const once = () => {
    const my = ++gen
    const p = spawnLogged(name, process.execPath, [bin, '--env', 'testnet'], { env: agentEnv(port, dirA_, wif), cwd: dirA_ })
    p.on('exit', (c) => {
      if (dying || my !== gen) return
      console.log(`[${name}] exited ${c} — respawning (state persists in dataDir)`)
      setTimeout(once, 2000)
    })
  }
  once()
}
spawnAgent('agent-a', 8091, dirA, keys[SELLER])
spawnAgent('agent-b', 8092, dirB, keys[BUYER])
for (const [name, port] of [
  ['agent-a', 8091],
  ['agent-b', 8092]
]) {
  // up-wait is generous: a first-boot discovery pass grinds through the
  // agent's whole on-chain history at index pace before the HTTP bind
  for (let i = 0; i < 600 && !(await up(port)); i++) await sleep(1000)
  if (!(await up(port))) die(`${name} never came up — see ${join(dir, name + '.log')}`)
  console.log(`${name} up on :${port}`)
}

// ---------- 3. operator-approved policies ----------
const policy = (who, wif) => ({
  v: 1,
  policyId: `codex-deal-${who}`,
  network: 'testnet',
  agentPub: pubOf(wif),
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4_000_000_000_000,
  domains: [DOMAIN],
  limits: {
    perPurchaseSats: 2000,
    dailyPurchaseSats: 50_000,
    perTxFeeSats: 1200,
    dailyFeeSats: 50_000,
    runTotalSats: 200_000,
    maxLockedSats: 10_000,
    maxUnconfirmedSats: 10_000,
    maxUnconfirmedCount: 32
  },
  scoring: { minimumScoreBps: 0, weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 }, collectMs: 800 },
  seller: { minPriceSats: 50, maxPriceSats: 2000, maxPreparationsPerDay: 200, sellableShards: [SHARD] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 500, timeoutMs: 10_000 },
  sandbox: { mode: 'sandbox-only' }
})
for (const [who, wif, d] of [
  [SELLER, keys[SELLER], dirA],
  [BUYER, keys[BUYER], dirB]
]) {
  const f = join(d, 'policy.json')
  writeFileSync(f, JSON.stringify(policy(who, wif), null, 1))
  console.log(await run(process.execPath, [join(root, 'tools/automation.mjs'), 'policy', 'approve', f, '--data', d]))
}

// ---------- 3b. warmup: proven funding-slot stock before the deal ----------
// A cold agent mints its funding trees at boot (~100k sats of anchors) and
// the slots only open once those txs prove — ~1-2 testnet blocks. Starting
// codex before then burns the whole 10-minute deal window on a block wait:
// the last run funded the escrow, then sat at awaiting_disclosure because
// the seller still had zero proven slots when its window closed.
const slotsReady = async (port) => {
  const f = await post(port, 'fund', {}).catch(() => undefined)
  const s = f?.marketSlots
  // disclosure publish rides the bbox lane; want/offer ride market — a deal
  // can only complete when both lanes hold proven stock
  return s !== undefined && s.bbox > 0 && s.market > 0
}
if (!argv.includes('--no-warm')) {
  const warmDeadline = Date.now() + Number(opt('--warm-min', 45)) * 60_000
  for (;;) {
    const a = await slotsReady(8091)
    const b = await slotsReady(8092)
    if (a && b) break
    if (Date.now() > warmDeadline) die('warmup timed out — funding slots never proved (see agent logs)')
    process.stdout.write(`\rwarmup: seller ${a ? 'slots ✓' : '…'} buyer ${b ? 'slots ✓' : '…'}  `)
    await sleep(15000)
  }
  console.log('\nwarmup done — funding slots proven on both agents')
}

// ---------- 4. two Codex sessions ----------
const SELLER_PROMPT = `You are driving a nektar memory-marketplace seller through its MCP tools (server "nektar"). Do exactly these steps, then stop:
1. Call the "remember" tool: {shard:"${SHARD}", data:"${FACT}"}
2. Call the "sell" tool: {shard:"${SHARD}", domains:["${DOMAIN}"], priceSats:${PRICE}}
3. Then call "deals" with no arguments every ~30 seconds. Stop when a "sale" operation reaches a terminal state (settled, disclosure_verified, useful, refunded, or failed with a reason) or after 10 minutes.
4. Print the final deals entry verbatim as your last message.`
const BUYER_PROMPT = `You are driving a nektar memory-marketplace buyer through its MCP tools (server "nektar"). Do exactly these steps, then stop:
1. Call the "want" tool: {domains:["${DOMAIN}"], question:"${QUESTION}", maxSats:${PRICE * 2}}
2. Then call "deals" with {operationId:<the want's operationId>} every ~30 seconds until it reaches a terminal state (disclosure_verified, useful, refunded, or failed with a reason) or after 10 minutes.
3. Print the final deals entry verbatim as your last message, including result.text if present.`

console.log(`deal: "${FACT}" priced ${PRICE} sats on domain ${DOMAIN}`)
const sellerRun = codexExec('seller', 8091, SELLER_PROMPT)
await sleep(20_000) // let remember+sell land before the want publishes
const buyerRun = codexExec('buyer', 8092, BUYER_PROMPT)

// ---------- 5. authoritative verdict from the agents' own state ----------
const deadline = Date.now() + TIMEOUT_MIN * 60_000
let settled
while (Date.now() < deadline) {
  const d = await post(8092, 'deals', {}).catch(() => ({}))
  const op = (d.deals ?? []).find((o) => o.role === 'buyer')
  if (op) {
    console.log(`buyer op ${op.operationId?.slice(0, 12)}… ${op.state}`)
    if (['disclosure_verified', 'useful', 'refunded', 'failed', 'settled'].includes(op.state)) {
      settled = await post(8092, 'deals', { operationId: op.operationId }).catch(() => op)
      break
    }
  }
  await sleep(15_000)
}
await Promise.race([Promise.all([sellerRun, buyerRun]), sleep(60_000)])
const ok = settled && (settled.result?.text?.includes(CANARY) || JSON.stringify(settled.data?.result ?? {}).includes(CANARY))
console.log('\n================ VERDICT ================')
console.log(`buyer state : ${settled?.state ?? 'never reached terminal'}`)
console.log(`canary back : ${ok ? CANARY : 'not in disclosed result'}`)
console.log(`escrow      : ${settled?.escrowTxid ?? '-'}`)
console.log(`disclosure  : ${settled?.disclosureTxid ?? '-'}`)
console.log(`logs        : ${dir}`)
if (!KEEP) cleanup()
process.exit(ok ? 0 : 1)
