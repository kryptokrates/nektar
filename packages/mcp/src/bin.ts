#!/usr/bin/env node
// mamem MCP server (stdio). Offline demo mode: a MockChain with Teranode-
// faithful script evaluation that mines a block after every accepted tx, so
// anchors are immediately provable. Live regtest wiring lives in
// packages/e2e/src/e2e-regtest.ts (funding/treasury needs the running stack).
import { MockChain, MockFunder } from '@mamem/chain'
import type { Transaction } from '@bsv/sdk'
import { MamemMemory } from './memory.js'
import { serveStdio } from './server.js'

const chain = new MockChain(); chain.verifyScripts = true
const funder = new MockFunder(); funder.topup(Number(process.env.MAMEM_DEMO_UTXOS ?? 200))
for (const t of funder.parents) chain.registerParent(t)
const broadcast = chain.broadcast.bind(chain)
chain.broadcast = async (tx: Transaction) => {
  const r = await broadcast(tx)
  if (r.status === 'success') chain.mine(1)
  return r
}
const mem = new MamemMemory(chain, funder, funder.keyPair, {
  batchPolicy: { maxLeaves: Number(process.env.MAMEM_BATCH ?? 4) },
  modelId: process.env.MAMEM_MODEL_ID ?? 'unspecified'
})
serveStdio(mem)
