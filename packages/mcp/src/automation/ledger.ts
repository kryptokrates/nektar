import { Transaction } from '@bsv/sdk'
import type { BroadcastResult } from '@nektar/chain'
import type { AutomationStore, TxState } from './store.js'
import { WalletCoordinator } from './wallet.js'
import type { ActionContext } from './policy.js'

export interface TxObservationInput { status: 'seen' | 'proven' | 'rejected' | 'conflicted', source: string, blockHeight?: number, reason?: string, spentBy?: string }

export function wrapBroadcast (store: AutomationStore, inner: (tx: Transaction) => Promise<BroadcastResult>): (tx: Transaction) => Promise<BroadcastResult> {
  const wallet = new WalletCoordinator(store, { broadcast: inner })
  return async tx => {
    const txid = tx.id('hex')
    const actionId = `owner:${txid}`
    try {
      const parents: Record<string, string> = {}
      const outs: Array<{ txid: string, vout: number, satoshis: number }> = []
      let vin = 0
      for (const inp of tx.inputs) {
        const srcTxid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
        const vout = inp.sourceOutputIndex ?? 0
        if (!srcTxid) continue
        outs.push({ txid: srcTxid, vout, satoshis: inp.sourceTransaction?.outputs[vout]?.satoshis ?? 0 })
        if (inp.sourceTransaction) { parents[srcTxid] = inp.sourceTransaction.toHex(); vin += inp.sourceTransaction.outputs[vout]?.satoshis ?? 0 }
      }
      await store.recordBroadcast({ txid, rawHex: tx.toHex(), parents, feeSats: Math.max(0, vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)), outs }, actionId, 'owner', 'owner')
    } catch { /* a ledger outage must not fabricate an authorization — the broadcaster still decides */ }
    try {
      const r = await inner(tx)
      await store.setTxState(actionId, r.status === 'success' ? 'submitted' : 'submission_unknown', { at: Date.now(), result: r }, true)
      return r
    } catch (e) {
      await store.setTxState(actionId, 'submission_unknown', { at: Date.now(), error: (e as Error).message }, true)
      throw e
    }
  }
}

export function leasePredicate (store: AutomationStore): (txid: string, vout: number) => Promise<boolean> {
  return (txid, vout) => store.leasedOutpoint(txid, vout)
}

export class TransactionWatcher {
  constructor (
    private store: AutomationStore,
    private observeFn: (txid: string) => Promise<TxObservationInput | undefined>,
    private onRejected?: (txid: string, reason: string) => Promise<void> | void
  ) {}

  async tick (): Promise<number> {
    let changed = 0
    for (const w of await this.store.walletTxs(['submitted', 'submission_unknown', 'observed'])) {
      try {
        const o = await this.observeFn(w.txid)
        if (!o) continue
        const next: TxState = o.status === 'proven' ? 'confirmed' : o.status === 'seen' ? (w.state === 'confirmed' ? 'confirmed' : 'observed') : o.status
        if (next === w.state) continue
        await this.store.setTxState(w.actionId, next, { at: Date.now(), ...o })
        changed++
        if (o.status === 'rejected' || o.status === 'conflicted') await this.onRejected?.(w.txid, o.reason ?? o.spentBy ?? o.status)
      } catch { /* unknown, not failed — try again next tick */ }
    }
    return changed
  }
}

export function ownerContext (txid: string): ActionContext {
  return { operationId: 'owner', actionId: `owner:${txid}`, policyHash: 'owner', networkId: 'owner' }
}
