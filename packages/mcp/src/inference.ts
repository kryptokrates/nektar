// attestInference — model-execution attestation for receipts.
//
// Honest scope: the shipped Groth16 proof (nChain zkscript ai_inference)
// binds exactly ONE statement — poseidon(input ‖ output ‖ poseidon(model))
// for the canned 784×16×10 quantized MLP + its demo input. We can attest
// THAT inference ran; we cannot mint proofs for other (model,input) — the
// demoAttestation builder fails closed on any deviation.
//
// Two levels:
//   - statement-verified: the attestation record re-derives (poseidon-toy)
//   - consensus-verified: the verifier UTXO was deployed + spent with the
//     proof — the spend txid is the on-chain attestation
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { AIINF_DEMO, demoAttestation, type InferenceAttestation } from '@mamem/zk'
import type { MamemMemory } from './memory.js'

/** emitted verifier artifacts — dev-local output of spike_groth16_aiinf.py
 *  (gitignored; the shipped proof is nChain's, the setup is dev-grade) */
const ARTIFACT_DIR = () => process.env.MAMEM_GROTH16_OUT ?? fileURLToPath(new URL('../../../../tools/groth16-spike/out/', import.meta.url))

export interface AttestInferenceResult {
  attestation: InferenceAttestation
  onchain: boolean
  statement: string
  note?: string
}

export async function attestInference (mem: MamemMemory, args: { input?: number[], output?: number } = {}): Promise<AttestInferenceResult> {
  const att = demoAttestation(args.input ?? AIINF_DEMO.input, args.output ?? AIINF_DEMO.output)

  const dir = ARTIFACT_DIR()
  const lf = join(dir, 'lock-aiinf.hex')
  const uf = join(dir, 'unlock-aiinf.hex')
  if (!existsSync(lf) || !existsSync(uf)) {
    return {
      attestation: att,
      onchain: false,
      statement: att.publicStatement,
      note: 'statement re-derived ✓ but verifier artifacts absent — emit them via tools/groth16-spike/spike_groth16_aiinf.py for the consensus-verified attestation'
    }
  }
  const lockHex = readFileSync(lf, 'utf8').trim()
  const unlockHex = readFileSync(uf, 'utf8').trim()
  att.proofCommit = createHash('sha256').update(Buffer.from(unlockHex, 'hex')).digest('hex')

  try {
    const { deployTxid, spendTxid } = await mem.attestInferenceOnchain(lockHex, unlockHex)
    att.deployTxid = deployTxid
    att.verifierTxid = spendTxid
    return { attestation: att, onchain: true, statement: att.publicStatement }
  } catch (e) {
    return { attestation: att, onchain: false, statement: att.publicStatement, note: `on-chain spend unavailable (attestation is statement-verified only): ${(e as Error).message}` }
  }
}
