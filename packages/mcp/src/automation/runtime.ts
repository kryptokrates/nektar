import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Knex } from 'knex'
import type { SignedWant, SignedOffer } from '@nektar/bstack'
import type { NektarMemory } from '../memory.js'
import { AutomationStore } from './store.js'
import { MarketEngine, type MarketMessage, type MarketTransport, type MarketOptions } from './market.js'
import { ScriptedExecutor, HttpChatExecutor, type AIExecutor } from './ai.js'
import type { Automation } from './types.js'

export type { Automation } from './types.js'

export interface AutomationConfig {
  network: string
  dataDir?: string
  ai?: AIExecutor
  intervalMs?: number
  call: (tool: string, args: any) => Promise<any>
  opts?: Partial<MarketOptions>
  log?: (line: string) => void
  marketHosts?: string[]
  messages?: { send: (toPub: string, msg: MarketMessage) => Promise<void>, collect: () => Promise<Array<{ from: string, msg: MarketMessage }>>, ack?: () => Promise<void> }
  treeSize?: number
  boxOffice?: string
  treeLowWater?: number
  /** blogs deal-journal stream id (`<name>_<10 chars>`); every deal event is
   *  appended as a signed numbered batch to tm_log_<stream> - env NEKTAR_LOG_STREAM */
  logStream?: string
  now?: () => number
}

function aiFromEnv (env = process.env): AIExecutor {
  const baseUrl = env.NEKTAR_AI_BASE_URL
  if (!baseUrl) return new ScriptedExecutor()
  if (!env.NEKTAR_AI_MODEL) throw new Error('NEKTAR_AI_BASE_URL is set but NEKTAR_AI_MODEL is not')
  const keyFile = env.NEKTAR_AI_API_KEY_FILE
  const apiKey = env.NEKTAR_AI_API_KEY ?? (keyFile && existsSync(keyFile) ? readFileSync(keyFile, 'utf8').trim() : undefined)
  const local = /^https?:\/\/(localhost|127\.0\.0\.1|\[::1\])(:|\/|$)/.test(baseUrl)
  const egress = env.NEKTAR_AI_EGRESS === 'local' || env.NEKTAR_AI_EGRESS === 'external' ? env.NEKTAR_AI_EGRESS : (local ? 'local' : 'external')
  return new HttpChatExecutor({ provider: env.NEKTAR_AI_PROVIDER ?? 'http', baseUrl, modelId: env.NEKTAR_AI_MODEL, apiKey, egress })
}

export async function openAutomation (mem: NektarMemory, cfg: AutomationConfig): Promise<Automation> {
  const knexFactory = (await import('knex')).default
  if (cfg.dataDir) mkdirSync(cfg.dataDir, { recursive: true })
  const kx: Knex = knexFactory({ client: 'sqlite3', connection: { filename: cfg.dataDir ? join(cfg.dataDir, 'automation.sqlite') : ':memory:' }, useNullAsDefault: true })
  const store = new AutomationStore(kx)
  await store.init()
  const key = mem.operatorKey()
  const marketHosts = (cfg.marketHosts ?? []).map(u => u.replace(/\/+$/, ''))
  const lanes = marketHosts.length === 0 ? undefined : await (async () => {
    const mod = await import('@nektar/bstack')
    return { mod, ledger: new mod.KnexTreeLedger(kx), journal: new mod.KnexCarrierJournal(kx) }
  })()
  const log = cfg.log ?? (() => {})
  const logStream = lanes === undefined ? undefined : (cfg.logStream ?? 'nektar_abcdefghij')
  const logState = lanes === undefined || logStream === undefined ? undefined : new lanes.mod.KnexLogState(kx)
  const fingerState = lanes === undefined ? undefined : new lanes.mod.KnexFingerState(kx)
  // bfinger counterparty resolution: an agent publishes {hosts, office, stream,
  // nets} signed under its identity; a counterparty resolves it lazily on the
  // first send and unions the record's hosts over its own configured set.
  // Positive resolutions cache 10 min, negatives 1 min. Resolved hosts are
  // accepted only when the record names this network, and screened through
  // fingerHostAllowed — https only off mock/regtest and never loopback,
  // private or link-local literals (the SSRF guard for dynamic hosts).
  const mockNet = cfg.network === 'mock' || cfg.network === 'regtest' || cfg.network.startsWith('mock')
  const fingerCache = new Map<string, { at: number, rec?: { hosts: string[], office: string, stream: string } }>()
  const resolveRecord = async (pub: string): Promise<{ hosts: string[], office: string, stream: string } | undefined> => {
    if (lanes === undefined) return undefined
    const t = cfg.now?.() ?? Date.now()
    const c = fingerCache.get(pub)
    if (c !== undefined && t - c.at < (c.rec === undefined ? 60_000 : 600_000)) return c.rec
    let rec: { hosts: string[], office: string, stream: string } | undefined
    try {
      const r = await lanes.mod.resolveFinger({ hosts: marketHosts, pub, tracker: await mem.sdkTracker(), allowHttp: mockNet, now: cfg.now })
      if (r !== undefined && (r.body.nets.length === 0 || r.body.nets.includes(cfg.network))) {
        const hosts = r.body.hosts.filter(u => lanes.mod.fingerHostAllowed(u, { allowHttp: mockNet, allowLocal: mockNet })).slice(0, 8)
        rec = { hosts, office: r.body.office, stream: r.body.stream }
      }
    } catch { /* no record (or no host serves one) — the configured set is the fallback */ }
    fingerCache.set(pub, { at: t, rec })
    return rec
  }
  const hostsFor = async (pub: string): Promise<string[]> =>
    [...new Set([...marketHosts, ...((await resolveRecord(pub))?.hosts ?? [])])]
  const transport: MarketTransport = {
    publishWant: async (sw: SignedWant) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await lanes.mod.publishArtifact({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, net: cfg.network, payload: { v: 1, kind: 'want', body: sw } })
    },
    publishOffer: async (so: SignedOffer) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      // the offer goes where the BUYER's record says it keeps its artifacts —
      // this is also the seller's lazy resolution of the buyer
      const hosts = so.offer.buyerPub !== undefined ? await hostsFor(so.offer.buyerPub) : marketHosts
      await lanes.mod.publishArtifact({ key, ledger: lanes.ledger, journal: lanes.journal, hosts, net: cfg.network, payload: { v: 1, kind: 'offer', body: so } })
    },
    send: async (to: string, msg: MarketMessage) => {
      if (msgs === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await msgs.send(to, msg)
    }
  }
  const engine = new MarketEngine({
    store, call: cfg.call, memory: mem as never, key, network: cfg.network,
    now: cfg.now,
    height: () => mem.chainHeight(), tip: () => mem.tipRef(), fetchTx: txid => mem.fetchTx(txid),
    txState: txid => mem.txState(txid),
    broadcast: tx => mem.broadcastTx(tx),
    settleEscrow: tx => { for (const inp of tx.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } if (tx.outputs[1]) mem.addOwnedOut(tx, 1) },
    transport, ai: cfg.ai ?? aiFromEnv(), opts: cfg.opts, logStream
  })
  const me = key.toPublicKey().toString()
  const withheldSeen = new Set<string>()
  const downNow = new Set<string>()
  const laneNotes = (r: { withheld: Array<{ host: string, txid: string }>, unreachable: string[] }): void => {
    for (const w of r.withheld) { const k2 = `${w.host}:${w.txid}`; if (!withheldSeen.has(k2)) { withheldSeen.add(k2); log(`automation: withheld ${w.host} ${w.txid}`) } }
    for (const h of r.unreachable) if (!downNow.has(h)) { downNow.add(h); log(`automation: unreachable ${h}`) }
    for (const h of [...downNow]) if (!r.unreachable.includes(h)) { downNow.delete(h); log(`automation: host back ${h}`) }
  }
  let pendingAcks: Array<{ txid: string, commitment: string, from: string, msg: MarketMessage | undefined, created: number }> = []
  let lastAckSkipped = -1
  const msgs = cfg.messages ?? (lanes === undefined ? undefined : {
    send: async (to: string, msg: MarketMessage): Promise<void> => {
      try {
        // recovery notes live as long as a deal can — everything else 24h
        const ttl = (msg as { type?: string }).type === 'note' || (msg as { type?: string }).type === 'sellerNote' ? 7 * 86400 : 86400
        // the envelope rides the RECIPIENT's office and hosts (its finger
        // record) — reading stays on our own office, configured set included
        const rec = await resolveRecord(to)
        await lanes.mod.sendBox({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: rec === undefined ? marketHosts : await hostsFor(to), office: rec?.office ?? cfg.boxOffice, to, body: { market: msg }, expiresAt: Math.floor(Date.now() / 1000) + ttl })
      } catch (e) { log(`automation: box: ${(e as Error).message}`); throw e }
    },
    collect: async (): Promise<Array<{ from: string, msg: MarketMessage }>> => {
      try {
        const tracker = await mem.sdkTracker()
        const r = await lanes.mod.readBox({ key, hosts: marketHosts, office: cfg.boxOffice, tracker })
        laneNotes(r)
        pendingAcks = r.items.map(i => ({ txid: i.txid, commitment: i.commitment, from: i.from, msg: (i.body as { market?: MarketMessage }).market, created: i.created }))
        // self-notes restore lost operations — ingest them before any other message in the batch
        const selfNote = (p: { from: string, msg?: MarketMessage }) => p.from === me && (p.msg?.type === 'note' || p.msg?.type === 'sellerNote')
        pendingAcks.sort((a, b) => Number(selfNote(b)) - Number(selfNote(a)))
        const out: Array<{ from: string, msg: MarketMessage }> = []
        for (const p of pendingAcks) if (p.msg !== undefined && typeof p.msg === 'object') out.push({ from: p.from, msg: p.msg })
        return out
      } catch (e) { log(`automation: box: ${(e as Error).message}`); return [] }
    },
    ack: async (): Promise<void> => {
      const items: Array<{ txid: string, commitment: string }> = []
      const held: typeof pendingAcks = []
      for (const p of pendingAcks) {
        // a recovery note from our own key stays listed until its deal is over
        if (p.from === me && p.msg?.type === 'note' && typeof (p.msg as { wantId?: unknown }).wantId === 'string' && !(await engine.noteSettled((p.msg as { wantId: string }).wantId))) { held.push(p); continue }
        if (p.from === me && p.msg?.type === 'sellerNote' && typeof (p.msg as { offerId?: unknown }).offerId === 'string' && !(await engine.noteSettled((p.msg as { offerId: string }).offerId, 'seller'))) { held.push(p); continue }
        // `delivered` is ingested to the inbox but processed on the next tick —
        // keep the envelope until the purchase settles so a database loss in
        // between does not lose the disclosure; junk with no matching purchase
        // is released once it is older than ten minutes
        if (p.msg?.type === 'delivered' && typeof (p.msg as { wantId?: unknown }).wantId === 'string') {
          const wantId = (p.msg as { wantId: string }).wantId
          if (!(await engine.noteSettled(wantId)) && ((await engine.hasPurchase(wantId)) || Date.now() / 1000 - p.created < 600)) { held.push(p); continue }
        }
        items.push({ txid: p.txid, commitment: p.commitment })
      }
      pendingAcks = held
      if (items.length === 0) return
      try {
        const r = await lanes.mod.ackBox({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, office: cfg.boxOffice, items })
        if (r.skipped > 0 && r.skipped !== lastAckSkipped) { lastAckSkipped = r.skipped; log(`automation: box ack: ${r.skipped} envelope(s) skipped — no bbox funding slot`) }
      } catch (e) { pendingAcks = pendingAcks.concat(items.map(i => ({ ...i, from: '', msg: undefined, created: 0 }))); log(`automation: box ack: ${(e as Error).message}`) }
    }
  })
  const lanePoll = async (): Promise<void> => {
    const { readWants, readOffers } = lanes!.mod
    const watch = await engine.watchList()
    const tracker = await mem.sdkTracker()
    const net = cfg.network
    for (const domain of watch.sellerDomains) {
      const r = await readWants(marketHosts, { net, domain }, { tracker, net, now: cfg.now?.() })
      const events = r.items.map(i => ({ seq: i.txid, kind: 'want', payload: i.payload.body }))
      if (events.length) await engine.ingestEvents(`lane:${net}:${domain}`, events, '')
      laneNotes(r)
    }
    for (const c of watch.collecting) {
      const r = await readOffers(marketHosts, { net, wantId: c.wantId, buyerPub: me }, { tracker, net, now: cfg.now?.() })
      const events = r.items.map(i => ({ seq: i.txid, kind: 'offer', payload: i.payload.body }))
      if (events.length) await engine.ingestEvents(`offers:${c.wantId}`, events, '')
      laneNotes(r)
    }
  }
  const poll = async (): Promise<void> => {
    if (lanes !== undefined) { try { await lanePoll() } catch (e) { log(`automation: lanes: ${(e as Error).message}`) } }
    if (msgs !== undefined) {
      let ingested = false
      try { for (const m of await msgs.collect()) await engine.ingestMessage(m.from, m.msg); ingested = true } catch (e) { log(`automation: messages: ${(e as Error).message}`) }
      if (ingested && msgs.ack !== undefined) { try { await msgs.ack() } catch (e) { log(`automation: box ack: ${(e as Error).message}`) } }
    }
  }
  let busy = false
  let again = false
  let running: Promise<void> | undefined
  const cycle = async (): Promise<void> => {
    if (busy) { again = true; await running; return }
    busy = true
    const run = (async () => {
      try {
        do {
          again = false
          await runCycle()
        } while (again && !stopped)
      } finally { busy = false; if (again && !stopped) void cycle() }
    })()
    running = run
    await run
  }
  const runCycle = async (): Promise<void> => {
    try {
      if (lanes !== undefined) {
        try {
          for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger'] as const) {
            const r = await lanes.mod.ensureTrees({
              key, ledger: lanes.ledger, kind,
              lowWater: kind === 'finger' ? 1 : cfg.treeLowWater,
              size: kind === 'finger' ? 4 : cfg.treeSize,
              satsPerOutput: kind === 'log' || kind === 'finger' ? 100 : undefined,
              lock: kind === 'bbox' ? await lanes.mod.boxFundingLock(key) : kind === 'log' ? await lanes.mod.logFundingLock(key) : kind === 'finger' ? await lanes.mod.fingerFundingLock(key) : undefined,
              takeFunding: async (sats: number) => (await mem.fundingOuts(sats))[0]!,
              broadcast: tx => mem.broadcastTx(tx),
              fetchTx: txid => mem.fetchTx(txid),
              onSpent: (tree) => { for (const inp of tree.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } const n = cfg.treeSize ?? 16; if (tree.outputs.length === n + 1 && (tree.outputs[n]!.satoshis ?? 0) > 0) mem.addOwnedOut(tree, n) }
            })
            // a proven log tree's BEEF must reach tm_log before hosts can
            // admit carriers spending it - journaled, publishes once
            for (const txid of r.proven) {
              const tx = await mem.fetchTx(txid)
              if (tx !== undefined && logStream !== undefined) await lanes.mod.publishMinedLog({ journal: lanes.journal, hosts: marketHosts, stream: logStream, tx })
            }
          }
        } catch (e) { log(`automation: ensureTrees: ${(e as Error).message}`) }
      }
      // own bfinger record: (re)publishes on the current host list, no-ops
      // when nothing changed, resubmits to any host that missed the objects
      if (lanes !== undefined && fingerState !== undefined && logStream !== undefined) {
        try {
          const r = await lanes.mod.publishFinger({
            key, state: fingerState, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts,
            body: { v: 1, hosts: marketHosts, office: cfg.boxOffice ?? lanes.mod.officeFor(), stream: logStream, nets: [cfg.network] },
            takeFunding: async (sats: number) => (await mem.fundingOuts(sats))[0]!,
            broadcast: tx => mem.broadcastTx(tx), fetchTx: txid => mem.fetchTx(txid),
            allowHttp: mockNet, now: cfg.now,
            onSpent: (tx) => { for (const inp of tx.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } }
          })
          if (r.changed) log(`automation: finger seq ${r.seq} ${r.tokenTxid ?? ''}`)
        } catch (e) {
          // 'no funding slot' is transient while the finger tree matures — retried next cycle, not worth a line
          if ((e as Error).message.includes('no funding slot')) return
          const m = `automation: finger: ${(e as Error).message.slice(0, 160)}`; if (m !== fingerError) { fingerError = m; log(m) }
        }
      }
      await poll(); await engine.tick()
      await appendLogEvents()
    } catch (e) { log(`automation: ${(e as Error).message}`) }
  }
  // blogs deal journal: every market event becomes one line in one batch
  const EV_CURSOR = 'log#events'
  let logError = ''
  let fingerError = ''
  const wantIdOf = (op: { data: Record<string, unknown> }): string | null =>
    (op.data.want as SignedWant | undefined)?.want?.wantId ?? null
  const appendLogEvents = async (): Promise<void> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return
    const cursor = (await logState.get(EV_CURSOR))?.seq ?? 0
    const rows = await store.eventsAfter(cursor, 500)
    if (rows.length === 0) { await maybeAnchor(); return }
    const { sha256, utf8, toHex } = await import('@nektar/core')
    const ns = new Map<string, number>()
    const lines: string[] = []
    let lastSeq = cursor
    for (const r of rows) {
      const op = await store.operation(r.operationId)
      if (op === undefined) { lastSeq = r.seq; continue }
      const n = (ns.get(op.operationId) ?? await store.eventCount(op.operationId, cursor)) + 1
      ns.set(op.operationId, n)
      const ev = r.evidence === undefined ? null : toHex(Uint8Array.from(sha256(utf8(lanes.mod.marketJson(r.evidence)))))
      lines.push(JSON.stringify({ v: 1, op: op.operationId, want: wantIdOf(op), role: op.role, n, state: r.to, cause: r.cause, t: r.at, ev }))
      lastSeq = r.seq
    }
    try {
      if (lines.length > 0) await lanes.mod.appendLog({ key, ledger: lanes.ledger, journal: lanes.journal, state: logState, hosts: marketHosts, stream: logStream, lines, now: cfg.now })
      // the cursor moves only after the batch reached quorum: a crash between
      // tick and append replays the same lines on the next cycle, and the
      // journal keeps the resubmitted carrier identical (same txid, same seq)
      await logState.set(EV_CURSOR, { seq: lastSeq, prev: '' })
      logError = ''
      await maybeAnchor()
    } catch (e) {
      const m = (e as Error).message
      if (m !== logError) { logError = m; log(`automation: log append: ${m.slice(0, 200)}`) }
    }
  }
  // anchors never block a cycle: they run on the inflight set like pushed work
  const maybeAnchor = async (): Promise<void> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return
    const work = (async () => {
      try {
        const r = await lanes.mod.anchorLog({
          key, ledger: lanes.ledger, journal: lanes.journal, state: logState, hosts: marketHosts, stream: logStream,
          broadcast: tx => mem.broadcastTx(tx), fetchTx: txid => mem.fetchTx(txid), now: cfg.now,
          log: line => log(`automation: ${line}`)
        })
        if (r !== undefined) log(`automation: log anchor interval ${r.interval} ${r.txid}`)
      } catch (e) { log(`automation: log anchor: ${(e as Error).message.slice(0, 200)}`) }
    })()
    inflight.add(work)
    void work.finally(() => { inflight.delete(work) })
  }

  // deal journal view for `deals { operationId }`: own stream head + the
  // counterparty's stream filtered to this deal. The counterparty's stream
  // id rides its signed artifact (want.journal / offer.journal).
  const dealJournal = async (op: import('./store.js').Operation): Promise<Record<string, unknown> | undefined> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return undefined
    const own = { stream: logStream, lastSeq: (await logState.get(logStream))?.seq ?? 0 }
    const cp = op.role === 'buyer'
      ? (() => {
          const sel = (op.data.selected as { key?: string } | undefined)?.key
          const so = sel !== undefined ? (op.data.offers as Record<string, { so: SignedOffer }> | undefined)?.[sel]?.so : undefined
          return so?.offer.journal?.stream !== undefined && sel !== undefined ? { stream: so.offer.journal.stream, producer: sel.split(':')[0]! } : undefined
        })()
      : (() => {
          const w = op.data.want as SignedWant | undefined
          return w?.want.journal?.stream !== undefined ? { stream: w.want.journal.stream, producer: w.want.pub } : undefined
        })()
    if (cp === undefined) return { own }
    try {
      // the counterparty's journal lives on ITS hosts and stream: the finger
      // record wins over the artifact's journal.stream hint when present
      const rec = await resolveRecord(cp.producer)
      const r = await lanes.mod.readLog({ hosts: await hostsFor(cp.producer), stream: rec?.stream ?? cp.stream, producer: cp.producer, tracker: await mem.sdkTracker() })
      const wantId = wantIdOf(op)
      const entries: Array<{ seq: number, n: number, state: string, cause: string, t: number }> = []
      for (const b of r.batches) for (const l of b.lines) {
        try {
          const j = JSON.parse(l) as { want?: string | null, op?: string, n?: number, state?: string, cause?: string, t?: number }
          if (j.want === wantId && typeof j.n === 'number') entries.push({ seq: b.seq, n: j.n, state: j.state ?? '', cause: j.cause ?? '', t: j.t ?? 0 })
        } catch { /* not a journal line */ }
      }
      const base = { stream: cp.stream, producer: cp.producer, gaps: r.gaps, anchoredThrough: r.anchoredThrough, entries }
      if (entries.length === 0) return { own, counterparty: { ...base, complete: false } }
      const lo = Math.min(...entries.map(e => e.seq))
      const hi = Math.max(...entries.map(e => e.seq))
      const complete = !r.gaps.some(g => g.to >= lo && g.from <= hi) && entries.every((e, i) => e.n === i + 1)
      return { own, counterparty: { ...base, complete } }
    } catch (e) { return { own, counterparty: { error: (e as Error).message.slice(0, 200) } } }
  }

  const timer = cfg.intervalMs === 0 ? undefined : setInterval(() => { void cycle() }, cfg.intervalMs ?? 5000)
  timer?.unref()
  let stopped = false
  const inflight = new Set<Promise<void>>()
  // push-driven progress: a verified tx proof (broadcaster callback or the
  // chain's own mined event) may make a funding tree provable and advance
  // completion_submitted → settled — run a cycle now, not at the next interval
  mem.onTxProof((txid, mp) => {
    if (stopped) return
    const work = (async () => {
      try {
        if (lanes !== undefined) {
          const unproven = await lanes.ledger.unproven()
          if (stopped) return
          if (unproven.some(u => u.txid === txid)) await lanes.ledger.markProven(txid, mp)
        }
      } catch (e) { log(`automation: txproof: ${(e as Error).message}`) }
      engine.hint(txid)
      if (!stopped) await cycle().catch(e => log(`automation: ${(e as Error).message}`))
    })()
    inflight.add(work)
    void work.finally(() => { inflight.delete(work) })
  })
  // a rejection callback is only a hint: clear the reconcile throttle for the
  // affected operation and run a cycle — the engine consults txState itself
  mem.onTxRejected((txid: string) => {
    if (stopped) return
    const work = (async () => {
      engine.hint(txid)
      if (!stopped) await cycle().catch(e => log(`automation: ${(e as Error).message}`))
    })()
    inflight.add(work)
    void work.finally(() => { inflight.delete(work) })
  })
  return {
    store, engine, cycle, logStream, dealJournal,
    marketSlots: async () => lanes === undefined ? { market: 0, bbox: 0, log: 0, finger: 0 } : { market: await lanes.ledger.remaining('market'), bbox: await lanes.ledger.remaining('bbox'), log: await lanes.ledger.remaining('log'), finger: await lanes.ledger.remaining('finger') },
    stop: async () => {
      stopped = true
      if (timer) clearInterval(timer)
      await Promise.allSettled([...inflight])
      while (busy) await new Promise(r => setTimeout(r, 25))
      await engine.idle()
      await kx.destroy()
    }
  }
}
