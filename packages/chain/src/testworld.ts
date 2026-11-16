// testworld — the only thing tests import for a chain or a funder.
// NEKTAR_TEST_TIER picks the backend: 'memory' (default) is a process-local
// MemoryChain with real admission rules and real proofs; 'stack' delegates to
// the existing regtest testkit; 'testnet' is the paid tier — only suites that
// gate on it (market-testnet) may run.
// Tier detection and the TestChain/TestFunder contracts live in testtier.ts
// (a leaf module) and are re-exported here so tests keep a single import site.
import type { PrivateKey } from '@bsv/sdk'
import { MemoryChain, MemoryFunder } from './memchain.js'
import { testTier } from './testtier.js'
import type { TestChain, TestFunder, TestT } from './testtier.js'

export { testTier, stackOnly } from './testtier.js'
export type { TestTier, TestChain, TestFunder, TestT } from './testtier.js'

/** the chain inside the shared treasury is per-process — a test that shadows
 *  `broadcast` would otherwise route later tests' broadcasts through a
 *  destroyed store; restore on teardown via t.after (see testkit.ts) */
const unshadowBroadcast = (chain: { broadcast?: unknown }): void => {
  delete (chain as { broadcast?: unknown }).broadcast
}

let mem: MemoryChain | undefined
/** the one process-wide memory chain — testChain(t) and testFunder(n) must
 *  see the same world, exactly like the per-process stack treasury today */
function memoryChain(): MemoryChain {
  mem ??= new MemoryChain()
  return mem
}

export async function testWorld(t: TestT, outputs = 30, sats = 10_000): Promise<{ chain: TestChain; funder: TestFunder } | undefined> {
  const chain = await testChain(t)
  if (chain === undefined) return undefined
  const funder = await testFunder(outputs, sats)
  return { chain, funder }
}

export async function testChain(t: TestT): Promise<TestChain | undefined> {
  if (testTier() === 'testnet') {
    t.skip('testnet tier has no shared chain — run with NEKTAR_TEST_TIER=stack')
    return undefined
  }
  if (testTier() === 'stack') {
    const { stackChain } = await import('./testkit.js')
    return await stackChain(t)
  }
  const chain = memoryChain()
  t.after?.(() => {
    chain.stopWatching()
    unshadowBroadcast(chain)
  })
  return chain
}

/** replaces `StackFunder.fund(await stackTreasury(), n, s)` */
export async function testFunder(outputs: number, sats = 10_000): Promise<TestFunder> {
  if (testTier() === 'stack') {
    const { StackFunder, stackTreasury } = await import('./testkit.js')
    return await StackFunder.fund(await stackTreasury(), outputs, sats)
  }
  if (testTier() === 'testnet') throw new Error('testFunder: testnet tier has no shared funder')
  return await MemoryFunder.fund(memoryChain(), outputs, sats)
}

/** replaces `StackFunder.dry()` — a funder with no utxos; take() throws */
export function dryFunder(key?: PrivateKey): TestFunder {
  return MemoryFunder.dry(key)
}
