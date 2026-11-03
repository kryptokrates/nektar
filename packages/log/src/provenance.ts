// COM-003: originator signatures over provenance. σ = Sign_source(provenanceDigest(d, p)).
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { provenanceDigest, type Bytes, type Provenance } from '@nektar/core'

export class ProvenanceError extends Error {}

/** returns p with σ filled in */
export function signProvenance (key: PrivateKey, d: Bytes, p: Provenance): Provenance {
  const sig = key.sign(Array.from(provenanceDigest(d, p)))
  return { ...p, signature: Uint8Array.from(sig.toDER() as number[]) }
}

/** true iff p.signature is a valid signature of pubHex over (d, p\σ) */
export function verifyProvenance (pubHex: string, d: Bytes, p: Provenance): boolean {
  if (p.signature.length === 0) return false
  let pub: PublicKey, sig: Signature
  try { pub = PublicKey.fromString(pubHex); sig = Signature.fromDER(Array.from(p.signature)) } catch { return false }
  return pub.verify(Array.from(provenanceDigest(d, p)), sig)
}
