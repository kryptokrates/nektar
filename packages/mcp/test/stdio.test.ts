// AGT-002 integration: the real MCP server process over stdio, driven by the
// OFFICIAL MCP SDK client (protocol negotiation, tools, resources, prompts).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { fileURLToPath } from 'node:url'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StdioClientTransport } from '@modelcontextprotocol/sdk/client/stdio.js'

const text = (r: unknown) => JSON.parse(((r as { content: Array<{ text: string }> }).content[0]!).text)

test('MCP stdio (SDK client): instructions, annotated tools, tool calls, resources, prompts', { timeout: 30_000 }, async () => {
  const bin = fileURLToPath(new URL('../src/bin.js', import.meta.url))
  const transport = new StdioClientTransport({ command: process.execPath, args: [bin], env: { ...process.env as Record<string, string>, NEKTAR_NETWORK: 'mock', NEKTAR_MODEL_ID: 'stdio-test', NEKTAR_DEMO_UTXOS: '20', NEKTAR_BATCH: '2' }, stderr: 'ignore' })
  const client = new Client({ name: 'nektar-test', version: '1.0.0' })
  await client.connect(transport)
  try {
    assert.equal(client.getServerVersion()?.name, 'nektar')
    assert.match(client.getInstructions() ?? '', /recall/)
    const { tools } = await client.listTools()
    assert.equal(tools.length, 44)
    const by = new Map(tools.map(t => [t.name, t]))
    assert.equal(by.get('recall')!.annotations?.readOnlyHint, true)
    assert.equal(by.get('revoke')!.annotations?.destructiveHint, true)
    assert.equal(by.get('remember')!.annotations?.readOnlyHint, false)
    await client.callTool({ name: 'remember', arguments: { shard: 'm', data: 'torque 45Nm' } })
    const r2 = text(await client.callTool({ name: 'remember', arguments: { shard: 'm', data: 'belt 10h' } }))
    assert.ok(r2.anchored, 'batch of 2 anchored')
    assert.deepEqual(text(await client.callTool({ name: 'prove', arguments: { shard: 'm', seq: 0 } })).result, { ok: true, settled: true, anchorState: 'mined' })
    const rec = text(await client.callTool({ name: 'recall', arguments: { shard: 'm', seq: 0 } }))
    assert.equal(rec.items[0].text, 'torque 45Nm'); assert.equal(rec.receipt.modelId, 'stdio-test')
    const bad = await client.callTool({ name: 'nope', arguments: {} })
    assert.equal(bad.isError, true)
    const cp = await client.readResource({ uri: 'nektar://checkpoint/m' })
    assert.ok(JSON.parse((cp.contents[0] as { text: string }).text).checkpoint.sig)
    const rr = JSON.parse(((await client.readResource({ uri: 'nektar://record/m/0' })).contents[0] as { text: string }).text)
    assert.deepEqual(rr.proof, { ok: true, settled: true, anchorState: 'mined' })
    const { prompts } = await client.listPrompts()
    assert.deepEqual(prompts.map(p => p.name).sort(), ['audit-answer', 'grounded-answer', 'remember-with-source'])
    const gp = await client.getPrompt({ name: 'grounded-answer', arguments: { question: 'What torque for port 4?', shard: 'm' } })
    assert.match((gp.messages[0]!.content as { text: string }).text, /recall/)
  } finally { await client.close() }
})
