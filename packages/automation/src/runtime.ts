import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import { createHash } from 'node:crypto'
import { Transaction, MerklePath } from '@bsv/sdk'
import { hintSeen } from '@nektar/chain'
import type { Knex } from 'knex'
import type { SignedWant, SignedOffer } from '@nektar/bstack'
import type { AutomationHost } from './host.js'
import { AutomationStore, type OutboxItem, type WalletTx } from './store.js'
import { TransactionWatcher, scanTxObservations } from './ledger.js'
import { MarketEngine, NONTERMINAL, type MarketTransport } from './market.js'
import { ScriptedExecutor, HttpChatExecutor, type AIExecutor } from './ai.js'
import { objectStoreFromEnv } from '@nektar/store'
import { deadline, nap, nektarEnv, signerPubHex, toSigner } from '@nektar/core'
import { WAITS } from './waits.js'
import { runPreflight, WATCHDOG_CHECKS, type PreflightReport } from './preflight.js'
import { killSwitchActive } from './kill.js'
import { abandonedReap, queueRepairs, trackFanout } from './fanout.js'
import { openLanes } from './lanes.js'
import { openOutbox } from './outbox.js'
import { openUpkeep } from './upkeep.js'
import { openWatches } from './watches.js'
import type { TxEffects } from './machines/effects.js'
import { executor, EXEC_SVC } from './rt/exec.js'
import { openEndpoint, pausedInvocations, type RtEndpoint } from './rt/endpoint.js'
import { isLocalDispatch, wireRuntime } from './rt/dispatch.js'
import {
  txObject,
  sweepService,
  dealObject,
  custodyObject,
  dealfanObject,
  transportObject,
  wakeService,
  cronObject,
  notifyObject,
  dealOpKey,
  opWantId,
  RT_OPTS_HEAVY,
  type WakeLegs
} from './rt/services.js'
import type { Rt, LaneInfra, AutomationConfig } from './rtctx.js'
import type { Automation } from './types.js'

export type { Automation } from './types.js'
export type { AutomationConfig } from './rtctx.js'
export { requeueAckItems, queueRepairs, trackFanout, abandonedReap } from './fanout.js'

function aiFromEnv(env = process.env): AIExecutor {
  const baseUrl = env.NEKTAR_AI_BASE_URL
  if (!baseUrl) return new ScriptedExecutor()
  if (!env.NEKTAR_AI_MODEL) throw new Error('NEKTAR_AI_BASE_URL is set but NEKTAR_AI_MODEL is not')
  const keyFile = env.NEKTAR_AI_API_KEY_FILE
  const apiKey = env.NEKTAR_AI_API_KEY ?? (keyFile && existsSync(keyFile) ? readFileSync(keyFile, 'utf8').trim() : undefined)
  const local = /^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/|$)/.test(baseUrl)
  const egress = env.NEKTAR_AI_EGRESS === 'local' || env.NEKTAR_AI_EGRESS === 'external' ? env.NEKTAR_AI_EGRESS : local ? 'local' : 'external'
  return new HttpChatExecutor({ provider: env.NEKTAR_AI_PROVIDER ?? 'http', baseUrl, modelId: env.NEKTAR_AI_MODEL, apiKey, egress })
}

// the assembler: store/wallet/preflight/lane-infra construction, the
// MarketEngine + transport wiring, the deal loop (deals/cycle/tick/watchdog),
// the push hooks, and the Automation surface. The four regions —
// openLanes/openOutbox/openUpkeep/openWatches — own their own machinery and
// meet through the Rt context; every cross-region call resolves at call time
// exactly as the old closure's lets did.
export async function openAutomation(mem: AutomationHost, cfg: AutomationConfig): Promise<Automation> {
  const knexFactory = (await import('knex')).default
  if (cfg.dataDir) mkdirSync(cfg.dataDir, { recursive: true })
  const kx: Knex = knexFactory({
    client: 'sqlite3',
    connection: { filename: cfg.dataDir ? join(cfg.dataDir, 'automation.sqlite') : ':memory:' },
    useNullAsDefault: true
  })
  // same contract records.sqlite gets in rtsupport — WAL so readers (outbox
  // pump, watchers, HTTP) don't queue behind the writer; busy_timeout so a
  // contended write retries instead of erroring. :memory: ignores both.
  await kx.raw('PRAGMA journal_mode=WAL')
  await kx.raw('PRAGMA busy_timeout=5000')
  // one clock domain: next_at/dueAt stamps and every engine-side due check
  // compare against cfg.now — mixing the real clock into row stamps while the
  // engine reads a virtual one makes fresh rows look not-yet-due forever
  const store = new AutomationStore(kx, cfg.now)
  await store.init()
  const key = mem.operatorSigner?.() ?? toSigner(mem.operatorKey())
  const me = await signerPubHex(key)
  const wallet = mem.operatorWallet()
  const objectStore = cfg.objectStore ?? objectStoreFromEnv()
  const marketHosts = (cfg.marketHosts ?? []).map((u) => u.replace(/\/+$/, ''))
  const log = cfg.log ?? (() => {})
  // A9 — the readiness gate. Preflight runs BEFORE anything can spend:
  // strict refuses to open on a red check; non-strict opens anyway and seeds
  // the host-health set the lane readers maintain.
  const pfCfg = { network: cfg.network, dataDir: cfg.dataDir, marketHosts, boxOffice: cfg.boxOffice, unattended: cfg.unattended, now: cfg.now }
  const runChecks = (only?: string[]) =>
    runPreflight({
      mem,
      store,
      objectStore,
      cfg: pfCfg,
      probes: { broadcaster: cfg.broadcasterProbe, spend: cfg.spendProbe },
      ...(only !== undefined ? { only } : {})
    })
  let lastReport: PreflightReport | undefined = await runChecks()
  if (cfg.strict === true && !lastReport.pass) {
    const failed = lastReport.checks
      .filter((c) => !c.pass)
      .map((c) => `${c.check}: ${c.detail}`)
      .join(' | ')
    await kx.destroy()
    throw new Error(`automation preflight failed (strict) — ${failed}`)
  }
  if (!lastReport.pass)
    log(
      `automation: preflight degraded — ${lastReport.checks
        .filter((c) => !c.pass)
        .map((c) => c.check)
        .join(', ')}`
    )
  const downNow = new Set<string>(lastReport.downHosts)
  /** deaf = every configured host unreachable — new deals pause, in-flight
   *  work (refunds, settlements, outbox retries) keeps running */
  const deafNow = (): boolean => marketHosts.length > 0 && marketHosts.every((h) => downNow.has(h))
  // lane infrastructure (funding trees, carrier journal, finger chain, log
  // anchors) is NOT recovery-note state — an op-db loss must not take carrier
  // funding with it: the recovered seller still has to emit the delivered
  // envelope. Lanes live in their own store; a pre-split deployment's lane
  // tables sit inside automation.sqlite, so seed lanes.sqlite with a copy.
  let kxLanes: Knex | undefined
  const lanes: LaneInfra | undefined =
    marketHosts.length === 0
      ? undefined
      : await (async () => {
          const mod = await import('@nektar/bstack')
          const lp = cfg.dataDir ? join(cfg.dataDir, 'lanes.sqlite') : ':memory:'
          kxLanes = knexFactory({ client: 'sqlite3', connection: { filename: lp }, useNullAsDefault: true })
          await kxLanes.raw('PRAGMA journal_mode=WAL')
          await kxLanes.raw('PRAGMA busy_timeout=5000')
          // seed a fresh lanes.sqlite by copying lane-table ROWS — a file copy
          // would drag every automation table in and miss WAL commits. The
          // call is unconditional: the sentinel inside migrateLaneTables
          // decides (a mid-copy crash leaves the file behind; file existence
          // is NOT the end state)
          if (cfg.dataDir && existsSync(join(cfg.dataDir, 'automation.sqlite'))) await mod.migrateLaneTables(join(cfg.dataDir, 'automation.sqlite'), kxLanes)
          return {
            mod,
            ledger: new mod.KnexTreeLedger(kxLanes),
            journal: new mod.KnexCarrierJournal(kxLanes),
            logState: new mod.KnexLogState(kxLanes),
            fingerState: new mod.KnexFingerState(kxLanes)
          }
        })()
  const logStream = lanes === undefined ? undefined : (cfg.logStream ?? 'nektar_abcdefghij')
  const logState = lanes === undefined || logStream === undefined ? undefined : lanes.logState
  const fingerState = lanes === undefined ? undefined : lanes.fingerState
  // journaled carriers serve with their funding parent's BUMP attached the
  // moment fetchTx can answer it — rows journaled while the tree was still
  // unproven heal at read instead of re-serving stale ancestry forever
  if (lanes !== undefined) lanes.journal.enrich = async (txid) => (await mem.fetchTx(txid))?.merklePath
  // a reorg orphans every journaled path ≥ the fork — strip them so the
  // enrich seam re-attaches against the new headers instead of serving the
  // dead fork's evidence to counterparties. The tree ledger demotes the same
  // way: a proven row whose BUMP was orphaned flips back to unproven so the
  // proving pass re-anchors it (proven is otherwise sticky forever)
  if (lanes !== undefined)
    mem.onReorg?.((from) => {
      // the demotion leg is a durable wake invocation — a crash can no
      // longer drop the reorg with the dead fork's proofs still marked
      // proven. NO idempotency key: two distinct reorgs at the same fork
      // height are different events — an event-identity key would drop the
      // second demotion inside the retention window and leave that fork's
      // dead proofs marked proven until the floor. The leg is a range
      // update (unproveFrom/clearPaths), idempotent on a repeat announce.
      void exec.send({ service: EXEC_SVC.wake, handler: 'reorg' }, { from }).catch((e) => log(`automation: reorg send: ${(e as Error).message}`))
    })
  // restart continuity: pending provision rows keep their chain watch —
  // without it the abandon horizon never reaps a rejected provision tx
  if (lanes !== undefined) {
    void lanes.ledger
      .unproven()
      .then((rows) => {
        for (const u of rows) if (u.kind === lanes.mod.PROVISION_KIND) mem.watchTx?.(u.txid)
      })
      .catch(() => {})
  }
  // every fan-out leg becomes a per-host repair row via trackFanout — see
  // the module-level doc; the io seam is just the store's own outbox
  const fanoutIo = {
    enqueue: (o: Omit<OutboxItem, 'attempts'>, opts?: { delayMs?: number }) => store.enqueue(o, opts),
    ackOutbox: (id: string) => store.ackOutbox(id),
    pullOutbox: (id: string) => store.pullOutbox(id)
  }
  // bfinger packs store: packs describeShard PUTs to bstore also commit as
  // kind-5 sub-records under our identity; the finger record's refs then
  // resolve the whole pack set — no /.well-known dependency for readers
  let packsStore: import('@nektar/store').PacksStore | undefined
  if (lanes !== undefined) {
    const { PacksStore } = await import('@nektar/store')
    packsStore = new PacksStore({
      publish: async (kind, id, body) => {
        const r = await lanes.mod.publishSubRecord({ key, wallet, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, kind, id, body })
        // the journaled `finger:sub:{kind}:{id}` key lets a repair row re-serve
        // a missed host — a pack PUT now converges like every other fan-out
        await queueRepairs(
          fanoutIo,
          `finger:sub:${kind}:${id}`,
          r.missed.map((m) => m.host)
        )
        return { commitment: r.commitment, txid: r.txid }
      }
    })
    mem.attachPacksStore(packsStore)
  }
  // identity records may name http and loopback/private hosts on the test
  // lanes, and anywhere the operator runs its own hosts and says so
  const localNet = cfg.network === 'regtest' || cfg.localHosts === true
  // the shared context the regions meet through — door fields (resolveRecord,
  // poll, upkeep, ...) are assigned by the region openers and the deal loop
  // below; every call site reaches them lazily, so nothing can invoke a door
  // before assembly finishes
  const rtAbort = new AbortController()
  const rt = {
    cfg,
    mem,
    kx,
    kxLanes,
    store,
    key,
    wallet,
    objectStore,
    marketHosts,
    log,
    lanes,
    logStream,
    logState,
    fingerState,
    packsStore,
    localNet,
    me,
    fanoutIo,
    stopped: false,
    signal: rtAbort.signal,
    wakeStats: new Map<string, number>(),
    inflight: new Set<Promise<void>>(),
    downNow,
    hotUntil: 0,
    busy: false,
    upkeepAt: 0,
    acking: false,
    watchBases: (cfg.watchHosts ?? []).map((u) => u.replace(/\/+$/, '')),
    watches: new Map<string, AbortController>()
  } as unknown as Rt
  rt.countWake = (src) => {
    rt.wakeStats.set(src, (rt.wakeStats.get(src) ?? 0) + 1)
  }
  openLanes(rt)
  const transport: MarketTransport = {
    publishWant: async (sw: SignedWant) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      const r = await lanes.mod.publishArtifact({
        key,
        wallet,
        ledger: lanes.ledger,
        journal: lanes.journal,
        hosts: marketHosts,
        net: cfg.network,
        payload: { v: 1, kind: 'want', body: sw }
      })
      await trackFanout(r, fanoutIo)
      // a topic publish has no addressee to resolve — wake the shims we
      // hold instead: every ls_nektar watcher co-hosted on the same infra
      // re-polls its wants lanes now, not on the shim's poll cadence
      for (const base of rt.watchBases) rt.notifyPeer({ watch: base }, 'ls_nektar')
    },
    publishOffer: async (so: SignedOffer) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      // the offer goes where the BUYER's record says it keeps its artifacts —
      // this is also the seller's lazy resolution of the buyer
      const rec = so.offer.buyerPub !== undefined ? await rt.resolveRecord(so.offer.buyerPub) : undefined
      const hosts = rec === undefined ? marketHosts : [...new Set([...marketHosts, ...rec.hosts])]
      const r = await lanes.mod.publishArtifact({
        key,
        wallet,
        ledger: lanes.ledger,
        journal: lanes.journal,
        hosts,
        net: cfg.network,
        payload: { v: 1, kind: 'offer', body: so },
        // a buyer advertising push gets the offer carrier the moment it is
        // minted — concurrent with the durable host fanout
        ...(rec?.push !== undefined ? { push: (beef: number[]) => rt.pushCarrier?.(rec, beef) } : {})
      })
      await trackFanout(r, fanoutIo)
      // the buyer's watch shim (if it advertises one) gets a wake hint — the
      // offer publish is only visible to its polls once they re-check anyway.
      // No advertised shim → shared-infra fallback: our own bases may front
      // their watchers too.
      rt.notifyPeer(rec, 'ls_nektar')
      if (rec?.watch === undefined) for (const base of rt.watchBases) rt.notifyPeer({ watch: base }, 'ls_nektar')
    },
    send: async (to: string, msg: import('./market.js').MarketMessage) => {
      if (rt.msgs === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await rt.msgs.send(to, msg)
    },
    repair: async (host: string, journalKey: string) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await lanes.mod.resubmitCarrier({ journal: lanes.journal, host, key: journalKey })
    },
    republish: async (treeTxid: string, hash: string) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      const tx = await mem.fetchTx(treeTxid)
      if (tx?.merklePath === undefined) throw new Error('tree not proven yet')
      const r = await lanes.mod.republishOne({ journal: lanes.journal, hosts: marketHosts, hash, treeTxid, merklePath: tx.merklePath })
      if (!r.done) throw new Error(`republish ${hash.slice(0, 12)}: ${r.missed.map((m) => `${m.host} (${m.error})`).join(', ') || 'not done'}`)
      // quorum held but a host missed: the enriched beef is journaled, so a
      // repair row re-serves it — the republish row is acked, only the host
      // lags. `:proved` keeps the actionId distinct from the submit fan-out's
      // row, whose terminal state would make a plain `repair:` id a silent no-op
      await queueRepairs(
        fanoutIo,
        hash,
        r.missed.map((m) => m.host),
        { idSuffix: ':proved' }
      )
      if (r.submitted === true) log(`automation: republished carrier ${hash.slice(0, 12)} anchored by ${treeTxid.slice(0, 12)}`)
    }
  }
  // the one executor — the injected cfg.exec (an in-process Dispatcher for
  // memory-tier tests, or a retargeted RestateExecutor) or the ambient
  // RestateExecutor (NEKTAR_RESTATE_URL)
  const exec = cfg.exec ?? executor()
  rt.exec = exec
  const engine = new MarketEngine({
    store,
    executor: exec,
    call: cfg.call,
    memory: mem,
    key,
    me,
    network: cfg.network,
    now: cfg.now,
    settleDepth: cfg.settleDepth,
    height: () => mem.chainHeight(),
    tip: () => mem.tipRef(),
    fetchTx: (txid) => mem.fetchTx(txid),
    txFacts: (txid) => mem.txFacts(txid),
    spenderOf: async (txid, vout) => await mem.spenderOf?.(txid, vout),
    broadcast: (tx) => mem.broadcastTx(tx),
    txWire: (txid, maxBytes) => mem.txWire(txid, maxBytes),
    readTx: (hex) => mem.readTxWire(hex),
    rememberTx: (tx) => {
      mem.rememberTx(tx)
    },
    ownOutput: async (tx, vout) => {
      await mem.funds.adopt(tx, vout)
    },
    settleEscrow: async (tx) => {
      await mem.funds.spent(tx)
    },
    txDied: async (tx) => {
      await mem.funds.died(tx)
    },
    recordAired: (tx) => {
      mem.recordAired?.(tx)
    },
    transport,
    ai: cfg.ai ?? aiFromEnv(),
    log,
    killed: () => killSwitchActive(cfg.dataDir).killed,
    deaf: deafNow,
    opts: {
      ...cfg.opts,
      ...(cfg.minCollectMs !== undefined ? { minCollectMs: cfg.minCollectMs } : {}),
      ...(cfg.collectQuietMs !== undefined ? { collectQuietMs: cfg.collectQuietMs } : {}),
      ...(cfg.wantExtendMs !== undefined ? { wantExtendMs: cfg.wantExtendMs } : {}),
      ...(cfg.deliverBoundMs !== undefined ? { deliverBoundMs: cfg.deliverBoundMs } : {})
    },
    logStream
  })
  rt.engine = engine
  // the wake legs — the domain half of each mem.onTx*/onHeight/onReorg
  // callback, shared by the local spawns and the nektar-wake service's
  // journaled handlers. Legs throw on failure: the local caller logs
  // (today's log-and-drop), the wake service propagates so the runtime
  // retries — the durable send is what a crash can no longer lose.
  const wakeLegs: WakeLegs = {
    // a verified proof (broadcaster callback or the chain's own mined
    // event) may make a funding tree provable and advance
    // completion_submitted → settled
    txProof:
      lanes === undefined
        ? undefined
        : async (txid, mpHex) => {
            const mp = MerklePath.fromHex(mpHex)
            const unproven = await lanes.ledger.unproven()
            if (rt.stopped) return
            if (unproven.some((u) => u.txid === txid)) {
              await lanes.ledger.markProven(txid, mp)
              // a proven topup frees funding-tree slots — pull any
              // slot-starved outbox item forward instead of letting it sit
              // out the retry backoff (measured: a want stalled ~15s on this)
              await store.unblockOutbox('funding-slot')
              // carriers minted on this tree while it was unproven carry a
              // 10-minute host eviction deadline that never clears on its own —
              // queue a per-row re-announce with the parent's BUMP attached
              await rt.queueRepublish(txid)
            } else if (await lanes.ledger.attachParentPath(txid, mp)) {
              // the proof was for a funding parent — stored tree beefs now
              // carry anchored ancestry, so their slots serve immediately
              await store.unblockOutbox('funding-slot')
            }
          },
    // the substrate's abandon horizon is the terminal verdict for a mint
    // that resubmitted but never mined — drop the row so the kind's mint
    // gate re-opens (same reap as a chain rejection, driven by the event)
    txAbandoned:
      lanes === undefined
        ? undefined
        : async (txid) => {
            const unproven = await lanes.ledger.unproven()
            const row = unproven.find((u) => u.txid === txid)
            if (row === undefined) return
            const tx = await mem.fetchTx(txid)
            await lanes.ledger.drop(txid)
            // carriers minted on the dead tree are unadmittable forever — reap
            // their journal rows too, else the repair leg resubmits them endlessly
            for (const j of (await lanes.journal.fundedBy?.(txid)) ?? []) await lanes.journal.del?.(j.hash)
            const reap = abandonedReap(row.kind)
            if (reap.ghost && tx !== undefined) rt.ghostMint(tx)
            log(reap.ghost ? `automation: dropped abandoned funding mint ${txid}` : `automation: dropped abandoned provision tx ${txid}`)
            await store.unblockOutbox('funding-slot')
          },
    // a reorg demotes every proof anchored at/above the fork horizon —
    // journal paths clear so enrich re-attaches against the new headers,
    // proven rows flip back to unproven so the proving pass re-anchors
    reorg:
      lanes === undefined
        ? undefined
        : async (from) => {
            // awaited, not void'd — this runs inside a journaled ctx.run:
            // returning early would journal 'done' while the demotion is
            // still in flight, and a crash or swallowed rejection drops it
            // permanently (one-shot send, no re-fire). A throw retries the
            // step — that's exactly what the journal is for.
            await Promise.all([lanes.journal.clearPaths?.(from), lanes.ledger.unproveFrom?.(from)])
          },
    // a tip advance frees every height-bound op (seize/refund deadlines)
    // at block arrival rather than the next floor tick
    dueAtHeight: async (tip) => await store.hasDueAtHeight(NONTERMINAL, tip)
  }
  // the watchdog pass — the cheap probe tier (chain/broadcaster/hosts/kill
  // state) on a slow tick; host losses update the deaf flag the engine
  // consults at every new-deal entry point. Shared by the local scheduler
  // row and the nektar-watchdog object's journaled tick
  const watchdogLeg = async (): Promise<void> => {
    try {
      const r = await runChecks([...WATCHDOG_CHECKS])
      if (lastReport !== undefined) {
        const byName = new Map(lastReport.checks.map((c) => [c.check, c]))
        for (const c of r.checks) byName.set(c.check, c)
        lastReport = { ...r, checks: [...byName.values()], pass: [...byName.values()].every((c) => c.pass) }
      } else lastReport = r
      for (const h of r.downHosts)
        if (!downNow.has(h)) {
          downNow.add(h)
          log(`automation: watchdog: host down ${h}`)
        }
      for (const h of Array.from(downNow)) if (marketHosts.includes(h) && !r.downHosts.includes(h)) await rt.hostBack(h)
    } catch (e) {
      log(`automation: watchdog: ${(e as Error).message}`)
    }
  }
  // the market wallet ledger gets the same dead-verdict rescan the mcp-side
  // ledger gets: a refused escrow can still air — the accept ships signed
  // bytes and the counterparty re-broadcasts them through its own guard.
  // Without this, a 'rejected' row is terminal forever: the input sits
  // released-but-spent and the change stays tombstoned. Re-observing dead
  // rows is what converges ledger and custody on the chain's truth.
  const rebuildStored = (w: WalletTx): Transaction => {
    const tx = Transaction.fromHex(w.rawHex)
    for (const inp of tx.inputs) {
      const hex = inp.sourceTXID !== undefined ? w.parents[inp.sourceTXID] : undefined
      if (hex !== undefined) inp.sourceTransaction = Transaction.fromHex(hex)
    }
    return tx
  }
  // machine mode: the same custody semantics as declared intents —
  // the table decides WHICH fire, the effector just executes. The tx
  // virtual object's ctx.run legs run these same impls when the executor
  // is restate — one effect set, two dispatch paths.
  const txFx: TxEffects = {
    txDied: async (w) => {
      await mem.funds.died(rebuildStored(w))
    },
    txSpent: async (w) => {
      await mem.funds.spent(rebuildStored(w))
      log(`automation: ${w.txid.slice(0, 16)}… aired after our ${w.state} verdict — custody re-marked`)
    },
    recordAired: (w) => {
      mem.recordAired?.(rebuildStored(w))
    }
  }
  // the sweep's observeFn — shared by the watcher (local ticks) and the
  // nektar-sweep service (restate mode sends each observation to the row's
  // tx object)
  const observeFn = async (txid: string) => {
    const f = await mem.txFacts(txid)
    if (f.verdict === 'mined') return { status: 'proven' as const, source: 'chain' }
    if (f.verdict === 'dead') return { status: 'rejected' as const, source: f.source ?? 'chain', reason: 'dead' }
    return hintSeen(f.hint) ? { status: 'seen' as const, source: 'broadcaster' } : undefined
  }
  const _ledgerWatcher = new TransactionWatcher(
    store,
    observeFn,
    async (w) => {
      await mem.funds.died(rebuildStored(w))
    },
    {
      onRevived: async (w) => {
        // inputs provably spent, change provably ours — spent() does both
        // marks; the change output's tombstone lifts on re-adoption
        const tx = rebuildStored(w)
        await mem.funds.spent(tx)
        // the refusal let the sats leave anyway — the day ledger must
        // count them or tomorrow's headroom is overstated
        mem.recordAired?.(tx)
        log(`automation: ${w.txid.slice(0, 16)}… aired after our ${w.state} verdict — custody re-marked`)
      },
      effects: txFx,
      exec
    }
  )
  let lastLedgerSweep = 0
  let lastWakeReport = 0
  // boot replay: 'prepared'/'submission_unknown' rows are signed intents a
  // crashed process never got a verdict for. The sweep.boot kick below
  // covers it — a journaled per-key fan-out.
  openUpkeep(rt)
  openWatches(rt)
  // the durable executor is unconditional: every routing site's send/call
  // lands at the restate ingress, and the handler defs serve on the
  // in-process endpoint the server invokes over HTTP/2.
  const txDeps = { store, fx: txFx, log, submit: (actionId: string) => engine.submitTx(actionId) }
  const sweepDeps = {
    scan: async function* () {
      for await (const x of scanTxObservations(store, observeFn, undefined)) yield { actionId: x.w.actionId, observe: x.observe }
    },
    resubmitRows: () => store.walletTxs(['prepared', 'submission_unknown']),
    log
  }
  const dealDeps = {
    advanceOp: (operationId: string) => engine.advanceOp(operationId),
    inboxEvent: (item: Parameters<typeof engine.dealEvent>[0]) => engine.dealEvent(item),
    reconcileOp: (operationId: string) => engine.reconcileOp(operationId),
    log
  }
  // the deals floor cadence — hot while a live op is in flight (the old
  // wheel row's adaptive ceiling, re-read on every arm)
  const idleMs = cfg.intervalMs ?? 1000
  const hotMs = cfg.hotMs ?? 250
  // fan-out dedup — the tick re-sends every pending row/live op each pass;
  // under restate each send is a durable queued invocation, so the same
  // send-identity is throttled to one per window (domain clock).
  const sendDedup = new Map<string, { at: number; ms: number }>()
  // due-arm dedup — a transition re-arms its stamped due_at on every write;
  // an identical (op, instant) delayed send is pure churn on the key's
  // FIFO. The ingress `due:` key is the durable dedup (it survives a
  // restart against the still-pending arm); this map is only the cheap
  // pre-filter — entries record the due instant so expired arms evict
  // instead of clear()-bursting every pending send at the cap.
  const armedDue = new Map<string, number>()
  let fanSeq = 0
  const walletCoord = engine.walletCoordinator
  const dealfanDeps = {
    pendingInbox: () => engine.pendingInbox(),
    liveOps: async () => (await store.operations({ states: NONTERMINAL })).map((o) => ({ operationId: o.operationId, wantId: opWantId(o) })),
    reconcileOps: async () => {
      // reconcile_at is stamped on the store's clock — compare on the same
      // clock (a virtual `now` ahead of Date.now() would select nothing)
      const now = (cfg.now ?? Date.now)()
      const rows = await store.operations({ role: 'buyer', states: ['funding_failed', 'refunded'], reconcileDue: now })
      return rows.map((o) => ({ operationId: o.operationId, wantId: opWantId(o) }))
    },
    // the resubmit cadence leg — 'prepared'/'submission_unknown' rows are
    // signed intents; the floor re-airs them on the engine's jittered cadence
    resubmitPending: () => engine.resubmitTick(),
    ceilingMs: () => (Date.now() < rt.hotUntil ? Math.min(hotMs, idleMs) : idleMs),
    // the arm's idempotency window quantizes on the domain clock — a virtual
    // now must quantize on itself or the arm keys wobble across drive()s
    now: () => (cfg.now ?? Date.now)(),
    // in-flight send dedup on the domain clock — virtual `now` in tests
    // advances with drive(), so windows expire on the same clock the
    // deadlines arm on. Restate makes every re-sent send a durable queued
    // invocation; the map bounds per-row/per-op re-fan to one send per
    // window instead of one per tick.
    sentRecently: (id: string, ms: number) => {
      const now = (cfg.now ?? Date.now)()
      const last = sendDedup.get(id)
      if (last !== undefined && now - last.at < ms) return true
      if (sendDedup.size > 10_000) {
        // evict entries whose window already closed — a clear() would drop
        // hot keys and re-fire every pending send in one burst
        for (const [k, v] of sendDedup) if (now - v.at >= v.ms) sendDedup.delete(k)
        if (sendDedup.size > 10_000) sendDedup.delete(sendDedup.keys().next().value!)
      }
      sendDedup.set(id, { at: now, ms })
      return false
    },
    // distinct ticks are distinct pokes — sentRecently above bounds their
    // rate, the seq keeps the durable key deduping replays, not cadence
    tickSeq: () => ++fanSeq,
    log
  }
  // the transport deps — outbox delivery as journaled invocations:
  // drain scans due rows (journaled), fans deliver per destination key,
  // and self-schedules its ceiling at the earliest next_at
  const transportDeps = {
    dueRows: () => store.dueOutbox(),
    nextAt: () => store.nextOutboxAt(),
    deliverItem: (item: OutboxItem) => engine.deliverOutboxItem(item),
    // next_at is stamped on the store's clock — the arm must subtract the
    // SAME clock or every due-row arm lands early/late by the clock skew
    // (a virtual `now` ahead of Date.now() would park due work indefinitely)
    now: () => (cfg.now ?? Date.now)(),
    log
  }
  // the custody deps — one fixed key serializes every wallet custody
  // leg (broadcasts, dead-verdicts, the resubmit scan); the journaled
  // successor of the run() chain for custody writes
  const custodyDeps = walletCoord !== undefined ? { wallet: walletCoord, log } : undefined
  // the wake deps — mem.onTx*/onHeight/onReorg callbacks become durable
  // invocations; the legs run journaled, the cycle tail leaves through
  // journaled sends (dealfan.tick + transport.drain)
  const wakeDeps = { legs: wakeLegs, hint: (txid: string) => engine.hint(txid), now: () => (cfg.now ?? Date.now)(), log }
  // the cadence deps — durable, self-arming invocations: each tick runs
  // the whole pass inside one journaled step then arms the next at its
  // ceiling. A throwing pass retries instead of silently stopping.
  const upkeepDeps = { tick: () => rt.upkeep(), ceilingMs: () => cfg.upkeepMs ?? WAITS.upkeepCadence.ms, now: () => (cfg.now ?? Date.now)(), log }
  const watchdogMs0 = cfg.watchdogMs ?? WAITS.watchdogCadence.ms
  const watchdogDeps = watchdogMs0 > 0 ? { tick: watchdogLeg, ceilingMs: () => watchdogMs0, now: () => (cfg.now ?? Date.now)(), log } : undefined
  const watchesDeps =
    rt.watchBases.length !== 0 ? { tick: () => rt.refreshWatches(), ceilingMs: () => WAITS.watchRearm.ms, now: () => (cfg.now ?? Date.now)(), log } : undefined
  // the notify deps — notifyPeer/pushCarrier push legs as journaled,
  // per-destination invocations (the legs lanes.ts exposes via rt.notifyLegs)
  const notifyDeps = rt.notifyLegs !== undefined ? { peer: rt.notifyLegs.peer, carrier: rt.notifyLegs.carrier, log } : undefined
  let rtEndpoint: RtEndpoint | undefined
  if (isLocalDispatch(exec)) {
    // injected in-process dispatcher (tests) — wire the same handler bodies
    // the endpoint would serve onto this runtime's own executor: no port,
    // no registration, no shared handler map — two automations in one
    // process stay isolated. Host-composed defs can't wire (they are
    // restate service objects, not bodies) — refuse loudly, not silently.
    if ((cfg.rtDefs ?? []).length !== 0)
      throw new Error('automation: cfg.rtDefs requires the restate endpoint — cfg.exec wiring serves automation services only')
    wireRuntime(exec, {
      tx: txDeps,
      sweep: sweepDeps,
      deal: dealDeps,
      dealfan: dealfanDeps,
      transport: transportDeps,
      custody: custodyDeps,
      wake: wakeDeps,
      crons: [
        [EXEC_SVC.upkeep, upkeepDeps],
        ...(watchdogDeps !== undefined ? ([[EXEC_SVC.watchdog, watchdogDeps]] as Array<[string, typeof watchdogDeps]>) : []),
        ...(watchesDeps !== undefined ? ([[EXEC_SVC.watches, watchesDeps]] as Array<[string, typeof watchesDeps]>) : [])
      ],
      notify: notifyDeps
    })
  } else {
    const txDef = await txObject(txDeps)
    const sweepDef = await sweepService(sweepDeps, txDef)
    // the per-op object — advance legs land on the op's own key, serialized
    // and journaled; the tick's NONTERMINAL loop fans out to it
    const dealDef = await dealObject(dealDeps)
    // the dealfan service — the whole tick as one journaled invocation:
    // inbox items → deal.event per correlation key, ops → deal.advance,
    // due reconcile rows → deal.reconcile. Scans journal; sends dedupe.
    const dealfanDef = await dealfanObject(dealfanDeps, dealDef)
    const transportDef = await transportObject(transportDeps)
    const custodyDefs = custodyDeps !== undefined ? [await custodyObject(custodyDeps)] : []
    const wakeDef = await wakeService(wakeDeps)
    const upkeepDef = await cronObject(EXEC_SVC.upkeep, upkeepDeps, RT_OPTS_HEAVY)
    const watchdogDefs = watchdogDeps !== undefined ? [await cronObject(EXEC_SVC.watchdog, watchdogDeps)] : []
    const watchesDefs = watchesDeps !== undefined ? [await cronObject(EXEC_SVC.watches, watchesDeps)] : []
    const notifyDefs = notifyDeps !== undefined ? [await notifyObject(notifyDeps)] : []
    try {
      rtEndpoint = await openEndpoint(
        [
          txDef,
          sweepDef,
          dealDef,
          dealfanDef,
          transportDef,
          wakeDef,
          upkeepDef,
          ...watchdogDefs,
          ...watchesDefs,
          ...notifyDefs,
          ...custodyDefs,
          // host-composed defs (mcp's confirmer/watcher region) — same endpoint
          ...(cfg.rtDefs ?? [])
        ],
        process.env,
        log
      )
    } catch (e) {
      // mid-assembly failure (e.g. endpoint port already bound) must not
      // leave the stores open — a held sqlite -wal fails temp-dir cleanup
      await kxLanes?.destroy().catch(() => undefined)
      await kx.destroy().catch(() => undefined)
      throw e
    }
    log(`automation: restate endpoint ${rtEndpoint.uri}`)
  }
  // the outbox opens AFTER the executor's handlers are registered — its
  // boot poke is a drain send, and under a local dispatcher an unregistered
  // send is refused on the spot (restate would have queued it durably).
  // Anything enqueued during assembly is still covered: the poke fires
  // once, here, with the drain's handler live.
  openOutbox(rt)
  // boot resubmit fans out through the sweep — each row's re-air is a
  // journaled invocation on its own tx object (local: the same handler)
  void exec.send({ service: EXEC_SVC.sweep, handler: 'boot', key: 'main' }, {}).catch((e) => log(`automation: sweep boot send: ${(e as Error).message}`))
  // the floor + cadence chains boot-kick once — each tick self-arms the
  // next. intervalMs===0 disables the deals floor entirely.
  if (cfg.intervalMs !== 0)
    void exec.send({ service: EXEC_SVC.dealfan, handler: 'tick', key: 'main' }, {}).catch((e) => log(`automation: dealfan kick: ${(e as Error).message}`))
  const watchdogOn = (cfg.watchdogMs ?? WAITS.watchdogCadence.ms) > 0
  for (const svc of [EXEC_SVC.upkeep, ...(watchdogOn ? [EXEC_SVC.watchdog] : []), ...(rt.watchBases.length > 0 ? [EXEC_SVC.watches] : [])])
    void exec.send({ service: svc, handler: 'tick', key: 'main' }, {}).catch((e) => log(`automation: ${svc} kick: ${(e as Error).message}`))
  for (const k of cfg.rtKicks ?? []) void exec.send(k, {}).catch((e) => log(`automation: ${k.service} kick: ${(e as Error).message}`))
  let again = false
  let running: Promise<void> | undefined
  // a phase that never settles parks EVERY later pass behind `running` —
  // bound each leg so the worst case is a logged timeout, not a silent
  // wedge (observed live: an unbounded leg froze the whole suite). The
  // leg's deadline is ambient — deadline() runs work inside the ALS
  // context, so inner IO merges it via ioSignal/nap and dies with the leg
  // (or at the successor leg's bound) instead of the abandoned promise
  // finishing inside the next pass's world
  const bound = async <T>(name: string, work: (signal: AbortSignal) => Promise<T>, ms = WAITS.dealsBound.ms): Promise<T | undefined> => {
    const r = await deadline(work, ms, { signal: rt.signal })
    if (r.outcome === 'settled') return r.value
    if (r.outcome === 'timeout') log(`automation: ${name} exceeded ${ms}ms — abandoning leg`)
    return undefined
  }
  const deals = async (): Promise<void> => {
    if (rt.busy) {
      again = true
      await running
      return
    }
    rt.busy = true
    const run = (async () => {
      try {
        do {
          again = false
          try {
            // phase timing: a pass that outruns the want collect window is a
            // silent deal killer — name the segment that ate it
            const t0 = Date.now()
            // one operation budget: the three legs share passBound — a hung
            // poll abandoning at its leg bound leaves the pass's remainder,
            // not a fresh legMs for the next leg to burn (nested fresh
            // bounds would let one pass eat 3x the budget)
            const passUntil = t0 + WAITS.passBound.ms
            const legMs = (): number => Math.max(1, Math.min(WAITS.dealsBound.ms, passUntil - Date.now()))
            await bound('poll', () => rt.poll(), legMs())
            const t1 = Date.now()
            await bound('tick', () => engine.tick(), legMs())
            const t2 = Date.now()
            // the ledger sweep rides the deals pass — serialized behind the
            // same busy flag, throttled to a chain-read cadence rather than
            // the deal loop's hot tick
            if (t2 - lastLedgerSweep > WAITS.ledgerSweep.ms) {
              lastLedgerSweep = t2
              // the scan is a journaled service invocation that sends each
              // observation to its row's tx object — the pass doesn't block
              // on it
              void exec
                .send({ service: EXEC_SVC.sweep, handler: 'tick', key: 'main' }, {})
                .catch((e) => log(`automation: sweep tick send: ${(e as Error).message}`))
            }
            rt.pushAcks()
            const t3 = Date.now()
            if (t3 - t0 > WAITS.passReport.ms) log(`automation: pass ${t3 - t0}ms (poll ${t1 - t0} tick ${t2 - t1} acks ${t3 - t2})`)
            // the push-vs-floor witness: which sources actually fired passes.
            // A path that never counts is dead push degrading to ceiling speed
            if (t3 - lastWakeReport > WAITS.wakeReport.ms) {
              lastWakeReport = t3
              if (rt.wakeStats.size > 0) log(`automation: wakes ${[...rt.wakeStats.entries()].map(([s, n]) => `${s}:${n}`).join(' ')}`)
            }
            // a live deal keeps the loop hot — the 5s inbound window alone
            // would let a deal cool to idle cadence between hops. A settled
            // proof awaiting depth only waits on BLOCKS, so it is excluded —
            // holding the loop hot for it would burn cycles per confirmation
            if (await store.hasLiveOps(NONTERMINAL)) rt.hotUntil = Date.now() + WAITS.hotWindow.ms
            // new lanes appear as deals start — re-derive watch subs at the
            // end of a pass so a fresh want is subscribed within one cycle;
            // under restate the object serializes it against a running tick
            // the re-derive poke is a hint — refreshWatches scans at run
            // time — so a pass of commits coalesces to one queued tick per
            // 5s bucket instead of one scan per pass
            if (rt.watchBases.length > 0) {
              const b = Math.floor((cfg.now ?? Date.now)() / 5000)
              void exec.send({ service: EXEC_SVC.watches, handler: 'tick', key: 'main' }, {}, { idempotencyKey: `watches:poke:${b}` }).catch(() => undefined)
            }
          } catch (e) {
            log(`automation: ${(e as Error).message}`)
          }
        } while (again && !rt.stopped)
      } finally {
        rt.busy = false
        if (again && !rt.stopped) void deals()
      }
    })()
    running = run
    await run
  }
  rt.deals = deals
  // wake-on-commit: a store write that lands fresh work (new op, state
  // change, inbox rows, an accepted tx event) fires the deals loop now,
  // not at the floor. The !busy gate is load-bearing — commits inside a
  // pass are visible to the pass's own scans, and firing deals() mid-pass
  // would set `again` on every engine transition (a per-pass retryer
  // would hot-spin instead of cooling to the ceiling).
  store.workReady = () => {
    rt.countWake('commit')
    if (!rt.busy && !rt.stopped) void rt.deals().catch((e) => log(`automation: ${(e as Error).message}`))
  }
  // committed deadlines arm the executor directly — a collect window or
  // seize dueAt fires at its instant; the floor is the missed-arm backstop.
  // Under restate the arm is a durable delayed send to the op's own deal
  // object — the runtime owns the timer, a restart can't drop the arm.
  store.dueAtArmed = (operationId, at) => {
    rt.countWake('dueAt')
    if (rt.stopped) return
    const dueKey = `${operationId}:${at}`
    if (armedDue.has(dueKey)) return
    if (armedDue.size > 10_000) {
      // evict fired arms — their delayed sends already landed; the ingress
      // `due:` key still dedupes any late re-arm against the pending arm
      const now = (cfg.now ?? Date.now)()
      for (const [k, at0] of armedDue) if (at0 < now) armedDue.delete(k)
      if (armedDue.size > 10_000) armedDue.delete(armedDue.keys().next().value!)
    }
    armedDue.set(dueKey, at)
    void (async () => {
      // key by the want correlation id — the arm lands on the same deal
      // object as the want's events/advances, serializing with them
      const op = await store.operation(operationId)
      await exec.send(
        { service: EXEC_SVC.deal, handler: 'due', key: dealOpKey({ operationId, wantId: op ? opWantId(op) : undefined }) },
        { op: operationId },
        // the arm fires on the DOMAIN clock — `at` is a store.now() timestamp;
        // mixing clocks under a virtual `now` skews every arm (a test clock
        // running ahead would push each due send past the deadline it arms).
        // The idempotency key makes the SAME (op, instant) arm dedupe across
        // a restart — the pending delayed invocation outlives this process's
        // memory, so the in-memory map alone can't see it.
        { delayMs: Math.max(0, at - (cfg.now ?? Date.now)()), idempotencyKey: `due:${dueKey}` }
      )
    })().catch((e) => {
      // the send never landed — release the dedup so the next arm retries
      // (the tick's throttled advance floor still catches the deadline)
      armedDue.delete(dueKey)
      log(`automation: deal due send ${operationId}: ${(e as Error).message}`)
    })
  }
  /** one full pass: deals, then upkeep — what tests and push events drive */
  const cycle = async (): Promise<void> => {
    // cycle() is the push path — a caller asking for a full pass gets the
    // ledger sweep too; the 15s wtx throttle is the floor cadence, not a
    // reason to sit on a verdict the substrate already verified
    lastLedgerSweep = -Infinity
    await deals()
    if (!rt.stopped) await rt.upkeep()
    // join the detached drain only at the cycle boundary — a scheduled
    // deals pass must not serialize on delivery RTTs, but tests and
    // event-driven cycles keep "one call = all queued work attempted"
    await rt.drainDone
  }
  rt.cycle = cycle
  // the cadence plane is fully executor-owned: dealfan self-arms the deals
  // floor (adaptive hot/idle ceiling on every arm), the cron objects carry
  // upkeep/watchdog/watches, and wakes are event sends. No wheel.
  if (rt.watchBases.length !== 0) {
    const b = Math.floor((cfg.now ?? Date.now)() / 5000)
    void exec.send({ service: EXEC_SVC.watches, handler: 'tick', key: 'main' }, {}, { idempotencyKey: `watches:poke:${b}` }).catch(() => undefined)
  }
  // push-driven progress: a verified tx proof (broadcaster callback or the
  // chain's own mined event) may make a funding tree provable and advance
  // completion_submitted → settled — run a cycle now, not at the next interval
  mem.onTxProof((txid, mp) => {
    if (rt.stopped) return
    rt.countWake('txProof')
    // the leg + cycle tail is a durable wake invocation — a crash between
    // proof receipt and markProven can no longer drop the wake; the
    // idempotency key dedupes a repeat announce of the SAME proof. Keyed
    // on the proof content, not the txid: a tx re-mined in a different
    // block (post-reorg) carries a different path — deduping by txid would
    // swallow the new path inside the retention window and the row would
    // stay unproven until the floor. send() — the producer discards the
    // result; the promise still joins at ACCEPTANCE (the leg is durably
    // recorded) without holding the callback for the handler's duration
    const mpHex = mp.toHex()
    const work = exec
      .send(
        { service: EXEC_SVC.wake, handler: 'txProof' },
        { txid, mpHex },
        {
          idempotencyKey: `wake:txProof:${txid}:${createHash('sha256').update(mpHex).digest('hex').slice(0, 16)}`
        }
      )
      .catch((e: unknown) => log(`automation: txproof: ${(e as Error).message}`))
    rt.inflight.add(work)
    const done = (): void => {
      rt.inflight.delete(work)
    }
    void work.then(done, done)
  })
  // a rejection callback is only a hint: clear the reconcile throttle for the
  // affected operation and run a cycle — the engine consults txFacts itself
  mem.onTxRejected((txid: string) => {
    if (rt.stopped) return
    rt.countWake('txRejected')
    // a rejection callback is only a hint — the wake leg reconsults the
    // ledger itself; the cycle tail runs inside the invocation. NO
    // idempotency key: a resubmitted tx carries the same txid — deduping
    // by txid would drop its second rejection inside the retention window
    // (hint + cycle are trivially re-runnable; a repeat announce costs one
    // journaled invocation, a dropped real rejection waits for the floor)
    const work = exec
      .send({ service: EXEC_SVC.wake, handler: 'txRejected' }, { txid })
      .catch((e: unknown) => log(`automation: txrejected: ${(e as Error).message}`))
    rt.inflight.add(work)
    const done = (): void => {
      rt.inflight.delete(work)
    }
    void work.then(done, done)
  })
  // a tip advance frees every height-bound op (seize/refund deadlines) at
  // block arrival rather than the next floor tick — hosts without a height
  // feed leave the floor as the only driver, same as today
  mem.onHeight?.((tip: number) => {
    if (rt.stopped) return
    rt.countWake('height')
    // the due-gate lives inside the wake leg — a tip that frees no op
    // costs one journaled probe, not a pass
    const work = exec
      .send({ service: EXEC_SVC.wake, handler: 'height' }, { tip }, { idempotencyKey: `wake:height:${tip}` })
      .catch((e: unknown) => log(`automation: height: ${(e as Error).message}`))
    rt.inflight.add(work)
    const done = (): void => {
      rt.inflight.delete(work)
    }
    void work.then(done, done)
  })
  // the substrate's abandon horizon is the terminal verdict for a mint that
  // resubmitted but never mined — drop the row so the kind's mint gate
  // re-opens (same reap as a chain rejection, driven by the event)
  mem.onTxAbandoned((txid: string) => {
    if (rt.stopped || lanes === undefined) return
    rt.countWake('txAbandoned')
    // NO idempotency key — same class as txRejected: a resubmit between
    // two abandons can recreate reapable state; the reap is idempotent, a
    // dropped second abandon is not
    const work = exec
      .send({ service: EXEC_SVC.wake, handler: 'txAbandoned' }, { txid })
      .catch((e: unknown) => log(`automation: txAbandoned: ${(e as Error).message}`))
    rt.inflight.add(work)
    const done = (): void => {
      rt.inflight.delete(work)
    }
    void work.then(done, done)
  })
  return {
    store,
    engine,
    cycle,
    deals,
    upkeep: () => rt.upkeep(),
    logStream,
    dealJournal: rt.dealJournal,
    objectStore,
    /** the executor-plane surface for the dashboards — a paused count that
     *  can only come from OUTSIDE the plane (a wedged FIFO can't report
     *  itself; the admin API reads fine while objects sit frozen) */
    rt: {
      adminUrl: nektarEnv('NEKTAR_RESTATE_ADMIN') ?? 'http://127.0.0.1:9070',
      pausedCount: () => pausedInvocations(nektarEnv('NEKTAR_RESTATE_ADMIN') ?? 'http://127.0.0.1:9070')
    },
    /** the latest full preflight report — health surfaces read this, the
     *  watchdog keeps its cheap tier fresh between full runs */
    health: () => ({ pass: lastReport?.pass ?? false, report: lastReport }),
    // the join flow's hot path — the lanes/publish sites read this array live;
    // a pushed host enters the next probe + fan-out pass without a restart
    addHost: (u) => {
      const n = u.replace(/\/+$/, '')
      if (n !== '' && !marketHosts.includes(n)) marketHosts.push(n)
    },
    removeHost: (u) => {
      const n = u.replace(/\/+$/, '')
      const i = marketHosts.indexOf(n)
      if (i >= 0) marketHosts.splice(i, 1)
      downNow.add(n) // the watchdog's revive check requires list membership — a spliced host stays down this run
    },
    bootstrap: () => (cfg.publishFinger === true && lanes !== undefined ? 'sealed' : 'unsealed'),
    /** the unioned wait read — wake witness counts + every unacked outbox row */
    waits: async () => ({ wakes: Object.fromEntries(rt.wakeStats), deferred: await store.waitingOn() }),
    /** re-run the full preflight now (automationDoctor) — refreshes the
     *  cached report and the seeded host-health set */
    preflight: async () => {
      lastReport = await runChecks()
      for (const h of lastReport.downHosts) downNow.add(h)
      // D5's assertion surface, on the doctor path it was built for: an
      // unclassified state or an op past its declared bound is a check row
      try {
        const audit = await engine.dealStateAudit()
        lastReport.checks.push({
          check: 'deal-states',
          pass: audit.violations.length === 0,
          detail:
            audit.violations.length === 0
              ? `${Object.keys(audit.limits).length} bounds declared; no violations`
              : `${audit.violations.length} violations: ${audit.violations
                  .slice(0, 3)
                  .map((v) => `${v.operationId.slice(0, 8)}:${v.state}`)
                  .join(', ')}`,
          fix: 'unclassified state or op past its declared bound — STATE_LIMITS/NONTERMINAL/PARKED must cover the union'
        })
      } catch {
        /* the audit surface is advisory — never let it fail the doctor */
      }
      return lastReport
    },
    // a counterparty's direct push lands here — same carriers a host read
    // would yield, verified by the same code, deduped by the same hashes
    pushCarrier: async (beef: number[]) => (rt.pushIngest !== undefined ? await rt.pushIngest(beef) : false),
    marketSlots: async () =>
      lanes === undefined
        ? { market: 0, bbox: 0, log: 0, finger: 0 }
        : {
            market: await lanes.ledger.remaining('market'),
            bbox: await lanes.ledger.remaining('bbox'),
            log: await lanes.ledger.remaining('log'),
            finger: await lanes.ledger.remaining('finger')
          },
    laneStock: async (): Promise<Record<string, { proven: number; anchored: number; unprovenTrees: number; taken: number }>> =>
      lanes === undefined
        ? {}
        : {
            market: await lanes.ledger.stock('market'),
            bbox: await lanes.ledger.stock('bbox'),
            log: await lanes.ledger.stock('log'),
            finger: await lanes.ledger.stock('finger')
          },
    // the durable carrier journal — delivered lane envelopes by artifact;
    // tests and operator diagnostics read delivery evidence through the same
    // interface production writes it (no second knex handle on the file)
    carriers: lanes?.journal,
    // what a provisioning transaction should carry: one lane-locked output
    // per slot, a whole tree's worth per lane — the block confirming the
    // money then also proves the first stock
    provisionPlan: async () => {
      if (lanes === undefined) return { outputs: [] }
      const outputs: Array<{ kind: string; lockingScriptHex: string; satoshis: number; count: number }> = []
      for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger']) {
        const spec = await rt.laneSpec(kind)
        if (spec.lock === undefined) continue
        outputs.push({ kind, lockingScriptHex: spec.lock.toHex(), satoshis: spec.satsPerOutput, count: spec.size })
      }
      return { outputs }
    },
    // a funding tx that already carries lane-locked outputs donates them as
    // slot stock — proven or not; the upkeep loop attaches the BUMP later
    adoptProvision: async (tx: import('@bsv/sdk').Transaction) => {
      if (lanes === undefined) return {}
      if (tx.merklePath === undefined) {
        const seen = await mem.fetchTx(tx.id('hex'))
        if (seen?.merklePath !== undefined) tx.merklePath = seen.merklePath
      }
      const laneTable: Array<{ kind: string; lockHex: string; minSats: number }> = []
      for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger']) {
        const spec = await rt.laneSpec(kind)
        if (spec.lock !== undefined) laneTable.push({ kind, lockHex: spec.lock.toHex(), minSats: spec.satsPerOutput })
      }
      const matched = lanes.mod.matchProvision(tx, laneTable)
      if (matched.length === 0) return {}
      const n = await lanes.ledger.adopt(tx, matched)
      if (n === 0) return {}
      // an unproven provision tx needs a chain watch: it is not one of this
      // instance's wallet txs, so nothing else registers it with the event
      // substrate — without this the abandon horizon can never reap it
      if (tx.merklePath === undefined) mem.watchTx?.(tx.id('hex'))
      // adopted slots are usable the moment the tx proves — wake deferred work
      await store.unblockOutbox('funding-slot')
      const counts: Record<string, number> = {}
      for (const s of matched) counts[s.kind] = (counts[s.kind] ?? 0) + 1
      return counts
    },
    stop: async () => {
      rt.stopped = true
      rtAbort.abort()
      for (const ac of rt.watches.values()) ac.abort()
      rt.watches.clear()
      // bounded like the drain below — a wedged inflight item must not
      // stall teardown before the deadline loop even starts
      {
        const r = await deadline(() => Promise.allSettled(rt.inflight).then(() => 'done' as const), WAITS.stopSettle.ms)
        if (r.outcome !== 'settled') log(`automation: stop: inflight settle deadline hit (${rt.inflight.size} pending) — proceeding`)
      }
      // drain in-flight work with a bound — a wedged producer must not hang
      // teardown forever (was an unbounded spin)
      const drainDeadline = Date.now() + WAITS.stopDrain.ms
      while (rt.busy || rt.upkeepRun !== undefined || rt.acking) {
        if (Date.now() >= drainDeadline) {
          log(`automation: stop: drain deadline hit (busy=${rt.busy} upkeep=${rt.upkeepRun !== undefined} acking=${rt.acking}) — proceeding`)
          break
        }
        await nap(WAITS.drainPoll.ms)
      }
      await engine.idle()
      await rtEndpoint?.close().catch(() => undefined)
      // an injected dispatcher owns armed delayed sends + in-flight work —
      // join it before the stores close or a post-stop dispatch hits a
      // destroyed pool
      await (exec as { close?: () => void | Promise<void> }).close?.()
      await kxLanes?.destroy()
      await kx.destroy()
    }
  }
}
