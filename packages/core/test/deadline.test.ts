import { test } from 'node:test'
import assert from 'node:assert/strict'
import { currentSignal, deadline, detached, ioSignal, nap, spread, stable01 } from '../src/index.js'

// deadline() — the abort-not-race wait primitive. The load-bearing claims:
// the bound ABORTS the work's signal (not just resolves the waiter), the
// outcome names which branch ended the wait, an abandoned leg's late
// rejection is swallowed (no unhandled rejection), and nap() observes
// cancellation as a throw.

test('deadline: settled work returns its value', async () => {
  const r = await deadline(() => Promise.resolve(42), 100)
  assert.deepEqual(r, { outcome: 'settled', value: 42 })
})

test('deadline: real work errors propagate, not eaten by the outcome type', async () => {
  await assert.rejects(
    deadline(() => {
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

// ── the ambient leg context ──────────────────────────────────────────────
// deadline() publishes its merged signal on AsyncLocalStorage; ioSignal/
// nap merge it automatically; detached() runs spawned work outside it.

test('leg context: currentSignal() is the work signal inside deadline(), undefined outside', async () => {
  assert.equal(currentSignal(), undefined)
  let inside: AbortSignal | undefined
  const r = await deadline((_signal) => {
    inside = currentSignal()
    return Promise.resolve('ok')
  }, 100)
  assert.deepEqual(r, { outcome: 'settled', value: 'ok' })
  assert.ok(inside !== undefined, 'the leg must publish its signal on the ambient context')
  assert.equal(inside!.aborted, true, 'leg end settles the composite — AbortSignal.any dependents release their source slots')
  assert.equal(currentSignal(), undefined, 'context must not leak past the leg')
})

test('leg context: ioSignal() inherits the ambient leg — aborts at the bound', async () => {
  let io: AbortSignal | undefined
  const r = await deadline(async () => {
    io = ioSignal(60_000)
    await nap(60_000) // ambient merges — dies with the leg, no explicit signal
    return 'never'
  }, 30)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal(io!.aborted, true, 'IO derived inside the leg shares the operation deadline by construction')
})

test('leg context: nap() with no explicit signal inside a leg dies with the leg', async () => {
  let name: string | undefined
  const r = await deadline(async () => {
    try {
      await nap(60_000)
    } catch (e) {
      name = (e as Error).name
      throw e
    }
  }, 25)
  assert.deepEqual(r, { outcome: 'timeout' })
  await nap(10) // the abandoned work's rejection lands after the race resolves
  assert.equal(name, 'AbortError', "timers/promises rejects AbortError regardless of the source signal's reason")
})

test('leg context: detached() work runs outside the leg — a spawn survives the bound', async () => {
  let spawned: AbortSignal | undefined
  const r = await deadline(async () => {
    // the spawn pattern: a post-commit hook / emitter launch — it must not
    // inherit (and die with) the leg that happened to be running
    detached(() => {
      spawned = currentSignal()
    })
    await nap(60_000)
    return 'never'
  }, 30)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal(spawned, undefined, "spawned work sees no ambient — it is not the leg's child")
})

test('leg context: spawned async work launched via detached() is not aborted when the leg times out', async () => {
  let saw: { ambient: AbortSignal | undefined; settled: boolean } | undefined
  const r = await deadline(async () => {
    detached(() => {
      const ambient = currentSignal()
      void nap(10).then(() => {
        saw = { ambient, settled: true }
      })
    })
    await nap(60_000)
    return 'never'
  }, 20)
  assert.deepEqual(r, { outcome: 'timeout' })
  await nap(30) // the spawn's own schedule — it must complete after the leg died
  assert.deepEqual(saw, { ambient: undefined, settled: true })
})

test('leg context: nested deadline() can only tighten — inner dies at the outer bound', async () => {
  let inner: AbortSignal | undefined
  const r = await deadline(async () => {
    const innerR = deadline(async (signal) => {
      inner = signal
      await nap(60_000)
      return 'inner-never'
    }, 60_000)
    // the outer leg dies first — the inner leg's huge budget must not save it
    return await innerR
  }, 30)
  assert.deepEqual(r, { outcome: 'timeout' })
  assert.equal(inner!.aborted, true, 'the inner leg cannot outlive the outer — one operation budget inward')
})

test("leg context: concurrent legs never see each other's signal", async () => {
  let sigA: AbortSignal | undefined
  let sigB: AbortSignal | undefined
  const [a, b] = await Promise.all([
    deadline(async (_s) => {
      sigA = currentSignal()
      await nap(5)
      return 'a'
    }, 5_000),
    deadline(async (_s) => {
      sigB = currentSignal()
      await nap(60_000)
      return 'b-never'
    }, 25)
  ])
  assert.deepEqual(a, { outcome: 'settled', value: 'a' })
  assert.deepEqual(b, { outcome: 'timeout' })
  assert.notEqual(sigA, sigB)
  assert.equal(sigA!.aborted, true, 'leg A settled — its composite settled at leg end')
  assert.equal(sigB!.aborted, true, 'leg B timed out — its composite aborted at the bound')
})

test('leg context: explicit signals still merge — ioSignal honors the caller umbrella', async () => {
  const ac = new AbortController()
  let io: AbortSignal | undefined
  // the runtime's shape: the umbrella arrives via opts.signal (rt.signal),
  // inner IO merges its own caller handle — both must reach the leg
  const p = deadline(
    async () => {
      io = ioSignal(60_000, ac.signal)
      await nap(60_000)
      return 'never'
    },
    60_000,
    { signal: ac.signal }
  )
  ac.abort()
  assert.deepEqual(await p, { outcome: 'aborted' }, 'a shutdown is not a slowness')
  assert.equal(io!.aborted, true, 'umbrella reaches IO derived inside the leg')
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
