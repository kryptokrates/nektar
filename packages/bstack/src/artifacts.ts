// Signed market artifacts — want ads and sale offers, moved verbatim from the
// retired packages/overlay/presence.ts (the only pieces the bstack market still
// needs). Canonical digests and signatures are byte-identical: prefix +
// marketJson(ad) → sha256 → DER sig. The `pay`/listing-fee fields and the
// output-script encoders had no remaining users and are gone.
import { PublicKey, Signature } from '@bsv/sdk'
import { sha256, utf8, toHex, toSigner, signerPubHex, type SignerLike } from '@nektar/core'
import { randomUUID } from 'node:crypto'

/** deterministic JSON: BigInt → decimal string, object keys sorted recursively, array order kept */
export const marketJson = (v: unknown): string =>
  JSON.stringify(v, (_k, x) =>
    typeof x === 'bigint'
      ? x.toString()
      : x !== null && typeof x === 'object' && !Array.isArray(x)
        ? Object.fromEntries(
            Object.keys(x as object)
              .sort()
              .map((k) => [k, (x as Record<string, unknown>)[k]])
          )
        : x
  )

const ARTIFACT_ID_RE = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/
const HASH64_RE = /^[0-9a-f]{64}$/i
const numLike = (v: unknown): boolean =>
  typeof v === 'bigint' || (typeof v === 'number' && Number.isSafeInteger(v) && v >= 0) || (typeof v === 'string' && /^[0-9]{1,20}$/.test(v))

// shared artifact machinery — canonical → sha256 → DER sign | verify
const sigFor = async <T>(key: SignerLike, obj: T, canonical: (t: T) => string): Promise<string> =>
  toHex(Uint8Array.from((await toSigner(key).sign(Array.from(sha256(utf8(canonical(obj)))))).toDER() as number[]))
const verifyDigest = (pubHex: string, digest: Uint8Array, sigHex: string): boolean => {
  try {
    return PublicKey.fromString(pubHex).verify(Array.from(digest), Signature.fromDER(sigHex, 'hex'))
  } catch {
    return false
  }
}
const DOMAIN_RE = /^[a-z0-9][a-z0-9._-]{0,47}$/

// ── want ads — the demand side: a buyer's signed "I need domain X" ──────
export interface WantAd {
  pub: string // buyer identity key
  wantId: string
  revision: number
  status: 'open' | 'canceled'
  taskHash: string
  previousHash?: string
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
  /** where this agent's blogs deal journal lives (ls_log stream id). Optional —
   *  absent changes nothing: the canonical digest covers only present fields. */
  journal?: { stream: string }
}
export interface SignedWant {
  want: WantAd
  sig: string
}

const WANT_PREFIX = 'nektar:market:want:1\n'
const wantCanonical = (w: WantAd) => `${WANT_PREFIX}${marketJson(w)}`
const wantDigest = (w: WantAd) => sha256(utf8(wantCanonical(w)))

export const wantTaskDigest = (fields: { domains: string[]; question?: string; maxSats?: number }): string =>
  toHex(Uint8Array.from(sha256(utf8(marketJson({ domains: [...fields.domains].sort(), maxSats: fields.maxSats ?? null, question: fields.question ?? '' })))))

// allowExpired: verify a signed historical artifact (evidence) — liveness is a
// read-path concern, not a property of the signature
const JOURNAL_RE = /^[a-z0-9][a-z0-9-]{0,31}_[a-z0-9]{10}$/i
const journalError = (j: unknown): string | null => {
  if (j === undefined) return null
  const s = (j as { stream?: unknown }).stream
  return typeof s === 'string' && JOURNAL_RE.test(s) ? null : 'bad journal'
}

// `now` is injectable so signers/readers on one clock domain (automation's
// engine clock) agree — signers pass the artifact's own ts
const wantShapeError = (w: WantAd, allowExpired = false, now = Date.now()): string | null => {
  if (typeof w?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(w.pub)) return 'bad pub'
  if (typeof w?.wantId !== 'string' || !ARTIFACT_ID_RE.test(w.wantId)) return 'bad wantId'
  if (!Number.isSafeInteger(w.revision) || w.revision < 1) return 'bad revision'
  if (w.status !== 'open' && w.status !== 'canceled') return 'bad status'
  if (typeof w.taskHash !== 'string' || !HASH64_RE.test(w.taskHash)) return 'bad taskHash'
  if (w.previousHash !== undefined && (typeof w.previousHash !== 'string' || !HASH64_RE.test(w.previousHash))) return 'bad previousHash'
  if (w.revision > 1 && w.previousHash === undefined) return 'revision>1 requires previousHash'
  if (!Array.isArray(w.domains) || w.domains.length === 0 || w.domains.length > 32 || !w.domains.every((d) => typeof d === 'string' && DOMAIN_RE.test(d)))
    return 'bad domains'
  if (w.question !== undefined && (typeof w.question !== 'string' || w.question.length > 560)) return 'bad question'
  if (w.maxSats !== undefined && (!Number.isSafeInteger(w.maxSats) || w.maxSats < 0)) return 'bad maxSats'
  if (w.endpoint !== undefined && (typeof w.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(w.endpoint))) return 'bad endpoint'
  if (!Number.isSafeInteger(w.ts) || w.ts <= 0) return 'bad ts'
  if (w.exp !== undefined && (!Number.isSafeInteger(w.exp) || (!allowExpired && w.exp <= now))) return 'expired'
  if (journalError(w.journal) !== null) return 'bad journal'
  return null
}

// per-producer stream state — signers auto-stamp streamSeq/prevHash; the
// stream commits are what a reader detects gaps in
const streamState = new Map<string, { seq: number; hash: string }>()
const GENESIS_HASH = '0'.repeat(64)
export function streamRef(pub: string, kind: string, prevHash?: string): { streamSeq: number; prevHash: string } {
  const k = `${pub}:${kind}`
  const s = streamState.get(k) ?? { seq: 0, hash: GENESIS_HASH }
  streamState.set(k, { seq: s.seq + 1, hash: prevHash ?? s.hash })
  return { streamSeq: s.seq + 1, prevHash: s.hash }
}
export function streamCommit(pub: string, kind: string, hash: string): void {
  const s = streamState.get(`${pub}:${kind}`)
  if (s) s.hash = hash
}

export type WantInput = Omit<WantAd, 'pub' | 'wantId' | 'revision' | 'status' | 'taskHash' | 'previousHash'> &
  Partial<Pick<WantAd, 'wantId' | 'revision' | 'status' | 'taskHash' | 'previousHash'>>

export async function signWant(key: SignerLike, fields: WantInput): Promise<SignedWant> {
  const pub = await signerPubHex(key)
  const want: WantAd = {
    ...fields,
    wantId: fields.wantId ?? randomUUID(),
    revision: fields.revision ?? 1,
    status: fields.status ?? 'open',
    taskHash: fields.taskHash ?? wantTaskDigest(fields),
    domains: [...fields.domains].sort(),
    pub
  }
  // no clock check at signing: expiry is a reader-side policy against the
  // reader's clock (allowExpired) — a signed-but-DOA artifact is refused by
  // hosts and engine alike; signers needn't second-guess other clocks
  const err = wantShapeError(want, true)
  if (err !== null) throw new Error(`signWant: ${err}`)
  const sr =
    fields.streamSeq !== undefined && fields.prevHash !== undefined ? { streamSeq: fields.streamSeq, prevHash: fields.prevHash } : streamRef(pub, 'want')
  Object.assign(want, sr)
  streamCommit(pub, 'want', toHex(Uint8Array.from(wantDigest(want))))
  return { want, sig: await sigFor(key, want, wantCanonical) }
}

export function verifyWant(want: WantAd, sigHex: string, opts?: { allowExpired?: boolean; now?: number }): boolean {
  if (wantShapeError(want, opts?.allowExpired ?? false, opts?.now ?? Date.now()) !== null) return false
  if (want.streamSeq === undefined || want.prevHash === undefined) return false // v2: stream fields required
  try {
    return verifyDigest(want.pub, wantDigest(want), sigHex)
  } catch {
    return false
  }
}

// ── offers — the enumerable shelf: a signed per-record sale listing ─────
// The leaf/hdp/batchR commitment lets a buyer verify inclusion BEFORE
// funding (the same checks sale#escrow runs).
export interface OfferAd {
  pub: string // seller identity key
  offerId: string
  revision: number
  previousHash?: string
  wantId?: string
  taskHash?: string
  wantRevision?: number
  buyerPub?: string
  shard: string // label the record for sale lives on
  seq: number // the record for sale
  shardId?: string // on-chain shard id when known
  leaf?: string // leaf commitment hex (verify-before-pay handle)
  hdp?: string // record identity hash hex
  batchR?: string // anchored batch root the leaf path reaches
  priceSats: number
  domains: string[] // from the shard's manifest — feed filtering
  replyTo?: string // pub of the want this offer answers — demand-meets-supply matching
  offer?: Record<string, unknown> // the public DisclosureSale quote (cfg/sellerPub/offerSig/expiresAt) — never `wrapped`
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
  /** where this agent's blogs deal journal lives (ls_log stream id). Optional —
   *  absent changes nothing: the canonical digest covers only present fields. */
  journal?: { stream: string }
}
export interface SignedOffer {
  offer: OfferAd
  sig: string
}

const OFFER_PREFIX = 'nektar:market:offer:1\n'
const offerCanonical = (o: OfferAd) => `${OFFER_PREFIX}${marketJson(o)}`
const offerDigest = (o: OfferAd) => sha256(utf8(offerCanonical(o)))

const QUOTE_CFG_KEYS = new Set(['sellerPkh', 'price', 'hWrapped', 'refundAfter', 'maxFee'])
const QUOTE_KEYS = new Set(['cfg', 'sellerPub', 'expiresAt', 'offerSig'])
export const publicQuoteError = (q: unknown): string | null => {
  if (q === null || typeof q !== 'object' || Array.isArray(q)) return 'quote is not an object'
  const o = q as Record<string, unknown>
  for (const k of Object.keys(o)) if (!QUOTE_KEYS.has(k)) return `unexpected quote field '${k}'`
  const c = o.cfg
  if (c === null || typeof c !== 'object' || Array.isArray(c)) return 'bad cfg'
  const cfg = c as Record<string, unknown>
  for (const k of Object.keys(cfg)) if (!QUOTE_CFG_KEYS.has(k)) return `unexpected cfg field '${k}'`
  for (const k of QUOTE_CFG_KEYS) if (!(k in cfg)) return `missing cfg.${k}`
  if (typeof cfg.sellerPkh !== 'string' || !/^[0-9a-f]{40}$/i.test(cfg.sellerPkh)) return 'bad sellerPkh'
  if (typeof cfg.hWrapped !== 'string' || !HASH64_RE.test(cfg.hWrapped)) return 'bad hWrapped'
  for (const k of ['price', 'refundAfter', 'maxFee'] as const) if (!numLike(cfg[k])) return `bad cfg.${k}`
  if (typeof o.sellerPub !== 'string' || !/^[0-9a-f]{66}$/i.test(o.sellerPub)) return 'bad sellerPub'
  if (typeof o.offerSig !== 'string' || !/^[0-9a-f]{20,220}$/i.test(o.offerSig)) return 'bad offerSig'
  if (o.expiresAt !== undefined && !numLike(o.expiresAt)) return 'bad expiresAt'
  return null
}

const offerShapeError = (o: OfferAd, allowExpired = false, now = Date.now()): string | null => {
  if (typeof o?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(o.pub)) return 'bad pub'
  if (typeof o?.offerId !== 'string' || !ARTIFACT_ID_RE.test(o.offerId)) return 'bad offerId'
  if (!Number.isSafeInteger(o.revision) || o.revision < 1) return 'bad revision'
  if (o.previousHash !== undefined && (typeof o.previousHash !== 'string' || !HASH64_RE.test(o.previousHash))) return 'bad previousHash'
  if (o.revision > 1 && o.previousHash === undefined) return 'revision>1 requires previousHash'
  if (o.wantId !== undefined && (typeof o.wantId !== 'string' || !ARTIFACT_ID_RE.test(o.wantId))) return 'bad wantId'
  if (o.taskHash !== undefined && (typeof o.taskHash !== 'string' || !HASH64_RE.test(o.taskHash))) return 'bad taskHash'
  if (o.wantRevision !== undefined && (!Number.isSafeInteger(o.wantRevision) || o.wantRevision < 1)) return 'bad wantRevision'
  if (o.buyerPub !== undefined && (typeof o.buyerPub !== 'string' || !/^[0-9a-f]{66}$/i.test(o.buyerPub))) return 'bad buyerPub'
  if (typeof o.shard !== 'string' || o.shard.length === 0 || o.shard.length > 64) return 'bad shard'
  if (!Number.isSafeInteger(o.seq) || o.seq < 0) return 'bad seq'
  if (!Number.isSafeInteger(o.priceSats) || o.priceSats < 0) return 'bad priceSats'
  if (!Array.isArray(o.domains) || o.domains.length > 32 || !o.domains.every((d) => typeof d === 'string' && DOMAIN_RE.test(d))) return 'bad domains'
  for (const h of [o.shardId, o.leaf, o.hdp, o.batchR]) if (h !== undefined && (typeof h !== 'string' || !/^[0-9a-f]{40,64}$/i.test(h))) return 'bad hex field'
  if (!Number.isSafeInteger(o.ts) || o.ts <= 0) return 'bad ts'
  if (o.exp !== undefined && (!Number.isSafeInteger(o.exp) || (!allowExpired && o.exp <= now))) return 'expired'
  if (journalError(o.journal) !== null) return 'bad journal'
  if (o.offer !== undefined) {
    const q = publicQuoteError(o.offer)
    if (q !== null) return q
  }
  return null
}

export type OfferInput = Omit<OfferAd, 'pub' | 'offerId' | 'revision' | 'previousHash'> & Partial<Pick<OfferAd, 'offerId' | 'revision' | 'previousHash'>>

export async function signOffer(key: SignerLike, fields: OfferInput): Promise<SignedOffer> {
  const pub = await signerPubHex(key)
  const offer: OfferAd = {
    ...fields,
    offerId: fields.offerId ?? randomUUID(),
    revision: fields.revision ?? 1,
    domains: [...fields.domains].sort(),
    pub
  }
  const err = offerShapeError(offer, true)
  if (err !== null) throw new Error(`signOffer: ${err}`)
  const sr =
    fields.streamSeq !== undefined && fields.prevHash !== undefined ? { streamSeq: fields.streamSeq, prevHash: fields.prevHash } : streamRef(pub, 'offer')
  Object.assign(offer, sr)
  streamCommit(pub, 'offer', toHex(Uint8Array.from(offerDigest(offer))))
  return { offer, sig: await sigFor(key, offer, offerCanonical) }
}

export function verifyOffer(offer: OfferAd, sigHex: string, opts?: { allowExpired?: boolean; now?: number }): boolean {
  if (offerShapeError(offer, opts?.allowExpired ?? false, opts?.now ?? Date.now()) !== null) return false
  if (offer.streamSeq === undefined || offer.prevHash === undefined) return false // v2: stream fields required
  try {
    return verifyDigest(offer.pub, offerDigest(offer), sigHex)
  } catch {
    return false
  }
}
