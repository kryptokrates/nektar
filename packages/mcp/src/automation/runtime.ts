import { existsSync, mkdirSync, readFileSync } from 'node:fs'
import { join } from 'node:path'
import type { Knex } from 'knex'
import type { SignedWant, SignedOffer } from '@nektar/overlay'
import type { NektarMemory } from '../memory.js'
import { sendEnvelope, collect } from '../mailbox.js'
import { AutomationStore } from './store.js'
import { MarketEngine, type MarketMessage, type MarketTransport, type MarketOptions } from './market.js'
import { ScriptedExecutor, HttpChatExecutor, type AIExecutor } from './ai.js'
import type { Automation } from './types.js'

export type { Automation } from './types.js'

export interface AutomationConfig {
  network: string
  dataDir?: string
  rendezvousUrls: string[]
  keeperUrl?: string
  overlayToken?: string
  ai?: AIExecutor
  intervalMs?: number
  call: (tool: string, args: any) => Promise<any>
  opts?: Partial<MarketOptions>
  log?: (line: string) => void
}

const KIND = { quote: 'offer', accept: 'acceptance', delivered: 'delivery-reference' } as const

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
  const rvs = cfg.rendezvousUrls.map(u => u.replace(/\/+$/, ''))
  const keeper = (cfg.keeperUrl ?? rvs[0])?.replace(/\/+$/, '')
  const log = cfg.log ?? (() => {})
  const post = async (path: string, body: unknown): Promise<void> => {
    let ok = false
    let last = 'no rendezvous configured'
    for (const rv of rvs) {
      try {
        const r = await fetch(`${rv}${path}`, { method: 'POST', headers: { 'content-type': 'application/json', ...(cfg.overlayToken ? { authorization: `Bearer ${cfg.overlayToken}` } : {}) }, body: JSON.stringify(body), signal: AbortSignal.timeout(15_000) })
        const j = await r.json().catch(() => ({})) as { ok?: boolean }
        if (r.ok && j.ok === true) ok = true; else last = `${rv}${path} → ${r.status}`
      } catch (e) { last = `${rv}${path}: ${(e as Error).message}` }
    }
    if (!ok) throw new Error(last)
  }
  const transport: MarketTransport = {
    publishWant: (sw: SignedWant) => post('/want', sw),
    publishOffer: (so: SignedOffer) => post('/offer', so),
    send: async (to: string, msg: MarketMessage) => {
      if (!keeper) throw new Error('no mailbox keeper configured')
      await sendEnvelope(key, keeper, { recipient: to, kind: KIND[msg.type], payload: { market: msg }, conversationId: msg.wantId, operationId: msg.offerId, expiresAt: Date.now() + 86_400_000 })
    }
  }
  const engine = new MarketEngine({
    store, call: cfg.call, memory: mem as never, key, network: cfg.network,
    height: () => mem.chainHeight(), fetchTx: txid => mem.fetchTx(txid),
    broadcast: tx => mem.broadcastTx(tx),
    settleEscrow: tx => { for (const inp of tx.inputs) { const id = inp.sourceTXID ?? inp.sourceTransaction?.id('hex'); if (id) mem.markSpentOut(id, inp.sourceOutputIndex ?? 0) } if (tx.outputs[1]) mem.addOwnedOut(tx, 1) },
    transport, ai: cfg.ai ?? aiFromEnv(), opts: cfg.opts
  })
  const seen = new Set<string>()
  const poll = async (): Promise<void> => {
    for (const rv of rvs) {
      try {
        const source = `rv:${rv}`
        const since = Number(await store.cursor(source) ?? -1)
        const r = await fetch(`${rv}/events?since=${since}&limit=500`, { signal: AbortSignal.timeout(15_000) })
        if (!r.ok) throw new Error(`events → ${r.status}`)
        const j = await r.json() as { cursor?: number, events?: Array<{ seq: number, kind: string, payload: unknown }> }
        const events = j.events ?? []
        const last = events.length ? events[events.length - 1]!.seq : since
        if (events.length) await engine.ingestEvents(source, events, String(last))
      } catch (e) { log(`automation: ${rv}: ${(e as Error).message}`) }
    }
    if (keeper) {
      try {
        for (const m of await collect(key, keeper, seen)) {
          const msg = (m.payload as { market?: MarketMessage })?.market
          if (msg && typeof msg.type === 'string') await engine.ingestMessage(m.sender, msg)
        }
      } catch (e) { log(`automation: mailbox: ${(e as Error).message}`) }
    }
  }
  let busy = false
  const cycle = async (): Promise<void> => {
    if (busy) return
    busy = true
    try { await poll(); await engine.tick() } catch (e) { log(`automation: ${(e as Error).message}`) } finally { busy = false }
  }
  const timer = cfg.intervalMs === 0 ? undefined : setInterval(() => { void cycle() }, cfg.intervalMs ?? 5000)
  timer?.unref()
  return {
    store, engine, cycle,
    stop: async () => { if (timer) clearInterval(timer); while (busy) await new Promise(r => setTimeout(r, 25)); await kx.destroy() }
  }
}
