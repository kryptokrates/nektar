import type { BoxRef, SignedOffer, SignedWant } from '@nektar/bstack'
import { digestOf } from '../policy.js'
import { classifyDeliveryError, type InboxItem, type Operation, type OutboxItem } from '../store.js'
import { MarketBase } from './base.js'
import { NONTERMINAL } from './types.js'
import { WAITS } from '../waits.js'
import { deadline, stable01 } from '@nektar/core'
import type { MarketMessage } from './types.js'
import { executor, EXEC_SVC } from '../rt/exec.js'
import { custodyResubmitPending } from '../rt/emit.js'

/** pump fan-out: deliveries are independent and off the engine mutex, so a
 *  small pool overlaps legs instead of serializing the whole due batch
 *  behind one slow host's timeout */
const OUTBOX_POOL = 4

export abstract class IngressOps extends MarketBase {
  // ── inputs ──────────────────────────────────────────────────────────────
  async ingestEvents(source: string, events: Array<{ seq: number | string; kind: string; payload: unknown }>, cursor: string): Promise<void> {
    const items = events
      .filter((e) => e.kind === 'want' || e.kind === 'offer')
      .map((e) => ({ messageId: String(e.seq), artifactHash: digestOf('nektar:market:inbox:1', e.payload), kind: e.kind, payload: e.payload }))
    await this.d.store.ingest(source, items, cursor)
  }

  async ingestMessage(fromPub: string, msg: MarketMessage, refs?: import('@nektar/bstack').BoxRef[]): Promise<void> {
    const payload = { from: fromPub, msg, ...(refs !== undefined ? { refs } : {}) }
    const h = digestOf('nektar:market:inbox:1', payload)
    await this.d.store.ingest(`msg:${fromPub}`, [{ messageId: h, artifactHash: h, kind: 'message', payload }], h)
  }

  // ── dispatched downward ── tick hands work to the phase layers below; the
  // contract is declared here and implemented by SellerOps (onWant) and
  // BuyerOps (onOffer, onMessage, advance)
  protected abstract onWant(sw: SignedWant): Promise<void>
  protected abstract onOffer(so: SignedOffer): Promise<void>
  protected abstract onMessage(from: string, m: MarketMessage, refs?: BoxRef[]): Promise<void>
  protected abstract advance(op: Operation, height: number): Promise<void>

  /** in-run resubmit cadence — boot replay is a start-up seam, not the
   *  only owner of a dropped ledger write. stable01(me) spreads each
   *  agent's cadence deterministically — restarts don't phase-align */
  private lastResubmitAt = -Infinity
  private resubmitEvery = -1

  /** the inbox scan with the self-note ordering — self-notes must restore
   *  their operation before anything else in the batch runs (a want lane
   *  event ingested ahead of the sellerNote would create a fresh sale op
   *  with the same dedupe key and the note would dedupe away). The dealfan
   *  service scans through this in both modes. */
  async pendingInbox(): Promise<InboxItem[]> {
    const inbox = await this.d.store.pendingInbox()
    const selfNote = (i: InboxItem): boolean =>
      i.kind === 'message' &&
      (i.payload as { from?: string; msg?: { type?: string } }).from === this.me &&
      ((i.payload as { msg?: { type?: string } }).msg?.type === 'note' || (i.payload as { msg?: { type?: string } }).msg?.type === 'sellerNote')
    inbox.sort((a, b) => Number(selfNote(b)) - Number(selfNote(a)))
    return inbox
  }

  /** the resubmit cadence leg — 'prepared'/'submission_unknown' rows are
   *  signed intents, not just boot leftovers: a dropped ledger write
   *  between broadcast and mark must not wedge the inputs until the next
   *  restart. Boot replay covers startup; the dealfan floor carries this. */
  async resubmitTick(): Promise<void> {
    if (this.resubmitEvery < 0) this.resubmitEvery = WAITS.resubmitCadence.ms + Math.floor(stable01(this.me) * WAITS.resubmitJitter.ms)
    if (this.now() - this.lastResubmitAt < this.resubmitEvery) return
    this.lastResubmitAt = this.now()
    if (this.wallet !== undefined) await custodyResubmitPending(this.d.executor ?? executor(), this.wallet).catch(() => undefined)
  }

  tick(): Promise<void> {
    return this.run(async () => {
      // the whole pass is one invocation on dealfan — the scans journal
      // (replay re-sends the SAME snapshot), every leg lands on its own
      // serialized key, and the tick self-arms the floor's next cadence.
      // call() keeps "tick() = one complete pass" semantics.
      const ex = this.d.executor ?? executor()
      await ex.call({ service: EXEC_SVC.dealfan, handler: 'tick' }, {}).catch((e: unknown) => {
        this.d.log?.(`market: dealfan tick: ${(e as Error).message}`)
      })
      // with the runtime's pump wired, the end-of-tick pass is a poke
      // (the drain runs off the engine mutex)
      if (this.d.store.outboxReady !== undefined) this.d.store.outboxReady()
      else await this.pumpOutbox()
    }, 'tick')
  }

  /** one op's advance leg — the try/stamp wrapper shared by the local tick
   *  (inside run()) and the deal VO's per-key invocation. advance() writes
   *  are revision-CAS: a restarted leg resumes from its committed prefix. */
  protected async advanceLeg(op: Operation, height: number): Promise<void> {
    try {
      await this.advance(op, height)
    } catch (e) {
      const cur = await this.d.store.operation(op.operationId)
      if (cur) await this.d.store.transition(cur.operationId, cur.revision, cur.state, { cause: 'tick_error', lastError: (e as Error).message.slice(0, 500) })
    }
  }

  /** the deal object's per-op entry — load + leg, no run() (the object's
   *  key IS the serialization). Returns false when the op is gone or
   *  terminal — the send's idempotent redelivery is then a no-op. */
  async advanceOp(operationId: string): Promise<boolean> {
    const op = await this.d.store.operation(operationId)
    if (op === undefined || !NONTERMINAL.includes(op.state)) return false
    await this.advanceLeg(op, await this.d.height())
    return true
  }

  /** the deal object's per-event entry — one inbox item's domain leg keyed
   *  by the deal's correlation id (wantId). No run(): the event key IS the
   *  serialization; per-key FIFO gives the same ordering the batch's
   *  self-note sort enforced. complete/fail rides inside the leg — an
   *  unprocessable item fails here, not on a retrying fan-out. */
  async dealEvent(i: { source: string; messageId: string; kind: string; payload: unknown }): Promise<void> {
    try {
      // new-deal gates leave the item pending WITHOUT spending an attempt —
      // a host flap must not dead-letter wants (the next fan-out re-sends)
      if (i.kind === 'want') {
        if (this.d.killed?.() || this.d.deaf?.()) return
        await this.onWant(i.payload as SignedWant)
      } else if (i.kind === 'offer') await this.onOffer(i.payload as SignedOffer)
      else {
        const p = i.payload as { from: string; msg: MarketMessage; refs?: BoxRef[] }
        await this.onMessage(p.from, p.msg, p.refs)
      }
      await this.d.store.completeInbox(i.source, i.messageId)
    } catch (e) {
      await this.d.store.failInbox(i.source, i.messageId, (e as Error).message)
    }
  }

  /** the deal object's reconcile entry — the tick's two rescan legs for ONE
   *  op: refused-escrow refund arming and the refunded rescan. */
  async reconcileOp(operationId: string): Promise<void> {
    const op = await this.d.store.operation(operationId)
    if (op === undefined || op.role !== 'buyer') return
    if (op.state === 'funding_failed') await this.reconcileRefused(op)
    else if (op.state === 'refunded') await this.reconcileRefundProof(op)
  }

  /** a refused escrow that lands anyway (manual reair, a peer that relayed
   *  it before the refusal returned) is a live covenant holding our sats —
   *  funding_failed is dead but the money is not. Arm the refund path at
   *  refundAfter; 'dead' verdicts leave the op alone. Direct rails are
   *  skipped: a landed P2PKH went to the seller — nothing locked on our
   *  side for refund() to reclaim. Proof-bound, not clock-bound: a
   *  seen-hint alone never arms — the tx can still die unconfirmed, and
   *  refund_pending claims locked sats a hint cannot establish. */
  private async reconcileRefused(op: Operation): Promise<void> {
    try {
      const pending = op.data.pendingEscrow as { cfg?: { direct?: boolean; refundAfter?: string | number }; utxo?: { txid?: string } } | undefined
      const txid = (op.data.escrowTxid as string | undefined) ?? pending?.utxo?.txid
      if (txid === undefined || this.d.txFacts === undefined || pending?.cfg?.direct === true) return
      if (!this.reconcileGate(op.operationId, txid, op.nextReconcileAt)) return
      const facts = await this.d.txFacts(txid)
      if (facts.verdict !== 'mined') return
      const cfg = (op.data.escrow as Record<string, unknown> | undefined) ?? pending?.cfg
      const utxo = (op.data.utxo as Record<string, unknown> | undefined) ?? pending?.utxo
      if (cfg === undefined || utxo === undefined) return
      const refundAfter = Number(cfg.refundAfter ?? 0)
      await this.d.store.transition(op.operationId, op.revision, 'refund_pending', {
        cause: 'refused_escrow_landed',
        data: { escrow: cfg, utxo, escrowTxid: txid },
        dueHeight: refundAfter > 0 ? refundAfter : null
      })
    } catch {
      /* status probes must not stall the tick */
    }
  }

  /** a refund accepted at submit then rejected by the network must not sit
   *  in 'refunded' claiming the money came back — the outpoint went to the
   *  seller. Bounded by PROOF, not the clock: 'refunded' is only truly
   *  terminal once the refund tx is proven mined; 'dead' un-marks it no
   *  matter when the rejection surfaces — a slow chain's late verdict must
   *  not leave a false money claim standing. */
  private async reconcileRefundProof(op: Operation): Promise<void> {
    try {
      const txid = op.data.refundTxid as string | undefined
      if (txid === undefined || this.d.txFacts === undefined || op.data.refundProven === true) return
      if (!this.reconcileGate(op.operationId, txid, op.nextReconcileAt)) return
      const verdict = (await this.d.txFacts(txid)).verdict
      if (verdict === 'dead')
        await this.d.store.transition(op.operationId, op.revision, 'uncertain', {
          cause: 'refund_rejected',
          data: { refundTxid: txid },
          dueHeight: null
        })
      else if (verdict === 'mined')
        await this.d.store.transition(op.operationId, op.revision, 'refunded', {
          cause: 'refund_proven',
          data: { refundProven: true }
        })
    } catch {
      /* status probes must not stall the tick */
    }
  }

  async pumpOutbox(): Promise<void> {
    const items = await this.d.store.dueOutbox()
    // bounded fan-out: deliveries never touch op state and rows are
    // independent, so lanes overlap — a dead host's full-timeout leg (or a
    // leg that never settles) can't serialize the fresh sends queued
    // behind it. Serial drain observed failing live: a buyer's accept sat
    // unattempted behind dead-host repair rows for the whole deal window.
    // items[i++] is synchronous — each row is claimed by exactly one worker.
    let i = 0
    await Promise.all(
      Array.from({ length: Math.min(OUTBOX_POOL, items.length) }, async () => {
        let item: OutboxItem | undefined
        while ((item = items.at(i++)) !== undefined) await this.pumpItem(item)
      })
    )
  }

  private async pumpItem(item: OutboxItem): Promise<void> {
    await this.deliverOutboxItem(item)
  }

  /** one row's delivery leg — the transport object's per-destination
   *  invocation and the local pump share it. Reads the row first: an
   *  acked/dead/re-scheduled row is skipped so a replayed or raced drain
   *  can't double-send. The verdict names what the leg settled; the
   *  attempts/dead-letter policy itself stays in the row (retryOutbox). */
  async deliverOutboxItem(item: OutboxItem): Promise<'acked' | 'retried' | 'deferred' | 'skipped'> {
    const cur = await this.d.store.outboxRow(item.actionId)
    if (cur === undefined || cur.status !== 'pending' || cur.nextAt > this.now()) return 'skipped'
    try {
      await this.deliverBounded(item)
      await this.d.store.ackOutbox(item.actionId)
      return 'acked'
    } catch (e) {
      let msg = (e as Error).message
      // resource starvation (a funding-tree slot being empty) is not a
      // delivery fault — defer without consuming attempts; unblockOutbox
      // wakes it the moment a topup proof frees a slot. The class rides
      // the row so a message that merely CONTAINS the keyword can't be
      // pulled forward by an unrelated wake.
      const cls = classifyDeliveryError(msg)
      if (cls === 'funding-slot') {
        await this.d.store.deferOutbox(item.actionId, msg)
        return 'deferred'
      }
      // a persisted payload can outgrow the box cap — built under an older
      // bound, or an escrow wire that came out larger than expected. The
      // `tx` field is advisory on every message that carries it (receivers
      // fall back to fetchTx), so drop it once before burning attempts;
      // an unshrinkable envelope dead-letters on the normal path.
      if (item.kind === 'message' && /range: content/.test(msg)) {
        const shrunk = this.shrinkWire(item.payload as MarketMessage)
        if (shrunk !== undefined) {
          try {
            await this.deliverBounded({ ...item, payload: shrunk })
            await this.d.store.ackOutbox(item.actionId)
            return 'acked'
          } catch (e2) {
            msg = (e2 as Error).message
          }
        }
      }
      await this.d.store.retryOutbox(item.actionId, msg, undefined, cls)
      return 'retried'
    }
  }

  /** one deliver gets one bound: the legs inside it (host submits, record
   *  lookups) are timeout-capped already, but a leg that never settles
   *  would park its worker for as long as it hangs — the row retries
   *  transport-classed instead of the queue starving behind it (and a
   *  recovered host pulls it forward with every other waiting row). The
   *  late-settling deliver's outcome is swallowed: the row already moved on. */
  private async deliverBounded(item: OutboxItem): Promise<void> {
    const r = await deadline(async () => await this.deliver(item), this.o.deliverBoundMs)
    if (r.outcome === 'timeout') throw new Error(`deliver timed out (bound ${this.o.deliverBoundMs}ms)`)
    if (r.outcome === 'aborted') throw new Error('deliver aborted')
  }

  /** the wire a market message can lose without changing its meaning — every
   *  reader falls back to `fetchTx` when `tx` is absent */
  private shrinkWire(m: MarketMessage): MarketMessage | undefined {
    const w = m as { type: string; tx?: string }
    if ((w.type === 'accept' || w.type === 'delivered' || w.type === 'note' || w.type === 'sellerNote') && typeof w.tx === 'string') {
      // the key must GO, not go undefined — a tx:undefined member still
      // serializes where a missing key doesn't, and the shrunk envelope is
      // the oversized-envelope recovery path
      const { tx: _drop, ...rest } = w
      return rest as MarketMessage
    }
    return undefined
  }

  private async deliver(i: OutboxItem): Promise<void> {
    if (i.kind === 'publish-want') return await this.d.transport.publishWant(i.payload as SignedWant)
    if (i.kind === 'publish-offer') return await this.d.transport.publishOffer(i.payload as SignedOffer)
    if (i.kind === 'repair-host') {
      if (this.d.transport.repair === undefined) throw new Error('transport cannot repair a missed host leg')
      return await this.d.transport.repair(i.destination, (i.payload as { key: string }).key)
    }
    if (i.kind === 'republish-proved') {
      if (this.d.transport.republish === undefined) throw new Error('transport cannot republish a proved carrier')
      const p = i.payload as { treeTxid: string; hash: string }
      return await this.d.transport.republish(p.treeTxid, p.hash)
    }
    if (i.kind === 'message') return await this.d.transport.send(i.destination, i.payload as MarketMessage, i.refs)
    throw new Error(`unknown outbox kind ${i.kind}`)
  }
}
