import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Knex } from 'knex'
import type { SignedWant, SignedOffer } from '@nektar/overlay'
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
  messages?: { send: (toPub: string, msg: MarketMessage) => Promise<void>, collect: () => Promise<Array<{ from: string, msg: MarketMessage }>> }
  treeSize?: number
  boxOffice?: string
  treeLowWater?: number
  now?: () => number
}

export function aiFromEnv (env = process.env): AIExecutor {
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
  const transport: MarketTransport = {
    publishWant: async (sw: SignedWant) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await lanes.mod.publishArtifact({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, net: cfg.network, payload: { v: 1, kind: 'want', body: sw } })
    },
    publishOffer: async (so: SignedOffer) => {
      if (lanes === undefined) throw new Error('no market hosts configured (NEKTAR_MARKET_HOSTS)')
      await lanes.mod.publishArtifact({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, net: cfg.network, payload: { v: 1, kind: 'offer', body: so } })
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
    broadcast: tx => mem.broadcastTx(tx),
    settleEscrow: tx => { for (const inp of tx.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } if (tx.outputs[1]) mem.addOwnedOut(tx, 1) },
    transport, ai: cfg.ai ?? aiFromEnv(), opts: cfg.opts
  })
  const me = key.toPublicKey().toString()
  const withheldSeen = new Set<string>()
  const downNow = new Set<string>()
  const laneNotes = (r: { withheld: Array<{ host: string, txid: string }>, unreachable: string[] }): void => {
    for (const w of r.withheld) { const k2 = `${w.host}:${w.txid}`; if (!withheldSeen.has(k2)) { withheldSeen.add(k2); log(`automation: withheld ${w.host} ${w.txid}`) } }
    for (const h of r.unreachable) if (!downNow.has(h)) { downNow.add(h); log(`automation: unreachable ${h}`) }
    for (const h of [...downNow]) if (!r.unreachable.includes(h)) { downNow.delete(h); log(`automation: host back ${h}`) }
  }
  const msgs = cfg.messages ?? (lanes === undefined ? undefined : {
    send: async (to: string, msg: MarketMessage): Promise<void> => {
      try {
        await lanes.mod.sendBox({ key, ledger: lanes.ledger, journal: lanes.journal, hosts: marketHosts, office: cfg.boxOffice, to, body: { market: msg }, expiresAt: Math.floor(Date.now() / 1000) + 86400 })
      } catch (e) { log(`automation: box: ${(e as Error).message}`); throw e }
    },
    collect: async (): Promise<Array<{ from: string, msg: MarketMessage }>> => {
      try {
        const tracker = await mem.sdkTracker()
        const r = await lanes.mod.readBox({ key, hosts: marketHosts, office: cfg.boxOffice, tracker })
        laneNotes(r)
        const out: Array<{ from: string, msg: MarketMessage }> = []
        for (const i of r.items) { const m = (i.body as { market?: MarketMessage }).market; if (m !== undefined && typeof m === 'object') out.push({ from: i.from, msg: m }) }
        return out
      } catch (e) { log(`automation: box: ${(e as Error).message}`); return [] }
    }
  })
  const lanePoll = async (): Promise<void> => {
    const { readWants, readOffers } = lanes!.mod
    const watch = await engine.watchList()
    const tracker = await mem.sdkTracker()
    const net = cfg.network
    for (const domain of watch.sellerDomains) {
      const r = await readWants(marketHosts, { net, domain }, { tracker, net })
      const events = r.items.map(i => ({ seq: i.txid, kind: 'want', payload: i.payload.body }))
      if (events.length) await engine.ingestEvents(`lane:${net}:${domain}`, events, '')
      laneNotes(r)
    }
    for (const c of watch.collecting) {
      const r = await readOffers(marketHosts, { net, wantId: c.wantId, buyerPub: me }, { tracker, net })
      const events = r.items.map(i => ({ seq: i.txid, kind: 'offer', payload: i.payload.body }))
      if (events.length) await engine.ingestEvents(`offers:${c.wantId}`, events, '')
      laneNotes(r)
    }
  }
  const poll = async (): Promise<void> => {
    if (lanes !== undefined) { try { await lanePoll() } catch (e) { log(`automation: lanes: ${(e as Error).message}`) } }
    if (msgs !== undefined) {
      try { for (const m of await msgs.collect()) await engine.ingestMessage(m.from, m.msg) } catch (e) { log(`automation: messages: ${(e as Error).message}`) }
    }
  }
  let busy = false
  const cycle = async (): Promise<void> => {
    if (busy) return
    busy = true
    try {
      if (lanes !== undefined) {
        try {
          for (const kind of ['market', 'bbox'] as const) await lanes.mod.ensureTrees({
            key, ledger: lanes.ledger, lowWater: cfg.treeLowWater, size: cfg.treeSize, kind,
            lock: kind === 'bbox' ? await lanes.mod.boxFundingLock(key) : undefined,
            takeFunding: async (sats: number) => (await mem.fundingOuts(sats))[0]!,
            broadcast: tx => mem.broadcastTx(tx),
            fetchTx: txid => mem.fetchTx(txid),
            onSpent: (tree) => { for (const inp of tree.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } const n = cfg.treeSize ?? 16; if (tree.outputs.length === n + 1 && (tree.outputs[n]!.satoshis ?? 0) > 0) mem.addOwnedOut(tree, n) }
          })
        } catch (e) { log(`automation: ensureTrees: ${(e as Error).message}`) }
      }
      await poll(); await engine.tick()
    } catch (e) { log(`automation: ${(e as Error).message}`) } finally { busy = false }
  }
  const timer = cfg.intervalMs === 0 ? undefined : setInterval(() => { void cycle() }, cfg.intervalMs ?? 5000)
  timer?.unref()
  return {
    store, engine, cycle,
    stop: async () => { if (timer) clearInterval(timer); while (busy) await new Promise(r => setTimeout(r, 25)); await kx.destroy() }
  }
}
