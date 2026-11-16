// tx-verdict.test.ts — the hint-vs-verdict split in TxOps: a pushed status
// string is a wake, never a durable verdict; only first-party deliveries
// (verified chain events, the abandon horizon, a broadcast refusal) and the
// ledger's own rows land terminal state. Unit-level — no chain stack needed.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, P2PKH, UnlockingScript } from '@bsv/sdk'
import { TxStore, type TxStatusEvent } from '@nektar/chain'
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
  const { ops } = mk({ network: 'testnet', txStatus: async () => 'REJECTED' })
  let fires = 0
  ops.onTxRejected(() => {
    fires++
  })
  ops.hintTxRejected('t1')
  assert.equal(fires, 1, 'the hint wakes the engine reconcile')
  assert.equal(await ops.txState('t1'), 'unknown', 'a status string never lands a durable verdict')
})

test('tx-verdict: first-party rejection is durable — and a later proof still wins', async () => {
  const { ops, store } = mk({ network: 'testnet' })
  const tx = mkTx()
  const txid = tx.id('hex')
  store.put(tx)
  ops.deliverTxRejected(txid)
  assert.equal(await ops.txState(txid), 'rejected')
  ops.deliverTxProof(txid, { blockHeight: 7 } as never)
  assert.equal(await ops.txState(txid), 'mined', 'a merkle path in hand outranks a recorded verdict')
  assert.equal(await ops.txState(txid), 'mined', 'the verdict set was cleared — no flip back')
})

test('tx-verdict: abandon + ledger rows are terminal; a stored tx alone is not "seen"', async () => {
  const { ops } = mk({ network: 'testnet' })
  let fires = 0
  ops.onTxRejected(() => {
    fires++
  })
  ops.deliverTxAbandoned('t1')
  assert.equal(fires, 1, 'the abandon verdict wakes the engine')
  assert.equal(await ops.txState('t1'), 'rejected', 'the abandon horizon IS a terminal verdict')

  const { ops: ops2 } = mk({ network: 'testnet', ledgerState: async () => 'conflicted' })
  assert.equal(await ops2.txState('t2'), 'rejected', "ledger 'conflicted' is first-party truth")

  let status = ''
  const { ops: ops3, store: store3 } = mk({ network: 'testnet', ledgerState: async () => 'submitted', txStatus: async () => status })
  const tx3 = mkTx()
  store3.put(tx3) // prepared-but-unbroadcast sits in the graph
  const txid3 = tx3.id('hex')
  assert.equal(await ops3.txState(txid3), 'unknown', 'the tx store alone must never answer "seen"')
  status = 'SEEN_ON_NETWORK'
  assert.equal(await ops3.txState(txid3), 'seen')
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
