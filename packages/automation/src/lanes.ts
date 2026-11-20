// lanes.ts — the lane layer: bfinger counterparty resolution + caching, the
// inbox msgs surface (sendBox/readBox/ackBox), host-health notes, the
// republish queue, and the want/offer/inbox poll passes.
import { BoundedMap, BoundedSet } from '@nektar/core'
import { WAITS } from './waits.js'
import { requeueAckItems, trackFanout } from './fanout.js'
import type { MarketMessage } from './market.js'
import type { FingerRec, Rt } from './rtctx.js'

export function openLanes(rt: Rt): void {
  const { cfg, mem, log, store } = rt
  const lanes = rt.lanes
  const marketHosts = rt.marketHosts
  const key = rt.key
  const wallet = rt.wallet
  // verified-carrier memos for the runtime's life: identical carrier bytes on
  // the next poll skip SPV/parse/signature (lanes) and admit+decrypt (box);
  // only positives are cached — a failed check always retries
  const laneCache =
    lanes === undefined ? undefined : new lanes.mod.VerifiedCache<{ txid: string; payload: import('@nektar/bstack').MarketPayload; lockingKey: string }>()
  const boxCache = lanes === undefined ? undefined : new lanes.mod.VerifiedCache<import('@nektar/bstack').BoxItem & { expires: number }>()
  // bfinger counterparty resolution: an agent publishes {hosts, office, stream,
  // nets} signed under its identity; a counterparty resolves it lazily on the
  // first send and unions the record's hosts over its own configured set.
  // Positive resolutions cache 10 min, negatives 1 min. Resolved hosts are
  // accepted only when the record names this network, and screened through
  // fingerHostAllowed — https only off regtest/local rigs and never loopback,
  // private or link-local literals (the SSRF guard for dynamic hosts).
  // identity records may name http and loopback/private hosts on the test
  // lanes, and anywhere the operator runs its own hosts and says so
  const fingerCache = new BoundedMap<string, { at: number; rec?: FingerRec }>(4096)
  const resolveRecord = async (pub: string, fresh = false): Promise<FingerRec | undefined> => {
    if (lanes === undefined) return undefined
    const t = cfg.now?.() ?? Date.now()
    const c = fingerCache.get(pub)
    // fresh callers (ack retries on deferred envelopes) bypass a cached miss —
    // a stale negative entry would otherwise strand receipts for a minute
    if (c !== undefined && t - c.at < (c.rec === undefined ? (fresh ? 0 : WAITS.fingerNeg.ms) : WAITS.fingerPos.ms)) return c.rec
    let rec: FingerRec | undefined
    try {
      const r = await lanes.mod.resolveFinger({ hosts: marketHosts, pub, tracker: mem.sdkTracker(), allowHttp: rt.localNet, now: cfg.now, timeoutMs: WAITS.resolveFingerIo.ms })
      if (r !== undefined && (r.body.nets.length === 0 || r.body.nets.includes(cfg.network))) {
        const hosts = r.body.hosts.filter((u) => lanes.mod.fingerHostAllowed(u, { allowHttp: rt.localNet, allowLocal: rt.localNet })).slice(0, 8)
        const push =
          r.body.push !== undefined && lanes.mod.fingerHostAllowed(r.body.push, { allowHttp: rt.localNet, allowLocal: rt.localNet }) ? r.body.push : undefined
        const watch =
          r.body.watch !== undefined && lanes.mod.fingerHostAllowed(r.body.watch, { allowHttp: rt.localNet, allowLocal: rt.localNet })
            ? r.body.watch
            : undefined
        rec = { hosts, office: r.body.office, stream: r.body.stream, ...(watch !== undefined ? { watch } : {}), ...(push !== undefined ? { push } : {}) }
      }
    } catch {
      /* no record (or no host serves one) — the configured set is the fallback */
    }
    fingerCache.set(pub, { at: t, rec })
    // event-driven deferred acks: an envelope deferred on an unresolved
    // sender record receipt-waits the next ack pass — the resolution IS the
    // event it was waiting on, so fire the pass now instead of on the tick
    if (rec !== undefined && deferredAcks.some((d) => d.from === pub)) void rt.msgs?.ack?.().catch(() => undefined)
    return rec
  }
  // all senders of a batch resolved together — N counterparty lookups cost
  // ~1 host RTT instead of N×(up to 5s) serial; each leg keeps its own
  // timeout + the shared cache
  const resolveRecords = async (pubs: string[], freshFor?: (pub: string) => boolean): Promise<Map<string, FingerRec | undefined>> =>
    new Map(await Promise.all(pubs.map(async (p) => [p, await resolveRecord(p, freshFor?.(p) ?? false).catch(() => undefined)] as const)))
  const hostsFor = async (pub: string): Promise<string[]> => [...new Set([...marketHosts, ...((await resolveRecord(pub))?.hosts ?? [])])]
  // a counterparty's advertised watch shim: POST {service,query} → the shim
  // wakes their long-polls early. Best-effort only — the shim re-verifies
  // real state and our own polling covers a miss, so it's fire-and-forget
  // the runtime umbrella reaches fetch-level IO: the leg's bound signal
  // (rt.activeSignal) or the runtime's (rt.signal) cancels an in-flight
  // call instead of the fetch outliving the leg that asked for it — one
  // operation deadline shared inward, not a fresh budget per hop
  const io = (ms: number): AbortSignal => {
    const own = AbortSignal.timeout(ms)
    const up = rt.activeSignal ?? rt.signal
    return up !== undefined ? AbortSignal.any([up, own]) : own
  }
  const notifyPeer = (rec: { watch?: string } | undefined, service: string, query?: unknown): void => {
    if (rec?.watch === undefined) return
    fetch(`${rec.watch.replace(/\/+$/, '')}/notify`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify(query === undefined ? { service } : { service, query }),
      signal: io(WAITS.pushIo.ms),
      redirect: 'error' // a redirect into an unscreened host is still an egress bypass
    }).catch(() => undefined)
  }
  // direct push: POST the journaled carrier BEEF to the recipient's own
  // /v1/lane — the same bytes the box/artifact submit carries, verified by
  // the same code on the far side (P2P for the present peer; the host copy
  // stays the store-and-forward fallback). Fire-and-forget — any failure
  // just means delivery happens on the ordinary read.
  const pushCarrier = (rec: { push?: string } | undefined, beef: number[]): void => {
    if (rec?.push === undefined || beef.length === 0) return
    fetch(rec.push, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ beef: Buffer.from(beef).toString('hex') }),
      signal: io(WAITS.pushIo.ms)
    })
      .then((r) => {
        if (!r.ok) log(`automation: push → ${rec.push}: ${r.status}`)
      })
      .catch(() => undefined)
  }
  rt.pushCarrier = pushCarrier
  // a carrier pushed straight to us by its sender — verify + ingest exactly
  // like the box/lane read it precedes (the durable copy still travels the
  // hosts and dedupes by artifactHash/seq on arrival)
  rt.pushIngest = async (beef: number[]): Promise<'envelope' | 'artifact' | false> => {
    if (lanes === undefined || beef.length === 0 || beef.length > 128 * 1024) return false
    const tracker = mem.sdkTracker()
    const w = lanes.mod.walletOrProto(key, wallet)
    const office = cfg.boxOffice ?? lanes.mod.officeFor()
    // sealed box envelope — the same admit/decrypt/verify as readBox's
    // openEnvelope (wrong-recipient pushes drop at the `to` check inside)
    const env = await lanes.mod.openEnvelope(beef, w, office, rt.me, tracker).catch(() => undefined)
    if (env !== undefined) {
      const msg = (env.body as { market?: MarketMessage }).market
      if (msg !== undefined && typeof msg === 'object') {
        rt.hotUntil = Date.now() + WAITS.hotWindow.ms
        await rt.engine.ingestMessage(env.from, msg, env.refs)
        log(`automation: push: ingested envelope ${env.txid.slice(0, 12)} from ${env.from.slice(0, 12)}`)
        rt.countWake('push:envelope')
        void rt.deals().catch((e) => log(`automation: push: ${(e as Error).message}`))
        return 'envelope'
      }
    }
    // ls_nektar artifact (offer) — the same checkCarrier + payload/body/
    // locking-key verification readOffers runs, then the same ingestEvents
    const chk = await lanes.mod.checkCarrier(beef, { tracker }).catch(() => undefined)
    if (chk === undefined || !chk.ok) return false
    try {
      const p = lanes.mod.decodePayload(new Uint8Array(chk.payload))
      const body = p.body as { offer?: { wantId?: string; buyerPub?: string; net?: string } }
      if (
        p.kind === 'offer' &&
        lanes.mod.verifyBody(p, cfg.now?.()) &&
        lanes.mod.carrierLockingKey(lanes.mod.bodyPub(p)) === chk.lockingKey &&
        body.offer?.buyerPub === rt.me &&
        body.offer.net === cfg.network &&
        typeof body.offer.wantId === 'string'
      ) {
        rt.hotUntil = Date.now() + WAITS.hotWindow.ms
        await rt.engine.ingestEvents(`offers:${body.offer.wantId}`, [{ seq: chk.carrier.id('hex'), kind: 'offer', payload: p.body }], '')
        log(`automation: push: ingested offer ${chk.carrier.id('hex').slice(0, 12)}`)
        rt.countWake('push:offer')
        void rt.deals().catch((e) => log(`automation: push: ${(e as Error).message}`))
        return 'artifact'
      }
    } catch {
      /* undecodable — false below */
    }
    return false
  }
  // a proven funding tree queues one republish row per journaled carrier it
  // anchors — each row's transport call attaches the BUMP, re-submits and
  // re-journals, so a dead host retries that leg alone instead of the whole
  // tree's fan-out
  const queueRepublish = async (treeTxid: string): Promise<void> => {
    for (const row of (await lanes?.journal.fundedBy?.(treeTxid)) ?? [])
      await store.enqueue({
        actionId: `reprove:${treeTxid}:${row.hash}`,
        operationId: 'lanes',
        kind: 'republish-proved',
        destination: 'hosts',
        payload: { treeTxid, hash: row.hash }
      })
  }
  const withheldSeen = new BoundedSet<string>(8192)
  // a host's recovery frees every transport-deferred outbox item — the
  // backoff was waiting on reachability, not delivery logic, so pull it
  // forward instead of waiting it out. One helper: the watchdog's recovery
  // branch and laneNotes take the same path
  const hostBack = async (h: string): Promise<void> => {
    rt.downNow.delete(h)
    log(`automation: host back ${h}`)
    const n = await store.unblockOutbox('transport')
    if (n > 0) log(`automation: outbox unblocked ×${n} (transport)`)
  }
  const laneNotes = (r: { withheld: Array<{ host: string; txid: string }>; unreachable: string[] }, answered: string[]): void => {
    for (const w of r.withheld) {
      const k2 = `${w.host}:${w.txid}`
      if (!withheldSeen.has(k2)) {
        withheldSeen.add(k2)
        log(`automation: withheld ${w.host} ${w.txid}`)
      }
    }
    for (const h of r.unreachable)
      if (!rt.downNow.has(h)) {
        rt.downNow.add(h)
        log(`automation: unreachable ${h}`)
      }
    // "back" means THIS READ got an answer — a quorum-short read that skipped
    // a host says nothing about it, so it stays down until a read reaches it
    for (const h of [...rt.downNow]) if (answered.includes(h)) void hostBack(h)
  }
  let pendingAcks: Array<{
    txid: string
    commitment: string
    from: string
    msg: MarketMessage | undefined
    refs?: import('@nektar/bstack').BoxRef[]
    created: number
  }> = []
  // envelopes pushed for ack whose sender's finger record hadn't resolved —
  // they leave pendingAcks once our own hosts drop their copy, but the copies
  // fanned out to the sender's hosts still need a receipt, so they retry here
  // until the record resolves (bounded, same window as the delivered rule)
  let deferredAcks: Array<{ txid: string; commitment: string; from: string; until: number }> = []
  let lastAckSkipped = -1
  rt.msgs =
    cfg.messages ??
    (lanes === undefined
      ? undefined
      : {
          send: async (to: string, msg: MarketMessage, refs?: import('@nektar/bstack').BoxRef[]): Promise<void> => {
            try {
              // recovery notes live as long as a deal can — everything else 24h
              const ttl = (msg as { type?: string }).type === 'note' || (msg as { type?: string }).type === 'sellerNote' ? 7 * 86400 : 86400
              // the envelope rides the RECIPIENT's office and hosts (its finger
              // record) — reading stays on our own office, configured set included
              const rec = await resolveRecord(to)
              const office = rec?.office ?? cfg.boxOffice
              const r = await lanes.mod.sendBox({
                key,
                wallet,
                ledger: lanes.ledger,
                journal: lanes.journal,
                hosts: rec === undefined ? marketHosts : await hostsFor(to),
                office,
                to,
                body: { market: msg },
                refs,
                expiresAt: Math.floor(Date.now() / 1000) + ttl,
                // the fast leg fires the moment the carrier exists — the push
                // and the durable host fanout race each other, either is fine
                ...(rec?.push !== undefined ? { push: (beef: number[]) => pushCarrier(rec, beef) } : {})
              })
              await trackFanout(r, rt.fanoutIo)
              // wake the recipient's box watch with the exact query their
              // shim holds — ls_bbox {office,to} is the subscription's shape
              notifyPeer(rec, 'ls_bbox', { office: office ?? lanes.mod.officeFor(), to })
              // the recipient advertises no shim → shared-infra fallback:
              // their watcher may be co-hosted on a shim we already poll
              if (rec?.watch === undefined)
                for (const base of rt.watchBases) notifyPeer({ watch: base }, 'ls_bbox', { office: office ?? lanes.mod.officeFor(), to })
            } catch (e) {
              log(`automation: box: ${(e as Error).message}`)
              throw e
            }
          },
          collect: async (): Promise<Array<{ from: string; msg: MarketMessage; refs?: import('@nektar/bstack').BoxRef[] }>> => {
            try {
              const tracker = mem.sdkTracker()
              const listAcks = (items: Awaited<ReturnType<typeof lanes.mod.readBox>>['items']) =>
                items.map((i) => ({
                  txid: i.txid,
                  commitment: i.commitment,
                  from: i.from,
                  msg: (i.body as { market?: MarketMessage }).market,
                  refs: i.refs,
                  created: i.created
                }))
              // quorum-1 read first: envelopes are content-addressed, so the
              // fastest host's answer delivers the items — a single slow or
              // dead host no longer gates message delivery by its timeout.
              // The full fan-out runs right behind it for completeness: an
              // envelope a missed submit left off that host still arrives
              // this cycle, and withheld/unreachable bookkeeping stays whole.
              const r =
                marketHosts.length > 1
                  ? await (async () => {
                      const fast = await lanes.mod.readBox({ key, hosts: marketHosts, office: cfg.boxOffice, tracker, wallet, quorum: 1, cache: boxCache })
                      void lanes.mod
                        .readBox({ key, hosts: marketHosts, office: cfg.boxOffice, tracker, wallet, cache: boxCache })
                        .then((full) => {
                          laneNotes(full, full.answered)
                          pendingAcks = listAcks(full.items)
                        })
                        .catch(() => undefined)
                      return fast
                    })()
                  : await lanes.mod.readBox({ key, hosts: marketHosts, office: cfg.boxOffice, tracker, wallet, cache: boxCache })
              laneNotes(r, r.answered)
              pendingAcks = listAcks(r.items)
              // self-notes restore lost operations — ingest them before any other message in the batch
              const selfNote = (p: { from: string; msg?: MarketMessage }) => p.from === rt.me && (p.msg?.type === 'note' || p.msg?.type === 'sellerNote')
              pendingAcks.sort((a, b) => Number(selfNote(b)) - Number(selfNote(a)))
              const out: Array<{ from: string; msg: MarketMessage; refs?: import('@nektar/bstack').BoxRef[] }> = []
              for (const p of pendingAcks) if (p.msg !== undefined && typeof p.msg === 'object') out.push({ from: p.from, msg: p.msg, refs: p.refs })
              return out
            } catch (e) {
              log(`automation: box: ${(e as Error).message}`)
              return []
            }
          },
          ack: async (): Promise<void> => {
            const items: Array<{ txid: string; commitment: string; from: string }> = []
            const held: typeof pendingAcks = []
            for (const p of pendingAcks) {
              // a recovery note from our own key stays listed until its deal is over
              if (
                p.from === rt.me &&
                p.msg?.type === 'note' &&
                typeof (p.msg as { wantId?: unknown }).wantId === 'string' &&
                !(await rt.engine.noteSettled((p.msg as { wantId: string }).wantId))
              ) {
                held.push(p)
                continue
              }
              if (
                p.from === rt.me &&
                p.msg?.type === 'sellerNote' &&
                typeof (p.msg as { offerId?: unknown }).offerId === 'string' &&
                !(await rt.engine.noteSettled((p.msg as { offerId: string }).offerId, 'seller'))
              ) {
                held.push(p)
                continue
              }
              // `accept` funds a quoted sale — keep the envelope until the
              // sellerNote settles so a database loss after processing does
              // not leave the rebuilt 'quoted' sale with nothing to ingest;
              // junk with no matching sale is released once it is old
              if (p.msg?.type === 'accept' && typeof (p.msg as { offerId?: unknown }).offerId === 'string') {
                const offerId = (p.msg as { offerId: string }).offerId
                if (
                  !(await rt.engine.noteSettled(offerId, 'seller')) &&
                  ((await rt.engine.hasSale(offerId)) || Math.max(0, Date.now() / 1000 - p.created) < 600)
                ) {
                  held.push(p)
                  continue
                }
              }
              // `delivered` is ingested to the inbox but processed on the next tick —
              // keep the envelope until the purchase settles so a database loss in
              // between does not lose the disclosure; junk with no matching purchase
              // is released once it is older than ten minutes
              if (p.msg?.type === 'delivered' && typeof (p.msg as { wantId?: unknown }).wantId === 'string') {
                const wantId = (p.msg as { wantId: string }).wantId
                if (!(await rt.engine.noteSettled(wantId)) && ((await rt.engine.hasPurchase(wantId)) || Math.max(0, Date.now() / 1000 - p.created) < 600)) {
                  held.push(p)
                  continue
                }
              }
              items.push({ txid: p.txid, commitment: p.commitment, from: p.from })
            }
            pendingAcks = held
            // re-merge still-valid deferred items — they retry until their
            // sender's record resolves (or their window lapses)
            const now = Math.floor(Date.now() / 1000)
            const lapsed = deferredAcks.filter((d) => d.until <= now).length
            if (lapsed > 0) log(`automation: box ack: ${lapsed} deferred receipt(s) expired unsent — sender record never resolved`)
            deferredAcks = deferredAcks.filter((d) => d.until > now)
            for (const d of deferredAcks) {
              if (!items.some((i) => i.txid === d.txid && i.commitment === d.commitment)) items.push(d)
            }
            if (items.length === 0) return
            try {
              // senders fan envelopes out to their own hosts ∪ our record's hosts —
              // the receipt has to reach every copy, not only the ones we read.
              // A sender whose record hasn't resolved yet only narrows this pass's
              // host set — its envelopes are re-deferred so a later pass receipts
              // the copies we never read instead of stranding them.
              const ackHosts = new Set(marketHosts)
              const priorUntil = new Map(deferredAcks.map((d) => [`${d.txid}:${d.commitment}`, d.until]))
              const retriedFrom = new Set(deferredAcks.map((d) => d.from))
              const retry: typeof deferredAcks = []
              const froms = [...new Set(items.map((i) => i.from).filter((f) => f !== rt.me))]
              const resolved = await resolveRecords(froms, (f) => retriedFrom.has(f))
              for (const f of froms) {
                const rec = resolved.get(f)
                if (rec !== undefined) {
                  for (const h of rec.hosts) ackHosts.add(h)
                  continue
                }
                for (const i of items)
                  if (i.from === f)
                    retry.push({ txid: i.txid, commitment: i.commitment, from: i.from, until: priorUntil.get(`${i.txid}:${i.commitment}`) ?? now + 600 })
              }
              deferredAcks = retry
              const r = await lanes.mod.ackBox({
                key,
                wallet,
                ledger: lanes.ledger,
                journal: lanes.journal,
                hosts: [...ackHosts],
                office: cfg.boxOffice,
                items
              })
              if (r.skipped > 0 && r.skipped !== lastAckSkipped) {
                lastAckSkipped = r.skipped
                log(`automation: box ack: ${r.skipped} envelope(s) skipped — no bbox funding slot`)
              }
            } catch (e) {
              pendingAcks = pendingAcks.concat(requeueAckItems(items))
              log(`automation: box ack: ${(e as Error).message}`)
            }
          }
        })
  // items a previous pass's background full read delivered after the fast
  // quorum-1 answer — drained at the top of the next pass so a slow host's
  // exclusive items still arrive without ever gating this pass on it
  const pendingLane = new Map<string, Array<{ seq: string; kind: 'want' | 'offer'; payload: unknown }>>()
  let lastSubSig = ''
  const lanePoll = async (): Promise<void> => {
    const { readWants, readOffers } = lanes!.mod
    const watch = await rt.engine.watchList()
    const tracker = mem.sdkTracker()
    const net = cfg.network
    const timeoutMs = cfg.laneReadTimeoutMs ?? WAITS.laneReadIo.ms
    // two-tier read, same shape as the inbox collect: quorum-1 returns the
    // fastest host's view so one slow host never gates the deal; the full
    // fan-out lands behind it for withheld/unreachable bookkeeping and for
    // items that only exist on the slow copy (merged next pass — store.ingest
    // dedups by seq so double-delivery is free)
    type ReadOpts = { timeoutMs: number; quorum?: number }
    const subs = [
      ...watch.sellerDomains.map((domain) => ({
        tag: `lane:${net}:${domain}`,
        kind: 'want' as const,
        call: (o: ReadOpts) => readWants(marketHosts, { net, domain }, { tracker, net, now: cfg.now?.(), cache: laneCache, ...o })
      })),
      ...watch.collecting.map((c) => ({
        tag: `offers:${c.wantId}`,
        kind: 'offer' as const,
        call: (o: ReadOpts) => readOffers(marketHosts, { net, wantId: c.wantId, buyerPub: rt.me }, { tracker, net, now: cfg.now?.(), cache: laneCache, ...o })
      }))
    ]
    // the watch set changes as sellers enable and wants collect — log the
    // transition once so a missed domain is visible without per-pass spam
    const subSig = subs.map((s) => s.tag).join(',')
    if (subSig !== lastSubSig) {
      lastSubSig = subSig
      log(`lanes: watching ${subs.length} subs ${subs.length <= 4 ? `(${subSig})` : ''}`)
      // a retired sub's staged items can never be claimed — the tag leaves
      // the watch set, so its pendingLane entry is garbage that otherwise
      // never collects
      const live = new Set(subs.map((s) => s.tag))
      for (const tag of pendingLane.keys()) if (!live.has(tag)) pendingLane.delete(tag)
    }
    // every lane read is independent — run them together, bounded, instead
    // of serially pricing the pass at the sum of all host latencies
    await Promise.all(
      subs.map(async (s) => {
        const staged = pendingLane.get(s.tag) ?? []
        pendingLane.delete(s.tag)
        const toEvents = (items: { txid: string; payload: { body: unknown } }[]) => [
          ...staged.splice(0),
          ...items.map((i) => ({ seq: i.txid, kind: s.kind, payload: i.payload.body }))
        ]
        if (marketHosts.length > 1) {
          const fast = await s.call({ timeoutMs, quorum: 1 })
          const seenFast = new Set(fast.items.map((i) => i.txid))
          void s
            .call({ timeoutMs })
            .then((full) => {
              laneNotes(full, Object.keys(full.perHost))
              // stage only what the fast quorum didn't already deliver —
              // re-delivering the full set makes every staged item a wasted
              // ingest pass for an item already in the store
              const rest = full.items.filter((i) => !seenFast.has(i.txid)).map((i) => ({ seq: i.txid, kind: s.kind, payload: i.payload.body }))
              if (rest.length > 0) {
                pendingLane.set(s.tag, [...(pendingLane.get(s.tag) ?? []), ...rest])
                // staged items deserve the next pass NOW, not whenever the
                // idle ceiling happens to fire — the same wake push uses
                rt.countWake('lane:staged')
                void rt.deals().catch(() => undefined)
              }
            })
            .catch(() => undefined)
          if ((fast.items as unknown[]).length > 0 || staged.length > 0)
            log(`lanes: ${s.tag} items=${(fast.items as unknown[]).length} staged=${staged.length}`)
          const events = toEvents(fast.items as never)
          if (events.length > 0) {
            rt.hotUntil = Date.now() + WAITS.hotWindow.ms
            await rt.engine.ingestEvents(s.tag, events, '')
          }
        } else {
          const r = await s.call({ timeoutMs })
          laneNotes(r, Object.keys(r.perHost))
          const events = toEvents(r.items as never)
          if (events.length > 0) {
            rt.hotUntil = Date.now() + WAITS.hotWindow.ms
            await rt.engine.ingestEvents(s.tag, events, '')
          }
        }
      })
    )
  }
  rt.poll = async (): Promise<void> => {
    // discovery reads and the inbox are independent — a stalled lane lookup
    // must not delay message collection, and receipts are pushed by the deal
    // loop after the tick, never before it
    await Promise.all([
      lanes !== undefined ? lanePoll().catch((e) => log(`automation: lanes: ${(e as Error).message}`)) : Promise.resolve(),
      rt.msgs !== undefined
        ? (async () => {
            try {
              const collected = await rt.msgs!.collect()
              if (collected.length > 0) rt.hotUntil = Date.now() + WAITS.hotWindow.ms
              for (const m of collected) await rt.engine.ingestMessage(m.from, m.msg, m.refs)
            } catch (e) {
              log(`automation: messages: ${(e as Error).message}`)
            }
          })()
        : Promise.resolve()
    ])
  }
  rt.resolveRecord = resolveRecord
  rt.hostsFor = hostsFor
  rt.notifyPeer = notifyPeer
  rt.hostBack = hostBack
  rt.queueRepublish = queueRepublish
}
