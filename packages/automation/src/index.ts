// the public surface of @nektar/automation — explicit re-exports (was 17
// star-exports, which made the promised boundary invisible to consumers;
// consumers reach the app through AutomationHost and the runtime/store seams)
export { type PreflightCheck, type PreflightReport, type Automation } from './types.js'
export { type OperatorWallet, AutomationHost } from './host.js'
export { type AutomationConfig, requeueAckItems, trackFanout, queueRepairs, openAutomation } from './runtime.js'
export {
  type Operation,
  type InboxItem,
  type OutboxItem,
  type BudgetAmounts,
  type BudgetResult,
  type TxState,
  type WalletTx,
  type Approval,
  type DeferClass,
  RevisionConflict,
  LeaseConflictError,
  AutomationStore,
  classifyDeliveryError
} from './store.js'
export { type TxObservationInput, wrapBroadcast, leasePredicate, splitDailySpend, TransactionWatcher, custodyTxEffects, txFromRow } from './ledger.js'
export { emitTxEvent, replayTxEffects, inertTxEffects, TX_EFFECT_OF, IN_TX_INTENTS, DESCRIPTIVE_INTENTS, type TxEffects } from './machines/effects.js'
export * from './rt/index.js'
export { obsEvent, opsFsmLegacy } from './machines/driver.js'
export {
  type MarketMessage,
  type MarketTransport,
  type MarketMemory,
  type MarketDeps,
  type MarketOptions,
  type StuckExit,
  STATE_LIMITS,
  NONTERMINAL,
  MarketEngine
} from './market.js'
export {
  type ActionContext,
  type PolicyDecision,
  type AutomationPolicy,
  type PurchaseIntent,
  type OfferIntent,
  policyError,
  canonicalJson,
  digestOf,
  policyHash,
  decidePurchase,
  decideOffer
} from './policy.js'
export {
  type ScoreWeights,
  type ScoringPolicy,
  type ScoreCandidate,
  type OfferRanking,
  SCORE_DIMENSIONS,
  DEFAULT_SCORE_WEIGHTS,
  rankOffers
} from './scoring.js'
export { type WalletDeps, type TxObservation, WalletCoordinator, DEFINITIVE, NEVER_LEFT, broadcastProvablyDead } from './wallet.js'
export {
  type ToolboxHandle,
  type ToolboxStoreHandle,
  type BasketRow,
  NEKTAR_BASKET,
  openToolboxWallet,
  openToolboxStore,
  basketRows,
  ToolboxFunder,
  fundToolbox
} from './toolbox.js'
export { RegtestWalletServices, LiveWalletServices } from './toolbox-services.js'
export { SqlFunds } from './sqlfunds.js'
export { type PreflightDeps, WATCHDOG_CHECKS, runPreflight } from './preflight.js'
export { type TradeEvidence, type RedactedTradeEvidence, redactTradeEvidence, collectTradeEvidence, verifyTradeEvidence } from './evidence.js'
export {
  type ConformanceClaims,
  type ConformanceToken,
  TOKEN_TTL_MS,
  conformanceConfigHash,
  suiteDigest,
  currentCommit,
  operatorPubs,
  verifyToken,
  issueToken,
  conformanceStatus,
  defaultSuiteDir
} from './conformance.js'
export { type KillState, killSwitchActive } from './kill.js'
export {
  type PatchProposal,
  type SandboxSpec,
  type SandboxResult,
  type IsolationBackend,
  parsePatch,
  validatePatch,
  dockerBackend,
  evaluatePatch
} from './sandbox.js'
export { type AIJob, type AIResult, type AIExecutor, type HttpChatConfig, validateResult, ScriptedExecutor, HttpChatExecutor } from './ai.js'
export { BudgetAuthority } from './budget.js'
