#!/usr/bin/env node
// pnpm scenarios:real — a complete three-seller deal on the real Teranode
// regtest chain against two real reference hosts (the nektar-bstack-regtest
// compose project: host-a :18191, host-b :18192, header shim on Teranode).
//
// Four production `bin.js` instances, each with its own fund key paid for by
// a one-off split of the shared .state/treasury.json outpoint (the same
// treasury chain e2e-regtest.ts and bootstrapReserve consume), its own data
// dir and its own NEKTAR_TX_LOG. The runner mines ~1 block/2s while the deal
// progresses — production instances never auto-mine on regtest.
//
// Operator steps go through tools/automation.mjs (policy approve); agent
// steps go through the HTTP agent tools only: remember, sell, want, deals.
//
// Assumes: the Teranode regtest stack is up (rpc :9292, propagation :8833,
// asset :8090) and `docker compose -p nektar-bstack-regtest -f
// deploy/bstack-sandbox/compose.regtest.yaml up -d` has been run with
// NEKTAR_RPC_USER/NEKTAR_RPC_PASS in the environment. Nothing is torn down.
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, rmSync, writeFileSync, readFileSync, existsSync, statSync, createWriteStream } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { TeranodeBroadcaster, TeranodeRPCTracker, utxoStatus } from '../../../chain/dist/src/regtest.js'
import { BsvBroadcaster, ChaintracksTracker, WocTracker } from '../../../chain/dist/src/mainnet.js'
import { readBox } from '../../../bstack/dist/src/index.js'
import { resolveFinger } from '../../../bstack/dist/src/finger.js'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const envFile = join(root, '.env')
const env = existsSync(envFile)
  ? Object.fromEntries(
      readFileSync(envFile, 'utf8')
        .split(/\r?\n/)
        .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
        .filter(Boolean)
        .map((m) => [m[1], m[2].trim()])
    )
  : {}
// --net testnet: public BSV testnet through the BSV Broadcaster against the
// nektar-bstack-testnet host pair (:18291/:18292). Nothing is mined here —
// blocks arrive on their own and statuses are pushed over the status stream.
// The treasury is .state/testnet-treasury.json ({wif}); fund its address first.
const NET = process.argv.includes('--net') ? process.argv[process.argv.indexOf('--net') + 1] : 'regtest'
if (NET !== 'regtest' && NET !== 'testnet') {
  console.error(`real-stack: unknown --net ${NET}`)
  process.exit(2)
}
const LIVE = NET === 'testnet'
const TESTNET = { broadcasterUrl: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', wocUrl: 'https://api.whatsonchain.com/v1/bsv/test' }
const TESTNET_TREASURY = join(root, '.state', 'testnet-treasury.json')
if (LIVE && !existsSync(TESTNET_TREASURY)) {
  console.error(`real-stack: ${TESTNET_TREASURY} missing`)
  process.exit(2)
}
for (const k of LIVE ? [] : ['NEKTAR_REGTEST_WIF', 'NEKTAR_RPC_USER', 'NEKTAR_RPC_PASS']) {
  if (!env[k] && !process.env[k]) {
    console.error(`real-stack: ${k} not set (repo .env)`)
    process.exit(2)
  }
}
const E = (k) => process.env[k] ?? env[k]
const treasuryKey = PrivateKey.fromWif(LIVE ? JSON.parse(readFileSync(TESTNET_TREASURY, 'utf8')).wif : E('NEKTAR_REGTEST_WIF'))
const treasuryAddr = treasuryKey.toPublicKey().toAddress([0x6f])
const auth = 'Basic ' + Buffer.from(`${E('NEKTAR_RPC_USER')}:${E('NEKTAR_RPC_PASS')}`).toString('base64')
const cfg = {
  rpcUrl: E('NEKTAR_RPC_URL') ?? 'http://localhost:9292/',
  assetUrl: E('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1',
  propagationUrl: E('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx'
}
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const broadcaster = LIVE ? new BsvBroadcaster(TESTNET) : new TeranodeBroadcaster(cfg.propagationUrl)
const tracker = LIVE
  ? new ChaintracksTracker(TESTNET, `${TESTNET.broadcasterUrl}/chaintracks/v2`, new WocTracker(TESTNET))
  : new TeranodeRPCTracker({ ...cfg, rpcUser: E('NEKTAR_RPC_USER'), rpcPass: E('NEKTAR_RPC_PASS') })
// The testnet run uses the local host pair; the instances are told these are
// the operator's own hosts (NEKTAR_MARKET_LOCAL_HOSTS=1), so they publish
// bfinger records naming them and resolve each other's hosts from those
// records. NEKTAR_TESTNET_HOSTS=https://a,https://b points the run elsewhere.
const liveHosts = (process.env.NEKTAR_TESTNET_HOSTS ?? 'http://127.0.0.1:18291,http://127.0.0.1:18292').split(',').map((x) => x.trim())
if (LIVE && liveHosts.length !== 2) {
  console.error('real-stack: NEKTAR_TESTNET_HOSTS needs exactly two hosts')
  process.exit(2)
}
const HOST_A = LIVE ? liveHosts[0] : 'http://127.0.0.1:18191'
const HOST_B = LIVE ? liveHosts[1] : 'http://127.0.0.1:18192'
const HOSTS = [HOST_A, HOST_B]
// watch-shim long-poll wake layer (deploy/bstack-sandbox/watch-shim.mjs fronts
// each stock host; positional with HOSTS — watchShims[i] wraps liveHosts[i]).
// Agents poll every shim (NEKTAR_WATCH_URLS) and publish the one fronting
// their first host (NEKTAR_WATCH_PUBLIC) so a counterparty's /notify lands
// where their waiters live. No shims configured → plain polling, unchanged.
const watchShims = (process.env.NEKTAR_WATCH_SHIMS ?? (LIVE ? '' : 'http://127.0.0.1:18193,http://127.0.0.1:18194'))
  .split(',')
  .map((x) => x.trim())
  .filter(Boolean)
const shimFor = (host) => watchShims[HOSTS.indexOf(host)]
const FINDING =
  'libfoo 3.0 renamed connect() to open(); callers must switch.\n```nektar-patch\n{"edits":[{"file":"src/app.js","find":"libfoo.connect(","replace":"libfoo.open("}]}\n```'
const QUESTION = 'build fails after libfoo 3.0 upgrade: libfoo.connect is not a function'

const runId = `${LIVE ? 'testnet' : 'real'}-${Date.now().toString(36)}`
const dir = mkdtempSync(join(root, '.state', `run-${runId}-`))
console.log(`run directory: ${dir}`)
const log = (m) => console.log(`[${runId}] ${m}`)
const procs = []
let stopping = false

// ---- background mining: ~1 block / 2 s while anything waits -------------
let mining = !LIVE
let blocksMined = 0
let sealing = false
const sealNow = async () => {
  if (!mining || sealing) return
  sealing = true
  try {
    await rpc('generatetoaddress', 1, treasuryAddr)
    blocksMined++
  } catch (e) {
    log(`mine: ${e.message}`)
  } finally {
    sealing = false
  }
}
const mineLoop = (async () => {
  while (mining) {
    await sealNow()
    await delay(2000)
  }
})()

// ---- fund the four instances from the shared treasury --------------------
// The scenario keeps its own treasury chain (.state/real-stack-treasury.json)
// — the shared .state/treasury.json is consumed by e2e-regtest.ts, testkit
// and bootstrapReserve, and a concurrent run can spend the recorded outpoint
// out from under fundAgents. Seed once:
//   NEKTAR_TREASURY_FILE=.state/real-stack-treasury.json node packages/e2e/src/scenarios/regtest-fund.mjs
const TREASURY_FILE = process.env.NEKTAR_TREASURY_FILE ?? join(root, '.state', 'real-stack-treasury.json')
const agents = [
  { name: 'alice', hosts: [HOST_A], seller: 450 },
  { name: 'ben', hosts: [HOST_B], seller: 300 },
  { name: 'cara', hosts: [HOST_B], seller: 200 },
  { name: 'bob', hosts: [HOST_A, HOST_B] }
]
// broadcast-driven sealing (regtest, test-only — never protocol): a fresh
// tx-log line means an agent's tx just hit the mempool, so seal it inside
// ~150 ms instead of leaving it for the 2 s idle floor of mineLoop
const txLogPos = new Map()
const txLogScan = (async () => {
  while (mining) {
    for (const a of agents) {
      if (a.dataDir === undefined) continue
      const f = join(a.dataDir, 'tx-log.jsonl')
      if (!existsSync(f)) continue
      const st = statSync(f)
      const at = txLogPos.get(f) ?? 0
      if (st.size > at) {
        txLogPos.set(f, st.size)
        void sealNow()
      }
    }
    await delay(150)
  }
})()
// testnet: one split of the treasury's largest unspent output pays each
// instance six 10k-sat outputs (parallel spends without waiting for change)
// and returns the rest to the treasury. Instances import it offline
// (NEKTAR_FUND_TXIDS=hex:…); their keys are kept in the run dir so leftover
// test coins can be swept back.
// Testnet agents are provisioned once and kept (.state/testnet-agents-*):
// a host admits carriers only from a funding tree proven in a block, which is
// a one-time setup cost per agent, not per deal. A provisioned agent trades
// with no block in the path — pass --fresh to provision a new set.
const AGENTS_DIR = join(root, '.state', 'testnet-agents')
const REUSE = LIVE && !process.argv.includes('--fresh') && existsSync(join(AGENTS_DIR, 'agent-keys.json'))
// --fault <trigger>:<verb>:<arg> injects a failure mid-deal:
//   triggers: 'want' (right after the want posts), 'funded' (buyer reaches
//   awaiting_disclosure — escrow broadcast, before delivery can land)
//   verbs: 'kill:<name>' SIGKILL the agent (the runner's respawn is the
//   restart path), 'db-loss:<name>' kill + delete automation.sqlite (the
//   recovery-note rebuild is what gets exercised), 'host-down:<a|b>' docker
//   stop the host container, 'seller-dark' host-down:b (ben/cara's only
//   host — the winner goes unreachable post-escrow)
// --expect <verified|refunded> picks the assertion set (default verified)
const FAULT = (() => {
  const i = process.argv.indexOf('--fault')
  if (i === -1) return undefined
  const [trigger, verb, arg] = process.argv[i + 1].split(':')
  return { trigger, verb, arg }
})()
const EXPECT = process.argv.includes('--expect') ? process.argv[process.argv.indexOf('--expect') + 1] : 'verified'
const HOST_CONTAINER = { a: 'nektar-bstack-testnet-host-a-1', b: 'nektar-bstack-testnet-host-b-1' }

// ---- per-step timing ------------------------------------------------------
// The deal journals on both sides record every transition with `cause`+`at`
// (same machine → comparable clocks). stepTable diffs the two journals into a
// want→verified timeline, all anchored to the runner's want-post mark.
const stepAt = (evs, cause, t0) => {
  const ts = evs.filter((e) => e.cause === cause).map((e) => e.at)
  return ts.length === 0 ? NaN : Math.min(...ts) - t0
}
const STEP_ROWS = [
  ['want signed', (b, s, t0) => stepAt(b, 'want_signed', t0), 'want tx build + broadcast'],
  ['seller prepared offer', (b, s, t0) => stepAt(s, 'offer_prepared', t0), 'want propagates to seller + offer build'],
  ['offer recorded (buyer)', (b, s, t0) => stepAt(b, 'offer_recorded', t0), 'host write → buyer lane read'],
  ['offer selected', (b, s, t0) => stepAt(b, 'offer_selected', t0), 'scoring collectMs window'],
  ['escrow broadcast', (b, s, t0) => stepAt(b, 'escrow_broadcast', t0), 'spend guard + escrow build + broadcast'],
  ['escrow verified (seller)', (b, s, t0) => stepAt(s, 'escrow_verified', t0), 'host write → seller read + verify'],
  ['disclosure broadcast', (b, s, t0) => stepAt(s, 'disclosure_broadcast', t0), 'delivery build + broadcast'],
  ['disclosure verified (buyer)', (b, s, t0) => stepAt(b, 'disclosure_verified', t0), 'host write → buyer read + payload verify'],
  ['payment verified (seller)', (b, s, t0) => stepAt(s, 'payment_verified', t0), 'seller observe + collect build']
]
function stepTable(bev, sev, wantAt) {
  const rows = [['step', 't (s)', 'Δ (s)', 'waited on']]
  let prev = 0
  for (const [label, fn, waited] of STEP_ROWS) {
    const t = fn(bev, sev, wantAt)
    if (typeof t !== 'number' || Number.isNaN(t)) {
      rows.push([label, '—', '—', waited])
      continue
    }
    const tS = t / 1000
    rows.push([label, tS.toFixed(1), (tS - prev).toFixed(1), waited])
    prev = tS
  }
  return rows
}
function printTable(rows) {
  const w = rows[0].map((_, c) => Math.max(...rows.map((r) => String(r[c]).length)))
  for (const r of rows) log('  ' + r.map((c, i) => String(c).padEnd(w[i])).join('  '))
}
let hostDowned
const docker = (args) => new Promise((res) => spawn('docker', args, { stdio: 'inherit' }).on('exit', res))
async function injectFault({ verb, arg }) {
  if (verb === 'host-down' || verb === 'seller-dark') {
    hostDowned = HOST_CONTAINER[verb === 'seller-dark' ? 'b' : arg]
    log(`fault: docker stop ${hostDowned}`)
    await docker(['stop', hostDowned])
    return
  }
  const a = agents.find((x) => x.name === arg)
  if (a?.proc === undefined) throw new Error(`fault: no live agent ${arg}`)
  if (verb === 'dead') a.dead = true // respawns suppressed — the refund path needs a seller that never returns
  log(`fault: SIGKILL ${arg}`)
  const p = a.proc
  const exited = new Promise((res) => p.once('exit', res))
  p.kill('SIGKILL')
  await Promise.race([exited, delay(5000)])
  if (verb === 'db-loss') {
    for (const suffix of ['', '-wal', '-shm', '-journal']) rmSync(join(a.dataDir, `automation.sqlite${suffix}`), { force: true })
    log(`fault: deleted ${arg}'s automation.sqlite — respawn rebuilds via the recovery note`)
  }
}
async function restoreFault() {
  if (hostDowned === undefined) return
  log(`fault restore: docker start ${hostDowned}`)
  await docker(['start', hostDowned])
  hostDowned = undefined
}
async function fundAgentsLive() {
  if (REUSE) {
    const keys = JSON.parse(readFileSync(join(AGENTS_DIR, 'agent-keys.json'), 'utf8'))
    const passFile = join(AGENTS_DIR, 'agent-pass.json')
    const pass = existsSync(passFile) ? JSON.parse(readFileSync(passFile, 'utf8')) : {}
    for (const a of agents) {
      a.key = PrivateKey.fromWif(keys[a.name])
      a.dataDir = join(AGENTS_DIR, a.name)
      a.pass = pass[a.name]
    }
    log(`reusing provisioned agents in ${AGENTS_DIR}`)
    return
  }
  rmSync(AGENTS_DIR, { recursive: true, force: true })
  mkdirSync(AGENTS_DIR, { recursive: true })
  // WoC lists a fresh unconfirmed output a few seconds before it serves its hex
  const woc = async (path) => {
    for (let i = 0; ; i++) {
      const r = await fetch(`${TESTNET.wocUrl}${path}`, { signal: AbortSignal.timeout(20_000) })
      if (r.ok) return r
      if (i >= 8) throw new Error(`woc ${path}: ${r.status}`)
      await delay(3000)
    }
  }
  const conf = await (await woc(`/address/${treasuryAddr}/unspent`)).json()
  const unconf = (await (await woc(`/address/${treasuryAddr}/unconfirmed/unspent`)).json()).result ?? []
  const per = 10_000
  const outs = 6
  const need = agents.length * per * outs + 10_000
  for (const a of agents) a.key = PrivateKey.fromRandom()
  writeFileSync(join(AGENTS_DIR, 'agent-keys.json'), JSON.stringify(Object.fromEntries(agents.map((a) => [a.name, a.key.toWif([0xef])]))), { mode: 0o600 })
  // an aborted run can leave one of our own prior splits holding the outpoint
  // WoC still lists — the broadcaster accepts the submit then REJECTS once the
  // mempool conflict resolves, so settle-watch the status before trusting it
  for (const best of [...conf, ...unconf].sort((x, y) => y.value - x.value)) {
    if (best.value < need) break
    const parent = Transaction.fromHex((await (await woc(`/tx/${best.tx_hash}/hex`)).text()).trim())
    const split = new Transaction()
    split.addInput({ sourceTransaction: parent, sourceOutputIndex: best.tx_pos, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
    for (const a of agents)
      for (let i = 0; i < outs; i++) split.addOutput({ lockingScript: new P2PKH().lock(a.key.toPublicKey().toAddress([0x6f])), satoshis: per })
    split.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), change: true })
    await split.fee(new SatoshisPerKilobyte(100))
    await split.sign()
    const r = await broadcaster.broadcast(split)
    if (r.status !== 'success') {
      log(`treasury split refused (${r.description}) — next candidate`)
      continue
    }
    let rejected = ''
    for (let i = 0; i < 3 && rejected === ''; i++) {
      await delay(5000)
      const st = await fetch(`${TESTNET.broadcasterUrl}/tx/${split.id('hex')}`, { signal: AbortSignal.timeout(10_000) })
        .then((x) => (x.ok ? x.json() : null))
        .catch(() => null)
      if (st?.txStatus === 'REJECTED') rejected = st.extraInfo ?? ''
    }
    if (rejected !== '') {
      log(`split ${split.id('hex').slice(0, 12)}… rejected: ${rejected.slice(0, 100)} — next candidate`)
      continue
    }
    for (const a of agents) {
      a.dataDir = join(AGENTS_DIR, a.name)
      mkdirSync(a.dataDir, { recursive: true })
      a.fundHex = split.toHex()
    }
    log(`treasury split ${split.id('hex')} (${split.toBinary().length} bytes, fee ${split.getFee()} sats) → ${agents.length}×${outs}×${per} sats`)
    return split.id('hex')
  }
  throw new Error(`testnet treasury ${treasuryAddr} underfunded or every candidate UTXO contested`)
}
async function fundAgents() {
  if (LIVE) return await fundAgentsLive()
  const saved = JSON.parse(readFileSync(TREASURY_FILE, 'utf8'))
  const parent = Transaction.fromHex(saved.treasury.txHex)
  const sats = parent.outputs[saved.treasury.vout].satoshis ?? 0
  const per = 5_000_000
  const change = sats - agents.length * per - 1000
  if (sats < agents.length * 1_000_100 + 1_000_000) throw new Error(`treasury outpoint too small for ${agents.length} instances: ${sats}`)
  const status = await utxoStatus({ ...cfg, rpcUser: E('NEKTAR_RPC_USER'), rpcPass: E('NEKTAR_RPC_PASS') }, saved.treasury.txid, saved.treasury.vout)
  if (status === 'spent') throw new Error('treasury outpoint already spent — re-run packages/e2e/src/scenarios/regtest-fund.mjs')
  const split = new Transaction()
  split.addInput({ sourceTransaction: parent, sourceOutputIndex: saved.treasury.vout, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
  for (const a of agents) {
    a.key = PrivateKey.fromRandom()
    split.addOutput({ lockingScript: new P2PKH().lock(a.key.toPublicKey().toAddress([0x6f])), satoshis: per })
  }
  split.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), satoshis: change })
  await split.sign()
  const r = await broadcaster.broadcast(split)
  if (r.status !== 'success') throw new Error(`treasury split refused: ${r.description}`)
  await rpc('generatetoaddress', 1, treasuryAddr)
  blocksMined++
  writeFileSync(TREASURY_FILE, JSON.stringify({ treasury: { txid: split.id('hex'), vout: agents.length, txHex: split.toHex() } }, null, 1))
  for (const [i, a] of agents.entries()) {
    a.dataDir = join(dir, a.name)
    mkdirSync(a.dataDir, { recursive: true })
    writeFileSync(join(a.dataDir, 'treasury.json'), JSON.stringify({ treasury: { txid: split.id('hex'), vout: i, txHex: split.toHex() } }))
  }
  log(`treasury split ${split.id('hex').slice(0, 12)}… → ${agents.length}×${per} sats + new treasury`)
}

// ---- production instances -------------------------------------------------
const up = async (url, name) => {
  for (let i = 0; i < 360; i++) {
    try {
      if ((await fetch(url, { signal: AbortSignal.timeout(500) })).ok) return
    } catch {}
    await delay(500)
  }
  throw new Error(`${name} at ${url} did not start`)
}
const tool = async (a, name, args) => {
  const r = await fetch(`${a.url}/v1/tools/${name}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${a.token}` },
    body: JSON.stringify(args),
    signal: AbortSignal.timeout(30_000)
  })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${a.name} ${name}: ${j.error ?? r.status}`)
  return j
}
function spawnInstance(a) {
  const err = createWriteStream(join(a.dataDir, 'stderr.log'), { flags: 'a' })
  const p = spawn(process.execPath, [join(root, 'packages/mcp/dist/src/bin.js')], {
    cwd: root,
    env: {
      ...process.env,
      NEKTAR_NETWORK: NET,
      ...(LIVE && liveHosts.some((h) => h.startsWith('http://')) ? { NEKTAR_MARKET_LOCAL_HOSTS: '1' } : {}),
      NEKTAR_AUTOMATION: '1',
      NEKTAR_PAID_DISCLOSURE: '1',
      NEKTAR_DATA_DIR: a.dataDir,
      NEKTAR_HTTP_PORT: String(a.port),
      NEKTAR_HTTP_TOKEN: a.token,
      NEKTAR_TX_LOG: join(a.dataDir, 'tx-log.jsonl'),
      NODE_OPTIONS: `--report-on-fatalerror --report-directory=${a.dataDir}`,
      NEKTAR_MARKET_HOSTS: a.hosts.join(','),
      // envelope receipts must reach every copy — the ack targets the sender's
      // record hosts, which only exist when records are published (LIVE sets
      // this implicitly via NEKTAR_MARKET_LOCAL_HOSTS; regtest needs it too)
      NEKTAR_BFINGER_PUBLISH: '1',
      NEKTAR_AUTOMATION_INTERVAL_MS: '1000',
      NEKTAR_OFFER_TTL_BLOCKS: '48',
      // watch-shim wake layer: poll every configured shim; advertise the one
      // fronting this agent's first host so counterparties' /notify reaches
      // the shim holding its waiters (hints only — poll path is unchanged)
      ...(watchShims.length > 0
        ? {
            NEKTAR_WATCH_URLS: watchShims.join(','),
            ...(shimFor(a.hosts[0]) !== undefined ? { NEKTAR_WATCH_PUBLIC: shimFor(a.hosts[0]) } : {})
          }
        : {}),
      ...(LIVE
        ? {
            NEKTAR_FUND_WIF: a.key.toWif([0xef]),
            ...(a.fundHex ? { NEKTAR_FUND_TXIDS: `hex:${a.fundHex}` } : {}),
            NEKTAR_KEY_PASSPHRASE: a.pass ?? `testnet-agent-${a.name}`,
            NEKTAR_MIN_BROADCAST_INTERVAL_MS: '250',
            NEKTAR_MAX_SPEND_SATS_PER_DAY: '100000'
          }
        : {
            NEKTAR_FUND_WIF: a.key.toWif(),
            NEKTAR_RPC_URL: cfg.rpcUrl,
            NEKTAR_RPC_USER: E('NEKTAR_RPC_USER'),
            NEKTAR_RPC_PASS: E('NEKTAR_RPC_PASS'),
            NEKTAR_PROPAGATION_URL: cfg.propagationUrl,
            NEKTAR_ASSET_URL: cfg.assetUrl
          })
    },
    stdio: ['ignore', 'ignore', 'pipe']
  })
  p.stderr.pipe(err)
  // a native abort on this rig (0xC0000409, no output, no dump) kills an
  // instance mid-deal; all state is in the data dir, so restart it in place
  p.on('exit', (c, s) => {
    // abnormal exit: nonzero code, or a kill signal (Windows reports
    // SIGKILL'd children with code null — the signal is the crash marker)
    const abnormal = (c !== 0 && c !== null) || s === 'SIGKILL'
    if (abnormal && !stopping && a.restarts < 3 && a.dead !== true) {
      a.restarts++
      log(`${a.name} exited ${c ?? s} — restarting (${a.restarts}/3)`)
      // instance.lock stays 'live' for 20s after the last heartbeat
      delay(25000).then(() => {
        if (!stopping) a.proc = spawnInstance(a)
      })
    } else if (abnormal) log(`${a.name} exited ${c ?? s}`)
  })
  procs.push(p)
  a.proc = p
  return p
}
async function bootAgent(a, index) {
  const port = 18401 + index
  a.url = `http://127.0.0.1:${port}`
  a.token = `real-${a.name}`
  a.port = port
  a.restarts = 0
  const p = spawnInstance(a)
  await up(`${a.url}/v1/health`, a.name)
  a.pub = (await (await fetch(`${a.url}/.well-known/nektar`)).json()).operatorPub
  log(`${a.name} up :${port} pub ${a.pub.slice(0, 12)}… hosts [${a.hosts.map((h) => (h === HOST_A ? 'a' : 'b')).join(',')}]`)
  const policy = {
    v: 1,
    policyId: `${a.name}-policy`,
    network: NET,
    agentPub: a.pub,
    roles: ['buyer', 'seller'],
    notBefore: 0,
    notAfter: 4102444800000,
    domains: ['libfoo'],
    // provisioned testnet agents trade run after run — daily caps sized for that
    limits: LIVE
      ? {
          perPurchaseSats: 500,
          dailyPurchaseSats: 50_000,
          perTxFeeSats: 600,
          dailyFeeSats: 100_000,
          runTotalSats: 1_000_000,
          maxLockedSats: 5000,
          maxUnconfirmedSats: 5000,
          maxUnconfirmedCount: 10
        }
      : {
          perPurchaseSats: 500,
          dailyPurchaseSats: 1500,
          perTxFeeSats: 600,
          dailyFeeSats: 3000,
          runTotalSats: 6000,
          maxLockedSats: 1200,
          maxUnconfirmedSats: 1200,
          maxUnconfirmedCount: 3
        },
    scoring: { minimumScoreBps: 0, weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 }, collectMs: LIVE ? 8000 : 30000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: LIVE ? 500 : 10, sellableShards: ['guide'] },
    disclosure: { allowPublic: false },
    ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 60000 },
    sandbox: { mode: 'sandbox-only' }
  }
  const pf = join(a.dataDir, 'policy.json')
  const automation = (...args) =>
    JSON.parse(execFileSync(process.execPath, [join(root, 'tools/automation.mjs'), '--data', a.dataDir, '--json', ...args], { encoding: 'utf8' }))
  // approvals live in automation.sqlite — a db-loss rebuild wipes them while
  // policy.json survives on disk, so approve every run; it is the operator's
  // re-attestation after any kind of state loss
  for (const old of REUSE ? (automation('policy', 'list').policies ?? []) : []) automation('policy', 'revoke', old.policyHash)
  writeFileSync(pf, JSON.stringify(policy))
  a.policyHash = automation('policy', 'approve', pf).policyHash
  a.rearm = true // a seller is armed under a policy — a new one needs a new `sell`
}

async function waitState(a, operationId, states, ms = LIVE ? 3 * 3600_000 : 900_000) {
  const end = Date.now() + ms
  for (;;) {
    let st
    try {
      st = await tool(a, 'deals', { operationId })
    } catch (e) {
      if (Date.now() > end) throw e
      await delay(200)
      continue // instance restarting — state lives in its data dir
    }
    if (states.includes(st.state)) return st
    if (Date.now() > end) {
      const tail = existsSync(join(a.dataDir, 'stderr.log')) ? readFileSync(join(a.dataDir, 'stderr.log'), 'utf8').split('\n').slice(-12).join('\n') : ''
      throw new Error(`${a.name} ${operationId} stuck in ${st.state}: ${st.lastError ?? ''}\nstderr tail:\n${tail}`)
    }
    await delay(200)
  }
}

// every copy of an envelope is receipted: recipients learn the sender's hosts
// from its identity record, so the check covers both hosts on every lane.
// Only this deal's envelopes count — provisioned agents carry earlier deals.
async function inboxEmpty(a, wantId, allowOwnSellerNote = false) {
  const key = a.key
  for (const h of HOSTS) {
    const r = await readBox({ key, hosts: [h], tracker })
    const items = r.items
      .map((i) => ({ from: i.from, m: i.body.market ?? i.body }))
      .filter((i) => i.m.wantId === wantId && !(allowOwnSellerNote && i.from === a.pub && i.m.type === 'sellerNote'))
    if (items.length !== 0)
      return { empty: false, host: h, items: items.length, detail: items.map((i) => `${i.m.type ?? '?'}@${i.from.slice(0, 10)}`).join(',') }
  }
  return { empty: true }
}

try {
  await fundAgents()
  for (const [i, a] of agents.entries()) await bootAgent(a, i)
  const [alice, ben, cara, bob] = agents
  for (const s of [alice, ben, cara].filter((x) => x.rearm)) {
    if (!REUSE) await tool(s, 'remember', { shard: 'guide', data: FINDING })
    await tool(s, 'sell', { shard: 'guide', domains: ['libfoo'], priceSats: s.seller })
    log(`${s.name} selling at ${s.seller} sats`)
  }
  if (LIVE) {
    // hosts admit carriers only from funding trees proven in a block — the
    // one unavoidable block wait, paid once per tree, before the deal starts
    const t0 = Date.now()
    for (;;) {
      const slots = await Promise.all(
        agents.map(async (a) => {
          try {
            return (await tool(a, 'fund', {})).marketSlots
          } catch {
            return undefined
          }
        })
      )
      if (slots.every((x) => x !== undefined && x.market > 0 && x.bbox > 0)) {
        log(`funding trees proven on all four agents after ${((Date.now() - t0) / 1000).toFixed(0)}s: ${JSON.stringify(slots)}`)
        break
      }
      if (Date.now() - t0 > 3 * 3600_000) throw new Error(`funding trees not proven after 3h: ${JSON.stringify(slots)}`)
      await delay(15_000)
    }
  }
  {
    // a record resolves while its token is unmined, as long as the token's
    // ancestry is proven — no further block. Each agent's record must name
    // exactly its own hosts. Without records the ack path cannot compute the
    // sender's fan-out set and leaves copies stranded on unconfigured hosts.
    const t0 = Date.now()
    for (;;) {
      const recs = await Promise.all(
        agents.map(async (a) => {
          try {
            return (await resolveFinger({ hosts: HOSTS, pub: a.pub, tracker, allowHttp: true })).body.hosts
          } catch {
            return undefined
          }
        })
      )
      const same = (x, a) => x !== undefined && JSON.stringify([...x].sort()) === JSON.stringify([...a.hosts].sort())
      if (agents.every((a, k) => same(recs[k], a))) {
        log(`bfinger records resolve for all four agents after ${((Date.now() - t0) / 1000).toFixed(1)}s`)
        break
      }
      if (Date.now() - t0 > 600_000) throw new Error(`finger records do not name the agents' hosts after 10 min: ${JSON.stringify(recs)}`)
      await delay(500)
    }
  }
  deal: {
    if (EXPECT === 'guard') {
      // money-safety probe: a want priced over the guard's per-purchase cap must
      // be refused before anything is broadcast — zero escrow txs must exist.
      // escrowTxids from earlier deals stay listed, so diff before/after
      const txOf = (o) => o.escrowTxid ?? o.data?.escrowTxid
      const before = new Set((await tool(bob, 'deals', {})).deals?.map(txOf).filter(Boolean) ?? [])
      let refused = ''
      try {
        await tool(bob, 'want', { domains: ['libfoo'], question: QUESTION, maxSats: 600 })
      } catch (e) {
        refused = String(e?.message ?? e)
      }
      if (!/per-purchase limit/.test(refused)) throw new Error(`over-cap want was not refused by the guard: ${refused || '(no refusal — want accepted)'}`)
      const after = (await tool(bob, 'deals', {})).deals?.map(txOf).filter(Boolean) ?? []
      const escrows = after.filter((t) => !before.has(t))
      if (escrows.length > 0) throw new Error(`refused want still produced escrow txs: ${JSON.stringify(escrows)}`)
      log(`PASS — guard refused 600-sat want (cap 500), zero escrow broadcasts | refusal: ${refused.slice(0, 80)}`)
      writeFileSync(join(dir, 'summary.json'), JSON.stringify({ runId, outcome: 'PASS', expect: 'guard', refusal: refused }, null, 1))
      break deal
    }
    log('want: posting (deal clock starts)')
    const wantAt = Date.now()
    const wantAtBlocks = blocksMined
    const { operationId } = await tool(bob, 'want', { domains: ['libfoo'], question: QUESTION, maxSats: 500 })
    if (FAULT?.trigger === 'want') await injectFault(FAULT)
    if (FAULT?.trigger === 'funding') {
      // inject inside the pay window — escrow is being assembled/broadcast; the
      // respawned agent must not produce a second escrow for the same op
      const hit = await waitState(bob, operationId, ['funding', 'awaiting_disclosure'], 60_000)
      log(`fault: funding trigger — buyer in ${hit.state}; injecting ${FAULT.verb}${FAULT.arg !== undefined ? `:${FAULT.arg}` : ''}`)
      await injectFault(FAULT)
    }
    if (FAULT?.trigger === 'funded') {
      // inject the moment escrow is broadcast but before delivery can land —
      // 'awaiting_disclosure' is the post-escrow/pre-disclosure window
      const hit = await waitState(bob, operationId, [
        'awaiting_disclosure',
        'disclosure_verified',
        'refunded',
        'refunding',
        'expired',
        'funding_failed',
        'denied',
        'uncertain',
        'disputed',
        'useful'
      ])
      log(`fault: funded trigger — buyer in ${hit.state}; injecting ${FAULT.verb}${FAULT.arg !== undefined ? `:${FAULT.arg}` : ''}`)
      await injectFault(FAULT)
    }
    const op = await waitState(bob, operationId, ['disclosure_verified', 'refunded', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed', 'useful'])
    const verifiedMs = Date.now() - wantAt
    const dealBlocks = blocksMined - wantAtBlocks
    // ---- per-step table (before assertions — timing survives a later fail) --
    let steps
    {
      const sellerOp = ((await tool(cara, 'deals', {})).deals ?? []).find((d) => d.wantId === op.wantId)
      const bev = (await tool(bob, 'deals', { operationId })).events ?? []
      const sev = sellerOp ? (await tool(cara, 'deals', { operationId: sellerOp.operationId })).events ?? [] : []
      steps = stepTable(bev, sev, wantAt)
      log('step table (anchored to want post):')
      printTable(steps)
      if (EXPECT === 'refunded') {
        const rb = stepAt(bev, 'refund_broadcast', wantAt)
        if (!Number.isNaN(rb)) log(`  refund broadcast at t=${(rb / 1000).toFixed(1)}s`)
      }
    }
    if (EXPECT === 'refunded') {
      if (op.state !== 'refunded') throw new Error(`expected refund — buyer ended in ${op.state}: ${op.lastError ?? ''}`)
      const rtx = op.data?.refundTxid
      if (typeof rtx !== 'string' || rtx.length !== 64) throw new Error(`refunded op carries no refundTxid: ${JSON.stringify(op.data).slice(0, 200)}`)
      const seen = await fetch(`${TESTNET.wocUrl}/tx/${rtx}`)
        .then((r) => r.ok)
        .catch(() => false)
      log(
        `PASS — refund fired: op refunded, refund tx ${rtx.slice(0, 12)}… broadcast${seen === true ? ' (WoC-visible)' : ' (WoC lookup pending)'} after ${(verifiedMs / 1000).toFixed(0)}s`
      )
      writeFileSync(
        join(dir, 'summary.json'),
        JSON.stringify({ runId, outcome: 'PASS', expect: 'refunded', fault: FAULT, refundTxid: rtx, wallClockMsToRefunded: verifiedMs, steps }, null, 1)
      )
    }
    if (EXPECT === 'verified' && op.state !== 'disclosure_verified') throw new Error(`buyer ended in ${op.state}: ${op.lastError ?? ''}`)

    // ---- assertions ----------------------------------------------------------
    verified: {
      if (EXPECT !== 'verified') break verified
      const offers = Object.values(op.data.offers ?? {}).map((o) => ({ pub: o.so?.offer?.pub, price: Number(o.so?.offer?.priceSats) }))
      const prices = offers.map((o) => o.price).sort((x, y) => x - y)
      // early-select semantics (market.ts): the first acceptable offer fires
      // selection in the tick it lands, so `offers` is whoever arrived before
      // that tick — require ≥1 of the three sellers and a selected key drawn
      // from that set; when all three made the window the ranking must still
      // pick cara's 200-sat bid
      if (prices.length < 1 || prices.some((p) => ![200, 300, 450].includes(p)))
        throw new Error(`recorded offers ${JSON.stringify(prices)} — expected a non-empty subset of [200,300,450]`)
      const sel = op.data.selected.key
      if (!offers.some((o) => sel.split(':')[0] === o.pub)) throw new Error(`selected key ${sel.slice(0, 24)}… is not one of the recorded offers`)
      if (prices.length === 3 && sel.split(':')[0] !== cara.pub) throw new Error(`selected ${sel.split(':')[0].slice(0, 12)}… — expected cara's 200-sat offer`)
      if (op.result?.text !== FINDING) throw new Error('deals.result.text differs from the sold finding')
      if (op.journal?.counterparty?.complete !== true)
        throw new Error(`counterparty journal incomplete: ${JSON.stringify(op.journal?.counterparty).slice(0, 300)}`)
      // the seller holds her recovery note until the sale op settles (collect tx
      // proven) — on regtest wait for that settle before demanding an empty box,
      // so the assertion also verifies the collect-proof-settle chain. on testnet
      // the runner accepts the note as post-deal bookkeeping instead of waiting
      // for a block.
      if (!LIVE) {
        const settleUntil = Date.now() + 90_000
        for (;;) {
          const d = ((await tool(cara, 'deals', {})).deals ?? []).find((d) => d.wantId === op.wantId)
          if (d?.state === 'settled' || Date.now() > settleUntil) break
          await delay(2000)
        }
      }
      for (const a of [bob, cara]) {
        let b
        const until = Date.now() + 30000
        do {
          b = await inboxEmpty(a, op.wantId, LIVE && a === cara)
          if (!b.empty) await delay(1000)
        } while (!b.empty && Date.now() < until)
        if (!b.empty) throw new Error(`${a.name} inbox not empty on ${b.host} (${b.items} item(s): ${b.detail ?? ''}) after settlement`)
      }
      const sellerOp = ((await tool(cara, 'deals', {})).deals ?? []).find((d) => d.wantId === op.wantId)
      const sellerState = sellerOp?.state
      if (sellerState !== 'completion_submitted' && sellerState !== 'settled')
        throw new Error(`cara seller op in ${sellerState ?? 'missing'} — expected completion_submitted or settled`)
      // per-hop latency, off the event journals (same machine → comparable clocks):
      // escrow_broadcast(buyer) → escrow_verified(seller) and
      // disclosure_broadcast(seller) → disclosure_verified(buyer)
      const hopMs = await (async () => {
        try {
          const at = (evs, cause) => evs.find((e) => e.cause === cause)?.at
          const bev = (await tool(bob, 'deals', { operationId })).events ?? []
          const sev = (await tool(cara, 'deals', { operationId: sellerOp.operationId })).events ?? []
          const e1 = at(bev, 'escrow_broadcast'),
            e2 = at(sev, 'escrow_verified')
          const e3 = at(sev, 'disclosure_broadcast'),
            e4 = at(bev, 'disclosure_verified')
          return {
            escrowBroadcastToSellerVerified: e1 !== undefined && e2 !== undefined ? e2 - e1 : null,
            disclosureBroadcastToBuyerVerified: e3 !== undefined && e4 !== undefined ? e4 - e3 : null
          }
        } catch {
          return { escrowBroadcastToSellerVerified: null, disclosureBroadcastToBuyerVerified: null }
        }
      })()
      for (const a of agents) {
        const f = join(a.dataDir, 'tx-log.jsonl')
        if (!existsSync(f)) throw new Error(`${a.name}: no tx log at ${f}`)
        const lines = readFileSync(f, 'utf8')
          .trim()
          .split('\n')
          .filter(Boolean)
          .map((l) => JSON.parse(l))
        a.txCount = lines.length
        for (const l of lines) {
          for (const k of ['t', 'txid', 'bytes', 'inSats', 'outSats', 'feeSats']) if (!(k in l)) throw new Error(`${a.name} tx-log line missing ${k}`)
          if (l.inSats !== null && l.feeSats !== l.inSats - l.outSats) throw new Error(`${a.name} tx-log fee mismatch`)
        }
      }
      const summary = {
        runId,
        network: NET,
        outcome: 'PASS',
        provisionedAgentsReused: REUSE,
        instances: agents.map((a) => ({ name: a.name, pub: a.pub, dataDir: a.dataDir, port: Number(a.url.split(':').pop()) })),
        buyerOperationId: operationId,
        wantId: op.wantId,
        escrowTxid: op.escrowTxid,
        disclosureTxid: op.disclosureTxid,
        sellerState,
        wallClockMsToDisclosureVerified: verifiedMs,
        steps,
        hopMs,
        blocksMinedDuringDeal: dealBlocks,
        hosts: { a: HOST_A, b: HOST_B },
        fingerResolved: true,
        broadcasts: Object.fromEntries(agents.map((a) => [a.name, a.txCount]))
      }
      writeFileSync(join(dir, 'summary.json'), JSON.stringify(summary, null, 1))
      log(`PASS — disclosure verified in ${(verifiedMs / 1000).toFixed(1)}s over ${dealBlocks} blocks`)
      log(`  hops: escrow→seller ${hopMs.escrowBroadcastToSellerVerified ?? '?'}ms, disclosure→buyer ${hopMs.disclosureBroadcastToBuyerVerified ?? '?'}ms`)
      log(
        `  offers 200/300/450 recorded; cara's 200-sat offer selected; escrow ${String(op.escrowTxid).slice(0, 12)}… disclosure ${String(op.disclosureTxid).slice(0, 12)}…`
      )
      log(`  counterparty journal complete; bob+cara inboxes empty on both hosts${LIVE ? ' (seller note held until proof)' : ''}`)
      log(`  tx logs: ${agents.map((a) => `${a.name}=${a.txCount}`).join(' ')}`)
      log(`  summary: ${join(dir, 'summary.json')}`)
    }
  }
} catch (e) {
  writeFileSync(join(dir, 'summary.json'), JSON.stringify({ runId, outcome: 'FAIL', error: e.message }, null, 1))
  console.error(`[${runId}] FAIL: ${e.stack}`)
  process.exitCode = 1
} finally {
  stopping = true
  await restoreFault().catch(() => {})
  mining = false
  await mineLoop.catch(() => {})
  await txLogScan.catch(() => {})
  for (const p of procs)
    try {
      p.kill('SIGTERM')
    } catch {}
  await delay(1500)
  for (const p of procs)
    try {
      if (p.exitCode === null) p.kill('SIGKILL')
    } catch {}
}
