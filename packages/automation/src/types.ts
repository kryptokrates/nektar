import type { AutomationStore } from './store.js'
import type { MarketEngine } from './market.js'

export interface PreflightCheck {
  check: string
  pass: boolean
  detail: string
  fix?: string
}

export interface PreflightReport {
  pass: boolean
  at: number
  checks: PreflightCheck[]
  /** hosts that failed the read probe — the runtime seeds its unreachable set
   *  from here so a non-strict start still knows who is deaf */
  downHosts: string[]
}

export interface Automation {
  store: AutomationStore
  engine: MarketEngine
  cycle: () => Promise<void>
  /** one deals pass (poll + tick + outbox join) — what the 1s scheduler job
   *  runs. Drivers that need a fast want/offer cadence call this per pass
   *  and keep upkeep on its own slower ceiling — cycle() = deals + upkeep
   *  serially, so a grinding housekeeping pass would hold the collect
   *  window hostage */
  deals: () => Promise<void>
  /** one housekeeping pass (funding trees, finger record, journals) —
   *  serialized internally; safe to fire detached on a slow cadence */
  upkeep: () => Promise<void>
  /** free funding-tree slots per kind — { market, bbox, log, finger }; zeros
   *  when no market hosts are configured */
  marketSlots: () => Promise<{ market: number; bbox: number; log: number; finger: number }>
  /** the stock breakdown behind marketSlots — proven leaves, anchored
   *  zero-conf leaves, trees still awaiting a block and consumed slots,
   *  per kind */
  laneStock: () => Promise<Record<string, { proven: number; anchored: number; unprovenTrees: number; taken: number }>>
  /** the durable carrier journal (delivered lane envelopes by artifact) —
   *  undefined when no market hosts are configured */
  carriers?: import('@nektar/bstack').CarrierJournal
  /** lane outputs a provisioning transaction should carry — one entry per
   *  lane with its funding lock, per-slot value and a whole tree's count */
  provisionPlan: () => Promise<{ outputs: Array<{ kind: string; lockingScriptHex: string; satoshis: number; count: number }> }>
  /** adopt the lane-locked outputs of a funding tx as slot stock —
   *  adopted count per kind ({} when nothing matched or lanes off) */
  adoptProvision: (tx: import('@bsv/sdk').Transaction) => Promise<Record<string, number>>
  /** this agent's blogs deal-journal stream id (undefined when journal off) */
  logStream?: string
  /** on-demand journal view for `deals { operationId }` — own head plus the
   *  counterparty's stream filtered to this deal */
  dealJournal?: (op: import('./store.js').Operation) => Promise<Record<string, unknown> | undefined>
  /** a signed carrier pushed straight to /v1/lane — same verify+ingest as a
   *  box/lane read (deduped by the same hashes). The kind ingested, or false */
  pushCarrier?: (beef: number[]) => Promise<'envelope' | 'artifact' | false>
  /** bstore/S3 object store in use (undefined = evidence/pack storage off) */
  objectStore?: import('@nektar/store').S3ObjectStore
  /** the latest preflight verdict — pass + the doctor-style report behind it */
  health: () => { pass: boolean; report: PreflightReport | undefined }
  /** re-run the full preflight now (the automationDoctor tool) */
  preflight: () => Promise<PreflightReport>
  /** B3 — 'sealed' = this agent publishes its bfinger record (publicly
   *  discoverable); 'unsealed' = it still requires configured hosts */
  bootstrap?: () => 'sealed' | 'unsealed'
  stop: () => Promise<void>
}
