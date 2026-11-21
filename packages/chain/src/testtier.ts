// testtier — the leaf of the test-world module trio: tier detection plus the
// TestChain/TestFunder contracts. No imports of testkit/memchain/testworld so
// those three may depend on this file without an import cycle.
import { nektarEnv } from '@nektar/core'
import type { MerklePath, PrivateKey, Transaction } from '@bsv/sdk'
import type { BlockHeader, BroadcastResult, Broadcaster, ChainTracker, Funder, Funds, TxStatusEvent } from './interfaces.js'
import type { TxFacts } from './txfacts.js'
import type { ChainEvents } from './events.js'

export type TestTier = 'memory' | 'stack' | 'testnet'

export function testTier(): TestTier {
  const v = nektarEnv('NEKTAR_TEST_TIER')
  if (v === 'stack' || v === 'testnet') return v
  return 'memory'
}

/** what tests may use from a chain — StackChain and MemoryChain both implement it */
export interface TestChain extends ChainTracker, Broadcaster {
  broadcast(tx: Transaction): Promise<BroadcastResult>
  getTx(txid: string): Transaction | undefined // sync; merklePath attached once mined
  isRejected(txid: string): boolean
  /** THE one status answer — bytes/proof/depth/verdict + outside hint */
  txFacts(txid: string): Promise<TxFacts>
  onStatus(cb: (ev: TxStatusEvent) => void): void
  readonly chainEvents: ChainEvents
  stopWatching(): void
  waitProof(txid: string, timeoutMs?: number): Promise<Transaction | undefined>
  currentHeight(): Promise<number>
  tipHeight(): Promise<number>
  isValidRootForHeight(root: string, height: number): Promise<boolean>
  headerAtHeight(height: number): Promise<BlockHeader | undefined>
  fetchTx(txid: string): Promise<Transaction | undefined>
  merklePathOf(txid: string): Promise<MerklePath | undefined>
  utxoStatus(txid: string, vout: number): Promise<'unspent' | 'spent' | 'unknown'>
  /** the txid that spent this outpoint — 'spent' names its spender */
  spenderOf?(txid: string, vout: number): Promise<string | undefined>
  pendingBroadcasts(): Promise<number>
  mine(n?: number): Promise<void>
}

export interface TestFunder extends Funder {
  keyPair: PrivateKey
  address(): string
  /** the single owner of this funder's coin state — purpose-aware takes and
   *  spent/died/adopt bookkeeping; `take`/`markSpent`/`addOwned` below are
   *  the harness shorthand, not the contract */
  readonly funds: Funds
  markSpent(txid: string, vout: number): void
  addOwned(tx: Transaction, vout: number): void
  readonly parents: Transaction[]
}

export type TestT = { skip: (m?: string) => void; after?: (fn: () => void) => void }

/** returns true (after t.skip with a reason) unless the tier is 'stack' —
 *  for tests that need the node itself (RPC, Docker hosts, spawned runtime) */
export function stackOnly(t: TestT, why: string): boolean {
  if (testTier() === 'stack') return false
  t.skip(`stack-only: ${why}`)
  return true
}
