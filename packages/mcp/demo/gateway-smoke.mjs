// pnpm mcp:smoke — talk to mamem THROUGH the Docker MCP Gateway (exactly what
// Claude Desktop / Cursor / VS Code run) and exercise remember → prove.
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

const profile = process.env.MAMEM_PROFILE ?? 'mamem'
const client = new Client({ name: 'gateway-smoke', version: '1.0.0' })
await client.connect(new StdioClientTransport({ command: 'docker', args: ['mcp', 'gateway', 'run', '--profile', profile], env: process.env, stderr: 'ignore' }))
const { tools } = await client.listTools()
const names = tools.map(t => t.name)
console.log(`gateway tools (${names.length}): ${names.join(', ')}`)
for (const n of ['remember', 'recall', 'prove']) if (!names.includes(n)) { console.error(`✖ ${n} missing`); process.exit(1) }
const call = async (name, args) => JSON.parse((await client.callTool({ name, arguments: args })).content[0].text)
await call('remember', { shard: 'smoke', data: 'gateway works', source: 'smoke' })
const a = await call('anchor', { shard: 'smoke' })
console.log(`anchored ${a?.txid?.slice(0, 16)}…`)
console.log(`prove: ${JSON.stringify((await call('prove', { shard: 'smoke', seq: 0 })).result)}`)
await client.close()
