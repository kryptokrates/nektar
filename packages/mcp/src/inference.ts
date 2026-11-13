// attestInference — model-execution attestation for receipts.
//
// Honest scope: the shipped Groth16 proof (nChain zkscript ai_inference)
// binds exactly ONE statement — poseidon(input ‖ output ‖ poseidon(model))
// for the canned 784×16×10 quantized MLP + its demo input. We can attest
// THAT inference ran; we cannot mint proofs for other (model,input) — the
// demoAttestation builder fails closed on any deviation.
//
// Three levels (Wright §18 — "executable on-chain if necessary"):
//   - statement-verified: the attestation record re-derives (poseidon-toy)
//   - attested+anchored: sha256(proof-as-pushed) is anchored in a ~300B
//     OP_RETURN tx (the DEFAULT — cheap, one tx); anyone can fetch the
//     proof bytes via /v1/zkproof/<commit> and re-check both
//   - consensus-checked: `onchain:true` deploys the ~398KB in-script
//     verifier and spends it — the dispute/showcase rail, not the hot path
import { createHash } from 'node:crypto'
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { AIINF_DEMO, demoAttestation, type InferenceAttestation } from '@nektar/zk'
import type { NektarMemory } from './memory.js'

/** emitted verifier artifacts — dev-local output of spike_groth16_aiinf.py
 *  (gitignored; the shipped proof is nChain's, the setup is dev-grade) */
const ARTIFACT_DIR = () => nektarEnv('NEKTAR_GROTH16_OUT') ?? fileURLToPath(new URL('../../../../tools/groth16-spike/out/', import.meta.url))

export interface AttestInferenceResult {
  attestation: InferenceAttestation
  onchain: boolean
  anchored: boolean
  statement: string
  note?: string
}

export async function attestInference(mem: NektarMemory, args: { input?: number[]; output?: number; onchain?: boolean } = {}): Promise<AttestInferenceResult> {
  const att = demoAttestation(args.input ?? AIINF_DEMO.input, args.output ?? AIINF_DEMO.output)

  const dir = ARTIFACT_DIR()
  const lf = join(dir, 'lock-aiinf.hex')
  const uf = join(dir, 'unlock-aiinf.hex')
  if (!existsSync(lf) || !existsSync(uf)) {
    return {
      attestation: att,
      onchain: false,
      anchored: false,
      statement: att.publicStatement,
      note: 'statement re-derived ✓ but verifier artifacts absent — emit them via tools/groth16-spike/spike_groth16_aiinf.py for anchored or consensus-checked attestation'
    }
  }
  const lockHex = readFileSync(lf, 'utf8').trim()
  const unlockBytes = Buffer.from(readFileSync(uf, 'utf8').trim(), 'hex')
  att.proofCommit = createHash('sha256').update(unlockBytes).digest('hex')
  // publish the proof bytes so /v1/zkproof/<commit> can serve them —
  // the attestation binds these exact bytes, not a claim about them
  mem.putProof(unlockBytes)
  // the store locator rides the attestation: chirp/<commit> resolves the
  // bytes off any public-read gateway — no origin call needed to re-verify
  const ps = mem.proofStoreRef(att.proofCommit)
  if (ps !== undefined) att.proofStore = ps

  // dispute/showcase rail (§18 "if necessary"): consensus runs the verifier
  if (args.onchain) {
    try {
      const { deployTxid, spendTxid } = await mem.attestInferenceOnchain(lockHex, unlockBytes.toString('hex'))
      att.deployTxid = deployTxid
      att.verifierTxid = spendTxid
      return { attestation: att, onchain: true, anchored: true, statement: att.publicStatement }
    } catch (e) {
      return {
        attestation: att,
        onchain: false,
        anchored: false,
        statement: att.publicStatement,
        note: `on-chain spend unavailable (attestation is statement-verified only): ${(e as Error).message}`
      }
    }
  }

  // normal path: anchor sha256(proof) in a small OP_RETURN attestation tx
  try {
    const a = await mem.anchorAttestation(unlockBytes)
    att.attestationTxid = a.txid
    att.commitment = a.commitment
    return { attestation: att, onchain: false, anchored: true, statement: att.publicStatement }
  } catch (e) {
    return {
      attestation: att,
      onchain: false,
      anchored: false,
      statement: att.publicStatement,
      note: `attestation anchor unavailable (statement-verified only): ${(e as Error).message}`
    }
  }
}
