import { P2PKH, type Transaction } from '@bsv/sdk'
import { signOffer, verifyOffer, verifyWant, deadAncestry, type SignedWant } from '@nektar/bstack'
import { publicSaleQuote, saleQuoteWire, verifyOfferSig } from '@nektar/identity'
import { DisclosureSaleHost } from '@nektar/anchor'
import { fromHex } from '@nektar/core'
import { spendValid, isHeaderUnavailable } from '@nektar/chain'
import { decideOffer } from '../policy.js'
import type { Operation, UtxoRef } from '../store.js'
import { IngressOps } from './ingress.js'
import { deliveredPackRefs, pkhOf, sparse } from './types.js'
import { unconfirmedAncestors, zeroconfVerdict } from './zeroconf.js'
import type { MarketMemory, MarketMessage } from './types.js'

export abstract class SellerOps extends IngressOps {
  // ── seller ──────────────────────────────────────────────────────────────
  protected async onWant(sw: SignedWant | undefined): Promise<void> {
    // new-deal gates run BEFORE the want is consumed — throwing keeps the
    // inbox item pending so the want is retried when the gate re-opens
    if (this.d.killed?.()) throw new Error('killed: operator stop flag is set')
    if (this.d.deaf?.()) throw new Error('deaf: no market hosts reachable')
    const sp = await this.sellerPolicy()
    if (!sp || sw === undefined || !verifyWant(sw.want, sw.sig, { now: this.now() })) return
    const w = sw.want
    if (w.pub === this.me) {
      this.d.log?.(`market: want ${w.wantId} refused — self-published; a node does not answer its own wants (a self-deal canary needs a second agent identity)`)
      return
    }
    if (w.status !== 'open' || w.net !== this.d.network) return
    if (w.exp !== undefined && w.exp <= this.now()) return
    const p = sp.policy
    if (p.ai.providers.length && !p.ai.providers.includes(this.d.ai.provider)) return
    if (this.d.ai.egress === 'external' && !p.ai.allowSensitiveEgress) return
    const dayStart = this.now() - 86_400_000
    const today = (await this.d.store.operations({ role: 'seller' })).filter((x) => x.kind === 'sale' && x.createdAt > dayStart).length
    if (today >= p.seller.maxPreparationsPerDay) return
    let pick:
      | {
          shard: string
          seq: number
          price: number
          manifest: NonNullable<Awaited<ReturnType<MarketMemory['shardManifest']>>>
          relevanceBps: number
          model: { provider: string; modelId: string }
        }
      | undefined
    for (const shard of p.seller.sellableShards) {
      const manifest = await this.d.memory.shardManifest(shard)
      if (!manifest || !w.domains.every((x) => manifest.domains.includes(x))) continue
      const price = manifest.offers?.find((x) => x.kind === 'record')?.priceSats ?? manifest.offers?.find((x) => x.kind === 'answer')?.priceSats
      if (price === undefined || (w.maxSats !== undefined && price > w.maxSats)) continue
      const decision = decideOffer(p, {
        network: this.d.network,
        agentPub: this.me,
        domains: w.domains,
        shard,
        priceSats: price,
        publicDisclosure: p.disclosure.allowPublic,
        now: this.now()
      })
      if (decision.kind !== 'allow') continue
      const candidates =
        this.d.memory.marketCandidates !== undefined
          ? (await this.d.memory.marketCandidates(shard)).map((r) => ({ id: String(r.seq), text: r.text }))
          : (await this.d.memory.records(shard))
              .filter((r) => (r.level ?? 0) === 0 && r.fragment.d.length > 0)
              .map((r) => ({ id: String(r.seq), text: Buffer.from(r.fragment.d).toString('utf8').slice(0, 4000) }))
      if (!candidates.length) continue
      const res = await this.d.ai.execute({
        jobId: `${w.pub}:${w.wantId}:${w.revision}:${shard}`,
        kind: 'match',
        question: w.question ?? '',
        domains: w.domains,
        candidates,
        maxOutputTokens: p.ai.maxOutputTokens,
        timeoutMs: p.ai.timeoutMs
      })
      if (res.stop !== 'ok' || res.choice === undefined) continue
      if (!pick || res.relevanceBps > pick.relevanceBps)
        pick = { shard, seq: Number(res.choice), price, manifest, relevanceBps: res.relevanceBps, model: res.model }
    }
    if (!pick) return
    const { operation, created } = await this.d.store.createOperation({
      role: 'seller',
      kind: 'sale',
      dedupeKey: `seller:${this.d.network}:${w.pub}:${w.wantId}:${w.revision}`,
      policyHash: sp.hash,
      network: this.d.network,
      state: 'preparing',
      data: { want: sw, shard: pick.shard, seq: pick.seq, priceSats: pick.price, ai: { relevanceBps: pick.relevanceBps, model: pick.model } }
    })
    if (!created) return
    try {
      const h = await this.d.height()
      const tip = await this.d.tip?.()
      const offer = await this.d.call('sale#offerMemory', {
        shard: pick.shard,
        seq: pick.seq,
        price: pick.price,
        refundAfter: h + this.o.refundBlocks,
        maxFee: this.o.saleMaxFee,
        expiresAt: h + this.o.offerTtlBlocks,
        ...(p.disclosure.allowPublic ? {} : { buyerPub: w.pub }),
        // the direct rail ships `wrapped` with an unproven payment — cap the
        // no-recourse exposure at directAcceptMaxSats when the operator set one
        ...(this.o.directAccept === true && (this.o.directAcceptMaxSats === undefined || pick.price <= this.o.directAcceptMaxSats) ? { direct: true } : {})
      })
      const c = offer.commitment
      const so = await signOffer(this.d.key, {
        offerId: operation.operationId,
        shard: pick.shard,
        seq: pick.seq,
        shardId: pick.manifest.shardId,
        domains: pick.manifest.domains,
        leaf: c.leaf,
        hdp: c.hdp,
        ...(c.batchR ? { batchR: c.batchR } : {}),
        priceSats: pick.price,
        offer: saleQuoteWire(publicSaleQuote(offer)),
        wantId: w.wantId,
        taskHash: w.taskHash,
        wantRevision: w.revision,
        buyerPub: w.pub,
        replyTo: w.pub,
        ts: this.now(),
        exp: this.now() + 86_400_000,
        net: this.d.network,
        ...(tip ? { tip: tip.tip, tipHeight: tip.tipHeight } : {}),
        ...(this.d.logStream !== undefined ? { journal: { stream: this.d.logStream } } : {})
      })
      const sellerNote: Extract<MarketMessage, { type: 'sellerNote' }> = {
        type: 'sellerNote',
        v: 1,
        wantId: w.wantId,
        offerId: operation.operationId,
        policyHash: sp.hash,
        want: sw,
        shard: pick.shard,
        seq: pick.seq,
        priceSats: pick.price,
        offer,
        signedOffer: so,
        dueHeight: h + this.o.refundBlocks
      }
      await this.d.store.transition(operation.operationId, 1, 'quoted', {
        cause: 'offer_prepared',
        data: { offer, signedOffer: so },
        dueHeight: h + this.o.refundBlocks,
        outbox: [
          { actionId: `${operation.operationId}:offer`, kind: 'publish-offer', destination: 'feed', payload: so },
          {
            actionId: `${operation.operationId}:quote`,
            kind: 'message',
            destination: w.pub,
            payload: { type: 'quote', wantId: w.wantId, offerId: operation.operationId, commitment: c, mode: offer.mode }
          },
          // encrypted self-note — rebuilds this sale if the automation database is lost
          { actionId: `${operation.operationId}:sellerNote`, kind: 'message', destination: this.me, payload: sellerNote }
        ]
      })
    } catch (e) {
      await this.d.store.transition(operation.operationId, 1, 'preparation_failed', {
        cause: 'preparation_error',
        lastError: (e as Error).message.slice(0, 500)
      })
    }
  }

  protected async onAccept(from: string, m: Extract<MarketMessage, { type: 'accept' }>): Promise<void> {
    const op = await this.d.store.operation(m.offerId)
    if (!op || op.role !== 'seller' || op.state !== 'quoted') return
    const w = op.data.want!.want
    const offer = op.data.offer!
    const refuse = async (reason: string) => {
      // a deterministic refusal must reach the buyer — 'quoted' alone leaves
      // them waiting for a delivery that can never come until refundAfter
      await this.d.store.transition(op.operationId, op.revision, 'quoted', {
        cause: 'accept_refused',
        evidence: { reason },
        outbox: [
          {
            actionId: `${op.operationId}:decline:${op.revision}`,
            kind: 'message',
            destination: w.pub,
            payload: { type: 'decline', wantId: w.wantId, offerId: op.operationId, reason }
          }
        ]
      })
    }
    if (from !== w.pub || m.wantId !== w.wantId) return
    if (m.depositorPkh !== pkhOf(from)) return await refuse('depositor is not the buyer key')
    // the rail is chosen by what the buyer actually funded, not by the
    // advertisement: a covenant output means escrow, a P2PKH to sellerPkh
    // means direct (only when the quote advertised it — payment-final)
    const cfg = { ...offer.cfg, price: BigInt(offer.cfg.price), refundAfter: BigInt(offer.cfg.refundAfter), maxFee: BigInt(offer.cfg.maxFee) }
    const escrowScript = DisclosureSaleHost.escrowScript(cfg, m.depositorPkh)
    const directScript = new P2PKH().lock(Array.from(fromHex(offer.cfg.sellerPkh))).toHex()
    const utxo = sparse(m.utxo)
    let direct = false
    if (utxo?.script === escrowScript) direct = false
    else if (utxo?.script === directScript && offer.direct === true) direct = true
    else
      return await refuse(
        offer.direct === true ? 'accept is neither the quoted covenant nor a P2PKH to the seller key' : 'escrow script does not match the quoted terms'
      )
    if (!Number.isSafeInteger(utxo.satoshis) || utxo.satoshis < Number(offer.cfg.price) + (direct ? 0 : 1))
      return await refuse(direct ? 'payment is underfunded' : 'escrow is underfunded')
    // the buyer hands over the payment itself. It is checked here, against our
    // own headers where its ancestry is proven, and then put on the network
    // by us — no indexer has to have seen it first.
    let tx: Transaction | undefined
    let spv = false
    if (typeof m.tx === 'string') {
      try {
        const got = await this.readTx(m.tx)
        if (got.tx.id('hex') !== utxo.txid) return await refuse('attached transaction is not the claimed escrow')
        tx = got.tx
        spv = got.spv
      } catch (e) {
        // a header outage is not a verdict — no decline is sent; the
        // rethrow reaches dealEvent, which holds the inbox item pending
        if (isHeaderUnavailable(e)) throw e
        return await refuse('attached escrow transaction is unreadable')
      }
    } else tx = await this.d.fetchTx(utxo.txid)
    const out = tx?.outputs[utxo.vout]
    if (!tx || tx.id('hex') !== utxo.txid || !out) return await refuse('escrow transaction not observable yet')
    if (out.lockingScript.toHex() !== utxo.script || out.satoshis !== utxo.satoshis) return await refuse('on-chain escrow output differs from the claimed one')
    // every escrow input whose parent we can resolve is evaluated locally —
    // a bogus or unsigned spend is refused before the network is even asked.
    // Parents the carry didn't include are fetched the same way readTx
    // resolves BEEF ancestry — a fetched parent attaches and evaluates
    let spends = 0
    for (let ix = 0; ix < tx.inputs.length; ix++) {
      const i = tx.inputs[ix]
      const ptxid = i.sourceTXID ?? i.sourceTransaction?.id('hex')
      if (i.sourceTransaction === undefined && ptxid !== undefined) i.sourceTransaction = await this.d.fetchTx(ptxid).catch(() => undefined)
      const vout = sparse(i.sourceOutputIndex)
      const src = i.sourceTransaction?.outputs[vout ?? -1]
      if (!src || vout === undefined || ptxid === undefined) continue
      if (!spendValid(tx, ix, { txid: ptxid, vout, satoshis: src.satoshis ?? 0, script: src.lockingScript.toHex() }))
        return await refuse('escrow input does not spend its claimed parent output')
      spends++
    }
    // an UNPROVEN escrow must carry resolvable ancestry — a skipped input is
    // dead ancestry the network rejects as missing-parent, so 'completing'
    // on it mints a deal that can never settle. A proven escrow's inputs
    // were validated by the network at inclusion; unresolved parents are
    // fine there.
    if (!spv && tx.merklePath === undefined) {
      const dead = await deadAncestry(tx, (id) => this.d.fetchTx(id).catch(() => undefined))
      if (dead.length > 0) return await refuse(`escrow ancestry unresolvable: ${dead.length} input(s) have no reachable parent`)
    }
    // the zero-conf policy — proven ancestry says where the payment came
    // from, not that it will confirm. 'proven' is the payment's OWN merkle
    // path — the wire copy's, or one our store already holds and verified
    // (txFacts.proof reads exactly that; a bytes lookup is not needed —
    // the accept decision stays offline). Never the spv flag: above the
    // cap or the ancestor bound the sale parks until that proof.
    const proven =
      tx.merklePath !== undefined || (this.d.txFacts !== undefined && (await this.d.txFacts(utxo.txid).catch(() => undefined))?.proof !== undefined)
    const ancestors = proven ? 0 : await unconfirmedAncestors(tx, (id) => this.d.fetchTx(id).catch(() => undefined), this.o.maxUnconfirmedAncestors)
    const verdict = zeroconfVerdict({
      proven,
      priceSats: Number(offer.cfg.price),
      unconfirmedAncestors: ancestors,
      opts: this.o
    })
    const zeroconf = { verdict, unconfirmedAncestors: ancestors, capSats: this.o.maxZeroconfSats }
    // the conflict watch — proof delivery and the abandon horizon now reach
    // the op through the existing wake legs
    if (!proven) this.d.watchTx?.(utxo.txid)
    // escrow rebroadcast starts before the bookkeeping — the 'completing'
    // transition, height fetch and custody mark all happen inside the
    // broadcaster RTT. The verdict is still joined before complete(): a
    // refused escrow never produces a 'delivered' claim.
    const escrowBroadcastP = typeof m.tx === 'string' && this.d.broadcast !== undefined ? this.d.broadcast(tx) : undefined
    this.d.rememberTx?.(tx)
    const h = await this.d.height()
    if (!direct && h + this.o.sellerSafetyBlocks >= Number(offer.cfg.refundAfter)) {
      await escrowBroadcastP?.catch(() => undefined)
      return await refuse('too close to the refund deadline')
    }
    if (verdict === 'await-proof') {
      // above the policy the sale holds delivery until the payment's own
      // proof — but join the rebroadcast verdict first: a refused payment
      // leaves the op 'quoted' exactly like the completing rail's refusal
      if (escrowBroadcastP !== undefined) {
        const br = await escrowBroadcastP
        if (br.status !== 'success') {
          await this.d.store.transition(op.operationId, op.revision, 'quoted', {
            cause: 'accept_refused',
            evidence: { reason: `escrow not accepted by the network: ${br.description ?? br.status}` }
          })
          return
        }
      }
      this.d.log?.(
        `market: sale ${op.operationId} holds delivery until the payment proves (price ${offer.cfg.price} > cap ${this.o.maxZeroconfSats} sats or ${ancestors} unconfirmed ancestor(s))`
      )
      await this.d.store.transition(op.operationId, op.revision, 'payment_pending', {
        cause: 'payment_awaiting_proof',
        data: direct ? { direct: true, utxo: m.utxo } : { escrow: { cfg: { ...offer.cfg, depositorPkh: m.depositorPkh }, utxo: m.utxo } },
        evidence: { spv, spends, zeroconf }
      })
      return
    }
    this.d.log?.(`market: sale ${op.operationId} delivering on ${verdict} payment (${ancestors} unconfirmed ancestor(s), cap ${this.o.maxZeroconfSats} sats)`)
    const next = await this.d.store.transition(op.operationId, op.revision, 'completing', {
      cause: direct ? 'payment_verified' : 'escrow_verified',
      // a direct payment is already the seller's money — mark it owned so the
      // funder/wallet layer takes custody of the output we just broadcast
      data: direct ? { direct: true, utxo: m.utxo } : { escrow: { cfg: { ...offer.cfg, depositorPkh: m.depositorPkh }, utxo: m.utxo } },
      evidence: { spv, spends, zeroconf }
    })
    if (direct) await this.d.ownOutput?.(tx, m.utxo.vout)
    if (escrowBroadcastP !== undefined) {
      const br = await escrowBroadcastP
      if (br.status !== 'success') {
        // back to 'quoted' like a refused accept — the buyer's outbox
        // redelivers until acked, and the next attempt re-verifies fresh
        await this.d.store.transition(next.operationId, next.revision, 'quoted', {
          cause: 'accept_refused',
          evidence: { reason: `escrow not accepted by the network: ${br.description ?? br.status}` }
        })
        return
      }
    }
    await this.complete(next)
  }

  /** the payment_pending driver — the payment verified but is unproven and
   *  above the zero-conf policy. Exits: its own proof releases delivery,
   *  a dead verdict declines the deal, the escrow's refund deadline lapses
   *  it, and the 3-day stuck bound escalates. NOTHING is delivered from
   *  this state except through the proven leg. */
  protected async settlePendingPayment(op: Operation, height: number): Promise<void> {
    const offer = op.data.offer!
    const w = op.data.want!.want
    const utxo = (op.data.direct === true ? op.data.utxo : op.data.escrow!.utxo) as UtxoRef
    const decline = async (cause: string, reason: string): Promise<void> => {
      await this.d.store.transition(op.operationId, op.revision, 'quoted', {
        cause,
        outbox: [
          {
            actionId: `${op.operationId}:decline:${op.revision}`,
            kind: 'message',
            destination: w.pub,
            payload: { type: 'decline', wantId: w.wantId, offerId: op.operationId, reason }
          }
        ]
      })
    }
    const tx = await this.d.fetchTx(utxo.txid).catch(() => undefined)
    const proven = tx?.merklePath !== undefined && height - tx.merklePath.blockHeight + 1 >= 1
    if (proven) {
      // escrow rail: a proof landing too late to outrun the refund deadline
      // lapses instead of completing — the direct rail never lapses on height
      if (op.data.direct !== true && height + this.o.sellerSafetyBlocks >= Number(offer.cfg.refundAfter))
        return await decline('payment_pending_lapsed', 'payment unconfirmed too close to the refund deadline')
      const next = await this.d.store.transition(op.operationId, op.revision, 'completing', {
        cause: 'payment_proven',
        evidence: { blockHeight: tx.merklePath!.blockHeight }
      })
      if (op.data.direct === true) await this.d.ownOutput?.(tx, utxo.vout)
      return await this.complete(next)
    }
    // a definitive dead verdict is the declined-accept shape again — the
    // buyer's outbox already stopped; tell them the payment never landed
    if (
      this.d.txFacts !== undefined &&
      this.reconcileGate(op.operationId, utxo.txid, op.nextReconcileAt) &&
      (await this.d.txFacts(utxo.txid)).verdict === 'dead'
    )
      return await decline('payment_died', 'payment did not confirm')
    if (op.data.direct !== true && height + this.o.sellerSafetyBlocks >= Number(offer.cfg.refundAfter))
      return await decline('payment_pending_lapsed', 'payment unconfirmed too close to the refund deadline')
    // otherwise the watch + the floor re-drive it — proof, conflict, lapse
  }

  protected async complete(op: Operation): Promise<void> {
    const e = op.data.escrow!
    const offer = op.data.offer!
    const w = op.data.want!.want
    // the direct rail: the payment is already a P2PKH output the seller owns —
    // no covenant to spend. Delivery hands `wrapped` over inside the envelope;
    // the payment txid stands in as the settlement reference the advance loop
    // watches for a merkle proof (rejected payment → completion_failed)
    if (op.data.direct === true) {
      const p = op.data.utxo!
      await this.d.store.transition(op.operationId, op.revision, 'completion_submitted', {
        cause: 'direct_delivery',
        data: { completeTxid: p.txid, sellerSats: p.satoshis },
        outbox: [
          {
            actionId: `${op.operationId}:delivered`,
            kind: 'message',
            destination: w.pub,
            payload: { type: 'delivered', wantId: w.wantId, offerId: op.operationId, txid: p.txid, value: offer.wrapped },
            ...(deliveredPackRefs(this.d, op) !== undefined ? { refs: deliveredPackRefs(this.d, op) } : {})
          }
        ]
      })
      return
    }
    const r = await this.d.call('sale#complete', { cfg: e.cfg, utxo: e.utxo, wrapped: offer.wrapped })
    if (r.accepted !== true) {
      await this.d.store.transition(op.operationId, op.revision, 'completion_failed', {
        cause: 'completion_refused',
        lastError: String(r.reason ?? 'refused').slice(0, 500)
      })
      return
    }
    await this.d.store.transition(op.operationId, op.revision, 'completion_submitted', {
      cause: 'disclosure_broadcast',
      data: { completeTxid: r.txid, sellerSats: r.sellerSats },
      outbox: [
        {
          actionId: `${op.operationId}:delivered`,
          kind: 'message',
          destination: w.pub,
          payload: { type: 'delivered', wantId: w.wantId, offerId: op.operationId, txid: r.txid, tx: this.d.txWire?.(r.txid, 3200) ?? r.txHex },
          ...(deliveredPackRefs(this.d, op) !== undefined ? { refs: deliveredPackRefs(this.d, op) } : {})
        }
      ]
    })
  }

  /** does a seller sale exist for this offer (held `accept` envelopes are
   *  dropped only when none exists AND the envelope is old) */
  async hasSale(offerId: string): Promise<boolean> {
    return (await this.d.store.operation(offerId)) !== undefined
  }

  /** the seller's own recovery note — rebuilds the quoted sale with its
   *  original operationId (the buyer's accept addresses it by offerId). Only
   *  our own key may feed it; a foreign or unsigned note is ignored. */
  protected async onSellerNote(n: Extract<MarketMessage, { type: 'sellerNote' }>): Promise<void> {
    const sw = sparse(n.want)
    if (sw === undefined || !verifyWant(sw.want, sw.sig, { allowExpired: true })) return
    if (sw.want.net !== this.d.network || sw.want.wantId !== n.wantId) return
    const leaf = n.offer?.commitment?.leaf ?? n.signedOffer?.offer.leaf ?? ''
    if (n.signedOffer !== undefined) {
      const so = n.signedOffer
      if (!verifyOffer(so.offer, so.sig, { allowExpired: true })) return
      if (so.offer.pub !== this.me || so.offer.offerId !== n.offerId || so.offer.wantId !== n.wantId) return
      if (so.offer.taskHash !== sw.want.taskHash || so.offer.wantRevision !== sw.want.revision) return
    } else {
      // compact variant: the published offer stays with the buyer — the quote
      // inside `offer` still proves it was signed for this sale by our key
      const q = n.offer
      if (
        !q ||
        q.sellerPub !== this.me ||
        !verifyOfferSig({ cfg: q.cfg, sellerPub: q.sellerPub, offerSig: q.offerSig, expiresAt: q.expiresAt }, sw.want.pub, leaf)
      )
        return
    }
    if (await this.d.store.operation(n.offerId)) return
    const dedupeKey = `seller:${this.d.network}:${sw.want.pub}:${n.wantId}:${sw.want.revision}`
    const { operation, created } = await this.d.store.createOperation({
      operationId: n.offerId,
      role: 'seller',
      kind: 'sale',
      dedupeKey,
      policyHash: n.policyHash,
      network: this.d.network,
      state: 'quoted',
      data: { want: sw, shard: n.shard, seq: n.seq, priceSats: n.priceSats, offer: n.offer, signedOffer: n.signedOffer, restored: true },
      dueHeight: n.dueHeight
    })
    if (!created) return
    await this.d.store.transition(operation.operationId, operation.revision, 'quoted', { cause: 'restored_from_note' })
  }
}
