import { test } from 'node:test'
import assert from 'node:assert/strict'
import knex from 'knex'
import { PrivateKey } from '@bsv/sdk'
import { utf8, fromHex, toHex, computeLeaf, identityHash } from '@nektar/core'
import { MemoryShardStore } from '../src/store.js'
import { KnexShardStore } from '../src/knex-store.js'
import { BatchLog } from '../src/batchlog.js'

const frag = (i: number) => ({
  d: utf8(`content ${i}`),
  p: { source: 'test', time: 1700000000 + i, signature: fromHex('00'.repeat(64)), modelId: 'test-model' }
})

for (const [name, makeStore] of [
  ['memory', async () => new MemoryShardStore()],
  [
    'knex-sqlite',
    async () => {
      const k = knex({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
      const s = new KnexShardStore(k)
      await s.init()
      const orig = s.all.bind(s)
      ;(s as any).all = async (sh: string) => {
        const r = await orig(sh)
        return r
      }
      ;(s as any).destroy = () => k.destroy()
      return s
    }
  ]
] as const) {
  test(`batchlog (${name}): dedup, promise, batch close, lifecycle`, async (t) => {
    const store = await makeStore()
    t.after(async () => {
      await (store as any).destroy?.()
    })
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
    const { verifyProof } = await import('@nektar/core')
    assert.equal(verifyProof(batch.leaves[1], proof, batch.R), true)

    await log.markAnchored('s1', batch.n, 'aa'.repeat(32))
    const all = await log['store'].all('s1')
    assert.equal(
      all.every((r) => r.state === 'anchored' && r.anchorTxid === 'aa'.repeat(32)),
      true
    )
  })

  test(`batchlog (${name}): concurrent remembers get unique seqs; dedup survives concurrency`, async (t) => {
    const store = await makeStore()
    t.after(async () => {
      await (store as any).destroy?.()
    })
    const log = new BatchLog(store, PrivateKey.fromRandom(), { maxLeaves: 100 })
    await log.ready
    const rs = await Promise.all([...Array(8).keys()].map((i) => log.remember('s1', frag(i))))
    const seqs = rs.map((r) => r.rec.seq).sort((a, b) => a - b)
    assert.deepEqual(seqs, [0, 1, 2, 3, 4, 5, 6, 7], 'every concurrent write lands on a distinct seq')
    // the SAME fragment racing itself dedups — no UNIQUE-violation thrown
    const dup = await Promise.all([log.remember('s1', frag(42)), log.remember('s1', frag(42))])
    assert.equal(dup[0].rec.seq, dup[1].rec.seq)
    assert.equal(dup.filter((d) => d.duplicate).length >= 1, true, 'at least one of the racing identical writes reports duplicate')
  })

  test(`batchlog (${name}): resetUnanchored returns a dropped batch to the pool`, async (t) => {
    const store = await makeStore()
    t.after(async () => {
      await (store as any).destroy?.()
    })
    const log = new BatchLog(store, PrivateKey.fromRandom(), { maxLeaves: 2 })
    await log.ready
    await log.remember('s1', frag(1))
    await log.remember('s1', frag(2))
    const b1 = await log.closeBatch('s1')
    assert.ok(b1)
    // anchor never lands: failed broadcast or crash before markAnchored —
    // the records were marked batched and sit stranded
    assert.equal(await log.closeBatch('s1'), null, 'batched records are not re-collected without reset')
    assert.equal(await log.resetUnanchored('s1'), 2, 'both orphans return to the pool')
    const b2 = await log.closeBatch('s1')
    assert.ok(b2)
    assert.deepEqual(
      b2.leaves.map((l) => toHex(l)),
      b1.leaves.map((l) => toHex(l)),
      're-collected batch carries the same leaves'
    )
    assert.equal(b2.n, b1.n, 'batch index is reused — the on-chain head still expects it')
    await log.markAnchored('s1', b2.n, 'bb'.repeat(32))
    assert.equal(await log.resetUnanchored('s1'), 0, 'anchored records are never reset')
  })

  test(`batchlog (${name}): prepare hook runs inside the queue with the assigned seq, before the leaf`, async (t) => {
    const store = await makeStore()
    t.after(async () => {
      await (store as any).destroy?.()
    })
    const log = new BatchLog(store, PrivateKey.fromRandom())
    await log.ready
    const plain = frag(1)
    const expectedPlainLeaf = toHex(computeLeaf(plain))
    let seen = -1
    const r = await log.remember('s1', plain, {}, (ff, seq) => {
      seen = seq
      ff.E = utf8('ciphertext')
      ff.hdp = identityHash(ff) // bind plaintext before any caller would drop d
    })
    assert.equal(seen, r.rec.seq, 'prepare observes the seq that was actually assigned')
    assert.notEqual(toHex(r.rec.leaf), expectedPlainLeaf, 'leaf is computed AFTER prepare mutated the fragment')
    assert.equal(toHex(r.rec.leaf), toHex(computeLeaf(r.rec.fragment)))
    // prepare does not run on a dedup hit
    let ran = false
    const d = await log.remember('s1', frag(1), {}, () => {
      ran = true
    })
    assert.equal(d.duplicate, true)
    assert.equal(ran, false)
  })
}
