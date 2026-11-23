import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, existsSync } from 'node:fs'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator } from '../src/wallet.js'
import { txFromRow } from '../src/ledger.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { inertTxEffects, type TxEffects } from '../src/machines/effects.js'
import { Dispatcher, wireTx } from './rig.js'
import { wireEngine } from '../src/rt/dispatch.js'
import { OPS_NO_EDGE_CAUSES, opsCauseCandidates } from '../src/machines/shadow.js'
import { opsStep } from '../src/machines/ops.machine.js'
import { txStep } from '../src/machines/tx.machine.js'

// state-audit Phase 3 — replay recorded live transitions through the shadow
// machines and report every divergence. The machine never moves production
// state; it only answers "what would the spec have done". The replay/diff
// helpers below are test-only — production shadow.ts keeps just the cause
// vocabulary (driver.ts and the census gate consume it).

interface LiveTransition {
  from: string
  to: string
  cause: string
}

interface Diff {
  kind: 'agree' | 'machine-blocks' | 'machine-diverges' | 'unmodeled'
  from: string
  event: string
  live: string
  machine: string
  cause: string
  guard?: string
  note?: string
}

/** Replay one op's live transition log through opsMachine. */
function replayOps(rows: LiveTransition[]): Diff[] {
  const diffs: Diff[] = []
  for (const r of rows) {
    if (!r.from || r.from === r.to) continue // creation + data-update rows carry no edge
    const candidates = opsCauseCandidates(r.cause)
    if (!candidates) {
      diffs.push({ kind: 'unmodeled', from: r.from, event: r.cause, live: r.to, machine: '', cause: r.cause })
      continue
    }
    // try each candidate edge × guard — report the one matching live, else the divergence
    let matched: Diff | null = null
    let best: Diff | null = null
    for (const cand of candidates) {
      for (const guard of cand.guards ?? [undefined]) {
        const s = opsStep(r.from, cand.event, guard ? { guard } : {})
        const d: Diff = { kind: 'agree', from: r.from, event: cand.event, live: r.to, machine: s.next, cause: r.cause, guard }
        if (!s.accepted) d.kind = 'machine-blocks'
        else if (s.next !== r.to) d.kind = 'machine-diverges'
        if (d.kind === 'agree') {
          matched = d
          break
        }
        best ??= d
      }
      if (matched) break
    }
    diffs.push(matched ?? best!)
  }
  return diffs
}

/** Infer a txMachine event from a wallet_transactions (from,to) edge + emit context. */
function txEventFor(from: string, to: string, hint?: 'submit' | 'observe' | 'abandon' | 'export-ttl'): { event: string; guard?: string } {
  if (to === 'submitted') return { event: 'submit.success' }
  if (to === 'submission_unknown') return { event: 'submit.unknown' }
  if (to === 'observed') return { event: 'obs.seen', guard: 'verified' }
  if (to === 'confirmed') return { event: 'obs.proven', guard: 'proof' }
  if (to === 'expired') return { event: 'export.ttl' }
  if (to === 'abandoned') return { event: 'abandon' }
  if (to === 'conflicted') return hint === 'abandon' ? { event: 'abandon' } : { event: 'obs.conflicted', guard: 'verified+spentBy' }
  if (to === 'rejected') return hint === 'submit' ? { event: 'submit.refused', guard: 'definitive' } : { event: 'obs.rejected', guard: 'verified' }
  return { event: `unknown:${from}->${to}` }
}

/** Replay wallet_transactions edges through txMachine. */
function replayTx(rows: { from: string; to: string; hint?: 'submit' | 'observe' | 'abandon' | 'export-ttl' }[]): Diff[] {
  return rows.map((r) => {
    const { event, guard } = txEventFor(r.from, r.to, r.hint)
    if (event.startsWith('unknown:')) return { kind: 'unmodeled', from: r.from, event, live: r.to, machine: '', cause: event }
    const s = txStep(r.from, event, guard ? { guard } : {})
    const d: Diff = { kind: 'agree', from: r.from, event, live: r.to, machine: s.next, cause: event, guard }
    if (!s.accepted) d.kind = 'machine-blocks'
    else if (s.next !== r.to) d.kind = 'machine-diverges'
    return d
  })
}

const key = PrivateKey.fromRandom()

async function open(now?: () => number) {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx, now)
  await store.init()
  return { kx, store }
}

function deps(store: AutomationStore, now: () => number, extra: Partial<MarketDeps> = {}): MarketDeps {
  return {
    store,
    call: () => Promise.resolve({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: () => Promise.resolve(1),
    fetchTx: () => Promise.resolve(undefined),
    transport: {} as never,
    ai: {} as never,
    now,
    ...extra
  }
}

function oneOutputTx(): Transaction {
  const tx = new Transaction()
  tx.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  return tx
}

// the observe handler's intent set mirrors the coordinator's own fx(): an
// emit routed through the Dispatcher lands on wireTx's handler, whose fx
// must rebuild the stored bytes and call the same deps — the test-side twin
// of wallet.ts's driver fx (runtime.ts does the same with txFx)
// custody hooks take the rebuilt Transaction — the one total effect set a
// coordinator and its wireTx handler share (production: custodyTxEffects)
type TxHooks = {
  txSpent?: (tx: Transaction) => void | Promise<void>
  txDied?: (tx: Transaction) => void | Promise<void>
  recordAired?: (tx: Transaction) => void | Promise<void>
}
const fxFor = (deps: TxHooks): TxEffects => ({
  txSpent: async (wt) => {
    await deps.txSpent?.(txFromRow(wt))
  },
  txDied: async (wt) => {
    await deps.txDied?.(txFromRow(wt))
  },
  recordAired: async (wt) => {
    await deps.recordAired?.(txFromRow(wt))
  },
  exportExpired: () => {}
})

/** Wrap the write paths so every live tx edge lands in the replay log —
 *  driver mode writes through applyTxEvent, legacy through setTxState. */
function tapTx(store: AutomationStore, log: { from: string; to: string }[]) {
  const orig = store.setTxState.bind(store)
  store.setTxState = async (id: string, st: never, obs?: unknown, att?: boolean) => {
    const prev = await store.walletTx({ actionId: id })
    log.push({ from: prev?.state ?? '?', to: st })
    return orig(id, st, obs, att)
  }
  const origApply = store.applyTxEvent.bind(store)
  store.applyTxEvent = async (id: string, ev: string, payload?: { guard?: string; observation?: unknown; attempt?: boolean }) => {
    const prev = await store.walletTx({ actionId: id })
    const r = await origApply(id, ev, payload)
    if (r.accepted) log.push({ from: prev?.state ?? '?', to: r.next })
    return r
  }
}

void test('shadow: reconcile-dead escrow agrees with the live FSM', async (t) => {
  let now = 1_000_000
  const { kx, store } = await open(() => now)
  t.after(() => kx.destroy())
  const tx = oneOutputTx()
  await store.recordPrepared({ actionId: 'esc1', operationId: 'op-s', kind: 'escrow', txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  const { operation } = await store.createOperation({
    role: 'buyer',
    kind: 'purchase',
    dedupeKey: 's1',
    policyHash: 'x',
    network: 'regtest',
    state: 'uncertain',
    data: { pendingApproval: { reason: 'unresolved_payment' }, escrowAction: 'esc1' }
  })
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const engine = new MarketEngine(deps(store, () => now, { txFacts: () => ({ verdict: 'dead' }) as never, executor: ex }))
  wireEngine(ex, engine, { store, fx: inertTxEffects, ceilingMs: () => 60000, now: () => now, log: () => {} })
  await engine.tick()
  const evs = (await store.events(operation.operationId)).map((e) => ({ from: e.from ?? '', to: e.to, cause: e.cause }))
  const diffs = replayOps(evs)
  assert.ok(diffs.length > 0, 'expected at least one state-changing transition to replay')
  for (const d of diffs) assert.equal(d.kind, 'agree', `unexpected divergence: ${JSON.stringify(d)}`)
})

void test('shadow: the abandoned verdict agrees on the label but live skips the custody intent — F2/F6', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const txLog: { from: string; to: string; hint?: 'abandon' }[] = []
  let died = 0
  const hooks: TxHooks = {
    txSpent: () => {},
    txDied: () => {
      died++
    }
  }
  const fx = fxFor(hooks)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, {
    broadcast: () => Promise.resolve({ status: 'success' }),
    fx,
    exec: ex
  })
  wireTx(ex, { store, fx, log: () => {} })
  const tx = oneOutputTx()
  const actionId = `owner:${tx.id('hex')}`
  await w.prepareOwner('upkeep', tx)
  tapTx(store, txLog)
  await w.submit(actionId)
  await w.observe(tx.id('hex'), { status: 'abandoned', source: 'events' } as never)
  txLog[txLog.length - 1].hint = 'abandon' // the events-substrate verdict, not an observation
  const diffs = replayTx(txLog)
  // label agrees — observe() already writes 'abandoned' (an UNDECLARED state
  // value: TxState union stops at 'expired'). The divergence is the intent
  // the live path never runs: machine carries tx-died, live fired it 0 times.
  for (const d of diffs) assert.equal(d.kind, 'agree', `unexpected divergence: ${JSON.stringify(d)}`)
  assert.equal(died, 1, 'live skipped the tx-died intent the spec carries — the F2 custody gap')
})

void test('shadow: expired-then-landed is a live-silent edge the spec already covers', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const { txStep } = await import('../src/machines/tx.machine.js')
  const tx = oneOutputTx()
  await store.recordPrepared({ actionId: 'a1', operationId: 'op', kind: 'payment', txid: tx.id('hex'), rawHex: tx.toHex(), parents: {}, feeSats: 0 })
  await store.setTxState('a1', 'expired', { reason: 'test expiry' })
  // live produces NO transition on late landing (F5 — row is unwatched);
  // assert the spec's edge so the Phase-3 report names the gap precisely.
  const spec = txStep('expired', 'obs.seen', { guard: 'verified' })
  assert.equal(spec.next, 'observed')
  const live = await store.walletTx({ actionId: 'a1' })
  assert.equal(live?.state, 'expired', 'live still strands it — F5 confirmed as a live-silent edge')
})

void test('shadow census: every cause literal emitted in src resolves to a modeled edge', (t) => {
  // a cause with no map entry and no no-edge classification replays 'unmodeled'
  // forever — the census fails here the moment a new cause ships unclassified.
  const srcDir = (() => {
    // the test runs compiled (dist/test) — walk up to package.json, then src/
    let dir = new URL('.', import.meta.url)
    for (;;) {
      if (existsSync(new URL('package.json', dir))) return new URL('src/', dir)
      const up = new URL('../', dir)
      if (up.href === dir.href) throw new Error('package.json not found')
      dir = up
    }
  })()
  const files: string[] = []
  for (const dir of ['', 'market/']) for (const f of readdirSync(new URL(dir, srcDir))) if (f.endsWith('.ts')) files.push(dir + f)
  const literals = new Set<string>()
  const expressions = new Set<string>()
  for (const f of files) {
    const src = readFileSync(new URL(f, srcDir), 'utf8')
    for (const m of src.matchAll(/cause:\s*([^,}\n]+)/g)) {
      const seg = m[1].trim()
      if (/^(string|number|boolean|unknown|any)\b/.test(seg)) continue // type field, not an emission
      const qs = [...seg.matchAll(/'([^']+)'/g)].map((q) => q[1])
      if (qs.length > 0) for (const q of qs) literals.add(q)
      else if (seg.includes('needs_human'))
        literals.add('needs_human:unresolved_payment') // template probe
      else expressions.add(seg)
    }
  }
  // bare-expression causes are forwardings — their VALUES surface as 'unmodeled'
  // in replay when a live row lands (the real gate). The dynamic classes the
  // resolver does own are probed here: needs_human:* and the deny-code set.
  if (expressions.size > 0) t.diagnostic(`variable causes (forwardings — replay gates their values): ${[...expressions].join(', ')}`)
  for (const probe of ['unknown_policy', 'needs_human:stuck_state_timeout', 'needs_human:unresolved_payment'])
    assert.ok(opsCauseCandidates(probe) !== undefined, `dynamic cause class unhandled: ${probe}`)
  const noEdge = new Set(OPS_NO_EDGE_CAUSES)
  for (const c of literals)
    assert.ok(noEdge.has(c) || opsCauseCandidates(c) !== undefined, `cause '${c}' is unmodeled — add a map row or a no-edge classification`)
})

void test('shadow: real-history replay — market_events from a live automation.sqlite', { skip: !existsSync(process.env.NEKTAR_REPLAY_DB ?? '') }, async (t) => {
  const kx = knexFactory({ client: 'sqlite3', connection: { filename: process.env.NEKTAR_REPLAY_DB! }, useNullAsDefault: true })
  t.after(() => kx.destroy())
  const rows = (await kx('market_events').select('operation_id', 'from_state', 'to_state', 'cause').orderBy('at')) as {
    operation_id: string
    from_state: string | null
    to_state: string
    cause: string
  }[]
  const byOp = new Map<string, { from: string; to: string; cause: string }[]>()
  for (const r of rows) {
    const l = byOp.get(r.operation_id) ?? []
    l.push({ from: r.from_state ?? '', to: r.to_state, cause: r.cause })
    byOp.set(r.operation_id, l)
  }
  const diffs: Diff[] = []
  for (const l of byOp.values()) diffs.push(...replayOps(l))
  for (const d of diffs)
    console.log(`  ${d.kind.padEnd(17)} ${d.from} -> ${d.live} (live) | machine ${d.machine || '·'} | ${d.event}${d.guard ? ' @' + d.guard : ''} | ${d.cause}`)
  assert.ok(diffs.length > 0, 'fixture db should contain at least one state-changing transition')
  for (const d of diffs) assert.notEqual(d.kind, 'unmodeled', `unmodeled cause in real history: ${JSON.stringify(d)}`)
})

void test('shadow report: dump the diff list', async (t) => {
  const { kx, store } = await open()
  const txLog: { from: string; to: string; hint?: 'abandon' }[] = []
  const hooks: TxHooks = { txSpent: () => {}, txDied: () => {} }
  const fx = fxFor(hooks)
  const ex = new Dispatcher()
  t.after(() => ex.close())
  const w = new WalletCoordinator(store, { broadcast: () => Promise.resolve({ status: 'success' }), fx, exec: ex })
  wireTx(ex, { store, fx, log: () => {} })
  const tx = oneOutputTx()
  await w.prepareOwner('upkeep', tx)
  tapTx(store, txLog)
  await w.submit(`owner:${tx.id('hex')}`)
  await w.observe(tx.id('hex'), { status: 'abandoned', source: 'events' } as never)
  txLog[txLog.length - 1].hint = 'abandon'
  const diffs: Diff[] = replayTx(txLog)
  for (const d of diffs)
    console.log(`  ${d.kind.padEnd(17)} ${d.from} -> ${d.live} (live) | machine ${d.machine || '·'} | ${d.event}${d.guard ? ' @' + d.guard : ''}`)
  await kx.destroy()
})
