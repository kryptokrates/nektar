import { test } from 'node:test'
import assert from 'node:assert/strict'
import type { AddressInfo } from 'node:net'
import knexFactory from 'knex'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory } from '../src/memory.js'
import { createHttpApi } from '../src/http.js'
import { AutomationStore } from '../src/automation/store.js'

test('dashboard automation inbox: agent bearer cannot approve; operator token can; origin and expiry enforced', async (t) => {
  process.env.NEKTAR_HTTP_TOKEN = 'agent-token'
  process.env.NEKTAR_OPERATOR_TOKEN = 'operator-token'
  const w = await stackWorld(t, 1)
  if (w === undefined) return
  const { chain, funder } = w
  const mem = new NektarMemory(chain, funder, funder.keyPair, {})
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  mem.automation = { store } as never
  const a = await store.requestApproval({
    operationId: 'op1',
    reason: 'price_above_policy',
    policyHash: 'h',
    actionDigest: 'd',
    maxExtraSats: 100,
    expiresAt: Date.now() + 60_000
  })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  t.after(async () => {
    await new Promise((r) => setTimeout(r, 100))
    ;(srv as any).closeAllConnections?.()
    srv.close()
    await kx.destroy()
    delete process.env.NEKTAR_HTTP_TOKEN
    delete process.env.NEKTAR_OPERATOR_TOKEN
  })
  const post = (auth: string, origin?: string) =>
    fetch(`${base}/v1/automation/approvals/${a.approvalId}`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: auth, ...(origin ? { origin } : {}) },
      body: JSON.stringify({ approve: true })
    })
  assert.equal((await post('Bearer agent-token')).status, 403, 'the agent bearer is not an operator credential')
  assert.equal((await post('Bearer operator-token', 'https://evil.example')).status, 403, 'cross-origin approval refused')
  const ok = await post('Bearer operator-token', 'http://127.0.0.1')
  assert.equal(ok.status, 200, await ok.text())
  assert.equal((await store.approvals('pending')).length, 0)
  assert.equal((await post('Bearer operator-token')).status, 400, 'already-decided approvals cannot be replayed')
  const dash = await (await fetch(`${base}/dashboard`, { headers: { authorization: 'Bearer agent-token' } })).text()
  assert.match(dash, /Automation/)
  assert.match(dash, /no pending approvals/)
})
