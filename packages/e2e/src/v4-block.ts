// GOAL-v4 e2e block (offline + regtest, same code): signed ingest,
// checkpoints + promise monitor, finality depth, persisted replay protection
// across a restart, and the HTTP API serving proofs/checkpoints/metrics.
import { mkdtempSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import type { AddressInfo } from 'node:net'
import { PrivateKey } from '@bsv/sdk'
import { utf8 } from '@nektar/core'
import type { Broadcaster, ChainTracker, Funder } from '@nektar/chain'
import { NektarMemory, FileSecurityStore, createHttpApi } from '@nektar/mcp'
import { signProvenance, verifyCheckpoint } from '@nektar/log'
import { signBinding, answerChallenge, issueClearance, proveAccess, makeRotationCert } from '@nektar/identity'
import { axiomHash, ontologyEntails, type Axiom } from '@nektar/graph'
import { timeBounds, eventually, causalClosure } from '@nektar/reasoning'

export interface V4Env {
  chain: Broadcaster & ChainTracker
  funder: Funder
  fundKey: PrivateKey
  settle: (mem: NektarMemory, shards: string[]) => Promise<void>
  mineMore: (n: number) => Promise<void>
  check: (cond: boolean, name: string) => void
  label: string
}

export async function runV4Block ({ chain, funder, fundKey, settle, mineMore, check, label }: V4Env): Promise<Record<string, string>> {
  const L = (s: string) => `${label}: ${s}`
  const secFile = join(mkdtempSync(join(tmpdir(), 'nektar-v4-')), 'security.json')
  const authority = PrivateKey.fromRandom()
  const opts = { requireSignedProvenance: true, finalityDepth: 2, batchPolicy: { maxLeaves: 2 }, securityStore: new FileSecurityStore(secFile), clearanceAuthorities: [authority.toPublicKey().toString()] }
  const mem = new NektarMemory(chain, funder, fundKey, opts)
  // COM-003 signed ingest
  const src = PrivateKey.fromRandom(); const srcPub = src.toPublicKey().toString()
  const P = { source: 'doi:10.5555/manual-rev3', time: Math.floor(Date.now() / 1000), signature: new Uint8Array(0), modelId: 'v4' }
  const sign = (d: string) => ({ ...signProvenance(src, utf8(d), P), sourcePub: srcPub })
  let refused = ''
  try { await mem.remember('v4', 'unsigned claim') } catch (e) { refused = (e as Error).message }
  try { await mem.remember('v4', 'torque 47Nm', sign('torque 45Nm')) } catch (e) { refused += ` | ${(e as Error).message}` }
  check(refused === 'unsigned provenance refused | provenance signature invalid', L('ingest refuses unsigned and mis-signed provenance'))
  await mem.remember('v4', 'torque 45Nm', sign('torque 45Nm'), 1)
  await mem.remember('v4', 'belt 10h', sign('belt 10h'))
  // LOG-007/005 checkpoint + monitor
  const cp = mem.checkpoints('v4').at(-1)
  check(!!cp && verifyCheckpoint(cp, mem.logPublicKey()) && cp.size === 2, L('signed checkpoint issued for the anchor'))
  check(mem.promiseViolations(60_000, Date.now() + 120_000).length === 0, L('promise monitor: every promise covered by a checkpoint within the 60 s merge delay'))
  // CHN-006 finality
  await settle(mem, ['v4'])
  const s1 = await mem.anchorStatus('v4', 0)
  const held = (await mem.recall('v4', { seq: 1, requireFinal: true })).items[0]!
  await mineMore(1)
  const s2 = await mem.anchorStatus('v4', 0)
  check(s1.state === 'mined' && /not final/.test(held.reason ?? '') && s2.state === 'final', L(`finality: mined(${s1.confirmations}) withheld → final(${s2.confirmations}) after depth 2`))
  // persisted replay protection across a restart
  const key = PrivateKey.fromRandom(); const pub = key.toPublicKey().toString()
  const nonce = mem.issueChallenge()
  const identity = { pubkey: pub, binding: signBinding(key, 'agent', '00'.repeat(32), 0), challengeNonce: nonce, challengeSig: answerChallenge(key, nonce) }
  const cred = issueClearance(authority, 2n, pub)
  const transcript = { queryId: `v4-${label}`, fragmentId: 'v4/0', nonce: new Uint8Array(32).fill(7) }
  const zk = { commitment: cred.commitment, proof: proveAccess(cred, 1n, 8, transcript)!, transcript, requiredLevel: 1n }
  const r1 = await mem.recall('v4', { seq: 0, identity, zk })
  mem.revoke(PrivateKey.fromRandom().toPublicKey().toString())
  const restarted = new NektarMemory(chain, funder, fundKey, { ...opts, securityStore: new FileSecurityStore(secFile) })
  check(r1.items[0]?.text === 'torque 45Nm' && /replayed/.test(restarted.verifyClearance({ pubkey: pub }, zk, 'v4/0').reason ?? '') && restarted.verifiedCaller(identity) !== null,
    L('restart: challenge still valid, ZK transcript replay still rejected'))
  // API-001 / OPS-002 over HTTP
  const srv = createHttpApi(mem).listen(0)
  await new Promise(r => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}/v1`
  try {
    const proof = await (await fetch(`${base}/proof/v4/1`, { signal: AbortSignal.timeout(10_000) })).json() as { result: { ok: boolean } }
    const served = await (await fetch(`${base}/checkpoint/v4`)).json() as { checkpoint: typeof cp, logPublicKey: string }
    const metrics = await (await fetch(`${base}/metrics`)).text()
    check(proof.result.ok && verifyCheckpoint(served.checkpoint!, served.logPublicKey) && /nektar_anchors_total 1/.test(metrics) && /nektar_promise_violations 0/.test(metrics),
      L('HTTP API serves a verifying proof, a verifying checkpoint and live metrics'))
  } finally { srv.close() }
  // GOAL-v5 paper gaps on real anchors: §8 Δ revisions, §19 epoch authorship,
  // §12 temporal checks on consensus time, §9 anchored ontology entailment
  const m5 = new NektarMemory(chain, funder, fundKey, { batchPolicy: { maxLeaves: 4 } })
  await m5.remember('law', 'Manual rev1: torque 45Nm')
  await m5.revise('law', 0, 'Manual rev2: torque 55Nm')
  const ax: Axiom[] = [{ kind: 'SubClassOf', sub: 'Cryptographer', sup: 'Scientist' }, { kind: 'ClassAssertion', cls: 'Cryptographer', ind: 'alice' }]
  for (const a of ax) await m5.remember('law', `axiom:${axiomHash(a)}`)
  await settle(m5, ['law'])
  check(await m5.materialize('law', 1) === 'Manual rev2: torque 55Nm' && Buffer.from((await m5.records('law'))[1]!.fragment.d).toString().startsWith('delta:0:'), L('§8 revision stored as Δ and materialised from proven links'))
  const anchoredAxioms = new Set<string>()
  for (const r of await m5.records('law')) {
    const d = Buffer.from(r.fragment.d).toString()
    if (d.startsWith('axiom:') && (await m5.prove('law', r.seq)).result.ok) anchoredAxioms.add(d.slice(6))
  }
  check(ontologyEntails(ax, { pred: 'instance', args: ['alice', 'scientist'] }, h => anchoredAxioms.has(h)).entailed, L('§9 entailment only from anchored axioms'))
  const tb = await timeBounds(m5.anchorTx(m5.tailLink('law')!.txid)!, chain)
  const facts = [{ phi: 'revised(manual)', t: tb.notAfter }]
  check(eventually(facts, 'revised(manual)', tb.notAfter - 1, tb.notAfter) && !causalClosure([{ conclusion: 'pre-dated', t: tb.notAfter - 1, cites: facts }]).ok, L('§12 temporal check + causal closure on consensus time'))
  const k1 = PrivateKey.fromRandom(); const cert = { ...makeRotationCert(src, k1.toPublicKey().toString(), P.time - 10, 3600), index: 1 }
  check((await mem.authorship('v4', 0, srcPub, [cert])).ok, L('§19 authorship of an epoch-0 write survives a later rotation'))
  return { v4anchor: mem.tailLink('v4')!.txid, v5anchor: m5.tailLink('law')!.txid }
}
