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
  /** free funding-tree slots per kind — { market, bbox, log, finger }; zeros
   *  when no market hosts are configured */
  marketSlots: () => Promise<{ market: number; bbox: number; log: number; finger: number }>
  /** this agent's blogs deal-journal stream id (undefined when journal off) */
  logStream?: string
  /** on-demand journal view for `deals { operationId }` — own head plus the
   *  counterparty's stream filtered to this deal */
  dealJournal?: (op: import('./store.js').Operation) => Promise<Record<string, unknown> | undefined>
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
