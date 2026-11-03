#!/usr/bin/env node
// Regenerate packages/zk/src/aiinf-demo.ts from the nChain zkscript
// ai_inference parameter files — the single source of truth for the demo
// vector (input ‖ output ‖ modelPoseidon ‖ publicStatement).
//
//   node tools/groth16-spike/gen-aiinf-demo.mjs [path/to/zkscript]
//   (defaults to ../zkscript relative to this repo)
import { readFileSync, writeFileSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'

const repo = join(dirname(fileURLToPath(import.meta.url)), '../..')
const DIR = process.argv[2] ?? join(repo, '../zkscript/examples/ai_inference')
const { modelPoseidon, publicStatement } = await import(join(repo, 'packages/zk/dist/src/poseidon-toy.js'))

const rows = f => readFileSync(join(DIR, 'parameters', f), 'utf8').trim().split('\n').map(l => l.trim().split(/\s+/).map(Number))
const col0 = f => rows(f).map(r => r[0])
const w1 = rows('weight_1.txt'), w2 = rows('weight_2.txt')
const b1 = col0('bias_1.txt'), b2 = col0('bias_2.txt')
const input = col0('input.txt')
const output = rows('expected_output.txt')[0][0]
const zero = rows('zero_relu.txt')[0][0]

const hm = modelPoseidon(w1, w2, b1, b2, zero)
const stmt = publicStatement(input, output, hm)

const out = `// GENERATED — nChain zkscript examples/ai_inference demo vector.
// Regenerate: node tools/groth16-spike/gen-aiinf-demo.mjs
// The shipped Groth16 proof binds exactly this (weights ‖ input ‖ output=${output})
// via publicStatement = poseidon(input ‖ output ‖ poseidon(model)).
// Toy Poseidon params — demo-grade, see poseidon-toy.ts.
export const AIINF_DEMO = {
  circuit: 'ai_inference-784x16x10-bls12381',
  setup: 'dev trusted setup (nChain example params — regtest-grade only)',
  license: 'nChain Research License — generated scripts non-commercial',
  zeroRelu: ${zero},
  output: ${output},
  input: [${input.join(',')}],
  modelPoseidon: 0x${hm.toString(16).padStart(64, '0')}n,
  publicStatement: 0x${stmt.toString(16).padStart(64, '0')}n
} as const
`
writeFileSync(join(repo, 'packages/zk/src/aiinf-demo.ts'), out)
console.log(`wrote packages/zk/src/aiinf-demo.ts — modelPoseidon ${hm.toString(16).slice(0, 16)}…, statement ${stmt.toString(16).slice(0, 16)}…`)
