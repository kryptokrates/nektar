import type { ChainTracker, MerklePath, PrivateKey, Transaction, WalletInterface } from '@bsv/sdk'
import type { BroadcastResult, FundingOut, Funds, FundsPurpose, Ingested } from '@nektar/chain'
import type { ObjectRef, PacksStore } from '@nektar/store'
import type { BudgetAuthority } from './budget.js'
import type { MarketMemory } from './market.js'
import type { Automation } from './types.js'

/** the wallet surface automation needs from the host — same structural Pick
 *  the memory engine exposes; declared here so this package names no
 *  application types */
export type OperatorWallet = Pick<WalletInterface, 'getPublicKey' | 'createSignature' | 'encrypt' | 'decrypt' | 'createHmac'>

/** the host surface the automation engine needs — the extracted package's
 *  only coupling to the application. NektarMemory satisfies this
 *  structurally; nothing in this package may import the app that hosts it.
 *
 *  It is deliberately a wide facade today (the actual pre-extraction
 *  contract, measured from real call sites) — narrowing it is follow-up
 *  work that no longer requires touching the host package */
export interface AutomationHost extends MarketMemory {
  fetchTx(txid: string): Promise<Transaction | undefined>
  rememberTx(tx: Transaction): void
  txWire(txid: string, maxBytes?: number): string | undefined
  readTxWire(hex: string): Promise<Ingested>
  onTxProof(cb: (txid: string, mp: MerklePath) => void): void
  onTxRejected(cb: (txid: string) => void): void
  /** the chain observer's abandon horizon fired for a txid — the terminal
   *  verdict for a mint that was resubmitted but never mined */
  onTxAbandoned(cb: (txid: string) => void): void
  /** register a txid this instance did not broadcast (e.g. a provision tx
   *  paid by another agent) with the chain-event substrate so proof delivery
   *  and the abandon horizon cover it */
  watchTx?(txid: string): void
  /** a header reorg orphaned everything proven at ≥ fromHeight — lanes strip
   *  stale paths from journaled evidence so the next read re-attaches proofs
   *  computed against the new fork */
  onReorg?(cb: (fromHeight: number) => void): void
  /** the chain tip advanced — wakes dueHeight-bound work (seize/refund
   *  deadlines) at block arrival instead of the poll floor; absent on
   *  hosts without a height feed, the floor covers it */
  onHeight?(cb: (height: number) => void): void
  /** the one transaction-facts answer — verdict (mined|pending|dead) with
   *  its source, held bytes/proof/depth, and the outside hint in a separate
   *  field that can never be mistaken for the verdict */
  txFacts(txid: string): Promise<import('@nektar/chain').TxFacts>
  broadcastTx(tx: Transaction): Promise<BroadcastResult>
  /** a tx our broadcast refused still aired — the counterparty re-broadcast
   *  the signed bytes it was handed. Retro-counts its spend in the guard's
   *  day ledger so the cap measures real outflow; absent on unguarded hosts. */
  recordAired?(tx: Transaction): void
  operatorKey(): PrivateKey
  operatorWallet(): OperatorWallet
  /** the custody-agnostic operator identity — client-custody hosts hold no
   *  private key and expose only this. Hosts without it are held-key by
   *  definition: `toSigner(operatorKey())` is the identical fallback */
  operatorSigner?(): import('@nektar/core').DigestSigner
  /** the one coin book — takes, spend/death/adoption bookkeeping route
   *  through it; callers never mark outpoints themselves */
  funds: Funds
  fundingOuts(satoshis: number, purpose?: FundsPurpose): Promise<FundingOut[]>
  fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }>
  chainHeight(): Promise<number>
  sdkTracker(): ChainTracker
  tipRef(): Promise<{ tip: string; tipHeight: number } | undefined>
  shards(): string[]
  attachPacksStore(store: PacksStore): void
  packStoreRef(shard: string): ObjectRef | undefined
  /** private shards must never be named in an announce — a URL-ref pack
   *  offer would leak the shard name even when the pack endpoint refuses */
  shardPrivate?(shard: string): boolean
  /** latest committed shard manifests — the domain tags an announce
   *  advertises per offered pack. Optional: announces carry no domain
   *  hints when the host can't project the catalog. */
  shardCatalog?(): Promise<Array<{ shard: string; domains?: string[] }>>
  budget?: BudgetAuthority
  automation?: Automation
}
