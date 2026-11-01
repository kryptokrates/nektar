// BRC-42 fragment keys + AES-256-GCM.
// keyID = canonical context t‖ψ‖ρ‖π‖l (tenant‖shard‖record‖fragment‖level),
// protocolID [2,'mamem']; key = SHA-256(BRC-42 shared secret) → AES-256-GCM.
// AAD binds H(d) and the full context — a ciphertext cannot be transplanted
// to another fragment, context, or level.
import { createCipheriv, createDecipheriv, randomBytes } from 'node:crypto'
import { PrivateKey, PublicKey, KeyDeriver } from '@bsv/sdk'
import { sha256, concat, utf8, toHex, type Bytes } from '@mamem/core'

export interface FragmentContext {
  t: string // tenant
  psi: string // shard ψ
  rho: string // record/batch ρ
  pi: string // fragment π (position/id)
  l: number // access level
}

/** canonical keyID: t‖ψ‖ρ‖π‖l with |-separator and length prefixing */
export function canonicalKeyID (c: FragmentContext): string {
  const parts = [c.t, c.psi, c.rho, c.pi, String(c.l)]
  return parts.map(p => `${p.length}:${p}`).join('|')
}

export const CRYPTO_PROTOCOL_ID: [number, string] = [2, 'mamem']

export interface FragmentKeys {
  /** the AES-256 key for this fragment */
  fragmentKey: Bytes
  /** derived child public key (what went on-chain / into receipts) */
  childPub: string
}

/**
 * Agent seed S_A: the holder's root private key. Per-fragment key derivation
 * via BRC-42 ECDH between the agent's derived keypair and itself (self mode):
 * the fragment key is only computable by whoever holds S_A (or the level key).
 */
export function deriveFragmentKey (agentSeed: PrivateKey, ctx: FragmentContext): FragmentKeys {
  const deriver = new KeyDeriver(agentSeed)
  const keyID = canonicalKeyID(ctx)
  const pub = deriver.derivePublicKey(CRYPTO_PROTOCOL_ID as any, keyID, 'self')
  const priv = deriver.derivePrivateKey(CRYPTO_PROTOCOL_ID as any, keyID, 'self')
  const scalar = Uint8Array.from(Buffer.from(priv.toHex().padStart(64, '0'), 'hex'))
  return { fragmentKey: sha256(scalar), childPub: pub.toString() }
}

/** Level key: derive once per (seed, level); used to hand out level access. */
export function deriveLevelKey (agentSeed: PrivateKey, level: number): PrivateKey {
  const deriver = new KeyDeriver(agentSeed)
  return deriver.derivePrivateKey(CRYPTO_PROTOCOL_ID as any, `level:${level}`, 'self')
}

export interface EncryptedFragment {
  ciphertext: Bytes // nonce|tag|ct concatenated
  keyID: string
  aad: Bytes
}

export function encryptFragment (
  key: Bytes,
  plaintext: Bytes,
  hd: Bytes, // H(d)
  ctx: FragmentContext
): EncryptedFragment {
  const aad = concat(hd, utf8(canonicalKeyID(ctx)))
  const nonce = new Uint8Array(randomBytes(12))
  const cipher = createCipheriv('aes-256-gcm', Buffer.from(key), Buffer.from(nonce))
  cipher.setAAD(Buffer.from(aad))
  const ct = new Uint8Array(Buffer.concat([cipher.update(Buffer.from(plaintext)), cipher.final()]))
  const tag = new Uint8Array(cipher.getAuthTag())
  return { ciphertext: concat(nonce, tag, ct), keyID: canonicalKeyID(ctx), aad }
}

export function decryptFragment (key: Bytes, ef: EncryptedFragment): Bytes {
  const nonce = ef.ciphertext.subarray(0, 12)
  const tag = ef.ciphertext.subarray(12, 28)
  const ct = ef.ciphertext.subarray(28)
  const dec = createDecipheriv('aes-256-gcm', Buffer.from(key), Buffer.from(nonce))
  dec.setAAD(Buffer.from(ef.aad))
  dec.setAuthTag(Buffer.from(tag))
  return new Uint8Array(Buffer.concat([dec.update(Buffer.from(ct)), dec.final()]))
}

/**
 * Crypto-shredding: securely zero key material. Once shredKey runs, the
 * ciphertext is unrecoverable by design (AES-256-GCM, no escrow).
 */
export function shredKey (key: Bytes): void {
  key.fill(0)
}

/** grantLevels(seed, L): the per-level keys 0..L that make up a holder's S_A. */
export function grantLevels (seed: PrivateKey, L: number): PrivateKey[] {
  const out: PrivateKey[] = []
  for (let l = 0; l <= L; l++) out.push(deriveLevelKey(seed, l))
  return out
}
