import type { AutomationStore } from './store.js'
import type { MarketEngine } from './market.js'

export interface Automation {
  store: AutomationStore
  engine: MarketEngine
  cycle: () => Promise<void>
  stop: () => Promise<void>
}
