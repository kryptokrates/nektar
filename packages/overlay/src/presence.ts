// tm_presence / ls_presence — the agent-directory topic.
// Agents "find each other" by publishing a signed OP_RETURN ad:
//   OP_RETURN 'nektar-agent' <pub 33B> <json{endpoint,shards,ts}> <sig DER>
// The signature binds endpoint+shards+ts to the advertiser's key — the same
// key that owns the anchor chain, so 'who serves shard X' is answered by the
// key that anchors X, not by whoever shouts loudest. Ads are 0-sat data
// outputs; latest-by-ts wins per pubkey.
import { PrivateKey, PublicKey, Signature, Script, Transaction } from '@bsv/sdk'
import { sha256, utf8, toHex } from '@nektar/core'
import type { Knex } from 'knex'
import type { TopicManager, AdmittanceInstructions, LookupService, LookupFormula, OutputAdmittedByTopic, OutputSpent } from '@bsv/overlay'

const TAG = 'nektar-agent'
const TABLE = 'nektar_ls_presence'

export interface PresenceAd {
  pub: string // 33B compressed pubkey hex — the agent's identity key
  endpoint: string // e.g. http://host:8080/mcp
  shards: string[] // shards it serves
  ts: number // ms epoch — latest wins
  exp?: number // ms epoch — ad expires; default none (explicit refresh recommended)
}
export interface SignedAd { ad: PresenceAd, sig: string } // sig = DER hex over the canonical digest

const canonical = (ad: PresenceAd) => `${ad.endpoint}|${[...ad.shards].sort().join(',')}|${ad.ts}|${ad.exp ?? ''}`
const digest = (ad: PresenceAd) => sha256(utf8(canonical(ad)))

// ── agent catalog — the signed "what I know" doc the directory indexes ──
// An ad says WHERE an agent is and WHICH shard labels it serves; the catalog
// says WHAT those shards cover (domain tags, descriptions, priced offers) and
// binds labels to on-chain shard ids. Signed by the same identity key, so the
// directory can index content claims that stay attributable to the agent.
export interface CatalogShardEntry {
  shard: string // local label — matches the ad's shards[] entry
  shardId?: string // on-chain id (hash160 of genesis owner) — checkable in ls_nektar
  domains: string[]
  desc?: string
  offers?: Array<{ kind: 'answer' | 'pack' | 'record', priceSats?: number, seq?: number }>
}
export interface AgentCatalog { pub: string, endpoint?: string, shards: CatalogShardEntry[], ts: number }
export interface SignedCatalog { catalog: AgentCatalog, sig: string }

/** deterministic JSON: object keys sorted recursively, array order kept */
const stableJson = (v: unknown): string => JSON.stringify(v, (_k, x) =>
  (x !== null && typeof x === 'object' && !Array.isArray(x)
    ? Object.fromEntries(Object.keys(x as object).sort().map(k => [k, (x as Record<string, unknown>)[k]]))
    : x))

const catalogCanonical = (c: AgentCatalog) => `${c.pub}|${c.ts}|${stableJson(c.shards)}`
const catalogDigest = (c: AgentCatalog) => sha256(utf8(catalogCanonical(c)))

const DOMAIN_RE = /^[a-z0-9][a-z0-9._-]{0,47}$/
const validEntry = (e: CatalogShardEntry): boolean =>
  typeof e?.shard === 'string' && e.shard.length > 0 && e.shard.length <= 64 &&
  Array.isArray(e.domains) && e.domains.length > 0 && e.domains.length <= 32 &&
  e.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d)) &&
  (e.desc === undefined || (typeof e.desc === 'string' && e.desc.length <= 560)) &&
  (e.shardId === undefined || (typeof e.shardId === 'string' && /^[0-9a-f]{40}$/i.test(e.shardId))) &&
  (e.offers === undefined || (Array.isArray(e.offers) && e.offers.length <= 64 &&
    e.offers.every(o => o && (o.kind === 'answer' || o.kind === 'pack' || o.kind === 'record'))))

export function signCatalog (key: PrivateKey, fields: Omit<AgentCatalog, 'pub'>): SignedCatalog {
  const catalog: AgentCatalog = { ...fields, pub: key.toPublicKey().toString() }
  return { catalog, sig: toHex(Uint8Array.from(key.sign(Array.from(catalogDigest(catalog))).toDER() as number[])) }
}

export function verifyCatalog (catalog: AgentCatalog, sigHex: string): boolean {
  try {
    if (typeof catalog?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(catalog.pub)) return false
    if (!Number.isSafeInteger(catalog.ts) || catalog.ts <= 0) return false
    if (catalog.endpoint !== undefined && (typeof catalog.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(catalog.endpoint))) return false
    if (!Array.isArray(catalog.shards) || catalog.shards.length === 0 || catalog.shards.length > 64 || !catalog.shards.every(validEntry)) return false
    return PublicKey.fromString(catalog.pub).verify(Array.from(catalogDigest(catalog)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// ── want ads — the demand side: a buyer's signed "I need domain X" ──────
// Wright's market routing is two-sided: catalogs answer "who claims X";
// a want says "who can answer X for at most N sats". Signed by the buyer's
// key so the request is attributable; latest-per-pub wins; exp honoured.
export interface WantAd {
  pub: string // buyer identity key
  domains: string[] // what it needs (same tag space as catalogs)
  question?: string // free-text what-for (leaks intent — keep generic)
  maxSats?: number // ceiling the buyer will pay per call/record
  endpoint?: string // where sellers can answer back
  ts: number
  exp?: number
}
export interface SignedWant { want: WantAd, sig: string }

const wantCanonical = (w: WantAd) => `${[...w.domains].sort().join(',')}|${w.question ?? ''}|${w.maxSats ?? ''}|${w.endpoint ?? ''}|${w.ts}|${w.exp ?? ''}`
const wantDigest = (w: WantAd) => sha256(utf8(wantCanonical(w)))

export function signWant (key: PrivateKey, fields: Omit<WantAd, 'pub'>): SignedWant {
  const want: WantAd = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() }
  return { want, sig: toHex(Uint8Array.from(key.sign(Array.from(wantDigest(want))).toDER() as number[])) }
}

export function verifyWant (want: WantAd, sigHex: string): boolean {
  try {
    if (typeof want?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(want.pub)) return false
    if (!Array.isArray(want.domains) || want.domains.length === 0 || want.domains.length > 32 || !want.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d))) return false
    if (want.question !== undefined && (typeof want.question !== 'string' || want.question.length > 560)) return false
    if (want.maxSats !== undefined && (!Number.isSafeInteger(want.maxSats) || want.maxSats < 0)) return false
    if (want.endpoint !== undefined && (typeof want.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(want.endpoint))) return false
    if (!Number.isSafeInteger(want.ts) || want.ts <= 0) return false
    if (want.exp !== undefined && (!Number.isSafeInteger(want.exp) || want.exp <= Date.now())) return false
    return PublicKey.fromString(want.pub).verify(Array.from(wantDigest(want)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// ── the event log — the rendezvous's JetStream: durable, sequenced ───────
// Every admitted ad/catalog/want appends {seq, kind, pub, domains[], ts};
// consumers hold a cursor and pull {since, domains, kind} — the directory
// serves the events, it never vouches for them (sigs ride inside).
// ── offers — the enumerable shelf: a signed per-record sale listing ─────
// The DisclosureSale offer artifact published as a market object: the leaf/
// hdp/batchR commitment lets a buyer verify inclusion BEFORE funding (the
// same checks sale#escrow runs). Keyed per (pub, shard, seq) — a seller
// lists many records; latest-per-key wins; expiry honoured at read.
export interface OfferAd {
  pub: string // seller identity key
  shard: string // label the record lives on
  seq: number // the record for sale
  shardId?: string // on-chain shard id when known
  leaf?: string // leaf commitment hex (verify-before-pay handle)
  hdp?: string // record identity hash hex
  batchR?: string // anchored batch root the leaf path reaches
  priceSats: number
  domains: string[] // from the shard's manifest — feed filtering
  offer?: Record<string, unknown> // the raw DisclosureSale offer (cfg/wrapped/sellerPub/offerSig/expiresAt/commitment)
  ts: number
  exp?: number
}
export interface SignedOffer { offer: OfferAd, sig: string }

const offerCanonical = (o: OfferAd) => `${o.shard}|${o.seq}|${o.shardId ?? ''}|${o.leaf ?? ''}|${o.hdp ?? ''}|${o.batchR ?? ''}|${o.priceSats}|${[...o.domains].sort().join(',')}|${o.ts}|${o.exp ?? ''}`
const offerDigest = (o: OfferAd) => sha256(utf8(offerCanonical(o)))
const offerKey = (o: OfferAd) => `${o.pub}:${o.shard}:${o.seq}`

export function signOffer (key: PrivateKey, fields: Omit<OfferAd, 'pub'>): SignedOffer {
  const offer: OfferAd = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() }
  return { offer, sig: toHex(Uint8Array.from(key.sign(Array.from(offerDigest(offer))).toDER() as number[])) }
}

export function verifyOffer (offer: OfferAd, sigHex: string): boolean {
  try {
    if (typeof offer?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(offer.pub)) return false
    if (typeof offer.shard !== 'string' || offer.shard.length === 0 || offer.shard.length > 64) return false
    if (!Number.isSafeInteger(offer.seq) || offer.seq < 0) return false
    if (!Number.isSafeInteger(offer.priceSats) || offer.priceSats < 0) return false
    if (!Array.isArray(offer.domains) || offer.domains.length > 32 || !offer.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d))) return false
    for (const h of [offer.shardId, offer.leaf, offer.hdp, offer.batchR]) if (h !== undefined && (typeof h !== 'string' || !/^[0-9a-f]{40,64}$/i.test(h))) return false
    if (!Number.isSafeInteger(offer.ts) || offer.ts <= 0) return false
    if (offer.exp !== undefined && (!Number.isSafeInteger(offer.exp) || offer.exp <= Date.now())) return false
    return PublicKey.fromString(offer.pub).verify(Array.from(offerDigest(offer)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

export type EventKind = 'ad' | 'catalog' | 'want' | 'offer'
export interface PresenceEvent { seq: number, kind: EventKind, pub: string, domains: string[], ts: number, payload: unknown }

export function signPresence (key: PrivateKey, fields: Omit<PresenceAd, 'pub'>): SignedAd {
  const ad: PresenceAd = { ...fields, shards: [...fields.shards].sort(), pub: key.toPublicKey().toString() }
  return { ad, sig: toHex(Uint8Array.from(key.sign(Array.from(digest(ad))).toDER() as number[])) }
}

export function verifyPresence (ad: PresenceAd, sigHex: string): boolean {
  try {
    if (typeof ad.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(ad.pub)) return false
    if (typeof ad.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(ad.endpoint)) return false
    if (!Array.isArray(ad.shards) || ad.shards.length > 64 || !ad.shards.every(s => typeof s === 'string' && s.length > 0 && s.length <= 64)) return false
    if (!Number.isSafeInteger(ad.ts) || ad.ts <= 0) return false
    if (ad.exp !== undefined && (!Number.isSafeInteger(ad.exp) || ad.exp <= Date.now())) return false // expired
    return PublicKey.fromString(ad.pub).verify(Array.from(digest(ad)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

export function presenceOutputScript ({ ad, sig }: SignedAd): string {
  const payload = JSON.stringify({ endpoint: ad.endpoint, shards: ad.shards, ts: ad.ts, ...(ad.exp !== undefined ? { exp: ad.exp } : {}) })
  return Script.fromASM(`OP_RETURN ${toHex(utf8(TAG))} ${ad.pub} ${toHex(utf8(payload))} ${sig}`).toHex()
}

const CAT_TAG = 'nektar-catalog'
const WANT_TAG = 'nektar-want'
const OFFER_TAG = 'nektar-offer'
/** on-chain catalog — same 4-push shape as presence ads; the payload carries
 *  everything except pub (push 1) so the canonical digest rebuilds. 0-sat
 *  data outputs ride tm_presence / the object plane like any admitted tx. */
export function catalogOutputScript ({ catalog, sig }: SignedCatalog): string {
  const payload = JSON.stringify({ ...(catalog.endpoint !== undefined ? { endpoint: catalog.endpoint } : {}), shards: catalog.shards, ts: catalog.ts })
  return Script.fromASM(`OP_RETURN ${toHex(utf8(CAT_TAG))} ${catalog.pub} ${toHex(utf8(payload))} ${sig}`).toHex()
}
export function wantOutputScript ({ want, sig }: SignedWant): string {
  const payload = JSON.stringify({ domains: want.domains, ...(want.question !== undefined ? { question: want.question } : {}), ...(want.maxSats !== undefined ? { maxSats: want.maxSats } : {}), ...(want.endpoint !== undefined ? { endpoint: want.endpoint } : {}), ts: want.ts, ...(want.exp !== undefined ? { exp: want.exp } : {}) })
  return Script.fromASM(`OP_RETURN ${toHex(utf8(WANT_TAG))} ${want.pub} ${toHex(utf8(payload))} ${sig}`).toHex()
}
export function offerOutputScript ({ offer, sig }: SignedOffer): string {
  const payload = JSON.stringify({ shard: offer.shard, seq: offer.seq, ...(offer.shardId ? { shardId: offer.shardId } : {}), ...(offer.leaf ? { leaf: offer.leaf } : {}), ...(offer.hdp ? { hdp: offer.hdp } : {}), ...(offer.batchR ? { batchR: offer.batchR } : {}), priceSats: offer.priceSats, domains: offer.domains, ...(offer.offer ? { offer: offer.offer } : {}), ts: offer.ts, ...(offer.exp !== undefined ? { exp: offer.exp } : {}) })
  return Script.fromASM(`OP_RETURN ${toHex(utf8(OFFER_TAG))} ${offer.pub} ${toHex(utf8(payload))} ${sig}`).toHex()
}

// The SDK's Script.chunks collapses OP_RETURN data into one chunk — decode the
// pushes ourselves. Everything after the leading 0x6a must be pushdata or it
// isn't our format.
function readPushes (hex: string): Buffer[] | null {
  const b = Buffer.from(hex, 'hex')
  if (b[0] !== 0x6a) return null
  const out: Buffer[] = []
  let i = 1
  while (i < b.length) {
    const op = b[i++]!
    let len: number
    if (op === 0) { out.push(Buffer.alloc(0)); continue }
    if (op <= 75) len = op
    else if (op === 0x4c) { len = b[i] ?? -1; i += 1 }
    else if (op === 0x4d) { len = i + 2 <= b.length ? b.readUInt16LE(i) : -1; i += 2 }
    else if (op === 0x4e) { len = i + 4 <= b.length ? b.readUInt32LE(i) : -1; i += 4 }
    else return null
    if (len < 0 || i + len > b.length) return null
    out.push(Buffer.from(b.subarray(i, i + len))); i += len
  }
  return out
}

export function parsePresence (scriptHex: string): SignedAd | null {
  try {
    const pushes = readPushes(scriptHex)
    if (!pushes || pushes.length !== 4) return null
    if (pushes[0]!.toString() !== TAG) return null
    const pub = pushes[1]!.toString('hex')
    const p = JSON.parse(pushes[2]!.toString())
    const ad: PresenceAd = { pub, endpoint: p.endpoint, shards: p.shards, ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) }
    return { ad, sig: pushes[3]!.toString('hex') }
  } catch { return null }
}

export function parseCatalogOutput (scriptHex: string): SignedCatalog | null {
  try {
    const pushes = readPushes(scriptHex)
    if (!pushes || pushes.length !== 4) return null
    if (pushes[0]!.toString() !== CAT_TAG) return null
    const pub = pushes[1]!.toString('hex')
    const p = JSON.parse(pushes[2]!.toString())
    const catalog: AgentCatalog = { pub, ...(p.endpoint !== undefined ? { endpoint: p.endpoint } : {}), shards: p.shards, ts: p.ts }
    return { catalog, sig: pushes[3]!.toString('hex') }
  } catch { return null }
}

export function parseWantOutput (scriptHex: string): SignedWant | null {
  try {
    const pushes = readPushes(scriptHex)
    if (!pushes || pushes.length !== 4) return null
    if (pushes[0]!.toString() !== WANT_TAG) return null
    const pub = pushes[1]!.toString('hex')
    const p = JSON.parse(pushes[2]!.toString())
    const want: WantAd = { pub, domains: p.domains, ...(p.question !== undefined ? { question: p.question } : {}), ...(p.maxSats !== undefined ? { maxSats: p.maxSats } : {}), ...(p.endpoint !== undefined ? { endpoint: p.endpoint } : {}), ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) }
    return { want, sig: pushes[3]!.toString('hex') }
  } catch { return null }
}

export function parseOfferOutput (scriptHex: string): SignedOffer | null {
  try {
    const pushes = readPushes(scriptHex)
    if (!pushes || pushes.length !== 4) return null
    if (pushes[0]!.toString() !== OFFER_TAG) return null
    const pub = pushes[1]!.toString('hex')
    const p = JSON.parse(pushes[2]!.toString())
    const offer: OfferAd = { pub, shard: p.shard, seq: p.seq, ...(p.shardId ? { shardId: p.shardId } : {}), ...(p.leaf ? { leaf: p.leaf } : {}), ...(p.hdp ? { hdp: p.hdp } : {}), ...(p.batchR ? { batchR: p.batchR } : {}), priceSats: p.priceSats, domains: p.domains, ...(p.offer ? { offer: p.offer } : {}), ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) }
    return { offer, sig: pushes[3]!.toString('hex') }
  } catch { return null }
}

/** any presence-topic output — ad, catalog, want or offer */
export type ParsedPresence = { kind: 'ad', ad: SignedAd } | { kind: 'catalog', sc: SignedCatalog } | { kind: 'want', sw: SignedWant } | { kind: 'offer', so: SignedOffer }
export function parseAnyPresence (scriptHex: string): ParsedPresence | null {
  const ad = parsePresence(scriptHex); if (ad) return { kind: 'ad', ad }
  const sc = parseCatalogOutput(scriptHex); if (sc) return { kind: 'catalog', sc }
  const sw = parseWantOutput(scriptHex); if (sw) return { kind: 'want', sw }
  const so = parseOfferOutput(scriptHex); if (so) return { kind: 'offer', so }
  return null
}

export function createTmPresence (): TopicManager {
  return {
    identifyAdmissibleOutputs: async (beef: number[], _previousCoins: number[]): Promise<AdmittanceInstructions> => {
      const tx = Transaction.fromBEEF(beef)
      const admit: AdmittanceInstructions = { outputsToAdmit: [], coinsToRetain: [] }
      for (const [i, o] of tx.outputs.entries()) {
        const hex = o.lockingScript.toHex()
        const p = parseAnyPresence(hex)
        if (p === null) continue
        const ok = p.kind === 'ad' ? verifyPresence(p.ad.ad, p.ad.sig)
          : p.kind === 'catalog' ? verifyCatalog(p.sc.catalog, p.sc.sig)
          : p.kind === 'want' ? verifyWant(p.sw.want, p.sw.sig)
          : verifyOffer(p.so.offer, p.so.sig)
        if (ok) admit.outputsToAdmit.push(i)
      }
      return admit
    },
    identifyNeededInputs: async () => [],
    getDocumentation: async () =>
      "Admits OP_RETURN outputs 'nektar-agent'|'nektar-catalog'|'nektar-want' <pub> <payload> <sig> where sig is a valid signature by pub over that kind's canonical digest. Stateless — no chain ordering required; latest-by-ts wins per kind in the lookup service.",
    getMetaData: async () => ({ name: 'tm_presence', shortDescription: 'nektar agent-directory: signed presence/catalog/want ads' })
  }
}

interface Stored { ad: PresenceAd, txid: string, outputIndex: number }
interface StoredCatalog { sc: SignedCatalog }

export function createLsPresence (kx?: Knex): LookupService & {
  init: () => Promise<void>
  announce: (s: SignedAd) => Promise<boolean>
  announceCatalog: (sc: SignedCatalog) => Promise<boolean>
  announceWant: (sw: SignedWant) => Promise<boolean>
  announceOffer: (so: SignedOffer) => Promise<boolean>
  catalogFor: (pub: string) => SignedCatalog | undefined
  wantFor: (pub: string) => SignedWant | undefined
  offersFor: (domain?: string) => SignedOffer[]
  feed: (q: { since?: number, domains?: string[], kind?: EventKind, limit?: number }) => Promise<PresenceEvent[]>
} {
  // latest ad per pub; shard → set of pubs advertising it
  const ads = new Map<string, Stored>()
  const shardIndex = new Map<string, Set<string>>()
  // latest signed catalog per pub; domain tag → set of pubs claiming it
  const catalogs = new Map<string, StoredCatalog>()
  const domainIndex = new Map<string, Set<string>>()
  // latest signed want per pub
  const wants = new Map<string, SignedWant>()
  // live offers keyed pub:shard:seq (one seller lists many records)
  const offers = new Map<string, SignedOffer>()
  // the durable event log — seq is the consumer cursor
  const events: PresenceEvent[] = []
  let nextSeq = 0
  const appendEvent = (kind: EventKind, pub: string, domains: string[], payload: unknown) => {
    const e: PresenceEvent = { seq: nextSeq++, kind, pub, domains, ts: Date.now(), payload }
    events.push(e)
    if (kx) void (async () => { if (await kx.schema.hasTable(EVENTS_TABLE)) await kx(EVENTS_TABLE).insert({ seq: e.seq, kind: e.kind, pub: e.pub, domains: JSON.stringify(e.domains), ts: e.ts, payload: JSON.stringify(e.payload) }) })().catch(() => {})
    return e
  }
  const EVENTS_TABLE = 'nektar_ls_events'
  const WANTS_TABLE = 'nektar_ls_wants'

  const reindex = (pub: string, ad: PresenceAd | undefined, prev?: PresenceAd) => {
    if (prev) for (const s of prev.shards) { const set = shardIndex.get(s); set?.delete(pub); if (set && set.size === 0) shardIndex.delete(s) }
    if (ad) for (const s of ad.shards) { const set = shardIndex.get(s) ?? new Set<string>(); set.add(pub); shardIndex.set(s, set) }
  }
  const reindexCatalog = (pub: string, cat: AgentCatalog | undefined, prev?: AgentCatalog) => {
    const doms = (c?: AgentCatalog) => new Set((c?.shards ?? []).flatMap(e => e.domains.map(d => d.toLowerCase())))
    if (prev) for (const d of doms(prev)) { const set = domainIndex.get(d); set?.delete(pub); if (set && set.size === 0) domainIndex.delete(d) }
    if (cat) for (const d of doms(cat)) { const set = domainIndex.get(d) ?? new Set<string>(); set.add(pub); domainIndex.set(d, set) }
  }

  const persist = async (s: Stored) => {
    if (!kx) return
    const row = { pub: s.ad.pub, txid: s.txid, output_index: s.outputIndex, endpoint: s.ad.endpoint, shards: JSON.stringify(s.ad.shards), ts: s.ad.ts }
    if (await kx.schema.hasTable(TABLE)) await kx(TABLE).insert(row).onConflict('pub').merge(row)
  }
  const unpersist = async (txid: string, vout: number) => {
    if (!kx || !(await kx.schema.hasTable(TABLE))) return
    await kx(TABLE).where({ txid, output_index: vout }).del()
  }
  const CAT_TABLE = 'nektar_ls_catalog'
  const persistCatalog = async (sc: SignedCatalog) => {
    if (!kx || !(await kx.schema.hasTable(CAT_TABLE))) return
    const row = { pub: sc.catalog.pub, ts: sc.catalog.ts, catalog: JSON.stringify(sc.catalog), sig: sc.sig }
    await kx(CAT_TABLE).insert(row).onConflict('pub').merge(row)
  }
  const persistWant = async (sw: SignedWant) => {
    if (!kx || !(await kx.schema.hasTable(WANTS_TABLE))) return
    const row = { pub: sw.want.pub, ts: sw.want.ts, want: JSON.stringify(sw.want), sig: sw.sig }
    await kx(WANTS_TABLE).insert(row).onConflict('pub').merge(row)
  }
  const OFFERS_TABLE = 'nektar_ls_offers'
  const persistOffer = async (so: SignedOffer) => {
    if (!kx || !(await kx.schema.hasTable(OFFERS_TABLE))) return
    const row = { key: offerKey(so.offer), pub: so.offer.pub, ts: so.offer.ts, offer: JSON.stringify(so.offer), sig: so.sig }
    await kx(OFFERS_TABLE).insert(row).onConflict('key').merge(row)
  }

  const formula = (s: Stored) => ({
    txid: s.txid, outputIndex: s.outputIndex,
    context: [...utf8(JSON.stringify(s.ad))]
  })

  return {
    admissionMode: 'locking-script',
    spendNotificationMode: 'txid',

    init: async () => {
      if (!kx) return
      if (!(await kx.schema.hasTable(TABLE))) {
        await kx.schema.createTable(TABLE, t => {
          t.string('pub').primary()
          t.string('txid').notNullable()
          t.integer('output_index').notNullable()
          t.string('endpoint').notNullable()
          t.string('shards').notNullable()
          t.bigInteger('ts').notNullable()
        })
      }
      for (const r of await kx(TABLE).select('*')) {
        const s: Stored = { ad: { pub: r.pub, endpoint: r.endpoint, shards: JSON.parse(r.shards), ts: Number(r.ts) }, txid: r.txid, outputIndex: r.output_index }
        ads.set(r.pub, s); reindex(r.pub, s.ad)
      }
      if (!(await kx.schema.hasTable(CAT_TABLE))) {
        await kx.schema.createTable(CAT_TABLE, t => {
          t.string('pub').primary()
          t.bigInteger('ts').notNullable()
          t.string('catalog').notNullable()
          t.string('sig').notNullable()
        })
      }
      for (const r of await kx(CAT_TABLE).select('*')) {
        const sc: SignedCatalog = { catalog: JSON.parse(r.catalog), sig: r.sig }
        const prev = catalogs.get(r.pub)
        if (prev && prev.sc.catalog.ts >= sc.catalog.ts) continue
        catalogs.set(r.pub, { sc }); reindexCatalog(r.pub, sc.catalog, prev?.sc.catalog)
      }
      if (!(await kx.schema.hasTable(WANTS_TABLE))) {
        await kx.schema.createTable(WANTS_TABLE, t => {
          t.string('pub').primary()
          t.bigInteger('ts').notNullable()
          t.string('want').notNullable()
          t.string('sig').notNullable()
        })
      }
      for (const r of await kx(WANTS_TABLE).select('*')) {
        const sw: SignedWant = { want: JSON.parse(r.want), sig: r.sig }
        const prev = wants.get(r.pub)
        if (!prev || prev.want.ts < sw.want.ts) wants.set(r.pub, sw)
      }
      if (!(await kx.schema.hasTable(EVENTS_TABLE))) {
        await kx.schema.createTable(EVENTS_TABLE, t => {
          t.integer('seq').primary()
          t.string('kind').notNullable()
          t.string('pub').notNullable()
          t.string('domains').notNullable()
          t.bigInteger('ts').notNullable()
          t.string('payload').notNullable()
        })
      }
      for (const r of await kx(EVENTS_TABLE).select('*').orderBy('seq')) {
        events.push({ seq: Number(r.seq), kind: r.kind as EventKind, pub: r.pub, domains: JSON.parse(r.domains), ts: Number(r.ts), payload: JSON.parse(r.payload) })
      }
      nextSeq = events.length ? events.at(-1)!.seq + 1 : 0
      if (!(await kx.schema.hasTable(OFFERS_TABLE))) {
        await kx.schema.createTable(OFFERS_TABLE, t => {
          t.string('key').primary()
          t.string('pub').notNullable()
          t.bigInteger('ts').notNullable()
          t.string('offer').notNullable()
          t.string('sig').notNullable()
        })
      }
      for (const r of await kx(OFFERS_TABLE).select('*')) {
        const so: SignedOffer = { offer: JSON.parse(r.offer), sig: r.sig }
        const prev = offers.get(r.key)
        if (!prev || prev.offer.ts < so.offer.ts) offers.set(r.key, so)
      }
    },

    outputAdmittedByTopic: (payload: OutputAdmittedByTopic) => {
      if (payload.mode !== 'locking-script') return
      const hex = typeof payload.lockingScript === 'string' ? payload.lockingScript : payload.lockingScript?.toHex?.() ?? ''
      const p = parseAnyPresence(hex)
      if (!p) return
      if (p.kind === 'ad') {
        const parsed = p.ad
        if (!verifyPresence(parsed.ad, parsed.sig)) return
        const prev = ads.get(parsed.ad.pub)
        if (prev && prev.ad.ts >= parsed.ad.ts) return // stale ad — latest wins
        const s: Stored = { ad: parsed.ad, txid: payload.txid, outputIndex: payload.outputIndex }
        ads.set(parsed.ad.pub, s)
        reindex(parsed.ad.pub, parsed.ad, prev?.ad)
        void persist(s)
        appendEvent('ad', parsed.ad.pub, parsed.ad.shards, parsed)
        return
      }
      if (p.kind === 'catalog') {
        const sc = p.sc
        if (!verifyCatalog(sc.catalog, sc.sig)) return
        const prev = catalogs.get(sc.catalog.pub)
        if (prev && prev.sc.catalog.ts >= sc.catalog.ts) return
        catalogs.set(sc.catalog.pub, { sc })
        reindexCatalog(sc.catalog.pub, sc.catalog, prev?.sc.catalog)
        void persistCatalog(sc)
        appendEvent('catalog', sc.catalog.pub, sc.catalog.shards.flatMap(e => e.domains), sc)
        return
      }
      if (p.kind === 'want') {
        const sw = p.sw
        if (!verifyWant(sw.want, sw.sig)) return
        const prevW = wants.get(sw.want.pub)
        if (prevW && prevW.want.ts >= sw.want.ts) return
        wants.set(sw.want.pub, sw)
        void persistWant(sw)
        appendEvent('want', sw.want.pub, sw.want.domains, sw)
        return
      }
      const so = p.so
      if (!verifyOffer(so.offer, so.sig)) return
      const ok_ = offerKey(so.offer)
      const prevO = offers.get(ok_)
      if (prevO && prevO.offer.ts >= so.offer.ts) return
      offers.set(ok_, so)
      void persistOffer(so)
      appendEvent('offer', so.offer.pub, so.offer.domains, so)
    },

    // Off-chain rendezvous: same verification, same latest-wins rule, no UTXO.
    // Synthetic outpoint = sha256 of the ad (32B hex — the Engine validates
    // output txids); a newer ad (on- or off-chain) replaces it by ts.
    announce: async (s: SignedAd) => {
      if (!verifyPresence(s.ad, s.sig)) return false
      const prev = ads.get(s.ad.pub)
      if (prev && prev.ad.ts >= s.ad.ts) return true
      const rec: Stored = { ad: s.ad, txid: toHex(digest(s.ad)), outputIndex: 0 }
      ads.set(s.ad.pub, rec)
      reindex(s.ad.pub, s.ad, prev?.ad)
      await persist(rec)
      appendEvent('ad', s.ad.pub, s.ad.shards, s)
      return true
    },

    // Signed catalog — same off-channel path as announce(): verified, latest-
    // wins per pub. The directory's content index: domain tag → claimants.
    announceCatalog: async (sc: SignedCatalog) => {
      if (!verifyCatalog(sc.catalog, sc.sig)) return false
      const prev = catalogs.get(sc.catalog.pub)
      if (prev && prev.sc.catalog.ts >= sc.catalog.ts) return true
      catalogs.set(sc.catalog.pub, { sc })
      reindexCatalog(sc.catalog.pub, sc.catalog, prev?.sc.catalog)
      await persistCatalog(sc)
      appendEvent('catalog', sc.catalog.pub, sc.catalog.shards.flatMap(e => e.domains), sc)
      return true
    },

    // Signed want — the demand side. Latest-per-pub wins; exp honoured at
    // read time. Every accepted want lands on the event log so agents
    // subscribed to its domains see it on their next cursor pull.
    announceWant: async (sw: SignedWant) => {
      if (!verifyWant(sw.want, sw.sig)) return false
      const prev = wants.get(sw.want.pub)
      if (prev && prev.want.ts >= sw.want.ts) return true
      wants.set(sw.want.pub, sw)
      await persistWant(sw)
      appendEvent('want', sw.want.pub, sw.want.domains, sw)
      return true
    },

    announceOffer: async (so: SignedOffer) => {
      if (!verifyOffer(so.offer, so.sig)) return false
      const k = offerKey(so.offer)
      const prev = offers.get(k)
      if (prev && prev.offer.ts >= so.offer.ts) return true
      offers.set(k, so)
      await persistOffer(so)
      appendEvent('offer', so.offer.pub, so.offer.domains, so)
      return true
    },

    catalogFor: (pub: string) => catalogs.get(pub)?.sc,
    wantFor: (pub: string) => wants.get(pub),
    offersFor: (domain?: string) => [...offers.values()].filter(so => so.offer.exp === undefined || so.offer.exp > Date.now()).filter(so => domain === undefined || so.offer.domains.some(d => d.toLowerCase() === domain.toLowerCase())),

    // JetStream-pull view: events after `since` cursor, optionally narrowed
    // by kind and/or domain tags (a consumer subscribed to 'soccer' sees any
    // event whose domains contain it).
    feed: async (q: { since?: number, domains?: string[], kind?: EventKind, limit?: number }): Promise<PresenceEvent[]> => {
      const since = q.since ?? -1
      const doms = q.domains?.length ? new Set(q.domains.map(d => d.toLowerCase())) : undefined
      const out: PresenceEvent[] = []
      for (const e of events) {
        if (e.seq <= since) continue
        if (q.kind && e.kind !== q.kind) continue
        if (doms && !e.domains.some(d => doms.has(d.toLowerCase()))) continue
        out.push(e)
        if (q.limit !== undefined && out.length >= q.limit) break
      }
      return out
    },

    outputSpent: (payload: OutputSpent) => {
      if (payload.mode === 'none') return
      for (const [pub, s] of [...ads]) {
        if (s.txid === payload.txid && s.outputIndex === payload.outputIndex) {
          ads.delete(pub); reindex(pub, undefined, s.ad); void unpersist(s.txid, s.outputIndex)
        }
      }
    },
    outputEvicted: (txid: string, outputIndex: number) => {
      for (const [pub, s] of [...ads]) {
        if (s.txid === txid && s.outputIndex === outputIndex) {
          ads.delete(pub); reindex(pub, undefined, s.ad); void unpersist(s.txid, s.outputIndex)
        }
      }
    },

    lookup: async ({ query }: { query?: any }): Promise<LookupFormula> => {
      const live = (s: Stored) => s.ad.exp === undefined || s.ad.exp > Date.now()
      const q = (query ?? {}) as { all?: boolean, shard?: string, pub?: string, domain?: string, q?: string }
      if (typeof q.domain === 'string') {
        // content query: domain tag → claimants' signed catalogs (context =
        // the signed catalog JSON so callers verify the sig themselves)
        const pubs = domainIndex.get(q.domain.toLowerCase()) ?? new Set<string>()
        return [...pubs].map(p => catalogs.get(p)!.sc).filter(sc => { const a = ads.get(sc.catalog.pub); return a === undefined || live(a) })
          .map(sc => ({ txid: toHex(catalogDigest(sc.catalog)), outputIndex: 0, context: [...utf8(JSON.stringify(sc))] }))
      }
      if (typeof q.q === 'string') {
        // free-text: substring over each entry's domains + description
        const needle = q.q.toLowerCase()
        const matches = (e: CatalogShardEntry) => e.domains.some(d => d.toLowerCase().includes(needle)) || e.desc?.toLowerCase().includes(needle)
        return [...catalogs.values()].map(c => c.sc)
          .filter(sc => sc.catalog.shards.some(matches))
          .filter(sc => (ads.get(sc.catalog.pub)?.ad.exp ?? Infinity) > Date.now())
          .map(sc => ({ txid: toHex(catalogDigest(sc.catalog)), outputIndex: 0, context: [...utf8(JSON.stringify(sc))] }))
      }
      if (q.all === true) return [...ads.values()].filter(live).map(formula)
      if (typeof q.shard === 'string') return [...(shardIndex.get(q.shard) ?? [])].map(p => ads.get(p)!).filter(live).map(formula)
      if (typeof q.pub === 'string') { const s = ads.get(q.pub); return s && live(s) ? [formula(s)] : [] }
      return []
    },

    getDocumentation: async () =>
      "nektar agent-directory lookup. Queries: {all:true} → latest signed ad per agent; {shard:'x'} → ads of agents serving shard x; {pub:'<66hex>'} → one agent's ad; {domain:'soccer'} → signed catalogs of agents claiming that domain tag; {q:'text'} → free-text match over catalog domains+descriptions. Context = the ad JSON (or the signed catalog for domain/q).",
    getMetaData: async () => ({ name: 'ls_presence', shortDescription: 'nektar agent-directory lookup service' })
  }
}
