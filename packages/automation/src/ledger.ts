import { Transaction } from '@bsv/sdk'
import { fullSourceTx, inputPrevout, sparse } from '@nektar/core'
import type { BroadcastResult, Funds } from '@nektar/chain'
import type { AutomationStore, WalletTx } from './store.js'
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

/** rebuild a stored row's bytes with their declared parents — the
 *  Transaction the custody effects operate on */
export function txFromRow(w: { rawHex: string; parents: Record<string, string> }): Transaction {
  const tx = Transaction.fromHex(w.rawHex)
  for (const inp of tx.inputs) {
    const hex = inp.sourceTXID !== undefined ? w.parents[inp.sourceTXID] : undefined
    if (hex !== undefined) inp.sourceTransaction = Transaction.fromHex(hex)
  }
  return tx
}

/** the ONE custody effect set — every emit site and the tx virtual object
 *  run the handlers built here so a declared intent can never commit
 *  uneffected. `afterChange` reconciles the reserve after custody changed
 *  under a dead verdict or a revive (the funding refresh hook). */
export function custodyTxEffects(d: {
  funds: Pick<Funds, 'spent' | 'died' | 'revive'>
  spendStatus?: (txid: string, vout: number) => Promise<'spent' | 'unspent' | 'unknown'>
  txLookup?: (txid: string) => Promise<Transaction | undefined>
  recordAired?: (tx: Transaction) => void | Promise<void>
  /** reconcile the reserve after custody changed under a dead verdict or a revive */
  afterChange?: () => Promise<unknown>
  log: (line: string) => void
}): TxEffects {
  // the custody mark is the durable half — a failed reconcile is logged and
  // never fails the effect (a retry would re-run the mark for nothing)
  const reconcile = async (): Promise<void> => {
    try {
      await d.afterChange?.()
    } catch (e) {
      d.log(`reserve reconcile failed: ${(e as Error).message}`)
    }
  }
  return {
    txDied: async (w) => {
      try {
        await d.funds.died(txFromRow(w))
      } catch (e) {
        d.log(`tx ${w.txid}: died() failed: ${(e as Error).message}`)
        throw e // a failed effect stays pending — the journal retries
      }
      await reconcile()
    },
    txSpent: async (w) => {
      // w is the row BEFORE the transition — a dead→live observation is the
      // counterparty-air path, the only case the reserve must reconcile
      const wasDead = ['rejected', 'expired', 'abandoned', 'conflicted'].includes(w.state)
      try {
        await d.funds.spent(txFromRow(w))
      } catch (e) {
        d.log(`tx ${w.txid}: spent() failed: ${(e as Error).message}`)
        throw e
      }
      if (wasDead) {
        d.log(`${w.txid.slice(0, 16)}… aired after our ${w.state} verdict — custody re-marked`)
        await reconcile()
      }
    },
    recordAired: async (w) => {
      try {
        await d.recordAired?.(txFromRow(w))
      } catch {
        /* day-ledger accounting is best-effort — the custody mark is the durable one. Absent dep = unguarded host = explicit no-op. */
      }
    },
    exportExpired: async (w) => {
      let freed = 0
      let inputs = 0
      try {
        const tx = Transaction.fromHex(w.rawHex)
        inputs = tx.inputs.length
        for (const inp of tx.inputs) {
          const srcTxid = inp.sourceTXID
          if (srcTxid === undefined) continue
          const vout = inp.sourceOutputIndex
          // provably-spent inputs stay dead — the export landed late or
          // something else consumed them. 'unknown' still frees: the
          // export's window already passed and the take-time probe burns a
          // later-landing spend anyway
          if ((await d.spendStatus?.(srcTxid, vout).catch(() => 'unknown' as const)) === 'spent') continue
          const parentHex: string | undefined = sparse(w.parents[srcTxid])
          const parent = parentHex !== undefined ? Transaction.fromHex(parentHex) : await d.txLookup?.(srcTxid).catch(() => undefined)
          if (parent === undefined) continue
          await d.funds.revive(parent, vout)
          freed++
        }
      } catch (e) {
        d.log(`expired export ${w.txid}: revive failed: ${(e as Error).message}`)
        throw e // a failed effect stays pending — the journal retries
      }
      d.log(`export ${w.txid} expired unaired — ${freed}/${inputs} input(s) returned to custody`)
    }
  }
}

export function wrapBroadcast(
  store: AutomationStore,
  inner: (tx: Transaction) => Promise<BroadcastResult>,
  opts: {
    /** the caller's one custody effect set — the wrapped verdicts run it */
    fx: TxEffects
    /** the caller's executor — absent = the ambient restate ingress */
    exec?: Executor
    log?: (line: string) => void
  }
): (tx: Transaction) => Promise<BroadcastResult> {
  return async (tx) => {
    const txid = tx.id('hex')
    const actionId = `owner:${txid}`
    let recorded = false
    // engine txs already carry a row under their own action — only an
    // untracked txid needs the owner:<txid> bookkeeping row
    const known = await store.walletTx({ txid }).catch(() => undefined)
    if (known === undefined) {
      try {
        const parents: Record<string, string> = {}
        const outs: Array<{ txid: string; vout: number; satoshis: number }> = []
        let vin = 0
        for (const inp of tx.inputs) {
          const prev = inputPrevout(inp)
          if (prev === undefined) continue
          const spent = sparse(inp.sourceTransaction?.outputs)?.[prev.vout]
          outs.push({ txid: prev.txid, vout: prev.vout, satoshis: spent?.satoshis ?? 0 })
          if (spent != undefined) {
            const full = fullSourceTx(inp)
            if (full !== undefined) parents[prev.txid] = full.toHex()
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
      } catch (e) {
        /* a ledger outage must not fabricate an authorization — the broadcaster still decides */
        opts.log?.(`ledger: owner record for ${txid} refused — ${(e as Error).message}`)
      }
    } else if (known.actionId === actionId) {
      // an earlier call for the same owner tx already recorded it
      recorded = true
    }
    // a row under any other action is that action's coordinator's to watch —
    // no write, no log: the wrapped call only airs bytes here
    try {
      const r = await inner(tx)
      if (recorded) {
        const w = await store.walletTx({ actionId }).catch(() => undefined)
        if (w !== undefined)
          await txEmit(
            opts.exec ?? executor(),
            store,
            w,
            r.status === 'success' ? 'submit.success' : 'submit.unknown',
            { observation: { at: Date.now(), result: r }, attempt: true },
            opts.fx
          ).catch(() => {})
      }
      return r
    } catch (e) {
      if (recorded) {
        const w = await store.walletTx({ actionId }).catch(() => undefined)
        if (w !== undefined)
          await txEmit(
            opts.exec ?? executor(),
            store,
            w,
            'submit.unknown',
            { observation: { at: Date.now(), error: (e as Error).message }, attempt: true },
            opts.fx
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
  exportedTtlMs: number | undefined,
  now: () => number = Date.now
): AsyncGenerator<{ w: WalletTx; observe: { event: string; guard?: string; observation: unknown } }> {
  const ttl = exportedTtlMs ?? WAITS.exportedTtl.ms
  const live = await store.walletTxs(['prepared', 'submitted', 'submission_unknown', 'observed', 'exported'])
  const dead = (await store.walletTxs(['rejected', 'expired', 'abandoned'])).filter((w) => now() - w.stateAt < ttl)
  for (const w of [...live, ...dead]) {
    try {
      if (w.state === 'submitted' || w.state === 'observed') await store.releaseLeases(w.actionId)
      const o = await observeFn(w.txid)
      if (o !== undefined) {
        const { event, guard } = obsEvent(o)
        yield { w, observe: { event, guard, observation: { at: now(), ...o } } }
        continue
      }
      if (w.state === 'exported' && exportedTtlMs !== undefined && now() - w.at >= exportedTtlMs) {
        yield {
          w,
          observe: { event: 'export.ttl', observation: { at: now(), reason: `exported tx unaired for ${Math.round((now() - w.at) / 60000)}min` } }
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
      now?: () => number
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
    for await (const { w, observe } of scanTxObservations(this.store, this.observeFn, this.opts.exportedTtlMs, this.opts.now)) {
      const r = await txEmit(this.opts.exec ?? executor(), this.store, w, observe.event, { guard: observe.guard, observation: observe.observation }, fx)
      if (r.next !== w.state) changed++
    }
    return changed
  }
}
