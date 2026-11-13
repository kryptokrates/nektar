import { PublicKey, Transaction, P2PKH, type PrivateKey } from '@bsv/sdk'
import { signWant, verifyWant, signOffer, verifyOffer, type SignedWant, type SignedOffer, type BoxRef } from '@nektar/bstack'
import { verifyOfferSig, publicSaleQuote, saleQuoteWire } from '@nektar/identity'
import { DisclosureSaleHost, CheckpointBond, COVENANT_ID } from '@nektar/anchor'
import { parseFragment, fromHex } from '@nektar/core'
import { TxStore, spendValid, type Ingested } from '@nektar/chain'
import type { AutomationStore, InboxItem, Operation, OutboxItem } from './store.js'
import { WalletCoordinator } from './wallet.js'
import { decidePurchase, decideOffer, digestOf, type AutomationPolicy, type ActionContext } from './policy.js'
import { rankOffers, type ScoreCandidate } from './scoring.js'
import type { AIExecutor } from './ai.js'

export type MarketMessage =
  | { type: 'quote'; wantId: string; offerId: string; commitment: any; mode: string }
  | {
      type: 'accept'
      wantId: string
      offerId: string
      utxo: { txid: string; vout: number; satoshis: number; script: string }
      depositorPkh: string
      tx?: string
    }
  | { type: 'delivered'; wantId: string; offerId: string; txid: string; tx: string }
  // encrypted self-note the buyer keeps in its own inbox until the deal ends —
  // enough to rebuild the operation after losing the automation database
  | {
      type: 'note'
      v: 1
      wantId: string
      offerId: string
      buyerOp?: string
      policyHash: string
      want: SignedWant
      offer: { so: SignedOffer; commitment?: any; mode?: string }
      escrow: any
      utxo: { txid: string; vout: number; satoshis: number; script: string }
      escrowAction: string
      amounts: { purchaseSats: number; feeSats: number; lockedSats: number }
      tx?: string
    }
  // the seller's equivalent — rebuilds the quoted sale after a database loss;
  // signedOffer may be dropped to fit the box limit (the buyer's offer is
  // already published — `offer.offerSig` then proves the quote came from us)
  | {
      type: 'sellerNote'
      v: 1
      wantId: string
      offerId: string
      policyHash: string
      want: SignedWant
      shard: string
      seq: number
      priceSats: number
      offer: any
      signedOffer?: SignedOffer
      dueHeight: number
    }

export interface MarketTransport {
  publishWant: (sw: SignedWant) => Promise<void>
  publishOffer: (so: SignedOffer) => Promise<void>
  /** refs: stock plaintext object references riding the envelope — digest-
   *  pinned locators for external objects (evidence bundles, packs) */
  send: (toPub: string, msg: MarketMessage, refs?: import('@nektar/bstack').BoxRef[]) => Promise<void>
}

interface MarketMemory {
  shardManifest: (shard: string) => Promise<{ shardId: string; domains: string[]; offers?: Array<{ kind: string; priceSats?: number }> } | undefined>
  records: (shard: string) => Promise<Array<{ seq: number; level?: number; fragment: { d: Uint8Array } }>>
  marketCandidates?: (shard: string) => Promise<Array<{ seq: number; text: string }>>
  /** bstore locator of a shard's published pack — becomes the stock refs a
   *  `delivered` envelope carries so evidence resolves origin-free */
  packStoreRef?: (shard: string) => { bucket: string; key: string; sha256: string; size: number; endpoint?: string; chirp?: string } | undefined
}

export interface MarketDeps {
  store: AutomationStore
  call: (tool: string, args: any) => Promise<any>
  memory: MarketMemory
  key: PrivateKey
  network: string
  height: () => Promise<number>
  tip?: () => Promise<{ tip: string; tipHeight: number } | undefined>
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  /** what the chain view knows about a tx — reconciles uncertain payment and
   *  settlement states without waiting on block heights */
  txState?: (txid: string) => Promise<'mined' | 'seen' | 'rejected' | 'unknown'>
  broadcast?: (tx: Transaction) => Promise<import('@nektar/chain').BroadcastResult>
  /** the instance's transaction graph: `txWire` is what a counterparty is
   *  handed (BEEF when the ancestry is proven, else Extended Format, else
   *  raw), `readTx` parses such a payload and runs SPV against the
   *  instance's own headers, `rememberTx` keeps an accepted transaction */
  txWire?: (txid: string, maxBytes?: number) => string | undefined
  readTx?: (hex: string) => Promise<Ingested>
  rememberTx?: (tx: Transaction) => void
  settleEscrow?: (tx: Transaction) => void
  afterPrepare?: (actionId: string) => void | Promise<void>
  transport: MarketTransport
  ai: AIExecutor
  /** this agent's blogs deal-journal stream id; signed into wants/offers so
   *  the counterparty (and anyone holding the deal) can follow the journal */
  logStream?: string
  /** operator stop flag — consulted at every new-deal and new-escrow entry
   *  point (fail closed). In-flight deals are never gated: refunds and
   *  settlements keep running so the stop can't strand money. */
  killed?: () => boolean
  /** watchdog flag — every configured host currently unreachable. New deals
   *  and new escrows pause while deaf; in-flight deals continue. */
  deaf?: () => boolean
  now?: () => number
  opts?: Partial<MarketOptions>
}

export interface MarketOptions {
  refundBlocks: number
  offerTtlBlocks: number
  saleMaxFee: number
  escrowFeeReserveSats: number
  sellerSafetyBlocks: number
  approvalTtlMs: number
}

const DEFAULTS: MarketOptions = {
  refundBlocks: 144,
  offerTtlBlocks: 12,
  saleMaxFee: 10,
  escrowFeeReserveSats: 500,
  sellerSafetyBlocks: 2,
  approvalTtlMs: 86_400_000
}
const pkhOf = (pub: string) => Buffer.from(PublicKey.fromString(pub).toHash() as number[]).toString('hex')
/** a bstore ObjectRef → stock plaintext ref (digest-pinned locator). https
 *  only — uhrp/local http gateways can't ride a stock ref; the envelope
 *  still carries the disclosure fields themselves. */
const packBoxRef = (r: { sha256: string; size: number; endpoint?: string; bucket: string; key: string }): import('@nektar/bstack').BoxRef | undefined => {
  if (r.endpoint === undefined || !r.endpoint.startsWith('https://')) return undefined
  return { url: `${r.endpoint.replace(/\/+$/, '')}/${r.bucket}/${r.key.split('/').map(encodeURIComponent).join('/')}`, sha256: r.sha256, length: r.size }
}
/** the stock refs a `delivered` envelope carries: a digest-pinned locator
 *  for the sold shard's stored pack so the buyer resolves evidence through
 *  bstore/CHIRP rather than the seller's origin */
const deliveredPackRefs = (d: MarketDeps, op: Operation): import('@nektar/bstack').BoxRef[] | undefined => {
  const shard = (op.data as { shard?: string }).shard
  const r = shard !== undefined ? d.memory.packStoreRef?.(shard) : undefined
  const ref = r !== undefined ? packBoxRef(r) : undefined
  return ref !== undefined ? [ref] : undefined
}
const CAP = /_cap$|_above_policy$|^domain_outside_policy$/
/** D7 — the stuck-deal rule: no FSM state is unbounded. Every nonterminal
 *  state declares a maxAge (measured against the op's last transition —
 *  activity refreshes it) and a forced exit: abort (nothing on-chain can be
 *  in flight), refund (escrow exists — reclaim it), or escalate:human
 *  ('uncertain' — a broadcast may have escaped; only a person can close it).
 *  Terminal states need no entry; 'escalated'/'uncertain' already ARE the
 *  human exit — their bound is a re-assert, not a transition. */
export type StuckExit = 'abort' | 'refund' | 'escalate:human'
export const STATE_LIMITS: Record<string, { maxAgeMs: number; exit: StuckExit }> = {
  // buyer
  published: { maxAgeMs: 60_000, exit: 'abort' },
  collecting: { maxAgeMs: 86_400_000, exit: 'abort' },
  selected: { maxAgeMs: 15 * 60_000, exit: 'abort' },
  escalated: { maxAgeMs: 7 * 86_400_000, exit: 'escalate:human' },
  funding: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' },
  awaiting_disclosure: { maxAgeMs: 6 * 3600_000, exit: 'refund' },
  refunding: { maxAgeMs: 15 * 60_000, exit: 'escalate:human' },
  uncertain: { maxAgeMs: 86_400_000, exit: 'escalate:human' },
  // seller
  preparing: { maxAgeMs: 10 * 60_000, exit: 'abort' },
  quoted: { maxAgeMs: 86_400_000, exit: 'abort' },
  completing: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' },
  completion_submitted: { maxAgeMs: 3 * 86_400_000, exit: 'escalate:human' },
  // observer
  watching: { maxAgeMs: 90 * 86_400_000, exit: 'escalate:human' },
  seizing: { maxAgeMs: 30 * 60_000, exit: 'escalate:human' }
}
const TERMINAL = [
  'expired',
  'denied',
  'refunded',
  'useful',
  'not_useful',
  'disputed',
  'canceled',
  'declined',
  'preparation_failed',
  'completion_failed',
  'settled',
  'enabled',
  'funding_failed',
  'seized',
  'seize_failed'
]

interface StoredOffer {
  so: SignedOffer
  commitment?: any
  mode?: string
  reject?: string
}

export class MarketEngine {
  private o: MarketOptions
  private now: () => number
  private me: string
  private chain: Promise<unknown> = Promise.resolve()
  private wallet?: WalletCoordinator
  private inflight = new Set<string>()
  /** per-operation reconcile throttle (engine clock, 30 s) — kept in memory:
   *  persisting it would need an event-writing transition per check, and the
   *  reconcile paths must not spam the event log */
  private reconcileDue = new Map<string, number>()
  /** pushed txids — a hint clears the throttle for the matching operation so a
   *  rejection/observation reconciles on the very next cycle */
  private hintedTxids = new Set<string>()
  public network: string
  get agentPub(): string {
    return this.me
  }

  constructor(private d: MarketDeps) {
    this.o = { ...DEFAULTS, ...d.opts }
    this.now = d.now ?? Date.now
    this.me = d.key.toPublicKey().toString()
    this.network = d.network
    if (d.broadcast) this.wallet = new WalletCoordinator(d.store, { broadcast: d.broadcast })
  }

  get pub(): string {
    return this.me
  }

  private run<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.chain.then(fn, fn)
    this.chain = p.catch(() => {})
    return p
  }

  /** resolves once every unit of work queued so far has settled */
  idle(): Promise<void> {
    return this.chain.then(() => undefined)
  }

  /** a pushed transaction status (broadcaster callback / node event) — only a
   *  hint: the next cycle runs the reconcile check immediately for operations
   *  watching this txid, and the engine still consults txState itself */
  hint(txid: string): void {
    this.hintedTxids.add(txid)
  }

  /** true when this operation's reconcile check may run now — at most once
   *  per 30 s of engine clock unless the watched txid was just hinted */
  private reconcileGate(operationId: string, txid: string | undefined): boolean {
    if (txid !== undefined && this.hintedTxids.delete(txid)) {
      this.reconcileDue.delete(operationId)
      return true
    }
    const next = this.reconcileDue.get(operationId) ?? 0
    if (next > this.now()) return false
    this.reconcileDue.set(operationId, this.now() + 30_000)
    return true
  }

  // ── owner surface ───────────────────────────────────────────────────────
  async enableSeller(policyHash: string): Promise<void> {
    if (this.d.killed?.()) throw new Error('killed: operator stop flag is set — new deals blocked')
    const p = await this.d.store.activePolicy(policyHash)
    if (!p || !p.roles.includes('seller') || p.agentPub !== this.me || p.network !== this.d.network)
      throw new Error('enableSeller: no active seller policy for this agent and network')
    const { operation, created } = await this.d.store.createOperation({
      role: 'seller',
      kind: 'enable',
      dedupeKey: 'seller-enabled',
      policyHash,
      network: this.d.network,
      state: 'enabled',
      data: { policyHash }
    })
    if (!created && operation.data.policyHash !== policyHash)
      await this.d.store.transition(operation.operationId, operation.revision, 'enabled', { cause: 'seller_policy_changed', data: { policyHash } })
  }

  private async sellerPolicy(): Promise<{ hash: string; policy: AutomationPolicy } | undefined> {
    const op = await this.d.store.operationByKey('seller-enabled')
    const hash = op?.data.policyHash as string | undefined
    const policy = hash ? await this.d.store.activePolicy(hash) : undefined
    return hash && policy ? { hash, policy } : undefined
  }

  async watchList(): Promise<{ sellerDomains: string[]; collecting: Array<{ wantId: string }> }> {
    const sellerDomains: string[] = []
    const en = await this.d.store.operationByKey('seller-enabled')
    if (en?.state === 'enabled') {
      const pr = await this.d.store.activePolicy(String(en.data.policyHash))
      if (pr?.roles?.includes('seller') === true) sellerDomains.push(...(pr.domains ?? []))
    }
    const collecting = (await this.d.store.operations({ role: 'buyer', states: ['collecting'] }))
      .map((o) => ({ wantId: (o.data.want as SignedWant | undefined)?.want?.wantId ?? '' }))
      .filter((x) => x.wantId !== '')
    return { sellerDomains, collecting }
  }

  publishWant(
    input: { domains: string[]; question?: string; maxSats: number; wantId?: string },
    policyHash: string
  ): Promise<{ wantId: string; operationId: string }> {
    if (this.d.killed?.()) throw new Error('killed: operator stop flag is set — new deals blocked')
    if (this.d.deaf?.()) throw new Error('deaf: no market hosts reachable — not starting a deal we cannot run')
    return this.run(async () => {
      const p = await this.d.store.activePolicy(policyHash)
      if (!p || !p.roles.includes('buyer') || p.agentPub !== this.me || p.network !== this.d.network)
        throw new Error('publishWant: no active buyer policy for this agent and network')
      const domains = input.domains.map((x) => String(x).toLowerCase())
      if (!domains.length || !domains.every((x) => p.domains.includes(x))) throw new Error('publishWant: domain outside the approved policy')
      if (!Number.isSafeInteger(input.maxSats) || input.maxSats <= 0 || input.maxSats > p.limits.perPurchaseSats)
        throw new Error('publishWant: maxSats outside the approved per-purchase limit')
      const now = this.now()
      const tip = await this.d.tip?.()
      const sw = await signWant(this.d.key, {
        domains,
        ...(input.question ? { question: input.question } : {}),
        maxSats: input.maxSats,
        ts: now,
        exp: now + p.scoring.collectMs,
        net: this.d.network,
        ...(tip ? { tip: tip.tip, tipHeight: tip.tipHeight } : {}),
        ...(input.wantId ? { wantId: input.wantId } : {}),
        ...(this.d.logStream !== undefined ? { journal: { stream: this.d.logStream } } : {})
      })
      const { operation, created } = await this.d.store.createOperation({
        role: 'buyer',
        kind: 'purchase',
        dedupeKey: `buyer:${this.d.network}:${sw.want.wantId}`,
        policyHash,
        network: this.d.network,
        state: 'published',
        data: { want: sw, offers: {} }
      })
      if (created) {
        await this.d.store.transition(operation.operationId, 1, 'collecting', {
          cause: 'want_signed',
          dueAt: now + p.scoring.collectMs,
          outbox: [{ actionId: `${operation.operationId}:want`, kind: 'publish-want', destination: 'feed', payload: sw }]
        })
      }
      return { wantId: (operation.data.want as SignedWant).want.wantId, operationId: operation.operationId }
    })
  }

  status(operationId: string): Promise<{ operation: Operation; events: Awaited<ReturnType<AutomationStore['events']>> } | undefined> {
    return this.run(async () => {
      const operation = await this.d.store.operation(operationId)
      if (!operation) return undefined
      const data = { ...operation.data }
      if (operation.role === 'seller' && data.offer) data.offer = { ...data.offer, wrapped: '[sealed]' }
      return { operation: { ...operation, data }, events: await this.d.store.events(operationId) }
    })
  }

  markUsefulness(operationId: string, useful: boolean, evidence: unknown): Promise<void> {
    return this.run(async () => {
      const op = await this.d.store.operation(operationId)
      if (!op || op.state !== 'disclosure_verified') throw new Error('markUsefulness: operation is not awaiting evaluation')
      await this.d.store.transition(op.operationId, op.revision, useful ? 'useful' : 'not_useful', {
        cause: 'sandbox_evaluation',
        evidence,
        data: { usefulness: { useful, evidence } }
      })
    })
  }

  watchBond(
    d: {
      cfg: { operatorPkh: string; bondReleaseHeight: string | number | bigint; maxFee: string | number | bigint; epochLen: string | number | bigint }
      txid: string
      vout: number
      tx?: string
    },
    policyHash: string
  ): Promise<{ operationId: string; deadline: number }> {
    if (this.d.killed?.()) throw new Error('killed: operator stop flag is set — new watches blocked')
    return this.run(async () => {
      if (!(await this.d.store.activePolicy(policyHash))) throw new Error('watchBond: no active policy')
      // a handed-over bond resolves without touching any indexer; the wire
      // form is verified (SPV) and remembered like every inbound payload
      let tx: Transaction | undefined
      if (typeof d.tx === 'string') {
        try {
          const got = await this.readTx(d.tx)
          if (got.tx.id('hex') === d.txid) {
            tx = got.tx
            this.d.rememberTx?.(tx)
          }
        } catch {
          /* unreadable — fall through to the chain view */
        }
      }
      tx ??= await this.d.fetchTx(d.txid)
      const out = tx?.outputs[d.vout]
      if (!tx || tx.id('hex') !== d.txid || !out) throw new Error('watchBond: bond output is not observable on this chain view')
      const cfg = {
        operatorPkh: d.cfg.operatorPkh,
        bondReleaseHeight: BigInt(d.cfg.bondReleaseHeight),
        maxFee: BigInt(d.cfg.maxFee),
        epochLen: BigInt(d.cfg.epochLen)
      }
      const utxo = { txid: d.txid, vout: d.vout, satoshis: out.satoshis ?? 0, script: out.lockingScript.toHex() }
      const state = new CheckpointBond(cfg).stateOf(utxo.script)
      if (state.seized !== 0n) throw new Error('watchBond: bond is already seized')
      const deadline = Number(state.deadline)
      const { operation } = await this.d.store.createOperation({
        role: 'observer',
        kind: 'bond-watch',
        dedupeKey: `bond:${this.d.network}:${d.txid}:${d.vout}`,
        policyHash,
        network: this.d.network,
        state: 'watching',
        data: {
          cfg: {
            operatorPkh: cfg.operatorPkh,
            bondReleaseHeight: cfg.bondReleaseHeight.toString(),
            maxFee: cfg.maxFee.toString(),
            epochLen: cfg.epochLen.toString()
          },
          utxo,
          n: Number(state.n)
        },
        dueHeight: deadline
      })
      return { operationId: operation.operationId, deadline }
    })
  }

  private async seize(op: Operation): Promise<void> {
    const c = op.data.cfg as Record<string, string>
    const next = await this.d.store.transition(op.operationId, op.revision, 'seizing', { cause: 'deadline_reached' })
    try {
      const r = await this.d.call('bondSeize', {
        payToScript: new P2PKH().lock(this.d.key.toPublicKey().toHash()).toHex(),
        bond: {
          cfg: { operatorPkh: c.operatorPkh, bondReleaseHeight: String(c.bondReleaseHeight), maxFee: String(c.maxFee), epochLen: String(c.epochLen) },
          utxo: op.data.utxo
        }
      })
      await this.d.store.transition(next.operationId, next.revision, 'seized', {
        cause: 'seize_broadcast',
        data: { seizeTxid: r.txid },
        dueHeight: null,
        evidence: { txid: r.txid }
      })
    } catch (e) {
      const msg = (e as Error).message
      await this.d.store.transition(next.operationId, next.revision, /not yet seizable/.test(msg) ? 'watching' : 'seize_failed', {
        cause: 'seize_refused',
        lastError: msg.slice(0, 500)
      })
    }
  }

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
      for (const op of await this.d.store.operations({ notStates: TERMINAL })) {
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
          if (txid === undefined || this.d.txState === undefined || this.now() - op.updatedAt > 60 * 60_000) continue
          if (!this.reconcileGate(op.operationId, txid)) continue
          if ((await this.d.txState(txid)) === 'rejected')
            await this.d.store.transition(op.operationId, op.revision, 'uncertain', {
              cause: 'refund_rejected',
              data: { refundTxid: txid },
              dueHeight: null
            })
        } catch {
          /* status probes must not stall the tick */
        }
      }
      await this.flush()
    })
  }

  private async flush(): Promise<void> {
    for (const item of await this.d.store.dueOutbox()) {
      try {
        await this.deliver(item)
        await this.d.store.ackOutbox(item.actionId)
      } catch (e) {
        const msg = (e as Error).message
        // resource starvation (a funding-tree slot being empty) is not a
        // delivery fault — defer without consuming attempts; unblockOutbox
        // wakes it the moment a topup proof frees a slot
        if (/funding slot/.test(msg)) await this.d.store.deferOutbox(item.actionId, msg)
        else await this.d.store.retryOutbox(item.actionId, msg)
      }
    }
  }

  private async deliver(i: OutboxItem): Promise<void> {
    if (i.kind === 'publish-want') return await this.d.transport.publishWant(i.payload as SignedWant)
    if (i.kind === 'publish-offer') return await this.d.transport.publishOffer(i.payload as SignedOffer)
    if (i.kind === 'message') return await this.d.transport.send(i.destination, i.payload as MarketMessage, i.refs)
    throw new Error(`unknown outbox kind ${i.kind}`)
  }

  // ── seller ──────────────────────────────────────────────────────────────
  private async onWant(sw: SignedWant): Promise<void> {
    // new-deal gates run BEFORE the want is consumed — throwing keeps the
    // inbox item pending so the want is retried when the gate re-opens
    if (this.d.killed?.()) throw new Error('killed: operator stop flag is set')
    if (this.d.deaf?.()) throw new Error('deaf: no market hosts reachable')
    const sp = await this.sellerPolicy()
    if (!sp || !sw?.want || !verifyWant(sw.want, sw.sig, { now: this.now() })) return
    const w = sw.want
    if (w.pub === this.me || w.status !== 'open' || w.net !== this.d.network) return
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
          model: unknown
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
        ...(p.disclosure.allowPublic ? {} : { buyerPub: w.pub })
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

  private async onAccept(from: string, m: Extract<MarketMessage, { type: 'accept' }>): Promise<void> {
    const op = await this.d.store.operation(m.offerId)
    if (!op || op.role !== 'seller' || op.state !== 'quoted') return
    const w = (op.data.want as SignedWant).want
    const offer = op.data.offer as any
    const refuse = async (reason: string) => {
      await this.d.store.transition(op.operationId, op.revision, 'quoted', { cause: 'accept_refused', evidence: { reason } })
    }
    if (from !== w.pub || m.wantId !== w.wantId) return
    if (m.depositorPkh !== pkhOf(from)) return await refuse('depositor is not the buyer key')
    const cfg = { ...offer.cfg, price: BigInt(offer.cfg.price), refundAfter: BigInt(offer.cfg.refundAfter), maxFee: BigInt(offer.cfg.maxFee) }
    const script = DisclosureSaleHost.escrowScript(cfg, m.depositorPkh)
    if (m.utxo?.script !== script) return await refuse('escrow script does not match the quoted terms')
    if (!Number.isSafeInteger(m.utxo.satoshis) || m.utxo.satoshis < Number(offer.cfg.price) + 1) return await refuse('escrow is underfunded')
    // the buyer hands over the escrow itself. It is checked here, against our
    // own headers where its ancestry is proven, and then put on the network
    // by us — no indexer has to have seen it first.
    let tx: Transaction | undefined
    let spv = false
    if (typeof m.tx === 'string') {
      try {
        const got = await this.readTx(m.tx)
        if (got.tx.id('hex') !== m.utxo.txid) return await refuse('attached transaction is not the claimed escrow')
        tx = got.tx
        spv = got.spv
      } catch {
        return await refuse('attached escrow transaction is unreadable')
      }
    } else tx = await this.d.fetchTx(m.utxo.txid)
    const out = tx?.outputs[m.utxo.vout]
    if (!tx || tx.id('hex') !== m.utxo.txid || !out) return await refuse('escrow transaction not observable yet')
    if (out.lockingScript.toHex() !== script || out.satoshis !== m.utxo.satoshis) return await refuse('on-chain escrow output differs from the claimed one')
    // every escrow input whose parent we can resolve is evaluated locally —
    // a bogus or unsigned spend is refused before the network is even asked
    let spends = 0
    for (let ix = 0; ix < tx.inputs.length; ix++) {
      const i = tx.inputs[ix]
      const src = i.sourceTransaction?.outputs[i.sourceOutputIndex ?? -1]
      const ptxid = i.sourceTXID ?? i.sourceTransaction?.id('hex')
      if (!src || i.sourceOutputIndex === undefined || ptxid === undefined) continue
      if (!spendValid(tx, ix, { txid: ptxid, vout: i.sourceOutputIndex, satoshis: src.satoshis ?? 0, script: src.lockingScript.toHex() }))
        return await refuse('escrow input does not spend its claimed parent output')
      spends++
    }
    if (typeof m.tx === 'string' && this.d.broadcast !== undefined) {
      const br = await this.d.broadcast(tx)
      if (br.status !== 'success') return await refuse(`escrow not accepted by the network: ${br.description ?? br.status}`)
    }
    this.d.rememberTx?.(tx)
    const h = await this.d.height()
    if (h + this.o.sellerSafetyBlocks >= Number(offer.cfg.refundAfter)) return await refuse('too close to the refund deadline')
    const next = await this.d.store.transition(op.operationId, op.revision, 'completing', {
      cause: 'escrow_verified',
      data: { escrow: { cfg: { ...offer.cfg, depositorPkh: m.depositorPkh }, utxo: m.utxo } },
      evidence: { spv, spends }
    })
    await this.complete(next)
  }

  private async complete(op: Operation): Promise<void> {
    const e = op.data.escrow as any
    const offer = op.data.offer as any
    const w = (op.data.want as SignedWant).want
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
    if (q.sellerPub !== o.pub || Number(q.cfg.price) !== o.priceSats) return 'quote does not match the listing'
    if (q.cfg.sellerPkh !== pkhOf(o.pub)) return 'payee is not the seller key'
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

  private async onOffer(so: SignedOffer): Promise<void> {
    if (!so?.offer?.wantId || !verifyOffer(so.offer, so.sig, { now: this.now() })) return
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${so.offer.wantId}`)
    if (!op || op.state !== 'collecting') return
    const offers = { ...(op.data.offers as Record<string, StoredOffer>) }
    const key = `${so.offer.pub}:${so.offer.offerId}`
    const prev = offers[key]
    if (prev && prev.so.offer.revision >= so.offer.revision) return
    if (!prev && Object.keys(offers).length >= 64) return
    const reject = this.offerReject(so, (op.data.want as SignedWant).want)
    offers[key] = { so, ...(reject ? { reject } : {}), ...(prev?.commitment ? { commitment: prev.commitment, mode: prev.mode } : {}) }
    await this.d.store.transition(op.operationId, op.revision, 'collecting', {
      cause: reject ? 'offer_rejected' : 'offer_recorded',
      data: { offers },
      evidence: { offerKey: key, ...(reject ? { reject } : {}) }
    })
  }

  private async onQuote(from: string, m: Extract<MarketMessage, { type: 'quote' }>): Promise<void> {
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${m.wantId}`)
    if (!op || op.state !== 'collecting') return
    const offers = { ...(op.data.offers as Record<string, StoredOffer>) }
    const key = `${from}:${m.offerId}`
    const cur = offers[key]
    if (!cur) throw new Error('quote arrived before its offer')
    if (!m.commitment || m.commitment.leaf !== cur.so.offer.leaf) return
    offers[key] = { ...cur, commitment: m.commitment, mode: m.mode }
    await this.d.store.transition(op.operationId, op.revision, 'collecting', { cause: 'quote_attached', data: { offers } })
  }

  private async onDelivered(from: string, m: Extract<MarketMessage, { type: 'delivered' }>, refs?: import('@nektar/bstack').BoxRef[]): Promise<void> {
    const op = await this.d.store.operationByKey(`buyer:${this.d.network}:${m.wantId}`)
    if (!op || op.state !== 'awaiting_disclosure') return
    const sel = op.data.selected as { key: string }
    const chosen = (op.data.offers as Record<string, StoredOffer>)[sel.key]
    if (from !== chosen.so.offer.pub || m.offerId !== chosen.so.offer.offerId) return
    const utxo = op.data.utxo as { txid: string; vout: number; satoshis: number; script: string }
    let got: Ingested
    try {
      got = await this.readTx(m.tx)
    } catch {
      return
    }
    const tx = got.tx
    // the reveal must be a valid spend of OUR escrow output — checked by
    // running the script here, so the disclosure is accepted the moment it
    // arrives instead of when an indexer shows it
    if (tx.id('hex') !== m.txid || !spendValid(tx, 0, utxo)) return
    this.d.rememberTx?.(tx)
    const q = chosen.so.offer.offer as any
    const claim = await this.d.call('sale#claim', {
      txHex: tx.toHex(),
      hWrapped: q.cfg.hWrapped,
      sellerPub: from,
      offerMode: chosen.mode,
      ...(chosen.commitment ? { commitment: chosen.commitment } : {})
    })
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
    const bad = chosen.commitment ? claim.verified !== true : false
    await this.d.store.transition(op.operationId, op.revision, bad ? 'disputed' : 'disclosure_verified', {
      cause: bad ? 'payload_does_not_match_commitment' : 'disclosure_verified',
      data: { result },
      dueHeight: null,
      evidence: {
        txid: m.txid,
        verified: claim.verified,
        authorVerified: claim.authorVerified,
        reason: claim.reason,
        spv: got.spv,
        ...(refs !== undefined ? { refs } : {})
      }
    })
  }

  private async readTx(hex: string): Promise<Ingested> {
    return this.d.readTx !== undefined ? await this.d.readTx(hex) : await TxStore.read(hex, undefined)
  }
  /** what the seller is handed for an escrow this instance funded */
  private escrowWire(txid: string, rawHex?: string): { tx: string } | {} {
    const w = this.d.txWire?.(txid) ?? rawHex
    return w !== undefined ? { tx: w } : {}
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
        restored: true
      }
    })
    if (!created) return
    await this.d.store.transition(operation.operationId, operation.revision, 'awaiting_disclosure', {
      cause: 'restored_from_note',
      dueHeight: Number(n.escrow.refundAfter)
    })
  }

  /** the seller's own recovery note — rebuilds the quoted sale with its
   *  original operationId (the buyer's accept addresses it by offerId). Only
   *  our own key may feed it; a foreign or unsigned note is ignored. */
  private async onSellerNote(n: Extract<MarketMessage, { type: 'sellerNote' }>): Promise<void> {
    const sw = n?.want
    if (!sw?.want || !verifyWant(sw.want, sw.sig, { allowExpired: true })) return
    if (sw.want.net !== this.d.network || sw.want.wantId !== n.wantId) return
    const leaf = n.offer?.commitment?.leaf ?? n.signedOffer?.offer.leaf ?? ''
    if (n.signedOffer !== undefined) {
      const so = n.signedOffer
      if (!so?.offer || !verifyOffer(so.offer, so.sig, { allowExpired: true })) return
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

  private async onMessage(from: string, m: MarketMessage, refs?: BoxRef[]): Promise<void> {
    if (m?.type === 'quote') return await this.onQuote(from, m)
    if (m?.type === 'accept') return await this.onAccept(from, m)
    if (m?.type === 'delivered') return await this.onDelivered(from, m, refs)
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

  private async select(op: Operation, height: number): Promise<void> {
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
    const ctx: ActionContext = { operationId: op.operationId, actionId: `${op.operationId}:escrow`, policyHash: op.policyHash, networkId: this.d.network }
    const b = await this.d.store.reserveBudget(ctx, amounts, approved)
    if (!b.ok) {
      if (CAP.test(b.code)) return await this.escalate(op, b.code, actionDigest, amounts.purchaseSats + amounts.feeSats, { offerKey: sel.key, amounts })
      await this.d.store.transition(op.operationId, op.revision, 'denied', { cause: b.code })
      return
    }
    const funding = await this.d.store.transition(op.operationId, op.revision, 'funding', {
      cause: approved ? 'human_approved' : 'policy_allowed',
      data: { escrowAction: ctx.actionId, amounts }
    })
    this.inflight.add(op.operationId)
    try {
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
          await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
            cause: 'escrow_build_refused',
            lastError: (e as Error).message.slice(0, 500)
          })
          return
        }
        const tx = (built.tx as Transaction | undefined) ?? Transaction.fromHex(built.txHex)
        escrowTx = tx
        for (const inp of tx.inputs) {
          if (!inp.sourceTransaction && inp.sourceTXID) inp.sourceTransaction = (await this.d.fetchTx(inp.sourceTXID)) as Transaction
        }
        await this.wallet.prepare(ctx, 'escrow', tx)
        const st = await this.d.store.transition(funding.operationId, funding.revision, 'funding', {
          cause: 'escrow_prepared',
          data: { pendingEscrow: { cfg: built.escrow, utxo: built.utxo, feeSats: built.feeSats } }
        })
        await this.d.afterPrepare?.(ctx.actionId)
        const sub = await this.wallet.submit(ctx.actionId)
        if (sub.state !== 'submitted' && sub.state !== 'observed' && sub.state !== 'confirmed') {
          if (sub.state === 'rejected' && /SpendGuard refused/i.test(sub.detail ?? '')) {
            await this.d.store.releaseBudget(ctx.actionId)
            await this.d.store.transition(st.operationId, st.revision, 'funding_failed', {
              cause: 'spend_guard_refused',
              lastError: (sub.detail ?? '').slice(0, 500)
            })
          } else
            await this.escalate(
              st,
              'unresolved_payment',
              digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'resolve-escrow' }),
              0,
              { reason: sub.detail ?? sub.state },
              'uncertain'
            )
          return
        }
        this.d.settleEscrow(tx)
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
          await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
            cause: 'escrow_refused_before_broadcast',
            lastError: (e as Error).message.slice(0, 500)
          })
          return
        }
      if (r.accepted !== true) {
        if (/SpendGuard refused/i.test(String(r.reason))) {
          await this.d.store.releaseBudget(ctx.actionId)
          await this.d.store.transition(funding.operationId, funding.revision, 'funding_failed', {
            cause: 'spend_guard_refused',
            lastError: String(r.reason).slice(0, 500)
          })
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
      if (actionId !== undefined) await this.d.store.releaseBudget(actionId)
      await cancel()
      await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_never_built' })
      return
    }
    if (this.d.txState === undefined || !this.reconcileGate(op.operationId, txid)) return
    const st = await this.d.txState(txid)
    if (st === 'unknown') {
      if (op.data.reconcilePing !== true)
        await this.d.store.transition(op.operationId, op.revision, 'uncertain', { cause: 'reconcile_pending', data: { reconcilePing: true } })
      return
    }
    if (st === 'rejected') {
      if (actionId !== undefined) await this.d.store.releaseBudget(actionId)
      await cancel()
      await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_rejected' })
      return
    }
    // mined | seen — the escrow is observable: commit and resume the normal path
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
      const script = DisclosureSaleHost.escrowScript(
        { ...q.cfg, price: BigInt(q.cfg.price), refundAfter: BigInt(q.cfg.refundAfter), maxFee: BigInt(q.cfg.maxFee) },
        depositorPkh
      )
      const tx = Transaction.fromHex(wtx.rawHex)
      const vout = tx.outputs.findIndex((o) => o.lockingScript.toHex() === script)
      if (vout < 0) return // cannot prove which output is the escrow — stay uncertain
      escrow = { ...q.cfg, depositorPkh }
      utxo = { txid, vout, satoshis: tx.outputs[vout].satoshis ?? 0, script }
    }
    if (escrow === undefined || utxo === undefined) return
    if (wtx !== undefined) this.d.settleEscrow?.(Transaction.fromHex(wtx.rawHex))
    if (actionId !== undefined) await this.d.store.commitBudget(actionId)
    await cancel()
    await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
      cause: 'reconciled_escrow_observed',
      data: { escrow, utxo, escrowTxid: txid },
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
   *  the operation stays `uncertain` for the operator */
  private async reconcileRefund(op: Operation, pa: { approvalId?: string }): Promise<void> {
    const tries = Number(op.data.refundTries ?? 0)
    if (tries >= 5) return
    if (!this.reconcileGate(op.operationId, op.data.escrowTxid as string | undefined)) return
    const r = await this.d.call('sale#refund', { cfg: op.data.escrow, utxo: op.data.utxo })
    if (r.accepted === true) {
      await this.d.store.refundBudget(op.data.escrowAction as string)
      if (pa.approvalId !== undefined) await this.d.store.cancelApproval(pa.approvalId)
      await this.d.store.transition(op.operationId, op.revision, 'refunded', {
        cause: 'refund_broadcast',
        data: { refundTxid: r.txid, refundSats: r.refundSats },
        dueHeight: null
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
    await this.escalate(
      op,
      'stuck_state_timeout',
      digestOf('nektar:automation:action:1', { op: op.operationId, kind: 'stuck-exit', state: op.state }),
      0,
      { state: op.state, ageMs: this.now() - op.updatedAt },
      'uncertain'
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
    for (const op of await this.d.store.operations({ notStates: TERMINAL })) {
      const lim = STATE_LIMITS[op.state]
      if (lim === undefined) violations.push({ operationId: op.operationId, state: op.state, ageMs: this.now() - op.updatedAt })
      else if (lim.exit !== 'escalate:human' && this.now() - op.updatedAt > lim.maxAgeMs)
        violations.push({ operationId: op.operationId, state: op.state, ageMs: this.now() - op.updatedAt })
    }
    return { limits: { ...STATE_LIMITS }, violations }
  }

  private async advance(op: Operation, height: number): Promise<void> {
    const lim = STATE_LIMITS[op.state]
    if (lim !== undefined && this.now() - op.updatedAt > lim.maxAgeMs) return await this.stuckExit(op, lim)
    if (op.role === 'buyer') {
      if (op.state === 'collecting' && op.dueAt !== undefined && op.dueAt <= this.now()) return await this.select(op, height)
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
          const sub = await this.wallet.submit(op.data.escrowAction as string)
          if (sub.state === 'submitted' || sub.state === 'observed' || sub.state === 'confirmed') {
            const wtx = await this.d.store.walletTx({ actionId: op.data.escrowAction as string })
            if (wtx) this.d.settleEscrow(Transaction.fromHex(wtx.rawHex))
            await this.d.store.commitBudget(op.data.escrowAction as string)
            const sel2 = op.data.selected as { key: string }
            const s2 = (op.data.offers as Record<string, StoredOffer>)[sel2.key]
            const so2 = s2.so
            const w2 = (op.data.want as SignedWant).want
            await this.d.store.transition(op.operationId, op.revision, 'awaiting_disclosure', {
              cause: 'escrow_resumed_same_bytes',
              data: { escrow: pending.cfg, utxo: pending.utxo, escrowTxid: pending.utxo.txid },
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
      if (op.state === 'awaiting_disclosure') {
        // a late rejection beats the refund path: the escrow never settled, so
        // the locked amount never left — releaseBudget frees the committed row
        const txid = op.data.escrowTxid as string | undefined
        if (txid !== undefined && this.d.txState !== undefined && this.reconcileGate(op.operationId, txid) && (await this.d.txState(txid)) === 'rejected') {
          await this.d.store.releaseBudget(op.data.escrowAction as string)
          await this.d.store.transition(op.operationId, op.revision, 'funding_failed', { cause: 'escrow_rejected_late', dueHeight: null })
          return
        }
        if (op.dueHeight !== undefined && height >= op.dueHeight) return await this.refund(op)
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
        if (tx?.merklePath)
          await this.d.store.transition(op.operationId, op.revision, 'settled', {
            cause: 'disclosure_proof_observed',
            evidence: { blockHeight: tx.merklePath.blockHeight }
          })
        else if (
          this.d.txState !== undefined &&
          this.reconcileGate(op.operationId, op.data.completeTxid as string) &&
          (await this.d.txState(op.data.completeTxid as string)) === 'rejected'
        ) {
          await this.d.store.transition(op.operationId, op.revision, 'completion_failed', { cause: 'disclosure_rejected_late' })
        }
      }
    }
  }
}
