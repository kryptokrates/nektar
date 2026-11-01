import { test } from 'node:test'
import assert from 'node:assert/strict'
import knex from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { utf8, fromHex, eq } from '@mamem/core'
import { MemoryShardStore } from '../src/store.js'
import { KnexShardStore } from '../src/knex-store.js'
import { BatchLog } from '../src/batchlog.js'

const frag = (i: number) => ({
  d: utf8(`content ${i}`),
  p: { source: 'test', time: 1700000000 + i, signature: fromHex('00'.repeat(64)), modelId: 'test-model' }
})

for (const [name, makeStore] of [
  ['memory', async () => new MemoryShardStore()],
  ['knex-sqlite', async () => { const k = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true }); const s = new KnexShardStore(k); await s.init(); const orig = s.all.bind(s); (s as any).all = async (sh: string) => { const r = await orig(sh); return r }; (s as any).destroy = () => k.destroy(); return s }]
] as const) {
  test(`batchlog (${name}): dedup, promise, batch close, lifecycle`, async (t) => {
    const store = await makeStore()
    t.after(async () => { await (store as any).destroy?.() })
    const log = new BatchLog(store, PrivateKey.fromRandom(), { maxLeaves: 2 })
    const r1 = await log.remember('s1', frag(1))
    const r2 = await log.remember('s1', frag(1))
    assert.equal(r2.duplicate, true, 'identity-hash dedup')
    assert.equal(r2.rec.seq, r1.rec.seq)
    assert.equal(log.verifyPromise(r1.promise), true, 'signed inclusion promise verifies')

    assert.equal(await log.shouldClose('s1'), false)
    await log.remember('s1', frag(2))
    assert.equal(await log.shouldClose('s1'), true)

    const batch = await log.closeBatch('s1')
    assert.ok(batch)
    assert.equal(batch.n, 0)
    assert.equal(batch.size, 2)
    const proof = log.batchProof(batch, 1)
    const { verifyProof } = await import('@mamem/core')
    assert.equal(verifyProof(batch.leaves[1]!, proof, batch.R), true)

    await log.markAnchored('s1', batch.n, 'aa'.repeat(32))
    const all = await log['store'].all('s1')
    assert.equal(all.every(r => r.state === 'anchored' && r.anchorTxid === 'aa'.repeat(32)), true)
  })
}
