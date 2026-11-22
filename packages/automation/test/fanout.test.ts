// fanout — every fan-out leg gets a per-host repair row. upkeep calls
// queueRepairs with journal keys (`log:{stream}:{seq}`, `logpub:{stream}:{txid}`,
// `finger:sub:{kind}:{id}`); the repair dispatcher hands payload.key to
// resubmitCarrier, which re-serves the journaled BEEF. These tests pin the
// row contract the wiring relies on.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { queueRepairs, trackFanout } from '../src/fanout.js'
import type { OutboxItem } from '../src/store.js'

const sink = () => {
  const rows: Array<Omit<OutboxItem, 'attempts'> & { delayMs?: number }> = []
  const acked: string[] = []
  return {
    rows,
    acked,
    io: {
      enqueue: (o: Omit<OutboxItem, 'attempts'>, opts?: { delayMs?: number }) => {
        rows.push({ ...o, ...(opts?.delayMs !== undefined ? { delayMs: opts.delayMs } : {}) })
        return Promise.resolve()
      },
      ackOutbox: (id: string) => {
        acked.push(id)
        return Promise.resolve()
      }
    }
  }
}

void test('queueRepairs: one repair-host row per missed host, payload carries the journal key', async () => {
  const s = sink()
  await queueRepairs(s.io, 'log:stream:7', ['h1', 'h2'])
  assert.equal(s.rows.length, 2)
  assert.deepEqual(
    s.rows.map((r) => [r.actionId, r.kind, r.destination]),
    [
      ['repair:log:stream:7:h1', 'repair-host', 'h1'],
      ['repair:log:stream:7:h2', 'repair-host', 'h2']
    ]
  )
  assert.deepEqual(s.rows[0].payload, { key: 'log:stream:7' })
  assert.equal(s.rows[0].delayMs, undefined, 'missed legs repair immediately')
})

void test('trackFanout: missed now, pending delayed, settled legs cancel their own repair', async () => {
  const s = sink()
  await trackFanout(
    {
      hash: 'deadbeef',
      missed: [{ host: 'h1', error: 'down' }],
      pending: ['h2'],
      settled: Promise.resolve({ took: ['h2'], missed: [] })
    },
    s.io
  )
  assert.equal(s.rows.length, 2)
  assert.equal(s.rows[0].actionId, 'repair:deadbeef:h1')
  assert.equal(s.rows[0].delayMs, undefined)
  assert.equal(s.rows[1].actionId, 'repair:deadbeef:h2')
  assert.equal(s.rows[1].delayMs, 20_000, 'a straggler is repaired only if it never lands')
  await new Promise((r) => setImmediate(r))
  assert.deepEqual(s.acked, ['repair:deadbeef:h2'], 'the leg that landed cancels its delayed repair row')
})
