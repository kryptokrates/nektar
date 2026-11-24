import { test } from 'node:test'
import assert from 'node:assert/strict'
import { deadline, nap } from '@nektar/core'
import { pacer, withRetry } from '../src/retry.js'

// retry — the shared curve carries the jitter so no caller can land a
// phase-aligned cadence: withRetry applies spread() (0.5x..1.5x) to the
// declared delay at every site. Mocked Math.random pins the spread end.
// Real-time waits (nap rides timers/promises, outside mock.timers reach):
// a 400ms pause must not have fired the spread(1000)=500 bound, a further
// 300ms must have. The margins are wide enough for CI clock jitter.

test('withRetry jitters the caller curve — the sleep is the spread value, not the literal', async (t) => {
  // spread(1000, 0) = ceil(1000 * 0.5) = 500 — the minimum-jitter end
  t.mock.method(Math, 'random', () => 0)
  let tries = 0
  let value: string | undefined
  const done = withRetry(
    () => {
      tries += 1
      return Promise.resolve(tries >= 2 ? 'ok' : 'again')
    },
    { attempts: 3, delay: () => 1000, retryIf: (r) => r !== 'ok' }
  )
  void done.then((v) => {
    value = v
  })
  // the first attempt runs without waiting
  for (let i = 0; i < 8; i++) await Promise.resolve()
  assert.equal(tries, 1)
  assert.equal(value, undefined)
  // under the curve literal the sleep is still pending at 400ms; at the
  // spread value (~500ms) the retry has already fired
  await nap(400)
  assert.equal(tries, 1, 'the jittered wait has not finished before its spread bound')
  await nap(300)
  assert.equal(tries, 2)
  assert.equal(value, 'ok')
})

test('pacer: an aborted caller inside deadline() never poisons subsequent callers', async () => {
  const pace = pacer(200)
  await pace() // seed last = now so the next caller sleeps
  const aborted = await deadline(() => pace(), 30)
  assert.equal(aborted.outcome, 'timeout')
  // a later caller must still resolve cleanly, not inherit the aborted tail rejection
  await pace()
})
