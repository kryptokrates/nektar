import { P2PKH, Transaction } from '@bsv/sdk'
import { verifyOffer, verifyWant, type BoxRef, type SignedOffer, type SignedWant } from '@nektar/bstack'
import { verifyOfferSig } from '@nektar/identity'
import { COVENANT_ID, DisclosureSaleHost } from '@nektar/anchor'
import { fromHex, parseFragment } from '@nektar/core'
import { spendValid, FundsStarvedError, type Ingested } from '@nektar/chain'
import type { Operation } from '../store.js'
import { decidePurchase, digestOf, type ActionContext, type AutomationPolicy } from '../policy.js'
import { rankOffers, type ScoreCandidate } from '../scoring.js'
import { SellerOps } from './seller.js'
import { CAP, PARKED, pkhOf, STATE_LIMITS, TERMINAL } from './types.js'
import type { MarketMessage, StoredOffer, StuckExit } from './types.js'
import { executor } from '../rt/exec.js'
import { custodyMarkRejected, custodySubmit } from '../rt/emit.js'

// a take that lost the race to a concurrent spend is not a deal fault —
// 'selected' re-enters fund() next tick and the funding window bounds the
// retry. Only starvation-class errors qualify; refusals stay terminal. The
// typed code is the contract across the package boundary; the text match
// stays as the compat floor for impls that still throw plain errors
function starved(e: unknown): boolean {
  if (e instanceof FundsStarvedError || (e as { code?: string }).code === 'STARVED') return true
  return /no spendable output|deal floor/i.test(e instanceof Error ? e.message : String(e))
}

export abstract class BuyerOps extends SellerOps {
  /** bounce a starved escrow build back to 'selected' for the next tick's
   *  retake — but only inside the funding window: the per-state clocks reset
   *  on each bounce, so the anchor rides in data.fundingSince and past the
   *  bound starvation is terminal after all (nothing was broadcast — a
   *  failed deal is honest here, 'uncertain' is not). */
  private async starveBounce(op: Operation, e: unknown): Promise<void> {
    const since = (op.data.fundingSince as number | undefined) ?? this.now()
    const exhausted = this.now() - since > STATE_LIMITS.funding.maxAgeMs
    await this.d.store.transition(op.operationId, op.revision, exhausted ? 'funding_failed' : 'selected', {
      cause: exhausted ? 'funding_starved_exhausted' : 'funding_starved',
      lastError: (e instanceof Error ? e.message : String(e)).slice(0, 500)
    })
  }

  /** a SpendGuard refusal splits on permanence: 'daily cap' is a window —
   *  the UTC-midnight reset or an operator raise lets the same deal fund —
   *  so it defers instead of dying; every other refusal is a policy no.
   *  The refused attempt's pendingEscrow is tombstone data (died() already
   *  freed its custody) — cleared so a retry prepares a fresh tx */
  private async spendRefused(funding: Operation, detail: string): Promise<void> {
    if (/daily cap/i.test(detail)) {
      const midnight = new Date()
      midnight.setUTCHours(24, 0, 0, 0)
      await this.d.store.transition(funding.operationId, funding.revision, 'funding_deferred', {
        cause: 'spend_cap_deferred',
        lastError: detail.slice(0, 500),
        data: { pendingEscrow: null },
        dueAt: midnight.getTime()
      })
      return
    }
    await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
      cause: 'spend_guard_refused',
      lastError: detail.slice(0, 500)
    })
  }

  // ── buyer ───────────────────────────────────────────────────────────────
  private offerReject(so: SignedOffer, w: SignedWant['want']): string | undefined {
    const o = so.offer
    if (o.buyerPub !== this.me) return 'not bound to this buyer'
    if (o.taskHash !== w.taskHash || o.wantRevision !== w.revision) return 'task or revision mismatch'
    if (o.net !== this.d.network) return 'wrong network'
    if (o.exp !== undefined && o.exp <= this.now()) return 'expired'
    if (w.maxSats !== undefined && o.priceSats > w.maxSats) return 'above the want ceiling'
    const q = o.offer as any
    if (!q?.cfg) return 'no signed quote'
    if (Number(q.cfg.price) !== o.priceSats) return 'quote does not match the listing'
    // the escrow pays cfg.sellerPkh — under derived custody the quote signer
    // is a custody child of the identity (never the identity itself, which
    // remote custody can't sign with); the SignedOffer's own sig is what binds
    // the quote to o.pub. The money-path invariant: the payee is exactly the
    // verified quote signer — anything else is a graft.
    if (q.cfg.sellerPkh !== pkhOf(q.sellerPub)) return 'payee is not the quote signer'
    if (Number(q.cfg.maxFee) > this.o.escrowFeeReserveSats) return 'fee bound too high'
    const leaf = o.leaf ?? ''
    if (!verifyOfferSig(q, this.me, leaf) && !verifyOfferSig(q, '', leaf)) return 'quote signature invalid'
    // a quote naming a covenant artifact must pin the same covenant we
    // instantiate — a lookalike with the same parameter layout fails here
    // before any sats are committed (quotes without the field are pre-artifact)
    if (q.artifact !== undefined && (q.artifact.scriptHash !== COVENANT_ID.scriptHash || q.artifact.artifactId !== COVENANT_ID.artifactId))
      return 'covenant artifact mismatch'
    return undefined
  }

  protected async onOffer(so: SignedOffer): Promise<void> {
    if (!so?.offer?.wantId || !verifyOffer(so.offer, so.sig, { now: this.now() })) return
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${so.offer.wantId}`)
    if (!op || op.state !== 'collecting') return
    const offers = { ...(op.data.offers as Record<string, StoredOffer>) }
    const key = `${so.offer.pub}:${so.offer.offerId}`
    const prev = offers[key]
    if (prev && prev.so.offer.revision >= so.offer.revision) return
    if (!prev && Object.keys(offers).length >= 64) return
    const reject = this.offerReject(so, (op.data.want as SignedWant).want)
    // a quote can beat its offer across the two delivery planes (the box
    // read is faster than the offers lane) — it parks on the op instead of
    // retrying, and the commitment attaches with the offer's own commit
    const pending = { ...(op.data.pendingQuotes as Record<string, { commitment?: StoredOffer['commitment']; mode?: StoredOffer['mode'] }> | undefined) }
    const parked = pending[key]
    if (parked !== undefined) delete pending[key]
    // the parked quote attaches only if its commitment matches this offer —
    // same leaf check onQuote applies on the live path
    const qsrc = prev?.commitment !== undefined ? prev : parked?.commitment?.leaf === so.offer.leaf ? parked : undefined
    offers[key] = { so, ...(reject ? { reject } : {}), ...(qsrc?.commitment !== undefined ? { commitment: qsrc.commitment, mode: qsrc.mode } : {}) }
    await this.d.store.transition(op.operationId, op.revision, 'collecting', {
      cause: reject ? 'offer_rejected' : 'offer_recorded',
      data: { offers, lastOfferAt: this.now(), ...(parked !== undefined ? { pendingQuotes: pending } : {}) },
      evidence: { offerKey: key, ...(reject ? { reject } : {}) }
    })
  }

  private async onQuote(from: string, m: Extract<MarketMessage, { type: 'quote' }>): Promise<void> {
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${m.wantId}`)
    if (!op || TERMINAL.includes(op.state)) return
    const offers = { ...(op.data.offers as Record<string, StoredOffer>) }
    const key = `${from}:${m.offerId}`
    const cur = offers[key]
    if (!cur) {
      // the quote beat its offer — same park convention as a delivered landing
      // early: keep it on the op, no retry budget burned (the feed is slower
      // than the box by construction, not by fault)
      if (op.state === 'collecting') {
        const pending = { ...(op.data.pendingQuotes as Record<string, { commitment?: StoredOffer['commitment']; mode?: StoredOffer['mode'] }> | undefined) }
        if (Object.keys(pending).length < 64) {
          pending[key] = { commitment: m.commitment, mode: m.mode }
          await this.d.store.transition(op.operationId, op.revision, op.state, { cause: 'quote_parked_before_offer', data: { pendingQuotes: pending } })
        }
      }
      return
    }
    if (!m.commitment || m.commitment.leaf !== cur.so.offer.leaf) return
    // early-select leaves 'collecting' on the first offer — a quote landing
    // in selected/funding/awaiting_disclosure still carries the commitment
    // the claim needs, so attach it whenever the offer is live
    offers[key] = { ...cur, commitment: m.commitment, mode: m.mode }
    await this.d.store.transition(op.operationId, op.revision, op.state, { cause: 'quote_attached', data: { offers } })
  }

  private async onDelivered(from: string, m: Extract<MarketMessage, { type: 'delivered' }>, refs?: import('@nektar/bstack').BoxRef[]): Promise<void> {
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${m.wantId}`)
    // a delivered landing while the op is funding_deferred is real evidence,
    // not noise: a refused escrow can still air via the counterparty's own
    // guard, and the reveal it carries is one-shot. Park it — the deferred
    // advance reconciles the escrow txid and replays the parked copy under
    // awaiting_disclosure. Terminal/absent ops still drop.
    if (op !== undefined && op.state === 'funding_deferred') {
      await this.d.store.transition(op.operationId, op.revision, 'funding_deferred', {
        cause: 'delivered_parked_while_deferred',
        data: { pendingDelivered: { from, m, refs } }
      })
      return
    }
    if (!op || op.state !== 'awaiting_disclosure') return
    const sel = op.data.selected as { key: string }
    const chosen = (op.data.offers as Record<string, StoredOffer>)[sel.key]
    if (from !== chosen.so.offer.pub || m.offerId !== chosen.so.offer.offerId) return
    const utxo = op.data.utxo as { txid: string; vout: number; satoshis: number; script: string }
    const q = chosen.so.offer.offer as any
    const direct = op.data.direct === true
    // quote-message mode wins (it was the authoritative channel); the signed
    // wire quote now carries it too so a delivered landing ahead of the quote
    // still claims under the right encoding — without it a key-mode payload
    // is parsed as a fragment and a good sale false-disputes.
    const offerMode = chosen.mode ?? q.mode
    // the signed offer carries leaf+hdp — it is the canonical commitment
    // evidence (signed, unlike the quote). The quote's commitment object adds
    // authorPub/anchorTxid/proof when it landed before selection; under
    // early-select the escrow can fund first, so derive the base here rather
    // than trust envelope timing. Restore paths inherit the same guarantee:
    // the buyerNote persists the signed offer even when the quote was lost.
    const commitment = {
      leaf: chosen.so.offer.leaf,
      hdp: chosen.so.offer.hdp,
      ...(chosen.commitment !== undefined ? chosen.commitment : {})
    }
    let spv = false
    let claim: any
    if (direct) {
      // direct rail: `wrapped` rides inside the envelope — nothing on-chain
      // to run, the hash commitment is the whole verification
      if (typeof m.value !== 'string' || m.value.length === 0) return
      claim = await this.d.call('sale#claim', {
        valueHex: m.value,
        hWrapped: q.cfg.hWrapped,
        sellerPub: from,
        offerMode,
        commitment
      })
    } else {
      let got: Ingested | undefined
      try {
        if (typeof m.tx === 'string') got = await this.readTx(m.tx)
      } catch {
        /* unreadable wire — fall through to the on-chain fetch */
      }
      if (got === undefined) {
        // shrunk envelope (or an unparsable wire) — the reveal is already
        // broadcast, fetch it by txid
        try {
          const t = await this.d.fetchTx(m.txid)
          if (t !== undefined) got = { tx: t, spv: false }
        } catch {
          /* unresolvable yet — parked below */
        }
      }
      if (got === undefined) {
        // a direct-pushed delivered can land before the disclosure tx is
        // fetchable anywhere; the envelope is one-shot, so park it on the op —
        // advance() re-runs this handler every tick, bounded by the refund
        // deadline that already guards this state
        await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
          cause: 'delivered_unresolvable',
          data: { pendingDelivered: { from, m, refs } }
        })
        return
      }
      const tx = got.tx
      // the reveal must be a valid spend of OUR escrow output — checked by
      // running the script here, so the disclosure is accepted the moment it
      // arrives instead of when an indexer shows it
      if (tx.id('hex') !== m.txid || !spendValid(tx, 0, utxo)) return
      this.d.rememberTx?.(tx)
      spv = got.spv
      claim = await this.d.call('sale#claim', {
        txHex: tx.toHex(),
        hWrapped: q.cfg.hWrapped,
        sellerPub: from,
        offerMode,
        commitment
      })
    }
    await this.d.store.settleBudget(op.data.escrowAction as string)
    let text: string | undefined
    try {
      const bytes = fromHex(claim.valueHex)
      text = Buffer.from(chosen.mode === 'fragment' ? parseFragment(bytes).d : bytes).toString('utf8')
    } catch {
      /* opaque payload */
    }
    // D4 — purchased content is UNTRUSTED counterparty data: the taint tag is
    // minted here at the verification boundary and must survive every consumer
    // surface (deals tool, importRecord). Content under this tag is data,
    // never instructions — the hostile-payload invariant is zero sats moved.
    const result = {
      mode: claim.mode,
      verified: claim.verified,
      authorVerified: claim.authorVerified,
      valueHex: claim.valueHex,
      text,
      taint: 'untrusted-purchased-content',
      disclosureTxid: m.txid
    }
    const bad = claim.verified !== true
    const next = await this.d.store.transition(op.operationId, op.revision, bad ? 'disputed' : 'disclosure_verified', {
      cause: bad ? 'payload_does_not_match_commitment' : 'disclosure_verified',
      data: { result, pendingDelivered: null },
      dueHeight: null,
      evidence: {
        txid: m.txid,
        verified: claim.verified,
        authorVerified: claim.authorVerified,
        reason: claim.reason,
        spv,
        ...(refs !== undefined ? { refs } : {})
      }
    })
    // auto-evaluation: with the policy opt-in the verification result IS the
    // usefulness judgment — cryptographic commitment satisfied ⇒ useful;
    // anything else already exited `disputed` above. Human evaluation stays
    // the default (autoUseful unset).
    if (!bad && this.o.autoUseful === true)
      await this.d.store.transition(next.operationId, next.revision, 'useful', {
        cause: 'auto_evaluation',
        data: { usefulness: { useful: true, evidence: { auto: true, verified: claim.verified, authorVerified: claim.authorVerified } } }
      })
  }

  /** what the seller is handed for an escrow this instance funded — bounded
   *  so the whole envelope stays under the box's MaxContent cap (16KiB sealed
   *  — roughly ~8KiB of payload once encoded/encrypted): the covenant script
   *  + fields already cost a chunk of it, and an EF carrying a fat parent
   *  (a mint tx) is exactly what blows the seal. Over budget the tx field is
   *  omitted and the seller falls back to fetchTx. */
  private escrowWire(txid: string, rawHex?: string): { tx: string } | {} {
    const w = this.d.txWire?.(txid, 3000) ?? rawHex
    return w !== undefined && w.length <= 6000 ? { tx: w } : {}
  }

  private noteFor(
    op: Operation,
    wantId: string,
    s: StoredOffer,
    escrow: any,
    utxo: any,
    escrowAction: string,
    amounts: { purchaseSats: number; feeSats: number; lockedSats: number },
    rawHex?: string
  ): Extract<MarketMessage, { type: 'note' }> {
    // the escrow travels inside the note too — after a database loss the
    // rebuilt operation can check and re-broadcast it without asking any
    // indexer; the wire stays small enough to fit the box beside the rest
    // (a pathological multi-input escrow simply falls back to fetchTx)
    const cand = this.d.txWire?.(utxo.txid, 2000) ?? rawHex
    const tx = cand !== undefined && cand.length <= 6000 ? cand : undefined
    return {
      type: 'note',
      v: 1,
      wantId,
      offerId: s.so.offer.offerId,
      buyerOp: op.operationId,
      policyHash: op.policyHash,
      want: op.data.want as SignedWant,
      offer: { so: s.so, ...(s.commitment !== undefined ? { commitment: s.commitment } : {}), ...(s.mode !== undefined ? { mode: s.mode } : {}) },
      escrow,
      utxo,
      escrowAction,
      amounts,
      ...(tx !== undefined ? { tx } : {})
    }
  }

  /** does a buyer purchase exist for this want (held `delivered` envelopes are
   *  dropped only when none exists AND the envelope is old) */
  async hasPurchase(wantId: string): Promise<boolean> {
    return (await this.d.store.operationByKey(`buyer:${this.d.network}:${wantId}`)) !== undefined
  }

  /** a recovery note settles only when its side has nothing left to do on-chain.
   *  Buyer notes key on wantId; seller notes key on offerId (= operationId). */
  async noteSettled(id: string, kind: 'buyer' | 'seller' = 'buyer'): Promise<boolean> {
    if (kind === 'seller') {
      const op = await this.d.store.operation(id)
      return op !== undefined && ['settled', 'expired', 'completion_failed', 'preparation_failed', 'declined', 'canceled'].includes(op.state)
    }
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${id}`)
    return op !== undefined && ['disclosure_verified', 'useful', 'not_useful', 'refunded', 'disputed'].includes(op.state)
  }

  /** a self-note rebuilds the operation after a database loss. Throwing keeps
   *  the inbox item pending (escrow not observable yet → retried next tick). */
  private async onNote(n: Extract<MarketMessage, { type: 'note' }>): Promise<void> {
    const sw = n?.want
    if (!sw?.want || !verifyWant(sw.want, sw.sig, { allowExpired: true })) return
    if (sw.want.pub !== this.me || sw.want.net !== this.d.network || sw.want.wantId !== n.wantId) return
    const so = n.offer?.so
    if (!so?.offer || !verifyOffer(so.offer, so.sig, { allowExpired: true })) return
    if (so.offer.wantId !== n.wantId || so.offer.buyerPub !== this.me || so.offer.offerId !== n.offerId) return
    // the escrow bytes ride in the note — no indexer needed to observe it;
    // a missing/foreign payload falls back to the chain view as before
    let tx: Transaction | undefined
    if (typeof n.tx === 'string') {
      try {
        const got = await this.readTx(n.tx)
        if (got.tx.id('hex') === n.utxo.txid) {
          tx = got.tx
          this.d.rememberTx?.(tx)
        }
      } catch {
        /* unreadable — fall through to the chain view */
      }
    }
    tx ??= await this.d.fetchTx(n.utxo.txid)
    const out = tx?.outputs[n.utxo.vout]
    if (!tx || !out) throw new Error('recovery note: escrow not observable yet')
    if (tx.id('hex') !== n.utxo.txid || out.lockingScript.toHex() !== n.utxo.script || out.satoshis !== n.utxo.satoshis) return
    const dedupeKey = `buyer:${this.d.network}:${n.wantId}`
    const key = `${so.offer.pub}:${so.offer.offerId}`
    const { operation, created } = await this.d.store.createOperation({
      // rebuilt under the original id — the seller's note has always kept the
      // offerId, and an external caller holding this handle must keep working
      ...(typeof n.buyerOp === 'string' ? { operationId: n.buyerOp } : {}),
      role: 'buyer',
      kind: 'purchase',
      dedupeKey,
      policyHash: n.policyHash,
      network: this.d.network,
      state: 'published',
      data: {
        want: sw,
        offers: {
          [key]: {
            so,
            ...(n.offer.commitment !== undefined ? { commitment: n.offer.commitment } : {}),
            ...(n.offer.mode !== undefined ? { mode: n.offer.mode } : {})
          }
        },
        selected: { key },
        escrow: n.escrow,
        utxo: n.utxo,
        escrowTxid: n.utxo.txid,
        escrowAction: n.escrowAction,
        amounts: n.amounts,
        restored: true,
        ...(n.escrow?.direct === true ? { direct: true } : {})
      }
    })
    if (!created) return
    await this.d.store.transition(operation.operationId, operation.revision, 'awaiting_disclosure', {
      cause: 'restored_from_note',
      dueHeight: Number(n.escrow.refundAfter)
    })
  }

  /** the seller's deterministic accept-refusal — the deal cannot complete;
   *  mark it declined and let the armed refund path reclaim at refundAfter
   *  (the covenant gates the unwind, the signal just ends the blind wait) */
  private async onDecline(from: string, m: Extract<MarketMessage, { type: 'decline' }>): Promise<void> {
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${m.wantId}`)
    if (op === undefined || op.state !== 'awaiting_disclosure') return
    const sel = op.data.selected as { key: string } | undefined
    const s = sel === undefined ? undefined : (op.data.offers as Record<string, StoredOffer>)[sel.key]
    if (s === undefined || s.so.offer.pub !== from || s.so.offer.offerId !== m.offerId) return
    await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
      cause: 'seller_declined',
      evidence: { reason: m.reason }
    })
  }

  protected async onMessage(from: string, m: MarketMessage, refs?: BoxRef[]): Promise<void> {
    if (m?.type === 'quote') return await this.onQuote(from, m)
    if (m?.type === 'accept') return await this.onAccept(from, m)
    if (m?.type === 'delivered') return await this.onDelivered(from, m, refs)
    if (m?.type === 'decline') return await this.onDecline(from, m)
    // recovery notes are only ever handled when they came from our own key
    if (m?.type === 'note' && from === this.me) return await this.onNote(m)
    if (m?.type === 'sellerNote' && from === this.me) return await this.onSellerNote(m)
  }

  private async escalate(op: Operation, reason: string, actionDigest: string, maxExtraSats: number, evidence: unknown, state = 'escalated'): Promise<void> {
    const a = await this.d.store.requestApproval({
      operationId: op.operationId,
      reason,
      policyHash: op.policyHash,
      actionDigest,
      maxExtraSats,
      expiresAt: this.now() + this.o.approvalTtlMs,
      evidence
    })
    await this.d.store.transition(op.operationId, op.revision, state, {
      cause: `needs_human:${reason}`,
      data: { pendingApproval: { approvalId: a.approvalId, actionDigest, reason } },
      evidence
    })
  }

  private async select(op: Operation, height: number, opts?: { expire?: boolean }): Promise<void> {
    const p = await this.d.store.activePolicy(op.policyHash)
    if (!p) {
      await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: 'policy_revoked_or_expired' })
      return
    }
    const offers = op.data.offers as Record<string, StoredOffer>
    const cands: ScoreCandidate[] = Object.entries(offers).map(([key, s]) => {
      const q = s.so.offer.offer as any
      const live = !s.reject && Number(q?.expiresAt ?? 0) !== 0 ? Number(q.expiresAt) > height : !s.reject
      const anchored = !!(s.commitment?.proof?.path && s.commitment?.batchR && s.commitment?.anchorTxid)
      return {
        offerKey: key,
        eligible: live && Number(q?.cfg?.refundAfter ?? 0) > height + this.o.sellerSafetyBlocks,
        maxAllInSats: s.so.offer.priceSats + Number(q?.cfg?.maxFee ?? 0) + this.o.escrowFeeReserveSats,
        features: { relevance: 10000, provenance: anchored ? 10000 : s.commitment?.leaf ? 5000 : 0, history: 5000, latency: 10000 }
      }
    })
    const ranking = rankOffers(cands, {
      maxAllInSats: p.limits.perPurchaseSats + p.limits.perTxFeeSats,
      minimumScoreBps: p.scoring.minimumScoreBps,
      weights: p.scoring.weights
    })
    const rejected = [
      ...ranking.rejected,
      ...Object.entries(offers)
        .filter(([, s]) => s.reject)
        .map(([k, s]) => ({ offerKey: k, reason: s.reject! }))
    ]
    const top = ranking.ranked[0]
    if (!top) {
      // a live want may still be answered — republish the same signed record
      // (unchanged revision, so arriving offers still match) and collect one
      // more window instead of expiring on a slow round. Only the want's own
      // exp ends the deal — the collection window is not the want's lifetime.
      const w = (op.data.want as SignedWant | undefined)?.want
      if (opts?.expire === true && w?.exp !== undefined && w.exp > this.now()) {
        await this.d.store.transition(op.operationId, op.revision, 'collecting', {
          cause: 'want_republished',
          dueAt: this.now() + p.scoring.collectMs,
          evidence: { rejected },
          outbox: [{ actionId: `${op.operationId}:want:r${op.revision}`, kind: 'publish-want', destination: 'feed', payload: op.data.want }]
        })
        return
      }
      // early-select passes expire:false — a window full of failing offers
      // can't end the collection early; only reaching dueAt may expire it
      if (opts?.expire !== false)
        await this.d.store.transition(op.operationId, op.revision, 'expired', { cause: 'no_qualifying_offer', evidence: { rejected }, dueAt: null })
      return
    }
    const next = await this.d.store.transition(op.operationId, op.revision, 'selected', {
      cause: 'offer_selected',
      data: { selected: { key: top.offerKey, scoreBps: top.scoreBps }, ranking: { ranked: ranking.ranked, rejected } },
      dueAt: null,
      evidence: { ranked: ranking.ranked, rejected }
    })
    await this.fund(next, p)
  }

  private async fund(op: Operation, p: AutomationPolicy): Promise<void> {
    // a new escrow is a new spend — paused while stopped or deaf, retried on
    // the next tick (the op stays 'selected'/'escalated', it does not fail)
    if (this.d.killed?.() || this.d.deaf?.()) return
    const sel = op.data.selected as { key: string }
    const s = (op.data.offers as Record<string, StoredOffer>)[sel.key]
    const q = s.so.offer.offer as any
    const w = (op.data.want as SignedWant).want
    const price = s.so.offer.priceSats,
      maxFee = Number(q.cfg.maxFee)
    const amounts = { purchaseSats: price, feeSats: maxFee + this.o.escrowFeeReserveSats, lockedSats: price + maxFee }
    const actionDigest = digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'escrow', offerKey: sel.key, cfg: q.cfg, amounts })
    const approved = op.state === 'escalated' && (await this.d.store.consumeApproval(op.operationId, actionDigest))
    if (op.state === 'escalated' && !approved) {
      const a = (await this.d.store.approvals()).find((x) => x.approvalId === (op.data.pendingApproval as any)?.approvalId)
      if (a?.status === 'denied') await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: 'human_denied' })
      else if (a && a.expiresAt <= this.now()) await this.d.store.transition(op.operationId, op.revision, 'expired', { cause: 'approval_expired' })
      return
    }
    if (!approved) {
      const decision = decidePurchase(p, {
        network: this.d.network,
        agentPub: this.me,
        domains: w.domains,
        priceSats: amounts.purchaseSats,
        feeSats: amounts.feeSats,
        lockedSats: amounts.lockedSats,
        now: this.now()
      })
      if (decision.kind === 'deny') {
        await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: decision.code })
        return
      }
      if (decision.kind === 'escalate')
        return await this.escalate(op, decision.code, actionDigest, amounts.purchaseSats + amounts.feeSats, { offerKey: sel.key, amounts })
    }
    // a released action is a tombstone — a starvation retry must reserve a
    // fresh one, so the id carries the attempt number (released rows never
    // count against caps; data.escrowAction tracks the live attempt)
    const tries = (op.data.fundingTries as number | undefined) ?? 0
    const ctx: ActionContext = {
      operationId: op.operationId,
      actionId: `${op.operationId}:escrow:${tries}`,
      policyHash: op.policyHash,
      networkId: this.d.network
    }
    const b = await this.d.store.reserveBudget(ctx, amounts, approved)
    if (!b.ok) {
      if (CAP.test(b.code)) return await this.escalate(op, b.code, actionDigest, amounts.purchaseSats + amounts.feeSats, { offerKey: sel.key, amounts })
      await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: b.code })
      return
    }
    const funding = await this.d.store.transition(op.operationId, op.revision, 'funding', {
      cause: approved ? 'human_approved' : 'policy_allowed',
      data: { escrowAction: ctx.actionId, amounts, fundingTries: tries + 1, fundingSince: (op.data.fundingSince as number | undefined) ?? this.now() }
    })
    this.inflight.add(op.operationId)
    try {
      // the direct rail — the quote advertised outright payment and the price
      // sits inside the operator's no-recourse ceiling. Same policy, budget
      // and wallet-custody mechanics; what changes is the settlement itself:
      // a P2PKH output to the seller, payment-final, instead of a covenant
      // a live (non-'sale#*') broadcast started but not yet joined — the
      // accept envelope queues/flies during the broadcaster RTT, then the
      // verdict is applied to the post-transition op below
      let pendingSubmit: Promise<{ state: string; txid: string; detail?: string }> | undefined
      const direct = q.direct === true && this.o.directMaxSats !== undefined && this.o.directMaxSats > 0 && price <= this.o.directMaxSats
      if (direct) {
        const payScript = new P2PKH().lock(Array.from(fromHex(q.cfg.sellerPkh))).toHex()
        const payOffer = {
          sellerPub: q.sellerPub,
          offerSig: q.offerSig,
          expiresAt: q.expiresAt,
          commitment: s.commitment ?? (s.so.offer.leaf ? { leaf: s.so.offer.leaf } : undefined)
        }
        let pay: any
        let payTx: Transaction | undefined
        if (this.wallet && this.d.settleEscrow) {
          let built: any
          try {
            built = await this.d.call('sale#payBuild', { amount: price, scriptHex: payScript, cfg: q.cfg, offer: payOffer })
          } catch (e) {
            await this.d.store.releaseBudget(ctx.actionId)
            if (starved(e)) return await this.starveBounce(funding, e)
            await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
              cause: 'payment_build_refused',
              lastError: (e as Error).message.slice(0, 500)
            })
            return
          }
          const tx = (built.tx as Transaction | undefined) ?? Transaction.fromHex(built.txHex)
          payTx = tx
          await Promise.all(
            tx.inputs.map(async (inp) => {
              if (!inp.sourceTransaction && inp.sourceTXID) inp.sourceTransaction = (await this.d.fetchTx(inp.sourceTXID)) as Transaction
            })
          )
          await this.wallet.prepare(ctx, 'payment', tx)
          await this.d.store.transition(funding.operationId, funding.revision, 'funding', {
            cause: 'payment_prepared',
            data: { pendingEscrow: { cfg: { direct: true, refundAfter: q.cfg.refundAfter, depositorPkh: pkhOf(this.me) }, utxo: built.utxo } }
          })
          await this.d.afterPrepare?.(ctx.actionId)
          // broadcast starts now; the accept flies while the RTT is out —
          // the verdict is joined after the awaiting_disclosure transition
          pendingSubmit = custodySubmit(this.d.executor ?? executor(), this.wallet, ctx.actionId)
          await this.d.settleEscrow(tx)
          this.d.rememberTx?.(tx)
          pay = { accepted: true, txid: built.utxo.txid, utxo: built.utxo }
        } else {
          try {
            pay = await this.d.call('sale#pay', { amount: price, scriptHex: payScript, cfg: q.cfg, offer: payOffer })
          } catch (e) {
            await this.d.store.releaseBudget(ctx.actionId)
            if (starved(e)) return await this.starveBounce(funding, e)
            await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
              cause: 'payment_refused_before_broadcast',
              lastError: (e as Error).message.slice(0, 500)
            })
            return
          }
        }
        if (pay.accepted !== true) {
          if (/SpendGuard refused/i.test(String(pay.reason))) {
            await this.d.store.releaseBudget(ctx.actionId)
            await this.spendRefused(funding, String(pay.reason))
          } else
            await this.escalate(
              funding,
              'unresolved_payment',
              digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-escrow' }),
              0,
              { reason: pay.reason },
              'uncertain'
            )
          return
        }
        await this.d.store.commitBudget(ctx.actionId)
        // the escrow slot carries {direct:true} so every rebuild path (note,
        // reconcile, wallet-row resume) reconstructs the rail from the data,
        // not the message that brought it
        const directEscrow = { ...q.cfg, direct: true, depositorPkh: pkhOf(this.me) }
        const cur = (await this.d.store.operation(op.operationId))!
        await this.d.store.transition(cur.operationId, cur.revision, 'awaiting_disclosure', {
          cause: 'payment_broadcast',
          data: { direct: true, escrow: directEscrow, utxo: pay.utxo, escrowTxid: pay.txid },
          dueHeight: Number(q.cfg.refundAfter),
          outbox: [
            {
              actionId: `${op.operationId}:accept`,
              kind: 'message',
              destination: s.so.offer.pub,
              payload: {
                type: 'accept',
                wantId: w.wantId,
                offerId: s.so.offer.offerId,
                utxo: pay.utxo,
                depositorPkh: pkhOf(this.me),
                ...this.escrowWire(pay.txid, payTx?.toHex() ?? pay.txHex)
              }
            },
            {
              actionId: `${op.operationId}:note`,
              kind: 'message',
              destination: this.me,
              payload: this.noteFor(op, w.wantId, s, directEscrow, pay.utxo, ctx.actionId, amounts, payTx?.toHex() ?? pay.txHex)
            }
          ]
        })
        return
      }
      let r: any
      let escrowTx: Transaction | undefined
      if (this.wallet && this.d.settleEscrow) {
        let built: any
        try {
          built = await this.d.call('sale#escrowBuild', {
            cfg: q.cfg,
            amount: amounts.lockedSats,
            offer: {
              sellerPub: q.sellerPub,
              offerSig: q.offerSig,
              expiresAt: q.expiresAt,
              commitment: s.commitment ?? (s.so.offer.leaf ? { leaf: s.so.offer.leaf } : undefined)
            }
          })
        } catch (e) {
          await this.d.store.releaseBudget(ctx.actionId)
          if (starved(e)) return await this.starveBounce(funding, e)
          await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
            cause: 'escrow_build_refused',
            lastError: (e as Error).message.slice(0, 500)
          })
          return
        }
        const tx = (built.tx as Transaction | undefined) ?? Transaction.fromHex(built.txHex)
        escrowTx = tx
        await Promise.all(
          tx.inputs.map(async (inp) => {
            if (!inp.sourceTransaction && inp.sourceTXID) inp.sourceTransaction = (await this.d.fetchTx(inp.sourceTXID)) as Transaction
          })
        )
        await this.wallet.prepare(ctx, 'escrow', tx)
        await this.d.store.transition(funding.operationId, funding.revision, 'funding', {
          cause: 'escrow_prepared',
          data: { pendingEscrow: { cfg: built.escrow, utxo: built.utxo, feeSats: built.feeSats } }
        })
        await this.d.afterPrepare?.(ctx.actionId)
        // broadcast starts now; the accept flies while the RTT is out —
        // the verdict is joined after the awaiting_disclosure transition
        pendingSubmit = custodySubmit(this.d.executor ?? executor(), this.wallet, ctx.actionId)
        await this.d.settleEscrow(tx)
        this.d.rememberTx?.(tx)
        r = { accepted: true, txid: built.utxo.txid, utxo: built.utxo, escrow: built.escrow }
      } else
        try {
          r = await this.d.call('sale#escrow', {
            cfg: q.cfg,
            amount: amounts.lockedSats,
            offer: {
              sellerPub: q.sellerPub,
              offerSig: q.offerSig,
              expiresAt: q.expiresAt,
              commitment: s.commitment ?? (s.so.offer.leaf ? { leaf: s.so.offer.leaf } : undefined)
            }
          })
        } catch (e) {
          await this.d.store.releaseBudget(ctx.actionId)
          if (starved(e)) return await this.starveBounce(funding, e)
          await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
            cause: 'escrow_refused_before_broadcast',
            lastError: (e as Error).message.slice(0, 500)
          })
          return
        }
      if (r.accepted !== true) {
        if (/SpendGuard refused/i.test(String(r.reason))) {
          await this.d.store.releaseBudget(ctx.actionId)
          await this.spendRefused(funding, String(r.reason))
        } else
          await this.escalate(
            funding,
            'unresolved_payment',
            digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-escrow' }),
            0,
            { reason: r.reason },
            'uncertain'
          )
        return
      }
      await this.d.store.commitBudget(ctx.actionId)
      const cur = (await this.d.store.operation(op.operationId))!
      await this.d.store.transition(cur.operationId, cur.revision, 'awaiting_disclosure', {
        cause: 'escrow_broadcast',
        data: { escrow: r.escrow, utxo: r.utxo, escrowTxid: r.txid },
        dueHeight: Number(q.cfg.refundAfter),
        outbox: [
          {
            actionId: `${op.operationId}:accept`,
            kind: 'message',
            destination: s.so.offer.pub,
            payload: {
              type: 'accept',
              wantId: w.wantId,
              offerId: s.so.offer.offerId,
              utxo: r.utxo,
              depositorPkh: r.escrow.depositorPkh,
              ...this.escrowWire(r.txid, escrowTx?.toHex())
            }
          },
          {
            actionId: `${op.operationId}:note`,
            kind: 'message',
            destination: this.me,
            payload: this.noteFor(op, w.wantId, s, r.escrow, r.utxo, ctx.actionId, amounts, escrowTx?.toHex())
          }
        ]
      })
      // join the broadcast that overlapped the transition above — on refusal
      // the state machine unwinds honestly: SpendGuard means the sats never
      // left (release covers the committed row); anything else stays
      // 'uncertain' for a human, since the wire may still confirm
      if (pendingSubmit !== undefined) {
        const sub = await pendingSubmit
        if (sub.state !== 'submitted' && sub.state !== 'observed' && sub.state !== 'confirmed') {
          const live = (await this.d.store.operation(op.operationId))!
          if (sub.state === 'rejected' && /SpendGuard refused/i.test(sub.detail ?? '')) {
            await this.d.store.releaseBudget(ctx.actionId)
            await this.spendRefused(live, sub.detail ?? '')
          } else
            await this.escalate(
              live,
              'unresolved_payment',
              digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-escrow' }),
              0,
              { reason: sub.detail ?? sub.state },
              'uncertain'
            )
        }
      }
    } finally {
      this.inflight.delete(op.operationId)
    }
  }

  private async refund(op: Operation): Promise<void> {
    const next = await this.d.store.transition(op.operationId, op.revision, 'refunding', { cause: 'refund_deadline_reached' })
    const r = await this.d.call('sale#refund', { cfg: op.data.escrow, utxo: op.data.utxo })
    if (r.accepted === true) {
      await this.d.store.refundBudget(op.data.escrowAction as string)
      await this.d.store.transition(next.operationId, next.revision, 'refunded', {
        cause: 'refund_broadcast',
        data: { refundTxid: r.txid, refundSats: r.refundSats },
        dueHeight: null
      })
    } else
      await this.escalate(
        next,
        'unresolved_settlement',
        digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-refund' }),
        0,
        { reason: r.reason },
        'uncertain'
      )
  }

  /** `uncertain` gets a machine answer before a human is needed: look at the
   *  escrow/disclosure transaction itself instead of waiting for a height or
   *  an operator decision. */
  private async reconcileUncertain(op: Operation): Promise<void> {
    const pa = op.data.pendingApproval as { approvalId?: string; reason?: string } | undefined
    if (pa?.reason === 'unresolved_settlement') return await this.reconcileRefund(op, pa)
    if (pa?.reason !== 'unresolved_payment') return
    const cancel = async () => {
      if (pa?.approvalId !== undefined) await this.d.store.cancelApproval(pa.approvalId)
    }
    const pending = op.data.pendingEscrow as { cfg?: any; utxo?: { txid?: string } } | undefined
    const actionId = op.data.escrowAction as string | undefined
    const wtx = actionId !== undefined ? await this.d.store.walletTx({ actionId }) : undefined
    const txid = pending?.utxo?.txid ?? wtx?.txid
    if (txid === undefined) {
      // stopped before the escrow was ever prepared — nothing was broadcast
      if (actionId !== undefined) {
        await this.d.store.releaseBudget(actionId)
        await this.d.store.releaseLeases(actionId)
      }
      await cancel()
      await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_never_built' })
      return
    }
    if (this.d.txFacts === undefined || !this.reconcileGate(op.operationId, txid, op.nextReconcileAt)) return
    const facts = await this.d.txFacts(txid)
    if (facts.verdict === 'dead') {
      // the prepared tx never aired — drop its input leases AND mark the row
      // dead, or the pair disagrees: a released lease beside a live-looking
      // 'prepared' row is exactly the wedge the lease-liveness check reads
      if (actionId !== undefined) {
        await this.d.store.releaseBudget(actionId)
        await this.d.store.releaseLeases(actionId)
        // a dead verdict on a row that may have escaped owes the coin book
        // its custody rollback — route through the coordinator so driver
        // mode journals the edge and both modes fire txDied
        if (wtx !== undefined && (wtx.state === 'prepared' || wtx.state === 'submission_unknown')) {
          if (this.wallet !== undefined) await custodyMarkRejected(this.d.executor ?? executor(), this.wallet, wtx.actionId, { cause: 'escrow_rejected' })
          else {
            await this.d.store.setTxState(actionId, 'rejected', { cause: 'escrow_rejected' })
            await Promise.resolve(this.d.txDied?.(Transaction.fromHex(wtx.rawHex))).catch(() => undefined)
          }
        }
      }
      await cancel()
      await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_rejected' })
      return
    }
    if (facts.verdict === 'pending' && facts.tx === undefined) {
      if (op.data.reconcilePing !== true)
        await this.d.store.transition(op.operationId, op.revision, 'uncertain', { cause: 'reconcile_pending', data: { reconcilePing: true } })
      return
    }
    // mined | held — the escrow is observable: commit and resume the normal
    // path (a broadcaster seen-hint is a wake signal, never escrow evidence)
    const sel = op.data.selected as { key: string }
    const s = (op.data.offers as Record<string, StoredOffer>)[sel.key]
    if (s === undefined) return
    const w = (op.data.want as SignedWant).want
    let escrow: any = pending?.cfg
    let utxo: any = pending?.utxo
    if ((escrow === undefined || utxo === undefined) && wtx !== undefined) {
      // the prepared-escrow record is gone but the wallet row survived — find
      // the escrow output in the stored bytes and rebuild the cfg from the offer
      const q = s.so.offer.offer as any
      const depositorPkh = pkhOf(this.me)
      const direct = q.direct === true
      const script = direct
        ? new P2PKH().lock(Array.from(fromHex(q.cfg.sellerPkh))).toHex()
        : DisclosureSaleHost.escrowScript(
            { ...q.cfg, price: BigInt(q.cfg.price), refundAfter: BigInt(q.cfg.refundAfter), maxFee: BigInt(q.cfg.maxFee) },
            depositorPkh
          )
      const tx = Transaction.fromHex(wtx.rawHex)
      const vout = tx.outputs.findIndex((o) => o.lockingScript.toHex() === script)
      if (vout < 0) return // cannot prove which output is the escrow — stay uncertain
      escrow = { ...q.cfg, depositorPkh, ...(direct ? { direct: true } : {}) }
      utxo = { txid, vout, satoshis: tx.outputs[vout].satoshis ?? 0, script }
    }
    if (escrow === undefined || utxo === undefined) return
    if (wtx !== undefined) await this.d.settleEscrow?.(Transaction.fromHex(wtx.rawHex))
    if (actionId !== undefined) await this.d.store.commitBudget(actionId)
    await cancel()
    await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
      cause: 'reconciled_escrow_observed',
      data: { escrow, utxo, escrowTxid: txid, ...(escrow.direct === true ? { direct: true } : {}) },
      dueHeight: Number(escrow.refundAfter),
      outbox: [
        {
          actionId: `${op.operationId}:accept`,
          kind: 'message',
          destination: s.so.offer.pub,
          payload: {
            type: 'accept',
            wantId: w.wantId,
            offerId: s.so.offer.offerId,
            utxo,
            depositorPkh: String(escrow.depositorPkh),
            ...this.escrowWire(txid, wtx?.rawHex)
          }
        },
        {
          actionId: `${op.operationId}:note`,
          kind: 'message',
          destination: this.me,
          payload: this.noteFor(
            op,
            w.wantId,
            s,
            escrow,
            utxo,
            actionId ?? `${op.operationId}:escrow`,
            op.data.amounts as { purchaseSats: number; feeSats: number; lockedSats: number },
            wtx?.rawHex
          )
        }
      ]
    })
  }

  /** a refused refund retries on the reconcile throttle — after five refusals
   *  the operation stays `uncertain` for the operator. An nLockTime-immature
   *  spend (the covenant's refundAfter height hasn't arrived) is NOT a
   *  refusal: park until the height and don't burn tries — nodes can only
   *  call it non-final below that height. */
  private async reconcileRefund(op: Operation, pa: { approvalId?: string }): Promise<void> {
    const tries = Number(op.data.refundTries ?? 0)
    const refundAfter = Number((op.data.escrow as { refundAfter?: string | number } | undefined)?.refundAfter ?? 0)
    if (refundAfter > 0 && (await this.d.height()) < refundAfter) {
      if (op.dueHeight !== refundAfter || tries !== 0)
        await this.d.store.transition(op.operationId, op.revision, 'uncertain', {
          cause: 'refund_wait_height',
          dueHeight: refundAfter,
          data: { refundTries: 0 }
        })
      return
    }
    if (tries >= 5) return
    if (!this.reconcileGate(op.operationId, op.data.escrowTxid as string | undefined, op.nextReconcileAt)) return
    const r = await this.d.call('sale#refund', { cfg: op.data.escrow, utxo: op.data.utxo })
    if (r.accepted === true) {
      await this.d.store.refundBudget(op.data.escrowAction as string)
      if (pa.approvalId !== undefined) await this.d.store.cancelApproval(pa.approvalId)
      await this.d.store.transition(op.operationId, op.revision, 'refunded', {
        cause: 'refund_broadcast',
        data: { refundTxid: r.txid, refundSats: r.refundSats },
        dueHeight: null
      })
    } else if (/non.?final|lock.?time|premature|immature|too early|height/i.test(String((r as { reason?: string }).reason ?? ''))) {
      // the broadcaster still calls it immature (height lag on its view) —
      // park on the same height gate rather than counting a try
      await this.d.store.transition(op.operationId, op.revision, 'uncertain', {
        cause: 'refund_retry_immature',
        dueHeight: refundAfter > 0 ? refundAfter : null,
        data: { refundTries: 0 }
      })
    } else {
      await this.d.store.transition(op.operationId, op.revision, 'uncertain', { cause: 'refund_retry_refused', data: { refundTries: tries + 1 } })
    }
  }

  /** D7 forced exit — an op in `state` longer than its declared maxAge leaves
   *  by the state's declared route. `updatedAt` is the state-entry clock:
   *  same-state activity (an offer recording, a retry note) bumps it, so
   *  only a truly silent state times out. */
  private async stuckExit(op: Operation, lim: { maxAgeMs: number; exit: StuckExit }): Promise<void> {
    if (lim.exit === 'abort') {
      // abort-eligible states never have a broadcast in flight — release any
      // reservation and leave by the terminal each state maps to
      if (op.data.escrowAction !== undefined) await this.d.store.releaseBudget(op.data.escrowAction as string)
      const to = op.state === 'preparing' ? 'preparation_failed' : op.state === 'collecting' ? 'expired' : 'canceled'
      await this.d.store.transition(op.operationId, op.revision, to, {
        cause: 'stuck_timeout',
        lastError: `state '${op.state}' exceeded its ${Math.round(lim.maxAgeMs / 1000)}s bound`,
        dueAt: null,
        dueHeight: null
      })
      return
    }
    if (lim.exit === 'refund') {
      if (op.data.direct === true) {
        // a direct payment can never be reclaimed — the honest terminal is
        // disputed, not refunded
        await this.d.store.transition(op.operationId, op.revision, 'disputed', { cause: 'stuck_timeout_payment_final', dueHeight: null })
        return
      }
      if (op.data.escrow !== undefined && op.data.utxo !== undefined) return await this.refund(op)
      // money never reached the escrow — abort is honest, 'refunded' is not
      if (op.data.escrowAction !== undefined) await this.d.store.releaseBudget(op.data.escrowAction as string)
      await this.d.store.transition(op.operationId, op.revision, 'canceled', { cause: 'stuck_timeout_no_escrow', dueHeight: null })
      return
    }
    // escalate:human — 'uncertain'/'escalated' already sit at this exit with a
    // pending approval; re-asserting would only spam events. Anything else
    // goes there now (a broadcast may have escaped — only a person closes it)
    if (op.state === 'uncertain' || op.state === 'escalated') return
    // 'escalated', not 'uncertain' — a stuck_state_timeout cause maps to the
    // unguarded needs.human edge whose declared target is 'escalated', and
    // reconcileUncertain has no route for this reason anyway (it only serves
    // unresolved_* waits). A blocked write here wedged the op silently.
    await this.escalate(
      op,
      'stuck_state_timeout',
      digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'stuck-exit', state: op.state }),
      0,
      { state: op.state, ageMs: this.now() - op.updatedAt },
      'escalated'
    )
  }

  /** D5's assertion surface: every nonterminal state's declared bound, and
   *  which ops currently violate theirs (post-tick this should be empty —
   *  an op past maxAge has already been pushed through its declared exit). */
  async dealStateAudit(): Promise<{
    limits: Record<string, { maxAgeMs: number; exit: StuckExit }>
    violations: Array<{ operationId: string; state: string; ageMs: number }>
  }> {
    const violations: Array<{ operationId: string; state: string; ageMs: number }> = []
    // PARKED states are declared-no-bound (external actor owns the exit) —
    // they're classified, not violations
    for (const op of await this.d.store.operations({ notStates: [...TERMINAL, ...PARKED] })) {
      const lim = STATE_LIMITS[op.state]
      if (lim === undefined) violations.push({ operationId: op.operationId, state: op.state, ageMs: this.now() - op.updatedAt })
      else if (lim.exit !== 'escalate:human' && this.now() - op.updatedAt > lim.maxAgeMs)
        violations.push({ operationId: op.operationId, state: op.state, ageMs: this.now() - op.updatedAt })
    }
    return { limits: { ...STATE_LIMITS }, violations }
  }

  protected async advance(op: Operation, height: number): Promise<void> {
    const lim = STATE_LIMITS[op.state]
    if (lim !== undefined && this.now() - op.updatedAt > lim.maxAgeMs) return await this.stuckExit(op, lim)
    if (op.role === 'buyer') {
      if (op.state === 'collecting') {
        const due = op.dueAt !== undefined && op.dueAt <= this.now()
        // early-select: the first acceptable offer fires selection once the
        // minimum collect window has passed — a cheaper offer landing inside
        // it still wins; collectMs stays as the collection ceiling. Ops from
        // before collectingSince existed treat the window as already open.
        const since = (op.data.collectingSince as number | undefined) ?? 0
        const windowOpen = this.now() - since >= this.o.minCollectMs
        const hasOffers = Object.keys((op.data.offers as Record<string, StoredOffer> | undefined) ?? {}).length > 0
        // quiet window (opt-in): while offers keep arriving the window stays
        // open past minCollectMs; it closes collectQuietMs after the last
        // offer. collectMs remains the hard ceiling either way.
        const quiet = this.o.collectQuietMs
        const quietDone = quiet > 0 && hasOffers && this.now() - ((op.data.lastOfferAt as number | undefined) ?? since) >= quiet
        if (due || (windowOpen && hasOffers && (quiet === 0 || quietDone))) return await this.select(op, height, { expire: due })
      }
      if (op.state === 'selected' || op.state === 'escalated') {
        const p = await this.d.store.activePolicy(op.policyHash)
        if (!p) {
          await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: 'policy_revoked_or_expired' })
          return
        }
        return await this.fund(op, p)
      }
      if (op.state === 'funding' && !this.inflight.has(op.operationId)) {
        const pending = op.data.pendingEscrow as { cfg: any; utxo: any } | undefined
        if (pending && this.wallet && this.d.settleEscrow) {
          const sub = await custodySubmit(this.d.executor ?? executor(), this.wallet, op.data.escrowAction as string)
          if (sub.state === 'submitted' || sub.state === 'observed' || sub.state === 'confirmed') {
            const wtx = await this.d.store.walletTx({ actionId: op.data.escrowAction as string })
            if (wtx) await this.d.settleEscrow(Transaction.fromHex(wtx.rawHex))
            await this.d.store.commitBudget(op.data.escrowAction as string)
            const sel2 = op.data.selected as { key: string }
            const s2 = (op.data.offers as Record<string, StoredOffer>)[sel2.key]
            const so2 = s2.so
            const w2 = (op.data.want as SignedWant).want
            await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
              cause: 'escrow_resumed_same_bytes',
              data: { escrow: pending.cfg, utxo: pending.utxo, escrowTxid: pending.utxo.txid, ...(pending.cfg.direct === true ? { direct: true } : {}) },
              dueHeight: Number(pending.cfg.refundAfter),
              outbox: [
                {
                  actionId: `${op.operationId}:accept`,
                  kind: 'message',
                  destination: so2.offer.pub,
                  payload: {
                    type: 'accept',
                    wantId: w2.wantId,
                    offerId: so2.offer.offerId,
                    utxo: pending.utxo,
                    depositorPkh: pending.cfg.depositorPkh,
                    ...this.escrowWire(pending.utxo.txid, wtx?.rawHex)
                  }
                },
                {
                  actionId: `${op.operationId}:note`,
                  kind: 'message',
                  destination: this.me,
                  payload: this.noteFor(
                    op,
                    w2.wantId,
                    s2,
                    pending.cfg,
                    pending.utxo,
                    op.data.escrowAction as string,
                    op.data.amounts as { purchaseSats: number; feeSats: number; lockedSats: number },
                    wtx?.rawHex
                  )
                }
              ]
            })
            return
          }
        }
        return await this.escalate(
          op,
          'unresolved_payment',
          digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-escrow' }),
          0,
          { note: 'process stopped between reserving and recording the escrow broadcast' },
          'uncertain'
        )
      }
      if (op.state === 'uncertain') return await this.reconcileUncertain(op)
      if (op.state === 'funding_deferred') {
        // a SpendGuard refusal does not mean the escrow never aired: the
        // accept already carried the signed tx and the counterparty may put
        // it on the wire through their own guard. Check the recorded txid
        // before waiting out the window — wire presence means the deal
        // funded anyway and 'selected' would build a SECOND escrow (double
        // pay). 'dead'/absent means the refusal held; the deferred retry is
        // safe because nothing left.
        const txid = op.data.escrowTxid as string | undefined
        if (txid !== undefined && this.d.txFacts !== undefined && this.reconcileGate(op.operationId, txid, op.nextReconcileAt)) {
          const v = (await this.d.txFacts(txid)).verdict
          if (v === 'mined' || v === 'pending') {
            await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
              cause: 'deferred_escrow_aired'
            })
            return
          }
        }
        // parked on a spend-policy window — resume at dueAt into a fresh
        // funding window (the parked span was never 'funding' time)
        if (op.dueAt !== undefined && op.dueAt <= this.now())
          await this.d.store.transition(op.operationId, op.revision, 'selected', {
            cause: 'spend_window_reset',
            data: { fundingSince: null }
          })
        return
      }
      if (op.state === 'refund_pending') {
        // armed by the funding_failed rescan — a covenant that vanished
        // after landing (reorg) has nothing left to reclaim
        const txid = op.data.escrowTxid as string | undefined
        if (
          txid !== undefined &&
          this.d.txFacts !== undefined &&
          this.reconcileGate(op.operationId, txid, op.nextReconcileAt) &&
          (await this.d.txFacts(txid)).verdict === 'dead'
        ) {
          await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'landed_escrow_gone', dueHeight: null })
          return
        }
        if (op.dueHeight !== undefined && height >= op.dueHeight) return await this.refund(op)
        return
      }
      if (op.state === 'awaiting_disclosure') {
        // a pushed 'delivered' can outrun tx propagation — its parked copy
        // re-runs the same verification until it resolves or the deal
        // deadline (refund) closes the state
        const pd = op.data.pendingDelivered as
          { from: string; m: Extract<MarketMessage, { type: 'delivered' }>; refs?: import('@nektar/bstack').BoxRef[] } | null | undefined
        if (pd != null) await this.onDelivered(pd.from, pd.m, pd.refs).catch(() => undefined)
        // a late rejection beats the refund path: the escrow never settled, so
        // the locked amount never left — releaseBudget frees the committed row
        const txid = op.data.escrowTxid as string | undefined
        if (
          txid !== undefined &&
          this.d.txFacts !== undefined &&
          this.reconcileGate(op.operationId, txid, op.nextReconcileAt) &&
          (await this.d.txFacts(txid)).verdict === 'dead'
        ) {
          await this.d.store.releaseBudget(op.data.escrowAction as string)
          await this.d.store.releaseLeases(op.data.escrowAction as string)
          await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_rejected_late', dueHeight: null })
          return
        }
        if (op.dueHeight !== undefined && height >= op.dueHeight) {
          // direct rail: the payment is final and the deliver-by lapsed —
          // record the outcome for scoring, there is nothing left to reclaim
          if (op.data.direct === true) {
            await this.d.store.transition(op.operationId, op.revision, 'disputed', { cause: 'delivery_lapsed_payment_final', dueHeight: null })
            return
          }
          return await this.refund(op)
        }
      }
    }
    if (op.role === 'observer' && (op.state === 'watching' || op.state === 'seizing') && op.dueHeight !== undefined && height >= op.dueHeight)
      return await this.seize(
        op.state === 'seizing' ? await this.d.store.transition(op.operationId, op.revision, 'watching', { cause: 'resume_after_restart' }) : op
      )
    if (op.role === 'seller' && op.kind === 'sale') {
      if (op.state === 'completing') return await this.complete(op)
      if (op.state === 'quoted' && op.dueHeight !== undefined && height >= op.dueHeight)
        await this.d.store.transition(op.operationId, op.revision, 'expired', { cause: 'quote_expired', dueHeight: null })
      if (op.state === 'completion_submitted') {
        const tx = await this.d.fetchTx(op.data.completeTxid as string)
        if (tx?.merklePath) {
          // settlement is money-final — a proof only counts once it sits
          // settleDepth blocks deep; shallower, the op parks and records the
          // proven height once (a same-state note, not a settle)
          const depth = height - tx.merklePath.blockHeight + 1
          if (depth >= (this.d.settleDepth ?? 1))
            await this.d.store.transition(op.operationId, op.revision, 'settled', {
              cause: 'disclosure_proof_observed',
              evidence: { blockHeight: tx.merklePath.blockHeight, depth }
            })
          else if (op.data.provenHeight !== tx.merklePath.blockHeight)
            await this.d.store.transition(op.operationId, op.revision, 'completion_submitted', {
              cause: 'disclosure_proof_awaiting_depth',
              data: { provenHeight: tx.merklePath.blockHeight }
            })
        } else if (
          this.d.txFacts !== undefined &&
          this.reconcileGate(op.operationId, op.data.completeTxid as string, op.nextReconcileAt) &&
          (await this.d.txFacts(op.data.completeTxid as string)).verdict === 'dead'
        ) {
          await this.d.store.transition(op.operationId, op.revision, 'completion_failed', { cause: 'disclosure_rejected_late' })
        }
      }
    }
  }
}
