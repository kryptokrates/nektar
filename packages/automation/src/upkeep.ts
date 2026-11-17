// upkeep.ts — the upkeep region: funding-tree refill + stuck/dead mint
// healing, own bfinger record, bstore evidence, blogs deal journal
// (append + anchor), and the deal-journal view `deals {operationId}` reads.
// The `upkeep` guard serializes it; the assembler's cycle() and tick drive it.
import { DEFINITIVE as BROADCAST_DEFINITIVE } from './wallet.js'
import { queueRepairs } from './fanout.js'
import type { SignedOffer, SignedWant } from '@nektar/bstack'
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
  // drop verdict — a dead endpoint must never reap a healthy mint.
  // txFacts is throttled: ChainEvents polls these txids on its own watch
  // floor and delivers rejections/proofs itself — this second read is only
  // the mint-gate's heal/reap input, so a short cache loses no signal
  const statusCache = new Map<string, { at: number; f: import('@nektar/chain').TxFacts }>()
  const facts = async (txid: string): Promise<import('@nektar/chain').TxFacts> => {
    const c = statusCache.get(txid)
    if (c !== undefined && Date.now() - c.at < 15_000) return c.f
    if (statusCache.size > 4096) statusCache.clear()
    const f = await mem.txFacts(txid)
    statusCache.set(txid, { at: Date.now(), f })
    return f
  }
  const stuckMints = new Map<string, { first: number; tries: number }>()
  const stuckMint = async (tree: import('@bsv/sdk').Transaction): Promise<'keep' | 'drop'> => {
    const tid = tree.id('hex')
    const now = Date.now()
    if (stuckMints.size > 4096) stuckMints.clear()
    const s = stuckMints.get(tid) ?? { first: now, tries: 0 }
    stuckMints.set(tid, s)
    if (now - s.first < 10 * 60_000) return 'keep'
    const r = await mem.broadcastTx(tree).catch((e: unknown) => ({ status: 'error' as const, description: e instanceof Error ? e.message : String(e) }))
    s.first = now // one heal attempt per window
    if (r.status === 'success') {
      s.tries = 0
      return 'keep'
    }
    if (!BROADCAST_DEFINITIVE.test(r.description ?? '')) return 'keep'
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
  const wantIdOf = (op: { data: Record<string, unknown> }): string | null => (op.data.want as SignedWant | undefined)?.want?.wantId ?? null
  // anchors never block a cycle: they run on the inflight set like pushed work
  const maybeAnchor = async (): Promise<void> => {
    if (lanes === undefined || logState === undefined || logStream === undefined) return
    const work = (async () => {
      try {
        const r = await lanes.mod.anchorLog({
          key,
          wallet,
          ledger: lanes.ledger,
          journal: lanes.journal,
          state: logState,
          hosts: marketHosts,
          stream: logStream,
          broadcast: (tx) => mem.broadcastTx(tx),
          fetchTx: (txid) => mem.fetchTx(txid),
          now: cfg.now,
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
    rt.inflight.add(work)
    void work.finally(() => {
      rt.inflight.delete(work)
    })
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
    const ns = new Map<string, number>()
    const lines: string[] = []
    let lastSeq = cursor
    for (const r of rows) {
      const op = await store.operation(r.operationId)
      if (op === undefined) {
        lastSeq = r.seq
        continue
      }
      const n = (ns.get(op.operationId) ?? (await store.eventCount(op.operationId, cursor))) + 1
      ns.set(op.operationId, n)
      const ev = r.evidence === undefined ? null : toHex(Uint8Array.from(sha256(utf8(lanes.mod.marketJson(r.evidence)))))
      lines.push(JSON.stringify({ v: 1, op: op.operationId, want: wantIdOf(op), role: op.role, n, state: r.to, cause: r.cause, t: r.at, ev }))
      lastSeq = r.seq
    }
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

  const runUpkeep = async (): Promise<void> => {
    try {
      if (lanes !== undefined) {
        try {
          for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger'] as const) {
            const spec = await laneSpec(kind)
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
              lowWater: kind === 'finger' ? 2 : kind === 'market' ? (cfg.treeLowWater ?? cfg.treeSize ?? 64) : (cfg.treeLowWater ?? spec.size),
              maxUnproven: cfg.treeMaxUnproven ?? 2,
              size: spec.size,
              satsPerOutput: spec.satsPerOutput,
              lock: kind === 'market' ? undefined : spec.lock,
              takeFunding: async (sats: number) => (await mem.fundingOuts(sats, 'upkeep'))[0],
              broadcast: (tx) => mem.broadcastTx(tx),
              fetchTx: (txid) => mem.fetchTx(txid),
              facts,
              onReject: (tree) => ghostMint(tree),
              onStuck: (tree) => stuckMint(tree),
              onSpent: (tree) => void mem.funds.spent(tree)
            })
            // a fresh market tree's slots are usable the moment it's added —
            // anchored ancestry — so wake slot-deferred work now instead of
            // letting the defer window lapse
            if (r.minted.length > 0 && kind === 'market') await store.unblockOutbox('funding-slot')
            // a proven log tree's BEEF must reach tm_log before hosts can
            // admit carriers spending it - journaled, publishes once
            for (const txid of r.proven) {
              const tx = await mem.fetchTx(txid)
              if (tx !== undefined && logStream !== undefined) {
                const pub = await lanes.mod.publishMinedLog({ journal: lanes.journal, hosts: marketHosts, stream: logStream, tx })
                await queueRepairs(
                  rt.fanoutIo,
                  `logpub:${logStream}:${txid}`,
                  pub.missed.map((m) => m.host)
                )
              }
              // carriers minted on this tree while it was unproven carry a
              // host-side eviction deadline that never clears on its own —
              // queue a per-row re-announce with the parent's BUMP attached
              if (kind === 'market' && tx?.merklePath !== undefined) await rt.queueRepublish(txid)
            }
          }
        } catch (e) {
          log(`automation: ensureTrees: ${(e as Error).message}`)
        }
      }
      // own bfinger record: (re)publishes on the current host list, no-ops
      // when nothing changed, resubmits to any host that missed the objects —
      // finger self-heals by reconcile each pass, not by repair rows (the
      // log/fan-out streams queue `repair-host` rows instead).
      // B1 — opt-in only: it costs sats and makes this agent publicly
      // discoverable, so it is never automatic (NEKTAR_BFINGER_PUBLISH=1).
      if (lanes !== undefined && fingerState !== undefined && logStream !== undefined && cfg.publishFinger === true) {
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
            broadcast: (tx) => mem.broadcastTx(tx),
            fetchTx: (txid) => mem.fetchTx(txid),
            allowHttp: localNet,
            now: cfg.now,
            onSpent: (tx) => void mem.funds.spent(tx)
          })
          if (r.changed) log(`automation: finger seq ${r.seq} ${r.tokenTxid ?? ''}`)
        } catch (e) {
          // 'no funding slot' is transient while the finger tree matures — retried next cycle, not worth a line
          if (!(e as Error).message.includes('no funding slot')) {
            const m = `automation: finger: ${(e as Error).message.slice(0, 160)}`
            if (m !== fingerError) {
              fingerError = m
              log(m)
            }
          }
        }
      }
      await storeEvidence()
      await appendLogEvents()
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
            const sel = (op.data.selected as { key?: string } | undefined)?.key
            const so = sel !== undefined ? (op.data.offers as Record<string, { so: SignedOffer }> | undefined)?.[sel]?.so : undefined
            return so?.offer.journal?.stream !== undefined && sel !== undefined ? { stream: so.offer.journal.stream, producer: sel.split(':')[0] } : undefined
          })()
        : (() => {
            const w = op.data.want as SignedWant | undefined
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
  rt.upkeep = async (): Promise<void> => {
    if (rt.upkeepRun !== undefined) return await rt.upkeepRun
    rt.upkeepAt = Date.now()
    rt.upkeepRun = runUpkeep().finally(() => {
      rt.upkeepRun = undefined
    })
    await rt.upkeepRun
  }
}
