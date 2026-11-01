// Selective disclosure (DIS-001/003/004/006, KEY-014) — no proof sharding
// (v2-legal-notes). Per-field commitments + scoped disclosure envelopes.
// Field leaf = H(TAG_FIELD ‖ salt ‖ name ‖ value); fragment's field root is
// the committed leaf content. An envelope reveals ONE field + salt + its path
// to ONE verifier, signed by the owner, with an expiry; the value can be
// encrypted to the verifier via BRC-42 ECDH (multi-recipient key wrapping).
import { PrivateKey, PublicKey, Signature, Point, BigNumber } from '@bsv/sdk'
import { sha256, dsha256, concat, utf8, fromHex, type Bytes } from '@mamem/core'
import { merkleRoot, merkleProof, reconstructRoot, type MerkleProof } from '@mamem/core'

export const TAG_FIELD = utf8('mamem:field:v1')
export const TAG_ENV = utf8('mamem:disclosure:v1')

// --- DIS-001: per-field commitments -----------------------------------------
export interface FieldCommitment { name: string, value: Bytes, salt: Bytes }
export function fieldLeaf (f: FieldCommitment): Bytes {
  return sha256(concat(TAG_FIELD, f.salt, utf8(f.name), f.value))
}
export function fieldRoot (fields: FieldCommitment[]): Bytes {
  return merkleRoot(fields.map(fieldLeaf))
}
export function fieldPath (fields: FieldCommitment[], index: number): MerkleProof {
  return merkleProof(fields.map(fieldLeaf), index)
}
export const randSalt = (): Bytes => crypto.getRandomValues(new Uint8Array(32))

// --- DIS-003/004: scoped disclosure envelope --------------------------------
export interface DisclosureEnvelope {
  fieldIndex: number
  field: FieldCommitment
  verifier: string // named verifier pubkey hex — the ONLY party it opens for
  purpose: string
  expiresAt: number // unix seconds
  nonce: Bytes
  sig: string // owner signature over the claims digest
  wrapped?: Bytes // value encrypted to the verifier (optional)
}
export function envDigest (e: Omit<DisclosureEnvelope, 'sig'>, fieldCount: number, root: Bytes): Bytes {
  const t8 = new Uint8Array(8); new DataView(t8.buffer).setBigUint64(0, BigInt(e.expiresAt), true)
  const n4 = new Uint8Array(4); new DataView(n4.buffer).setUint32(0, fieldCount, true)
  return dsha256(concat(TAG_ENV, utf8(String(e.fieldIndex)), utf8(e.field.name), e.field.value, e.field.salt, fromHex(e.verifier), utf8(e.purpose), t8, e.nonce, e.wrapped ?? new Uint8Array(0), n4, root))
}
export function issueDisclosure (owner: PrivateKey, fields: FieldCommitment[], index: number, verifierPub: string, expiresAt: number, purpose = 'audit', wrapped?: Bytes): DisclosureEnvelope {
  const field = fields[index]!
  const root = fieldRoot(fields)
  const e: Omit<DisclosureEnvelope, 'sig'> = { fieldIndex: index, field, verifier: verifierPub, purpose, expiresAt, nonce: crypto.getRandomValues(new Uint8Array(16)), wrapped }
  return { ...e, sig: owner.sign(Array.from(envDigest(e, fields.length, root))).toDER('hex') as string }
}
/** fragment content commits the field count WITH the root — a path with the
 * wrong index or length can't be substituted. */
export const TAG_FIELDS = utf8('mamem:fields:v1')
export function fieldsCommitment (fields: FieldCommitment[]): Bytes {
  const n4 = new Uint8Array(4); new DataView(n4.buffer).setUint32(0, fields.length, true)
  return sha256(concat(TAG_FIELDS, n4, fieldRoot(fields)))
}
/** height of the merkle path expected for `count` leaves (ceil log2). */
export function pathHeightFor (count: number): number {
  let h = 0, n = 1
  while (n < count) { n <<= 1; h++ }
  return h
}
/** verify envelope: signature + verifier match + not expired + field path →
 * root, with index/count binding. `root` is the fragment's committed field
 * root (verified separately against the anchored fragment leaf). */
export function verifyDisclosure (e: DisclosureEnvelope, ownerPub: string, verifierPub: string, fieldCount: number, root: Bytes, siblings: Bytes[], atTime: number): { ok: boolean, reason?: string } {
  if (atTime >= e.expiresAt) return { ok: false, reason: 'expired' }
  if (e.verifier !== verifierPub) return { ok: false, reason: 'verifier' }
  if (siblings.length !== pathHeightFor(fieldCount)) return { ok: false, reason: 'path-height' }
  if (e.fieldIndex < 0 || e.fieldIndex >= fieldCount) return { ok: false, reason: 'index' }
  try {
    if (!PublicKey.fromString(ownerPub).verify(Array.from(envDigest(e, fieldCount, root)), Signature.fromDER(e.sig, 'hex'))) return { ok: false, reason: 'signature' }
  } catch { return { ok: false, reason: 'signature' } }
  const leaf = fieldLeaf(e.field)
  const r = reconstructRoot(leaf, { index: e.fieldIndex, siblings })
  return Buffer.from(r).toString('hex') === Buffer.from(root).toString('hex')
    ? { ok: true } : { ok: false, reason: 'field-path' }
}
/** verifyDisclosure + the anchored fragment check: `verifyAnchor` (injected —
 * @mamem/verify's verifyFragment) must confirm the fragment leaf is anchored
 * and committed to (fieldCount, fieldRoot) before the envelope is trusted.
 * Dependency-injected to keep @mamem/identity free of the verify→anchor cycle. */
export async function verifyDisclosureAnchored (
  e: DisclosureEnvelope, ownerPub: string, verifierPub: string,
  fieldCount: number, fieldRoot: Bytes, siblings: Bytes[], atTime: number,
  verifyAnchor: () => Promise<boolean>
): Promise<{ ok: boolean, reason?: string }> {
  if (!(await verifyAnchor())) return { ok: false, reason: 'anchor' }
  return verifyDisclosure(e, ownerPub, verifierPub, fieldCount, fieldRoot, siblings, atTime)
}

// --- KEY-014: multi-recipient key wrapping (BRC-42 ECDH) ---------------------
/** wrap a symmetric key for one recipient via ECDH: K_wrap = KDF(ECDH(priv, pubR)).
 * The holder of (pubR, privR) alone can unwrap. */
// ECDH wrap: shared = priv_sender · P_recipient; then HKDF-SHA256(shared,
// salt=rand32, info=context) as the AES-256-GCM key with AAD=context. Output
// carries salt‖nonce‖tag‖ct — a fresh pad per wrap, tamper-detecting.
import { createCipheriv, createDecipheriv, randomBytes, hkdfSync } from 'node:crypto'
export interface WrappedKey { salt: Bytes, nonce: Bytes, ct: Bytes } // ct includes the 16B GCM tag
function ecdhShared (priv: PrivateKey, pubHex: string): Bytes {
  const Pp = PublicKey.fromString(pubHex) as any
  const shared = new Point(Pp.x, Pp.y).mul(new BigNumber(priv.toHex(), 16))
  return Buffer.from(shared.encode(true)) as Uint8Array
}
function wrapKeyFor (shared: Bytes, salt: Bytes, context: string): Bytes {
  return new Uint8Array(hkdfSync('sha256', Buffer.from(shared), Buffer.from(salt), Buffer.from(context), 32))
}
export function wrapFor (sender: PrivateKey, recipientPub: string, key: Bytes, context: string): WrappedKey {
  const salt = new Uint8Array(randomBytes(32))
  const nonce = new Uint8Array(randomBytes(12))
  const k = wrapKeyFor(ecdhShared(sender, recipientPub), salt, context)
  const c = createCipheriv('aes-256-gcm', Buffer.from(k), Buffer.from(nonce))
  c.setAAD(Buffer.from(context))
  const ct = new Uint8Array(Buffer.concat([c.update(Buffer.from(key)), c.final()]))
  return { salt, nonce, ct: concat(ct, new Uint8Array(c.getAuthTag())) }
}
export function unwrapFor (recipient: PrivateKey, senderPub: string, w: WrappedKey, context: string, keyLen = 32): Bytes {
  const k = wrapKeyFor(ecdhShared(recipient, senderPub), w.salt, context)
  const ct = w.ct.subarray(0, w.ct.length - 16)
  const tag = w.ct.subarray(w.ct.length - 16)
  const d = createDecipheriv('aes-256-gcm', Buffer.from(k), Buffer.from(w.nonce))
  d.setAAD(Buffer.from(context))
  d.setAuthTag(Buffer.from(tag))
  return new Uint8Array(Buffer.concat([d.update(Buffer.from(ct)), d.final()])).subarray(0, keyLen)
}
/** wrap one key for many recipients: [recipientPub, wrapped] pairs. */
export function wrapMulti (sender: PrivateKey, key: Bytes, recipients: string[], context: string): Array<{ pub: string, wrapped: WrappedKey }> {
  return recipients.map(pub => ({ pub, wrapped: wrapFor(sender, pub, key, context) }))
}
export function unwrapMulti (recipient: PrivateKey, senderPub: string, wraps: Array<{ pub: string, wrapped: WrappedKey }>, context: string, keyLen = 32): Bytes | null {
  const mine = wraps.find(w => w.pub === recipient.toPublicKey().toString())
  if (!mine) return null
  return unwrapFor(recipient, senderPub, mine.wrapped, context, keyLen)
}
