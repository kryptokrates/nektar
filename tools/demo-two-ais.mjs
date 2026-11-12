#!/usr/bin/env node
// tools/demo-two-ais.mjs — "another AI reads your link": two nektar
// instances on one (mock) network. AI A commits knowledge on :8090,
// AI B pulls it over real HTTP via importShard — re-verifying every
// record against the shared chain before trusting a byte of it.
import { MockChain, MockFunder } from '../packages/chain/dist/src/index.js'
import { NektarMemory } from '../packages/mcp/dist/src/memory.js'
import { createHttpApi } from '../packages/mcp/dist/src/http.js'
import { callTool } from '../packages/mcp/dist/src/server.js'

const listen = (srv) => new Promise((r) => srv.once('listening', r))
const chain = new MockChain()
chain.verifyScripts = true
const funder = new MockFunder()
funder.topup(50)
for (const t of funder.parents) chain.registerParent(t)

const memA = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
const memB = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })

const srvA = createHttpApi(memA).listen(0, '127.0.0.1')
const srvB = createHttpApi(memB).listen(0, '127.0.0.1')
await Promise.all([listen(srvA), listen(srvB)])
const portA = srvA.address().port
const baseA = `http://127.0.0.1:${portA}`

console.log(`─ AI A (your instance) :${portA} ─────────────────────────────`)
await memA.remember('guide', 'nektar anchors encrypted, provenance-signed agent memory on BSV')
await memA.remember('guide', 'every grounded answer commits a receipt anyone can verify')
chain.mine(1)
memA.confirmAnchor('guide', memA.tailLink('guide').txid)
console.log('  remembered 2 facts on shard "guide" + anchored them on-chain')
console.log('  serving them publicly:')
const disco = await (await fetch(`${baseA}/.well-known/nektar`)).json()
console.log(' ', disco.publicShards.map((s) => `${s.shard} (${s.records} records)`).join(', '))

console.log(`\n─ AI B (a different instance) :${srvB.address().port} ────────`)
console.log('  sees the link → calls its own tool:')
console.log(`  importShard {from: "${baseA}", shard: "guide"}`)
const imp = await callTool(memB, 'importShard', { from: baseA, shard: 'guide' })
console.log(`  → fetched the pack, re-verified every record on the chain itself:`)
console.log(`    imported=${imp.imported}  anchored=${imp.anchored}  unverifiable=[${imp.unverifiable.join(', ')}]`)

console.log('\n─ AI B now "knows" it — and can prove it ────────────────────')
const rec = await memB.recall('guide')
for (const i of rec.items) console.log(`  knows: "${i.text}"  [state: ${i.state}]`)
const { result } = await memB.prove('guide', 0)
console.log(`  prove("guide",0) → ok=${result.ok} — the record verifies against the chain`)

console.log("\nThat's the whole handshake: the link WAS the access.")
srvA.close()
srvB.close()
