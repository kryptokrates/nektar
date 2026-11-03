// Host adapter for the V2 NektarCheckpointBond covenant (GOAL-v7).
// StatefulSmartContract: state {n, lastRoot} lives after OP_RETURN; the SDK
// computes the OP_PUSH_TX preimage and the unlocking layout — no
// hand-assembled script bytes.
import { PrivateKey, Transaction, LockingScript, UnlockingScript, Hash } from '@bsv/sdk'
import {
  RunarContract, computeOpPushTx, serializeState,
  extractStateFromScript, buildP2PKHScript
} from 'runar-sdk'
import { NEKTAR_CHECKPOINTBOND_ARTIFACT } from '@nektar/contracts'

export interface BondConfig {
  operatorPkh: string       // 20B hex — checkpoint writer
  bondReleaseHeight: bigint // locktime gate for close()
  maxFee: bigint
}

export interface BondUtxo { txid: string, vout: number, satoshis: number, script: string }
export interface BondState { n: bigint, lastRoot: string }

export class CheckpointBond {
  private contract: RunarContract
  constructor (private cfg: BondConfig) {
    this.contract = new RunarContract(NEKTAR_CHECKPOINTBOND_ARTIFACT, [
      cfg.operatorPkh, cfg.bondReleaseHeight, cfg.maxFee
    ])
  }

  get params (): BondConfig { return this.cfg }

  private stateHex (n: bigint, lastRoot: string): string {
    const c = this.cfg
    return serializeState(NEKTAR_CHECKPOINTBOND_ARTIFACT.stateFields!, {
      operatorPkh: c.operatorPkh, bondReleaseHeight: c.bondReleaseHeight,
      maxFee: c.maxFee, n, lastRoot
    })
  }

  /** bond locking script = code part + OP_RETURN + serialized state */
  lockingScript (n = 0n, lastRoot = ''): string {
    return (this.contract as any).getCodePartHex() + '6a' + this.stateHex(n, lastRoot)
  }

  /** read {n, lastRoot} back out of a bond script */
  stateOf (script: string): BondState {
    const d = extractStateFromScript(NEKTAR_CHECKPOINTBOND_ARTIFACT, script) as any
    if (!d) throw new Error('bond: not a stateful bond script')
    return { n: BigInt(d.n), lastRoot: typeof d.lastRoot === 'string' ? d.lastRoot : Buffer.from(d.lastRoot).toString('hex') }
  }

  private sigFor (tx: Transaction, script: string, satoshis: number, methodIndex: number, ownerKey: PrivateKey): string {
    const c = this.contract as any
    const sigSub = c.getSubscriptForSigning
      ? c.getSubscriptForSigning(script, methodIndex)
      : script
    const pre = computeOpPushTx(tx, 0, sigSub, satoshis).preimageHex
    return Buffer.from(ownerKey.sign(Array.from(Hash.sha256(Buffer.from(pre, 'hex')))).toDER()).toString('hex') + '41'
  }

  /** commit: continuation {n+1, newRoot} + bounded change to operator. */
  buildCommit (utxo: BondUtxo, ownerKey: PrivateKey, newRoot: string, fee = 1n): { tx: Transaction, state: BondState } {
    const cur = this.stateOf(utxo.script)
    const bondSats = BigInt(utxo.satoshis) - fee - 1n // 1 sat to the auto-change
    const next: BondState = { n: cur.n + 1n, lastRoot: newRoot }
    const contScript = (this.contract as any).getCodePartHex() + '6a' + this.stateHex(next.n, next.lastRoot)

    const tx = new Transaction()
    tx.addInput({
      sourceTXID: utxo.txid, sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(bondSats), lockingScript: LockingScript.fromHex(contScript) })
    tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.operatorPkh)) })

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 0)
    const sig = this.sigFor(tx, utxo.script, utxo.satoshis, 0, ownerKey)
    const unlock = c.buildStatefulPrefix(opSig, true) +
      this.contract.buildUnlockingScript('commit', [
        sig, ownerKey.toPublicKey().toString(), newRoot, bondSats,
        this.cfg.operatorPkh, 1n, preimageHex
      ])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx, state: next }
  }

  /** close: terminal — whole bond (minus bounded fee) back to operator after
   *  release height. tx.lockTime must be set by the caller's clock.
   *  NOTE (runar 0.4.6): the compiled frame injects _changePKH/_changeAmount
   *  slots for EVERY method of a multi-method contract even though the ABI
   *  only lists them on commit — close must carry the same padded arg frame
   *  and a 1-sat change output, or the emitted continuation check fails. */
  buildClose (utxo: BondUtxo, ownerKey: PrivateKey, lockTime: number, fee = 1n): { tx: Transaction } {
    const payoutSats = BigInt(utxo.satoshis) - fee - 1n
    const tx = new Transaction()
    tx.addInput({
      sourceTXID: utxo.txid, sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(payoutSats), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.operatorPkh)) })
    tx.addOutput({ satoshis: 1, lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.operatorPkh)) })
    tx.lockTime = lockTime

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 1)
    const sig = this.sigFor(tx, utxo.script, utxo.satoshis, 1, ownerKey)
    const unlock = c.buildStatefulPrefix(opSig, true) +
      this.contract.buildUnlockingScript('close', [
        sig, ownerKey.toPublicKey().toString(), payoutSats,
        this.cfg.operatorPkh, 1n, preimageHex
      ])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }
}
