export type FeedFragment = {
  shard: string
  seq: number
  hdp: string
  text: string
  encrypted?: boolean
  borgGroup?: string
  level: number
  class?: string
  batchN: number
  anchorTxid?: string
  settled?: boolean
  tainted?: boolean
  createdAt: number
  path?: string
  mediaType?: string
  chirpUrl?: string
  mpeRoot?: string
  commitHash?: string
}

export type RepoAnchorSummary = {
  n: number
  size: number
  txid: string
  R: string
  Rp: string
  settled: boolean
  blockHeight: number
  createdAt: number
}

export type RepoTag = {
  repo: string
  tag: string
  commitHash: string
  anchorTxid?: string
  anchorN: number
  message?: string
  authorPub?: string
  signature?: string
  createdAt: number
}

export type UnifiedDiffLine = {
  op: ' ' | '+' | '-' | string
  text: string
}

export type UnifiedFileDiff = {
  status: 'added' | 'modified' | 'deleted' | string
  path: string
  binary?: boolean
  additions: number
  deletions: number
  lines?: UnifiedDiffLine[]
}

export type BlameLine = {
  lineNo: number
  text: string
  commitHash: string
  authorPub?: string
  modelId?: string
  timestamp: number
  anchorTxid?: string
  anchorN: number
}

export type MergeConflict = {
  path: string
  baseText?: string
  oursText?: string
  theirsText?: string
  binary?: boolean
}

export type CapsuleVerifyReport = {
  ok: boolean
  repo: string
  branch: string
  headCommit: string
  commitsVerified: number
  treesVerified: number
  blobsVerified: number
  anchorsVerified: number
  settledAnchors: number
  genesisShardId?: string
  reason?: string
}

export type TreeDiffEntry = {
  kind: 'added' | 'modified' | 'deleted' | string
  path: string
  oldBlobHash?: string
  newBlobHash?: string
}

export type CapsuleInspectResult = {
  repo: string
  branch: string
  genesisPub?: string
  genesisShardId?: string
  ownerPub?: string
  headCommit?: string
  exportedAt: number
  verification: CapsuleVerifyReport
  commits: RepoCommit[]
  tree?: { treeHash?: string; entries?: RepoTreeEntry[] }
  blobs?: Record<string, RepoBlob>
  anchors?: RepoAnchorSummary[]
  localExists: boolean
  diffVsLocal?: TreeDiffEntry[]
}

export type RepoSummary = {
  repo: string
  branch: string
  branches: string[]
  tags?: RepoTag[]
  genesisPub?: string
  genesisShardId?: string
  headCommit?: string
  commitCount: number
  fileCount: number
  fragmentCount: number
  anchorCount: number
  settledAnchors: number
  headAnchorTxid?: string
  headAnchorN: number
  updatedAt: number
  latestMessage?: string
  anchors?: RepoAnchorSummary[]
}

export type RepoParentRef = {
  commitHash: string
  anchorTxid?: string
  anchorVout?: number
  repoGenesis?: string
}

export type RepoCommit = {
  v: number
  commitHash: string
  repo: string
  branch: string
  seq?: number
  parents?: RepoParentRef[]
  treeHash: string
  mpeRoot: string
  authorPub: string
  modelId: string
  policyCommit?: string
  message: string
  timestamp: number
  signature: string
  anchorTxid?: string
  anchorN: number
  fragmentLeaf?: string
}

export type RepoTreeEntry = {
  mode: string
  path: string
  blobHash: string
  mediaType: string
  size: number
  contentSha256: string
  chirpUrl?: string
  encrypted?: boolean
}

export type RepoBlob = {
  blobHash: string
  path: string
  mediaType: string
  size: number
  contentSha256: string
  inline?: string
  chirpUrl?: string
  encrypted?: boolean
  borgGroup?: string
  borgEpoch?: number
}

export type Receipt = {
  seq: number
  tool: string
  sats: number | null
  shards: string[]
  at: number | null
  url: string
  token?: string
  answer?: string
  anchored?: boolean
  verdict?: 'green' | 'amber' | 'red'
}

export type Host = {
  url: string
  state: string
  saved?: boolean
}

export type PendingApproval = {
  id: string
  reason: string
  maxExtraSats: number
  operationId: string
  awakeableId?: string
  expiresAt: string
}

export type AutomationOp = {
  role: string
  kind: string
  state: string
  lastError?: string
}

export type DealFeedEntry = {
  op: string
  at: number
  to: string
  cause: string
  role?: 'buyer' | 'seller' | string
  priceSats?: number
  shard?: string
  paymentTxid?: string
  deliveredText?: string
  matchAudit?: {
    mode?: string
    action?: string
    score?: number
    confidence?: number
    reason?: string
    fallback?: boolean
  }
}

export type MarketTapeEvent = {
  id: string
  kind: 'want' | 'offer' | 'deal' | string
  side: 'buy' | 'sell' | 'clear' | string
  domains?: string[]
  summary?: string
  sats: number
  pub?: string
  shard?: string
  status?: string
  isSelf?: boolean
  at: number
}

export type PulseSignal = {
  seq: number
  event: string
  ts: number
  tape?: MarketTapeEvent
}

export type WaitRow = {
  actionId?: string
  operationId: string
  kind: string
  status: string
  deferClass?: string | null
  lastError?: string | null
  attempts: number
  nextAt: number
}

export type RestateRuntime = {
  executor: string
  adminUrl: string | null
  pending: WaitRow[]
  nextAt: number | null
  journal: Array<{ op: string; at: number; to: string; cause: string }>
}

export type ListedShard = {
  shard: string
  domains: string[]
  records: number
  works: number
  earned: number
  offers: string
  active?: boolean
  grade?: 'S' | 'A' | 'B' | 'C' | string
  unitCount?: number
  modes?: string[]
  priceSats?: number
  pricePerUnit?: boolean
  queryPriceSats?: number
  subscriptionPriceSats?: number
  freshnessClass?: string
  usageRights?: string
}

export type CommodityTickerRow = {
  ontologyId: string
  grade: 'S' | 'A' | 'B' | 'C' | string
  pricingMode: 'spot' | 'per_query' | 'subscription' | string
  minSatsPer1kUnits: number
  medianSatsPer1kUnits: number
  minSatsPerTriple: string
  medianSatsPerTriple: string
  minAskSats: number
  activeOffers: number
  bondedSellers: number
  liveFeeds: number
  totalUnitsAvailable: number
  domains?: string[]
  sampleShards?: string[]
}

export type CommodityTickerResponse = {
  rows: CommodityTickerRow[]
  updatedAt: number
  advisoryNote: string
}

export type PeerInfo = {
  pub: string
  handle?: string
  publicUrl?: string
  bboxOffice?: string
  sellDomains?: string[]
  bountyDomains?: string[]
  isHub?: boolean
}

export type BountyInfo = {
  id: string
  domain: string
  question: string
  rewardSats: number
  filled: number
  maxFills: number
}

export type UnmatchedWant = {
  wantId: string
  domain: string
  query: string
  maxSats: number
  seenAt: number
}

export type PolicyRow = {
  policyHash: string
  policyId: string
  approvedAt: number
  approvedBy: string
  revokedAt?: number
  active: boolean
  policy: {
    roles: string[]
    domains?: string[]
    limits: Record<string, number>
    seller?: Record<string, unknown>
    aiMatching?: Record<string, unknown>
  } | null
}

export type AIStatus = {
  enabled: boolean
  mode: string
  requests?: number
  rejections?: number
  escalations?: number
  fallbacks?: number
  errors?: number
  avgLatencyMs?: number
  endpoint?: string
  model?: string
  circuitOpen?: boolean
}

export type AssetBasketItem = {
  outpoint: string
  txid: string
  vout: number
  satoshis: number
  spendable: boolean
  tags?: string[]
  customInstructions?: string
  meta?: Record<string, unknown>
}

export type AssetBasketSummary = {
  basket: string
  title: string
  description: string
  count: number
  totalSats: number
  items: AssetBasketItem[]
  error?: string
}

export type EconomicsSummary = {
  tokensIn: number
  tokensOut: number
  tokensSaved: number
  earnedSats: number
  spentSats: number
  netSats: number
  factsBought: number
  factsSold: number
  workBounties: number
  estSavedUsdCents: number
}

export type EconomicsState = {
  today: EconomicsSummary
  allTime: EconomicsSummary
}

export type ReputationVector6D = {
  subjectPub: string
  ontologyId: string
  accBp: number
  consBp: number
  dispBp: number
  predBp: number
  collabBp: number
  provBp: number
  eigenTrustBp: number
  compositeBp: number
  directDeals: number
  attestCount: number
  updatedAt: number
}

export type ReputationAttestation = {
  attestHash: string
  role: 'buyer_rates_seller' | 'seller_rates_buyer' | string
  issuerPub: string
  subjectPub: string
  ontologyId: string
  dealId: string
  dealReceiptHash: string
  token: string
  evidenceHash?: string
  accBp: number
  consBp: number
  dispBp: number
  predBp: number
  collabBp: number
  provBp: number
  compositeBp: number
  createdAt: number
}

export type DealOutcome = {
  dealId: string
  subjectPub: string
  ontologyId: string
  exitCode: number
  note?: string
  recordedAt: number
}

export type ReputationState = {
  ontologyId: string
  ontologies: string[]
  vectors: ReputationVector6D[]
  attestations: ReputationAttestation[]
  outcomes: DealOutcome[]
  trustedSellers?: string[] | null
  priorityBuyers?: string[] | null
  sybilOrUntrusted?: string[] | null
  advisoryNote: string
}

export type DistillQuarantine = {
  id: string
  shard: string
  seq: number
  ruleId: string
  contentHash: string
  createdAt: number
}

export type SleepCycleState = {
  status?: string
  lastRunAt?: number
  totalRuns?: number
  totalDistilled?: number
  totalQuarantined?: number
  totalDropped?: number
  totalSuperseded?: number
  priceDecayed?: number
  lastError?: string
}

export type DistillState = {
  distilledCount: number
  quarantinedCount: number
  quarantined: DistillQuarantine[]
  marketShards: string[]
  idleSeconds: number
  lastCycle?: SleepCycleState
}

export type MicroEarnerState = {
  enabled: boolean
  status: 'funded_idle' | 'earning' | 'disabled' | 'error' | string
  targetFloatSats: number
  balanceSats: number
  earnedSatsTotal: number
  tasksCompleted: number
  bountiesSubmitted: number
  lastRunAt: number
  lastError?: string
}

export type State = {
  network: string
  model: string
  pub: string
  uptimeSec: number
  loop: { steps: { name: string; done: boolean }[]; done: number; frontier: string; next: string }
  treasury: {
    address?: string | null
    identityPub?: string | null
    sats: number
    confirmedSats?: number
    unprovenSats?: number
    reservedSats?: number
    spent?: number
    spent24h?: number
    dailySpent?: number
    dailyCapSats?: number
    perDealCapSats?: number
    vault?: {
      txid: string
      sats: number
      maxPerSpendSats: number
      paused: boolean
    } | null
    utxos: number
    fuelUtxos?: number
    reserveUtxos?: number
    defaultUtxos?: number
    walletMode?: string
    custody?: 'bsv-desktop' | 'embedded' | string
    baskets?: AssetBasketSummary[]
    basketItems?: number
    basketLockedSats?: number
    feeRateSatPerKB?: number
    funded: boolean
    payments: string
  }
  receipts: { count: number; earned: number; earned24h: number; amounts: number[]; list: Receipt[] }
  memory: { shards: number; repos?: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
  repos?: RepoSummary[]
  feed?: FeedFragment[]
  ai?: AIStatus | null
  market: ListedShard[]
  ticker?: CommodityTickerResponse
  marketTape?: MarketTapeEvent[]
  economics?: EconomicsState
  reputation?: ReputationState
  distill?: DistillState
  microEarner?: MicroEarnerState
  net: {
    hosts: Host[]
    seeds: string[]
    slots: Record<string, number> | null
    boot: string | null
    push: string | null
    peers?: PeerInfo[]
    bounties?: BountyInfo[]
    liquidity?: {
      fillRate: number
      medianTimeToOfferMs: number
      zeroRate: number
      hubSupplyShare: number
      organicSellers: number
      activePeers: number
      sseWakes: number
      pollWakes: number
      bboxWakes: number
    } | null
    unmatchedWants?: UnmatchedWant[]
  }
  automation: {
    pending: PendingApproval[]
    ops: AutomationOp[]
    deals?: DealFeedEntry[]
    rt: RestateRuntime
  } | null
  share: { enabled: boolean; url: string }
  settings: {
    policies: PolicyRow[]
    hosts: { env: string[]; added: string[]; blocked: string[] }
  }
}

export type DockClient = {
  id: string
  label: string
  tier: string
  tierLabel: string
  summary: string
  configPath: string
  detected: boolean
  docked: boolean
  oneClick: boolean
  note?: string
}

export type HubFeeSummary = {
  wantSats: number
  offerSats: number
  bboxSats: number
  egressSatsPer100: number
  minTopupSats: number
}

export type HubCatalogEntry = {
  url: string
  name: string
  source: 'seed' | 'joined' | 'discovered' | 'blocked' | string
  connected: boolean
  reachable: boolean
  latencyMs: number
  network?: string
  networkMismatch?: boolean
  role?: 'spine' | 'leaf' | string
  feePreset?: string
  fees?: HubFeeSummary | null
  topics?: string[]
  linkedHubs?: string[]
  peersCount?: number
  error?: string
}

export type NavTab = 'home' | 'agents' | 'hubs' | 'wallet' | 'settings'

export type DrawerTarget =
  | { kind: 'memory'; item: FeedFragment }
  | { kind: 'receipt'; item: Receipt }
  | { kind: 'deal'; item: DealFeedEntry }
  | { kind: 'peer'; item: PeerInfo }
  | null
