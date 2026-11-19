// msgbox — the standard MessageBox lane maps the msgs surface onto the
// package client, holds collected ids until ack consumes them (ack = the
// protocol's own receipt — it deletes host-side), and composites with the
// lightweb lane: send fans to both (one delivered leg is a delivery),
// collect merges with canonical-key dedup.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { msgboxMsgs, compositeMsgs, msgboxLane, MSGBOX } from '../src/msgbox.js'
import type { MsgBoxClientLike } from '../src/msgbox.js'
import type { MarketMessage } from '../src/market.js'

const stubClient = () => {
  const calls: { sends: any[]; lists: number; acks: string[][] } = { sends: [], lists: 0, acks: [] }
  const rows: Array<{ messageId: string; sender: string; body: unknown }> = []
  const client: MsgBoxClientLike = {
    sendMessage: async (a) => {
      calls.sends.push(a)
      return { status: 'success', messageId: 'm1' }
    },
    listMessages: async () => {
      calls.lists++
      return rows
    },
    acknowledgeMessage: async (a) => {
      calls.acks.push(a.messageIds)
      return { status: 'success' }
    }
  }
  return { calls, rows, client }
}

const note = (): MarketMessage => ({ type: 'note', wantId: 'w1' }) as MarketMessage

test('send wraps the note in {market,refs} addressed to the market box', async () => {
  const { calls, client } = stubClient()
  const m = msgboxMsgs({ client })
  await m.send('02aa', note(), [{ host: 'h', ref: 'r' } as never])
  assert.equal(calls.sends.length, 1)
  assert.equal(calls.sends[0].recipient, '02aa')
  assert.equal(calls.sends[0].messageBox, MSGBOX)
  assert.equal(calls.sends[0].body.market.type, 'note')
  assert.deepEqual(calls.sends[0].body.refs, [{ host: 'h', ref: 'r' }])
})

test('send omits refs when none are carried', async () => {
  const { calls, client } = stubClient()
  await msgboxMsgs({ client }).send('02aa', note())
  assert.equal('refs' in calls.sends[0].body, false)
})

test('collect returns market items and holds ids until ack deletes them', async () => {
  const { calls, rows, client } = stubClient()
  rows.push(
    { messageId: 'id1', sender: '03bb', body: { market: { type: 'accept', offerId: 'o1' }, refs: [{ h: 1 }] } },
    { messageId: 'id2', sender: '03cc', body: { market: { type: 'delivered', wantId: 'w1' } } },
    // a foreign-box-shaped row with no market field is skipped, not held
    { messageId: 'id3', sender: '03dd', body: { text: 'hello' } }
  )
  const m = msgboxMsgs({ client })
  const got = await m.collect()
  assert.equal(got.length, 2)
  assert.equal(got[0].from, '03bb')
  assert.equal((got[0].msg as { type: string }).type, 'accept')
  assert.deepEqual(got[0].refs, [{ h: 1 }])
  await m.ack!()
  assert.deepEqual(calls.acks, [['id1', 'id2']])
})

test('ack on an empty collection is a no-op', async () => {
  const { calls, client } = stubClient()
  const m = msgboxMsgs({ client })
  await m.collect()
  await m.ack!()
  assert.equal(calls.acks.length, 0)
})

test('composite send fires on both lanes and delivers when only one fails', async () => {
  const a = stubClient()
  const b = stubClient()
  const c = compositeMsgs(msgboxMsgs({ client: a.client }), msgboxMsgs({ client: b.client }))
  await c.send('02aa', note())
  assert.equal(a.calls.sends.length, 1)
  assert.equal(b.calls.sends.length, 1)
  // one leg down → still a delivery, no throw
  a.client.sendMessage = async () => {
    throw new Error('lane A down')
  }
  await c.send('02aa', note())
  // both down → the error surfaces so the outbox retries
  b.client.sendMessage = async () => {
    throw new Error('lane B down')
  }
  await assert.rejects(c.send('02aa', note()), /lane A down/)
})

test('composite collect merges and dedups the same note arriving on both lanes', async () => {
  const a = stubClient()
  const b = stubClient()
  const body = { type: 'accept', offerId: 'o1' }
  a.rows.push({ messageId: 'a1', sender: '03bb', body: { market: body } })
  // same logical note, member order shuffled as plain-JSON lanes can produce
  b.rows.push({ messageId: 'b1', sender: '03bb', body: { market: { offerId: 'o1', type: 'accept' } } })
  b.rows.push({ messageId: 'b2', sender: '03cc', body: { market: { type: 'note', wantId: 'w9' } } })
  const c = compositeMsgs(msgboxMsgs({ client: a.client }), msgboxMsgs({ client: b.client }))
  const got = await c.collect()
  assert.equal(got.length, 2)
  assert.equal(got[0].from, '03bb')
  assert.equal(got[1].from, '03cc')
  await c.ack!()
  assert.deepEqual(a.calls.acks, [['a1']])
  assert.deepEqual(b.calls.acks, [['b1', 'b2']])
})

test('msgboxLane stays inert when disabled or a custom net has no host', () => {
  const wallet = {} as never
  assert.equal(msgboxLane({ wallet, network: 'testnet', enabled: false }), undefined)
  assert.equal(msgboxLane({ wallet, network: 'regtest', enabled: true }), undefined)
  // an injected client still works on a custom net — the test seam
  const { client } = stubClient()
  const lane = msgboxLane({ wallet, network: 'regtest', enabled: true, client })
  assert.ok(lane !== undefined)
})
