// upkeep.ts — the upkeep region: funding-tree refill + stuck/dead mint
// healing, own bfinger record, bstore evidence, blogs deal journal
// (append + anchor), and the deal-journal view `deals {operationId}` reads.
// The `upkeep` guard serializes it; the assembler's cycle() and tick drive it.
import { P2PKH, SatoshisPerKilobyte, Transaction } from '@bsv/sdk'
import { DEFINITIVE as BROADCAST_DEFINITIVE } from './wallet.js'
import { abandonedReap, queueRepairs, trackFanout } from './fanout.js'
import { signerAddress, unlockP2PKH, BoundedMap, BoundedSet, nektarEnvFlag, nektarEnvNum, deadline, detached, sparse, spread } from '@nektar/core'
import { FundsStarvedError } from '@nektar/chain'
import { WAITS } from './waits.js'
import { deadAncestry } from '@nektar/bstack'
import { openRefusals } from './refusals.js'
import { TERMINAL } from './market/types.js'
import type {} from '@nektar/bstack'
import type { MarketOpData } from './store.js'
import type { Rt } from './rtctx.js'

export function openUpkeep(rt: Rt): void {
  const { cfg, mem, store, log } = rt
  const lanes = rt.lanes
  const key = rt.key
  const wallet = rt.wallet
  const marketHosts = rt.marketHosts
  const logStream = rt.logStream
  const logState = rt.logState
  const fingerState = rt.fingerState
  const packsStore = rt.packsStore
  const objectStore = rt.objectStore
  const localNet = rt.localNet
  // dead-mint cleanup — a reaped mint never landed: its outputs tombstone
  // and its inputs return to the spendable pool (a truly-spent one is
  // re-caught by the take-time spend check)
  const ghostMint = (tree: import('@bsv/sdk').Transaction): void => {
    void mem.funds.died(tree)
  }
  // stuck-mint healing — a mint the chain observer can't see ('unknown') gets
  // one resubmit per age window; only a definitive refusal counts toward the
  // drop verdict — a dead endpoint must never reap a healthy mint. The age
  // window keys on the row's durable added_at (unprovenSince): the old
  // in-memory first-seen reset on every restart, so a churned process could
  // hold phantom mints under the heal window indefinitely (observed: two
  // never-propagated bbox mints pinned the mint gate ~50min). This map now
  // only rate-limits resubmits and counts definitive refusals per process.
  // txFacts is throttled: ChainEvents polls these txids on its own watch
  // floor and delivers rejections/proofs itself — this second read is only
  // the mint-gate's heal/reap input, so a short cache loses no signal
  const statusCache = new BoundedMap<string, { at: number; f: import('@nektar/chain').TxFacts }>(4096)
  const facts = async (txid: string): Promise<import('@nektar/chain').TxFacts> => {
    const c = statusCache.get(txid)
    if (c !== undefined && Date.now() - c.at < WAITS.statusCache.ms) return c.f
    const f = await mem.txFacts(txid)
    statusCache.set(txid, { at: Date.now(), f })
    return f
  }
  // dead-ancestry strikes — treeTxid:parentTxid → first-seen; the same gap
  // on two upkeep passes evicts (a single fetch miss must not reap stock
  // whose parent was simply between propagation hops)
  const deadSuspects = new BoundedMap<string, number>(4096)
  const stuckMints = new BoundedMap<string, { first: number; tries: number; last: number }>(4096)
  // refused-ancestor registry — a refusal that names its ancestor is
  // first-party evidence the ancestor is dead to this mempool: every
  // outpoint whose unproven ancestry contains it can never air, whatever
  // its own status word says. Registered on funds' append-only
  // deadParents seam (assigning deadParent would clobber custody's
  // verdict oracle — and is what check:arch's oracle-write gate bans) —
  // the mark prices exactly the outpoints the ancestry walk can reach:
  // a refused sibling never poisons a clean parent, and the revive path
  // in died() consults the same verdict before resurrecting an input.
  const refusals = openRefusals({ fetchTx: (txid) => mem.fetchTx(txid).catch(() => undefined) })
  mem.funds.deadParents?.push(refusals.parent)
  // refusals whose wording the registry didn't recognize — logged once
  // per distinct text so an unrecognized production shape is visible
  // instead of looking like the parser is working
  const unmatchedRefusals = new BoundedSet<string>(256)
  const broadcast = async (tx: import('@bsv/sdk').Transaction): Promise<import('@nektar/chain').BroadcastResult> => {
    const r = await mem.broadcastTx(tx)
    if (r.status === 'success') return r
    if (
      refusals.noteRefusal(r.description) === 0 &&
      BROADCAST_DEFINITIVE.test(r.description ?? '') &&
      !(r.description ?? '').startsWith('transport:') &&
      !unmatchedRefusals.has(r.description ?? '')
    ) {
      unmatchedRefusals.add(r.description ?? '')
      log(`automation: definitive refusal named no ancestor — unrecognized wording: ${(r.description ?? '').slice(0, 200)}`)
    }
    return r
  }
  const stuckMint = async (tree: import('@bsv/sdk').Transaction, unprovenSince?: number): Promise<'keep' | 'drop'> => {
    const tid = tree.id('hex')
    const now = Date.now()
    const s = stuckMints.get(tid) ?? { first: unprovenSince ?? now, tries: 0, last: 0 }
    stuckMints.set(tid, s)
    // eligibility rides the row's durable age when known; rows predating the
    // column (addedAt undefined) fall back to this process's first sighting
    const first = unprovenSince ?? s.first
    if (now - first < WAITS.mintHeal.ms) return 'keep'
    if (s.last !== 0 && now - s.last < WAITS.mintHeal.ms) return 'keep' // one resubmit per window per process
    const r = await broadcast(tree).catch((e: unknown) => ({
      status: 'error' as const,
      description: `transport: ${e instanceof Error ? e.message : String(e)}`
    }))
    s.last = now // rate-limit resubmits; eligibility stays on `first`
    if (r.status === 'success') {
      s.tries = 0
      return 'keep'
    }
    // a thrown broadcast is transport (never reached the wire) — only a
    // resolved DEFINITIVE verdict can retire the stuck mint
    if (!BROADCAST_DEFINITIVE.test(r.description ?? '') || (r.description ?? '').startsWith('transport:')) return 'keep'
    s.tries++
    return s.tries >= 2 ? 'drop' : 'keep'
  }
  // per-lane funding shape — the same table provisions stock:
  // every lane-locked output of the funding tx is a slot
  const laneSpec = async (kind: string): Promise<{ lock: import('@bsv/sdk').LockingScript | undefined; satsPerOutput: number; size: number }> => {
    const mod = lanes?.mod
    return {
      lock:
        mod === undefined
          ? undefined
          : kind === 'bbox'
            ? await mod.boxFundingLock(key, wallet)
            : kind === 'log'
              ? await mod.logFundingLock(key, wallet)
              : kind === 'finger'
                ? await mod.fingerFundingLock(key, wallet)
                : await mod.marketFundingLock(wallet),
      satsPerOutput: kind === 'log' || kind === 'finger' ? 100 : 1,
      size: kind === 'finger' ? 4 : (cfg.treeSize ?? 64)
    }
  }

  // bstore evidence: once a buyer op verifies the disclosure, the redacted
  // bundle (question + disclosure tx out, digests in) goes to the object
  // store under a deterministic key, and a same-state 'evidence_stored'
  // transition commits the {bucket,key,sha256,size} ref into the op — the
  // blogs journal then anchors the ref's digest. Deterministic bytes + key
  // make a crash-and-retry idempotent: same put, exactly one event. A store
  // failure is logged once per distinct message and retried next cycle; it
  // never blocks the deal. Sellers store nothing (the buyer's bundle is the
  // record).
  let evidenceError = ''
  const storeEvidence = async (): Promise<void> => {
    if (objectStore === undefined) return
    const { collectTradeEvidence, redactTradeEvidence } = await import('./evidence.js')
    for (const op of await store.operations({ role: 'buyer', states: ['disclosure_verified', 'useful'] })) {
      if (op.data.evidenceRef !== undefined) continue
      try {
        const bundle = redactTradeEvidence(await collectTradeEvidence(store, op, (txid) => mem.fetchTx(txid)))
        const bytes = new TextEncoder().encode(JSON.stringify(bundle))
        const key = `evidence/${cfg.network}/${op.operationId}.json`
        const digest = Buffer.from(await crypto.subtle.digest('SHA-256', bytes)).toString('hex')
        const head = await objectStore.head(key).catch(() => undefined)
        const ref: import('@nektar/store').ObjectRef =
          head?.sha256 === digest ? { bucket: objectStore.bucket, key, sha256: digest, size: head.size } : await objectStore.put(key, bytes, 'application/json')
        try {
          // content-addressed like packs: chirp://<root> resolves the bundle
          // from any store holding chirp/<hash> objects, not just our bucket.
          // A retry after a crash only republishes when the root object is
          // actually missing — same keys otherwise, so head-before-put keeps
          // the crash-and-retry idempotent instead of counting second puts
          const { chirpRefFor, storeChirp } = await import('@nektar/store')
          const c = chirpRefFor(bytes)
          if ((await objectStore.head(c.rootKey).catch(() => undefined)) === undefined) await storeChirp(objectStore, bytes)
          ref.chirp = c.url
          ref.endpoint = objectStore.endpoint
        } catch {
          /* chirp publish is best-effort — the keyed ref alone still resolves */
        }
        const cur = await store.operation(op.operationId)
        if (cur === undefined || cur.data.evidenceRef !== undefined) continue
        await store.transition(op.operationId, cur.revision, cur.state, { cause: 'evidence_stored', data: { evidenceRef: ref }, evidence: { ref } })
        evidenceError = ''
      } catch (e) {
        const m = `automation: evidence ${op.operationId}: ${(e as Error).message.slice(0, 160)}`
        if (m !== evidenceError) {
          evidenceError = m
          log(m)
        }
      }
    }
  }
  // blogs deal journal: every market event becomes one line in one batch
  const EV_CURSOR = 'log#events'
  let logError = ''
  let fingerError = ''
  const wantIdOf = (op: { data: MarketOpData }): string | null => sparse(op.data.want?.want)?.wantId ?? null
  // anchors never block a cycle: they run on the inflight set like pushed work
  const maybeAnchor = (): Promise<void> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return Promise.resolve()
    const work = detached(() =>
      (async () => {
        try {
          const r = await lanes.mod.anchorLog({
            key,
            wallet,
            ledger: lanes.ledger,
            journal: lanes.journal,
            state: logState,
            hosts: marketHosts,
            stream: logStream,
            broadcast: (tx) => broadcast(tx),
            fetchTx: (txid) => mem.fetchTx(txid),
            now: cfg.now,
            feeRateSatPerKb: mem.feeRateSatPerKb,
            log: (line) => log(`automation: ${line}`)
          })
          if (r !== undefined) {
            log(`automation: log anchor interval ${r.interval} ${r.txid}`)
            // a host that missed the mined-anchor publish gets a repair row
            // keyed `logpub:{stream}:{txid}` — the journaled BEEF re-serves it
            if (r.missed !== undefined && r.missed.length > 0)
              await queueRepairs(
                rt.fanoutIo,
                `logpub:${logStream}:${r.txid}`,
                r.missed.map((m) => m.host)
              )
          }
        } catch (e) {
          log(`automation: log anchor: ${(e as Error).message.slice(0, 200)}`)
        }
      })()
    )
    rt.inflight.add(work)
    const done = (): void => {
      rt.inflight.delete(work)
    }
    void work.then(done, done)
    return Promise.resolve()
  }
  const appendLogEvents = async (): Promise<void> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return
    const cursor = (await logState.get(EV_CURSOR))?.seq ?? 0
    const rows = await store.eventsAfter(cursor, 500)
    if (rows.length === 0) {
      await maybeAnchor()
      return
    }
    const { sha256, utf8, toHex } = await import('@nektar/core')
    const t0 = Date.now()
    // two set-shaped reads cover the whole batch — the per-row
    // operation()+eventCount() pair was ~2 serial queries per event
    const ids = [...new Set(rows.map((r) => r.operationId))]
    const [ops, bases] = await Promise.all([store.opsByIds(ids), store.eventCountsByOp(ids, cursor)])
    const ns = new Map<string, number>()
    const lines: string[] = []
    let lastSeq = cursor
    for (const r of rows) {
      const op = ops.get(r.operationId)
      if (op === undefined) {
        lastSeq = r.seq
        continue
      }
      const n = (ns.get(op.operationId) ?? bases.get(op.operationId) ?? 0) + 1
      ns.set(op.operationId, n)
      const ev = r.evidence === undefined ? null : toHex(Uint8Array.from(sha256(utf8(lanes.mod.marketJson(r.evidence)))))
      lines.push(JSON.stringify({ v: 1, op: op.operationId, want: wantIdOf(op), role: op.role, n, state: r.to, cause: r.cause, t: r.at, ev }))
      lastSeq = r.seq
    }
    if (rows.length > 100) log(`automation: log append ${rows.length} rows ${Date.now() - t0}ms`)
    try {
      if (lines.length > 0) {
        const r = await lanes.mod.appendLog({
          key,
          wallet,
          ledger: lanes.ledger,
          journal: lanes.journal,
          state: logState,
          hosts: marketHosts,
          stream: logStream,
          lines,
          now: cfg.now
        })
        // a host that missed this batch keeps a numbered gap in its replica —
        // repair row per missed leg re-serves the journaled carrier
        // (`log:{stream}:{seq}`), same contract as every other fan-out
        await queueRepairs(
          rt.fanoutIo,
          `log:${logStream}:${r.seq}`,
          r.missed.map((m) => m.host)
        )
        const byKey = new Map<string, string[]>()
        for (const m of r.healMissed) {
          const hs = byKey.get(m.key) ?? []
          hs.push(m.host)
          byKey.set(m.key, hs)
        }
        for (const [k, hosts] of byKey) await queueRepairs(rt.fanoutIo, k, hosts)
      }
      // the cursor moves only after the batch reached quorum: a crash between
      // tick and append replays the same lines on the next cycle, and the
      // journal keeps the resubmitted carrier identical (same txid, same seq)
      await logState.set(EV_CURSOR, { seq: lastSeq, prev: '' })
      logError = ''
      await maybeAnchor()
    } catch (e) {
      const m = (e as Error).message
      if (m !== logError) {
        logError = m
        log(`automation: log append: ${m.slice(0, 200)}`)
      }
    }
  }

  // the directory card's last-published state — the canon compare is what
  // makes republishing change-driven instead of a per-pass publish
  let announceLast: { canon: string; exp: number; hash?: string; publishedAt?: number } | undefined
  let announceError = ''
  let reserveError = ''
  // tree-mint starvation backoff — avoids burning 4-7s of starved take() +
  // liveSet() scans every 10s when the treasury is below dealFloor; cleared
  // immediately when funds.adopted or txProof fires
  let starveBackoffMs = 0
  let starveUntil = 0
  rt.rtUpkeepClearStarve = () => {
    starveBackoffMs = 0
    starveUntil = 0
  }
  // retention sweep cadence — the prune phase itself decides daily; the
  // clock lives here so a mid-pass abandon doesn't re-enter it early
  let lastPruneAt = -Infinity
  let lastConsolidateAt = -Infinity

  let upkeepPhase = 'start'
  const bound = async <T>(work: (signal: AbortSignal) => Promise<T>, ms = WAITS.upkeepBound.ms): Promise<T | undefined> => {
    // a leg that never settles parks EVERY later upkeep on upkeepRun — a
    // logged abandon frees the single-flight; the section name tells us
    // which call site swallowed the wait. The leg's deadline is ambient —
    // inner IO merging ioSignal/nap dies at the SUCCESSOR's bound instead
    // of double-running phases
    const r = await deadline(work, ms, { signal: rt.signal })
    if (r.outcome === 'settled') return r.value
    if (r.outcome === 'timeout') log(`automation: upkeep(${upkeepPhase}) exceeded ${ms}ms — abandoning`)
    return undefined
  }
  const runUpkeep = async (signal?: AbortSignal): Promise<void> => {
    const cancelled = (): boolean => rt.stopped || signal?.aborted === true
    try {
      if (lanes !== undefined) {
        try {
          upkeepPhase = 'reap'
          // dead-ancestry reap — an unproven tree whose input chain has a gap
          // the network cannot serve (a killed parent mint, a dropped draft)
          // can neither prove nor anchor: it wedges the mint gate and strands
          // journaled carriers on an endless resubmit. Two strikes on the
          // same gap guards lookup flakes; the reap drops the tree AND the
          // carriers it funded so stock refills instead of retrying poison.
          for (const u of await lanes.ledger.unproven()) {
            if (cancelled()) return
            const tree = await mem.fetchTx(u.txid).catch(() => undefined)
            if (tree === undefined || tree.merklePath !== undefined) continue
            const gaps = await deadAncestry(tree, (txid) => mem.fetchTx(txid).catch(() => undefined))
            if (gaps.length === 0) {
              for (const k2 of deadSuspects.keys()) if (k2.startsWith(`${u.txid}:`)) deadSuspects.delete(k2)
              continue
            }
            const struck = gaps.filter((g) => {
              const k2 = `${u.txid}:${g}`
              if (deadSuspects.has(k2)) return true
              deadSuspects.set(k2, Date.now())
              return false
            })
            if (struck.length === 0) continue
            if (abandonedReap(u.kind).ghost) ghostMint(tree)
            await lanes.ledger.drop(u.txid)
            let reaped = 0
            for (const row of (await lanes.journal.fundedBy?.(u.txid)) ?? []) {
              await lanes.journal.del?.(row.hash)
              reaped++
            }
            await store.unblockOutbox('funding-slot')
            log(
              `automation: dead-ancestry: dropped ${u.kind} tree ${u.txid.slice(0, 12)} — unservable parent ${struck[0].slice(0, 12)}; reaped ${reaped} journaled carrier(s)`
            )
          }
          // Mint lane trees BEFORE splitting the reserve so a cold-start boot
          // spends confirmed funding UTXOs directly (0-block anchored stock for
          // market + bbox) instead of spending an unconfirmed reserve split.
          upkeepPhase = 'trees'
          if (Date.now() >= starveUntil) {
            for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger'] as const) {
              // stop() drains upkeep with a bound — bail between phases so a
              // late pass doesn't outlive the pool it's about to write to
              if (cancelled()) return
              const spec = await laneSpec(kind)
              const st = await lanes.ledger.stock?.(kind)
              const coldStart = st !== undefined && st.proven === 0 && st.taken === 0
              const r = await lanes.mod.ensureTrees({
                key,
                wallet,
                ledger: lanes.ledger,
                kind,
                // every lane refills a whole tree ahead: with the proof now
                // travelling on the FundingOut, an anchored tree serves the
                // market lane at once, and proven stock is already there when
                // the vendored-codec lanes (which demand the tree's own mined
                // path) need it. Proven-only kinds default lowWater to the
                // full tree — a shallower buffer is a ~1 block stall on
                // stock-out since they cannot dip into anchored stock
                lowWater:
                  kind === 'finger' ? (cfg.treeLowWater ?? 2) : kind === 'market' ? (cfg.treeLowWater ?? cfg.treeSize ?? 64) : (cfg.treeLowWater ?? spec.size),
                maxUnproven: cfg.treeMaxUnproven ?? (kind === 'finger' || kind === 'log' || coldStart ? 1 : 2),
                changeFanout: cfg.changeFanout,
                size: spec.size,
                satsPerOutput: spec.satsPerOutput,
                reserveSats: cfg.treeReserveSats,
                feeRateSatPerKb: mem.feeRateSatPerKb,
                lock: kind === 'market' ? undefined : spec.lock,
                takeFunding: async (sats: number) => (await mem.fundingOuts(sats, 'upkeep'))[0],
                broadcast: (tx) => broadcast(tx),
                fetchTx: (txid) => mem.fetchTx(txid),
                facts,
                onReject: (tree) => ghostMint(tree),
                // a mint refused at broadcast: the refusal's ancestor already
                // fed refusedAncestors through `broadcast`; a definitive
                // refusal ghosts the take so the input's reservation frees
                // and its dead-parent side stays quarantined. A maybe-aired
                // (transport) failure keeps the reservation — a phantom
                // reservation beats a double-spend of a mint that may land.
                onRefused: (tree, r) => {
                  if (BROADCAST_DEFINITIVE.test(r.description ?? '') && !(r.description ?? '').startsWith('transport:')) ghostMint(tree)
                },
                onStuck: (tree, unprovenSince) => stuckMint(tree, unprovenSince),
                onSpent: (tree) => void mem.funds.spent(tree)
              })
              // a fresh market/bbox tree's slots are usable the moment it's added
              // (anchored ancestry), and newly proven trees unblock proven-only
              // lanes too — wake slot-deferred work now instead of waiting out the defer window
              if ((r.minted.length > 0 && (kind === 'market' || kind === 'bbox')) || r.proven.length > 0) await store.unblockOutbox('funding-slot')
              // a proven log tree's BEEF must reach tm_log before hosts can
              // admit carriers spending it - journaled, publishes once
              for (const txid of r.proven) {
                const tx = await mem.fetchTx(txid)
                if (kind === 'log' && tx !== undefined && logStream !== undefined) {
                  const pub = await lanes.mod.publishMinedLog({ journal: lanes.journal, hosts: marketHosts, stream: logStream, tx })
                  await queueRepairs(
                    rt.fanoutIo,
                    `logpub:${logStream}:${txid}`,
                    pub.missed.map((m) => m.host)
                  )
                }
                // carriers minted on this tree while it was unproven carry a
                // host-side eviction deadline that never clears on its own —
                // queue a per-row re-announce with the parent's BUMP attached;
                // every tree kind carries carriers (market offers, finger
                // records, log batches, box envelopes) — fundedBy() finds only
                // rows that recorded the tree they spend
                if (tx?.merklePath !== undefined) await rt.queueRepublish(txid)
              }
            }
            starveBackoffMs = 0
            starveUntil = 0
          }
          upkeepPhase = 'consolidate'
          // UTXO consolidation: mature wallets fragment into hundreds of small
          // change UTXOs that slow boot reconciliation and break single-input
          // reserve splits. Merge up to 16 confirmed non-dust UTXOs into clean
          // medium outputs at most once per 30 min (opt-out: NEKTAR_AUTO_CONSOLIDATE=0).
          const nowCon = cfg.now?.() ?? Date.now()
          if (
            nektarEnvFlag('NEKTAR_AUTO_CONSOLIDATE') !== false &&
            mem.funds.takeMany !== undefined &&
            nowCon - lastConsolidateAt >= 30 * 60_000 &&
            !cancelled()
          ) {
            try {
              const rate = mem.feeRateSatPerKb ?? (cfg.network === 'main' ? 100 : 1)
              const inputFee = Math.max(1, Math.ceil((148 * rate) / 1000))
              const minConSats = Math.max(546, inputFee * 3)
              const outSats = cfg.reserveOutSats ?? (mem.funds.dealFloor || 25_000) * 2
              const wantOutsCon = cfg.reserveOuts ?? 2
              const allOuts = await mem.funds.outs()
              const smallConfirmed = allOuts.filter((u) => u.tx.merklePath !== undefined && u.satoshis >= minConSats && u.satoshis < outSats)
              const mediums = allOuts.filter((u) => u.satoshis >= outSats)
              const needReserve = outSats * wantOutsCon + 600
              const canSingleSplit = allOuts.some((u) => u.satoshis >= needReserve)
              const sortedSmall = [...smallConfirmed].sort((a, b) => a.satoshis - b.satoshis)
              const totalSmall = sortedSmall.reduce((s, u) => s + u.satoshis, 0)
              const batchSum = sortedSmall.slice(0, 16).reduce((s, u) => s + u.satoshis, 0)
              const shouldConsolidate =
                smallConfirmed.length > 24 ||
                (wantOutsCon > 0 && mediums.length < wantOutsCon && !canSingleSplit && smallConfirmed.length >= 2 && totalSmall >= needReserve)
              if (shouldConsolidate && batchSum > minConSats * 2) {
                const targetSats = Math.min(batchSum, Math.max(needReserve + 2_000, outSats * Math.max(2, wantOutsCon)))
                const ins = await mem.funds.takeMany('upkeep', targetSats, {
                  maxInputs: 16,
                  minOutputSats: minConSats,
                  maxOutputSats: outSats - 1,
                  confirmedOnly: true,
                  selfPay: true
                })
                if (ins.length >= 2) {
                  try {
                    const addr = await signerAddress(key, cfg.network === 'main' ? [0x00] : [0x6f])
                    const lock = new P2PKH().lock(addr)
                    const ctx = new Transaction()
                    let inSum = 0
                    for (const u of ins) {
                      ctx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: unlockP2PKH(key) })
                      inSum += u.satoshis
                    }
                    const estFee = Math.max(200, Math.ceil(((ins.length * 148 + (wantOutsCon + 1) * 34 + 20) * rate) / 1000) + 100)
                    let rem = inSum - estFee
                    let made = 0
                    for (let i = 0; i < wantOutsCon && rem >= outSats + 546; i++) {
                      ctx.addOutput({ satoshis: outSats, lockingScript: lock })
                      rem -= outSats
                      made++
                    }
                    ctx.addOutput({ lockingScript: lock, change: true })
                    await ctx.fee(new SatoshisPerKilobyte(rate))
                    await ctx.sign()
                    const br = await broadcast(ctx)
                    if (br.status === 'success') {
                      lastConsolidateAt = nowCon
                      await mem.funds.spent(ctx)
                      log(`automation: consolidate: merged ${ins.length} UTXOs (${inSum} sats) into ${made}x${outSats} + change`)
                    } else if (BROADCAST_DEFINITIVE.test(br.description ?? '') && !(br.description ?? '').startsWith('transport:')) {
                      await mem.funds.died(ctx)
                    }
                  } catch (e) {
                    for (const u of ins) mem.funds.release(u.tx.id('hex'), u.vout)
                    throw e
                  }
                } else {
                  for (const u of ins) mem.funds.release(u.tx.id('hex'), u.vout)
                }
              }
            } catch (e) {
              const m = `automation: consolidate: ${(e as Error).message.slice(0, 160)}`
              if (m !== reserveError) {
                reserveError = m
                log(m)
              }
            }
          }
          upkeepPhase = 'reserve'
          // reserve policy: keep `reserveOuts` medium self-pay outputs on
          // hand so one dominant UTXO is never the single coin a deal escrow
          // and a tree mint both reach for — a mint burns one medium while a
          // deal funds from another. mintTree with our own P2PKH lock is the
          // denomination: spent() adopts every output the moment the tx
          // broadcasts, so the reserve is spendable with no index round.
          const wantOuts = cfg.reserveOuts ?? 2
          if (wantOuts > 0 && !cancelled()) {
            try {
              // dealFloor 0 = "no floor configured" — size the medium off
              // the default, never off zero
              const outSats = cfg.reserveOutSats ?? (mem.funds.dealFloor || 25_000) * 2
              const allOuts = await mem.funds.outs()
              const mediums = allOuts.filter((u) => u.satoshis >= outSats)
              const totalSats = allOuts.reduce((s, u) => s + u.satoshis, 0)
              const needSats = outSats * wantOuts + 600 + (mem.funds.dealFloor ?? 0)
              if (mediums.length < wantOuts && totalSats >= needSats) {
                const src = sparse((await mem.fundingOuts(outSats * wantOuts + 600, 'upkeep'))[0])
                if (src !== undefined) {
                  const dtx = await lanes.mod.mintTree({
                    key,
                    wallet,
                    funding: src,
                    n: wantOuts,
                    satsPerOutput: outSats,
                    lock: new P2PKH().lock(await signerAddress(key)),
                    changeOutputs: cfg.changeFanout ?? 4,
                    feeRateSatPerKb: mem.feeRateSatPerKb
                  })
                  const br = await broadcast(dtx)
                  if (br.status === 'success') {
                    await mem.funds.spent(dtx)
                    log(`automation: reserve: split ${src.satoshis} into ${wantOuts}x${outSats} medium outputs`)
                  } else if (BROADCAST_DEFINITIVE.test(br.description ?? '') && !(br.description ?? '').startsWith('transport:')) {
                    // only a provably-dead verdict frees the inputs — a
                    // transport/'error' result may have aired; the phantom
                    // reservation beats a double-spend of a split that lands
                    await mem.funds.died(dtx)
                  }
                }
              }
            } catch (e) {
              // starvation is a state, not a fault — same posture as the
              // funding-slot gate: log once per message and retry next pass
              const m = `automation: reserve: ${(e as Error).message.slice(0, 160)}`
              if (m !== reserveError) {
                reserveError = m
                log(m)
              }
            }
          }
        } catch (e) {
          if (e instanceof FundsStarvedError) {
            starveBackoffMs = starveBackoffMs === 0 ? WAITS.upkeepStarveBase.ms : Math.min(WAITS.upkeepStarveCap.ms, starveBackoffMs * 2)
            starveUntil = Date.now() + spread(starveBackoffMs)
          }
          log(`automation: ensureTrees: ${(e as Error).message}`)
        }
      }
      upkeepPhase = 'finger'
      // own bfinger record: (re)publishes on the current host list, no-ops
      // when nothing changed, resubmits to any host that missed the objects —
      // finger self-heals by reconcile each pass, not by repair rows (the
      // log/fan-out streams queue `repair-host` rows instead).
      // B1 — opt-in only: it costs sats and makes this agent publicly
      // discoverable, so it is never automatic (NEKTAR_BFINGER_PUBLISH=1).
      if (lanes !== undefined && fingerState !== undefined && logStream !== undefined && cfg.publishFinger === true && !rt.stopped) {
        try {
          const packsRef = packsStore?.ref()
          const r = await lanes.mod.publishFinger({
            key,
            wallet,
            state: fingerState,
            ledger: lanes.ledger,
            journal: lanes.journal,
            hosts: marketHosts,
            body: {
              v: 1,
              hosts: marketHosts,
              office: cfg.boxOffice ?? lanes.mod.officeFor(),
              stream: logStream,
              nets: [cfg.network],
              ...(cfg.watchPublic !== undefined ? { watch: cfg.watchPublic } : {}),
              ...(cfg.pushPublic !== undefined ? { push: cfg.pushPublic } : {})
            },
            refs: packsRef !== undefined ? [packsRef] : undefined,
            takeFunding: async (sats: number) => (await mem.fundingOuts(sats, 'upkeep'))[0],
            broadcast: (tx) => broadcast(tx),
            fetchTx: (txid) => mem.fetchTx(txid),
            allowHttp: localNet,
            now: cfg.now,
            feeRateSatPerKb: mem.feeRateSatPerKb,
            onSpent: (tx) => void mem.funds.spent(tx),
            onDied: (tx) => void mem.funds.died(tx)
          })
          if (r.changed) log(`automation: finger seq ${r.seq} ${r.tokenTxid ?? ''}`)
        } catch (e) {
          // 'no funding slot' is transient while the finger tree matures — retried next cycle, not worth a line
          if (!(e as Error).message.includes('no funding slot')) {
            // one stack frame names the throw site — 'null.satoshis'-class
            // crashes were invisible inside the dedupe before this
            const site =
              (e as Error).stack
                ?.split('\n')
                .find((l) => l.includes('.js:'))
                ?.trim() ?? ''
            const m = `automation: finger: ${(e as Error).message.slice(0, 120)} @ ${site.slice(0, 80)}`
            if (m !== fingerError) {
              fingerError = m
              log(m)
            }
          }
        }
      }
      upkeepPhase = 'announce'
      // own directory announce: a signed card on the reserved dir lane naming
      // our hosts + offered seed packs — opt-in (NEKTAR_ANNOUNCE=1), same
      // cost/discoverability posture as publishFinger. Republishes when the
      // card content changes or exp enters its last half — steady-state is
      // ~one carrier per 3.5 days, not one per upkeep pass.
      if (lanes !== undefined && cfg.announce === true && !rt.stopped) {
        try {
          // the card mirrors what /.well-known/nektar offers: public shards
          // with a store ref (bstore-backed → resolvable origin-free), tagged
          // by their catalog domains so seeders can filter
          const catalog = (await mem.shardCatalog?.().catch(() => [])) ?? []
          const announcePacks: import('@nektar/bstack').AnnouncePack[] = []
          for (const s of mem.shards()) {
            if (s.startsWith('__') || mem.shardPrivate?.(s) === true) continue
            const store = mem.packStoreRef(s)
            // no bstore ref — fall back to the operator's public /v1/pack
            // URL when it has one (pushPublic is the same public surface);
            // a shard with neither can't be imported at all, so skip it
            const packUrl = cfg.pushPublic !== undefined ? `${cfg.pushPublic.replace(/\/+$/, '')}/v1/pack/${encodeURIComponent(s)}` : undefined
            if (store === undefined && packUrl === undefined) continue
            const domains = catalog.find((m) => m.shard === s)?.domains
            announcePacks.push({
              shard: s,
              ...(domains !== undefined && domains.length > 0 ? { domains } : {}),
              ...(store !== undefined ? { store } : {}),
              ...(packUrl !== undefined ? { packUrl } : {})
            })
          }
          const announceCanon = lanes.mod.marketJson({ hosts: marketHosts, packs: announcePacks })
          const now = cfg.now?.() ?? Date.now()
          // keep-alive while the card's funding tree is unproven: hosts evict
          // an admitted carrier whose funding lacks its BUMP inside the
          // FUNDING_PROOF_MS window (10 min, vendored constant) — a slow
          // mempool stretch outlives it, so re-announce well inside the
          // window while the tree still waits on a block. Once the tree
          // proves, queueRepublish re-records the same txid and this stops.
          let unprovenFunding = false
          if (announceLast?.hash !== undefined) {
            const row = await lanes.journal.get(announceLast.hash).catch(() => undefined)
            const tree = row?.fundingTxid !== undefined ? await mem.fetchTx(row.fundingTxid).catch(() => undefined) : undefined
            unprovenFunding = tree !== undefined && tree.merklePath === undefined
          }
          if (
            announceLast === undefined ||
            announceLast.canon !== announceCanon ||
            announceLast.exp - now <= lanes.mod.MAX_LIFETIME_MS / 2 ||
            (unprovenFunding && now - (announceLast.publishedAt ?? now) > 4 * 60_000)
          ) {
            const sa = await lanes.mod.signAnnounce(key, {
              hosts: marketHosts,
              ...(announcePacks.length > 0 ? { packs: announcePacks } : {}),
              ts: now,
              exp: now + lanes.mod.MAX_LIFETIME_MS,
              net: cfg.network
            })
            const r = await lanes.mod.publishArtifact({
              key,
              wallet,
              ledger: lanes.ledger,
              journal: lanes.journal,
              hosts: marketHosts,
              net: cfg.network,
              payload: { v: 1, kind: 'announce', body: sa }
            })
            await trackFanout(r, rt.fanoutIo)
            announceLast = { canon: announceCanon, exp: sa.announce.exp ?? now, hash: r.hash, publishedAt: now }
            announceError = ''
            log(`automation: announce ${r.txid.slice(0, 12)} packs=${announcePacks.length}`)
          }
        } catch (e) {
          // 'no funding slot' is transient while the market tree matures, but
          // a stock-out must leave a trace — dedupe like every other cause
          const msg = (e as Error).message.includes('no funding slot') ? 'no funding slot' : (e as Error).message.slice(0, 160)
          const m = `automation: announce: ${msg}`
          if (m !== announceError) {
            announceError = m
            log(m)
          }
        }
      }
      const stopped = (): boolean => rt.stopped
      if (stopped()) return
      upkeepPhase = 'evidence'
      await storeEvidence()
      if (stopped()) return
      upkeepPhase = 'log-events'
      await appendLogEvents()
      upkeepPhase = 'prune'
      // retention sweep — terminal rows and terminal-op journals older than
      // the window die, then the WAL truncates so the file actually shrinks.
      // Daily, not per-pass: deletes are IO the hot loop never needs, and a
      // sweep that raced every pass would churn the WAL for nothing. The
      // tx-side set is 'confirmed' only — the one state with no outgoing
      // edge; rejected/abandoned can still legally revive and their journal
      // is the revive audit.
      const nowMs = cfg.now?.() ?? Date.now()
      if (nowMs - lastPruneAt >= WAITS.pruneInterval.ms) {
        lastPruneAt = nowMs
        const retentionMs = (nektarEnvNum('NEKTAR_JOURNAL_RETENTION_DAYS') ?? 30) * WAITS.day.ms
        const n = await store.prune(nowMs - retentionMs, { ops: TERMINAL, txs: ['confirmed'] })
        const prunedTrees = (await lanes?.ledger.prune?.(nowMs - retentionMs).catch(() => 0)) ?? 0
        const prunedCarriers = (await lanes?.journal.prune?.(nowMs - retentionMs).catch(() => 0)) ?? 0
        const total = n.inbox + n.outbox + n.events + n.txEvents + prunedTrees + prunedCarriers
        if (total > 0)
          log(
            `automation: pruned ${total} rows (inbox=${n.inbox} outbox=${n.outbox} events=${n.events} txEvents=${n.txEvents} trees=${prunedTrees} carriers=${prunedCarriers})`
          )
      }
      upkeepPhase = 'done'
    } catch (e) {
      log(`automation: ${(e as Error).message}`)
    }
  }

  // deal journal view for `deals { operationId }`: own stream head + the
  // counterparty's stream filtered to this deal. The counterparty's stream
  // id rides its signed artifact (want.journal / offer.journal).
  const dealJournal = async (op: import('./store.js').Operation): Promise<Record<string, unknown> | undefined> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return undefined
    const own = { stream: logStream, lastSeq: (await logState.get(logStream))?.seq ?? 0 }
    const cp =
      op.role === 'buyer'
        ? (() => {
            const sel = op.data.selected?.key
            const so = sel !== undefined ? op.data.offers?.[sel]?.so : undefined
            return so?.offer.journal?.stream !== undefined && sel !== undefined ? { stream: so.offer.journal.stream, producer: sel.split(':')[0] } : undefined
          })()
        : (() => {
            const w = op.data.want
            return w?.want.journal?.stream !== undefined ? { stream: w.want.journal.stream, producer: w.want.pub } : undefined
          })()
    if (cp === undefined) return { own }
    try {
      // the counterparty's journal lives on ITS hosts and stream: the finger
      // record wins over the artifact's journal.stream hint when present
      const rec = await rt.resolveRecord(cp.producer)
      const r = await lanes.mod.readLog({
        hosts: await rt.hostsFor(cp.producer),
        stream: rec?.stream ?? cp.stream,
        producer: cp.producer,
        tracker: mem.sdkTracker()
      })
      const wantId = wantIdOf(op)
      const entries: Array<{ seq: number; n: number; state: string; cause: string; t: number }> = []
      for (const b of r.batches)
        for (const l of b.lines) {
          try {
            const j = JSON.parse(l) as { want?: string | null; op?: string; n?: number; state?: string; cause?: string; t?: number }
            if (j.want === wantId && typeof j.n === 'number') entries.push({ seq: b.seq, n: j.n, state: j.state ?? '', cause: j.cause ?? '', t: j.t ?? 0 })
          } catch {
            /* not a journal line */
          }
        }
      const base = { stream: cp.stream, producer: cp.producer, gaps: r.gaps, anchoredThrough: r.anchoredThrough, entries }
      if (entries.length === 0) return { own, counterparty: { ...base, complete: false } }
      const lo = Math.min(...entries.map((e) => e.seq))
      const hi = Math.max(...entries.map((e) => e.seq))
      const complete = !r.gaps.some((g) => g.to >= lo && g.from <= hi) && entries.every((e, i) => e.n === i + 1)
      return { own, counterparty: { ...base, complete } }
    } catch (e) {
      return { own, counterparty: { error: (e as Error).message.slice(0, 200) } }
    }
  }

  rt.ghostMint = ghostMint
  rt.laneSpec = laneSpec
  rt.dealJournal = dealJournal
  let upkeepInner: Promise<void> | undefined
  rt.upkeep = async (): Promise<void> => {
    if (rt.upkeepRun !== undefined) return await rt.upkeepRun
    // if a timed-out pass is still unwinding, don't start an overlapping pass
    if (upkeepInner !== undefined) return
    // a queued pass after stop() would only write into a destroyed pool —
    // the drain already owns the one still running
    if (rt.stopped) return
    rt.upkeepAt = Date.now()
    const t0 = Date.now()
    rt.upkeepRun = bound((signal) => {
      upkeepInner = runUpkeep(signal).finally(() => {
        upkeepInner = undefined
      })
      return upkeepInner
    }).finally(() => {
      rt.upkeepRun = undefined
      const ms = Date.now() - t0
      if (ms > WAITS.passReport.ms) log(`automation: upkeep ${ms}ms`)
    })
    await rt.upkeepRun
  }
}
