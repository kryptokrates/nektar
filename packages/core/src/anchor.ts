// Anchor field codec + chained root. Anchor = PushDrop output with fields
//   [proto="mamem", version=u8, shard utf8, n u64le, size u64le,
//    R_n 32B, R'_n 32B, poseidonRoot reserved-empty]
// R'_n = H(TAG_RPRIME | R'_{n-1} | R_n | size_le8), R'_-1 = 0^32.
import { dsha256, concat, utf8, u64le, eq, ZERO32, type Bytes } from './hash.js'
import { TAG_RPRIME } from './domain.js'

export const PROTO = 'mamem'
export const VERSION = 1
export const PROTOCOL_ID: [number, string] = [1, 'mamem']
export const FIELD_COUNT = 8

export interface AnchorData {
  shard: string
  n: number
  size: number
  R: Bytes
  Rp: Bytes
  poseidonRoot?: Bytes // reserved; must be empty or 32B for now
}

export function chainRp (prevRp: Bytes, R: Bytes, size: number): Bytes {
  return dsha256(concat(TAG_RPRIME, prevRp, R, u64le(size)))
}

export const GENESIS_PREV_RP: Bytes = ZERO32

export function anchorFields (a: AnchorData): number[][] {
  return [
    [...utf8(PROTO)],
    [VERSION],
    [...utf8(a.shard)],
    [...u64le(a.n)],
    [...u64le(a.size)],
    [...a.R],
    [...a.Rp],
    [...(a.poseidonRoot ?? new Uint8Array(0))]
  ]
}

export function expectedRp (prev: AnchorData | null, R: Bytes, size: number): Bytes {
  return chainRp(prev ? prev.Rp : GENESIS_PREV_RP, R, size)
}

export { eq }

// Shard-id binding (tm_mamem enforces it): shard = hex(hash160(locking pubkey))
// where the locking pubkey is the BRC-42 child of the shard key under
// (PROTOCOL_ID, keyID=shardLabel, 'self').
import { hash160, toHex } from './hash.js'

export function shardIdFromPubkey (pubkeyBytes: Bytes): string {
  return toHex(hash160(pubkeyBytes))
}
