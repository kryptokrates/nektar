// attestops.ts — the on-chain attestation edge extracted from NektarMemory:
// OP_FALSE OP_RETURN sha256(proof) commitments (GOAL-v6 S1a — a timestamped
// commitment, NOT an anchor), the membership attest wrapper, attestation
// verification, and the Groth16 deploy/spend pair. attestationTxs is shared
// by reference with TxOps/restore.
import { Transaction, LockingScript, UnlockingScript, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { toHex, signerAddress, unlockP2PKH, type DigestSigner } from '@nektar/core'
import type { FundingOut, Broadcaster } from '@nektar/chain'

export interface AttestOpsDeps {
  /** txid → attestation/model-verifier tx this instance broadcast (shared map) */
  attestationTxs: Map<string, Transaction>
  anchorTxs: Map<string, Transaction>
  takeFunding: (satoshis: number) => Promise<FundingOut>
  fundSigner: () => DigestSigner
  chain: Broadcaster
  feeRateSatPerKb: () => number
  markSpentOut: (txid: string, vout: number) => void
  addOwnedOut: (tx: Transaction, vout: number) => void
  metricsInc: (name: string, labels?: Record<string, string>, n?: number, help?: string) => void
  proveMembership: (shard: string, seq: number, queryIdHex: string) => Promise<{ bytes: Uint8Array; ms: number }>
}

export class AttestOps {
  constructor(private deps: AttestOpsDeps) {}

  /** anchor sha256(proofBytes) in an OP_FALSE OP_RETURN data tx funded by the
   *  fee key (~1 sat of fee at regtest rates). Returns the txid + commitment. */
  async anchorAttestation(proofBytes: Uint8Array): Promise<{ txid: string; commitment: string }> {
    const { buildAttestationTx, attestationCommitment } = await import('./onchain.js')
    const commitment = attestationCommitment(proofBytes)
    const f = await this.deps.takeFunding(5_000)
    const { tx, changeVout } = await buildAttestationTx(f, this.deps.fundSigner(), commitment, this.deps.feeRateSatPerKb())
    const r = await this.deps.chain.broadcast(tx)
    if (r.status !== 'success') throw new Error(`attestation broadcast failed: ${r.description}`)
    this.deps.attestationTxs.set(tx.id('hex'), tx)
    this.deps.markSpentOut(f.tx.id('hex'), f.vout)
    this.deps.addOwnedOut(tx, changeVout)
    this.deps.metricsInc('nektar_anchors_total', {}, 1, 'anchors broadcast')
    return { txid: tx.id('hex'), commitment: toHex(commitment) }
  }

  /** holder side (§13/S1a): produce the STARK membership proof AND anchor its
   *  commitment on-chain. The queryId stays unconsumed — the verifier's own
   *  verifyMembership call still succeeds (single use is its privilege). */
  async attestMembership(shard: string, seq: number, queryIdHex: string): Promise<{ bytes: Uint8Array; ms: number; txid: string; commitment: string }> {
    const { bytes, ms } = await this.deps.proveMembership(shard, seq, queryIdHex)
    const a = await this.anchorAttestation(bytes)
    return { bytes, ms, ...a }
  }

  /** verifier side: fetch the attestation tx, extract the OP_RETURN hash and
   *  check it equals sha256(supplied proof bytes). Only txs broadcast by THIS
   *  instance are resolvable (chain fetch by txid is an ops gap, CHN-*). */
  async verifyAttestation(txid: string, proofBytes: Uint8Array): Promise<{ ok: boolean; reason?: string; commitment?: string; vout?: number }> {
    const { attestationsOf, attestationCommitment } = await import('./onchain.js')
    const tx = this.deps.attestationTxs.get(txid) ?? this.deps.anchorTxs.get(txid)
    if (!tx) return { ok: false, reason: 'attestation tx unknown to this instance' }
    const want = toHex(attestationCommitment(proofBytes))
    const hit = attestationsOf(tx).find((a) => toHex(a.commitment) === want)
    if (!hit) return { ok: false, reason: 'no attestation output carries sha256(proof)' }
    return { ok: true, commitment: want, vout: hit.vout }
  }

  /** deploy an in-script Groth16 verifier (lock) and spend it with the proof
   *  (unlock) — consensus itself runs the verification. The spend txid is the
   *  attestation a receipt cites: it exists on-chain or it doesn't. */
  async attestInferenceOnchain(lockHex: string, unlockHex: string): Promise<{ deployTxid: string; spendTxid: string }> {
    const LOCK_SATS = 20_000
    const addr = await signerAddress(this.deps.fundSigner(), [0x6f])
    const f = await this.deps.takeFunding(LOCK_SATS + 5_000)
    const tx1 = new Transaction()
    tx1.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: unlockP2PKH(this.deps.fundSigner()) })
    tx1.addOutput({ lockingScript: LockingScript.fromHex(lockHex), satoshis: LOCK_SATS })
    tx1.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
    await tx1.fee(new SatoshisPerKilobyte(this.deps.feeRateSatPerKb()))
    await tx1.sign()
    const b1 = await this.deps.chain.broadcast(tx1)
    if (b1.status !== 'success') throw new Error(`verifier deploy failed: ${b1.description}`)
    this.deps.markSpentOut(f.tx.id('hex'), f.vout)
    this.deps.addOwnedOut(tx1, 1)
    this.deps.attestationTxs.set(tx1.id('hex'), tx1)
    const tx2 = new Transaction()
    tx2.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
    tx2.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: LOCK_SATS - 5_000 })
    const b2 = await this.deps.chain.broadcast(tx2)
    if (b2.status !== 'success') throw new Error(`proof spend failed: ${b2.description}`)
    this.deps.addOwnedOut(tx2, 0)
    this.deps.attestationTxs.set(tx2.id('hex'), tx2)
    return { deployTxid: tx1.id('hex'), spendTxid: tx2.id('hex') }
  }
}
