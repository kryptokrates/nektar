import type { ChainTracker, MerklePath, PrivateKey, Transaction, WalletInterface } from '@bsv/sdk'
import type { BroadcastResult, FundingOut, Ingested } from '@nektar/chain'
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
  txState(txid: string): Promise<'mined' | 'seen' | 'rejected' | 'unknown'>
  broadcastTx(tx: Transaction): Promise<BroadcastResult>
  operatorKey(): PrivateKey
  operatorWallet(): OperatorWallet
  fundingOuts(satoshis: number): Promise<FundingOut[]>
  fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }>
  addOwnedOut(tx: Transaction, vout: number): void
  markSpentOut(txid: string, vout: number): void
  chainHeight(): Promise<number>
  sdkTracker(): ChainTracker
  tipRef(): Promise<{ tip: string; tipHeight: number } | undefined>
  shards(): string[]
  attachPacksStore(store: PacksStore): void
  packStoreRef(shard: string): ObjectRef | undefined
  budget?: BudgetAuthority
  automation?: Automation
}
