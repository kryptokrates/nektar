// Generate the committed L_ZKM2 fixture (packages/zk/test/fixtures/zkm2-n5i4.json):
// a REAL RISC Zero succinct receipt for leaf 4 of a 5-leaf core/merkle.ts batch
// (gen-input.mjs 5 4). Run inside the docker toolchain (nektar-stark-build)
// after `pnpm stark:build`. Usage: node tools/stark/gen-fixture-mem.mjs
import { execFileSync } from 'node:child_process'
import { writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'

const input = JSON.parse(execFileSync('node', [fileURLToPath(new URL('./gen-input.mjs', import.meta.url)), '5', '4'], { encoding: 'utf8' }))
const out = execFileSync(
  'docker',
  ['run', '--rm', '-i', '-v', 'nektar-stark-target:/work/target', 'nektar-stark-build', '/work/target/release/nektar-stark', 'prove'],
  { input: JSON.stringify(input), encoding: 'utf8', maxBuffer: 64 * 1024 * 1024, env: { ...process.env, MSYS_NO_PATHCONV: '1' } }
)
const r = JSON.parse(out.trim().split('\n').pop())
const fx = {
  note: 'real RISC Zero succinct receipt: leaf 4 of a 5-leaf core/merkle.ts batch (tools/stark/gen-input.mjs 5 4)',
  input,
  imageId: r.imageId,
  journal: r.journal,
  receipt: r.receipt,
  ms: r.ms
}
const f = fileURLToPath(new URL('../../packages/zk/test/fixtures/zkm2-n5i4.json', import.meta.url))
writeFileSync(f, JSON.stringify(fx) + '\n')
console.log(`fixture written — ${Math.round(r.receipt.length / 2 / 1024)} KB receipt, prove ${r.ms} ms`)
