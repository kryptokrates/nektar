import type { AutomationStore } from './store.js'
import type { MarketEngine } from './market.js'

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
  objectStore?: import('../objectstore.js').S3ObjectStore
  /** the latest preflight verdict — pass + the doctor-style report behind it */
  health: () => { pass: boolean; report: import('./preflight.js').PreflightReport | undefined }
  /** re-run the full preflight now (the automationDoctor tool) */
  preflight: () => Promise<import('./preflight.js').PreflightReport>
  /** B3 — 'sealed' = this agent publishes its bfinger record (publicly
   *  discoverable); 'unsealed' = it still requires configured hosts */
  bootstrap?: () => 'sealed' | 'unsealed'
  stop: () => Promise<void>
}
