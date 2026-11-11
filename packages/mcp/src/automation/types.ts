import type { AutomationStore } from './store.js'
import type { MarketEngine } from './market.js'

export interface Automation {
  store: AutomationStore
  engine: MarketEngine
  cycle: () => Promise<void>
  /** free funding-tree slots per kind — { market, bbox }; zeros when no
   *  market hosts are configured */
  marketSlots: () => Promise<{ market: number, bbox: number }>
  stop: () => Promise<void>
}
