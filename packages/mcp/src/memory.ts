// NektarMemory — the product service the MCP server wraps.
// Wires log (append/batch) → anchor (head UTXO per shard) → overlay (submit)
// → verify (proof bundles) → crypto (BRC-42/AES-GCM), all behind the chain
// interfaces so it runs identically on the mock chain and regtest.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { execFile } from 'node:child_process'
import { join } from 'node:path'
import { PrivateKey, PublicKey, Signature, Transaction, LockingScript, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import {
  utf8, fromHex, toHex, sha256, dsha256, concat, field, eq, hash160,
  computeLeaf, merkleRoot, merkleProof, identityHash, diff, apply, encodeDelta, decodeDelta,
  TAG_RECEIPT_SIG,
  type Fragment, type Bytes, type Provenance
} from '@nektar/core'
import { BatchLog, MemoryShardStore, verifyProvenance, signCheckpoint, checkpointDigest, PromiseMonitor, type ShardStore, type LogRecord, type Checkpoint } from '@nektar/log'
import { Metrics } from './metrics.js'

const DELTA_PREFIX = 'delta:'

/** write refused at ingest (COM-003) */
export class IngestError extends Error {}
import { AnchorChain, CheckpointBond, type BondUtxo } from '@nektar/anchor'
import { AutomatonChain, type AutomatonDef, type AutomatonHead } from '@nektar/automaton'
import { createNektarOverlay, type NektarOverlay } from '@nektar/overlay'
import { buildProofBundle, verifyFragment, type ProofBundle, type AnchorLink, type VerifyResult } from '@nektar/verify'
import { deriveFragmentKey, deriveLevelKey, encryptFragment, decryptFragment, shredKey, type FragmentContext } from '@nektar/crypto'
import type { Broadcaster, ChainTracker, Funder } from '@nektar/chain'
import * as idbind from '@nektar/identity'
import { proveMembershipStark, verifyMembershipAnchoredStark, starkQueryId, proveDerivationStark, verifyDerivationBound, buildSubsetProof, encodeZkdis, decodeZkdis, verifySubsetAnchored, decodeZkm2, decodeZkder, verifyCombinedStark } from '@nektar/zk'

export interface NektarMemoryOptions {
  signingKey?: PrivateKey
  shardKeys?: Map<string, PrivateKey>
  agentSeed?: PrivateKey // enables encryption when set
  maxLevel?: number // highest access level this seed may hold
  burstLimit?: number
  /** fee rate for anchor txs in sats per KB (runar-sdk fee = ceil(size*rate/1000));
   *  default 1 (mock/regtest); mainnet uses ~100 (ARC policy) */
  feeRateSatPerKb?: number
  batchPolicy?: import('@nektar/log').ClosePolicy
  overlay?: NektarOverlay // if set, anchors are also submitted to tm_nektar
  modelId?: string // stamped into receipts
  /** §16/§18: stronger ℱ binding — the operator-declared model identity
   *  stamped into every receipt: invocation params (temperature, seed…) and,
   *  for local models, a hash of the weights file. */
  model?: { id?: string, params?: Record<string, unknown>, weightsHash?: string }
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
  policyDag?: import('@nektar/graph').PolicyDAG
  anchoredPolicyRoot?: string
  /** ZKA: authority pubkeys trusted to issue clearance. Level>0 reads are
   *  refused when unset — self-issued clearance is never accepted. */
  clearanceAuthorities?: string[]
  /** COM-003: refuse writes without a valid originator signature (sourcePub) */
  requireSignedProvenance?: boolean
  /** network the instance runs on — used for the funding-address prefix in
   *  fundingStatus (testnet-family → 0x6f). Optional; absent = mainnet prefix. */
  network?: 'mock' | 'regtest' | 'testnet' | 'main' | 'ttn'
  /** CHN-006: confirmations after which an anchor counts as final (default 1) */
  finalityDepth?: number
  /** LOG-005: maximum merge delay for inclusion promises (default 10 min) */
  maxMergeDelayMs?: number
  /** persistence for challenges, ZK transcripts, issued ZK queries, revocations
   *  (default: in-memory, lost on restart) */
  securityStore?: SecurityStateStore
  /** persistence for anchor heads/tails/checkpoints (restart-safe, no fork) */
  stateStore?: MemoryStateStore
  /** data dir for the public proof store (proofs/<sha256>) — defaults to
   *  NEKTAR_DATA_DIR or ./.nektar; proofs committed in private cites are
   *  served publicly for independent re-verification. */
  dataDir?: string
  /** §12 SLA bond: when set, a NektarCheckpointBond covenant carries an
   *  unforkable on-chain checkpoint history — every anchor commits the
   *  checkpoint digest, so a missing commit within the SLA window is
   *  publicly provable (and slashable once `seize` ships). */
  bond?: {
    /** key that signs commit/close — defaults to the funding key */
    ownerKey?: PrivateKey
    /** bond size in sats (default 5_000) */
    sats?: number
    /** blocks after deploy when the bond may be closed (default 1008 ≈ 1 week) */
    horizonBlocks?: number
    /** §12 SLA window in blocks: each anchor's commit renews the seize
     *  deadline to commitHeight + epochBlocks (default 144 ≈ 1 day). */
    epochBlocks?: number
    /** fee bound enforced by the covenant (default 10) */
    maxFee?: bigint
  }
}

/** persisted anchor state: per shard the head, the succession, checkpoints,
 *  rotation certs and every anchor tx as atomic BEEF (with BUMP once mined) */
export interface PersistedMemoryState {
  version: 1
  shards: Record<string, { headTxid: string, confirmed: boolean, tails: AnchorLink[], checkpoints: Checkpoint[], certs: import('@nektar/identity').RotationCert[], anchors: Record<string, string> }>
  /** proof bundles for records imported via importShard — keyed 'shard:seq';
   *  these verify against the SOURCE chain's anchors (prove() fast-path) */
  importedBundles?: Record<string, ProofBundle>
  promises: Array<{ shard: string, seq: number, issuedAt: number }>
  /** owner-declared shard policy: `private` shards refuse plain merkle
   *  citations — records on them may only be cited via a ZK proof.
   *  Monotone: once private, a shard never goes back. */
  flags?: Record<string, { private?: boolean }>
  /** §12 SLA bond covenant head (when opts.bond set); restart-safe like the
   *  shard heads — losing it would strand the bond UTXO. */
  bond?: { txid: string, vout: number, satoshis: number, script: string, operatorPkh: string, releaseHeight: string, maxFee: string, epochLen?: string }
}
export interface MemoryStateStore { load: () => PersistedMemoryState | undefined, save: (s: PersistedMemoryState) => void }

/** portable shard pack (exportShard/importShard): public records + the
 *  ProofBundle of each anchored one. The importer re-runs verifyFragment —
 *  a pack is data + proofs, never trusted statements. */
export interface ShardPack {
  v: 1
  kind: 'nektar-shard-pack'
  shard: string
  exportedAt: number
  /** the exporter's latest committed manifest for this shard, when described —
   *  advisory metadata about the source (importers never re-claim it) */
  manifest?: ShardManifest
  records: Array<{
    seq: number, data: string, level?: number, bundle?: ProofBundle
    prov?: { source?: string, time?: number, signature?: string, modelId?: string, modality?: Provenance['modality'] }
  }>
}

/** a saleable access mode declared in a shard manifest: 'answer' = paid
 *  groundedAnswer calls, 'pack' = the public shard pack, 'record' = one
 *  record via the sale escrow (seq identifies it). priceSats is a hint —
 *  the binding quote is the invoice issued at call time. */
export interface ManifestOffer { kind: 'answer' | 'pack' | 'record', priceSats?: number, seq?: number }

/** a shard's self-description committed to __catalog__ like any other
 *  memory: "this shard covers these domains" is an anchored, provenance-
 *  stamped claim — latest record wins, prior manifests stay anchored as
 *  history. shardId binds the label to the on-chain anchor chain so a
 *  directory answer is checkable against ls_nektar, not merely asserted. */
export interface ShardManifest {
  v: 1
  type: 'shard-manifest'
  shard: string
  /** on-chain shard id = hash160(genesis owner pubkey) */
  shardId: string
  domains: string[]
  desc?: string
  offers?: ManifestOffer[]
  /** settlement context — the chain this seller pays/anchors on; a buyer's
   *  peerCall refuses invoices whose network tag doesn't match. */
  network?: string
  issuedAt: number
}
export const CATALOG_SHARD = '__catalog__'

/** pluggable persistence for replay-protection state (see securitystore.ts) */
export interface SecurityStateStore {
  // zkQueries rows are [queryId, expiry, nonce?, verifierId?] — the two extra
  // fields (v2) let verifyCombined reconstruct the StarkQuery for the §13 Π
  // binding; legacy 2-field rows still consume but can't serve a combined proof.
  load: () => { challenges: Array<[string, number]>, zkSeen: string[], zkQueries: Array<[string, number] | [string, number, string, string] | [string, number, string, string, boolean]>, revocations: string[] }
  save: (s: ReturnType<SecurityStateStore['load']>) => void
}

/** caller identity: binding + answer to a challenge issued by this instance */
export interface CallerIdentity { pubkey: string, binding: import('@nektar/identity').IdentityBinding, challengeNonce: string, challengeSig: string }
/** clearance presentation: PUBLIC commitment + proof + scoped transcript */
export interface ZkClearance {
  commitment: import('@nektar/identity').ClearanceCommitment
  proof: import('@nektar/identity').RangeProof | import('@nektar/identity').RangeProofBP
  transcript: import('@nektar/identity').ZkTranscript
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
  /** the effective level applied — ≥ the requested level, raised by premiseRefs */
  level?: number
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
  /** §16/§18 model binding: which ℱ produced the answer. `id` is the declared
   *  model identifier (mirrors modelId), `params` the invocation parameters a
   *  re-runner needs (temperature, seed, ctx size…), `weightsHash` commits the
   *  local model's weights so a receipt names the exact artifact. An LLM is
   *  not provably deterministic — this binds the receipt to the strongest
   *  reproducible identity the stack can offer. */
  model?: { id: string, params?: Record<string, unknown>, weightsHash?: string }
  /** §16 delegation map μ_i: sub-decisions asserted by delegating agents.
   *  signature = Sign_delegator(H(TAG_DELEGATION‖delegator‖decision‖answer)).
   *  Bound inside the signed receipt digest so the map cannot be added to or
   *  stripped after the fact — a delegation is part of the anchored claim. */
  delegations?: Array<{ delegator: string, decision: string, signature: string }>
  /** §13 Π_j: the ZKAccess artifact itself (wire form of the clearance
   *  presentation the read was gated on), bound into the digest. `zkaccess` is
   *  the server's verdict; `zk` lets a third party RE-VERIFY the proof via
   *  verifyReceiptZk — not just trust the flag. */
  zk?: import('@nektar/identity').ClearancePresentationWire
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
  const modelBytes = r.model ? utf8(canonicalJson(r.model)) : new Uint8Array(0)
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
    field(modelBytes),
    Uint8Array.from(t)
  ))
}

/** §13: re-verify the receipt's ZKAccess artifact — the Π_j a third party
 *  checks rather than trusting the `zkaccess` flag. Fails when the artifact is
 *  absent (nothing to re-verify), malformed, or the proof doesn't hold. */
export function verifyReceiptZk (r: Pick<AnswerReceipt, 'zk'>, opts?: { trustedAuthorities?: string[], holder?: string }): { ok: boolean, reason?: string } {
  if (!r.zk) return { ok: false, reason: 'no zk artifact on receipt' }
  let p: import('@nektar/identity').ClearancePresentation
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

export class NektarMemory {
  /** optimistic-claim surface (regtest/main runtimes attach a ClaimService) */
  claims: any
  attachClaims (c: any): void { this.claims = c }
  /** AGT-011 declared model identity — set by modelManifest; receipts reference it.
   *  DECLARED, not proven: an anchored manifest says which model the operator
   *  says answered, not that the inference ran those weights (needs zkML/TEE).
   *  (Distinct from opts.modelId — the string stamped into AnswerReceipts.) */
  declaredModel?: import('@nektar/reasoning').ModelIdentity
  /** anchor a model manifest on the __model__ shard and bind it to receipts. */
  async modelManifest (m: Parameters<typeof import('@nektar/reasoning').modelIdentity>[0]) {
    const { modelIdentity } = await import('@nektar/reasoning')
    this.declaredModel = await modelIdentity(m)
    const { seq } = await this.remember('__model__', JSON.stringify({ type: 'model-manifest', ...this.declaredModel }))
    const a = await this.anchorShard('__model__')
    return { modelId: this.declaredModel, anchor: { shard: '__model__', seq, txid: a?.txid }, note: 'declared model identity — anchored and bound into subsequent receipts; not a proof of inference' }
  }
  private log: BatchLog
  private anchors = new Map<string, AnchorChain>()
  private tails = new Map<string, AnchorLink[]>()
  private anchorTxs = new Map<string, import('@bsv/sdk').Transaction>()

  constructor (
    private chain: ChainTracker & Broadcaster,
    private funder: Funder,
    private fundKey: PrivateKey,
    private opts: NektarMemoryOptions = {},
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
  async broadcastTx (tx: import('@bsv/sdk').Transaction): Promise<import('@nektar/chain').BroadcastResult> { return await this.chain.broadcast(tx) }
  /** the operator fee key — covenant hosts need it as owner/buyer key */
  operatorKey (): PrivateKey { return this.fundKey }
  /** funding outs for covenant hosts (escrow deposits, covenant inputs) —
   *  callers markSpent/addOwned after a successful broadcast like
   *  anchorAttestation does. */
  async fundingOuts (satoshis: number): Promise<import('@nektar/chain').FundingOut[]> {
    const u = await this.takeFunding(satoshis)
    return [u]
  }
  /** §7 key-sale (GOAL-v7 V3): re-derive the fragment key for an encrypted
   *  record — the decryption capability being sold. Caller must ensure the
   *  record's level is within this instance's maxLevel. */
  async fragmentKeyFor (rec: import('@nektar/log').LogRecord): Promise<Uint8Array> {
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

  /** bundles of pack-imported records ('shard:seq' → ProofBundle) — the only
   *  proof path for records anchored by a DIFFERENT instance's chain */
  private importedBundles = new Map<string, ProofBundle>()

  /** rotate a shard's owner key on-chain (rotation cert emitted by AnchorChain).
   *  D4 (§19): the cert is also written to the `__identity__` shard and anchored
   *  — a third party reconstructs the pk(0)→pk(i) ledger from chain data alone. */
  async rotateShard (shard: string, newKey: PrivateKey): Promise<{ txid: string, n: number }> {
    const ac = this.anchors.get(shard)
    if (!ac) throw new Error(`no anchor for shard ${shard}`)
    const r = await ac.rotate(newKey)
    this.opts.shardKeys?.set(shard, newKey)
    this.anchorTxs.set(r.tx.id('hex'), r.tx)
    this.authorChainCache.delete(shard)
    const cert = ac.rotationCerts.at(-1)
    if (cert) {
      await this.remember('__identity__', JSON.stringify({ type: 'rotation-cert', shard, cert }))
      try { await this.anchorShard('__identity__') } catch { /* best-effort: stays pending until the next anchor */ }
    }
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

  /** §19 third-party revocation proof: the `__revocation__` shard record for
   *  `revoke:<pub>` plus its SPV-verifiable inclusion bundle — an external
   *  verifier can check "key X was revoked" against chain data alone rather
   *  than trusting an operator's local revocation list. Returns the record's
   *  plaintext `data`, its `prov` and `E` so the verifier can recompute
   *  `leaf = computeLeaf({d,p,E})` and bind the TEXT to the proven leaf —
   *  an inclusion proof for a DIFFERENT record is not evidence. */
  async revocationProof (pubHex: string): Promise<{ seq: number, data: string, prov?: import('@nektar/core').Provenance, E?: Bytes, leaf: string, bundle?: import('@nektar/verify').ProofBundle, anchored: boolean, root?: string, smtProof?: string } | null> {
    for (const rec of await this.store.all('__revocation__')) {
      let data: string
      if (rec.fragment.d.length) data = Buffer.from(rec.fragment.d).toString('utf8')
      else if (rec.fragment.E?.length && this.opts.agentSeed) {
        // encrypted deployment — the operator holds the seed; decrypt to read
        const k = await this.fragmentKeyFor(rec)
        const aadLen = (rec.fragment.E[0]! << 8) | rec.fragment.E[1]!
        const aad = rec.fragment.E.slice(2, 2 + aadLen)
        const ciphertext = rec.fragment.E.slice(2 + aadLen)
        data = Buffer.from(decryptFragment(k, { ciphertext, aad, keyID: '' })).toString('utf8')
      } else continue
      const entry = idbind.parseRevocationRecord(data)
      if (!entry || entry.pk.toLowerCase() !== pubHex.toLowerCase()) continue
      const { bundle, result } = await this.prove('__revocation__', rec.seq)
      return { seq: rec.seq, data, prov: rec.fragment.p, E: rec.fragment.E, leaf: toHex(rec.leaf), bundle, anchored: result.ok === true,
        root: entry.root, smtProof: entry.proof ? idbind.serializeSmtProof(entry.proof) : undefined }
    }
    return null
  }

  /** §19 CRL status: is `pubHex` revoked, provable against the LATEST anchored
   *  SMT root? The newest `revoke:` record's embedded root equals the current
   *  SMT root only when no revoke happened after it — `matchesCurrent` says so;
   *  a verifier recomputes via idbind.verifyRevocationStatus(pk, root, proof)
   *  which accepts both membership (revoked) and non-membership proofs. */
  async revocationStatus (pubHex: string): Promise<{ revoked: boolean, root: string, matchesCurrent: boolean, smtProof: string, seq?: number, anchored: boolean }> {
    const key = idbind.revocationKey(pubHex)
    const smtProof = idbind.serializeSmtProof(this.revocations.prove(key))
    const current = toHex(this.revocations.root())
    let root = '', seq: number | undefined, anchored = false
    for (const rec of await this.store.all('__revocation__')) {
      if (!rec.fragment.d.length) continue
      const data = Buffer.from(rec.fragment.d).toString('utf8')
      const entry = idbind.parseRevocationRecord(data)
      if (!entry) continue
      const { result } = await this.prove('__revocation__', rec.seq)
      root = entry.root; seq = rec.seq; anchored = result.ok === true
    }
    return { revoked: this.revocations.has(key), root, matchesCurrent: root === current, smtProof, seq, anchored }
  }

  // ── §12 SLA bond (NektarCheckpointBond) ────────────────────────────────
  private bondContract?: CheckpointBond
  private bondHead?: BondUtxo
  private bondKey (): PrivateKey { return this.opts.bond?.ownerKey ?? this.fundKey }

  /** deploy the checkpoint-bond covenant: `sats` locked, releasable after
   *  `releaseHeight` (default: current height + horizonBlocks). From then on
   *  every anchor commits its checkpoint digest — the on-chain history is
   *  unforkable and timestamped, so promise/SLA violations are provable by
   *  anyone replaying the commit chain. */
  async bondDeploy (sats = this.opts.bond?.sats ?? 5_000, releaseHeight?: bigint): Promise<{ txid: string, releaseHeight: bigint }> {
    if (this.bondHead) throw new Error('bond already deployed')
    const key = this.bondKey()
    const release = releaseHeight ?? BigInt(await this.chain.currentHeight()) + BigInt(this.opts.bond?.horizonBlocks ?? 1008)
    const pkh = key.toPublicKey().toHash()
    const cfg = { operatorPkh: typeof pkh === 'string' ? pkh : toHex(Uint8Array.from(pkh)), bondReleaseHeight: release, maxFee: this.opts.bond?.maxFee ?? 10n, epochLen: BigInt(this.opts.bond?.epochBlocks ?? 144) }
    this.bondContract = new CheckpointBond(cfg)
    const fund = await this.takeFunding(sats + 600)
    const { tx, utxo } = this.bondContract.buildDeploy({ tx: fund.tx, vout: fund.vout, satoshis: fund.satoshis }, key, sats)
    const br = await this.chain.broadcast(tx)
    if (br.status !== 'success') { this.bondContract = undefined; throw new Error(`bond deploy broadcast failed: ${br.description ?? 'unknown'}`) }
    this.bondHead = utxo
    this.persistState()
    return { txid: tx.id('hex'), releaseHeight: release }
  }

  /** current on-chain bond state + head — absent when no bond was deployed.
   *  `deadline` is the seize threshold: if chain height reaches it without a
   *  commit, ANYONE may take the bond (§12 deadline-slashing). */
  bondStatus (): { n: number, lastRoot: string, headTxid: string, sats: number, releaseHeight: bigint, deadline: bigint, seized: boolean } | null {
    if (!this.bondContract || !this.bondHead) return null
    const s = this.bondContract.stateOf(this.bondHead.script)
    return { n: Number(s.n), lastRoot: s.lastRoot, headTxid: this.bondHead.txid, sats: this.bondHead.satoshis, releaseHeight: this.bondContract.params.bondReleaseHeight, deadline: s.deadline, seized: s.seized === 1n }
  }

  /** seize an overdue bond — permissionless: valid only after the on-chain
   *  deadline. Any observer can run this against a bond head they hold; the
   *  payout goes to `payToScript` (default: this instance's operator). */
  async bondSeize (payToScript?: string): Promise<{ txid: string } | null> {
    if (!this.bondContract || !this.bondHead) return null
    const height = await this.chain.currentHeight()
    const s = this.bondContract.stateOf(this.bondHead.script)
    if (s.seized !== 0n) throw new Error('bond already seized')
    if (BigInt(height) < s.deadline) throw new Error(`bond not yet seizable — deadline ${s.deadline}, height ${height}`)
    const { buildP2PKHScript } = await import('@nektar/anchor')
    const pkh = this.bondContract.params.operatorPkh
    const { tx } = await this.bondContract.buildSeize(this.bondHead, payToScript ?? buildP2PKHScript(pkh), height, this.chain)
    this.bondHead = { txid: tx.id('hex'), vout: 1, satoshis: tx.outputs[1]!.satoshis!, script: tx.outputs[1]!.lockingScript.toHex() }
    this.persistState()
    return { txid: tx.id('hex') }
  }

  /** build + sign the payment tx for an invoice (payQuote → invoice fields)
   *  using the instance's own fund key — the agent pays without an external
   *  wallet. §16 audit trail: settlement IS an anchored tx — the payment
   *  receipt is publicly re-verifiable, not a server-side claim. */
  async payInvoice (inv: { invoiceId: string, amount: number, lockingScriptHex: string, opReturn: string }): Promise<{ payment: { invoiceId: string, transaction: string }, txid: string }> {
    const f = await this.takeFunding(inv.amount + 600)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(this.fundKey) })
    tx.addOutput({ satoshis: inv.amount, lockingScript: LockingScript.fromHex(inv.lockingScriptHex) })
    // OP_FALSE OP_RETURN + ascii tag push — settlePayment parses '006a'+utf8(tag)
    const tagHex = Buffer.from(inv.opReturn, 'utf8').toString('hex')
    tx.addOutput({ satoshis: 0, lockingScript: LockingScript.fromHex(`006a${tagHex.length / 2 < 0x4c ? (tagHex.length / 2).toString(16).padStart(2, '0') : '4c' + (tagHex.length / 2).toString(16).padStart(2, '0')}${tagHex}`) })
    tx.addOutput({ lockingScript: new P2PKH().lock(this.fundKey.toPublicKey().toAddress((this.opts.network ?? 'main') === 'main' ? [0x00] : [0x6f])), change: true })
    await tx.fee(new SatoshisPerKilobyte(this.opts.feeRateSatPerKb ?? 1))
    await tx.sign()
    // atomic BEEF — carries the funding parent so the seller's script
    // validation and broadcaster resolve input values (settlePayment's
    // 0101010000 branch; "BEEF preferred" per the invoice docs).
    // Serialize BEFORE marking the reserve spent — a throw here must not
    // drain the funding UTXO.
    const transaction = tx.toHexAtomicBEEF()
    ;(this.funder as any).markSpent?.(f.tx.id('hex'), f.vout)
    ;(this.funder as any).addOwned?.(tx, tx.outputs.length - 1)
    return { payment: { invoiceId: inv.invoiceId, transaction }, txid: tx.id('hex') }
  }

  /** D4 (§19 M_U): the shard's anchored rotation ledger — genesis owner pub
   *  plus every `rotation-cert` record in `__identity__` with its inclusion
   *  bundle. A verifier replays verifyCertChain over certs it can each prove
   *  were anchored; a withheld cert can only make the chain look older. */
  async rotationChainProof (shard: string): Promise<{
    genesisPub: string | null
    certs: Array<{ cert: import('@nektar/identity').RotationCert, seq: number, data: string, prov?: import('@nektar/core').Provenance, E?: Bytes, leaf: string, bundle?: import('@nektar/verify').ProofBundle, anchored: boolean }>
  } | null> {
    if (!this.anchors.get(shard)) return null
    const ac = await this.authorChain(shard)
    const certs: NonNullable<Awaited<ReturnType<typeof this.rotationChainProof>>>['certs'] = []
    for (const rec of await this.store.all('__identity__')) {
      let data: string
      if (rec.fragment.d.length) data = Buffer.from(rec.fragment.d).toString('utf8')
      else if (rec.fragment.E?.length && this.opts.agentSeed) {
        const k = await this.fragmentKeyFor(rec)
        const aadLen = (rec.fragment.E[0]! << 8) | rec.fragment.E[1]!
        const aad = rec.fragment.E.slice(2, 2 + aadLen)
        const ciphertext = rec.fragment.E.slice(2 + aadLen)
        data = Buffer.from(decryptFragment(k, { ciphertext, aad, keyID: '' })).toString('utf8')
      } else continue
      let cert: import('@nektar/identity').RotationCert
      try {
        const j = JSON.parse(data)
        if (j.type !== 'rotation-cert' || j.shard !== shard) continue
        cert = j.cert
      } catch { continue }
      const { bundle, result } = await this.prove('__identity__', rec.seq)
      certs.push({ cert, seq: rec.seq, data, prov: rec.fragment.p, E: rec.fragment.E, leaf: toHex(rec.leaf), bundle, anchored: result.ok === true })
    }
    certs.sort((a, b) => a.cert.index - b.cert.index)
    return { genesisPub: ac?.genesisPub ?? null, certs }
  }

  // ── persisted replay-protection state (challenges, transcripts, issued ZK queries, revocations)
  /** ZK queries issued by THIS verifier: queryId hex → {nonce, verifierId, expiry, consumed} (single use).
   *  nonce+verifierId let verifyCombined rebuild the StarkQuery for the Π binding.
   *  `consumed` entries are kept as tombstones — the re-verify ledger. */
  private zkQueries = new Map<string, { nonce: string, verifierId: string, expiry: number, consumed?: boolean }>()
  private persistSecurityState (): void {
    this.opts.securityStore?.save({
      challenges: [...this.challenges].filter(([, exp]) => exp >= Date.now()),
      zkSeen: [...this.zkSeen],
      // consumed tombstones persist past expiry — they are the re-verify
      // ledger: a proof whose queryId was consumed IS the committed one.
      zkQueries: [...this.zkQueries].filter(([, e]) => e.expiry >= Date.now() || e.consumed === true).map(([q, e]) => [q, e.expiry, e.nonce, e.verifierId, e.consumed === true]),
      revocations: [...this.revokedPubs]
    })
  }
  private restoreSecurityState (): void {
    const s = this.opts.securityStore?.load()
    if (!s) return
    for (const [n, exp] of s.challenges) this.challenges.set(n, exp)
    for (const k of s.zkSeen) this.zkSeen.add(k)
    for (const row of s.zkQueries ?? []) this.zkQueries.set(row[0], { expiry: row[1], nonce: row[2] ?? '', verifierId: row[3] ?? '', consumed: row[4] === true })
    for (const pub of s.revocations) { this.revocations.insert(idbind.deriveKey(fromHex(pub)), sha256(utf8('revoked'))); this.revokedPubs.push(pub) }
  }
  /** verifier side: issue a single-use ZK membership query (default 10 min).
   *  queryId = dsha256('nektar:zkq:v2' ‖ nonce ‖ verifierId); the prover binds it into the STARK journal. */
  issueZkQuery (verifierId: string, ttlMs = 600_000): { nonce: string, verifierId: string, queryId: string } {
    const nonce = idbind.issueChallenge()
    const queryId = toHex(starkQueryId({ nonce: fromHex(nonce), verifierId }))
    this.zkQueries.set(queryId, { nonce, verifierId, expiry: Date.now() + ttlMs })
    this.persistSecurityState()
    return { nonce, verifierId, queryId }
  }
  private consumeZkQuery = (queryIdHex: string): boolean => {
    const e = this.zkQueries.get(queryIdHex)
    if (e === undefined || e.expiry < Date.now() || e.consumed === true) return false
    e.consumed = true
    this.persistSecurityState()
    return true
  }
  /** issued-and-fresh query WITHOUT consuming it — verifyCombined checks the
   *  §13 Π binding first, consumes once at the end (single use stays). */
  private peekZkQuery (queryIdHex: string): { nonce: string, verifierId: string } | undefined {
    const e = this.zkQueries.get(queryIdHex)
    if (e === undefined || e.expiry < Date.now() || e.consumed === true || e.nonce === '' || e.verifierId === '') return undefined
    return { nonce: e.nonce, verifierId: e.verifierId }
  }
  /** re-verify registry: a queryId passes iff it was CONSUMED — i.e. this
   *  exact proof already verified once (consume runs last, only after all
   *  other checks held). The tombstone survives expiry + restart. */
  private zkQueryConsumed = (queryIdHex: string): boolean => this.zkQueries.get(queryIdHex)?.consumed === true

  // ── P14 full-mode automata (AUT-005): one Rúnar NektarAutomaton UTXO per id,
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
    return { t: 'nektar', psi: shard, rho: 'rec', pi: String(seq), l: level }
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
  private async takeFunding (satoshis: number): Promise<import('@nektar/chain').FundingOut> {
    const f = this.funder as any
    if (typeof f.take === 'function') return await f.take(satoshis)
    if (typeof f.takeReserve === 'function') { const u = await f.takeReserve(); if (u) return u }
    const u = (f.utxos ?? []).find((u: { satoshis?: number }) => (u.satoshis ?? 0) >= satoshis)
    if (!u) throw new Error('no funding UTXO available for the attestation tx')
    return u
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  fundingStatus (): { address: string, balanceSats: number, utxos: number, funded: boolean } {
    const f = this.funder as any
    const utxos: { satoshis?: number }[] = f.utxos ?? []
    const prefix = this.opts.network && this.opts.network !== 'main' ? [0x6f] : undefined
    return {
      address: this.fundKey.toPublicKey().toAddress(prefix),
      balanceSats: utxos.reduce((s, u) => s + (u.satoshis ?? 0), 0),
      utxos: utxos.length,
      funded: utxos.length > 0
    }
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
    this.metrics.inc('nektar_anchors_total', {}, 1, 'anchors broadcast')
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

  /** any tx this instance broadcast and remembers — anchor or attestation
   *  (model-verifier deploy/spend land in attestationTxs too) */
  knownTx (txid: string) { return this.anchorTxs.get(txid) ?? this.attestationTxs.get(txid) }

  /** deploy an in-script Groth16 verifier (lock) and spend it with the proof
   *  (unlock) — consensus itself runs the verification. The spend txid is the
   *  attestation a receipt cites: it exists on-chain or it doesn't. */
  async attestInferenceOnchain (lockHex: string, unlockHex: string): Promise<{ deployTxid: string, spendTxid: string }> {
    const { Transaction, LockingScript, UnlockingScript, P2PKH, SatoshisPerKilobyte } = await import('@bsv/sdk')
    const LOCK_SATS = 20_000
    const addr = this.fundKey.toPublicKey().toAddress([0x6f])
    const f = await this.takeFunding(LOCK_SATS + 5_000)
    const tx1 = new Transaction()
    tx1.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: new P2PKH().unlock(this.fundKey) })
    tx1.addOutput({ lockingScript: LockingScript.fromHex(lockHex), satoshis: LOCK_SATS })
    tx1.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
    await tx1.fee(new SatoshisPerKilobyte(this.opts.feeRateSatPerKb ?? 1))
    await tx1.sign()
    const b1 = await this.chain.broadcast(tx1)
    if (b1.status !== 'success') throw new Error(`verifier deploy failed: ${b1.description}`)
    ;(this.funder as any).markSpent?.(f.tx.id('hex'), f.vout)
    ;(this.funder as any).addOwned?.(tx1, 1)
    this.attestationTxs.set(tx1.id('hex'), tx1)
    const tx2 = new Transaction()
    tx2.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
    tx2.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: LOCK_SATS - 5_000 })
    const b2 = await this.chain.broadcast(tx2)
    if (b2.status !== 'success') throw new Error(`proof spend failed: ${b2.description}`)
    ;(this.funder as any).addOwned?.(tx2, 0)
    this.attestationTxs.set(tx2.id('hex'), tx2)
    return { deployTxid: tx1.id('hex'), spendTxid: tx2.id('hex') }
  }

  // ── public proof store + private-cite re-verification ────────────────────
  /** proofs committed inside receipts are public artifacts (a ZK proof reveals
   *  nothing): stored under <dataDir>/proofs/<sha256> and served publicly via
   *  GET /v1/proof/<sha256> so a verifier can re-run them without trusting us. */
  private proofDir (): string {
    return join(this.opts.dataDir ?? process.env.NEKTAR_DATA_DIR ?? join(process.cwd(), '.nektar'), 'proofs')
  }
  /** store a proof artifact; returns its sha256 commitment hex */
  putProof (bytes: Uint8Array): string {
    const commit = toHex(sha256(bytes))
    mkdirSync(this.proofDir(), { recursive: true })
    writeFileSync(join(this.proofDir(), commit), Buffer.from(bytes))
    return commit
  }
  /** fetch a stored proof by commitment; integrity-checked — a corrupted or
   *  foreign file can never masquerade as the committed artifact. */
  getProof (commitHex: string): Uint8Array | undefined {
    try {
      const b = readFileSync(join(this.proofDir(), commitHex))
      const u = new Uint8Array(b)
      return toHex(sha256(u)) === commitHex ? u : undefined
    } catch { return undefined }
  }

  /** re-verify a committed private-cite proof WITHOUT consuming anything:
   *  runs the full anchored STARK verification with the tombstone registry —
   *  the queryId must already be CONSUMED, i.e. this exact proof verified
   *  once at commit. The shard is resolved from the anchor's own script data
   *  (private cites never carry the shard name); the cite's hdp stays
   *  unverifiable-by-design — ZK hides which leaf it was. */
  async reverifyMembershipBytes (bytes: Uint8Array, anchorTxid: string): Promise<{ ok: boolean, reason?: string, anchorTxid?: string }> {
    const anchorTx = this.anchorTxs.get(anchorTxid)
    if (!anchorTx) return { ok: false, reason: 'committed anchor tx unknown to this instance' }
    const { decodeNektarAnchor } = await import('@nektar/contracts')
    const data = decodeNektarAnchor(anchorTx.outputs[0]!.lockingScript.toHex())
    if (!data) return { ok: false, reason: 'committed tx output 0 is not a nektar anchor' }
    const shardName = [...this.anchors.entries()].find(([, ac]) => ac.getShardId() === data.shard)?.[0]
    if (shardName === undefined) return { ok: false, reason: 'anchor shard is not one of ours' }
    const tail = this.tails.get(shardName) ?? []
    const res = await verifyMembershipAnchoredStark(bytes, anchorTx, this.chain, { consume: this.zkQueryConsumed },
      { shard: data.shard, isCanonical: (txid: string) => tail.some(l => l.txid === txid) })
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
  async verifyCombined (shard: string, seq: number, membershipBytes: Uint8Array, zk: ZkClearance, opts?: { scope?: string, derivationBytes?: Uint8Array, callerPubkey?: string }): Promise<{ ok: boolean, reason?: string, anchorTxid?: string, statement: string }> {
    const statement = 'Π_j = (π_j, ZKAccess): some record is in this anchored batch AND the clearance bound to the same verifier query proves level ≥ required — reveals no leaf, index, path, level or credential; not which record, not truth'
    const rec = (await this.store.all(shard)).find(r => r.seq === seq)
    if (!rec || rec.batchIndex === undefined) return { ok: false, reason: 'not batched', statement }
    const tail = this.tails.get(shard) ?? []
    const link = tail.find(l => l.n === rec.batchIndex)
    const anchorTx = link ? this.anchorTxs.get(link.txid) : undefined
    if (!anchorTx) return { ok: false, reason: 'anchor tx not found', statement }
    const shardId = this.anchors.get(shard)?.getShardId()
    if (!shardId) return { ok: false, reason: 'unknown shard', statement }
    const scope = opts?.scope ?? `${shard}/${seq}`
    // cheap header reads first — verify re-checks header == journal byte for byte
    let mHdr
    try { mHdr = decodeZkm2(membershipBytes) } catch (e) { return { ok: false, reason: `codec: ${(e as Error).message}`, statement } }
    const queryIdHex = toHex(mHdr.queryId)
    let dHdr
    if (opts?.derivationBytes !== undefined) {
      try { dHdr = decodeZkder(opts.derivationBytes) } catch (e) { return { ok: false, reason: `codec: ${(e as Error).message}`, statement } }
    }
    const issued = this.peekZkQuery(queryIdHex)
    if (!issued) return { ok: false, reason: 'proof queryId was not issued by this verifier (or expired)', statement }
    // the stored query MUST recompute to the proof's queryId — binds the
    // registry row to THIS proof; without it an inconsistent row could satisfy
    // the Π check against a different query
    if (toHex(starkQueryId({ nonce: fromHex(issued.nonce), verifierId: issued.verifierId })) !== queryIdHex) {
      return { ok: false, reason: 'issued query does not recompute to the proof queryId', statement }
    }
    if (!verifyCombinedStark({ nonce: fromHex(issued.nonce), verifierId: issued.verifierId }, zk.transcript, dHdr ? { queryId: toHex(dHdr.queryId) } : undefined)) {
      return { ok: false, reason: 'Π binding failed: clearance or derivation is not bound to the same verifier query as the membership proof', statement }
    }
    const res = await verifyMembershipAnchoredStark(membershipBytes, anchorTx, this.chain, { consume: () => true },
      { shard: shardId, isCanonical: (txid: string) => tail.some(l => l.txid === txid) })
    if (!res.ok) return { ok: false, reason: res.reason, statement }
    if (opts?.derivationBytes !== undefined) {
      const policyRoot = await this.expectedPolicyRoot(shard)
      if (!policyRoot) return { ok: false, reason: 'no anchored policy root configured', statement }
      const dv = verifyDerivationBound(opts.derivationBytes, { consume: () => true }, policyRoot)
      if (!dv.ok) return { ok: false, reason: `derivation: ${dv.reason}`, statement }
    }
    const clear = this.verifyClearance({ pubkey: opts?.callerPubkey ?? zk.commitment.holder }, zk, scope)
    if (!clear.ok) return { ok: false, reason: `clearance: ${clear.reason}`, statement }
    if (!this.consumeZkQuery(queryIdHex)) return { ok: false, reason: 'queryId expired or consumed mid-verify', statement }
    return { ok: true, anchorTxid: res.anchorTxid, statement }
  }

  /** GOAL-v6 S5 proof sharding (DIS-002; US 12,730,827 B2 under the Open BSV
   *  pledge — BSV only, see NOTICE): default ON, `NEKTAR_PROOF_SHARDING=0`
   *  opts out for patent-cautious operators → 'disabled'. */
  private proofShardingOn (): boolean { return process.env.NEKTAR_PROOF_SHARDING !== '0' }

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
  async proveDerivation (shard: string, query: { pred: string, args: string[], edb?: import('@nektar/datalog').Fact[] }, queryIdHex: string): Promise<{ bytes: Uint8Array, ms: number }> {
    if (!/^[0-9a-f]{64}$/.test(queryIdHex)) throw new Error('queryId must be 32-byte hex (issued by the verifier)')
    const dag = this.opts.policyDag
    if (!dag) throw new Error('no policy DAG configured on this instance')
    const { derivationPath, nodeHash } = await import('@nektar/datalog')
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
  /** §8: store a revision as Δ against `baseSeq` (base must be readable as plaintext).
   *  The delta is derived from the base — App E floors its level at the base's. */
  async revise (shard: string, baseSeq: number, newText: string, prov?: Partial<Provenance> & { sourcePub?: string, epoch?: number }, level = 0): Promise<RememberResult> {
    const base = await this.materialize(shard, baseSeq, { requireProof: false })
    return await this.remember(shard, `${DELTA_PREFIX}${baseSeq}:${encodeDelta(diff(base, newText))}`, prov, level, { premiseRefs: [{ shard, seq: baseSeq }] })
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
      await this.logAccess({ event: 'traverse', shard, seq: s, follows: Number(m[1]) })
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
  private authorChainCache = new Map<string, { genesisPub: string, certs: import('@nektar/identity').RotationCert[] } | null>()
  private async authorChain (shard: string): Promise<{ genesisPub: string, certs: import('@nektar/identity').RotationCert[] } | null> {
    if (this.authorChainCache.has(shard)) return this.authorChainCache.get(shard)!
    let out: { genesisPub: string, certs: import('@nektar/identity').RotationCert[] } | null = null
    const genesis = (this.tails.get(shard) ?? []).find(l => l.n === 0)
    const gtx = genesis ? this.anchorTxs.get(genesis.txid) : undefined
    if (gtx) {
      const { decodeNektarAnchor } = await import('@nektar/contracts')
      const dec = decodeNektarAnchor(gtx.outputs[0]!.lockingScript.toHex())
      if (dec) out = { genesisPub: dec.ownerPub, certs: [...this.anchorChain(shard).rotationCerts] }
    }
    this.authorChainCache.set(shard, out)
    return out
  }

  /** §19: was record seq written by the key of its recorded epoch, inside that key's window? */
  async authorship (shard: string, seq: number, genesisPub: string, certs: import('@nektar/identity').RotationCert[]) {
    const r = (await this.store.all(shard)).find(x => x.seq === seq)
    if (!r?.authorPub || r.authorEpoch === undefined) return { ok: false, reason: 'record carries no verified author epoch' }
    return idbind.verifyAuthorship(r.authorPub, r.authorEpoch, r.fragment.p.time, genesisPub, certs)
  }

  /** D6 (App E auditability): optional per-access log — decrypt/traverse events
   *  appended to `__access__` as timestamped records that join the batch flow
   *  and anchor with everything else. OFF by default (`NEKTAR_ACCESS_LOG=1`)
   *  because a readable log of who-read-what leaks the access pattern itself —
   *  enable only where that tradeoff is acceptable. Writes bypass the public
   *  `remember` (no auto-anchor mid-read) and never break a read on failure. */
  private accessLogEnabled (): boolean { return process.env.NEKTAR_ACCESS_LOG === '1' }
  async logAccess (entry: Record<string, unknown>): Promise<void> {
    if (!this.accessLogEnabled()) return
    try {
      await this.log.remember('__access__', {
        d: utf8(JSON.stringify({ type: 'access', at: Date.now(), ...entry })),
        p: { source: 'access-log', time: Math.floor(Date.now() / 1000), signature: new Uint8Array(0), modelId: this.opts.modelId ?? 'unknown' }
      })
    } catch { /* audit writes are best-effort — never break a read */ }
  }

  /** App E traversal constraint: a record derived from cited premises may not
   *  fall below their maximum access level — `premiseRefs` floors the new
   *  record's level at max(cited levels), so a conclusion can't leak into a
   *  lower-clearance read than its most-sensitive input. */
  async premiseLevelFloor (premiseRefs: Array<{ shard: string, seq: number }>): Promise<number> {
    let floor = 0
    for (const r of premiseRefs) {
      const rec = (await this.store.all(r.shard)).find(x => x.seq === r.seq)
      if (rec !== undefined && (rec.level ?? 0) > floor) floor = rec.level ?? 0
    }
    return floor
  }

  async remember (shard: string, d: string, prov?: Partial<Provenance> & { sourcePub?: string, epoch?: number }, level = 0, opts?: { premiseRefs?: Array<{ shard: string, seq: number }> }): Promise<RememberResult> {
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
      shard, seq: rec.seq, leaf: toHex(rec.leaf), hdp: toHex(rec.hdp), duplicate, level,
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
      this.metrics.inc('nektar_anchor_failures_total', { shard }, 1, 'anchor broadcasts that failed')
      throw e
    }
    this.metrics.inc('nektar_anchors_total', {}, 1, 'anchors broadcast')
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
    // §12 SLA: the bond covenant mirrors the checkpoint on-chain — lastRoot
    // commits to the FULL signed checkpoint (digest binds logId+size+n+R+R'+
    // time+txid), making the history unforkable and publicly replayable.
    if (this.bondContract && this.bondHead) {
      const cp = cps[cps.length - 1]!
      const lastRoot = toHex(Uint8Array.from(checkpointDigest(cp)))
      const key = this.opts.bond?.ownerKey ?? this.fundKey
      try {
        const c = await this.bondContract.buildCommit(this.bondHead, key, lastRoot, await this.chain.currentHeight(), this.chain)
        this.bondHead = { txid: c.tx.id('hex'), vout: 0, satoshis: c.tx.outputs[0]!.satoshis!, script: c.tx.outputs[0]!.lockingScript.toHex() }
        this.persistState()
      } catch { this.metrics.inc('nektar_bond_commit_failures_total', {}, 1, 'bond commit broadcasts that failed') }
    }
    if (this.opts.overlay) {
      await this.opts.overlay.engine.submit({ beef: this.anchorChain(shard).beefOf(res.tx), topics: ['tm_nektar'] }, undefined, 'current-tx')
    }
    return { txid, n: res.head.data.n }
  }

  private cps = new Map<string, Checkpoint[]>()
  private touched = new Set<string>()
  private promises: Array<{ shard: string, seq: number, issuedAt: number }> = []
  /** owner-declared shard policy — `private` shards may only be cited via ZK
   *  proofs; the flag is monotone (public→private allowed, never back) and
   *  persisted with the rest of the state so it survives restarts. */
  private flags = new Map<string, { private?: boolean }>()
  /** is this shard declared private? */
  shardPrivate (shard: string): boolean { return this.flags.get(shard)?.private === true }

  /** shards an operator can offer as public packs — excludes private shards,
   *  `__*` system shards, and any shard holding level>0 records (the same
   *  gates exportShardPack enforces; listing here means "fetchable"). */
  async publicShards (): Promise<string[]> {
    const out: string[] = []
    for (const s of this.shards()) {
      if (s.startsWith('__') || this.shardPrivate(s)) continue
      if ((await this.store.all(s)).some(r => (r.level ?? 0) > 0)) continue
      out.push(s)
    }
    return out
  }
  /** declare a shard private — monotone; once set it cannot be unset. */
  setShardPrivate (shard: string): void { const f = this.flags.get(shard) ?? {}; f.private = true; this.flags.set(shard, f); this.persistState() }

  /** the shard's on-chain id — hash160 of the genesis owner pubkey, stable
   *  across owner rotations (the anchor head is authoritative when present).
   *  Binds a manifest's label to the anchor chain a verifier can look up. */
  shardId (shard: string): string { return this.anchorChain(shard).getShardId() }

  /** read a record's plaintext — decrypting E via the level key when the
   *  instance holds the seed (same path recall/disclose use). */
  private async recordText (rec: LogRecord): Promise<string | undefined> {
    if (rec.fragment.d.length) return Buffer.from(rec.fragment.d).toString('utf8')
    if (rec.fragment.E?.length && this.opts.agentSeed) {
      const k = await this.fragmentKeyFor(rec)
      const aadLen = (rec.fragment.E[0]! << 8) | rec.fragment.E[1]!
      const aad = rec.fragment.E.slice(2, 2 + aadLen)
      return Buffer.from(decryptFragment(k, { ciphertext: rec.fragment.E.slice(2 + aadLen), aad, keyID: '' })).toString('utf8')
    }
    return undefined
  }

  /** latest committed manifest per described shard — the catalog projection
   *  the rendezvous indexes (one entry per shard, latest record wins). */
  async shardCatalog (): Promise<ShardManifest[]> {
    const by = new Map<string, { m: ShardManifest, seq: number }>()
    for (const rec of await this.store.all(CATALOG_SHARD)) {
      const data = await this.recordText(rec)
      if (!data) continue
      let m: ShardManifest
      try { m = JSON.parse(data) } catch { continue }
      if (m?.v !== 1 || m.type !== 'shard-manifest' || typeof m.shard !== 'string') continue
      const cur = by.get(m.shard)
      if (!cur || rec.seq > cur.seq) by.set(m.shard, { m, seq: rec.seq })
    }
    return [...by.values()].map(x => x.m)
  }

  /** latest committed manifest for one shard */
  async shardManifest (shard: string): Promise<ShardManifest | undefined> {
    return (await this.shardCatalog()).find(m => m.shard === shard)
  }

  /** the content price a manifest declares for `kind` ('pack' or 'read' via
   *  'answer' offers) — 0 when free/undeclared. Drives the paid-content gate. */
  async contentPrice (shard: string, kind: 'read' | 'pack'): Promise<number> {
    const m = await this.shardManifest(shard).catch(() => undefined)
    const match = m?.offers?.find(o => (kind === 'read' ? o.kind === 'answer' : o.kind === 'pack') && Number.isSafeInteger(o.priceSats))
    return match?.priceSats ?? 0
  }

  /** land a purchased record locally — call AFTER the sale claim verified
   *  it (leaf-commitment + provenance are the buyer's responsibility; this
   *  only stores what was already checked). The record keeps the seller's
   *  provenance fields; an __imports__ audit record commits the acquisition
   *  (source shard:seq, leaf, settlement txid) so the lineage is anchored. */
  async importRecord (args: { data: string, provenance?: { sourcePub?: string, hash?: string, timestamp?: number, sig?: string }, sourceShard?: string, sourceSeq?: number, leaf?: string, saleTxid?: string, shard?: string }): Promise<{ seq: number }> {
    const shard = args.shard ?? 'bought'
    const prov = args.provenance?.sourcePub
      ? { hash: args.provenance.hash ?? '', timestamp: args.provenance.timestamp ?? Date.now(), sig: args.provenance.sig ?? '', sourcePub: args.provenance.sourcePub }
      : undefined
    const { seq } = await this.remember(shard, args.data, prov)
    await this.remember('__imports__', JSON.stringify({
      type: 'record-import', fromShard: args.sourceShard ?? 'unknown', fromSeq: args.sourceSeq ?? null,
      ...(args.leaf ? { leaf: args.leaf } : {}), ...(args.saleTxid ? { saleTxid: args.saleTxid } : {}),
      toShard: shard, seq, importedAt: Date.now()
    })).catch(() => undefined)
    return { seq }
  }

  /** Commit a shard manifest to __catalog__ — the domain tags another agent
   *  searches by ('who knows about soccer'). The claim is anchored and
   *  timestamped like any memory: false advertising stays attributable.
   *  Re-describing appends a newer manifest; the old one remains history. */
  async describeShard (shard: string, fields: { domains: string[], desc?: string, offers?: ManifestOffer[] }): Promise<{ manifest: ShardManifest, seq: number }> {
    if (shard.startsWith('__')) throw new Error('describeShard: system shards (__*) cannot be described')
    if (!this.shards().includes(shard)) throw new Error(`describeShard: unknown shard "${shard}" — describe a shard this instance has written`)
    const domains = [...new Set((fields.domains ?? []).map(d => String(d).trim().toLowerCase()).filter(d => /^[a-z0-9][a-z0-9._-]{0,47}$/.test(d)))].slice(0, 32)
    if (domains.length === 0) throw new Error('describeShard: at least one valid domain tag required (a-z0-9._-, up to 48 chars)')
    const desc = fields.desc !== undefined ? String(fields.desc).slice(0, 560) : undefined
    const offers = (fields.offers ?? [])
      .map(o => ({ kind: String(o?.kind) as ManifestOffer['kind'], ...(o?.priceSats !== undefined ? { priceSats: Math.max(0, Math.floor(Number(o.priceSats))) } : {}), ...(o?.seq !== undefined ? { seq: Math.floor(Number(o.seq)) } : {}) }))
      .filter(o => o.kind === 'answer' || o.kind === 'pack' || o.kind === 'record')
      .slice(0, 64)
    const manifest: ShardManifest = {
      v: 1, type: 'shard-manifest', shard, shardId: this.shardId(shard), domains,
      ...(desc ? { desc } : {}), ...(offers.length ? { offers } : {}),
      network: process.env.NEKTAR_NETWORK ?? 'mock', issuedAt: Date.now()
    }
    const r = await this.remember(CATALOG_SHARD, JSON.stringify(manifest))
    return { manifest, seq: r.seq }
  }
  /** the committed hash + anchor of a fragment — for redacted (private) cites
   *  that carry the proof value without naming the shard. */
  async fragmentRef (shard: string, seq: number): Promise<{ hdp: string, anchorTxid?: string } | undefined> {
    const rec = (await this.store.all(shard)).find(r => r.seq === seq)
    return rec ? { hdp: Buffer.from(rec.hdp).toString('hex'), anchorTxid: rec.anchorTxid } : undefined
  }
  /** OPS-002 metrics (Prometheus text via metrics.render()) */
  readonly metrics = (() => {
    const m = new Metrics()
    m.gauge('nektar_unconfirmed_anchors', () => [...this.tails.values()].flat().filter(l => !this.anchorTxs.get(l.txid)?.merklePath).length, 'anchors without a BUMP yet')
    m.gauge('nektar_anchor_lag_ms', () => { const cov = this.covered(); const p = this.promises.filter(x => !cov(x)); return p.length ? Date.now() - Math.min(...p.map(x => x.issuedAt)) : 0 }, 'age of the oldest promise not yet checkpointed')
    m.gauge('nektar_promise_violations', () => this.promiseViolations(this.opts.maxMergeDelayMs ?? 600_000).length, 'promises past the max merge delay without a checkpoint')
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
    // §12 SLA watchdog: if no anchor renewed the bond for half an epoch,
    // commit the current root again — a lapsed deadline makes the bond
    // seizable by anyone, so the operator must refresh before it passes.
    if (this.bondContract && this.bondHead) {
      const height = await this.chain.currentHeight()
      const s = this.bondContract.stateOf(this.bondHead.script)
      const margin = this.bondContract.params.epochLen / 2n > 0n ? this.bondContract.params.epochLen / 2n : 1n
      if (s.seized === 0n && BigInt(height) + margin >= s.deadline) {
        try {
          const c = await this.bondContract.buildCommit(this.bondHead, this.opts.bond?.ownerKey ?? this.fundKey, s.lastRoot, height, this.chain)
          this.bondHead = { txid: c.tx.id('hex'), vout: 0, satoshis: c.tx.outputs[0]!.satoshis!, script: c.tx.outputs[0]!.lockingScript.toHex() }
          this.persistState()
          this.metrics.inc('nektar_bond_renewals_total', {}, 1, 'bond deadline renewals committed by the SLA watchdog')
        } catch { this.metrics.inc('nektar_bond_commit_failures_total', {}, 1, 'bond commit broadcasts that failed') }
      }
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
  async authorizeAction (action: import('@nektar/policy').CedarAction, args: { identity?: CallerIdentity, zk?: ZkClearance, resource?: { id?: string, attrs?: { level?: number, shard?: string, class?: string } } }) {
    const pol = this.opts.policy
    if (!pol) return { policySetHash: null, decision: 'not-required' as const, action }
    const { authorize, canonicalizePolicySet } = await import('@nektar/policy')
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
    const bond = this.bondContract && this.bondHead ? {
      txid: this.bondHead.txid, vout: this.bondHead.vout, satoshis: this.bondHead.satoshis, script: this.bondHead.script,
      operatorPkh: this.bondContract.params.operatorPkh, releaseHeight: this.bondContract.params.bondReleaseHeight.toString(), maxFee: this.bondContract.params.maxFee.toString(), epochLen: this.bondContract.params.epochLen.toString()
    } : undefined
    st.save({ version: 1, shards, promises: this.promises, flags: Object.fromEntries(this.flags), importedBundles: Object.fromEntries(this.importedBundles), bond })
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
    for (const [k, b] of Object.entries(s.importedBundles ?? {})) this.importedBundles.set(k, b)
    for (const [shard, f] of Object.entries(s.flags ?? {})) this.flags.set(shard, f)
    if (s.bond) {
      this.bondContract = new CheckpointBond({ operatorPkh: s.bond.operatorPkh, bondReleaseHeight: BigInt(s.bond.releaseHeight), maxFee: BigInt(s.bond.maxFee), epochLen: BigInt(s.bond.epochLen ?? this.opts.bond?.epochBlocks ?? 144) })
      this.bondHead = { txid: s.bond.txid, vout: s.bond.vout, satoshis: s.bond.satoshis, script: s.bond.script }
    }
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
    /** per-call invocation params merged into receipt.model.params (the
     * caller knows temperature/seed; the operator owns id + weightsHash) */
    modelParams?: Record<string, unknown>
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
      const { authorize, canonicalizePolicySet } = await import('@nektar/policy')
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
      const { Datalog } = await import('@nektar/datalog')
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
      // per record before the serve guards. Local check uses the live SMT;
      // third parties use revocationProof/revocationStatus — SMT membership
      // proofs verified against roots embedded in anchored `revoke:` records.
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
                    await this.logAccess({ event: 'decrypt', shard, seq: r.seq, level: l, caller: opts?.identity?.pubkey })
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
      if (item.unverifiable) this.metrics.inc('nektar_rejected_reads_total', { reason: (item.reason ?? 'unknown').split(/[:\d]/)[0]!.trim() }, 1, 'records withheld from a read, by reason')
      texts.push(item)
    }
    const answer = texts.map(i => i.text ?? '[unverifiable]').join('\n')
    const verified = out.length === 0 || (out.every(r => r.anchorTxid) && !verifyFailed)
    const unsigned: Omit<AnswerReceipt, 'signature' | 'signerPub'> = {
      answer,
      fragmentRefs: out,
      checkResult: verified ? 'verified' : 'unverifiable',
      truthNote: 'inclusion and anchor-chain integrity verified; the TRUTH of the recorded statements is NOT established — a lie, once written, is preserved and marked',
      modelId: this.opts.model?.id ?? this.opts.modelId ?? 'unknown',
      model: this.opts.model || opts?.modelParams
        ? { id: this.opts.model?.id ?? this.opts.modelId ?? 'unknown', params: { ...this.opts.model?.params, ...opts?.modelParams }, weightsHash: this.opts.model?.weightsHash }
        : undefined,
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
    // pack-imported record: its proof is the bundle the exporter shipped —
    // verifyFragment re-checks leaf→batch→anchor→headers, zero trust needed
    const ib = this.importedBundles.get(`${shard}:${seq}`)
    if (ib) return { bundle: ib, result: await verifyFragment(ib, this.chain) }
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
    this.metrics.observe('nektar_proof_latency_ms', performance.now() - t0, 'verifyFragment latency')
    return { bundle, result }
  }

  // ── shard packs (cold-start): export public records + their proof bundles —
  //  import re-verifies everything against the live chain, zero trust. ──────

  /** Export a PUBLIC shard as a portable pack: each record's plaintext +
   *  provenance + the ProofBundle prove() produces for anchored ones.
   *  Private shards and level>0 records refuse — a pack must never carry
   *  gated content. */
  async exportShardPack (shard: string): Promise<ShardPack> {
    if (this.shardPrivate(shard)) throw new Error(`exportShard: shard "${shard}" is private — private shards refuse export`)
    const recs = (await this.store.all(shard)).sort((a, b) => a.seq - b.seq)
    if (recs.length === 0) throw new Error(`exportShard: shard "${shard}" is empty or unknown`)
    if (recs.some(r => (r.level ?? 0) > 0)) throw new Error(`exportShard: shard "${shard}" holds level>0 records — refuses export`)
    const records: ShardPack['records'] = []
    for (const r of recs) {
      let bundle: ProofBundle | undefined
      if (r.anchorTxid !== undefined && r.batchIndex !== undefined) {
        const p = await this.prove(shard, r.seq)
        bundle = p.bundle
      }
      records.push({
        seq: r.seq,
        data: Buffer.from(r.fragment.d).toString('utf8'),
        prov: {
          source: r.fragment.p.source,
          time: r.fragment.p.time,
          signature: Buffer.from(r.fragment.p.signature).toString('hex'),
          modelId: r.fragment.p.modelId,
          ...(r.fragment.p.modality ? { modality: r.fragment.p.modality } : {})
        },
        level: r.level ?? 0,
        ...(bundle ? { bundle } : {})
      })
    }
    const manifest = await this.shardManifest(shard)
    return { v: 1, kind: 'nektar-shard-pack', shard, exportedAt: Date.now(), ...(manifest ? { manifest } : {}), records }
  }

  /** Import a shard pack: records re-land via remember() (byte-identical prov
   *  keeps leaves matching the source bundles), every bundle re-verifies via
   *  verifyFragment against the live chain, and the import event itself is
   *  recorded on __imports__ — the lineage is anchored, not asserted. The
   *  target shard must be empty (no mixing imported + native seqs). */
  async importShardPack (pack: unknown): Promise<{ imported: number, anchored: number, unverifiable: string[], skipped: number }> {
    const p = pack as ShardPack
    if (p?.v !== 1 || p?.kind !== 'nektar-shard-pack' || typeof p.shard !== 'string' || !Array.isArray(p.records)) {
      throw new Error('importShard: malformed pack — expected {v:1, kind:"nektar-shard-pack", shard, records[]}')
    }
    if ((await this.store.all(p.shard)).length > 0) throw new Error(`importShard: shard "${p.shard}" already has records — import needs an empty shard`)
    let imported = 0, anchored = 0, skipped = 0
    const unverifiable: string[] = []
    for (const rec of [...p.records].sort((a, b) => a.seq - b.seq)) {
      const prov = typeof rec.prov === 'object' && rec.prov !== null ? {
        ...(rec.prov as Record<string, unknown>),
        signature: typeof (rec.prov as Record<string, unknown>).signature === 'string'
          ? new Uint8Array(Buffer.from((rec.prov as Record<string, unknown>).signature as string, 'hex'))
          : new Uint8Array(0)
      } as Partial<Provenance> : undefined
      const r = await this.remember(p.shard, rec.data, prov, rec.level ?? 0)
      if (r.seq !== rec.seq) { skipped++; continue } // dedup/position drift — don't fabricate seqs
      imported++
      if (rec.bundle) {
        const recLeaf = (await this.store.all(p.shard)).find(x => x.seq === r.seq)?.leaf
        if (!recLeaf || Buffer.from(recLeaf).toString('hex') !== rec.bundle.leaf) {
          unverifiable.push(`${p.shard}:${r.seq}: imported data does not recompute the bundle's leaf`)
          continue
        }
        const v = await verifyFragment(rec.bundle, this.chain)
        if (v.ok) { this.importedBundles.set(`${p.shard}:${r.seq}`, rec.bundle); anchored++ }
        else unverifiable.push(`${p.shard}:${r.seq}: ${v.reason}`)
      }
    }
    this.persistState()
    await this.remember('__imports__', JSON.stringify({
      type: 'shard-import', shard: p.shard, exportedAt: p.exportedAt,
      imported, anchored, unverifiable: unverifiable.length, importedAt: Date.now()
    })).catch(() => undefined) // the audit record is best-effort — never block the import
    return { imported, anchored, unverifiable, skipped }
  }

  /** doctor: one-call health surface — funding, shard/record/anchor counts,
   *  unproven anchors, payments, prover availability. The tool layer may
   *  replace `funding` with the richer ops.wallet() view when present. */
  async doctor (): Promise<{
    network: string
    /** mem-side reserve view by default; the server swaps in ops.wallet()
     *  (spend caps, runway) on live networks — hence the open shape */
    funding: Record<string, unknown>
    shards: number
    records: number
    anchoredRecords: number
    unprovenAnchors: number
    importedBundles: number
    publicPacks: number
    payments: 'on' | 'off'
    prover: string
    bond: { n: number, lastRoot: string, headTxid: string, sats: number, releaseHeight: string, deadline: string, seized: boolean } | null
  }> {
    const shards = this.shards()
    let records = 0, anchoredRecords = 0
    for (const s of shards) for (const r of await this.store.all(s)) { records++; if (r.anchorTxid !== undefined) anchoredRecords++ }
    const prover = process.env.NEKTAR_STARK_PROVER
      ? (existsSync(process.env.NEKTAR_STARK_PROVER) ? 'native binary' : 'NEKTAR_STARK_PROVER set but file missing')
      : await new Promise<string>(res => {
        execFile('docker', ['image', 'inspect', process.env.NEKTAR_STARK_IMAGE ?? 'nektar-stark-build'], { timeout: 5_000 }, e =>
          res(e ? 'absent (set NEKTAR_STARK_PROVER or build nektar-stark-build)' : 'docker image'))
      })
    return {
      network: this.opts.network ?? 'main',
      funding: this.fundingStatus(),
      shards: shards.length,
      records,
      anchoredRecords,
      unprovenAnchors: this.unprovenAnchors().length,
      importedBundles: this.importedBundles.size,
      publicPacks: (await this.publicShards()).length,
      payments: /^(1|true|yes)$/i.test(process.env.NEKTAR_PAYMENTS ?? '') ? 'on' : 'off',
      prover,
      bond: (() => { const b = this.bondStatus(); return b ? { ...b, releaseHeight: b.releaseHeight.toString(), deadline: b.deadline.toString() } : null })()
    }
  }

}
