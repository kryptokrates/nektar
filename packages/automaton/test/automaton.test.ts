import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { utf8, dsha256, toHex } from '@nektar/core'
import { stackWorld } from '@nektar/chain'
import {
  Automaton,
  AutomatonError,
  AutomatonChain,
  decodeNektarAutomaton,
  deltaTable,
  deltaPath,
  leafEntry,
  verifyTrace,
  reconcile,
  canonicalDef,
  defHash,
  type AutomatonDef
} from '../src/index.js'

// A →(a)→ B →(b)→ C(final). Small deterministic FSM.
const DEF: AutomatonDef = {
  Q: ['A', 'B', 'C'],
  Sigma: ['a', 'b'],
  delta: [
    [0, 0, 1],
    [1, 1, 2]
  ],
  q0: 0,
  F: [2],
  O: ['ok']
}
const TBL = deltaTable(DEF)
const OUT = toHex(dsha256(utf8('out')))

// production path: NektarProvider over the regtest stack with real node script
// evaluation — the chain is the oracle; owner key ≠ funding key.
async function deployed(t: { skip: (m?: string) => void }, def = DEF) {
  const w = await stackWorld(t, 24)
  if (w === undefined) return undefined
  const { chain, funder } = w
  const ac = new AutomatonChain(def, PrivateKey.fromRandom(), funder, funder.keyPair, chain)
  await ac.deploy()
  return ac
}
const SCRIPT_INVALID = /SCRIPT_INVALID/

test('AUT-005: deploy + valid steps accepted by script evaluation; head decoded from chain', async (t) => {
  const ac = await deployed(t)
  if (ac === undefined) return
  assert.equal(ac.getHead()!.state.q, 0)
  const h1 = await ac.step(0, 1, OUT)
  assert.deepEqual([h1.state.q, h1.state.n, h1.state.lastOut], [1, 1, OUT])
  const h2 = await ac.step(1, 2, OUT)
  assert.deepEqual([h2.state.q, h2.state.n], [2, 2])
  assert.ok(decodeNektarAutomaton(h2.tx.outputs[0]!.lockingScript.toHex()), 'continuation authenticates')
  const sh = h2.tx.outputs[0]!.lockingScript.toHex()
  const flipped = sh.slice(0, 40) + (sh.slice(40, 42) === '00' ? '01' : '00') + sh.slice(42)
  assert.equal(decodeNektarAutomaton(flipped), null, 'tampered contract code rejected by decoder')
})

test('AUT-005 negatives: wrong from / not-in-δ / tampered path / wrong signer rejected by script', async (t) => {
  const ac = await deployed(t)
  if (ac === undefined) return
  const p = deltaPath(TBL, 1, 1, 2)
  await assert.rejects(() => ac.step(1, 2, OUT, { from: 1, sibs: p.sibs.map(toHex), dir: p.dir }), SCRIPT_INVALID, 'from != q')
  const q = deltaPath(TBL, 0, 0, 1)
  await assert.rejects(() => ac.step(1, 2, OUT, { from: 0, sibs: q.sibs.map(toHex), dir: q.dir }), SCRIPT_INVALID, '(A,b,C) not in δ')
  const sw = q.sibs.map(toHex)
  ;[sw[0], sw[1]] = [sw[1]!, sw[0]!]
  await assert.rejects(() => ac.step(0, 1, OUT, { sibs: sw, dir: q.dir }), SCRIPT_INVALID, 'tampered path')
  await assert.rejects(() => ac.step(0, 1, OUT, { pubKey: PrivateKey.fromRandom().toPublicKey().toString() }), SCRIPT_INVALID, 'wrong signer')
  await assert.rejects(() => ac.step(0, 1, '00'), SCRIPT_INVALID, 'outHash must be 32 bytes')
  assert.equal(ac.getHead()!.state.n, 0, 'rejected steps never advance the head')
  assert.throws(() => deltaPath(TBL, 0, 1, 2), AutomatonError, 'off-δ path refused locally')
})

test('AUT-005: wrong deltaRoot — a path for another δ is rejected', async (t) => {
  const ac = await deployed(t)
  if (ac === undefined) return
  const other = deltaTable({
    ...DEF,
    delta: [
      [0, 0, 1],
      [1, 1, 0]
    ]
  })
  const p = deltaPath(other, 0, 0, 1)
  await assert.rejects(() => ac.step(0, 1, OUT, { sibs: p.sibs.map(toHex), dir: p.dir }), SCRIPT_INVALID)
})

test('AUT-005: halt from non-final state fails; from final passes', async (t) => {
  const ac = await deployed(t)
  if (ac === undefined) return
  await assert.rejects(() => ac.halt(), /not final — halt refused locally/, 'halt from q=A (local)')
  await assert.rejects(() => ac.halt({ asState: 2 }), SCRIPT_INVALID, 'q=A with the F-path of C (contract)')
  await ac.step(0, 1, OUT)
  await ac.step(1, 2, OUT)
  const h = await ac.halt()
  assert.deepEqual([h.state.q, h.state.n, h.state.halted], [2, 3, true])
  await assert.rejects(() => ac.halt(), AutomatonError, 'halted: refused locally')
  ac.unguarded = true
  await assert.rejects(() => ac.halt(), SCRIPT_INVALID, 'halted: contract refuses a second halt')
  await assert.rejects(() => ac.step(0, 1, OUT, { from: 2, ...pathOf(2, 0, 0) }), /SCRIPT_INVALID|not in δ/, 'halted: no step')
})
const pathOf = async (f: number, s: number, t: number) => {
  const d = deltaTable({ ...DEF, delta: [...DEF.delta, [f, s, t]] })
  const p = deltaPath(d, f, s, t)
  return { sibs: p.sibs.map(toHex), dir: p.dir }
}

test('AUT-005: step reads the CURRENT on-chain q (not the deploy-time value)', async (t) => {
  // regression: a runar terminal method reads mutable fields from constructor
  // slots; every method must continue state so q comes from the preimage
  const def: AutomatonDef = {
    Q: ['A', 'B', 'C'],
    Sigma: ['a', 'b'],
    delta: [
      [0, 0, 1],
      [1, 1, 2],
      [2, 0, 0]
    ],
    q0: 0,
    F: [0],
    O: []
  }
  const ac = await deployed(t, def)
  if (ac === undefined) return
  await ac.step(0, 1, OUT)
  ac.unguarded = true
  await assert.rejects(() => ac.halt({ asState: 0 }), SCRIPT_INVALID, 'q=B is not final even though q0=A is')
  await ac.step(1, 2, OUT)
  await ac.step(0, 0, OUT)
  assert.equal((await ac.halt()).state.halted, true, 'back in A (final) → halt accepted')
})

test('AUT-001: canonical def + deltaRoot + determinism', () => {
  assert.equal(canonicalDef(DEF), canonicalDef({ ...DEF, Q: [...DEF.Q] }), 'canonical def stable')
  assert.equal(defHash(DEF).length, 32)
  assert.throws(
    () =>
      deltaTable({
        ...DEF,
        delta: [
          [0, 0, 1],
          [0, 0, 2]
        ]
      }),
    AutomatonError,
    'non-deterministic δ rejected'
  )
  assert.throws(
    () => deltaTable({ ...DEF, delta: Array.from({ length: 17 }, (_, i) => [i % 3, i % 2, (i + 1) % 3]) }),
    AutomatonError,
    '>16 transitions rejected'
  )
  assert.equal(leafEntry(0, 0, 1).length, 24, 'leaf entry is 24 bytes')
})

test('AUT-002/003/004: light-mode step, trace verify, acceptance', async () => {
  const a = new Automaton(DEF)
  assert.equal(await a.step(0).then((e) => e.to), 1)
  await assert.rejects(() => a.step(0), AutomatonError, 'no δ(B,a) — rejected locally')
  assert.equal(await a.step(1).then((e) => e.to), 2)
  const events = a.events.map((e) => ({ ...e, anchorTxid: 'aa'.repeat(32) }))
  const vr = verifyTrace(DEF, events, () => true)
  assert.ok(vr.ok, 'chained trace verifies')
  const broken = [
    { ...events[0]!, anchorTxid: 'x' },
    { ...events[1]!, from: 0, anchorTxid: 'x' }
  ]
  assert.ok(!verifyTrace(DEF, broken, () => true).ok, 'broken chain flagged')
  assert.ok(!verifyTrace(DEF, events, (ev) => ev.from !== 0).ok, 'unproven leaf flagged')
  assert.ok(new Automaton(DEF).accepts([0, 1]), 'word ab accepted')
  assert.ok(!new Automaton(DEF).accepts([0]), 'word a not accepted')
  assert.ok(!new Automaton(DEF).accepts([1]), 'word b not accepted')
})

test('AUT-008: reorg mirror reconcile', () => {
  const live = { q: 2, n: 2, headTxid: 'live1' }
  assert.equal(reconcile({ q: 2, n: 2, headTxid: 'live1' }, live).action, 'in-sync')
  assert.equal(reconcile({ q: 1, n: 1, headTxid: 'old' }, live).action, 'rollback')
  assert.equal(reconcile({ q: 1, n: 1 }, { q: 0, n: 0 }).action, 'rebroadcast')
})
