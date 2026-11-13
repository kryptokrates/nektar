import { Transaction } from '@bsv/sdk'
import type { BroadcastResult } from '@nektar/chain'
import type { AutomationStore, WalletTx, TxState } from './store.js'
import type { ActionContext } from './policy.js'

export interface WalletDeps {
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
}

export type TxObservation =
  | { status: 'seen'; source: string }
  | { status: 'proven'; source: string; blockHeight: number }
  | { status: 'rejected'; source: string; reason: string }
  | { status: 'conflicted'; source: string; spentBy: string }

const NEVER_LEFT = /SpendGuard refused/i
export const DEFINITIVE =
  /rejected|missing inputs|mempool-conflict|double.?spen|utxo already spent|bad-txns|non-mandatory|unlocking scripts invalid|fee too low/i
const ALREADY = /already (known|in|mined|exists)|txn-already/i

export class WalletCoordinator {
  constructor(
    private store: AutomationStore,
    private deps: WalletDeps
  ) {}

  async prepare(ctx: ActionContext, kind: string, tx: Transaction): Promise<WalletTx> {
    const parents: Record<string, string> = {}
    const outs: Array<{ txid: string; vout: number; satoshis: number }> = []
    let vin = 0
    for (const inp of tx.inputs) {
      const src = inp.sourceTransaction
      const txid = inp.sourceTXID ?? src?.id('hex')
      const vout = inp.sourceOutputIndex ?? 0
      const sats = src?.outputs[vout]?.satoshis
      if (!txid || !src || sats === undefined) throw new Error('prepare: every input needs its real source transaction')
      if (src.id('hex') !== txid) throw new Error('prepare: source transaction does not match the input outpoint')
      parents[txid] = src.toHex()
      outs.push({ txid, vout, satoshis: sats })
      vin += sats
    }
    const feeSats = vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
    if (feeSats < 0) throw new Error('prepare: outputs exceed inputs')
    const txid = tx.id('hex')
    const prev = await this.store.walletTx({ actionId: ctx.actionId })
    if (prev) {
      if (prev.txid !== txid) throw new Error(`action ${ctx.actionId} already prepared a different transaction (${prev.txid})`)
      return prev
    }
    if (!(await this.store.leaseOutpoints(ctx.actionId, outs))) throw new Error('prepare: an input is already leased to another action')
    return await this.store.recordPrepared({ actionId: ctx.actionId, operationId: ctx.operationId, kind, txid, rawHex: tx.toHex(), parents, feeSats })
  }

  async prepareOwner(kind: string, tx: Transaction, actionId = `owner:${tx.id('hex')}`): Promise<WalletTx> {
    const parents: Record<string, string> = {}
    const outs: Array<{ txid: string; vout: number; satoshis: number }> = []
    let vin = 0
    for (const inp of tx.inputs) {
      const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      const vout = inp.sourceOutputIndex ?? 0
      if (!txid) throw new Error('owner: input without an outpoint')
      outs.push({ txid, vout, satoshis: inp.sourceTransaction?.outputs[vout]?.satoshis ?? 0 })
      if (inp.sourceTransaction) {
        parents[txid] = inp.sourceTransaction.toHex()
        vin += inp.sourceTransaction.outputs[vout]?.satoshis ?? 0
      }
    }
    const prev = await this.store.walletTx({ actionId })
    if (prev) return prev
    return await this.store
      .recordPrepared({
        actionId,
        operationId: 'owner',
        kind,
        txid: tx.id('hex'),
        rawHex: tx.toHex(),
        parents,
        feeSats: Math.max(0, vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0))
      })
      .then(async (w) => {
        for (const o of outs) await this.store.leaseOutpoints(actionId, [o])
        return w
      })
  }

  async rebuildTx(w: WalletTx): Promise<Transaction> {
    return this.rebuild(w)
  }

  private rebuild(w: WalletTx): Transaction {
    const tx = Transaction.fromHex(w.rawHex)
    if (tx.id('hex') !== w.txid) throw new Error(`stored bytes for ${w.actionId} do not hash to ${w.txid}`)
    for (const inp of tx.inputs) {
      const hex = inp.sourceTXID ? w.parents[inp.sourceTXID] : undefined
      if (hex) inp.sourceTransaction = Transaction.fromHex(hex)
    }
    return tx
  }

  async submit(actionId: string): Promise<{ state: TxState; txid: string; detail?: string }> {
    const w = await this.store.walletTx({ actionId })
    if (!w) throw new Error(`submit: nothing prepared for ${actionId}`)
    if (w.state !== 'prepared' && w.state !== 'submission_unknown' && w.state !== 'submitted') return { state: w.state, txid: w.txid }
    let res: BroadcastResult
    try {
      res = await this.deps.broadcast(this.rebuild(w))
    } catch (e) {
      res = { status: 'error', description: `transport: ${(e as Error).message}` }
    }
    const detail = res.description ?? ''
    if (res.status === 'success' || ALREADY.test(detail)) {
      await this.store.setTxState(actionId, 'submitted', { at: Date.now(), result: res }, true)
      return { state: 'submitted', txid: w.txid }
    }
    if (w.state === 'submitted') {
      await this.store.setTxState(actionId, 'submitted', { at: Date.now(), result: res, resubmission: true }, true)
      return { state: 'submitted', txid: w.txid, detail }
    }
    if (NEVER_LEFT.test(detail)) {
      await this.store.setTxState(actionId, 'rejected', { at: Date.now(), result: res, escaped: false }, true)
      await this.store.releaseLeases(actionId)
      return { state: 'rejected', txid: w.txid, detail }
    }
    const state: TxState = DEFINITIVE.test(detail) && !detail.startsWith('transport:') ? 'rejected' : 'submission_unknown'
    await this.store.setTxState(actionId, state, { at: Date.now(), result: res }, true)
    return { state, txid: w.txid, detail }
  }

  async observe(txid: string, o: TxObservation): Promise<TxState | undefined> {
    const w = await this.store.walletTx({ txid })
    if (!w) return undefined
    const next: TxState = o.status === 'proven' ? 'confirmed' : o.status === 'seen' ? (w.state === 'confirmed' ? 'confirmed' : 'observed') : o.status
    await this.store.setTxState(w.actionId, next, { at: Date.now(), ...o })
    return next
  }

  async resubmitPending(): Promise<Array<{ actionId: string; state: TxState }>> {
    const out: Array<{ actionId: string; state: TxState }> = []
    for (const w of await this.store.walletTxs(['prepared', 'submission_unknown']))
      out.push({ actionId: w.actionId, state: (await this.submit(w.actionId)).state })
    return out
  }
}
