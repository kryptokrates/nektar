// Generate the committed L_ZKDer fixture (packages/zk/test/fixtures/zkder-permit.json):
// a REAL RISC Zero succinct receipt for the 2-step `permit(a, s1)` path from
// gen-vectors-datalog.mjs. Run inside the docker toolchain (nektar-stark-build)
// after `pnpm stark:build`. Usage: node tools/stark/gen-fixture-der.mjs
import { execFileSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const gen = execFileSync('node', [fileURLToPath(new URL('./gen-vectors-datalog.mjs', import.meta.url))], { encoding: 'utf8' })
const line = gen.split('\n').find((l) => l.includes('"case":"der:two-step"'))
if (!line) throw new Error('der:two-step vector not generated')
const input = (({ case: _c, expect: _e, ...rest }) => rest)(JSON.parse(line))
const out = execFileSync(
  'docker',
  ['run', '--rm', '-i', '-v', 'nektar-stark-target:/work/target', 'nektar-stark-build', '/work/target/release/nektar-stark', 'prove-der'],
  { input: JSON.stringify(input), encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, env: { ...process.env, MSYS_NO_PATHCONV: '1' } }
)
const r = JSON.parse(out.trim().split('\n').pop())
const fx = {
  imageId: r.imageId,
  journal: r.journal,
  receipt: r.receipt,
  ms: r.ms,
  statement: '§14 committed path: permit(a,s1) under the gen-vectors policy DAG'
}
const f = fileURLToPath(new URL('../../packages/zk/test/fixtures/zkder-permit.json', import.meta.url))
writeFileSync(f, JSON.stringify(fx) + '\n')
console.log(`fixture written — ${Math.round(r.receipt.length / 2 / 1024)} KB receipt, prove ${r.ms} ms`)
