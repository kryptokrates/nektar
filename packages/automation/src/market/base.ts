import { P2PKH, PrivateKey, PublicKey, type Transaction } from '@bsv/sdk'
import { signWant } from '@nektar/bstack'
import { CheckpointBond } from '@nektar/anchor'
import { TxStore, isHeaderUnavailable, type Ingested } from '@nektar/chain'
import { sparse, stable01 } from '@nektar/core'
import type { AutomationStore, Operation } from '../store.js'
import { WalletCoordinator } from '../wallet.js'
import { txFromRow } from '../ledger.js'
import type { TxEffects } from '../machines/effects.js'
import type { AutomationPolicy } from '../policy.js'
import { DEFAULTS, STATE_LIMITS } from './types.js'
import { WAITS } from '../waits.js'
import { executor } from '../rt/exec.js'
import { custodyResubmitPending, custodySubmit } from '../rt/emit.js'
import type { MarketDeps, MarketOptions } from './types.js'

export abstract class MarketBase {
  protected o: MarketOptions
  protected now: () => number
  protected me: string
  protected chain: Promise<unknown> = Promise.resolve()
  protected wallet?: WalletCoordinator
  /** the engine's custody effect set — coordinator, emits, and any inline
   *  machine write all run these same handlers */
  protected readonly txFx: TxEffects
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
    this.me =
      d.me ??
      (d.key instanceof PrivateKey
        ? d.key.toPublicKey().toString()
        : (() => {
            throw new Error('market: deps.me required when deps.key is a DigestSigner — resolve pubKeyHex() before construction')
          })())
    this.network = d.network
    // the engine owns the state bounds; the store stamps a bounded state's
    // stuck deadline at commit so the wheel wakes at its instant (the
    // advance() scan stays the missed-arm floor)
    d.store.residenceBound = (s) => STATE_LIMITS[s]?.maxAgeMs
    // one effect set for the whole engine — the host's custodyTxEffects when
    // it passes one, else the legacy triple mapped through txFromRow so
    // engine-only embeddings and tests keep working
    this.txFx = d.txEffects ?? {
      txSpent: async (w) => {
        await d.settleEscrow?.(txFromRow(w))
      },
      txDied: async (w) => {
        await d.txDied?.(txFromRow(w))
      },
      recordAired: async (w) => {
        await d.recordAired?.(txFromRow(w))
      },
      exportExpired: async () => {}
    }
    if (d.broadcast)
      this.wallet = new WalletCoordinator(d.store, {
        broadcast: d.broadcast,
        exec: d.executor,
        fx: this.txFx
      })
  }

  get pub(): string {
    return this.me
  }

  // the serialized-work mutex: every run() queues behind the previous
  // link on purpose — custody ordering over one coin book is deliberate,
  // so the queue must not drop a wedged link. But a hung link wedges
  // EVERY later run() silently — a bound() frees the caller's wait, never
  // the chain, and the orphan keeps the link forever. Detect instead:
  // when the head link outlives the hold bound, name it once per link —
  // a wedge in the log beats a silent one.
  private headSince = 0
  private headName = 'run'
  private headWarned = false
  private static readonly HOLD_MS = WAITS.runHold.ms
  protected run<T>(fn: () => Promise<T>, name = 'run'): Promise<T> {
    const held = this.headSince === 0 ? 0 : this.now() - this.headSince
    if (held > MarketBase.HOLD_MS && !this.headWarned) {
      this.headWarned = true
      this.d.log?.(`market: run() chain held ${held}ms by '${this.headName}' — a queued link never settled; every later run() waits behind it`)
    }
    const mark = (): void => {
      this.headSince = this.now()
      this.headName = name
      this.headWarned = false
    }
    const p = this.chain.then(mark, mark).then(fn, fn)
    this.chain = p.catch(() => {})
    // headSince belongs to the running link — clear on settle, strictly
    // before the next link's mark can set its own. then(both), not
    // .finally: a void'd .finally-derived promise inherits p's rejection
    // and lands as an unhandledRejection even when the caller handled p
    const clear = (): void => {
      this.headSince = 0
    }
    void p.then(clear, clear)
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
    return this.run(async () => (this.wallet !== undefined ? await custodyResubmitPending(this.d.executor ?? executor(), this.wallet) : []), 'resubmit')
  }

  /** a single row's broadcast+verdict leg — routed to the custody object
   *  under restate (serialized + journaled there), the coordinator direct
   *  locally. No run() chain: callers inside invocations can't re-enter it */
  submitTx(actionId: string): Promise<{ state: import('../store.js').TxState; txid: string; detail?: string } | undefined> {
    if (this.wallet === undefined) return Promise.resolve(undefined)
    return custodySubmit(this.d.executor ?? executor(), this.wallet, actionId)
  }

  /** the custody object's deps — the coordinator the VO serializes. */
  get walletCoordinator(): WalletCoordinator | undefined {
    return this.wallet
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
   *  per 30 s of engine clock unless the watched txid was just hinted.
   *  `nextReconcileAt` is the op's persisted cooldown (a restart no longer
   *  resets probe rate); the in-memory map covers the race where this tick
   *  re-checks before the fire-and-forget column write lands. */
  protected reconcileGate(operationId: string, txid: string | undefined, nextReconcileAt?: number): boolean {
    if (txid !== undefined && this.hintedTxids.delete(txid)) {
      this.reconcileDue.delete(operationId)
      void this.d.store.markReconciled(operationId, 0).catch(() => undefined)
      return true
    }
    const next = Math.max(nextReconcileAt ?? 0, this.reconcileDue.get(operationId) ?? 0)
    if (next > this.now()) return false
    // deterministic per-op spread on top of the cadence — the 30s floor is
    // the rate LIMIT (never probed faster), and stable01 desynchronizes the
    // agents whose probes would otherwise land on the same ticks
    const until = this.now() + WAITS.reconcileGate.ms + Math.floor(stable01(operationId) * WAITS.reconcileJitter.ms)
    this.reconcileDue.set(operationId, until)
    void this.d.store.markReconciled(operationId, until).catch(() => undefined)
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
    const hash = op?.data.policyHash
    const policy = hash ? await this.d.store.activePolicy(hash) : undefined
    return hash && policy ? { hash, policy } : undefined
  }

  async watchList(): Promise<{ sellerDomains: string[]; collecting: Array<{ wantId: string }> }> {
    const sellerDomains: string[] = []
    const en = await this.d.store.operationByKey('seller-enabled')
    if (en?.state === 'enabled') {
      const pr = await this.d.store.activePolicy(String(en.data.policyHash))
      if (sparse(pr?.roles)?.includes('seller') === true) sellerDomains.push(...(sparse(pr?.domains) ?? []))
    }
    const collecting = (await this.d.store.operations({ role: 'buyer', states: ['collecting'] }))
      .map((o) => ({ wantId: sparse(o.data.want?.want)?.wantId ?? '' }))
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
      return { wantId: operation.data.want!.want.wantId, operationId: operation.operationId }
    }, 'publishWant')
  }

  status(operationId: string): Promise<{ operation: Operation; events: Awaited<ReturnType<AutomationStore['events']>> } | undefined> {
    return this.run(async () => {
      const operation = await this.d.store.operation(operationId)
      if (!operation) return undefined
      const data = { ...operation.data }
      if (operation.role === 'seller' && data.offer) data.offer = { ...data.offer, wrapped: '[sealed]' }
      return { operation: { ...operation, data }, events: await this.d.store.events(operationId) }
    }, 'status')
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
    }, 'markUsefulness')
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
        } catch (e) {
          // a header outage is not "unreadable" — it reaches the retry
          // boundary instead of downgrading to the unverified chain view
          if (isHeaderUnavailable(e)) throw e
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
    }, 'watchBond')
  }

  protected async seize(op: Operation): Promise<void> {
    const c = op.data.cfg!
    const next = await this.d.store.transition(op.operationId, op.revision, 'seizing', { cause: 'deadline_reached' })
    try {
      const r = await this.d.call('bondSeize', {
        payToScript: new P2PKH().lock(PublicKey.fromString(this.me).toHash()).toHex(),
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
