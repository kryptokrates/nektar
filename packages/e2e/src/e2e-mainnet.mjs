#!/usr/bin/env node
// Opt-in repeatable live BSV mainnet E2E smoke runner.
//
// Safe by default:
//   - `--dry-run`: runs zero-sat `runMainnetCheck`, inspects live treasury
//     balance, and prints the exact satoshi cost estimate (0 sats spent).
//   - Live mode requires `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` plus a funded
//     gitignored `.state/mainnet-test/treasury.json` (40,000–50,000 sats float;
//     ~4,500–6,500 sats net cost; remainder auto-swept to `NEKTAR_SWEEP_TO`).
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const ROOT = resolve(fileURLToPath(new URL('../../..', import.meta.url)))
const dist = (p) => pathToFileURL(join(ROOT, p)).href

const { PrivateKey, Transaction, P2PKH } = await import(dist('packages/mcp/node_modules/@bsv/sdk/dist/esm/mod.js'))
const { nap, writeOut, nektarEnv, setNektarEnv } = await import(dist('packages/core/dist/src/index.js'))
const { livePort, indexFailover, merklePathMainnet, ReserveFunder, fundsOf, WitnessedTracker } = await import(dist('packages/chain/dist/src/index.js'))
const { minihost } = await import(dist('packages/bstack/dist/src/testing.js'))
const { NektarMemory } = await import(dist('packages/mcp/dist/src/memory.js'))
const { callTool } = await import(dist('packages/mcp/dist/src/server.js'))
const { runMainnetCheck } = await import(dist('packages/mcp/dist/src/mainnet-check.js'))
const { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS, Dispatcher } = await import(dist('packages/automation/dist/src/index.js'))

const dryRun = process.argv.includes('--dry-run')
const stateDir = join(ROOT, '.state/mainnet-test')
const treasuryFile = join(stateDir, 'treasury.json')

const mc = {
  broadcasterUrl: 'https://arcade.gorillapool.io',
  broadcasterUrls: ['https://arcade.gorillapool.io', 'https://arcade-v2-us-1.bsvblockchain.tech'],
  indexerUrl: 'https://bananablocks.com/api/v1',
  indexerUrls: ['https://api.whatsonchain.com/v1/bsv/main']
}

const log = (m) => writeOut(`${m}\n`)
const index = indexFailover([mc.indexerUrl, ...mc.indexerUrls])

function ensureKeys() {
  mkdirSync(stateDir, { recursive: true })
  if (existsSync(treasuryFile)) return JSON.parse(readFileSync(treasuryFile, 'utf8'))
  const mk = () => {
    const k = PrivateKey.fromRandom()
    return { address: k.toPublicKey().toAddress([0x00]), pub: k.toPublicKey().toString(), wif: k.toWif() }
  }
  const rec = { network: 'main', createdAt: new Date().toISOString(), treasury: mk(), alice: mk(), ben: mk() }
  writeFileSync(treasuryFile, JSON.stringify(rec, null, 2), { mode: 0o600 })
  return rec
}

const keys = ensureKeys()

if (dryRun) {
  const chk = await runMainnetCheck({
    dataDir: stateDir,
    env: {
      ...process.env,
      NEKTAR_MAINNET_CONFIRM: 'I_UNDERSTAND',
      NEKTAR_WALLET: 'toolbox',
      NEKTAR_KEY_PASSPHRASE: nektarEnv('NEKTAR_KEY_PASSPHRASE') ?? 'dryrun-only'
    }
  })
  const utxos = await index.utxos(keys.treasury.address).catch(() => [])
  const bal = utxos.reduce((s, u) => s + u.satoshis, 0)
  log(`e2e-mainnet --dry-run (0 sats spent)`)
  log(`  check-mainnet: ${chk.ok ? 'READY' : 'NOT READY'}`)
  for (const c of chk.checks) log(`    [${c.status.toUpperCase()}] ${c.name}: ${c.detail}`)
  log(`  treasury address: ${keys.treasury.address} (live balance: ${bal} sats across ${utxos.length} UTXO(s))`)
  log(`  recommended live float: 40,000 - 50,000 sats (minimum 30,000 sats; ~4,500 - 6,500 sats net cost; remainder swept to NEKTAR_SWEEP_TO)`)
  process.exit(chk.ok ? 0 : 1)
}

if (nektarEnv('NEKTAR_MAINNET_CONFIRM') !== 'I_UNDERSTAND') {
  throw new Error('Refusing live mainnet spend: set NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND (or pass --dry-run for zero-sat preflight)')
}
const sweepTo = nektarEnv('NEKTAR_SWEEP_TO')
if (!sweepTo) {
  throw new Error('Set NEKTAR_SWEEP_TO=<1... mainnet address> so leftover sats sweep back automatically')
}

setNektarEnv('NEKTAR_PAID_DISCLOSURE', '1')
setNektarEnv('NEKTAR_SETTLE_DEPTH', '1')
setNektarEnv('NEKTAR_ANCHORED_LANES', undefined)

const runDir = join(stateDir, `e2e-${Date.now().toString(36)}`)
const hostDir = join(runDir, 'hosts')
mkdirSync(hostDir, { recursive: true })

const hostPort = await livePort(mc, 'main', { dataDir: hostDir })
const startTip = await hostPort.height()
log(`e2e-mainnet: livePort ready (tip=${startTip}, sweepTo=${sweepTo})`)

const tUtxos = await index.utxos(keys.treasury.address)
const totalFund = tUtxos.reduce((s, u) => s + u.satoshis, 0)
if (totalFund < 30_000) {
  throw new Error(`Treasury ${keys.treasury.address} has ${totalFund} sats (< 30,000 sats minimum). Fund 40,000-50,000 sats and retry.`)
}

// Split treasury into alice (2 main) + ben (2 main)
const tKey = PrivateKey.fromWif(keys.treasury.wif)
const splitTx = new Transaction()
for (const u of tUtxos) {
  const hex = await index.txHex(u.txid)
  if (!hex) throw new Error(`Could not fetch funding tx ${u.txid}`)
  splitTx.addInput({ sourceTransaction: Transaction.fromHex(hex), sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(tKey) })
}
const splitFee = Math.max(200, Math.ceil(((tUtxos.length * 148 + 4 * 34 + 20) * 150) / 1000))
const each = Math.floor((totalFund - splitFee) / 4)
splitTx.addOutput({ lockingScript: new P2PKH().lock(keys.alice.address), satoshis: each })
splitTx.addOutput({ lockingScript: new P2PKH().lock(keys.alice.address), satoshis: each })
splitTx.addOutput({ lockingScript: new P2PKH().lock(keys.ben.address), satoshis: each })
splitTx.addOutput({ lockingScript: new P2PKH().lock(keys.ben.address), satoshis: totalFund - splitFee - each * 3 })
await splitTx.sign()
const splitRes = await hostPort.broadcast(splitTx)
if (splitRes.status !== 'success') throw new Error(`Split broadcast failed: ${splitRes.description}`)
log(`e2e-mainnet: split ${totalFund} sats -> ${splitTx.id('hex')}`)

const h1 = await minihost(['main'], { tracker: hostPort.tracker, office: 'nektar_abcdefghij', logStream: 'nektar_abcdefghij', finger: true })
const h2 = await minihost(['main'], { tracker: hostPort.tracker, office: 'nektar_abcdefghij', logStream: 'nektar_abcdefghij', finger: true })
const hosts = [h1.url, h2.url]
const domain = `e2e-main-${Date.now().toString(36)}`
const finding = 'Live mainnet e2e verified finding: valve torque is 45Nm.'

async function seed(name, wif, vouts) {
  const dataDir = join(runDir, name)
  mkdirSync(dataDir, { recursive: true })
  const fundKey = PrivateKey.fromWif(wif)
  const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  const reserve = new ReserveFunder(fundKey, join(dataDir, 'reserve.json'))
  for (const v of vouts) reserve.addOwned(splitTx, v)
  const port = await livePort(mc, 'main', {
    dataDir,
    valueOf: (txid, vout) => reserve.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis
  })
  const chain = {
    broadcast: (tx) => port.broadcast(tx),
    currentHeight: () => port.height(),
    isValidRootForHeight: (r, h) => port.tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h) => port.tracker.headerAtHeight(h),
    tipHeight: () => port.height()
  }
  const mem = new NektarMemory(chain, reserve, fundKey, {
    network: 'main',
    modelId: 'e2e-mainnet',
    feeRateSatPerKb: 100,
    statusHint: port.statusHint,
    batchPolicy: { maxLeaves: 1 },
    funds: fundsOf(reserve, {
      pkhHex,
      proofOf: async (tx) => {
        const mp = await port.proofLookup?.(tx.id('hex'))
        if (mp !== undefined) tx.merklePath = mp
      }
    })
  })
  if (port.proofLookup) mem.proofLookup = port.proofLookup
  if (port.txLookup) mem.txLookup = port.txLookup
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: 'main',
    localHosts: true,
    dataDir,
    marketHosts: hosts,
    intervalMs: 0,
    exec: new Dispatcher(),
    call: (tool, args) => callTool(mem, tool, args),
    ai: new ScriptedExecutor(),
    publishFinger: false,
    treeSize: 4,
    treeLowWater: 2,
    reserveOuts: 2,
    opts: { offerTtlBlocks: 48, maxZeroconfSats: 100_000, maxUnconfirmedAncestors: 8 }
  })
  mem.automation = rt
  const policyHash = await rt.store.approvePolicy(
    {
      v: 1,
      policyId: `e2e-${name}`,
      network: 'main',
      agentPub: pub,
      roles: ['buyer', 'seller'],
      notBefore: 0,
      notAfter: 4_000_000_000_000,
      domains: [domain],
      limits: {
        perPurchaseSats: 500,
        dailyPurchaseSats: 50_000,
        perTxFeeSats: 1000,
        dailyFeeSats: 50_000,
        runTotalSats: 500_000,
        maxLockedSats: 5000,
        maxUnconfirmedSats: 5000,
        maxUnconfirmedCount: 16
      },
      scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: 10_000 },
      seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: 50, sellableShards: ['guide'] },
      disclosure: { allowPublic: false },
      ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
      sandbox: { mode: 'sandbox-only' }
    },
    'e2e:mainnet'
  )
  return { mem, rt, policyHash }
}

let alice, ben
try {
  ;[alice, ben] = await Promise.all([seed('alice', keys.alice.wif, [0, 1]), seed('ben', keys.ben.wif, [2, 3])])
  await Promise.all([alice.rt.upkeep(), ben.rt.upkeep()])
  await ben.mem.remember('guide', finding)
  await ben.mem.describeShard('guide', { domains: [domain], offers: [{ kind: 'record', priceSats: 200 }] })
  const anchorTxid = ben.mem.anchorLinks('guide')[0]?.txid
  const ga = await callTool(ben.mem, 'groundedAnswer', { claims: [{ text: 'valve torque is 45Nm', cites: [{ shard: 'guide', seq: 0 }] }] })
  log(`e2e-mainnet: waiting for 1 block confirmation of anchor ${anchorTxid} & lane trees...`)
  for (;;) {
    const mp = await merklePathMainnet(mc, anchorTxid).catch(() => null)
    if (mp?.merklePath) {
      ben.mem.confirmAnchor('guide', anchorTxid)
      const vr = await callTool(ben.mem, 'verifyReceipt', { token: ga.receiptToken })
      if (!vr.ok) throw new Error(`verifyReceipt failed: ${JSON.stringify(vr)}`)
      log(`e2e-mainnet: SPV verifyReceipt PASS at height ${mp.merklePath.blockHeight}`)
      break
    }
    await nap(15_000)
  }
  for (let i = 0; i < 40; i++) {
    await Promise.all([alice.rt.upkeep(), ben.rt.upkeep()])
    const [as1, bs1] = await Promise.all([alice.rt.laneStock(), ben.rt.laneStock()])
    if (as1.bbox.proven > 0 && bs1.bbox.proven > 0) break
    await nap(10_000)
  }
  await ben.rt.engine.enableSeller(ben.policyHash)
  const w1 = await alice.rt.engine.publishWant({ domains: [domain], question: 'valve torque?', maxSats: 450 }, alice.policyHash)
  for (let i = 0; i < 300; i++) {
    await Promise.all([alice.rt.deals(), ben.rt.deals()])
    const bo = (await alice.rt.store.operations({ role: 'buyer' })).find((o) => o.data.want?.want?.wantId === w1.wantId)
    if (bo && ['disclosure_verified', 'useful'].includes(bo.state)) {
      log(`e2e-mainnet: Deal 1 PASS escrow=${bo.data.escrowTxid} disclosure=${bo.data.result?.disclosureTxid}`)
      break
    }
    await nap(500)
  }
  // Persisted headers-cache offline PoW reload check
  const oldH = startTip - 20
  const oldHdr = await hostPort.tracker.headerAtHeight(oldH)
  hostPort.tracker.flushPersisted?.()
  const offline = {
    currentHeight: () => Promise.reject(new Error('offline')),
    headerAtHeight: () => Promise.reject(new Error('offline')),
    isValidRootForHeight: () => Promise.reject(new Error('offline'))
  }
  const reloaded = new WitnessedTracker(offline, offline, { persistPath: join(hostDir, 'headers-cache.json') })
  if (!oldHdr || !(await reloaded.isValidRootForHeight(oldHdr.merkleRoot, oldH))) throw new Error('headers-cache offline PoW reload failed')
  log(`e2e-mainnet: headers-cache offline PoW reload PASS (height=${oldH})`)
} finally {
  await alice?.rt.stop().catch(() => {})
  await ben?.rt.stop().catch(() => {})
  await h1.close()
  await h2.close()
}

// Conflict-free return sweep of all remaining P2PKH UTXOs back to NEKTAR_SWEEP_TO
await nap(5_000)
const wallets = [
  { name: 'treasury', addr: keys.treasury.address, key: PrivateKey.fromWif(keys.treasury.wif) },
  { name: 'alice', addr: keys.alice.address, key: PrivateKey.fromWif(keys.alice.wif) },
  { name: 'ben', addr: keys.ben.address, key: PrivateKey.fromWif(keys.ben.wif) }
]
const lockMap = new Map(wallets.map((w) => [new P2PKH().lock(w.addr).toHex(), w]))
const txMap = new Map()
const spent = new Set()
const cands = new Map()
for (const w of wallets) {
  const list = await index.utxos(w.addr).catch(() => [])
  for (const u of list) {
    if (!txMap.has(u.txid)) {
      const hex = await index.txHex(u.txid).catch(() => undefined)
      if (hex) txMap.set(u.txid, Transaction.fromHex(hex))
    }
    const tx = txMap.get(u.txid)
    const out = tx?.outputs[u.vout]
    if (!tx || !out) continue
    const owner = lockMap.get(out.lockingScript.toHex())
    if (owner) cands.set(`${u.txid}:${u.vout}`, { tx, vout: u.vout, satoshis: out.satoshis, key: owner.key })
  }
}
for (const tx of txMap.values()) for (const inp of tx.inputs) if (inp.sourceTXID) spent.add(`${inp.sourceTXID}:${inp.sourceOutputIndex}`)
const ins = [...cands.entries()].filter(([op, it]) => !spent.has(op) && it.satoshis > 30).map(([, it]) => it)
const inTotal = ins.reduce((s, i) => s + i.satoshis, 0)
if (inTotal > 300) {
  const sw = new Transaction()
  for (const i of ins) sw.addInput({ sourceTransaction: i.tx, sourceOutputIndex: i.vout, unlockingScriptTemplate: new P2PKH().unlock(i.key) })
  const fee = Math.max(200, Math.ceil(((ins.length * 148 + 44) * 150) / 1000))
  sw.addOutput({ lockingScript: new P2PKH().lock(sweepTo), satoshis: inTotal - fee })
  await sw.sign()
  const sr = await hostPort.broadcast(sw)
  log(`e2e-mainnet: return sweep ${sw.id('hex')} -> ${sr.status} (${inTotal - fee} sats -> ${sweepTo})`)
}
