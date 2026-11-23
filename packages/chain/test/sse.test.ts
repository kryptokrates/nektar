// sse.test.ts — the outbound-only broadcaster status stream (statusStream):
// events in order, comments ignored, malformed data does not kill the stream,
// reconnects resume via Last-Event-ID, cursor persists the last id.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { statusStream } from '../src/mainnet.js'
import { nap } from '@nektar/core'

const stream = (chunks: string[], open = false): Response =>
  new Response(
    new ReadableStream({
      start(ctl) {
        const enc = new TextEncoder()
        for (const c of chunks) ctl.enqueue(enc.encode(c))
        if (!open) ctl.close()
      }
    }),
    { status: 200, headers: { 'content-type': 'text/event-stream' } }
  )

const sleep = nap

test('statusStream: events in order, malformed skipped, reconnect sends Last-Event-ID', async () => {
  const seen: Array<{ id?: string; event?: string; data?: string }> = []
  const lastEventIds: Array<string | undefined> = []
  const persisted: string[] = []
  let call = 0
  const fetchImpl = ((_input: unknown, init?: RequestInit) => {
    call++
    lastEventIds.push(new Headers(init?.headers).get('Last-Event-ID') ?? undefined)
    if (call === 1) {
      return stream([
        ': keepalive\n\n',
        'id: 7\nevent: status\ndata: {"txid":"a","txStatus":"SEEN_ON_NETWORK"}\n\n',
        'id: 8\ndata: {malformed\n\n',
        'id: 9\nevent: status\ndata: {"txid":"b","txStatus":"MINED","merklePath":"00"}\n\n'
      ]) // stream ends → reconnect
    }
    return stream(['id: 10\nevent: status\ndata: {"txid":"c","txStatus":"IMMUTABLE"}\n\n'], true)
  }) as unknown as typeof fetch

  const s = statusStream(
    { broadcasterUrl: 'https://b.test', callbackToken: 'tok', fetchImpl },
    (e) => {
      seen.push(e)
    },
    {
      minBackoffMs: 10,
      cursor: {
        get: () => undefined,
        set: (id) => {
          persisted.push(id)
        }
      }
    }
  )
  for (let i = 0; i < 100 && (seen.length < 4 || call < 2); i++) await sleep(10)
  s.close()
  assert.equal(call >= 2, true, 'reconnect after the first stream ended')
  // ids 7,8,9 on the first connection, 10 on the second — in order; the
  // malformed data line was delivered but did not kill the stream (the
  // consumer JSON-parses and skips it — see runtime wiring)
  assert.deepEqual(
    seen.map((e) => e.id),
    ['7', '8', '9', '10']
  )
  assert.equal(seen[2]?.event, 'status')
  assert.equal(lastEventIds[0], undefined, 'first connect has no Last-Event-ID')
  assert.equal(lastEventIds[1], '9', 'reconnect resumes at the last delivered id')
  assert.deepEqual(persisted, ['7', '8', '9', '10'], 'cursor.set persisted each id')
})

test('statusStream: cursor advances only after onEvent resolves', async () => {
  const atDelivery: string[] = []
  const persisted: string[] = []
  const fetchImpl = (() => stream(['id: 1\ndata: {"a":1}\n\n', 'id: 2\ndata: {"b":2}\n\n'], true)) as unknown as typeof fetch
  const s = statusStream(
    { broadcasterUrl: 'https://b.test', callbackToken: 'tok', fetchImpl },
    (e) => {
      atDelivery.push(`${e.id}:${persisted.at(-1) ?? 'none'}`)
    },
    { minBackoffMs: 10, cursor: { get: () => undefined, set: (id) => void persisted.push(id) } }
  )
  for (let i = 0; i < 100 && atDelivery.length < 2; i++) await sleep(10)
  s.close()
  // inside onEvent for id '2' the persisted cursor is still '1' — a crash
  // mid-handler redelivers on restart
  assert.deepEqual(atDelivery, ['1:none', '2:1'])
  assert.deepEqual(persisted, ['1', '2'])
})

test('statusStream: a throwing onEvent reports via onError and the cursor still advances', async () => {
  const errors: Error[] = []
  const persisted: string[] = []
  const seen: string[] = []
  const fetchImpl = (() => stream(['id: 1\ndata: x\n\n', 'id: 2\ndata: y\n\n'], true)) as unknown as typeof fetch
  const s = statusStream(
    { broadcasterUrl: 'https://b.test', callbackToken: 'tok', fetchImpl },
    (e) => {
      seen.push(e.id!)
      if (e.id === '1') throw new Error('poison')
    },
    { minBackoffMs: 10, cursor: { get: () => undefined, set: (id) => void persisted.push(id) }, onError: (e) => errors.push(e) }
  )
  for (let i = 0; i < 100 && seen.length < 2; i++) await sleep(10)
  s.close()
  assert.equal(errors.length >= 1, true, 'the poison event surfaced through onError')
  assert.deepEqual(persisted, ['1', '2'], 'a poison event must not wedge the stream')
})

test('statusStream: cursor.get seeds the first Last-Event-ID', async () => {
  let got: string | undefined
  const fetchImpl = ((_input: unknown, init?: RequestInit) => {
    got = new Headers(init?.headers).get('Last-Event-ID') ?? undefined
    return stream([], true)
  }) as unknown as typeof fetch
  const s = statusStream({ broadcasterUrl: 'https://b.test', callbackToken: 'tok', fetchImpl }, () => {}, { cursor: { get: () => '42', set: () => {} } })
  for (let i = 0; i < 50 && got === undefined; i++) await sleep(10)
  s.close()
  assert.equal(got, '42')
})

test('statusStream: a 401 endpoint rotates to the next broadcaster; the cursor resets on switch', async () => {
  const seen: Array<{ id?: string }> = []
  const calls: Array<{ url: string; lastEventId?: string }> = []
  const fetchImpl = ((input: unknown, init?: RequestInit) => {
    const url = String(input)
    calls.push({ url, lastEventId: new Headers(init?.headers).get('Last-Event-ID') ?? undefined })
    if (url.startsWith('https://a.test')) return new Response(null, { status: 401 })
    return stream(['id: 5\ndata: {"ok":1}\n\n'], true)
  }) as unknown as typeof fetch
  const s = statusStream(
    { broadcasterUrl: 'https://a.test', broadcasterUrls: ['https://a.test', 'https://b.test'], callbackToken: 'tok', fetchImpl },
    (e) => {
      seen.push(e)
    },
    { minBackoffMs: 10, cursor: { get: () => '99', set: () => {} } }
  )
  for (let i = 0; i < 100 && seen.length < 1; i++) await sleep(10)
  s.close()
  assert.equal(calls[0]?.url.startsWith('https://a.test'), true, 'primary endpoint is tried first')
  assert.equal(
    calls.some((c) => c.url.startsWith('https://b.test')),
    true,
    'a 401 rotates the stream to the next broadcaster — the push path survives a keyless primary'
  )
  const bCall = calls.find((c) => c.url.startsWith('https://b.test'))
  assert.equal(bCall?.lastEventId, undefined, 'Last-Event-ID resets on endpoint switch — cursors are per-endpoint')
  assert.deepEqual(
    seen.map((e) => e.id),
    ['5']
  )
})
