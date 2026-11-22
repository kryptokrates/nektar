import { test } from 'node:test'
import assert from 'node:assert/strict'
import { withRetry } from '../src/retry.js'

// retry — the shared curve carries the jitter so no caller can land a
// phase-aligned cadence: withRetry applies spread() (0.5x..1.5x) to the
// declared delay at every site. Mocked Math.random pins the spread end;
// fake timers make the bound exact — an unjittered delay resolves at the
// curve's literal value, a jittered one at the spread value.

test('withRetry jitters the caller curve — the sleep is the spread value, not the literal', async (t) => {
  t.mock.timers.enable({ apis: ['setTimeout'], now: 0 })
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
  // under the curve literal the sleep is still pending; at the spread
  // value (half here) the retry has already fired
  t.mock.timers.tick(499)
  for (let i = 0; i < 8; i++) await Promise.resolve()
  assert.equal(tries, 1, 'the jittered wait has not finished before its spread bound')
  t.mock.timers.tick(1)
  for (let i = 0; i < 8; i++) await Promise.resolve()
  assert.equal(tries, 2)
  assert.equal(value, 'ok')
})
