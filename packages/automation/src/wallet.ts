import { Transaction } from '@bsv/sdk'
import { sparse } from '@nektar/core'
import { ALREADY, broadcastProvablyDead, DEFINITIVE, NEVER_LEFT, type BroadcastResult } from '@nektar/chain'
import type { AutomationStore, WalletTx, TxState } from './store.js'
import type { ActionContext } from './policy.js'
import { replayTxEffects, type TxEffects } from './machines/effects.js'
import { executor, PermanentError, type Executor } from './rt/exec.js'
import { txEmit } from './rt/emit.js'
import { obsEvent } from './machines/driver.js'
import { OPS_TABLE } from './machines/ops.machine.js'

/** the ops table's terminal set — an op past one of these states is over:
 *  re-airing its wallet tx is broadcaster quota spent on a dead deal */
const TERMINAL_OPS = new Set(OPS_TABLE.terminal)

/** the resubmit scan's attempt bound — past it the row stops re-airing and
 *  the abandon horizon owns the verdict: a tx that can't land in ~60
 *  broadcasts (≈1h at the resubmit cadence) isn't going to, and every
 *  cadence-air is quota the fleet could spend on live rows. The watcher
 *  still observes the row — stopping airs is not stopping the watch. */
const RESUBMIT_BOUND = 60

export interface WalletDeps {
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** this runtime's executor — preferred over the ambient executor() so an
   *  injected cfg.exec (test Dispatcher) routes the emits too; absent = the
   *  ambient restate ingress */
  exec?: Executor
  /** the host's one custody effect set (custodyTxEffects) — the table
   *  declares WHICH intents fire (first-party txDied on a dead verdict —
   *  a SpendGuard-refused tx has no chain presence for `deadParent` to
   *  read, so this ledger's verdict is the only first-party signal a
   *  phantom parent's children can wedge on — txSpent on air and on
   *  dead→live revive, recordAired on the refused-but-landed path); the
   *  effect set answers 'how'. Same-key emits inside a routed handler run
   *  it directly, so coordinator and object never disagree. */
  fx: TxEffects
}

export type TxObservation =
  | { status: 'seen'; source: string }
  | { status: 'proven'; source: string; blockHeight: number }
  | { status: 'rejected'; source: string; reason: string }
  | { status: 'conflicted'; source: string; spentBy: string }
  | { status: 'abandoned'; source: string; reason?: string }

// the broadcast-verdict vocabulary lives in @nektar/chain (verdict.ts) — it
// interprets what chain broadcasters emit and bstack's finger chain gates on
// the same doctrine. Re-exported here for existing import sites.
export { NEVER_LEFT, DEFINITIVE, broadcastProvablyDead }

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
      const vout = sparse(inp.sourceOutputIndex) ?? 0
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
    // lease+record in one transaction — a crash between the two manufactures
    // a 'leased'-with-no-tx row take() frees but prepare() refuses forever
    return await this.store.recordPrepared({ actionId: ctx.actionId, operationId: ctx.operationId, kind, txid, rawHex: tx.toHex(), parents, feeSats }, outs)
  }

  async prepareOwner(kind: string, tx: Transaction, actionId = `owner:${tx.id('hex')}`): Promise<WalletTx> {
    const parents: Record<string, string> = {}
    const outs: Array<{ txid: string; vout: number; satoshis: number }> = []
    let vin = 0
    for (const inp of tx.inputs) {
      const txid = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
      const vout = sparse(inp.sourceOutputIndex) ?? 0
      if (!txid) throw new Error('owner: input without an outpoint')
      outs.push({ txid, vout, satoshis: inp.sourceTransaction?.outputs[vout]?.satoshis ?? 0 })
      if (inp.sourceTransaction) {
        parents[txid] = inp.sourceTransaction.toHex()
        vin += inp.sourceTransaction.outputs[vout]?.satoshis ?? 0
      }
    }
    const prev = await this.store.walletTx({ actionId })
    if (prev) return prev
    // atomic lease+record — the old shape recorded first then leased
    // per-outpoint ignoring the result: a conflict left the tx row written
    // with inputs still claimable elsewhere
    return await this.store.recordPrepared(
      {
        actionId,
        operationId: 'owner',
        kind,
        txid: tx.id('hex'),
        rawHex: tx.toHex(),
        parents,
        feeSats: Math.max(0, vin - tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0))
      },
      outs
    )
  }

  rebuildTx(w: WalletTx): Promise<Transaction> {
    return Promise.resolve(this.rebuild(w))
  }

  private rebuild(w: WalletTx): Transaction {
    const tx = Transaction.fromHex(w.rawHex)
    if (tx.id('hex') !== w.txid) throw new PermanentError(`stored bytes for ${w.actionId} do not hash to ${w.txid}`)
    for (const inp of tx.inputs) {
      const hex = inp.sourceTXID ? w.parents[inp.sourceTXID] : undefined
      if (hex) inp.sourceTransaction = Transaction.fromHex(hex)
    }
    return tx
  }

  async submit(actionId: string): Promise<{ state: TxState; txid: string; detail?: string }> {
    const w = await this.store.walletTx({ actionId })
    if (!w) throw new PermanentError(`submit: nothing prepared for ${actionId}`, 404)
    if (w.state !== 'prepared' && w.state !== 'submission_unknown' && w.state !== 'submitted') return { state: w.state, txid: w.txid }
    let res: BroadcastResult
    try {
      res = await this.deps.broadcast(this.rebuild(w))
    } catch (e) {
      res = { status: 'error', description: `transport: ${(e as Error).message}` }
    }
    const detail = res.description ?? ''
    // broadcast already ran above (the coordinator IS the 'broadcast'
    // effect) — emit the verdict; the table carries custody
    const obs = { at: Date.now(), result: res }
    const ex = this.deps.exec ?? executor()
    if (res.status === 'success' || ALREADY.test(detail)) {
      await txEmit(ex, this.store, w, 'submit.success', { observation: obs, attempt: true }, this.deps.fx)
      return { state: 'submitted', txid: w.txid }
    }
    if (w.state === 'submitted') {
      await txEmit(ex, this.store, w, 'resubmit', { observation: { ...obs, resubmission: true }, attempt: true }, this.deps.fx)
      return { state: 'submitted', txid: w.txid, detail }
    }
    if (NEVER_LEFT.test(detail) || (DEFINITIVE.test(detail) && !detail.startsWith('transport:'))) {
      const r = await txEmit(
        ex,
        this.store,
        w,
        'submit.refused',
        {
          guard: 'definitive',
          observation: { ...obs, ...(NEVER_LEFT.test(detail) ? { escaped: false } : {}) },
          attempt: true
        },
        this.deps.fx
      )
      return { state: r.next, txid: w.txid, detail }
    }
    // 'submit.unknown' has no edge on an already-unknown row — a repeat
    // unknown is a 'resubmit' self-edge there (records the attempt)
    const r = await txEmit(ex, this.store, w, w.state === 'prepared' ? 'submit.unknown' : 'resubmit', { observation: obs, attempt: true }, this.deps.fx)
    return { state: r.next, txid: w.txid, detail }
  }

  async observe(txid: string, o: TxObservation): Promise<TxState | undefined> {
    const w = await this.store.walletTx({ txid })
    if (!w) return undefined
    // the observation contract: 'seen'/'proven'/'rejected'/'conflicted' are
    // only ever emitted from verified sources (the watchers gate hints) —
    // a raw hint belongs in the observeFn, never in this call
    const { event, guard } = obsEvent(o)
    const r = await txEmit(this.deps.exec ?? executor(), this.store, w, event, { guard, observation: { at: Date.now(), ...o } }, this.deps.fx)
    return r.next
  }

  /** a verified-dead verdict arriving off the submit/observe path (deal
   *  reconcile, op cancel) — routed through the same machine write as
   *  observe() so the terminal state carries its custody intents by
   *  construction: state + event row + lease release + tx-died in one
   *  commit, never a bare setTxState that strands phantom adoption */
  async markRejected(w: WalletTx, observation: unknown): Promise<void> {
    await txEmit(this.deps.exec ?? executor(), this.store, w, 'obs.rejected', { guard: 'verified', observation }, this.deps.fx)
  }

  /** markRejected keyed by actionId — the custody object's entry shape; a
   *  missing row no-ops (the verdict arrived for a tx we don't track). */
  async markRejectedById(actionId: string, observation: unknown): Promise<void> {
    const w = await this.store.walletTx({ actionId })
    if (w !== undefined) await this.markRejected(w, observation)
  }

  /** scan prepared/submission_unknown rows eligible for re-air (after
   *  replaying any committed-but-uneffected custody intents). Separated from
   *  per-row submission so the custody object's global scan can fan out to
   *  each row's own actionId key instead of bypassing per-actionId locks. */
  async resubmitCandidates(): Promise<string[]> {
    await replayTxEffects(this.store, this.deps.fx).catch(() => 0)
    const ids: string[] = []
    for (const w of await this.store.walletTxs(['prepared', 'submission_unknown'])) {
      // attempt bound: past RESUBMIT_BOUND the abandon horizon owns the row —
      // the scan stops adding airs; the watcher still observes it (STATE-09)
      if (w.attempts >= RESUBMIT_BOUND) continue
      // a terminal op's tx is a dead deal's leftover — its row still watches
      // for a landing but nothing re-airs it on the op's behalf. An absent op
      // row (owner txs carry operationId 'owner') means "no op to be over":
      // keep resubmitting
      const op = await this.store.operation(w.operationId).catch(() => undefined)
      if (op !== undefined && TERMINAL_OPS.has(op.state)) continue
      ids.push(w.actionId)
    }
    return ids
  }

  async resubmitPending(): Promise<Array<{ actionId: string; state: TxState }>> {
    const out: Array<{ actionId: string; state: TxState }> = []
    for (const actionId of await this.resubmitCandidates()) {
      out.push({ actionId, state: (await this.submit(actionId)).state })
    }
    return out
  }
}
