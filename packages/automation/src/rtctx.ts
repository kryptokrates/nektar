// rtctx.ts — the runtime's shared context: one object carrying the locals the
// old openAutomation closure shared across its lanes/outbox/upkeep/watches
// regions plus the cross-region doors the regions call each other through.
// Regions read/write rt.* exactly where the closure read/wrote its locals —
// the assembler fills the construction slots before opening the regions.
import type { Knex } from 'knex'
import type { PrivateKey } from '@bsv/sdk'
import type { AutomationHost, OperatorWallet } from './host.js'
import type { AutomationStore } from './store.js'
import type { MarketEngine, MarketMessage, MarketOptions } from './market.js'
import type { AIExecutor } from './ai.js'

export interface AutomationConfig {
  network: string
  dataDir?: string
  ai?: AIExecutor
  intervalMs?: number
  /** how often the timer runs upkeep (trees, identity record, journal); push events run it at once */
  upkeepMs?: number
  call: (tool: string, args: any) => Promise<any>
  opts?: Partial<MarketOptions>
  log?: (line: string) => void
  marketHosts?: string[]
  /** the operator's own hosts: accept http and loopback/private addresses in
   *  identity records (env NEKTAR_MARKET_LOCAL_HOSTS=1). Off, a record may
   *  only name public https hosts. */
  localHosts?: boolean
  messages?: {
    send: (toPub: string, msg: MarketMessage, refs?: import('@nektar/bstack').BoxRef[]) => Promise<void>
    collect: () => Promise<Array<{ from: string; msg: MarketMessage; refs?: import('@nektar/bstack').BoxRef[] }>>
    ack?: () => Promise<void>
  }
  treeSize?: number
  boxOffice?: string
  treeLowWater?: number
  /** unproven mints allowed in flight per kind — non-market lanes default 2
   *  so one mint's block-wait never gates the next refill (market stays 1:
   *  its slots serve on anchored ancestry anyway) */
  treeMaxUnproven?: number
  /** blogs deal-journal stream id (`<name>_<10 chars>`); every deal event is
   *  appended as a signed numbered batch to tm_log_<stream> - env NEKTAR_LOG_STREAM */
  logStream?: string
  /** confirmations a settlement proof must carry before a completion may
   *  settle (default 1) — regtest 1, live the memory's finality depth */
  settleDepth?: number
  /** bstore/S3 object store for deal evidence — absent = feature off.
   *  Default: objectStoreFromEnv() (NEKTAR_BSTORE_*). */
  objectStore?: import('@nektar/store').S3ObjectStore
  /** A9 — strict startup: refuse to open the runtime unless preflight is
   *  green. Non-strict starts anyway, seeded with the checks' down-host set. */
  strict?: boolean
  /** NEKTAR_UNATTENDED — the A11 conformance token becomes a required check;
   *  honored only while the token validates. Implies strict when set. */
  unattended?: boolean
  /** stronger broadcaster liveness probe (the transport knows the endpoint —
   *  an HTTP answer proves the socket where txFacts can't) */
  broadcasterProbe?: () => Promise<boolean>
  /** publish our bfinger identity record in upkeep — opt-in (it costs sats
   *  and makes this agent publicly discoverable; NEKTAR_BFINGER_PUBLISH=1) */
  publishFinger?: boolean
  /** publish our directory announce on the reserved dir lane in upkeep —
   *  opt-in (NEKTAR_ANNOUNCE=1): a signed card naming our hosts + offered
   *  seed packs so fresh instances can enumerate live operators without a
   *  baked-in registry */
  announce?: boolean
  /** watchdog cadence — the cheap checks (chain/broadcaster/hosts) re-run on
   *  this tick (default 60s; 0 disables) */
  watchdogMs?: number
  /** watch-shim base URLs (NEKTAR_WATCH_URLS) — long-poll wake in front of
   *  ordinary BRC-24 hosts. Each subscription mirrors a query the poll loop
   *  already issues; a changed result-set runs deals() at once. Optional:
   *  absent/down shim = plain polling. */
  watchHosts?: string[]
  /** this agent's own watch-shim URL, advertised in the bfinger record so
   *  counterparties can /notify our long-polls on outbound sends (requires
   *  publishFinger — records without it simply carry no watch field) */
  watchPublic?: string
  /** this agent's own push base URL, advertised in the bfinger record so
   *  counterparties POST a signed carrier BEEF to `${push}/v1/lane` for
   *  immediate ingest (requires publishFinger — records without it simply
   *  carry no push field; senders fall back to store-and-forward) */
  pushPublic?: string
  /** per-read failure ceiling on lane/box host lookups (NEKTAR_LANE_READ_TIMEOUT_MS)
   *  — a host that cannot answer inside this bound is treated as unreachable,
   *  never waited on beyond it. Default 5000. */
  laneReadTimeoutMs?: number
  /** hot-path poll cadence while any op is non-terminal (NEKTAR_HOT_INTERVAL_MS)
   *  — the loop runs min(hotMs, idleMs); a wake or inbound item keeps it hot.
   *  Default 250. */
  hotMs?: number
  /** minimum time a want stays in `collecting` before an acceptable offer may
   *  early-select (NEKTAR_MIN_COLLECT_MS) — a cheaper offer landing inside the
   *  window still wins; collectMs stays the outer ceiling. Default 1000. */
  minCollectMs?: number
  /** quiet-extended collect window (NEKTAR_COLLECT_QUIET_MS) — opt-in; see
   *  MarketOptions.collectQuietMs. Default 0 (off). */
  collectQuietMs?: number
  now?: () => number
}

/** the lane infrastructure bundle the assembler builds when marketHosts is
 *  non-empty — funding-tree ledger, carrier journal, log + finger state and
 *  the lazily-imported @nektar/bstack module itself */
export interface LaneInfra {
  mod: typeof import('@nektar/bstack')
  ledger: InstanceType<typeof import('@nektar/bstack').KnexTreeLedger>
  journal: InstanceType<typeof import('@nektar/bstack').KnexCarrierJournal>
  logState: InstanceType<typeof import('@nektar/bstack').KnexLogState>
  fingerState: InstanceType<typeof import('@nektar/bstack').KnexFingerState>
}

/** a resolved bfinger record — the counterparty's advertised hosts/office/
 *  stream/watch, cached positive 10 min / negative 1 min by the lanes layer */
export interface FingerRec {
  hosts: string[]
  office: string
  stream: string
  watch?: string
  /** direct-push base URL — POST signed carriers to `${push}/v1/lane`;
   *  absent = this peer only answers store-and-forward reads */
  push?: string
}

type Msgs = NonNullable<AutomationConfig['messages']>

/** the shared runtime context — every field is one of the old closure's
 *  locals, kept mutable so regions observe each other's writes exactly as
 *  captured lets did */
export interface Rt {
  cfg: AutomationConfig
  mem: AutomationHost
  kx: Knex
  kxLanes?: Knex
  store: AutomationStore
  key: PrivateKey
  wallet: OperatorWallet
  objectStore: import('@nektar/store').S3ObjectStore | undefined
  marketHosts: string[]
  log: (line: string) => void
  lanes?: LaneInfra
  logStream?: string
  logState?: LaneInfra['logState']
  fingerState?: LaneInfra['fingerState']
  packsStore?: import('@nektar/store').PacksStore
  localNet: boolean
  me: string
  engine: MarketEngine
  msgs?: Msgs
  fanoutIo: {
    enqueue: (o: Omit<import('./store.js').OutboxItem, 'attempts'>, opts?: { delayMs?: number }) => Promise<unknown>
    ackOutbox: (id: string) => Promise<unknown>
    pullOutbox: (id: string) => Promise<unknown>
  }
  // shared loop state — written by one region, read by others (and by stop)
  stopped: boolean
  /** the one timer wheel — every recurring job registers a name + ceiling
   *  here instead of owning a private interval (Stage 6) */
  scheduler?: import('./scheduler.js').Scheduler
  inflight: Set<Promise<void>>
  downNow: Set<string>
  hotUntil: number
  busy: boolean
  upkeepRun?: Promise<void>
  upkeepAt: number
  acking: boolean
  pumping: boolean
  pumpAgain: boolean
  pumpDone?: Promise<void>
  watchBases: string[]
  watches: Map<string, AbortController>
  // cross-region doors — assigned by the region openers (and the assembler
  // for deals/cycle) before any of them can run
  resolveRecord: (pub: string, fresh?: boolean) => Promise<FingerRec | undefined>
  hostsFor: (pub: string) => Promise<string[]>
  notifyPeer: (rec: { watch?: string } | undefined, service: string, query?: unknown) => void
  /** a signed carrier pushed straight to us by its sender — verify + ingest
   *  exactly like the box/lane read it precedes (deduped by the same hashes).
   *  Returns the carrier kind ingested, or false when nothing verified. */
  pushIngest?: (beef: number[]) => Promise<'envelope' | 'artifact' | false>
  /** fire-and-forget carrier push to a counterparty's advertised /v1/lane —
   *  the durable host copy is already submitted when this runs */
  pushCarrier?: (rec: { push?: string } | undefined, beef: number[]) => void
  hostBack: (h: string) => Promise<void>
  queueRepublish: (treeTxid: string) => Promise<void>
  poll: () => Promise<void>
  pushAcks: () => void
  pumpOutbox: () => void
  upkeep: () => Promise<void>
  ghostMint: (tree: import('@bsv/sdk').Transaction) => void
  laneSpec: (kind: string) => Promise<{ lock: import('@bsv/sdk').LockingScript | undefined; satsPerOutput: number; size: number }>
  dealJournal: (op: import('./store.js').Operation) => Promise<Record<string, unknown> | undefined>
  refreshWatches: () => Promise<void>
  deals: () => Promise<void>
  cycle: () => Promise<void>
}
