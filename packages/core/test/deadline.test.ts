import { test } from 'node:test'
import assert from 'node:assert/strict'
import { deadline, nap, spread, stable01 } from '../src/index.js'

// deadline() — the abort-not-race wait primitive. The load-bearing claims:
// the bound ABORTS the work's signal (not just resolves the waiter), the
// outcome names which branch ended the wait, an abandoned leg's late
// rejection is swallowed (no unhandled rejection), and nap() observes
// cancellation as a throw.

test('deadline: settled work returns its value', async () => {
  const r = await deadline(async () => 42, 100)
  assert.deepEqual(r, { outcome: 'settled', value: 42 })
})

test('deadline: real work errors propagate, not eaten by the outcome type', async () => {
  await assert.rejects(
    deadline(async () => {
      throw new Error('boom')
    }, 100),
    /boom/
  )
})

test('deadline: a slow leg reports timeout AND receives the abort', async () => {
  let sawAbort: string | undefined
  const r = await deadline(async (signal) => {
    signal.addEventListener(
      'abort',
      () => {
        sawAbort = 'yes'
      },
      { once: true }
    )
    await nap(60_000, { signal })
    return 'never'
  }, 30)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal(sawAbort, 'yes', 'the bound must reach the work — the race alone never cancels it')
})

test('deadline: the caller signal names the outcome aborted, not timeout', async () => {
  const ac = new AbortController()
  const p = deadline(async (signal) => await nap(60_000, { signal }).then(() => 'never'), 60_000, { signal: ac.signal })
  ac.abort()
  assert.deepEqual(await p, { outcome: 'aborted' }, 'a shutdown is not a slowness — do not log it as an overrun')
})

test("deadline: an abandoned leg's late rejection is swallowed", async () => {
  // regression: without the suppressor, the orphaned promise's throw lands
  // as an unhandled rejection AFTER the caller moved on
  const r = await deadline(async (signal) => {
    if (signal.aborted) throw new Error('late reject')
    await new Promise((res) => signal.addEventListener('abort', res, { once: true }))
    throw new Error('late reject')
  }, 20)
  assert.deepEqual(r, { outcome: 'timeout' })
  await nap(10) // give the orphaned rejection a tick to surface if unswallowed
})

test('nap: resolves on time; rejects AbortError on signal', async () => {
  await nap(1)
  const ac = new AbortController()
  const p = nap(60_000, { signal: ac.signal })
  ac.abort()
  await assert.rejects(p, (e: Error) => e.name === 'AbortError')
})

test('spread: bounded [0.5x, 1.5x]; stable01 is deterministic and in [0,1)', () => {
  for (const rand of [0, 0.5, 0.999]) {
    const v = spread(1000, rand)
    assert.ok(v >= 500 && v <= 1500, `spread(1000, ${rand}) = ${v}`)
  }
  assert.equal(stable01('outbox:action:1'), stable01('outbox:action:1'))
  assert.notEqual(stable01('a'), stable01('b'))
  const s = stable01('any-key')
  assert.ok(s >= 0 && s < 1)
})
