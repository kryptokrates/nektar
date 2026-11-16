// custody.ts — funding-UTXO lifecycle + spend construction extracted from
// NektarMemory: purpose-labeled takes over the Funds owner, payInvoice
// (BEEF hand-off to a seller's settlePayment), sendSats (one-call on-chain
// spend, wallet-custody buildOnly path), and the fundingStatus read. The
// Funds impl is the custody view chosen by the runtime — wallet basket or
// memory pool — and callers never mark spent or owned directly.
import { Transaction, LockingScript, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { unlockP2PKH, signerAddress, type SignerLike } from '@nektar/core'
import type { FundingOut, Funds, FundPurpose } from '@nektar/chain'

export interface CustodyOpsDeps {
  /** the single owner of coin state — selection, reservation, spend marks,
   *  abandonment and adoption live behind take/spent/died/adopt */
  funds: Funds
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
  txStatus?: (txid: string) => Promise<string>
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

  /** one reserved funding outpoint — take() holds it until spent() consumes
   *  it or died()/release() frees it; purpose labels the allocation class */
  async takeFunding(purpose: FundPurpose, satoshis: number): Promise<FundingOut> {
    return await this.deps.funds.take(purpose, satoshis)
  }

  /** Bounded submit confirmation, not a wait loop: up to 3 reads (now,
   *  +700ms, +1500ms). A rejection-family answer is returned immediately, a
   *  seen-family answer settles at once, anything else resolves after the
   *  third read. Without a txStatus dep the check is a no-op. */
  private async accepted(txid: string): Promise<string | undefined> {
    const txStatus = this.deps.txStatus
    if (txStatus === undefined) return undefined
    const SEEN = ['RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'MINED', 'IMMUTABLE']
    for (let i = 0; i < 3; i++) {
      if (i === 1) await new Promise((r) => setTimeout(r, 700))
      if (i === 2) await new Promise((r) => setTimeout(r, 1500))
      let st = ''
      try {
        st = (await txStatus(txid)).toUpperCase()
      } catch {
        st = ''
      }
      if (st === 'REJECTED' || st.startsWith('DOUBLE_SPEND')) return st
      if (SEEN.includes(st)) return undefined
    }
    return undefined
  }

  /** a successful broadcast consumed our inputs and made our change */
  async spentTx(tx: Transaction): Promise<void> {
    await this.deps.funds.spent(tx)
  }

  /** the tx left our hands unbroadcast (an exported payment) — its inputs
   *  are gone, but its change must NOT come back: an unbroadcast spend that
   *  adopted its own change would mint phantom coins. If the tx ever lands,
   *  the reconcile path adopts the live outputs. */
  private async consumedInputs(tx: Transaction): Promise<void> {
    for (const inp of tx.inputs) {
      const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      if (txid !== undefined) await this.deps.funds.drop(txid, inp.sourceOutputIndex)
    }
  }
  /** coins sent to us by someone else */
  async adoptOut(tx: Transaction, vout: number): Promise<void> {
    await this.deps.funds.adopt(tx, vout)
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  async fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }> {
    const prefix = this.deps.network && this.deps.network !== 'main' ? [0x6f] : undefined
    const balanceSats = await this.deps.funds.balance()
    return {
      address: await signerAddress(this.deps.fundSigner(), prefix),
      balanceSats,
      utxos: (await this.deps.funds.list()).length,
      funded: balanceSats > 0
    }
  }

  async payInvoice(inv: {
    invoiceId: string
    amount: number
    lockingScriptHex: string
    opReturn: string
  }): Promise<{ payment: { invoiceId: string; transaction: string }; txid: string }> {
    const f = await this.takeFunding('deal', inv.amount + 600)
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
    await this.consumedInputs(tx)
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
    const f = await this.takeFunding('deal', args.amount + 600)
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
    if (br.status !== 'success') {
      this.deps.funds.release(f.tx.id('hex'), f.vout)
      throw new Error(`sendSats broadcast failed: ${br.description ?? 'unknown'}`)
    }
    const rejected = await this.accepted(tx.id('hex'))
    if (rejected !== undefined) {
      this.deps.funds.release(f.tx.id('hex'), f.vout)
      throw new Error(`sendSats rejected after submit: ${rejected}`)
    }
    this.deps.txs(tx)
    await this.spentTx(tx)
    return { txid: tx.id('hex'), paidSats: args.amount, txHex: tx.toHex(), utxo }
  }

  /** one transaction that provisions a new identity: arbitrary lane-locked
   *  outputs plus a plain P2PKH funds leg — same funding/sign/guarded
   *  broadcast path as sendSats */
  async provisionPay(args: {
    outputs: Array<{ satoshis: number; scriptHex: string }>
    address?: string
    fundsSats?: number
  }): Promise<{ txid: string; hex: string; outputs: number; fee: number }> {
    if (args.outputs.length === 0) throw new Error('provisionPay: at least one output required')
    if (args.outputs.length > 1000) throw new Error('provisionPay: too many outputs (max 1000)')
    for (const o of args.outputs) {
      if (!Number.isInteger(o.satoshis) || o.satoshis < 1 || o.satoshis > 10_000) throw new Error('provisionPay: output satoshis must be an integer 1..10000')
    }
    if (args.fundsSats !== undefined && (!Number.isInteger(args.fundsSats) || args.fundsSats < 546))
      throw new Error('provisionPay: fundsSats must be an integer >= 546')
    const total = args.outputs.reduce((s, o) => s + o.satoshis, 0) + (args.fundsSats ?? 0)
    const f = await this.takeFunding('deal', total + 600)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: unlockP2PKH(this.deps.fundSigner()) })
    for (const o of args.outputs) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.scriptHex) })
    if (args.fundsSats !== undefined && args.address !== undefined) tx.addOutput({ satoshis: args.fundsSats, lockingScript: new P2PKH().lock(args.address) })
    tx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(this.deps.fundSigner(), this.changePrefix())), change: true })
    await tx.fee(new SatoshisPerKilobyte(this.deps.feeRateSatPerKb ?? 1))
    await tx.sign()
    const br = await this.deps.broadcast(tx)
    if (br.status !== 'success') {
      this.deps.funds.release(f.tx.id('hex'), f.vout)
      throw new Error(`provisionPay broadcast failed: ${br.description ?? 'unknown'}`)
    }
    const rejected = await this.accepted(tx.id('hex'))
    if (rejected !== undefined) {
      this.deps.funds.release(f.tx.id('hex'), f.vout)
      throw new Error(`provisionPay rejected after submit: ${rejected}`)
    }
    this.deps.txs(tx)
    await this.spentTx(tx)
    const fee = (f.tx.outputs[f.vout].satoshis ?? 0) - (tx.outputs[tx.outputs.length - 1].satoshis ?? 0) - total
    return { txid: tx.id('hex'), hex: tx.toHex(), outputs: tx.outputs.length - 1, fee }
  }
}
