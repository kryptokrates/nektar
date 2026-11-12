// Scripted MCP session (official SDK client): spawns the nektar server over
// stdio and drives it the way an agent would. Health check for the Docker image.
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

const bin = fileURLToPath(new URL('../dist/src/bin.js', import.meta.url))
const client = new Client({ name: 'nektar-demo', version: '1.0.0' })
await client.connect(
  new StdioClientTransport({ command: process.execPath, args: [bin], env: { ...process.env, NEKTAR_MODEL_ID: 'scripted-demo' }, stderr: 'inherit' })
)
const call = async (name, args) => JSON.parse((await client.callTool({ name, arguments: args })).content[0].text)

const { tools } = await client.listTools()
console.log(`tools: ${tools.map((t) => t.name).join(', ')}`)
await call('remember', { shard: 'manual', data: 'drain port 4 torque 45Nm', source: 'manual-rev1' })
await call('remember', { shard: 'manual', data: 'pump P2 inspected 2026-10-30', source: 'log' })
const a = await call('anchor', { shard: 'manual' })
console.log(`anchor txid=${a?.txid ?? '(batch already closed)'}`)
console.log(`prove seq0: ${JSON.stringify((await call('prove', { shard: 'manual', seq: 0 })).result)}`)
const r = await call('recall', { shard: 'manual', seq: 0 })
console.log(`recall: "${r.items?.[0]?.text ?? ''}" check=${r.receipt?.checkResult} model=${r.receipt?.modelId}`)
console.log(`truth note: ${r.receipt?.truthNote}`)
const d = await call('automaton', { op: 'deploy', id: 'doc', def: { Q: ['draft', 'published'], Sigma: ['publish'], delta: [[0, 0, 1]], q0: 0, F: [1], O: [] } })
const s = await call('automaton', { op: 'step', id: 'doc', sym: 0, to: 1, outHash: 'ab'.repeat(32) })
console.log(`automaton: deploy ${d.txid.slice(0, 12)}… → step q=${s.q} (${s.statement})`)
await client.close()
