// L_ZKMem v2 — Wright §13 membership proof on the wire.
//   'ZKM2' ‖ ver(1) ‖ imageId(32) ‖ root(32) ‖ size(u32 LE) ‖ queryId(32) ‖ receiptLen(u32 LE) ‖ receipt
// The header repeats the receipt's public journal so verifiers can route and
// pre-check cheaply; verification REQUIRES header == journal byte for byte.
export const ZKM2_MAGIC = Uint8Array.from([0x5a, 0x4b, 0x4d, 0x32]) // "ZKM2"
export const ZKM2_VERSION = 1
export const ZKM2_MAX_RECEIPT = 1_500_000 // succinct receipts are ~224 KB; bound the parser
const HEADER = 4 + 1 + 32 + 32 + 4 + 32 + 4

export class ZkmCodecError extends Error {}

export interface ZkMembershipProof {
  imageId: Uint8Array // pinned guest program identity
  root: Uint8Array // R_n: the double-SHA-256 batch root anchored on BSV
  size: number // batch size committed in the anchor
  queryId: Uint8Array // verifier-issued, single-use
  receipt: Uint8Array // RISC Zero succinct receipt (one ZK-STARK), bincode
}

const u32 = (v: number) => { const b = new Uint8Array(4); new DataView(b.buffer).setUint32(0, v, true); return b }

export function encodeZkm2 (p: ZkMembershipProof): Uint8Array {
  for (const [k, v] of [['imageId', p.imageId], ['root', p.root], ['queryId', p.queryId]] as const) if (v.length !== 32) throw new ZkmCodecError(`${k} must be 32 bytes`)
  if (!Number.isInteger(p.size) || p.size < 1 || p.size > 0xffffffff) throw new ZkmCodecError('size out of range')
  if (p.receipt.length === 0 || p.receipt.length > ZKM2_MAX_RECEIPT) throw new ZkmCodecError('receipt length out of range')
  const out = new Uint8Array(HEADER + p.receipt.length)
  let o = 0
  const put = (b: Uint8Array) => { out.set(b, o); o += b.length }
  put(ZKM2_MAGIC); put(Uint8Array.of(ZKM2_VERSION)); put(p.imageId); put(p.root); put(u32(p.size)); put(p.queryId); put(u32(p.receipt.length)); put(p.receipt)
  return out
}

export function decodeZkm2 (b: Uint8Array): ZkMembershipProof {
  if (b.length < HEADER) throw new ZkmCodecError('truncated header')
  for (let i = 0; i < 4; i++) if (b[i] !== ZKM2_MAGIC[i]) throw new ZkmCodecError('bad magic (not L_ZKMem v2)')
  if (b[4] !== ZKM2_VERSION) throw new ZkmCodecError(`unsupported version ${b[4]}`)
  const dv = new DataView(b.buffer, b.byteOffset, b.byteLength)
  const size = dv.getUint32(69, true)
  if (size < 1) throw new ZkmCodecError('size must be >= 1')
  const len = dv.getUint32(HEADER - 4, true)
  if (len === 0 || len > ZKM2_MAX_RECEIPT) throw new ZkmCodecError('receipt length out of range')
  if (b.length !== HEADER + len) throw new ZkmCodecError(b.length < HEADER + len ? 'truncated receipt' : 'trailing bytes')
  return { imageId: b.slice(5, 37), root: b.slice(37, 69), size, queryId: b.slice(73, 105), receipt: b.slice(HEADER) }
}
