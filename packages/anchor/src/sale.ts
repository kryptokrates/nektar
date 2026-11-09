// Host adapter for the V3 DisclosureSale covenant (GOAL-v7, BIP-199 shape).
// Stateless terminal contract: the BUYER funds the escrow utxo; `buy`
// reveals `wrapped` in the same tx that pays the seller (the seller
// completes unilaterally — no signature needed, the preimage is the
// claim credential); `refund` returns the deposit to the DEPOSITOR
// (buyer) after refundAfter. The seller can never take a deposit
// without revealing.
import { PrivateKey, Transaction, LockingScript, UnlockingScript, Hash, P2PKH } from '@bsv/sdk'
import { RunarContract, computeOpPushTx, buildP2PKHScript } from 'runar-sdk'
import { DISCLOSURESALE_ARTIFACT } from '@nektar/contracts'

/** The seller's published offer — depositorPkh is added by each buyer
 *  when they self-build the escrow script. */
export interface SaleConfig {
  sellerPkh: string   // 20B hex — paid on buy
  price: bigint       // sats paid to seller on buy
  hWrapped: string    // sha256(wrapped) — 32B hex
  refundAfter: bigint // locktime gate for depositor refund
  maxFee: bigint
  /** 36B hex outpoint of the ClaimVerdict UTXO — binds the escrow to a
   *  settled claim (buyGated only). Plain buy/refund escrows use the
   *  null-outpoint sentinel; the field is baked into the locking script
   *  either way, so every reconstruction must default identically. */
  verdictOutpoint?: string
}

/** null outpoint (txid all-zero, vout 0) — sentinel for ungated escrows */
export const NULL_VERDICT_OUTPOINT = '00'.repeat(36)

/** cfg + the buyer's pkh — the full escrow instance. */
export interface EscrowConfig extends SaleConfig { depositorPkh: string }

export interface SaleUtxo { txid: string, vout: number, satoshis: number, script: string }

export class DisclosureSaleHost {
  private contract: RunarContract
  constructor (private cfg: EscrowConfig) {
    this.contract = new RunarContract(DISCLOSURESALE_ARTIFACT, [
      cfg.sellerPkh, cfg.price, cfg.hWrapped, cfg.refundAfter, cfg.maxFee, cfg.depositorPkh,
      cfg.verdictOutpoint ?? NULL_VERDICT_OUTPOINT
    ])
  }

  get params (): EscrowConfig { return this.cfg }

  /** the escrow's locking script (stateless — no state suffix) */
  lockingScript (): string {
    return this.contract.getLockingScript()
  }

  /** buyer-side: the escrow script for a seller offer + my pkh — no seller
   *  involvement needed, the offer's fields are all public. */
  static escrowScript (cfg: SaleConfig, depositorPkh: string): string {
    return new DisclosureSaleHost({ ...cfg, depositorPkh }).lockingScript()
  }

  /** buyer-side: fund the escrow — signed P2PKH inputs paying `amount`
   *  into the escrow script, change back to the depositor. The deposit
   *  should be price + maxFee + a small change margin (the covenant
   *  bounds the buy fee anyway). */
  async buildEscrow (fundingUtxos: Array<{ tx: Transaction, vout: number, satoshis: number }>, buyerKey: PrivateKey, amount: number, changeTo?: string): Promise<{ tx: Transaction, utxo: SaleUtxo }> {
    const tx = new Transaction()
    let total = 0
    const p2pkh = new P2PKH()
    for (const u of fundingUtxos) {
      tx.addInput({
        sourceTransaction: u.tx, sourceOutputIndex: u.vout,
        unlockingScriptTemplate: p2pkh.unlock(buyerKey)
      })
      total += u.satoshis
    }
    tx.addOutput({ satoshis: amount, lockingScript: LockingScript.fromHex(this.lockingScript()) })
    const depositorPkh = changeTo ?? this.cfg.depositorPkh
    const change = total - amount - 250 // ~feerate headroom for the funding tx itself
    if (change < 0) throw new Error(`buildEscrow: funding utxos ${total} < amount ${amount} + fee`)
    if (change > 0) tx.addOutput({ satoshis: change, lockingScript: LockingScript.fromHex(buildP2PKHScript(depositorPkh)) })
    await tx.sign()
    return { tx, utxo: { txid: tx.id('hex'), vout: 0, satoshis: amount, script: this.lockingScript() } }
  }

  /** seller-side: reveal `wrapped` — outputs [price→seller, change→depositor].
   *  The preimage arg is the claim credential; anyone may broadcast it. */
  /** a minimal synthetic parent so the fee can resolve the input value —
   *  SpendGuard fails closed on unresolved inputs, so the escrow's own
   *  script+satoshis stand in as the source output */
  private static sourceOf (utxo: SaleUtxo): Transaction {
    const parent = new Transaction()
    parent.addOutput({ satoshis: utxo.satoshis, lockingScript: LockingScript.fromHex(utxo.script) })
    return parent
  }

  buildBuy (utxo: SaleUtxo, wrapped: string, changeSats: bigint): { tx: Transaction } {
    const tx = new Transaction()
    tx.addInput({
      sourceTransaction: DisclosureSaleHost.sourceOf(utxo), sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(this.cfg.price), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.sellerPkh)) })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.depositorPkh)) })

    const c = this.contract as any
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, 0)
    const unlock = c.buildStatefulPrefix(opSig) +
      this.contract.buildUnlockingScript('buy', [wrapped, changeSats, preimageHex])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }

  /** depositor-side: reclaim the escrow after refundAfter — depositor
   *  signed, locktime-gated, output [change→depositor]. */
  buildRefund (utxo: SaleUtxo, depositorKey: PrivateKey, lockTime: number, changeSats: bigint): { tx: Transaction } {
    const tx = new Transaction()
    tx.addInput({
      sourceTransaction: DisclosureSaleHost.sourceOf(utxo), sourceOutputIndex: utxo.vout,
      unlockingScript: new UnlockingScript(), sequence: 0xfffffffe
    })
    tx.addOutput({ satoshis: Number(changeSats), lockingScript: LockingScript.fromHex(buildP2PKHScript(this.cfg.depositorPkh)) })
    tx.lockTime = lockTime

    const c = this.contract as any
    // codeSeparator index = refund's position among PUBLIC methods (claims.ts
    // convention) — buyGated was inserted later, shifting refund to index 2
    const rIdx = DISCLOSURESALE_ARTIFACT.abi.methods.filter((m: any) => m.isPublic).findIndex((m: any) => m.name === 'refund')
    const { sigHex: opSig, preimageHex } = c.computeOpPushTxWithCodeSep(tx, 0, utxo.script, utxo.satoshis, rIdx)
    const sigSub = c.getSubscriptForSigning
      ? c.getSubscriptForSigning(utxo.script, rIdx)
      : utxo.script
    const sigPre = computeOpPushTx(tx, 0, sigSub, utxo.satoshis).preimageHex
    const sig = Buffer.from(depositorKey.sign(Array.from(Hash.sha256(Buffer.from(sigPre, 'hex')))).toDER()).toString('hex') + '41'
    const unlock = c.buildStatefulPrefix(opSig) +
      this.contract.buildUnlockingScript('refund', [sig, depositorKey.toPublicKey().toString(), changeSats, preimageHex])
    tx.inputs[0]!.unlockingScript = UnlockingScript.fromHex(unlock)
    return { tx }
  }
}
