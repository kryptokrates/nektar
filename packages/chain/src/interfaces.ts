// Chain-facing interfaces — everything chain-related goes through these so the
// whole system is testable offline on the mock chain, and swappable to Teranode
// regtest (NEKTAR_NETWORK=regtest) without code changes.
import type { Transaction, MerklePath } from '@bsv/sdk'

export interface BlockHeader {
  height: number
  hash: string // display hex
  prevHash: string
  merkleRoot: string // display hex
  time: number
  bits: number
  nonce: number
}

export interface ChainTracker {
  currentHeight: () => Promise<number>
  isValidRootForHeight: (root: string, height: number, signal?: AbortSignal) => Promise<boolean>
  headerAtHeight: (height: number) => Promise<BlockHeader | undefined>
  /** subscribe to tip changes; reorg: blocks removed from tip */
  onReorg?: (cb: (removed: BlockHeader[], newTip: BlockHeader) => void) => void
  /** the real tip height (currentHeight may be offset, e.g. MockChain adds
   *  100 for the SDK coinbase-maturity rule) — used for confirmation counts */
  tipHeight?: () => Promise<number>
}

export interface BroadcastResult {
  status: 'success' | 'error'
  txid?: string
  description?: string
}

export type TxStatus = 'received' | 'unconfirmed' | 'mined' | 'rejected' | 'double-spent'

export interface TxStatusEvent {
  txid: string
  status: TxStatus
  blockHeight?: number
  merklePath?: MerklePath
}

export interface Broadcaster {
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** statuses pushed as the chain progresses (mock emits on mine) */
  onStatus?: (cb: (ev: TxStatusEvent) => void) => void
}

export interface FundingOut {
  tx: Transaction
  vout: number
  satoshis: number
}

/** Supplies funding inputs (P2PKH) for anchor txs. */
export interface Funder {
  /** returns a utxo of at least `satoshis`, or throws */
  take: (satoshis: number) => Promise<FundingOut>
  address: () => Promise<string> | string
}
