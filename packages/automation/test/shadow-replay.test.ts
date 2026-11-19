import { test } from 'node:test'
import assert from 'node:assert/strict'
import knexFactory from 'knex'
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { AutomationStore } from '../src/store.js'
import { WalletCoordinator } from '../src/wallet.js'
import { MarketEngine } from '../src/market.js'
import type { MarketDeps } from '../src/market/types.js'
import { replayOps, replayTx, type Diff } from '../src/machines/shadow.js'

// state-audit Phase 3 — replay recorded live transitions through the shadow
// machines and report every divergence. The machine never moves production
// state; it only answers "what would the spec have done".

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
    call: async () => ({}),
    memory: {} as never,
    key,
    network: 'regtest',
    height: async () => 1,
    fetchTx: async () => undefined,
    transport: {} as never,
    ai: {} as never,
    now,
    ...extra
  } as MarketDeps
}

function oneOutputTx(): Transaction {
  const tx = new Transaction()
  tx.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toHash()) })
  return tx
}

/** Wrap setTxState so every live tx edge lands in the replay log. */
function tapTx(store: AutomationStore, log: { from: string; to: string }[]) {
  const orig = store.setTxState.bind(store)
  store.setTxState = async (id: string, st: never, obs?: unknown, att = false) => {
    const prev = await store.walletTx({ actionId: id })
    log.push({ from: prev?.state ?? '?', to: st })
    return orig(id, st, obs, att)
  }
}

test('shadow: reconcile-dead escrow agrees with the live FSM', async (t) => {
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
  const engine = new MarketEngine(deps(store, () => now, { txFacts: async () => ({ verdict: 'dead' }) as never }))
  await engine.tick()
  const evs = (await store.events(operation.operationId)).map((e) => ({ from: e.from ?? '', to: e.to, cause: e.cause }))
  const diffs = replayOps(evs)
  assert.ok(diffs.length > 0, 'expected at least one state-changing transition to replay')
  for (const d of diffs) assert.equal(d.kind, 'agree', `unexpected divergence: ${JSON.stringify(d)}`)
})

test('shadow: the abandoned verdict agrees on the label but live skips the custody intent — F2/F6', async (t) => {
  const { kx, store } = await open()
  t.after(() => kx.destroy())
  const txLog: { from: string; to: string; hint?: 'abandon' }[] = []
  let died = 0
  const w = new WalletCoordinator(store, {
    broadcast: async () => ({ status: 'success' }),
    txSpent: () => {},
    txDied: () => {
      died++
    }
  })
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

test('shadow: expired-then-landed is a live-silent edge the spec already covers', async (t) => {
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

test('shadow report: dump the diff list', async () => {
  const { kx, store } = await open()
  const txLog: { from: string; to: string; hint?: 'abandon' }[] = []
  const w = new WalletCoordinator(store, { broadcast: async () => ({ status: 'success' }), txSpent: () => {}, txDied: () => {} })
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
