// upkeep.ts — the upkeep region: funding-tree refill + stuck/dead mint
// healing, own bfinger record, bstore evidence, blogs deal journal
// (append + anchor), and the deal-journal view `deals {operationId}` reads.
// The `upkeep` guard serializes it; the assembler's cycle() and tick drive it.
import { P2PKH } from '@bsv/sdk'
import { DEFINITIVE as BROADCAST_DEFINITIVE } from './wallet.js'
import { abandonedReap, queueRepairs, trackFanout } from './fanout.js'
import { signerAddress } from '@nektar/core'
import { deadAncestry } from '@nektar/bstack'
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
  // dead-ancestry strikes — treeTxid:parentTxid → first-seen; the same gap
  // on two upkeep passes evicts (a single fetch miss must not reap stock
  // whose parent was simply between propagation hops)
  const deadSuspects = new Map<string, number>()
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

  // the directory card's last-published state — the canon compare is what
  // makes republishing change-driven instead of a per-pass publish
  let announceLast: { canon: string; exp: number; hash?: string; publishedAt?: number } | undefined
  let announceError = ''
  let reserveError = ''

  let upkeepPhase = 'start'
  const bound = async <T>(p: Promise<T>, ms = 300_000): Promise<T | undefined> => {
    let timer: ReturnType<typeof setTimeout> | undefined
    const timeout = new Promise<undefined>((resolve) => {
      timer = setTimeout(() => {
        // a leg that never settles parks EVERY later upkeep on upkeepRun —
        // a logged abandon frees the single-flight; the section name tells
        // us which call site swallowed the wait
        log(`automation: upkeep(${upkeepPhase}) exceeded ${ms}ms — abandoning`)
        resolve(undefined)
      }, ms)
    })
    try {
      return await Promise.race([p, timeout])
    } finally {
      clearTimeout(timer)
    }
  }
  const runUpkeep = async (): Promise<void> => {
    try {
      if (lanes !== undefined) {
        try {
          upkeepPhase = 'reserve'
          // reserve policy: keep `reserveOuts` medium self-pay outputs on
          // hand so one dominant UTXO is never the single coin a deal escrow
          // and a tree mint both reach for — a mint burns one medium while a
          // deal funds from another. mintTree with our own P2PKH lock is the
          // denomination: spent() adopts every output the moment the tx
          // broadcasts, so the reserve is spendable with no index round.
          const wantOuts = cfg.reserveOuts ?? 2
          if (wantOuts > 0 && !rt.stopped) {
            try {
              // dealFloor 0 = "no floor configured" — size the medium off
              // the default, never off zero
              const outSats = cfg.reserveOutSats ?? (mem.funds.dealFloor || 25_000) * 2
              const mediums = (await mem.funds.outs()).filter((u) => u.satoshis >= outSats)
              if (mediums.length < wantOuts) {
                const src = (await mem.fundingOuts(outSats * wantOuts + 600, 'upkeep'))[0]
                if (src !== undefined) {
                  const dtx = await lanes.mod.mintTree({
                    key,
                    wallet,
                    funding: src,
                    n: wantOuts,
                    satsPerOutput: outSats,
                    lock: new P2PKH().lock(await signerAddress(key))
                  })
                  const br = await mem.broadcastTx(dtx)
                  if (br.status === 'success') {
                    await mem.funds.spent(dtx)
                    log(`automation: reserve: split ${src.satoshis} into ${wantOuts}x${outSats} medium outputs`)
                  } else {
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
          upkeepPhase = 'reap'
          // dead-ancestry reap — an unproven tree whose input chain has a gap
          // the network cannot serve (a killed parent mint, a dropped draft)
          // can neither prove nor anchor: it wedges the mint gate and strands
          // journaled carriers on an endless resubmit. Two strikes on the
          // same gap guards lookup flakes; the reap drops the tree AND the
          // carriers it funded so stock refills instead of retrying poison.
          for (const u of await lanes.ledger.unproven()) {
            if (rt.stopped) return
            const tree = await mem.fetchTx(u.txid).catch(() => undefined)
            if (tree === undefined || tree.merklePath !== undefined) continue
            const gaps = await deadAncestry(tree, (txid) => mem.fetchTx(txid).catch(() => undefined))
            if (gaps.length === 0) {
              for (const k2 of [...deadSuspects.keys()]) if (k2.startsWith(`${u.txid}:`)) deadSuspects.delete(k2)
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
          upkeepPhase = 'trees'
          for (const kind of ['market', 'bbox', ...(logStream !== undefined ? ['log'] : []), 'finger'] as const) {
            // stop() drains upkeep with a bound — bail between phases so a
            // late pass doesn't outlive the pool it's about to write to
            if (rt.stopped) return
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
              lowWater:
                kind === 'finger' ? (cfg.treeLowWater ?? 2) : kind === 'market' ? (cfg.treeLowWater ?? cfg.treeSize ?? 64) : (cfg.treeLowWater ?? spec.size),
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
              // queue a per-row re-announce with the parent's BUMP attached;
              // every tree kind carries carriers (market offers, finger
              // records, log batches, box envelopes) — fundedBy() finds only
              // rows that recorded the tree they spend
              if (tx?.merklePath !== undefined) await rt.queueRepublish(txid)
            }
          }
        } catch (e) {
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
            broadcast: (tx) => mem.broadcastTx(tx),
            fetchTx: (txid) => mem.fetchTx(txid),
            allowHttp: localNet,
            now: cfg.now,
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
      if (rt.stopped) return
      upkeepPhase = 'evidence'
      await storeEvidence()
      if (rt.stopped) return
      upkeepPhase = 'log-events'
      await appendLogEvents()
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
    // a queued pass after stop() would only write into a destroyed pool —
    // the drain already owns the one still running
    if (rt.stopped) return
    rt.upkeepAt = Date.now()
    const t0 = Date.now()
    rt.upkeepRun = bound(runUpkeep()).finally(() => {
      rt.upkeepRun = undefined
      const ms = Date.now() - t0
      if (ms > 2_000) log(`automation: upkeep ${ms}ms`)
    })
    await rt.upkeepRun
  }
}
