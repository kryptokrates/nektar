import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Knex } from 'knex'
import type { SignedWant, SignedOffer } from '@nektar/bstack'
import type { AutomationHost } from './host.js'
import { AutomationStore, type OutboxItem } from './store.js'
import { MarketEngine, TERMINAL, type MarketTransport } from './market.js'
import { ScriptedExecutor, HttpChatExecutor, type AIExecutor } from './ai.js'
import { objectStoreFromEnv } from '@nektar/store'
import { runPreflight, WATCHDOG_CHECKS, type PreflightReport } from './preflight.js'
import { killSwitchActive } from './kill.js'
import { abandonedReap, queueRepairs, trackFanout } from './fanout.js'
import { openLanes } from './lanes.js'
import { openOutbox } from './outbox.js'
import { openUpkeep } from './upkeep.js'
import { Scheduler } from './scheduler.js'
import { openWatches } from './watches.js'
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
  const store = new AutomationStore(kx)
  await store.init()
  const key = mem.operatorKey()
  const wallet = mem.operatorWallet()
  const objectStore = cfg.objectStore ?? objectStoreFromEnv()
  const marketHosts = (cfg.marketHosts ?? []).map((u) => u.replace(/\/+$/, ''))
  const log = cfg.log ?? (() => {})
  // A9 — the readiness gate. Preflight runs BEFORE anything can spend:
  // strict refuses to open on a red check; non-strict opens anyway and seeds
  // the host-health set the lane readers maintain.
  const pfCfg = { network: cfg.network, dataDir: cfg.dataDir, marketHosts, boxOffice: cfg.boxOffice, unattended: cfg.unattended, now: cfg.now }
  const runChecks = (only?: string[]) =>
    runPreflight({ mem, store, objectStore, cfg: pfCfg, probes: { broadcaster: cfg.broadcasterProbe }, ...(only !== undefined ? { only } : {}) })
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
          const lanesExisted = cfg.dataDir === undefined || existsSync(lp)
          kxLanes = knexFactory({ client: 'sqlite3', connection: { filename: lp }, useNullAsDefault: true })
          // seed a fresh lanes.sqlite by copying lane-table ROWS — a file copy
          // would drag every automation table in and miss WAL commits
          if (!lanesExisted && existsSync(join(cfg.dataDir!, 'automation.sqlite')))
            await mod.migrateLaneTables(join(cfg.dataDir!, 'automation.sqlite'), kxLanes)
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
    ackOutbox: (id: string) => store.ackOutbox(id)
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
        await queueRepairs(fanoutIo, `finger:sub:${kind}:${id}`, r.missed.map((m) => m.host))
        return { commitment: r.commitment, txid: r.txid }
      }
    })
    mem.attachPacksStore(packsStore)
  }
  // identity records may name http and loopback/private hosts on the test
  // lanes, and anywhere the operator runs its own hosts and says so
  const localNet = cfg.network === 'regtest' || cfg.localHosts === true
  const me = key.toPublicKey().toString()
  // the shared context the regions meet through — door fields (resolveRecord,
  // poll, upkeep, ...) are assigned by the region openers and the deal loop
  // below; every call site reaches them lazily, so nothing can invoke a door
  // before assembly finishes
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
    inflight: new Set<Promise<void>>(),
    downNow,
    hotUntil: 0,
    busy: false,
    upkeepAt: 0,
    acking: false,
    pumping: false,
    pumpAgain: false,
    watchBases: (cfg.watchHosts ?? []).map((u) => u.replace(/\/+$/, '')),
    watches: new Map<string, AbortController>()
  } as unknown as Rt
  // the one timer wheel — deals/outbox/upkeep/watches/watchdog register
  // named jobs with their own ceilings; wakes (chain events, enqueues,
  // inbound items) pull a job to due-now instead of each job polling alone
  rt.scheduler = new Scheduler()
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
    },
    publishOffer: async (so: SignedOffer) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      // the offer goes where the BUYER's record says it keeps its artifacts —
      // this is also the seller's lazy resolution of the buyer
      const hosts = so.offer.buyerPub !== undefined ? await rt.hostsFor(so.offer.buyerPub) : marketHosts
      const r = await lanes.mod.publishArtifact({
        key,
        wallet,
        ledger: lanes.ledger,
        journal: lanes.journal,
        hosts,
        net: cfg.network,
        payload: { v: 1, kind: 'offer', body: so }
      })
      await trackFanout(r, fanoutIo)
      // the buyer's watch shim (if it advertises one) gets a wake hint — the
      // offer publish is only visible to its polls once they re-check anyway
      if (so.offer.buyerPub !== undefined) rt.notifyPeer(await rt.resolveRecord(so.offer.buyerPub), 'ls_nektar')
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
  const engine = new MarketEngine({
    store,
    call: cfg.call,
    memory: mem,
    key,
    network: cfg.network,
    now: cfg.now,
    settleDepth: cfg.settleDepth,
    height: () => mem.chainHeight(),
    tip: () => mem.tipRef(),
    fetchTx: (txid) => mem.fetchTx(txid),
    txFacts: (txid) => mem.txFacts(txid),
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
    transport,
    ai: cfg.ai ?? aiFromEnv(),
    killed: () => killSwitchActive(cfg.dataDir).killed,
    deaf: deafNow,
    opts: { ...cfg.opts, ...(cfg.minCollectMs !== undefined ? { minCollectMs: cfg.minCollectMs } : {}) },
    logStream
  })
  rt.engine = engine
  openOutbox(rt)
  openUpkeep(rt)
  openWatches(rt)
  let again = false
  let running: Promise<void> | undefined
  // Two loops. The deal loop reads the hosts and advances operations; it
  // does nothing else, so a deal never queues behind housekeeping. Upkeep
  // (funding trees, the identity record, the journal, evidence) runs beside
  // it under its own guard.
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
            await rt.poll()
            await engine.tick()
            rt.pushAcks()
            // join the detached drain before ending the pass — a pass isn't
            // complete while a delivery is in flight (cycle() semantics in
            // tests: one call = all queued work attempted)
            await rt.pumpDone
            // a live deal keeps the loop hot — the 5s inbound window alone
            // would let a deal cool to idle cadence between hops. A settled
            // proof awaiting depth only waits on BLOCKS, so it is excluded —
            // holding the loop hot for it would burn cycles per confirmation
            if (
              (await store.operations({ notStates: TERMINAL })).filter((o) => o.state !== 'completion_submitted' || o.data.provenHeight === undefined).length >
              0
            )
              rt.hotUntil = Date.now() + 5_000
            // new lanes appear as deals start — re-derive watch subs at the
            // end of a pass so a fresh want is subscribed within one cycle
            if (rt.watchBases.length > 0) void rt.refreshWatches().catch(() => undefined)
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
  /** one full pass: deals, then upkeep — what tests and push events drive */
  const cycle = async (): Promise<void> => {
    await deals()
    if (!rt.stopped) await rt.upkeep()
  }
  rt.cycle = cycle
  const upkeepMs = cfg.upkeepMs ?? 10_000
  // A4 — while the loop is hot (recent inbound activity) the deals cadence
  // floors at hotMs so an in-flight deal never waits a full interval for
  // the next hop; the ceiling fn re-reads hotUntil on every fire.
  const idleMs = cfg.intervalMs ?? 1000
  const hotMs = cfg.hotMs ?? 250
  if (cfg.intervalMs !== 0) {
    rt.scheduler!.add({ name: 'deals', run: () => void deals(), ceilingMs: () => (Date.now() < rt.hotUntil ? Math.min(hotMs, idleMs) : idleMs) })
    rt.scheduler!.add({ name: 'upkeep', run: () => rt.upkeep().catch(() => {}), ceilingMs: () => upkeepMs })
  }
  // A10 — the watchdog: the cheap probe tier (chain/broadcaster/hosts/kill
  // state) re-runs on a slow tick; host losses update the deaf flag the
  // engine consults at every new-deal entry point
  const watchdogMs = cfg.watchdogMs ?? 60_000
  if (watchdogMs > 0)
    rt.scheduler!.add({
      name: 'watchdog',
      run: () => {
        const work = (async () => {
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
            for (const h of [...downNow]) if (marketHosts.includes(h) && !r.downHosts.includes(h)) await rt.hostBack(h)
          } catch (e) {
            log(`automation: watchdog: ${(e as Error).message}`)
          }
        })()
        rt.inflight.add(work)
        void work.finally(() => {
          rt.inflight.delete(work)
        })
      },
      ceilingMs: () => watchdogMs
    })
  // watch queries derive from the engine's live watch-list — re-derive on a
  // slow tick plus after every deals pass so new wants appear in ≤1 cycle
  if (rt.watchBases.length !== 0) rt.scheduler!.add({ name: 'watches', run: () => rt.refreshWatches().catch(() => undefined), ceilingMs: () => 15_000 })
  void rt.refreshWatches().catch(() => undefined)
  // push-driven progress: a verified tx proof (broadcaster callback or the
  // chain's own mined event) may make a funding tree provable and advance
  // completion_submitted → settled — run a cycle now, not at the next interval
  mem.onTxProof((txid, mp) => {
    if (rt.stopped) return
    const work = (async () => {
      try {
        if (lanes !== undefined) {
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
        }
      } catch (e) {
        log(`automation: txproof: ${(e as Error).message}`)
      }
      engine.hint(txid)
      if (!rt.stopped) await cycle().catch((e) => log(`automation: ${(e as Error).message}`))
    })()
    rt.inflight.add(work)
    void work.finally(() => {
      rt.inflight.delete(work)
    })
  })
  // a rejection callback is only a hint: clear the reconcile throttle for the
  // affected operation and run a cycle — the engine consults txFacts itself
  mem.onTxRejected((txid: string) => {
    if (rt.stopped) return
    const work = (async () => {
      engine.hint(txid)
      if (!rt.stopped) await cycle().catch((e) => log(`automation: ${(e as Error).message}`))
    })()
    rt.inflight.add(work)
    void work.finally(() => {
      rt.inflight.delete(work)
    })
  })
  // the substrate's abandon horizon is the terminal verdict for a mint that
  // resubmitted but never mined — drop the row so the kind's mint gate
  // re-opens (same reap as a chain rejection, driven by the event)
  mem.onTxAbandoned((txid: string) => {
    if (rt.stopped || lanes === undefined) return
    const work = (async () => {
      try {
        const unproven = await lanes.ledger.unproven()
        const row = unproven.find((u) => u.txid === txid)
        if (row === undefined) return
        const tx = await mem.fetchTx(txid)
        await lanes.ledger.drop(txid)
        const reap = abandonedReap(row.kind)
        if (reap.ghost && tx !== undefined) rt.ghostMint(tx)
        log(reap.ghost ? `automation: dropped abandoned funding mint ${txid}` : `automation: dropped abandoned provision tx ${txid}`)
        await store.unblockOutbox('funding-slot')
        if (!rt.stopped) await cycle().catch((e) => log(`automation: ${(e as Error).message}`))
      } catch (e) {
        log(`automation: txAbandoned: ${(e as Error).message}`)
      }
    })()
    rt.inflight.add(work)
    void work.finally(() => {
      rt.inflight.delete(work)
    })
  })
  return {
    store,
    engine,
    cycle,
    logStream,
    dealJournal: rt.dealJournal,
    objectStore,
    /** the latest full preflight report — health surfaces read this, the
     *  watchdog keeps its cheap tier fresh between full runs */
    health: () => ({ pass: lastReport?.pass ?? false, report: lastReport }),
    bootstrap: () => (cfg.publishFinger === true && lanes !== undefined ? 'sealed' : 'unsealed'),
    /** re-run the full preflight now (automationDoctor) — refreshes the
     *  cached report and the seeded host-health set */
    preflight: async () => {
      lastReport = await runChecks()
      for (const h of lastReport.downHosts) downNow.add(h)
      return lastReport
    },
    marketSlots: async () =>
      lanes === undefined
        ? { market: 0, bbox: 0, log: 0, finger: 0 }
        : {
            market: await lanes.ledger.remaining('market'),
            bbox: await lanes.ledger.remaining('bbox'),
            log: await lanes.ledger.remaining('log'),
            finger: await lanes.ledger.remaining('finger')
          },
    laneStock: async (): Promise<Record<string, { proven: number; anchored: number; unprovenTrees: number }>> =>
      lanes === undefined
        ? {}
        : {
            market: await lanes.ledger.stock('market'),
            bbox: await lanes.ledger.stock('bbox'),
            log: await lanes.ledger.stock('log'),
            finger: await lanes.ledger.stock('finger')
          },
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
      if (rt.scheduler !== undefined) await rt.scheduler.stop()
      if (rt.outboxTimer) clearTimeout(rt.outboxTimer)
      for (const ac of rt.watches.values()) ac.abort()
      rt.watches.clear()
      await Promise.allSettled(rt.inflight)
      // drain in-flight work with a bound — a wedged producer must not hang
      // teardown forever (was an unbounded spin)
      const drainDeadline = Date.now() + 30_000
      while (rt.busy || rt.upkeepRun !== undefined || rt.acking || rt.pumping) {
        if (Date.now() >= drainDeadline) {
          log(
            `automation: stop: drain deadline hit (busy=${rt.busy} upkeep=${rt.upkeepRun !== undefined} acking=${rt.acking} pumping=${rt.pumping}) — proceeding`
          )
          break
        }
        await new Promise((r) => setTimeout(r, 25))
      }
      await engine.idle()
      await kxLanes?.destroy()
      await kx.destroy()
    }
  }
}
