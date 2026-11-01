// Off-chain STARK verification (WASM build of the RISC Zero receipt verifier,
// runs in Node on any OS). Pure: bytes in → journal out; no network, no state.
import { createRequire } from 'node:module'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { decodeZkm2, ZkmCodecError, type ZkMembershipProof } from './codec.js'

const require = createRequire(import.meta.url)
const wasm = require('../../../stark/verifier.js') as { verify: (receipt: Uint8Array, imageId: Uint8Array) => string }

/** the guest program identity mamem trusts (built reproducibly by `pnpm stark:build`) */
export const PINNED_IMAGE_ID: string = JSON.parse(readFileSync(fileURLToPath(new URL('../../../stark/image-id.json', import.meta.url)), 'utf8')).imageId

export interface Journal { version: number, root: string, size: number, queryId: string }
export type StarkVerdict = { ok: true, journal: Journal, proof: ZkMembershipProof } | { ok: false, reason: string }

const hex = (b: Uint8Array) => Buffer.from(b).toString('hex')

/** decode L_ZKMem v2, check the pinned image id, verify the ZK-STARK, and
 *  require header == journal. Anchoring/SPV/queryId checks are the caller's. */
export function verifyZkm2 (bytes: Uint8Array, pinnedImageId = PINNED_IMAGE_ID): StarkVerdict {
  let p: ZkMembershipProof
  try { p = decodeZkm2(bytes) } catch (e) { return { ok: false, reason: e instanceof ZkmCodecError ? `codec: ${e.message}` : 'codec error' } }
  if (hex(p.imageId) !== pinnedImageId) return { ok: false, reason: 'image id is not the pinned mamem membership program' }
  const r = JSON.parse(wasm.verify(p.receipt, p.imageId)) as { ok: boolean, journal?: Journal, error?: string }
  if (!r.ok || !r.journal) return { ok: false, reason: `stark: ${r.error ?? 'invalid'}` }
  const j = r.journal
  if (j.version !== 1) return { ok: false, reason: `journal version ${j.version}` }
  if (j.root !== hex(p.root) || j.size !== p.size || j.queryId !== hex(p.queryId)) return { ok: false, reason: 'header does not match the proven journal' }
  return { ok: true, journal: j, proof: p }
}
