import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { MarketBase } from '../src/market/base.js'
import { WAITS } from '../src/waits.js'
import type { MarketDeps } from '../src/market/types.js'

// wake-coverage — the poll-vs-push inventory made executable. Every
// scheduler job, every package-level setInterval, and every commit site
// that lands fresh work must appear in a classification map below. A new
// wait site that arrives without a wake source or a floor/probe class
// fails here — the event plane's coverage gate (event-arch P3/P7: wakes
// shorten latency, bounded polls carry correctness).

// tests run compiled (dist/test/*.js) — anchor on package.json so src/
// resolves to real sources, never dist/*.d.ts
const pkgRoot = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..')
const srcDir = join(pkgRoot, 'src')

const read = (f: string): string => readFileSync(f, 'utf8')
const srcFiles = (dir = srcDir): string[] =>
  readdirSync(dir, { withFileTypes: true }).flatMap((e) => (e.isDirectory() ? srcFiles(join(dir, e.name)) : e.name.endsWith('.ts') ? [join(dir, e.name)] : []))

test('every scheduler job declares its wake source', () => {
  // the checked-in wake map — add a job's entry with the producer signal(s)
  // that fire it, or classify it 'probe-tier'/'floor-only' deliberately
  const WAKE_MAP: Record<string, string> = {
    deals:
      'wakes: watch changed:true, push: fast-leg, onTxProof/Rejected/Abandoned→cycle(), store.workReady commits, dueAtArmed→dueIn, onHeight dueHeight — floor: hot/idle ceiling',
    upkeep: 'wakes: rides cycle() on every push — floor: upkeepMs ceiling',
    watchdog: 'probe-tier: hosts cannot push; it IS the recovery producer (hostBack→unblockOutbox) — floor: watchdogMs',
    watches: 'wakes: post-pass refreshWatches + changed:self — floor: 15s re-arm',
    outbox: 'wakes: store.outboxReady on every enqueue + dueIn(next_at) exact — floor: 60s backstop + end-of-tick poke'
  }
  const found = new Set<string>()
  for (const f of srcFiles()) for (const m of read(f).matchAll(/scheduler?!?\.add\(\{\s*name:\s*'([^']+)'/g)) found.add(m[1])
  for (const name of found) assert.ok(WAKE_MAP[name] !== undefined, `scheduler job '${name}' has no wake-map entry — name its wake source or classify it`)
})

test('every wait primitive site is classified — the census sees all wait classes', () => {
  // setInterval is only ONE wait class. A wait primitive the census can't
  // see is a wait that lands undocumented: setTimeout sleeps, Promise.race
  // abandonments, AbortSignal.timeout dead-man switches, sleep() loops.
  // Every file holding one must be classified below — the helper itself
  // (deadline/scheduler/retry), a named WAITS duration, an io bound, a
  // deliberate mechanism, or a test-tier harness. ev/wait-class-blind.
  const PRIMITIVES = /setTimeout\(|Promise\.race\(|AbortSignal\.timeout|[^a-zA-Z]sleep\(/
  const CLASSIFIED: Record<string, string> = {
    'automation/src/scheduler.ts': 'the wheel — the one setTimeout owner',
    'automation/src/watches.ts': 'watch-shim reconnect sleep — WAITS.watchBackoff*, jittered',
    'automation/src/lanes.ts': 'io() merges rt.activeSignal — per-call io bound',
    'automation/src/toolbox-services.ts': 'WAITS.rpcIo io bound',
    'automation/src/ai.ts': 'AbortSignal.timeout — sandbox io bound (aborts INTO the work)',
    'core/src/deadline.ts': 'deadline()/nap()/spread()/stable01 — THE bound helper',
    'chain/src/retry.ts': 'sleep()/pacer — the shared wait helper (spread-jittered)',
    'chain/src/events.ts': 'waitFor helper + the shared substrate poller floor',
    'chain/src/indexer.ts': 'fetch abort bound — io',
    'chain/src/mainnet.ts': 'fetch io bounds + SSE reconnect (spread) + spendProbe bound',
    'chain/src/regtest.ts': 'rpc io bounds + withRetry curves (spread inside withRetry)',
    'chain/src/spendguard.ts': 'min-interval spend spacing — deliberate mechanism',
    'chain/src/headers.ts': 'bounded header fetch race — io',
    'chain/src/p2p.ts': 'peer reconnect — spread-jittered',
    'chain/src/testkit.ts': 'test-tier harness — regtest mine/wait helpers',
    'bstack/src/hosts.ts': 'fetch io bounds',
    'bstack/src/testing.ts': 'test fault-injection delay',
    'mcp/src/funding.ts': 'wait-for-funds — fundingWake signal + bounded poll',
    'mcp/src/custody.ts': 'accepted() bounded submit probe — spread-jittered',
    'mcp/src/runtime.ts': 'teardown dead-man switch',
    'mcp/src/bin.ts': 'startup probe io + teardown bound',
    'mcp/src/dispatch/memory.ts': 'pack fetch io bound',
    'store/src/objectstore.ts': 's3 fetch io bound',
    'anchor/src/runarchain.ts': 'heal broadcast retry — spread-jittered',
    'zk/src/stark/prove.ts': 'prover service io bound',
    'zk/src/stark/datalog.ts': 'prover subprocess kill bound — dead-man switch'
  }
  const packagesDir = join(pkgRoot, '..')
  for (const pkg of readdirSync(packagesDir, { withFileTypes: true }))
    if (pkg.isDirectory() && pkg.name !== 'e2e' && pkg.name !== 'watcher') {
      const src = join(packagesDir, pkg.name, 'src')
      let files: string[] = []
      try {
        files = srcFiles(src)
      } catch {
        continue
      }
      for (const f of files)
        if (!f.endsWith('.d.ts') && PRIMITIVES.test(read(f))) {
          const rel = f.replaceAll('\\', '/')
          assert.ok(
            Object.keys(CLASSIFIED).some((ok) => rel.endsWith(ok)),
            `${rel}: wait primitive(s) found with no classification — name its class (helper / WAITS duration / io bound / mechanism / harness) in the census map`
          )
        }
    }
})

test('no raw setInterval outside the classified floors', () => {
  // setInterval is the poll primitive — every site must be a named floor or
  // liveness mechanism. Recurring work that waits on a producer belongs on
  // the Scheduler wheel (wake(name)/dueIn) or a push hook instead.
  const FLOORS = [
    'packages/chain/src/events.ts', // THE shared substrate floor — poke()-instrumented
    'packages/chain/src/p2p.ts', // rate-window reset — a throttle wheel, not a wait
    'packages/mcp/src/runtime.ts', // confirmer floor — onAnchorQueued wakes it
    'packages/mcp/src/rtsupport.ts', // instance-lock heartbeat — liveness, not a wait
    'packages/mcp/src/http.ts' // session reaper + SSE keepalive — TTL/transport liveness
  ]
  const packagesDir = join(pkgRoot, '..')
  for (const pkg of readdirSync(packagesDir, { withFileTypes: true }))
    if (pkg.isDirectory() && pkg.name !== 'e2e') {
      const src = join(packagesDir, pkg.name, 'src')
      let files: string[] = []
      try {
        files = srcFiles(src)
      } catch {
        continue
      }
      for (const f of files)
        if (!f.endsWith('.d.ts')) {
          const hits = read(f).match(/setInterval\(/g)
          if (hits !== null && hits.length > 0)
            assert.ok(
              FLOORS.some((ok) => f.replaceAll('\\', '/').endsWith(ok)),
              `${f}: ${hits.length} setInterval site(s) — classify the floor in the wake-coverage map or move the wait to a push hook`
            )
        }
    }
})

test('store.commit hooks fire exactly where work lands', async (t) => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  t.after(() => void kx.destroy())
  let work = 0
  const armed: number[] = []
  store.workReady = () => work++
  store.dueAtArmed = (at) => armed.push(at)

  await t.test('createOperation fires on created, not on a dedupe hit', async () => {
    const before = work
    const a = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'k1',
      policyHash: 'p',
      network: 'regtest',
      state: 'published',
      data: {}
    })
    assert.equal(a.created, true)
    assert.equal(work, before + 1)
    const b = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'k1',
      policyHash: 'p',
      network: 'regtest',
      state: 'published',
      data: {}
    })
    assert.equal(b.created, false)
    assert.equal(work, before + 1)
  })

  await t.test('a committed dueAt arms the deadline', async () => {
    const at = Date.now() + 60_000
    const before = armed.length
    const { created } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'k2',
      policyHash: 'p',
      network: 'regtest',
      state: 'collecting',
      data: {},
      dueAt: at
    })
    assert.equal(created, true)
    assert.equal(armed.length, before + 1)
    assert.equal(armed.at(-1), at)
  })

  await t.test('transition fires on state change only — self-edge data stamps do not wake', async () => {
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'k3',
      policyHash: 'p',
      network: 'regtest',
      state: 'published',
      data: {}
    })
    const before = work
    const n1 = await store.transition(operation.operationId, operation.revision, 'collecting', { cause: 'want_signed' })
    assert.equal(n1.state, 'collecting')
    assert.equal(work, before + 1)
    // tick_error self-edge: a label write, not work — must not fire
    const n2 = await store.transition(n1.operationId, n1.revision, n1.state, { cause: 'tick_error', lastError: 'x' })
    assert.equal(n2.state, 'collecting')
    assert.equal(work, before + 1)
  })

  await t.test('transition dueAt arms; a null dueAt clears without arming', async () => {
    const { operation } = await store.createOperation({
      role: 'buyer',
      kind: 'purchase',
      dedupeKey: 'k4',
      policyHash: 'p',
      network: 'regtest',
      state: 'collecting',
      data: {}
    })
    const at = Date.now() + 30_000
    const before = armed.length
    await store.transition(operation.operationId, operation.revision, 'collecting', { cause: 'tick_error', dueAt: at })
    assert.equal(armed.length, before + 1)
    assert.equal(armed.at(-1), at)
    await store.transition(operation.operationId, operation.revision + 1, 'collecting', { cause: 'tick_error', dueAt: null })
    assert.equal(armed.length, before + 1)
  })

  await t.test('a bounded state stamps its residence deadline at commit — explicit dueAt wins', async () => {
    // ev/unarmed-deadline: the stuck bound is derivable at write time, so
    // the wheel wakes at its instant instead of the next advance() scan
    store.residenceBound = (s) => (s === 'collecting' ? 60_000 : s === 'selected' ? 15_000 : undefined)
    try {
      const t0 = Date.now()
      const { operation } = await store.createOperation({
        role: 'buyer',
        kind: 'purchase',
        dedupeKey: 'k5',
        policyHash: 'p',
        network: 'regtest',
        state: 'collecting',
        data: {}
      })
      assert.ok(operation.dueAt !== undefined && operation.dueAt >= t0 + 60_000, 'the created op carries the stamped bound')
      assert.equal(armed.at(-1), operation.dueAt, 'the stamped bound armed the wheel')
      // explicit dueAt wins over the derived bound (collect window semantics)
      const at = Date.now() + 5_000
      const n = await store.transition(operation.operationId, operation.revision, 'selected', { cause: 'test_harness', dueAt: at })
      assert.equal(n.dueAt, at)
      // and a bare state change re-derives the new state's bound
      const n2 = await store.transition(n.operationId, n.revision, 'collecting', { cause: 'test_harness' })
      assert.ok(n2.dueAt !== undefined && n2.dueAt >= t0 + 60_000, 'the re-entered state re-stamped its bound')
    } finally {
      store.residenceBound = undefined
    }
  })

  await t.test('ingest fires on accepted rows, not on a duplicate batch', async () => {
    const before = work
    const a = await store.ingest('s', [{ messageId: 'm1', artifactHash: 'h1', kind: 'want', payload: {} }], 'c1')
    assert.equal(a.accepted, 1)
    assert.equal(work, before + 1)
    const d = await store.ingest('s', [{ messageId: 'm1', artifactHash: 'h1', kind: 'want', payload: {} }], 'c2')
    assert.equal(d.accepted, 0)
    assert.equal(work, before + 1)
  })

  await t.test('applyTxEvent fires on accepted, never on a rejected step', async () => {
    const t = new Transaction()
    t.addOutput({ satoshis: 7, lockingScript: new P2PKH().lock(PrivateKey.fromRandom().toPublicKey().toHash()) })
    await store.recordPrepared({ actionId: 'a1', operationId: 'op:a1', kind: 'escrow', txid: t.id('hex'), rawHex: t.toHex(), parents: {}, feeSats: 1 }, [
      { txid: 'ff'.repeat(32), vout: 0, satoshis: 1000 }
    ])
    const before = work
    const bad = await store.applyTxEvent('a1', 'obs.proven', { guard: 'proof' })
    assert.equal(bad.accepted, false)
    assert.equal(work, before)
    const ok = await store.applyTxEvent('a1', 'submit', { attempt: true, guard: 'leases-held' })
    assert.equal(ok.accepted, true)
    assert.equal(work, before + 1)
  })
})

test('a wedged run() link is detected and named — the bound resolves the wait, the detector catches the work', async () => {
  // ev/abandon-holds-resource: Promise.race abandons the WAIT, never the
  // work — a hung link keeps the serialized chain forever. The hold
  // detector must fire once, naming the holder so the orphan is findable.
  const logs: string[] = []
  let now = 1_000_000
  class TestEngine extends MarketBase {
    expose<T>(fn: () => Promise<T>, name?: string): Promise<T> {
      return this.run(fn, name)
    }
  }
  const d = {
    store: {},
    key: PrivateKey.fromRandom(),
    network: 'regtest',
    now: () => now,
    log: (line: string) => logs.push(line)
  } as unknown as MarketDeps
  const eng = new TestEngine(d)
  const hang = eng.expose(() => new Promise<never>(() => {}), 'wedged-leg')
  // mark() lands on the chain microtask — the holder is only named once
  // its link has actually taken the head
  await new Promise((r) => setImmediate(r))
  // a queued link joining after the hold bound sees the holder's name
  now += WAITS.runHold.ms + 1
  void eng.expose(async () => undefined, 'victim').catch(() => undefined)
  await new Promise((r) => setImmediate(r))
  assert.equal(logs.length, 1)
  assert.match(logs[0], /held .* by 'wedged-leg'/, `the log must name the holder: ${logs[0]}`)
  // once per link — a third caller doesn't re-warn for the same head
  void eng.expose(async () => undefined, 'victim2').catch(() => undefined)
  await new Promise((r) => setImmediate(r))
  assert.equal(logs.length, 1)
  void hang
})

test('runtime wires the hooks with the busy gate and push clears the ledger throttle', () => {
  // source contract — the wiring is one function over two seams; the
  // assertions pin the semantics the hooks exist for (no scheduler needed)
  const rt = read(join(srcDir, 'runtime.ts'))
  assert.match(
    rt,
    /store\.workReady\s*=\s*\(\)\s*=>\s*\{[^{]*if \(!rt\.busy && !rt\.stopped\)/,
    'workReady must gate on !rt.busy — mid-pass commits ride the pass, unconditional wakes hot-spin'
  )
  assert.match(rt, /store\.dueAtArmed\s*=.*dueIn\('deals'/s, 'committed dueAt must arm the deals wheel')
  assert.match(rt, /lastLedgerSweep\s*=\s*-Infinity/, 'the push path must clear the wtx throttle — a pushed verdict never waits out the cadence')
  assert.match(rt, /mem\.onHeight\?\.\(/, 'dueHeight wake must ride the host height hook')
  assert.match(rt, /rt\.countWake\s*=/, 'the runtime must expose the wake-source counter — unmeasured push paths degrade invisibly')
  assert.match(rt, /countWake\('commit'\)/, 'the commit wake must count itself in wakeStats')
})
