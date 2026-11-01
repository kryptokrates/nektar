// verifyMembership — verifier side: L_ZKMem bytes → decode → vkeyHash check →
// root == anchored poseidonRoot → groth16 verify → nullifier-store dedup.
// verifyMembershipAnchored first runs the anchor/SPV check itself so the
// poseidonRoot comes from a verified on-chain anchor — MCP/e2e use this one.
import * as snarkjs from 'snarkjs'
import { readFileSync } from 'node:fs'
import type { Transaction } from '@bsv/sdk'
import { decodeZkMem, ZkCodecError, BN254_R, BN254_Q } from './codec.js'
import type { NullifierStore } from './nullstore.js'
import { queryIdOf, type ZkQuery } from './query.js'
import { dsha256 } from '@mamem/core'
import { decodeMamemAnchor, isValidAnchor } from '@mamem/contracts'

export interface VerifyResult {
  ok: boolean, reason?: string, root?: string, nullifier?: string, anchorTxid?: string
}
export interface AnchoredHead { poseidonRoot: bigint, anchorTxid: string }

// Pinned vkey hash — sha256 of artifacts/vkey.json from the dev ceremony
// (packages/zk/scripts/ceremony.mjs). A proof carrying a different key's hash
// is rejected before any curve math.
export const PINNED_VKEY_HASH = 'e2c6892f94a6077e38fc057bac8f3ecd22e7382f597e38ab3d4e965e8e79c73b'

// vkey cache — loaded once per path; the file's hash MUST equal the pin
const _vkeys = new Map<string, { vkey: any, vkeyHash: Uint8Array }>()
export function loadVkey (vkeyPath: string): { vkey: any, vkeyHash: Uint8Array } {
  if (!_vkeys.has(vkeyPath)) {
    const raw = readFileSync(vkeyPath)
    const vkeyHash = dsha256(raw)
    if (Buffer.from(vkeyHash).toString('hex') !== PINNED_VKEY_HASH) {
      throw new Error(`vkey at ${vkeyPath} does not match the pinned ceremony key (${Buffer.from(vkeyHash).toString('hex')})`)
    }
    _vkeys.set(vkeyPath, { vkey: JSON.parse(raw.toString('utf8')), vkeyHash })
  }
  return _vkeys.get(vkeyPath)!
}

function toSnarkProof (zm: ReturnType<typeof decodeZkMem>) {
  // snarkjs expects projective coords: pi_a/pi_c len-3 with "1",
  // pi_b len-3 with ["1","0"]
  return {
    pi_a: [zm.pi_a[0].toString(), zm.pi_a[1].toString(), '1'],
    pi_b: [
      [zm.pi_b[0][0].toString(), zm.pi_b[0][1].toString()],
      [zm.pi_b[1][0].toString(), zm.pi_b[1][1].toString()],
      ['1', '0']
    ],
    pi_c: [zm.pi_c[0].toString(), zm.pi_c[1].toString(), '1'],
    protocol: 'groth16', curve: 'bn128'
  }
}

/** internal — verify against an already-verified anchored head (tests only) */
export async function verifyMembership (
  bytes: Uint8Array,
  head: AnchoredHead,
  query: ZkQuery,
  vkeyPath: string,
  store: NullifierStore
): Promise<VerifyResult> {
  let zm
  try { zm = decodeZkMem(bytes) } catch (e) { return { ok: false, reason: `decode: ${(e as Error).message}` } }
  const { vkey, vkeyHash } = loadVkey(vkeyPath)
  if (Buffer.from(zm.vkeyHash).toString('hex') !== Buffer.from(vkeyHash).toString('hex')) {
    return { ok: false, reason: 'vkeyHash mismatch' }
  }
  if (zm.root !== head.poseidonRoot) {
    return { ok: false, reason: 'root != anchored poseidonRoot' }
  }
  if (zm.queryId !== queryIdOf(query)) {
    return { ok: false, reason: 'queryId mismatch' }
  }
  const publicSignals = [zm.root.toString(), zm.nullifier.toString(), zm.queryId.toString()]
  const ok = await snarkjs.groth16.verify(vkey, publicSignals, toSnarkProof(zm)).catch(() => false)
  if (!ok) return { ok: false, reason: 'groth16 verify failed' }
  if (store.has(zm.nullifier)) return { ok: false, reason: 'nullifier replay' }
  store.add(zm.nullifier)
  return { ok: true, root: zm.root.toString(), nullifier: zm.nullifier.toString(), anchorTxid: head.anchorTxid }
}

export interface MembershipExpectation {
  shard: string // the expected shard — decodeMamemAnchor(...).shard must match
  minN?: number // optional minimum batch index
  /** when an overlay/lookup is available, check the anchor is on the shard's
   * canonical chain (head or an ancestor). Return true iff canonical. */
  isCanonical?: (txid: string) => boolean | Promise<boolean>
}

/**
 * verifyMembershipAnchored — production path. Decodes the poseidonRoot from a
 * REAL anchor tx, checks its BUMP against the header chain AND binds it to the
 * expected shard (+canonical chain when a lookup is provided), so a prover
 * can't substitute an anchor from their own batch.
 */
export async function verifyMembershipAnchored (
  bytes: Uint8Array,
  anchorTx: Transaction,
  tracker: any, // ChainTracker
  query: ZkQuery,
  vkeyPath: string,
  store: NullifierStore,
  expected: MembershipExpectation
): Promise<VerifyResult> {
  const data = decodeMamemAnchor(anchorTx.outputs[0]!.lockingScript.toHex())
  if (!data || !isValidAnchor(data)) return { ok: false, reason: 'anchor tx output 0 not a mamem anchor' }
  if (expected.shard !== data.shard) return { ok: false, reason: 'anchor shard mismatch' }
  if (expected.minN !== undefined && data.n < expected.minN) return { ok: false, reason: 'anchor shard mismatch' }
  if (expected.isCanonical && !(await expected.isCanonical(anchorTx.id('hex')))) {
    return { ok: false, reason: 'anchor not on canonical shard chain' }
  }
  if (!anchorTx.merklePath) return { ok: false, reason: 'NotMined' }
  const mined = await anchorTx.merklePath.verify(anchorTx.id('hex'), tracker)
  if (!mined) return { ok: false, reason: 'BEEFInvalid: BUMP root not in header chain' }
  const pr = (data as any).poseidonRoot
  if (!pr || (pr as Uint8Array).length !== 32) {
    return { ok: false, reason: 'anchor has no poseidonRoot (legacy/opt-out batch)' }
  }
  const poseidonRoot = BigInt('0x' + Buffer.from(pr).toString('hex'))
  return verifyMembership(bytes, { poseidonRoot, anchorTxid: anchorTx.id('hex') }, query, vkeyPath, store)
}

// combined Π = (membership, access): ZKAccess's transcript.queryId must equal
// the derived queryIdOf(query) and its nonce must be the query's nonce — the
// verifierId is bound inside queryIdOf. Reject any mismatch.
export function verifyCombined (query: ZkQuery, zkTranscript: { queryId: string, nonce: Uint8Array }): boolean {
  return zkTranscript.queryId === queryIdOf(query).toString() &&
    Buffer.from(zkTranscript.nonce).equals(Buffer.from(query.nonce))
}
export { ZkCodecError, BN254_R, BN254_Q }
