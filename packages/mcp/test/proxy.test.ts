import { test } from 'node:test'
import assert from 'node:assert/strict'
import { createServer } from 'node:http'
import { mkdtempSync, rmSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { testWorld } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import { seedWelcome } from '../src/welcome.js'

void test('L0 LLM Memory Proxy: OpenAI & Anthropic injection, composite key auth, SSE streaming, and opt-in capture', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkdtempSync(join(tmpdir(), 'nektar-proxy-'))
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })

  const seen: Array<{ path: string; auth?: string; apiKey?: string; body: Record<string, unknown> }> = []
  const upstream = createServer((req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c: Buffer) => chunks.push(c))
    req.on('end', () => {
      const parsed = chunks.length ? (JSON.parse(Buffer.concat(chunks).toString('utf8')) as Record<string, unknown>) : {}
      seen.push({
        path: req.url ?? '',
        auth: typeof req.headers.authorization === 'string' ? req.headers.authorization : undefined,
        apiKey: typeof req.headers['x-api-key'] === 'string' ? req.headers['x-api-key'] : undefined,
        body: parsed
      })
      if (parsed.stream === true) {
        res.writeHead(200, { 'content-type': 'text/event-stream' })
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: 'Hello ' } }] })}\n\n`)
        res.write(`data: ${JSON.stringify({ choices: [{ delta: { content: 'streamed!' } }] })}\n\n`)
        res.end('data: [DONE]\n\n')
        return
      }
      if ((req.url ?? '').includes('/messages')) {
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(JSON.stringify({ id: 'msg_1', content: [{ type: 'text', text: 'Anthropic reply' }] }))
        return
      }
      res.writeHead(200, { 'content-type': 'application/json' })
      res.end(JSON.stringify({ id: 'chatcmpl_1', choices: [{ message: { role: 'assistant', content: 'OpenAI reply' } }] }))
    })
  }).listen(0, '127.0.0.1')
  await new Promise((r) => upstream.once('listening', r))
  const upBase = `http://127.0.0.1:${(upstream.address() as AddressInfo).port}`

  const prevOai = process.env.NEKTAR_PROXY_OPENAI_UPSTREAM
  const prevAnt = process.env.NEKTAR_PROXY_ANTHROPIC_UPSTREAM
  const prevTok = process.env.NEKTAR_HTTP_TOKEN
  process.env.NEKTAR_PROXY_OPENAI_UPSTREAM = upBase
  process.env.NEKTAR_PROXY_ANTHROPIC_UPSTREAM = upBase
  process.env.NEKTAR_HTTP_TOKEN = 'nektar-proxy-tok-999'

  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    // 1. Unauthenticated proxy call is refused
    const unauth = await fetch(`${base}/v1/proxy/openai/v1/chat/completions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: 'Bearer sk-openai-only' },
      body: JSON.stringify({ model: 'gpt-4o', messages: [{ role: 'user', content: 'How does groundedAnswer work?' }] })
    })
    assert.equal(unauth.status, 401)

    // 2. Composite key `nkt:<nektarToken>:<openaiKey>` authenticates and strips prefix before upstream
    const r1 = await fetch(`${base}/v1/proxy/openai/v1/chat/completions`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: 'Bearer nkt:nektar-proxy-tok-999:sk-real-key-42' },
      body: JSON.stringify({ model: 'gpt-4o', messages: [{ role: 'user', content: 'How does groundedAnswer work?' }] })
    })
    assert.equal(r1.status, 200)
    assert.ok(Number(r1.headers.get('x-nektar-memory-hits')) >= 1, 'recalled welcome fragments injected')
    assert.equal(seen[0].auth, 'Bearer sk-real-key-42', 'upstream receives clean OpenAI key only')
    const msgs = seen[0].body.messages as Array<{ role: string; content: string }>
    assert.equal(msgs[0].role, 'system')
    assert.match(msgs[0].content, /\[welcome:\d+\]/)

    // 3. Streaming SSE passthrough + opt-in capture via X-Nektar-Token + X-Nektar-Capture
    const r2 = await fetch(`${base}/v1/proxy/openai/v1/chat/completions`, {
      method: 'POST',
      headers: {
        'content-type': 'application/json',
        authorization: 'Bearer sk-stream-key',
        'x-nektar-token': 'nektar-proxy-tok-999',
        'x-nektar-capture': '1',
        'x-nektar-shard': 'captured-agent'
      },
      body: JSON.stringify({ model: 'gpt-4o', stream: true, messages: [{ role: 'user', content: 'Remember torque spec 45Nm' }] })
    })
    assert.equal(r2.status, 200)
    const sseBody = await r2.text()
    assert.match(sseBody, /Hello /)
    assert.match(sseBody, /streamed!/)
    // Wait briefly for async post-turn capture
    await new Promise((r) => setTimeout(r, 100))
    const capRecs = await mem.records('captured-agent')
    assert.equal(capRecs.length, 1, 'opt-in capture saved turn to target shard')

    // 4. Anthropic /v1/messages with composite x-api-key injects into `system`
    const r3 = await fetch(`${base}/v1/proxy/anthropic/v1/messages`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', 'x-api-key': 'nkt:nektar-proxy-tok-999:sk-ant-real-7' },
      body: JSON.stringify({ model: 'claude-sonnet-4-5', system: 'Be brief.', messages: [{ role: 'user', content: 'Tell me about groundedAnswer' }] })
    })
    assert.equal(r3.status, 200)
    assert.equal(seen[2].apiKey, 'sk-ant-real-7')
    const sysText = typeof seen[2].body.system === 'string' ? seen[2].body.system : ''
    assert.match(sysText, /Be brief\./)
    assert.match(sysText, /\[welcome:\d+\]/)

    // 5. Path traversal refused
    const bad = await fetch(`${base}/v1/proxy/openai/v1/..%2fsecret`, {
      headers: { 'x-nektar-token': 'nektar-proxy-tok-999' }
    })
    assert.equal(bad.status, 400)
  } finally {
    srv.close()
    upstream.close()
    if (prevOai === undefined) delete process.env.NEKTAR_PROXY_OPENAI_UPSTREAM
    else process.env.NEKTAR_PROXY_OPENAI_UPSTREAM = prevOai
    if (prevAnt === undefined) delete process.env.NEKTAR_PROXY_ANTHROPIC_UPSTREAM
    else process.env.NEKTAR_PROXY_ANTHROPIC_UPSTREAM = prevAnt
    if (prevTok === undefined) delete process.env.NEKTAR_HTTP_TOKEN
    else process.env.NEKTAR_HTTP_TOKEN = prevTok
    rmSync(dir, { recursive: true, force: true })
  }
})
