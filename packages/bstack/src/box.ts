import { Hash, PrivateKey, ProtoWallet, Utils, type ChainTracker } from '@bsv/sdk'
import { sealEnvelope, encodePlaintext, envelopeRecord, envelopeCarrier, envelopeFundingLock } from '@lightwebinc/bbox-host/codec/send.js'
import { decodeEnvelope, encodeReceipt, checkBox, checkOffice, MaxAcks } from '@lightwebinc/bbox-host/codec/boxrec.js'
import { checkContent, verifyContentSignature } from '@lightwebinc/bbox-host/codec/content.js'
import { JObject, parseJSON, type JValue } from '@lightwebinc/bbox-host/codec/jcs.js'
import { admit } from '@lightwebinc/bbox-host/codec/admit.js'
import { submitAll, lookupAll, type Quorum } from './hosts.js'
import type { TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'

export const BOX = 'market'
export const DEFAULT_OFFICE = 'nektar_abcdefghij'
export const officeFor = (_net?: string): string => DEFAULT_OFFICE
const ORIGINATOR = 'nektar'

const toJ = (v: unknown): JValue => {
  if (v === null || typeof v === 'boolean' || typeof v === 'number' || typeof v === 'string') return v as JValue
  if (Array.isArray(v)) return v.map(toJ) as JValue
  const o = new JObject()
  for (const [k, x] of Object.entries(v as Record<string, unknown>)) o.set(k, toJ(x))
  return o
}
const fromJ = (v: JValue): unknown => {
  if (v instanceof JObject) {
    const o: Record<string, unknown> = {}
    for (const m of v.members) o[m.name] = fromJ(m.value)
    return o
  }
  if (Array.isArray(v)) return v.map(fromJ)
  return v
}
export const boxFundingLock = async (key: PrivateKey): Promise<import('@bsv/sdk').LockingScript> => await envelopeFundingLock(new ProtoWallet(key), ORIGINATOR)

export async function sendBox(p: {
  key: PrivateKey
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  office?: string
  to: string
  body: object
  expiresAt?: number
  quorum?: 'all' | 'majority' | 'one' | number
}): Promise<{ txid: string; took: string[]; admitted: string[]; missed: Array<{ host: string; error: string }> }> {
  const office = p.office ?? DEFAULT_OFFICE
  checkOffice(office)
  checkBox(BOX)
  const w = new ProtoWallet(p.key)
  const from = new Uint8Array(Array.from(p.key.toPublicKey().encode(true) as number[]))
  const to = new Uint8Array(Utils.toArray(p.to, 'hex'))
  const plain = encodePlaintext(new JObject().set('nektar', toJ(p.body)))
  const artifactHash = Utils.toHex(Hash.sha256(new Uint8Array([...to, ...plain])))
  const topics = [`tm_bbox_${office}`]
  const hit = await p.journal.get(artifactHash)
  let beef: number[], txid: string
  if (hit !== undefined) {
    beef = hit.beef
    txid = hit.txid
  } else {
    const slot = await p.ledger.take('bbox')
    if (slot === undefined) throw new Error('no funding slot')
    const nowSec = Math.floor(Date.now() / 1000)
    const content = await sealEnvelope(w, ORIGINATOR, from, to, plain, nowSec)
    const env = { office, to, box: BOX, from, created: nowSec, expires: p.expiresAt ?? nowSec + 86400, content, extra: [] }
    const record = envelopeRecord(env)
    const carrier = await envelopeCarrier(w, ORIGINATOR, record, slot.tree, slot.vout)
    beef = carrier.toAtomicBEEF()
    txid = carrier.id('hex')
    await p.journal.put(artifactHash, { txid, beef, topics })
  }
  // the journal is the retry layer — a dead host must not put seconds of
  // sleeps on the message path, and a send is done when quorum answered
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [], early: true })
  if (!r.ok) throw new Error(`box submit failed: ${JSON.stringify(r.missed).slice(0, 200)}`)
  return { txid, took: r.took, admitted: r.admitted, missed: r.missed }
}

export interface BoxItem {
  txid: string
  from: string
  body: object
  created: number
  commitment: string
}

const bytewise = (a: Uint8Array, b: Uint8Array): number => {
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i]! - b[i]!
  return a.length - b.length
}

export async function ackBox(p: {
  key: PrivateKey
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  office?: string
  items: Array<{ txid: string; commitment: string }>
  quorum?: Quorum
}): Promise<{ receipts: string[]; acked: number; resent: number; skipped: number }> {
  const office = p.office ?? DEFAULT_OFFICE
  checkOffice(office)
  const topics = [`tm_bbox_${office}`]
  const resend = new Map<string, { txid: string; beef: number[] }>()
  const fresh: Array<{ txid: string; commitment: string }> = []
  let resent = 0
  for (const i of p.items) {
    const hit = await p.journal.get(`ack:${i.txid}`)
    if (hit !== undefined) {
      resent++
      resend.set(hit.txid, { txid: hit.txid, beef: hit.beef })
    } else fresh.push(i)
  }
  let acked = 0,
    skipped = 0
  const receipts: string[] = []
  const w = new ProtoWallet(p.key)
  const by = new Uint8Array(Array.from(p.key.toPublicKey().encode(true) as number[]))
  for (let c = 0; c < fresh.length; c += MaxAcks) {
    const chunk = fresh.slice(c, c + MaxAcks)
    const slot = await p.ledger.take('bbox')
    if (slot === undefined) {
      skipped += chunk.length
      continue
    }
    const acks = [...new Map(chunk.map((i) => [i.commitment, new Uint8Array(Utils.toArray(i.commitment, 'hex'))])).values()].sort(bytewise)
    const record = encodeReceipt({ office, by, acks, created: Math.floor(Date.now() / 1000), extra: [] })
    const carrier = await envelopeCarrier(w, ORIGINATOR, record, slot.tree, slot.vout)
    const beef = carrier.toAtomicBEEF()
    const txid = carrier.id('hex')
    for (const i of chunk) await p.journal.put(`ack:${i.txid}`, { txid, beef, topics })
    resend.set(txid, { txid, beef })
    acked += chunk.length
  }
  // receipts fan out in parallel with no sleeps — the journal re-serves any
  // host that misses this round on the next cycle
  await Promise.all(
    [...resend.values()].map(async (r) => {
      const s = await submitAll(p.hosts, r.beef, topics, { quorum: p.quorum, retryWaits: [], early: true })
      if (s.ok) receipts.push(r.txid)
    })
  )
  return { receipts, acked, resent, skipped }
}

export async function readBox(p: {
  key: PrivateKey
  hosts: string[]
  office?: string
  tracker: ChainTracker
  limit?: number
  timeoutMs?: number
}): Promise<{ items: BoxItem[]; withheld: Array<{ host: string; txid: string }>; unreachable: string[] }> {
  const office = p.office ?? DEFAULT_OFFICE
  const me = p.key.toPublicKey().toString()
  const w = new ProtoWallet(p.key)
  // interactive read path: a host that cannot answer inside a few seconds is
  // down for deal purposes — reported unreachable, not waited on
  const r = await lookupAll(p.hosts, 'ls_bbox', { office, to: me }, { timeoutMs: p.timeoutMs ?? 5_000 })
  const items = new Map<string, BoxItem>()
  const perHost = new Map<string, string[]>()
  const unreachable: string[] = []
  for (const a of r.answers) {
    if ('error' in a) {
      unreachable.push(a.host)
      continue
    }
    // envelopes open in parallel — each admit may fetch a header
    const opened = await Promise.all(a.outputs.map(async (o) => await openEnvelope(o.beef, w, office, me, p.tracker).catch(() => undefined)))
    const ids: string[] = []
    for (const it of opened) {
      if (it !== undefined) {
        ids.push(it.txid)
        items.set(it.txid, it)
      }
    }
    perHost.set(a.host, ids)
  }
  const withheld: Array<{ host: string; txid: string }> = []
  for (const [host, ids] of perHost) for (const txid of items.keys()) if (!ids.includes(txid)) withheld.push({ host, txid })
  let out = [...items.values()]
  if (p.limit !== undefined) out = out.slice(0, p.limit)
  return { items: out, withheld, unreachable }
}

async function openEnvelope(beef: number[], w: ProtoWallet, office: string, me: string, tracker: ChainTracker): Promise<BoxItem | undefined> {
  // spec §8.1 via the real codec verdict, then §4.7 recipient checks in order
  const adm = await admit(new Uint8Array(beef), [], { office, headers: tracker }).catch(() => undefined)
  if (adm === undefined || adm.kind !== 'envelope' || adm.carrier === undefined) return undefined
  const env = decodeEnvelope(new Uint8Array(adm.carrier.record))
  if (env.office !== office) return undefined
  if (Utils.toHex(Array.from(env.to)) !== me) return undefined
  if (env.expires !== 0 && env.expires <= Math.floor(Date.now() / 1000)) return undefined
  const c = checkContent(env)
  verifyContentSignature(c, new Uint8Array(env.from))
  // §4.7 check 1 undecryptable → BRC-78 open
  const inner = JSON.parse(new TextDecoder().decode(new Uint8Array(env.content))) as { content: string }
  const brc78 = new Uint8Array(Array.from(Buffer.from(inner.content, 'base64')))
  const keyID = Utils.toBase64(Array.from(brc78.subarray(4 + 66, 4 + 66 + 32)))
  const ciphertext = brc78.subarray(4 + 66 + 32)
  const opened = await w
    .decrypt({ ciphertext: Array.from(ciphertext), protocolID: [2, 'message encryption'], keyID, counterparty: Utils.toHex(Array.from(env.from)) }, ORIGINATOR)
    .catch(() => undefined)
  if (opened === undefined) return undefined
  // §4.7 checks 2,3: plaintext-json / plaintext-shape
  const doc = parseJSON(new Uint8Array(opened.plaintext))
  if (!(doc instanceof JObject) || !doc.has('nektar')) return undefined
  return {
    txid: Utils.toHex(Array.from(adm.txid)),
    from: Utils.toHex(Array.from(env.from)),
    body: fromJ(doc.get('nektar')!) as object,
    created: env.created,
    commitment: Utils.toHex(Array.from(adm.carrier!.commitment))
  }
}
