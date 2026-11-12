// API-001 + OPS-002 integration: real HTTP server on an ephemeral port.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { AddressInfo } from 'node:net'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { verifyCheckpoint } from '@nektar/log'
import { NektarMemory, createHttpApi, OPENAPI } from '../src/index.js'

process.env.NEKTAR_TOOL_PROFILE = 'operator'

test('HTTP API: tools, public proof + checkpoint, metrics move, every route documented', async () => {
  const w = await stackWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const srv = createHttpApi(mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/v1`
  const post = (name: string, b: unknown) => fetch(`${base}/tools/${name}`, { method: 'POST', body: JSON.stringify(b), signal: AbortSignal.timeout(5000) })
  try {
    assert.deepEqual(await (await fetch(`${base}/health`)).json(), { ok: true })
    assert.equal((await post('remember', { shard: 'm', data: 'torque 45Nm' })).status, 200)
    await post('remember', { shard: 'm', data: 'belt 10h' })
    assert.equal((await fetch(`${base}/checkpoint/none`)).status, 404)
    await chain.mine(1)
    mem.confirmAnchor('m', mem.tailLink('m')!.txid)
    const pr = (await (await fetch(`${base}/proof/m/0`)).json()) as { result: { ok: boolean }; bundle: unknown }
    assert.deepEqual(pr.result, { ok: true, settled: true, anchorState: 'mined' })
    assert.ok(pr.bundle)
    const cp = (await (await fetch(`${base}/checkpoint/m`)).json()) as { checkpoint: Parameters<typeof verifyCheckpoint>[0]; logPublicKey: string }
    assert.ok(verifyCheckpoint(cp.checkpoint, cp.logPublicKey), 'served checkpoint verifies')
    assert.equal((await post('nope', {})).status, 404)
    assert.match(((await (await post('revoke', { pkHex: 'aa' })).json()) as { error: string }).error, /33-byte/)
    await post('recall', { shard: 'm', seq: 0 })
    const metrics = await (await fetch(`${base}/metrics`)).text()
    assert.match(metrics, /^nektar_anchors_total 1$/m)
    assert.match(metrics, /^nektar_proof_latency_ms_count [1-9]/m)
    assert.match(metrics, /^nektar_unconfirmed_anchors 0$/m)
    assert.match(metrics, /^nektar_promise_violations 0$/m)
    // every implemented route prefix is documented, and vice versa
    const documented = Object.keys(OPENAPI.paths).sort()
    assert.deepEqual(documented, [
      '/.well-known/nektar',
      '/.well-known/x402',
      '/dashboard',
      '/mcp',
      '/v1/badge',
      '/v1/checkpoint/{shard}',
      '/v1/costs',
      '/v1/health',
      '/v1/metrics',
      '/v1/openapi.json',
      '/v1/pack/{shard}',
      '/v1/pay/quote',
      '/v1/proof/{shard}/{seq}',
      '/v1/tools',
      '/v1/tools/{name}',
      '/v1/verify',
      '/v1/wallet',
      '/v1/zkproof/{commit}'
    ])
    assert.equal(((await (await fetch(`${base}/openapi.json`)).json()) as { openapi: string }).openapi, '3.1.0')
  } finally {
    srv.close()
  }
})

test('HTTP API: NEKTAR_HTTP_TOKEN gates every route except /v1/health', async (t) => {
  process.env.NEKTAR_HTTP_TOKEN = 't0ken-xyz'
  try {
    const w = await stackWorld(t, 4)
    if (w === undefined) return
    const { chain, funder } = w
    const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
    const srv = createHttpApi(mem).listen(0)
    await new Promise((r) => srv.once('listening', r))
    const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
    const authed = { headers: { authorization: 'Bearer t0ken-xyz' } }
    try {
      assert.equal((await fetch(`${base}/v1/tools`)).status, 401)
      assert.equal((await fetch(`${base}/v1/tools`, authed)).status, 200)
      assert.equal((await fetch(`${base}/mcp`, { method: 'POST' })).status, 401)
      assert.equal((await fetch(`${base}/v1/health`)).status, 200, 'health stays public for monitors')
      assert.equal((await fetch(`${base}/v1/proof/m/0`, { headers: { authorization: 'Bearer wrong' } })).status, 401)
    } finally {
      srv.close()
    }
  } finally {
    delete process.env.NEKTAR_HTTP_TOKEN
  }
})

test('verify page: OG unfurl meta + install CTA + copyable badge markdown', async (t) => {
  const w = await stackWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const srv = createHttpApi(mem).listen(0)
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  const post = (name: string, b: unknown) => fetch(`${base}/v1/tools/${name}`, { method: 'POST', body: JSON.stringify(b), signal: AbortSignal.timeout(5000) })
  try {
    await post('remember', { shard: 'm', data: 'torque 45Nm' })
    await post('remember', { shard: 'm', data: 'belt 10h' })
    await chain.mine(1)
    mem.confirmAnchor('m', mem.tailLink('m')!.txid)
    const g = (await (await post('groundedAnswer', { claims: [{ text: 'torque 45Nm', cites: [{ shard: 'm', seq: 0 }] }] })).json()) as {
      share?: { token: string }
    }
    await chain.mine(1)
    mem.confirmAnchor('__receipts__', mem.tailLink('__receipts__')!.txid)
    const html = await (await fetch(`${base}/v1/verify?r=${encodeURIComponent(g.share!.token)}`, { headers: { accept: 'text/html' } })).text()
    assert.match(html, /<meta property="og:title"/, 'OG title for unfurls')
    assert.match(html, /<meta property="og:description"/, 'OG description')
    // minimal page: one share action, operator identity, tech inside details
    assert.match(html, /Share this verification/, 'one-click share button')
    assert.match(html, /navigator\.share|clipboard/, 'share/copy handler wired')
    assert.match(html, /operator <span class=mono>/, 'operator identity visible')
    assert.match(html, /<details/, 'checks stay behind collapsed details')
    assert.match(html, /v1\/badge\?r=/, 'badge markdown inside details')
    assert.match(html, /mcp:install|git clone/, 'install command inside details')
    // the clutter stays OFF the page
    assert.doesNotMatch(html, /importShard \{from/, 'no visible import snippets')
    assert.doesNotMatch(html, /this operator: \d+ anchors/, 'no counters')
    // on a real network the page links the anchor tx to a block explorer
    const savedNet = process.env.NEKTAR_NETWORK
    process.env.NEKTAR_NETWORK = 'ttn'
    try {
      const htmlTtn = await (await fetch(`${base}/v1/verify?r=${encodeURIComponent(g.share!.token)}`, { headers: { accept: 'text/html' } })).text()
      assert.match(htmlTtn, /test\.whatsonchain\.com\/tx\/[0-9a-f]{64}/, 'anchor tx links to the testnet explorer')
    } finally {
      if (savedNet === undefined) delete process.env.NEKTAR_NETWORK
      else process.env.NEKTAR_NETWORK = savedNet
    }
  } finally {
    srv.close()
  }
})
