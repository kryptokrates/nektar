// pnpm mcp:smoke — talk to nektar THROUGH the Docker MCP Gateway (exactly what
// Claude Desktop / Cursor / VS Code run) and exercise remember → prove.
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

const profile = process.env.NEKTAR_PROFILE ?? 'nektar'
const client = new Client({ name: 'gateway-smoke', version: '1.0.0' })
await client.connect(new StdioClientTransport({ command: 'docker', args: ['mcp', 'gateway', 'run', '--profile', profile], env: process.env, stderr: 'ignore' }))
const { tools } = await client.listTools()
const names = tools.map((t) => t.name)
console.log(`gateway tools (${names.length}): ${names.join(', ')}`)
for (const n of ['remember', 'recall', 'prove'])
  if (!names.includes(n)) {
    console.error(`✖ ${n} missing`)
    process.exit(1)
  }
const call = async (name, args) => JSON.parse((await client.callTool({ name, arguments: args })).content[0].text)
const w = await call('remember', { shard: 'smoke', data: `gateway works ${new Date().toISOString()}`, source: 'smoke' })
const a = await call('anchor', { shard: 'smoke' })
console.log(`remembered seq=${w.seq}; anchored n=${a?.n} ${a?.txid?.slice(0, 16)}…  (seq/n > 0 ⇒ state persisted from an earlier session)`)
// on regtest the confirmer mines + attaches the BUMP asynchronously: poll (bounded)
let proof
for (let t = 0; t < 30; t++) {
  proof = (await call('prove', { shard: 'smoke', seq: 0 })).result
  if (proof.ok || proof.reason !== 'NotMined') break
  await new Promise((r) => setTimeout(r, 2000))
}
console.log(`prove: ${JSON.stringify(proof)}`)
if (!proof.ok) process.exit(1)
await client.close()
