// the public surface of @nektar/automation — explicit re-exports (was 17
// star-exports, which made the promised boundary invisible to consumers;
// consumers reach the app through AutomationHost and the runtime/store seams)
export { type PreflightCheck, type PreflightReport, type Automation } from './types.js'
export { type OperatorWallet, AutomationHost } from './host.js'
export { type AutomationConfig, requeueAckItems, openAutomation } from './runtime.js'
export {
  type Operation,
  type InboxItem,
  type OutboxItem,
  type BudgetAmounts,
  type BudgetResult,
  type TxState,
  type WalletTx,
  type Approval,
  RevisionConflict,
  AutomationStore
} from './store.js'
export { type TxObservationInput, wrapBroadcast, leasePredicate, TransactionWatcher } from './ledger.js'
export {
  type MarketMessage,
  type MarketTransport,
  type MarketMemory,
  type MarketDeps,
  type MarketOptions,
  type StuckExit,
  STATE_LIMITS,
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
export { type WalletDeps, type TxObservation, WalletCoordinator } from './wallet.js'
export {
  type ToolboxHandle,
  NEKTAR_BASKET,
  RegtestWalletServices,
  openToolboxWallet,
  ToolboxFunder,
  fundToolbox,
  LiveWalletServices,
  ToolboxReserveFunder
} from './toolbox.js'
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
