// bfinger identity records (bfinger v0.8.4, committed-record spec §§2-4,7,10):
// a signed record names the hosts carrying an identity's artifacts, its bbox
// office and its blogs stream, and is anchored by a mined state token that
// commits to the carrier's txid. The record encodings, tags, derivations and
// admission verdicts are upstream's (vendored @lightwebinc/bfinger-host);
// carriers, funding trees and unlock scripts are bcommon's, as for artifacts
// and log batches.
import { PublicKey, Transaction, P2PKH, Hash, Utils, SatoshisPerKilobyte, type ChainTracker, type LockingScript } from '@bsv/sdk'
import { toSigner, unlockP2PKH, signerAddress, egressAllowed, dbg, sparse, isEfStub, rebroadcastAncestors, type SignerLike } from '@nektar/core'
import { isHeaderUnavailable } from '@nektar/chain'
import {
  mintCarrier,
  fundingLock,
  lockFields,
  unlocker,
  commitment,
  CborMap,
  encode as cborEncode,
  encodeRefs,
  bytesEqual,
  readWire,
  subjectTx,
  type Derivation,
  type Ref,
  type SigningWallet
} from '@lightwebinc/bcommon'
import { broadcastProvablyDead, type FundingOut } from '@nektar/chain'
import {
  encodeRecord,
  validateRecord,
  KindCreate,
  KindUpdate,
  KindSub,
  KindManifest,
  MagicV1,
  type CommittedRecord
} from '@lightwebinc/bfinger-host/dist/record.js'
import { Protocol, KeyIDProfile, KeyIDRecord, profileLockingKey, recordLockingKey } from '@lightwebinc/bfinger-host/dist/keys.js'
import { Tag, inspectToken } from '@lightwebinc/bfinger-host/dist/token.js'
import { decodeCarrier } from '@lightwebinc/bfinger-host/dist/carrier.js'
import { FundingTag, decodeFunding } from '@lightwebinc/bfinger-host/dist/funding.js'
import { checkOffice } from '@lightwebinc/bbox-host/codec/boxrec.js'
import { checkStream } from '@lightwebinc/blogs-host/dist/index.js'
import { submitAll, lookupAll, type SubmitAllResult } from './hosts.js'
import { carrierFundingLockHex, walletOrProto } from './carrier.js'
import { fundingSlotReason } from './tree.js'
import type { TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'
import type { Knex } from 'knex'

export const FINGER_KIND = 'finger'
export const FINGER_TOPIC = 'tm_finger'
const ORIGINATOR = 'nektar'
const DERIVATION_PROFILE: Derivation = { protocol: Protocol, keyID: KeyIDProfile }
const DERIVATION_RECORD: Derivation = { protocol: Protocol, keyID: KeyIDRecord }
const ZERO32 = new Uint8Array(32)

export const fingerFundingLock = async (key: SignerLike, wallet?: SigningWallet): Promise<LockingScript> =>
  await fundingLock(walletOrProto(key, wallet), ORIGINATOR, DERIVATION_RECORD, new Uint8Array(FundingTag))

// ── the record body ────────────────────────────────────────────────────────

export interface FingerBody {
  v: 1
  hosts: string[] /* overlay hosts carrying this identity's artifacts, 1..8 */
  office: string /* bbox office */
  stream: string /* blogs journal stream */
  nets: string[] /* network labels this record is authoritative for */
  /** optional watch-shim base URL — counterparties POST {service,query} to
   *  `${watch}/notify` to wake this identity's long-polls early (hints only:
   *  the shim answers changed/not-changed from real backend state, so a
   *  stale or forged notify just causes a re-check, never a false positive) */
  watch?: string
  /** optional direct-push endpoint on this identity's own agent — senders
   *  POST a signed carrier BEEF to it and the recipient ingests it
   *  immediately (same verify/decrypt as a host read). Store-and-forward
   *  boxes/hosts stay the fallback when absent or unreachable. */
  push?: string
}

const NET_LABEL = /^[a-z][a-z0-9-]{0,15}$/

/** Shape-check a body. `allowHttp` is for regtest/local-rig hosts: records on a
 *  real network name https hosts only. Throws on any violation. */
export function checkFingerBody(body: FingerBody | null, opts?: { allowHttp?: boolean }): void {
  if (body === null || typeof body !== 'object' || Number(body.v) !== 1) throw new Error('finger body: v must be 1')
  if (!Array.isArray(body.hosts) || body.hosts.length < 1 || body.hosts.length > 8) throw new Error('finger body: 1..8 hosts')
  for (const h of body.hosts) {
    let u: URL
    try {
      u = new URL(h)
    } catch {
      throw new Error(`finger body: bad host URL ${JSON.stringify(h).slice(0, 60)}`)
    }
    if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) {
      throw new Error(`finger body: host must be https ${u.protocol}//`)
    }
  }
  try {
    checkOffice(body.office)
  } catch {
    throw new Error('finger body: bad office')
  }
  try {
    checkStream(body.stream)
  } catch {
    throw new Error('finger body: bad stream')
  }
  if (!Array.isArray(body.nets) || body.nets.length < 1 || body.nets.length > 8 || body.nets.some((n) => typeof n !== 'string' || !NET_LABEL.test(n)))
    throw new Error('finger body: bad nets')
  if (body.watch !== undefined) {
    let u: URL
    try {
      u = new URL(body.watch)
    } catch {
      throw new Error('finger body: bad watch URL')
    }
    if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) {
      throw new Error(`finger body: watch must be https ${u.protocol}//`)
    }
  }
  if (body.push !== undefined) {
    let u: URL
    try {
      u = new URL(body.push)
    } catch {
      throw new Error('finger body: bad push URL')
    }
    if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) {
      throw new Error(`finger body: push must be https ${u.protocol}//`)
    }
  }
}

function bodyToMap(body: FingerBody): CborMap {
  const pairs = [
    { key: 'v', val: 1n },
    { key: 'hosts', val: body.hosts },
    { key: 'office', val: body.office },
    { key: 'stream', val: body.stream },
    { key: 'nets', val: body.nets }
  ]
  if (body.watch !== undefined) pairs.push({ key: 'watch', val: body.watch })
  if (body.push !== undefined) pairs.push({ key: 'push', val: body.push })
  return new CborMap(pairs)
}

function mapToBody(m: CborMap): FingerBody {
  const v = m.get('v')
  const w = m.get('watch')
  const p = m.get('push')
  const body = {
    v: typeof v === 'bigint' ? Number(v) : v,
    hosts: m.get('hosts'),
    office: m.get('office'),
    stream: m.get('stream'),
    nets: m.get('nets'),
    ...(typeof w === 'string' ? { watch: w } : {}),
    ...(typeof p === 'string' ? { push: p } : {})
  } as FingerBody
  return body
}

const bodyHash = (m: CborMap): string => Utils.toHex(Hash.sha256(Array.from(cborEncode(m))))

// ── durable publisher state ────────────────────────────────────────────────

export interface FingerStateRow {
  seq: number
  tokenTxid: string
  tokenBeef: number[]
  bodyHash: string
  /** hash of the last committed refs — a store root moving republishes the
   *  record even when the body is unchanged */
  refsHash?: string
  witness?: string
  /** last full chain re-serve — the O(seq) pass is for hosts missing chain
   *  members, so it runs per host-set at most once per CHAIN_SERVE_MS; a
   *  changed host list resets the mark because a new host may hold nothing.
   *  `through` marks partial progress: the serve amortizes CHAIN_SERVE_BATCH
   *  seqs per pass so a deep history can't park a whole upkeep. */
  served?: { at: number; hosts: string; through?: number }
}

export interface FingerState {
  get(): Promise<FingerStateRow | undefined>
  set(s: FingerStateRow): Promise<void>
}

export class MemoryFingerState implements FingerState {
  private row: FingerStateRow | undefined
  get() {
    return Promise.resolve(this.row)
  }
  set(s: FingerStateRow) {
    this.row = s
    return Promise.resolve()
  }
}

export class KnexFingerState implements FingerState {
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
    if (!(await this.kx.schema.hasTable('bstack_finger_state'))) {
      await this.kx.schema.createTable('bstack_finger_state', (t) => {
        t.increments('id').primary()
        t.integer('seq').notNullable()
        t.string('tokenTxid').notNullable()
        t.text('tokenBeef').notNullable()
        t.string('bodyHash').notNullable()
        t.string('refsHash').nullable()
        t.string('witness').nullable()
        t.text('served').nullable()
      })
    } else {
      if (!(await this.kx.schema.hasColumn('bstack_finger_state', 'refsHash'))) {
        await this.kx.schema.alterTable('bstack_finger_state', (t) => {
          t.string('refsHash').nullable()
        })
      }
      if (!(await this.kx.schema.hasColumn('bstack_finger_state', 'served'))) {
        await this.kx.schema.alterTable('bstack_finger_state', (t) => {
          t.text('served').nullable()
        })
      }
    }
  }
  async get() {
    await this.ensure()
    const r = await this.kx('bstack_finger_state').orderBy('id').first()
    return r === undefined
      ? undefined
      : {
          seq: Number(r.seq),
          tokenTxid: String(r.tokenTxid),
          tokenBeef: JSON.parse(String(r.tokenBeef)) as number[],
          bodyHash: String(r.bodyHash),
          ...(r.refsHash !== null && r.refsHash !== undefined ? { refsHash: String(r.refsHash) } : {}),
          ...(r.witness !== null && r.witness !== undefined ? { witness: String(r.witness) } : {}),
          ...(r.served !== null && r.served !== undefined ? { served: JSON.parse(String(r.served)) as { at: number; hosts: string } } : {})
        }
  }
  async set(s: FingerStateRow): Promise<void> {
    await this.ensure()
    const row = {
      seq: s.seq,
      tokenTxid: s.tokenTxid,
      tokenBeef: JSON.stringify(s.tokenBeef),
      bodyHash: s.bodyHash,
      refsHash: s.refsHash ?? null,
      witness: s.witness ?? null,
      served: s.served === undefined ? null : JSON.stringify(s.served)
    }
    if ((await this.kx('bstack_finger_state').count('id as n').first())?.n === 0) {
      await this.kx('bstack_finger_state').insert(row)
    } else {
      await this.kx('bstack_finger_state').update(row)
    }
  }
}

// ── publish ────────────────────────────────────────────────────────────────

/** a full chain re-serve is O(seq) submits — worth it only when a host may
 *  actually lack members, so it runs at most once per window per host set;
 *  a changed host list always earns an immediate serve (it may hold nothing) */
const CHAIN_SERVE_MS = 15 * 60_000
// a pass serves at most this many seqs (carriers AND tokens each) — a deep
// history converges over a few upkeeps instead of parking one for minutes
const CHAIN_SERVE_BATCH = 24
const chainServeDue = (prev: FingerStateRow | undefined, hostSig: string, now: number): boolean => {
  const s = prev?.served
  if (s === undefined || s.hosts !== hostSig) return true
  if ((s.through ?? prev?.seq ?? 0) < (prev?.seq ?? 0)) return true // partial serve resumes
  return now - s.at >= CHAIN_SERVE_MS
}

/** Publish (or re-publish) the identity's finger record. Journaled under
 *  `finger:<seq>` BEFORE submission so a retry reuses the same carrier and
 *  token. `changed:false` when the stored body hash matches — the journaled
 *  objects are still re-submitted to the CURRENT host list (late-joining or
 *  newly resolved hosts learn the record that way).
 *
 *  The token is broadcast and the call returns without waiting for mining.
 *  On later calls, when fetchTx shows the token mined, the proven BEEF is
 *  re-journaled and re-submitted — that is the "once admission prerequisites
 *  are met" resubmission: tm_finger admits an unmined token on its own
 *  validity, and a reader resolves it either once the served token BEEF
 *  carries a proof or while its ancestry is proven (zero-conf). */
// rebroadcastAncestors lives in @nektar/core — re-exported for existing
// importers
export { rebroadcastAncestors }

export async function publishFinger(p: {
  key: SignerLike
  wallet?: SigningWallet
  state: FingerState
  ledger: TreeLedger /* kind 'finger' */
  journal: CarrierJournal
  hosts: string[]
  body: FingerBody
  /** store refs committed alongside the body — e.g. the packs store. Old
   *  readers ignore them; new readers resolve stores through them. */
  refs?: Ref[]
  takeFunding: (sats: number) => Promise<FundingOut>
  broadcast: (tx: Transaction) => Promise<{ status: string; description?: string }>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  allowHttp?: boolean
  onSpent?: (tx: Transaction) => void | Promise<void>
  /** broadcast refused after the ancestor-heal retry — the token is dead:
   *  its funding input unreserves, its own outputs tombstone. Without this
   *  the take's reservation leaks for the process lifetime and the next
   *  cycle finds 'no spendable output' with the file showing free sats. */
  onDied?: (tx: Transaction) => void | Promise<void>
  now?: () => number
  /** caller-resolved sat/kB — funded spends should price off the port's
   *  rate, not the SDK LivePolicy's single-endpoint quote */
  feeRateSatPerKb?: number
}): Promise<{ changed: boolean; seq: number; carrierTxid?: string; tokenTxid?: string }> {
  checkFingerBody(p.body, { allowHttp: p.allowHttp })
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  let map = bodyToMap(p.body)
  let hash = bodyHash(map)
  let refsHash = Utils.toHex(Hash.sha256(Array.from(cborEncode(encodeRefs(p.refs ?? [])))))
  const topics = [FINGER_TOPIC]
  const prev = await p.state.get()

  // a bundle hosts refuse structurally can never be admitted — serving it
  // is pure retry waste. Journaled beefs freeze at mint time, so serve
  // paths heal first (an ancestor that mined since gets its path through
  // fetchTx), then skip what still cannot close
  const servable = async (beef: number[]): Promise<number[] | undefined> => {
    let tx: Transaction
    try {
      tx = Transaction.fromBEEF(beef)
    } catch {
      return undefined
    }
    if (!beefGaps(tx)) return beef
    if (!(await healAncestry(tx, p.fetchTx)) || beefGaps(tx)) return undefined
    return tx.toBEEF()
  }

  // proof propagation for an in-flight token: attach the mined proof to the
  // stored tx (which still carries its full ancestry) and re-submit. The
  // proven copy lives in STATE, not the journal — the journal is insert-only
  // (the journaled first attempt is the idempotent object), while the BEEF a
  // host needs is the latest one carrying the proof. While the token itself
  // is still unmined, healAncestry patches stale ancestry instead — an
  // ancestor that has since been mined gets its proof, and source gaps are
  // filled from fetchTx — so the BEEF can pass SPV through ancestry.
  let latestBeef: number[] | undefined
  if (prev !== undefined) {
    try {
      const stx = Transaction.fromBEEF(prev.tokenBeef)
      if (stx.merklePath === undefined) {
        const mined = await p.fetchTx(prev.tokenTxid).catch(() => undefined)
        if (mined?.merklePath !== undefined) {
          stx.merklePath = mined.merklePath
          latestBeef = stx.toBEEF()
          await p.state.set({ ...prev, tokenBeef: latestBeef })
        } else if (await healAncestry(stx, p.fetchTx)) {
          latestBeef = stx.toBEEF()
          await p.state.set({ ...prev, tokenBeef: latestBeef })
        }
      }
    } catch {
      /* resubmission below uses whatever state holds */
    }
  }

  if (prev !== undefined && prev.bodyHash === hash && prev.refsHash === refsHash) {
    const tokenBeef = latestBeef ?? sparse(prev.tokenBeef) ?? (await p.journal.get(`finger:token:${prev.seq}`))?.beef
    const hostSig = p.hosts.join(',')
    if (chainServeDue(prev, hostSig, Date.now())) {
      // re-serve the WHOLE chain, not just the head: a host that never joined
      // (or lost) the create refuses a head-only resubmit 'no-current' and the
      // record stays unresolvable there forever. Journal rows are the idempotent
      // objects — hosts dedupe what they already hold. Carriers land before
      // tokens so parents precede the members that reference them; inside each
      // phase every beef carries its own ancestry, so a bounded fan-out is
      // safe — and the window is amortized: CHAIN_SERVE_BATCH seqs per pass,
      // resuming at `through`, so a deep history can't park one upkeep for
      // minutes (observed live: ~90 records held an upkeep >300s and the leg
      // was abandoned). `through` only counts partial progress — a completed
      // serve that's due again restarts from 1.
      const done = prev.served?.hosts === hostSig && (prev.served.through ?? 0) < prev.seq ? (prev.served.through ?? 0) : 0
      const upto = Math.min(done + CHAIN_SERVE_BATCH, prev.seq)
      const seqs = Array.from({ length: upto - done }, (_, i) => done + i + 1)
      const serve = async <T>(items: T[], fn: (x: T) => Promise<unknown>, width = 8): Promise<void> => {
        for (let i = 0; i < items.length; i += width) await Promise.all(items.slice(i, i + width).map(fn))
      }
      await serve(seqs, async (n) => {
        const c = await p.journal.get(`finger:carrier:${n}`)
        if (c !== undefined) await submitAll(p.hosts, c.beef, c.topics)
      })
      await serve(seqs, async (n) => {
        const t = n === prev.seq && tokenBeef !== undefined ? { beef: tokenBeef } : sparse(await p.journal.get(`finger:token:${n}`))
        const sv = t === undefined ? undefined : sparse(await servable(t.beef))
        if (sv !== undefined) await submitAll(p.hosts, sv, topics)
      })
      // `at` moves only on completion — a partial pass leaves the original
      // timestamp so chainServeDue keeps resuming until `through` reaches seq
      const complete = upto >= prev.seq
      await p.state.set({ ...prev, served: { at: complete ? Date.now() : (prev.served?.at ?? Date.now()), hosts: hostSig, through: upto } })
    } else if (tokenBeef !== undefined) {
      // inside the window the head alone re-touches every host — O(1), and a
      // host that somehow lost just the tip still heals between serves
      const c = await p.journal.get(`finger:carrier:${prev.seq}`)
      if (c !== undefined) await submitAll(p.hosts, c.beef, c.topics)
      const sv = sparse(await servable(tokenBeef))
      if (sv !== undefined) await submitAll(p.hosts, sv, topics)
    }
    return { changed: false, seq: prev.seq, carrierTxid: (await p.journal.get(`finger:carrier:${prev.seq}`))?.txid, tokenTxid: prev.tokenTxid }
  }

  // build the record. Salt and witness derive from (key, seq) so a retried
  // mint reproduces byte-identical record bytes — and therefore the same
  // carrier — even across a crash between the journal write and state.set.
  // The witness is secret until the next record reveals it; deriving it from
  // the private key keeps it unguessable without a random table to keep.
  const seq = (prev?.seq ?? 0) + 1
  const kind = prev === undefined ? KindCreate : KindUpdate
  const pub33 = new Uint8Array(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[])
  const witness = await derived(s, 'witness', seq)
  const prevC = prev === undefined ? ZERO32 : prevCommitment(prev.tokenBeef)
  const prevWitness =
    prev !== undefined ? (prev.witness !== undefined ? new Uint8Array(Utils.toArray(prev.witness, 'hex')) : await derived(s, 'witness', prev.seq)) : undefined
  let record: CommittedRecord = {
    magic: MagicV1,
    identityKey: pub33,
    seq: BigInt(seq),
    kind,
    prev: prevC,
    salt: await derived(s, 'salt', seq),
    wc: new Uint8Array(Hash.sha256(Array.from(witness))),
    ...(prevWitness !== undefined ? { prevWitness } : {}),
    notBefore: 0n,
    notAfter: 0n,
    body: map,
    refs: p.refs ?? [],
    unknown: []
  }

  // the carrier, journaled per seq BEFORE submission. A hit means this seq
  // was already journaled: the journaled record is the committed one, so its
  // body — not the caller's — is what gets published and stored.
  let carrierBeef: number[] | undefined, carrierTxid: string | undefined
  const chit = await p.journal.get(`finger:carrier:${seq}`)
  if (chit !== undefined) {
    try {
      const dec = decodeCarrier(Transaction.fromAtomicBEEF(chit.beef))
      if (typeof dec === 'string') throw new Error(`finger journal: bad stored carrier ${dec}`)
      // a carrier minted under a rotated identity replays forever but can
      // never evaluate — the seq must be re-minted, not re-served
      if (carrierFundingLockHex(chit.beef) !== (await fingerFundingLock(s, w)).toHex()) throw new Error('finger carrier minted under a different identity')
      carrierBeef = chit.beef
      carrierTxid = chit.txid
      record = dec.record
      map = dec.record.body
      hash = bodyHash(map)
      // the journaled carrier is what commits — its refs, not the caller's
      refsHash = Utils.toHex(Hash.sha256(Array.from(cborEncode(encodeRefs(record.refs)))))
    } catch (e) {
      if (e instanceof Error && e.message.startsWith('finger journal:')) throw e
      // truncated bytes (a crash mid-write) name a tx that was never
      // broadcast — the journal is insert-only, so drop the row and re-mint
      // the seq deterministically instead of wedging every future publish
      // on a decode error
      if (p.journal.del === undefined) throw e
      await p.journal.del(`finger:carrier:${seq}`)
    }
  }
  if (carrierBeef === undefined) {
    validateRecord(record)
    const slot = await p.ledger.take(FINGER_KIND, (await fingerFundingLock(s, w)).toHex())
    if (slot === undefined) throw new Error(`publishFinger: ${await fundingSlotReason(p.ledger, FINGER_KIND)}`)
    const carrier = await mintCarrier(w, ORIGINATOR, DERIVATION_RECORD, encodeRecord(record), slot.tree, slot.vout)
    carrierBeef = carrier.toAtomicBEEF()
    carrierTxid = carrier.id('hex')
    await p.journal.put(`finger:carrier:${seq}`, { txid: carrierTxid, beef: carrierBeef, topics, fundingTxid: slot.tree.id('hex') })
  }

  // the state token: spends the previous token (update) plus wallet funding
  // for the fee — wallet funding, not a bf\x02 leaf, because a funding output
  // spent by anything but its own carrier is the kill-switch signal (§7).
  const tkey = `finger:token:${seq}`
  let tokenBeefOut: number[], tokenTxid: string, tokenTx: Transaction
  const thit = await p.journal.get(tkey)
  if (thit !== undefined) {
    tokenBeefOut = thit.beef
    tokenTxid = thit.txid
    tokenTx = Transaction.fromBEEF(thit.beef)
  } else {
    const c = commitment(Transaction.fromAtomicBEEF(carrierBeef))
    tokenTx = new Transaction()
    if (prev !== undefined) {
      const prevToken = Transaction.fromBEEF(prev.tokenBeef)
      tokenTx.addInput({ sourceTransaction: prevToken, sourceOutputIndex: 0, unlockingScriptTemplate: unlocker(w, ORIGINATOR, DERIVATION_PROFILE) })
    }
    tokenTx.addOutput({ satoshis: 1, lockingScript: await lockFields(w, ORIGINATOR, DERIVATION_PROFILE, [new Uint8Array(Tag), new Uint8Array(c)], true) })
    tokenTx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(s)), change: true })
    const fund = await p.takeFunding(5000)
    tokenTx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: unlockP2PKH(s) })
    await tokenTx.fee(p.feeRateSatPerKb !== undefined ? new SatoshisPerKilobyte(Math.max(1, p.feeRateSatPerKb)) : undefined)
    await tokenTx.sign()
    // the fee parent may be a not-yet-proven tx — carry whatever ancestry is
    // obtainable now so the BEEF passes host SPV while the token is unmined
    await healAncestry(tokenTx, p.fetchTx)
    tokenTxid = tokenTx.id('hex')
    tokenBeefOut = tokenTx.toBEEF()
    await p.journal.put(tkey, { txid: tokenTxid, beef: tokenBeefOut, topics, fundingTxid: fund.tx.id('hex') })
  }

  // a host missing earlier chain members refuses the head 'no-current' —
  // re-serve the journaled chain before the head (idempotent on the host).
  // Bounded: once per host-set per window — inside the window the fresh
  // head's own prev carries the chain forward on hosts that hold it.
  const hostSig = p.hosts.join(',')
  const preServe = chainServeDue(prev, hostSig, Date.now())
  if (preServe) {
    for (let n = 1; n < seq; n++) {
      const c = await p.journal.get(`finger:carrier:${n}`)
      if (c !== undefined) await submitAll(p.hosts, c.beef, c.topics)
      const t = await p.journal.get(`finger:token:${n}`)
      const sv = t === undefined ? undefined : sparse(await servable(t.beef))
      if (sv !== undefined) await submitAll(p.hosts, sv, t?.topics ?? topics)
    }
  }
  // independent objects, same hosts — carrier and token beef each carry
  // their own ancestry, so admission needs no ordering between them
  await Promise.all([
    submitAll(p.hosts, carrierBeef, topics),
    servable(tokenBeefOut).then((sv) => (sv === undefined ? undefined : submitAll(p.hosts, sv, topics)))
  ])
  // broadcast on EVERY publish, not just the mint: a journaled token whose
  // first broadcast was refused (or crashed before it) must not commit
  // locally — descendants would chain on a tx the network never saw and fail
  // 'missing inputs' forever, each retry poisoning the next. Idempotent.
  let br = await p.broadcast(tokenTx)
  if (br.status !== 'success' && /missing input/i.test(JSON.stringify(br))) {
    // the tx's own ancestry ran ahead of the network: a parent (previous
    // record token or fee parent) is absent — evicted or never landed.
    // Still refused → throw: stall at this seq rather than commit a
    // phantom (prev stays pinned at the last good one).
    await rebroadcastAncestors(p, tokenTx)
    br = await p.broadcast(tokenTx)
  }
  if (br.status !== 'success') {
    // the mint is dead — but the chain UNDER it may have died first: a
    // persisted head (or an older token) that was evicted and never
    // re-aired turns every later publish into an orphan that spends and
    // re-marks a funding outpoint forever. Walk the journaled token rows
    // back — the newest token that still airs is the true head — delete
    // the forked rows and re-anchor the state there; the next pass mints
    // on live ground instead of re-serving the corpse below.
    // ONLY a provably-dead verdict may enter this: 'error' with no wire
    // verdict (transport failure, unverdicted description) is
    // submission_unknown — the bytes may have aired, so tombstoning then
    // forges a second token for the same seq — a real chain fork — and
    // onDied would free a live input for a double-spend. Keep every row:
    // the mint is deterministic, so the next pass re-broadcasts the same
    // bytes idempotently. Same doctrine as funds.died's broadcastProvablyDead.
    const dead = broadcastProvablyDead(br.description ?? JSON.stringify(br))
    if (dead && prev !== undefined && p.journal.del !== undefined) {
      let head: { seq: number; tokenTxid: string; tokenBeef: number[] } | undefined
      // bounded walk: each probed-dead row is deleted as we go, so a fork
      // deeper than the cap converges across passes instead of one publish
      // paying hundreds of broadcast probes — or none when it dies first
      let probes = 0
      for (let n = prev.seq; n >= 1 && probes < 16; n--) {
        const t = await p.journal.get(`finger:token:${n}`)
        if (t === undefined) continue
        const tx = Transaction.fromBEEF(t.beef)
        const pr =
          tx.merklePath !== undefined
            ? { status: 'success' as const }
            : await p
                .broadcast(tx)
                .catch((e: unknown) => ({ status: 'error' as const, description: `transport: ${e instanceof Error ? e.message : String(e)}` }))
        if (pr.status === 'success') {
          head = { seq: n, tokenTxid: t.txid, tokenBeef: t.beef }
          break
        }
        if (!broadcastProvablyDead(pr.description ?? JSON.stringify(pr))) {
          // not provably dead — submission_unknown: maybe-aired, so keep the
          // row and re-anchor on it. If it truly is dead its own verdict
          // settles DEFINITIVE on a later pass and the walk resumes.
          head = { seq: n, tokenTxid: t.txid, tokenBeef: t.beef }
          break
        }
        probes++
        await p.journal.del(`finger:token:${n}`)
        await p.journal.del(`finger:carrier:${n}`)
      }
      if (head !== undefined && head.seq < prev.seq) {
        for (let n = head.seq + 1; n <= seq; n++) await p.journal.del(`finger:token:${n}`)
        for (let n = head.seq + 1; n <= seq; n++) await p.journal.del(`finger:carrier:${n}`)
        // the stored witness AND body fingerprint belong to the dead head —
        // keep either and republishing the same body hits the dedup gate and
        // silently never re-airs. The row type wants a string and a rewound
        // state has no honest fingerprint to restore, so '' — it can never
        // equal a real hash
        await p.state.set({
          ...prev,
          seq: head.seq,
          tokenTxid: head.tokenTxid,
          tokenBeef: head.tokenBeef,
          bodyHash: '',
          refsHash: undefined,
          witness: undefined
        })
      } else {
        // the chain airs fine — the dead link was this mint's own input
        // (fresh funding died or the heal never reached it): drop just
        // this seq's token so the next pass re-mints instead of re-serving
        // the same refused tx forever
        await p.journal.del(`finger:token:${seq}`)
      }
    }
    if (dead && p.onDied !== undefined) await p.onDied(tokenTx)
    throw new Error(`publishFinger: token broadcast refused ${JSON.stringify(br).slice(0, 120)}`)
  }
  if (p.onSpent !== undefined) await p.onSpent(tokenTx)
  await p.state.set({
    seq,
    tokenTxid,
    tokenBeef: tokenBeefOut,
    bodyHash: hash,
    refsHash,
    witness: Utils.toHex(Array.from(await derived(s, 'witness', seq))),
    served: preServe ? { at: Date.now(), hosts: hostSig } : prev?.served
  })
  return { changed: true, seq, carrierTxid, tokenTxid }
}

/** Publish store material — a kind-5 sub-record or a kind-6 manifest — as a
 *  carrier under this identity. Store material never advances the chain and
 *  carries no token: ls_finger indexes it like any carrier and answers it
 *  through the {identityKey, carrier} class, which is exactly what a record
 *  `refs` entry's `head` resolves through. Every store record is create-
 *  shaped (seq 1, zero prev — the transition rules admit nothing else), so
 *  `id` is only the journal's idempotency slot: a retried id re-submits the
 *  journaled carrier, never a re-minted one, and a member's commitment is
 *  stable across crashes and republishes. */
export async function publishSubRecord(p: {
  key: SignerLike
  wallet?: SigningWallet
  ledger: TreeLedger /* kind 'finger' */
  journal: CarrierJournal
  hosts: string[]
  kind: 5 | 6
  /** journal idempotency key (member index / manifest version) — NOT the
   *  record's seq, which is always 1 */
  id: number
  body: CborMap
}): Promise<{ commitment: string; txid: string; missed: SubmitAllResult['missed']; pending: string[]; settled: SubmitAllResult['settled'] }> {
  const jkey = `finger:sub:${p.kind}:${p.id}`
  const hit = await p.journal.get(jkey)
  if (hit !== undefined) {
    // legs come back to the caller — a missed host gets a repair row like
    // every other fanned-out carrier (was silently dropped: no quorum, no
    // tracking, a pack PUT could "succeed" with zero hosts answering)
    const r = await submitAll(p.hosts, hit.beef, [FINGER_TOPIC])
    const c = Utils.toHex(Array.from(commitment(Transaction.fromAtomicBEEF(hit.beef))))
    return { commitment: c, txid: hit.txid, missed: r.missed, pending: r.pending, settled: r.settled }
  }
  const s = toSigner(p.key)
  const w = walletOrProto(p.key, p.wallet)
  const record: CommittedRecord = {
    magic: MagicV1,
    identityKey: new Uint8Array(PublicKey.fromString(await s.pubKeyHex()).encode(true) as number[]),
    seq: 1n,
    kind: p.kind,
    prev: ZERO32,
    salt: await derived(s, `salt:${p.kind}`, p.id),
    wc: new Uint8Array(Hash.sha256(Array.from(await derived(s, `witness:${p.kind}`, p.id)))),
    notBefore: 0n,
    notAfter: 0n,
    body: p.body,
    refs: [],
    unknown: []
  }
  validateRecord(record)
  const slot = await p.ledger.take(FINGER_KIND, (await fingerFundingLock(s, w)).toHex())
  if (slot === undefined) throw new Error(`publishSubRecord: ${await fundingSlotReason(p.ledger, FINGER_KIND)}`)
  const carrier = await mintCarrier(w, ORIGINATOR, DERIVATION_RECORD, encodeRecord(record), slot.tree, slot.vout)
  const beef = carrier.toAtomicBEEF()
  const txid = carrier.id('hex')
  await p.journal.put(jkey, { txid, beef, topics: [FINGER_TOPIC], fundingTxid: slot.tree.id('hex') })
  const r = await submitAll(p.hosts, beef, [FINGER_TOPIC])
  return { commitment: Utils.toHex(Array.from(commitment(carrier))), txid, missed: r.missed, pending: r.pending, settled: r.settled }
}

/** Repair the ancestry a BEEF carries for `tx` so it passes SPV while `tx`
 *  itself is still unmined. For every ancestor without a proof: if fetchTx
 *  now returns it proven, attach the path and cut its input sources (a proven
 *  tx terminates BEEF ancestry — the rule tree.ts markProven applies); if it
 *  stays unproven, fill input sources missing from the graph via their
 *  sourceTXID and keep walking. Depth-capped, visited-guarded; never touches
 *  `tx` itself. Returns whether anything changed. */
/** a bundle's structural refusal: an unproven entry names a parent the
 *  beef doesn't carry — no amount of re-serving makes the host's source
 *  check pass, so the serve path heals first and skips what stays open.
 *  BUMP-anchored entries terminate the walk; a coinbase-shaped input is a
 *  terminal, not a hole. */
function beefGaps(tx: Transaction): boolean {
  const seen = new Set<string>()
  const q: Transaction[] = [tx]
  while (q.length > 0) {
    const t = q.shift()!
    if (isEfStub(t)) continue
    const id = t.id('hex')
    if (seen.has(id)) continue
    seen.add(id)
    if (t.merklePath !== undefined) continue
    for (const i of t.inputs) {
      if (i.sourceTransaction === undefined) {
        if (i.sourceTXID !== undefined && i.sourceTXID !== '00'.repeat(32)) return true
        continue
      }
      q.push(i.sourceTransaction)
    }
  }
  return false
}

// an EF-sourced tx materializes each input's sourceTransaction as a stub:
// zero inputs, outputs null-filled except the spent index. Serializing one
// into a beef crashes the SDK writer (null.satoshis) — and a stub is a
// claimed-data leaf, not a verifiable parent, so it was never a real source.
// Detach stubs so gap detection sees (and fetches) the true ancestry.
const dropStubs = (t: Transaction): void => {
  for (const i of t.inputs) {
    if (i.sourceTransaction !== undefined && isEfStub(i.sourceTransaction)) i.sourceTransaction = undefined
  }
}

async function healAncestry(tx: Transaction, fetchTx: (txid: string) => Promise<Transaction | undefined>): Promise<boolean> {
  let changed = false
  const seen = new Set<string>([tx.id('hex')])
  // stub links fill concurrently — an input's missing parent fetch no longer
  // queues behind its siblings' RTTs
  const fill = async (a: Transaction): Promise<void> => {
    await Promise.all(
      a.inputs.map(async (i) => {
        if (i.sourceTransaction === undefined && i.sourceTXID !== undefined) {
          const src = await fetchTx(i.sourceTXID).catch(() => undefined)
          if (src !== undefined) {
            dropStubs(src)
            i.sourceTransaction = src
            changed = true
          }
        }
      })
    )
  }
  await fill(tx)
  let frontier = tx.inputs.filter((i) => i.sourceTransaction !== undefined).map((i) => i.sourceTransaction!)
  let depth = 1
  // the DFS walked one node at a time — proof probes and stub fills now
  // run per-level in parallel; the heal costs depth RTTs, not per-input serial
  while (frontier.length > 0 && depth <= 8) {
    const level = frontier
    frontier = []
    const results = await Promise.all(
      level.map(async (a) => {
        const id = a.id('hex')
        if (seen.has(id)) return { a, fetched: undefined, done: true }
        seen.add(id)
        if (a.merklePath !== undefined) return { a, fetched: undefined, done: true } // proven — ancestry terminates here
        const fetched = await fetchTx(id).catch(() => undefined)
        if (fetched?.merklePath === undefined) {
          dropStubs(a)
          await fill(a)
        }
        return { a, fetched, done: false }
      })
    )
    for (const r of results) {
      if (r.done) continue
      if (r.fetched?.merklePath !== undefined) {
        r.a.merklePath = r.fetched.merklePath
        for (const i of r.a.inputs) i.sourceTransaction = undefined
        changed = true
        continue // do not descend past a proven tx
      }
      for (const i of r.a.inputs) if (i.sourceTransaction !== undefined) frontier.push(i.sourceTransaction)
    }
    depth++
  }
  return changed
}

/** A deterministic 32 bytes for (key, label, seq): salt and witness material
 *  secret until revealed, stable across a crash retry. */
async function derived(key: SignerLike, label: string, seq: number): Promise<Uint8Array> {
  const s = toSigner(key)
  if (s.deriveSecret === undefined) throw new Error('signer does not support secret derivation')
  // `nektar finger ${label} ` + key + ` ${seq}` keeps the key-backed output
  // byte-identical to the old key.toWif() interpolation
  return await s.deriveSecret(`nektar finger ${label} `, ` ${seq}`)
}

/** The commitment the current token carries — the previous record's C. */
function prevCommitment(tokenBeef: number[]): Uint8Array {
  const tx = Transaction.fromBEEF(tokenBeef)
  for (const out of tx.outputs) {
    const t = inspectToken(out.lockingScript)
    if (t.kind === 'token' && t.token.valid) return new Uint8Array(t.token.c)
  }
  throw new Error('finger state: stored token carries no valid token field')
}

// ── resolve ────────────────────────────────────────────────────────────────

/** Ask every host `ls_finger` for the identity's current state, run the
 *  upstream verdicts and the §10 reader checks, and return the record behind
 *  the highest-sequence valid answer that `quorum` hosts agree on. undefined
 *  when nothing valid is found or the quorum is not met. */
export async function resolveFinger(p: {
  hosts: string[]
  pub: string /* 66-hex identity key */
  tracker: ChainTracker
  pin?: (pub: string) => Promise<string | undefined>
  quorum?: number
  allowHttp?: boolean
  now?: () => number
  timeoutMs?: number
}): Promise<
  | {
      body: FingerBody
      seq: number
      tokenTxid: string
      /** the winning record's committed store refs — already decoded by the
       *  stock codec; empty when the record publishes no stores */
      refs: Ref[]
      hostsAnswered: string[]
      unreachable: string[]
    }
  | undefined
> {
  const pinned = p.pin !== undefined ? await p.pin(p.pub) : undefined
  const { answers } = await lookupAll(p.hosts, 'ls_finger', { identityKey: p.pub.toLowerCase() }, { timeoutMs: p.timeoutMs })
  const unreachable: string[] = []
  const profileKey = profileLockingKey(p.pub)
  const recordKey = recordLockingKey(p.pub)

  // candidate: host -> verified {seq, tokenTxid, cHex, body}
  const byCarrier = new Map<string, { seq: number; tokenTxid: string; body: FingerBody; refs: Ref[]; hosts: string[] }>()
  for (const a of answers) {
    if ('error' in a) {
      unreachable.push(a.host)
      continue
    }
    // a current-state answer carries the token, the current carrier and the
    // previous carrier: the record is the carrier the token commits to
    let token: Transaction | undefined
    let cBytes: Uint8Array | undefined
    const carriers: Transaction[] = []
    for (const out of a.outputs) {
      const tx = parseBeef(out.beef)
      if (tx === undefined) continue
      const tt = tx.outputs.map((o) => inspectToken(o.lockingScript)).find((i) => i.kind === 'token')
      if (tt?.kind === 'token') {
        if (tt.token.valid) {
          token = tx
          cBytes = new Uint8Array(tt.token.c)
        }
        continue
      }
      const d = decodeCarrier(tx)
      if (typeof d !== 'string') carriers.push(tx)
    }
    const carrier =
      cBytes === undefined
        ? undefined
        : carriers.find((tx) => {
            try {
              return bytesEqual(new Uint8Array(commitment(tx)), cBytes)
            } catch {
              return false
            }
          })
    if (token === undefined || carrier === undefined || cBytes === undefined) continue
    // §10.3: the token is proven, or valid through proven ancestry (unmined)
    if (token.merklePath !== undefined) {
      if (!(await token.merklePath.verify(token.id('hex'), p.tracker))) continue
    } else {
      let ok = false
      try {
        ok = await token.verify(p.tracker)
      } catch (e) {
        // a header outage is "couldn't ask", not an invalid token — the
        // whole resolution rejects instead of reporting a miss
        if (isHeaderUnavailable(e)) throw e
        ok = false
      }
      if (!ok) continue
    }
    // §10.3: locked to the identity's profile derivation
    const tlock = token.outputs.map((o) => inspectToken(o.lockingScript)).find((i) => i.kind === 'token')
    if (tlock?.kind !== 'token') continue
    const dec = decodeCarrier(carrier)
    if (typeof dec === 'string') continue
    // §10.4: the record is the one the token commits to (C = carrier txid, hash order)
    if (!bytesEqual(cBytes, new Uint8Array(dec.c))) continue
    const rec = dec.record
    // §10.5: identity, derivation of both locks
    if (Utils.toHex(Array.from(rec.identityKey)) !== p.pub.toLowerCase()) continue
    if (Utils.toHex(Array.from(dec.lockingKey.encode(true) as number[])) !== recordKey.encode(true, 'hex')) continue
    const tokKey = tokenKeyOf(token)
    if (tokKey === undefined || tokKey !== profileKey.encode(true, 'hex')) continue
    // §10.4: the carrier's funding parent — mined or 1-hop anchored bf\x02
    // funding leaf under the identity's record derivation (consistent with
    // NEKTAR_ANCHORED_LANES=all / provenParent)
    const parent = carrier.inputs[0]?.sourceTransaction
    const pout = parent?.outputs[carrier.inputs[0].sourceOutputIndex]
    if (parent === undefined || pout === undefined) continue
    let parentOk = false
    try {
      if (parent.merklePath !== undefined) {
        parentOk = await parent.merklePath.verify(parent.id('hex'), p.tracker)
      } else if (parent.inputs.length > 0) {
        parentOk = true
        for (const pi of parent.inputs) {
          const gp = pi.sourceTransaction
          if (!gp?.merklePath || !(await gp.merklePath.verify(gp.id('hex'), p.tracker))) {
            parentOk = false
            break
          }
        }
      }
    } catch (e) {
      if (isHeaderUnavailable(e)) throw e
      parentOk = false
    }
    if (!parentOk) continue
    const fkey = decodeFunding(pout.lockingScript)
    if (fkey === undefined || fkey.encode(true, 'hex') !== recordKey.encode(true, 'hex')) continue
    // §10.5: validity window, when the record sets one (unix seconds)
    if (rec.notBefore !== 0n || rec.notAfter !== 0n) {
      const t = BigInt(Math.floor((p.now ?? Date.now)() / 1000))
      if (rec.notBefore !== 0n && t < rec.notBefore) continue
      if (rec.notAfter !== 0n && t > rec.notAfter) continue
    }
    if (pinned !== undefined && !bytesEqual(cBytes, pinnedBytes(pinned))) continue
    const body = mapToBody(rec.body)
    try {
      checkFingerBody(body, { allowHttp: p.allowHttp })
    } catch {
      continue
    }
    const cHex = Utils.toHex(Array.from(cBytes))
    const hit = byCarrier.get(cHex)
    if (hit === undefined) byCarrier.set(cHex, { seq: Number(rec.seq), tokenTxid: token.id('hex'), body, refs: rec.refs, hosts: [a.host] })
    else hit.hosts.push(a.host)
  }

  const quorum = p.quorum ?? 1
  const winners = [...byCarrier.values()].filter((c) => c.hosts.length >= quorum).sort((x, y) => y.seq - x.seq)
  const win = winners.at(0)
  if (win === undefined) return undefined
  return { body: win.body, seq: win.seq, tokenTxid: win.tokenTxid, refs: win.refs, hostsAnswered: win.hosts, unreachable }
}

/** Walk a committed store ref's `head`: ask `ls_finger` for the carrier under
 *  `{identityKey, carrier}` (the third stock question class — a carrier by its
 *  commitment, scoped to the identity that published it). Returns the decoded
 *  sub-record bodies — kind-5 members carry the object locator, a kind-6
 *  manifest lists every member. Membership against the ref root is the
 *  reader's own check: the host only asserts it holds the carrier. */
export async function resolveStoreMembers(p: {
  hosts: string[]
  pub: string
  /** the refs entry's head — carrier commitment, 64-hex display order */
  carrier: string
  tracker: ChainTracker
  timeoutMs?: number
}): Promise<Array<{ txid: string; kind: number; body: CborMap }>> {
  const { answers } = await lookupAll(p.hosts, 'ls_finger', { identityKey: p.pub.toLowerCase(), carrier: p.carrier.toLowerCase() }, { timeoutMs: p.timeoutMs })
  const out: Array<{ txid: string; kind: number; body: CborMap }> = []
  const seen = new Set<string>()
  for (const a of answers) {
    if ('error' in a) continue
    for (const o of a.outputs) {
      const tx = parseBeef(o.beef)
      if (tx === undefined) continue
      // the tracker param is not decorative: the host only ASSERTS it holds
      // this carrier — the carried beef is evidence only once its proofs
      // check against our own headers AND its commitment matches p.carrier.
      let verified = false
      try {
        const pathOk = tx.merklePath === undefined || (await tx.merklePath.verify(tx.id('hex'), p.tracker))
        verified = pathOk && (await tx.verify(p.tracker))
      } catch (e) {
        if (isHeaderUnavailable(e)) throw e
        verified = false
      }
      if (!verified) continue
      const d = decodeCarrier(tx)
      if (typeof d === 'string') continue
      // fab/resend-forgeable: verify the carrier's commitment matches the requested ref
      if (Utils.toHex(Array.from(d.c)) !== p.carrier.toLowerCase()) continue
      const rec = d.record
      if (rec.kind !== KindSub && rec.kind !== KindManifest) continue
      if (Utils.toHex(Array.from(rec.identityKey)) !== p.pub.toLowerCase()) continue
      const txid = tx.id('hex')
      if (seen.has(txid)) continue
      seen.add(txid)
      out.push({ txid, kind: rec.kind, body: rec.body })
    }
  }
  return out
}

/** The locking key a valid token carries. */
function tokenKeyOf(token: Transaction): string | undefined {
  for (const out of token.outputs) {
    const i = inspectToken(out.lockingScript)
    if (i.kind === 'token' && i.token.valid) return i.token.lockingKey.encode(true, 'hex') as string
  }
  return undefined
}

// a pin may be the 32-byte commitment or its hash-order hex
function pinnedBytes(pin: string): Uint8Array {
  try {
    return new Uint8Array(Utils.toArray(pin, 'hex'))
  } catch {
    return new Uint8Array(0)
  }
}

/** untrusted host bytes → the subject transaction. readWire is the stock
 *  bounded walk: BEEF V1/V2/Atomic, every declared count and length held to
 *  the bytes present (256 KiB default bound), inputs linked to the entries
 *  holding them and proven tx's BUMPs attached — malformed shapes refuse
 *  before the SDK parser ever sees them. */
function parseBeef(beef: number[]): Transaction | undefined {
  try {
    return subjectTx(readWire(new Uint8Array(beef)))
  } catch (e) {
    dbg('NEKTAR_DEBUG_PARSEBEEF', `parseBeef refused ${beef.length}B: ${(e as Error).message}`)
    return undefined
  }
}

// ── host screening (SSRF) ──────────────────────────────────────────────────

/** True when a resolved-record host URL is safe to contact: https (or http
 *  under allowHttp) and not a loopback/private/link-local target. The guard
 *  lives here so `resolveFinger` stays transport-agnostic and every consumer
 *  (runtime hostsFor) screens through one place. */
export function fingerHostAllowed(url: string, opts?: { allowHttp?: boolean; allowLocal?: boolean }): boolean {
  // shared implementation lives in @nektar/core — every outbound-fetch
  // surface screens through egressAllowed, not just lanes
  return egressAllowed(url, opts)
}
