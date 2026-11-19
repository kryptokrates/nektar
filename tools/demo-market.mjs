#!/usr/bin/env node
// tools/demo-market.mjs — the live testnet two-agent deal, one command.
// Creates sports + nutrition agents (agent-new), funds both from a testnet
// treasury, boots them against the nektar.run quorum, seeds one shard each,
// then sells and buys cross-wise and watches both deals reach disclosure.
//
//   node tools/demo-market.mjs                        # treasury from .state/testnet-wif.txt
//   node tools/demo-market.mjs --treasury-wif <wif>
//   node tools/demo-market.mjs --sats 70000 --price 4000 --timeout 480
//
// The agents stay up when the demo finishes — the join cards print at the end.
import { existsSync, readFileSync, openSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'
import { createRequire } from 'node:module'
import { spawn, execFileSync } from 'node:child_process'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const argv = process.argv.slice(2)
const flag = (k) => {
  const i = argv.indexOf(k)
  if (i < 0) return undefined
  return argv[i + 1]
}
const die = (m, code = 1) => {
  console.error(`demo-market: ${m}`)
  process.exit(code)
}
const ts = () => new Date().toISOString().slice(11, 23)
const log = (m) => console.log(`demo-market ${ts()} ${m}`)
const req = createRequire(join(root, 'packages/mcp/package.json'))
const { PrivateKey, Transaction, P2PKH } = req('@bsv/sdk')

const ARC = process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const INDEX = 'https://test.bananablocks.com/api/v1'
const agentSats = Number(flag('--sats') ?? 70_000)
const priceSats = Number(flag('--price') ?? 4_000)
const timeoutMs = Number(flag('--timeout') ?? 480) * 1000

// --- treasury ----------------------------------------------------------------
const wifArg = flag('--treasury-wif') ?? process.env.NEKTAR_TREASURY_WIF
const wifFile = join(root, '.state', 'testnet-wif.txt')
const wif = wifArg ?? (existsSync(wifFile) ? readFileSync(wifFile, 'utf8').trim().split(/\s+/)[0] : undefined)
if (wif === undefined) die('no treasury — pass --treasury-wif <wif> or write .state/testnet-wif.txt')
const treasury = PrivateKey.fromWif(wif)
const treasAddr = treasury.toAddress([0x6f]) // testnet — a mainnet-shaped addr fails the index lookup

// --- agents ------------------------------------------------------------------
const mkAgent = (name, port, domains) => {
  const out = execFileSync(
    process.execPath,
    [join(root, 'tools', 'agent-new.mjs'), '--testnet', '--name', name, '--port', String(port), '--domains', domains.join(','), '--json'],
    {
      encoding: 'utf8',
      stdio: ['ignore', 'pipe', 'inherit']
    }
  )
  return JSON.parse(out)
}
log('creating agents (agent-new)…')
const sports = mkAgent('demo-sports', Number(flag('--port-a') ?? 8099), ['sports', 'nutrition'])
const nutrition = mkAgent('demo-nutrition', Number(flag('--port-b') ?? 8100), ['sports', 'nutrition'])
log(`  sports    → ${sports.fundingAddress} (policy ${sports.policyHash.slice(0, 12)}…)`)
log(`  nutrition → ${nutrition.fundingAddress} (policy ${nutrition.policyHash.slice(0, 12)}…)`)

// --- fund both ---------------------------------------------------------------
const { indexFor } = await import(pathToFileURL(join(root, 'packages/chain/dist/src/indexer.js')).href)
const index = indexFor({ url: INDEX })
const utxos = await index.utxos(treasAddr)
if (utxos === undefined) die(`indexer could not serve unspents for ${treasAddr}`)
const need = agentSats * 2 + 2000
const picked = []
let vin = 0
for (const u of utxos.sort((a, b) => (a.satoshis ?? 0) - (b.satoshis ?? 0))) {
  picked.push(u)
  vin += u.satoshis ?? 0
  if (vin >= need) break
}
if (vin < need) die(`treasury ${treasAddr} holds ${vin} sats < ${need} needed — refill it first`)

const ftx = new Transaction()
for (const u of picked) {
  const hex = await index.txHex(u.txid)
  if (hex === undefined) die(`indexer has no bytes for parent ${u.txid} — wait a moment and retry`)
  ftx.addInput({ sourceTransaction: Transaction.fromHex(hex), sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(treasury) })
}
ftx.addOutput({ satoshis: agentSats, lockingScript: new P2PKH().lock(sports.fundingAddress) })
ftx.addOutput({ satoshis: agentSats, lockingScript: new P2PKH().lock(nutrition.fundingAddress) })
ftx.addOutput({ lockingScript: new P2PKH().lock(treasAddr), change: true })
await ftx.fee()
await ftx.sign()

const { BsvBroadcaster } = await import(pathToFileURL(join(root, 'packages/chain/dist/src/mainnet.js')).href)
const bc = new BsvBroadcaster({ broadcasterUrl: ARC, indexerUrl: INDEX })
const br = await bc.broadcast(ftx)
if (br.status !== 'success') die(`funding broadcast failed: ${br.description}`)
const fundingHex = ftx.toHex()
log(`funded both agents — ${ftx.id('hex')} (${agentSats} sats each)`)

// --- boot --------------------------------------------------------------------
const parseEnv = (file) =>
  Object.fromEntries(
    readFileSync(file, 'utf8')
      .split(/\r?\n/)
      .filter((l) => /^[A-Za-z_][A-Za-z0-9_]*=/.test(l))
      .map((l) => [l.slice(0, l.indexOf('=')), l.slice(l.indexOf('=') + 1)])
  )
const bin = join(root, 'packages/mcp/dist/src/bin.js')
const boot = (card) => {
  const fd = openSync(join(card.dataDir, 'agent.log'), 'a')
  const child = spawn(process.execPath, [`--env-file=${card.envFile}`, bin], {
    env: { ...process.env, ...parseEnv(card.envFile), NEKTAR_FUND_TXIDS: `hex:${fundingHex}` },
    detached: true,
    stdio: ['ignore', fd, fd]
  })
  child.unref()
  return child.pid
}
const pids = [boot(sports), boot(nutrition)]
log(`agents booting (pids ${pids.join(', ')}) — funding hex adopts at startup`)

const call = async (card, tool, args = {}) => {
  const r = await fetch(`http://127.0.0.1:${card.port}/v1/tools/${tool}`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: `Bearer ${card.bearer}` },
    body: JSON.stringify(args)
  })
  const j = await r.json().catch(() => ({}))
  if (!r.ok) throw new Error(`${tool} on :${card.port} — HTTP ${r.status}: ${j.error ?? JSON.stringify(j)}`)
  if (j?.error) throw new Error(`${tool} on :${card.port} — ${j.error}`)
  return j
}
for (const c of [sports, nutrition]) {
  const deadline = Date.now() + 60_000
  let up = false
  while (Date.now() < deadline) {
    const ok = await fetch(`http://127.0.0.1:${c.port}/v1/health`)
      .then((r) => r.ok)
      .catch(() => false)
    if (ok) {
      up = true
      break
    }
    await new Promise((r) => setTimeout(r, 500))
  }
  if (!up) die(`agent :${c.port} never answered /v1/health — see ${c.dataDir}/agent.log`)
}
log('both healthy — waiting for the funding UTXOs to land in custody')
for (const c of [sports, nutrition]) {
  const deadline = Date.now() + 60_000
  for (;;) {
    const f = await call(c, 'fund', {}).catch(() => undefined)
    if ((f?.spendableSats ?? 0) > 0) break
    if (Date.now() > deadline) die(`agent :${c.port} shows no spendable sats — see ${c.dataDir}/agent.log`)
    await new Promise((r) => setTimeout(r, 1000))
  }
}

// --- seed + trade --------------------------------------------------------------
const FACTS = {
  [sports.fundingAddress]: {
    card: sports,
    shard: 'sports',
    data: 'Recovery note: Zone-2 volume under 150bpm rebuilds aerobic base without taxing glycogen stores needed for match day.'
  },
  [nutrition.fundingAddress]: {
    card: nutrition,
    shard: 'nutrition',
    data: 'Fuel note: 30-60g carbohydrate per hour keeps blood glucose steady across a 90-minute effort; fat adaptation shifts the ceiling, not the floor.'
  }
}
for (const f of Object.values(FACTS)) {
  await call(f.card, 'remember', { shard: f.shard, data: f.data })
  await call(f.card, 'sell', { shard: f.shard, domains: [f.shard], priceSats, desc: `${f.shard} intel` })
  log(`${f.shard} seller armed`)
}
// each wants what the other sells — offers flow through the live quorum
await call(sports, 'want', { domains: ['nutrition'], question: 'post-match recovery fuel', maxSats: priceSats * 2 })
await call(nutrition, 'want', { domains: ['sports'], question: 'match-day conditioning intel', maxSats: priceSats * 2 })
log('wants published — deals drive themselves now (zeroconf escrow + carried ancestry)')

// --- watch ---------------------------------------------------------------------
const DONE_BUYER = new Set(['disclosure_verified', 'useful'])
const DONE_SELLER = new Set(['completion_submitted', 'settled', 'collected'])
const deadline = Date.now() + timeoutMs
// per-deal transition timeline — every (deal, state) pair's first-seen time,
// so the settle report shows where the deal actually waited
const firstSeen = new Map()
let last = ''
while (Date.now() < deadline) {
  const [sd, nd] = await Promise.all([call(sports, 'deals', {}), call(nutrition, 'deals', {})])
  const all = [...(sd.deals ?? []), ...(nd.deals ?? [])]
  for (const d of all) {
    const k = `${d.role}:${String(d.operationId ?? d.escrowTxid ?? '?').slice(-8)}:${d.state}`
    if (!firstSeen.has(k)) firstSeen.set(k, Date.now())
  }
  const buys = all.filter((d) => d.role === 'buyer' && DONE_BUYER.has(d.state)).length
  const sales = all.filter((d) => d.role === 'seller' && DONE_SELLER.has(d.state)).length
  const chainStr = (d) =>
    d.chain !== undefined
      ? `<${Object.entries(d.chain)
          .map(([k, v]) => `${k}=${v}`)
          .join(',')}>`
      : ''
  const line = `  deals: ${all.map((d) => `${d.role}:${d.state}${chainStr(d)}`).join('  ') || 'none yet'}`
  if (line !== last) {
    log(line)
    last = line
  }
  if (buys >= 2 && sales >= 2) {
    console.log('\n# BOTH DEALS SETTLED — live testnet, peer-carried ancestry, zeroconf')
    for (const d of all)
      console.log(
        `  ${d.role}  ${d.state}  escrow ${d.escrowTxid ?? '-'}  disclosure ${d.disclosureTxid ?? d.completeTxid ?? '-'}  sats ${d.sellerSats ?? d.priceSats ?? '-'}`
      )
    console.log('\n# transition timeline (first-seen, ms-since-wants)')
    const t0 = [...firstSeen.values()].sort()[0] ?? Date.now()
    for (const [k, t] of [...firstSeen.entries()].sort((a, b) => a[1] - b[1])) console.log(`  +${String(t - t0).padStart(6)}ms  ${k}`)
    console.log('\nagents still running:')
    console.log(`  sports    MCP ${sports.mcp}  bearer ${sports.bearer}  log ${sports.dataDir}\\agent.log`)
    console.log(`  nutrition MCP ${nutrition.mcp}  bearer ${nutrition.bearer}  log ${nutrition.dataDir}\\agent.log`)
    process.exit(0)
  }
  await new Promise((r) => setTimeout(r, 4000))
}
const t0 = [...firstSeen.values()].sort()[0] ?? Date.now()
console.error('\n# partial timeline at timeout (ms-since-wants)')
for (const [k, t] of [...firstSeen.entries()].sort((a, b) => a[1] - b[1])) console.error(`  +${String(t - t0).padStart(6)}ms  ${k}`)
die(`deals did not settle inside ${timeoutMs / 1000}s — agents still running, check deals on each instance`)
