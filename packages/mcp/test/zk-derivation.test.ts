// Wright §14 ZK derivation attestation, verifier side: the committed REAL
// RISC Zero receipt (the gen-vectors 2-step permit path) is verified through
// NektarMemory against an anchored dagroot record on the mock chain.
// NEKTAR_STARK_PROVE=1 additionally proves end-to-end through the instance.
import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { parseProgram } from '@nektar/datalog'
import { PolicyDAG } from '@nektar/graph'
import { encodeZkder } from '@nektar/zk'
import { NektarMemory, type NektarMemoryOptions, type SecurityStateStore } from '../src/index.js'

const fx = JSON.parse(readFileSync(new URL('../../../zk/test/fixtures/zkder-permit.json', import.meta.url), 'utf8'))
const H = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))
const proofBytes = () =>
  encodeZkder({
    imageId: H(fx.imageId),
    policyRoot: H(fx.journal.policyRoot),
    conclusionHash: H(fx.journal.conclusionHash),
    queryId: H(fx.journal.queryId),
    receipt: H(fx.receipt)
  })

// the SAME program the fixture was proven against (gen-vectors-datalog.mjs)
const PROG = `
@id r-allow
allowed(P, F) :- member(P, verified), fragment(F).
@id r-permit
permit(P, F) :- allowed(P, F), cleared(P).
@id r-review
review(P, F) :- permit(P, F), senior(P).
@id r-pair
pair(P) :- member(P, verified), member(P, verified).
`
const dag = () => new PolicyDAG([{ name: 'base', rules: parseProgram(PROG).rules }])

async function world(t: { skip: (m?: string) => void }, opts: NektarMemoryOptions = {}) {
  const w = await stackWorld(t, 4)
  if (w === undefined) return
  const { chain, funder: f } = w
  const mem = new NektarMemory(chain, f, f.keyPair, { batchPolicy: { maxLeaves: 10 }, ...opts })
  return { chain, f, mem }
}

// seed the fixture queryId as an issued (unused) ZK query via securityStore
const seedQuery: SecurityStateStore = {
  load: () => ({ challenges: [], zkSeen: [], zkQueries: [[fx.journal.queryId, Date.now() + 600_000]], revocations: [] }),
  save: () => {}
}

test('STARK derivation verifies against the anchored dagroot record + issued queryId', async (t) => {
  const d = dag()
  const root = Buffer.from(d.policyRoot()).toString('hex')
  assert.equal(root, fx.journal.policyRoot, 'test DAG must reproduce the fixture policy root')
  const _w = await world(t, { policyDag: d, securityStore: seedQuery })
  if (_w === undefined) return
  const { chain, mem } = _w
  await mem.remember('__policy__', `dagroot:${root}`)
  await mem.anchorShard('__policy__')
  await chain.mine(1)
  mem.confirmAnchor('__policy__', mem.tailLink('__policy__')!.txid)
  const r = await mem.verifyDerivation('__policy__', proofBytes())
  assert.ok(r.ok, JSON.stringify(r))
  assert.equal(r.policyRoot, root)
  assert.match(r.statement, /not which conclusion|derivation/)
  // replay: single use
  const replay = await mem.verifyDerivation('__policy__', proofBytes())
  assert.equal(replay.ok, false)
  assert.match(String(replay.reason), /already used|not issued/)
})

test('STARK derivation NEGATIVES: unanchored root, mismatch, unissued query', async t => {
  const d = dag()
  // 1. dagroot record exists but is NOT anchored → no trusted root
  const w1 = await world(t, { policyDag: d, securityStore: seedQuery })
  if (w1 === undefined) return
  await w1.mem.remember('__policy__', `dagroot:${fx.journal.policyRoot}`) // unanchored
  const r1 = await w1.mem.verifyDerivation('__policy__', proofBytes())
  assert.equal(r1.ok, false)
  assert.match(String(r1.reason), /no anchored policy DAG root/)
  // 2. anchoredPolicyRoot configured but ≠ recomputed → deny everything (same rule as recall)
  const w2 = await world(t, { policyDag: d, anchoredPolicyRoot: 'ff'.repeat(32), securityStore: seedQuery })
  if (w2 === undefined) return
  const r2 = await w2.mem.verifyDerivation('__policy__', proofBytes())
  assert.equal(r2.ok, false)
  assert.match(String(r2.reason), /no anchored policy DAG root/)
  // 3. anchored record present, query never issued
  const w3 = await world(t, { policyDag: d })
  if (w3 === undefined) return
  await w3.mem.remember('__policy__', `dagroot:${fx.journal.policyRoot}`)
  await w3.mem.anchorShard('__policy__')
  await w3.chain.mine(1)
  w3.mem.confirmAnchor('__policy__', w3.mem.tailLink('__policy__')!.txid)
  const r3 = await w3.mem.verifyDerivation('__policy__', proofBytes())
  assert.equal(r3.ok, false)
  assert.match(String(r3.reason), /queryId/)
})

test('end-to-end: NektarMemory proves (RISC Zero) and verifies its own derivation', { skip: process.env.NEKTAR_STARK_PROVE !== '1' }, async (t) => {
  const d = dag()
  const _w = await world(t, { policyDag: d })
  if (_w === undefined) return
  const { chain, mem } = _w
  await mem.remember('s', 's1 data')
  await mem.anchorShard('s')
  await chain.mine(1)
  mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const root = Buffer.from(d.policyRoot()).toString('hex')
  await mem.remember('__policy__', `dagroot:${root}`)
  await mem.anchorShard('__policy__')
  await chain.mine(1)
  mem.confirmAnchor('__policy__', mem.tailLink('__policy__')!.txid)
  const q = mem.issueZkQuery('auditor')
  const { bytes } = await mem.proveDerivation(
    's',
    {
      pred: 'permit',
      args: ['a', 's/0'],
      edb: [
        { pred: 'member', args: ['a', 'verified'] },
        { pred: 'cleared', args: ['a'] }
      ]
    },
    q.queryId
  )
  const ok = await mem.verifyDerivation('__policy__', bytes)
  assert.ok(ok.ok, JSON.stringify(ok))
  const replay = await mem.verifyDerivation('__policy__', bytes)
  assert.equal(replay.ok, false, 'replay rejected (query consumed)')
})
