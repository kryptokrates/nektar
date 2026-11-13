// custody.ts — funding-UTXO lifecycle + spend construction extracted from
// NektarMemory: takeFunding over the three funder shapes (StackFunder.take /
// ReserveFunder.takeReserve / plain utxo peek), markSpent/addOwned
// notifications, payInvoice (BEEF hand-off to a seller's settlePayment),
// sendSats (one-call on-chain spend, wallet-custody buildOnly path), and the
// fundingStatus read. The funder itself is the custody view chosen by the
// runtime — file-backed reserve or the toolbox wallet.
import { Transaction, LockingScript, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { unlockP2PKH, signerAddress, type SignerLike } from '@nektar/core'
import type { FundingOut } from '@nektar/chain'

/** the funder shapes callers hand us: testkit take(), reserve
 *  takeReserve(), or a bare utxos list — probed, not widened */
interface FunderLike {
  take?: (satoshis: number) => Promise<FundingOut>
  takeReserve?: (spendCheck?: (txid: string, vout: number) => Promise<boolean>, minSats?: number) => Promise<FundingOut | null>
  utxos?: FundingOut[]
  markSpent?: (txid: string, vout: number) => void
  addOwned?: (tx: Transaction, vout: number) => void
}

export interface CustodyOpsDeps {
  funder: FunderLike
  fundSigner: () => SignerLike
  broadcast: (tx: Transaction) => Promise<{ status: string; description?: string }>
  /** the instance's tx graph — broadcast spends land here so change/spend
   *  ancestry resolves without an indexer */
  txs: (tx: Transaction) => void
  /** automation ledger: owner-namespace broadcast records; undefined in
   *  non-automation deployments. A getter — the runtime assigns
   *  mem.ledger AFTER the constructor runs. */
  ledger?: () =>
    | {
        recordBroadcast: (
          t: { txid: string; rawHex: string; parents: Record<string, string>; feeSats: number; outs: { txid: string; vout: number; satoshis: number }[] },
          actionId: string,
          operationId: string,
          kind: string
        ) => Promise<unknown>
        setTxState: (actionId: string, state: import('@nektar/automation').TxState, observation?: unknown, attempt?: boolean) => Promise<void>
      }
    | undefined
  network?: string
  feeRateSatPerKb?: number
}

const opReturnScript = (opReturn: string): LockingScript => {
  // OP_FALSE OP_RETURN + ascii tag push — settlePayment parses '006a'+utf8(tag)
  const tagHex = Buffer.from(opReturn, 'utf8').toString('hex')
  return LockingScript.fromHex(
    `006a${tagHex.length / 2 < 0x4c ? (tagHex.length / 2).toString(16).padStart(2, '0') : '4c' + (tagHex.length / 2).toString(16).padStart(2, '0')}${tagHex}`
  )
}

export class CustodyOps {
  constructor(private deps: CustodyOpsDeps) {}

  private changePrefix(): number[] | undefined {
    return (this.deps.network ?? 'main') === 'main' ? [0x00] : [0x6f]
  }

  /** one funding outpoint from the fee reserve (StackFunder.take / ReserveFunder.takeReserve / peeked utxo) */
  async takeFunding(satoshis: number): Promise<FundingOut> {
    const f = this.deps.funder
    if (typeof f.take === 'function') return await f.take(satoshis)
    if (typeof f.takeReserve === 'function') {
      const u = await f.takeReserve(undefined, satoshis)
      if (u) return u
    }
    const u = (f.utxos ?? []).find((u) => (u.satoshis ?? 0) >= satoshis)
    if (!u) throw new Error('no funding UTXO available for the attestation tx')
    return u
  }

  /** return a tx output to the reserve as owned (change/escrow outs) */
  addOwnedOut(tx: Transaction, vout: number): void {
    this.deps.funder.addOwned?.(tx, vout)
  }
  /** mark a spent input against the reserve after broadcast */
  markSpentOut(txid: string, vout: number): void {
    this.deps.funder.markSpent?.(txid, vout)
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  async fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }> {
    const utxos = this.deps.funder.utxos ?? []
    const prefix = this.deps.network && this.deps.network !== 'main' ? [0x6f] : undefined
    return {
      address: await signerAddress(this.deps.fundSigner(), prefix),
      balanceSats: utxos.reduce((s, u) => s + (u.satoshis ?? 0), 0),
      utxos: utxos.length,
      funded: utxos.length > 0
    }
  }

  async payInvoice(inv: {
    invoiceId: string
    amount: number
    lockingScriptHex: string
    opReturn: string
  }): Promise<{ payment: { invoiceId: string; transaction: string }; txid: string }> {
    const f = await this.takeFunding(inv.amount + 600)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: unlockP2PKH(this.deps.fundSigner()) })
    tx.addOutput({ satoshis: inv.amount, lockingScript: LockingScript.fromHex(inv.lockingScriptHex) })
    tx.addOutput({ satoshis: 0, lockingScript: opReturnScript(inv.opReturn) })
    tx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(this.deps.fundSigner(), this.changePrefix())), change: true })
    await tx.fee(new SatoshisPerKilobyte(this.deps.feeRateSatPerKb ?? 1))
    await tx.sign()
    // atomic BEEF — carries the funding parent so the seller's script
    // validation and broadcaster resolve input values (settlePayment's
    // 0101010000 branch; "BEEF preferred" per the invoice docs).
    // Serialize BEFORE marking the reserve spent — a throw here must not
    // drain the funding UTXO.
    const transaction = tx.toHexAtomicBEEF()
    const ledger = this.deps.ledger?.()
    if (ledger) {
      const txid = tx.id('hex')
      await ledger.recordBroadcast(
        {
          txid,
          rawHex: tx.toHex(),
          parents: { [f.tx.id('hex')]: f.tx.toHex() },
          feeSats: 0,
          outs: [{ txid: f.tx.id('hex'), vout: f.vout, satoshis: f.satoshis }]
        },
        `owner:${txid}`,
        'owner',
        'exported-payment'
      )
      await ledger.setTxState(`owner:${txid}`, 'exported', { invoiceId: inv.invoiceId })
    }
    this.markSpentOut(f.tx.id('hex'), f.vout)
    return { payment: { invoiceId: inv.invoiceId, transaction }, txid: tx.id('hex') }
  }

  /** one-call payment: build + sign + broadcast a plain spend to a payee
   *  address or locking script. Unlike payInvoice (which returns a BEEF for
   *  the payee's settlePayment), this airs the tx and returns its txid —
   *  the way to fund another agent's fee address. `buildOnly` returns the
   *  fee'd, template-unlocked tx without broadcasting — the wallet-custody
   *  path signs and submits it itself (market direct rail). `utxo` names
   *  the payment output so an accept envelope can point at it. */
  async sendSats(args: {
    amount: number
    address?: string
    scriptHex?: string
    opReturn?: string
    buildOnly?: boolean
  }): Promise<{ txid: string; paidSats: number; tx?: Transaction; txHex?: string; utxo?: { txid: string; vout: number; satoshis: number; script: string } }> {
    if (!args.address && !args.scriptHex) throw new Error('sendSats: address or scriptHex required')
    const script = args.scriptHex ? LockingScript.fromHex(args.scriptHex) : new P2PKH().lock(args.address!)
    const f = await this.takeFunding(args.amount + 600)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: unlockP2PKH(this.deps.fundSigner()) })
    tx.addOutput({ satoshis: args.amount, lockingScript: script })
    if (args.opReturn) tx.addOutput({ satoshis: 0, lockingScript: opReturnScript(args.opReturn) })
    tx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(this.deps.fundSigner(), this.changePrefix())), change: true })
    await tx.fee(new SatoshisPerKilobyte(this.deps.feeRateSatPerKb ?? 1))
    // tx.id/toHex serialize the unlocking script, so nothing can be read off
    // the tx until templates resolve in sign()
    await tx.sign()
    const utxo = { txid: tx.id('hex'), vout: 0, satoshis: args.amount, script: script.toHex() }
    if (args.buildOnly === true) {
      // wallet coordinator path: signed but not aired — prepare serializes
      // the tx and submit does the broadcast + ownership marks
      return { txid: utxo.txid, paidSats: args.amount, tx, txHex: tx.toHex(), utxo }
    }
    const br = await this.deps.broadcast(tx)
    if (br.status !== 'success') throw new Error(`sendSats broadcast failed: ${br.description ?? 'unknown'}`)
    this.deps.txs(tx)
    this.markSpentOut(f.tx.id('hex'), f.vout)
    this.addOwnedOut(tx, tx.outputs.length - 1)
    return { txid: tx.id('hex'), paidSats: args.amount, txHex: tx.toHex(), utxo }
  }
}
