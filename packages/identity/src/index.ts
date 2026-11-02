// @mamem/identity — UUID↔pubkey bindings, challenge-response auth, and
// rotation certificates (IDN-002..005, IDN-007).
//
// Rotation certificate: C(i) = Sign_sk(i-1)(H(TAG_ROT ‖ pk(i-1) ‖ pk(i) ‖ t_i))
// — a chain from pk(0). Each cert carries a validity window [t_i, t_i+δ_i):
//   - no NEW signatures are valid after t_i+δ_i (the key rotated away);
//   - signatures made inside the window remain verifiable forever
//     (historical validation uses the signing-time t, not wall-clock time).
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { concat, dsha256, utf8, toHex, fromHex, type Bytes } from '@mamem/core'

export const TAG_ROT = utf8('mamem:rotcert:v1')
export const TAG_IDBIND = utf8('mamem:idbind:v1')

// --- UUID↔pubkey binding ----------------------------------------------------
export interface IdentityBinding {
  uuid: string
  pubkey: string // compressed pubkey hex
  prevHash: string // hash of the previous binding in the log ('00'*32 for first)
  seq: number
  sig: string // Sign_uuidOwner(H(TAG_IDBIND ‖ uuid ‖ pubkey ‖ prevHash ‖ seq_le8))
}
export function bindingDigest (b: Omit<IdentityBinding, 'sig'>): Bytes {
  const seq8 = new Uint8Array(8); new DataView(seq8.buffer).setBigUint64(0, BigInt(b.seq), true)
  return dsha256(concat(TAG_IDBIND, utf8(b.uuid), fromHex(b.pubkey), fromHex(b.prevHash), seq8))
}
export function signBinding (key: PrivateKey, uuid: string, prevHash: string, seq: number): IdentityBinding {
  const pubkey = key.toPublicKey().toString()
  const sig = key.sign(Array.from(bindingDigest({ uuid, pubkey, prevHash, seq }))).toDER('hex') as string
  return { uuid, pubkey, prevHash, seq, sig }
}
export function verifyBinding (b: IdentityBinding): boolean {
  try {
    const pub = PublicKey.fromString(b.pubkey)
    return pub.verify(Array.from(bindingDigest(b)), Signature.fromDER(b.sig, 'hex'))
  } catch { return false }
}

// --- challenge-response -----------------------------------------------------
// own domain tag: a challenge answer can never be confused with a binding sig.
// The VERIFIER issues the nonce (and must check it issued it — see
// MamemMemory.issueChallenge); a caller-chosen nonce proves nothing.
export const TAG_CHALLENGE = utf8('mamem:challenge:v1')
export function issueChallenge (nonce: Uint8Array = crypto.getRandomValues(new Uint8Array(32))): string {
  return toHex(nonce)
}
export function answerChallenge (key: PrivateKey, nonceHex: string): string {
  return key.sign(Array.from(dsha256(concat(TAG_CHALLENGE, fromHex(nonceHex))))).toDER('hex') as string
}
export function verifyChallenge (pubkeyHex: string, nonceHex: string, sigHex: string): boolean {
  try {
    return PublicKey.fromString(pubkeyHex).verify(Array.from(dsha256(concat(TAG_CHALLENGE, fromHex(nonceHex)))), Signature.fromDER(sigHex, 'hex'))
  } catch { return false }
}

// --- rotation certificates --------------------------------------------------
export interface RotationCert {
  index: number
  prevPubkey: string // pk(i-1)
  pubkey: string    // pk(i)
  issuedAt: number  // t_i — unix seconds; key(i-1) deprecated at t_i+δ_i
  horizon: number   // δ_i seconds — max validity window of pk(i-1)
  sig: string       // Sign_sk(i-1)(digest)
}
export function rotDigest (c: Omit<RotationCert, 'sig'>): Bytes {
  const t8 = new Uint8Array(8); new DataView(t8.buffer).setBigUint64(0, BigInt(c.issuedAt), true)
  return dsha256(concat(TAG_ROT, fromHex(c.prevPubkey), fromHex(c.pubkey), t8))
}
export function makeRotationCert (oldKey: PrivateKey, newPub: string, issuedAt: number, horizon = 86_400): RotationCert {
  const prevPubkey = oldKey.toPublicKey().toString()
  const sig = oldKey.sign(Array.from(rotDigest({ index: 0, prevPubkey, pubkey: newPub, issuedAt, horizon }))).toDER('hex') as string
  return { index: 0, prevPubkey, pubkey: newPub, issuedAt, horizon, sig }
}
export function verifyRotationCert (c: RotationCert): boolean {
  try {
    return PublicKey.fromString(c.prevPubkey).verify(Array.from(rotDigest(c)), Signature.fromDER(c.sig, 'hex'))
  } catch { return false }
}
/** Verify a cert chain pk(0)→pk(i); returns the active pubkey or null. */
/** Verify a cert chain pk(0)→pk(i); optional inclusionCheck (e.g. a Merkle
 * proof against the anchored rotation-log root) must pass for every cert. */
export function verifyCertChain (genesisPub: string, certs: RotationCert[], opts?: { inclusionCheck?: (c: RotationCert) => boolean }): { activePub: string, expiredAt: number | null } | null {
  let cur = genesisPub, expiredAt: number | null = null
  const sorted = [...certs].sort((a, b) => a.index - b.index)
  for (const c of sorted) {
    if (c.prevPubkey !== cur) return null
    if (!verifyRotationCert(c)) return null
    if (opts?.inclusionCheck && !opts.inclusionCheck(c)) return null
    cur = c.pubkey
    expiredAt = c.issuedAt + c.horizon
  }
  return { activePub: cur, expiredAt }
}
/** key deprecated for NEW signatures at cert.issuedAt + cert.horizon. A use at
 * useTime is valid iff useTime < expiry (or no cert rotates away from it). */
export function keyValidAt (cert: RotationCert, useTime: number): boolean {
  return useTime < cert.issuedAt + cert.horizon
}
/** §19 epoch binding: the key of `epoch` (0 = genesis) in a verified rotation
 *  chain and its signing window [from, until): a key may sign from the moment
 *  it was certified until its successor's deprecation horizon expires. */
export function epochKey (genesisPub: string, certs: RotationCert[], epoch: number): { pub: string, from: number, until: number } | null {
  const sorted = [...certs].sort((a, b) => a.index - b.index)
  if (!verifyCertChain(genesisPub, sorted) || epoch < 0 || epoch > sorted.length) return null
  const pub = epoch === 0 ? genesisPub : sorted[epoch - 1]!.pubkey
  const from = epoch === 0 ? -Infinity : sorted[epoch - 1]!.issuedAt
  const until = sorted[epoch] ? sorted[epoch]!.issuedAt + sorted[epoch]!.horizon : Infinity
  return { pub, from, until }
}
/** authorship survives rotation: signer must equal the epoch key and the
 *  write time must fall inside that key's signing window */
export function verifyAuthorship (signerPub: string, epoch: number, writtenAt: number, genesisPub: string, certs: RotationCert[]): { ok: boolean, reason?: string } {
  const k = epochKey(genesisPub, certs, epoch)
  if (!k) return { ok: false, reason: 'rotation chain invalid or epoch out of range' }
  if (k.pub !== signerPub) return { ok: false, reason: `signer is not the epoch-${epoch} key` }
  if (writtenAt < k.from || writtenAt >= k.until) return { ok: false, reason: `written at ${writtenAt} outside epoch-${epoch} window` }
  return { ok: true }
}
export * from "./smt.js"
export * from "./custody.js"
export * from "./disclosure.js"
export * from "./sale.js"
export * from "./zkaccess.js"
export * from "./wire.js"
export type { RangeProofBP } from "./bp.js"
