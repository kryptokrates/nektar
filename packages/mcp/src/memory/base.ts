// MemBase — the shared state every memory layer sees: instance fields, the
// ops-module slots the NektarMemory constructor fills, and the public
// surfaces other layers delegate through. Layer members are `protected` so
// the chain (keys → tx → anchor → custody → packs → zk → records → read)
// resolves them through `this` exactly as the monolith did.
import { EventEmitter } from 'node:events'
import type { PrivateKey } from '@bsv/sdk'
import type { Transaction } from '@bsv/sdk'
import type { BatchLog, ShardStore, Checkpoint } from '@nektar/log'
import type { AnchorChain } from '@nektar/anchor'
import type { AutomatonChain } from '@nektar/automaton'
import type { ProofBundle, AnchorLink } from '@nektar/verify'
import type { Broadcaster, ChainTracker, FunderLike, Funds } from '@nektar/chain'
import { TxStore } from '@nektar/chain'
import type { SignerLike } from '@nektar/core'
import type { Metrics } from '../metrics.js'
import type { BondOps } from '../bonds.js'
import type { RevocationOps } from '../revocations.js'
import type { CustodyOps } from '../custody.js'
import type { TxOps } from '../txops.js'
import type { AnchorOps } from '../anchorops.js'
import type { IdentityOps } from '../identityops.js'
import type { KeyOps } from '../keyops.js'
import type { CryptoOps } from '../cryptoops.js'
import type { CatalogOps } from '../catalogops.js'
import type { PackOps } from '../packops.js'
import type { PackWire } from '../packwire.js'
import type { AuthorshipOps } from '../authorshipops.js'
import type { ZkQueryOps } from '../zkquery.js'
import type { AttestOps } from '../attestops.js'
import type { ProofStore } from '../proofstore.js'
import type { ZkProveOps } from '../zkprove.js'
import type { NektarMemoryOptions } from '../types.js'

/** write refused at ingest (COM-003) */
export class IngestError extends Error {}

export abstract class MemBase {
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

  // constructor inputs — assigned by NektarMemory's ctor before any op wiring
  protected chain!: ChainTracker & Broadcaster
  protected funder!: FunderLike | undefined
  protected fundKey!: PrivateKey | undefined
  protected opts!: NektarMemoryOptions
  protected store!: ShardStore

  protected log!: BatchLog
  protected logKey!: SignerLike
  protected _logPub?: string
  /** the one coin book — the Funds impl the runtime chose (or the
   *  funder-adapting fundsOf on test tiers) */
  public funds!: Funds
  public budget?: import('@nektar/automation').BudgetAuthority
  public automation?: import('@nektar/automation').Automation
  public ledger?: import('@nektar/automation').AutomationStore
  /** every transaction this instance built, broadcast or was handed, with
   *  its proof once one exists — parents for spends and BEEF for counterparties
   *  come from here, not from an indexer */
  get txs(): TxStore {
    return (this.opts.txStore ??= new TxStore())
  }
  /** network lookups for what the instance was never handed (a foreign bond,
   *  a transaction from before a database loss, a proof missed while down).
   *  Constructor inputs from the chain port; each is a single attempt and
   *  never sleeps. */
  txLookup?: (txid: string) => Promise<Transaction | undefined>
  proofLookup?: (txid: string) => Promise<import('@bsv/sdk').MerklePath | undefined>
  /** registers interest with the chain-event substrate so proof delivery
   *  and the abandon horizon cover a tx this instance did not broadcast
   *  (an externally-paid provision tx, say) */
  watchTx?: (txid: string) => void

  /** state-change events for live surfaces (dashboard SSE, watchers) —
   *  emitted once per successful mutating tool call by the server layer */
  readonly events = new EventEmitter()
  /** OPS-002 metrics (Prometheus text via metrics.render()) — the gauges are
   *  registered by the NektarMemory ctor (they read lazy instance state) */
  public metrics!: Metrics

  // ops-module slots — constructed in the NektarMemory ctor
  protected txOps!: TxOps
  protected anchorOps!: AnchorOps
  protected identityOps!: IdentityOps
  protected keyOps!: KeyOps
  protected cryptoOps!: CryptoOps
  protected catalogOps!: CatalogOps
  protected packOps!: PackOps
  protected packWire!: PackWire
  protected authorshipOps!: AuthorshipOps
  protected zkQueryOps!: ZkQueryOps
  protected attestOps!: AttestOps
  protected zkProveOps!: ZkProveOps
  protected custodyOps!: CustodyOps
  protected revocationOps!: RevocationOps
  protected bonds!: BondOps
  protected proofStore!: ProofStore

  // shared mutable state
  protected anchors = new Map<string, AnchorChain>()
  protected tails = new Map<string, AnchorLink[]>()
  protected anchorTxs = new Map<string, Transaction>()
  protected txidShard = new Map<string, string>()
  /** pre-resolved shard owner pubkeys — runtime fills this for wallet-mode
   *  shards so sync paths (restoreState/resume) never need a round trip. */
  protected _shardPubs = new Map<string, string>()
  /** bundles of pack-imported records ('shard:seq' → ProofBundle) — the only
   *  proof path for records anchored by a DIFFERENT instance's chain */
  protected importedBundles = new Map<string, ProofBundle>()
  protected cps = new Map<string, Checkpoint[]>()
  protected touched = new Set<string>()
  protected promises: Array<{ shard: string; seq: number; issuedAt: number }> = []
  /** owner-declared shard policy — `private` shards may only be cited via ZK
   *  proofs; the flag is monotone (public→private allowed, never back) and
   *  persisted with the rest of the state so it survives restarts. */
  protected flags = new Map<string, { private?: boolean }>()
  /** bstore refs of packs this instance PUT (describeShard) — persisted in
   *  state so /.well-known/nektar keeps naming them across restarts */
  protected packStores = new Map<string, import('@nektar/store').ObjectRef>()
  protected savedPacksStore: { members?: import('@nektar/store').PackMember[]; manifestSeq?: number } | undefined

  // ── persisted replay-protection state (challenges, transcripts, issued ZK queries, revocations)
  protected challenges = new Map<string, number>() // nonce → expiry (ms)
  protected zkSeen = new Set<string>() // transcript (queryId ‖ nonce) already used
  /** ZK queries issued by THIS verifier: queryId hex → {nonce, verifierId, expiry, consumed} (single use).
   *  nonce+verifierId let verifyCombined rebuild the StarkQuery for the Π binding.
   *  `consumed` entries are kept as tombstones — the re-verify ledger. */
  protected zkQueries = new Map<string, { nonce: string; verifierId: string; expiry: number; consumed?: boolean }>()

  // ── P14 full-mode automata (AUT-005): one Rúnar NektarAutomaton UTXO per id,
  // funded by the instance funder, owned by its own key.
  protected automata = new Map<string, AutomatonChain>()

  // ── GOAL-v6 S1a (§13 "on-chain if necessary"): cheap receipt commitments —
  // OP_FALSE OP_RETURN sha256(proof) in a fee-key-funded data tx. Not an
  // anchor: no shard state, no contract. Off-chain WASM verify stays
  // authoritative; the chain only timestamps the commitment.
  protected attestationTxs = new Map<string, Transaction>()
}
