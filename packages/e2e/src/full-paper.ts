// The "full paper" scenario (GOAL-v3 §Testing): one flow through every layer
// of arXiv:2506.13246 as implemented — run by the e2e on the regtest stack
// and the regtest e2e (Teranode) with the same code; only `settle` differs.
import { PrivateKey, type Transaction } from '@bsv/sdk'
import { utf8, toHex, hash160 } from '@nektar/core'
import type { Broadcaster, ChainTracker, Funder } from '@nektar/chain'
import { NektarMemory } from '@nektar/mcp'
import { canonicalizePolicySet } from '@nektar/policy'
import { parseProgram } from '@nektar/datalog'
import { AORGraph, GraphAnchor, PolicyDAG } from '@nektar/graph'
import {
  signBinding,
  answerChallenge,
  issueClearance,
  proveAccess,
  deriveKey,
  issueDisclosure,
  verifyDisclosure,
  fieldRoot,
  fieldPath,
  randSalt
} from '@nektar/identity'
import { verifyCombinedStark, type StarkQuery } from '@nektar/zk'
import {
  ground,
  commitReceipt,
  CitationIndex,
  onRevision,
  timeBounds,
  buildAuditBundle,
  verifyAuditBundle,
  modelIdentity,
  type FragmentFetch,
  type Claim
} from '@nektar/reasoning'

export interface PaperEnv {
  chain: Broadcaster & ChainTracker
  funder: Funder
  fundKey: PrivateKey
  /** mine and make the latest anchor of each shard provable (BUMP + confirm) */
  settle: (mem: NektarMemory, shards: string[]) => Promise<void>
  check: (cond: boolean, name: string) => void
  label: string
}

export async function runFullPaper({ chain, funder, fundKey, settle, check, label }: PaperEnv): Promise<Record<string, string>> {
  const txids: Record<string, string> = {}
  const L = (s: string) => `${label}: ${s}`
  // ── instance: trusted clearance authority, anchored Cedar + policy DAG ──
  const authority = PrivateKey.fromRandom()
  const cedar = canonicalizePolicySet(
    'paper',
    'permit(principal is Agent, action == Action::"read", resource is Fragment) when { resource.level <= principal.level };'
  )
  const dag = new PolicyDAG([
    {
      name: 'org',
      rules: parseProgram(`@id org-read
permit(P, read, F) :- member(P, verified), level(P, L), fragment(F).`).rules
    }
  ])
  const dagRoot = toHex(dag.policyRoot())
  const signingKey = PrivateKey.fromRandom()
  const ownerKey = PrivateKey.fromRandom()
  const mem = new NektarMemory(chain, funder, fundKey, {
    agentSeed: PrivateKey.fromRandom(),
    maxLevel: 2,
    batchPolicy: { maxLeaves: 2 },
    signingKey,
    shardKeys: new Map([['manual', ownerKey]]),
    policy: { policyText: cedar.text, anchoredPolicySetHash: cedar.hash },
    policyDag: dag,
    anchoredPolicyRoot: dagRoot,
    clearanceAuthorities: [authority.toPublicKey().toString()],
    modelId: 'paper-agent'
  })
  await mem.remember('__policy__', `cedar:${cedar.hash}`)
  await mem.remember('__policy__', `dagroot:${dagRoot}`)
  // ── §4/§13: agent A writes encrypted, level-tagged memories ─────────────
  const w0 = await mem.remember('manual', 'requires(torque, 45).', { source: 'manual-rev1' }, 2)
  const w1 = await mem.remember('manual', 'applies(port4, torque).', { source: 'manual-rev1' }, 2)
  await settle(mem, ['__policy__', 'manual'])
  check((await mem.prove('__policy__', 1)).result.ok, L('Cedar policy hash + DAG root anchored and provable'))
  txids.policy = mem.tailLink('__policy__')!.txid
  txids.manual = mem.tailLink('manual')!.txid

  // ── reader B: verified identity + ZK clearance + ZK membership ───────────
  const B = PrivateKey.fromRandom()
  const pubB = B.toPublicKey().toString()
  const credB = issueClearance(authority, 3n, pubB)
  const asB = (seq: number) => {
    const nonce = mem.issueChallenge()
    // ONE verifier-issued query feeds both proofs (§13 dual commitment)
    const zq = mem.issueZkQuery('nektar-service')
    const q: StarkQuery = { nonce: Uint8Array.from(Buffer.from(zq.nonce, 'hex')), verifierId: zq.verifierId }
    const transcript = { queryId: zq.queryId, fragmentId: `manual/${seq}`, nonce: q.nonce }
    return {
      q,
      queryId: zq.queryId,
      transcript,
      identity: { pubkey: pubB, binding: signBinding(B, 'agent-b', '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(B, nonce) },
      zk: { commitment: credB.commitment, proof: proveAccess(credB, 2n, 8, transcript)!, transcript, requiredLevel: 2n }
    }
  }
  const b0 = asB(0)
  const rB = await mem.recall('manual', { seq: 0, identity: b0.identity, zk: b0.zk })
  check(
    rB.items[0]?.text === 'requires(torque, 45).' &&
      rB.receipt.zkaccess === 'verified' &&
      rB.receipt.cedar === 'allow' &&
      rB.receipt.dag?.state === 'dag-allow',
    L('B: ZK clearance ≥2 + Cedar ALLOW + Datalog-derived permit → decrypted level-2 record')
  )
  if (process.env.NEKTAR_E2E_STARK === '0') console.log('  (STARK membership skipped: NEKTAR_E2E_STARK=0)')
  else {
    const { bytes } = await mem.proveMembership('manual', 0, b0.queryId)
    const vm = await mem.verifyMembership('manual', 0, bytes)
    check(
      vm.ok && verifyCombinedStark(b0.q, b0.transcript),
      L(`ZK membership (STARK) against the SPV-verified SHA-256 anchor + one shared query Π=(membership, access) (${vm.reason ?? 'ok'})`)
    )
  }
  // reader C: verified identity, no clearance → nothing
  const C = PrivateKey.fromRandom()
  const cn = mem.issueChallenge()
  const rC = await mem.recall('manual', {
    seq: 0,
    identity: {
      pubkey: C.toPublicKey().toString(),
      binding: signBinding(C, 'agent-c', '00'.repeat(32), 0),
      challengeNonce: cn,
      challengeSig: answerChallenge(C, cn)
    }
  })
  check(rC.items[0]?.text === undefined && rC.items[0]?.unverifiable === true, L('C (no clearance) gets neither plaintext nor ciphertext'))

  // ── §11 AORG: reasoning recorded; a newer manual revision refines it ────
  const rules = parseProgram(`@id r-spec
spec(S, V) :- requires(P, V), applies(S, P).`).rules
  const g = new AORGraph(rules)
  const ep = (phi: string, t: number) => g.append({ kind: 'episode', phi, t: { logPos: t }, context: ['manual'], sigma: [] }, [])
  const e1 = ep('requires(torque, 45)', 1)
  const e2 = ep('applies(port4, torque)', 2)
  const d45 = g.append({ kind: 'derived', phi: 'spec(port4, 45)', t: { logPos: 3 }, context: ['manual'], sigma: [] }, [e1.h, e2.h], 'r-spec')
  const w2 = await mem.remember('manual', 'requires(torque, 55).', { source: 'manual-rev2' }, 2)
  await mem.remember('manual', 'note(revision, 2).', { source: 'manual-rev2' }, 2)
  const e3 = ep('requires(torque, 55)', 4)
  const rf = g.append({ kind: 'refinement', phi: 'spec(port4, 55)', t: { logPos: 5 }, context: ['manual'], sigma: [], refines: d45.h }, [e3.h, e2.h], 'r-spec')
  const ga = new GraphAnchor(mem, '__graph__')
  for (const h of [e1.h, e2.h, d45.h, e3.h, rf.h]) await ga.record(h)
  txids.graph = await ga.anchor()
  check(
    g.currentView(4).some((v) => v.phi === 'spec(port4, 45)') &&
      g.currentView(6).some((v) => v.phi === 'spec(port4, 55)') &&
      !g.currentView(6).some((v) => v.phi === 'spec(port4, 45)'),
    L('contradiction handled as refinement: currentView changes, nothing deleted')
  )

  // ── §5 grounding + AGT-011/012 receipt + AOR-008c correction loop ───────
  const fetch: FragmentFetch = async (ref) => {
    const a = asB(ref.seq)
    const r = await mem.recall(ref.shard, { seq: ref.seq, identity: a.identity, zk: a.zk })
    return { verified: r.receipt.checkResult === 'verified' && (await mem.prove(ref.shard, ref.seq)).result.ok === true, text: r.items[0]?.text }
  }
  const m = (seq: number) => ({ shard: 'manual', seq })
  const answer: Claim[] = [
    { text: 'port 4 is torqued to 45 Nm', literal: 'spec(port4, 45)', cites: [m(0), m(1)] },
    { text: 'the pump housing is blue', cites: [] }
  ]
  await settle(mem, ['manual', '__graph__'])
  const gv = await ga.verifyVertex(d45.h)
  check(
    gv,
    L(`reasoning vertex anchored and provable${gv ? '' : ` (prove: ${JSON.stringify((await mem.prove('__graph__', ga.proveVertex(d45.h).ref.seq)).result)})`}`)
  )
  const report = await ground(answer, fetch, { rules })
  check(
    report.claims[0].reason === 'grounded:entailed' && report.claims[1].reason === 'uncited' && report.flagged.length === 1,
    L('grounding: entailed claim kept, unsupported statement flagged')
  )
  const model = await modelIdentity({ kind: 'api', provider: 'anthropic', release: 'claude-paper-run' })
  const committed = await commitReceipt(
    {
      answer: report.kept.map((c) => c.text).join('; '),
      fragmentRefs: [m(0), m(1)],
      checkResult: 'verified',
      modelId: model,
      timestamp: Math.floor(Date.now() / 1000),
      groundingReportHash: report.reportHash
    },
    mem
  )
  const idx = new CitationIndex()
  idx.add({ receiptHash: committed.receiptHash, claims: answer })
  const corr = await onRevision(idx, m(0), m(2), fetch, { rules, mem })
  check(
    corr.length === 1 && corr[0].withdrawn[0] === 'port 4 is torqued to 45 Nm',
    L('manual revision: the citing answer is re-checked and its claim withdrawn')
  )

  // ── §4 automaton: the answer's lifecycle as an on-chain state machine ────
  const auto = await mem.automatonDeploy('answer-lifecycle', {
    Q: ['drafted', 'grounded', 'published', 'corrected'],
    Sigma: ['ground', 'publish', 'correct'],
    delta: [
      [0, 0, 1],
      [1, 1, 2],
      [2, 2, 3]
    ],
    q0: 0,
    F: [3],
    O: []
  })
  const ac = mem.automaton('answer-lifecycle')
  await ac.step(0, 1, committed.receiptHash)
  await ac.step(1, 2, committed.receiptHash)
  const halted = await ac.halt({}).catch(() => null) // q=published ∉ F → contract refuses
  const s3 = await ac.step(2, 3, corr[0].correctionHash)
  const hl = await ac.halt()
  check(halted === null && s3.state.q === 3 && hl.state.halted, L('automaton: drafted→grounded→published→corrected; premature halt refused'))
  txids.automatonDeploy = auto.tx.id('hex')
  txids.automatonHalt = hl.tx.id('hex')

  // ── §17 identity: owner rotation + revocation of the old key ─────────────
  const newOwner = PrivateKey.fromRandom()
  txids.rotate = (await mem.rotateShard('manual', newOwner)).txid
  const oldPub = new Uint8Array(Buffer.from(ownerKey.toPublicKey().toString(), 'hex'))
  const revRoot = mem.revoke(ownerKey.toPublicKey().toString())
  check(
    mem.revocations.has(deriveKey(oldPub)) && !mem.revocations.has(deriveKey(new Uint8Array(Buffer.from(newOwner.toPublicKey().toString(), 'hex')))),
    L('old owner key revoked; new owner key not revoked')
  )
  await mem.remember('__revocation__', `revocation-root:${toHex(revRoot)}`)
  await mem.remember('__revocation__', 'rotated-owner:manual')
  await settle(mem, ['manual', '__receipts__', '__corrections__', '__revocation__'])
  check(
    (await mem.prove('manual', 0)).result.ok && (await mem.prove('__revocation__', 0)).result.ok,
    L('records anchored before rotation still verify; revocation root anchored')
  )
  txids.revocation = mem.tailLink('__revocation__')!.txid
  check(
    (await mem.prove('__receipts__', committed.anchor!.seq)).result.ok && (await mem.prove('__corrections__', corr[0].anchor!.seq)).result.ok,
    L('answer receipt + correction anchored and provable')
  )

  // ── §16 selective disclosure to an auditor (expiring) ───────────────────
  const auditor = PrivateKey.fromRandom().toPublicKey().toString()
  const fields = [
    { name: 'torque', value: utf8('45'), salt: randSalt() },
    { name: 'port', value: utf8('4'), salt: randSalt() }
  ]
  const exp = Math.floor(Date.now() / 1000) + 60
  const env = issueDisclosure(signingKey, fields, 0, auditor, exp)
  const sibs = fieldPath(fields, 0).siblings
  check(
    verifyDisclosure(env, signingKey.toPublicKey().toString(), auditor, 2, fieldRoot(fields), sibs, exp - 30).ok &&
      verifyDisclosure(env, signingKey.toPublicKey().toString(), auditor, 2, fieldRoot(fields), sibs, exp + 1).reason === 'expired',
    L('auditor disclosure verifies, then expires')
  )

  // ── §16 audit bundle, verified offline with headers + OUR policy only ───
  const edb = [
    { pred: 'member', args: [toHex(hash160(Buffer.from(pubB, 'hex'))), 'verified'] },
    { pred: 'level', args: [toHex(hash160(Buffer.from(pubB, 'hex'))), '2'] },
    { pred: 'fragment', args: ['manual/0'] }
  ]
  const derivation = dag.query(edb, 'permit', [toHex(hash160(Buffer.from(pubB, 'hex'))), 'read', 'manual/0'])!
  const promise = (p: { seq: number; leaf: string; signature: string }) => ({ seq: p.seq, leaf: p.leaf, signature: p.signature })
  const bundle = buildAuditBundle({
    receipt: committed.receipt,
    fragments: [
      { ref: m(0), bundle: (await mem.prove('manual', 0)).bundle! },
      { ref: m(1), bundle: (await mem.prove('manual', 1)).bundle! }
    ],
    signers: [
      { ref: m(0), pubkey: signingKey.toPublicKey().toString(), promise: promise(w0.promise) },
      { ref: m(1), pubkey: signingKey.toPublicKey().toString(), promise: promise(w1.promise) }
    ],
    policy: { edb, derivation }
  })
  const offline = JSON.parse(JSON.stringify(bundle))
  const av = await verifyAuditBundle(offline, chain, { dag })
  const tampered = JSON.parse(JSON.stringify(bundle))
  tampered.receipt.answer = 'port 4 is torqued to 99 Nm'
  check(
    av.ok && !(await verifyAuditBundle(tampered, chain, { dag })).ok,
    L(`audit bundle: output → fragments → signers → policy path → anchors verifies offline; tamper rejected (${av.reasons.join('; ') || 'ok'})`)
  )
  const tb = await timeBounds(mem.anchorTx(txids.manual) as Transaction, chain)
  check(tb.notAfter > 0, L(`consensus time: manual existed no later than block ${tb.blockHeight} (t=${tb.notAfter})`))
  check(w2.seq === 2, L('revision recorded as a new record (seq 2), the original stays'))
  return txids
}
