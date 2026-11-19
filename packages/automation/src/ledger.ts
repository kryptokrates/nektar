import { Transaction } from '@bsv/sdk'
import type { BroadcastResult } from '@nektar/chain'
import type { AutomationStore, TxState, WalletTx } from './store.js'
import { WalletCoordinator } from './wallet.js'

export interface TxObservationInput {
  status: 'seen' | 'proven' | 'rejected' | 'conflicted'
  source: string
  blockHeight?: number
  reason?: string
  spentBy?: string
}

export function wrapBroadcast(store: AutomationStore, inner: (tx: Transaction) => Promise<BroadcastResult>): (tx: Transaction) => Promise<BroadcastResult> {
  const wallet = new WalletCoordinator(store, { broadcast: inner })
  return async (tx) => {
    const txid = tx.id('hex')
    const actionId = `owner:${txid}`
    let recorded = false
    try {
      const parents: Record<string, string> = {}
      const outs: Array<{ txid: string; vout: number; satoshis: number }> = []
      let vin = 0
      for (const inp of tx.inputs) {
        let srcTxid = inp.sourceTXID
        if (srcTxid === undefined && inp.sourceTransaction !== undefined) {
          try {
            srcTxid = inp.sourceTransaction.id('hex')
          } catch {
            /* sparse EF stand-in — only the spent output is carried */
          }
        }
        const vout = inp.sourceOutputIndex ?? 0
        if (!srcTxid) continue
        // EF wires carry sparse parents — the spent output entry can be null
        const spent = inp.sourceTransaction?.outputs?.[vout]
        outs.push({ txid: srcTxid, vout, satoshis: spent?.satoshis ?? 0 })
        if (spent !== undefined && spent !== null) {
          try {
            parents[srcTxid] = inp.sourceTransaction!.toHex()
          } catch {
            /* spent output known, full parent bytes are not — hex stays out */
          }
          vin += spent.satoshis ?? 0
        }
      }
      await store.recordBroadcast(
        { txid, rawHex: tx.toHex(), parents, feeSats: Math.max(0, vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)), outs },
        actionId,
        'owner',
        'owner'
      )
      recorded = true
    } catch {
      /* a ledger outage must not fabricate an authorization — the broadcaster still decides */
    }
    try {
      const r = await inner(tx)
      if (recorded)
        await store.setTxState(actionId, r.status === 'success' ? 'submitted' : 'submission_unknown', { at: Date.now(), result: r }, true).catch(() => {})
      return r
    } catch (e) {
      if (recorded) await store.setTxState(actionId, 'submission_unknown', { at: Date.now(), error: (e as Error).message }, true).catch(() => {})
      throw e
    }
  }
}

export function leasePredicate(store: AutomationStore): (txid: string, vout: number) => Promise<boolean> {
  return (txid, vout) => store.leasedOutpoint(txid, vout)
}

/** today's SpendGuard records split by purpose: txids the wallet ledger
 *  knows as escrow/payment are deal spend; everything else is upkeep
 *  (mints, anchors, provisions). Doctor + `fund` report the split so an
 *  upkeep-heavy day can't silently eat the deal budget */
export async function splitDailySpend(
  store: AutomationStore,
  costs: Array<{ txid: string; spend: number; at: string }>,
  now = Date.now()
): Promise<{ deal: number; upkeep: number }> {
  const day = new Date(now).toISOString().slice(0, 10)
  let deal = 0,
    upkeep = 0
  for (const c of costs.filter((c) => c.at.startsWith(day))) {
    const w = await store.walletTx({ txid: c.txid }).catch(() => undefined)
    if (w !== undefined && (w.kind === 'escrow' || w.kind === 'payment')) deal += c.spend
    else upkeep += c.spend
  }
  return { deal, upkeep }
}

export class TransactionWatcher {
  constructor(
    private store: AutomationStore,
    private observeFn: (txid: string) => Promise<TxObservationInput | undefined>,
    /** terminal death of a tracked tx — the row travels so the handler can
     *  rebuild the tx (parents are stored) and free its custody */
    private onRejected?: (w: WalletTx, reason: string) => Promise<void> | void,
    private opts?: {
      /** ms an 'exported' row may sit unaired before its leases release and
       *  custody takes its inputs back — the counterparty's window to air */
      exportedTtlMs?: number
      /** fired once when an export expires — the caller revives whatever
       *  inputs the chain still shows live */
      onExportExpired?: (w: WalletTx) => Promise<void> | void
    }
  ) {}

  async tick(): Promise<number> {
    let changed = 0
    for (const w of await this.store.walletTxs(['submitted', 'submission_unknown', 'observed', 'exported'])) {
      try {
        // a lease means 'we may still air these bytes' — once the tx is
        // seen or submitted its inputs belong to custody's bookkeeping,
        // and stale leases from killed runs only wedge the change chain
        if (w.state === 'submitted' || w.state === 'observed') await this.store.releaseLeases(w.actionId)
        const o = await this.observeFn(w.txid)
        if (o !== undefined) {
          const next: TxState = o.status === 'proven' ? 'confirmed' : o.status === 'seen' ? (w.state === 'confirmed' ? 'confirmed' : 'observed') : o.status
          if (next !== w.state) {
            await this.store.setTxState(w.actionId, next, { at: Date.now(), ...o })
            changed++
            if (o.status === 'rejected' || o.status === 'conflicted') await this.onRejected?.(w, o.reason ?? o.spentBy ?? o.status)
            if (next === 'confirmed' || next === 'rejected' || next === 'conflicted') await this.store.releaseLeases(w.actionId)
          }
          continue
        }
        // a signed payment handed to a counterparty gets one window to air —
        // without this 'exported' is a one-way door: the inputs sit in the
        // wallet as relinquished AND leased, stranded silently forever
        if (w.state === 'exported' && this.opts?.exportedTtlMs !== undefined && Date.now() - w.at >= this.opts.exportedTtlMs) {
          await this.store.releaseLeases(w.actionId)
          await this.store.setTxState(w.actionId, 'expired', {
            at: Date.now(),
            reason: `exported tx unaired for ${Math.round((Date.now() - w.at) / 60000)}min`
          })
          changed++
          await this.opts.onExportExpired?.(w)
        }
      } catch {
        /* unknown, not failed — try again next tick */
      }
    }
    return changed
  }
}
