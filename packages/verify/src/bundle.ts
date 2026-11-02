// Proof bundle: everything a verifier needs to check one fragment, offline,
// with block headers only.
//
//   leaf →(batch path)→ R_n →(anchor PushDrop field)→ anchor tx
//        →(atomicBEEF + BUMP)→ block header (chainTracker)
//   anchor-chain consistency: chainTail [anchor_0..n] re-verified link by link.
import { Transaction } from '@bsv/sdk'
import { decodeMamemAnchor, isValidAnchor } from '@mamem/contracts'
import { reconstructRoot, verifyProof, chainRp, GENESIS_PREV_RP, eq, toHex, fromHex, heightForLeafCount, type Bytes, type AnchorData } from '@mamem/core'
import type { ChainTracker } from '@mamem/chain'

export interface AnchorLink {
  txid: string
  outputIndex: number
  n: number
  size: number
  R: string // hex
  Rp: string // hex
}

export interface ProofBundle {
  version: 1
  shard: string
  n: number // batch index
  size: number // leaf count in batch
  leaf: string // hex — computeLeaf(fragment)
  leafIndex: number
  path: string[] // sibling hashes, hex, internal byte order
  anchor: AnchorLink
  chainTail: AnchorLink[] // anchors 0..n for consistency verification
  anchorBEEF: number[] // atomic BEEF of the anchor tx (with BUMP)
}

export type VerifyResult =
  | { ok: true, settled: boolean, anchorState: 'broadcast' | 'mined' }
  | { ok: false, reason: 'LeafPathMismatch' | 'AnchorFieldMismatch' | 'BEEFInvalid' | 'NotMined' | 'ChainInconsistent' | 'BadBundle', detail?: string }

/** Build a bundle from a BatchLog record + anchor head + ls history. */
export function buildProofBundle (args: {
  shard: string
  leaf: Bytes
  leafIndex: number
  siblings: Bytes[]
  batchN: number
  batchSize: number
  batchR: Bytes
  anchorTx: Transaction
  chainTail: AnchorLink[]
}): ProofBundle {
  const anchorData = decodeMamemAnchor(args.anchorTx.outputs[0]!.lockingScript.toHex())
  if (!anchorData || !isValidAnchor(anchorData)) throw new Error('anchor tx output 0 is not a mamem anchor')
  const anchor: AnchorLink = {
    txid: args.anchorTx.id('hex'), outputIndex: 0,
    n: anchorData.n, size: anchorData.size, R: toHex(anchorData.R), Rp: toHex(anchorData.Rp)
  }
  return {
    version: 1, shard: anchorData.shard, n: args.batchN, size: args.batchSize,
    leaf: toHex(args.leaf), leafIndex: args.leafIndex,
    path: args.siblings.map(toHex),
    anchor, chainTail: args.chainTail,
    anchorBEEF: args.anchorTx.toAtomicBEEF()
  }
}

/**
 * VerifyFragment — paper §5. Checks, in order:
 * 1. leaf→R_n batch path
 * 2. the anchor tx's PushDrop fields carry that R_n, shard, n, size
 * 3. the anchor tx's BUMP terminates in the header chain (SPV)
 * 4. anchor-chain consistency across chainTail (R'_i links, n contiguous)
 * The verifier needs block headers only — no trust in the bundle's producer.
 */
export async function verifyFragment (bundle: ProofBundle, tracker: ChainTracker): Promise<VerifyResult> {
  // 0) leaf-count binding (ADR-002): index must be inside the batch and the
  // path length must match the committed leaf count — otherwise an odd-batch
  // "duplicate last leaf" or an interior node can be presented as a leaf.
  if (!Number.isInteger(bundle.leafIndex) || bundle.leafIndex < 0 || bundle.leafIndex >= bundle.size) {
    return { ok: false, reason: 'BadBundle', detail: `leafIndex ${bundle.leafIndex} outside batch size ${bundle.size}` }
  }
  if (bundle.path.length !== heightForLeafCount(bundle.size)) {
    return { ok: false, reason: 'BadBundle', detail: `path length ${bundle.path.length} != heightForLeafCount(${bundle.size})` }
  }
  // 1) batch path
  const leaf = fromHex(bundle.leaf)
  const siblings = bundle.path.map(fromHex)
  const recomputedR = reconstructRoot(leaf, { index: bundle.leafIndex, siblings })

  // 2) anchor fields from the BEEF
  let anchorTx: Transaction
  try {
    anchorTx = Transaction.fromAtomicBEEF(bundle.anchorBEEF)
  } catch (e) {
    return { ok: false, reason: 'BadBundle', detail: `BEEF parse: ${(e as Error).message}` }
  }
  const data = decodeMamemAnchor(anchorTx.outputs[0]!.lockingScript.toHex())
  if (!data || !isValidAnchor(data)) return { ok: false, reason: 'BadBundle', detail: 'output 0 not a mamem anchor' }
  if (data.shard !== bundle.shard || data.n !== bundle.n || data.size !== bundle.size) {
    return { ok: false, reason: 'AnchorFieldMismatch', detail: 'shard/n/size mismatch' }
  }
  if (!eq(data.R, recomputedR)) return { ok: false, reason: 'LeafPathMismatch', detail: 'recomputed R_n != anchor R_n' }

  // 3) SPV: BUMP → headers. Mining is settlement, not validity — a broadcast
  // anchor is already anchored, so a missing BUMP does NOT fail verification;
  // it downgrades the result to settled:false. A PRESENT-but-invalid BUMP is a
  // real failure (the bundle claims mining it cannot prove).
  let settled = false
  let anchorState: 'broadcast' | 'mined' = 'broadcast'
  if (anchorTx.merklePath) {
    const mined = await anchorTx.merklePath.verify(anchorTx.id('hex'), tracker as any)
    if (!mined) return { ok: false, reason: 'BEEFInvalid', detail: 'BUMP root not in header chain' }
    settled = true
    anchorState = 'mined'
  }

  // 4) anchor-chain consistency over chainTail
  const tail = bundle.chainTail.slice().sort((a, b) => a.n - b.n)
  const lastLink = tail[tail.length - 1]
  // The last tail entry must EQUAL the on-chain anchor fields decoded from
  // the BEEF — since R'_n recursively commits the whole tail, authenticating
  // the last link authenticates the entire tail.
  if (tail.length === 0 ||
      lastLink!.txid !== bundle.anchor.txid ||
      lastLink!.txid !== anchorTx.id('hex') ||
      lastLink!.n !== data.n || lastLink!.size !== data.size ||
      toHex(fromHex(lastLink!.R)) !== toHex(data.R) ||
      toHex(fromHex(lastLink!.Rp)) !== toHex(data.Rp)) {
    return { ok: false, reason: 'ChainInconsistent', detail: 'chainTail head does not match the on-chain anchor' }
  }
  for (let i = 0; i < tail.length; i++) {
    const link = tail[i]!
    if (link.n !== i) return { ok: false, reason: 'ChainInconsistent', detail: `gap at n=${i}` }
    const prevRp = i === 0 ? GENESIS_PREV_RP : fromHex(tail[i - 1]!.Rp)
    if (!eq(fromHex(link.Rp), chainRp(prevRp, fromHex(link.R), link.size))) {
      return { ok: false, reason: 'ChainInconsistent', detail: `R' link broken at n=${i}` }
    }
  }
  return { ok: true, settled, anchorState }
}
