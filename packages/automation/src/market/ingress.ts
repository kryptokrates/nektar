import type { BoxRef, SignedOffer, SignedWant } from '@nektar/bstack'
import { digestOf } from '../policy.js'
import { classifyDeliveryError, type InboxItem, type Operation, type OutboxItem } from '../store.js'
import { MarketBase } from './base.js'
import { NONTERMINAL } from './types.js'
import type { MarketMessage } from './types.js'

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

  tick(): Promise<void> {
    return this.run(async () => {
      // self-notes must restore their operation before anything else in the
      // batch runs — a want lane event ingested ahead of the sellerNote would
      // otherwise create a fresh sale op with the same dedupe key and the note
      // would be deduped away
      const inbox = await this.d.store.pendingInbox()
      const selfNote = (i: InboxItem): boolean =>
        i.kind === 'message' &&
        (i.payload as { from?: string; msg?: { type?: string } }).from === this.me &&
        ((i.payload as { msg?: { type?: string } }).msg?.type === 'note' || (i.payload as { msg?: { type?: string } }).msg?.type === 'sellerNote')
      inbox.sort((a, b) => Number(selfNote(b)) - Number(selfNote(a)))
      for (const i of inbox) {
        try {
          // new-deal gates leave the item pending WITHOUT spending an
          // attempt — a host flap must not dead-letter wants
          if (i.kind === 'want') {
            if (this.d.killed?.() || this.d.deaf?.()) continue
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
      const height = await this.d.height()
      for (const op of await this.d.store.operations({ states: NONTERMINAL })) {
        try {
          await this.advance(op, height)
        } catch (e) {
          const cur = await this.d.store.operation(op.operationId)
          if (cur)
            await this.d.store.transition(cur.operationId, cur.revision, cur.state, { cause: 'tick_error', lastError: (e as Error).message.slice(0, 500) })
        }
      }
      // a refund accepted at submit then rejected by the network must not sit
      // in 'refunded' claiming the money came back — the outpoint went to the
      // seller. escrow (advance) and collect (completion_submitted) already
      // reconcile late rejections; this is the same watch for refundTxid,
      // bounded to the 60 min after the transition — post-submit rejections
      // surface within a couple of blocks on a slow chain
      for (const op of await this.d.store.operations({ role: 'buyer', states: ['refunded'] })) {
        try {
          const txid = op.data.refundTxid as string | undefined
          if (txid === undefined || this.d.txFacts === undefined || this.now() - op.updatedAt > 60 * 60_000) continue
          if (!this.reconcileGate(op.operationId, txid)) continue
          if ((await this.d.txFacts(txid)).verdict === 'dead')
            await this.d.store.transition(op.operationId, op.revision, 'uncertain', {
              cause: 'refund_rejected',
              data: { refundTxid: txid },
              dueHeight: null
            })
        } catch {
          /* status probes must not stall the tick */
        }
      }
      // with the runtime's detached pump wired, the end-of-tick pass is a
      // poke (drain runs off the engine mutex); without it the engine
      // flushes inline — tests drive tick() directly and rely on that
      if (this.d.store.outboxReady !== undefined) this.d.store.outboxReady()
      else await this.pumpOutbox()
    })
  }

  async pumpOutbox(): Promise<void> {
    for (const item of await this.d.store.dueOutbox()) {
      try {
        await this.deliver(item)
        await this.d.store.ackOutbox(item.actionId)
      } catch (e) {
        let msg = (e as Error).message
        // resource starvation (a funding-tree slot being empty) is not a
        // delivery fault — defer without consuming attempts; unblockOutbox
        // wakes it the moment a topup proof frees a slot. The class rides
        // the row so a message that merely CONTAINS the keyword can't be
        // pulled forward by an unrelated wake.
        const cls = classifyDeliveryError(msg)
        if (cls === 'funding-slot') await this.d.store.deferOutbox(item.actionId, msg)
        else {
          // a persisted payload can outgrow the box cap — built under an older
          // bound, or an escrow wire that came out larger than expected. The
          // `tx` field is advisory on every message that carries it (receivers
          // fall back to fetchTx), so drop it once before burning attempts;
          // an unshrinkable envelope dead-letters on the normal path.
          if (item.kind === 'message' && /range: content/.test(msg)) {
            const shrunk = this.shrinkWire(item.payload as MarketMessage)
            if (shrunk !== undefined) {
              try {
                await this.deliver({ ...item, payload: shrunk })
                await this.d.store.ackOutbox(item.actionId)
                continue
              } catch (e2) {
                msg = (e2 as Error).message
              }
            }
          }
          await this.d.store.retryOutbox(item.actionId, msg, undefined, cls)
        }
      }
    }
  }

  /** the wire a market message can lose without changing its meaning — every
   *  reader falls back to `fetchTx` when `tx` is absent */
  private shrinkWire(m: MarketMessage): MarketMessage | undefined {
    const w = m as { type: string; tx?: string }
    if ((w.type === 'accept' || w.type === 'delivered' || w.type === 'note' || w.type === 'sellerNote') && typeof w.tx === 'string')
      return { ...m, tx: undefined } as MarketMessage
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
