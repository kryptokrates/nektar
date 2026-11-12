// txlog.test.ts — NEKTAR_TX_LOG accounting: one JSON line per successful
// broadcast with txid, raw size and fee derived from the source outputs.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { Transaction, P2PKH } from '@bsv/sdk'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { txLogBroadcast } from '../src/runtime.js'

test('tx log: two successful broadcasts → two lines with correct fee', async (t) => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-txlog-'))
  const logPath = join(dir, 'tx-log.jsonl')
  const w = await stackWorld(t, 3, 10_000)
  if (w === undefined) return
  const { chain, funder } = w
  const broadcast = txLogBroadcast(chain.broadcast.bind(chain), logPath)
  const spend = (parent: Transaction, sats: number): Transaction => {
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: parent, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
    tx.addOutput({ lockingScript: new P2PKH().lock(funder.address()), satoshis: sats })
    return tx
  }
  const t1 = spend(funder.parents[0]!, 9_000) // fee 1_000
  const t2 = spend(funder.parents[1]!, 9_500) // fee 500
  await t1.sign()
  await t2.sign()
  assert.equal((await broadcast(t1)).status, 'success')
  assert.equal((await broadcast(t2)).status, 'success')
  const lines = readFileSync(logPath, 'utf8')
    .trim()
    .split('\n')
    .map((l) => JSON.parse(l) as { txid: string; bytes: number; inSats: number; outSats: number; feeSats: number; t: number })
  assert.equal(lines.length, 2)
  assert.equal(lines[0]!.txid, t1.id('hex'))
  assert.equal(lines[0]!.bytes, t1.toBinary().length)
  assert.equal(lines[0]!.inSats, 10_000)
  assert.equal(lines[0]!.outSats, 9_000)
  assert.equal(lines[0]!.feeSats, 1_000)
  assert.equal(lines[1]!.feeSats, 500)
  assert.ok(lines[0]!.t > 0)
  // a failed broadcast writes nothing
  const bad = spend(funder.parents[0]!, 100) // re-spends t1's outpoint
  await bad.sign()
  assert.equal((await broadcast(bad)).status, 'error')
  assert.equal(readFileSync(logPath, 'utf8').trim().split('\n').length, 2)
})

test('tx log: no path → the wrap is a pass-through', async (t) => {
  const chain = (await stackChain(t))!
  const inner = chain.broadcast.bind(chain)
  assert.equal(txLogBroadcast(inner, undefined), inner)
  assert.equal(txLogBroadcast(inner, ''), inner)
})
