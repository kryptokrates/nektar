// watches.ts — the watch-shim region: long-poll subscriptions that mirror
// queries the poll loop already issues; a changed result-set wakes deals()
// early. Every failure mode degrades to the ordinary poll path.
import { detached } from '@nektar/core'
import type { Rt } from './rtctx.js'
import { WAITS } from './waits.js'

export function openWatches(rt: Rt): void {
  const { cfg, log } = rt
  const lanes = rt.lanes
  const watchOne = async (base: string, service: string, query: unknown, signal: AbortSignal): Promise<void> => {
    let cursor: string | undefined
    // jittered backoff on consecutive failures, reset on any answered
    // request — and the nap ends the moment the watch itself is dropped,
    // so an aborted subscription never leaves a 30s zombie sleep
    let backoff = WAITS.watchBackoffMin.ms
    const nap = (ms: number): Promise<void> =>
      new Promise((resolve) => {
        const t = setTimeout(() => {
          signal.removeEventListener('abort', done)
          resolve()
        }, ms)
        const done = (): void => {
          clearTimeout(t)
          resolve()
        }
        signal.addEventListener('abort', done, { once: true })
      })
    const fail = async (): Promise<void> => {
      await nap(Math.min(backoff, WAITS.watchBackoffCap.ms) * (0.5 + Math.random()))
      backoff = Math.min(backoff * 2, WAITS.watchBackoffCap.ms)
    }
    const stopped = (): boolean => rt.stopped
    const alive = (): boolean => !rt.stopped && !signal.aborted
    while (alive()) {
      try {
        const u = `${base}/watch?service=${encodeURIComponent(service)}&query=${encodeURIComponent(JSON.stringify(query))}${cursor !== undefined ? `&cursor=${encodeURIComponent(cursor)}` : ''}&waitMs=25000`
        // the shim holds up to 25s; a shim that accepts and never answers
        // must not pin this loop — a timeout is a retry, not an exit
        const r = await fetch(u, { signal: AbortSignal.any([signal, AbortSignal.timeout(WAITS.watchFetchIo.ms)]) })
        if (!r.ok) {
          await fail()
          continue
        }
        const j = (await r.json()) as { changed?: boolean; cursor?: string }
        cursor = j.cursor ?? cursor
        backoff = WAITS.watchBackoffMin.ms
        if (j.changed === true && !stopped()) {
          rt.hotUntil = Date.now() + WAITS.hotWindow.ms
          rt.countWake('watch')
          // a wake spawns a pass — spawned work never inherits the
          // spawning chain's leg context (watch loops are context-free
          // today; the rule stays explicit so a refactor can't regress it)
          void detached(() => rt.deals().catch((e) => log(`automation: watch: ${(e as Error).message}`)))
        }
      } catch {
        if (!alive()) return
        await fail()
      }
    }
  }
  rt.refreshWatches = async (): Promise<void> => {
    if (rt.watchBases.length === 0 || rt.stopped) return
    const want = new Map<string, { base: string; service: string; query: unknown }>()
    try {
      const w = await rt.engine.watchList()
      const net = cfg.network
      for (const domain of w.sellerDomains)
        for (const base of rt.watchBases) want.set(`${base}|ls_nektar|wants:${domain}`, { base, service: 'ls_nektar', query: { wants: { net, domain } } })
      for (const c of w.collecting)
        for (const base of rt.watchBases)
          want.set(`${base}|ls_nektar|offers:${c.wantId}`, { base, service: 'ls_nektar', query: { offers: { net, wantId: c.wantId, buyerPub: rt.me } } })
      // office watch only when envelopes ride lanes (cfg.messages bypasses)
      if (lanes !== undefined && cfg.messages === undefined) {
        const q = { office: cfg.boxOffice ?? lanes.mod.officeFor(), to: rt.me }
        for (const base of rt.watchBases) want.set(`${base}|ls_bbox|office`, { base, service: 'ls_bbox', query: q })
      }
    } catch {
      return
    }
    for (const [k, ac] of rt.watches)
      if (!want.has(k)) {
        ac.abort()
        rt.watches.delete(k)
      }
    for (const [k, w] of want)
      if (!rt.watches.has(k)) {
        const ac = new AbortController()
        rt.watches.set(k, ac)
        const work = watchOne(w.base, w.service, w.query, ac.signal).catch(() => undefined)
        rt.inflight.add(work)
        const done = (): void => {
          rt.inflight.delete(work)
        }
        void work.then(done, done)
      }
  }
}
