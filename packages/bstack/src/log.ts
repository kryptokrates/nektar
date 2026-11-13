// blogs deal journals (Lightweb blogs spec v0.6.3): every producer is a
// stream — one signed, numbered batch per append, gaps visible by sequence,
// mined interval anchors for completeness. The record encodings and the
// admission verdict are upstream's (vendored @lightwebinc/blogs-host); the
// carriers, funding trees and unlock scripts are bcommon's, as for market
// artifacts, with the blogs derivations and tags of spec section 6.1.
import { PublicKey, Transaction, type ChainTracker } from '@bsv/sdk'
import { mintCarrier, fundingLock, lockFields, unlocker, type Derivation, type SigningWallet } from '@lightwebinc/bcommon'
import { P2PKH } from '@bsv/sdk'
import { toSigner, signerAddress, type SignerLike } from '@nektar/core'
import {
  EncodingLines,
  ProfileFull,
  TagAnchor,
  TagFunding,
  MaxIntervalBatches,
  checkStream,
  decodeAnchor,
  decodeBatch,
  encodeAnchor,
  encodeBatch,
  admit as logAdmit,
  bytesEq,
  fromHex,
  toHex,
  type Anchor,
  type Coins
} from '@lightwebinc/blogs-host/dist/index.js'
import { intervalRoot } from '@lightwebinc/blogs-host/dist/merkle.js'
import { readPushDrop } from '@lightwebinc/blogs-host/dist/script.js'
import { submitAll, lookupAll, type Quorum } from './hosts.js'
import { ancestorsOf } from './finger.js'
import { carrierFundingLockHex, walletOrProto } from './carrier.js'
import type { TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'
import type { Knex } from 'knex'

export const LOG_KIND = 'log'
export const DEFAULT_STREAM = 'nektar_abcdefghij'
const ORIGINATOR = 'nektar'
const DERIVATION_BATCH: Derivation = { protocol: [1, 'blogs'], keyID: 'batch' }
const DERIVATION_ANCHOR: Derivation = { protocol: [1, 'blogs'], keyID: 'anchor' }
const ZERO32 = '00'.repeat(32)

export const logTopic = (stream: string): string => `tm_log_${stream}`
export const logFundingLock = async (key: SignerLike, wallet?: SigningWallet): Promise<import('@bsv/sdk').LockingScript> =>
  await fundingLock(walletOrProto(key, wallet), ORIGINATOR, DERIVATION_BATCH, TagFunding)

// ── stream state ─────────────────────────────────────────────────────────

/** The producer's durable head per stream; extra carries auxiliary records. */
export interface LogState {
  get(stream: string): Promise<{ seq: number; prev: string; extra?: Record<string, unknown> } | undefined>
  set(stream: string, s: { seq: number; prev: string; extra?: Record<string, unknown> }): Promise<void>
}

export class MemoryLogState implements LogState {
  private m = new Map<string, { seq: number; prev: string; extra?: Record<string, unknown> }>()
  async get(s: string) {
    return this.m.get(s)
  }
  async set(s: string, v: { seq: number; prev: string; extra?: Record<string, unknown> }) {
    this.m.set(s, v)
  }
}

export class KnexLogState implements LogState {
  constructor(private kx: Knex) {}
  private ensured?: Promise<void>
  /** create the table up front (migrations/seed copies need the DDL before
   *  they can insert rows; methods self-ensure lazily otherwise) */
  async init(): Promise<void> {
    await this.ensure()
  }
  private async ensure(): Promise<void> {
    return await (this.ensured ??= this.doEnsure().catch((e) => {
      this.ensured = undefined
      throw e
    }))
  }
  private async doEnsure(): Promise<void> {
    if (!(await this.kx.schema.hasTable('bstack_log_state'))) {
      await this.kx.schema.createTable('bstack_log_state', (t) => {
        t.string('stream').primary()
        t.integer('seq').notNullable()
        t.string('prev').notNullable()
        t.text('extra').nullable()
      })
    }
  }
  async get(s: string) {
    await this.ensure()
    const r = await this.kx('bstack_log_state').where({ stream: s }).first()
    return r === undefined
      ? undefined
      : { seq: Number(r.seq), prev: String(r.prev), ...(r.extra ? { extra: JSON.parse(r.extra) as Record<string, unknown> } : {}) }
  }
  async set(s: string, v: { seq: number; prev: string; extra?: Record<string, unknown> }): Promise<void> {
    await this.ensure()
    await this.kx('bstack_log_state')
      .insert({ stream: s, seq: v.seq, prev: v.prev, extra: v.extra !== undefined ? JSON.stringify(v.extra) : null })
      .onConflict('stream')
      .merge({ seq: v.seq, prev: v.prev, extra: v.extra !== undefined ? JSON.stringify(v.extra) : null })
  }
}

// aux stream keys inside LogState (never cross checkStream)
const anchorKey = (s: string): string => `${s}#anchor` // seq=interval, prev=txid(hash order), extra={first,count}
const coverKey = (s: string): string => `${s}#cover` // seq=last anchored batch seq
const flightKey = (s: string): string => `${s}#flight` // seq=interval, prev=txid display, extra={first,count}

// ── append ────────────────────────────────────────────────────────────────

/** Append one batch of lines to the stream: seq = head + 1, prev = head C.
 *  Journaled under `log:<stream>:<seq>` BEFORE submission — a retry resubmits
 *  the same BEEF and never consumes a second slot or seq. The head advances
 *  only after the journal write AND a quorum of hosts took the carrier. */
export async function appendLog(p: {
  key: SignerLike
  wallet?: SigningWallet
  ledger: TreeLedger
  journal: CarrierJournal
  state: LogState
  hosts: string[]
  stream: string
  lines: string[]
  now?: () => number
  quorum?: Quorum
}): Promise<{ seq: number; txid: string; commitment: string; took: string[]; missed: Array<{ host: string; error: string }> }> {
  checkStream(p.stream)
  if (p.lines.length === 0) throw new Error('appendLog: empty batch')
  const head = (await p.state.get(p.stream)) ?? { seq: 0, prev: ZERO32 }
  const seq = head.seq + 1
  const topics = [logTopic(p.stream)]
  const jkey = `log:${p.stream}:${seq}`
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  const logLockHex = (await logFundingLock(s, w)).toHex()
  let hit = await p.journal.get(jkey)
  if (hit !== undefined && carrierFundingLockHex(hit.beef) !== logLockHex && p.journal.del !== undefined) {
    await p.journal.del(jkey)
    hit = undefined
  }
  let beef: number[], txid: string, commitment: string
  if (hit !== undefined) {
    beef = hit.beef
    txid = hit.txid
    commitment = toHex(new Uint8Array(Transaction.fromAtomicBEEF(beef).hash() as number[]))
  } else {
    const content = new TextEncoder().encode(p.lines.map((l) => (l.endsWith('\n') ? l : `${l}\n`)).join(''))
    const record = encodeBatch({
      producer: new Uint8Array(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[]),
      stream: p.stream,
      seq,
      prev: fromHex(head.prev),
      timestamp: (p.now ?? Date.now)(),
      count: p.lines.length,
      encoding: EncodingLines,
      profile: ProfileFull,
      content,
      size: content.length,
      extra: []
    })
    const slot = await p.ledger.take(LOG_KIND, logLockHex)
    if (slot === undefined) throw new Error('no funding slot')
    // boundary: the wallet only signs the record lock — the carrier itself is
    // unmineable and must never be routed through a wallet action
    const carrier = await mintCarrier(w, ORIGINATOR, DERIVATION_BATCH, record, slot.tree, slot.vout)
    beef = carrier.toAtomicBEEF()
    txid = carrier.id('hex')
    commitment = toHex(new Uint8Array(carrier.hash() as number[]))
    await p.journal.put(jkey, { txid, beef, topics })
  }
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum, retryWaits: [] })
  if (!r.ok) throw new Error(`log submit failed: ${JSON.stringify(r.missed).slice(0, 200)}`)
  await p.state.set(p.stream, { seq, prev: commitment })
  return { seq, txid, commitment, took: r.took, missed: r.missed }
}

// ── read ──────────────────────────────────────────────────────────────────

const producerBytes = (hex: string): Uint8Array => fromHex(hex.toLowerCase())

interface HeldBatch {
  seq: number
  commitment: string
  timestamp: number
  lines: string[]
  prev: string
}

/** Verify one answered carrier BEEF with the upstream admission verdict plus
 *  the funding parent's SPV, then keep it only when it is `producer`'s batch
 *  on `stream` at `seq`. */
async function admitBatch(beef: number[], o: { stream: string; producer: Uint8Array; seq: number; tracker: ChainTracker }): Promise<HeldBatch | undefined> {
  try {
    const tx = Transaction.fromAtomicBEEF(beef)
    const a = await logAdmit(tx, new Map() as Coins, { stream: o.stream, policy: { open: true }, headers: o.tracker })
    if (a.kind !== 'batch' || a.carrier === undefined) return undefined
    const b = a.carrier.batch
    if (b.stream !== o.stream || !b.producer.every((x, i) => x === o.producer[i]) || b.seq !== o.seq) return undefined
    const parent = tx.inputs[0].sourceTransaction
    if (parent?.merklePath === undefined || !(await parent.merklePath.verify(parent.id('hex'), o.tracker))) return undefined
    const lines =
      b.profile === ProfileFull && b.encoding === EncodingLines && b.content !== undefined
        ? new TextDecoder()
            .decode(b.content)
            .split('\n')
            .filter((l) => l.length > 0)
        : []
    return { seq: b.seq, commitment: toHex(a.carrier.commitment), timestamp: b.timestamp, lines, prev: toHex(b.prev) }
  } catch {
    return undefined
  }
}

/** Ask every host the three free ls_log question classes and union the
 *  admitted answers (sibling repair): batch seqs walk up from `fromSeq`
 *  until no host answers `scanAhead` consecutive positions; intervals walk
 *  from 1 and stop at the first position no host answers. */
export async function readLog(p: { hosts: string[]; stream: string; producer: string; tracker: ChainTracker; fromSeq?: number; scanAhead?: number }): Promise<{
  batches: Array<{ seq: number; commitment: string; timestamp: number; lines: string[] }>
  gaps: Array<{ from: number; to: number }>
  head: number
  anchoredThrough: number
  withheld: Array<{ host: string; seq: number }>
  unreachable: string[]
}> {
  checkStream(p.stream)
  const producer = producerBytes(p.producer)
  const unreachable = new Set<string>()
  const ahead = p.scanAhead ?? 8

  // per-seq union of verified batches
  const bySeq = new Map<number, Map<string, HeldBatch>>() // seq -> commitment -> batch
  const served = new Map<number, Set<string>>() // seq -> hosts that served a valid batch
  let seq = p.fromSeq ?? 1
  let emptyRun = 0
  let head = 0
  while (emptyRun < ahead) {
    const { answers } = await lookupAll(p.hosts, 'ls_log', { stream: p.stream, producer: p.producer.toLowerCase(), seq })
    let any = false
    for (const a of answers) {
      if ('error' in a || !('outputs' in a)) {
        unreachable.add(a.host)
        continue
      }
      for (const out of a.outputs) {
        const h = await admitBatch(out.beef, { stream: p.stream, producer, seq, tracker: p.tracker })
        if (h === undefined) continue
        any = true
        if (!bySeq.has(seq)) bySeq.set(seq, new Map())
        bySeq.get(seq)!.set(h.commitment, h)
        if (!served.has(seq)) served.set(seq, new Set())
        served.get(seq)!.add(a.host)
      }
    }
    if (any) {
      emptyRun = 0
      head = seq
    } else emptyRun++
    seq++
  }
  const first = bySeq.size === 0 ? 0 : Math.min(...bySeq.keys())

  // canonical chain: prefer the candidate whose prev names the previous pick;
  // a seq with no candidates extends a missing run, and a seq whose only
  // candidates break the prev link is a broken position reported as {s,s}.
  const gaps: Array<{ from: number; to: number }> = []
  const ordered: HeldBatch[] = []
  const withheld: Array<{ host: string; seq: number }> = []
  const commitmentOf = new Map<number, string>() // seq -> canonical commitment
  if (head > 0) {
    let expectedPrev: string | undefined
    let gapStart: number | undefined
    const closeGap = (to: number) => {
      if (gapStart !== undefined) {
        gaps.push({ from: gapStart, to })
        gapStart = undefined
      }
    }
    for (let s = first; s <= head; s++) {
      const cands = bySeq.get(s)
      if (cands === undefined || cands.size === 0) {
        if (gapStart === undefined) gapStart = s
        expectedPrev = undefined
        continue
      }
      const list = [...cands.values()].sort((a, b) => a.commitment.localeCompare(b.commitment))
      // the first seen seq (or one straight after a gap) cannot link
      // backwards — any candidate starts a segment. A real break is a
      // mismatch against a picked predecessor.
      const matching = expectedPrev === undefined ? list : list.filter((c) => c.prev === expectedPrev)
      const pick = matching.length === 0 ? list[0] : matching[0]
      if (matching.length === 0 && expectedPrev !== undefined) gaps.push({ from: s, to: s })
      closeGap(s - 1)
      expectedPrev = pick.commitment
      commitmentOf.set(s, pick.commitment)
      ordered.push(pick)
      for (const host of p.hosts) if (!unreachable.has(host) && !(served.get(s)?.has(host) ?? false)) withheld.push({ host, seq: s })
    }
    closeGap(head)
    gaps.sort((a, b) => a.from - b.from)
  }

  // interval anchors: tokens must verify mined against the tracker, decode,
  // chain (first/prev), and recompute the root over the covered commitments
  let anchoredThrough = 0
  const commitments = new Map<number, string>(commitmentOf)
  for (const b of ordered) if (b.seq > 1 && !commitments.has(b.seq - 1)) commitments.set(b.seq - 1, b.prev)
  let interval = 1
  let prevTokenTxid = ZERO32
  let prevFirst = 1,
    prevCount = 0
  for (;;) {
    const { answers } = await lookupAll(p.hosts, 'ls_log', { stream: p.stream, producer: p.producer.toLowerCase(), interval })
    const tokens: Transaction[] = []
    for (const a of answers) {
      if ('error' in a || !('outputs' in a)) {
        unreachable.add(a.host)
        continue
      }
      for (const out of a.outputs) {
        try {
          tokens.push(Transaction.fromBEEF(out.beef))
        } catch {
          try {
            tokens.push(Transaction.fromAtomicBEEF(out.beef))
          } catch {
            /* */
          }
        }
      }
    }
    if (tokens.length === 0) break
    let okToken: { tx: Transaction; anchor: Anchor } | undefined
    for (const tx of tokens) {
      try {
        if (tx.merklePath === undefined || !(await tx.merklePath.verify(tx.id('hex'), p.tracker))) continue
        const pd = tx.outputs
          .map((o) => readPushDrop(new Uint8Array(o.lockingScript.toBinary())))
          .find((r) => r !== undefined && r.fields.length === 3 && bytesEq(r.fields[0], TagAnchor))
        if (pd === undefined) continue
        const fields = pd.fields
        const a = decodeAnchor(fields[1])
        if (a.stream !== p.stream || toHex(a.producer) !== p.producer.toLowerCase()) continue
        if (a.interval !== interval || a.first !== prevFirst + prevCount || toHex(a.prev) !== prevTokenTxid) continue
        const covered: Uint8Array[] = []
        let missing = false
        for (let s = a.first; s < a.first + a.count; s++) {
          const c = commitments.get(s)
          if (c === undefined) {
            missing = true
            break
          }
          covered.push(fromHex(c))
        }
        if (missing || toHex(intervalRoot(covered)) !== toHex(a.root)) continue
        okToken = { tx, anchor: a }
        break
      } catch {
        /* not the token */
      }
    }
    if (okToken === undefined) break
    anchoredThrough = okToken.anchor.first + okToken.anchor.count - 1
    prevTokenTxid = toHex(new Uint8Array(okToken.tx.hash() as number[]))
    prevFirst = okToken.anchor.first
    prevCount = okToken.anchor.count
    interval++
  }

  return {
    batches: ordered.map((b) => ({ seq: b.seq, commitment: b.commitment, timestamp: b.timestamp, lines: b.lines })),
    gaps,
    head,
    anchoredThrough,
    withheld,
    unreachable: [...unreachable]
  }
}

// ── mined-object publication (funding trees, anchor tokens) ──────────────

/** Publish one mined transaction's BEEF (with proof) to the log topic —
 *  exactly once per (stream, txid), journaled under `logpub:`. */
export async function publishMinedLog(p: { journal: CarrierJournal; hosts: string[]; stream: string; tx: Transaction; quorum?: Quorum }): Promise<boolean> {
  const txid = p.tx.id('hex')
  const jkey = `logpub:${p.stream}:${txid}`
  const topics = [logTopic(p.stream)]
  let beef: number[]
  const hit = await p.journal.get(jkey)
  if (hit !== undefined) {
    beef = hit.beef
  } else {
    // atomic BEEF — the canonical single-subject form an engine hands a
    // whole-tx lookup service; non-atomic BEEF parses as garbage there
    beef = p.tx.toAtomicBEEF()
    await p.journal.put(jkey, { txid, beef, topics })
  }
  const r = await submitAll(p.hosts, beef, topics, { quorum: p.quorum ?? 'one', retryWaits: [] })
  return r.ok
}

// ── anchor ────────────────────────────────────────────────────────────────

/** Build and broadcast the anchor token for the next unanchored interval —
 *  or finish an in-flight one (mined → publish BEEF to the topic → advance
 *  the anchor head). Interval 1 spends a blogs funding output; later
 *  intervals spend the previous token plus funding outputs for the fee.
 *  Returns undefined when there is nothing new to anchor: fewer than
 *  `minBatches` unanchored batches AND the oldest is fresher than
 *  `maxAgeMs` (engine clock). */
export async function anchorLog(p: {
  key: SignerLike
  wallet?: SigningWallet
  ledger: TreeLedger
  journal: CarrierJournal
  state: LogState
  hosts: string[]
  stream: string
  broadcast: (tx: Transaction) => Promise<{ status: string }>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  now?: () => number
  minBatches?: number
  maxAgeMs?: number
  quorum?: Quorum
  log?: (s: string) => void
}): Promise<{ interval: number; first: number; count: number; root: string; txid: string } | undefined> {
  checkStream(p.stream)
  const now = p.now ?? Date.now
  const head = await p.state.get(p.stream)
  if (head === undefined || head.seq === 0) return undefined
  const log = p.log ?? (() => {})

  // an anchor in flight: mined? then publish + advance the head and stop
  const flight = await p.state.get(flightKey(p.stream))
  if (flight !== undefined) {
    const tx = await p.fetchTx(flight.prev)
    if (tx?.merklePath === undefined) return undefined
    await publishMinedLog({ journal: p.journal, hosts: p.hosts, stream: p.stream, tx, quorum: p.quorum })
    const ex = flight.extra as { first: number; count: number }
    const a = await p.state.get(anchorKey(p.stream))
    await p.state.set(anchorKey(p.stream), { seq: flight.seq, prev: toHex(new Uint8Array(tx.hash() as number[])), extra: { first: ex.first, count: ex.count } })
    await p.state.set(coverKey(p.stream), { seq: ex.first + ex.count - 1, prev: '' })
    await p.state.set(flightKey(p.stream), { seq: 0, prev: '' })
    void a
    return { interval: flight.seq, first: ex.first, count: ex.count, root: '', txid: flight.prev }
  }

  const cover = (await p.state.get(coverKey(p.stream)))?.seq ?? 0
  const first = cover + 1
  const count = head.seq - cover
  if (count === 0) return undefined
  if (count < (p.minBatches ?? 32)) {
    const hit = await p.journal.get(`log:${p.stream}:${first}`)
    if (hit === undefined) {
      log(`log anchor: no journaled carrier for seq ${first}`)
      return undefined
    }
    const ts = decodeBatch(recordOf(Transaction.fromAtomicBEEF(hit.beef))).timestamp
    if (now() - ts < (p.maxAgeMs ?? 3_600_000)) return undefined
  }
  if (count > MaxIntervalBatches) return undefined

  // commitments of every covered batch, from the journaled carriers
  const covered: Uint8Array[] = []
  for (let s = first; s <= head.seq; s++) {
    const hit = await p.journal.get(`log:${p.stream}:${s}`)
    if (hit === undefined) {
      log(`log anchor: journaled carrier missing for seq ${s}`)
      return undefined
    }
    covered.push(new Uint8Array(Transaction.fromAtomicBEEF(hit.beef).hash() as number[]))
  }
  const root = intervalRoot(covered)

  const ah = await p.state.get(anchorKey(p.stream))
  const interval = (ah?.seq ?? 0) + 1
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  const record = encodeAnchor({
    producer: new Uint8Array(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[]),
    stream: p.stream,
    interval,
    prev: ah === undefined ? new Uint8Array(32) : fromHex(ah.prev),
    first,
    count,
    root,
    timestamp: now(),
    extra: []
  })

  const tx = new Transaction()
  // inputs: interval>1 spends the previous token; fee paid by log funding slots
  if (interval > 1) {
    const prevHit = await p.journal.get(`log-anchor:${p.stream}:${interval - 1}`)
    if (prevHit === undefined) {
      log(`log anchor: previous token tx not journaled`)
      return undefined
    }
    const prevTx = Transaction.fromBEEF(prevHit.beef)
    tx.addInput({ sourceTransaction: prevTx, sourceOutputIndex: 0, unlockingScriptTemplate: unlocker(w, ORIGINATOR, DERIVATION_ANCHOR) })
  }
  const token = await lockFields(w, ORIGINATOR, DERIVATION_ANCHOR, [TagAnchor, record], true)
  tx.addOutput({ satoshis: 1, lockingScript: token })
  tx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(s)), change: true })
  // funding inputs until the fee is covered
  const logLock = (await logFundingLock(s, w)).toHex()
  for (let i = 0; i < 8; i++) {
    const slot = await p.ledger.take(LOG_KIND, logLock)
    if (slot === undefined) {
      log('log anchor: no funding slot for the fee')
      return undefined
    }
    tx.addInput({ sourceTransaction: slot.tree, sourceOutputIndex: slot.vout, unlockingScriptTemplate: unlocker(w, ORIGINATOR, DERIVATION_BATCH) })
    try {
      await tx.fee()
      await tx.sign()
      const txid = tx.id('hex')
      let br = await p.broadcast(tx)
      if (br.status !== 'success' && /missing input/i.test(JSON.stringify(br))) {
        // the anchor chain outran the network — a parent (previous anchor or
        // a funding slot tree) is absent from mempool. Re-broadcast the
        // tx's ancestors oldest-first and retry once before giving up; the
        // journaled prev tokens carry their ancestry in their BEEF.
        try {
          for (const anc of ancestorsOf(tx)) {
            if (anc.merklePath === undefined) await p.broadcast(anc).catch(() => undefined)
          }
        } catch {
          /* heal is best-effort — the retried broadcast decides */
        }
        br = await p.broadcast(tx)
      }
      if (br.status !== 'success') {
        log(`log anchor: broadcast refused ${JSON.stringify(br).slice(0, 120)}`)
        return undefined
      }
      const aHit = `log-anchor:${p.stream}:${interval}`
      await p.journal.put(aHit, { txid, beef: tx.toBEEF(), topics: [logTopic(p.stream)] })
      await p.state.set(flightKey(p.stream), { seq: interval, prev: txid, extra: { first, count } })
      return { interval, first, count, root: toHex(root), txid }
    } catch (e) {
      tx.inputs.pop()
      if (i === 7) {
        log(`log anchor: cannot cover fee: ${(e as Error).message.slice(0, 120)}`)
        return undefined
      }
    }
  }
  return undefined
}

/** The record inside a carrier's single PushDrop field. */
function recordOf(carrier: Transaction): Uint8Array {
  const r = readPushDrop(new Uint8Array(carrier.outputs[0].lockingScript.toBinary()))
  if (r === undefined || r.fields.length < 1) throw new Error('carrier has no PushDrop fields')
  return r.fields[0]
}
