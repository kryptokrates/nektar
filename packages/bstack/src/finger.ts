// bfinger identity records (bfinger v0.8.4, committed-record spec §§2-4,7,10):
// a signed record names the hosts carrying an identity's artifacts, its bbox
// office and its blogs stream, and is anchored by a mined state token that
// commits to the carrier's txid. The record encodings, tags, derivations and
// admission verdicts are upstream's (vendored @lightwebinc/bfinger-host);
// carriers, funding trees and unlock scripts are bcommon's, as for artifacts
// and log batches.
import {
  PrivateKey, ProtoWallet, Transaction, P2PKH, Hash, Utils,
  type ChainTracker, type LockingScript
} from '@bsv/sdk'
import {
  mintCarrier, fundingLock, lockFields, unlocker, commitment,
  CborMap, encode as cborEncode, type Derivation
} from '@lightwebinc/bcommon'
import type { FundingOut } from '@nektar/chain'
import {
  encodeRecord, decodeRecord, validateRecord, KindCreate, KindUpdate,
  MagicV1, type CommittedRecord
} from '@lightwebinc/bfinger-host/dist/record.js'
import { Protocol, KeyIDProfile, KeyIDRecord, profileLockingKey, recordLockingKey } from '@lightwebinc/bfinger-host/dist/keys.js'
import { Tag, inspectToken } from '@lightwebinc/bfinger-host/dist/token.js'
import { decodeCarrier } from '@lightwebinc/bfinger-host/dist/carrier.js'
import { FundingTag, decodeFunding } from '@lightwebinc/bfinger-host/dist/funding.js'
import { checkOffice } from '@lightwebinc/bbox-host/codec/boxrec.js'
import { checkStream } from '@lightwebinc/blogs-host/dist/index.js'
import { submitAll, lookupAll } from './hosts.js'
import type { TreeLedger } from './tree.js'
import type { CarrierJournal } from './journal.js'
import type { Knex } from 'knex'

export const FINGER_KIND = 'finger'
export const FINGER_TOPIC = 'tm_finger'
const ORIGINATOR = 'nektar'
const DERIVATION_PROFILE: Derivation = { protocol: Protocol, keyID: KeyIDProfile }
const DERIVATION_RECORD: Derivation = { protocol: Protocol, keyID: KeyIDRecord }
const ZERO32 = new Uint8Array(32)

export const fingerFundingLock = async (key: PrivateKey): Promise<LockingScript> =>
  await fundingLock(new ProtoWallet(key), ORIGINATOR, DERIVATION_RECORD, new Uint8Array(FundingTag))

// ── the record body ────────────────────────────────────────────────────────

export interface FingerBody {
  v: 1
  hosts: string[] /* overlay hosts carrying this identity's artifacts, 1..8 */
  office: string /* bbox office */
  stream: string /* blogs journal stream */
  nets: string[] /* network labels this record is authoritative for */
}

const NET_LABEL = /^[a-z][a-z0-9-]{0,15}$/

/** Shape-check a body. `allowHttp` is for mock/regtest hosts: records on a
 *  real network name https hosts only. Throws on any violation. */
export function checkFingerBody (body: FingerBody, opts?: { allowHttp?: boolean }): void {
  if (body === null || typeof body !== 'object' || body.v !== 1) throw new Error('finger body: v must be 1')
  if (!Array.isArray(body.hosts) || body.hosts.length < 1 || body.hosts.length > 8) throw new Error('finger body: 1..8 hosts')
  for (const h of body.hosts) {
    let u: URL
    try { u = new URL(h) } catch { throw new Error(`finger body: bad host URL ${JSON.stringify(h).slice(0, 60)}`) }
    if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) {
      throw new Error(`finger body: host must be https ${u.protocol}//`)
    }
  }
  try { checkOffice(body.office) } catch { throw new Error('finger body: bad office') }
  try { checkStream(body.stream) } catch { throw new Error('finger body: bad stream') }
  if (!Array.isArray(body.nets) || body.nets.length < 1 || body.nets.length > 8 ||
    body.nets.some(n => typeof n !== 'string' || !NET_LABEL.test(n))) throw new Error('finger body: bad nets')
}

function bodyToMap (body: FingerBody): CborMap {
  return new CborMap([
    { key: 'v', val: 1n },
    { key: 'hosts', val: body.hosts },
    { key: 'office', val: body.office },
    { key: 'stream', val: body.stream },
    { key: 'nets', val: body.nets }
  ])
}

function mapToBody (m: CborMap): FingerBody {
  const v = m.get('v')
  const body = {
    v: typeof v === 'bigint' ? Number(v) : v,
    hosts: m.get('hosts'), office: m.get('office'), stream: m.get('stream'), nets: m.get('nets')
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
  witness?: string
}

export interface FingerState {
  get (): Promise<FingerStateRow | undefined>
  set (s: FingerStateRow): Promise<void>
}

export class MemoryFingerState implements FingerState {
  private row: FingerStateRow | undefined
  async get () { return this.row }
  async set (s: FingerStateRow) { this.row = s }
}

export class KnexFingerState implements FingerState {
  constructor (private kx: Knex) {}
  private ensured?: Promise<void>
  private async ensure (): Promise<void> {
    return await (this.ensured ??= this.doEnsure().catch(e => { this.ensured = undefined; throw e }))
  }
  private async doEnsure (): Promise<void> {
    if (!(await this.kx.schema.hasTable('bstack_finger_state'))) {
      await this.kx.schema.createTable('bstack_finger_state', t => {
        t.increments('id').primary(); t.integer('seq').notNullable(); t.string('tokenTxid').notNullable()
        t.text('tokenBeef').notNullable(); t.string('bodyHash').notNullable(); t.string('witness').nullable()
      })
    }
  }
  async get () {
    await this.ensure()
    const r = await this.kx('bstack_finger_state').orderBy('id').first()
    return r === undefined ? undefined : {
      seq: Number(r.seq), tokenTxid: String(r.tokenTxid), tokenBeef: JSON.parse(String(r.tokenBeef)) as number[],
      bodyHash: String(r.bodyHash), ...(r.witness !== null && r.witness !== undefined ? { witness: String(r.witness) } : {})
    }
  }
  async set (s: FingerStateRow): Promise<void> {
    await this.ensure()
    const row = { seq: s.seq, tokenTxid: s.tokenTxid, tokenBeef: JSON.stringify(s.tokenBeef), bodyHash: s.bodyHash, witness: s.witness ?? null }
    if ((await this.kx('bstack_finger_state').count('id as n').first())?.n === 0) {
      await this.kx('bstack_finger_state').insert(row)
    } else {
      await this.kx('bstack_finger_state').update(row)
    }
  }
}

// ── publish ────────────────────────────────────────────────────────────────

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
export async function publishFinger (p: {
  key: PrivateKey
  state: FingerState
  ledger: TreeLedger /* kind 'finger' */
  journal: CarrierJournal
  hosts: string[]
  body: FingerBody
  takeFunding: (sats: number) => Promise<FundingOut>
  broadcast: (tx: Transaction) => Promise<{ status: string }>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  allowHttp?: boolean
  onSpent?: (tx: Transaction) => void | Promise<void>
  now?: () => number
}): Promise<{ changed: boolean, seq: number, carrierTxid?: string, tokenTxid?: string }> {
  checkFingerBody(p.body, { allowHttp: p.allowHttp })
  const w = new ProtoWallet(p.key)
  let map = bodyToMap(p.body)
  let hash = bodyHash(map)
  const topics = [FINGER_TOPIC]
  const prev = await p.state.get()

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
    } catch { /* resubmission below uses whatever state holds */ }
  }

  if (prev !== undefined && prev.bodyHash === hash) {
    const ch = await p.journal.get(`finger:carrier:${prev.seq}`)
    if (ch !== undefined) await submitAll(p.hosts, ch.beef, topics, { retryWaits: [] })
    const tokenBeef = latestBeef ?? prev.tokenBeef ?? (await p.journal.get(`finger:token:${prev.seq}`))?.beef
    if (tokenBeef !== undefined) await submitAll(p.hosts, tokenBeef, topics, { retryWaits: [] })
    return { changed: false, seq: prev.seq, carrierTxid: ch?.txid, tokenTxid: prev.tokenTxid }
  }

  // build the record. Salt and witness derive from (key, seq) so a retried
  // mint reproduces byte-identical record bytes — and therefore the same
  // carrier — even across a crash between the journal write and state.set.
  // The witness is secret until the next record reveals it; deriving it from
  // the private key keeps it unguessable without a random table to keep.
  const seq = (prev?.seq ?? 0) + 1
  const kind = prev === undefined ? KindCreate : KindUpdate
  const pub33 = new Uint8Array(p.key.toPublicKey().encode(true) as number[])
  const witness = derived(p.key, 'witness', seq)
  const prevC = prev === undefined ? ZERO32 : prevCommitment(prev.tokenBeef)
  const prevWitness = prev !== undefined
    ? (prev.witness !== undefined ? new Uint8Array(Utils.toArray(prev.witness, 'hex')) : derived(p.key, 'witness', prev.seq))
    : undefined
  let record: CommittedRecord = {
    magic: MagicV1, identityKey: pub33, seq: BigInt(seq), kind,
    prev: prevC, salt: derived(p.key, 'salt', seq),
    wc: new Uint8Array(Hash.sha256(Array.from(witness))),
    ...(prevWitness !== undefined ? { prevWitness } : {}),
    notBefore: 0n, notAfter: 0n, body: map, refs: [], unknown: []
  }

  // the carrier, journaled per seq BEFORE submission. A hit means this seq
  // was already journaled: the journaled record is the committed one, so its
  // body — not the caller's — is what gets published and stored.
  let carrierBeef: number[], carrierTxid: string
  const chit = await p.journal.get(`finger:carrier:${seq}`)
  if (chit !== undefined) {
    carrierBeef = chit.beef; carrierTxid = chit.txid
    const dec = decodeCarrier(Transaction.fromAtomicBEEF(carrierBeef))
    if (typeof dec === 'string') throw new Error(`finger journal: bad stored carrier ${dec}`)
    record = dec.record
    map = dec.record.body
    hash = bodyHash(map)
  } else {
    validateRecord(record)
    const slot = await p.ledger.take(FINGER_KIND)
    if (slot === undefined) throw new Error('publishFinger: no funding slot')
    const carrier = await mintCarrier(w, ORIGINATOR, DERIVATION_RECORD, encodeRecord(record), slot.tree, slot.vout)
    carrierBeef = carrier.toAtomicBEEF()
    carrierTxid = carrier.id('hex')
    await p.journal.put(`finger:carrier:${seq}`, { txid: carrierTxid, beef: carrierBeef, topics })
  }

  // the state token: spends the previous token (update) plus wallet funding
  // for the fee — wallet funding, not a bf\x02 leaf, because a funding output
  // spent by anything but its own carrier is the kill-switch signal (§7).
  const tkey = `finger:token:${seq}`
  let tokenBeefOut: number[], tokenTxid: string, tokenTx: Transaction
  const thit = await p.journal.get(tkey)
  if (thit !== undefined) {
    tokenBeefOut = thit.beef; tokenTxid = thit.txid; tokenTx = Transaction.fromBEEF(thit.beef)
  } else {
    const c = commitment(Transaction.fromAtomicBEEF(carrierBeef))
    tokenTx = new Transaction()
    if (prev !== undefined) {
      const prevToken = Transaction.fromBEEF(prev.tokenBeef)
      tokenTx.addInput({ sourceTransaction: prevToken, sourceOutputIndex: 0, unlockingScriptTemplate: unlocker(w, ORIGINATOR, DERIVATION_PROFILE) })
    }
    tokenTx.addOutput({ satoshis: 1, lockingScript: await lockFields(w, ORIGINATOR, DERIVATION_PROFILE, [new Uint8Array(Tag), new Uint8Array(c)], true) })
    tokenTx.addOutput({ lockingScript: new P2PKH().lock(p.key.toPublicKey().toAddress()), change: true })
    const fund = await p.takeFunding(5000)
    tokenTx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(p.key) })
    await tokenTx.fee()
    await tokenTx.sign()
    // the fee parent may be a not-yet-proven tx — carry whatever ancestry is
    // obtainable now so the BEEF passes host SPV while the token is unmined
    await healAncestry(tokenTx, p.fetchTx)
    tokenTxid = tokenTx.id('hex')
    tokenBeefOut = tokenTx.toBEEF()
    await p.journal.put(tkey, { txid: tokenTxid, beef: tokenBeefOut, topics })
  }

  await submitAll(p.hosts, carrierBeef, topics, { retryWaits: [] })
  await submitAll(p.hosts, tokenBeefOut, topics, { retryWaits: [] })
  if (thit === undefined) {
    const br = await p.broadcast(tokenTx)
    if (br.status !== 'success') throw new Error(`publishFinger: token broadcast refused ${JSON.stringify(br).slice(0, 120)}`)
  }
  if (p.onSpent !== undefined) await p.onSpent(tokenTx)
  await p.state.set({ seq, tokenTxid, tokenBeef: tokenBeefOut, bodyHash: hash, witness: Utils.toHex(Array.from(derived(p.key, 'witness', seq))) })
  return { changed: true, seq, carrierTxid, tokenTxid }
}

/** Repair the ancestry a BEEF carries for `tx` so it passes SPV while `tx`
 *  itself is still unmined. For every ancestor without a proof: if fetchTx
 *  now returns it proven, attach the path and cut its input sources (a proven
 *  tx terminates BEEF ancestry — the rule tree.ts markProven applies); if it
 *  stays unproven, fill input sources missing from the graph via their
 *  sourceTXID and keep walking. Depth-capped, visited-guarded; never touches
 *  `tx` itself. Returns whether anything changed. */
async function healAncestry (tx: Transaction, fetchTx: (txid: string) => Promise<Transaction | undefined>): Promise<boolean> {
  let changed = false
  const seen = new Set<string>([tx.id('hex')])
  const walk = async (a: Transaction, depth: number): Promise<void> => {
    if (depth > 8) return
    const id = a.id('hex')
    if (seen.has(id)) return
    seen.add(id)
    if (a.merklePath !== undefined) return // proven — ancestry terminates here
    const fetched = await fetchTx(id).catch(() => undefined)
    if (fetched?.merklePath !== undefined) {
      a.merklePath = fetched.merklePath
      for (const i of a.inputs) i.sourceTransaction = undefined
      changed = true
      return // do not descend past a proven tx
    }
    for (const i of a.inputs) {
      if (i.sourceTransaction === undefined && i.sourceTXID !== undefined) {
        const src = await fetchTx(i.sourceTXID).catch(() => undefined)
        if (src !== undefined) { i.sourceTransaction = src; changed = true }
      }
      if (i.sourceTransaction !== undefined) await walk(i.sourceTransaction, depth + 1)
    }
  }
  for (const i of tx.inputs) {
    if (i.sourceTransaction === undefined && i.sourceTXID !== undefined) {
      const src = await fetchTx(i.sourceTXID).catch(() => undefined)
      if (src !== undefined) { i.sourceTransaction = src; changed = true }
    }
    if (i.sourceTransaction !== undefined) await walk(i.sourceTransaction, 1)
  }
  return changed
}

/** A deterministic 32 bytes for (key, label, seq): salt and witness material
 *  secret until revealed, stable across a crash retry. */
function derived (key: PrivateKey, label: string, seq: number): Uint8Array {
  return new Uint8Array(Hash.sha256(Utils.toArray(`nektar finger ${label} ${key.toWif()} ${seq}`, 'utf8')))
}

/** The commitment the current token carries — the previous record's C. */
function prevCommitment (tokenBeef: number[]): Uint8Array {
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
export async function resolveFinger (p: {
  hosts: string[]
  pub: string /* 66-hex identity key */
  tracker: ChainTracker
  pin?: (pub: string) => Promise<string | undefined>
  quorum?: number
  allowHttp?: boolean
  now?: () => number
}): Promise<{
  body: FingerBody
  seq: number
  tokenTxid: string
  hostsAnswered: string[]
  unreachable: string[]
} | undefined> {
  const pinned = p.pin !== undefined ? await p.pin(p.pub) : undefined
  const { answers } = await lookupAll(p.hosts, 'ls_finger', { identityKey: p.pub.toLowerCase() })
  const unreachable: string[] = []
  const profileKey = profileLockingKey(p.pub)
  const recordKey = recordLockingKey(p.pub)

  // candidate: host -> verified {seq, tokenTxid, cHex, body}
  const byCarrier = new Map<string, { seq: number, tokenTxid: string, body: FingerBody, hosts: string[] }>()
  for (const a of answers) {
    if ('error' in a) { unreachable.push(a.host); continue }
    // a current-state answer carries the token, the current carrier and the
    // previous carrier: the record is the carrier the token commits to
    let token: Transaction | undefined
    let cBytes: Uint8Array | undefined
    const carriers: Transaction[] = []
    for (const out of a.outputs) {
      const tx = parseBeef(out.beef)
      if (tx === undefined) continue
      const tt = tx.outputs.map(o => inspectToken(o.lockingScript)).find(i => i.kind === 'token')
      if (tt?.kind === 'token') { if (tt.token.valid) { token = tx; cBytes = new Uint8Array(tt.token.c) } continue }
      const d = decodeCarrier(tx)
      if (typeof d !== 'string') carriers.push(tx)
    }
    const carrier = cBytes === undefined ? undefined : carriers.find(tx => {
      try { return bytesEq(new Uint8Array(commitment(tx)), cBytes) } catch { return false }
    })
    if (token === undefined || carrier === undefined || cBytes === undefined) continue
    // §10.3: the token is proven, or valid through proven ancestry (unmined)
    if (token.merklePath !== undefined) {
      if (!(await token.merklePath.verify(token.id('hex'), p.tracker))) continue
    } else {
      let ok = false
      try { ok = await token.verify(p.tracker) } catch { ok = false }
      if (!ok) continue
    }
    // §10.3: locked to the identity's profile derivation
    const tlock = token.outputs.map(o => inspectToken(o.lockingScript)).find(i => i.kind === 'token')
    if (tlock?.kind !== 'token') continue
    const dec = decodeCarrier(carrier)
    if (typeof dec === 'string') continue
    // §10.4: the record is the one the token commits to (C = carrier txid, hash order)
    if (!bytesEq(cBytes, new Uint8Array(dec.c))) continue
    const rec = dec.record
    // §10.5: identity, derivation of both locks
    if (Utils.toHex(Array.from(rec.identityKey)) !== p.pub.toLowerCase()) continue
    if (Utils.toHex(Array.from(dec.lockingKey.encode(true) as number[])) !== recordKey.encode(true, 'hex')) continue
    const tokKey = tokenKeyOf(token)
    if (tokKey === undefined || tokKey !== profileKey.encode(true, 'hex')) continue
    // §10.4: the carrier's funding parent — a mined bf\x02 funding leaf under
    // the identity's record derivation
    const parent = carrier.inputs[0]?.sourceTransaction
    const pout = parent?.outputs[carrier.inputs[0]!.sourceOutputIndex]
    if (parent === undefined || pout === undefined) continue
    if (parent.merklePath === undefined || !(await parent.merklePath.verify(parent.id('hex'), p.tracker))) continue
    const fkey = decodeFunding(pout.lockingScript)
    if (fkey === undefined || fkey.encode(true, 'hex') !== recordKey.encode(true, 'hex')) continue
    // §10.5: validity window, when the record sets one (unix seconds)
    if (rec.notBefore !== 0n || rec.notAfter !== 0n) {
      const t = BigInt(Math.floor((p.now ?? Date.now)() / 1000))
      if (rec.notBefore !== 0n && t < rec.notBefore) continue
      if (rec.notAfter !== 0n && t > rec.notAfter) continue
    }
    if (pinned !== undefined && !bytesEq(cBytes, pinnedBytes(pinned))) continue
    const body = mapToBody(rec.body)
    try { checkFingerBody(body, { allowHttp: p.allowHttp }) } catch { continue }
    const cHex = Utils.toHex(Array.from(cBytes))
    const hit = byCarrier.get(cHex)
    if (hit === undefined) byCarrier.set(cHex, { seq: Number(rec.seq), tokenTxid: token.id('hex'), body, hosts: [a.host] })
    else hit.hosts.push(a.host)
  }

  const quorum = p.quorum ?? 1
  const winners = [...byCarrier.values()].filter(c => c.hosts.length >= quorum).sort((x, y) => y.seq - x.seq)
  const win = winners[0]
  if (win === undefined) return undefined
  return { body: win.body, seq: win.seq, tokenTxid: win.tokenTxid, hostsAnswered: win.hosts, unreachable }
}

/** The locking key a valid token carries. */
function tokenKeyOf (token: Transaction): string | undefined {
  for (const out of token.outputs) {
    const i = inspectToken(out.lockingScript)
    if (i.kind === 'token' && i.token.valid) return i.token.lockingKey.encode(true, 'hex') as string
  }
  return undefined
}

const bytesEq = (a: Uint8Array, b: Uint8Array): boolean => a.length === b.length && a.every((x, i) => x === b[i])

// a pin may be the 32-byte commitment or its hash-order hex
function pinnedBytes (pin: string): Uint8Array {
  try { return new Uint8Array(Utils.toArray(pin, 'hex')) } catch { return new Uint8Array(0) }
}

function parseBeef (beef: number[]): Transaction | undefined {
  try { return Transaction.fromBEEF(beef) } catch { /* */ }
  try { return Transaction.fromAtomicBEEF(beef) } catch { /* */ }
  return undefined
}

// ── host screening (SSRF) ──────────────────────────────────────────────────

/** True when a resolved-record host URL is safe to contact: https (or http
 *  under allowHttp) and not a loopback/private/link-local target. The guard
 *  lives here so `resolveFinger` stays transport-agnostic and every consumer
 *  (runtime hostsFor) screens through one place. */
export function fingerHostAllowed (url: string, opts?: { allowHttp?: boolean, allowLocal?: boolean }): boolean {
  let u: URL
  try { u = new URL(url) } catch { return false }
  if (u.protocol !== 'https:' && !(opts?.allowHttp === true && u.protocol === 'http:')) return false
  if (opts?.allowLocal === true) return true // mock/regtest: hosts are the test rig's
  const h = u.hostname.toLowerCase()
  if (h === 'localhost' || h.endsWith('.localhost')) return false
  const ip = /^\d{1,3}(\.\d{1,3}){3}$/.test(h) ? h.split('.').map(Number) : undefined
  if (ip !== undefined) {
    const [a, b] = ip
    if (a === 127 || a === 0) return false // loopback / this-host
    if (a === 10 || (a === 172 && b >= 16 && b <= 31) || (a === 192 && b === 168)) return false // private
    if (a === 169 && b === 254) return false // link-local (metadata endpoint)
    if (a >= 224) return false // multicast/reserved
    if (ip.some(o => o > 255)) return false
  }
  if (h === '::1' || h === '[::1]' || h.startsWith('fe80') || h.startsWith('[fe80')) return false
  return true
}
