// rt-determinism.test.ts — the replay-determinism guard for the service
// bodies: every send a body issues must be derivable from its journaled
// steps, so a replayed invocation re-issues the IDENTICAL send stream. An
// unjournaled clock/counter read feeding an idempotencyKey or a delayMs
// re-keys the send on replay — under restate the journal mismatches and
// the invocation dies instead of completing. The harness records the
// (journal, sends) pair, moves the domain clock across a bucket boundary,
// and replays the body answering run() from the journal alone.
import test, { beforeEach } from 'node:test'
import assert from 'node:assert/strict'
import type { RtSend } from '../src/rt/exec.js'
import {
  wakeTxProofBody,
  wakeTxRejectedBody,
  wakeTxAbandonedBody,
  wakeHeightBody,
  cronTickBody,
  dealfanTickBody,
  transportDrainBody,
  sweepTickBody,
  sweepBootBody
} from '../src/rt/services.js'

type Run = <T>(name: string, fn: () => Promise<T>) => Promise<T>
/** a body normalized to the two seams the journal covers — arg baked in */
type Body = (send: RtSend, run: Run) => Promise<unknown>
interface SendRec {
  service: string
  handler: string
  key?: string
  delayMs?: number
  idempotencyKey?: string
}
interface JournalEntry {
  name: string
  value: unknown
}

/** the record pass: run() executes and journals each step's value */
const record = async (body: Body, sends: SendRec[], journal: JournalEntry[]): Promise<void> => {
  const send: RtSend = async (target, _arg, opts) => {
    sends.push({ service: target.service, handler: target.handler, key: target.key, delayMs: opts?.delayMs, idempotencyKey: opts?.idempotencyKey })
  }
  const run: Run = async <T>(name: string, fn: () => Promise<T>): Promise<T> => {
    const value = await fn()
    journal.push({ name, value })
    return value
  }
  await body(send, run)
}

/** the replay pass: run() answers from the journal without re-executing —
 *  the journaled values, not the moved clock, must drive the sends */
const replay = async (body: Body, sends: SendRec[], journal: JournalEntry[]): Promise<void> => {
  const send: RtSend = async (target, _arg, opts) => {
    sends.push({ service: target.service, handler: target.handler, key: target.key, delayMs: opts?.delayMs, idempotencyKey: opts?.idempotencyKey })
  }
  const run: Run = async <T>(name: string, _fn: () => Promise<T>): Promise<T> => {
    const e = journal.shift()
    assert.ok(e !== undefined, `replay ran a journaled step the record pass never ran: ${name}`)
    assert.equal(e.name, name, `journal mismatch — replay ran '${name}', the journal holds '${e.name}'`)
    return e.value as T
  }
  await body(send, run)
  assert.equal(journal.length, 0, 'the replay left journaled steps unconsumed')
}

// the mutable domain clock — deps.now reads it; +1500ms crosses the 1s
// coalesce bucket the wake tails quantize on
let clock = 1_000_000

/** record → move the clock → replay: the send stream must be identical */
const deterministic = async (body: Body, msg: string): Promise<void> => {
  const sends: SendRec[] = []
  const journal: JournalEntry[] = []
  await record(body, sends, journal)
  clock += 1500
  const sends2: SendRec[] = []
  await replay(body, sends2, journal)
  assert.deepEqual(sends2, sends, msg)
}

beforeEach(() => {
  clock = 1_000_000
})

void test('deterministic: wake.txProof journals its poke bucket', async () => {
  const deps = { legs: { txProof: () => Promise.resolve() }, hint: () => {}, now: () => clock, log: () => {} }
  await deterministic((send, run) => wakeTxProofBody(deps)(send, { txid: 'tx1', mpHex: 'aa'.repeat(40) }, run), 'txProof re-keyed its cycle tail')
})

void test('deterministic: wake.txRejected journals its poke bucket', async () => {
  const deps = { legs: {}, hint: () => {}, now: () => clock, log: () => {} }
  await deterministic((send, run) => wakeTxRejectedBody(deps)(send, { txid: 'tx2' }, run), 'txRejected re-keyed its cycle tail')
})

void test('deterministic: wake.txAbandoned journals its poke bucket', async () => {
  const deps = { legs: { txAbandoned: () => Promise.resolve() }, hint: () => {}, now: () => clock, log: () => {} }
  await deterministic((send, run) => wakeTxAbandonedBody(deps)(send, { txid: 'tx3' }, run), 'txAbandoned re-keyed its cycle tail')
})

void test('deterministic: wake.height journals its poke bucket', async () => {
  const deps = { legs: { dueAtHeight: () => Promise.resolve(true) }, hint: () => {}, now: () => clock, log: () => {} }
  await deterministic((send, run) => wakeHeightBody(deps)(send, { tip: 100 }, run), 'height re-keyed its due poke')
})

void test('deterministic: cron.tick re-arms with the journaled clock', async () => {
  const deps = { tick: () => Promise.resolve(), ceilingMs: () => 2000, now: () => clock, log: () => {} }
  await deterministic((send, run) => cronTickBody('nektar-testcron', deps)(send, run), 'cron re-armed on the moved clock')
})

void test('deterministic: dealfan.tick re-fans on journaled inputs', async () => {
  const deps = {
    pendingInbox: () => Promise.resolve([{ source: 's1', messageId: 'm1', kind: 'want', payload: { wantId: 'w1' } }]),
    liveOps: () => Promise.resolve([]),
    reconcileOps: () => Promise.resolve([]),
    // the lane-discovery leg — wired so the body runs it inside its
    // journaled run('poll') on every pass
    poll: () => Promise.resolve(),
    ceilingMs: () => 0,
    now: () => clock,
    sentRecently: () => false,
    log: () => {}
  }
  await deterministic((send, run) => dealfanTickBody(deps)(send, run), 'dealfan re-keyed its fan sends')
  // verify batching: all local reads + dup filtering + arm planning live in
  // one 'plan' step so idle ticks never pay 10+ Restate roundtrips
  const steps: string[] = []
  await dealfanTickBody(deps)(
    () => Promise.resolve(),
    (name, fn) => {
      steps.push(name)
      return fn()
    }
  )
  assert.ok(steps.length <= 3, `expected ≤3 journaled steps per dealfan tick, got ${steps.length}: ${steps.join(', ')}`)
})

void test('deterministic: transport.drain re-arms with the journaled clock', async () => {
  const item = { actionId: 'a1', operationId: 'op1', kind: 'k', destination: 'h1', payload: {}, attempts: 0, nextAt: clock + 5000 }
  const deps = {
    dueRows: () => Promise.resolve([item]),
    nextAt: () => Promise.resolve(clock + 5000),
    deliverItem: () => Promise.resolve('acked' as const),
    now: () => clock,
    log: () => {}
  }
  await deterministic((send, run) => transportDrainBody(deps)(send, run), 'drain re-keyed or re-timed its self-arm')
})

void test('deterministic: sweep.tick re-issues the journaled scan', async () => {
  const deps = {
    scan: async function* () {
      yield { actionId: 'a1', observe: { event: 'obs.seen', guard: 'verified' } }
    },
    resubmitRows: () => Promise.resolve([]),
    log: () => {}
  }
  await deterministic((send, run) => sweepTickBody(deps)(send, run), 'sweep tick diverged on replay')
})

void test('deterministic: sweep.boot re-issues the journaled row set', async () => {
  const deps = {
    scan: async function* () {},
    resubmitRows: () => Promise.resolve([{ actionId: 'a1' }]),
    log: () => {}
  }
  await deterministic((send, run) => sweepBootBody(deps)(send, run), 'sweep boot diverged on replay')
})
