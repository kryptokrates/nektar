// FeeVault — GOAL-v7 V1 host adapter for the MamemFeeVault covenant.
// Caps are consensus rules inside the locking script: payments only to
// `destPkh`, <= perTxCap, <= rolling windowCap per windowBlocks (nLocktime),
// fee <= maxFee. Manual covenant: 16-byte state (spentInWindow‖windowStart,
// LE) appended after OP_RETURN in the locking script.
// Spend outputs (fixed): [continuation, pay→destPkh, change→ownerPkh].
import { Transaction, PrivateKey, LockingScript, Hash } from '@bsv/sdk'
import { RunarContract, computeOpPushTx } from 'runar-sdk'
import { MAMEM_FEEVAULT_ARTIFACT } from '@mamem/contracts'
import type { Broadcaster, Funder, FundingOut } from '@mamem/chain'

export interface VaultParams {
  destPkh: string // 20B hex — sole payable address (anchor-funding wallet)
  ownerPkh: string // 20B hex — operator pubkey hash (spend auth)
  perTxCap: bigint
  windowCap: bigint
  windowBlocks: bigint
  maxFee: bigint
}
export interface VaultUtxo { txid: string, vout: number, satoshis: number, script: string }

const le8 = (n: bigint | number): string => { const b = Buffer.alloc(8); b.writeBigUInt64LE(BigInt(n)); return b.toString('hex') }
const fromLe8 = (hex: string): bigint => Buffer.from(hex, 'hex').readBigUInt64LE(0)
const p2pkhHex = (pkh: string): string => `76a914${pkh}88ac`

export class FeeVault {
  readonly params: VaultParams
  private contract: RunarContract
  constructor (params: VaultParams) {
    this.params = params
    this.contract = new RunarContract(MAMEM_FEEVAULT_ARTIFACT, [params.destPkh, params.ownerPkh, params.perTxCap, params.windowCap, params.windowBlocks, params.maxFee])
  }
  /** vault locking script = contract code + OP_RETURN + 16B state */
  lockingScript (spentInWindow = 0n, windowStart = 0n): string {
    return this.contract.getLockingScript() + '6a' + le8(spentInWindow) + le8(windowStart)
  }
  stateOf (scriptHex: string): { spentInWindow: bigint, windowStart: bigint } {
    return { spentInWindow: fromLe8(scriptHex.slice(-32, -16)), windowStart: fromLe8(scriptHex.slice(-16)) }
  }

  /** Build the covenant spend: [continuation, pay→destPkh, change→ownerPkh]. */
  buildSpend (vault: VaultUtxo, ownerKey: PrivateKey, payAmount: bigint, locktime: number, feeSats: bigint, changeSats = 1n): { tx: Transaction, newState: { spentInWindow: bigint, windowStart: bigint } } {
    const st = this.stateOf(vault.script)
    const lt = BigInt(locktime)
    const rolled = lt >= st.windowStart + this.params.windowBlocks
    const newState = { spentInWindow: rolled ? payAmount : st.spentInWindow + payAmount, windowStart: rolled ? lt : st.windowStart }
    const contSats = BigInt(vault.satoshis) - payAmount - changeSats - feeSats
    const tx = new Transaction()
    tx.addInput({ sourceTXID: vault.txid, sourceOutputIndex: vault.vout, unlockingScript: new LockingScript(), sequence: 0xfffffffe })
    tx.addOutput({ satoshis: Number(contSats), lockingScript: LockingScript.fromHex(this.lockingScript(newState.spentInWindow, newState.windowStart)) })
    tx.addOutput({ satoshis: Number(payAmount), lockingScript: LockingScript.fromHex(p2pkhHex(this.params.destPkh)) })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(p2pkhHex(this.params.ownerPkh)) })
    tx.lockTime = locktime
    const { preimageHex } = computeOpPushTx(tx, 0, vault.script, vault.satoshis)
    const digest = Hash.sha256(Hash.sha256(Buffer.from(preimageHex, 'hex')))
    const sigHex = (ownerKey.sign(Array.from(digest)).toDER('hex') as string) + '41'
    const unlock = this.contract.buildUnlockingScript('spend', [preimageHex, sigHex, ownerKey.toPublicKey().toString(), payAmount, contSats, changeSats])
    tx.inputs[0]!.unlockingScript = LockingScript.fromHex(unlock)
    return { tx, newState }
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
