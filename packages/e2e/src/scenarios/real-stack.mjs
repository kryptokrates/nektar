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
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, existsSync, createWriteStream } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { TeranodeBroadcaster, TeranodeRPCTracker, utxoStatus } from '../../../chain/dist/src/regtest.js'
import { BsvBroadcaster, ChaintracksTracker, WocTracker } from '../../../chain/dist/src/mainnet.js'
import { readBox } from '../../../bstack/dist/src/index.js'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const envFile = join(root, '.env')
const env = existsSync(envFile)
  ? Object.fromEntries(readFileSync(envFile, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()]))
  : {}
// --net testnet: public BSV testnet through the BSV Broadcaster against the
// nektar-bstack-testnet host pair (:18291/:18292). Nothing is mined here —
// blocks arrive on their own and statuses are pushed over the status stream.
// The treasury is .state/testnet-treasury.json ({wif}); fund its address first.
const NET = process.argv.includes('--net') ? process.argv[process.argv.indexOf('--net') + 1] : 'regtest'
if (NET !== 'regtest' && NET !== 'testnet') { console.error(`real-stack: unknown --net ${NET}`); process.exit(2) }
const LIVE = NET === 'testnet'
const TESTNET = { broadcasterUrl: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', wocUrl: 'https://api.whatsonchain.com/v1/bsv/test' }
const TESTNET_TREASURY = join(root, '.state', 'testnet-treasury.json')
if (LIVE && !existsSync(TESTNET_TREASURY)) { console.error(`real-stack: ${TESTNET_TREASURY} missing`); process.exit(2) }
for (const k of LIVE ? [] : ['NEKTAR_REGTEST_WIF', 'NEKTAR_RPC_USER', 'NEKTAR_RPC_PASS']) {
  if (!env[k] && !process.env[k]) { console.error(`real-stack: ${k} not set (repo .env)`); process.exit(2) }
}
const E = k => process.env[k] ?? env[k]
const treasuryKey = PrivateKey.fromWif(LIVE ? JSON.parse(readFileSync(TESTNET_TREASURY, 'utf8')).wif : E('NEKTAR_REGTEST_WIF'))
const treasuryAddr = treasuryKey.toPublicKey().toAddress([0x6f])
const auth = 'Basic ' + Buffer.from(`${E('NEKTAR_RPC_USER')}:${E('NEKTAR_RPC_PASS')}`).toString('base64')
const cfg = { rpcUrl: E('NEKTAR_RPC_URL') ?? 'http://localhost:9292/', assetUrl: E('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1', propagationUrl: E('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx' }
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'content-type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json(); if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`); return j.result
}
const broadcaster = LIVE ? new BsvBroadcaster(TESTNET) : new TeranodeBroadcaster(cfg.propagationUrl)
const tracker = LIVE
  ? new ChaintracksTracker(TESTNET, `${TESTNET.broadcasterUrl}/chaintracks/v2`, new WocTracker(TESTNET))
  : new TeranodeRPCTracker({ ...cfg, rpcUser: E('NEKTAR_RPC_USER'), rpcPass: E('NEKTAR_RPC_PASS') })
const HOST_A = LIVE ? 'http://127.0.0.1:18291' : 'http://127.0.0.1:18191'
const HOST_B = LIVE ? 'http://127.0.0.1:18292' : 'http://127.0.0.1:18192'
const HOSTS = [HOST_A, HOST_B]
const FINDING = 'libfoo 3.0 renamed connect() to open(); callers must switch.\n```nektar-patch\n{"edits":[{"file":"src/app.js","find":"libfoo.connect(","replace":"libfoo.open("}]}\n```'
const QUESTION = 'build fails after libfoo 3.0 upgrade: libfoo.connect is not a function'

const runId = `${LIVE ? 'testnet' : 'real'}-${Date.now().toString(36)}`
const dir = mkdtempSync(join(root, '.state', `run-${runId}-`))
console.log(`run directory: ${dir}`)
const log = m => console.log(`[${runId}] ${m}`)
const procs = []
let stopping = false

// ---- background mining: ~1 block / 2 s while anything waits -------------
let mining = !LIVE
let blocksMined = 0
const mineLoop = (async () => {
  while (mining) {
    try { await rpc('generatetoaddress', 1, treasuryAddr); blocksMined++ } catch (e) { log(`mine: ${e.message}`) }
    await delay(2000)
  }
})()

// ---- fund the four instances from the shared treasury --------------------
// The treasury chain (.state/treasury.json) is the same one e2e-regtest.ts
// spends and bootstrapReserve consumes: one split pays each instance's own
// fund key 5M sats plus a new treasury outpoint, mined before use.
const TREASURY_FILE = join(root, '.state', 'treasury.json')
const agents = [
  { name: 'alice', hosts: [HOST_A], seller: 450 },
  { name: 'ben', hosts: [HOST_B], seller: 300 },
  { name: 'cara', hosts: [HOST_B], seller: 200 },
  { name: 'bob', hosts: [HOST_A, HOST_B] }
]
// testnet: one split of the treasury's largest unspent output pays each
// instance six 15k-sat outputs (parallel spends without waiting for change)
// and returns the rest to the treasury. Instances import it offline
// (NEKTAR_FUND_TXIDS=hex:…); their keys are kept in the run dir so leftover
// test coins can be swept back.
async function fundAgentsLive () {
  const woc = async path => { const r = await fetch(`${TESTNET.wocUrl}${path}`, { signal: AbortSignal.timeout(20_000) }); if (!r.ok) throw new Error(`woc ${path}: ${r.status}`); return r }
  const conf = await (await woc(`/address/${treasuryAddr}/unspent`)).json()
  const unconf = (await (await woc(`/address/${treasuryAddr}/unconfirmed/unspent`)).json()).result ?? []
  const best = [...conf, ...unconf].sort((x, y) => y.value - x.value)[0]
  const per = 15_000; const outs = 6
  if (best === undefined || best.value < agents.length * per * outs + 10_000) throw new Error(`testnet treasury ${treasuryAddr} underfunded: ${best?.value ?? 0} sats`)
  const parent = Transaction.fromHex((await (await woc(`/tx/${best.tx_hash}/hex`)).text()).trim())
  const split = new Transaction()
  split.addInput({ sourceTransaction: parent, sourceOutputIndex: best.tx_pos, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
  for (const a of agents) {
    a.key = PrivateKey.fromRandom()
    for (let i = 0; i < outs; i++) split.addOutput({ lockingScript: new P2PKH().lock(a.key.toPublicKey().toAddress([0x6f])), satoshis: per })
  }
  split.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), change: true })
  await split.fee(new SatoshisPerKilobyte(100)); await split.sign()
  writeFileSync(join(dir, 'agent-keys.json'), JSON.stringify(Object.fromEntries(agents.map(a => [a.name, a.key.toWif([0xef])]))), { mode: 0o600 })
  const r = await broadcaster.broadcast(split)
  if (r.status !== 'success') throw new Error(`treasury split refused: ${r.description}`)
  for (const a of agents) { a.dataDir = join(dir, a.name); mkdirSync(a.dataDir, { recursive: true }); a.fundHex = split.toHex() }
  log(`treasury split ${split.id('hex')} (${split.toBinary().length} bytes, fee ${split.getFee()} sats) → ${agents.length}×${outs}×${per} sats`)
  return split.id('hex')
}
async function fundAgents () {
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
  for (const a of agents) { a.key = PrivateKey.fromRandom(); split.addOutput({ lockingScript: new P2PKH().lock(a.key.toPublicKey().toAddress([0x6f])), satoshis: per }) }
  split.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), satoshis: change })
  await split.sign()
  const r = await broadcaster.broadcast(split)
  if (r.status !== 'success') throw new Error(`treasury split refused: ${r.description}`)
  await rpc('generatetoaddress', 1, treasuryAddr); blocksMined++
  writeFileSync(TREASURY_FILE, JSON.stringify({ treasury: { txid: split.id('hex'), vout: agents.length, txHex: split.toHex() } }, null, 1))
  for (const [i, a] of agents.entries()) {
    a.dataDir = join(dir, a.name); mkdirSync(a.dataDir, { recursive: true })
    writeFileSync(join(a.dataDir, 'treasury.json'), JSON.stringify({ treasury: { txid: split.id('hex'), vout: i, txHex: split.toHex() } }))
  }
  log(`treasury split ${split.id('hex').slice(0, 12)}… → ${agents.length}×${per} sats + new treasury`)
}

// ---- production instances -------------------------------------------------
const up = async (url, name) => {
  for (let i = 0; i < 360; i++) { try { if ((await fetch(url, { signal: AbortSignal.timeout(500) })).ok) return } catch {} await delay(500) }
  throw new Error(`${name} at ${url} did not start`)
}
const tool = async (a, name, args) => {
  const r = await fetch(`${a.url}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: `Bearer ${a.token}` }, body: JSON.stringify(args), signal: AbortSignal.timeout(30_000) })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${a.name} ${name}: ${j.error ?? r.status}`)
  return j
}
function spawnInstance (a) {
  const err = createWriteStream(join(a.dataDir, 'stderr.log'), { flags: 'a' })
  const p = spawn(process.execPath, [join(root, 'packages/mcp/dist/src/bin.js')], {
    cwd: root,
    env: {
      ...process.env,
      NEKTAR_NETWORK: NET,
      NEKTAR_AUTOMATION: '1',
      NEKTAR_PAID_DISCLOSURE: '1',
      NEKTAR_DATA_DIR: a.dataDir,
      NEKTAR_HTTP_PORT: String(a.port),
      NEKTAR_HTTP_TOKEN: a.token,
      NEKTAR_TX_LOG: join(a.dataDir, 'tx-log.jsonl'),
      NODE_OPTIONS: `--report-on-fatalerror --report-directory=${a.dataDir}`,
      NEKTAR_MARKET_HOSTS: a.hosts.join(','),
      NEKTAR_AUTOMATION_INTERVAL_MS: '1000',
      NEKTAR_OFFER_TTL_BLOCKS: '48',
      ...(LIVE
        ? { NEKTAR_FUND_WIF: a.key.toWif([0xef]), NEKTAR_FUND_TXIDS: `hex:${a.fundHex}`, NEKTAR_KEY_PASSPHRASE: `run-${runId}-${a.name}`, NEKTAR_MIN_BROADCAST_INTERVAL_MS: '250', NEKTAR_MAX_SPEND_SATS_PER_DAY: '100000' }
        : {
            NEKTAR_FUND_WIF: a.key.toWif(),
            NEKTAR_RPC_URL: cfg.rpcUrl, NEKTAR_RPC_USER: E('NEKTAR_RPC_USER'), NEKTAR_RPC_PASS: E('NEKTAR_RPC_PASS'),
            NEKTAR_PROPAGATION_URL: cfg.propagationUrl, NEKTAR_ASSET_URL: cfg.assetUrl
          })
    },
    stdio: ['ignore', 'ignore', 'pipe']
  })
  p.stderr.pipe(err)
  // a native abort on this rig (0xC0000409, no output, no dump) kills an
  // instance mid-deal; all state is in the data dir, so restart it in place
  p.on('exit', c => {
    if (c && c !== 0 && c !== null && !stopping && a.restarts < 3) {
      a.restarts++
      log(`${a.name} exited ${c} — restarting (${a.restarts}/3)`)
      // instance.lock stays 'live' for 20s after the last heartbeat
      delay(25000).then(() => { if (!stopping) a.proc = spawnInstance(a) })
    } else if (c && c !== 0 && c !== null) log(`${a.name} exited ${c}`)
  })
  procs.push(p)
  return p
}
async function bootAgent (a, index) {
  const port = 18401 + index
  a.url = `http://127.0.0.1:${port}`
  a.token = `real-${a.name}`
  a.port = port
  a.restarts = 0
  const p = spawnInstance(a)
  await up(`${a.url}/v1/health`, a.name)
  a.pub = (await (await fetch(`${a.url}/.well-known/nektar`)).json()).operatorPub
  log(`${a.name} up :${port} pub ${a.pub.slice(0, 12)}… hosts [${a.hosts.map(h => h.split(':').pop()).join(',')}]`)
  const policy = {
    v: 1, policyId: `${a.name}-policy`, network: NET, agentPub: a.pub, roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4102444800000, domains: ['libfoo'],
    limits: { perPurchaseSats: 500, dailyPurchaseSats: 1500, perTxFeeSats: 600, dailyFeeSats: 3000, runTotalSats: 6000, maxLockedSats: 1200, maxUnconfirmedSats: 1200, maxUnconfirmedCount: 3 },
    scoring: { minimumScoreBps: 0, weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 }, collectMs: LIVE ? 120000 : 30000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
    disclosure: { allowPublic: false }, ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 60000 }, sandbox: { mode: 'sandbox-only' }
  }
  const pf = join(a.dataDir, 'policy.json'); writeFileSync(pf, JSON.stringify(policy))
  const { policyHash } = JSON.parse(execFileSync(process.execPath, [join(root, 'tools/automation.mjs'), '--data', a.dataDir, '--json', 'policy', 'approve', pf], { encoding: 'utf8' }))
  a.policyHash = policyHash
}

async function waitState (a, operationId, states, ms = LIVE ? 3 * 3600_000 : 900_000) {
  const end = Date.now() + ms
  for (;;) {
    let st
    try { st = await tool(a, 'deals', { operationId }) } catch (e) {
      if (Date.now() > end) throw e
      await delay(1500); continue // instance restarting — state lives in its data dir
    }
    if (states.includes(st.state)) return st
    if (Date.now() > end) {
      const tail = existsSync(join(a.dataDir, 'stderr.log')) ? readFileSync(join(a.dataDir, 'stderr.log'), 'utf8').split('\n').slice(-12).join('\n') : ''
      throw new Error(`${a.name} ${operationId} stuck in ${st.state}: ${st.lastError ?? ''}\nstderr tail:\n${tail}`)
    }
    await delay(1500)
  }
}

// on testnet the local hosts are http loopback: bfinger refuses http hosts
// outside mock/regtest, so no identity record is published and a recipient
// cannot learn the sender's extra hosts to send its receipt there — the copy
// on a host the recipient never reads expires after 24h. LIVE therefore
// checks only the agent's own configured hosts; regtest keeps all HOSTS.
async function inboxEmpty (a) {
  const key = a.key
  for (const h of LIVE ? a.hosts : HOSTS) {
    const r = await readBox({ key, hosts: [h], tracker })
    if (r.items.length !== 0) return { empty: false, host: h, items: r.items.length }
  }
  return { empty: true }
}

try {
  await fundAgents()
  for (const [i, a] of agents.entries()) await bootAgent(a, i)
  const [alice, ben, cara, bob] = agents
  for (const s of [alice, ben, cara]) {
    await tool(s, 'remember', { shard: 'guide', data: FINDING })
    await tool(s, 'sell', { shard: 'guide', domains: ['libfoo'], priceSats: s.seller })
    log(`${s.name} selling at ${s.seller} sats`)
  }
  if (LIVE) {
    // hosts admit carriers only from funding trees proven in a block — the
    // one unavoidable block wait, paid once per tree, before the deal starts
    const t0 = Date.now()
    for (;;) {
      const slots = await Promise.all(agents.map(async a => { try { return (await tool(a, 'fund', {})).marketSlots } catch { return undefined } }))
      if (slots.every(x => x !== undefined && x.market > 0 && x.bbox > 0)) { log(`funding trees proven on all four agents after ${((Date.now() - t0) / 1000).toFixed(0)}s: ${JSON.stringify(slots)}`); break }
      if (Date.now() - t0 > 3 * 3600_000) throw new Error(`funding trees not proven after 3h: ${JSON.stringify(slots)}`)
      await delay(15_000)
    }
  }
  log('want: posting (deal clock starts)')
  const wantAt = Date.now(); const wantAtBlocks = blocksMined
  const { operationId } = await tool(bob, 'want', { domains: ['libfoo'], question: QUESTION, maxSats: 500 })
  const op = await waitState(bob, operationId, ['disclosure_verified', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed', 'useful'])
  const verifiedMs = Date.now() - wantAt
  const dealBlocks = blocksMined - wantAtBlocks
  if (op.state !== 'disclosure_verified') throw new Error(`buyer ended in ${op.state}: ${op.lastError ?? ''}`)

  // ---- assertions ----------------------------------------------------------
  const offers = Object.values(op.data.offers ?? {}).map(o => ({ pub: o.so?.offer?.pub, price: Number(o.so?.offer?.priceSats) }))
  const prices = offers.map(o => o.price).sort((x, y) => x - y)
  if (JSON.stringify(prices) !== JSON.stringify([200, 300, 450])) throw new Error(`recorded offers ${JSON.stringify(prices)} — expected [200,300,450]`)
  const sel = op.data.selected.key
  if (sel.split(':')[0] !== cara.pub) throw new Error(`selected ${sel.split(':')[0].slice(0, 12)}… — expected cara's 200-sat offer`)
  if (op.result?.text !== FINDING) throw new Error('deals.result.text differs from the sold finding')
  if (op.journal?.counterparty?.complete !== true) throw new Error(`counterparty journal incomplete: ${JSON.stringify(op.journal?.counterparty).slice(0, 300)}`)
  for (const a of [bob, cara]) {
    // the delivered/seller-note envelopes are held until the op reaches a
    // settled state and released on the following ack cycle — poll briefly.
    // The seller settles only once its collect tx is proven in a block, so
    // on testnet this waits for the next block (after the deal clock stopped)
    let b
    const until = Date.now() + (LIVE ? 90 * 60_000 : 30000)
    do { b = await inboxEmpty(a); if (!b.empty) await delay(1000) } while (!b.empty && Date.now() < until)
    if (!b.empty) throw new Error(`${a.name} inbox not empty on ${b.host} (${b.items} item(s)) after settlement`)
  }
  for (const a of agents) {
    const f = join(a.dataDir, 'tx-log.jsonl')
    if (!existsSync(f)) throw new Error(`${a.name}: no tx log at ${f}`)
    const lines = readFileSync(f, 'utf8').trim().split('\n').filter(Boolean).map(l => JSON.parse(l))
    a.txCount = lines.length
    for (const l of lines) {
      for (const k of ['t', 'txid', 'bytes', 'inSats', 'outSats', 'feeSats']) if (!(k in l)) throw new Error(`${a.name} tx-log line missing ${k}`)
      if (l.inSats !== null && l.feeSats !== l.inSats - l.outSats) throw new Error(`${a.name} tx-log fee mismatch`)
    }
  }
  const summary = {
    runId, network: NET, outcome: 'PASS',
    instances: agents.map(a => ({ name: a.name, pub: a.pub, dataDir: a.dataDir, port: Number(a.url.split(':').pop()) })),
    buyerOperationId: operationId, wantId: op.wantId, escrowTxid: op.escrowTxid, disclosureTxid: op.disclosureTxid,
    wallClockMsToDisclosureVerified: verifiedMs, blocksMinedDuringDeal: dealBlocks,
    hosts: { a: HOST_A, b: HOST_B },
    broadcasts: Object.fromEntries(agents.map(a => [a.name, a.txCount]))
  }
  writeFileSync(join(dir, 'summary.json'), JSON.stringify(summary, null, 1))
  log(`PASS — disclosure verified in ${(verifiedMs / 1000).toFixed(1)}s over ${dealBlocks} blocks`)
  log(`  offers 200/300/450 recorded; cara's 200-sat offer selected; escrow ${String(op.escrowTxid).slice(0, 12)}… disclosure ${String(op.disclosureTxid).slice(0, 12)}…`)
  log(`  counterparty journal complete; bob+cara inboxes empty on ${LIVE ? 'their own hosts' : 'both hosts'}`)
  log(`  tx logs: ${agents.map(a => `${a.name}=${a.txCount}`).join(' ')}`)
  log(`  summary: ${join(dir, 'summary.json')}`)
} catch (e) {
  writeFileSync(join(dir, 'summary.json'), JSON.stringify({ runId, outcome: 'FAIL', error: e.message }, null, 1))
  console.error(`[${runId}] FAIL: ${e.stack}`)
  process.exitCode = 1
} finally {
  stopping = true
  mining = false
  await mineLoop.catch(() => {})
  for (const p of procs) try { p.kill('SIGTERM') } catch {}
  await delay(1500)
  for (const p of procs) try { if (p.exitCode === null) p.kill('SIGKILL') } catch {}
}
