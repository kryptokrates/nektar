// GOAL-v7 V3 host layer — atomic paid disclosure (DisclosureSale covenant).
// The covenant itself (contracts/src/DisclosureSale.runar.ts) enforces:
// `buy` reveals `wrapped` in the same tx that pays the seller, bound by
// sha256(wrapped) == hWrapped. This module is the OFF-CHAIN side: wrapping
// the value to the buyer (BRC-42 ECDH — on-chain exposure is safe), parsing
// the revealed `wrapped` out of the buy tx, unwrapping and verifying it.
// Tx building/broadcast stays in @nektar/anchor's DisclosureSaleHost — this
// package must not import @nektar/anchor (anchor already depends on us).
import { PrivateKey, PublicKey, Signature, Transaction, Hash } from '@bsv/sdk'
import { sha256, concat, toHex, fromHex, type Bytes } from '@nektar/core'
import { wrapFor, unwrapFor, type WrappedKey } from './disclosure.js'

/** fixed wrap context — binds the ECDH pad to the sale channel. */
export const SALE_CTX = 'nektar:sale:v1'

// --- seller side ----------------------------------------------------------

/** Wrap `value` to the buyer via BRC-42; the blob (salt‖nonce‖ct) is the
 *  on-chain `wrapped` arg, hWrapped commits the covenant to it. */
export function wrapSaleValue(sellerKey: PrivateKey, buyerPub: string, value: Bytes): { wrapped: Bytes; hWrapped: string } {
  const w = wrapFor(sellerKey, buyerPub, value, SALE_CTX)
  const wrapped = concat(w.salt, w.nonce, w.ct)
  return { wrapped, hWrapped: toHex(sha256(wrapped)) }
}

/** Public (non-encrypted) sale: `wrapped` is the value itself — e.g. a
 *  serialized {field,salt,path} triple for paid public disclosure. */
export function plainSaleValue(value: Bytes): { wrapped: Bytes; hWrapped: string } {
  return { wrapped: value, hWrapped: toHex(sha256(value)) }
}

export interface SaleOffer {
  /** fields to construct a DisclosureSaleHost with (sellerPkh = seller key's) */
  cfg: { sellerPkh: string; price: bigint; hWrapped: string; refundAfter: bigint; maxFee: bigint }
  /** hex the seller supplies as `wrapped` in buy() */
  wrapped: string
  /** seller pubkey (the buyer verifies offerSig against it) */
  sellerPub: string
  /** §19 horizon: block height at which this offer lapses — the signed offer
   *  cannot be funded once the chain tip reaches it. 0/absent = no expiry. */
  expiresAt?: bigint
  /** §16/17 delegation signature: σ_seller = Sign(H(cfg‖commitment‖buyerPub‖expiresAt))
   *  — a non-repudiable "this seller offered this commitment to this buyer"
   *  attestation the buyer checks BEFORE funding the escrow. */
  offerSig: string
}

/** canonical bytes the offer signature covers — every field a MITM could swap. */
function offerDigest(
  cfg: { sellerPkh: string; price: bigint | number; hWrapped: string; refundAfter: bigint | number; maxFee: bigint | number },
  buyerPub: string,
  leaf: string,
  expiresAt: bigint | number = 0
): Bytes {
  const u64 = (n: bigint | number) => {
    const b = Buffer.alloc(8)
    b.writeBigUInt64LE(BigInt(n))
    return new Uint8Array(b)
  }
  return sha256(
    concat(
      fromHex(cfg.sellerPkh),
      fromHex(cfg.hWrapped),
      u64(cfg.price),
      u64(cfg.refundAfter),
      u64(cfg.maxFee),
      fromHex(buyerPub || '00'),
      fromHex(leaf || '00'.repeat(32)),
      u64(expiresAt)
    )
  )
}

/** Build a sale listing: cfg for the covenant + the committed `wrapped` +
 *  the seller's signed offer attestation. `leaf` binds the offer to a
 *  specific anchored memory record when set (sale op=offerMemory). */
export function offerDisclosure(params: {
  sellerKey: PrivateKey
  value: Bytes
  price: bigint
  refundAfter: bigint
  maxFee: bigint
  /** recipient pubkey — omit for public disclosure */
  buyerPub?: string
  /** anchored leaf this payload commits to (empty = unbound sale) */
  leaf?: string
  /** §19 horizon: block height at which the offer lapses (0 = no expiry) */
  expiresAt?: bigint
}): SaleOffer {
  const { wrapped, hWrapped } = params.buyerPub ? wrapSaleValue(params.sellerKey, params.buyerPub, params.value) : plainSaleValue(params.value)
  const sellerPub = params.sellerKey.toPublicKey().toString()
  const sellerPkh = toHex(new Uint8Array(params.sellerKey.toPublicKey().toHash() as number[]))
  const cfg = { sellerPkh, price: params.price, hWrapped, refundAfter: params.refundAfter, maxFee: params.maxFee }
  const expiresAt = params.expiresAt ?? 0n
  const digest = offerDigest(cfg, params.buyerPub ?? '', params.leaf ?? '', expiresAt)
  const offerSig = params.sellerKey.sign(Array.from(Hash.sha256(Buffer.from(digest)))).toDER('hex') as string
  return { cfg, wrapped: toHex(wrapped), sellerPub, expiresAt, offerSig }
}

/** Buyer-side: verify the seller's offer attestation before funding the
 *  escrow — refuses MITM-substituted cfgs AND tampered expiry horizons. */
export function verifyOfferSig(
  offer: { cfg: SaleOffer['cfg']; sellerPub: string; offerSig: string; expiresAt?: bigint | number },
  buyerPub = '',
  leaf = ''
): boolean {
  try {
    const digest = offerDigest(offer.cfg, buyerPub, leaf, offer.expiresAt ?? 0)
    const sig = Signature.fromDER(Array.from(Buffer.from(offer.offerSig, 'hex')))
    return PublicKey.fromString(offer.sellerPub).verify(Array.from(Hash.sha256(Buffer.from(digest))), sig)
  } catch {
    return false
  }
}

export type SaleQuote = Omit<SaleOffer, 'wrapped'>

export function publicSaleQuote(offer: SaleOffer): SaleQuote {
  const q: SaleQuote = {
    cfg: { sellerPkh: offer.cfg.sellerPkh, price: offer.cfg.price, hWrapped: offer.cfg.hWrapped, refundAfter: offer.cfg.refundAfter, maxFee: offer.cfg.maxFee },
    sellerPub: offer.sellerPub,
    offerSig: offer.offerSig
  }
  if (offer.expiresAt !== undefined) q.expiresAt = offer.expiresAt
  return q
}

export function saleQuoteWire(q: SaleQuote): Record<string, unknown> {
  const n = (v: bigint | number | string | undefined): number | string | undefined =>
    typeof v === 'bigint' ? (v <= BigInt(Number.MAX_SAFE_INTEGER) ? Number(v) : v.toString()) : v
  const out: Record<string, unknown> = {
    cfg: { sellerPkh: q.cfg.sellerPkh, price: n(q.cfg.price), hWrapped: q.cfg.hWrapped, refundAfter: n(q.cfg.refundAfter), maxFee: n(q.cfg.maxFee) },
    sellerPub: q.sellerPub,
    offerSig: q.offerSig
  }
  if (q.expiresAt !== undefined) out.expiresAt = n(q.expiresAt)
  return out
}

// --- buyer side -----------------------------------------------------------

/** Minimal push-opcode reader (script pushes only — a parser, not an author). */
export function parsePushes(scriptHex: string): Bytes[] {
  const b = fromHex(scriptHex)
  const out: Bytes[] = []
  for (let i = 0; i < b.length;) {
    const op = b[i++]!
    let len = 0
    if (op <= 0x4b) len = op
    else if (op === 0x4c) len = b[i++]!
    else if (op === 0x4d) {
      len = b[i]! | (b[i + 1]! << 8)
      i += 2
    } else if (op === 0) {
      out.push(new Uint8Array(0))
      continue
    } else if (op === 0x4e) {
      len = b[i]! | (b[i + 1]! << 8) | (b[i + 2]! << 16) | (b[i + 3]! << 24)
      i += 4
    } else if (op === 0x4f) {
      out.push(Uint8Array.of(0x81))
      continue
    } else if (op >= 0x51 && op <= 0x60) {
      out.push(Uint8Array.of(op - 0x50))
      continue
    } else throw new Error(`parsePushes: non-push opcode 0x${op.toString(16)} at ${i - 1}`)
    out.push(b.slice(i, i + len))
    i += len
  }
  return out
}

/** Extract the buy() args from a spending tx's input-0 unlocking script.
 *  Frame: opSig ‖ wrapped ‖ changeSats ‖ preimage ‖ 00-selector —
 *  the buyer/depositor pkh is pinned in the escrow script, not the args. */
export function parseBuyArgs(unlockingHex: string): { wrapped: Bytes; changeSats: Bytes } {
  const p = parsePushes(unlockingHex)
  if (p.length < 5 || !p[1] || !p[2]) throw new Error(`parseBuyArgs: expected ≥5 pushes, got ${p.length}`)
  return { wrapped: p[1]!, changeSats: p[2]! }
}

/** Fetch `wrapped` from a buy transaction (input 0 must be the listing spend). */
export function parseBuyWrapped(tx: Transaction): Bytes {
  const unlock = tx.inputs[0]?.unlockingScript
  if (!unlock) throw new Error('parseBuyWrapped: input 0 has no unlocking script')
  return parseBuyArgs(unlock.toHex()).wrapped
}

// --- key-sale payload (encrypted records) -----------------------------------
// Paper-native shape: the anchored fragment is f_i = {c_i, H(d_i), π_i}.
// Selling an encrypted record means handing over the FRAGMENT KEY (the
// §7 decryption capability) plus the ciphertext + provenance — the buyer
// recomputes computeLeaf({d,p,hdp,E}) against the anchored leaf.
import { serializeProvenance, parseProvenance } from '@nektar/core'
import { decryptFragment } from '@nektar/crypto'

/** encode {fragmentKey‖E‖serializeProvenance(p)} with u32le length frames. */
export function encodeKeySale(fragmentKey: Bytes, E: Bytes, p: import('@nektar/core').Provenance): Bytes {
  const field = (b: Bytes) => {
    const l = Buffer.alloc(4)
    l.writeUInt32LE(b.length)
    return concat(new Uint8Array(l), b)
  }
  return concat(field(fragmentKey), field(E), field(serializeProvenance(p)))
}

/** decode a key-sale payload back into its parts. */
export function decodeKeySale(b: Bytes): { fragmentKey: Bytes; E: Bytes; p: import('@nektar/core').Provenance } {
  const take = (off: number) => {
    if (off + 4 > b.length) throw new Error('keySale: truncated')
    const len = (b[off]! | (b[off + 1]! << 8) | (b[off + 2]! << 16) | (b[off + 3]! << 24)) >>> 0
    if (off + 4 + len > b.length) throw new Error('keySale: truncated')
    return { v: b.slice(off + 4, off + 4 + len), next: off + 4 + len }
  }
  const k = take(0)
  const e = take(k.next)
  const p = take(e.next)
  if (p.next !== b.length) throw new Error('keySale: trailing bytes')
  if (k.v.length !== 32) throw new Error('keySale: fragmentKey must be 32B')
  return { fragmentKey: k.v, E: e.v, p: parseProvenance(p.v) }
}

/** Buyer-side: decrypt a key-sale payload → the fragment. E carries
 *  [aadLen‖aad] inline (see memory.ts encrypt path), so no context is
 *  needed — returns {d, p, E} for the caller's leaf/provenance checks. */
export function claimKeySale(payload: Bytes): { d: Bytes; p: import('@nektar/core').Provenance; E: Bytes } {
  const { fragmentKey, E, p } = decodeKeySale(payload)
  const aadLen = (E[0]! << 8) | E[1]!
  const aad = E.slice(2, 2 + aadLen)
  const ciphertext = E.slice(2 + aadLen)
  const d = decryptFragment(fragmentKey, { ciphertext, aad, keyID: '' })
  return { d, p, E }
}

export interface ClaimResult {
  value: Bytes
  mode: 'private' | 'public'
  wrapped: Bytes
}

/** Buyer claim: parse `wrapped` from the buy tx, check sha256 == hWrapped,
 *  unwrap to the buyer's key (private) or return as-is (public listing —
 *  detectable because the blob is NOT a well-formed WrappedKey). */
export function claimDisclosure(
  tx: Transaction,
  opts: {
    hWrapped: string
    buyerKey?: PrivateKey
    sellerPub?: string
  }
): ClaimResult {
  const wrapped = parseBuyWrapped(tx)
  if (toHex(sha256(wrapped)) !== opts.hWrapped) throw new Error('claimDisclosure: wrapped does not match the committed hWrapped')
  if (opts.buyerKey && opts.sellerPub) {
    const wk: WrappedKey = { salt: wrapped.subarray(0, 32), nonce: wrapped.subarray(32, 44), ct: wrapped.subarray(44) }
    try {
      // plaintext len = ct − 16B GCM tag (wrapFor defaults to 32B keys)
      return { value: unwrapFor(opts.buyerKey, opts.sellerPub, wk, SALE_CTX, wk.ct.length - 16), mode: 'private', wrapped }
    } catch {
      /* not a private wrap — fall through to public */
    }
  }
  return { value: wrapped, mode: 'public', wrapped }
}
