// L_ZKMem v2 — Wright §13 membership proof on the wire.
//   'ZKM2' ‖ ver ‖ alg(1) ‖ imageId(32) ‖ root(32) ‖ size(u32 LE) ‖ queryId(32) ‖ receiptLen(u32 LE) ‖ receipt
// ver=2 (current): carries the GEN-007 per-structure algorithm id `alg`
//   (0x01 = SHA-256d suite). ver=1 (legacy, no alg byte) still decodes —
//   its suite is implied SHA-256d.
// The header repeats the receipt's public journal so verifiers can route and
// pre-check cheaply; verification REQUIRES header == journal byte for byte.
export const ZKM2_MAGIC = Uint8Array.from([0x5a, 0x4b, 0x4d, 0x32]) // "ZKM2"
export const ZKM2_VERSION = 2 // rev 2: GEN-007 algorithm id byte
/** GEN-007: 0x01 = the sha256d suite (Merkle path fold + STARK over SHA-256) */
export const ALG_SHA256D = 0x01
export const ZKM2_MAX_RECEIPT = 1_500_000 // succinct receipts are ~224 KB; bound the parser
const HEADER_V1 = 4 + 1 + 32 + 32 + 4 + 32 + 4
const HEADER = HEADER_V1 + 1 // + algorithm byte

export class ZkmCodecError extends Error {}

export interface ZkMembershipProof {
  /** GEN-007 algorithm suite id (sha256d); rev-1 wires imply it */
  algorithm?: number
  imageId: Uint8Array // pinned guest program identity
  root: Uint8Array // R_n: the double-SHA-256 batch root anchored on BSV
  size: number // batch size committed in the anchor
  queryId: Uint8Array // verifier-issued, single-use
  receipt: Uint8Array // RISC Zero succinct receipt (one ZK-STARK), bincode
}

const u32 = (v: number) => { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, v, true); return b }

export function encodeZkm2 (p: ZkMembershipProof): Uint8Array {
  const alg = p.algorithm ?? ALG_SHA256D
  if (alg !== ALG_SHA256D) throw new ZkmCodecError(`unsupported algorithm ${alg}`)
  for (const [k, v] of [['imageId', p.imageId], ['root', p.root], ['queryId', p.queryId]] as const) if (v.length !== 32) throw new ZkmCodecError(`${k} must be 32 bytes`)
  if (!Number.isInteger(p.size) || p.size < 1 || p.size > 0xffffffff) throw new ZkmCodecError('size out of range')
  if (p.receipt.length === 0 || p.receipt.length > ZKM2_MAX_RECEIPT) throw new ZkmCodecError('receipt length out of range')
  const out = new Uint8Array(HEADER + p.receipt.length)
  let o = 0
  const put = (b: Uint8Array) => { out.set(b, o); o += b.length }
  put(ZKM2_MAGIC); put(Uint8Array.of(ZKM2_VERSION)); put(Uint8Array.of(alg)); put(p.imageId); put(p.root); put(u32(p.size)); put(p.queryId); put(u32(p.receipt.length)); put(p.receipt)
  return out
}

export function decodeZkm2 (b: Uint8Array): ZkMembershipProof {
  if (b.length < HEADER_V1) throw new ZkmCodecError('truncated header')
  for (let i = 0; i < 4; i++) if (b[i] !== ZKM2_MAGIC[i]) throw new ZkmCodecError('bad magic (not L_ZKMem v2)')
  const ver = b[4]!
  if (ver !== 1 && ver !== ZKM2_VERSION) throw new ZkmCodecError(`unsupported version ${ver}`)
  const off = ver === 1 ? 0 : 1 // rev 2 inserts the algorithm byte at index 5
  const alg = ver === 1 ? ALG_SHA256D : b[5]!
  if (alg !== ALG_SHA256D) throw new ZkmCodecError(`unsupported algorithm ${alg}`)
  const header = HEADER_V1 + off
  if (b.length < header) throw new ZkmCodecError('truncated header')
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const size = dv.getUint32(69 + off, true)
  if (size < 1) throw new ZkmCodecError('size must be >= 1')
  const len = dv.getUint32(header - 4, true)
  if (len === 0 || len > ZKM2_MAX_RECEIPT) throw new ZkmCodecError('receipt length out of range')
  if (b.length !== header + len) throw new ZkmCodecError(b.length < header + len ? 'truncated receipt' : 'trailing bytes')
  return { algorithm: alg, imageId: b.slice(5 + off, 37 + off), root: b.slice(37 + off, 69 + off), size, queryId: b.slice(73 + off, 105 + off), receipt: b.slice(header) }
}
