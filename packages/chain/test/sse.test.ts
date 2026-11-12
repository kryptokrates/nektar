// sse.test.ts — the outbound-only broadcaster status stream (statusStream):
// events in order, comments ignored, malformed data does not kill the stream,
// reconnects resume via Last-Event-ID, cursor persists the last id.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { statusStream } from '../src/mainnet.js'

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

const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms))

test('statusStream: events in order, malformed skipped, reconnect sends Last-Event-ID', async () => {
  const seen: Array<{ id?: string; event?: string; data?: string }> = []
  const lastEventIds: Array<string | undefined> = []
  const persisted: string[] = []
  let call = 0
  const fetchImpl = (async (_input: unknown, init?: RequestInit) => {
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

test('statusStream: cursor.get seeds the first Last-Event-ID', async () => {
  let got: string | undefined
  const fetchImpl = (async (_input: unknown, init?: RequestInit) => {
    got = new Headers(init?.headers).get('Last-Event-ID') ?? undefined
    return stream([], true)
  }) as unknown as typeof fetch
  const s = statusStream({ broadcasterUrl: 'https://b.test', callbackToken: 'tok', fetchImpl }, () => {}, { cursor: { get: () => '42', set: () => {} } })
  for (let i = 0; i < 50 && got === undefined; i++) await sleep(10)
  s.close()
  assert.equal(got, '42')
})
