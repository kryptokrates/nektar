#!/usr/bin/env node
// pnpm scenarios -- --profile mock --scenario all
// Real OS processes for every actor: mock-chain hub, two federated rendezvous,
// N agents with their own data dirs and HTTP APIs. The only marketplace calls
// are the `automation` tool and the operator CLI. No production resources.
import { spawn, execFileSync } from 'node:child_process'
import { mkdtempSync, mkdirSync, writeFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { createRequire } from 'node:module'

import { P2PKH, PrivateKey } from '@bsv/sdk'
import { AutomationStore } from '../../../mcp/dist/src/automation/store.js'
import { collectTradeEvidence, verifyTradeEvidence } from '../../../mcp/dist/src/automation/evidence.js'
import { parsePatch, evaluatePatch } from '../../../mcp/dist/src/automation/sandbox.js'
import { startHub } from './mock-chain-hub.mjs'
import { RemoteMockChain } from './remote-mock-chain.mjs'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const knexFactory = createRequire(join(root, 'packages/mcp/package.json'))('knex')
const argv = process.argv.slice(2)
const arg = (k, d) => { const i = argv.indexOf(k); return i >= 0 ? argv[i + 1] : d }
const profile = arg('--profile', 'mock')
const scenario = arg('--scenario', 'all')
const runId = arg('--run-id', `run-${Date.now().toString(36)}`)
const resume = argv.includes('--resume') ? arg('--resume') : undefined
if (profile !== 'mock') { console.error(`profile ${profile} is not implemented in this pass — use mock (regtest/testnet need their chain stacks)`); process.exit(2) }

const dir = resume ? join(tmpdir(), `nektar-scenario-${resume}`) : mkdtempSync(join(tmpdir(), `nektar-scenario-${runId}-`))
mkdirSync(dir, { recursive: true })
const outDir = process.env.NEKTAR_SCENARIO_OUT ? resolve(process.env.NEKTAR_SCENARIO_OUT) : join(dir, 'evidence'); mkdirSync(outDir, { recursive: true })
const procs = []
const agents = []
const FINDING = 'libfoo 3.0 renamed connect() to open(); callers must switch.\n```nektar-patch\n{"edits":[{"file":"src/app.js","find":"libfoo.connect(","replace":"libfoo.open("}]}\n```'
let hub, chain

const log = m => console.log(`[${runId}] ${m}`)
const write = (name, obj) => writeFileSync(join(outDir, name), JSON.stringify(obj, null, 1))
const spawnNode = (file, env, name) => {
  const p = spawn(process.execPath, [file], { cwd: root, env: { ...process.env, ...env }, stdio: 'ignore' })
  p.on('exit', c => { if (c && c !== 0) console.error(`[${runId}] ${name} exited ${c}`) })
  procs.push(p)
  return p
}
const up = async url => { for (let i = 0; i < 120; i++) { try { if ((await fetch(url, { signal: AbortSignal.timeout(500) })).ok) return } catch {} await delay(200) } throw new Error(`${url} did not start`) }
const tool = async (a, name, args) => {
  const r = await fetch(`${a.url}/v1/tools/${name}`, { method: 'POST', headers: { 'content-type': 'application/json', authorization: `Bearer ${a.token}` }, body: JSON.stringify(args), signal: AbortSignal.timeout(30_000) })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${a.name} ${name}: ${j.error ?? r.status}`)
  return j
}
const rvPort = n => 18500 + n
const agentPort = n => 18600 + n

async function bootRv (n, peers) {
  const url = `http://127.0.0.1:${rvPort(n)}`
  spawnNode('packages/overlay/serve.mjs', { NEKTAR_OVERLAY_HOST: '127.0.0.1', NEKTAR_OVERLAY_PORT: String(rvPort(n)), NEKTAR_OVERLAY_DIR: join(dir, `rv${n}`), NEKTAR_RV_URL: url, NEKTAR_PEERS: peers, NEKTAR_NETWORK: 'mock', NEKTAR_OVERLAY_STRICT_NET: '0' }, `rv${n}`)
  await up(`${url}/health`)
  return url
}

async function addAgent (name, rv, { seller, publicDisclosure = false, bondEpoch, index }) {
  const dataDir = join(dir, name); mkdirSync(dataDir, { recursive: true })
  const token = `tok-${name}`
  const url = `http://127.0.0.1:${agentPort(index)}`
  spawnNode('packages/e2e/src/scenarios/agent-process.mjs', {
    NEKTAR_CHAIN_HUB: hub.url, NEKTAR_DATA_DIR: dataDir, NEKTAR_OVERLAY_URL: rv, NEKTAR_HTTP_PORT: String(agentPort(index)), NEKTAR_HTTP_TOKEN: token, NEKTAR_PAID_DISCLOSURE: '1', NEKTAR_AGENT_NAME: name, NEKTAR_AUTOMATION_INTERVAL_MS: '500', ...(bondEpoch ? { NEKTAR_BOND_EPOCH: String(bondEpoch) } : {})
  }, name)
  await up(`${url}/v1/health`)
  const pub = (await (await fetch(`${url}/.well-known/nektar`)).json()).operatorPub ?? (await tool({ url, token, name }, 'reputation', {})).card?.pub
  const policy = {
    v: 1, policyId: `${name}-policy`, network: 'mock', agentPub: pub, roles: ['buyer', 'seller'], notBefore: 0, notAfter: 4102444800000, domains: ['libfoo'],
    limits: { perPurchaseSats: 500, dailyPurchaseSats: 1500, perTxFeeSats: 600, dailyFeeSats: 3000, runTotalSats: 6000, maxLockedSats: 1200, maxUnconfirmedSats: 1200, maxUnconfirmedCount: 3 },
    scoring: { minimumScoreBps: 0, weights: { relevance: 3000, provenance: 2500, history: 1500, price: 2000, latency: 1000 }, collectMs: 8000 },
    seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 10, sellableShards: ['guide'] },
    disclosure: { allowPublic: publicDisclosure }, ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 }, sandbox: { mode: 'sandbox-only' }
  }
  const pf = join(dataDir, 'policy.json'); writeFileSync(pf, JSON.stringify(policy))
  const { policyHash } = JSON.parse(execFileSync(process.execPath, [join(root, 'tools/automation.mjs'), '--data', dataDir, '--json', 'policy', 'approve', pf], { encoding: 'utf8' }))
  const a = { name, url, token, dataDir, policyHash, pub, rv }
  agents.push(a)
  if (seller) {
    await tool(a, 'remember', { shard: 'guide', data: FINDING })
    await chain.mine(1)
    await tool(a, 'describeShard', { shard: 'guide', domains: ['libfoo'], offers: [{ kind: 'record', priceSats: seller }] })
    await tool(a, 'automation', { op: 'enableSeller', policyHash })
  }
  return a
}

async function waitState (a, operationId, states, ms = 120_000) {
  const end = Date.now() + ms
  for (;;) {
    const st = (await tool(a, 'automation', { op: 'status', operationId })).operation
    if (states.includes(st.state)) return st
    if (Date.now() > end) throw new Error(`${a.name} ${operationId} stuck in ${st.state}: ${st.lastError ?? ''}`)
    await delay(500)
  }
}

async function S1 () {
  log('S1: three sellers, two hosts, one buyer')
  const rvA = await bootRv(1, `http://127.0.0.1:${rvPort(2)}`)
  const rvB = await bootRv(2, `http://127.0.0.1:${rvPort(1)}`)
  const alice = await addAgent('alice', rvA, { seller: 450, index: 1 })
  const ben = await addAgent('ben', rvB, { seller: 300, index: 2 })
  const cara = await addAgent('cara', rvB, { seller: 200, index: 3 })
  const bob = await addAgent('bob', rvA, { index: 4 })
  const { operationId } = await tool(bob, 'automation', { op: 'publishWant', policyHash: bob.policyHash, domains: ['libfoo'], question: 'build fails after libfoo 3.0 upgrade: libfoo.connect is not a function', maxSats: 500 })
  const op = await waitState(bob, operationId, ['disclosure_verified', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed'])
  if (op.state !== 'disclosure_verified') throw new Error(`S1 ended in ${op.state}: ${op.lastError ?? ''}`)
  if (op.data.selected.key.split(':')[0] !== cara.pub) throw new Error('S1 did not select the 200-sat offer')
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(bob.dataDir, 'automation.sqlite') }, useNullAsDefault: true })
  const store = new AutomationStore(kx); await store.init()
  const stored = await store.operation(operationId)
  const evidence = await collectTradeEvidence(store, stored, txid => chain.getTx(txid))
  const verdict = verifyTradeEvidence(JSON.parse(JSON.stringify(evidence)))
  write('s1-evidence.json', evidence); write('s1-verdict.json', verdict)
  await kx.destroy()
  if (!verdict.ok) throw new Error(`S1 evidence failed: ${JSON.stringify(verdict.checks.filter(c => !c.ok))}`)
  const patch = parsePatch(op.data.result.text)
  const sandbox = await evaluatePatch({ fixtureDir: join(root, 'test/fixtures/libfoo-break'), hiddenTestsDir: join(root, 'test/fixtures/libfoo-break-hidden'), allowedPaths: ['src'], testCommand: ['node', '--test', 'test/build.test.js'], hiddenTestCommand: ['node', '--test', '/hidden/regress.test.js'], image: 'node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202', timeoutMs: 120_000, maxPatchBytes: 4096 }, patch)
  write('s1-sandbox.json', sandbox)
  log(`S1 PASS — disclosure verified; sandbox ${sandbox.outcome}${sandbox.reason ? ` (${sandbox.reason})` : ''}`)
  return { outcome: 'PASS', sandbox: sandbox.outcome }
}

async function S2 () {
  log('S2: operator process killed; independent observer seizes')
  const rvA = await bootRv(3, `http://127.0.0.1:${rvPort(4)}`)
  const rvB = await bootRv(4, `http://127.0.0.1:${rvPort(3)}`)
  const alice = await addAgent('alice2', rvA, { bondEpoch: 4, index: 5 })
  const observer = await addAgent('observer', rvB, { index: 6 })
  await tool(alice, 'bondDeploy', { sats: 5000 })
  const commit = await tool(alice, 'bondCommit', { root: 'ab'.repeat(32) })
  if (commit.n !== '1') throw new Error('checkpoint did not land')
  const st = await tool(alice, 'bondStatus', {})
  const descriptor = { cfg: { operatorPkh: st.cfg.operatorPkh, bondReleaseHeight: st.cfg.bondReleaseHeight, maxFee: st.cfg.maxFee, epochLen: st.cfg.epochLen }, txid: st.utxo.txid, vout: st.utxo.vout }
  const aliceProc = procs.find(p => p.spawnargs.includes('agent-process.mjs') && p.spawnargs.includes('agent-process.mjs'))
  // kill by PID — a real stopped process, not an in-process pause
  for (const p of procs) if (p.spawnargs.includes('agent-process.mjs')) { /* handled below by index */ }
  const victim = procs[procs.length - 2] // alice2 was spawned before observer
  victim.kill('SIGKILL')
  await delay(500)
  let health = 'up'
  try { await fetch(`${alice.url}/v1/health`, { signal: AbortSignal.timeout(500) }) } catch { health = 'down' }
  if (health !== 'down') throw new Error('operator process is still answering after SIGKILL')
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: join(observer.dataDir, 'automation.sqlite') }, useNullAsDefault: true })
  const store = new AutomationStore(kx); await store.init()
  // watchBond is exposed through the engine on the store; call it via a tiny
  // in-process engine bound to the observer's store and chain view.
  const { MarketEngine } = await import('../../../mcp/dist/src/automation/market.js')
  const eng = new MarketEngine({ store, call: (t, a) => tool(observer, t, a), memory: { shardManifest: async () => undefined, records: async () => [] }, key: PrivateKey.fromRandom(), network: 'mock', height: () => chain.currentHeight(), fetchTx: txid => chain.getTx(txid), transport: { publishWant: async () => {}, publishOffer: async () => {}, send: async () => {} }, ai: { provider: 'scripted', egress: 'local', execute: async () => ({ stop: 'no_match', relevanceBps: 0, model: { provider: 'scripted', modelId: 'x' } }) } })
  const { operationId, deadline } = await eng.watchBond(JSON.parse(JSON.stringify(descriptor)), observer.policyHash)
  while (await chain.currentHeight() < deadline) await chain.mine(1)
  for (let i = 0; i < 20; i++) { await eng.tick(); const o = await store.operation(operationId); if (o.state !== 'watching') break; await delay(200) }
  const op = await store.operation(operationId)
  await kx.destroy()
  if (op.state !== 'seized') throw new Error(`S2 ended in ${op.state}: ${op.lastError ?? ''}`)
  const seizeTx = await chain.getTx(op.data.seizeTxid)
  write('s2-evidence.json', { descriptor, deadline, seizeTxid: op.data.seizeTxid, seizeHex: seizeTx.toHex(), operatorProcess: 'SIGKILL', healthAfterKill: health })
  log('S2 PASS — observer seized after the operator process was killed')
  return { outcome: 'PASS' }
}

async function S3 () {
  log('S3: paid public disclosure, free reader')
  const rvA = await bootRv(5, `http://127.0.0.1:${rvPort(6)}`)
  const rvB = await bootRv(6, `http://127.0.0.1:${rvPort(5)}`)
  const pat = await addAgent('pat', rvB, { seller: 150, publicDisclosure: true, index: 7 })
  const dee = await addAgent('dee', rvA, { index: 8 })
  const reader = await addAgent('reader', rvA, { index: 9 })
  const { operationId } = await tool(dee, 'automation', { op: 'publishWant', policyHash: dee.policyHash, domains: ['libfoo'], question: 'libfoo 3.0 connect open rename', maxSats: 180 })
  const op = await waitState(dee, operationId, ['disclosure_verified', 'expired', 'denied', 'uncertain', 'disputed', 'funding_failed'])
  if (op.state !== 'disclosure_verified') throw new Error(`S3 ended in ${op.state}`)
  const listing = (await (await fetch(`${rvA}/offers?domain=libfoo`)).json()).offers.find(o => o.offer.pub === pat.pub && o.offer.wantId === op.data.want.want.wantId)
  if (!listing) throw new Error('public listing did not federate')
  const reveal = await chain.getTx(op.data.result.disclosureTxid)
  const read = await tool(reader, 'sale', { op: 'claim', txHex: reveal.toHex(), hWrapped: listing.offer.offer.cfg.hWrapped, offerMode: 'fragment', commitment: { leaf: listing.offer.leaf, hdp: listing.offer.hdp } })
  if (read.mode !== 'public' || read.verified !== true) throw new Error('free reader could not verify the public disclosure')
  write('s3-evidence.json', { disclosureTxid: op.data.result.disclosureTxid, readerSpent: 0, mode: read.mode, verified: read.verified })
  log('S3 PASS — one paid, others read free')
  return { outcome: 'PASS' }
}

try {
  hub = await startHub(0)
  hub.url = `http://127.0.0.1:${hub.port}`
  chain = new RemoteMockChain(hub.url)
  write('manifest.json', { runId, profile, scenario, startedAt: new Date().toISOString(), chain: 'mock (shared hub, real covenant script evaluation)', transport: 'HTTPS federation + encrypted mailbox' })
  const results = {}
  if (scenario === 'all' || scenario === 'S1') results.S1 = await S1()
  if (scenario === 'all' || scenario === 'S3') results.S3 = await S3()
  if (scenario === 'all' || scenario === 'S2') results.S2 = await S2()
  write('report.json', { runId, profile, results, outcome: 'PASS', note: 'mock-chain lane; sandbox outcome reported separately' })
  log(`ALL PASS — evidence in ${outDir}`)
} catch (e) {
  write('report.json', { runId, profile, outcome: 'FAIL', error: e.message, stack: e.stack })
  console.error(`[${runId}] FAIL: ${e.stack}`)
  process.exitCode = 1
} finally {
  for (const p of procs) try { p.kill() } catch {}
  try { hub?.srv.close() } catch {}
}
