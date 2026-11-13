import { Transaction, type ChainTracker } from '@bsv/sdk'
import {
  HeaderTracker,
  isNativeSource,
  type LookupFormula,
  type LookupQuestion,
  type LookupService,
  type Module,
  type ModuleHost,
  type OutputAdmittedByTopic,
  type OutputSpent,
  type TopicManager
} from '@lightwebinc/bcommon'
import { checkCarrier, checkSweep, carrierLockingKey } from './carrier.js'
import { allLanes, decodePayload, lanesFor, verifyBody, bodyPub, bodyDomains, bodyExp, MAX_LIFETIME_MS, LS_NAME, type MarketPayload } from './market.js'

interface Host extends Partial<ModuleHost> {
  log: ModuleHost['log']
  chainTracker?: ChainTracker
  engine?: { submit: (beef: number[], topics: readonly string[]) => Promise<unknown> }
}

function trackerOf(host: Host): ChainTracker {
  if (host.chainTracker !== undefined) return host.chainTracker
  const url = process.env.OVERLAY_CHAIN_TRACKER_URL ?? ''
  if (!isNativeSource(url)) throw new Error('nektar module: host.chainTracker unset and OVERLAY_CHAIN_TRACKER_URL is not a native header source')
  return new HeaderTracker(url)
}

const laneNet = (topic: string): string | undefined => /^tm_nektar_([a-z0-9]+)_[0-9a-f]$/.exec(topic)?.[1]

// a carrier admitted on unproven funding ancestry must anchor within ~one
// block or its claims are evicted — bounds a double-spent tree's orphaned
// entries (the BEEF 0-conf rule needs an expiry since no probe can watch
// the funding outpoint's conflicting spend through ModuleHost today)
const FUNDING_PROOF_MS = 10 * 60_000

class LaneIndex {
  entries = new Map<string, { beef: number[]; payload: MarketPayload; outpoint: string; fundingDeadline?: number }>()
  byOutpoint = new Map<string, string>()
  // outpoint already claimed by an admitted carrier; second carrier refused, same carrier fine
  claim(outpoint: string, txid: string): 'admit' | 'dup' | 'reject' {
    if (this.byOutpoint.get(outpoint) === txid) return 'dup'
    if (this.byOutpoint.has(outpoint)) return 'reject'
    this.byOutpoint.set(outpoint, txid)
    return 'admit'
  }
  record(txid: string, beef: number[], payload: MarketPayload, outpoint: string, fundingDeadline?: number): void {
    this.entries.set(txid, { beef, payload, outpoint, fundingDeadline })
    this.byOutpoint.set(outpoint, txid)
  }
  spent(txid: string): void {
    const e = this.entries.get(txid)
    if (e) {
      this.entries.delete(txid)
      this.byOutpoint.delete(e.outpoint)
    }
  }
  spendOutpoint(o: string): void {
    const t = this.byOutpoint.get(o)
    if (t !== undefined) this.spent(t)
  }
}

function makeTopicManager(
  net: string,
  lane: string,
  tracker: ChainTracker,
  idx: LaneIndex,
  now: () => number,
  log?: (m: string, x?: Record<string, unknown>) => void
): TopicManager {
  const say = log ?? (() => {})
  return {
    getDocumentation: async () => `nektar market lane ${lane}`,
    getMetaData: async () => ({ name: lane, shortDescription: 'nektar market lane' }),
    identifyNeededInputs: async () => [],
    identifyAdmissibleOutputs: async (beef: number[]) => {
      const c = await checkCarrier(beef, { tracker })
      if (!c.ok) {
        const sw = await checkSweep(beef, { tracker, heldOutpoints: (op) => idx.byOutpoint.has(op) })
        if (sw.ok) return { outputsToAdmit: [0], coinsToRetain: [] }
        say('tm refuse', { lane, reason: c.reason })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      let p: MarketPayload
      try {
        p = decodePayload(c.payload)
      } catch {
        say('tm refuse', { lane, reason: 'payload' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      const exp = bodyExp(p)
      if (exp === undefined || !Number.isSafeInteger(exp) || exp <= now() || exp > now() + MAX_LIFETIME_MS) {
        say('tm refuse', { lane, reason: 'exp' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      if (!verifyBody(p, now())) {
        say('tm refuse', { lane, reason: 'sig' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      if (carrierLockingKey(bodyPub(p)) !== c.lockingKey) {
        say('tm refuse', { lane, reason: 'binding' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      const b = p.body as { want?: { net?: string }; offer?: { net?: string } }
      if ((p.kind === 'want' ? b.want?.net : b.offer?.net) !== net) {
        say('tm refuse', { lane, reason: 'net' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      let lanes: string[]
      try {
        lanes = lanesFor(net, bodyDomains(p))
      } catch {
        say('tm refuse', { lane, reason: 'domains' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      if (!lanes.includes(lane)) {
        say('tm refuse', { lane, reason: 'lane' })
        return { outputsToAdmit: [], coinsToRetain: [] }
      }
      const r = idx.claim(c.fundingOutpoint, c.carrier.id('hex'))
      return { outputsToAdmit: r === 'reject' ? [] : [0], coinsToRetain: [] }
    }
  }
}

function makeLookup(nets: string[], lanes: string[], shared: Map<string, LaneIndex>, tracker: ChainTracker, now: () => number): LookupService {
  const idxOf = (topic: string): LaneIndex | undefined => shared.get(topic)
  const match = (f: (e: { payload: MarketPayload }) => boolean): Array<{ txid: string; outputIndex: number }> => {
    const out: Array<{ txid: string; outputIndex: number }> = []
    for (const l of lanes) {
      const ix = shared.get(l)!
      // retired artifacts are neither returned nor kept; a carrier admitted
      // on unproven funding ancestry is evicted when its proof deadline lapses
      for (const [txid, e] of ix.entries) {
        const exp = bodyExp(e.payload)
        if ((exp !== undefined && exp <= now()) || (e.fundingDeadline !== undefined && e.fundingDeadline <= now())) ix.spent(txid)
      }
      for (const [txid, e] of ix.entries) if (f(e)) out.push({ txid, outputIndex: 0 })
    }
    return out
  }
  const wantNet = (p: MarketPayload): string | undefined => {
    const b = p.body as { want?: { net?: string }; offer?: { net?: string } }
    return p.kind === 'want' ? b.want?.net : b.offer?.net
  }
  return {
    admissionMode: 'whole-tx',
    spendNotificationMode: 'txid',
    getDocumentation: async () => 'nektar market lookup',
    getMetaData: async () => ({ name: LS_NAME, shortDescription: 'nektar market lookup' }),
    outputAdmittedByTopic: async (p: OutputAdmittedByTopic) => {
      const idx = idxOf(p.topic)
      if (idx === undefined || p.mode !== 'whole-tx') return
      const c = await checkCarrier(p.atomicBEEF, { tracker })
      const txid = Transaction.fromAtomicBEEF(p.atomicBEEF).id('hex')
      if (c.ok) {
        let m: MarketPayload | undefined
        try {
          m = decodePayload(c.payload)
        } catch {
          /* sweep tombstone */
        }
        if (m !== undefined) idx.record(txid, p.atomicBEEF, m, c.fundingOutpoint, c.unprovenFunding ? now() + FUNDING_PROOF_MS : undefined)
        return
      }
      // admitted sweep: only a MINED sweep frees held outpoints
      const sw = await checkSweep(p.atomicBEEF, { tracker, heldOutpoints: (op) => idx.byOutpoint.has(op) })
      if (sw.ok) for (const op of sw.spends) idx.spendOutpoint(op)
    },
    outputSpent: (p: OutputSpent) => {
      for (const l of lanes) shared.get(l)!.spent(p.txid)
    },
    outputEvicted: (txid: string) => {
      for (const l of lanes) shared.get(l)!.spent(txid)
    },
    lookup: async (q: LookupQuestion): Promise<LookupFormula> => {
      if (q.service !== LS_NAME) throw new Error(`unknown service ${q.service}`)
      const query = q.query as Record<string, unknown>
      for (const k of Object.keys(query ?? {})) if (!['wants', 'offers', 'want'].includes(k)) throw new Error(`unknown member ${k}`)
      const member = (query.wants ?? query.offers ?? query.want) as { net?: string } | undefined
      if (member === undefined || !nets.includes(String(member.net))) throw new Error(`unknown net ${String(member?.net)}`)
      const net = String(member.net)
      let out: Array<{ txid: string; outputIndex: number }>
      if (query.wants !== undefined) {
        const w = query.wants as { net: string; domain: string; limit?: number }
        out = match(
          (e) =>
            e.payload.kind === 'want' &&
            wantNet(e.payload) === net &&
            bodyDomains(e.payload)
              .map((d) => d.toLowerCase())
              .includes(String(w.domain).toLowerCase())
        ).slice(0, w.limit ?? 100)
      } else if (query.offers !== undefined) {
        const o = query.offers as { net: string; wantId: string; buyerPub: string }
        out = match(
          (e) =>
            e.payload.kind === 'offer' &&
            wantNet(e.payload) === net &&
            (e.payload.body as { offer: { wantId?: string; buyerPub?: string } }).offer.wantId === o.wantId &&
            (e.payload.body as { offer: { buyerPub?: string } }).offer.buyerPub === o.buyerPub
        )
      } else {
        const w = query.want as { net: string; pub: string; wantId: string }
        out = match(
          (e) =>
            e.payload.kind === 'want' &&
            wantNet(e.payload) === net &&
            (e.payload.body as { want: { pub: string; wantId: string } }).want.pub === w.pub &&
            (e.payload.body as { want: { wantId: string } }).want.wantId === w.wantId
        )
      }
      return out
    }
  }
}

export function createNektarModule(host: Host, nets: string[], opts?: { now?: () => number }): Required<Module> {
  const now = opts?.now ?? Date.now
  const shared = new Map<string, LaneIndex>()
  const topics: Record<string, TopicManager> = {}
  for (const net of nets)
    for (const lane of allLanes(net)) {
      const ix = new LaneIndex()
      shared.set(lane, ix)
      topics[lane] = makeTopicManager(net, lane, trackerOf(host), ix, now, host.log)
    }
  const lookups: Record<string, LookupService> = {}
  lookups[LS_NAME] = makeLookup(nets, nets.flatMap(allLanes), shared, trackerOf(host), now)
  return { topics, lookups }
}

export default function create(host: Host): Required<Module> {
  const nets = (nektarEnv('NEKTAR_NETS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  if (nets.length === 0) throw new Error('NEKTAR_NETS is required')
  const topicsEnv = (process.env.OVERLAY_TOPICS ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  const lanes = new Set(nets.flatMap(allLanes))
  for (const t of topicsEnv) if (laneNet(t) !== undefined && !lanes.has(t)) throw new Error(`topic ${t} is not a nektar lane of NEKTAR_NETS`)
  return createNektarModule(host, nets)
}
