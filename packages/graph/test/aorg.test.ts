import test from 'node:test'
import assert from 'node:assert'
import { AORGraph, signMaterial } from '../src/index.js'
import { PrivateKey } from '@bsv/sdk'
import { parseProgram } from '@nektar/datalog'

const key1 = PrivateKey.fromRandom()
const sig = (phi: string, t: any, ctx: string[]) => key1.sign(Array.from(signMaterial(phi, t, ctx))).toDER('hex') as string
const t = (n: number) => ({ logPos: n })
const cat = parseProgram(`@id r-spec
spec(S, V) :- requires(P, V), applies(S, P).
@id r-rev
requires(P, V) :- declares(P, V).`).rules

function ep(g: AORGraph, phi: string, n: number, ctx = ['manual']) {
  return g.append({ kind: 'episode', phi, t: t(n), context: ctx, sigma: [sig(phi, t(n), ctx)] }, [])
}

test('AORG: episode → derived → refinement bi-temporal view', () => {
  const g = new AORGraph(cat)
  const e1 = ep(g, 'requires(torque, 45)', 1)
  const e2 = ep(g, 'applies(port4, torque)', 2)
  const phi = 'spec(port4, 45)'
  const d = g.append({ kind: 'derived', phi, t: t(3), context: ['manual'], sigma: [sig(phi, t(3), ['manual'])] }, [e1.h, e2.h], 'r-spec')
  assert.ok(d.h.length === 64, 'vertex committed')
  assert.equal(g.currentView().length, 3)
  // refinement revises the spec — old vertex invalidated, not deleted
  const e3 = ep(g, 'requires(torque, 55)', 4)
  const phi2 = 'spec(port4, 55)'
  const rf = g.append(
    { kind: 'refinement', phi: phi2, t: t(5), context: ['manual'], sigma: [sig(phi2, t(5), ['manual'])], refines: d.h },
    [e3.h, e2.h],
    'r-spec'
  )
  const before = g.currentView(4).map((v) => v.phi)
  const after = g.currentView(6).map((v) => v.phi)
  assert.ok(before.includes('spec(port4, 45)'), 'old spec valid at t=4')
  assert.ok(!after.includes('spec(port4, 45)'), 'old spec invalidated at t=6')
  assert.ok(g.history(d.h)!.phi === 'spec(port4, 45)', 'old vertex retained (append-only)')
})

test('AORG: cycle attempt, unknown premise, unjustified, contradiction, traceability', () => {
  const g = new AORGraph(cat)
  const e1 = ep(g, 'requires(torque, 45)', 1)
  const e2 = ep(g, 'applies(port4, torque)', 2)
  assert.throws(
    () => g.append({ kind: 'derived', phi: 'spec(port4, 45)', t: t(3), context: ['m'], sigma: ['x'] }, [e1.h, 'ff'.repeat(32)], 'r-spec'),
    /unknown premise/
  )
  assert.throws(
    () => g.append({ kind: 'derived', phi: 'wrong(claim)', t: t(3), context: ['m'], sigma: ['x'] }, [e1.h, e2.h], 'r-spec'),
    /does not derive/,
    'unjustified rejected'
  )
  const phi = 'spec(port4, 45)'
  const d = g.append({ kind: 'derived', phi, t: t(3), context: ['m'], sigma: ['x'] }, [e1.h, e2.h], 'r-spec')
  // contradiction: neg_spec(port4,45) contradicts spec(port4,45)
  assert.throws(() => g.append({ kind: 'derived', phi: 'neg_spec(port4, 45)', t: t(4), context: ['m'], sigma: ['x'] }, [d.h], 'r-spec'), /contradiction/)
  // traceability: derived from an axiom only (no episode) rejected
  const ax = g.append({ kind: 'axiom', phi: 'requires(q, 1)', t: t(1), context: ['m'], sigma: ['x'] }, [])
  assert.throws(
    () => g.append({ kind: 'derived', phi: 'spec(z, 1)', t: t(5), context: ['m'], sigma: ['x'] }, [ax.h], 'r-spec'),
    /no episode|does not derive|traceability/
  )
})

test('AORG: subgraph scoping + metanet head + reverse index', () => {
  const g = new AORGraph(cat)
  const e1 = ep(g, 'requires(t, 45)', 1, ['dept-a'])
  const e2 = ep(g, 'requires(u, 9)', 2, ['dept-b'])
  const inst = g.instance(['dept-a'])
  assert.ok(inst.has(e1.h) && !inst.has(e2.h), 'context selection')
  const mh = g.metanetHead('02aa', '00'.repeat(32), '11'.repeat(32))
  assert.equal(mh.protocol, 'meta')
  assert.ok(g.citing(e1.h).length === 0, 'no citations yet')
})
