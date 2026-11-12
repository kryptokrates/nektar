// bstack interop smoke on PUBLIC BSV testnet — proves the megaplan's object
// and carrier plane against the real network with stock reference hosts:
//
//   fund → mint 3 funding trees (market/bbox/finger) → one testnet block
//   → publish a want carrier + read it back via ls_nektar
//   → send a self-addressed bbox envelope (+ stock refs) + readBox it back
//   → publish kind-5/kind-6 store material under the identity
//   → publishFinger token → mined → re-submit with proof → resolveFinger
//
// Hosts are the two stock reference hosts from compose.testnet.yaml
// (bbox-host + nektar/blogs/finger modules, tracker=live chain shim):
//   docker compose -p nektar-bstack-testnet -f deploy/bstack-sandbox/compose.testnet.yaml up -d
//
// Funding: NEKTAR_FUND_WIF wins; else .state/ttn-wif.txt; else the ttn-data
//   keystore (.state/ttn-data/fee.key.json + NEKTAR_KEY_PASSPHRASE /
//   .state/ttn-passphrase). The key must hold testnet UTXOs (~10k sats).
// Run: node packages/bstack/tools/e2e-testnet.mjs
import { PrivateKey, Transaction, P2PKH } from '@bsv/sdk'
import { BsvBroadcaster, WocTracker, discoverUtxos, merklePathMainnet } from '@nektar/chain'
import {
  mintTree,
  MemoryTreeLedger,
  MemoryCarrierJournal,
  MemoryFingerState,
  boxFundingLock,
  fingerFundingLock,
  publishArtifact,
  readWants,
  sendBox,
  readBox,
  publishFinger,
  publishSubRecord,
  resolveFinger,
  signWant
} from '@nektar/bstack'
import { CborMap } from '@lightwebinc/bcommon'
import { existsSync, readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

// tools/ → bstack/ → packages/ → repo root; .state lives at root regardless of cwd
const stateFile = (rel) => new URL(`.state/${rel}`, new URL('../../../', import.meta.url))

let failures = 0
const check = (ok, name) => {
  console.log(`${ok ? 'PASS' : 'FAIL'} ${name}`)
  if (!ok) failures++
}
const sleep = (ms) => new Promise((r) => setTimeout(r, ms))

// ── config ──────────────────────────────────────────────────────────────────
const cfg = {
  broadcasterUrl: process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech',
  wocUrl: process.env.NEKTAR_WOC_URL ?? 'https://api.whatsonchain.com/v1/bsv/test',
  broadcasterApiKey: process.env.NEKTAR_BROADCASTER_API_KEY,
  wocApiKey: process.env.NEKTAR_WOC_API_KEY
}
const HOSTS = (process.env.NEKTAR_MARKET_HOSTS ?? 'http://127.0.0.1:18291,http://127.0.0.1:18292')
  .split(',')
  .map((s) => s.trim())
  .filter(Boolean)
const NET = 'testnet'
const broadcaster = new BsvBroadcaster(cfg)
const tracker = new WocTracker(cfg)

function loadWif() {
  if (process.env.NEKTAR_FUND_WIF) return process.env.NEKTAR_FUND_WIF
  const wifFile = fileURLToPath(stateFile('ttn-wif.txt'))
  if (existsSync(wifFile)) return readFileSync(wifFile, 'utf8').trim()
  const passFile = fileURLToPath(stateFile('ttn-passphrase'))
  const pass = process.env.NEKTAR_KEY_PASSPHRASE ?? (existsSync(passFile) ? readFileSync(passFile, 'utf8').trim() : undefined)
  const keyFile = fileURLToPath(stateFile('ttn-data/fee.key.json'))
  if (!existsSync(keyFile)) throw new Error('no NEKTAR_FUND_WIF, no .state/ttn-wif.txt, no key file')
  return import('../../mcp/dist/src/keystore.js').then((m) => JSON.parse(m.readKeyFile(keyFile, pass, true)).wif)
}

// ── 0. hosts reachable ───────────────────────────────────────────────────────
for (const h of HOSTS) {
  try {
    const r = await fetch(`${h}/`, { signal: AbortSignal.timeout(8_000) })
    check(true, `host reachable: ${h} (${r.status})`)
  } catch (e) {
    check(false, `host reachable: ${h} — ${e.message} (compose.testnet.yaml up?)`)
  }
}
if (failures > 0) {
  console.log('hosts unreachable — start compose.testnet.yaml first')
  process.exit(1)
}

// ── 1. funding ───────────────────────────────────────────────────────────────
const key = PrivateKey.fromWif(await loadWif())
const addr = key.toPublicKey().toAddress([0x6f])
console.log(`funding key → ${addr}`)
const utxos = await discoverUtxos(cfg, addr)
check(utxos.length > 0, `testnet UTXOs discovered (${utxos.length}, total ${utxos.reduce((a, u) => a + u.satoshis, 0)} sats)`)
if (utxos.length === 0) {
  console.log(`fund ${addr} via a testnet faucet, then re-run`)
  process.exit(1)
}
const pool = [...utxos]
const takeFunding = async (sats) => {
  const i = pool.findIndex((u) => u.satoshis >= sats + 200)
  if (i < 0) throw new Error(`no UTXO covers ${sats} sats`)
  return pool.splice(i, 1)[0]
}
// mintTree pays change back to the same P2PKH — chain the change output of
// each broadcast tx into the pool so a handful of UTXOs funds everything
const chainChange = (tx) => {
  const i = tx.outputs.length - 1 // mintTree appends change last
  const o = tx.outputs[i]
  if (o?.satoshis !== undefined && o.satoshis > 0) pool.push({ tx, vout: i, satoshis: o.satoshis })
}

// ── 2. mint + prove three funding trees (one block wait) ────────────────────
const ledger = new MemoryTreeLedger()
const journal = new MemoryCarrierJournal()
const trees = []
for (const [kind, lock] of [
  ['market', undefined],
  ['bbox', await boxFundingLock(key)],
  ['finger', await fingerFundingLock(key)]
]) {
  const funding = await takeFunding(800)
  const tx = await mintTree({ key, funding, n: 4, satsPerOutput: 1, ...(lock !== undefined ? { lock } : {}) })
  const br = await broadcaster.broadcast(tx)
  check(br.status === 'success', `${kind} funding tree broadcast (${tx.id('hex').slice(0, 16)}…)`)
  chainChange(tx)
  trees.push({ kind, tx })
}
console.log('waiting for a testnet block (typically ~10 min)…')
const deadline = Date.now() + 25 * 60_000
for (const t of trees) {
  const txid = t.tx.id('hex')
  let path
  while (Date.now() < deadline) {
    path = await merklePathMainnet(cfg, txid).catch(() => null)
    if (path?.merklePath) break
    await sleep(30_000)
    process.stdout.write('.')
  }
  check(path?.merklePath !== undefined, `${t.kind} tree mined — ${txid.slice(0, 16)}…`)
  if (path?.merklePath === undefined) continue
  await ledger.add(t.tx, 4, t.kind)
  await ledger.markProven(txid, path.merklePath)
}
if (failures > 0) process.exit(1)

// ── 3. market want carrier → ls_nektar read-back ────────────────────────────
const domain = 'e2e-testnet'
const sw = signWant(key, { domains: [domain], question: 'bstack testnet smoke', maxSats: 1, ts: Date.now(), exp: Date.now() + 3600_000, net: NET })
const pub = await publishArtifact({ key, ledger, journal, hosts: HOSTS, net: NET, payload: { v: 1, kind: 'want', body: sw } })
check(pub.missed.length === 0 && pub.admitted.length === HOSTS.length, `want carrier admitted by ${pub.admitted.length}/${HOSTS.length} hosts (${pub.txid.slice(0, 16)}…)`)
let wantBack
for (let i = 0; i < 20 && wantBack === undefined; i++) {
  const r = await readWants(HOSTS, { net: NET, domain }, { tracker, net: NET })
  wantBack = r.items.find((it) => it.payload?.body?.want?.wantId === sw.want.wantId)
  if (wantBack === undefined) await sleep(5000)
}
check(wantBack !== undefined, `want readable back via ls_nektar (wantId ${sw.want.wantId.slice(0, 12)}…)`)

// ── 4. bbox envelope + stock refs → ls_bbox read-back ────────────────────────
const ref = { url: 'https://nektar.run/.well-known/nektar', sha256: 'a'.repeat(64), length: 512 }
const sent = await sendBox({ key, ledger, journal, hosts: HOSTS, to: key.toPublicKey().toString(), body: { market: { type: 'smoke', at: Date.now() } }, refs: [ref] })
// submitAll resolves early once quorum lands — the remaining leg completes in
// the background, so `took` (answered ok) is the honest claim, not `admitted`
check(sent.missed.length === 0, `envelope submitted to ${sent.took.length}/${HOSTS.length} hosts (${sent.txid.slice(0, 16)}…)`)
let envBack
for (let i = 0; i < 20 && envBack === undefined; i++) {
  const r = await readBox({ key, hosts: HOSTS, tracker, timeoutMs: 15_000 })
  envBack = r.items.find((it) => it.txid === sent.txid)
  if (envBack === undefined) await sleep(5000)
}
check(envBack !== undefined, 'envelope opened via ls_bbox (BRC-78 sealed, stock plaintext)')
if (envBack !== undefined) {
  const got = envBack.refs?.find((r) => r.url === ref.url)
  check(got !== undefined, `stock refs[] member survived host round-trip (sha256 ${ref.sha256.slice(0, 12)}…)`)
}

// ── 5. store material: kind-5 member + kind-6 manifest ───────────────────────
const member = await publishSubRecord({
  key,
  ledger,
  journal,
  hosts: HOSTS,
  kind: 5,
  id: 1,
  body: new CborMap([{ key: 'v', val: 1n }, { key: 'shard', val: 'smoke' }, { key: 'sha256', val: new Uint8Array(32) }, { key: 'size', val: 1n }])
})
check(!!member.commitment, `kind-5 store member admitted (commitment ${member.commitment.slice(0, 16)}…)`)
const manifest = await publishSubRecord({
  key,
  ledger,
  journal,
  hosts: HOSTS,
  kind: 6,
  id: 1,
  body: new CborMap([
    { key: 'v', val: 1n },
    { key: 'name', val: 'e2e-smoke' },
    { key: 'members', val: [new CborMap([{ key: 'c', val: new Uint8Array(member.commitment.match(/../g).map((h) => parseInt(h, 16))) }])] }
  ])
})
check(!!manifest.commitment, `kind-6 manifest admitted (commitment ${manifest.commitment.slice(0, 16)}…)`)

// ── 6. finger record → mined token → ls_finger resolution ───────────────────
const state = new MemoryFingerState()
const fetchTx = async (txid) => {
  const r = await fetch(`${cfg.wocUrl}/tx/${txid}/hex`, { signal: AbortSignal.timeout(15_000) }).catch(() => null)
  if (r === null || !r.ok) return undefined
  return Transaction.fromHex((await r.text()).trim())
}
const broadcast = async (tx) => broadcaster.broadcast(tx)
const body = { v: 1, hosts: HOSTS, office: 'nektar_abcdefghij', stream: 'nektar_abcdefghij', nets: [NET] }
let rec = await publishFinger({ key, state, ledger, journal, hosts: HOSTS, body, takeFunding, broadcast, fetchTx, allowHttp: true })
check(rec.changed && rec.tokenTxid !== undefined, `finger record + token submitted (${rec.tokenTxid?.slice(0, 16)}…)`)
console.log('waiting for the record token to mine (~10 min)…')
{
  const dl = Date.now() + 25 * 60_000
  let mined = false
  while (Date.now() < dl && !mined) {
    mined = (await merklePathMainnet(cfg, rec.tokenTxid).catch(() => null)) !== null
    if (!mined) {
      await sleep(30_000)
      process.stdout.write('.')
    }
  }
  check(mined, `record token mined (${rec.tokenTxid.slice(0, 16)}…)`)
}
// second call is idempotent — attaches the mined proof to the stored tx
rec = await publishFinger({ key, state, ledger, journal, hosts: HOSTS, body, takeFunding, broadcast, fetchTx, allowHttp: true })
let resolved
for (let i = 0; i < 20 && resolved === undefined; i++) {
  resolved = await resolveFinger({ hosts: HOSTS, pub: key.toPublicKey().toString(), tracker, allowHttp: true }).catch(() => undefined)
  if (resolved === undefined) await sleep(5000)
}
check(resolved !== undefined, `identity record resolvable via ls_finger (seq ${resolved?.seq}, answered by ${resolved?.hostsAnswered?.length ?? 0} hosts)`)
if (resolved !== undefined) check(resolved.body.office === body.office && resolved.body.stream === body.stream, 'resolved body matches what we published')

console.log(failures === 0 ? '\ntestnet smoke: all checks passed' : `\ntestnet smoke: ${failures} check(s) FAILED`)
process.exit(failures === 0 ? 0 : 1)
