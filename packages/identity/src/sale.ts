// GOAL-v7 V3 host layer — atomic paid disclosure (DisclosureSale covenant).
// The covenant itself (contracts/src/DisclosureSale.runar.ts) enforces:
// `buy` reveals `wrapped` in the same tx that pays the seller, bound by
// sha256(wrapped) == hWrapped. This module is the OFF-CHAIN side: wrapping
// the value to the buyer (BRC-42 ECDH — on-chain exposure is safe), parsing
// the revealed `wrapped` out of the buy tx, unwrapping and verifying it.
// Tx building/broadcast stays in @mamem/anchor's DisclosureSaleHost — this
// package must not import @mamem/anchor (anchor already depends on us).
import { PrivateKey, Transaction } from '@bsv/sdk'
import { sha256, concat, toHex, fromHex, type Bytes } from '@mamem/core'
import { wrapFor, unwrapFor, type WrappedKey } from './disclosure.js'

/** fixed wrap context — binds the ECDH pad to the sale channel. */
export const SALE_CTX = 'mamem:sale:v1'

// --- seller side ----------------------------------------------------------

/** Wrap `value` to the buyer via BRC-42; the blob (salt‖nonce‖ct) is the
 *  on-chain `wrapped` arg, hWrapped commits the covenant to it. */
export function wrapSaleValue (sellerKey: PrivateKey, buyerPub: string, value: Bytes): { wrapped: Bytes, hWrapped: string } {
  const w = wrapFor(sellerKey, buyerPub, value, SALE_CTX)
  const wrapped = concat(w.salt, w.nonce, w.ct)
  return { wrapped, hWrapped: toHex(sha256(wrapped)) }
}

/** Public (non-encrypted) sale: `wrapped` is the value itself — e.g. a
 *  serialized {field,salt,path} triple for paid public disclosure. */
export function plainSaleValue (value: Bytes): { wrapped: Bytes, hWrapped: string } {
  return { wrapped: value, hWrapped: toHex(sha256(value)) }
}

export interface SaleOffer {
  /** fields to construct a DisclosureSaleHost with (sellerPkh = seller key's) */
  cfg: { sellerPkh: string, price: bigint, hWrapped: string, refundAfter: bigint, maxFee: bigint }
  /** hex the buyer must supply as `wrapped` in buy() */
  wrapped: string
}

/** Build a sale listing: cfg for the covenant + the committed `wrapped`. */
export function offerDisclosure (params: {
  sellerKey: PrivateKey
  value: Bytes
  price: bigint
  refundAfter: bigint
  maxFee: bigint
  /** recipient pubkey — omit for public disclosure */
  buyerPub?: string
}): SaleOffer {
  const { wrapped, hWrapped } = params.buyerPub
    ? wrapSaleValue(params.sellerKey, params.buyerPub, params.value)
    : plainSaleValue(params.value)
  const sellerPkh = toHex(new Uint8Array(params.sellerKey.toPublicKey().toHash() as number[]))
  return {
    cfg: { sellerPkh, price: params.price, hWrapped, refundAfter: params.refundAfter, maxFee: params.maxFee },
    wrapped: toHex(wrapped)
  }
}

// --- buyer side -----------------------------------------------------------

/** Minimal push-opcode reader (script pushes only — a parser, not an author). */
export function parsePushes (scriptHex: string): Bytes[] {
  const b = fromHex(scriptHex)
  const out: Bytes[] = []
  for (let i = 0; i < b.length;) {
    const op = b[i++]!
    let len = 0
    if (op <= 0x4b) len = op
    else if (op === 0x4c) len = b[i++]!
    else if (op === 0x4d) { len = b[i]! | (b[i + 1]! << 8); i += 2 }
    else if (op === 0) { out.push(new Uint8Array(0)); continue }
    else if (op === 0x4e) { len = b[i]! | (b[i + 1]! << 8) | (b[i + 2]! << 16) | (b[i + 3]! << 24); i += 4 }
    else throw new Error(`parsePushes: non-push opcode 0x${op.toString(16)} at ${i - 1}`)
    out.push(b.slice(i, i + len)); i += len
  }
  return out
}

/** Extract the buy() args from a spending tx's input-0 unlocking script.
 *  Frame: opSig ‖ wrapped ‖ buyerPkh ‖ changeSats ‖ preimage ‖ 00-selector. */
export function parseBuyArgs (unlockingHex: string): { wrapped: Bytes, buyerPkh: Bytes } {
  const p = parsePushes(unlockingHex)
  if (p.length < 5 || !p[1] || !p[2]) throw new Error(`parseBuyArgs: expected ≥5 pushes, got ${p.length}`)
  return { wrapped: p[1]!, buyerPkh: p[2]! }
}

/** Fetch `wrapped` from a buy transaction (input 0 must be the listing spend). */
export function parseBuyWrapped (tx: Transaction): Bytes {
  const unlock = tx.inputs[0]?.unlockingScript
  if (!unlock) throw new Error('parseBuyWrapped: input 0 has no unlocking script')
  return parseBuyArgs(unlock.toHex()).wrapped
}

export interface ClaimResult { value: Bytes, mode: 'private' | 'public', wrapped: Bytes }

/** Buyer claim: parse `wrapped` from the buy tx, check sha256 == hWrapped,
 *  unwrap to the buyer's key (private) or return as-is (public listing —
 *  detectable because the blob is NOT a well-formed WrappedKey). */
export function claimDisclosure (tx: Transaction, opts: {
  hWrapped: string
  buyerKey?: PrivateKey
  sellerPub?: string
}): ClaimResult {
  const wrapped = parseBuyWrapped(tx)
  if (toHex(sha256(wrapped)) !== opts.hWrapped) throw new Error('claimDisclosure: wrapped does not match the committed hWrapped')
  if (opts.buyerKey && opts.sellerPub) {
    const wk: WrappedKey = { salt: wrapped.subarray(0, 32), nonce: wrapped.subarray(32, 44), ct: wrapped.subarray(44) }
    try {
      // plaintext len = ct − 16B GCM tag (wrapFor defaults to 32B keys)
      return { value: unwrapFor(opts.buyerKey, opts.sellerPub, wk, SALE_CTX, wk.ct.length - 16), mode: 'private', wrapped }
    } catch { /* not a private wrap — fall through to public */ }
  }
  return { value: wrapped, mode: 'public', wrapped }
}
