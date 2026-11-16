// testworld.ts — the two test tiers behind one surface. Tests ask for
// `testWorld`/`testChain`/`testFunder`/`dryFunder` and get the in-memory
// world by default; NEKTAR_TEST_TIER=stack selects the real regtest stack
// (testkit.ts). Stack-only tests still gate on `skipUnlessStack`, which
// skips early on the memory tier without loading .env or probing the node.
import { Transaction, PrivateKey, MerklePath } from '@bsv/sdk'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, Funder, FundingOut, TxStatusEvent } from './interfaces.js'
import type { ChainEvents } from './events.js'
import { stackChain, stackWorld, stackTreasury, StackFunder, testTier, type StackTreasury } from './testkit.js'
import { memoryTreasury, MemoryTreasury, MemoryFunder, MemoryChain } from './memchain.js'

export type TestTier = 'memory' | 'stack'
export { testTier }

/** what tests use from a chain — StackChain and MemoryChain both satisfy it */
export interface TestChain extends ChainTracker, Broadcaster {
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** sync view of txs this chain broadcast — merklePath attached on proof
   *  delivery. NektarMemory.txState calls this WITHOUT await. */
  getTx: (txid: string) => Transaction | undefined
  isRejected: (txid: string) => boolean
  onStatus: (cb: (ev: TxStatusEvent) => void) => void
  /** the shared status substrate — runtimes hang confirmer/ledger watches on
   *  it instead of running private proof loops */
  readonly chainEvents: ChainEvents
  /** release the status watch — teardown calls this so poll loops stop */
  stopWatching: () => void
  waitProof: (txid: string, timeoutMs?: number) => Promise<Transaction | undefined>
  currentHeight: () => Promise<number>
  tipHeight: () => Promise<number>
  isValidRootForHeight: (root: string, height: number) => Promise<boolean>
  headerAtHeight: (height: number) => Promise<BlockHeader | undefined>
  /** node-side lookup for txids this chain did not itself broadcast */
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  merklePathOf: (txid: string) => Promise<MerklePath | undefined>
  utxoStatus: (txid: string, vout: number) => Promise<'unspent' | 'spent' | 'unknown'>
  pendingBroadcasts: () => Promise<number>
  mine: (n?: number) => Promise<void>
}

/** what tests use from a funder — StackFunder and MemoryFunder both satisfy it */
export interface TestFunder extends Funder {
  keyPair: PrivateKey
  take: (satoshis: number) => Promise<FundingOut>
  markSpent: (txid: string, vout: number) => void
  addOwned: (tx: Transaction, vout: number) => void
  address: () => string
  /** the broadcast funding parents (mined on the memory tier, mempool on stack) */
  readonly parents: Transaction[]
}

type TestHandle = { skip: (m?: string) => void; after?: (fn: () => void) => void }

/** tests that shadow the shared chain's broadcast with their own property
 *  would route later tests' broadcasts through it — teardown removes it */
const unshadow = (chain: TestChain): void => {
  delete (chain as Partial<TestChain>).broadcast
}

/** a bare chain handle — the treasury's shared chain on both tiers */
export async function testChain(t: TestHandle): Promise<TestChain | undefined> {
  if (testTier() === 'stack') return await stackChain(t)
  const chain = memoryTreasury().chain
  t.after?.(() => {
    chain.stopWatching()
    unshadow(chain)
  })
  return chain
}

/** `{ chain, funder }` — a fresh funded identity key on the shared chain.
 *  On the memory tier there is no skip: the world always exists. */
export async function testWorld(
  t: TestHandle,
  outputs = 30,
  sats = 10_000
): Promise<{ chain: TestChain; funder: TestFunder; treasury: StackTreasury | MemoryTreasury } | undefined> {
  if (testTier() === 'stack') return await stackWorld(t, outputs, sats)
  const treasury = memoryTreasury()
  const chain = treasury.chain
  t.after?.(() => {
    chain.stopWatching()
    unshadow(chain)
  })
  const funder = await MemoryFunder.fund(treasury, outputs, sats)
  return { chain, funder, treasury }
}

/** `StackFunder.fund(await stackTreasury(), …)` at call sites — one funded
 *  identity on the shared treasury of the active tier. The stack-tier branch
 *  throws when the stack is unconfigured, like today — callers gate on
 *  `testChain`/`testWorld`/`skipUnlessStack` first, as they do now. */
export async function testFunder(outputs: number, sats = 10_000): Promise<TestFunder> {
  if (testTier() === 'stack') return await StackFunder.fund(await stackTreasury(), outputs, sats)
  return await MemoryFunder.fund(memoryTreasury(), outputs, sats)
}

/** `StackFunder.dry()` at call sites — a funder with no utxos; take() throws */
export function dryFunder(key: PrivateKey = PrivateKey.fromRandom()): TestFunder {
  return testTier() === 'stack' ? StackFunder.dry(key) : MemoryFunder.dry(key)
}

export { MemoryChain, MemoryTreasury, MemoryFunder, memoryTreasury }
