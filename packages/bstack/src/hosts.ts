import { decodePayload, verifyBody, bodyPub } from './market.js'
import { carrierLockingKey, checkCarrier } from './carrier.js'
import type { ChainTracker } from '@bsv/sdk'

export type Quorum = 'all' | 'majority' | 'one' | number

const need = (q: Quorum | undefined, n: number): number =>
  q === 'all' ? n : q === 'one' ? 1 : typeof q === 'number' ? Math.max(1, Math.min(q, n)) : Math.ceil(n / 2)

export async function submitAll(
  hosts: string[],
  beef: number[],
  topics: string[],
  opts?: { quorum?: Quorum; timeoutMs?: number; retryWaits?: number[]; early?: boolean }
): Promise<{ ok: boolean; took: string[]; admitted: string[]; missed: Array<{ host: string; error: string }> }> {
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
  // early: resolve the moment `n` hosts took it — a slow straggler no longer
  // prices every send at its own latency; its leg still completes in the
  // background so the object lands on every host anyway. Failure can only be
  // known once all legs answered, so a failed call still waits for all.
  const rs =
    opts?.early === true
      ? await new Promise<Leg[]>((resolve) => {
          const acc: Leg[] = []
          let ok = 0
          for (const j of jobs)
            void j.then((r) => {
              acc.push(r)
              if (r.ok) ok++
              if (ok >= n || acc.length === hosts.length) resolve([...acc])
            })
        })
      : await Promise.all(jobs)
  const took = rs.filter((r) => r.ok).map((r) => r.host)
  const admitted = rs.filter((r) => r.ok && r.admitted === true).map((r) => r.host)
  const missed = rs.filter((r) => !r.ok).map((r) => ({ host: r.host, error: r.error! }))
  return { ok: took.length >= n, took, admitted, missed }
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
  items: Array<{ txid: string; payload: import('./market.js').MarketPayload }>
  perHost: Record<string, string[]>
  withheld: Array<{ host: string; txid: string }>
  unreachable: string[]
}

export async function readItems(
  hosts: string[],
  service: string,
  query: unknown,
  o: { tracker: ChainTracker; net: string; accept: (p: import('./market.js').MarketPayload) => boolean; now?: number; timeoutMs?: number }
): Promise<ReadResult> {
  const { answers } = await lookupAll(hosts, service, query, { timeoutMs: o.timeoutMs })
  const items = new Map<string, { txid: string; payload: import('./market.js').MarketPayload }>()
  const seenBy = new Map<string, Set<string>>()
  const perHost: Record<string, string[]> = {}
  const unreachable: string[] = []
  for (const a of answers) {
    if ('error' in a || !('outputs' in a)) {
      unreachable.push(a.host)
      continue
    }
    perHost[a.host] = []
    // carriers verify in parallel — a host may answer many, and each check
    // can mean a header fetch against the tracker
    const checks = await Promise.all(a.outputs.map(async (o2) => await checkCarrier(o2.beef, { tracker: o.tracker }).catch(() => undefined)))
    for (const c of checks) {
      if (c === undefined || !c.ok) continue
      const txid = c.carrier.id('hex')
      if (!seenBy.has(txid)) {
        try {
          const p = decodePayload(c.payload)
          const b = p.body as { want?: { net?: string }; offer?: { net?: string } }
          const pnet = p.kind === 'want' ? b.want?.net : b.offer?.net
          if (!verifyBody(p, o.now) || carrierLockingKey(bodyPub(p)) !== c.lockingKey || pnet !== o.net || !o.accept(p)) {
            seenBy.set(txid, new Set())
            continue
          }
          items.set(txid, { txid, payload: p })
          seenBy.set(txid, new Set())
        } catch {
          seenBy.set(txid, new Set())
          continue
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

export const readWants = (
  hosts: string[],
  q: { net: string; domain: string; limit?: number },
  o: { tracker: ChainTracker; net: string; now?: number; timeoutMs?: number }
) =>
  readItems(
    hosts,
    'ls_nektar',
    { wants: q },
    {
      ...o,
      accept: (p) => p.kind === 'want' && (p.body as { want: { domains: string[] } }).want.domains.map((d) => d.toLowerCase()).includes(q.domain.toLowerCase())
    }
  )
export const readOffers = (
  hosts: string[],
  q: { net: string; wantId: string; buyerPub: string },
  o: { tracker: ChainTracker; net: string; now?: number; timeoutMs?: number }
) =>
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
export const readWant = (hosts: string[], q: { net: string; pub: string; wantId: string }, o: { tracker: ChainTracker; net: string; now?: number }) =>
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
