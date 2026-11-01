// pnpm zk:ceremony — local Groth16 ceremony for circuits/membership.circom.
// Writes artifacts/{membership.zkey, vkey.json, ceremony-transcript.json}.
// Intermediates (ptau, contrib zkeys, r1cs) go to build/ (gitignored).
import * as snarkjs from 'snarkjs'
import { createHash, randomBytes } from 'node:crypto'
import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import { fileURLToPath } from 'node:url'

const here = dirname(fileURLToPath(import.meta.url))
const zk = join(here, '..')
const build = join(zk, 'build')
const artifacts = join(zk, 'artifacts')
mkdirSync(build, { recursive: true })
mkdirSync(artifacts, { recursive: true })

const sha = (p) => createHash('sha256').update(readFileSync(p)).digest('hex')

// --- powers of tau (circuit-agnostic, pot14) ---
const curve = await snarkjs.curves.getCurveFromName('bn128')
if (!existsSync(`${build}/pot14_final.ptau`)) {
  await snarkjs.powersOfTau.newAccumulator(curve, 14, `${build}/pot14_0.ptau`)
  for (let i = 0; i < 2; i++) {
    await snarkjs.powersOfTau.contribute(`${build}/pot14_${i}.ptau`, `${build}/pot14_${i + 1}.ptau`, `ptau-c${i}`, randomBytes(32).toString('hex'))
  }
  await snarkjs.powersOfTau.preparePhase2(`${build}/pot14_2.ptau`, `${build}/pot14_final.ptau`)
  console.log('ptau done')
} else console.log('reusing build/pot14_final.ptau')

// --- circuit compile (r1cs only — wasm is built once at circuits/membership_js)
if (!existsSync(`${build}/membership.r1cs`)) throw new Error('build/membership.r1cs missing — compile the circuit first (circom2 --r1cs)')

// --- zkey: phase 2 with 2 contributions ---
await snarkjs.zKey.newZKey(`${build}/membership.r1cs`, `${build}/pot14_final.ptau`, `${build}/membership_0.zkey`)
await snarkjs.zKey.contribute(`${build}/membership_0.zkey`, `${build}/membership_1.zkey`, 'zk-c1', randomBytes(32).toString('hex'))
await snarkjs.zKey.contribute(`${build}/membership_1.zkey`, `${artifacts}/membership.zkey`, 'zk-c2', randomBytes(32).toString('hex'))

const vk = await snarkjs.zKey.exportVerificationKey(`${artifacts}/membership.zkey`)
writeFileSync(`${artifacts}/vkey.json`, JSON.stringify(vk))
const transcript = {
  circuit: 'circuits/membership.circom (depth 16)',
  contributors: ['ptau-c0', 'ptau-c1', 'zk-c1', 'zk-c2'],
  timestamps: [new Date().toISOString()],
  r1csHash: sha(`${build}/membership.r1cs`),
  ptauHash: sha(`${build}/pot14_final.ptau`),
  zkeyHash: sha(`${artifacts}/membership.zkey`),
  vkeyHash: sha(`${artifacts}/vkey.json`)
}
writeFileSync(`${artifacts}/ceremony-transcript.json`, JSON.stringify(transcript, null, 2))
console.log('ceremony done — vkeyHash', transcript.vkeyHash)
