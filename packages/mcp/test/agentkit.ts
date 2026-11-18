// agentkit.ts — the one test-agent builder (audit F9). Four suites carried
// near-identical mkagent copies plus six policy literals; the differences
// were knob values only. The testnet suite keeps its own live-custody
// builder — this is the memory-tier one.
import { join } from 'node:path'
import { testFunder, memoryPort, type TestChain } from '@nektar/chain'
import { openAutomation, ScriptedExecutor, DEFAULT_SCORE_WEIGHTS } from '@nektar/automation'
import type { Automation, AutomationPolicy, MarketOptions, MarketMessage } from '@nektar/automation'
import { NektarMemory } from '../src/memory.js'
import { callTool } from '../src/server.js'

export interface TestAgent {
  mem: NektarMemory
  rt: Automation
  pub: string
  policyHash: string
  name: string
}

export interface TestPolicyOpts {
  policyId?: string
  network?: string
  domains?: string[]
  collectMs?: number
  maxPreparationsPerDay?: number
  limits?: Partial<AutomationPolicy['limits']>
}

/** the baseline trading policy — tight caps so deal tests exercise the guard;
 *  every per-suite difference is an override, not another literal */
export const testPolicyFor = (pub: string, o: TestPolicyOpts = {}): AutomationPolicy => ({
  v: 1,
  policyId: o.policyId ?? 'demo',
  network: o.network ?? 'regtest',
  agentPub: pub,
  roles: ['buyer', 'seller'],
  notBefore: 0,
  notAfter: 4_000_000_000_000,
  domains: o.domains ?? ['libfoo'],
  limits: {
    perPurchaseSats: 500,
    dailyPurchaseSats: 1000,
    perTxFeeSats: 1200, // 500 escrow reserve + payload-sized maxFee headroom (same bound as market-testnet)
    dailyFeeSats: 2000,
    runTotalSats: 5000,
    maxLockedSats: 1000,
    maxUnconfirmedSats: 1000,
    maxUnconfirmedCount: 3,
    ...o.limits
  },
  scoring: { minimumScoreBps: 0, weights: { ...DEFAULT_SCORE_WEIGHTS }, collectMs: o.collectMs ?? 1000 },
  seller: { minPriceSats: 100, maxPriceSats: 1000, maxPreparationsPerDay: o.maxPreparationsPerDay ?? 5, sellableShards: ['guide'] },
  disclosure: { allowPublic: false },
  ai: { providers: ['scripted'], allowSensitiveEgress: false, maxOutputTokens: 200, timeoutMs: 5000 },
  sandbox: { mode: 'sandbox-only' }
})

export interface TestAgentOpts {
  hosts: string[]
  chain: TestChain
  dir: string
  /** virtual clock — tests drive it by hand; production uses Date.now() */
  now?: () => number
  logs?: string[]
  /** in-process market-message bus: callers hand the suite's bus() helper */
  msgs?: {
    for: (pub: string) => { send: (to: string, msg: MarketMessage) => Promise<void>; collect: () => Promise<Array<{ from: string; msg: MarketMessage }>> }
  }
  call?: (tool: string, args: any) => Promise<any>
  objectStore?: import('@nektar/store').S3ObjectStore
  /** lane refill floor — 1 keeps every leaf's parent tree proven through a
   *  deal (BEEF-path tests); unset = production refill-ahead */
  treeLowWater?: number
  /** watch-shim base + the public URL advertised in the bfinger record */
  watch?: string
  opts?: Partial<MarketOptions>
  /** funded outputs to mint — upkeep lanes + tree mints draw real stock */
  funderOutputs?: number
  /** sats per funded output — one big output is the dominant-UTXO fixture */
  funderSats?: number
  /** upkeep's medium-output reserve policy (NEKTAR_RESERVE_OUTS/_SATS) */
  reserveOuts?: number
  reserveOutSats?: number
  /** own directory card (NEKTAR_ANNOUNCE=1) */
  announce?: boolean
  publishFinger?: boolean
  policy?: TestPolicyOpts
}

export const testAgent = async (name: string, o: TestAgentOpts): Promise<TestAgent> => {
  const funder = await testFunder(o.funderOutputs ?? 60, o.funderSats)
  // the port's pull answers — every production assembly wires these from the
  // ChainPort; without proofLookup the memory tier's only proof path is the
  // events rotation, which freezes once the tip idles (tip-gated ticks)
  const port = memoryPort(o.chain)
  const mem = new NektarMemory(o.chain, funder, funder.keyPair, {
    batchPolicy: { maxLeaves: 1 },
    modelId: 'test-model-1',
    proofLookup: port.proofLookup,
    txLookup: port.txLookup,
    height: () => port.height(),
    watchTx: (txid) => o.chain.chainEvents.watch(txid),
    ...(o.objectStore !== undefined ? { objectStore: o.objectStore } : {})
  })
  return await wireAgent(name, mem, o)
}

const wireAgent = async (name: string, mem: NektarMemory, o: TestAgentOpts): Promise<TestAgent> => {
  const pub = mem.operatorKey().toPublicKey().toString()
  const rt = await openAutomation(mem, {
    network: 'regtest',
    dataDir: join(o.dir, name),
    marketHosts: o.hosts,
    intervalMs: 0,
    call: o.call ?? ((t, a) => callTool(mem, t, a)),
    ...(o.logs !== undefined ? { log: (l) => o.logs!.push(`${name}:${l}`) } : {}),
    ai: new ScriptedExecutor(),
    ...(o.now !== undefined ? { now: o.now } : {}),
    ...(o.msgs !== undefined ? { messages: o.msgs.for(pub) } : {}),
    ...(o.objectStore !== undefined ? { objectStore: o.objectStore } : {}),
    ...(o.publishFinger === true ? { publishFinger: true } : {}),
    ...(o.treeLowWater !== undefined ? { treeLowWater: o.treeLowWater } : {}),
    ...(o.reserveOuts !== undefined ? { reserveOuts: o.reserveOuts } : {}),
    ...(o.reserveOutSats !== undefined ? { reserveOutSats: o.reserveOutSats } : {}),
    ...(o.announce === true ? { announce: true } : {}),
    ...(o.watch !== undefined ? { watchHosts: [o.watch], watchPublic: o.watch } : {}),
    opts: { offerTtlBlocks: 10000, refundBlocks: 10000, ...o.opts }
  })
  mem.automation = rt // bin.ts does the same in production
  return { mem, rt, pub, policyHash: await rt.store.approvePolicy(testPolicyFor(pub, o.policy), 'test:operator'), name }
}

/** restart on the SAME memory + dataDir — the killed-agent respawn: sqlite
 *  state survives, op records rebuild from automation.sqlite */
export const respawnAgent = async (a: TestAgent, o: TestAgentOpts): Promise<TestAgent> => await wireAgent(a.name, a.mem, o)
