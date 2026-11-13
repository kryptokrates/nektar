// types.ts — the mcp domain vocabulary leaf: options, persisted-state shapes,
// wire packs/manifests, security-store contracts, caller/receipt types.
// Extracted so the *ops modules and NektarMemory can share these without a
// memory ↔ ops type cycle (check:cruise counts type-only edges).
import type { PrivateKey, WalletInterface } from '@bsv/sdk'
import type { SignerLike, Provenance } from '@nektar/core'
import type { AnchorLink, ProofBundle } from '@nektar/verify'
import type { Checkpoint } from '@nektar/log'
import type { TxStore } from '@nektar/chain'

/** the operator's signing surface for protocol artifacts — carrier locks,
 *  record signatures, envelope seal/open. `ProtoWallet` satisfies this
 *  locally; any BRC-100 wallet implementing the crypto subset can be
 *  injected (e.g. @bsv/wallet-toolbox `Wallet`, or WalletClient to an
 *  external signer) so the operator key never lives in-process. Plumbing
 *  ops that genuinely need private-key material — P2PKH funding inputs,
 *  change scripts, deterministic salts — stay on `operatorKey()`. */
export type OperatorWallet = Pick<WalletInterface, 'getPublicKey' | 'createSignature' | 'encrypt' | 'decrypt' | 'createHmac'>

export interface NektarMemoryOptions {
  signingKey?: SignerLike
  /** injected signing surface for lanes/bstack protocol calls — defaults to
   *  `new ProtoWallet(fundKey)`. NOTE: unmineable carriers and covenant txs
   *  never become wallet `createAction`s — this surface only signs. */
  wallet?: OperatorWallet
  shardKeys?: Map<string, SignerLike>
  /** pre-resolved owner pubkeys for wallet-custody shardKeys — the runtime
   *  resolves them (async) before constructing NektarMemory so the sync
   *  restoreState/resume path never needs a wallet round trip. */
  shardPubs?: Map<string, string> | Record<string, string>
  agentSeed?: PrivateKey // enables encryption when set
  maxLevel?: number // highest access level this seed may hold
  burstLimit?: number
  /** fee rate for anchor txs in sats per KB (runar-sdk fee = ceil(size*rate/1000));
   *  default 1 (regtest); mainnet uses ~100 (broadcaster policy) */
  feeRateSatPerKb?: number
  batchPolicy?: import('@nektar/log').ClosePolicy
  modelId?: string // stamped into receipts
  /** §16/§18: stronger ℱ binding — the operator-declared model identity
   *  stamped into every receipt: invocation params (temperature, seed…) and,
   *  for local models, a hash of the weights file. */
  model?: { id?: string; params?: Record<string, unknown>; weightsHash?: string }
  /** Instance-level Cedar enforcement (POL-010/013): when set, EVERY recall and
   * disclose MUST pass Cedar ALLOW against this policy, and the policy text
   * must hash to `anchoredPolicySetHash` (decoded from the anchored policy-set
   * record — never a request field). No bypass possible. */
  policy?: { policyText: string; anchoredPolicySetHash: string }
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
  network?: 'regtest' | 'testnet' | 'main' | 'ttn'
  /** chain-observer seam (live networks) — lets txState ask the
   *  broadcaster's /tx/{txid} verdict when the local view can't decide;
   *  injected by the runtime (`fetchTxStatus` in @nektar/chain) */
  txStatus?: (txid: string) => Promise<string>
  /** the instance's transaction graph — the live runtime passes a persisted
   *  one; default is in-memory */
  txStore?: TxStore
  /** CHN-006: confirmations after which an anchor counts as final (default 1) */
  finalityDepth?: number
  /** LOG-005: maximum merge delay for inclusion promises (default 10 min) */
  maxMergeDelayMs?: number
  /** persistence for challenges, ZK transcripts, issued ZK queries, revocations
   *  (default: in-memory, lost on restart) */
  securityStore?: SecurityStateStore
  /** persistence for anchor heads/tails/checkpoints (restart-safe, no fork) */
  stateStore?: MemoryStateStore
  /** bstore/S3 object store: public shard packs are also PUT to the gateway
   *  when a manifest is committed (env NEKTAR_BSTORE_*); absent = feature off
   *  and /v1/pack/{shard} stays the only pack endpoint */
  objectStore?: import('@nektar/store').S3ObjectStore
  /** the bfinger packs store: stored packs also commit as kind-5 sub-records
   *  under the operator identity so a reader resolves them through the
   *  record's refs — no /.well-known or operator origin needed. */
  packsStore?: import('@nektar/store').PacksStore
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
  shards: Record<
    string,
    {
      headTxid: string
      confirmed: boolean
      tails: AnchorLink[]
      checkpoints: Checkpoint[]
      certs: import('@nektar/identity').RotationCert[]
      anchors: Record<string, string>
    }
  >
  /** proof bundles for records imported via importShard — keyed 'shard:seq';
   *  these verify against the SOURCE chain's anchors (prove() fast-path) */
  importedBundles?: Record<string, ProofBundle>
  promises: Array<{ shard: string; seq: number; issuedAt: number }>
  /** owner-declared shard policy: `private` shards refuse plain merkle
   *  citations — records on them may only be cited via a ZK proof.
   *  Monotone: once private, a shard never goes back. */
  flags?: Record<string, { private?: boolean }>
  /** §12 SLA bond covenant head (when opts.bond set); restart-safe like the
   *  shard heads — losing it would strand the bond UTXO. */
  bond?: { txid: string; vout: number; satoshis: number; script: string; operatorPkh: string; releaseHeight: string; maxFee: string; epochLen?: string }
  /** raw-tx hex for recently broadcast txs (bond commits/seizes/escrows) —
   *  lets covenant spends build preimages from a fresh parent before
   *  indexers serve it. Bounded (last ~64). */
  txHex?: Record<string, string>
  /** object-store refs of packs this instance PUT to bstore — keyed by shard;
   *  lets /.well-known/nektar name the store location across restarts */
  packStores?: Record<string, import('@nektar/store').ObjectRef>
  /** proofCommit → the bstore endpoint/bucket the proof bytes were PUT under
   *  `chirp/<commit>` — lets a verifier resolve proofs origin-free */
  proofStores?: Record<string, { endpoint: string; bucket: string }>
  /** the bfinger packs store's members + manifest seq — survives a restart
   *  that boots without lanes so a later lanes-up run keeps committing. */
  packsStore?: { members?: import('@nektar/store').PackMember[]; manifestSeq?: number }
}
export interface MemoryStateStore {
  load: () => PersistedMemoryState | undefined
  save: (s: PersistedMemoryState) => void
}

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
    seq: number
    data: string
    level?: number
    bundle?: ProofBundle
    /** serialized fragment hex — carries E/hdp so encrypted records re-land
     *  byte-identically and their bundle leaf re-verifies (S3) */
    frag?: string
    prov?: { source?: string; time?: number; signature?: string; modelId?: string; modality?: Provenance['modality'] }
  }>
  /** anchorTxid → AtomicBEEF hex — the inclusion evidence for anchored records;
   *  the verifier checks these against ITS headers, not the exporter's word */
  evidence?: Record<string, string>
}

/** a saleable access mode declared in a shard manifest: 'answer' = paid
 *  groundedAnswer calls, 'pack' = the public shard pack, 'record' = one
 *  record via the sale escrow (seq identifies it). priceSats is a hint —
 *  the binding quote is the invoice issued at call time. */
export interface ManifestOffer {
  kind: 'answer' | 'pack' | 'record'
  priceSats?: number
  seq?: number
}

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
  /** settlement context — the chain this seller pays/anchors on; the market
   *  refuses artifacts whose network tag doesn't match. */
  network?: string
  issuedAt: number
}
export const CATALOG_SHARD = '__catalog__'

/** pluggable persistence for replay-protection state (see securitystore.ts) */
export interface SecurityStateStore {
  // zkQueries rows are [queryId, expiry, nonce?, verifierId?] — the two extra
  // fields (v2) let verifyCombined reconstruct the StarkQuery for the §13 Π
  // binding; legacy 2-field rows still consume but can't serve a combined proof.
  load: () => {
    challenges: Array<[string, number]>
    zkSeen: string[]
    zkQueries: Array<[string, number] | [string, number, string, string] | [string, number, string, string, boolean]>
    revocations: string[]
  }
  save: (s: ReturnType<SecurityStateStore['load']>) => void
}

/** caller identity: binding + answer to a challenge issued by this instance */
export interface CallerIdentity {
  pubkey: string
  binding: import('@nektar/identity').IdentityBinding
  challengeNonce: string
  challengeSig: string
}
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
  promise: { seq: number; leaf: string; signature: string }
  anchored?: { txid: string; n: number }
  /** the effective level applied — ≥ the requested level, raised by premiseRefs */
  level?: number
}

export interface AnswerReceipt {
  answer: string
  fragmentRefs: Array<{ shard: string; seq: number; hdp: string; anchorTxid?: string }>
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
  dag?: { policyRoot: string; derivationHash?: string; state: string }
  /** §16/§18 model binding: which ℱ produced the answer. `id` is the declared
   *  model identifier (mirrors modelId), `params` the invocation parameters a
   *  re-runner needs (temperature, seed, ctx size…), `weightsHash` commits the
   *  local model's weights so a receipt names the exact artifact. An LLM is
   *  not provably deterministic — this binds the receipt to the strongest
   *  reproducible identity the stack can offer. */
  model?: { id: string; params?: Record<string, unknown>; weightsHash?: string }
  /** §16 delegation map μ_i: sub-decisions asserted by delegating agents.
   *  signature = Sign_delegator(H(TAG_DELEGATION‖delegator‖decision‖answer)).
   *  Bound inside the signed receipt digest so the map cannot be added to or
   *  stripped after the fact — a delegation is part of the anchored claim. */
  delegations?: Array<{ delegator: string; decision: string; signature: string }>
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
