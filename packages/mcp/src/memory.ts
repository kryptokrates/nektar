// NektarMemory — the product service the MCP server wraps.
// Wires log (append/batch) → anchor (head UTXO per shard) → broadcast
// → verify (proof bundles) → crypto (BRC-42/AES-GCM), all behind the chain
// interfaces so it runs identically on regtest and live networks.
import { existsSync } from 'node:fs'
import { execFile } from 'node:child_process'
import { EventEmitter } from 'node:events'
import { join } from 'node:path'
import { BudgetAuthority } from '@nektar/automation'
import type { Automation } from '@nektar/automation'
import { PrivateKey, PublicKey, Signature, Transaction } from '@bsv/sdk'
import {
  utf8,
  toHex,
  sha256,
  dsha256,
  concat,
  field,
  merkleRoot,
  merkleProof,
  identityHash,
  diff,
  apply,
  encodeDelta,
  decodeDelta,
  TAG_RECEIPT_SIG,
  type Fragment,
  type Bytes,
  type Provenance,
  walletSigner,
  signerPubHex,
  toSigner,
  nektarEnv,
  type SignerLike,
  type DigestSigner,
  type SignerWallet
} from '@nektar/core'
import { BatchLog, MemoryShardStore, verifyProvenance, PromiseMonitor, type ShardStore, type LogRecord, type Checkpoint } from '@nektar/log'
import { Metrics } from './metrics.js'

const DELTA_PREFIX = 'delta:'

/** write refused at ingest (COM-003) */
export class IngestError extends Error {}
import { AnchorChain, type CheckpointBond, type BondUtxo } from '@nektar/anchor'
import { BondOps } from './bonds.js'
import { RevocationOps } from './revocations.js'
import { CustodyOps, type CustodyOpsDeps } from './custody.js'
import { TxOps, type TxOpsDeps } from './txops.js'
import { AnchorOps } from './anchorops.js'
import { IdentityOps } from './identityops.js'
import { KeyOps } from './keyops.js'
import { CryptoOps } from './cryptoops.js'
import { CatalogOps } from './catalogops.js'
import { PackOps } from './packops.js'
import { PackWire } from './packwire.js'
import { AuthorshipOps } from './authorshipops.js'
import { ZkQueryOps } from './zkquery.js'
import { AttestOps } from './attestops.js'
import { ProofStore } from './proofstore.js'
import { AutomatonChain, type AutomatonDef, type AutomatonHead } from '@nektar/automaton'
import { buildProofBundle, verifyFragment, type ProofBundle, type AnchorLink, type VerifyResult } from '@nektar/verify'
import { encryptFragment, decryptFragment, shredKey, type FragmentContext } from '@nektar/crypto'
import { TxStore } from '@nektar/chain'
import type { Broadcaster, ChainTracker, Funder } from '@nektar/chain'
import * as idbind from '@nektar/identity'
import { ZkProveOps } from './zkprove.js'

// the domain vocabulary lives in ./types.js — a leaf module so the *ops
// modules can share these types with NektarMemory without a memory↔ops
// type cycle. Re-exported here so existing './memory.js' imports keep working.
import {
  CATALOG_SHARD,
  type OperatorWallet,
  type NektarMemoryOptions,
  type PersistedMemoryState,
  type MemoryStateStore,
  type ShardPack,
  type ManifestOffer,
  type ShardManifest,
  type SecurityStateStore,
  type CallerIdentity,
  type ZkClearance,
  type RememberResult,
  type AnswerReceipt
} from './types.js'
export type {
  OperatorWallet,
  NektarMemoryOptions,
  PersistedMemoryState,
  MemoryStateStore,
  ShardPack,
  ManifestOffer,
  ShardManifest,
  SecurityStateStore,
  CallerIdentity,
  ZkClearance,
  RememberResult,
  AnswerReceipt
}
export { CATALOG_SHARD }

// (types moved to ./types.js — see the import/export block above)

/** recursive canonical JSON (sorted keys) — same scheme as reasoning/canonicalJson
 *  so digest bindings are stable across verifiers. */
function canonicalJson(v: unknown): string {
  if (v === null || typeof v !== 'object') return JSON.stringify(v)
  if (Array.isArray(v)) return `[${v.map(canonicalJson).join(',')}]`
  return `{${Object.keys(v as Record<string, unknown>)
    .sort()
    .map((k) => `${JSON.stringify(k)}:${canonicalJson((v as Record<string, unknown>)[k])}`)
    .join(',')}}`
}

/** §18: the signed receipt digest — every receipt field except signature.
 *  Explicit field order (no JSON ambiguity): answer, each ref
 *  (shard,seq,hdp,txid), checkResult, truthNote, modelId, zkaccess, cedar,
 *  dag triple, delegation map, zk artifact, timestamp. */
function receiptDigest(r: Omit<AnswerReceipt, 'signature' | 'signerPub'>): Bytes {
  const refs = r.fragmentRefs.map((f) => concat(utf8(f.shard), utf8(String(f.seq)), utf8(f.hdp), utf8(f.anchorTxid ?? '')))
  const dagBytes = r.dag ? concat(utf8(r.dag.policyRoot), utf8(r.dag.derivationHash ?? ''), utf8(r.dag.state)) : new Uint8Array(0)
  const delBytes = concat(...(r.delegations ?? []).map((d) => concat(utf8(d.delegator), utf8(d.decision), utf8(d.signature))))
  const zkBytes = r.zk ? utf8(canonicalJson(r.zk)) : new Uint8Array(0)
  const modelBytes = r.model ? utf8(canonicalJson(r.model)) : new Uint8Array(0)
  const t = Buffer.alloc(8)
  t.writeBigUInt64LE(BigInt(r.timestamp ?? 0))
  return dsha256(
    concat(
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
    )
  )
}

/** §13: re-verify the receipt's ZKAccess artifact — the Π_j a third party
 *  checks rather than trusting the `zkaccess` flag. Fails when the artifact is
 *  absent (nothing to re-verify), malformed, or the proof doesn't hold. */
export function verifyReceiptZk(r: Pick<AnswerReceipt, 'zk'>, opts?: { trustedAuthorities?: string[]; holder?: string }): { ok: boolean; reason?: string } {
  if (!r.zk) return { ok: false, reason: 'no zk artifact on receipt' }
  let p: import('@nektar/identity').ClearancePresentation
  try {
    p = idbind.decodePresentation(r.zk)
  } catch (e) {
    return { ok: false, reason: `malformed zk artifact: ${(e as Error).message}` }
  }
  return idbind.verifyAccess(p.commitment, p.requiredLevel, p.proof, p.transcript, { trustedAuthorities: opts?.trustedAuthorities, holder: opts?.holder })
    ? { ok: true }
    : { ok: false, reason: 'clearance proof does not verify' }
}

/** verify a receipt's terminal signature against its embedded signerPub */
export function verifyAnswerReceipt(r: AnswerReceipt): { ok: boolean; reason?: string } {
  if (!r.signature || !r.signerPub) return { ok: false, reason: 'unsigned receipt' }
  const { signature, signerPub, ...unsigned } = r
  try {
    const ok = PublicKey.fromString(signerPub).verify([...receiptDigest(unsigned)], Signature.fromDER(signature, 'hex'))
    return ok ? { ok: true } : { ok: false, reason: 'signature does not bind this receipt to the signer' }
  } catch (e) {
    return { ok: false, reason: `malformed signature: ${(e as Error).message}` }
  }
}

export class NektarMemory {
  /** optimistic-claim surface (regtest/main runtimes attach a ClaimService) */
  claims: any
  attachClaims(c: any): void {
    this.claims = c
  }
  /** AGT-011 declared model identity — set by modelManifest; receipts reference it.
   *  DECLARED, not proven: an anchored manifest says which model the operator
   *  says answered, not that the inference ran those weights (needs zkML/TEE).
   *  (Distinct from opts.modelId — the string stamped into AnswerReceipts.) */
  declaredModel?: import('@nektar/reasoning').ModelIdentity
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
  private log: BatchLog
  private anchors = new Map<string, AnchorChain>()
  private tails = new Map<string, AnchorLink[]>()
  private anchorTxs = new Map<string, import('@bsv/sdk').Transaction>()
  private txidShard = new Map<string, string>()

  constructor(
    private chain: ChainTracker & Broadcaster,
    private funder: Funder,
    private fundKey: PrivateKey | undefined,
    private opts: NektarMemoryOptions = {},
    private store: ShardStore = new MemoryShardStore()
  ) {
    if (opts.shardPubs !== undefined) this._shardPubs = opts.shardPubs instanceof Map ? new Map(opts.shardPubs) : new Map(Object.entries(opts.shardPubs))
    this.keyOps = new KeyOps({
      opts: this.opts,
      fundKey: () => this.fundKey,
      shardPubs: this._shardPubs
    })
    this.logKey = opts.signingKey ?? (opts.wallet !== undefined ? this.walletLogSigner() : PrivateKey.fromRandom())
    this._logPub = this.logKey instanceof PrivateKey ? this.logKey.toPublicKey().toString() : undefined
    this.log = new BatchLog(store, this.logKey, opts.batchPolicy ?? { maxLeaves: 16 })
    if (opts.dataDir) this.budget = new BudgetAuthority(opts.dataDir)
    this.bonds = new BondOps({
      chain: this.chain,
      txs: () => this.txs,
      fetchTx: (t) => this.fetchTx(t),
      takeFunding: (s) => this.takeFunding(s),
      fundKey: this.fundSigner(),
      bondOpts: () => this.opts.bond,
      network: this.opts.network,
      persist: () => this.persistState()
    })
    this.anchorOps = new AnchorOps({
      anchors: this.anchors,
      tails: this.tails,
      anchorTxs: this.anchorTxs,
      shards: () => this.shards(),
      persistState: () => this.persistState(),
      log: this.log,
      anchorChainFor: (s) => this.anchorChain(s),
      txidShard: this.txidShard,
      cps: this.cps,
      logKey: () => toSigner(this.logKey),
      metricsInc: (n, l, x, h) => this.metrics.inc(n, l, x, h),
      covered: () => this.covered(),
      getPromises: () => this.promises,
      setPromises: (p) => {
        this.promises = p
      },
      bondsActive: () => this.bonds.active,
      bondsMirror: (r) => this.bonds.mirror(r)
    })
    this.cryptoOps = new CryptoOps({ opts: this.opts })
    this.catalogOps = new CatalogOps({
      opts: this.opts,
      store: this.store,
      recordText: (r) => this.recordText(r),
      shards: () => this.shards(),
      shardId: (s) => this.shardId(s),
      remember: (s, d, p, l) => this.remember(s, d, p, l),
      exportShardPack: (s) => this.exportShardPack(s),
      storePack: (s, p, n) => this.storePack(s, p, n),
      metricsInc: (n) => this.metrics.inc(n)
    })
    this.authorshipOps = new AuthorshipOps({
      opts: this.opts,
      anchors: this.anchors,
      tails: this.tails,
      anchorTxs: this.anchorTxs,
      txidShard: this.txidShard,
      anchorChain: (s) => this.anchorChain(s),
      shardPubs: this._shardPubs,
      storeAll: (s) => this.store.all(s),
      storeBySeq: (s, seq) => this.store.bySeq(s, seq),
      canEncrypt: () => this.canEncrypt(),
      decryptRecord: (r) => this.decryptRecord(r),
      prove: (s, seq) => this.prove(s, seq),
      remember: (s, d) => this.remember(s, d),
      anchorShard: (s) => this.anchorShard(s),
      persistState: () => this.persistState()
    })
    this.packWire = new PackWire({
      shardPrivate: (s) => this.shardPrivate(s),
      storeAll: (s) => this.store.all(s),
      prove: (s, seq) => this.prove(s, seq),
      shardManifest: (s) => this.shardManifest(s),
      anchorTxs: this.anchorTxs,
      txidShard: this.txidShard,
      importedBundles: this.importedBundles,
      touched: this.touched,
      logRemember: (s, f, m) => this.log.remember(s, f, m),
      remember: (s, d, p, l) => this.remember(s, d, p, l),
      chain: this.chain,
      persistState: () => this.persistState()
    })
    this.packOps = new PackOps({
      opts: this.opts,
      packStores: this.packStores,
      savedPacksStore: () => this.savedPacksStore,
      shardId: (s) => this.shardId(s),
      persistState: () => this.persistState(),
      metricsInc: (n) => this.metrics.inc(n)
    })
    this.attestOps = new AttestOps({
      attestationTxs: this.attestationTxs,
      anchorTxs: this.anchorTxs,
      takeFunding: (s) => this.takeFunding(s),
      fundSigner: () => this.fundSigner(),
      chain: this.chain,
      feeRateSatPerKb: () => this.opts.feeRateSatPerKb ?? 1,
      markSpentOut: (t, v) => this.markSpentOut(t, v),
      addOwnedOut: (tx, v) => this.addOwnedOut(tx, v),
      metricsInc: (n, l, x, h) => this.metrics.inc(n, l, x, h),
      proveMembership: (s, q, qid) => this.proveMembership(s, q, qid)
    })
    this.proofStore = new ProofStore({
      dataDir: () => this.opts.dataDir ?? nektarEnv('NEKTAR_DATA_DIR') ?? join(process.cwd(), '.nektar'),
      objectStore: () => this.opts.objectStore,
      persistState: () => this.persistState(),
      metricsInc: (n) => this.metrics.inc(n)
    })
    this.zkProveOps = new ZkProveOps({
      store: this.store,
      tails: this.tails,
      anchorTxs: this.anchorTxs,
      anchors: this.anchors,
      chain: this.chain,
      consumeZkQuery: (q) => this.consumeZkQuery(q),
      peekZkQuery: (q) => this.peekZkQuery(q),
      zkQueryConsumed: (q) => this.zkQueryConsumed(q),
      verifyClearance: (c, z, s) => this.verifyClearance(c, z, s),
      policyDag: () => this.opts.policyDag,
      anchoredPolicyRoot: () => this.opts.anchoredPolicyRoot
    })
    this.zkQueryOps = new ZkQueryOps({
      zkQueries: this.zkQueries,
      persistSecurityState: () => this.persistSecurityState()
    })
    this.identityOps = new IdentityOps({
      challenges: this.challenges,
      zkSeen: this.zkSeen,
      clearanceAuthorities: () => this.opts.clearanceAuthorities,
      policy: () => this.opts.policy,
      persistSecurityState: () => this.persistSecurityState()
    })
    this.txOps = new TxOps({
      txs: () => this.txs,
      anchorTxs: this.anchorTxs,
      attestationTxs: this.attestationTxs,
      txidShard: this.txidShard,
      attachProof: (s, t, mp) => this.attachProof(s, t, mp),
      chain: this.chain as TxOpsDeps['chain'],
      sdkTracker: () => this.sdkTracker(),
      txLookup: () => this.txLookup,
      proofLookup: () => this.proofLookup,
      network: this.opts.network,
      txStatus: this.opts.txStatus
    })
    this.custodyOps = new CustodyOps({
      funder: this.funder as CustodyOpsDeps['funder'],
      fundSigner: () => this.fundSigner(),
      broadcast: (tx) => this.chain.broadcast(tx),
      txs: (tx) => this.txs.put(tx),
      ledger: () => this.ledger,
      network: this.opts.network,
      feeRateSatPerKb: this.opts.feeRateSatPerKb
    })
    this.revocationOps = new RevocationOps({
      store: this.store,
      canEncrypt: () => this.canEncrypt(),
      decryptRecord: (r) => this.decryptRecord(r),
      prove: (s, seq) => this.prove(s, seq),
      persistSecurityState: () => this.persistSecurityState()
    })
    this.restoreSecurityState()
    this.restoreState()
    // push confirmations: backends that emit status events (regtest stack)
    // deliver 'mined' proofs straight in — no confirmation polling
    this.chain.onStatus?.((ev) => {
      if (ev.status === 'mined' && ev.merklePath) this.deliverTxProof(ev.txid, ev.merklePath)
      if (ev.status === 'rejected' || ev.status === 'double-spent') this.deliverTxRejected(ev.txid)
    })
  }
  private logKey: SignerLike
  private _logPub?: string
  public budget?: import('@nektar/automation').BudgetAuthority
  public automation?: Automation
  public ledger?: import('@nektar/automation').AutomationStore
  /** every transaction this instance built, broadcast or was handed, with
   *  its proof once one exists — parents for spends and BEEF for counterparties
   *  come from here, not from an indexer */
  get txs(): TxStore {
    return (this.opts.txStore ??= new TxStore())
  }
  /** network lookups for what the instance was never handed (a foreign bond,
   *  a transaction from before a database loss, a proof missed while down).
   *  Set by the live runtime; each is a single attempt and never sleeps. */
  txLookup?: (txid: string) => Promise<import('@bsv/sdk').Transaction | undefined>
  proofLookup?: (txid: string) => Promise<import('@bsv/sdk').MerklePath | undefined>
  private txOps!: TxOps
  private anchorOps!: AnchorOps
  private identityOps!: IdentityOps
  private keyOps!: KeyOps
  private cryptoOps!: CryptoOps
  private catalogOps!: CatalogOps
  private packOps!: PackOps
  private packWire!: PackWire
  private authorshipOps!: AuthorshipOps
  private zkQueryOps!: ZkQueryOps
  private attestOps!: AttestOps
  private zkProveOps!: ZkProveOps

  async fetchTx(txid: string): Promise<import('@bsv/sdk').Transaction | undefined> {
    return await this.txOps.fetchTx(txid)
  }
  /** keep a transaction (and the ancestry it carries) in the instance's graph */
  rememberTx(tx: import('@bsv/sdk').Transaction): void {
    this.txOps.rememberTx(tx)
  }
  /** hex to hand a counterparty: BEEF when the ancestry is proven, else EF, else raw */
  txWire(txid: string, maxBytes?: number): string | undefined {
    return this.txOps.txWire(txid, maxBytes)
  }
  /** read a counterparty's wire transaction and run SPV against our own headers */
  async readTxWire(hex: string): Promise<import('@nektar/chain').Ingested> {
    return await this.txOps.readTxWire(hex)
  }
  /** state-change events for live surfaces (dashboard SSE, watchers) —
   *  emitted once per successful mutating tool call by the server layer */
  readonly events = new EventEmitter()
  /** subscribe to verified tx proofs — fed by verified broadcaster callbacks
   *  (http layer checks the BUMP first) and by chain 'mined' status events */
  onTxProof(cb: (txid: string, mp: import('@bsv/sdk').MerklePath) => void): void {
    this.txOps.onTxProof(cb)
  }
  /** a caller-verified or chain-observed merkle proof: attach to its anchor,
   *  then notify subscribers */
  deliverTxProof(txid: string, mp: import('@bsv/sdk').MerklePath): void {
    this.txOps.deliverTxProof(txid, mp)
  }

  /** subscribe to definitive transaction rejections — fed by verified
   *  broadcaster callbacks (REJECTED / DOUBLE_SPEND_ATTEMPTED) and by chain
   *  'rejected'/'double-spent' status events */
  onTxRejected(cb: (txid: string) => void): void {
    this.txOps.onTxRejected(cb)
  }
  /** record a rejection the backend reported, then notify subscribers */
  deliverTxRejected(txid: string): void {
    this.txOps.deliverTxRejected(txid)
  }

  /** what the chain view knows about a tx right now */
  async txState(txid: string): Promise<'mined' | 'seen' | 'rejected' | 'unknown'> {
    return await this.txOps.txState(txid)
  }
  /** the log signing key's pubkey (verifies promises and checkpoints) */
  async logPublicKey(): Promise<string> {
    return (this._logPub ??= await signerPubHex(this.logKey))
  }

  /** log receipt signer under wallet custody: createSignature under a stable
   *  keyID — same pub every restart, no key material in-process. */
  private walletLogSigner(): DigestSigner {
    return this.keyOps.walletLogSigner()
  }

  /** arbitrary wallet-derived signer for a nektar keyID — used by rotate to
   *  assign a new shard owner without key material crossing the API. Throws
   *  when the instance has no wallet custody. */
  walletKeySigner(keyID: string): DigestSigner {
    return this.keyOps.walletKeySigner(keyID)
  }

  private shardKey(shard: string): SignerLike {
    return this.keyOps.shardKey(shard)
  }

  private anchorChain(shard: string): AnchorChain {
    if (!this.anchors.has(shard)) {
      this.anchors.set(
        shard,
        new AnchorChain(shard, this.shardKey(shard), this.funder, this.fundSigner(), this.chain, {
          burstLimit: this.opts.burstLimit,
          feeRateSatPerKb: this.opts.feeRateSatPerKb,
          shardPub: this._shardPubs.get(shard)
        })
      )
    }
    return this.anchors.get(shard)!
  }
  /** pre-resolved shard owner pubkeys — runtime fills this for wallet-mode
   *  shards so sync paths (restoreState/resume) never need a round trip. */
  private _shardPubs = new Map<string, string>()

  /** the shard's current anchor head (undefined before the first anchor) */
  anchorHead(shard: string) {
    return this.anchorOps.anchorHead(shard)
  }
  /** an anchor tx by txid (as broadcast, with BUMP when attached) */
  anchorTx(txid: string) {
    return this.anchorOps.anchorTx(txid)
  }
  /** the shard's log records (read view — used by prove/disclose tests) */
  async records(shard: string) {
    return await this.catalogOps.records(shard)
  }
  /** plaintext candidates for marketplace matching — owner-side decrypt of own
   *  encrypted level-0 records; the sale still delivers only the fragment key */
  async marketCandidates(shard: string): Promise<Array<{ seq: number; text: string }>> {
    return await this.catalogOps.marketCandidates(shard)
  }
  /** broadcast a fully-signed tx through the instance's broadcaster (SpendGuard stays in front) — covenant host tools only */
  async broadcastTx(tx: import('@bsv/sdk').Transaction): Promise<import('@nektar/chain').BroadcastResult> {
    const r = await this.chain.broadcast(tx)
    if (r.status === 'success') this.txs.put(tx)
    return r
  }
  /** the operator fee key — throws under wallet custody (NEKTAR_WALLET=client);
   *  all signing call sites take `operatorSigner()`/`fundSigner()` instead. */
  operatorKey(): PrivateKey {
    return this.keyOps.operatorKey()
  }

  /** the operator signing surface — the fund key when held, otherwise the
   *  wallet's identity key. Every legacy `.sign()` site routes here. */
  operatorSigner(): DigestSigner {
    return this.keyOps.operatorSigner()
  }
  /** funding-signer — P2PKH inputs, covenant funding calls, change output
   *  derivation. Toolbox mode: keyDeriver root key (exact legacy custody —
   *  the app holds it). Client mode: a stable self-derived wallet keyID
   *  (nektar-fund) whose pkh the reserve funds and spends under. */
  fundSigner(): DigestSigner {
    return this.keyOps.fundSigner()
  }
  /** register a pre-resolved shard owner pubkey (wallet-mode restore path) */
  registerShardPub(shard: string, pub: string): void {
    this.keyOps.registerShardPub(shard, pub)
  }

  operatorWallet(): OperatorWallet {
    return this.keyOps.operatorWallet()
  }
  /** disclosure-wrap custody: the wallet's BRC-42 encrypt/decrypt under
   *  wallet mode, else the raw fund key — ECDH cannot go through a signer
   *  (the wallet deliberately never exposes the shared point). */
  wrapCustody(): PrivateKey | import('@bsv/sdk').WalletInterface {
    return this.keyOps.wrapCustody()
  }
  /** funding outs for covenant hosts (escrow deposits, covenant inputs) —
   *  callers markSpent/addOwned after a successful broadcast like
   *  anchorAttestation does. */
  async fundingOuts(satoshis: number): Promise<import('@nektar/chain').FundingOut[]> {
    const u = await this.takeFunding(satoshis)
    return [u]
  }
  private custodyOps!: CustodyOps
  /** §7 key-sale (GOAL-v7 V3): re-derive the fragment key for an encrypted
   *  record — the decryption capability being sold. Caller must ensure the
   *  record's level is within this instance's maxLevel. Wallet custody: the
   *  fragment key is a deterministic wallet hmac under the record's canonical
   *  keyID (self counterparty) — no seed bytes leave the wallet and the
   *  derived capability can still be handed over in a sale. */
  async fragmentKeyFor(rec: import('@nektar/log').LogRecord): Promise<Uint8Array> {
    return await this.cryptoOps.fragmentKeyFor(rec)
  }
  /** all fragment-key candidates this instance can derive, wallet scheme
   *  first (the write scheme under wallet custody), legacy seed second —
   *  dual-read: a record decrypts under exactly one (GCM tag enforces it). */
  private async fragmentKeyCandidates(c: FragmentContext): Promise<Uint8Array[]> {
    return await this.cryptoOps.fragmentKeyCandidates(c)
  }
  /** decrypt an encrypted record — tries every custody scheme (dual-read) */
  private async decryptRecord(rec: import('@nektar/log').LogRecord): Promise<string | undefined> {
    return await this.cryptoOps.decryptRecord(rec)
  }
  private canEncrypt(): boolean {
    return this.cryptoOps.canEncrypt()
  }
  /** return a tx output to the reserve as owned (change/escrow outs) */
  addOwnedOut(tx: import('@bsv/sdk').Transaction, vout: number): void {
    this.custodyOps.addOwnedOut(tx, vout)
  }
  /** mark a spent input against the reserve after broadcast */
  markSpentOut(txid: string, vout: number): void {
    this.custodyOps.markSpentOut(txid, vout)
  }
  /** current chain height for locktime-gated covenant calls */
  async chainHeight(): Promise<number> {
    return this.chain.tipHeight ? await this.chain.tipHeight() : await this.chain.currentHeight()
  }
  /** the instance's chain tracker in @bsv/sdk ChainTracker shape (bstack checkCarrier/verify) */
  sdkTracker(): import('@bsv/sdk').ChainTracker {
    return this.chain
  }
  /** {hash,height} pair near the current tip — walks back a few blocks because
   *  the header index lags the tip; the chain-bound identity an artifact claims */
  async tipRef(): Promise<{ tip: string; tipHeight: number } | undefined> {
    const h0 = await this.chainHeight()
    for (let h = h0; h > h0 - 3 && h > 0; h--) {
      const hdr = await this.chain.headerAtHeight(h).catch(() => undefined)
      if (hdr) return { tip: hdr.hash, tipHeight: h }
    }
    return undefined
  }
  /** the shard's latest anchor link (tail of the on-chain succession) */
  tailLink(shard: string) {
    return this.anchorOps.tailLink(shard)
  }
  /** anchors still waiting for their BUMP */
  unprovenAnchors(): Array<{ shard: string; txid: string }> {
    return this.anchorOps.unprovenAnchors()
  }
  /** attach a BUMP to an anchor (must prove THIS txid) and mark the chain confirmed */
  attachProof(shard: string, txid: string, mp: import('@bsv/sdk').MerklePath): void {
    this.anchorOps.attachProof(shard, txid, mp)
  }
  /** shards this instance has written or anchored */
  shards(): string[] {
    return [...new Set([...this.anchors.keys(), ...this.touched])]
  }
  /** every anchor link of the shard, genesis first (read-only copy) */
  anchorLinks(shard: string): AnchorLink[] {
    return this.anchorOps.anchorLinks(shard)
  }

  /** bundles of pack-imported records ('shard:seq' → ProofBundle) — the only
   *  proof path for records anchored by a DIFFERENT instance's chain */
  private importedBundles = new Map<string, ProofBundle>()

  /** rotate a shard's owner key on-chain (rotation cert emitted by AnchorChain).
   *  D4 (§19): the cert is also written to the `__identity__` shard and anchored
   *  — a third party reconstructs the pk(0)→pk(i) ledger from chain data alone. */
  async rotateShard(shard: string, newKey: SignerLike): Promise<{ txid: string; n: number }> {
    return await this.authorshipOps.rotateShard(shard, newKey)
  }

  /** revocation registry (IDN-007): lives in revocations.ts — SMT keyed by
   *  derive_key(pubkey); tests/server read the SMT directly via this getter */
  private revocationOps!: RevocationOps
  get revocations(): idbind.Smt {
    return this.revocationOps.smt
  }
  /** revoke a 33-byte pubkey: SMT key = derive_key(pubkey); persisted */
  revoke(pubHex: string): Uint8Array {
    return this.revocationOps.revoke(pubHex)
  }
  isRevoked(pubHex: string): boolean {
    return this.revocationOps.isRevoked(pubHex)
  }

  /** §19 third-party revocation proof — implemented in revocations.ts */
  async revocationProof(pubHex: string) {
    return await this.revocationOps.revocationProof(pubHex)
  }

  /** §19 CRL status — implemented in revocations.ts */
  async revocationStatus(pubHex: string) {
    return await this.revocationOps.revocationStatus(pubHex)
  }

  // ── §12 SLA bond (NektarCheckpointBond) — lifecycle lives in bonds.ts ──
  private bonds!: BondOps

  async bondDeploy(sats = this.opts.bond?.sats ?? 5_000, releaseHeight?: bigint, force = false): Promise<{ txid: string; releaseHeight: bigint }> {
    return await this.bonds.deploy(sats, releaseHeight, force)
  }
  bondStatus(): ReturnType<BondOps['status']> {
    return this.bonds.status()
  }
  async bondSeize(payToScript?: string, bond?: { cfg: ConstructorParameters<typeof CheckpointBond>[0]; utxo: BondUtxo }): Promise<{ txid: string } | null> {
    return await this.bonds.seize(payToScript, bond)
  }
  async bondCommit(root: string): Promise<{ txid: string; n: bigint } | null> {
    return await this.bonds.commit(root)
  }
  async bondClose(): Promise<{ txid: string } | null> {
    return await this.bonds.close()
  }

  /** build + sign the payment tx for an invoice (payQuote → invoice fields)
   *  using the instance's own fund key — the agent pays without an external
   *  wallet. §16 audit trail: settlement IS an anchored tx — the payment
   *  receipt is publicly re-verifiable, not a server-side claim. */
  async payInvoice(inv: {
    invoiceId: string
    amount: number
    lockingScriptHex: string
    opReturn: string
  }): Promise<{ payment: { invoiceId: string; transaction: string }; txid: string }> {
    return await this.custodyOps.payInvoice(inv)
  }

  /** one-call payment: build + sign + broadcast a plain spend to a payee
   *  address or locking script. Unlike payInvoice (which returns a BEEF for
   *  the payee's settlePayment), this airs the tx and returns its txid —
   *  the way to fund another agent's fee address. `buildOnly` returns the
   *  fee'd, template-unlocked tx without broadcasting — the wallet-custody
   *  path signs and submits it itself (market direct rail). `utxo` names
   *  the payment output so an accept envelope can point at it. */
  async sendSats(args: {
    amount: number
    address?: string
    scriptHex?: string
    opReturn?: string
    buildOnly?: boolean
  }): Promise<{ txid: string; paidSats: number; tx?: Transaction; txHex?: string; utxo?: { txid: string; vout: number; satoshis: number; script: string } }> {
    return await this.custodyOps.sendSats(args)
  }

  /** D4 (§19 M_U): the shard's anchored rotation ledger — genesis owner pub
   *  plus every `rotation-cert` record in `__identity__` with its inclusion
   *  bundle. A verifier replays verifyCertChain over certs it can each prove
   *  were anchored; a withheld cert can only make the chain look older. */
  async rotationChainProof(shard: string): Promise<{
    genesisPub: string | null
    certs: Array<{
      cert: import('@nektar/identity').RotationCert
      seq: number
      data: string
      prov?: import('@nektar/core').Provenance
      E?: Bytes
      leaf: string
      bundle?: import('@nektar/verify').ProofBundle
      anchored: boolean
    }>
  } | null> {
    return await this.authorshipOps.rotationChainProof(shard)
  }

  // ── persisted replay-protection state (challenges, transcripts, issued ZK queries, revocations)
  /** ZK queries issued by THIS verifier: queryId hex → {nonce, verifierId, expiry, consumed} (single use).
   *  nonce+verifierId let verifyCombined rebuild the StarkQuery for the Π binding.
   *  `consumed` entries are kept as tombstones — the re-verify ledger. */
  private zkQueries = new Map<string, { nonce: string; verifierId: string; expiry: number; consumed?: boolean }>()
  private persistSecurityState(): void {
    this.opts.securityStore?.save({
      challenges: [...this.challenges].filter(([, exp]) => exp >= Date.now()),
      zkSeen: [...this.zkSeen],
      // consumed tombstones persist past expiry — they are the re-verify
      // ledger: a proof whose queryId was consumed IS the committed one.
      zkQueries: [...this.zkQueries]
        .filter(([, e]) => e.expiry >= Date.now() || e.consumed === true)
        .map(([q, e]) => [q, e.expiry, e.nonce, e.verifierId, e.consumed === true]),
      revocations: this.revocationOps.snapshot()
    })
  }
  private restoreSecurityState(): void {
    const s = this.opts.securityStore?.load()
    if (!s) return
    for (const [n, exp] of s.challenges) this.challenges.set(n, exp)
    for (const k of s.zkSeen) this.zkSeen.add(k)
    for (const row of s.zkQueries ?? [])
      this.zkQueries.set(row[0], { expiry: row[1], nonce: row[2] ?? '', verifierId: row[3] ?? '', consumed: row[4] === true })
    this.revocationOps.restore(s.revocations)
  }
  /** verifier side: issue a single-use ZK membership query (default 10 min).
   *  queryId = dsha256('nektar:zkq:v2' ‖ nonce ‖ verifierId); the prover binds it into the STARK journal. */
  issueZkQuery(verifierId: string, ttlMs = 600_000): { nonce: string; verifierId: string; queryId: string } {
    return this.zkQueryOps.issueZkQuery(verifierId, ttlMs)
  }
  private consumeZkQuery = (queryIdHex: string): boolean => this.zkQueryOps.consumeZkQuery(queryIdHex)
  /** issued-and-fresh query WITHOUT consuming it — verifyCombined checks the
   *  §13 Π binding first, consumes once at the end (single use stays). */
  private peekZkQuery(queryIdHex: string): { nonce: string; verifierId: string } | undefined {
    return this.zkQueryOps.peekZkQuery(queryIdHex)
  }
  /** re-verify registry: a queryId passes iff it was CONSUMED — i.e. this
   *  exact proof already verified once (consume runs last, only after all
   *  other checks held). The tombstone survives expiry + restart. */
  private zkQueryConsumed = (queryIdHex: string): boolean => this.zkQueryOps.zkQueryConsumed(queryIdHex)

  // ── P14 full-mode automata (AUT-005): one Rúnar NektarAutomaton UTXO per id,
  // funded by the instance funder, owned by its own key.
  private automata = new Map<string, AutomatonChain>()
  async automatonDeploy(id: string, def: AutomatonDef, ownerKey?: SignerLike): Promise<AutomatonHead> {
    if (this.automata.has(id)) throw new Error(`automaton ${id} already deployed`)
    // wallet custody: a per-automaton derived keyID — deterministic across
    // restarts (same wallet → same owner pubkey), no stored key material.
    const owner =
      ownerKey ??
      (this.opts.wallet !== undefined
        ? walletSigner(this.opts.wallet as SignerWallet, { protocolID: [2, 'nektar'], keyID: `nektar-automaton:${id}`, counterparty: 'self' })
        : PrivateKey.fromRandom())
    const ac = new AutomatonChain(def, owner, this.funder, this.fundSigner(), this.chain)
    const h = await ac.deploy()
    this.automata.set(id, ac)
    return h
  }
  automaton(id: string): AutomatonChain {
    const ac = this.automata.get(id)
    if (!ac) throw new Error(`no automaton ${id}`)
    return ac
  }

  private ctx(shard: string, seq: number, level: number): FragmentContext {
    return this.cryptoOps.ctx(shard, seq, level)
  }

  /** holder side: STARK membership proof bound to a verifier-issued queryId
   *  (Wright §13 — contract/mechanism documented on ZkProveOps). */
  async proveMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number }> {
    return await this.zkProveOps.proveMembership(shard, seq, queryIdHex)
  }

  // ── GOAL-v6 S1a (§13 "on-chain if necessary"): cheap receipt commitments —
  // OP_FALSE OP_RETURN sha256(proof) in a fee-key-funded data tx. Not an
  // anchor: no shard state, no contract. Off-chain WASM verify stays
  // authoritative; the chain only timestamps the commitment.
  private attestationTxs = new Map<string, import('@bsv/sdk').Transaction>()
  /** one funding outpoint from the fee reserve — CustodyOps probes the
   *  three funder shapes (StackFunder.take / takeReserve / peeked utxo) */
  private async takeFunding(satoshis: number): Promise<import('@nektar/chain').FundingOut> {
    return await this.custodyOps.takeFunding(satoshis)
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  async fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }> {
    return await this.custodyOps.fundingStatus()
  }

  /** anchor sha256(proofBytes) in an OP_FALSE OP_RETURN data tx funded by the
   *  fee key (~1 sat of fee at regtest rates). Returns the txid + commitment. */
  async anchorAttestation(proofBytes: Uint8Array): Promise<{ txid: string; commitment: string }> {
    return await this.attestOps.anchorAttestation(proofBytes)
  }

  /** holder side (§13/S1a): produce the STARK membership proof AND anchor its
   *  commitment on-chain. The queryId stays unconsumed — the verifier's own
   *  verifyMembership call still succeeds (single use is its privilege). */
  async attestMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number; txid: string; commitment: string }> {
    return await this.attestOps.attestMembership(shard, seq, queryIdHex)
  }

  /** verifier side: fetch the attestation tx, extract the OP_RETURN hash and
   *  check it equals sha256(supplied proof bytes). Only txs broadcast by THIS
   *  instance are resolvable (chain fetch by txid is an ops gap, CHN-*). */
  async verifyAttestation(txid: string, proofBytes: Uint8Array): Promise<{ ok: boolean; reason?: string; commitment?: string; vout?: number }> {
    return await this.attestOps.verifyAttestation(txid, proofBytes)
  }

  /** verifier side: anchored-STARK verification against THIS shard's
   *  canonical chain; the queryId is ours, single use (ZkProveOps). */
  async verifyMembership(shard: string, seq: number, bytes: Uint8Array): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    return await this.zkProveOps.verifyMembership(shard, seq, bytes)
  }

  /** any tx this instance broadcast and remembers — anchor or attestation
   *  (model-verifier deploy/spend land in attestationTxs too) */
  knownTx(txid: string) {
    return this.txOps.knownTx(txid)
  }

  /** deploy an in-script Groth16 verifier (lock) and spend it with the proof
   *  (unlock) — consensus itself runs the verification. The spend txid is the
   *  attestation a receipt cites: it exists on-chain or it doesn't. */
  async attestInferenceOnchain(lockHex: string, unlockHex: string): Promise<{ deployTxid: string; spendTxid: string }> {
    return await this.attestOps.attestInferenceOnchain(lockHex, unlockHex)
  }

  // ── public proof store + private-cite re-verification ────────────────────
  // proofs committed inside receipts are public artifacts, served via
  // GET /v1/proof/<sha256> — storage, locator and integrity rules live in
  // ProofStore (canonical docs there).
  /** store a proof artifact; returns its sha256 commitment hex */
  putProof(bytes: Uint8Array): string {
    return this.proofStore.put(bytes)
  }
  /** fetch a stored proof by commitment — digest-checked (ProofStore). */
  getProof(commitHex: string): Uint8Array | undefined {
    return this.proofStore.get(commitHex)
  }
  /** the {endpoint,bucket} locator a receipt carries so a remote verifier
   *  needs no origin call (ProofStore). */
  proofStoreRef(commitHex: string): { endpoint: string; bucket: string } | undefined {
    return this.proofStore.ref(commitHex)
  }
  /** proof bytes by commitment — local dir first, then the locator a receipt
   *  carried; digest-checked (ProofStore). */
  async fetchProof(commitHex: string, store?: { endpoint?: string; bucket?: string }): Promise<Uint8Array | undefined> {
    return await this.proofStore.fetch(commitHex, store)
  }

  /** re-verify a committed private-cite proof WITHOUT consuming — tombstone
   *  registry check: this exact proof must already have verified once
   *  (ZkProveOps). */
  async reverifyMembershipBytes(bytes: Uint8Array, anchorTxid: string): Promise<{ ok: boolean; reason?: string; anchorTxid?: string }> {
    return await this.zkProveOps.reverifyMembershipBytes(bytes, anchorTxid)
  }

  /** verifier side: §13 Π_j = (π_j, ZKAccess) as ONE verifiable object —
   *  same-query binding across clearance/membership/derivation, single-use
   *  consume at the end (ZkProveOps). */
  async verifyCombined(
    shard: string,
    seq: number,
    membershipBytes: Uint8Array,
    zk: ZkClearance,
    opts?: { scope?: string; derivationBytes?: Uint8Array; callerPubkey?: string }
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; statement: string }> {
    return await this.zkProveOps.verifyCombined(shard, seq, membershipBytes, zk, opts)
  }

  /** holder side: selective subset-disclosure proof (L_ZKDis v2) over one
   *  anchored batch — reveals positions + leaf hashes, never contents
   *  (ZkProveOps). */
  async proveSubset(
    shard: string,
    seqs: number[]
  ): Promise<{ ok: true; bytes: Uint8Array; batchIndex: number; size: number; indices: number[] } | { ok: false; reason: string }> {
    return await this.zkProveOps.proveSubset(shard, seqs)
  }

  /** verifier side: claimed batch root → OUR anchored link, then the same
   *  shard/canonical/SPV checks + multiproof fold (ZkProveOps). */
  async verifySubset(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; anchorTxid?: string; size?: number; indices?: number[]; seqs?: number[]; statement: string }> {
    return await this.zkProveOps.verifySubset(shard, bytes)
  }

  /** holder side: §14 derivation STARK under the anchored policy DAG —
   *  journal = (policyRoot, conclusionHash, queryId) (ZkProveOps). */
  async proveDerivation(
    shard: string,
    query: { pred: string; args: string[]; edb?: import('@nektar/datalog').Fact[] },
    queryIdHex: string
  ): Promise<{ bytes: Uint8Array; ms: number }> {
    return await this.zkProveOps.proveDerivation(shard, query, queryIdHex)
  }

  /** verifier side: proven policyRoot must be this verifier's anchored DAG
   *  root; queryId ours, single use (ZkProveOps). */
  async verifyDerivation(
    shard: string,
    bytes: Uint8Array
  ): Promise<{ ok: boolean; reason?: string; policyRoot?: string; conclusionHash?: string; statement: string }> {
    return await this.zkProveOps.verifyDerivation(shard, bytes)
  }

  /**
   * remember: encrypt (if agentSeed configured) → append (dedup) → promise.
   * If the batch threshold is reached, closes the batch and anchors it.
   */
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
  private async authorGuard(shard: string, r: LogRecord): Promise<string | undefined> {
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

  /** resolve the shard's owner cert chain: genesis pub from the genesis
   *  anchor's decoded ownerPub + live rotation certs. Cached per shard. */
  private async authorChain(shard: string): Promise<{ genesisPub: string; certs: import('@nektar/identity').RotationCert[] } | null> {
    return await this.authorshipOps.authorChain(shard)
  }

  /** §19: was record seq written by the key of its recorded epoch, inside that key's window? */
  async authorship(shard: string, seq: number, genesisPub: string, certs: import('@nektar/identity').RotationCert[]) {
    return await this.authorshipOps.authorship(shard, seq, genesisPub, certs)
  }

  /** D6 (App E auditability): optional per-access log — decrypt/traverse events
   *  appended to `__access__` as timestamped records that join the batch flow
   *  and anchor with everything else. OFF by default (`NEKTAR_ACCESS_LOG=1`)
   *  because a readable log of who-read-what leaks the access pattern itself —
   *  enable only where that tradeoff is acceptable. Writes bypass the public
   *  `remember` (no auto-anchor mid-read) and never break a read on failure. */
  private accessLogEnabled(): boolean {
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

  /** Close the pending batch of a shard and broadcast the anchor. */
  async anchorShard(shard: string): Promise<{ txid: string; n: number } | null> {
    return await this.anchorOps.anchorShard(shard)
  }

  private cps = new Map<string, Checkpoint[]>()
  private touched = new Set<string>()
  private promises: Array<{ shard: string; seq: number; issuedAt: number }> = []
  /** owner-declared shard policy — `private` shards may only be cited via ZK
   *  proofs; the flag is monotone (public→private allowed, never back) and
   *  persisted with the rest of the state so it survives restarts. */
  private flags = new Map<string, { private?: boolean }>()
  /** bstore refs of packs this instance PUT (describeShard) — persisted in
   *  state so /.well-known/nektar keeps naming them across restarts */
  private packStores = new Map<string, import('@nektar/store').ObjectRef>()
  private proofStore!: ProofStore
  private savedPacksStore: { members?: import('@nektar/store').PackMember[]; manifestSeq?: number } | undefined
  /** is this shard declared private? */
  shardPrivate(shard: string): boolean {
    return this.flags.get(shard)?.private === true
  }

  /** shards an operator can offer as public packs — excludes private shards,
   *  `__*` system shards, and any shard holding level>0 records (the same
   *  gates exportShardPack enforces; listing here means "fetchable"). */
  async publicShards(): Promise<string[]> {
    const out: string[] = []
    for (const s of this.shards()) {
      if (s.startsWith('__') || this.shardPrivate(s)) continue
      if ((await this.store.all(s)).some((r) => (r.level ?? 0) > 0)) continue
      out.push(s)
    }
    return out
  }
  /** declare a shard private — monotone; once set it cannot be unset. */
  setShardPrivate(shard: string): void {
    const f = this.flags.get(shard) ?? {}
    f.private = true
    this.flags.set(shard, f)
    this.persistState()
  }

  /** the shard's on-chain id — hash160 of the genesis owner pubkey, stable
   *  across owner rotations (the anchor head is authoritative when present).
   *  Binds a manifest's label to the anchor chain a verifier can look up. */
  async shardId(shard: string): Promise<string> {
    const ac = this.anchorChain(shard)
    await ac.ready
    return ac.getShardId()
  }

  /** read a record's plaintext — decrypting E via the level key when the
   *  instance holds the seed (same path recall/disclose use). */
  private async recordText(rec: LogRecord): Promise<string | undefined> {
    return await this.cryptoOps.recordText(rec)
  }

  /** latest committed manifest per described shard — the catalog projection
   *  the market indexes (one entry per shard, latest record wins). */
  async shardCatalog(): Promise<ShardManifest[]> {
    return await this.catalogOps.shardCatalog()
  }

  /** latest committed manifest for one shard */
  async shardManifest(shard: string): Promise<ShardManifest | undefined> {
    return await this.catalogOps.shardManifest(shard)
  }

  /** the content price a manifest declares for `kind` ('pack' or 'read' via
   *  'answer' offers) — 0 when free/undeclared. Drives the paid-content gate. */
  async contentPrice(shard: string, kind: 'read' | 'pack'): Promise<number> {
    return await this.catalogOps.contentPrice(shard, kind)
  }

  /** land a purchased record locally — call AFTER the sale claim verified
   *  it (leaf-commitment + provenance are the buyer's responsibility; this
   *  only stores what was already checked). The record keeps the seller's
   *  provenance fields; an __imports__ audit record commits the acquisition
   *  (source shard:seq, leaf, settlement txid) so the lineage is anchored. */
  async importRecord(args: Parameters<CatalogOps['importRecord']>[0]): Promise<{ seq: number }> {
    return await this.catalogOps.importRecord(args)
  }

  /** Commit a shard manifest to __catalog__ — the domain tags another agent
   *  searches by ('who knows about soccer'). The claim is anchored and
   *  timestamped like any memory: false advertising stays attributable.
   *  Re-describing appends a newer manifest; the old one remains history. */
  async describeShard(
    shard: string,
    fields: { domains: string[]; desc?: string; offers?: ManifestOffer[] }
  ): Promise<{ manifest: ShardManifest; seq: number }> {
    return await this.catalogOps.describeShard(shard, fields)
  }
  /** PUT a shard pack to bstore when its bytes differ from the stored copy —
   *  the stored ref pins sha256, so a record change must re-store or remote
   *  importers keep resolving the stale pack. Also commits the fresh pack as
   *  a bfinger store member (kind-5 + re-issued manifest) so the identity
   *  record's refs resolve the current set with no /.well-known lookup.
   *  Best-effort: a store failure never fails the caller. Returns the
   *  (possibly pre-existing) ref. */
  async storePack(shard: string, pack: ShardPack, network?: string): Promise<import('@nektar/store').ObjectRef | undefined> {
    return await this.packOps.storePack(shard, pack, network)
  }
  /** attach the bfinger packs store post-construction (the automation
   *  runtime builds it once lanes exist); restores members persisted by a
   *  previous run and binds state persistence. */
  attachPacksStore(store: import('@nektar/store').PacksStore): void {
    this.packOps.attachPacksStore(store)
  }
  /** the configured bstore/S3 object store (undefined = feature off) */
  get objectStore(): import('@nektar/store').S3ObjectStore | undefined {
    return this.opts.objectStore
  }
  get feeRateSatPerKb(): number {
    return this.opts.feeRateSatPerKb ?? 1
  }
  /** pack-store refs this instance PUT to bstore — the `.well-known` doc
   *  names them so importers can fetch through the store instead of HTTP. */
  packStoreRef(shard: string): import('@nektar/store').ObjectRef | undefined {
    return this.packOps.packStoreRef(shard)
  }
  /** fetch + verify a pack through the object store — every byte checks
   *  against the ref's digest or the CHIRP closure before importShardPack's
   *  own re-verification. Resolution order: a chirp:// ref resolves through
   *  the signed store first, then unsigned against the advertised endpoint
   *  (no credentials needed on a public-read gateway); the keyed object is
   *  the compatibility fallback, signed then unsigned. */
  async fetchStoredPack(ref: { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }): Promise<ShardPack> {
    return await this.packOps.fetchStoredPack(ref)
  }
  /** the committed hash + anchor of a fragment — for redacted (private) cites
   *  that carry the proof value without naming the shard. */
  async fragmentRef(shard: string, seq: number): Promise<{ hdp: string; anchorTxid?: string } | undefined> {
    const rec = await this.store.bySeq(shard, seq)
    return rec ? { hdp: Buffer.from(rec.hdp).toString('hex'), anchorTxid: rec.anchorTxid } : undefined
  }
  /** OPS-002 metrics (Prometheus text via metrics.render()) */
  readonly metrics = (() => {
    const m = new Metrics()
    m.gauge(
      'nektar_unconfirmed_anchors',
      () => [...this.tails.values()].flat().filter((l) => !this.anchorTxs.get(l.txid)?.merklePath).length,
      'anchors without a BUMP yet'
    )
    m.gauge(
      'nektar_anchor_lag_ms',
      () => {
        const cov = this.covered()
        const p = this.promises.filter((x) => !cov(x))
        return p.length ? Date.now() - Math.min(...p.map((x) => x.issuedAt)) : 0
      },
      'age of the oldest promise not yet checkpointed'
    )
    // promiseViolations is async (the log pubkey may need a wallet round
    // trip); the gauge refreshes on scrape and reports the last computed
    // value. No eager refresh — field initializers run before this.opts.
    let pvCount = 0
    const pvRefresh = () => {
      void this.promiseViolations(this.opts.maxMergeDelayMs ?? 600_000).then(
        (v) => {
          pvCount = v.length
        },
        () => {}
      )
    }
    m.gauge(
      'nektar_promise_violations',
      () => {
        pvRefresh()
        return pvCount
      },
      'promises past the max merge delay without a checkpoint'
    )
    return m
  })()
  private covered() {
    return (p: { shard: string; seq: number }) => (this.cps.get(p.shard) ?? []).some((c) => c.size > p.seq)
  }
  /** LOG-005: own promises not covered by a valid checkpoint within the delay */
  async promiseViolations(maxMergeDelayMs: number, now = Date.now()) {
    const mon = new PromiseMonitor(await this.logPublicKey(), (s) => this.cps.get(s)?.[0]?.logId)
    for (const p of this.promises) mon.observePromise(p)
    for (const list of this.cps.values()) for (const c of list) mon.observeCheckpoint(c)
    return mon.violations(now, maxMergeDelayMs)
  }
  /** LOG-007: signed checkpoints issued for the shard, oldest first */
  checkpoints(shard: string): Checkpoint[] {
    return [...(this.cps.get(shard) ?? [])]
  }

  /** LOG-006: anchor every shard whose close policy fires now (interval /
   *  bytes / cost thresholds also fire without a new write — call periodically) */
  async flushDue(): Promise<Array<{ shard: string; txid: string; n: number }>> {
    const out: Array<{ shard: string; txid: string; n: number }> = []
    for (const s of this.touched) {
      if (await this.log.closeReason(s)) {
        const a = await this.anchorShard(s)
        if (a) out.push({ shard: s, ...a })
      }
    }
    // §12 SLA watchdog: if no anchor renewed the bond for half an epoch,
    // commit the current root again — a lapsed deadline makes the bond
    // seizable by anyone, so the operator must refresh before it passes.
    {
      const renewed = await this.bonds.renewIfDue()
      if (renewed === 'committed') this.metrics.inc('nektar_bond_renewals_total', {}, 1, 'bond deadline renewals committed by the SLA watchdog')
      else if (renewed === 'failed') this.metrics.inc('nektar_bond_commit_failures_total', {}, 1, 'bond commit broadcasts that failed')
    }
    return out
  }

  /** CHN-006: pending (not batched/anchored or not mined), mined (k < depth), final */
  async anchorStatus(shard: string, seq: number): Promise<{ state: 'pending' | 'mined' | 'final'; confirmations: number; depth: number }> {
    const depth = this.opts.finalityDepth ?? 1
    const rec = await this.store.bySeq(shard, seq)
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
  issueChallenge(ttlMs = 300_000): string {
    return this.identityOps.issueChallenge(ttlMs)
  }
  /** caller identity from VERIFIED inputs only: the binding is for this
   *  pubkey, the nonce was issued here and is fresh, the answer is signed.
   *  id = hash160(pubkey). Anything else → null (no principal → DENY). */
  verifiedCaller(identity?: CallerIdentity): { pubkey: string; id: string } | null {
    return this.identityOps.verifiedCaller(identity)
  }
  /** ZK clearance gate: issuer ∈ instance-trusted authorities, credential
   *  bound to the verified caller, transcript scoped to THIS read and unused. */
  verifyClearance(caller: { pubkey: string } | null, zk: ZkClearance | undefined, scope: string): { ok: boolean; reason?: string } {
    return this.identityOps.verifyClearance(caller, zk, scope)
  }
  /** Cedar principal: verified id; level = the ZK-PROVEN bound, else 0. */
  private cedarPrincipal(caller: { id: string } | null, zkOk: boolean, requiredLevel?: bigint): { type: 'Agent' | 'User'; id: string; level: number } | null {
    return this.identityOps.cedarPrincipal(caller, zkOk, requiredLevel)
  }

  /** authorize an action for the MCP `authorize` tool with the same verified
   *  inputs recall uses (never caller-asserted ids or levels). */
  async authorizeAction(
    action: import('@nektar/policy').CedarAction,
    args: { identity?: CallerIdentity; zk?: ZkClearance; resource?: { id?: string; attrs?: { level?: number; shard?: string; class?: string } } }
  ) {
    return await this.identityOps.authorizeAction(action, args)
  }

  /** broadcaster-callback path: a MINED push names only the txid — resolve the
   *  shard and confirm; no-op for txids we didn't anchor. Never a poll, always push. */
  confirmByTxid(txid: string): boolean {
    const shard = this.txidShard.get(txid)
    if (!shard) return false
    this.confirmAnchor(shard, txid)
    return true
  }

  /** Mark anchor txs mined (drive from broadcaster status events). */
  confirmAnchor(shard: string, txid: string): void {
    this.anchorOps.confirmAnchor(shard, txid)
  }

  // ── persisted anchor state (restart without forking the shard) ───────────
  private persistState(): void {
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
        headTxid: head.tx.id('hex'),
        confirmed: !!head.tx.merklePath,
        tails: this.tails.get(shard) ?? [],
        checkpoints: this.cps.get(shard) ?? [],
        certs: [...ac.rotationCerts],
        anchors
      }
    }
    const bond = this.bonds.snapshot()
    st.save({
      version: 1,
      shards,
      promises: this.promises,
      flags: Object.fromEntries(this.flags),
      importedBundles: Object.fromEntries(this.importedBundles),
      bond,
      packStores: Object.fromEntries(this.packStores),
      proofStores: Object.fromEntries(this.proofStore.stores),
      packsStore: this.opts.packsStore?.snapshot() ?? this.savedPacksStore
    })
  }
  private restoreState(): void {
    const s = this.opts.stateStore?.load()
    if (!s) return
    for (const [shard, p] of Object.entries(s.shards)) {
      for (const [txid, hex] of Object.entries(p.anchors)) {
        const tx = Transaction.fromAtomicBEEF(Array.from(Buffer.from(hex, 'hex')))
        if (tx.id('hex') !== txid) throw new Error(`restore: anchor ${txid} BEEF mismatch`)
        this.anchorTxs.set(txid, tx)
        this.txidShard.set(txid, shard)
      }
      const headTx = this.anchorTxs.get(p.headTxid)
      if (!headTx) throw new Error(`restore: head ${p.headTxid} of shard ${shard} missing`)
      this.anchorChain(shard).resume(headTx, { confirmed: p.confirmed, certs: p.certs })
      this.tails.set(shard, p.tails)
      this.cps.set(shard, p.checkpoints)
      this.touched.add(shard)
    }
    this.promises.push(...s.promises)
    // state files written before the transaction graph kept parents here
    for (const hex of Object.values(s.txHex ?? {})) {
      try {
        this.txs.put(Transaction.fromHex(hex))
      } catch {
        /* unreadable entry */
      }
    }
    for (const [k, b] of Object.entries(s.importedBundles ?? {})) this.importedBundles.set(k, b)
    for (const [shard, f] of Object.entries(s.flags ?? {})) this.flags.set(shard, f)
    for (const [shard, r] of Object.entries(s.packStores ?? {})) this.packStores.set(shard, r)
    for (const [commit, r] of Object.entries(s.proofStores ?? {})) this.proofStore.stores.set(commit, r)
    this.savedPacksStore = s.packsStore
    if (s.bond) this.bonds.restore(s.bond)
  }

  /**
   * recall: fetch fragments for a shard (optionally only anchored), decrypt
   * when an agentSeed is held, and attach a receipt.
   */
  async recall(
    shard: string,
    opts?: {
      seq?: number
      question?: string
      /** ZKAccess gate: a clearance proof covering `requiredLevel` is checked
       * before any level-gated ciphertext is decrypted. Keys alone still enforce
       * confidentiality; the proof additionally gates the READ PATH. */
      zk?: ZkClearance
      /** Verified caller identity (challenge-response against a nonce issued by
       * THIS instance via issueChallenge): the Cedar principal id and the
       * clearance holder — never a self-asserted claim. */
      identity?: CallerIdentity
      /** CHN-006: withhold records whose anchor has fewer than finalityDepth confirmations */
      requireFinal?: boolean
      /** §16 delegation map: sub-decisions the caller asserts were made by other
       * agents (each = makeDelegation(key, decision, answer)). Bound into the
       * signed receipt so the map is anchored, not post-hoc. */
      delegations?: Array<{ delegator: string; decision: string; signature: string }>
      /** per-call invocation params merged into receipt.model.params (the
       * caller knows temperature/seed; the operator owns id + weightsHash) */
      modelParams?: Record<string, unknown>
    }
  ): Promise<{ items: Array<{ seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string }>; receipt: AnswerReceipt }> {
    const single = opts?.seq !== undefined ? await this.store.bySeq(shard, opts.seq) : undefined
    const items = opts?.seq !== undefined ? (single ? [single] : []) : await this.store.all(shard)
    const caller = this.verifiedCaller(opts?.identity)
    let zkState: AnswerReceipt['zkaccess'] = 'not-required'
    let zkOk = false
    let zkReason: string | undefined
    if (opts?.zk) {
      // a clearance transcript is scoped to one fragment (seq given) or the shard
      const v = this.verifyClearance(caller, opts.zk, opts.seq !== undefined ? `${shard}/${opts.seq}` : shard)
      zkOk = v.ok
      zkReason = v.reason
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
            const d = authorize(policy.policyText, principal, 'read', {
              type: 'Fragment',
              id: `${shard}/${r.seq}`,
              attrs: { level: r.level ?? 0, shard, class: 'fragment' }
            })
            if (!d.allow) {
              cedarState = 'deny'
              break
            }
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
              ...items.map((r) => ({ pred: 'fragment', args: [`${shard}/${r.seq}`] }))
            ]
          : []
        const { facts: derived, conflicts } = dag.evaluate(facts)
        dagDerivationHash = new Datalog(dag.rules).derivHash(derived)
        if (conflicts.length > 0) {
          dagState = `dag-deny:policy conflict on ${conflicts[0].claim}`
          for (const r of items) dagPermit.set(r.seq, false)
        } else {
          // per-item: each fragment needs its own permit; non-permitted items
          // are withheld (consistent with Cedar per-item denial)
          let denied = 0
          for (const r of items) {
            const ok = caller != null && (cedarState === 'allow' || cedarState === 'not-required') && derived.has(`permit(${caller.id},read,${shard}/${r.seq})`)
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
    const texts: Array<{ seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string }> = []
    // S6.1: any item whose SPV verification fails flips the whole receipt to
    // 'unverifiable' — anchor presence alone never earns 'verified'
    let verifyFailed = false
    for (const r of items) {
      const ref = { shard, seq: r.seq, hdp: toHex(r.hdp), anchorTxid: r.anchorTxid }
      out.push(ref)
      const item: { seq: number; state: string; text?: string; unverifiable?: boolean; reason?: string; zk?: string } = { seq: r.seq, state: r.state }
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
        item.reason = zkOk
          ? `zkaccess: proven level ${provenLevel} < ${level}`
          : `zkaccess ${opts?.zk ? 'rejected' : 'required'}${zkReason ? `: ${zkReason}` : ''}`
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
        } else if (this.canEncrypt()) {
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
              for (const fragmentKey of await this.fragmentKeyCandidates(this.ctx(shard, r.seq, l))) {
                let pt: Uint8Array
                try {
                  pt = decryptFragment(fragmentKey, ef)
                } catch {
                  continue
                }
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
              }
            }
            if (item.text === undefined && item.reason === undefined) {
              item.unverifiable = true
              item.reason = 'no held level key decrypts'
            }
          } catch {
            item.unverifiable = true
            item.reason = 'decrypt failed'
          }
        } else {
          item.unverifiable = true
          item.reason = 'no encryption custody held'
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
      if (item.unverifiable)
        this.metrics.inc(
          'nektar_rejected_reads_total',
          { reason: (item.reason ?? 'unknown').split(/[:\d]/)[0].trim() },
          1,
          'records withheld from a read, by reason'
        )
      texts.push(item)
    }
    const answer = texts.map((i) => i.text ?? '[unverifiable]').join('\n')
    const verified = out.length === 0 || (out.every((r) => r.anchorTxid) && !verifyFailed)
    const unsigned: Omit<AnswerReceipt, 'signature' | 'signerPub'> = {
      answer,
      fragmentRefs: out,
      checkResult: verified ? 'verified' : 'unverifiable',
      truthNote:
        'inclusion and anchor-chain integrity verified; the TRUTH of the recorded statements is NOT established — a lie, once written, is preserved and marked',
      modelId: this.opts.model?.id ?? this.opts.modelId ?? 'unknown',
      model:
        this.opts.model || opts?.modelParams
          ? {
              id: this.opts.model?.id ?? this.opts.modelId ?? 'unknown',
              params: { ...this.opts.model?.params, ...opts?.modelParams },
              weightsHash: this.opts.model?.weightsHash
            }
          : undefined,
      zkaccess: zkState,
      zk: opts?.zk
        ? idbind.encodePresentation({
            commitment: opts.zk.commitment,
            proof: opts.zk.proof,
            transcript: opts.zk.transcript,
            requiredLevel: opts.zk.requiredLevel
          })
        : undefined,
      cedar: cedarState,
      dag: dagState && dagPolicyRoot ? { policyRoot: dagPolicyRoot, derivationHash: dagDerivationHash, state: dagState } : undefined,
      delegations: opts?.delegations,
      timestamp: Date.now()
    }
    // §18 terminal commitment: σ = Sign_logKey(H(answer‖trace‖τ))
    const receipt: AnswerReceipt = {
      ...unsigned,
      signerPub: await this.logPublicKey(),
      signature: (await toSigner(this.logKey).sign([...receiptDigest(unsigned)])).toDER('hex') as string
    }
    return { items: texts, receipt }
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

  // ── shard packs (cold-start): export public records + their proof bundles —
  //  import re-verifies everything against the live chain, zero trust. ──────

  /** Export a PUBLIC shard as a portable pack: each record's plaintext +
   *  provenance + the ProofBundle prove() produces for anchored ones.
   *  Private shards and level>0 records refuse — a pack must never carry
   *  gated content. */
  async exportShardPack(shard: string): Promise<ShardPack> {
    return await this.packWire.exportShardPack(shard)
  }

  /** Import a shard pack: records re-land via remember() (byte-identical prov
   *  keeps leaves matching the source bundles), every bundle re-verifies via
   *  verifyFragment against the live chain, and the import event itself is
   *  recorded on __imports__ — the lineage is anchored, not asserted. The
   *  target shard must be empty (no mixing imported + native seqs). */
  async importShardPack(pack: unknown): Promise<{ imported: number; anchored: number; unverifiable: string[]; skipped: number }> {
    return await this.packWire.importShardPack(pack)
  }

  /** doctor: one-call health surface — funding, shard/record/anchor counts,
   *  unproven anchors, payments, prover availability. The tool layer may
   *  replace `funding` with the richer ops.wallet() view when present. */
  async doctor(): Promise<{
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
    bond: { n: number; lastRoot: string; headTxid: string; sats: number; releaseHeight: string; deadline: string; seized: boolean } | null
  }> {
    const shards = this.shards()
    let records = 0,
      anchoredRecords = 0
    for (const s of shards)
      for (const r of await this.store.all(s)) {
        records++
        if (r.anchorTxid !== undefined) anchoredRecords++
      }
    const proverBin = nektarEnv('NEKTAR_STARK_PROVER')
    const prover = proverBin
      ? existsSync(proverBin)
        ? 'native binary'
        : 'NEKTAR_STARK_PROVER set but file missing'
      : await new Promise<string>((res) => {
          execFile('docker', ['image', 'inspect', nektarEnv('NEKTAR_STARK_IMAGE') ?? 'nektar-stark-build'], { timeout: 5_000 }, (e) =>
            res(e ? 'absent (set NEKTAR_STARK_PROVER or build nektar-stark-build)' : 'docker image')
          )
        })
    return {
      network: this.opts.network ?? 'main',
      funding: await this.fundingStatus(),
      shards: shards.length,
      records,
      anchoredRecords,
      unprovenAnchors: this.unprovenAnchors().length,
      importedBundles: this.importedBundles.size,
      publicPacks: (await this.publicShards()).length,
      payments: /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAYMENTS') ?? '') ? 'on' : 'off',
      prover,
      bond: (() => {
        const b = this.bondStatus()
        return b ? { ...b, releaseHeight: b.releaseHeight.toString(), deadline: b.deadline.toString() } : null
      })()
    }
  }

  /** doctorRepair: the fix half of `doctor{fix:true}` — applies only repairs
   *  that cannot lose state. Anything needing a spend or a judgment call is
   *  reported as `refused` with a reason, never attempted silently. */
  async doctorRepair(): Promise<{ fixed: string[]; refused: string[] }> {
    const fixed: string[] = [],
      refused: string[] = []
    const dir = this.opts.dataDir
    if (!dir) {
      refused.push('no dataDir on this instance — nothing to repair')
      return { fixed, refused }
    }
    const { existsSync, readFileSync, rmSync, writeFileSync, mkdirSync } = await import('node:fs')
    if (!existsSync(dir)) {
      mkdirSync(dir, { recursive: true })
      fixed.push(`recreated missing data dir ${dir}`)
    }
    // instance.lock — refuse only when a *live* heartbeat owns it
    const lock = (await import('node:path')).join(dir, 'instance.lock')
    if (existsSync(lock)) {
      let l: { owner?: string; beat?: number } | null = null
      try {
        l = JSON.parse(readFileSync(lock, 'utf8'))
      } catch {
        l = null
      }
      const live = l?.beat !== undefined && Date.now() - l.beat < 20_000
      if (live) refused.push(`instance.lock held by live instance ${l?.owner} — stop it first, do not delete`)
      else {
        rmSync(lock)
        fixed.push(l === null ? 'removed corrupt instance.lock' : `removed stale instance.lock (beat ${Math.round((Date.now() - (l?.beat ?? 0)) / 1000)}s old)`)
      }
    }
    // data-dir VERSION marker — establishes the migration baseline (v1)
    const ver = (await import('node:path')).join(dir, 'VERSION')
    if (!existsSync(ver)) {
      writeFileSync(ver, '1')
      fixed.push('wrote VERSION=1 (migration baseline)')
    }
    // unproven anchors need a spend — report, never fix silently
    const unproven = this.unprovenAnchors().length
    if (unproven > 0) refused.push(`${unproven} unproven anchor(s) — re-anchoring spends sats; handle deliberately`)
    return { fixed, refused }
  }
}
