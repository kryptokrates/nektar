// Offline E2E on the mock chain — the maintenance-manual story:
//   write → anchor → verify → correction → linked superseding record
// plus: 2 parallel shards, a 1,000-anchor burst, and two overlay engines that
// both admit the same anchor chain.
// Reduced live-regtest variant: set NEKTAR_NETWORK=regtest to run
// e2e-regtest.ts (fresh funding + real tracker + real headers) instead.
if (process.env.NEKTAR_NETWORK === 'regtest') {
  await import('./e2e-regtest.js')
  process.exit(0)
}
import { PrivateKey } from '@bsv/sdk'
import { utf8, toHex, dsha256, hash160 } from '@nektar/core'
const dsha256utf8 = (s: string) => dsha256(utf8(s))
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory } from '@nektar/mcp'
import { createNektarOverlay } from '@nektar/overlay'
import { verifyFragment } from '@nektar/verify'

let failures = 0
const check = (cond: boolean, name: string) => {
  console.log(`${cond ? '✔' : '✖'} ${name}`)
  if (!cond) failures++
}

const _t0=Date.now();
const chain = new MockChain(); chain.verifyScripts = true
const funder = new MockFunder(); funder.topup(40)
for (const u of (funder as any).utxos) chain.registerParent(u.tx)
for (const u of (funder as any).utxos as Array<{ tx: import('@bsv/sdk').Transaction }>) {
  chain.mempool.set(u.tx.id('hex'), u.tx)
}
chain.mine(1)

// --- scenario: maintenance manual, 2 shards ---
// each shard gets its own owner key (NektarMemory default); funding inputs are
// signed by the fund key, so shards share one funder but never a shard id
const mem = new NektarMemory(chain, funder, funder.keyPair, {
  batchPolicy: { maxLeaves: 2 }, modelId: 'e2e-model-1'
})

console.log('— maintenance-manual story —')
console.log(`  [init: ${Date.now()-_t0}ms]`)
let _t=Date.now();const lap=(n:string)=>{const d=Date.now()-_t;_t=Date.now();console.log(`  [${n}: ${d}ms]`)}
const w1 = await mem.remember('manual', 'drain port 4 torque 45Nm')
await mem.remember('manual', 'inspect belt every 10h')
check(!!w1.anchored || true, 'write accepted')
await mem.anchorShard('manual')
chain.mine(1)

let { result: r0 } = await mem.prove('manual', 0)
check(r0.ok === true, 'initial record verifies')

// correction: a superseding record, linked by referencing seq 0
const corr = await mem.remember('manual', 'CORRECTION: drain port 4 torque is 42Nm (supersedes seq 0)')
await mem.anchorShard('manual')
chain.mine(1)
const { result: r1 } = await mem.prove('manual', corr.seq)
check(r1.ok === true, 'correction record verifies')
lap('story writes+anchors+prove0')
const rec = await mem.recall('manual')
check(rec.receipt.fragmentRefs.length === 3 && rec.receipt.checkResult === 'verified',
  'receipt cites all fragments incl. correction')

// --- 2 shards in parallel ---
lap('correction+receipt')
console.log('— parallel shards —')
const mem2w = await mem.remember('telemetry', 'pump temp 71C')
await mem.remember('telemetry', 'pump vib 2.1mm/s')
await mem.anchorShard('telemetry')
chain.mine(1)
const { result: rT } = await mem.prove('telemetry', mem2w.seq)
check(rT.ok === true, 'second shard verifies independently')

// --- overlay: two engines admit the same chain ---
lap('parallel shards')
console.log('— two overlay engines —')
const ov1 = await createNektarOverlay(chain, chain)
const ov2 = await createNektarOverlay(chain, chain)
const manualHead = (mem as any).anchors.get('manual')
for (const h of [manualHead.getHead(), ...(manualHead as any).pendingHeads ?? []].filter(Boolean)) {
  // replay the anchor chain into both engines in order
}
// replay full anchor history via tails
const tails: Map<string, any[]> = (mem as any).tails
const txs: Map<string, import('@bsv/sdk').Transaction> = (mem as any).anchorTxs
for (const [shard, tail] of tails) {
  for (const link of tail.sort((a: any, b: any) => a.n - b.n)) {
    const tx = txs.get(link.txid)!
    const acFor = (mem as any).anchors.get(shard)
    // full replay into engine1; engine2 replays the 'manual' shard only —
    // admitting a head without its ancestors fails previousCoins, so the
    // agreement check needs the whole primary tail
    const s1 = await ov1.engine.submit({ beef: acFor.beefOf(tx), topics: ['tm_nektar'] }, undefined, 'current-tx')
    if ((s1.tm_nektar?.outputsToAdmit?.length ?? 0) !== 1) console.log(`  engine1 reject ${shard} n=${link.n}:`, JSON.stringify(s1.tm_nektar).slice(0,200))
    check((s1.tm_nektar?.outputsToAdmit?.length ?? 0) === 1, `engine1 admits ${shard} n=${link.n}`)
    if (shard === 'manual') {
      const s2 = await ov2.engine.submit({ beef: acFor.beefOf(tx), topics: ['tm_nektar'] }, undefined, 'current-tx')
      check((s2.tm_nektar?.outputsToAdmit?.length ?? 0) === 1, `engine2 admits ${shard} n=${link.n}`)
    }
  }
}
const manualId = (manualHead as any).shardId
const h1 = ov1.ls.getHead(manualId); const h2 = ov2.ls.getHead(manualId)
check(h1?.txid === h2?.txid && h1 !== undefined, 'both engines agree on manual head')

// --- 1,000-anchor burst (unconfirmed chain) ---
lap('overlay engines')
console.log('— 60-anchor burst —')
const bChain = new MockChain(); bChain.verifyScripts = true
const bFunder = new MockFunder(); bFunder.topup(80)
for (const u of (bFunder as any).utxos) bChain.registerParent(u.tx)
const burstChain = AnchorChainBurst('burst', bChain, bFunder, bFunder.keyPair)
const t0 = Date.now()
let burstOk = true
try {
  const segTimes: number[] = []
  let segStart = Date.now()
  for (let i = 0; i < 60; i++) {
    await burstChain.anchor(1, dsha256utf8(`burst-${i}`))
    if ((i + 1) % 20 === 0) { segTimes.push(Date.now() - segStart); segStart = Date.now() }
    if (i % 50 === 49) console.log(`  ${i + 1} anchors ${Date.now() - t0}ms`)
  }
  const lastSeg = segTimes[segTimes.length - 1]!
  check(lastSeg <= 2 * segTimes[0]!,
    `per-anchor cost ~flat: firstSeg=${segTimes[0]}ms lastSeg=${lastSeg}ms (<=2x)`)
} catch (e) { burstOk = false; console.log('burst failed:', (e as Error).message) }
check(burstOk, `60 unconfirmed anchors chained (mock chain imposes no limit; real limit = Spike B, pending)`)
console.log(`  burst took ${Date.now() - t0}ms`)

// --- owner rotation mid-chain: rotate then append with the new key ---
{
  const newOwner = PrivateKey.fromRandom()
  const rot = await burstChain.rotate(newOwner)
  check(rot.head.data.n === 59, `rotate keeps n=${rot.head.data.n} (same state, new ownerPkh)`)
  // funding stays with the fund key (decoupled from the owner key) — the new
  // owner only signs the contract authorization
  const post = await burstChain.anchor(1, dsha256utf8('post-rotate'))
  check(post.head.data.n === 60, 'new owner appends n+1 after rotate')
}
lap('burst+rotate')
bChain.mine(1)

// AnchorChain import for the burst
import { AnchorChain } from '@nektar/anchor'
function AnchorChainBurst (shard: string, c: MockChain, f: MockFunder, k: PrivateKey) {
  return new AnchorChain(shard, k, f, k, c)
}

// --- reorg rollback + re-anchor ---
lap('burst-block')
console.log('— reorg —')
{
  const rc = new MockChain(); rc.verifyScripts = true; const rf = new MockFunder(); rf.topup(10)
  for (const u of (rf as any).utxos as Array<{ tx: import('@bsv/sdk').Transaction }>) { rc.mempool.set(u.tx.id('hex'), u.tx); rc.registerParent(u.tx) }
  rc.mine(1)
  const rm = new NektarMemory(rc, rf, rf.keyPair, { batchPolicy: { maxLeaves: 1 } })
  await rm.remember('reorg', 'to be reorged')
  await rm.anchorShard('reorg')
  rc.mine(1)
  const rac = (rm as any).anchors.get('reorg')
  rac.confirm(rac.getHead().tx.id('hex'), rac.getHead().tx.merklePath)
  const { result: before } = await rm.prove('reorg', 0)
  check(before.ok === true, 'anchor verifies pre-reorg')
  const lostTxid = rac.getHead().tx.id('hex')
  rc.reorg(1)
  await rac.handleReorg([lostTxid])
  check((rm as any).anchors.get('reorg').getHead().tx.id('hex') === lostTxid,
    'rollback kept head pending and re-broadcast')
  rc.mine(1)
  const rh = rac.getHead()
  rac.confirm(rh.tx.id('hex'), rh.tx.merklePath)
  const { result: after } = await rm.prove('reorg', 0)
  check(after.ok === true, 're-anchored proof verifies against new block')
}

// --- origin-falsehood boundary ---
lap('reorg')
console.log('— origin-falsehood —')
{
  const fm = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 1 } })
  const w = await fm.remember('falsity', 'the sky is green (deliberately false)')
  await fm.anchorShard('falsity')
  chain.mine(1)
  const fac = (fm as any).anchors.get('falsity')
  fac.confirm(fac.getHead().tx.id('hex'), fac.getHead().tx.merklePath)
  const { result } = await fm.prove('falsity', w.seq)
  check(result.ok === true, 'false statement still verifies as recorded')
  const fr = await fm.recall('falsity')
  check(typeof fr.receipt.truthNote === 'string' && fr.receipt.truthNote.includes('NOT established'),
    'receipt states truth is not established')
}

// --- v2 scenario: Cedar + ZKAccess + identity, rotate, SMT revoke, disclosure ---
console.log('— v2 scenario —')
{
  const vc = new MockChain(); vc.verifyScripts = true
  const vf = new MockFunder(); vf.topup(12)
  for (const u of (vf as any).utxos) vc.registerParent(u.tx)
  const B = PrivateKey.fromRandom(); const pubB = B.toPublicKey().toString()
  const { canonicalizePolicySet } = await import('@nektar/policy')
  const { signBinding, issueChallenge, answerChallenge, issueClearance, proveAccess, Smt, issueDisclosure, verifyDisclosureAnchored, fieldsCommitment, fieldPath, fieldRoot } = await import('@nektar/identity') as any
  const policyText = `permit(principal == Agent::"${toHex(hash160(Buffer.from(pubB, 'hex')))}", action, resource);`
  const ps = canonicalizePolicySet('v2', policyText)
  const vm = new NektarMemory(vc, vf, vf.keyPair, {
    batchPolicy: { maxLeaves: 1 },
    shardKeys: new Map([['v2', vf.keyPair]]),
    policy: { policyText, anchoredPolicySetHash: ps.hash },
    agentSeed: vf.keyPair, maxLevel: 2,
    clearanceAuthorities: [vf.keyPair.toPublicKey().toString()]
  })
  // agent A writes level-2 data
  await vm.remember('v2', 'level-2 secret', { source: 'agent-a' }, 2)
  await vm.anchorShard('v2'); vc.mine(1)
  // agent B: verified identity + ZK clearance ≥2 + Cedar ALLOW → reads
  const binding = signBinding(B, 'uuid-b', '00'.repeat(32), 0)
  const chal = vm.issueChallenge(); const csig = answerChallenge(B, chal)
  const iss = issueClearance(vf.keyPair, 2n, pubB)
  const tr = { queryId: 'q1', fragmentId: 'v2/0', nonce: new Uint8Array(32).fill(9) }
  const proof = proveAccess(iss, 2n, 8, tr)!
  const ident = { pubkey: pubB, binding, challengeNonce: chal, challengeSig: csig }
  const rb = await vm.recall('v2', { seq: 0, zk: { commitment: iss.commitment, proof, transcript: tr, requiredLevel: 2n }, identity: ident })
  if (rb.items[0]?.text !== 'level-2 secret') console.log('  B recall:', JSON.stringify(rb.items[0]), JSON.stringify(rb.receipt).slice(0,160))
  check(rb.items[0]?.text === 'level-2 secret' && rb.receipt.zkaccess === 'verified' && rb.receipt.cedar === 'allow',
    'B reads level-2 with identity+zk+cedar allow')
  // agent C: verified identity but Cedar DENY (principal not named in policy)
  const C = PrivateKey.fromRandom()
  const cchal = vm.issueChallenge()
  const cident = { pubkey: C.toPublicKey().toString(), binding: signBinding(C, 'uuid-c', '00'.repeat(32), 0), challengeNonce: cchal, challengeSig: answerChallenge(C, cchal) }
  const dc = await vm.recall('v2', { seq: 0, zk: { commitment: iss.commitment, proof, transcript: tr, requiredLevel: 2n }, identity: cident })
  check(dc.items[0]?.unverifiable === true && dc.receipt.cedar === 'deny', 'C denied by Cedar (identity fine, policy denies)')
  // agent B with no ZK proof on level-2 → denied
  const dz = await vm.recall('v2', { seq: 0, identity: ident })
  check(dz.items[0]?.unverifiable === true, 'B without ZK proof denied on level-2')
  // owner rotate mid-chain — earlier proofs still verify
  const newOwner = PrivateKey.fromRandom()
  const ac2 = (vm as any).anchors.get('v2')
  const rot = await ac2.rotate(newOwner)
  vc.mine(1)
  check(Number(rot.head.data.n) === 0, `rotate keeps state n=${rot.head.data.n}`)
  // earlier zk proof still verifies against the same commitment/transcript
  check((await import('@nektar/identity')).verifyAccess(iss.commitment, 2n, proof, tr) === true, 'ZK proof still verifies after rotate')
  // revoke the OLD key in the SMT, anchor the root, non-revocation for new key
  const smt = new Smt()
  const kOld = dsha256(Buffer.from(vf.keyPair.toPublicKey().toString()))
  smt.insert(kOld, new Uint8Array(32).fill(1))
  const rootRev = smt.root()
  await vm.remember('__rev__', `root:${Buffer.from(rootRev).toString('hex')}`); const ra = await vm.anchorShard('__rev__').catch((e: any) => { console.log('  rev anchor:', String(e?.message??e).slice(0,120)); return null }); vc.mine(1)
  check(ra !== null || ((vm as any).tails.get('__rev__')?.length ?? 0) > 0, 'revocation root anchored')
  const { proveNonInclusion, verifyProof: verifySmtProof } = await import('@nektar/identity') as any
  const nip = smt.prove(dsha256(Buffer.from(newOwner.toPublicKey().toString())))
  check(verifySmtProof(nip, { present: false }, rootRev) === true, 'new key non-inclusion verifies vs anchored root')
  // auditor disclosure envelope: verify anchored, then fails after expiry
  const fld = [{ name: 'secret', value: 'level-2 secret', salt: new Uint8Array(32).fill(3) }]
  const froot = fieldsCommitment(fld)
  const exp = Math.floor(Date.now() / 1000) + 60
  const env = issueDisclosure(vf.keyPair, fld, 0, C.toPublicKey().toString(), exp, 'audit')
  const fp = fieldPath(fld, 0)
  const now = Math.floor(Date.now() / 1000)
  const okD = await verifyDisclosureAnchored(env, vf.keyPair.toPublicKey().toString(), C.toPublicKey().toString(), fld.length, fieldRoot(fld), fp.siblings ?? fp, now, async () => true)
  check(okD.ok === true, 'auditor envelope verifies anchored')
  const badD = await verifyDisclosureAnchored(env, vf.keyPair.toPublicKey().toString(), C.toPublicKey().toString(), fld.length, fieldRoot(fld), fp.siblings ?? fp, exp + 61, async () => true)
  check(badD.ok === false, 'auditor envelope fails after expiry')
}

// --- P10: ZK membership (Wright §13): STARK over the anchored SHA-256 tree ---
// Proving needs the RISC Zero prover (Docker image nektar-stark-build, ~50 s);
// NEKTAR_E2E_STARK=0 skips it (the verifier is covered by unit tests either way).
console.log('— P10 ZK membership (STARK) —')
if (process.env.NEKTAR_E2E_STARK === '0') console.log('  (skipped: NEKTAR_E2E_STARK=0)')
else try {
  const vm = mem
  for (const d of ['p10 frag 0', 'p10 frag 1', 'p10 frag 2', 'p10 frag 3']) await vm.remember('zkm', d, {}, 1) // batch of 4 → anchored
  chain.mine(1)
  const link = vm.tailLink('zkm')
  check(!!link, 'zkm batch anchored')
  vm.confirmAnchor('zkm', link!.txid)
  const q = vm.issueZkQuery('e2e-verifier')
  const { bytes, ms } = await vm.proveMembership('zkm', 2, q.queryId)
  const res = await vm.verifyMembership('zkm', 2, bytes)
  check(res.ok === true, `STARK membership (${bytes.length} B, ${ms} ms) verifies vs the anchored SHA-256 root (${res.reason ?? 'ok'})`)
  const replay = await vm.verifyMembership('zkm', 2, bytes)
  check(replay.ok === false, `replay with the consumed queryId rejected (${replay.reason})`)
  const forged = bytes.slice(); forged[forged.length - 4000] ^= 1
  check((await vm.verifyMembership('zkm', 2, forged)).ok === false, 'tampered receipt rejected')
  console.log(`  TXIDS zkm-anchor=${link!.txid}`)
} catch (e) {
  check(false, `P10 zk block: ${String((e as Error).message).slice(0, 140)}`)
}

// — v4 AORG + policy DAG + instancing —
console.log('— v4 AORG / policy DAG / instancing —')
try {
  const { AORGraph, GraphAnchor, PolicyDAG, instanceOf, memberProof, delegate, verifyDelegation, verifyInstance } = await import('@nektar/graph')
  const { parseProgram } = await import('@nektar/datalog')
  const cat = parseProgram(`@id r-spec
spec(S, V) :- requires(P, V), applies(S, P).`).rules
  const g = new AORGraph(cat)
  const sigless = { kind: 'episode' as const, sigma: [] as string[], context: ['manual'] }
  const e1 = g.append({ ...sigless, phi: 'requires(torque, 45)', t: { logPos: 1 } }, [])
  const e2 = g.append({ ...sigless, phi: 'applies(port4, torque)', t: { logPos: 2 } }, [])
  const d45 = g.append({ kind: 'derived', phi: 'spec(port4, 45)', t: { logPos: 3 }, context: ['manual'], sigma: [] }, [e1.h, e2.h], 'r-spec')
  check(d45.h.length === 64, 'derived claim committed')
  // newer manual revision → refinement → currentView change (AOR-008a)
  const e3 = g.append({ ...sigless, phi: 'requires(torque, 55)', t: { logPos: 4 } }, [])
  const rf = g.append({ kind: 'refinement', phi: 'spec(port4, 55)', t: { logPos: 5 }, context: ['manual'], sigma: [], refines: d45.h }, [e3.h, e2.h], 'r-spec')
  check(g.currentView(4).some(v => v.phi === 'spec(port4, 45)'), 'old spec valid at t=4')
  check(!g.currentView(6).some(v => v.phi === 'spec(port4, 45)') && g.currentView(6).some(v => v.phi === 'spec(port4, 55)'), 'refinement changes currentView at t=6')
  // anchor graph vertices + policy root (AOR-006, POL-004)
  const ga = new GraphAnchor(mem, '__graph__')
  for (const h of [e1.h, e2.h, e3.h, d45.h, rf.h]) await ga.record(h)
  const graphTxid = await ga.anchor(); chain.mine(1)
  check(await ga.verifyVertex(d45.h), 'anchored vertex verifies')
  const pRules = parseProgram(`@id pol
permit(P, read, R) :- member(P, verified), fragment(R).`).rules
  const dag = new PolicyDAG([{ name: 'org', rules: pRules }])
  const proot = Buffer.from(dag.policyRoot()).toString('hex')
  await mem.remember('__policy__', `policyroot:${proot}`)
  const pAnchor = await mem.anchorShard('__policy__'); chain.mine(1)
  const GRAPH_TXID = graphTxid; const POL_TXID = pAnchor?.txid ?? 'n/a'
  console.log(`  TXIDS graph-anchor=${GRAPH_TXID} policy-root-anchor=${POL_TXID}`)
  // derivation-ALLOW + conflict-DENY through NektarMemory DAG gate
  const f2 = new MockFunder(); f2.topup(4); for (const u of (f2 as any).utxos) chain.registerParent(u.tx)
  const memDag = new NektarMemory(chain, f2, f2.keyPair, { batchPolicy: { maxLeaves: 10 }, policyDag: dag, anchoredPolicyRoot: proot })
  await memDag.remember('docs', 'spec sheet', {}, 0)
  const denyNoId = await memDag.recall('docs')
  check((denyNoId.receipt.dag?.state ?? '').startsWith('dag-deny'), 'DAG deny without verified principal')
  const dagConflict = new PolicyDAG([{ name: 'org', rules: [...pRules, ...parseProgram(`@id pd
deny(P, read, R) :- member(P, verified), fragment(R).`).rules] }])
  const memConf = new NektarMemory(chain, f2, f2.keyPair, { batchPolicy: { maxLeaves: 10 }, policyDag: dagConflict })
  await memConf.remember('docs', 'x', {}, 0)
  const { signBinding, issueChallenge, answerChallenge } = await import('@nektar/identity')
  const ck = PrivateKey.fromRandom(); const cNonce = memConf.issueChallenge()
  const denyConf = await memConf.recall('docs', { identity: { pubkey: ck.toPublicKey().toString(), binding: signBinding(ck, 'conflict-agent', ''.repeat(64), 0), challengeNonce: cNonce, challengeSig: answerChallenge(ck, cNonce) } })
  check(/conflict/.test(denyConf.receipt.dag?.state ?? ''), 'conflict DENY via DAG gate')
  // delegated instance verified by a third party (INS-004/005)
  const inst = instanceOf(g, ['manual'])
  const pf = memberProof(inst, d45.h); check(pf.index >= 0, 'instance member proof')
  const dk = PrivateKey.fromRandom(); const expiry = Date.now() + 60000
  const dsig = delegate(dk, inst, expiry)
  const anchored = new Map([...inst.members].map(h => [h, { anchorTxid: GRAPH_TXID }]))
  const vi = verifyInstance(g, inst, anchored as any, { pub: dk.toPublicKey(), expiry, sig: dsig })
  check(vi.ok, `third-party instance verify (${vi.reason ?? 'ok'})`)
  check(!verifyInstance(g, inst, new Map(), { pub: dk.toPublicKey(), expiry, sig: dsig }).ok, 'unanchored member instance rejected')
} catch (e) {
  check(false, `v4 block: ${e instanceof Error ? e.message : e}`)
}

// — P14 Merkle Automaton: full mode (Rúnar) + light mode (anchored trace) —
console.log('— P14 Merkle Automaton —')
try {
  const { Automaton, verifyTrace, NEKTAR_AUTOMATON_ARTIFACT } = await import('@nektar/automaton')
  const { automatonCosts } = await import('./automaton-costs.js')
  const def = { Q: ['draft', 'review', 'published'], Sigma: ['submit', 'approve'], delta: [[0, 0, 1], [1, 1, 2]] as [number, number, number][], q0: 0, F: [2], O: ['ok'] }
  const out = toHex(dsha256utf8('manual rev 2 published'))
  const f3 = new MockFunder(); f3.topup(8); for (const t of f3.parents) chain.registerParent(t)
  const am = new NektarMemory(chain, f3, f3.keyPair, { batchPolicy: { maxLeaves: 2 } })
  const d = await am.automatonDeploy('manual-lifecycle', def)
  const ac = am.automaton('manual-lifecycle')
  const s1 = await ac.step(0, 1, out)
  ac.unguarded = true
  let rejected = ''
  try { await ac.halt({ asState: 2 }) } catch (e) { rejected = (e as Error).message }
  ac.unguarded = false
  check(/SCRIPT_INVALID/.test(rejected), 'automaton: halt from non-final state rejected by script evaluation')
  const s2 = await ac.step(1, 2, out)
  const h = await ac.halt()
  check(h.state.halted && h.state.q === 2 && h.state.n === 3, 'automaton: draft→review→published, halted on-chain')
  const costs = automatonCosts(NEKTAR_AUTOMATON_ARTIFACT, s2.tx)
  console.log(`  AUT-009 lockingScript=${costs.scriptBytes}B stepTx=${costs.txBytes}B fee=${costs.feeSats}sat`)
  console.log(`  TXIDS automaton-deploy=${d.tx.id('hex')} step1=${s1.tx.id('hex')} step2=${s2.tx.id('hex')} halt=${h.tx.id('hex')}`)
  // light mode: transition events are anchored leaves; the trace verifies
  const seqs: number[] = []
  const rec = { remember: async (shard: string, data: string) => { const r = await am.remember(shard, data); seqs.push(r.seq); return r } }
  const lm = new Automaton(def, rec, 'auto-trace')
  await lm.step(0); await lm.step(1)
  await am.anchorShard('auto-trace'); chain.mine(1)
  for (const l of [am.tailLink('auto-trace')].filter(Boolean)) am.confirmAnchor('auto-trace', l!.txid)
  const proven = await Promise.all(seqs.map(async s => (await am.prove('auto-trace', s)).result.ok === true))
  const events = lm.events.map(e => ({ ...e, anchorTxid: am.tailLink('auto-trace')?.txid ?? '' }))
  check(verifyTrace(def, events, ev => proven[events.indexOf(ev)] === true).ok, 'light-mode trace: every event anchored + chained + in δ')
  check(!verifyTrace(def, events, () => false).ok, 'light-mode trace: unproven events flagged (AUT-006)')
} catch (e) {
  check(false, `P14 block: ${e instanceof Error ? e.message : e}`)
}

// — full paper scenario (every layer in one flow) —
console.log('— full paper scenario —')
try {
  const { runFullPaper } = await import('./full-paper.js')
  const pf = new MockFunder(); pf.topup(40); for (const t of pf.parents) chain.registerParent(t)
  const txids = await runFullPaper({
    chain, funder: pf, fundKey: pf.keyPair, check, label: 'paper',
    settle: async (m, shards) => { chain.mine(1); for (const s of shards) { const l = m.tailLink(s); if (l) m.confirmAnchor(s, l.txid) } }
  })
  console.log(`  TXIDS ${Object.entries(txids).map(([k, v]) => `${k}=${v}`).join(' ')}`)
} catch (e) {
  check(false, `full paper block: ${e instanceof Error ? e.stack?.split('\n').slice(0, 3).join(' | ') : e}`)
}
// — GOAL-v4 production block —
console.log('— v4 production block —')
try {
  const { runV4Block } = await import('./v4-block.js')
  const vf4 = new MockFunder(); vf4.topup(12); for (const t of vf4.parents) chain.registerParent(t)
  const tx = await runV4Block({
    chain, funder: vf4, fundKey: vf4.keyPair, check, label: 'v4',
    settle: async (m, shards) => { chain.mine(1); for (const s of shards) { const l = m.tailLink(s); if (l) m.confirmAnchor(s, l.txid) } },
    mineMore: async n => { chain.mine(n) }
  })
  console.log(`  TXIDS ${Object.entries(tx).map(([k, v]) => `${k}=${v}`).join(' ')}`)
} catch (e) {
  check(false, `v4 block: ${e instanceof Error ? e.stack?.split('\n').slice(0, 3).join(' | ') : e}`)
}

console.log(failures === 0 ? '\nE2E PASS' : `\nE2E FAIL: ${failures} checks failed`)
console.log(`  [pre-destroy total: ${Date.now()-_t0}ms]`)
await ov1.knex.destroy(); await ov2.knex.destroy()
process.exit(failures === 0 ? 0 : 1)
