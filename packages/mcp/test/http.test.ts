// API-001 + OPS-002 integration: real HTTP server on an ephemeral port.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { AddressInfo } from 'node:net'
import { MockChain, MockFunder } from '@mamem/chain'
import { verifyCheckpoint } from '@mamem/log'
import { MamemMemory, createHttpApi, OPENAPI } from '../src/index.js'

test('HTTP API: tools, public proof + checkpoint, metrics move, every route documented', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(10); for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const srv = createHttpApi(mem).listen(0)
  await new Promise(r => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/v1`
  const post = (name: string, b: unknown) => fetch(`${base}/tools/${name}`, { method: 'POST', body: JSON.stringify(b), signal: AbortSignal.timeout(5000) })
  try {
    assert.deepEqual(await (await fetch(`${base}/health`)).json(), { ok: true })
    assert.equal((await post('remember', { shard: 'm', data: 'torque 45Nm' })).status, 200)
    await post('remember', { shard: 'm', data: 'belt 10h' })
    assert.equal((await fetch(`${base}/checkpoint/none`)).status, 404)
    chain.mine(1); mem.confirmAnchor('m', mem.tailLink('m')!.txid)
    const pr = await (await fetch(`${base}/proof/m/0`)).json() as { result: { ok: boolean }, bundle: unknown }
    assert.deepEqual(pr.result, { ok: true, settled: true, anchorState: 'mined' }); assert.ok(pr.bundle)
    const cp = await (await fetch(`${base}/checkpoint/m`)).json() as { checkpoint: Parameters<typeof verifyCheckpoint>[0], logPublicKey: string }
    assert.ok(verifyCheckpoint(cp.checkpoint, cp.logPublicKey), 'served checkpoint verifies')
    assert.equal((await post('nope', {})).status, 404)
    assert.match(((await (await post('revoke', { pkHex: 'aa' })).json()) as { error: string }).error, /33-byte/)
    await post('recall', { shard: 'm', seq: 0 })
    const metrics = await (await fetch(`${base}/metrics`)).text()
    assert.match(metrics, /^mamem_anchors_total 1$/m)
    assert.match(metrics, /^mamem_proof_latency_ms_count [1-9]/m)
    assert.match(metrics, /^mamem_unconfirmed_anchors 0$/m)
    assert.match(metrics, /^mamem_promise_violations 0$/m)
    // every implemented route prefix is documented, and vice versa
    const documented = Object.keys(OPENAPI.paths).sort()
    assert.deepEqual(documented, ['/.well-known/x402', '/mcp', '/v1/checkpoint/{shard}', '/v1/costs', '/v1/health', '/v1/metrics', '/v1/openapi.json', '/v1/pay/quote', '/v1/proof/{shard}/{seq}', '/v1/tools', '/v1/tools/{name}', '/v1/verify', '/v1/wallet'])
    assert.equal(((await (await fetch(`${base}/openapi.json`)).json()) as { openapi: string }).openapi, '3.1.0')
  } finally { srv.close() }
})

test('HTTP API: MAMEM_HTTP_TOKEN gates every route except /v1/health', async () => {
  process.env.MAMEM_HTTP_TOKEN = 't0ken-xyz'
  try {
    const chain = new MockChain(); chain.verifyScripts = true
    const funder = new MockFunder(); funder.topup(4); for (const t of funder.parents) chain.registerParent(t)
    const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
    const srv = createHttpApi(mem).listen(0)
    await new Promise(r => srv.once('listening', r))
    const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
    const authed = { headers: { authorization: 'Bearer t0ken-xyz' } }
    try {
      assert.equal((await fetch(`${base}/v1/tools`)).status, 401)
      assert.equal((await fetch(`${base}/v1/tools`, authed)).status, 200)
      assert.equal((await fetch(`${base}/mcp`, { method: 'POST' })).status, 401)
      assert.equal((await fetch(`${base}/v1/health`)).status, 200, 'health stays public for monitors')
      assert.equal((await fetch(`${base}/v1/proof/m/0`, { headers: { authorization: 'Bearer wrong' } })).status, 401)
    } finally { srv.close() }
  } finally { delete process.env.MAMEM_HTTP_TOKEN }
})
