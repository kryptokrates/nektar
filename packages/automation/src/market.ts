// MarketEngine split — the engine is layered by phase under ./market/ as a
// chain of abstract classes (types → MarketBase → IngressOps → SellerOps →
// BuyerOps), each extending the last. This file is the stable './market.js'
// specifier: it re-exports the shared types/constants and the concrete
// engine class, so no import site changes.
import { BuyerOps } from './market/buyer.js'

export { STATE_LIMITS, TERMINAL } from './market/types.js'
export type { MarketDeps, MarketMemory, MarketMessage, MarketOptions, MarketTransport, StuckExit } from './market/types.js'

export class MarketEngine extends BuyerOps {}
