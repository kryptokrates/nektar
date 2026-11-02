// MamemMemory — the product service the MCP server wraps.
// Wires log (append/batch) → anchor (head UTXO per shard) → overlay (submit)
// → verify (proof bundles) → crypto (BRC-42/AES-GCM), all behind the chain
// interfaces so it runs identically on the mock chain and regtest.
import { PrivateKey, PublicKey, Signature, Transaction } from '@bsv/sdk'
import {
  utf8, fromHex, toHex, sha256, dsha256, concat, field, eq, hash160,
  computeLeaf, merkleRoot, merkleProof, identityHash, diff, apply, encodeDelta, decodeDelta,
  TAG_RECEIPT_SIG,
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
import { proveMembershipStark, verifyMembershipAnchoredStark, starkQueryId, proveDerivationStark, verifyDerivationBound, buildSubsetProof, encodeZkdis, decodeZkdis, verifySubsetAnchored } from '@mamem/zk'

export interface MamemMemoryOptions {
  signingKey?: PrivateKey
  shardKeys?: Map<string, PrivateKey>
  agentSeed?: PrivateKey // enables encryption when set
  maxLevel?: number // highest access level this seed may hold
  burstLimit?: number
  /** fee rate for anchor txs in sats per KB (runar-sdk fee = ceil(size*rate/1000));
   *  default 1 (mock/regtest); mainnet uses ~100 (ARC policy) */
  feeRateSatPerKb?: number
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
  /** §16 delegation map μ_i: sub-decisions asserted by delegating agents.
   *  signature = Sign_delegator(H(TAG_DELEGATION‖delegator‖decision‖answer)).
   *  Bound inside the signed receipt digest so the map cannot be added to or
   *  stripped after the fact — a delegation is part of the anchored claim. */
  delegations?: Array<{ delegator: string, decision: string, signature: string }>
  /** §13 Π_j: the ZKAccess artifact itself (wire form of the clearance
   *  presentation the read was gated on), bound into the digest. `zkaccess` is
   *  the server's verdict; `zk` lets a third party RE-VERIFY the proof via
   *  verifyReceiptZk — not just trust the flag. */
  zk?: import('@mamem/identity').ClearancePresentationWire
  /** §18 terminal commitment: τ, the log signer's pubkey, and
   *  σ = Sign_logKey(receiptDigest(this)) over output+trace+time. */
  timestamp?: number
  signerPub?: string
  signature?: string
}

/** recursive canonical JSON (sorted keys) — same scheme as reasoning/canonicalJson
 *  so digest bindings are stable across verifiers. */
function canonicalJson (v: unknown): string {
  if (v === null || typeof v !== 'object') return JSON.stringify(v)
  if (Array.isArray(v)) return `[${v.map(canonicalJson).join(',')}]`
  return `{${Object.keys(v as Record<string, unknown>).sort()
    .map(k => `${JSON.stringify(k)}:${canonicalJson((v as Record<string, unknown>)[k])}`).join(',')}}`
}

/** §18: the signed receipt digest — every receipt field except signature.
 *  Explicit field order (no JSON ambiguity): answer, each ref
 *  (shard,seq,hdp,txid), checkResult, truthNote, modelId, zkaccess, cedar,
 *  dag triple, delegation map, zk artifact, timestamp. */
export function receiptDigest (r: Omit<AnswerReceipt, 'signature' | 'signerPub'>): Bytes {
  const refs = r.fragmentRefs.map(f => concat(utf8(f.shard), utf8(String(f.seq)), utf8(f.hdp), utf8(f.anchorTxid ?? '')))
  const dagBytes = r.dag ? concat(utf8(r.dag.policyRoot), utf8(r.dag.derivationHash ?? ''), utf8(r.dag.state)) : new Uint8Array(0)
  const delBytes = concat(...(r.delegations ?? []).map(d => concat(utf8(d.delegator), utf8(d.decision), utf8(d.signature))))
  const zkBytes = r.zk ? utf8(canonicalJson(r.zk)) : new Uint8Array(0)
  const t = Buffer.alloc(8); t.writeBigUInt64LE(BigInt(r.timestamp ?? 0))
  return dsha256(concat(
    TAG_RECEIPT_SIG,
    field(utf8(r.answer)),
    field(concat(...refs)),
    field(utf8(r.checkResult)),
    field(utf8(r.truthNote)),
    field(utf8(r.modelId)),
    field(utf8(r.zkaccess ?? '')),
    field(utf8(r.cedar ?? '')),
    field(dagBytes),
    field(delBytes),
    field(zkBytes),
    Uint8Array.from(t)
  ))
}

/** §13: re-verify the receipt's ZKAccess artifact — the Π_j a third party
 *  checks rather than trusting the `zkaccess` flag. Fails when the artifact is
 *  absent (nothing to re-verify), malformed, or the proof doesn't hold. */
export function verifyReceiptZk (r: Pick<AnswerReceipt, 'zk'>, opts?: { trustedAuthorities?: string[], holder?: string }): { ok: boolean, reason?: string } {
  if (!r.zk) return { ok: false, reason: 'no zk artifact on receipt' }
  let p: import('@mamem/identity').ClearancePresentation
  try { p = idbind.decodePresentation(r.zk) } catch (e) { return { ok: false, reason: `malformed zk artifact: ${(e as Error).message}` } }
  return idbind.verifyAccess(p.commitment, p.requiredLevel, p.proof, p.transcript, { trustedAuthorities: opts?.trustedAuthorities, holder: opts?.holder })
    ? { ok: true } : { ok: false, reason: 'clearance proof does not verify' }
}

/** verify a receipt's terminal signature against its embedded signerPub */
export function verifyAnswerReceipt (r: AnswerReceipt): { ok: boolean, reason?: string } {
  if (!r.signature || !r.signerPub) return { ok: false, reason: 'unsigned receipt' }
  const { signature, signerPub, ...unsigned } = r
  try {
    const ok = PublicKey.fromString(signerPub).verify([...receiptDigest(unsigned)], Signature.fromDER(signature, 'hex'))
    return ok ? { ok: true } : { ok: false, reason: 'signature does not bind this receipt to the signer' }
  } catch (e) { return { ok: false, reason: `malformed signature: ${(e as Error).message}` } }
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
      this.anchors.set(shard, new AnchorChain(shard, this.shardKey(shard), this.funder, this.fundKey, this.chain, { burstLimit: this.opts.burstLimit, feeRateSatPerKb: this.opts.feeRateSatPerKb }))
    }
    return this.anchors.get(shard)!
  }

  /** the shard's current anchor head (undefined before the first anchor) */
  anchorHead (shard: string) { return this.anchors.get(shard)?.getHead() }
  /** an anchor tx by txid (as broadcast, with BUMP when attached) */
  anchorTx (txid: string) { return this.anchorTxs.get(txid) }
  /** the shard's log records (read view — used by prove/disclose tests) */
  async records (shard: string) { return await this.store.all(shard) }
  /** broadcast a fully-signed tx through the instance's broadcaster (SpendGuard stays in front) — covenant host tools only */
  async broadcastTx (tx: import('@bsv/sdk').Transaction): Promise<import('@mamem/chain').BroadcastResult> { return await this.chain.broadcast(tx) }
  /** the operator fee key — covenant hosts need it as owner/buyer key */
  operatorKey (): PrivateKey { return this.fundKey }
  /** funding outs for covenant hosts (escrow deposits, covenant inputs) —
   *  callers markSpent/addOwned after a successful broadcast like
   *  anchorAttestation does. */
  async fundingOuts (satoshis: number): Promise<import('@mamem/chain').FundingOut[]> {
    const u = await this.takeFunding(satoshis)
    return [u]
  }
  /** §7 key-sale (GOAL-v7 V3): re-derive the fragment key for an encrypted
   *  record — the decryption capability being sold. Caller must ensure the
   *  record's level is within this instance's maxLevel. */
  async fragmentKeyFor (rec: import('@mamem/log').LogRecord): Promise<Uint8Array> {
    if (!this.opts.agentSeed) throw new Error('fragmentKeyFor: instance has no agentSeed (encryption disabled)')
    const level = rec.level ?? 0
    const { fragmentKey } = deriveFragmentKey(deriveLevelKey(this.opts.agentSeed, level), this.ctx(rec.shard, rec.seq, level))
    return fragmentKey
  }
  /** return a tx output to the reserve as owned (change/escrow outs) */
  addOwnedOut (tx: import('@bsv/sdk').Transaction, vout: number): void {
    ;(this.funder as any).addOwned?.(tx, vout)
  }
  /** mark a spent input against the reserve after broadcast */
  markSpentOut (txid: string, vout: number): void {
    ;(this.funder as any).markSpent?.(txid, vout)
  }
  /** current chain height for locktime-gated covenant calls */
  async chainHeight (): Promise<number> { return this.chain.tipHeight ? await this.chain.tipHeight() : await this.chain.currentHeight() }
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

  // ── GOAL-v6 S1a (§13 "on-chain if necessary"): cheap receipt commitments —
  // OP_FALSE OP_RETURN sha256(proof) in a fee-key-funded data tx. Not an
  // anchor: no shard state, no contract. Off-chain WASM verify stays
  // authoritative; the chain only timestamps the commitment.
  private attestationTxs = new Map<string, import('@bsv/sdk').Transaction>()
  /** one funding outpoint from the fee reserve (MockFunder.take / ReserveFunder.takeReserve / peeked utxo) */
  private async takeFunding (satoshis: number): Promise<import('@mamem/chain').FundingOut> {
    const f = this.funder as any
    if (typeof f.take === 'function') return await f.take(satoshis)
    if (typeof f.takeReserve === 'function') { const u = await f.takeReserve(); if (u) return u }
    const u = (f.utxos ?? []).find((u: { satoshis?: number }) => (u.satoshis ?? 0) >= satoshis)
    if (!u) throw new Error('no funding UTXO available for the attestation tx')
    return u
  }

  /** anchor sha256(proofBytes) in an OP_FALSE OP_RETURN data tx funded by the
   *  fee key (~1 sat of fee at regtest rates). Returns the txid + commitment. */
  async anchorAttestation (proofBytes: Uint8Array): Promise<{ txid: string, commitment: string }> {
    const { buildAttestationTx, attestationCommitment } = await import('./onchain.js')
    const commitment = attestationCommitment(proofBytes)
    const f = await this.takeFunding(5_000)
    const { tx, changeVout } = await buildAttestationTx(f, this.fundKey, commitment, this.opts.feeRateSatPerKb ?? 1)
    const r = await this.chain.broadcast(tx)
    if (r.status !== 'success') throw new Error(`attestation broadcast failed: ${r.description}`)
    this.attestationTxs.set(tx.id('hex'), tx)
    ;(this.funder as any).markSpent?.(f.tx.id('hex'), f.vout)
    ;(this.funder as any).addOwned?.(tx, changeVout)
    this.metrics.inc('mamem_anchors_total', {}, 1, 'anchors broadcast')
    return { txid: tx.id('hex'), commitment: toHex(commitment) }
  }

  /** holder side (§13/S1a): produce the STARK membership proof AND anchor its
   *  commitment on-chain. The queryId stays unconsumed — the verifier's own
   *  verifyMembership call still succeeds (single use is its privilege). */
  async attestMembership (shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array, ms: number, txid: string, commitment: string }> {
    const { bytes, ms } = await this.proveMembership(shard, seq, queryIdHex)
    const a = await this.anchorAttestation(bytes)
    return { bytes, ms, ...a }
  }

  /** verifier side: fetch the attestation tx, extract the OP_RETURN hash and
   *  check it equals sha256(supplied proof bytes). Only txs broadcast by THIS
   *  instance are resolvable (chain fetch by txid is an ops gap, CHN-*). */
  async verifyAttestation (txid: string, proofBytes: Uint8Array): Promise<{ ok: boolean, reason?: string, commitment?: string, vout?: number }> {
    const { attestationsOf, attestationCommitment } = await import('./onchain.js')
    const tx = this.attestationTxs.get(txid) ?? this.anchorTxs.get(txid)
    if (!tx) return { ok: false, reason: 'attestation tx unknown to this instance' }
    const want = toHex(attestationCommitment(proofBytes))
    const hit = attestationsOf(tx).find(a => toHex(a.commitment) === want)
    if (!hit) return { ok: false, reason: 'no attestation output carries sha256(proof)' }
    return { ok: true, commitment: want, vout: hit.vout }
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

  /** GOAL-v6 S5 proof sharding (DIS-002; US 12,730,827 B2 under the Open BSV
   *  pledge — BSV only, see NOTICE): default ON, `MAMEM_PROOF_SHARDING=0`
   *  opts out for patent-cautious operators → 'disabled'. */
  private proofShardingOn (): boolean { return process.env.MAMEM_PROOF_SHARDING !== '0' }

  /** holder side: a selective inclusion proof for a SUBSET of one batch's
   *  records — per-leaf shard subpaths (K=64-leaf subtrees of the same
   *  anchored tree) plus one shared level-6→root multiproof, encoded as
   *  L_ZKDis v2. The anchored root is untouched; the proof reveals positions
   *  and leaf hashes, never record contents. */
  async proveSubset (shard: string, seqs: number[]): Promise<{ ok: true, bytes: Uint8Array, batchIndex: number, size: number, indices: number[] } | { ok: false, reason: string }> {
    if (!this.proofShardingOn()) return { ok: false, reason: 'disabled' }
    if (!Array.isArray(seqs) || seqs.length === 0 || !seqs.every(s => Number.isInteger(s) && s >= 0)) return { ok: false, reason: 'seqs must be a non-empty list of record seqs' }
    if (new Set(seqs).size !== seqs.length) return { ok: false, reason: 'duplicate seqs' }
    const recs = await this.store.all(shard)
    const found = new Map<number, LogRecord>()
    for (const r of recs) if (seqs.includes(r.seq)) found.set(r.seq, r)
    const missing = seqs.filter(s => !found.has(s))
    if (missing.length > 0) return { ok: false, reason: `no record(s) seq ${missing.join(',')}` }
    const batchIndex = found.get(seqs[0]!)!.batchIndex
    if (batchIndex === undefined) return { ok: false, reason: 'not batched' }
    if (![...found.values()].every(r => r.batchIndex === batchIndex)) return { ok: false, reason: 'seqs span multiple batches — one subset proof covers one batch' }
    const batch = recs.filter(r => r.batchIndex === batchIndex).sort((a, b) => (a.leafIndex ?? 0) - (b.leafIndex ?? 0))
    const leaves = batch.map(r => r.leaf)
    const indices = seqs.map(s => batch.findIndex(r => r.seq === s))
    const proof = buildSubsetProof(leaves, indices)
    return { ok: true, bytes: encodeZkdis(proof), batchIndex, size: leaves.length, indices: [...indices].sort((a, b) => a - b) }
  }

  /** verifier side: the proof's claimed batch root locates OUR anchored link
   *  with that R — then the same shard/canonical/SPV checks as
   *  verifyMembership, and the multiproof must fold to the anchored R. */
  async verifySubset (shard: string, bytes: Uint8Array): Promise<{ ok: boolean, reason?: string, anchorTxid?: string, size?: number, indices?: number[], seqs?: number[], statement: string }> {
    const statement = 'proves the listed leaf hashes sit at the listed positions in this anchored batch (selective subset disclosure; record contents stay private) — not truth'
    if (!this.proofShardingOn()) return { ok: false, reason: 'disabled', statement }
    let hdr
    try { hdr = decodeZkdis(bytes) } catch (e) { return { ok: false, reason: `codec: ${(e as Error).message}`, statement } }
    const rootHex = toHex(hdr.batchRoot)
    const tail = this.tails.get(shard) ?? []
    const link = tail.find(l => l.R === rootHex)
    const anchorTx = link ? this.anchorTxs.get(link.txid) : undefined
    if (!link || !anchorTx) return { ok: false, reason: 'no anchor of this shard commits that batch root', statement }
    // expected shard = THIS instance's shard id — never read from the anchor
    const shardId = this.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement }
    const res = await verifySubsetAnchored(bytes, anchorTx, this.chain,
      { shard: shardId, isCanonical: (txid: string) => tail.some(l => l.txid === txid) })
    const batch = (await this.store.all(shard)).filter(r => r.batchIndex === link.n)
    const seqs = res.indices?.map(i => batch.find(r => (r.leafIndex ?? 0) === i)?.seq ?? i)
    return { ok: res.ok, reason: res.reason, anchorTxid: res.anchorTxid, size: res.size, indices: res.indices, seqs, statement }
  }

  /** the policy root the verifier trusts: the configured anchored root when it
   *  equals the recomputed DAG root (same rule as recall's POL-016 gate), else
   *  an anchored `dagroot:<root>` record in `shard` matching the recomputed root. */
  private async expectedPolicyRoot (shard: string): Promise<string | undefined> {
    const dag = this.opts.policyDag
    if (!dag) return undefined
    const recomputed = toHex(dag.policyRoot())
    if (this.opts.anchoredPolicyRoot !== undefined) return this.opts.anchoredPolicyRoot === recomputed ? recomputed : undefined
    const hit = (await this.store.all(shard)).find(r => r.anchorTxid !== undefined && Buffer.from(r.fragment.d).toString('utf8') === `dagroot:${recomputed}`)
    return hit ? recomputed : undefined
  }

  /** Wright §14 holder side: a ZK-STARK that the prover knows a committed
   *  derivation path to `query.pred(query.args)` under the anchored policy DAG
   *  — every step applies a rule node Merkle-proven against the policy root.
   *  The instance supplies `fragment(<shard>/<seq>)` facts from its own store
   *  (same as recall); caller EDB facts are private context premises.
   *  Reveals neither the path nor any fact; journal = (policyRoot, conclusionHash, queryId). */
  async proveDerivation (shard: string, query: { pred: string, args: string[], edb?: import('@mamem/datalog').Fact[] }, queryIdHex: string): Promise<{ bytes: Uint8Array, ms: number }> {
    if (!/^[0-9a-f]{64}$/.test(queryIdHex)) throw new Error('queryId must be 32-byte hex (issued by the verifier)')
    const dag = this.opts.policyDag
    if (!dag) throw new Error('no policy DAG configured on this instance')
    const { derivationPath, nodeHash } = await import('@mamem/datalog')
    const edb = [
      ...(await this.store.all(shard)).map(r => ({ pred: 'fragment', args: [`${shard}/${r.seq}`] })),
      ...(query.edb ?? [])
    ]
    const { facts, conflicts } = dag.evaluate(edb)
    if (conflicts.length > 0) throw new Error(`policy conflict on ${conflicts[0]!.claim} — no derivation attestation`)
    const path = derivationPath(facts, query.pred, query.args)
    if (!path) throw new Error(`${query.pred}(${query.args.join(',')}) not entailed by the policy DAG over this EDB`)
    // snapshot the committed node set — the anchored root covers exactly this list
    const nodeHs = [...dag.nodes.keys()]
    const nodeBytes = nodeHs.map(h => new Uint8Array(Buffer.from(h, 'hex')))
    const layerOf = (ruleId: string) => dag.layers.find(l => l.rules.some(r => r.id === ruleId))?.name
    const steps = path.steps.map(s => {
      const rule = dag.rules.find(r => r.id === s.ruleId)
      const tau = dag.ruleNode.get(s.ruleId)
      if (!rule || !tau || layerOf(s.ruleId) === undefined) throw new Error(`step rule ${s.ruleId} is not a committed DAG rule`)
      const ruleIndex = nodeHs.indexOf(tau)
      return {
        rule: { id: rule.id, layer: layerOf(s.ruleId)!, head: rule.head, body: rule.body },
        ruleIndex,
        ruleSiblings: merkleProof(nodeBytes, ruleIndex).siblings.map(sib => toHex(sib)),
        inputs: s.inputs.map(i => {
          if (i.fromStep !== null) return { fact: i.fact, fromStep: i.fromStep }
          const h = toHex(nodeHash(i.fact))
          const ci = nodeHs.indexOf(h)
          return ci >= 0
            ? { fact: i.fact, fromStep: null, committedIndex: ci, committedSiblings: merkleProof(nodeBytes, ci).siblings.map(sib => toHex(sib)) }
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
  async verifyDerivation (shard: string, bytes: Uint8Array): Promise<{ ok: boolean, reason?: string, policyRoot?: string, conclusionHash?: string, statement: string }> {
    const statement = 'proves a committed derivation path under the anchored policy DAG (ZK-STARK, no trusted setup) — not which conclusion, not the premises, not truth'
    const expected = await this.expectedPolicyRoot(shard)
    if (!expected) return { ok: false, reason: 'no anchored policy DAG root (policyDag unset, root mismatch, or dagroot record not anchored)', statement }
    const res = verifyDerivationBound(bytes, { consume: this.consumeZkQuery }, expected)
    return { ok: res.ok, reason: res.reason, policyRoot: res.journal?.policyRoot, conclusionHash: res.journal?.conclusionHash, statement }
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
  /** §19 read-side author check: returns a deny reason or undefined.
   *  Enforces (a) operator-local revocation of the author pubkey and
   *  (b) epoch-window authorship against the shard's owner cert chain —
   *  when the record's author is on the shard's own chain. */
  private async authorGuard (shard: string, r: LogRecord): Promise<string | undefined> {
    if (r.authorPub && this.isRevoked(r.authorPub)) return 'revoked key'
    if (r.authorPub && r.authorEpoch !== undefined) {
      const ac = await this.authorChain(shard)
      // epoch-window authorship is enforceable only when the author is on the
      // SHARD'S owner chain (a rotated-out or forged-epoch own write is
      // withheld). Foreign authors carry no cert chain — their originator σ
      // still binds them; there is nothing to check an epoch claim against.
      const chainKeys = ac ? new Set([ac.genesisPub, ...ac.certs.map(c => c.pubkey)]) : null
      if (ac && chainKeys?.has(r.authorPub)) {
        const a = idbind.verifyAuthorship(r.authorPub, r.authorEpoch, r.fragment.p.time, ac.genesisPub, ac.certs)
        if (!a.ok) return `authorship: ${a.reason}`
      }
    }
    return undefined
  }

  /** resolve the shard's owner cert chain: genesis pub from the genesis
   *  anchor's decoded ownerPub + live rotation certs. Cached per shard. */
  private authorChainCache = new Map<string, { genesisPub: string, certs: import('@mamem/identity').RotationCert[] } | null>()
  private async authorChain (shard: string): Promise<{ genesisPub: string, certs: import('@mamem/identity').RotationCert[] } | null> {
    if (this.authorChainCache.has(shard)) return this.authorChainCache.get(shard)!
    let out: { genesisPub: string, certs: import('@mamem/identity').RotationCert[] } | null = null
    const genesis = (this.tails.get(shard) ?? []).find(l => l.n === 0)
    const gtx = genesis ? this.anchorTxs.get(genesis.txid) : undefined
    if (gtx) {
      const { decodeMamemAnchor } = await import('@mamem/contracts')
      const dec = decodeMamemAnchor(gtx.outputs[0]!.lockingScript.toHex())
      if (dec) out = { genesisPub: dec.ownerPub, certs: [...this.anchorChain(shard).rotationCerts] }
    }
    this.authorChainCache.set(shard, out)
    return out
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
    if (prov?.modality !== undefined) {
      if (!['deontic', 'epistemic', 'temporal'].includes(prov.modality.kind) || typeof prov.modality.qualifier !== 'string' || prov.modality.qualifier.length === 0) {
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
    /** §16 delegation map: sub-decisions the caller asserts were made by other
     * agents (each = makeDelegation(key, decision, answer)). Bound into the
     * signed receipt so the map is anchored, not post-hoc. */
    delegations?: Array<{ delegator: string, decision: string, signature: string }>
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
    // S6.1: any item whose SPV verification fails flips the whole receipt to
    // 'unverifiable' — anchor presence alone never earns 'verified'
    let verifyFailed = false
    for (const r of items) {
      const ref = { shard, seq: r.seq, hdp: toHex(r.hdp), anchorTxid: r.anchorTxid }
      out.push(ref)
      const item: { seq: number, state: string, text?: string, unverifiable?: boolean, reason?: string, zk?: string } = { seq: r.seq, state: r.state }
      if (opts?.zk) item.zk = zkOk ? 'verified' : 'rejected'
      const level = BigInt(r.level ?? 0)
      const fin = opts?.requireFinal ? await this.anchorStatus(shard, r.seq) : undefined
      // §19 author guard: revocation + epoch-window authorship, evaluated once
      // per record before the serve guards. Operator-local revocation only —
      // third-party revocation proofs need an anchored root (deferred).
      const authorDeny = r.authorPub ? await this.authorGuard(shard, r) : undefined
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
      } else if (authorDeny) {
        item.unverifiable = true
        item.reason = authorDeny
      } else if (r.fragment.E && r.fragment.E.length > 0) {
        // KEY-008: decrypt ONLY after verifyFragment succeeds for this record.
        const { result } = await this.prove(shard, r.seq)
        if (!result.ok) {
          item.unverifiable = true
          item.reason = `verify: ${(result as any).reason}`
          verifyFailed = true
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
                  // COM-003 strict audit: re-verify the originator signature
                  // over the DECRYPTED plaintext (it was bound at ingest).
                  if (r.authorPub && r.fragment.p.signature?.length && !verifyProvenance(r.authorPub, pt, r.fragment.p)) {
                    item.unverifiable = true
                    item.reason = 'provenance sig invalid post-decrypt'
                  } else {
                    item.text = Buffer.from(pt).toString('utf8')
                  }
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
        // S6.1: plaintext items run the SAME SPV verification as encrypted
        // ones (verify only — there is no decrypt step). An unanchored or
        // failed record is withheld and marked, never served as verified.
        const { result } = await this.prove(shard, r.seq)
        if (!result.ok) {
          item.unverifiable = true
          item.reason = `verify: ${(result as any).reason}`
          verifyFailed = true
        } else {
          item.text = r.fragment.d.length ? Buffer.from(r.fragment.d).toString('utf8') : undefined
        }
      }
      if (item.unverifiable) this.metrics.inc('mamem_rejected_reads_total', { reason: (item.reason ?? 'unknown').split(/[:\d]/)[0]!.trim() }, 1, 'records withheld from a read, by reason')
      texts.push(item)
    }
    const answer = texts.map(i => i.text ?? '[unverifiable]').join('\n')
    const verified = out.length === 0 || (out.every(r => r.anchorTxid) && !verifyFailed)
    const unsigned: Omit<AnswerReceipt, 'signature' | 'signerPub'> = {
      answer,
      fragmentRefs: out,
      checkResult: verified ? 'verified' : 'unverifiable',
      truthNote: 'inclusion and anchor-chain integrity verified; the TRUTH of the recorded statements is NOT established — a lie, once written, is preserved and marked',
      modelId: this.opts.modelId ?? 'unknown',
      zkaccess: zkState,
      zk: opts?.zk ? idbind.encodePresentation({ commitment: opts.zk.commitment, proof: opts.zk.proof, transcript: opts.zk.transcript, requiredLevel: opts.zk.requiredLevel }) : undefined,
      cedar: cedarState,
      dag: dagState && dagPolicyRoot ? { policyRoot: dagPolicyRoot, derivationHash: dagDerivationHash, state: dagState } : undefined,
      delegations: opts?.delegations,
      timestamp: Date.now()
    }
    // §18 terminal commitment: σ = Sign_logKey(H(answer‖trace‖τ))
    const receipt: AnswerReceipt = {
      ...unsigned,
      signerPub: this.logKey.toPublicKey().toString(),
      signature: this.logKey.sign([...receiptDigest(unsigned)]).toDER('hex') as string
    }
    return { items: texts, receipt }
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
