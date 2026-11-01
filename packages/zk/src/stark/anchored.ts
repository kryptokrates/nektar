// verifyMembershipAnchoredStark — production verifier (Wright §13 + App. C):
// the proven root MUST be the R_n of a real, SPV-verified mamem anchor of the
// EXPECTED shard on its canonical chain, and the queryId MUST be one this
// verifier issued and has not seen before. Order: cheap checks first.
import type { Transaction } from '@bsv/sdk'
import { dsha256, utf8 } from '@mamem/core'
import { decodeMamemAnchor, isValidAnchor } from '@mamem/contracts'
import { verifyZkm2, PINNED_IMAGE_ID } from './verify.js'
import { decodeZkm2 } from './codec.js'
import { decodeZkdis, verifySubsetProof } from './subproof.js'

/** verifier challenge: queryId = dsha256('mamem:zkq:v2' ‖ nonce ‖ verifierId) (32 bytes, no field reduction) */
export interface StarkQuery { nonce: Uint8Array, verifierId: string }
export function starkQueryId (q: StarkQuery): Uint8Array {
  return dsha256(Buffer.concat([utf8('mamem:zkq:v2'), q.nonce, utf8(q.verifierId)]))
}

/** combined Π = (membership, ZKAccess [, §14 derivation]) over ONE verifier
 *  query (§13/§14 shared commitment): the access transcript — and, when given,
 *  the derivation journal — must carry this query's id and nonce. */
export function verifyCombinedStark (
  q: StarkQuery,
  accessTranscript: { queryId: string, nonce: Uint8Array },
  derivationJournal?: { queryId: string }
): boolean {
  return accessTranscript.queryId === Buffer.from(starkQueryId(q)).toString('hex') &&
    Buffer.from(accessTranscript.nonce).equals(Buffer.from(q.nonce)) &&
    (derivationJournal === undefined || derivationJournal.queryId === Buffer.from(starkQueryId(q)).toString('hex'))
}

/** single-use query registry (persisted by the caller) */
export interface QueryRegistry {
  /** true iff this queryId was issued here, is unexpired and unused; marks it used */
  consume: (queryIdHex: string) => boolean
}

export interface StarkExpectation {
  shard: string // hash160(genesis owner) of OUR shard — never read from the proof
  isCanonical?: (txid: string) => boolean | Promise<boolean>
}
export interface StarkVerifyResult { ok: boolean, reason?: string, anchorTxid?: string, root?: string, size?: number }

export async function verifyMembershipAnchoredStark (
  bytes: Uint8Array,
  anchorTx: Transaction,
  tracker: any, // ChainTracker
  queries: QueryRegistry,
  expected: StarkExpectation,
  pinnedImageId = PINNED_IMAGE_ID
): Promise<StarkVerifyResult> {
  const txid = anchorTx.id('hex')
  const data = decodeMamemAnchor(anchorTx.outputs[0]!.lockingScript.toHex())
  if (!data || !isValidAnchor(data)) return { ok: false, reason: 'anchor tx output 0 is not a mamem anchor' }
  if (data.shard !== expected.shard) return { ok: false, reason: 'anchor belongs to another shard' }
  if (expected.isCanonical && !(await expected.isCanonical(txid))) return { ok: false, reason: 'anchor not on the canonical shard chain' }
  if (!anchorTx.merklePath) return { ok: false, reason: 'NotMined' }
  if (!(await anchorTx.merklePath.verify(txid, tracker))) return { ok: false, reason: 'BEEFInvalid: BUMP root not in header chain' }
  // header pre-check before the (more expensive) STARK verification
  let hdr
  try { hdr = decodeZkm2(bytes) } catch (e) { return { ok: false, reason: `codec: ${(e as Error).message}` } }
  const R = Buffer.from(data.R).toString('hex')
  if (Buffer.from(hdr.root).toString('hex') !== R) return { ok: false, reason: 'proven root is not the anchored R' }
  if (hdr.size !== data.size) return { ok: false, reason: 'proven size is not the anchored batch size' }
  const v = verifyZkm2(bytes, pinnedImageId)
  if (!v.ok) return { ok: false, reason: v.reason }
  // last: single use (only consumed when everything else holds)
  if (!queries.consume(v.journal.queryId)) return { ok: false, reason: 'queryId not issued by this verifier, expired, or already used' }
  return { ok: true, anchorTxid: txid, root: R, size: data.size }
}

export interface SubsetVerifyResult { ok: boolean, reason?: string, anchorTxid?: string, root?: string, size?: number, indices?: number[] }

/** DIS-002 verifier (GOAL-v6 S5): an L_ZKDis v2 subset proof is accepted iff
 *  its claimed batch root and size equal R_n/size of a real, SPV-verified
 *  mamem anchor of the EXPECTED shard on its canonical chain, and every
 *  disclosed leaf folds through its shard subpath + the shared top multiproof
 *  to that root. No query binding — the proof reveals positions and leaf
 *  hashes; binding the disclosed records to a question is the caller's. */
export async function verifySubsetAnchored (
  bytes: Uint8Array,
  anchorTx: Transaction,
  tracker: any, // ChainTracker
  expected: StarkExpectation
): Promise<SubsetVerifyResult> {
  const txid = anchorTx.id('hex')
  const data = decodeMamemAnchor(anchorTx.outputs[0]!.lockingScript.toHex())
  if (!data || !isValidAnchor(data)) return { ok: false, reason: 'anchor tx output 0 is not a mamem anchor' }
  if (data.shard !== expected.shard) return { ok: false, reason: 'anchor belongs to another shard' }
  if (expected.isCanonical && !(await expected.isCanonical(txid))) return { ok: false, reason: 'anchor not on the canonical shard chain' }
  if (!anchorTx.merklePath) return { ok: false, reason: 'NotMined' }
  if (!(await anchorTx.merklePath.verify(txid, tracker))) return { ok: false, reason: 'BEEFInvalid: BUMP root not in header chain' }
  // header pre-check before the multiproof reconstruction
  let hdr
  try { hdr = decodeZkdis(bytes) } catch (e) { return { ok: false, reason: `codec: ${(e as Error).message}` } }
  const R = Buffer.from(data.R).toString('hex')
  if (Buffer.from(hdr.batchRoot).toString('hex') !== R) return { ok: false, reason: 'proven root is not the anchored R' }
  if (hdr.size !== data.size) return { ok: false, reason: 'proven size is not the anchored batch size' }
  const v = verifySubsetProof(hdr)
  if (!v.ok) return { ok: false, reason: v.reason }
  return { ok: true, anchorTxid: txid, root: R, size: data.size, indices: v.indices }
}
