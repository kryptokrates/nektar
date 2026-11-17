import { Hash, PublicKey, Utils, type ChainTracker, type WalletInterface } from '@bsv/sdk'
import { toSigner, signerPubHex, type SignerLike } from '@nektar/core'
import { sealEnvelope, encodePlaintext, envelopeRecord, envelopeCarrier, envelopeFundingLock } from '@lightwebinc/bbox-host/codec/send.js'
import { decodeEnvelope, encodeReceipt, checkBox, checkOffice, MaxAcks } from '@lightwebinc/bbox-host/codec/boxrec.js'
import { checkContent, verifyContentSignature } from '@lightwebinc/bbox-host/codec/content.js'
import { open as openPlaintext, parsePlaintext, MaxRefs } from '@lightwebinc/bbox-host/codec/payment.js'
import { JObject, type JValue } from '@lightwebinc/bbox-host/codec/jcs.js'
import { admit } from '@lightwebinc/bbox-host/codec/admit.js'
import { submitAll, lookupAll, VerifiedCache, type Quorum } from './hosts.js'
import { carrierFundingLockHex, walletOrProto } from './carrier.js'
import { fundingSlotReason, type TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'

export const BOX = 'market'
export const DEFAULT_OFFICE = 'nektar_abcdefghij'
export const officeFor = (_net?: string): string => DEFAULT_OFFICE

/** A stock plaintext reference to an external object (spec §4.5): digests are
 *  lowercase hex on the wire; urls admit https:// and uhrp:// only — a
 *  chirp:// identifier does not ride here, put the store URL instead. */
export interface BoxRef {
  url: string
  sha256: string
  length: number
  key?: string
}
const ORIGINATOR = 'nektar'

/** spec §8.3 answer window — hosts refuse `created` further ahead than this */
const ENVELOPE_CREATED_SKEW_S = 3600
/** recipient-side cap on sender-declared envelope lifetime (7d) */
const ENVELOPE_MAX_HOLD_S = 7 * 24 * 3600

const toJ = (v: unknown): JValue => {
  if (v === null || typeof v === 'boolean' || typeof v === 'number' || typeof v === 'string') return v
  if (Array.isArray(v)) return v.map(toJ)
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
type BoxWallet = Pick<WalletInterface, 'getPublicKey' | 'createSignature' | 'encrypt' | 'decrypt'>

export const boxFundingLock = async (key: SignerLike, wallet?: BoxWallet): Promise<import('@bsv/sdk').LockingScript> =>
  await envelopeFundingLock(walletOrProto(key, wallet), ORIGINATOR)

export async function sendBox(p: {
  key: SignerLike
  wallet?: BoxWallet
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  office?: string
  to: string
  body: object
  /** external objects the body points at — deal evidence, packs — carried as
   *  the stock plaintext `refs` member instead of nektar-body fields, so any
   *  stock reader sees a digest-pinned locator, not app JSON. */
  refs?: BoxRef[]
  expiresAt?: number
  quorum?: 'all' | 'majority' | 'one' | number
  /** fired the moment the signed carrier exists (journaled, before the host
   *  fanout) — direct-push to a present peer overlaps the submitAll RTT */
  push?: (beef: number[]) => void
}): Promise<{
  txid: string
  hash: string
  took: string[]
  admitted: string[]
  missed: Array<{ host: string; error: string }>
  pending: string[]
  settled: Promise<{ took: string[]; missed: Array<{ host: string; error: string }> }>
}> {
  const office = p.office ?? DEFAULT_OFFICE
  checkOffice(office)
  checkBox(BOX)
  if (p.refs !== undefined && p.refs.length > MaxRefs) throw new Error(`sendBox: ${p.refs.length} refs > ${MaxRefs}`)
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  const from = new Uint8Array(Array.from(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[]))
  const to = new Uint8Array(Utils.toArray(p.to, 'hex'))
  const doc = new JObject().set('nektar', toJ(p.body))
  if (p.refs !== undefined && p.refs.length > 0) doc.set('refs', toJ(p.refs))
  const plain = encodePlaintext(doc)
  const artifactHash = Utils.toHex(Hash.sha256(new Uint8Array([...to, ...plain])))
  const topics = [`tm_bbox_${office}`]
  const bboxLockHex = (await boxFundingLock(s, w)).toHex()
  let hit = await p.journal.get(artifactHash)
  // a journaled carrier minted under a rotated identity can never evaluate —
  // drop it and let the mint path re-derive under the current key
  if (hit !== undefined && carrierFundingLockHex(hit.beef) !== bboxLockHex && p.journal.del !== undefined) {
    await p.journal.del(artifactHash)
    hit = undefined
  }
  let beef: number[], txid: string
  if (hit !== undefined) {
    beef = hit.beef
    txid = hit.txid
  } else {
    const slot = await p.ledger.take('bbox', bboxLockHex)
    if (slot === undefined) throw new Error(await fundingSlotReason(p.ledger, 'bbox'))
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
  p.push?.(beef)
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [], early: true })
  if (!r.ok) throw new Error(`box submit failed: ${JSON.stringify(r.missed).slice(0, 200)}`)
  return { txid, hash: artifactHash, took: r.took, admitted: r.admitted, missed: r.missed, pending: r.pending, settled: r.settled }
}

export interface BoxItem {
  txid: string
  from: string
  body: object
  /** stock plaintext refs the sender attached (object locators, digest-pinned) */
  refs?: BoxRef[]
  created: number
  commitment: string
}

const bytewise = (a: Uint8Array, b: Uint8Array): number => {
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i] - b[i]
  return a.length - b.length
}

export async function ackBox(p: {
  key: SignerLike
  wallet?: BoxWallet
  ledger: TreeLedger
  journal: CarrierJournal
  hosts: string[]
  office?: string
  items: Array<{ txid: string; commitment: string }>
  quorum?: Quorum
}): Promise<{ receipts: string[]; acked: number; resent: number; skipped: number; stockReason?: string }> {
  const office = p.office ?? DEFAULT_OFFICE
  checkOffice(office)
  const topics = [`tm_bbox_${office}`]
  const resend = new Map<string, { txid: string; beef: number[] }>()
  const fresh: Array<{ txid: string; commitment: string }> = []
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  const by = new Uint8Array(Array.from(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[]))
  const bboxLock = (await boxFundingLock(s, w)).toHex()
  let resent = 0
  for (const i of p.items) {
    const hit = await p.journal.get(`ack:${i.txid}`)
    if (hit !== undefined && carrierFundingLockHex(hit.beef) !== bboxLock && p.journal.del !== undefined) {
      await p.journal.del(`ack:${i.txid}`)
      fresh.push(i)
    } else if (hit !== undefined) {
      resent++
      resend.set(hit.txid, { txid: hit.txid, beef: hit.beef })
    } else fresh.push(i)
  }
  let acked = 0,
    skipped = 0
  let stockOut: string | undefined
  const receipts: string[] = []
  for (let c = 0; c < fresh.length; c += MaxAcks) {
    const chunk = fresh.slice(c, c + MaxAcks)
    const slot = await p.ledger.take('bbox', bboxLock)
    if (slot === undefined) {
      skipped += chunk.length
      stockOut ??= await fundingSlotReason(p.ledger, 'bbox')
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
  return { receipts, acked, resent, skipped, ...(stockOut !== undefined ? { stockReason: stockOut } : {}) }
}

export async function readBox(p: {
  key: SignerLike
  wallet?: BoxWallet
  hosts: string[]
  office?: string
  tracker: ChainTracker
  limit?: number
  timeoutMs?: number
  /** stop waiting once this many hosts have answered — the interactive read
   *  path cares about the envelopes, not per-host coverage; withheld and
   *  unreachable are then reported only for hosts that did answer */
  quorum?: number
  /** memo of envelopes that already admitted + opened — a hit skips the
   *  decrypt; the expiry gate is re-applied every read */
  cache?: VerifiedCache<BoxItem & { expires: number }>
}): Promise<{ items: BoxItem[]; withheld: Array<{ host: string; txid: string }>; unreachable: string[]; answered: string[] }> {
  const office = p.office ?? DEFAULT_OFFICE
  const me = await signerPubHex(p.key)
  const w = walletOrProto(p.key, p.wallet)
  // interactive read path: a host that cannot answer inside a few seconds is
  // down for deal purposes — reported unreachable, not waited on
  const r = await lookupAll(p.hosts, 'ls_bbox', { office, to: me }, { timeoutMs: p.timeoutMs ?? 5_000, quorum: p.quorum })
  const items = new Map<string, BoxItem>()
  const perHost = new Map<string, string[]>()
  const unreachable: string[] = []
  for (const a of r.answers) {
    if ('error' in a) {
      unreachable.push(a.host)
      continue
    }
    // envelopes open in parallel — each admit may fetch a header; a cache hit
    // skips the admit + decrypt entirely (failed opens are never cached)
    const nowS = Math.floor(Date.now() / 1000)
    const opened = await Promise.all(
      a.outputs.map(async (o) => {
        // a malformed/error output (host-reported, e.g. an unevaluatable
        // record with no beef) must not poison the whole listing — one bad
        // item yields undefined, the rest of the inbox still opens
        if (!Array.isArray(o.beef) || o.beef.length === 0) return undefined
        try {
          const ck = VerifiedCache.key(o.beef)
          const hit = p.cache?.get(ck)
          if (hit !== undefined) {
            // expires is the only time-dependent gate in openEnvelope — the
            // rest of the envelope is fixed bytes already admitted once
            if (hit.expires !== 0 && hit.expires <= nowS) {
              p.cache?.delete(ck)
              return undefined
            }
            return hit
          }
          const it = await openEnvelope(o.beef, w, office, me, p.tracker).catch(() => undefined)
          if (it !== undefined) p.cache?.set(ck, it)
          return it
        } catch {
          return undefined
        }
      })
    )
    const ids: string[] = []
    for (const it of opened) {
      if (it !== undefined) {
        ids.push(it.txid)
        // expires rides the cache entry; the caller-facing BoxItem is unchanged
        items.set(it.txid, {
          txid: it.txid,
          from: it.from,
          body: it.body,
          ...(it.refs !== undefined ? { refs: it.refs } : {}),
          created: it.created,
          commitment: it.commitment
        })
      }
    }
    perHost.set(a.host, ids)
  }
  const withheld: Array<{ host: string; txid: string }> = []
  for (const [host, ids] of perHost) for (const txid of items.keys()) if (!ids.includes(txid)) withheld.push({ host, txid })
  let out = [...items.values()]
  if (p.limit !== undefined) out = out.slice(0, p.limit)
  // perHost's keys are the hosts that ANSWERED this read — a quorum-short
  // read leaves the skipped hosts out entirely, which is what lets a caller
  // distinguish an answered-healthy host from one the read never reached
  return { items: out, withheld, unreachable, answered: [...perHost.keys()] }
}

export async function openEnvelope(
  beef: number[],
  w: BoxWallet,
  office: string,
  me: string,
  tracker: ChainTracker
): Promise<(BoxItem & { expires: number }) | undefined> {
  // spec §8.1 via the real codec verdict, then §4.7 recipient checks in order
  const adm = await admit(new Uint8Array(beef), [], { office, headers: tracker }).catch(() => undefined)
  if (adm === undefined || adm.kind !== 'envelope' || adm.carrier === undefined) return undefined
  const env = decodeEnvelope(new Uint8Array(adm.carrier.record))
  if (env.office !== office) return undefined
  if (Utils.toHex(Array.from(env.to)) !== me) return undefined
  // spec §8.3 recipient-side window — the codec bounds only `expires > created`
  // (and 0 = "never"), so a hostile sender can pin an envelope in our queues by
  // postdating `created` or declaring `expires=0`/year-9999. created beyond the
  // §8.3 answer window is refused; a missing/far expiry is clamped to MAX_HOLD.
  const nowS = Math.floor(Date.now() / 1000)
  if (env.created > nowS + ENVELOPE_CREATED_SKEW_S) return undefined
  const expires = env.expires === 0 || env.expires > nowS + ENVELOPE_MAX_HOLD_S ? nowS + ENVELOPE_MAX_HOLD_S : env.expires
  if (expires <= nowS) return undefined
  const c = checkContent(env)
  verifyContentSignature(c, new Uint8Array(env.from))
  // §4.7 check 1 undecryptable → BRC-78 open (stock codec: the cipher bytes
  // were already validated by checkContent; counterparty is the sender)
  const opened = await openPlaintext(w, c, new Uint8Array(env.from)).catch(() => undefined)
  if (opened === undefined) return undefined
  // §4.7 checks 2,3: plaintext-json / plaintext-shape — the stock parser keeps
  // unknown members in doc, so {nektar: ...} survives alongside body/refs/payment
  let doc: JObject
  let refs: BoxItem['refs']
  try {
    const plain = parsePlaintext(opened)
    doc = plain.doc
    refs = plain.refs?.map((r) => ({
      url: r.url,
      sha256: Utils.toHex(Array.from(r.sha256)),
      length: r.length,
      ...(r.key !== undefined ? { key: Utils.toHex(Array.from(r.key)) } : {})
    }))
  } catch {
    return undefined
  }
  if (!doc.has('nektar')) return undefined
  return {
    txid: Utils.toHex(Array.from(adm.txid)),
    from: Utils.toHex(Array.from(env.from)),
    body: fromJ(doc.get('nektar')!) as object,
    ...(refs !== undefined ? { refs } : {}),
    created: env.created,
    commitment: Utils.toHex(Array.from(adm.carrier.commitment)),
    expires
  }
}
