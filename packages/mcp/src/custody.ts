// custody.ts — spend construction over the one Funds lifecycle: take()
// reserves the outpoint, spent() consumes a broadcast's inputs and adopts
// its change, died() frees a spend that never landed, exported() signs
// inputs away without adopting change (payInvoice's BEEF hand-off). The
// Funds impl is the custody view the runtime chose — file-backed reserve,
// toolbox wallet, or the test-tier adapter.
import { Transaction, LockingScript, MerklePath, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { unlockP2PKH, signerAddress, type SignerLike } from '@nektar/core'
import type { FundingOut, Funds, FundsPurpose } from '@nektar/chain'

export interface CustodyOpsDeps {
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
  /** the proof the instance already holds for a txid (verified on delivery
   *  into TxStore) — attached to a funding parent so a tree minted on it can
   *  serve zero-conf. Never a network lookup: hand over only what we hold. */
  proofOf?: (txid: string) => MerklePath | undefined
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
  constructor(private deps: CustodyOpsDeps) {
    // hint-only pass-over plugs into the Funds selection: a candidate whose
    // parent the broadcaster calls REJECTED/DOUBLE_SPEND is skipped for this
    // take, but the hint writes nothing — custody state stays first-party.
    const txStatus = deps.txStatus
    if (txStatus !== undefined) {
      deps.funds.hintSkip = async (tx: Transaction): Promise<boolean> => {
        if (tx.merklePath !== undefined) return false
        const txid = tx.id('hex')
        if (deps.proofOf?.(txid) !== undefined) return false
        try {
          const st = (await txStatus(txid)).toUpperCase()
          return st === 'REJECTED' || st.startsWith('DOUBLE_SPEND')
        } catch {
          return false
        }
      }
    }
  }

  private changePrefix(): number[] | undefined {
    return (this.deps.network ?? 'main') === 'main' ? [0x00] : [0x6f]
  }

  /** one funding outpoint reserved for `purpose` — the Funds impl picks the
   *  smallest sufficient candidate, the held proof attaches for ancestry */
  async takeFunding(satoshis: number, purpose: FundsPurpose = 'publish'): Promise<FundingOut> {
    const u = await this.deps.funds.take(purpose, satoshis)
    // the proof travels with the funds: a path the instance already holds for
    // the funding parent attaches here so a tree minted on it is anchored.
    // Held stock only — never fetched, never overwritten.
    if (u.tx.merklePath === undefined) {
      const mp = this.deps.proofOf?.(u.tx.id('hex'))
      if (mp !== undefined) u.tx.merklePath = MerklePath.fromBinary(mp.toBinary())
    }
    return u
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

  /** a broadcast we made: consume its inputs, adopt outputs paying us — the
   *  Funds impl decides which outputs lock to our address */
  async recordSpend(tx: Transaction): Promise<void> {
    await this.deps.funds.spent(tx)
  }
  /** the spend never landed (rejected/abandoned): inputs free, outputs tombstoned */
  async recordDeath(tx: Transaction): Promise<void> {
    await this.deps.funds.died(tx)
  }
  /** coins sent by someone else */
  async recordAdopt(tx: Transaction, vout: number): Promise<boolean> {
    return await this.deps.funds.adopt(tx, vout)
  }

  /** fee-reserve status for the fundingStatus MCP tool — mem-side view only
   *  (address + tracked UTXOs); spend caps/runway live in ops.wallet() which
   *  the tool prefers when the runtime passes it through. */
  async fundingStatus(): Promise<{ address: string; balanceSats: number; utxos: number; funded: boolean }> {
    const utxos = await this.deps.funds.outs()
    return {
      address: await this.deps.funds.address(),
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
    const f = await this.takeFunding(inv.amount + 600, 'deal')
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
    // the signed payment goes to the seller to air: inputs are consumed but
    // our change is adopted only when discovery sees the tx land
    await this.deps.funds.exported(tx)
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
    const f = await this.takeFunding(args.amount + 600, 'deal')
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
      await this.deps.funds.died(tx)
      throw new Error(`sendSats broadcast failed: ${br.description ?? 'unknown'}`)
    }
    const rejected = await this.accepted(tx.id('hex'))
    if (rejected !== undefined) {
      await this.deps.funds.died(tx)
      throw new Error(`sendSats rejected after submit: ${rejected}`)
    }
    this.deps.txs(tx)
    await this.deps.funds.spent(tx)
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
    const f = await this.takeFunding(total + 600)
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: f.tx, sourceOutputIndex: f.vout, unlockingScriptTemplate: unlockP2PKH(this.deps.fundSigner()) })
    for (const o of args.outputs) tx.addOutput({ satoshis: o.satoshis, lockingScript: LockingScript.fromHex(o.scriptHex) })
    if (args.fundsSats !== undefined && args.address !== undefined) tx.addOutput({ satoshis: args.fundsSats, lockingScript: new P2PKH().lock(args.address) })
    tx.addOutput({ lockingScript: new P2PKH().lock(await signerAddress(this.deps.fundSigner(), this.changePrefix())), change: true })
    await tx.fee(new SatoshisPerKilobyte(this.deps.feeRateSatPerKb ?? 1))
    await tx.sign()
    const br = await this.deps.broadcast(tx)
    if (br.status !== 'success') {
      await this.deps.funds.died(tx)
      throw new Error(`provisionPay broadcast failed: ${br.description ?? 'unknown'}`)
    }
    const rejected = await this.accepted(tx.id('hex'))
    if (rejected !== undefined) {
      await this.deps.funds.died(tx)
      throw new Error(`provisionPay rejected after submit: ${rejected}`)
    }
    this.deps.txs(tx)
    await this.deps.funds.spent(tx)
    const fee = (f.tx.outputs[f.vout].satoshis ?? 0) - (tx.outputs[tx.outputs.length - 1].satoshis ?? 0) - total
    return { txid: tx.id('hex'), hex: tx.toHex(), outputs: tx.outputs.length - 1, fee }
  }
}
