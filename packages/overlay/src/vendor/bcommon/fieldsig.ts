/**
 * The field signature a signed PushDrop carries as its last field: the
 * producer's signature over the other fields, under the output's own
 * locking key.
 */
import { Signature, type PublicKey } from '@bsv/sdk'

/**
 * Verify a PushDrop field signature.
 *
 * ProtoWallet.createSignature signs sha256(fields concatenated) as the ECDSA
 * digest directly, and PublicKey.verify hashes its message once with sha256
 * before verifying. So the raw concatenation is what is passed here, and the
 * digest checked is sha256 of the signed fields, the same one the Go side
 * checks with hash.Sha256. Confirmed against @bsv/sdk 2.7.1's ProtoWallet.js
 * and PublicKey.js rather than assumed.
 */
export function verifyFieldSignature(key: PublicKey, signed: number[], der: number[]): boolean {
  try {
    return key.verify(signed, Signature.fromDER(der))
  } catch {
    return false
  }
}
