import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { decodeFunding } from '@lightwebinc/bcommon'
import knex from 'knex'
import { mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { mintTree, FUNDING_TAG, KnexTreeLedger, MemoryTreeLedger } from '../src/tree.js'
import { fund, prove } from './helpers.js'

test('tree: n=8 mints 8 funding outputs + change, each decodes our tag', async () => {
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 8, satsPerOutput: 1000 })
  assert.equal(t.outputs.length, 9)
  for (let i = 0; i < 8; i++) assert.ok(decodeFunding(t.outputs[i]!.lockingScript, Array.from(FUNDING_TAG)))
})

test('ledger: unproven tree is not handed out; once proven it is', async () => {
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 2, satsPerOutput: 1000 })
  const l = new MemoryTreeLedger()
  await l.add(t, 2)
  assert.equal(await l.take(), undefined)
  prove(t); await l.markProven(t.id('hex'))
  assert.ok(await l.take())
})

test('ledger: two instances on one sqlite file never hand the same slot twice', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'bstack-'))
  const f = join(dir, 'l.db')
  const kx = (x: string) => knex({ client: 'sqlite3', connection: { filename: x }, useNullAsDefault: true })
  const k1 = kx(f)
  const key = PrivateKey.fromRandom()
  const t = await mintTree({ key, funding: fund(key), n: 4, satsPerOutput: 1000 })
  prove(t)
  const a = new KnexTreeLedger(k1)
  await a.add(t, 4)
  const k2 = kx(f)
  const b = new KnexTreeLedger(k2)
  const got = new Set<string>()
  for (let i = 0; i < 10; i++) {
    const s = (i % 2 ? b : a); const r = await s.take()
    if (r) { const k = `${r.tree.id('hex')}:${r.vout}`; assert.ok(!got.has(k), `dup ${k}`); got.add(k) }
  }
  assert.equal(got.size, 4)
  await k1.destroy(); await k2.destroy(); rmSync(dir, { recursive: true, force: true })
})
