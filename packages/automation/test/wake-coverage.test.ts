import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, readdirSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'

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
    const ok = await store.applyTxEvent('a1', 'submit', { attempt: true })
    assert.equal(ok.accepted, true)
    assert.equal(work, before + 1)
  })
})

test('runtime wires the hooks with the busy gate and push clears the ledger throttle', () => {
  // source contract — the wiring is one function over two seams; the
  // assertions pin the semantics the hooks exist for (no scheduler needed)
  const rt = read(join(srcDir, 'runtime.ts'))
  assert.match(
    rt,
    /store\.workReady\s*=\s*\(\)\s*=>\s*\{\s*if \(!rt\.busy && !rt\.stopped\)/,
    'workReady must gate on !rt.busy — mid-pass commits ride the pass, unconditional wakes hot-spin'
  )
  assert.match(rt, /store\.dueAtArmed\s*=.*dueIn\('deals'/s, 'committed dueAt must arm the deals wheel')
  assert.match(rt, /lastLedgerSweep\s*=\s*-Infinity/, 'the push path must clear the wtx throttle — a pushed verdict never waits out the cadence')
  assert.match(rt, /mem\.onHeight\?\.\(/, 'dueHeight wake must ride the host height hook')
})
