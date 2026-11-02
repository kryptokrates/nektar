// Host adapter for the V3 DisclosureSale covenant (GOAL-v7).
// Stateless terminal contract: buy() reveals `wrapped` in the same tx that
// pays the seller; refund() returns the listing after refundAfter.
import { PrivateKey, Transaction, LockingScript, UnlockingScript, Hash } from '@bsv/sdk'
import { RunarContract, computeOpPushTx, buildP2PKHScript } from 'runar-sdk'
import { DISCLOSURESALE_ARTIFACT } from '@mamem/contracts'

export interface SaleConfig {
  sellerPkh: string   // 20B hex — paid on buy / refunded to on refund
  price: bigint       // sats paid to seller on buy
  hWrapped: string    // sha256(wrapped) — 32B hex
  refundAfter: bigint // locktime gate for refund
  maxFee: bigint
}

export interface SaleUtxo { txid: string, vout: number, satoshis: number, script: string }

export class DisclosureSaleHost {
  private contract: RunarContract
  constructor (private cfg: SaleConfig) {
    this.contract = new RunarContract(DISCLOSURESALE_ARTIFACT, [
      cfg.sellerPkh, cfg.price, cfg.hWrapped, cfg.refundAfter, cfg.maxFee
    ])
  }

  get params (): SaleConfig { return this.cfg }

  /** the listing's locking script (stateless — no state suffix) */
  lockingScript (): string {
    return this.contract.getLockingScript()
  }

  /** buy: reveal `wrapped` (preimage arg) — outputs [price→seller, change→buyer].
   *  The caller funds the tx (listing utxo may cover it all in tests). */
  buildBuy (utxo: SaleUtxo, wrapped: string, buyerPkh: string, changeSats: bigint): { tx: Transaction } {
    const tx = new Transaction()
    tx.addInput({
      sourceTXID: utxo.txid, sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(this.cfg.price), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.sellerPkh)) })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(buildP2PKHScript(buyerPkh)) })

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 0)
    const unlock = c.buildStatefulPrefix(opSig) +
      this.contract.buildUnlockingScript('buy', [wrapped, buyerPkh, changeSats, preimageHex])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }

  /** refund: seller-signed, locktime-gated — output [change→seller]. */
  buildRefund (utxo: SaleUtxo, sellerKey: PrivateKey, lockTime: number, changeSats: bigint): { tx: Transaction } {
    const tx = new Transaction()
    tx.addInput({
      sourceTXID: utxo.txid, sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.sellerPkh)) })
    tx.lockTime = lockTime

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 1)
    const sigSub = c.getSubscriptForSigning
      ? c.getSubscriptForSigning(utxo.script, 1)
      : utxo.script
    const sigPre = computeOpPushTx(tx, 0, sigSub, utxo.satoshis).preimageHex
    const sig = Buffer.from(sellerKey.sign(Array.from(Hash.sha256(Buffer.from(sigPre, 'hex')))).toDER()).toString('hex') + '41'
    const unlock = c.buildStatefulPrefix(opSig) +
      this.contract.buildUnlockingScript('refund', [sig, sellerKey.toPublicKey().toString(), changeSats, preimageHex])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }
}
