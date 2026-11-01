// L_ZKMem v1 binary encoding — strict, exact-length, fuzz-safe.
// 'ZKM1' | vkeyHash 32 | root 32 | nullifier 32 | queryId 32 |
// pi_a 64 | pi_b 128 | pi_c 64   (each Fq/Fq2 element 32B big-endian, < p)
// r = scalar field (signals: root/nullifier/queryId), q = base field (proof coords)
export const BN254_R = 21888242871839275222246405745257275088548364400416034343698204186575808495617n
export const BN254_Q = 21888242871839275222246405745257275088696311157297823662689037894645226208583n
const MAGIC = 'ZKM1'
const HLEN = 4 + 1 + 32 * 4 + 64 + 128 + 64 // magic4 + version1 + 4×32 + 256

export class ZkCodecError extends Error {}

export interface ZkMemProof {
  vkeyHash: Uint8Array
  root: bigint
  nullifier: bigint
  queryId: bigint
  pi_a: [bigint, bigint]
  pi_b: [[bigint, bigint], [bigint, bigint]]
  pi_c: [bigint, bigint]
}

function readField (buf: Uint8Array, off: number, p: bigint): bigint {
  let v = 0n
  for (let i = 0; i < 32; i++) v = (v << 8n) | BigInt(buf[off + i]!)
  if (v >= p) throw new ZkCodecError(`non-canonical field element >= field at offset ${off}`)
  return v
}
function writeField (v: bigint, out: Uint8Array, off: number, p: bigint): void {
  if (v < 0n || v >= p) throw new ZkCodecError('field element out of range')
  let x = v
  for (let i = 31; i >= 0; i--) { out[off + i] = Number(x & 0xffn); x >>= 8n }
}

export function encodeZkMem (p: ZkMemProof, version = 1): Uint8Array {
  const out = new Uint8Array(HLEN)
  out.set(Buffer.from(MAGIC), 0); out[4] = version
  out.set(p.vkeyHash, 5)
  writeField(p.root, out, 37, BN254_R); writeField(p.nullifier, out, 69, BN254_R); writeField(p.queryId, out, 101, BN254_R)
  writeField(p.pi_a[0], out, 133, BN254_Q); writeField(p.pi_a[1], out, 165, BN254_Q)
  writeField(p.pi_b[0][0], out, 197, BN254_Q); writeField(p.pi_b[0][1], out, 229, BN254_Q)
  writeField(p.pi_b[1][0], out, 261, BN254_Q); writeField(p.pi_b[1][1], out, 293, BN254_Q)
  writeField(p.pi_c[0], out, 325, BN254_Q); writeField(p.pi_c[1], out, 357, BN254_Q)
  return out
}

export function decodeZkMem (buf: Uint8Array): ZkMemProof {
  if (buf.length !== HLEN) throw new ZkCodecError(`L_ZKMem wrong length ${buf.length} (want ${HLEN})`)
  if (Buffer.from(buf.subarray(0, 4)).toString() !== MAGIC) throw new ZkCodecError('L_ZKMem bad magic')
  if (buf[4] !== 1) throw new ZkCodecError(`L_ZKMem bad version ${buf[4]}`)
  const vkeyHash = buf.slice(5, 37)
  return {
    vkeyHash,
    root: readField(buf, 37, BN254_R), nullifier: readField(buf, 69, BN254_R), queryId: readField(buf, 101, BN254_R),
    pi_a: [readField(buf, 133, BN254_Q), readField(buf, 165, BN254_Q)],
    pi_b: [[readField(buf, 197, BN254_Q), readField(buf, 229, BN254_Q)], [readField(buf, 261, BN254_Q), readField(buf, 293, BN254_Q)]],
    pi_c: [readField(buf, 325, BN254_Q), readField(buf, 357, BN254_Q)]
  }
}
