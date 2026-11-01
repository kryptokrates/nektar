// MamemMemory — the product service the MCP server wraps.
// Wires log (append/batch) → anchor (head UTXO per shard) → overlay (submit)
// → verify (proof bundles) → crypto (BRC-42/AES-GCM), all behind the chain
// interfaces so it runs identically on the mock chain and regtest.
import { PrivateKey, Transaction } from '@bsv/sdk'
import {
  utf8, fromHex, toHex, sha256, eq, hash160,
  computeLeaf, merkleRoot, merkleProof, identityHash, diff, apply, encodeDelta, decodeDelta,
  type Fragment, type Bytes, type Provenance
} from '@mamem/core'
import { BatchLog, MemoryShardStore, verifyProvenance, signCheckpoint, PromiseMonitor, type ShardStore, type LogRecord, type Checkpoint } from '@mamem/log'
import { Metrics } from './metrics.js'

const DELTA_PREFIX = 'delta:'

/** write refused at ingest (COM-003) */
export class IngestError extends Error {}
import { AnchorChain } from '@mamem/anchor'
import { AutomatonChain, type AutomatonDef, type AutomatonHead } from '@mamem/automaton'
import { createMamemOverlay, type MamemOverlay } from '@mamem/overlay'
import { buildProofBundle, verifyFragment, type ProofBundle, type AnchorLink, type VerifyResult } from '@mamem/verify'
import { deriveFragmentKey, deriveLevelKey, encryptFragment, decryptFragment, shredKey, type FragmentContext } from '@mamem/crypto'
import type { Broadcaster, ChainTracker, Funder } from '@mamem/chain'
import * as idbind from '@mamem/identity'
import { proveMembershipStark, verifyMembershipAnchoredStark, starkQueryId } from '@mamem/zk'

export interface MamemMemoryOptions {
  signingKey?: PrivateKey
  shardKeys?: Map<string, PrivateKey>
  agentSeed?: PrivateKey // enables encryption when set
  maxLevel?: number // highest access level this seed may hold
  burstLimit?: number
  batchPolicy?: import('@mamem/log').ClosePolicy
  overlay?: MamemOverlay // if set, anchors are also submitted to tm_mamem
  modelId?: string // stamped into receipts
  /** Instance-level Cedar enforcement (POL-010/013): when set, EVERY recall and
   * disclose MUST pass Cedar ALLOW against this policy, and the policy text
   * must hash to `anchoredPolicySetHash` (decoded from the anchored policy-set
   * record — never a request field). No bypass possible. */
  policy?: { policyText: string, anchoredPolicySetHash: string }
  /** POL-004/016: an anchored policy DAG configured on the instance. When set,
   * reads are allowed only when Cedar ALLOWs AND the DAG derives permit with
   * no conflict; the receipt carries the DAG's own root + derivation hash.
   * `anchoredPolicyRoot` is the root decoded from the anchored DAG-root
   * record — recall recomputes dag.policyRoot() and DENIES on mismatch. */
  policyDag?: import('@mamem/graph').PolicyDAG
  anchoredPolicyRoot?: string
  /** ZKA: authority pubkeys trusted to issue clearance. Level>0 reads are
   *  refused when unset — self-issued clearance is never accepted. */
  clearanceAuthorities?: string[]
  /** COM-003: refuse writes without a valid originator signature (sourcePub) */
  requireSignedProvenance?: boolean
  /** CHN-006: confirmations after which an anchor counts as final (default 1) */
  finalityDepth?: number
  /** LOG-005: maximum merge delay for inclusion promises (default 10 min) */
  maxMergeDelayMs?: number
  /** persistence for challenges, ZK transcripts, issued ZK queries, revocations
   *  (default: in-memory, lost on restart) */
  securityStore?: SecurityStateStore
  /** persistence for anchor heads/tails/checkpoints (restart-safe, no fork) */
  stateStore?: MemoryStateStore
}

/** persisted anchor state: per shard the head, the succession, checkpoints,
 *  rotation certs and every anchor tx as atomic BEEF (with BUMP once mined) */
export interface PersistedMemoryState {
  version: 1
  shards: Record<string, { headTxid: string, confirmed: boolean, tails: AnchorLink[], checkpoints: Checkpoint[], certs: import('@mamem/identity').RotationCert[], anchors: Record<string, string> }>
  promises: Array<{ shard: string, seq: number, issuedAt: number }>
}
export interface MemoryStateStore { load: () => PersistedMemoryState | undefined, save: (s: PersistedMemoryState) => void }

/** pluggable persistence for replay-protection state (see securitystore.ts) */
export interface SecurityStateStore {
  load: () => { challenges: Array<[string, number]>, zkSeen: string[], zkQueries: Array<[string, number]>, revocations: string[] }
  save: (s: ReturnType<SecurityStateStore['load']>) => void
}

/** caller identity: binding + answer to a challenge issued by this instance */
export interface CallerIdentity { pubkey: string, binding: import('@mamem/identity').IdentityBinding, challengeNonce: string, challengeSig: string }
/** clearance presentation: PUBLIC commitment + proof + scoped transcript */
export interface ZkClearance {
  commitment: import('@mamem/identity').ClearanceCommitment
  proof: import('@mamem/identity').RangeProof | import('@mamem/identity').RangeProofBP
  transcript: import('@mamem/identity').ZkTranscript
  requiredLevel: bigint
}

export interface RememberResult {
  shard: string
  seq: number
  leaf: string
  hdp: string
  duplicate: boolean
  promise: { seq: number, leaf: string, signature: string }
  anchored?: { txid: string, n: number }
}

export interface AnswerReceipt {
  answer: string
  fragmentRefs: Array<{ shard: string, seq: number, hdp: string, anchorTxid?: string }>
  checkResult: 'verified' | 'unverifiable' | 'failed'
  /** Always: proofs establish inclusion+integrity, never truth of content. */
  truthNote: string
  modelId: string
  /** ZKAccess result for level-gated reads (ZKA-001..006). */
  zkaccess?: 'verified' | 'rejected' | 'not-required'
  /** Cedar decision for the read (POL-010/013). */
  cedar?: 'allow' | 'deny' | 'hash-mismatch' | 'not-required'
  /** POL-016: when a policy DAG gates the read — its anchored root, the
   * derivation hash and the dag decision summary. */
  dag?: { policyRoot: string, derivationHash?: string, state: string }
}

export class MamemMemory {
  private log: BatchLog
  private anchors = new Map<string, AnchorChain>()
  private tails = new Map<string, AnchorLink[]>()
  private anchorTxs = new Map<string, import('@bsv/sdk').Transaction>()

  constructor (
    private chain: ChainTracker & Broadcaster,
    private funder: Funder,
    private fundKey: PrivateKey,
    private opts: MamemMemoryOptions = {},
    private store: ShardStore = new MemoryShardStore()
  ) {
    this.logKey = opts.signingKey ?? PrivateKey.fromRandom()
    this.log = new BatchLog(store, this.logKey, opts.batchPolicy ?? { maxLeaves: 16 })
    this.restoreSecurityState()
    this.restoreState()
  }
  private logKey: PrivateKey
  /** the log signing key's pubkey (verifies promises and checkpoints) */
  logPublicKey (): string { return this.logKey.toPublicKey().toString() }

  private shardKey (shard: string): PrivateKey {
    if (!this.opts.shardKeys) this.opts.shardKeys = new Map()
    if (!this.opts.shardKeys.has(shard)) this.opts.shardKeys.set(shard, PrivateKey.fromRandom())
    return this.opts.shardKeys.get(shard)!
  }

  private anchorChain (shard: string): AnchorChain {
    if (!this.anchors.has(shard)) {
      this.anchors.set(shard, new AnchorChain(shard, this.shardKey(shard), this.funder, this.fundKey, this.chain, { burstLimit: this.opts.burstLimit }))
    }
    return this.anchors.get(shard)!
  }

  /** the shard's current anchor head (undefined before the first anchor) */
  anchorHead (shard: string) { return this.anchors.get(shard)?.getHead() }
  /** an anchor tx by txid (as broadcast, with BUMP when attached) */
  anchorTx (txid: string) { return this.anchorTxs.get(txid) }
  /** the shard's log records (read view — used by prove/disclose tests) */
  async records (shard: string) { return await this.store.all(shard) }
  /** the shard's latest anchor link (tail of the on-chain succession) */
  tailLink (shard: string) { return this.tails.get(shard)?.at(-1) }
  /** anchors still waiting for their BUMP */
  unprovenAnchors (): Array<{ shard: string, txid: string }> {
    return this.shards().flatMap(s => (this.tails.get(s) ?? []).filter(l => !this.anchorTxs.get(l.txid)?.merklePath).map(l => ({ shard: s, txid: l.txid })))
  }
  /** attach a BUMP to an anchor (must prove THIS txid) and mark the chain confirmed */
  attachProof (shard: string, txid: string, mp: import('@bsv/sdk').MerklePath): void {
    const tx = this.anchorTxs.get(txid)
    if (!tx) throw new Error(`attachProof: unknown anchor ${txid}`)
    if (!mp.path[0]?.some(n => n.txid === true && n.hash === txid)) throw new Error('attachProof: BUMP does not contain this txid')
    tx.merklePath = mp
    this.confirmAnchor(shard, txid)
  }
  /** shards this instance has written or anchored */
  shards (): string[] { return [...new Set([...this.anchors.keys(), ...this.touched])] }
  /** every anchor link of the shard, genesis first (read-only copy) */
  anchorLinks (shard: string): AnchorLink[] { return [...(this.tails.get(shard) ?? [])] }

  /** rotate a shard's owner key on-chain (rotation cert emitted by AnchorChain) */
  async rotateShard (shard: string, newKey: PrivateKey): Promise<{ txid: string, n: number }> {
    const ac = this.anchors.get(shard)
    if (!ac) throw new Error(`no anchor for shard ${shard}`)
    const r = await ac.rotate(newKey)
    this.opts.shardKeys?.set(shard, newKey)
    this.anchorTxs.set(r.tx.id('hex'), r.tx)
    this.persistState()
    return { txid: r.tx.id('hex'), n: r.head.data.n }
  }

  /** revocation registry (IDN-007): SMT keyed by derive_key(pubkey); its root
   *  is anchored with every revoke event */
  readonly revocations = new idbind.Smt()
  private revokedPubs: string[] = []
  /** revoke a 33-byte pubkey: SMT key = derive_key(pubkey); persisted */
  revoke (pubHex: string): Uint8Array {
    const pk = fromHex(pubHex)
    if (pk.length !== 33) throw new Error('revoke: pubkey must be a 33-byte compressed public key')
    const root = this.revocations.insert(idbind.deriveKey(pk), sha256(utf8('revoked')))
    if (!this.revokedPubs.includes(pubHex)) this.revokedPubs.push(pubHex)
    this.persistSecurityState()
    return root
  }
  isRevoked (pubHex: string): boolean { return this.revocations.has(idbind.deriveKey(fromHex(pubHex))) }

  // ── persisted replay-protection state (challenges, transcripts, issued ZK queries, revocations)
  /** ZK membership queries issued by THIS verifier: queryId hex → expiry (single use) */
  private zkQueries = new Map<string, number>()
  private persistSecurityState (): void {
    this.opts.securityStore?.save({
      challenges: [...this.challenges].filter(([, exp]) => exp >= Date.now()),
      zkSeen: [...this.zkSeen],
      zkQueries: [...this.zkQueries].filter(([, exp]) => exp >= Date.now()),
      revocations: [...this.revokedPubs]
    })
  }
  private restoreSecurityState (): void {
    const s = this.opts.securityStore?.load()
    if (!s) return
    for (const [n, exp] of s.challenges) this.challenges.set(n, exp)
    for (const k of s.zkSeen) this.zkSeen.add(k)
    for (const [q, exp] of s.zkQueries ?? []) this.zkQueries.set(q, exp)
    for (const pub of s.revocations) { this.revocations.insert(idbind.deriveKey(fromHex(pub)), sha256(utf8('revoked'))); this.revokedPubs.push(pub) }
  }
  /** verifier side: issue a single-use ZK membership query (default 10 min).
   *  queryId = dsha256('mamem:zkq:v2' ‖ nonce ‖ verifierId); the prover binds it into the STARK journal. */
  issueZkQuery (verifierId: string, ttlMs = 600_000): { nonce: string, verifierId: string, queryId: string } {
    const nonce = idbind.issueChallenge()
    const queryId = toHex(starkQueryId({ nonce: fromHex(nonce), verifierId }))
    this.zkQueries.set(queryId, Date.now() + ttlMs)
    this.persistSecurityState()
    return { nonce, verifierId, queryId }
  }
  private consumeZkQuery = (queryIdHex: string): boolean => {
    const exp = this.zkQueries.get(queryIdHex)
    if (exp === undefined || exp < Date.now()) return false
    this.zkQueries.delete(queryIdHex)
    this.persistSecurityState()
    return true
  }

  // ── P14 full-mode automata (AUT-005): one Rúnar MamemAutomaton UTXO per id,
  // funded by the instance funder, owned by its own key.
  private automata = new Map<string, AutomatonChain>()
  async automatonDeploy (id: string, def: AutomatonDef, ownerKey = PrivateKey.fromRandom()): Promise<AutomatonHead> {
    if (this.automata.has(id)) throw new Error(`automaton ${id} already deployed`)
    const ac = new AutomatonChain(def, ownerKey, this.funder, this.fundKey, this.chain)
    const h = await ac.deploy()
    this.automata.set(id, ac)
    return h
  }
  automaton (id: string): AutomatonChain {
    const ac = this.automata.get(id)
    if (!ac) throw new Error(`no automaton ${id}`)
    return ac
  }

  private ctx (shard: string, seq: number, level: number): FragmentContext {
    return { t: 'mamem', psi: shard, rho: 'rec', pi: String(seq), l: level }
  }

  /** Wright §13 holder side: π ⊢ leaf ∈ the SAME double-SHA-256 batch tree whose
   *  root R_n is anchored on BSV, bound to a verifier-issued queryId. ZK-STARK
   *  (RISC Zero succinct receipt): reveals neither the leaf, its index nor the path. */
  async proveMembership (shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array, ms: number }> {
    if (!/^[0-9a-f]{64}$/.test(queryIdHex)) throw new Error('queryId must be 32-byte hex (issued by the verifier)')
    const recs = await this.store.all(shard)
    const rec = recs.find(r => r.seq === seq)
    if (!rec || rec.batchIndex === undefined) throw new Error('fragment not batched')
    const batch = recs.filter(r => r.batchIndex === rec.batchIndex).sort((a, b) => (a.leafIndex ?? 0) - (b.leafIndex ?? 0))
    const leaves = batch.map(r => r.leaf)
    const index = batch.findIndex(r => r.seq === seq)
    const { siblings } = merkleProof(leaves, index)
    const { bytes, ms } = await proveMembershipStark({ leaf: rec.leaf, index, size: leaves.length, siblings, queryId: fromHex(queryIdHex) })
    return { bytes, ms }
  }

  /** verifier side: the proof's root must be the R of an SPV-verified anchor of
   *  THIS shard on its canonical chain, and the queryId one we issued (single use). */
  async verifyMembership (shard: string, seq: number, bytes: Uint8Array): Promise<{ ok: boolean, reason?: string, anchorTxid?: string, statement: string }> {
    const rec = (await this.store.all(shard)).find(r => r.seq === seq)
    if (!rec || rec.batchIndex === undefined) return { ok: false, reason: 'not batched', statement: 'no anchor' }
    const link = (this.tails.get(shard) ?? []).find(l => l.n === rec.batchIndex)
    const anchorTx = link ? this.anchorTxs.get(link.txid) : undefined
    if (!anchorTx) return { ok: false, reason: 'anchor tx not found', statement: 'no anchor' }
    const tail = this.tails.get(shard) ?? []
    // expected shard = THIS instance's shard id (hash160 of the genesis owner,
    // from our own anchor chain) — never read from the anchor being verified
    const shardId = this.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement: 'no anchor' }
    const res = await verifyMembershipAnchoredStark(bytes, anchorTx, this.chain, { consume: this.consumeZkQuery },
      { shard: shardId, isCanonical: (txid: string) => tail.some(l => l.txid === txid) })
    return { ok: res.ok, reason: res.reason, anchorTxid: res.anchorTxid, statement: 'proves some record is in this anchored batch (ZK-STARK, no trusted setup) — not which one, not its content, not truth' }
  }

  /**
   * remember: encrypt (if agentSeed configured) → append (dedup) → promise.
   * If the batch threshold is reached, closes the batch and anchors it.
   */
  /** §8: store a revision as Δ against `baseSeq` (base must be readable as plaintext) */
  async revise (shard: string, baseSeq: number, newText: string, prov?: Partial<Provenance> & { sourcePub?: string, epoch?: number }, level = 0): Promise<RememberResult> {
    const base = await this.materialize(shard, baseSeq, { requireProof: false })
    return await this.remember(shard, `${DELTA_PREFIX}${baseSeq}:${encodeDelta(diff(base, newText))}`, prov, level)
  }
  /** §8: rebuild version `seq` by applying Δs along its chain; with
   *  requireProof every link (base and each Δ record) must verify first */
  async materialize (shard: string, seq: number, opts: { requireProof?: boolean } = {}): Promise<string> {
    const recs = await this.store.all(shard)
    const text = async (s: number): Promise<string> => {
      const r = recs.find(x => x.seq === s)
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
      return apply(await text(Number(m[1])), decodeDelta(m[2]!))
    }
    return await text(seq)
  }
  /** §19: was record seq written by the key of its recorded epoch, inside that key's window? */
  async authorship (shard: string, seq: number, genesisPub: string, certs: import('@mamem/identity').RotationCert[]) {
    const r = (await this.store.all(shard)).find(x => x.seq === seq)
    if (!r?.authorPub || r.authorEpoch === undefined) return { ok: false, reason: 'record carries no verified author epoch' }
    return idbind.verifyAuthorship(r.authorPub, r.authorEpoch, r.fragment.p.time, genesisPub, certs)
  }

  async remember (shard: string, d: string, prov?: Partial<Provenance> & { sourcePub?: string, epoch?: number }, level = 0): Promise<RememberResult> {
    const p: Provenance = {
      source: prov?.source ?? 'agent',
      time: prov?.time ?? Math.floor(Date.now() / 1000),
      signature: prov?.signature ?? new Uint8Array(0),
      modelId: prov?.modelId ?? this.opts.modelId ?? 'unknown'
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
    if (this.opts.agentSeed && level > (this.opts.maxLevel ?? 0)) throw new IngestError(`level ${level} exceeds this instance's maxLevel ${this.opts.maxLevel ?? 0}`)
    this.touched.add(shard)
    // provisional seq for ctx = next seq (identity hash is checked inside remember)
    const seqGuess = await this.store.nextSeq(shard)
    if (this.opts.agentSeed) {
      const c = this.ctx(shard, seqGuess, level)
      const { fragmentKey } = deriveFragmentKey(deriveLevelKey(this.opts.agentSeed!, level), c)
      const hd = sha256(fragment.d)
      const ef = encryptFragment(fragmentKey, fragment.d, hd, c)
      // E = [2-byte aadLen][aad][nonce|tag|ct] — aad is not secret, storing it
      // lets recall decrypt without re-deriving H(d) (d is dropped).
      const aadLen = Buffer.alloc(2); aadLen.writeUInt16BE(ef.aad.length)
      fragment.E = Uint8Array.from(Buffer.concat([aadLen, Buffer.from(ef.aad), Buffer.from(ef.ciphertext)]))
      shredKey(fragmentKey)
      // identity hash H(d|p) must bind the PLAINTEXT — computed before d is
      // dropped, otherwise every encrypted record with the same provenance
      // hashes to H(∅|p) and later writes are silently deduplicated away
      fragment.hdp = identityHash(fragment)
      fragment.d = new Uint8Array(0) // plaintext is not retained with E
    }
    // level (Cedar/ZK read gate) and author epoch are stored WITH the record
    const { rec, promise, duplicate } = await this.log.remember(shard, fragment, {
      level, ...(prov?.sourcePub !== undefined ? { authorPub: prov.sourcePub, authorEpoch: prov.epoch ?? 0 } : {})
    })
    if (!duplicate) this.promises.push({ shard, seq: rec.seq, issuedAt: Date.now() })
    const out: RememberResult = {
      shard, seq: rec.seq, leaf: toHex(rec.leaf), hdp: toHex(rec.hdp), duplicate,
      promise: { seq: rec.seq, leaf: promise.leaf, signature: promise.signature }
    }
    if (await this.log.shouldClose(shard)) {
      const anch = await this.anchorShard(shard)
      if (anch) out.anchored = anch
    }
    return out
  }

  /** Close the pending batch of a shard and broadcast the anchor. */
  async anchorShard (shard: string): Promise<{ txid: string, n: number } | null> {
    const batch = await this.log.closeBatch(shard)
    if (!batch) return null
    let res
    try { res = await this.anchorChain(shard).anchor(batch.size, batch.R) } catch (e) {
      this.metrics.inc('mamem_anchor_failures_total', { shard }, 1, 'anchor broadcasts that failed')
      throw e
    }
    this.metrics.inc('mamem_anchors_total', {}, 1, 'anchors broadcast')
    const txid = res.tx.id('hex')
    this.anchorTxs.set(txid, res.tx)
    await this.log.markAnchored(shard, batch.n, txid)
    // record the link for chain tails / proofs
    const link: AnchorLink = {
      txid, outputIndex: 0, n: res.head.data.n, size: res.head.data.size,
      R: toHex(res.head.data.R), Rp: toHex(res.head.data.Rp)
    }
    const tail = this.tails.get(shard) ?? []
    tail.push(link); this.tails.set(shard, tail)
    // LOG-007: the log's signed checkpoint for this anchor
    const cps = this.cps.get(shard) ?? []
    cps.push(signCheckpoint(this.logKey, {
      logId: this.anchorChain(shard).getShardId(), size: tail.reduce((s, l) => s + l.size, 0), n: link.n,
      root: link.R, chainedRoot: link.Rp, time: Date.now(), anchorTxid: txid
    }))
    this.cps.set(shard, cps)
    this.persistState()
    if (this.opts.overlay) {
      await this.opts.overlay.engine.submit({ beef: this.anchorChain(shard).beefOf(res.tx), topics: ['tm_mamem'] }, undefined, 'current-tx')
    }
    return { txid, n: res.head.data.n }
  }

  private cps = new Map<string, Checkpoint[]>()
  private touched = new Set<string>()
  private promises: Array<{ shard: string, seq: number, issuedAt: number }> = []
  /** OPS-002 metrics (Prometheus text via metrics.render()) */
  readonly metrics = (() => {
    const m = new Metrics()
    m.gauge('mamem_unconfirmed_anchors', () => [...this.tails.values()].flat().filter(l => !this.anchorTxs.get(l.txid)?.merklePath).length, 'anchors without a BUMP yet')
    m.gauge('mamem_anchor_lag_ms', () => { const cov = this.covered(); const p = this.promises.filter(x => !cov(x)); return p.length ? Date.now() - Math.min(...p.map(x => x.issuedAt)) : 0 }, 'age of the oldest promise not yet checkpointed')
    m.gauge('mamem_promise_violations', () => this.promiseViolations(this.opts.maxMergeDelayMs ?? 600_000).length, 'promises past the max merge delay without a checkpoint')
    return m
  })()
  private covered () {
    return (p: { shard: string, seq: number }) => (this.cps.get(p.shard) ?? []).some(c => c.size > p.seq)
  }
  /** LOG-005: own promises not covered by a valid checkpoint within the delay */
  promiseViolations (maxMergeDelayMs: number, now = Date.now()) {
    const mon = new PromiseMonitor(this.logPublicKey(), s => this.cps.get(s)?.[0]?.logId)
    for (const p of this.promises) mon.observePromise(p)
    for (const list of this.cps.values()) for (const c of list) mon.observeCheckpoint(c)
    return mon.violations(now, maxMergeDelayMs)
  }
  /** LOG-007: signed checkpoints issued for the shard, oldest first */
  checkpoints (shard: string): Checkpoint[] { return [...(this.cps.get(shard) ?? [])] }

  /** LOG-006: anchor every shard whose close policy fires now (interval /
   *  bytes / cost thresholds also fire without a new write — call periodically) */
  async flushDue (): Promise<Array<{ shard: string, txid: string, n: number }>> {
    const out: Array<{ shard: string, txid: string, n: number }> = []
    for (const s of this.touched) {
      if (await this.log.closeReason(s)) { const a = await this.anchorShard(s); if (a) out.push({ shard: s, ...a }) }
    }
    return out
  }

  /** CHN-006: pending (not batched/anchored or not mined), mined (k < depth), final */
  async anchorStatus (shard: string, seq: number): Promise<{ state: 'pending' | 'mined' | 'final', confirmations: number, depth: number }> {
    const depth = this.opts.finalityDepth ?? 1
    const rec = (await this.store.all(shard)).find(r => r.seq === seq)
    const mp = rec?.anchorTxid ? this.anchorTxs.get(rec.anchorTxid)?.merklePath : undefined
    if (!mp) return { state: 'pending', confirmations: 0, depth }
    const tip = this.chain.tipHeight ? await this.chain.tipHeight() : await this.chain.currentHeight()
    const confirmations = Math.max(0, tip - mp.blockHeight + 1)
    return { state: confirmations >= depth ? 'final' : 'mined', confirmations, depth }
  }

  // ── verified callers (IDN-003) and clearance (ZKA-001..006) ──────────────
  private challenges = new Map<string, number>() // nonce → expiry (ms)
  private zkSeen = new Set<string>() // transcript (queryId ‖ nonce) already used
  /** The VERIFIER issues the challenge nonce; only nonces issued by this
   *  instance and still fresh (default 5 min) authenticate a caller. */
  issueChallenge (ttlMs = 300_000): string {
    const n = idbind.issueChallenge()
    this.challenges.set(n, Date.now() + ttlMs)
    this.persistSecurityState()
    return n
  }
  /** caller identity from VERIFIED inputs only: the binding is for this
   *  pubkey, the nonce was issued here and is fresh, the answer is signed.
   *  id = hash160(pubkey). Anything else → null (no principal → DENY). */
  verifiedCaller (identity?: CallerIdentity): { pubkey: string, id: string } | null {
    if (!identity || identity.binding.pubkey !== identity.pubkey) return null
    const exp = this.challenges.get(identity.challengeNonce)
    if (exp === undefined || exp < Date.now()) return null
    if (!idbind.verifyBinding(identity.binding) || !idbind.verifyChallenge(identity.pubkey, identity.challengeNonce, identity.challengeSig)) return null
    return { pubkey: identity.pubkey, id: toHex(hash160(fromHex(identity.pubkey))) }
  }
  /** ZK clearance gate: issuer ∈ instance-trusted authorities, credential
   *  bound to the verified caller, transcript scoped to THIS read and unused. */
  verifyClearance (caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string): { ok: boolean, reason?: string } {
    if (!zk) return { ok: false, reason: 'no clearance proof' }
    if (!caller) return { ok: false, reason: 'clearance needs a verified caller identity' }
    const trusted = this.opts.clearanceAuthorities
    if (!trusted?.length) return { ok: false, reason: 'no trusted clearance authority configured' }
    if (!trusted.includes(zk.commitment.pub)) return { ok: false, reason: 'clearance issued by an untrusted authority' }
    if (zk.commitment.holder !== caller.pubkey) return { ok: false, reason: 'clearance holder is not the verified caller' }
    if (zk.transcript.fragmentId !== scope) return { ok: false, reason: `transcript scope ${zk.transcript.fragmentId} != ${scope}` }
    const seen = `${zk.transcript.queryId}|${toHex(zk.transcript.nonce)}`
    if (this.zkSeen.has(seen)) return { ok: false, reason: 'clearance transcript replayed' }
    if (!idbind.verifyAccess(zk.commitment, zk.requiredLevel, zk.proof, zk.transcript, { trustedAuthorities: trusted, holder: caller.pubkey })) return { ok: false, reason: 'clearance proof invalid' }
    this.zkSeen.add(seen)
    this.persistSecurityState()
    return { ok: true }
  }
  /** Cedar principal: verified id; level = the ZK-PROVEN bound, else 0. */
  private cedarPrincipal (caller: { id: string } | null, zkOk: boolean, requiredLevel?: bigint): { type: 'Agent' | 'User', id: string, level: number } | null {
    return caller ? { type: 'Agent', id: caller.id, level: zkOk ? Number(requiredLevel ?? 0n) : 0 } : null
  }

  /** authorize an action for the MCP `authorize` tool with the same verified
   *  inputs recall uses (never caller-asserted ids or levels). */
  async authorizeAction (action: import('@mamem/policy').CedarAction, args: { identity?: CallerIdentity, zk?: ZkClearance, resource?: { id?: string, attrs?: { level?: number, shard?: string, class?: string } } }) {
    const pol = this.opts.policy
    if (!pol) return { policySetHash: null, decision: 'not-required' as const, action }
    const { authorize, canonicalizePolicySet } = await import('@mamem/policy')
    const hash = canonicalizePolicySet('instance', pol.policyText).hash
    if (hash !== pol.anchoredPolicySetHash) return { policySetHash: hash, decision: 'deny' as const, reason: 'policy hash mismatch' }
    const caller = this.verifiedCaller(args.identity)
    if (!caller) return { policySetHash: hash, decision: 'deny' as const, reason: 'unverified identity' }
    const rid = args.resource?.id ?? 'frag'
    const zk = args.zk ? this.verifyClearance(caller, args.zk, rid) : { ok: false }
    const resource = { type: 'Fragment' as const, id: rid, attrs: { level: args.resource?.attrs?.level ?? 0, shard: args.resource?.attrs?.shard ?? 'default', class: args.resource?.attrs?.class ?? 'fragment' } }
    const d = authorize(pol.policyText, this.cedarPrincipal(caller, zk.ok, args.zk?.requiredLevel)!, action, resource)
    return { policySetHash: hash, decision: d.allow ? 'allow' as const : 'deny' as const, zkaccess: args.zk ? (zk.ok ? 'verified' : 'rejected') : 'not-required' }
  }

  /** Mark anchor txs mined (drive from broadcaster status events). */
  confirmAnchor (shard: string, txid: string): void {
    const ac = this.anchors.get(shard)
    if (!ac) return
    ac.confirm(txid, this.anchorTxs.get(txid)?.merklePath)
    this.persistState()
  }

  // ── persisted anchor state (restart without forking the shard) ───────────
  private persistState (): void {
    const st = this.opts.stateStore
    if (!st) return
    const shards: PersistedMemoryState['shards'] = {}
    for (const [shard, ac] of this.anchors) {
      const head = ac.getHead()
      if (!head) continue
      const anchors: Record<string, string> = {}
      for (const l of this.tails.get(shard) ?? []) {
        const tx = this.anchorTxs.get(l.txid)
        if (tx) anchors[l.txid] = Buffer.from(ac.beefOf(tx, true)).toString('hex')
      }
      // the head may be a rotate spend that adds no tail link
      anchors[head.tx.id('hex')] = Buffer.from(ac.beefOf(head.tx, true)).toString('hex')
      shards[shard] = {
        headTxid: head.tx.id('hex'), confirmed: !!head.tx.merklePath, tails: this.tails.get(shard) ?? [],
        checkpoints: this.cps.get(shard) ?? [], certs: [...ac.rotationCerts], anchors
      }
    }
    st.save({ version: 1, shards, promises: this.promises })
  }
  private restoreState (): void {
    const s = this.opts.stateStore?.load()
    if (!s) return
    for (const [shard, p] of Object.entries(s.shards)) {
      for (const [txid, hex] of Object.entries(p.anchors)) {
        const tx = Transaction.fromAtomicBEEF(Array.from(Buffer.from(hex, 'hex')))
        if (tx.id('hex') !== txid) throw new Error(`restore: anchor ${txid} BEEF mismatch`)
        this.anchorTxs.set(txid, tx)
      }
      const headTx = this.anchorTxs.get(p.headTxid)
      if (!headTx) throw new Error(`restore: head ${p.headTxid} of shard ${shard} missing`)
      this.anchorChain(shard).resume(headTx, { confirmed: p.confirmed, certs: p.certs })
      this.tails.set(shard, p.tails); this.cps.set(shard, p.checkpoints); this.touched.add(shard)
    }
    this.promises.push(...s.promises)
  }

  /**
   * recall: fetch fragments for a shard (optionally only anchored), decrypt
   * when an agentSeed is held, and attach a receipt.
   */
  async recall (shard: string, opts?: {
    seq?: number, question?: string,
    /** ZKAccess gate: a clearance proof covering `requiredLevel` is checked
     * before any level-gated ciphertext is decrypted. Keys alone still enforce
     * confidentiality; the proof additionally gates the READ PATH. */
    zk?: ZkClearance,
    /** Verified caller identity (challenge-response against a nonce issued by
     * THIS instance via issueChallenge): the Cedar principal id and the
     * clearance holder — never a self-asserted claim. */
    identity?: CallerIdentity
    /** CHN-006: withhold records whose anchor has fewer than finalityDepth confirmations */
    requireFinal?: boolean
  }): Promise<{ items: Array<{ seq: number, state: string, text?: string, unverifiable?: boolean, reason?: string, zk?: string }>, receipt: AnswerReceipt }> {
    const all = await this.store.all(shard)
    const items = (opts?.seq !== undefined ? all.filter(r => r.seq === opts.seq) : all)
    const caller = this.verifiedCaller(opts?.identity)
    let zkState: AnswerReceipt['zkaccess'] = 'not-required'
    let zkOk = false
    let zkReason: string | undefined
    if (opts?.zk) {
      // a clearance transcript is scoped to one fragment (seq given) or the shard
      const v = this.verifyClearance(caller, opts.zk, opts.seq !== undefined ? `${shard}/${opts.seq}` : shard)
      zkOk = v.ok; zkReason = v.reason
      zkState = zkOk ? 'verified' : 'rejected'
    }
    const provenLevel = zkOk ? opts!.zk!.requiredLevel : 0n
    // Cedar enforcement is an INSTANCE property — no policy option in the
    // request, no caller-supplied policy text or hash, no bypass.
    let cedarState: 'allow' | 'deny' | 'hash-mismatch' | 'not-required' = 'not-required'
    const policy = this.opts.policy
    if (policy) {
      const { authorize, canonicalizePolicySet } = await import('@mamem/policy')
      // hash the INSTANCE policy text ourselves — never trust a request hash
      const computed = canonicalizePolicySet('instance', policy.policyText).hash
      if (computed !== policy.anchoredPolicySetHash) {
        cedarState = 'hash-mismatch'
      } else {
        // principal identity must be VERIFIED: id = hash160 of the bound pubkey,
        // level = the ZK proof's proven bound (requiredLevel), not a claim.
        const principal = this.cedarPrincipal(caller, zkOk, opts?.zk?.requiredLevel)
        if (!principal) {
          cedarState = 'deny' // no verified identity → nothing served
        } else {
          cedarState = 'allow'
          for (const r of items) {
            const d = authorize(policy.policyText, principal, 'read', { type: 'Fragment', id: `${shard}/${r.seq}`, attrs: { level: r.level ?? 0, shard, class: 'fragment' } })
            if (!d.allow) { cedarState = 'deny'; break }
          }
        }
      }
    }
    // Policy-DAG gate (POL-004/008/016): when the instance configures a DAG,
    // the read is allowed only when Cedar ALLOWs AND the DAG derives permit
    // with no conflict. The DAG + its EDB come from the instance and the
    // VERIFIED caller facts below — never from the request.
    let dagState: string | undefined
    let dagPolicyRoot: string | undefined
    let dagDerivationHash: string | undefined
    const dagPermit = new Map<number, boolean>() // per-fragment DAG permit
    const dag = this.opts.policyDag
    if (dag) {
      const { Datalog } = await import('@mamem/datalog')
      // mirror the v2 Cedar fix: the configured DAG must match the anchored
      // root decoded from the DAG-root record — a mismatch denies everything
      const recomputed = Buffer.from(dag.policyRoot()).toString('hex')
      dagPolicyRoot = recomputed
      if (this.opts.anchoredPolicyRoot !== undefined && this.opts.anchoredPolicyRoot !== recomputed) {
        dagState = 'dag-deny:policy DAG root mismatch'
        for (const r of items) dagPermit.set(r.seq, false)
      } else {
        const facts = caller
          ? [
              { pred: 'member', args: [caller.id, 'verified'] },
              ...(zkOk ? [{ pred: 'level', args: [caller.id, String(provenLevel)] }] : []),
              ...items.map(r => ({ pred: 'fragment', args: [`${shard}/${r.seq}`] }))
            ]
          : []
        const { facts: derived, conflicts } = dag.evaluate(facts)
        dagDerivationHash = new Datalog(dag.rules).derivHash(derived)
        if (conflicts.length > 0) {
          dagState = `dag-deny:policy conflict on ${conflicts[0]!.claim}`
          for (const r of items) dagPermit.set(r.seq, false)
        } else {
          // per-item: each fragment needs its own permit; non-permitted items
          // are withheld (consistent with Cedar per-item denial)
          let denied = 0
          for (const r of items) {
            const ok = caller != null && (cedarState === 'allow' || cedarState === 'not-required') &&
              derived.has(`permit(${caller.id},read,${shard}/${r.seq})`)
            dagPermit.set(r.seq, ok)
            if (!ok) denied++
          }
          dagState = denied === 0 ? 'dag-allow' : `dag-deny:${denied} item(s) lack DAG permit`
        }
      }
      // global deny only for conflicts/root mismatch (ambiguous or untrusted
      // policy); per-item non-permits are withheld individually below
      if (/conflict|mismatch/.test(dagState)) cedarState = 'deny'
    }
    const out: AnswerReceipt['fragmentRefs'] = []
    const texts: Array<{ seq: number, state: string, text?: string, unverifiable?: boolean, reason?: string, zk?: string }> = []
    for (const r of items) {
      const ref = { shard, seq: r.seq, hdp: toHex(r.hdp), anchorTxid: r.anchorTxid }
      out.push(ref)
      const item: { seq: number, state: string, text?: string, unverifiable?: boolean, reason?: string, zk?: string } = { seq: r.seq, state: r.state }
      if (opts?.zk) item.zk = zkOk ? 'verified' : 'rejected'
      const level = BigInt(r.level ?? 0)
      const fin = opts?.requireFinal ? await this.anchorStatus(shard, r.seq) : undefined
      if (fin && fin.state !== 'final') {
        item.unverifiable = true
        item.reason = `not final: ${fin.confirmations}/${fin.depth} confirmations`
      } else if (level > 0n && provenLevel < level) {
        // ZKA gate — applies to EVERY level>0 record, plaintext or encrypted:
        // nothing is served without a valid clearance proof covering its level
        item.unverifiable = true
        item.reason = zkOk ? `zkaccess: proven level ${provenLevel} < ${level}` : `zkaccess ${opts?.zk ? 'rejected' : 'required'}${zkReason ? `: ${zkReason}` : ''}`
        item.zk = 'rejected'
        zkState = 'rejected'
      } else if (dagPermit.get(r.seq) === false) {
        // policy-DAG per-item deny — withhold this fragment only
        item.unverifiable = true
        item.reason = 'dag deny'
      } else if (cedarState !== 'allow' && cedarState !== 'not-required') {
        // Cedar: DENY or a policy-hash mismatch — never serve anything
        item.unverifiable = true
        item.reason = `cedar ${cedarState}`
      } else if (r.fragment.E && r.fragment.E.length > 0) {
        // KEY-008: decrypt ONLY after verifyFragment succeeds for this record.
        const { result } = await this.prove(shard, r.seq)
        if (!result.ok) {
          item.unverifiable = true
          item.reason = `verify: ${(result as any).reason}`
        } else if (this.opts.agentSeed) {
          try {
            const buf = Buffer.from(r.fragment.E)
            const aadLen = buf.readUInt16BE(0)
            const ef = {
              ciphertext: new Uint8Array(buf.subarray(2 + aadLen)),
              keyID: '',
              aad: new Uint8Array(buf.subarray(2, 2 + aadLen))
            }
            // try each granted level key — disjoint keyspaces mean only the
            // level the fragment was written at will pass GCM auth.
            for (let l = 0; l <= (this.opts.maxLevel ?? 0); l++) {
              try {
                const { fragmentKey } = deriveFragmentKey(deriveLevelKey(this.opts.agentSeed, l), this.ctx(shard, r.seq, l))
                const pt = decryptFragment(fragmentKey, ef)
                // defence in depth: the key level that decrypted must also be
                // covered by the proven clearance (the record's level was
                // already checked above)
                if (BigInt(l) > provenLevel) {
                  item.unverifiable = true
                  item.reason = `zkaccess: key level ${l} > proven ${provenLevel}`
                  item.zk = 'rejected'
                  zkState = 'rejected'
                } else {
                  item.text = Buffer.from(pt).toString('utf8')
                }
                shredKey(fragmentKey)
                break
              } catch { /* try next level */ }
            }
            if (item.text === undefined && item.reason === undefined) { item.unverifiable = true; item.reason = 'no held level key decrypts' }
          } catch (e) {
            item.unverifiable = true
            item.reason = 'decrypt failed'
          }
        } else {
          item.unverifiable = true
          item.reason = 'no agent seed held'
        }
      } else {
        item.text = r.fragment.d.length ? Buffer.from(r.fragment.d).toString('utf8') : undefined
      }
      if (item.unverifiable) this.metrics.inc('mamem_rejected_reads_total', { reason: (item.reason ?? 'unknown').split(/[:\d]/)[0]!.trim() }, 1, 'records withheld from a read, by reason')
      texts.push(item)
    }
    const answer = texts.map(i => i.text ?? '[unverifiable]').join('\n')
    const verified = out.length === 0 || out.every(r => r.anchorTxid)
    return {
      items: texts,
      receipt: {
        answer,
        fragmentRefs: out,
        checkResult: verified ? 'verified' : 'unverifiable',
        truthNote: 'inclusion and anchor-chain integrity verified; the TRUTH of the recorded statements is NOT established — a lie, once written, is preserved and marked',
        modelId: this.opts.modelId ?? 'unknown',
        zkaccess: zkState,
        cedar: cedarState,
        dag: dagState && dagPolicyRoot ? { policyRoot: dagPolicyRoot, derivationHash: dagDerivationHash, state: dagState } : undefined
      }
    }
  }

  /**
   * prove: build a proof bundle for one record and verify it against headers.
   * A lie stays a lie: if the record was never anchored, the receipt says
   * 'unverifiable' — we never claim provenance we cannot show.
   */
  async prove (shard: string, seq: number): Promise<{ bundle?: ProofBundle, result: VerifyResult | { ok: false, reason: 'NotAnchored' | 'NotFound' } }> {
    const rec = await this.store.all(shard).then(rs => rs.find(r => r.seq === seq))
    if (!rec) return { result: { ok: false, reason: 'NotFound' } }
    if (!rec.anchorTxid || rec.batchIndex === undefined) {
      return { result: { ok: false, reason: 'NotAnchored' } }
    }
    // rebuild the batch leaves from records of the same batch
    const batchRecs = (await this.store.all(shard)).filter(r => r.batchIndex === rec.batchIndex)
      .sort((a, b) => (a.leafIndex ?? 0) - (b.leafIndex ?? 0))
    const leaves = batchRecs.map(r => r.leaf)
    const R = merkleRoot(leaves)
    const head = this.anchorChain(shard).getHead()
    // find the anchor tx for this batch: search chain tail then head history
    const link = (this.tails.get(shard) ?? []).find(l => l.n === rec.batchIndex)
    if (!link || !head) return { result: { ok: false, reason: 'NotAnchored' } }
    const anchorTx = this.anchorTxs.get(link.txid)
    if (!anchorTx) return { result: { ok: false, reason: 'NotAnchored' } }
    this.anchorChain(shard).beefOf(anchorTx, true)
    const bundle = buildProofBundle({
      shard, leaf: rec.leaf, leafIndex: rec.leafIndex ?? 0,
      siblings: merkleProof(leaves, rec.leafIndex ?? 0).siblings,
      batchN: rec.batchIndex, batchSize: leaves.length, batchR: R,
      // consistency is proven genesis → this batch's anchor; later links are
      // not part of this record's proof (the verifier requires the tail to end
      // at the proven anchor)
      anchorTx, chainTail: (this.tails.get(shard) ?? []).filter(l => l.n <= rec.batchIndex!)
    })
    const t0 = performance.now()
    const result = await verifyFragment(bundle, this.chain)
    this.metrics.observe('mamem_proof_latency_ms', performance.now() - t0, 'verifyFragment latency')
    return { bundle, result }
  }

}
