// nektar anchor helpers (Phase-0 spike). Anchor = PushDrop output with fields
// [proto="nektar", version=u8, shard utf8, n u64le, size u64le, R_n 32B, R'_n 32B]
// locked to a shard key via BRC-42 (protocolID [1,'nektar'], keyID = shard).
import { createHash } from 'node:crypto'
import { Transaction, PushDrop, ProtoWallet, PrivateKey, Script } from '@bsv/sdk'

export const PROTO = 'nektar'
export const VERSION = 1
export const PROTOCOL_ID = [1, 'nektar']
export const ZERO32 = new Uint8Array(32)

export function dsha256(b) {
  return new Uint8Array(
    createHash('sha256')
      .update(Buffer.from(createHash('sha256').update(Buffer.from(b)).digest()))
      .digest()
  )
}

export function u64le(n) {
  const b = Buffer.alloc(8)
  b.writeBigUInt64LE(BigInt(n))
  return new Uint8Array(b)
}
export function readU64le(f) {
  return Number(Buffer.from(f).readBigUInt64LE())
}

// R'_n = dsha256(R'_{n-1} | R_n | size_le8); R'_-1 = 0^32
export function chainRp(prevRp, R, size) {
  return dsha256(Buffer.concat([Buffer.from(prevRp), Buffer.from(R), Buffer.from(u64le(size))]))
}

export function anchorFields({ shard, n, size, R, Rp }) {
  return [[...Buffer.from(PROTO, 'utf8')], [VERSION], [...Buffer.from(shard, 'utf8')], [...u64le(n)], [...u64le(size)], [...R], [...Rp]]
}

export function parseAnchorFields(lockingScript) {
  let dec
  try {
    dec = PushDrop.decode(lockingScript, 'before')
  } catch {
    return null
  }
  const f = dec.fields
  if (f.length !== 7) return null
  if (Buffer.from(f[0]).toString('utf8') !== PROTO) return null
  if (f[1].length !== 1 || f[1][0] !== VERSION) return null
  if (f[5].length !== 32 || f[6].length !== 32) return null
  return {
    shard: Buffer.from(f[2]).toString('utf8'),
    n: readU64le(f[3]),
    size: readU64le(f[4]),
    R: Uint8Array.from(f[5]),
    Rp: Uint8Array.from(f[6]),
    lockingPublicKey: dec.lockingPublicKey
  }
}

// shardKey: a PrivateKey identifying the shard (keyID = shard name under BRC-42).
export async function anchorLockingScript(wallet, shard, fields) {
  const pd = new PushDrop(wallet)
  return pd.lock(fields, PROTOCOL_ID, shard, 'self', true, false, 'before')
}

export function anchorUnlockTemplate(wallet, shard, sourceSatoshis, lockingScript) {
  const pd = new PushDrop(wallet)
  return pd.unlock(PROTOCOL_ID, shard, 'self', 'all', false, sourceSatoshis, lockingScript)
}

// p2pkh unlock for funding inputs
import { P2PKH } from '@bsv/sdk'
export function p2pkhUnlock(key) {
  return new P2PKH().unlock(key)
}
export function p2pkhLock(key) {
  return new P2PKH().lock(key.toPublicKey().toAddress([0x6f]))
}

export { Transaction, PrivateKey, ProtoWallet, Script }
