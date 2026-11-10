// One scenario agent as a real OS process: NektarMemory + openAutomation over
// the shared mock-chain hub, served through the production HTTP API.
import { MockFunder } from '../../../chain/dist/src/index.js'
import { NektarMemory } from '../../../mcp/dist/src/memory.js'
import { createHttpApi } from '../../../mcp/dist/src/http.js'
import { callTool } from '../../../mcp/dist/src/server.js'
import { openAutomation } from '../../../mcp/dist/src/automation/runtime.js'
import { RemoteMockChain } from './remote-mock-chain.mjs'
import { PrivateKey } from '@bsv/sdk'

const env = process.env
const chain = new RemoteMockChain(env.NEKTAR_CHAIN_HUB)
const funder = new MockFunder(env.NEKTAR_AGENT_WIF ? PrivateKey.fromWif(env.NEKTAR_AGENT_WIF) : undefined)
funder.topup(30)
for (const t of funder.parents) await chain.registerParent(t)
const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 }, modelId: 'scripted', network: 'mock', bond: env.NEKTAR_BOND_EPOCH ? { epochBlocks: Number(env.NEKTAR_BOND_EPOCH) } : undefined })
const au = await openAutomation(mem, {
  network: 'mock', dataDir: env.NEKTAR_DATA_DIR, rendezvousUrls: (env.NEKTAR_OVERLAY_URL ?? '').split(',').filter(Boolean),
  intervalMs: Number(env.NEKTAR_AUTOMATION_INTERVAL_MS ?? 1000),
  call: (tool, args) => callTool(mem, tool, args)
})
mem.automation = au
const srv = createHttpApi(mem).listen(Number(env.NEKTAR_HTTP_PORT), '127.0.0.1', () => console.log(`agent ${env.NEKTAR_AGENT_NAME} listening :${env.NEKTAR_HTTP_PORT}`))
const stop = async () => { await au.stop().catch(() => {}); srv.close(); process.exit(0) }
process.on('SIGTERM', () => { void stop() }); process.on('SIGINT', () => { void stop() })
