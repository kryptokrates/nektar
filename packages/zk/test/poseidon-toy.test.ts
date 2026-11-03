import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { poseidonToy, modelPoseidon, publicStatement, fr, FR_MODULUS } from '../src/poseidon-toy.js'
import { AIINF_DEMO } from '../src/aiinf-demo.js'

const here = fileURLToPath(new URL('../../test', import.meta.url)) // runs from dist/test → back to src test dir
const vec = { input: AIINF_DEMO.input, output: AIINF_DEMO.output, zeroRelu: AIINF_DEMO.zeroRelu, modelPoseidon: '0x' + AIINF_DEMO.modelPoseidon.toString(16), publicStatement: '0x' + AIINF_DEMO.publicStatement.toString(16) }

test('reproduces nChain ai_inference public statement (toy Poseidon)', () => {
  // self-contained vector: input(784) + output + modelPoseidon → the exact
  // Fr the Groth16 proof binds as its single public input
  const stmt = publicStatement(vec.input, vec.output, BigInt(vec.modelPoseidon))
  assert.equal(stmt, BigInt(vec.publicStatement))
})

test('sponge absorbs negatives as Fr wrap-around', () => {
  // arkworks Fr::from(-1) = P - 1
  assert.equal(fr(-1), FR_MODULUS - 1n)
  assert.equal(poseidonToy([-1]), poseidonToy([FR_MODULUS - 1n]))
})

test('rate-boundary absorb permutes mid-stream', () => {
  // sanity: results differ for different inputs but are deterministic
  assert.equal(poseidonToy([1, 2, 3]), poseidonToy([1, 2, 3]))
  assert.notEqual(poseidonToy([1, 2, 3]), poseidonToy([1, 2, 4]))
  assert.notEqual(poseidonToy([1]), poseidonToy([2]))
})

// full re-derivation from nChain's parameter files — dev-local only, runs when
// the zkscript checkout sits alongside the repo (not on CI)
const AIINF = process.env.MAMEM_AIINF_DIR ?? join(here, '../../../../zkscript/examples/ai_inference')
const P = join(AIINF, 'parameters')
const rowsOf = (f: string): number[][] =>
  readFileSync(join(P, f), 'utf8').trim().split('\n').map(l => l.trim().split(/\s+/).map(Number))
const col0 = (f: string): number[] => rowsOf(f).map(r => r[0]!)

test('modelPoseidon re-derives from parameter files', t => {
  if (!existsSync(join(P, 'weight_1.txt'))) {
    t.skip('nChain zkscript checkout not present (set MAMEM_AIINF_DIR)')
    return
  }
  const hm = modelPoseidon(rowsOf('weight_1.txt'), rowsOf('weight_2.txt'),
    col0('bias_1.txt'), col0('bias_2.txt'), rowsOf('zero_relu.txt')[0]![0]!)
  assert.equal(hm, BigInt(vec.modelPoseidon))
  const stmt = publicStatement(col0('input.txt'), rowsOf('expected_output.txt')[0]![0]!, hm)
  assert.equal(stmt, BigInt(vec.publicStatement))
})
