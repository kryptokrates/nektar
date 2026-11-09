#!/usr/bin/env node
// 10-agent ttn market swarm — sellers list priced offers, buyers publish wants,
// sellers respond with priced replies. Every artifact is a real signed net+tip-
// bound payload on Teranode testnet, landing on the rendezvous feed.
//   NEKTAR_OVERLAY_URL=<rv> node tools/swarm-ttn.mjs
import { openRuntime } from '../packages/mcp/dist/src/runtime.js'
import { callTool } from '../packages/mcp/dist/src/server.js'
import fs from 'fs'
const RV = process.env.NEKTAR_OVERLAY_URL ?? 'https://rv.nektar.run'
const BASE = '/tmp/nektar-swarm'
fs.mkdirSync(BASE, { recursive: true })
const agents = []
for (let i = 0; i < 10; i++) {
  const dir = `${BASE}/a${i}`
  fs.mkdirSync(dir, { recursive: true })
  const rt = await openRuntime({ network: 'ttn', dataDir: dir, main: {
    passphrase: 'demo-only',
    arcUrl: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech',
    chaintracksUrl: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech/chaintracks/v2'
  } })
  agents.push({ i, rt, pub: rt.mem.operatorKey().toPublicKey().encode('hex', true) })
}
const sellers = agents.slice(0, 5), buyers = agents.slice(5)
console.log(`swarm: ${agents.length} agents on ttn → ${RV}`)
console.log(`sellers: ${sellers.map(a => `a${a.i}`).join(',')} · buyers: ${buyers.map(a => `a${a.i}`).join(',')}`)

// sellers: remember a record + list it priced on the market
for (const s of sellers) {
  await callTool(s.rt.mem, 'remember', { shard: 'soccer', data: `a${s.i}: Metz ${s.i}-${s.i} Lyon` }).catch(e => ({ err: e.message })) // anchoring needs funding — off-chain listing still works
  const o = await callTool(s.rt.mem, 'publishOffer', { shard: 'soccer', seq: 0, priceSats: 100 + s.i * 50, domains: ['soccer', 'ligue-1'], rendezvousUrl: RV }).catch(e => ({ err: e.message }))
  console.log(`a${s.i}: record + offer ${o.err ?? `${o.offer?.priceSats} sats`}`)
}
// buyers: publish a want each
const wants = []
for (const b of buyers) {
  const w = await callTool(b.rt.mem, 'publishWant', { domains: ['soccer'], maxSats: 500, desc: `a${b.i} wants a soccer result`, rendezvousUrl: RV }).catch(e => ({ err: e.message }))
  wants.push({ buyer: b, pub: w.pub })
  console.log(`a${b.i}: want ${String(w.pub).slice(0, 12)}…${w.err ? ' ERR ' + w.err : ''}`)
}
// each seller answers one buyer's want — demand meets supply
for (let k = 0; k < sellers.length; k++) {
  const s = sellers[k], w = wants[k]
  if (!w.pub) continue
  const r = await callTool(s.rt.mem, 'respond', { wantPub: w.pub, shard: 'soccer', seq: 0, priceSats: 100 + s.i * 50, domains: ['soccer', 'ligue-1'], rendezvousUrl: RV }).catch(e => ({ err: e.message }))
  console.log(`a${s.i} → a${w.buyer.i}:`, r.err ?? `priced reply ${r.offer?.priceSats} sats`)
}
for (const a of agents) await a.rt.close()
console.log('swarm done — 10 agents, signed net+tip on ttn, indexed on the rv')
