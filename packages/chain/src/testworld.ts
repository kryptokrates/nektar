// testworld — the only thing tests import for a chain or a funder.
// NEKTAR_TEST_TIER picks the backend: 'memory' (default) is a process-local
// MemoryChain with real admission rules and real proofs; 'stack' delegates to
// the existing regtest testkit; 'testnet' is the paid tier — only suites that
// gate on it (market-testnet) may run.
// Tier detection and the TestChain/TestFunder contracts live in testtier.ts
// (a leaf module) and are re-exported here so tests keep a single import site.
// The stack backend loads lazily so the memory tier never touches testkit's
// .env or the node config.
import type { PrivateKey } from '@bsv/sdk'
import { MemoryFunder, memoryTreasury } from './memchain.js'
import type { MemoryTreasury } from './memchain.js'
import { testTier } from './testtier.js'
import type { TestChain, TestFunder, TestT } from './testtier.js'
import type { StackTreasury } from './testkit.js'

export { testTier, stackOnly } from './testtier.js'
export type { TestTier, TestChain, TestFunder, TestT } from './testtier.js'

/** the chain inside the shared treasury is per-process — a test that shadows
 *  `broadcast` would otherwise route later tests' broadcasts through a
 *  destroyed store; restore on teardown via t.after (see testkit.ts) */
const unshadowBroadcast = (chain: { broadcast?: unknown }): void => {
  delete (chain as { broadcast?: unknown }).broadcast
}

/** `{ chain, funder, treasury }` — a fresh funded identity key on the shared
 *  treasury of the active tier. On the memory tier there is no skip: the
 *  world always exists; the treasury is the process-local MemoryTreasury. */
export async function testWorld(
  t: TestT,
  outputs = 30,
  sats = 10_000
): Promise<{ chain: TestChain; funder: TestFunder; treasury: StackTreasury | MemoryTreasury } | undefined> {
  if (testTier() === 'testnet') {
    t.skip('testnet tier has no shared world — only gated suites may run')
    return undefined
  }
  if (testTier() === 'stack') {
    const { stackWorld } = await import('./testkit.js')
    return await stackWorld(t, outputs, sats)
  }
  const treasury = memoryTreasury()
  const chain = treasury.chain
  t.after?.(() => {
    chain.stopWatching()
    unshadowBroadcast(chain)
  })
  const funder = await MemoryFunder.fund(treasury, outputs, sats)
  return { chain, funder, treasury }
}

/** a bare chain handle — the shared chain of the active tier */
export async function testChain(t: TestT): Promise<TestChain | undefined> {
  if (testTier() === 'testnet') {
    t.skip('testnet tier has no shared chain — run with NEKTAR_TEST_TIER=stack')
    return undefined
  }
  if (testTier() === 'stack') {
    const { stackChain } = await import('./testkit.js')
    return await stackChain(t)
  }
  const chain = memoryTreasury().chain
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
  return await MemoryFunder.fund(memoryTreasury(), outputs, sats)
}

/** replaces `StackFunder.dry()` — a funder with no utxos; take() throws.
 *  A dry funder never touches the chain, so the memory-tier one serves every
 *  tier (keeps testkit off the import graph on the memory tier). */
export function dryFunder(key?: PrivateKey): TestFunder {
  return MemoryFunder.dry(key)
}
