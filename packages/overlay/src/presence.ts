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
import { EventEmitter } from 'node:events'
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
  /** signed network binding — 'ttn'|'main'|etc; sig covers it (can't replay cross-net) */
  net?: string
  /** the publisher's chain tip height at publish — a verifiable timeline claim */
  tipHeight?: number
  /** SPV binding — the blockhash at tipHeight on the claimed chain; verifiable
   *  against real headers, so a ttn-signed artifact can't fake being mainnet */
  tip?: string
  /** F2 producer stream — monotonic seq + predecessor hash (signature-covered) */
  streamSeq?: number
  prevHash?: string
}
export interface SignedWant { want: WantAd, sig: string, pay?: { beef?: number[] | string } }

const wantCanonical = (w: WantAd) => `${[...w.domains].sort().join(',')}|${w.question ?? ''}|${w.maxSats ?? ''}|${w.endpoint ?? ''}|${w.ts}|${w.exp ?? ''}|${w.net ?? ''}|${w.tipHeight ?? ''}|${w.tip ?? ''}|${w.streamSeq ?? ''}|${w.prevHash ?? ''}`
const wantDigest = (w: WantAd) => sha256(utf8(wantCanonical(w)))

// per-producer stream state — signers auto-stamp streamSeq/prevHash; the rv's
// interval commitments cover the same stream a reader detects gaps in
const streamState = new Map<string, { seq: number, hash: string }>()
const GENESIS_HASH = '0'.repeat(64)
export function streamRef (pub: string, kind: string, prevHash?: string): { streamSeq: number, prevHash: string } {
  const k = `${pub}:${kind}`
  const s = streamState.get(k) ?? { seq: 0, hash: GENESIS_HASH }
  streamState.set(k, { seq: s.seq + 1, hash: prevHash ?? s.hash })
  return { streamSeq: s.seq + 1, prevHash: s.hash }
}
export function streamCommit (pub: string, kind: string, hash: string): void {
  const s = streamState.get(`${pub}:${kind}`); if (s) s.hash = hash
}

export function signWant (key: PrivateKey, fields: Omit<WantAd, 'pub'>): SignedWant {
  const pub = key.toPublicKey().toString()
  const sr = fields.streamSeq !== undefined && fields.prevHash !== undefined ? { streamSeq: fields.streamSeq, prevHash: fields.prevHash } : streamRef(pub, 'want')
  const want: WantAd = { ...fields, ...sr, domains: [...fields.domains].sort(), pub }
  return { want, sig: toHex(Uint8Array.from(key.sign(Array.from(wantDigest(want))).toDER() as number[])) }
}

export function verifyWant (want: WantAd, sigHex: string): boolean {
  if (want.streamSeq === undefined || want.prevHash === undefined) return false // v2: stream fields required
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
  replyTo?: string // pub of the want this offer answers — demand-meets-supply matching
  offer?: Record<string, unknown> // the raw DisclosureSale offer (cfg/wrapped/sellerPub/offerSig/expiresAt/commitment)
  ts: number
  exp?: number
  /** signed network binding — 'ttn'|'main'|etc; sig covers it (can't replay cross-net) */
  net?: string
  /** the publisher's chain tip height at publish — a verifiable timeline claim */
  tipHeight?: number
  /** SPV binding — the blockhash at tipHeight on the claimed chain; verifiable
   *  against real headers, so a ttn-signed artifact can't fake being mainnet */
  tip?: string
  streamSeq?: number
  prevHash?: string
}
export interface SignedOffer { offer: OfferAd, sig: string, pay?: { beef?: number[] | string } }

const offerCanonical = (o: OfferAd) => `${o.shard}|${o.seq}|${o.shardId ?? ''}|${o.leaf ?? ''}|${o.hdp ?? ''}|${o.batchR ?? ''}|${o.priceSats}|${[...o.domains].sort().join(',')}|${o.ts}|${o.exp ?? ''}|${o.replyTo ?? ''}|${o.net ?? ''}|${o.tipHeight ?? ''}|${o.tip ?? ''}|${o.streamSeq ?? ''}|${o.prevHash ?? ''}`
const offerDigest = (o: OfferAd) => sha256(utf8(offerCanonical(o)))
const offerKey = (o: OfferAd) => `${o.pub}:${o.shard}:${o.seq}`

export function signOffer (key: PrivateKey, fields: Omit<OfferAd, 'pub'>): SignedOffer {
  const pub = key.toPublicKey().toString()
  const sr = fields.streamSeq !== undefined && fields.prevHash !== undefined ? { streamSeq: fields.streamSeq, prevHash: fields.prevHash } : streamRef(pub, 'offer')
  const offer: OfferAd = { ...fields, ...sr, domains: [...fields.domains].sort(), pub }
  return { offer, sig: toHex(Uint8Array.from(key.sign(Array.from(offerDigest(offer))).toDER() as number[])) }
}

export function verifyOffer (offer: OfferAd, sigHex: string): boolean {
  if (offer.streamSeq === undefined || offer.prevHash === undefined) return false // v2: stream fields required
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

// ── settlements — realized-price announcements (signed, txid-checkable) ──
// The tx is already on-chain (nektarpay:<invoiceId>); the POST is the
// signed announcement that makes realized prices indexable per domain.
export interface SettlementAd {
  pub: string // seller identity key
  invoiceId: string
  tool: string // what was bought (recall|groundedAnswer|exportShard|...)
  sats: number
  txid: string // settlement tx — independently checkable on-chain
  domains: string[] // the sold content's declared domains
  ts: number
}
export interface SignedSettlement { ad: SettlementAd, sig: string }
const settlementCanonical = (s: SettlementAd) => `${s.invoiceId}|${s.tool}|${s.sats}|${s.txid}|${[...s.domains].sort().join(',')}|${s.ts}`
const settlementDigest = (s: SettlementAd) => sha256(utf8(settlementCanonical(s)))
export function signSettlement (key: PrivateKey, fields: Omit<SettlementAd, 'pub'>): SignedSettlement {
  const ad: SettlementAd = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() }
  return { ad, sig: toHex(Uint8Array.from(key.sign(Array.from(settlementDigest(ad))).toDER() as number[])) }
}
export function verifySettlement (ad: SettlementAd, sigHex: string): boolean {
  try {
    if (typeof ad?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(ad.pub)) return false
    if (typeof ad.invoiceId !== 'string' || ad.invoiceId.length < 4 || ad.invoiceId.length > 64) return false
    if (typeof ad.txid !== 'string' || !/^[0-9a-f]{64}$/i.test(ad.txid)) return false
    if (!Number.isSafeInteger(ad.sats) || ad.sats < 0) return false
    if (typeof ad.tool !== 'string' || ad.tool.length === 0 || ad.tool.length > 48) return false
    if (!Array.isArray(ad.domains) || !ad.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d))) return false
    if (!Number.isSafeInteger(ad.ts) || ad.ts <= 0) return false
    return PublicKey.fromString(ad.pub).verify(Array.from(settlementDigest(ad)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// ── reputation cards — per-domain vectors over committed history ────────
// Wright: reputation is measurable performance over time, multidimensional,
// never one score. The card is a signed derivation over anchored records
// (receipts, settlements, activity) — the inputs are chain-verifiable; the
// signature just makes the published snapshot attributable.
export interface ReputationEntry { domain: string, receipts?: number, settledSats?: number, settlements?: number, verifiedCites?: number }
export interface ReputationCard {
  pub: string
  entries: ReputationEntry[]
  shardId?: string
  since?: number // activity window start
  asOf: number // computed at
}
export interface SignedReputation { card: ReputationCard, sig: string }
const repCanonical = (c: ReputationCard) => `${c.shardId ?? ''}|${JSON.stringify(c.entries)}|${c.since ?? ''}|${c.asOf}`
const repDigest = (c: ReputationCard) => sha256(utf8(repCanonical(c)))
export function signReputation (key: PrivateKey, fields: Omit<ReputationCard, 'pub'>): SignedReputation {
  const card: ReputationCard = { ...fields, pub: key.toPublicKey().toString() }
  return { card, sig: toHex(Uint8Array.from(key.sign(Array.from(repDigest(card))).toDER() as number[])) }
}
export function verifyReputationCard (card: ReputationCard, sigHex: string): boolean {
  try {
    if (typeof card?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(card.pub)) return false
    if (!Array.isArray(card.entries) || card.entries.length > 256 || !card.entries.every(e => e && typeof e.domain === 'string' && DOMAIN_RE.test(e.domain))) return false
    if (!Number.isSafeInteger(card.asOf) || card.asOf <= 0) return false
    return PublicKey.fromString(card.pub).verify(Array.from(repDigest(card)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

export type EventKind = 'ad' | 'catalog' | 'want' | 'offer' | 'settlement' | 'reputation' | 'delegation' | 'attestation' | 'revocation' | 'beacon' | 'interval' | 'envelope'

// ── beacon — an rv's signed self-announcement (BRC-126 ADVERT semantics over
// HTTPS): "I serve these topics at this endpoint on this net." Any rv or agent
// can hold the set, so the index itself is discoverable, not hardcoded. ──────
export interface BeaconAd {
  pub: string // the rv's identity key
  endpoint: string // https://rv.example — where its presence API lives
  net?: string // 'ttn'|'main'|etc — which chain lane it indexes
  topics: string[] // e.g. ['tm_presence'] — what it serves (SHIP advertisement)
  lookups?: string[] // e.g. ['ls_presence'] — lookup services it exposes (SLAP)
  ts: number
  exp?: number
}
// the standard-shapes accessor — a beacon rendered as a BRC-88 SHIP/SLAP ad
export const beaconShip = (b: BeaconAd) => ({ SHIP: b.topics, SLAP: b.lookups ?? [], endpoint: b.endpoint, net: b.net, pub: b.pub })
export interface SignedBeacon { beacon: BeaconAd, sig: string }
const beaconCanonical = (b: BeaconAd) => `${b.endpoint}|${[...b.topics].sort().join(',')}|${b.net ?? ''}|${b.ts}|${b.exp ?? ''}|${[...(b.lookups ?? [])].sort().join(',')}`
export function signBeacon (key: PrivateKey, b: Omit<BeaconAd, 'pub'>): SignedBeacon {
  const beacon = { ...b, pub: key.toPublicKey().toString() } as BeaconAd
  return { beacon, sig: toHex(Uint8Array.from(key.sign(Array.from(digestBeacon(beacon))).toDER() as number[])) }
}
const digestBeacon = (b: BeaconAd) => sha256(utf8(beaconCanonical(b)))
export function verifyBeacon (b: BeaconAd, sigHex: string): boolean {
  try {
    return PublicKey.fromString(b.pub).verify(Array.from(digestBeacon(b)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// ── revocation — an owner's signed tombstone for a grant. Relays MUST verify:
// pub equality alone is not authentication, anyone could claim to be an owner.
export interface RevocationAd { grantId: string, pub: string, ts: number }
export interface SignedRevocation { revocation: RevocationAd, sig: string }

// ── F2 producer stream — each publisher's signed sequence + predecessor link,
// plus the interval commitment a host anchors so a reader can detect omission,
// reordering, and alteration in a known range. Spec: producer signs sequence
// monotonic-by-one; interval binds [first..last] + merkleRoot + prevInterval.
export interface StreamRef { seq: number, prevHash?: string } // seq monotonic, prevHash = sha256(prev event canonical)
export interface IntervalCommit {
  pub: string // producer
  streamId: string // the stream's name (e.g. 'presence')
  firstSeq: number
  lastSeq: number
  eventCount: number
  merkleRoot: string // sha256 of the leaf-set (canonical order)
  prevIntervalHash?: string // chain back to the prior interval
  ts: number
}
export interface SignedInterval { commit: IntervalCommit, sig: string }
const intervalCanonical = (c: IntervalCommit) => `${c.streamId}|${c.firstSeq}|${c.lastSeq}|${c.eventCount}|${c.merkleRoot}|${c.prevIntervalHash ?? ''}|${c.ts}`
export function signInterval (key: PrivateKey, c: Omit<IntervalCommit, 'pub'>): SignedInterval {
  const commit = { ...c, pub: key.toPublicKey().toString() } as IntervalCommit
  return { commit, sig: toHex(Uint8Array.from(key.sign(Array.from(sha256(utf8(intervalCanonical(commit))))).toDER() as number[])) }
}
export function verifyInterval (c: IntervalCommit, sigHex: string): boolean {
  try { return PublicKey.fromString(c.pub).verify(Array.from(sha256(utf8(intervalCanonical(c)))), Signature.fromDER(sigHex, 'hex')) } catch { return false }
}

// ── F6 async mailbox — a signed encrypted envelope kept by the rv until the
// recipient collects it. The keeper never sees the content: it stores the
// envelope + ciphertextHash; delivery proof is the signature + dedup by id.
export interface EnvelopeAd {
  messageId: string // sender-chosen unique id — dedup key
  version: 1
  sender: string // sender pub
  recipient: string // recipient pub — keeper serves to this key only
  conversationId?: string
  operationId?: string
  kind: 'request' | 'offer' | 'acceptance' | 'delivery-reference' | 'acknowledgement' | 'cancellation'
  createdAt: number
  expiresAt?: number
  replyTo?: string
  ciphertextHash: string // sha256 of the encrypted payload — content stays opaque to the keeper
  ciphertext: string // hex — encrypted payload (AES-GCM, recipient-pub-derived)
  encryptionProfile?: string
}
export interface SignedEnvelope { envelope: EnvelopeAd, sig: string }
const envCanonical = (e: EnvelopeAd) => `${e.messageId}|${e.version}|${e.sender}|${e.recipient}|${e.conversationId ?? ''}|${e.operationId ?? ''}|${e.kind}|${e.createdAt}|${e.expiresAt ?? ''}|${e.replyTo ?? ''}|${e.ciphertextHash}|${e.encryptionProfile ?? ''}`
export function signEnvelope (key: PrivateKey, e: Omit<EnvelopeAd, 'sender' | 'version'>): SignedEnvelope {
  const envelope = { ...e, version: 1, sender: key.toPublicKey().toString() } as EnvelopeAd
  return { envelope, sig: toHex(Uint8Array.from(key.sign(Array.from(sha256(utf8(envCanonical(envelope))))).toDER() as number[])) }
}
export function verifyEnvelope (e: EnvelopeAd, sigHex: string): boolean {
  try { return PublicKey.fromString(e.sender).verify(Array.from(sha256(utf8(envCanonical(e)))), Signature.fromDER(sigHex, 'hex')) } catch { return false }
}
const revCanonical = (r: RevocationAd) => `${r.grantId}|${r.pub}|${r.ts}`
export function signRevocation (key: PrivateKey, r: RevocationAd): SignedRevocation {
  return { revocation: { ...r, pub: key.toPublicKey().toString() }, sig: toHex(Uint8Array.from(key.sign(Array.from(sha256(utf8(revCanonical(r))))).toDER() as number[])) }
}
export function verifyRevocation (r: RevocationAd, sigHex: string): boolean {
  try { return PublicKey.fromString(r.pub).verify(Array.from(sha256(utf8(revCanonical(r)))), Signature.fromDER(sigHex, 'hex')) } catch { return false }
}
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

export function createLsPresence (kx?: Knex, opts?: { net?: string, strictNet?: boolean, maxAgeMs?: number, listingFee?: { addr?: string, rateSatsPerHour?: number, freeWindowMs?: number }, tracker?: { headerAtHeight: (h: number) => Promise<{ hash: string } | undefined> } }): LookupService & {
  init: () => Promise<void>
  announce: (s: SignedAd) => Promise<boolean>
  announceCatalog: (sc: SignedCatalog) => Promise<boolean>
  announceWant: (sw: SignedWant) => Promise<boolean>
  announceOffer: (so: SignedOffer) => Promise<boolean>
  announceSettlement: (s: SignedSettlement) => Promise<boolean>
  announceReputation: (sr: SignedReputation) => Promise<boolean>
  announceDelegation: (sd: SignedDelegation) => Promise<boolean>
  announceAttestation: (sa: SignedAttestation) => Promise<boolean>
  revokeDelegation: (r: { grantId: string, pub: string, ts: number, sig?: string }) => Promise<boolean>
  delegationsFor: (grantee?: string) => SignedDelegation[]
  attestationsFor: (pub?: string) => SignedAttestation[]
  catalogFor: (pub: string) => SignedCatalog | undefined
  wantFor: (pub: string) => SignedWant | undefined
  offersFor: (domain?: string) => SignedOffer[]
  reputationFor: (pub: string) => SignedReputation | undefined
  reputationsFor: (domain?: string) => SignedReputation[]
  prices: (q: { domain?: string, tool?: string }) => { count: number, sats: number[], minSats: number | null, maxSats: number | null, medianSats: number | null, totalSats: number, settlements: SettlementAd[] }
  feed: (q: { since?: number, domains?: string[], kind?: EventKind, limit?: number }) => Promise<PresenceEvent[]>
  announceBeacon: (sb: SignedBeacon) => Promise<boolean>
  peersFor: () => SignedBeacon[]
  announceInterval: (si: SignedInterval) => Promise<boolean>
  intervalsFor: (pub?: string) => SignedInterval[]
  announceEnvelope: (se: SignedEnvelope) => Promise<boolean>
  mailboxFor: (recipient: string) => SignedEnvelope[]
  /** evict expired entries from the live maps — returns how many died */
  sweep: () => number
  bus: EventEmitter
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
  // signed settlement announcements (append — history IS the price feed)
  const settlements: SignedSettlement[] = []
  // latest reputation card per pub
  const reps = new Map<string, SignedReputation>()
  // delegations keyed per grantId; a revocation tombstones the grant
  const delegations = new Map<string, SignedDelegation>()
  const revoked = new Set<string>()
  // latest attestation per pub
  const attestations = new Map<string, SignedAttestation>()
  // live beacon ads per rv pub — the index of indexes
  const beacons = new Map<string, SignedBeacon>()
  // producer interval commitments — the repairable-history anchors
  const intervals: SignedInterval[] = []
  // mailbox — envelopes keyed by messageId, dedup'd
  const envelopes = new Map<string, SignedEnvelope>()
  // the durable event log — seq is the consumer cursor
  const events: PresenceEvent[] = []
  /** push feed for live surfaces (dashboard SSE) — emits each event as it lands */
  const bus = new EventEmitter()
  /** lazily probed — can the tracker resolve headers at all (genesis)? */
  let spvCapable: boolean | undefined
  // net aliases — ttn/testnet/mock/regtest are one lane; main/mainnet/bsv the other
  const netLane = (n?: string) => !n ? undefined : (n === 'main' || n === 'mainnet' || n === 'bsv' ? 'main' : 'test')
  const netEq = (a?: string, b?: string) => netLane(a) === netLane(b)
  // expiry — only explicit exp expires; no implicit TTL (fee is the price of listing)
  const alive = (e: { exp?: number, ts?: number }) => e.exp === undefined || e.exp > Date.now()
  // bstore-style listing fee — exp beyond the free window must be paid for:
  // `pay:{beef}` = a tx paying rate/hour to the rv's listing address
  const fee = opts?.listingFee
  const paidFor = (ts: number, exp: number | undefined, pay?: { beef?: number[] | string }): boolean => {
    if (!fee?.addr || exp === undefined || exp <= ts + (fee.freeWindowMs ?? 3_600_000)) return true // free window
    if (!pay?.beef) return false
    try {
      const bytes = typeof pay.beef === 'string' ? Array.from({ length: pay.beef.length / 2 }, (_, i) => parseInt((pay.beef as string).slice(i * 2, i * 2 + 2), 16)) : pay.beef
      const tx = Transaction.fromBEEF(bytes)
      const want = Math.ceil((exp - ts) / 3_600_000) * (fee.rateSatsPerHour ?? 10)
      return tx.outputs.some(o => (o.satoshis ?? 0) >= want && (o.lockingScript as unknown as { address?: () => string }).address?.() === fee.addr)
    } catch { return false }
  }
  let nextSeq = 0
  const appendEvent = (kind: EventKind, pub: string, domains: string[], payload: unknown) => {
    const e: PresenceEvent = { seq: nextSeq++, kind, pub, domains, ts: Date.now(), payload }
    events.push(e)
    bus.emit('event', e)
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
  const SETTLE_TABLE = 'nektar_ls_settlements'
  const persistSettlement = async (s: SignedSettlement) => {
    if (!kx || !(await kx.schema.hasTable(SETTLE_TABLE))) return
    await kx(SETTLE_TABLE).insert({ pub: s.ad.pub, invoice_id: s.ad.invoiceId, txid: s.ad.txid, tool: s.ad.tool, sats: s.ad.sats, domains: JSON.stringify(s.ad.domains), ts: s.ad.ts, ad: JSON.stringify(s.ad), sig: s.sig })
  }
  const REPS_TABLE = 'nektar_ls_reputation'
  const persistRep = async (sr: SignedReputation) => {
    if (!kx || !(await kx.schema.hasTable(REPS_TABLE))) return
    const row = { pub: sr.card.pub, as_of: sr.card.asOf, card: JSON.stringify(sr.card), sig: sr.sig }
    await kx(REPS_TABLE).insert(row).onConflict('pub').merge(row)
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
      if (!(await kx.schema.hasTable(SETTLE_TABLE))) {
        await kx.schema.createTable(SETTLE_TABLE, t => {
          t.increments('id')
          t.string('pub').notNullable()
          t.string('invoice_id').notNullable()
          t.string('txid').notNullable()
          t.string('tool').notNullable()
          t.integer('sats').notNullable()
          t.string('domains').notNullable()
          t.bigInteger('ts').notNullable()
          t.string('ad').notNullable()
          t.string('sig').notNullable()
        })
      }
      for (const r of await kx(SETTLE_TABLE).select('*')) settlements.push({ ad: JSON.parse(r.ad), sig: r.sig })
      if (!(await kx.schema.hasTable(REPS_TABLE))) {
        await kx.schema.createTable(REPS_TABLE, t => {
          t.string('pub').primary()
          t.bigInteger('as_of').notNullable()
          t.string('card').notNullable()
          t.string('sig').notNullable()
        })
      }
      for (const r of await kx(REPS_TABLE).select('*')) {
        const sr: SignedReputation = { card: JSON.parse(r.card), sig: r.sig }
        const prev = reps.get(r.pub)
        if (!prev || prev.card.asOf < sr.card.asOf) reps.set(r.pub, sr)
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
      if (!paidFor(s.ad.ts, s.ad.exp, (s as unknown as { pay?: { beef?: number[] | string } }).pay)) return false
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
      if (!paidFor(sw.want.ts, sw.want.exp, sw.pay)) return false // exp beyond free window must be paid
      // signed net binding — reject artifacts claiming a different network
      if (opts?.net && ((sw.want.net && !netEq(sw.want.net, opts.net)) || (opts.strictNet && !sw.want.net))) return false
      // SPV binding — claimed tip must match this chain's header at that height.
      // strict: reject unbound only when the tracker can actually verify
      // (genesis probe — a tracker that can't serve block 1 can't verify at all)
      if (opts?.tracker) {
        spvCapable ??= !!(await opts.tracker.headerAtHeight(1).catch(() => undefined))
        if (sw.want.tip || sw.want.tipHeight != null) {
          const h = sw.want.tipHeight != null ? await opts.tracker.headerAtHeight(sw.want.tipHeight).catch(() => undefined) : undefined
          // soft: reject only a proven mismatch; strict: require positive verification
          if (h ? h.hash !== sw.want.tip : opts.strictNet) return false
        } else if (opts.strictNet && spvCapable) return false
      }
      const prev = wants.get(sw.want.pub)
      if (prev && prev.want.ts >= sw.want.ts) return true
      wants.set(sw.want.pub, sw)
      await persistWant(sw)
      appendEvent('want', sw.want.pub, sw.want.domains, sw)
      return true
    },

    announceOffer: async (so: SignedOffer) => {
      if (!verifyOffer(so.offer, so.sig)) return false
      if (!paidFor(so.offer.ts, so.offer.exp, so.pay)) return false
      if (opts?.net && ((so.offer.net && !netEq(so.offer.net, opts.net)) || (opts.strictNet && !so.offer.net))) return false
      if (opts?.tracker) {
        spvCapable ??= !!(await opts.tracker.headerAtHeight(1).catch(() => undefined))
        if (so.offer.tip || so.offer.tipHeight != null) {
          const h = so.offer.tipHeight != null ? await opts.tracker.headerAtHeight(so.offer.tipHeight).catch(() => undefined) : undefined
          if (h ? h.hash !== so.offer.tip : opts.strictNet) return false
        } else if (opts.strictNet && spvCapable) return false
      }
      const k = offerKey(so.offer)
      const prev = offers.get(k)
      if (prev && prev.offer.ts >= so.offer.ts) return true
      offers.set(k, so)
      await persistOffer(so)
      appendEvent('offer', so.offer.pub, so.offer.domains, so)
      return true
    },

    announceSettlement: async (s: SignedSettlement) => {
      if (!verifySettlement(s.ad, s.sig)) return false
      if (settlements.some(x => x.ad.pub === s.ad.pub && x.ad.invoiceId === s.ad.invoiceId)) return true // idempotent
      settlements.push(s)
      await persistSettlement(s)
      appendEvent('settlement', s.ad.pub, s.ad.domains, s)
      return true
    },

    announceReputation: async (sr: SignedReputation) => {
      if (!verifyReputationCard(sr.card, sr.sig)) return false
      const prev = reps.get(sr.card.pub)
      if (prev && prev.card.asOf >= sr.card.asOf) return true
      reps.set(sr.card.pub, sr)
      await persistRep(sr)
      appendEvent('reputation', sr.card.pub, sr.card.entries.map(e => e.domain), sr)
      return true
    },

    announceDelegation: async (sd: SignedDelegation) => {
      if (!verifyDelegation(sd.grant, sd.sig)) return false
      const prev = delegations.get(sd.grant.grantId)
      if (prev && prev.grant.ts >= sd.grant.ts) return true
      delegations.set(sd.grant.grantId, sd)
      appendEvent('delegation', sd.grant.ownerPub, sd.grant.scope.domains ?? [], sd)
      return true
    },
    announceAttestation: async (sa: SignedAttestation) => {
      if (!verifyAttestation(sa.attestation, sa.sig)) return false
      const prev = attestations.get(sa.attestation.pub)
      if (prev && prev.attestation.ts >= sa.attestation.ts) return true
      attestations.set(sa.attestation.pub, sa)
      appendEvent('attestation', sa.attestation.pub, [], sa)
      return true
    },
    // only the grant's owner may tombstone it — revocation kills the grant
    // AND cascades: every descendant grant (transitive parentGrantId) dies
    // with it — a revoked link breaks the whole chain below it
    revokeDelegation: async (r: { grantId: string, pub: string, ts: number, sig?: string }) => {
      const d = delegations.get(r.grantId)
      if (!d || d.grant.ownerPub !== r.pub) return false
      // authenticated revocation — owner-signed; unsigned requests are refused
      if (!r.sig || !verifyRevocation(r, r.sig)) return false
      revoked.add(r.grantId)
      for (const sd of delegations.values()) if (sd.grant.parentGrantId === r.grantId) revoked.add(sd.grant.grantId)
      appendEvent('revocation', r.pub, [], r)
      return true
    },
    delegationsFor: (grantee?: string) => [...delegations.values()].filter(sd => !revoked.has(sd.grant.grantId) && !revoked.has(sd.grant.parentGrantId ?? '') && sd.grant.expiresAt > Date.now()).filter(sd => grantee === undefined || sd.grant.granteePub === grantee),
    attestationsFor: (pub?: string) => [...attestations.values()].filter(sa => pub === undefined || sa.attestation.pub === pub),

    catalogFor: (pub: string) => catalogs.get(pub)?.sc,
    wantFor: (pub: string) => { const w = wants.get(pub); return w && alive(w.want) ? w : undefined },
    offersFor: (domain?: string) => [...offers.values()].filter(so => alive(so.offer)).filter(so => domain === undefined || so.offer.domains.some(d => d.toLowerCase() === domain.toLowerCase())),
    reputationFor: (pub: string) => reps.get(pub),
    reputationsFor: (domain?: string) => [...reps.values()].filter(sr => domain === undefined || sr.card.entries.some(e => e.domain === domain.toLowerCase())),
    // realized-price index: settled sales per (domain, tool) — count + sats
    // distribution. Signed announcements; txids checkable on-chain.
    prices: (q: { domain?: string, tool?: string }) => {
      const dom = q.domain?.toLowerCase()
      const rows = settlements.filter(s =>
        (dom === undefined || s.ad.domains.some(d => d.toLowerCase() === dom)) &&
        (q.tool === undefined || s.ad.tool === q.tool))
      const sats = rows.map(r => r.ad.sats).sort((a, b) => a - b)
      return {
        count: rows.length,
        sats,
        minSats: sats[0] ?? null,
        maxSats: sats.at(-1) ?? null,
        medianSats: sats.length ? sats[Math.floor(sats.length / 2)]! : null,
        totalSats: sats.reduce((a, b) => a + b, 0),
        settlements: rows.map(r => r.ad)
      }
    },

    /** live push feed — 'event' fires per appended PresenceEvent (SSE dashboard) */
    bus,

    // expiry sweep — silence frees the maps; history stays in /events
    announceBeacon: async (sb: SignedBeacon) => {
      if (!verifyBeacon(sb.beacon, sb.sig)) return false
      if (opts?.net && sb.beacon.net && !netEq(sb.beacon.net, opts.net)) return false
      const prev = beacons.get(sb.beacon.pub)
      if (prev && prev.beacon.ts >= sb.beacon.ts) return true
      beacons.set(sb.beacon.pub, sb)
      appendEvent('beacon', sb.beacon.pub, sb.beacon.topics, sb)
      return true
    },
    peersFor: () => [...beacons.values()].filter(sb => alive(sb.beacon)),
    announceInterval: async (si: SignedInterval) => {
      if (!verifyInterval(si.commit, si.sig)) return false
      if (opts?.net && (si.commit as any).net && !netEq((si.commit as any).net, opts.net)) return false
      intervals.push(si)
      appendEvent('interval', si.commit.pub, [si.commit.streamId], si)
      return true
    },
    intervalsFor: (pub?: string) => intervals.filter(si => pub === undefined || si.commit.pub === pub),
    announceEnvelope: async (se: SignedEnvelope) => {
      if (!verifyEnvelope(se.envelope, se.sig)) return false
      if (envelopes.has(se.envelope.messageId)) return true // dedup — same message, same slot
      envelopes.set(se.envelope.messageId, se)
      appendEvent('envelope', se.envelope.sender, [], se)
      return true
    },
    mailboxFor: (recipient: string) => [...envelopes.values()].filter(se => se.envelope.recipient === recipient && alive({ ts: se.envelope.createdAt, exp: se.envelope.expiresAt })),

    sweep: () => {
      let n = 0
      for (const [k, v] of envelopes) if (!alive({ ts: v.envelope.createdAt, exp: v.envelope.expiresAt })) { envelopes.delete(k); n++ }
      for (const [k, v] of beacons) if (!alive(v.beacon)) { beacons.delete(k); n++ }
      for (const [k, v] of wants) if (!alive(v.want)) { wants.delete(k); n++ }
      for (const [k, v] of offers) if (!alive(v.offer)) { offers.delete(k); n++ }
      for (const [k, v] of ads) if (!alive(v.ad)) { ads.delete(k); n++ }
      for (const [k, v] of catalogs) if (!alive(v.sc.catalog)) { catalogs.delete(k); n++ }
      return n
    },

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
      const live = (s: Stored) => alive(s.ad)
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

// ── attestation layer ────────────────────────────────────────────────────────
// signed delegation — an owner grants bounded authority to a grantee (agent X
// acts under owner Y's bounded permission: scope, spendCap, ttl). Wright's
// liability primitive as a checkable object.
export interface DelegationAd {
  grantId: string // uuid/hex id of the grant
  ownerPub: string // the owner granting authority
  granteePub: string // the agent receiving bounded authority
  scope: { tools?: string[], domains?: string[] } // bounded permissions
  spendCapSats: number // max cumulative spend under this grant
  /** F3 grant-v2 fields — per-call cap + explicit lifetime cap; both enforced */
  perActionCapSats?: number
  totalCapSats?: number
  expiresAt: number // unix ms — the grant dies on its own
  ts: number
  /** nested chain: the grantee re-delegates a SUBSET of its own grant —
   *  child's ownerPub == parent's granteePub, scope ⊆ parent, cap ≤ parent,
   *  expiry ≤ parent. A chain is only as strong as its tightest link. */
  parentGrantId?: string
}
export interface SignedDelegation { grant: DelegationAd, sig: string }

const delegationCanonical = (g: DelegationAd) => `${g.grantId}|${g.ownerPub}|${g.granteePub}|${[...(g.scope.tools ?? [])].sort().join(',')}|${[...(g.scope.domains ?? [])].sort().join(',')}|${g.spendCapSats}|${g.expiresAt}|${g.ts}|${g.parentGrantId ?? ''}|${g.perActionCapSats ?? ''}|${g.totalCapSats ?? ''}`
const delegationDigest = (g: DelegationAd) => sha256(utf8(delegationCanonical(g)))

export function signDelegation (key: PrivateKey, fields: Omit<DelegationAd, 'ownerPub'>): SignedDelegation {
  const grant: DelegationAd = {
    ...fields,
    perActionCapSats: fields.perActionCapSats ?? fields.spendCapSats,
    totalCapSats: fields.totalCapSats ?? fields.spendCapSats,
    scope: { tools: fields.scope.tools ? [...fields.scope.tools].sort() : undefined, domains: fields.scope.domains ? [...fields.scope.domains].sort() : undefined },
    ownerPub: key.toPublicKey().toString()
  }
  return { grant, sig: toHex(Uint8Array.from(key.sign(Array.from(delegationDigest(grant))).toDER() as number[])) }
}

/** a child grant must be a strict subset of its parent: the grantee
 *  re-delegates only what it holds — tools/domains ⊆ parent's (an empty
 *  parent field means unrestricted), cap/expiry ≤ parent's, and the
 *  child's ownerPub must be the parent's granteePub. */
export function delegationSubset (child: DelegationAd, parent: DelegationAd): boolean {
  if (child.ownerPub !== parent.granteePub) return false
  if (child.parentGrantId !== parent.grantId) return false
  const sub = (a?: string[], b?: string[]) => b === undefined || a === undefined || a.every(x => b.includes(x))
  if (!sub(child.scope?.tools, parent.scope?.tools) || !sub(child.scope?.domains, parent.scope?.domains)) return false
  if (child.spendCapSats > parent.spendCapSats) return false
  if (child.expiresAt > parent.expiresAt) return false
  return true
}

export function verifyDelegation (grant: DelegationAd, sigHex: string): boolean {
  if (grant.perActionCapSats === undefined || grant.totalCapSats === undefined) return false // v2: explicit caps required
  try {
    if (typeof grant?.grantId !== 'string' || grant.grantId.length === 0 || grant.grantId.length > 128) return false
    for (const p of [grant.ownerPub, grant.granteePub]) if (typeof p !== 'string' || !/^[0-9a-f]{66}$/i.test(p)) return false
    if (!Number.isSafeInteger(grant.spendCapSats) || grant.spendCapSats < 0) return false
    if (!Number.isSafeInteger(grant.expiresAt) || grant.expiresAt <= Date.now()) return false
    if (!Number.isSafeInteger(grant.ts) || grant.ts <= 0) return false
    for (const arr of [grant.scope?.tools, grant.scope?.domains]) if (arr !== undefined && (!Array.isArray(arr) || arr.length > 64 || !arr.every(s => typeof s === 'string' && s.length > 0 && s.length <= 64))) return false
    return PublicKey.fromString(grant.ownerPub).verify(Array.from(delegationDigest(grant)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// signed attestation — "model M answered" declared + signed (OpenWALDO stance:
// attributable facts + the limits of what's verified — never a truth claim)
export interface AttestationAd {
  pub: string // the attesting agent
  modelId: string // e.g. 'llama3.1-70b' — declares the model
  weightsHash?: string // open-weight commitment — recomputable when public
  provider?: string // where the model runs
  claims?: string[] // what the attestation asserts
  ts: number
}
export interface SignedAttestation { attestation: AttestationAd, sig: string }

const attestationCanonical = (a: AttestationAd) => `${a.modelId}|${a.weightsHash ?? ''}|${a.provider ?? ''}|${[...(a.claims ?? [])].sort().join(',')}|${a.ts}`
const attestationDigest = (a: AttestationAd) => sha256(utf8(attestationCanonical(a)))

export function signAttestation (key: PrivateKey, fields: Omit<AttestationAd, 'pub'>): SignedAttestation {
  const attestation: AttestationAd = { ...fields, pub: key.toPublicKey().toString() }
  return { attestation, sig: toHex(Uint8Array.from(key.sign(Array.from(attestationDigest(attestation))).toDER() as number[])) }
}

export function verifyAttestation (attestation: AttestationAd, sigHex: string): boolean {
  try {
    if (typeof attestation?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(attestation.pub)) return false
    if (typeof attestation.modelId !== 'string' || attestation.modelId.length === 0 || attestation.modelId.length > 128) return false
    if (attestation.weightsHash !== undefined && (typeof attestation.weightsHash !== 'string' || !/^[0-9a-f]{40,128}$/i.test(attestation.weightsHash))) return false
    if (attestation.provider !== undefined && (typeof attestation.provider !== 'string' || attestation.provider.length > 256)) return false
    if (attestation.claims !== undefined && (!Array.isArray(attestation.claims) || attestation.claims.length > 64 || !attestation.claims.every(c => typeof c === 'string' && c.length <= 512))) return false
    if (!Number.isSafeInteger(attestation.ts) || attestation.ts <= 0) return false
    return PublicKey.fromString(attestation.pub).verify(Array.from(attestationDigest(attestation)), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}
