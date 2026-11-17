import { decodePayload, verifyBody, bodyPub, bodyExp, type MarketPayload } from './market-wire.js'
import { carrierLockingKey, checkCarrier } from './carrier.js'
import { sha256, toHex } from '@nektar/core'
import type { ChainTracker } from '@bsv/sdk'

export type Quorum = 'all' | 'majority' | 'one' | number

/** positive-verdict memo keyed by carrier bytes: an identical BEEF re-checked
 *  every poll skips SPV/parse/signature work, while a FAILED check is never
 *  cached — a transient header miss must be retried next pass */
export class VerifiedCache<T> {
  private readonly m = new Map<string, T>()
  constructor(private readonly max = 2048) {}
  static key(beef: number[]): string {
    return toHex(sha256(Uint8Array.from(beef)))
  }
  get(k: string): T | undefined {
    const v = this.m.get(k)
    if (v !== undefined) {
      this.m.delete(k)
      this.m.set(k, v)
    }
    return v
  }
  set(k: string, v: T): void {
    this.m.delete(k)
    this.m.set(k, v)
    if (this.m.size > this.max) this.m.delete(this.m.keys().next().value!)
  }
  delete(k: string): void {
    this.m.delete(k)
  }
}

const need = (q: Quorum | undefined, n: number): number =>
  q === 'all' ? n : q === 'one' ? 1 : typeof q === 'number' ? Math.max(1, Math.min(q, n)) : Math.ceil(n / 2)

export interface SubmitAllResult {
  ok: boolean
  took: string[]
  admitted: string[]
  missed: Array<{ host: string; error: string }>
  /** legs that had not answered when an `early` call resolved at quorum —
   *  they are neither success nor failure yet; `settled` reports their fate */
  pending: string[]
  /** every leg's final answer — resolves once all hosts have answered (for
   *  non-early calls it is already resolved). Never rejects. */
  settled: Promise<{ took: string[]; missed: Array<{ host: string; error: string }> }>
}

export async function submitAll(
  hosts: string[],
  beef: number[],
  topics: string[],
  opts?: { quorum?: Quorum; timeoutMs?: number; retryWaits?: number[]; early?: boolean }
): Promise<SubmitAllResult> {
  const timeoutMs = opts?.timeoutMs ?? 15000
  // journaled callers pass retryWaits:[] — their own journal is the retry layer,
  // so a dead host costs one refused fetch instead of ~7s of sleeps per cycle
  const waits = opts?.retryWaits ?? [1000, 2000, 4000]
  const n = need(opts?.quorum, hosts.length)
  type Leg = { host: string; ok: boolean; admitted?: boolean; error?: string }
  const per = async (host: string): Promise<Leg> => {
    for (let t = 0; t <= waits.length; t++) {
      try {
        const r = await fetch(`${host}/submit`, {
          method: 'POST',
          headers: { 'content-type': 'application/octet-stream', 'x-topics': JSON.stringify(topics) },
          body: Buffer.from(beef),
          signal: AbortSignal.timeout(timeoutMs)
        })
        if (r.ok) {
          const steak = (await r.json().catch(() => ({}))) as Record<string, { outputsToAdmit?: number[] }>
          const admitted = topics.some((t) => (steak[t]?.outputsToAdmit?.length ?? 0) > 0)
          return { host, ok: true, admitted }
        }
        if (r.status < 500) return { host, ok: false, error: `http ${r.status}` }
        if (t === waits.length) return { host, ok: false, error: `http ${r.status}: ${(await r.text().catch(() => '')).slice(0, 120)}` }
      } catch (e) {
        if (t === waits.length) return { host, ok: false, error: (e as Error).message.slice(0, 80) }
      }
      await new Promise((r) => setTimeout(r, waits[t]))
    }
    return { host, ok: false, error: 'retries exhausted' }
  }
  const jobs = hosts.map(per)
  // every leg's final answer — never rejects; an early caller uses it to
  // learn what the stragglers did after quorum was declared
  const settled = Promise.all(jobs).then((rs) => ({
    took: rs.filter((r) => r.ok).map((r) => r.host),
    missed: rs.filter((r) => !r.ok).map((r) => ({ host: r.host, error: r.error! }))
  }))
  // early: resolve the moment `n` hosts took it — a slow straggler no longer
  // prices every send at its own latency; its leg still completes in the
  // background so the object lands on every host anyway. Failure can only be
  // known once all legs answered, so a failed call still waits for all.
  const answered = new Set<string>()
  const rs =
    opts?.early === true
      ? await new Promise<Leg[]>((resolve) => {
          const acc: Leg[] = []
          let ok = 0
          for (const j of jobs)
            void j.then((r) => {
              acc.push(r)
              answered.add(r.host)
              if (r.ok) ok++
              if (ok >= n || acc.length === hosts.length) resolve([...acc])
            })
        })
      : await Promise.all(jobs)
  const took = rs.filter((r) => r.ok).map((r) => r.host)
  const admitted = rs.filter((r) => r.ok && r.admitted === true).map((r) => r.host)
  const missed = rs.filter((r) => !r.ok).map((r) => ({ host: r.host, error: r.error! }))
  const pending = hosts.filter((h) => !answered.has(h))
  return { ok: took.length >= n, took, admitted, missed, pending, settled }
}

export async function lookupAll(
  hosts: string[],
  service: string,
  query: unknown,
  opts?: { timeoutMs?: number; quorum?: number }
): Promise<{ answers: Array<{ host: string; outputs: Array<{ beef: number[]; outputIndex: number }> } | { host: string; error: string }> }> {
  const per = async (host: string) => {
    try {
      const r = await fetch(`${host}/lookup`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ service, query }),
        signal: AbortSignal.timeout(opts?.timeoutMs ?? 15000)
      })
      if (!r.ok) return { host, error: `http ${r.status}` }
      const j = (await r.json()) as { type?: string; outputs?: Array<{ beef: number[]; outputIndex: number }> }
      return { host, outputs: j.outputs ?? [] }
    } catch (e) {
      return { host, error: (e as Error).message.slice(0, 80) }
    }
  }
  const quorum = opts?.quorum ?? hosts.length
  if (quorum >= hosts.length) return { answers: await Promise.all(hosts.map(per)) }
  // quorum-N: resolve the moment N hosts have answered (success or error —
  // the caller asked for N independent reads, not N successes). Slow/dead
  // hosts no longer gate the read by their full timeout; unanswered hosts
  // are simply absent from `answers`, not fabricated failures.
  return {
    answers: await new Promise((resolve) => {
      const out: Array<Awaited<ReturnType<typeof per>>> = []
      let settled = 0
      let ok = 0
      for (const h of hosts) {
        void per(h).then((r) => {
          out.push(r)
          settled++
          if ('outputs' in r) ok++
          if (ok >= quorum || settled === hosts.length) resolve(out)
        })
      }
    })
  }
}

export interface ReadResult {
  items: Array<{ txid: string; payload: import('./market-wire.js').MarketPayload }>
  perHost: Record<string, string[]>
  withheld: Array<{ host: string; txid: string }>
  unreachable: string[]
}

export interface ReadOpts {
  tracker: ChainTracker
  net: string
  accept: (p: MarketPayload) => boolean
  now?: number
  timeoutMs?: number
  quorum?: number
  /** memo of carriers that already passed SPV + signature — a hit skips that
   *  work; the time/query-dependent gates below are re-applied every read */
  cache?: VerifiedCache<{ txid: string; payload: MarketPayload; lockingKey: string }>
}

export async function readItems(hosts: string[], service: string, query: unknown, o: ReadOpts): Promise<ReadResult> {
  const { answers } = await lookupAll(hosts, service, query, { timeoutMs: o.timeoutMs, quorum: o.quorum })
  const items = new Map<string, { txid: string; payload: import('./market-wire.js').MarketPayload }>()
  const seenBy = new Map<string, Set<string>>()
  const perHost: Record<string, string[]> = {}
  const unreachable: string[] = []
  const netOf = (p: MarketPayload): string | undefined => {
    const b = p.body as { want?: { net?: string }; offer?: { net?: string } }
    return p.kind === 'want' ? b.want?.net : b.offer?.net
  }
  for (const a of answers) {
    if ('error' in a || !('outputs' in a)) {
      unreachable.push(a.host)
      continue
    }
    perHost[a.host] = []
    // carriers verify in parallel — a host may answer many, and each check
    // can mean a header fetch against the tracker; a VerifiedCache hit skips
    // all of it (negative verdicts are never cached, so a transient miss is
    // always retried)
    type Check =
      | { hit: { txid: string; payload: MarketPayload; lockingKey: string }; ck: string; c?: undefined }
      | { c: Awaited<ReturnType<typeof checkCarrier>> & { ok: true }; ck: string; hit?: undefined }
      | undefined
    const checks: Check[] = await Promise.all(
      a.outputs.map(async (o2): Promise<Check> => {
        // a malformed/error output (host-reported, no beef) must not poison
        // the whole listing — skip it, the rest of the lane still verifies
        if (!Array.isArray(o2.beef) || o2.beef.length === 0) return undefined
        try {
          const ck = VerifiedCache.key(o2.beef)
          const hit = o.cache?.get(ck)
          if (hit !== undefined) return { hit, ck }
          const c = await checkCarrier(o2.beef, { tracker: o.tracker }).catch(() => undefined)
          if (c === undefined || !c.ok) return undefined
          return { c, ck }
        } catch {
          return undefined
        }
      })
    )
    for (const r of checks) {
      if (r === undefined) continue
      const txid = r.hit !== undefined ? r.hit.txid : r.c.carrier.id('hex')
      if (!seenBy.has(txid)) {
        seenBy.set(txid, new Set())
        if (r.hit !== undefined) {
          // verified before — only re-apply what can change between reads:
          // expiry (`now` gates nothing else in the shape checks), this
          // read's net, and the caller's accept
          const exp = bodyExp(r.hit.payload)
          const expired = exp !== undefined && exp <= (o.now ?? Date.now())
          if (expired) o.cache?.delete(r.ck)
          if (!expired && netOf(r.hit.payload) === o.net && o.accept(r.hit.payload)) items.set(txid, { txid, payload: r.hit.payload })
        } else {
          try {
            const p = decodePayload(r.c.payload)
            if (verifyBody(p, o.now) && carrierLockingKey(bodyPub(p)) === r.c.lockingKey && netOf(p) === o.net && o.accept(p)) {
              items.set(txid, { txid, payload: p })
              o.cache?.set(r.ck, { txid, payload: p, lockingKey: r.c.lockingKey })
            }
          } catch {
            // undecodable payload — the empty seenBy set already recorded
          }
        }
      }
      if (items.has(txid)) {
        seenBy.get(txid)!.add(a.host)
        perHost[a.host].push(txid)
      }
    }
  }
  const withheld: Array<{ host: string; txid: string }> = []
  const unreachableSet = new Set(unreachable)
  for (const [txid, hs] of seenBy) if (items.has(txid)) for (const h of hosts) if (!hs.has(h) && !unreachableSet.has(h)) withheld.push({ host: h, txid })
  return { items: [...items.values()], perHost, withheld, unreachable }
}

export const readWants = (hosts: string[], q: { net: string; domain: string; limit?: number }, o: Omit<ReadOpts, 'accept'>) =>
  readItems(
    hosts,
    'ls_nektar',
    { wants: q },
    {
      ...o,
      accept: (p) => p.kind === 'want' && (p.body as { want: { domains: string[] } }).want.domains.map((d) => d.toLowerCase()).includes(q.domain.toLowerCase())
    }
  )
export const readOffers = (hosts: string[], q: { net: string; wantId: string; buyerPub: string }, o: Omit<ReadOpts, 'accept'>) =>
  readItems(
    hosts,
    'ls_nektar',
    { offers: q },
    {
      ...o,
      accept: (p) =>
        p.kind === 'offer' &&
        (p.body as { offer: { wantId?: string; buyerPub?: string } }).offer.wantId === q.wantId &&
        (p.body as { offer: { buyerPub?: string } }).offer.buyerPub === q.buyerPub
    }
  )
export const readWant = (hosts: string[], q: { net: string; pub: string; wantId: string }, o: Omit<ReadOpts, 'accept'>) =>
  readItems(
    hosts,
    'ls_nektar',
    { want: q },
    {
      ...o,
      accept: (p) =>
        p.kind === 'want' &&
        (p.body as { want: { pub: string; wantId: string } }).want.pub === q.pub &&
        (p.body as { want: { wantId: string } }).want.wantId === q.wantId
    }
  )
