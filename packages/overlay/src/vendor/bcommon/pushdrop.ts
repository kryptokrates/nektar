/**
 * A lock-before PushDrop read only in the one encoding the producer's
 * template writes. The twin of the Go pushdrop.CheckCanonical.
 *
 * The SDK's PushDrop.decode reads looser forms too: a wider push than a
 * field needs, a trailing opcode, drops written one at a time, and any key
 * PublicKey.fromString takes, including an uncompressed one or a compressed
 * one whose x is at or above the field prime. Each is another script for the
 * same fields, so a host that keys on script bytes would see two outputs
 * where there is one.
 */
import { OP, PushDrop, type LockingScript, type PublicKey, type Script } from '@bsv/sdk'
import { strictPublicKey } from './pubkey.js'

/** d in the push a minimal encoding uses, as the template writes it. */
function minimalPush(d: readonly number[]): number[] {
  const n = d.length
  if (n === 0 || (n === 1 && d[0] === 0)) return [OP.OP_0]
  if (n === 1 && d[0]! >= 1 && d[0]! <= 16) return [0x50 + d[0]!]
  if (n === 1 && d[0] === 0x81) return [OP.OP_1NEGATE]
  if (n <= 75) return [n, ...d]
  if (n <= 0xff) return [OP.OP_PUSHDATA1, n, ...d]
  if (n <= 0xffff) return [OP.OP_PUSHDATA2, n & 0xff, n >> 8, ...d]
  return [OP.OP_PUSHDATA4, n & 0xff, (n >> 8) & 0xff, (n >> 16) & 0xff, n >>> 24, ...d]
}

/** The lock-before PushDrop of key and fields as the template writes it. */
function canonicalLock(key: readonly number[], fields: readonly number[][]): number[] {
  const out = [...minimalPush(key), OP.OP_CHECKSIG]
  for (const f of fields) out.push(...minimalPush(f))
  let n = fields.length
  for (; n > 1; n -= 2) out.push(OP.OP_2DROP)
  if (n === 1) out.push(OP.OP_DROP)
  return out
}

/**
 * The key and fields (a signature included, when the lock is signed) of a
 * lock-before PushDrop, or undefined when the script is not exactly the one
 * the template writes for them: the key pushed directly in its canonical
 * compressed encoding (strictPublicKey), OP_CHECKSIG, each field minimally
 * pushed, then OP_2DROP per pair of fields and OP_DROP for one left over.
 */
export function decodeStrictPushDrop(script: Script): { lockingPublicKey: PublicKey; fields: number[][] } | undefined {
  const keyBytes = script.chunks[0]?.data
  if (keyBytes === undefined || strictPublicKey(keyBytes) === undefined) return undefined
  let decoded: { lockingPublicKey: PublicKey; fields: number[][] }
  try {
    decoded = PushDrop.decode(script as LockingScript, 'before')
  } catch {
    return undefined
  }
  const want = canonicalLock(keyBytes, decoded.fields)
  const got = script.toBinary()
  if (got.length !== want.length || !got.every((x, i) => x === want[i])) return undefined
  return decoded
}
