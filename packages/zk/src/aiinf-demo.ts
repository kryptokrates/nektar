// GENERATED — nChain zkscript examples/ai_inference demo vector.
// Regenerate: node tools/groth16-spike/gen-aiinf-demo.mjs
// The shipped Groth16 proof binds exactly this (weights ‖ input ‖ output=9)
// via publicStatement = poseidon(input ‖ output ‖ poseidon(model)).
// Toy Poseidon params — demo-grade, see poseidon-toy.ts.
import { createHash } from 'node:crypto'
import { publicStatement } from './poseidon-toy.js'

export const AIINF_DEMO = {
  circuit: 'ai_inference-784x16x10-bls12381',
  setup: 'dev trusted setup (nChain example params — regtest-grade only)',
  license: 'nChain Research License — generated scripts non-commercial',
  zeroRelu: 93,
  output: 9,
  input: [
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 10, 128, 154, 253, 253, 253, 242, 47, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 32, 131, 197, 252, 253, 252, 208, 196, 253, 84, 0, 101, 170, 56, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 29, 85, 229, 252, 252, 177, 56, 56, 13,
    0, 56, 75, 123, 246, 253, 84, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 134, 252, 253, 227, 103, 3, 0, 0, 0, 0, 76, 243, 252, 252, 178, 9, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 135, 253, 251, 75, 0, 0, 0, 0, 0, 26, 255, 253, 253, 253, 51, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 85, 252, 244, 82, 0, 0, 7, 57, 144, 243,
    253, 252, 252, 151, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 19, 231, 253, 240, 197, 147, 204, 252, 252, 177, 216, 252, 252, 28, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 75, 203, 252, 252, 252, 253, 227, 103, 3, 141, 252, 214, 15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 26, 113, 113, 51, 0, 0,
    63, 254, 247, 50, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 138, 253, 196, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 225, 253, 109, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 225, 228, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 60, 241, 114, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 13, 209, 252, 113, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 57, 252, 252, 25, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    107, 252, 151, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 16, 216, 253, 28, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 54, 252, 196, 9, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 104, 252, 130, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0,
    0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 29, 227, 31, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0
  ],
  modelPoseidon: 0x0df36e5c244b8c3e2c23534cdc844944726617eea7844c3a9b0e6f279948339en,
  publicStatement: 0x5c034d38618d75a74962aaa7fa8c76525245c8a5c20a32aaeb7efd65c7e1a9bdn
} as const

/** the receipt-embedded attestation record (reasoning.Receipt.modelAttestation
 *  + `input`, needed by verifiers to re-derive the statement) */
export interface InferenceAttestation {
  circuit: string
  setup: string
  license?: string
  modelPoseidon: string // 0x-fr — binds the exact weights
  input: number[] // raw input — public demo data, needed for re-derivation
  inputCommit: string // sha256 hex of the canonical input
  output: string // the argmax the proof asserts
  publicStatement: string // 0x-fr — the proof's single public input
  proofCommit?: string // sha256 hex of the unlocking-script bytes (proof + statement as pushed)
  /** bstore endpoint/bucket the proof bytes sit under `chirp/<proofCommit>` —
   *  a verifier resolves them off any public-read gateway, origin-free */
  proofStore?: { endpoint: string; bucket: string }
  attestationTxid?: string // ~300B OP_RETURN tx anchoring sha256(proof) — the normal path
  commitment?: string // the anchored commitment (= proofCommit when bound this way)
  verifierTxid?: string // on-chain spend that ran the verifier under consensus — the dispute/showcase rail
  deployTxid?: string // the verifier UTXO deployment
}

/** re-derive the statement and check it matches — a verifier's offline check */
export function validateAttestation(a: { modelPoseidon: string; input: readonly number[]; output: number | string; publicStatement: string }): {
  ok: boolean
  reason?: string
} {
  let model: bigint, stmt: bigint
  try {
    model = BigInt(a.modelPoseidon)
    stmt = BigInt(a.publicStatement)
  } catch {
    return { ok: false, reason: 'attestation field elements are not integers' }
  }
  const out = typeof a.output === 'number' ? a.output : Number(a.output)
  if (!Number.isInteger(out)) return { ok: false, reason: 'attestation output is not an integer' }
  const derived = publicStatement(a.input, out, model)
  return derived === stmt
    ? { ok: true }
    : {
        ok: false,
        reason: `statement ${stmt.toString(16).slice(0, 16)}… does not re-derive from (input ‖ output ‖ modelPoseidon) — got ${derived.toString(16).slice(0, 16)}…`
      }
}

/** build the attestation for a (modelPoseidon, input, output) triple — only the
 *  canned demo vector has a shipped Groth16 proof today, so the statement must
 *  equal AIINF_DEMO's; anything else fails closed (we cannot mint proofs). */
export function demoAttestation(input: readonly number[] = AIINF_DEMO.input, output: number = AIINF_DEMO.output): InferenceAttestation {
  const stmt = publicStatement(input, output, AIINF_DEMO.modelPoseidon)
  if (stmt !== AIINF_DEMO.publicStatement) {
    throw new Error(
      `no proof exists for (input, output) ≠ demo vector — statement ${'0x' + stmt.toString(16)} ≠ shipped proof's ${'0x' + AIINF_DEMO.publicStatement.toString(16)}`
    )
  }
  const inputCommit = createHash('sha256')
    .update(
      Buffer.concat(
        input.map((v) => {
          const b = Buffer.alloc(4)
          b.writeInt32LE(v)
          return b
        })
      )
    )
    .digest('hex')
  return {
    circuit: AIINF_DEMO.circuit,
    setup: AIINF_DEMO.setup,
    license: AIINF_DEMO.license,
    modelPoseidon: '0x' + AIINF_DEMO.modelPoseidon.toString(16).padStart(64, '0'),
    input: [...input],
    inputCommit,
    output: String(output),
    publicStatement: '0x' + stmt.toString(16).padStart(64, '0')
  }
}
