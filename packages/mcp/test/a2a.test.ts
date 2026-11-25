import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, rmSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { testWorld } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import { seedWelcome } from '../src/welcome.js'

void test('A2A: /.well-known/agent.json satisfies legacy + A2A Agent Card schema and POST /a2a runs tasks/send', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkdtempSync(join(tmpdir(), 'nektar-a2a-'))
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    const card = (await (await fetch(`${base}/.well-known/agent.json`)).json()) as {
      name: string
      url: string
      protocolVersion: string
      skills: Array<{ id: string }>
      endpoints: Record<string, string>
    }
    assert.equal(card.name, 'nektar')
    assert.match(card.url, /\/a2a$/)
    assert.ok(card.protocolVersion)
    assert.ok(card.skills.some((s) => s.id === 'recall'))
    assert.match(card.endpoints.proxyOpenai, /\/v1\/proxy\/openai\/v1$/)
    assert.match(card.endpoints.oauth, /oauth-authorization-server$/)

    // Natural text A2A task routes to recall
    const r1 = (await (
      await fetch(`${base}/a2a`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          jsonrpc: '2.0',
          id: 1,
          method: 'tasks/send',
          params: { id: 'task-1', message: { role: 'user', parts: [{ type: 'text', text: 'groundedAnswer' }] } }
        })
      })
    ).json()) as { result: { id: string; status: { state: string }; artifacts: Array<{ parts: Array<{ data?: { items?: unknown[] } }> }> } }
    assert.equal(r1.result.id, 'task-1')
    assert.equal(r1.result.status.state, 'completed')
    assert.ok((r1.result.artifacts[0].parts[0].data?.items?.length ?? 0) >= 1)

    // Structured tool A2A task
    const r2 = (await (
      await fetch(`${base}/a2a`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({
          jsonrpc: '2.0',
          id: 2,
          method: 'message/send',
          params: { message: { role: 'user', parts: [{ type: 'data', data: { tool: 'remember', args: { shard: 'a2a', data: 'stored via a2a' } } }] } }
        })
      })
    ).json()) as { result: { status: { state: string } } }
    assert.equal(r2.result.status.state, 'completed')
    assert.equal((await mem.records('a2a')).length, 1)
  } finally {
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})
