// tx-verdict.test.ts — the hint-vs-verdict split in TxOps.txFacts: a pushed
// status string is a wake, never a durable verdict; only first-party
// deliveries (verified chain events, the abandon horizon, a broadcast
// refusal) and the ledger's own rows land terminal state. The outside word
// sits in facts.hint — it can inform 'seen' but can never become a verdict.
// Unit-level — no chain stack needed.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, UnlockingScript } from '@bsv/sdk'
import { TxStore, hintSeen, type TxStatusEvent } from '@nektar/chain'
import { TxOps, watcherObservation, type TxOpsDeps } from '../src/txops.js'

const mk = (over: Partial<TxOpsDeps> = {}) => {
  const store = new TxStore()
  const deps: TxOpsDeps = {
    txs: () => store,
    anchorTxs: new Map(),
    attestationTxs: new Map(),
    txidShard: new Map(),
    attachProof: () => {},
    chain: {},
    sdkTracker: () => ({}) as never,
    txLookup: () => undefined,
    proofLookup: () => undefined,
    ...over
  }
  return { ops: new TxOps(deps), store }
}

const mkTx = (): Transaction => {
  const parent = new Transaction()
  parent.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock('1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa') })
  const t = new Transaction()
  t.addInput({ sourceTXID: parent.id('hex'), sourceOutputIndex: 0, unlockingScript: new UnlockingScript(), sequence: 0xffffffff })
  t.addOutput({ satoshis: 900, lockingScript: parent.outputs[0].lockingScript })
  return t
}

test('tx-verdict: a rejection HINT wakes subscribers but writes no verdict', async () => {
  const { ops } = mk({ statusHint: async () => ({ status: 'REJECTED', answered: true }) })
  let fires = 0
  ops.onTxRejected(() => {
    fires++
  })
  ops.hintTxRejected('t1')
  assert.equal(fires, 1, 'the hint wakes the engine reconcile')
  const f = await ops.txFacts('t1')
  assert.equal(f.verdict, 'pending', 'a status string never lands a durable verdict')
  assert.equal(f.hint?.status, 'REJECTED', 'the outside word stays visible in the hint field')
})

test('tx-verdict: first-party rejection is durable — and a later proof still wins', async () => {
  const { ops, store } = mk()
  const tx = mkTx()
  const txid = tx.id('hex')
  store.put(tx)
  ops.deliverTxRejected(txid)
  assert.equal((await ops.txFacts(txid)).verdict, 'dead')
  assert.equal((await ops.txFacts(txid)).source, 'first-party')
  ops.deliverTxProof(txid, { blockHeight: 7 } as never)
  assert.equal((await ops.txFacts(txid)).verdict, 'mined', 'a merkle path in hand outranks a recorded verdict')
  assert.equal((await ops.txFacts(txid)).verdict, 'mined', 'the verdict set was cleared — no flip back')
  assert.equal((await ops.txFacts(txid)).source, 'proof')
})

test('tx-verdict: abandon + ledger rows are terminal; a stored tx alone is not "seen"', async () => {
  const { ops } = mk()
  let fires = 0
  ops.onTxRejected(() => {
    fires++
  })
  ops.deliverTxAbandoned('t1')
  assert.equal(fires, 1, 'the abandon verdict wakes the engine')
  const f1 = await ops.txFacts('t1')
  assert.equal(f1.verdict, 'dead', 'the abandon horizon IS a terminal verdict')
  assert.equal(f1.source, 'abandon')

  const { ops: ops2 } = mk({ ledgerState: async () => 'conflicted' })
  const f2 = await ops2.txFacts('t2')
  assert.equal(f2.verdict, 'dead', "ledger 'conflicted' is first-party truth")
  assert.equal(f2.source, 'first-party')

  let status = ''
  const { ops: ops3, store: store3 } = mk({
    ledgerState: async () => 'submitted',
    statusHint: async () => ({ status, answered: true })
  })
  const tx3 = mkTx()
  store3.put(tx3) // prepared-but-unbroadcast sits in the graph
  const txid3 = tx3.id('hex')
  const f3 = await ops3.txFacts(txid3)
  assert.equal(f3.verdict, 'pending')
  assert.equal(f3.tx, undefined, 'the tx store alone must never answer "held"')
  assert.ok(!hintSeen(f3.hint), 'an empty status word is not "seen"')
  status = 'SEEN_ON_NETWORK'
  const f4 = await ops3.txFacts(txid3)
  assert.equal(f4.verdict, 'pending')
  assert.ok(hintSeen(f4.hint), 'a seen-family status word is the "seen" hint')
})

test('tx-verdict: a pull-found proof attaches to the anchor OBJECT, not just the store — push and pull share one seam', async () => {
  // regression: fetchTx used to write pull-found proofs to txs() + the
  // returned copy only — anchorTxs (the object unprovenAnchors /
  // verifyMembership / timeBounds read) stayed unproven forever even with
  // the proof in hand. Both proof sources must route through
  // deliverTxProof → attachProof like the push path does.
  for (const source of ['chain.getTx', 'proofLookup'] as const) {
    const anchorTx = mkTx()
    const txid = anchorTx.id('hex')
    const mp = { blockHeight: 7 } as never as import('@bsv/sdk').MerklePath
    const attachCalls: string[] = []
    const provenCopy = mkTx()
    provenCopy.merklePath = mp
    const { ops } = mk({
      anchorTxs: new Map([[txid, anchorTx]]),
      txidShard: new Map([[txid, 's']]),
      attachProof: (_s, t, m) => {
        attachCalls.push(t)
        anchorTx.merklePath = m as never // the real attachProof's write
      },
      chain: source === 'chain.getTx' ? { getTx: async () => provenCopy } : {},
      proofLookup: () => (source === 'proofLookup' ? async () => mp : undefined)
    })
    const got = await ops.fetchTx(txid)
    assert.deepEqual(attachCalls, [txid], `${source}: the proof must go through attachProof — the anchor's own object is the read site`)
    assert.equal(anchorTx.merklePath, mp, `${source}: the anchor object carries the proof — no divergent representation`)
    assert.equal(got?.merklePath, mp)
  }
})

test('tx-verdict: watcherObservation — only verified terminal records land durable verdicts', () => {
  const ev = (status: TxStatusEvent['status'], verified: boolean) => ({ ev: { txid: 't', status }, verified })
  assert.equal(watcherObservation(ev('rejected', false), 'SEEN_ON_NETWORK'), undefined, 'a hinted rejection is never durable')
  assert.deepEqual(watcherObservation(ev('rejected', true), ''), { status: 'rejected', source: 'chain', reason: 'rejected' })
  assert.deepEqual(watcherObservation(ev('double-spent', true), ''), { status: 'rejected', source: 'chain', reason: 'double-spent' })
  assert.deepEqual(watcherObservation(ev('mined', true), ''), { status: 'proven', source: 'broadcaster' })
  assert.equal(watcherObservation(undefined, 'REJECTED'), undefined, 'a REJECTED status string is a hint, not a verdict')
  assert.equal(watcherObservation(undefined, 'DOUBLE_SPEND_ATTEMPTED'), undefined)
  assert.deepEqual(watcherObservation(undefined, 'SEEN_ON_NETWORK'), { status: 'seen', source: 'broadcaster' })
  assert.deepEqual(
    watcherObservation(ev('mined', false), 'MINED'),
    { status: 'seen', source: 'broadcaster' },
    'a hinted MINED is still only seen — no proof in hand'
  )
})

// spv/reimport-spent gate: a first-party spend record blocks re-import
// while the spender is live or unrecorded; rejected/conflicted frees it
test('importable: live spenders block re-import, dead spenders reopen it', async () => {
  const { importable } = (await import('../src/txops.js')) as {
    importable?: (spender: string | undefined, spenderLedgerState: string | undefined) => boolean
  }
  assert.ok(importable !== undefined, 'txops must export importable')
  const cases: Array<[string | undefined, string | undefined, boolean]> = [
    [undefined, undefined, true],
    [undefined, 'submitted', true],
    ['aa'.repeat(32), 'submitted', false],
    ['aa'.repeat(32), 'confirmed', false],
    ['aa'.repeat(32), 'observed', false],
    ['aa'.repeat(32), undefined, false],
    ['aa'.repeat(32), 'rejected', true],
    ['aa'.repeat(32), 'conflicted', true]
  ]
  for (const [spender, st, want] of cases) assert.equal(importable(spender, st), want, 'spender=' + spender + ' state=' + st)
})

// the legacy reserve reconcile was removed with the file reserve — refresh
// now asks the wallet's spendable view; `importable` is the re-import gate
// that survives: a live first-party spender blocks re-import
