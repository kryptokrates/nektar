#!/usr/bin/env node
// E10 — anchored-admission verdict against patched vendored bbox hosts.
//
// The patch under test (deploy/bstack-sandbox/modules/bbox/bbox-module.js):
// checkCarrierBEEF now accepts a carrier's BEEF of N>=2 transactions where the
// one BUMP anchors any ancestor — not only the direct parent — provided every
// tx is the carrier or descends to it. Vectors:
//
//   a) classic 2-tx carrier on a proven tree            → ADMITTED (regression)
//   b) anchored 3-tx carrier: unproven tree, proven
//      grandparent                                    → ADMITTED (the unlock)
//   c) anchored 3-tx + unrelated 4th tx                 → 'beef' refusal
//      (the descendancy rule still bites — no smuggling)
//
// Usage: node tools/tn-anchored-probe.mjs [host] — default the patched
// testnet host-a. Spends one reserve utxo (~2k sats in tree minting).
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath, pathToFileURL } from 'node:url'

const ROOT = fileURLToPath(new URL('..', import.meta.url))
const dist = (p) => pathToFileURL(join(ROOT, p)).href
const { PrivateKey, ProtoWallet, Transaction, Beef } = await import(dist('packages/mcp/node_modules/@bsv/sdk/dist/esm/mod.js'))
const { merklePathMainnet } = await import(dist('packages/chain/dist/src/index.js'))
const { mintTree } = await import(dist('packages/bstack/dist/src/tree.js'))
const codecDir = readdirSync(join(ROOT, 'node_modules/.pnpm')).find((d) => d.startsWith('@lightwebinc+bbox-host@'))
const codec = await import(dist(`node_modules/.pnpm/${codecDir}/node_modules/@lightwebinc/bbox-host/dist/send.js`))
const { JObject } = await import(dist(`node_modules/.pnpm/${codecDir}/node_modules/@lightwebinc/bbox-host/dist/jcs.js`))
const { sealEnvelope, encodePlaintext, envelopeRecord, envelopeCarrier, envelopeFundingLock } = codec
const toJ = (v) => {
  if (v === null || typeof v === 'number' || typeof v === 'string' || typeof v === 'boolean') return v
  if (v instanceof Uint8Array) return Array.from(v)
  if (Array.isArray(v)) return v.map(toJ)
  const o = new JObject()
  for (const [k, x] of Object.entries(v)) o.set(k, toJ(x))
  return o
}

const HOST = process.argv[2] ?? 'http://127.0.0.1:28291'
const TOPIC = 'tm_bbox_nektar_abcdefghij'
const OFFICE = 'nektar_abcdefghij'
const ORIGINATOR = 'nektar'
const INDEX = process.env.NEKTAR_TN_INDEX ?? 'https://test.bananablocks.com/api/v1'
const KEYS = JSON.parse(readFileSync(join(ROOT, '.state/testnet-agents/agent-keys.json'), 'utf8'))

const key = PrivateKey.fromWif(KEYS.alice)
const wallet = new ProtoWallet(key)
const pub = Uint8Array.from(key.toPublicKey().encode(true))

const post = async (beef, label, topic = TOPIC) => {
  const r = await fetch(`${HOST}/submit`, {
    method: 'POST',
    headers: { 'content-type': 'application/octet-stream', 'x-topics': JSON.stringify([topic]) },
    body: Uint8Array.from(beef),
    signal: AbortSignal.timeout(30_000)
  })
  const text = (await r.text()).slice(0, 300)
  console.log(`${label}: http ${r.status} ${text}`)
  return { status: r.status, text }
}

const ARC = process.env.NEKTAR_TN_ARC ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech'
const attachProof = async (tx) => {
  if (tx.merklePath !== undefined) return tx
  const r = await merklePathMainnet({ broadcasterUrl: ARC, indexerUrl: INDEX }, tx.id('hex'))
  if (r === null) throw new Error(`no proof for ${tx.id('hex')}`)
  tx.merklePath = r.merklePath
  return tx
}

// the suite's own lanes hold everything the classic carrier needs; the
// anchored case needs one fresh unproven tree over a proven treasury parent.
const unprovenAnchoredTree = async () => {
  const res = JSON.parse(readFileSync(join(ROOT, '.state/testnet-agents/alice/reserve.json'), 'utf8'))
  const parent = res.entries.find((e) => e.satoshis > 5000)
  if (parent === undefined) throw new Error('no reserve entry >5k sats')
  const tx = Transaction.fromHex(parent.txHex)
  await attachProof(tx)
  const lock = await envelopeFundingLock(wallet, ORIGINATOR)
  return await mintTree({ key, wallet, funding: { tx, vout: parent.vout, satoshis: parent.satoshis }, n: 3, satsPerOutput: 1000, lock })
}

const nowSec = Math.floor(Date.now() / 1000)
const mkEnvelopeCarrier = async (tree, vout) => {
  const from = pub,
    to = pub
  const doc = new JObject().set('nektar', toJ({ v: 1, kind: 'anchored-probe' }))
  const content = await sealEnvelope(wallet, ORIGINATOR, from, to, encodePlaintext(doc), nowSec)
  const env = { office: OFFICE, to, box: 'market', from, created: nowSec, expires: nowSec + 3600, content, extra: [] }
  const record = envelopeRecord(env)
  return await envelopeCarrier(wallet, ORIGINATOR, record, tree, vout)
}

// (a) regression control: the E6 foreign-chain carrier, whose BUMP anchors a
// devchain parent — the engine's SPV gate must still refuse it post-patch.
const fcat = readFileSync(join(ROOT, '.state/beefcat/stream3.bcat'))
const carrierLen = fcat.readUInt32BE(8)
const foreign = fcat.subarray(12, 12 + carrierLen)
await post(Array.from(foreign), 'a) foreign-chain carrier (engine SPV)')

// (b) anchored: fresh unproven tree over a proven treasury parent.
const tree = await unprovenAnchoredTree()
const carrierB = await mkEnvelopeCarrier(tree, 0)
const beefB = carrierB.toAtomicBEEF()
const parsed = Beef.fromBinary(Uint8Array.from(beefB))
console.log(`b) anchored beef: ${parsed.txs.length} txs, ${parsed.bumps.length} bumps`)
await post(beefB, 'b) anchored 3-tx carrier (unproven tree, proven grandparent)')

// (c) anchored + one unrelated carried tx — descendancy must refuse. The
// subject is a fresh carrier on slot 1; the stray carrier on slot 2 is never
// submitted alone, so merged into this beef it is an unrelated extra tx.
const carrierC = await mkEnvelopeCarrier(tree, 1)
const stray = await mkEnvelopeCarrier(tree, 2)
const beefC = Beef.fromBinary(Uint8Array.from(carrierC.toAtomicBEEF()))
beefC.mergeBeef(stray.toAtomicBEEF())
await post(Array.from(beefC.toBinary()), 'c) anchored carrier + unrelated carrier tx')
