import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { fundsOf } from '@nektar/chain'
import { CustodyOps, paymentFeeBoundSats } from '../src/custody.js'

const key = PrivateKey.fromRandom()
const parent = (): Transaction => {
  const p = new Transaction()
  p.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: UnlockingScript.fromASM('OP_1') })
  p.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  return p
}
const pathFor = (tx: Transaction): MerklePath =>
  new MerklePath(9, [
    [
      { offset: 0, hash: '11'.repeat(32) },
      { offset: 1, hash: tx.id('hex'), txid: true }
    ]
  ])

// proofOf is an optional dep — the cast keeps this file compiling while the
// field is added; a pathless funder tx should pick up the held proof
const ops = (u: { tx: Transaction; vout: number; satoshis: number }, proofOf?: (txid: string) => MerklePath | undefined): CustodyOps =>
  new CustodyOps({
    funds: fundsOf({ utxos: [u], address: () => key.toAddress() }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    proofOf
  })

void test('custody: takeFunding attaches the held proof to a pathless funder tx', async () => {
  const tx = parent()
  const mp = pathFor(tx)
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, (txid) => (txid === tx.id('hex') ? mp : undefined)).takeFunding(1000)
  // a copy, not the store's own object — consumers may extract() on it
  assert.deepEqual(out.tx.merklePath, mp)
})

void test('custody: takeFunding leaves the tx pathless when no proof is held', async () => {
  const tx = parent()
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => undefined).takeFunding(1000)
  assert.equal(out.tx.merklePath, undefined)
})

void test('custody: takeFunding never overwrites a proof the funder already carried', async () => {
  const tx = parent()
  const own = pathFor(tx)
  tx.merklePath = own
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => pathFor(tx)).takeFunding(1000)
  assert.equal(out.tx.merklePath, own)
})

// G1: a candidate whose parent the broadcaster rejected is passed over, and
// ONLY a sealed dead verdict heals — a pushed status word is read-only:
// died() frees live pending inputs, so a hint must never reach it
const reserveOps = (
  candidates: Array<{ tx: Transaction; vout: number; satoshis: number }>,
  facts?: (txid: string) => Promise<import('@nektar/chain').TxFacts>
): { ops: CustodyOps; spent: string[] } => {
  const spent: string[] = []
  const ops = new CustodyOps({
    funds: fundsOf({
      markSpent: (txid: string, vout: number) => spent.push(`${txid}:${vout}`),
      utxos: candidates,
      address: () => key.toAddress()
    }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    proofOf: () => undefined,
    facts
  })
  return { ops, spent }
}

const hintFacts = (status: string): import('@nektar/chain').TxFacts => ({
  verdict: 'pending',
  hint: { status, answered: true }
})

void test('custody: a broadcaster-rejected parent is passed over, never healed', async () => {
  const bad = parent()
  const good = parent()
  // parent() is deterministic — an extra dust output makes the txids differ
  good.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const { ops, spent } = reserveOps(
    [
      { tx: bad, vout: 0, satoshis: 100_000 },
      { tx: good, vout: 0, satoshis: 100_000 }
    ],
    (txid) => Promise.resolve(hintFacts(txid === bad.id('hex') ? 'REJECTED' : 'SEEN_ON_NETWORK'))
  )
  const u = await ops.takeFunding(1000)
  assert.equal(u.tx.id('hex'), good.id('hex'))
  // a pushed status word passes the candidate over — read-only. It must
  // NEVER reach died(): healing frees the refused tx's inputs and
  // tombstones the phantom's outputs, so a hint alone cannot write
  assert.deepEqual(spent, [], 'a hint is not a verdict — nothing writes')
})

void test('custody: a sealed dead verdict heals the phantom parent', async () => {
  const bad = parent()
  const good = parent()
  good.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const { ops, spent } = reserveOps(
    [
      { tx: bad, vout: 0, satoshis: 100_000 },
      { tx: good, vout: 0, satoshis: 100_000 }
    ],
    (txid): Promise<import('@nektar/chain').TxFacts> => Promise.resolve(txid === bad.id('hex') ? { verdict: 'dead' } : hintFacts('SEEN_ON_NETWORK'))
  )
  const u = await ops.takeFunding(1000)
  assert.equal(u.tx.id('hex'), good.id('hex'))
  // a sealed verdict heals: the phantom can never exist on chain, so its
  // output is tombstoned rather than re-served forever
  assert.deepEqual(spent, [`${bad.id('hex')}:0`])
})

void test('custody: a proven candidate is never status-queried', async () => {
  const p = parent()
  p.merklePath = pathFor(p)
  const statuses: string[] = []
  const { ops } = reserveOps([{ tx: p, vout: 0, satoshis: 100_000 }], (txid) => {
    statuses.push(txid)
    return Promise.resolve(hintFacts('REJECTED'))
  })
  const u = await ops.takeFunding(1000)
  assert.equal(u.tx, p)
  assert.deepEqual(statuses, [], 'a proof-carrying parent needs no status hint')
})

void test('custody: a rejection-family hint after submit is a wake, not a verdict — nothing dies', async () => {
  const marks: string[] = []
  const p = parent()
  p.merklePath = pathFor(p)
  const ops = new CustodyOps({
    funds: fundsOf({
      utxos: [{ tx: p, vout: 0, satoshis: 100_000 }],
      markSpent: () => marks.push('markSpent'),
      addOwned: () => marks.push('addOwned'),
      address: () => key.toAddress()
    }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    facts: () => Promise.resolve(hintFacts('REJECTED'))
  })
  const r = await ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 })
  assert.ok(r.txid.length > 0)
  // a pushed status word must never reach died(): the tx stays submitted and
  // its spend is recorded — a later sealed verdict is what heals
  assert.deepEqual(marks, ['markSpent', 'addOwned', 'addOwned'], 'the spend settles, no tombstones')
})

void test('custody: a sealed dead verdict after submit throws and tombstones', async () => {
  const marks: string[] = []
  const p = parent()
  p.merklePath = pathFor(p)
  const ops = new CustodyOps({
    funds: fundsOf({
      utxos: [{ tx: p, vout: 0, satoshis: 100_000 }],
      markSpent: () => marks.push('markSpent'),
      addOwned: () => marks.push('addOwned'),
      address: () => key.toAddress()
    }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    facts: (): Promise<import('@nektar/chain').TxFacts> => Promise.resolve({ verdict: 'dead', hint: { status: 'REJECTED', answered: true } })
  })
  await assert.rejects(
    ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 }),
    /rejected after submit/
  )
  // the dead tx's two owned outputs (payment-to-self + change) are tombstoned
  // so they can never be handed out — but the taken parent was never marked
  // spent; its hold is released back into the pool
  assert.deepEqual(marks, ['markSpent', 'markSpent'], 'the taken input returns to the reserve')
})

// the direct rail's funding take is amount + the payment's actual fee bound —
// a fixed covenant-class reserve starved small payments (a 50-sat deal asked
// for a ≥650-sat utxo). The bound tracks the configured fee rate.
void test('custody: sendSats takes amount + the rate-derived fee bound, not a fixed reserve', async () => {
  const p = parent()
  p.merklePath = pathFor(p)
  let asked = 0
  const base = fundsOf({ utxos: [{ tx: p, vout: 0, satoshis: 100_000 }], markSpent: () => {}, addOwned: () => {}, address: () => key.toAddress() })
  const ops = new CustodyOps({
    funds: {
      ...base,
      take: (purpose: never, sats: number) => {
        asked = sats
        return base.take(purpose, sats)
      }
    },
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    feeRateSatPerKb: 150
  })
  const r = await ops.sendSats({ amount: 50, address: key.toPublicKey().toAddress(), buildOnly: true })
  assert.equal(asked, 50 + paymentFeeBoundSats(150), 'the take asks for amount + the ~450B payment bound at the configured rate')
  assert.equal(r.utxo?.satoshis, 50)
  // the bound really covers the fee this shape pays — not just a number
  const fee = 100_000 - (r.tx?.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0) ?? 0)
  assert.ok(fee > 0 && fee <= paymentFeeBoundSats(150), `fee ${fee} inside the ${paymentFeeBoundSats(150)} bound`)
})

void test('custody: a seen status returns after one read — bounded confirmation, not a wait loop', async () => {
  let reads = 0
  const p = parent()
  p.merklePath = pathFor(p)
  const ops = new CustodyOps({
    funds: fundsOf({
      utxos: [{ tx: p, vout: 0, satoshis: 100_000 }],
      markSpent: () => {},
      addOwned: () => {},
      address: () => key.toAddress()
    }),
    fundSigner: () => key,
    broadcast: () => Promise.resolve({ status: 'success' }),
    txs: () => {},
    facts: () => {
      reads++
      return Promise.resolve(hintFacts('SEEN_ON_NETWORK'))
    }
  })
  await ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 })
  assert.equal(reads, 1, 'first live read settles the check')
})
