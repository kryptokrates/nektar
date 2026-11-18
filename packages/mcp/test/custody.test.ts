import { test } from 'node:test'
import assert from 'node:assert/strict'
import { MerklePath, PrivateKey, P2PKH, Transaction, UnlockingScript } from '@bsv/sdk'
import { fundsOf } from '@nektar/chain'
import { CustodyOps, type CustodyOpsDeps } from '../src/custody.js'

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
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    proofOf
  } as CustodyOpsDeps)

test('custody: takeFunding attaches the held proof to a pathless funder tx', async () => {
  const tx = parent()
  const mp = pathFor(tx)
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, (txid) => (txid === tx.id('hex') ? mp : undefined)).takeFunding(1000)
  // a copy, not the store's own object — consumers may extract() on it
  assert.deepEqual(out.tx.merklePath, mp)
})

test('custody: takeFunding leaves the tx pathless when no proof is held', async () => {
  const tx = parent()
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => undefined).takeFunding(1000)
  assert.equal(out.tx.merklePath, undefined)
})

test('custody: takeFunding never overwrites a proof the funder already carried', async () => {
  const tx = parent()
  const own = pathFor(tx)
  tx.merklePath = own
  const out = await ops({ tx, vout: 0, satoshis: 100_000 }, () => pathFor(tx)).takeFunding(1000)
  assert.equal(out.tx.merklePath, own)
})

// G1: a candidate whose parent the broadcaster rejected is passed over and
// healed — a status hint is never written to custody state, but died()
// tombstones the phantom output and frees the refused tx's inputs
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
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    proofOf: () => undefined,
    facts
  } as CustodyOpsDeps)
  return { ops, spent }
}

const hintFacts = (status: string): import('@nektar/chain').TxFacts => ({
  verdict: 'pending',
  hint: { status, answered: true }
})

test('custody: takeFunding skips a broadcaster-rejected parent and heals its phantom', async () => {
  const bad = parent()
  const good = parent()
  // parent() is deterministic — an extra dust output makes the txids differ
  good.addOutput({ satoshis: 1, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  const statuses: string[] = []
  const { ops, spent } = reserveOps(
    [
      { tx: bad, vout: 0, satoshis: 100_000 },
      { tx: good, vout: 0, satoshis: 100_000 }
    ],
    async (txid) => {
      statuses.push(txid)
      return hintFacts(txid === bad.id('hex') ? 'REJECTED' : 'SEEN_ON_NETWORK')
    }
  )
  const u = await ops.takeFunding(1000)
  assert.equal(u.tx.id('hex'), good.id('hex'))
  assert.deepEqual(statuses, [bad.id('hex'), good.id('hex')], 'one refusal probe per candidate')
  // a refusal-class parent heals by death: its output can never exist on
  // chain, so the phantom is tombstoned rather than re-served forever
  assert.deepEqual(spent, [`${bad.id('hex')}:0`])
})

test('custody: a proven candidate is never status-queried', async () => {
  const p = parent()
  p.merklePath = pathFor(p)
  const statuses: string[] = []
  const { ops } = reserveOps([{ tx: p, vout: 0, satoshis: 100_000 }], async (txid) => {
    statuses.push(txid)
    return hintFacts('REJECTED')
  })
  const u = await ops.takeFunding(1000)
  assert.equal(u.tx, p)
  assert.deepEqual(statuses, [], 'a proof-carrying parent needs no status hint')
})

test('custody: provisionPay throws when the broadcaster rejects right after submit', async () => {
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
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    facts: async () => hintFacts('REJECTED')
  } as CustodyOpsDeps)
  await assert.rejects(
    ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 }),
    /rejected after submit/
  )
  // rejection is a death: the dead tx's two owned outputs (payment-to-self +
  // change) are tombstoned so they can never be handed out — but the taken
  // parent was never marked spent; its hold is released back into the pool
  assert.deepEqual(marks, ['markSpent', 'markSpent'], 'the taken input returns to the reserve')
})

test('custody: a seen status returns after one read — bounded confirmation, not a wait loop', async () => {
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
    broadcast: async () => ({ status: 'success' }),
    txs: () => {},
    facts: async () => {
      reads++
      return hintFacts('SEEN_ON_NETWORK')
    }
  } as CustodyOpsDeps)
  await ops.provisionPay({ outputs: [{ satoshis: 1, scriptHex: '51' }], address: key.toPublicKey().toAddress(), fundsSats: 1_000 })
  assert.equal(reads, 1, 'first live read settles the check')
})
