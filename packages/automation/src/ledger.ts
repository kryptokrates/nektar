import { Transaction } from '@bsv/sdk'
import { sparse } from '@nektar/core'
import type { BroadcastResult } from '@nektar/chain'
import type { AutomationStore, WalletTx } from './store.js'
import { WalletCoordinator } from './wallet.js'
import { replayTxEffects, type TxEffects } from './machines/effects.js'
import { obsEvent } from './machines/driver.js'
import { executor, type Executor } from './rt/exec.js'
import { txEmit } from './rt/emit.js'
import { WAITS } from './waits.js'

export interface TxObservationInput {
  status: 'seen' | 'proven' | 'rejected' | 'conflicted' | 'abandoned'
  source: string
  blockHeight?: number
  reason?: string
  spentBy?: string
}

export function wrapBroadcast(
  store: AutomationStore,
  inner: (tx: Transaction) => Promise<BroadcastResult>,
  /** the caller's executor — absent = the ambient restate ingress */
  exec?: Executor
): (tx: Transaction) => Promise<BroadcastResult> {
  const _wallet = new WalletCoordinator(store, { broadcast: inner, exec })
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
        const vout = sparse(inp.sourceOutputIndex) ?? 0
        if (!srcTxid) continue
        // EF wires carry sparse parents — the spent output entry can be null
        const spent = sparse(inp.sourceTransaction?.outputs)?.[vout]
        outs.push({ txid: srcTxid, vout, satoshis: spent?.satoshis ?? 0 })
        if (spent != undefined) {
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
      if (recorded) {
        const w = await store.walletTx({ actionId }).catch(() => undefined)
        if (w !== undefined)
          await txEmit(
            exec ?? executor(),
            store,
            w,
            r.status === 'success' ? 'submit.success' : 'submit.unknown',
            { observation: { at: Date.now(), result: r }, attempt: true },
            {}
          ).catch(() => {})
      }
      return r
    } catch (e) {
      if (recorded) {
        const w = await store.walletTx({ actionId }).catch(() => undefined)
        if (w !== undefined)
          await txEmit(
            exec ?? executor(),
            store,
            w,
            'submit.unknown',
            { observation: { at: Date.now(), error: (e as Error).message }, attempt: true },
            {}
          ).catch(() => {})
      }
      throw e
    }
  }
}

/** the sweep's scan half: live+dead rows × observeFn → (row, observe)
 *  pairs — the watcher tick emits them serially in-process (each emit
 *  rides the caller's executor); the sweep service sends each to the
 *  row's own tx object (per-key serialized, journaled invocations).
 *  Release-lease bookkeeping rides the scan. */
export async function* scanTxObservations(
  store: AutomationStore,
  observeFn: (txid: string) => Promise<TxObservationInput | undefined>,
  exportedTtlMs: number | undefined
): AsyncGenerator<{ w: WalletTx; observe: { event: string; guard?: string; observation: unknown } }> {
  const ttl = exportedTtlMs ?? WAITS.exportedTtl.ms
  const live = await store.walletTxs(['submitted', 'submission_unknown', 'observed', 'exported'])
  const dead = (await store.walletTxs(['rejected', 'expired', 'abandoned'])).filter((w) => Date.now() - w.stateAt < ttl)
  for (const w of [...live, ...dead]) {
    try {
      if (w.state === 'submitted' || w.state === 'observed') await store.releaseLeases(w.actionId)
      const o = await observeFn(w.txid)
      if (o !== undefined) {
        const { event, guard } = obsEvent(o)
        yield { w, observe: { event, guard, observation: { at: Date.now(), ...o } } }
        continue
      }
      if (w.state === 'exported' && exportedTtlMs !== undefined && Date.now() - w.at >= exportedTtlMs) {
        yield {
          w,
          observe: { event: 'export.ttl', observation: { at: Date.now(), reason: `exported tx unaired for ${Math.round((Date.now() - w.at) / 60000)}min` } }
        }
      }
    } catch {
      /* unknown, not failed — try again next tick */
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
    private opts: {
      /** ms an 'exported' row may sit unaired before its leases release and
       *  custody takes its inputs back — the counterparty's window to air */
      exportedTtlMs?: number
      /** the table's intent names resolve to these post-commit calls — the
       *  watcher is a pure effector: observe → emit → intents */
      effects: TxEffects
      /** the caller's executor — the driver's emits ride it (an injected
       *  cfg.exec Dispatcher included); absent = the ambient restate ingress */
      exec?: Executor
    }
  ) {}

  /** the watcher is a pure effector — enumerate, observe, emit through
   *  txStep, let the table's intents run custody. The verdict mapping lives
   *  in obsEvent; observation sources stay verified-only upstream in
   *  observeFn. */
  async tick(): Promise<number> {
    const fx = this.opts.effects
    // every sweep retries committed-but-uneffected intents first — the
    // crash window between commit and effect dispatch self-heals here
    await replayTxEffects(this.store, fx).catch(() => 0)
    let changed = 0
    for await (const { w, observe } of scanTxObservations(this.store, this.observeFn, this.opts.exportedTtlMs)) {
      const r = await txEmit(this.opts.exec ?? executor(), this.store, w, observe.event, { guard: observe.guard, observation: observe.observation }, fx)
      if (r.next !== w.state) changed++
    }
    return changed
  }
}
