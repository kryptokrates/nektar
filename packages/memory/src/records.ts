// MemRecords — the record write path and its read-side guards: remember,
// revise/materialize (Δ chains), the App E premise floor, author guard,
// access logging, and prove() (both materialize and recall consume it).
import {
  utf8,
  toHex,
  sha256,
  identityHash,
  diff,
  apply,
  encodeDelta,
  decodeDelta,
  merkleRoot,
  merkleProof,
  type Fragment,
  type Provenance,
  nektarEnv
} from '@nektar/core'
import { verifyProvenance, type LogRecord } from '@nektar/log'
import { buildProofBundle, verifyFragment, type ProofBundle, type VerifyResult } from '@nektar/verify'
import { encryptFragment, shredKey } from '@nektar/crypto'
import * as idbind from '@nektar/identity'
import type { RememberResult } from './types.js'
import { IngestError } from './base.js'
import { MemZk } from './zk.js'

const DELTA_PREFIX = 'delta:'

export abstract class MemRecords extends MemZk {
  /** anchor a model manifest on the __model__ shard and bind it to receipts. */
  async modelManifest(m: Parameters<typeof import('@nektar/reasoning').modelIdentity>[0]) {
    const { modelIdentity } = await import('@nektar/reasoning')
    this.declaredModel = await modelIdentity(m)
    const { seq } = await this.remember('__model__', JSON.stringify({ type: 'model-manifest', ...this.declaredModel }))
    const a = await this.anchorShard('__model__')
    return {
      modelId: this.declaredModel,
      anchor: { shard: '__model__', seq, txid: a?.txid },
      note: 'declared model identity — anchored and bound into subsequent receipts; not a proof of inference'
    }
  }

  /** §8: store a revision as Δ against `baseSeq` (base must be readable as plaintext).
   *  The delta is derived from the base — App E floors its level at the base's. */
  async revise(
    shard: string,
    baseSeq: number,
    newText: string,
    prov?: Partial<Provenance> & { sourcePub?: string; epoch?: number },
    level = 0
  ): Promise<RememberResult> {
    const base = await this.materialize(shard, baseSeq, { requireProof: false })
    return await this.remember(shard, `${DELTA_PREFIX}${baseSeq}:${encodeDelta(diff(base, newText))}`, prov, level, { premiseRefs: [{ shard, seq: baseSeq }] })
  }
  /** §8: rebuild version `seq` by applying Δs along its chain; with
   *  requireProof every link (base and each Δ record) must verify first */
  async materialize(shard: string, seq: number, opts: { requireProof?: boolean } = {}): Promise<string> {
    const recs = await this.store.all(shard)
    const text = async (s: number): Promise<string> => {
      const r = recs.find((x) => x.seq === s)
      if (!r) throw new Error(`materialize: no record ${shard}/${s}`)
      if (opts.requireProof !== false) {
        const pr = await this.prove(shard, s)
        if (!pr.result.ok) throw new Error(`materialize: ${shard}/${s} not provable (${(pr.result as { reason?: string }).reason})`)
      }
      if (r.fragment.E && r.fragment.E.length) throw new Error('materialize: encrypted record — read through recall')
      const d = Buffer.from(r.fragment.d).toString('utf8')
      if (!d.startsWith(DELTA_PREFIX)) return d
      const m = /^delta:(\d+):([\s\S]*)$/.exec(d)!
      if (Number(m[1]) >= s) throw new Error('materialize: delta must reference an earlier record')
      await this.logAccess({ event: 'traverse', shard, seq: s, follows: Number(m[1]) })
      return apply(await text(Number(m[1])), decodeDelta(m[2]))
    }
    return await text(seq)
  }
  /** §19 read-side author check: returns a deny reason or undefined.
   *  Enforces (a) operator-local revocation of the author pubkey and
   *  (b) epoch-window authorship against the shard's owner cert chain —
   *  when the record's author is on the shard's own chain. */
  protected async authorGuard(shard: string, r: LogRecord): Promise<string | undefined> {
    if (r.authorPub && this.isRevoked(r.authorPub)) return 'revoked key'
    if (r.authorPub && r.authorEpoch !== undefined) {
      const ac = await this.authorChain(shard)
      // epoch-window authorship is enforceable only when the author is on the
      // SHARD'S owner chain (a rotated-out or forged-epoch own write is
      // withheld). Foreign authors carry no cert chain — their originator σ
      // still binds them; there is nothing to check an epoch claim against.
      const chainKeys = ac ? new Set([ac.genesisPub, ...ac.certs.map((c) => c.pubkey)]) : null
      if (ac && chainKeys?.has(r.authorPub)) {
        const a = idbind.verifyAuthorship(r.authorPub, r.authorEpoch, r.fragment.p.time, ac.genesisPub, ac.certs)
        if (!a.ok) return `authorship: ${a.reason}`
      }
    }
    return undefined
  }

  /** D6 (App E auditability): optional per-access log — decrypt/traverse events
   *  appended to `__access__` as timestamped records that join the batch flow
   *  and anchor with everything else. OFF by default (`NEKTAR_ACCESS_LOG=1`)
   *  because a readable log of who-read-what leaks the access pattern itself —
   *  enable only where that tradeoff is acceptable. Writes bypass the public
   *  `remember` (no auto-anchor mid-read) and never break a read on failure. */
  protected accessLogEnabled(): boolean {
    return nektarEnv('NEKTAR_ACCESS_LOG') === '1'
  }
  async logAccess(entry: Record<string, unknown>): Promise<void> {
    if (!this.accessLogEnabled()) return
    try {
      await this.log.remember('__access__', {
        d: utf8(JSON.stringify({ type: 'access', at: Date.now(), ...entry })),
        p: { source: 'access-log', time: Math.floor(Date.now() / 1000), signature: new Uint8Array(0), modelId: this.opts.modelId ?? 'unknown' }
      })
    } catch {
      /* audit writes are best-effort — never break a read */
    }
  }

  /** App E traversal constraint: a record derived from cited premises may not
   *  fall below their maximum access level — `premiseRefs` floors the new
   *  record's level at max(cited levels), so a conclusion can't leak into a
   *  lower-clearance read than its most-sensitive input. */
  async premiseLevelFloor(premiseRefs: Array<{ shard: string; seq: number }>): Promise<number> {
    let floor = 0
    for (const r of premiseRefs) {
      const rec = await this.store.bySeq(r.shard, r.seq)
      if (rec !== undefined && (rec.level ?? 0) > floor) floor = rec.level ?? 0
    }
    return floor
  }

  /**
   * remember: encrypt (if agentSeed configured) → append (dedup) → promise.
   * If the batch threshold is reached, closes the batch and anchors it.
   */
  async remember(
    shard: string,
    d: string,
    prov?: Partial<Provenance> & { sourcePub?: string; epoch?: number },
    level = 0,
    opts?: { premiseRefs?: Array<{ shard: string; seq: number }> }
  ): Promise<RememberResult> {
    // App E: derived content inherits the max level of its premises — the
    // caller may ask for higher, never lower.
    if (opts?.premiseRefs?.length) level = Math.max(level, await this.premiseLevelFloor(opts.premiseRefs))
    const p: Provenance = {
      source: prov?.source ?? 'agent',
      time: prov?.time ?? Math.floor(Date.now() / 1000),
      signature: prov?.signature ?? new Uint8Array(0),
      modelId: prov?.modelId ?? this.opts.modelId ?? 'unknown'
    }
    if (prov?.modality !== undefined) {
      if (
        !['deontic', 'epistemic', 'temporal'].includes(prov.modality.kind) ||
        typeof prov.modality.qualifier !== 'string' ||
        prov.modality.qualifier.length === 0
      ) {
        throw new IngestError('modality must be {kind: deontic|epistemic|temporal, qualifier: string}')
      }
      p.modality = prov.modality
    }
    const fragment: Fragment = { d: utf8(d), p }
    // COM-003: the originator signature is checked against the PLAINTEXT,
    // before encryption drops it
    if (prov?.sourcePub !== undefined) {
      if (!verifyProvenance(prov.sourcePub, fragment.d, p)) throw new IngestError('provenance signature invalid')
    } else if (this.opts.requireSignedProvenance) {
      throw new IngestError('unsigned provenance refused')
    }
    // an encrypted write above the held levels could never be decrypted again
    if (this.canEncrypt() && level > (this.opts.maxLevel ?? 0))
      throw new IngestError(`level ${level} exceeds this instance's maxLevel ${this.opts.maxLevel ?? 0}`)
    this.touched.add(shard)
    // encryption runs inside the shard queue (log.remember's prepare hook):
    // the seq the key context binds is the seq actually assigned — a guessed
    // seq could interleave with another writer and produce an undecryptable
    // record. The dedup identity hash still binds the plaintext (computed
    // upstream of prepare).
    const seed = this.canEncrypt() ? (this.opts.agentSeed ?? null) : undefined
    const { rec, promise, duplicate } = await this.log.remember(
      shard,
      fragment,
      {
        level,
        ...(prov?.sourcePub !== undefined ? { authorPub: prov.sourcePub, authorEpoch: prov.epoch ?? 0 } : {})
      },
      seed === undefined
        ? undefined
        : async (frag, seq) => {
            const c = this.ctx(shard, seq, level)
            // wallet custody writes under the hmac scheme; legacy seed custody
            // keeps the BRC-42 derivation (fragmentKeyCandidates orders wallet
            // first, so index 0 IS the write scheme)
            const fragmentKey = (await this.fragmentKeyCandidates(c))[0]
            const hd = sha256(frag.d)
            const ef = encryptFragment(fragmentKey, frag.d, hd, c)
            // E = [2-byte aadLen][aad][nonce|tag|ct] — aad is not secret, storing it
            // lets recall decrypt without re-deriving H(d) (d is dropped).
            const aadLen = Buffer.alloc(2)
            aadLen.writeUInt16BE(ef.aad.length)
            frag.E = Uint8Array.from(Buffer.concat([aadLen, Buffer.from(ef.aad), Buffer.from(ef.ciphertext)]))
            shredKey(fragmentKey)
            // identity hash H(d|p) must bind the PLAINTEXT — computed before d is
            // dropped, otherwise every encrypted record with the same provenance
            // hashes to H(∅|p) and later writes are silently deduplicated away
            frag.hdp = identityHash(frag)
            frag.d = new Uint8Array(0) // plaintext is not retained with E
          }
    )
    if (!duplicate) this.promises.push({ shard, seq: rec.seq, issuedAt: Date.now() })
    const out: RememberResult = {
      shard,
      seq: rec.seq,
      leaf: toHex(rec.leaf),
      hdp: toHex(rec.hdp),
      duplicate,
      level,
      promise: { seq: rec.seq, leaf: promise.leaf, signature: promise.signature }
    }
    if (await this.log.shouldClose(shard)) {
      const anch = await this.anchorShard(shard)
      if (anch) out.anchored = anch
    }
    return out
  }

  /**
   * prove: build a proof bundle for one record and verify it against headers.
   * A lie stays a lie: if the record was never anchored, the receipt says
   * 'unverifiable' — we never claim provenance we cannot show.
   */
  async prove(shard: string, seq: number): Promise<{ bundle?: ProofBundle; result: VerifyResult | { ok: false; reason: 'NotAnchored' | 'NotFound' } }> {
    const rec = await this.store.bySeq(shard, seq)
    if (!rec) return { result: { ok: false, reason: 'NotFound' } }
    // pack-imported record: its proof is the bundle the exporter shipped —
    // verifyFragment re-checks leaf→batch→anchor→headers, zero trust needed
    const ib = this.importedBundles.get(`${shard}:${seq}`)
    if (ib) return { bundle: ib, result: await verifyFragment(ib, this.chain) }
    if (!rec.anchorTxid || rec.batchIndex === undefined) {
      return { result: { ok: false, reason: 'NotAnchored' } }
    }
    // rebuild the batch leaves from records of the same batch
    const batchRecs = await this.store.byBatch(shard, rec.batchIndex)
    const leaves = batchRecs.map((r) => r.leaf)
    const R = merkleRoot(leaves)
    const head = this.anchorChain(shard).getHead()
    // find the anchor tx for this batch: search chain tail then head history
    const link = (this.tails.get(shard) ?? []).find((l) => l.n === rec.batchIndex)
    if (!link || !head) return { result: { ok: false, reason: 'NotAnchored' } }
    const anchorTx = this.anchorTxs.get(link.txid)
    if (!anchorTx) return { result: { ok: false, reason: 'NotAnchored' } }
    this.anchorChain(shard).beefOf(anchorTx, true)
    const bundle = buildProofBundle({
      shard,
      leaf: rec.leaf,
      leafIndex: rec.leafIndex ?? 0,
      siblings: merkleProof(leaves, rec.leafIndex ?? 0).siblings,
      batchN: rec.batchIndex,
      batchSize: leaves.length,
      batchR: R,
      // consistency is proven genesis → this batch's anchor; later links are
      // not part of this record's proof (the verifier requires the tail to end
      // at the proven anchor)
      anchorTx,
      chainTail: (this.tails.get(shard) ?? []).filter((l) => l.n <= rec.batchIndex!)
    })
    const t0 = performance.now()
    const result = await verifyFragment(bundle, this.chain)
    this.metrics.observe('nektar_proof_latency_ms', performance.now() - t0, 'verifyFragment latency')
    return { bundle, result }
  }
}
