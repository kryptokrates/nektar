// pnpm bench — measured (never estimated) costs on this machine, offline mock
// chain with full script evaluation. Writes BENCHMARKS.md at the repo root.
import { writeFileSync } from 'node:fs'
import { cpus, totalmem } from 'node:os'
import { PrivateKey } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { NektarMemory } from '@nektar/mcp'
import { verifyFragment } from '@nektar/verify'
import { authorize } from '@nektar/policy'
import { Datalog, parseProgram } from '@nektar/datalog'
import { issueClearance, proveAccess, verifyAccess } from '@nektar/identity'
import { NEKTAR_AUTOMATON_ARTIFACT } from '@nektar/automaton'
import { automatonCosts } from './automaton-costs.js'

const rows: string[][] = []
const ms = (t0: bigint) => Number(process.hrtime.bigint() - t0) / 1e6
async function time (label: string, n: number, fn: (i: number) => Promise<unknown> | unknown, unit = 'ms/op'): Promise<number> {
  const t0 = process.hrtime.bigint()
  for (let i = 0; i < n; i++) await fn(i)
  const per = ms(t0) / n
  rows.push([label, String(n), `${per.toFixed(3)} ${unit}`, `${(1000 / per).toFixed(1)} op/s`])
  console.log(`${label}: ${per.toFixed(3)} ms/op (n=${n})`)
  return per
}
const facts: string[][] = []
const fact = (k: string, v: string) => { facts.push([k, v]); console.log(`${k}: ${v}`) }

function world (utxos: number, maxLeaves: number) {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(utxos); for (const t of funder.parents) chain.registerParent(t)
  return { chain, funder, mem: new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves } }) }
}

// 1. off-chain append (no anchor) — the write path's hot loop
{ const { mem } = world(2, 1_000_000); await time('remember (append, unanchored)', 2000, i => mem.remember('s', `record ${i}`)) }
// 2. anchor append: one contract spend per batch, script-evaluated
{
  const { mem } = world(60, 1)
  await mem.remember('s', 'genesis')
  await time('anchor append (1 Rúnar spend, script-evaluated)', 30, i => mem.remember('s', `b${i}`))
}
// 3. proof bundle size + verify latency
{
  const { chain, mem } = world(10, 16)
  for (let i = 0; i < 16; i++) await mem.remember('s', `r${i}`)
  chain.mine(1); mem.confirmAnchor('s', mem.tailLink('s')!.txid)
  const { bundle } = await mem.prove('s', 5)
  fact('proof bundle JSON size (16-leaf batch)', `${JSON.stringify(bundle).length} B`)
  fact('anchor atomic BEEF size', `${bundle!.anchorBEEF.length} B`)
  await time('verifyFragment (leaf→R→anchor→BUMP→headers)', 50, () => verifyFragment(bundle!, chain))
  // 4. ZK membership (Wright §13): RISC Zero ZK-STARK over the anchored SHA-256 tree
  const qs = [mem.issueZkQuery('bench')]
  const proofs: Uint8Array[] = []
  await time('ZK membership prove (STARK, 16-leaf batch, CPU, Docker prover)', 1, async () => { proofs.push((await mem.proveMembership('s', 5, qs[0]!.queryId)).bytes) })
  fact('L_ZKMem v2 proof size (succinct receipt)', `${proofs[0]!.length} B`)
  await time('ZK membership verify (WASM STARK + SPV of the anchor)', 1, () => mem.verifyMembership('s', 5, proofs[0]!))
}
// 5. ZK clearance (Bulletproofs, 8-bit)
{
  const cred = issueClearance(PrivateKey.fromRandom(), 5n, PrivateKey.fromRandom().toPublicKey().toString())
  const t = { queryId: 'q', fragmentId: 'f', nonce: new Uint8Array(32) }
  let p: ReturnType<typeof proveAccess> = null
  await time('ZK clearance prove (Bulletproof, 8-bit)', 5, () => { p = proveAccess(cred, 3n, 8, t) })
  await time('ZK clearance verify (Bulletproof, 8-bit)', 5, () => verifyAccess(cred.commitment, 3n, p!, t, { trustedAuthorities: [cred.commitment.pub], holder: cred.commitment.holder }))
}
// 6. Cedar decision
{
  const pol = 'permit(principal is Agent, action == Action::"read", resource is Fragment) when { resource.level <= principal.level };'
  await time('Cedar authorize (WASM)', 200, i => authorize(pol, { type: 'Agent', id: 'a', level: i % 4 }, 'read', { type: 'Fragment', id: 'f', attrs: { level: 2, shard: 's', class: 'c' } }))
}
// 7. Datalog policy derivation
{
  const prog = parseProgram(`@id r1
permit(P, read, F) :- member(P, verified), level(P, L), frag(F, L), not revoked(P).
@id r2
member(P, verified) :- bound(P).`)
  const edb = Array.from({ length: 200 }, (_, i) => [{ pred: 'bound', args: [`p${i}`] }, { pred: 'level', args: [`p${i}`, String(i % 4)] }, { pred: 'frag', args: [`f${i}`, String(i % 4)] }]).flat()
  edb.push({ pred: 'revoked', args: ['p3'] })
  const dl = new Datalog(prog.rules)
  let n = 0
  await time('Datalog evaluate (2 rules, 601 EDB facts)', 50, () => { n = dl.run(edb).size })
  fact('Datalog derived facts per run', String(n))
}
// 8. Automaton step (full mode)
{
  const { mem } = world(12, 4)
  const def = { Q: ['a', 'b'], Sigma: ['x', 'y'], delta: [[0, 0, 1], [1, 1, 0]] as [number, number, number][], q0: 0, F: [0], O: [] }
  await mem.automatonDeploy('m', def)
  const ac = mem.automaton('m')
  let last = ac.getHead()!
  await time('automaton step (Rúnar, δ Merkle path, script-evaluated)', 10, async i => { last = await ac.step(i % 2, (i + 1) % 2, 'ab'.repeat(32)) })
  const c = automatonCosts(NEKTAR_AUTOMATON_ARTIFACT, last.tx)
  fact('automaton locking script', `${c.scriptBytes} B`)
  fact('automaton step tx', `${c.txBytes} B, fee ${c.feeSats} sat (runar fee rate setting 1)`)
}

const md = `# nektar benchmarks (measured)

Generated by \`pnpm bench\` (packages/e2e/src/bench.ts) on ${new Date().toISOString().slice(0, 10)} — ${cpus()[0]?.model ?? 'cpu'} ×${cpus().length}, ${(totalmem() / 2 ** 30).toFixed(0)} GiB, Node ${process.version}.
Offline mock chain with **full Teranode-faithful script evaluation** of every contract spend. Regtest numbers (network latency, mining) are printed by \`NEKTAR_NETWORK=regtest pnpm e2e\` (AUT-009 line) and are not included here.

| operation | n | latency | throughput |
|---|---|---|---|
${rows.map(r => `| ${r.join(' | ')} |`).join('\n')}

| size / count | value |
|---|---|
${facts.map(f => `| ${f.join(' | ')} |`).join('\n')}

Notes
- The anchor append cost is per **batch**, not per record: one Rúnar spend anchors up to 65,536 leaves (ZK companion-tree bound), so per-record on-chain cost falls linearly with batch size.
- \`NektarAutomatonZk\` (on-chain Schnorr PoK gate) compiles to 883,419 B and is excluded from production (100 KB rule); it is not benchmarked.
- Single-machine run; numbers vary between runs. Re-run \`pnpm bench\` to refresh.
`
writeFileSync(new URL('../../../../BENCHMARKS.md', import.meta.url), md)
console.log('wrote BENCHMARKS.md')
