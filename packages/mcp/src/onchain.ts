// GOAL-v6 S1a — §13 cheap on-chain attestation ("on-chain if necessary").
// After a STARK membership proof verifies, its EXISTENCE + binding can be
// anchored on-chain for ~1 sat of fee: a separate data-output tx funded by
// the fee key carrying OP_FALSE OP_RETURN <tag> <sha256(proof)>. The WASM
// verifier stays authoritative — the chain only timestamps the commitment.
// This is NOT a new anchor: no shard state, no contract, plain P2PKH spend.
import { Transaction, PrivateKey, P2PKH, Script, LockingScript, SatoshisPerKilobyte } from '@bsv/sdk'
import { sha256, fromHex, utf8, type Bytes } from '@nektar/core'
import type { FundingOut } from '@nektar/chain'

/** attestation push tag — decoders route on it; versioned by suffix */
export const ATTEST_TAG = 'nektar:attest:v1'

/** the commitment anchored on-chain: single SHA-256 over the proof bytes */
export function attestationCommitment(proofBytes: Bytes): Bytes {
  return sha256(proofBytes)
}

/** OP_FALSE OP_RETURN <tag> <32B commitment> — a provably unspendable data output */
export function attestationScript(commitment: Bytes): LockingScript {
  if (commitment.length !== 32) throw new Error('attestation commitment must be 32 bytes')
  const s = new Script()
    .writeOpCode(0x00) // OP_FALSE
    .writeOpCode(0x6a) // OP_RETURN
    .writeBin([...utf8(ATTEST_TAG)])
    .writeBin([...commitment])
  return s as LockingScript
}

/** extract the attestation commitment from a locking script, or null.
 *  Layout: OP_FALSE OP_RETURN <tag push> <32B push> — pushes are parsed
 *  directly (the SDK folds the OP_RETURN suffix into one chunk). */
export function decodeAttestationScript(scriptHex: string): { tag: string; commitment: Uint8Array } | null {
  try {
    const hex = scriptHex.startsWith('0x') ? scriptHex.slice(2) : scriptHex
    if (!hex.startsWith('006a')) return null
    const rest = Buffer.from(hex.slice(4), 'hex')
    const pushes: Buffer[] = []
    for (let o = 0; o < rest.length;) {
      const len = rest[o]!
      if (len === 0 || len > 75 || o + 1 + len > rest.length) return null // all fields ≤ 75 B
      pushes.push(rest.subarray(o + 1, o + 1 + len))
      o += 1 + len
    }
    if (pushes.length !== 2 || pushes[1]!.length !== 32) return null
    const tag = pushes[0]!.toString('utf8')
    if (tag !== ATTEST_TAG) return null
    return { tag, commitment: new Uint8Array(pushes[1]!) }
  } catch {
    return null
  }
}

/** every attestation commitment in a tx's outputs */
export function attestationsOf(tx: Transaction): Array<{ vout: number; tag: string; commitment: Uint8Array }> {
  const out: Array<{ vout: number; tag: string; commitment: Uint8Array }> = []
  tx.outputs.forEach((o, vout) => {
    const d = decodeAttestationScript(o.lockingScript.toHex())
    if (d) out.push({ vout, ...d })
  })
  return out
}

/**
 * Build (unsigned→signed) a data-output tx: one P2PKH funding input from the
 * fee key, output 0 = the attestation data output (0 sats, unspendable),
 * output 1 = change back to the fee key. `feeRateSatPerKb` matches the
 * anchor fee policy (default 1 on mock/regtest, ~100 on main).
 */
export async function buildAttestationTx(
  funding: FundingOut,
  fundKey: PrivateKey,
  commitment: Bytes,
  feeRateSatPerKb = 1,
  changeAddress?: string
): Promise<{ tx: Transaction; changeVout: number }> {
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: funding.tx, sourceOutputIndex: funding.vout, unlockingScriptTemplate: new P2PKH().unlock(fundKey) })
  tx.addOutput({ lockingScript: attestationScript(commitment), satoshis: 0 })
  tx.addOutput({ lockingScript: new P2PKH().lock(changeAddress ?? fundKey.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee(new SatoshisPerKilobyte(feeRateSatPerKb))
  await tx.sign()
  return { tx, changeVout: tx.outputs.length - 1 }
}
