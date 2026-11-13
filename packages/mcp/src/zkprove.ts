// zkprove.ts — the entire STARK proving/verification surface extracted from
// NektarMemory: membership, subset-disclosure, policy-derivation and the §13
// combined Π_j verifier, plus private-cite re-verification against consumed
// tombstones. NektarMemory no longer knows STARK exists — the query registry
// stays single-owner in ZkQueryOps via the consume/peek callbacks, and the
// anchor lookup stays in the shared maps passed by reference.
import { toHex, fromHex, merkleProof, nektarEnv } from '@nektar/core'
import {
  proveMembershipStark,
  verifyMembershipAnchoredStark,
  starkQueryId,
  proveDerivationStark,
  verifyDerivationBound,
  buildSubsetProof,
  encodeZkdis,
  decodeZkdis,
  verifySubsetAnchored,
  decodeZkm2,
  decodeZkder,
  verifyCombinedStark
} from '@nektar/zk'
import type { Transaction } from '@bsv/sdk'
import type { Broadcaster } from '@nektar/chain'
import type { AnchorChain } from '@nektar/anchor'
import type { AnchorLink } from '@nektar/verify'
import type { ShardStore, LogRecord } from '@nektar/log'
import type { PolicyDAG } from '@nektar/graph'
import type { Fact } from '@nektar/datalog'
import type { ZkClearance } from './types.js'

export interface ZkProveOpsDeps {
  store: ShardStore
  /** shard → anchor chain tail (canonical-link list) — shared maps are
   *  read-only here; the memory facade stays the single owner/writer */
  tails: ReadonlyMap<string, AnchorLink[]>
  anchorTxs: ReadonlyMap<string, Transaction>
  anchors: ReadonlyMap<string, AnchorChain>
  chain: Broadcaster
  /** single-use consume — ZkQueryOps stays the registry owner */
  consumeZkQuery: (queryIdHex: string) => boolean
  peekZkQuery: (queryIdHex: string) => { nonce: string; verifierId: string } | undefined
  /** consumed-tombstone check — private-cite re-verification needs a query
   *  that ALREADY verified once */
  zkQueryConsumed: (queryIdHex: string) => boolean
  verifyClearance: (caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string) => { ok: boolean; reason?: string }
  policyDag: () => PolicyDAG | undefined
  anchoredPolicyRoot: () => string | undefined
}

export class ZkProveOps {
  constructor(private deps: ZkProveOpsDeps) {}

  /** GOAL-v6 S5 proof sharding (DIS-002; US 12,730,827 B2 under the Open BSV
   *  pledge — BSV only, see NOTICE): default ON, `NEKTAR_PROOF_SHARDING=0`
   *  opts out for patent-cautious operators → 'disabled'. */
  private proofShardingOn(): boolean {
    return nektarEnv('NEKTAR_PROOF_SHARDING') !== '0'
  }

  /** Wright §13 holder side: π ⊢ leaf ∈ the SAME double-SHA-256 batch tree
   *  whose root R_n is anchored on BSV, bound to a verifier-issued queryId.
   *  ZK-STARK (RISC Zero succinct receipt): reveals neither the leaf, its
   *  index nor the path. */
  async proveMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }> {
    if (!/^[0-9a-f]{64}$/.test(queryIdHex)) throw new Error('queryId must be 32-byte hex (issued by the verifier)')
    const rec = await this.deps.store.bySeq(shard, seq)
    if (!rec || rec.batchIndex === undefined) throw new Error('fragment not batched')
    const batch = await this.deps.store.byBatch(shard, rec.batchIndex)
    const leaves = batch.map((r) => r.leaf)
    const index = batch.findIndex((r) => r.seq === seq)
    const { siblings } = merkleProof(leaves, index)
    const { bytes, ms } = await proveMembershipStark({ leaf: rec.leaf, index, size: leaves.length, siblings, queryId: fromHex(queryIdHex) })
    return { bytes, ms }
  }

  /** verifier side: the proof's root must be the R of an SPV-verified anchor of
   *  THIS shard on its canonical chain, and the queryId one we issued (single use). */
  async verifyMembership(shard: string, seq: number, bytes: Uint8Array): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    const rec = await this.deps.store.bySeq(shard, seq)
    if (!rec || rec.batchIndex === undefined) return { ok: false, reason: 'not batched', statement: 'no anchor' }
    const link = (this.deps.tails.get(shard) ?? []).find((l) => l.n === rec.batchIndex)
    const anchorTx = link ? this.deps.anchorTxs.get(link.txid) : undefined
    if (!anchorTx) return { ok: false, reason: 'anchor tx not found', statement: 'no anchor' }
    const tail = this.deps.tails.get(shard) ?? []
    // expected shard = THIS instance's shard id (hash160 of the genesis owner,
    // from our own anchor chain) — never read from the anchor being verified
    await this.deps.anchors.get(shard)?.ready
    const shardId = this.deps.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement: 'no anchor' }
    const res = await verifyMembershipAnchoredStark(
      bytes,
      anchorTx,
      this.deps.chain,
      { consume: this.deps.consumeZkQuery },
      { shard: shardId, isCanonical: (txid: string) => tail.some((l) => l.txid === txid) }
    )
    return {
      ok: res.ok,
      reason: res.reason,
      anchorTxid: res.anchorTxid,
      statement: 'proves some record is in this anchored batch (ZK-STARK, no trusted setup) — not which one, not its content, not truth'
    }
  }

  /** re-verify a committed private-cite proof WITHOUT consuming anything:
   *  runs the full anchored STARK verification with the tombstone registry —
   *  the queryId must already be CONSUMED, i.e. this exact proof verified
   *  once at commit. The shard is resolved from the anchor's own script data
   *  (private cites never carry the shard name); the cite's hdp stays
   *  unverifiable-by-design — ZK hides which leaf it was. */
  async reverifyMembershipBytes(bytes: Uint8Array, anchorTxid: string): Promise<{ ok: boolean; reason?: string; anchorTxid?: string }> {
    const anchorTx = this.deps.anchorTxs.get(anchorTxid)
    if (!anchorTx) return { ok: false, reason: 'committed anchor tx unknown to this instance' }
    const { decodeNektarAnchor } = await import('@nektar/contracts')
    const data = decodeNektarAnchor(anchorTx.outputs[0].lockingScript.toHex())
    if (!data) return { ok: false, reason: 'committed tx output 0 is not a nektar anchor' }
    for (const [, ac] of this.deps.anchors) await ac.ready
    const shardName = [...this.deps.anchors.entries()].find(([, ac]) => ac.getShardId() === data.shard)?.[0]
    if (shardName === undefined) return { ok: false, reason: 'anchor shard is not one of ours' }
    const tail = this.deps.tails.get(shardName) ?? []
    const res = await verifyMembershipAnchoredStark(
      bytes,
      anchorTx,
      this.deps.chain,
      { consume: this.deps.zkQueryConsumed },
      { shard: data.shard, isCanonical: (txid: string) => tail.some((l) => l.txid === txid) }
    )
    return { ok: res.ok, reason: res.reason, anchorTxid: res.anchorTxid }
  }

  /** verifier side (§13 Π_j = (π_j, ZKAccess(ℓ_q ≥ ℓ_j))): the dual commitment
   *  as ONE verifiable object — the L_ZKMem membership STARK, the ZKAccess
   *  clearance and (optionally) a §14 derivation STARK must all carry the SAME
   *  verifier-issued query: transcript.queryId == each STARK journal's queryId
   *  and transcript.nonce == the nonce this verifier issued with it. That is
   *  exactly `verifyCombinedStark`; the individual verifiers then run with the
   *  consume delegated to one final consumeZkQuery (issued-ness + single use
   *  are checked here, artifact validity there). */
  async verifyCombined(
    shard: string,
    seq: number,
    membershipBytes: Uint8Array,
    zk: ZkClearance,
    opts?: { scope?: string; derivationBytes?: Uint8Array; callerPubkey?: string }
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    const statement =
      'Π_j = (π_j, ZKAccess): some record is in this anchored batch AND the clearance bound to the same verifier query proves level ≥ required — reveals no leaf, index, path, level or credential; not which record, not truth'
    const rec = await this.deps.store.bySeq(shard, seq)
    if (!rec || rec.batchIndex === undefined) return { ok: false, reason: 'not batched', statement }
    const tail = this.deps.tails.get(shard) ?? []
    const link = tail.find((l) => l.n === rec.batchIndex)
    const anchorTx = link ? this.deps.anchorTxs.get(link.txid) : undefined
    if (!anchorTx) return { ok: false, reason: 'anchor tx not found', statement }
    await this.deps.anchors.get(shard)?.ready
    const shardId = this.deps.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement }
    const scope = opts?.scope ?? `${shard}/${seq}`
    // cheap header reads first — verify re-checks header == journal byte for byte
    let mHdr
    try {
      mHdr = decodeZkm2(membershipBytes)
    } catch (e) {
      return { ok: false, reason: `codec: ${(e as Error).message}`, statement }
    }
    const queryIdHex = toHex(mHdr.queryId)
    let dHdr
    if (opts?.derivationBytes !== undefined) {
      try {
        dHdr = decodeZkder(opts.derivationBytes)
      } catch (e) {
        return { ok: false, reason: `codec: ${(e as Error).message}`, statement }
      }
    }
    const issued = this.deps.peekZkQuery(queryIdHex)
    if (!issued) return { ok: false, reason: 'proof queryId was not issued by this verifier (or expired)', statement }
    // the stored query MUST recompute to the proof's queryId — binds the
    // registry row to THIS proof; without it an inconsistent row could satisfy
    // the Π check against a different query
    if (toHex(starkQueryId({ nonce: fromHex(issued.nonce), verifierId: issued.verifierId })) !== queryIdHex) {
      return { ok: false, reason: 'issued query does not recompute to the proof queryId', statement }
    }
    if (
      !verifyCombinedStark({ nonce: fromHex(issued.nonce), verifierId: issued.verifierId }, zk.transcript, dHdr ? { queryId: toHex(dHdr.queryId) } : undefined)
    ) {
      return { ok: false, reason: 'Π binding failed: clearance or derivation is not bound to the same verifier query as the membership proof', statement }
    }
    const res = await verifyMembershipAnchoredStark(
      membershipBytes,
      anchorTx,
      this.deps.chain,
      { consume: () => true },
      { shard: shardId, isCanonical: (txid: string) => tail.some((l) => l.txid === txid) }
    )
    if (!res.ok) return { ok: false, reason: res.reason, statement }
    if (opts?.derivationBytes !== undefined) {
      const policyRoot = await this.expectedPolicyRoot(shard)
      if (!policyRoot) return { ok: false, reason: 'no anchored policy root configured', statement }
      const dv = verifyDerivationBound(opts.derivationBytes, { consume: () => true }, policyRoot)
      if (!dv.ok) return { ok: false, reason: `derivation: ${dv.reason}`, statement }
    }
    const clear = this.deps.verifyClearance({ pubkey: opts?.callerPubkey ?? zk.commitment.holder }, zk, scope)
    if (!clear.ok) return { ok: false, reason: `clearance: ${clear.reason}`, statement }
    if (!this.deps.consumeZkQuery(queryIdHex)) return { ok: false, reason: 'queryId expired or consumed mid-verify', statement }
    return { ok: true, anchorTxid: res.anchorTxid, statement }
  }

  /** holder side: a selective inclusion proof for a SUBSET of one batch's
   *  records — per-leaf shard subpaths (K=64-leaf subtrees of the same
   *  anchored tree) plus one shared level-6→root multiproof, encoded as
   *  L_ZKDis v2. The anchored root is untouched; the proof reveals positions
   *  and leaf hashes, never record contents. */
  async proveSubset(
    shard: string,
    seqs: number[]
  ): Promise<{ ok: true; bytes: Uint8Array; batchIndex: number; size: number; indices: number[] } | { ok: false; reason: string }> {
    if (!this.proofShardingOn()) return { ok: false, reason: 'disabled' }
    if (!Array.isArray(seqs) || seqs.length === 0 || !seqs.every((s) => Number.isInteger(s) && s >= 0))
      return { ok: false, reason: 'seqs must be a non-empty list of record seqs' }
    if (new Set(seqs).size !== seqs.length) return { ok: false, reason: 'duplicate seqs' }
    const found = new Map<number, LogRecord>()
    for (const s of seqs) {
      const r = await this.deps.store.bySeq(shard, s)
      if (r !== undefined) found.set(s, r)
    }
    const missing = seqs.filter((s) => !found.has(s))
    if (missing.length > 0) return { ok: false, reason: `no record(s) seq ${missing.join(',')}` }
    const batchIndex = found.get(seqs[0])!.batchIndex
    if (batchIndex === undefined) return { ok: false, reason: 'not batched' }
    if (![...found.values()].every((r) => r.batchIndex === batchIndex))
      return { ok: false, reason: 'seqs span multiple batches — one subset proof covers one batch' }
    const batch = await this.deps.store.byBatch(shard, batchIndex)
    const leaves = batch.map((r) => r.leaf)
    const indices = seqs.map((s) => batch.findIndex((r) => r.seq === s))
    const proof = buildSubsetProof(leaves, indices)
    return { ok: true, bytes: encodeZkdis(proof), batchIndex, size: leaves.length, indices: [...indices].sort((a, b) => a - b) }
  }

  /** verifier side: the proof's claimed batch root locates OUR anchored link
   *  with that R — then the same shard/canonical/SPV checks as
   *  verifyMembership, and the multiproof must fold to the anchored R. */
  async verifySubset(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; size?: number; indices?: number[]; seqs?: number[]; statement: string }> {
    const statement =
      'proves the listed leaf hashes sit at the listed positions in this anchored batch (selective subset disclosure; record contents stay private) — not truth'
    if (!this.proofShardingOn()) return { ok: false, reason: 'disabled', statement }
    let hdr
    try {
      hdr = decodeZkdis(bytes)
    } catch (e) {
      return { ok: false, reason: `codec: ${(e as Error).message}`, statement }
    }
    const rootHex = toHex(hdr.batchRoot)
    const tail = this.deps.tails.get(shard) ?? []
    const link = tail.find((l) => l.R === rootHex)
    const anchorTx = link ? this.deps.anchorTxs.get(link.txid) : undefined
    if (!link || !anchorTx) return { ok: false, reason: 'no anchor of this shard commits that batch root', statement }
    // expected shard = THIS instance's shard id — never read from the anchor
    await this.deps.anchors.get(shard)?.ready
    const shardId = this.deps.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement }
    const res = await verifySubsetAnchored(bytes, anchorTx, this.deps.chain, {
      shard: shardId,
      isCanonical: (txid: string) => tail.some((l) => l.txid === txid)
    })
    const batch = await this.deps.store.byBatch(shard, link.n)
    const seqs = res.indices?.map((i) => batch.find((r) => (r.leafIndex ?? 0) === i)?.seq ?? i)
    return { ok: res.ok, reason: res.reason, anchorTxid: res.anchorTxid, size: res.size, indices: res.indices, seqs, statement }
  }

  /** the policy root the verifier trusts: the configured anchored root when it
   *  equals the recomputed DAG root (same rule as recall's POL-016 gate), else
   *  an anchored `dagroot:<root>` record in `shard` matching the recomputed root. */
  private async expectedPolicyRoot(shard: string): Promise<string | undefined> {
    const dag = this.deps.policyDag()
    if (!dag) return undefined
    const recomputed = toHex(dag.policyRoot())
    const anchored = this.deps.anchoredPolicyRoot()
    if (anchored !== undefined) return anchored === recomputed ? recomputed : undefined
    const hit = (await this.deps.store.all(shard)).find(
      (r) => r.anchorTxid !== undefined && Buffer.from(r.fragment.d).toString('utf8') === `dagroot:${recomputed}`
    )
    return hit ? recomputed : undefined
  }

  /** Wright §14 holder side: a ZK-STARK that the prover knows a committed
   *  derivation path to `query.pred(query.args)` under the anchored policy DAG
   *  — every step applies a rule node Merkle-proven against the policy root.
   *  The instance supplies `fragment(<shard>/<seq>)` facts from its own store
   *  (same as recall); caller EDB facts are private context premises.
   *  Reveals neither the path nor any fact; journal = (policyRoot, conclusionHash, queryId). */
  async proveDerivation(shard: string, query: { pred: string; args: string[]; edb?: Fact[] }, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }> {
    if (!/^[0-9a-f]{64}$/.test(queryIdHex)) throw new Error('queryId must be 32-byte hex (issued by the verifier)')
    const dag = this.deps.policyDag()
    if (!dag) throw new Error('no policy DAG configured on this instance')
    const { derivationPath, nodeHash } = await import('@nektar/datalog')
    const edb = [...(await this.deps.store.all(shard)).map((r) => ({ pred: 'fragment', args: [`${shard}/${r.seq}`] })), ...(query.edb ?? [])]
    const { facts, conflicts } = dag.evaluate(edb)
    if (conflicts.length > 0) throw new Error(`policy conflict on ${conflicts[0].claim} — no derivation attestation`)
    const path = derivationPath(facts, query.pred, query.args)
    if (!path) throw new Error(`${query.pred}(${query.args.join(',')}) not entailed by the policy DAG over this EDB`)
    // snapshot the committed node set — the anchored root covers exactly this list
    const nodeHs = [...dag.nodes.keys()]
    const nodeBytes = nodeHs.map((h) => new Uint8Array(Buffer.from(h, 'hex')))
    const layerOf = (ruleId: string) => dag.layers.find((l) => l.rules.some((r) => r.id === ruleId))?.name
    const steps = path.steps.map((s) => {
      const rule = dag.rules.find((r) => r.id === s.ruleId)
      const tau = dag.ruleNode.get(s.ruleId)
      if (!rule || !tau || layerOf(s.ruleId) === undefined) throw new Error(`step rule ${s.ruleId} is not a committed DAG rule`)
      const ruleIndex = nodeHs.indexOf(tau)
      return {
        rule: { id: rule.id, layer: layerOf(s.ruleId)!, head: rule.head, body: rule.body },
        ruleIndex,
        ruleSiblings: merkleProof(nodeBytes, ruleIndex).siblings.map((sib) => toHex(sib)),
        inputs: s.inputs.map((i) => {
          if (i.fromStep !== null) return { fact: i.fact, fromStep: i.fromStep }
          const h = toHex(nodeHash(i.fact))
          const ci = nodeHs.indexOf(h)
          return ci >= 0
            ? { fact: i.fact, fromStep: null, committedIndex: ci, committedSiblings: merkleProof(nodeBytes, ci).siblings.map((sib) => toHex(sib)) }
            : { fact: i.fact, fromStep: null }
        }),
        output: s.output
      }
    })
    const { bytes, ms } = await proveDerivationStark({ nodeCount: nodeHs.length, policyRoot: dag.policyRoot(), queryId: fromHex(queryIdHex), steps })
    return { bytes, ms }
  }

  /** verifier side: the proven policyRoot must be this verifier's anchored
   *  policy DAG root and the queryId one it issued (single use). */
  async verifyDerivation(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; policyRoot?: string; conclusionHash?: string; statement: string }> {
    const statement =
      'proves a committed derivation path under the anchored policy DAG (ZK-STARK, no trusted setup) — not which conclusion, not the premises, not truth'
    const expected = await this.expectedPolicyRoot(shard)
    if (!expected) return { ok: false, reason: 'no anchored policy DAG root (policyDag unset, root mismatch, or dagroot record not anchored)', statement }
    const res = verifyDerivationBound(bytes, { consume: this.deps.consumeZkQuery }, expected)
    return { ok: res.ok, reason: res.reason, policyRoot: res.journal?.policyRoot, conclusionHash: res.journal?.conclusionHash, statement }
  }
}
