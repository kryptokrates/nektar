import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { utf8, toHex, dsha256 } from '@nektar/core'
import { BatchLog, MemoryShardStore, signProvenance, verifyProvenance, signCheckpoint, verifyCheckpoint, PromiseMonitor } from '../src/index.js'

const prov = { source: 'doi:10.1/x', time: 1_700_000_000, signature: new Uint8Array(0), modelId: 'm' }

test('COM-003: provenance signature binds data and every provenance field', async () => {
  const src = PrivateKey.fromRandom()
  const pub = src.toPublicKey().toString()
  const d = utf8('torque 45Nm')
  const p = await signProvenance(src, d, prov)
  assert.ok(verifyProvenance(pub, d, p))
  assert.equal(verifyProvenance(pub, utf8('torque 46Nm'), p), false, 'tampered data')
  assert.equal(verifyProvenance(pub, d, { ...p, source: 'doi:10.1/y' }), false, 'tampered source')
  assert.equal(verifyProvenance(pub, d, { ...p, time: p.time + 1 }), false, 'tampered time')
  assert.equal(verifyProvenance(PrivateKey.fromRandom().toPublicKey().toString(), d, p), false, 'wrong key')
  assert.equal(verifyProvenance(pub, d, prov), false, 'unsigned')
  assert.equal(verifyProvenance('zz', d, p), false, 'malformed key')
})

test('LOG-006: close policy — leaves, age, bytes, cost thresholds with an injected clock', async () => {
  let now = 1000
  const mk = (policy: ConstructorParameters<typeof BatchLog>[2]) => new BatchLog(new MemoryShardStore(), PrivateKey.fromRandom(), policy, () => now)
  const frag = (s: string) => ({ d: utf8(s), p: prov })
  const leaves = mk({ maxLeaves: 2 })
  await leaves.remember('s', frag('a'))
  assert.equal(await leaves.closeReason('s'), null)
  await leaves.remember('s', frag('b'))
  assert.equal(await leaves.closeReason('s'), 'leaves')
  const age = mk({ maxLeaves: 100, maxAgeMs: 5000 })
  await age.remember('s', frag('a'))
  now += 4999
  assert.equal(await age.closeReason('s'), null)
  now += 1
  assert.equal(await age.closeReason('s'), 'age')
  await age.closeBatch('s')
  assert.equal(await age.closeReason('s'), null, 'age resets after close')
  const bytes = mk({ maxLeaves: 100, maxBytes: 200 })
  await bytes.remember('s', frag('x'.repeat(50)))
  assert.equal(await bytes.closeReason('s'), null)
  await bytes.remember('s', frag('y'.repeat(150)))
  assert.equal(await bytes.closeReason('s'), 'bytes')
  const cost = mk({ maxLeaves: 100, maxCostSats: 1, satsPerByte: 0.01 })
  await cost.remember('s', frag('z'.repeat(20)))
  assert.equal(await cost.closeReason('s'), null)
  await cost.remember('s', frag('w'.repeat(100)))
  assert.equal(await cost.closeReason('s'), 'cost')
  assert.equal(await mk({ maxLeaves: 1 }).closeReason('empty'), null, 'nothing pending never closes')
})

test('LOG-007/005: signed checkpoints; monitor flags uncovered promises, ignores forged checkpoints', async () => {
  const logKey = PrivateKey.fromRandom()
  const logPub = logKey.toPublicKey().toString()
  const base = { logId: 'aa'.repeat(20), n: 0, root: toHex(dsha256(utf8('r'))), chainedRoot: toHex(dsha256(utf8('rp'))), anchorTxid: 'bb'.repeat(32) }
  const cp = await signCheckpoint(logKey, { ...base, size: 2, time: 1500 })
  assert.ok(verifyCheckpoint(cp, logPub))
  assert.equal(verifyCheckpoint({ ...cp, size: 3 }, logPub), false, 'size tamper')
  assert.equal(verifyCheckpoint(cp, PrivateKey.fromRandom().toPublicKey().toString()), false, 'wrong log key')
  const mon = new PromiseMonitor(logPub, () => base.logId)
  for (const seq of [0, 1, 2]) mon.observePromise({ shard: 's', seq, issuedAt: 1000 })
  assert.equal(mon.observeCheckpoint(cp), true)
  assert.equal(mon.observeCheckpoint(await signCheckpoint(PrivateKey.fromRandom(), { ...base, size: 99, time: 1500 })), false, 'forged checkpoint ignored')
  assert.deepEqual(mon.violations(1800, 1000), [], 'inside the merge delay nothing is late')
  assert.deepEqual(
    mon.violations(2500, 1000).map((v) => v.seq),
    [2],
    'seq 2 not covered by size-2 checkpoint'
  )
  mon.observeCheckpoint(await signCheckpoint(logKey, { ...base, n: 1, size: 3, time: 2600 }))
  assert.deepEqual(
    mon.violations(3000, 1000).map((v) => v.seq),
    [2],
    'a checkpoint after the deadline does not cure the violation'
  )
  assert.deepEqual(mon.violations(3000, 2000), [], 'with a longer delay it is fulfilled')
})
