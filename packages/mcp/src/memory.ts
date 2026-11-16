// NektarMemory — the product service the MCP server wraps.
// Wires log (append/batch) → anchor (head UTXO per shard) → broadcast
// → verify (proof bundles) → crypto (BRC-42/AES-GCM), all behind the chain
// interfaces so it runs identically on regtest and live networks.
//
// The class surface lives in @nektar/memory as a layer chain — each layer
// extends the last so `this.*` resolves exactly as the single file did:
//   base (state + ops slots) → keys → tx → anchor → custody → packs → zk
//   → records (write path) → read (recall/doctor) → NektarMemory (ctor wiring)
import { join } from 'node:path'
import { BudgetAuthority } from '@nektar/automation'
import { PrivateKey } from '@bsv/sdk'
import { toSigner, nektarEnv } from '@nektar/core'
import { BatchLog, MemoryShardStore, type ShardStore } from '@nektar/log'
import { Metrics } from './metrics.js'
import { BondOps } from './bonds.js'
import { RevocationOps } from './revocations.js'
import { CustodyOps } from './custody.js'
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
import { ZkProveOps } from './zkprove.js'
import { fundsOf } from '@nektar/chain'
import type { Broadcaster, ChainTracker, FunderLike } from '@nektar/chain'
import { MemRead, verifyAnswerReceipt, verifyReceiptZk } from '@nektar/memory'
import { IngestError } from '@nektar/memory'

// the domain vocabulary lives in @nektar/memory (re-exported via ./types.js)
// so the *ops modules share these types with NektarMemory without a
// memory↔ops type cycle. Re-exported here so existing './memory.js' imports
// keep working.
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
/** @public — the domain vocabulary's './memory.js' re-export surface */
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
export { CATALOG_SHARD, IngestError, verifyAnswerReceipt, verifyReceiptZk }

// (types moved to ./types.js — see the import/export block above)

export class NektarMemory extends MemRead {
  constructor(
    chain: ChainTracker & Broadcaster,
    funder: FunderLike | undefined,
    fundKey: PrivateKey | undefined,
    opts: NektarMemoryOptions = {},
    store: ShardStore = new MemoryShardStore()
  ) {
    super()
    this.chain = chain
    this.funder = funder
    this.fundKey = fundKey
    this.opts = opts
    this.store = store
    if (opts.shardPubs !== undefined) this._shardPubs = opts.shardPubs instanceof Map ? new Map(opts.shardPubs) : new Map(Object.entries(opts.shardPubs))
    // the one coin book — take/spent/died/adopt all route here; absent an
    // injected impl the funder is adapted (test tiers)
    if (opts.funds === undefined && this.funder === undefined) throw new Error('NektarMemory: no coin book — pass opts.funds or a funder')
    this.funds = opts.funds ?? fundsOf(this.funder!, opts.dealFloorSats ?? 0)
    this.keyOps = new KeyOps({
      opts: this.opts,
      fundKey: () => this.fundKey,
      shardPubs: this._shardPubs
    })
    this.logKey = opts.signingKey ?? (opts.wallet !== undefined ? this.walletLogSigner() : PrivateKey.fromRandom())
    this._logPub = this.logKey instanceof PrivateKey ? this.logKey.toPublicKey().toString() : undefined
    this.log = new BatchLog(store, this.logKey, opts.batchPolicy ?? { maxLeaves: 16 })
    this.metrics = (() => {
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
      // value.
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
    if (opts.dataDir) this.budget = new BudgetAuthority(opts.dataDir)
    this.bonds = new BondOps({
      chain: this.chain,
      txs: () => this.txs,
      fetchTx: (t) => this.fetchTx(t),
      takeFunding: (s) => this.takeFunding(s, 'publish'),
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
      funds: this.funds,
      fundSigner: () => this.fundSigner(),
      chain: this.chain,
      feeRateSatPerKb: () => this.opts.feeRateSatPerKb ?? 1,
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
      statusHint: this.opts.statusHint,
      height: this.opts.height,
      // the ledger's own verdict rows are first-party truth — wired lazily
      // because ledger is optional on tiers without durable state
      ledgerState: async (txid) => (await this.ledger?.walletTx({ txid }))?.state,
      proofPullMs: Number(nektarEnv('NEKTAR_PROOF_PULL_MS') ?? 120_000)
    })
    this.custodyOps = new CustodyOps({
      funds: this.funds,
      fundSigner: () => this.fundSigner(),
      broadcast: (tx) => this.chain.broadcast(tx),
      txs: (tx) => this.txs.put(tx),
      proofOf: (txid) => this.txs.get(txid)?.merklePath,
      facts: (txid) => this.txOps.txFacts(txid),
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
    // constructor-input hooks (chain port answers + durable ledger) — the
    // runtime injects them here; nothing is assigned post-construction
    this.txLookup = opts.txLookup
    this.proofLookup = opts.proofLookup
    this.watchTx = opts.watchTx
    this.ledger = opts.ledger
    this.restoreSecurityState()
    this.restoreState()
    // push confirmations: backends that emit status events (regtest stack)
    // deliver 'mined' proofs straight in — no confirmation polling
    this.chain.onStatus?.((ev) => {
      if (ev.status === 'mined' && ev.merklePath) this.deliverTxProof(ev.txid, ev.merklePath)
      if (ev.status === 'rejected' || ev.status === 'double-spent') this.deliverTxRejected(ev.txid)
    })
    // the abandon horizon is a real verdict (observer-computed terminal for
    // a watched tx that never proved) — feed it to the same durable path
    this.events.on('txAbandoned', ({ txid }: { txid: string }) => this.txOps.deliverTxAbandoned(txid))
  }
}
