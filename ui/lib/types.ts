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
    utxos: number
    fuelUtxos?: number
    reserveUtxos?: number
    defaultUtxos?: number
    walletMode?: string
    feeRateSatPerKB?: number
    funded: boolean
    payments: string
  }
  receipts: { count: number; earned: number; earned24h: number; amounts: number[]; list: Receipt[] }
  memory: { shards: number; records: number; anchored: number; unproven: number; coverage: number; prover: string }
  feed?: FeedFragment[]
  ai?: AIStatus | null
  market: ListedShard[]
  marketTape?: MarketTapeEvent[]
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

export type NavTab = 'home' | 'memory' | 'wallet' | 'node'

export type DrawerTarget =
  | { kind: 'memory'; item: FeedFragment }
  | { kind: 'receipt'; item: Receipt }
  | { kind: 'deal'; item: DealFeedEntry }
  | { kind: 'peer'; item: PeerInfo }
  | null
