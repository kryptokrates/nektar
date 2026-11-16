// index.ts — the bstack surface, grouped by what a reader implements.
// Adding an export here is a wire-contract decision: the first group is what
// a counterparty implementation speaks (envelopes, codecs, queries); the
// second is the delivery plane that runs it (lanes, trees, hosts, journal);
// the last is the sandbox-host module a stock overlay loads.

// ── wire contract ──────────────────────────────────────────────────────────
export {
  MAX_PAYLOAD,
  MAX_DOMAINS,
  MAX_LIFETIME_MS,
  LS_NAME,
  encodePayload,
  decodePayload,
  laneNibble,
  laneOf,
  allLanes,
  lanesFor,
  verifyBody,
  bodyPub,
  bodyDomains,
  bodyExp,
  type MarketPayload
} from './market-wire.js'
export {
  BOX,
  DEFAULT_OFFICE,
  officeFor,
  boxFundingLock,
  sendBox,
  ackBox,
  readBox,
  type BoxRef,
  type BoxItem
} from './box.js'
export {
  LOG_KIND,
  DEFAULT_STREAM,
  logTopic,
  logFundingLock,
  appendLog,
  readLog,
  publishMinedLog,
  anchorLog,
  type LogState,
  MemoryLogState,
  KnexLogState
} from './log.js'
export {
  FINGER_KIND,
  FINGER_TOPIC,
  fingerFundingLock,
  checkFingerBody,
  publishFinger,
  publishSubRecord,
  resolveFinger,
  resolveStoreMembers,
  fingerHostAllowed,
  type FingerBody,
  type FingerStateRow,
  type FingerState,
  MemoryFingerState,
  KnexFingerState
} from './finger.js'
export {
  marketJson,
  wantTaskDigest,
  streamRef,
  streamCommit,
  signWant,
  verifyWant,
  publicQuoteError,
  signOffer,
  verifyOffer,
  type WantAd,
  type SignedWant,
  type WantInput,
  type OfferAd,
  type SignedOffer,
  type OfferInput
} from './artifacts.js'

// ── delivery plane ─────────────────────────────────────────────────────────
export {
  DERIVATION,
  PROVISION_KIND,
  FUNDING_TAG,
  ORIGINATOR,
  MAX_TREE_OUTPUTS,
  mintTree,
  marketFundingLock,
  anchoredTree,
  matchProvision,
  fundingSlotReason,
  MemoryTreeLedger,
  KnexTreeLedger,
  feeFor,
  type TreeLedger,
  type Fees
} from './tree.js'
export { TREE_SIZE, ensureTrees } from './trees.js'
export {
  submitAll,
  lookupAll,
  readItems,
  readWants,
  readOffers,
  readWant,
  VerifiedCache,
  type Quorum,
  type SubmitAllResult,
  type ReadResult,
  type ReadOpts
} from './hosts.js'
export { LANE_TABLES, migrateLaneTables } from './lanes.js'
export { MemoryCarrierJournal, KnexCarrierJournal, type CarrierJournal } from './journal.js'
export { artifactHash, publishArtifact, resubmitCarrier, republishOne, republishProved } from './publish.js'
export {
  walletOrProto,
  buildCarrier,
  buildSweep,
  checkCarrier,
  checkSweep,
  carrierLockingKey,
  carrierFundingLockHex,
  type CarrierCheck,
  type SweepCheck
} from './carrier.js'

// ── sandbox host module ────────────────────────────────────────────────────
export { createNektarModule } from './module.js'
