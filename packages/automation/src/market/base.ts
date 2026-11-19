import { P2PKH, type Transaction } from '@bsv/sdk'
import { signWant, type SignedWant } from '@nektar/bstack'
import { CheckpointBond } from '@nektar/anchor'
import { TxStore, type Ingested } from '@nektar/chain'
import type { AutomationStore, Operation } from '../store.js'
import { WalletCoordinator } from '../wallet.js'
import type { AutomationPolicy } from '../policy.js'
import { DEFAULTS } from './types.js'
import type { MarketDeps, MarketOptions } from './types.js'

export abstract class MarketBase {
  protected o: MarketOptions
  protected now: () => number
  protected me: string
  protected chain: Promise<unknown> = Promise.resolve()
  protected wallet?: WalletCoordinator
  protected inflight = new Set<string>()
  /** per-operation reconcile throttle (engine clock, 30 s) — kept in memory:
   *  persisting it would need an event-writing transition per check, and the
   *  reconcile paths must not spam the event log */
  protected reconcileDue = new Map<string, number>()
  /** pushed txids — a hint clears the throttle for the matching operation so a
   *  rejection/observation reconciles on the very next cycle */
  protected hintedTxids = new Set<string>()
  public network: string
  get agentPub(): string {
    return this.me
  }

  constructor(protected d: MarketDeps) {
    this.o = { ...DEFAULTS, ...d.opts }
    // contradictory configuration is a loud refusal, not a quiet deal that
    // can never settle: a refund unlocking inside the seller's safety
    // margin means the counterparty can pull the escrow back while the
    // seller still treats the settlement as unconfirmed.
    if (this.o.refundBlocks <= this.o.sellerSafetyBlocks)
      throw new Error(
        `market: refundBlocks (${this.o.refundBlocks}) must exceed sellerSafetyBlocks (${this.o.sellerSafetyBlocks}) — the refund window sits inside the seller's safety margin`
      )
    this.now = d.now ?? Date.now
    this.me = d.key.toPublicKey().toString()
    this.network = d.network
    if (d.broadcast)
      this.wallet = new WalletCoordinator(d.store, { broadcast: d.broadcast, txDied: d.txDied, txSpent: d.settleEscrow, recordAired: d.recordAired })
  }

  get pub(): string {
    return this.me
  }

  protected run<T>(fn: () => Promise<T>): Promise<T> {
    const p = this.chain.then(fn, fn)
    this.chain = p.catch(() => {})
    return p
  }

  /** resolves once every unit of work queued so far has settled */
  idle(): Promise<void> {
    return this.chain.then(() => undefined)
  }

  /** boot replay: rows a crashed process left 'prepared' or
   *  'submission_unknown' re-submit from their stored bytes — the ledger row
   *  is the intent and 'already known' answers settle rows that actually
   *  made the wire. Queued behind in-flight work, never racing a live deal */
  resubmitPending(): Promise<Array<{ actionId: string; state: import('../store.js').TxState }>> {
    return this.run(async () => (await this.wallet?.resubmitPending()) ?? [])
  }

  /** a pushed transaction status (broadcaster callback / node event) — only a
   *  hint: the next cycle runs the reconcile check immediately for operations
   *  watching this txid, and the engine still consults txFacts itself */
  hint(txid: string): void {
    // foreign txids are never consumed by reconcileGate — cap the set so an
    // unbounded hint stream can't grow it forever (oldest evicted first)
    if (this.hintedTxids.size >= 4096) {
      const oldest = this.hintedTxids.values().next().value
      if (oldest !== undefined) this.hintedTxids.delete(oldest)
    }
    this.hintedTxids.add(txid)
  }

  /** true when this operation's reconcile check may run now — at most once
   *  per 30 s of engine clock unless the watched txid was just hinted */
  protected reconcileGate(operationId: string, txid: string | undefined): boolean {
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

  protected async sellerPolicy(): Promise<{ hash: string; policy: AutomationPolicy } | undefined> {
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
        // the want lives past one collection window — sellers under load may
        // still answer while the buyer republishes at each boundary
        exp: now + p.scoring.collectMs + this.o.wantExtendMs,
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
          data: { collectingSince: now },
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

  protected async seize(op: Operation): Promise<void> {
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

  protected async readTx(hex: string): Promise<Ingested> {
    return this.d.readTx !== undefined ? await this.d.readTx(hex) : await TxStore.read(hex, undefined)
  }
}
