// MCP over Streamable HTTP (the transport remote connectors use), SDK client.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { AddressInfo } from 'node:net'
import { Client } from '@modelcontextprotocol/sdk/client/index.js'
import { StreamableHTTPClientTransport } from '@modelcontextprotocol/sdk/client/streamableHttp.js'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory, createHttpApi } from '../src/index.js'

test('MCP Streamable HTTP: session, tools, call, resource; unknown session rejected', { timeout: 30_000 }, async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(10); for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise(r => srv.once('listening', r))
  const url = new URL(`http://127.0.0.1:${(srv.address() as AddressInfo).port}/mcp`)
  const client = new Client({ name: 'http-test', version: '1.0.0' })
  try {
    await client.connect(new StreamableHTTPClientTransport(url))
    assert.equal((await client.listTools()).tools.length, 12)
    await client.callTool({ name: 'remember', arguments: { shard: 'h', data: 'a' } })
    await client.callTool({ name: 'remember', arguments: { shard: 'h', data: 'b' } })
    const rr = await client.readResource({ uri: 'mamem://record/h/0' })
    assert.equal(JSON.parse((rr.contents[0] as { text: string }).text).state, 'anchored')
    const res = await fetch(url, { method: 'POST', headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream', 'mcp-session-id': 'forged' }, body: JSON.stringify({ jsonrpc: '2.0', id: 1, method: 'tools/list' }) })
    assert.equal(res.status, 404, 'forged session id refused')
  } finally { await client.close(); srv.close() }
})
