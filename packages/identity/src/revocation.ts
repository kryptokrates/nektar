// §19 CRL helpers — revocation entries anchored on the __revocation__ shard.
// Record format (self-contained CRL entry):
//   revoke:<pkHex>:root=<smtRootHex>[:proof=<json SmtProof>]
// The embedded proof makes the anchored record alone sufficient evidence —
// a third party needs no operator-held SMT state: parse the record, then
// verifyProof(proof, {present: revoked}, root) with key = derive_key(pk).
// Records without :proof= (pre-CRL writes) still parse, proof = undefined.
import { sha256, utf8, toHex, fromHex, type Bytes } from '@nektar/core'
import { deriveKey, verifyProof, type SmtProof, type SmtKey } from './smt.js'

export const REVOKED_VALUE: Bytes = sha256(utf8('revoked'))

export function serializeSmtProof(p: SmtProof): string {
  return JSON.stringify({ key: toHex(p.key), siblings: p.siblings.map(([d, h]) => [d, toHex(h)]) })
}
export function deserializeSmtProof(s: string): SmtProof {
  const j = JSON.parse(s)
  return { key: fromHex(j.key), siblings: j.siblings.map(([d, h]: [number, string]) => [d, fromHex(h)] as [number, Bytes]) }
}

export interface RevocationEntry {
  pk: string
  root: string
  proof?: SmtProof
}

/** `revoke:<pk>:root=<hex>` or `revoke:<pk>:root=<hex>:proof=<json>` */
export function makeRevocationRecord(pkHex: string, root: Bytes, proof?: SmtProof): string {
  const base = `revoke:${pkHex}:root=${toHex(root)}`
  return proof ? `${base}:proof=${serializeSmtProof(proof)}` : base
}

/** parse a __revocation__ shard record; null if not a revocation entry. */
export function parseRevocationRecord(data: string): RevocationEntry | null {
  const m = /^revoke:([0-9a-f]{66}):root=([0-9a-f]{64})(?::proof=(.+))?$/i.exec(data)
  if (!m) return null
  const e: RevocationEntry = { pk: m[1]!, root: m[2]! }
  if (m[3]) {
    try {
      e.proof = deserializeSmtProof(m[3])
    } catch {
      /* malformed proof — entry still parses, verification fails closed */
    }
  }
  return e
}

const eqB = (a: Bytes, b: Bytes) => a.length === b.length && a.every((x, i) => x === b[i])

/** does this entry cryptographically prove `pk` revoked under `root`?
 *  binds BOTH the key (proof must be for derive_key(pk), not a neighbour)
 *  and the anchored root. */
export function verifyRevocationEntry(e: { pk: string; root: string; proof?: SmtProof }): boolean {
  if (!e.proof) return false
  try {
    const key = revocationKey(e.pk)
    return eqB(e.proof.key as SmtKey, key) && verifyProof(e.proof, { present: true, value: REVOKED_VALUE }, fromHex(e.root))
  } catch {
    return false
  }
}

/** verify a status answer: `proof` against `root` for pk's key — present iff revoked. */
export function verifyRevocationStatus(pkHex: string, root: string, proof: SmtProof): { ok: boolean; revoked: boolean } {
  try {
    const key = revocationKey(pkHex)
    if (!eqB(proof.key as SmtKey, key)) return { ok: false, revoked: false }
    if (verifyProof(proof, { present: true, value: REVOKED_VALUE }, fromHex(root))) return { ok: true, revoked: true }
    if (verifyProof(proof, { present: false }, fromHex(root))) return { ok: true, revoked: false }
    return { ok: false, revoked: false }
  } catch {
    return { ok: false, revoked: false }
  }
}

/** key under which pk's revocation status lives in the SMT (derive_key). */
export function revocationKey(pkHex: string): Bytes {
  return deriveKey(fromHex(pkHex))
}
