/**
 * A public key taken from someone else, accepted only in its one canonical
 * encoding. The twin of the Go guard.ParsePubKey.
 *
 * The SDK's PublicKey.fromString takes a compressed key whose x is at or
 * above the field prime (02 || p+1 reads as the point with x = 1), so one
 * point has more than one encoding. It reduces x when it writes the key
 * back, so a key compared as a parsed key is safe, but the bytes a script or
 * a record carries are not the bytes the key writes, and anything keyed by
 * those bytes sees two keys. This accepts 33 bytes, the prefix 0x02 or 0x03,
 * and an x below the prime that names a point on the curve, and nothing
 * else.
 */
import { PublicKey } from '@bsv/sdk'

/** secp256k1's field prime p. */
const fieldPrime = 0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2fn

function toHex(b: ArrayLike<number>): string {
  let s = ''
  for (let i = 0; i < b.length; i++) s += (b[i]! & 0xff).toString(16).padStart(2, '0')
  return s
}

/** The key, or undefined when the bytes are not its canonical encoding. */
export function strictPublicKey(b: ArrayLike<number>): PublicKey | undefined {
  if (b.length !== 33 || (b[0] !== 0x02 && b[0] !== 0x03)) return undefined
  let x = 0n
  for (let i = 1; i < 33; i++) x = (x << 8n) | BigInt(b[i]! & 0xff)
  if (x >= fieldPrime) return undefined
  const hex = toHex(b)
  let key: PublicKey
  try {
    key = PublicKey.fromString(hex)
  } catch {
    return undefined
  }
  // The point is on the curve, and it writes back as the bytes it was read
  // from.
  if (!key.validate() || key.toString() !== hex) return undefined
  return key
}

/**
 * strictPublicKey over hex, which must be the key's 66 digits, in either
 * case, as the Go guard.ParsePubKeyHex reads it.
 */
export function strictPublicKeyHex(hex: string): PublicKey | undefined {
  if (!/^[0-9a-fA-F]{66}$/.test(hex)) return undefined
  const b: number[] = []
  for (let i = 0; i < 66; i += 2) b.push(parseInt(hex.slice(i, i + 2), 16))
  return strictPublicKey(b)
}
