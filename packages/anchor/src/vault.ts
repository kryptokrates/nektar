// Host adapter for the V1 MamemFeeVault covenant (GOAL-v7).
// The contract is a StatefulSmartContract: mutable props carry the window
// state; the SDK serializes state / builds scripts / computes the
// OP_PUSH_TX preimage — no hand-assembled script bytes here.
import { PrivateKey, Transaction, LockingScript, UnlockingScript, Hash } from '@bsv/sdk'
import {
  RunarContract, computeOpPushTx, serializeState,
  extractStateFromScript, buildP2PKHScript
} from 'runar-sdk'
import type { Broadcaster, Funder, FundingOut } from '@mamem/chain'
import { MAMEM_FEEVAULT_ARTIFACT } from '@mamem/contracts'

/** GOAL-v7 name kept for compatibility. */
export type VaultParams = VaultConfig

export interface VaultConfig {
  destPkh: string    // 20B hex — the only payable address (anchor funding wallet)
  ownerPkh: string   // 20B hex — operator key
  perTxCap: bigint
  windowCap: bigint
  windowBlocks: bigint
  maxFee: bigint
}

export interface VaultUtxo { txid: string, vout: number, satoshis: number, script: string }

export class FeeVault {
  private contract: RunarContract
  constructor (private cfg: VaultConfig) {
    this.contract = new RunarContract(MAMEM_FEEVAULT_ARTIFACT, [
      cfg.destPkh, cfg.ownerPkh, cfg.perTxCap, cfg.windowCap,
      cfg.windowBlocks, cfg.maxFee
    ])
  }

  get params (): VaultConfig { return this.cfg }

  /** vault locking script = code part + OP_RETURN + serialized state */
  lockingScript (spentInWindow = 0n, windowStart = 0n): string {
    return (this.contract as any).getCodePartHex() + '6a' + this.stateHex(spentInWindow, windowStart)
  }

  private stateHex (spentInWindow: bigint, windowStart: bigint): string {
    const c = this.cfg
    return serializeState(MAMEM_FEEVAULT_ARTIFACT.stateFields!, {
      destPkh: c.destPkh, ownerPkh: c.ownerPkh, perTxCap: c.perTxCap,
      windowCap: c.windowCap, windowBlocks: c.windowBlocks, maxFee: c.maxFee,
      spentInWindow, windowStart
    })
  }

  /** read {spentInWindow, windowStart} back out of a vault script */
  stateOf (script: string): { spentInWindow: bigint, windowStart: bigint } {
    const d = extractStateFromScript(MAMEM_FEEVAULT_ARTIFACT, script) as any
    if (!d) throw new Error('vault: not a stateful vault script')
    return { spentInWindow: BigInt(d.spentInWindow), windowStart: BigInt(d.windowStart) }
  }

  /** build a vault spend tx: payment → destPkh, continuation, change → ownerPkh.
   *  Returns { tx } — the caller broadcasts it (MockChain or regtest). */
  buildSpend (utxo: VaultUtxo, ownerKey: PrivateKey, payAmount: bigint, lockTime: number, fee = 1n): { tx: Transaction } {
    const cur = this.stateOf(utxo.script)
    const lt = BigInt(lockTime)
    const [newSpent, newStart] = lt >= cur.windowStart + this.cfg.windowBlocks
      ? [payAmount, lt] : [cur.spentInWindow + payAmount, cur.windowStart]

    // continuation script = code part + serialized new state
    const contScript = (this.contract as any).getCodePartHex() + '6a' + this.stateHex(newSpent, newStart)

    const ownerPkhScript = buildP2PKHScript(this.cfg.ownerPkh) // pkh-based script
    const destScript = buildP2PKHScript(this.cfg.destPkh)
    const contSats = BigInt(utxo.satoshis) - payAmount - fee - 1n // 1 sat to change
    const changeSats = 1n

    const tx = new Transaction()
    tx.addInput({
      sourceTXID: utxo.txid, sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(contSats), lockingScript: LockingScript.fromHex(contScript) })
    tx.addOutput({ satoshis: Number(payAmount), lockingScript: LockingScript.fromHex(destScript) })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(ownerPkhScript) })
    tx.lockTime = lockTime

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep
      ? c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 0)
      : computeOpPushTx(tx, 0, utxo.script, utxo.satoshis, MAMEM_FEEVAULT_ARTIFACT.codeSeparatorIndices?.[0] ?? 0)
    // user sig: checkSig runs after OP_CODESEPARATOR → sign over the trimmed subscript
    const sigSub = c.getSubscriptForSigning
      ? c.getSubscriptForSigning(utxo.script, 0)
      : utxo.script
    const sigPreimage = computeOpPushTx(tx, 0, sigSub, utxo.satoshis).preimageHex
    const sig = Buffer.from(ownerKey.sign(Array.from(Hash.sha256(Buffer.from(sigPreimage, 'hex')))).toDER()).toString('hex') + '41'
    // needsCodePart=true: this method is non-terminal stateful with _changePKH —
    // the SDK pushes the code part as a witness item before the opSig.
    const unlock = c.buildStatefulPrefix(opSig, true) +
      this.contract.buildUnlockingScript('spend', [
        sig, ownerKey.toPublicKey().toString(), payAmount, newSpent, newStart, contSats,
        this.cfg.ownerPkh, changeSats, preimageHex
      ])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }
}

/** Funder that mints capped funding UTXOs through the vault covenant. */
export class VaultFunder implements Funder {
  constructor (
    private vault: FeeVault,
    private ownerKey: PrivateKey,
    private broadcaster: Broadcaster,
    private current: () => Promise<VaultUtxo>,
    private height: () => Promise<number>,
    private feeSats: bigint = 2n
  ) {}
  async address (): Promise<string> { return this.vault.params.destPkh }
  async take (satoshis: number): Promise<FundingOut> {
    const u = await this.current()
    const pay = BigInt(satoshis) + this.feeSats
    const { tx } = this.vault.buildSpend(u, this.ownerKey, pay, await this.height(), this.feeSats)
    const res = await this.broadcaster.broadcast(tx)
    if (res.status !== 'success') throw new Error(`vault spend refused: ${res.description ?? res.status}`)
    return { tx, vout: 1, satoshis: Number(pay) }
  }
}
