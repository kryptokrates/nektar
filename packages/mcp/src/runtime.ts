// openRuntime: the ONE assembly for the MCP / HTTP servers. cfg picks the
// network; a ChainPort answers every chain-side question (spend path,
// witnessed headers, proof + raw-tx lookups, the status-hint stream, height)
// and openCustody answers the coin side — the spine below is identical on
// every network. Tests inject {chain: memoryPort(...), funds} for the
// memory tier; production builds the port from cfg (regtest | live).
//   NEKTAR_NETWORK=regtest  Teranode regtest stack: persistent records
//                          (sqlite), anchor state, security state, funding
//                          reserve and keys in NEKTAR_DATA_DIR.
//   NEKTAR_NETWORK=testnet|main  live nets — same shape, real custody.
import { existsSync, readFileSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { PrivateKey, Transaction } from '@bsv/sdk'
import {
  regtestPort,
  livePort,
  temporalSourcesFor,
  indexFor,
  ChainEvents,
  type ChainPort,
  type Funds,
  type RegtestConfig,
  type MainnetConfig,
  type TxStatusEvent
} from '@nektar/chain'
import { handleBroadcasterStatus } from './http.js'
import { nektarEnv, nektarEnvNum, mkLog } from '@nektar/core'
import { NektarMemory, type NektarMemoryOptions } from './memory.js'
import { FileSecurityStore, FileStateStore } from './securitystore.js'
import { passphraseFromEnv, KeystoreError } from './keystore.js'
import { fileURLToPath } from 'node:url'
import {
  loadKeys,
  txLogBroadcast,
  acquireLock,
  keyLockFile,
  acquireInstanceLock,
  openRecordsDb,
  healLedgerOnProof,
  persistedTxStore,
  buildRecordsWatcher
} from './rtsupport.js'
import { openCustody, regtestSeed, liveFunding, type CustodyResult } from './funding.js'

export interface RuntimeConfig {
  network: 'regtest' | 'testnet' | 'main'
  dataDir: string
  regtest?: RegtestConfig & { fundWif: string }
  main?: MainnetConfig & {
    fundWif?: string // fee key; absent → load/generate fee.key.json
    passphrase?: string // NEKTAR_KEY_PASSPHRASE — mandatory on main
    feeRateSatPerKb?: number // default: max(100, broadcaster policy)
    maxFeePerTx?: number // sats (default 1000)
    maxSpendPerDay?: number // sats (default 20000)
    minBroadcastIntervalMs?: number // default 10000
  }
  modelId?: string
  batchLeaves?: number
  /** regtest only: mine a block when anchors wait for confirmation */
  autoMine?: boolean
  confirmEveryMs?: number
  /** pubkeys trusted to issue clearance credentials (level>0 reads) */
  clearanceAuthorities?: string[]
  /** GOAL-v6 S3 (§12): header/time sources for consensus time, in priority
   *  order — sources[0] is authoritative for hashes/heights. Default ['bsv']:
   *  single-source, bit-identical to the old behaviour. 'btc' adds a
   *  read-only WhatsOnChain BTC tracker whose block times vote in the
   *  weighted median. */
  temporalSources?: string[]
}

/** parse NEKTAR_TEMPORAL_SOURCES=bsv[,btc] (default bsv); unknown names are fatal */
function temporalSourceNames(env = process.env): string[] {
  const names = (env.NEKTAR_TEMPORAL_SOURCES ?? 'bsv')
    .split(',')
    .map((s) => s.trim().toLowerCase())
    .filter(Boolean)
  for (const n of names) if (n !== 'bsv' && n !== 'btc') throw new Error(`unknown temporal source "${n}" (supported: bsv,btc)`)
  return names.length === 0 ? ['bsv'] : names
}

export function configFromEnv(env = process.env): RuntimeConfig {
  const network = (env.NEKTAR_NETWORK ?? 'regtest') as RuntimeConfig['network']
  if (network !== 'regtest' && network !== 'testnet' && network !== 'main')
    throw new Error(
      `NEKTAR_NETWORK must be regtest|testnet|main — got '${String(env.NEKTAR_NETWORK)}'${env.NEKTAR_NETWORK === 'mock' ? ' (the in-process mock chain was removed — testnet is the zero-infra option)' : ''}`
    )
  const dataDir = env.NEKTAR_DATA_DIR ?? join(process.cwd(), '.nektar')
  const secret = (k: string) =>
    nektarEnv(k, env) ?? (env[`${k}_FILE`] && existsSync(env[`${k}_FILE`]!) ? readFileSync(env[`${k}_FILE`]!, 'utf8').trim() : undefined)
  const cfg: RuntimeConfig = {
    network,
    dataDir,
    modelId: env.NEKTAR_MODEL_ID,
    batchLeaves: Number(env.NEKTAR_BATCH ?? 4),
    autoMine: env.NEKTAR_AUTOMINE === '1',
    // regtest cadence (live nets override below). The confirmer's tip gate
    // makes an idle tick one height RPC — proofs surface ~1s after a block.
    confirmEveryMs: Number(env.NEKTAR_CONFIRM_MS ?? 1000),
    clearanceAuthorities: (env.NEKTAR_CLEARANCE_AUTHORITIES ?? '')
      .split(',')
      .map((s) => s.trim())
      .filter(Boolean),
    temporalSources: temporalSourceNames(env)
  }
  if (network === 'regtest') {
    const fundWif = secret('NEKTAR_FUND_WIF') ?? secret('NEKTAR_REGTEST_WIF')
    const rpcPass = secret('NEKTAR_RPC_PASS')
    if (!fundWif || !rpcPass) throw new Error(`${network}: NEKTAR_FUND_WIF (or _FILE) and NEKTAR_RPC_PASS (or _FILE) are required`)
    cfg.regtest = {
      fundWif,
      rpcUrl: env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
      rpcUser: env.NEKTAR_RPC_USER ?? 'teranode',
      rpcPass,
      propagationUrl: env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
      assetUrl: env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
    }
  }
  if (network === 'main' || network === 'testnet') {
    if (network === 'main' && env.NEKTAR_MAINNET_CONFIRM !== 'I_UNDERSTAND') {
      throw new Error('mainnet mode spends REAL BSV. Refusing to start. Set NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND to proceed.')
    }
    // testNet = any test network (testnet encoding, test coins, low fee floor)
    const testNet = network !== 'main'
    const urls =
      network === 'testnet' ? { broadcaster: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', indexer: 'https://test.bananablocks.com/api/v1' } : undefined
    cfg.confirmEveryMs = Number(env.NEKTAR_CONFIRM_MS ?? (testNet ? 30_000 : 60_000))
    cfg.main = {
      fundWif: secret('NEKTAR_FUND_WIF'),
      passphrase: secret('NEKTAR_KEY_PASSPHRASE'),
      // testnet defaults: BSV Broadcaster + WoC endpoints (probed live); main
      // keeps the built-in GorillaPool/WoC defaults when env vars are unset
      broadcasterUrl: env.NEKTAR_BROADCASTER_URL ?? urls?.broadcaster,
      // NEKTAR_BROADCASTER_URLS: comma-separated extra routes — the same
      // tx is submitted to all endpoints concurrently, first acceptance wins
      broadcasterUrls: env.NEKTAR_BROADCASTER_URLS?.split(',')
        .map((s) => s.trim().replace(/\/+$/, ''))
        .filter(Boolean),
      broadcasterApiKey: secret('NEKTAR_BROADCASTER_API_KEY'),
      indexerUrl: env.NEKTAR_INDEXER_URL ?? env.NEKTAR_WOC_URL ?? urls?.indexer,
      indexerApiKey: secret('NEKTAR_INDEXER_API_KEY') ?? secret('NEKTAR_WOC_API_KEY'),
      chaintracksUrl: env.NEKTAR_CHAINTRACKS_URL,
      // fee floor: 100 sat/kB main (GorillaPool policy), 1 sat/kB test nets
      // (a zero fee rate would produce free-of-fee txs miners may reject)
      feeRateSatPerKb: env.NEKTAR_FEE_RATE_SAT_PER_KB ? Number(env.NEKTAR_FEE_RATE_SAT_PER_KB) : testNet ? 1 : undefined,
      maxFeePerTx: Number(env.NEKTAR_MAX_FEE_SATS_PER_TX ?? 1000),
      // 'unlimited' is the documented opt-out — Number('unlimited') is NaN,
      // and NaN would silently disable EVERY cap (fee > NaN is false)
      maxSpendPerDay: env.NEKTAR_MAX_SPEND_SATS_PER_DAY === 'unlimited' ? Number.MAX_SAFE_INTEGER : Number(env.NEKTAR_MAX_SPEND_SATS_PER_DAY ?? 20_000),
      minBroadcastIntervalMs: Number(env.NEKTAR_MIN_BROADCAST_INTERVAL_MS ?? 250)
    }
  }
  return cfg
}

export interface Runtime {
  mem: NektarMemory
  close: () => Promise<void>
  network: RuntimeConfig['network']
  /** mainnet-only ops surface for /v1/wallet + /v1/costs */
  ops?: {
    wallet: () => Promise<{
      address: string
      balanceSats: number
      utxos: number
      spentTodaySats: number
      caps: { maxFeePerTx: number; maxPerDay: number; minIntervalMs: number }
      avgFeePerAnchorSats: number
      runwayAnchors: number | null
    }>
    costs: () => Array<{ txid: string; bytes: number; fee: number; spend: number; at: string }>
    /** fundingImport tool: credit a tx paying the fee address */
    importFunding?: (txid: string) => Promise<unknown>
    /** fundingRefresh tool: reconcile the wallet view against chain truth */
    refreshFunding?: () => Promise<unknown>
  }
}

/** the injected assembly surface — the memory/test tier supplies both;
 *  production supplies neither (the port is built from cfg.network) */
export interface RuntimePorts {
  chain?: ChainPort
  /** injected coin book — bypasses wallet custody entirely. Production tiers
   *  always build custody; only the in-memory/test tier injects one. */
  funds?: Funds
  wallet?: NektarMemoryOptions['wallet']
}

export async function openRuntime(cfg: RuntimeConfig, ports: RuntimePorts = {}): Promise<Runtime> {
  // fail fast: paid tools need a valid operator wallet BEFORE the first 402 —
  // an invoice served to a missing address is a payment that can never settle.
  const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
  if (paymentsEnabled()) requireOperatorWallet()
  const { objectStoreFromEnv } = await import('@nektar/store')
  if (ports.funds !== undefined && ports.chain === undefined)
    throw new Error('openRuntime: injected funds need an injected chain port — custody and chain answers must come from the same world')
  const injected = ports.funds !== undefined
  const net = ports.chain?.network ?? cfg.network
  const nlog = mkLog(`nektar ${net}`)
  const live = net === 'main' || net === 'testnet'
  const mc = cfg.main
  const bondEpoch = nektarEnv('NEKTAR_BOND_EPOCH')
  const bondMaxFee = nektarEnv('NEKTAR_BOND_MAXFEE')
  const base: NektarMemoryOptions = {
    batchPolicy: { maxLeaves: cfg.batchLeaves ?? 4, maxAgeMs: 60_000 },
    modelId: cfg.modelId ?? 'unspecified',
    clearanceAuthorities: cfg.clearanceAuthorities,
    network: net,
    objectStore: objectStoreFromEnv(),
    bond:
      bondEpoch || bondMaxFee
        ? {
            epochBlocks: Number(bondEpoch ?? 144),
            ...(bondMaxFee !== undefined ? { maxFee: BigInt(bondMaxFee) } : {})
          }
        : undefined
  }
  mkdirSync(cfg.dataDir, { recursive: true })
  // live custody needs the passphrase BEFORE keys/decrypt; the injected tier
  // has no encrypted material
  const pass = live ? mc?.passphrase : passphraseFromEnv()
  if (net === 'main' && !pass) throw new KeystoreError(`${net} requires NEKTAR_KEY_PASSPHRASE (or _FILE) for keys at rest`)
  const externalWallet = live && !injected && nektarEnv('NEKTAR_WALLET') === 'client'
  const { release: releaseLock } = await acquireInstanceLock(cfg.dataDir)
  // encryption is mandatory on main only — test-coin keys are mode-0600
  // plaintext when no passphrase is set (set it and they encrypt the same)
  const { keys, save } = loadKeys(cfg.dataDir, pass, net === 'main', !externalWallet)
  // input-value resolver for the guard's fee check — custody outs (read
  // live each call: a refresh may swap the array), stored txs, anchor txs
  const { txStore, open: openTxStore } = persistedTxStore()
  let memRef: NektarMemory | undefined
  let fundsRef: CustodyResult['funds'] | undefined
  const utxosOf = () => fundsRef?.utxos
  const valueOf = (txid: string, vout: number): number | undefined =>
    utxosOf()?.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis ??
    txStore.get(txid)?.outputs[vout]?.satoshis ??
    memRef?.anchorTx(txid)?.outputs[vout]?.satoshis
  // the chain port — built from cfg on production, injected on test tiers
  let eventsRef: ChainEvents | undefined
  const port =
    ports.chain ??
    (net === 'regtest'
      ? regtestPort(
          { ...cfg.regtest!, fundAddress: PrivateKey.fromWif(cfg.regtest!.fundWif).toPublicKey().toAddress([0x6f]) },
          {
            valueOf,
            guard: {
              // D2 hard daily cap: default 10_000 sats/day of fees on regtest
              // too — 'unlimited' is an explicit opt-out for soak/stress runs,
              // and only when EVERY set cap agrees (mixed signals never
              // silently un-guard the spend path)
              ...(() => {
                const caps = [nektarEnv('NEKTAR_DAILY_CAP_SATS'), nektarEnv('NEKTAR_MAX_SPEND_SATS_PER_DAY')].filter((x) => x !== undefined)
                const unlimited = caps.length > 0 && caps.every((x) => x === 'unlimited')
                return {
                  unlimited,
                  maxPerDay: unlimited ? Number.MAX_SAFE_INTEGER : Math.min(...caps.filter((x) => x !== 'unlimited').map((x) => Number(x)), 10_000),
                  maxFeePerTx: Number(nektarEnv('NEKTAR_MAX_FEE_SATS_PER_TX') ?? 1_000),
                  minIntervalMs: Number(nektarEnv('NEKTAR_MIN_BROADCAST_INTERVAL_MS') ?? 0)
                }
              })(),
              ledgerFile: join(cfg.dataDir, 'spend-ledger.json')
            }
          }
        )
      : await livePort(mc!, net, {
          valueOf,
          guard: {
            maxFeePerTx: mc!.maxFeePerTx,
            maxPerDay: mc!.maxSpendPerDay,
            minIntervalMs: mc!.minBroadcastIntervalMs,
            ledgerFile: join(cfg.dataDir, 'spend-ledger.json')
          },
          held: async (txid) => await memRef?.fetchTx(txid),
          note: (ev) => eventsRef?.note(ev),
          dataDir: cfg.dataDir,
          // the port's log lines carry their own `nektar <net>:` tag — bare stamp
          log: mkLog('')
        }))
  // records db + tx store open before custody — SqlFunds binds the `utxos`
  // table on the same knex and rehydrates coins from tx_store
  const { knex, store, ledger, wrapBroadcast, leasePredicate } = await openRecordsDb(cfg.dataDir)
  await openTxStore(knex)
  // custody — an injected coin book skips the whole wallet assembly
  const custody: CustodyResult = injected
    ? {
        funds: ports.funds!,
        wallet: ports.wallet,
        fundKey: cfg.regtest?.fundWif ? PrivateKey.fromWif(cfg.regtest.fundWif) : undefined,
        fundAddress: '',
        pkhHex: ''
      }
    : await openCustody(cfg, port, pass, { knex, txStore })
  fundsRef = custody.funds
  // regtest seeds its treasury before the tx-store snapshot so the minted
  // funding outs land in the parent graph like every other utxo
  if (!injected && port.network === 'regtest') {
    await regtestSeed({ rc: cfg.regtest!, port, funds: custody.funds, dataDir: cfg.dataDir })
    nlog(`coin book sqlite utxos — ${custody.funds.utxos?.length ?? 0} funding utxo(s)`)
  }
  for (const u of await custody.funds.outs()) txStore.put(u.tx)
  const shardKeys = new Map(Object.entries(keys.shardKeys).map(([s, h]) => [s, PrivateKey.fromHex(h)]))
  // the one chain-event substrate for this process: every "is it proven?"
  // consumer (confirmer, anchor parents, ledger) registers interest here and
  // receives 'tx' pushes instead of running a private poll loop. observe comes
  // from the port — each adapter knows how ITS evidence is produced.
  const events = new ChainEvents({
    intervalMs: cfg.confirmEveryMs ?? (net === 'regtest' ? 2_000 : 30_000),
    abandonAfterBlocks: port.abandonAfterBlocks,
    height: async () => await port.height(),
    observe: port.observe
  })
  eventsRef = events
  // a tracked tx any verdict calls dead can never land — its adopted outputs
  // are phantom: died() tombstones them and revives provably-live inputs.
  // Shared by the watcher (rejected/conflicted) and the abandon handler —
  // terminal-for-custody writes MUST go through this path
  const diedFromRow = async (w: Awaited<ReturnType<typeof ledger.walletTxs>>[number], tag: string): Promise<void> => {
    try {
      const tx = Transaction.fromHex(w.rawHex)
      for (const inp of tx.inputs) {
        const hex = inp.sourceTXID !== undefined ? w.parents[inp.sourceTXID] : undefined
        if (hex !== undefined) inp.sourceTransaction = Transaction.fromHex(hex)
      }
      await custody.funds.died(tx)
    } catch (e) {
      nlog(`${tag} tx ${w.txid}: died() failed: ${(e as Error).message}`)
    }
    await ledger.releaseLeases(w.actionId).catch(() => undefined)
    await (fundingRefresh?.() ?? custody.funds.refresh?.()?.then(() => undefined) ?? Promise.resolve())
  }
  events.on('abandoned', ({ txid, tip, seenAtHeight }) => {
    nlog(`tx ${txid} not mined after abandon horizon — abandoned (tip ${tip}, watched since ${seenAtHeight})`)
    // repair-by-name: an ABSENCE verdict — mark 'abandoned' (never
    // 'conflicted': that label means a proven competing spend) — so the
    // trailing watch still catches a late landing, and run the custody
    // rollback every terminal verdict owes the coin book
    void (async () => {
      try {
        const hit = (await ledger.walletTxs(['submitted', 'observed', 'submission_unknown'])).find((w) => w.txid === txid)
        if (hit !== undefined) {
          const { txEmit, executor, txFsmLegacy } = await import('@nektar/automation')
          if (!txFsmLegacy()) {
            await txEmit(
              executor(),
              ledger,
              hit,
              'abandon',
              { observation: { reason: `not mined (tip ${tip}, watched since height ${seenAtHeight})` } },
              {
                txDied: (row) => diedFromRow(row, 'abandoned')
              }
            )
          } else {
            await ledger.setTxState(hit.actionId, 'abandoned', { reason: `not mined (tip ${tip}, watched since height ${seenAtHeight})` })
            await diedFromRow(hit, 'abandoned')
          }
        }
        memRef?.events.emit('txAbandoned', { txid, tip, seenAtHeight })
      } catch (e) {
        nlog(`abandoned ${txid} mark failed: ${(e as Error).message}`)
      }
    })()
  })
  // §12: header TIMES are the weighted median across temporal sources;
  // single-source (default) is bit-identical to the raw tracker
  const temporal = temporalSourcesFor(cfg.temporalSources ?? ['bsv'], port.tracker, live ? { indexerApiKey: mc?.indexerApiKey, fetchImpl: mc?.fetchImpl } : {})
  const chain = {
    broadcast: async (tx: Transaction) => {
      // attach parent txs so the broadcaster can send Extended Format (nodes
      // cannot resolve unconfirmed parents from raw txs) — local sources only
      for (const inp of tx.inputs) {
        if (inp.sourceTransaction || !inp.sourceTXID) continue
        const p = txStore.get(inp.sourceTXID) ?? utxosOf()?.find((u) => u.tx.id('hex') === inp.sourceTXID)?.tx ?? memRef?.anchorTx(inp.sourceTXID)
        if (p) inp.sourceTransaction = p
      }
      const r = await port.broadcast(tx)
      if (r.status === 'success') {
        txStore.put(tx)
        events.watch(tx.id('hex'))
        memRef?.metrics.inc('nektar_fee_sats_total', {}, port.guard?.costs().at(-1)?.fee ?? 0, 'total anchor fee spend (sats)')
      } else if (r.description?.startsWith('SpendGuard refused')) {
        memRef?.metrics.inc(
          'nektar_spend_refused_total',
          { reason: r.description.split(': ')[1]?.split(' ')[0] ?? 'unknown' },
          1,
          'broadcasts refused by SpendGuard'
        )
      }
      return r
    },
    // push contract: mem's onStatus listener delivers proofs/rejects the
    // events substrate emits — pushed SSE statuses stay hints
    onStatus: (cb: (ev: TxStatusEvent) => void) => {
      events.on('tx', cb)
      events.start()
    },
    currentHeight: async () => await port.height(),
    isValidRootForHeight: (r: string, h: number) => port.tracker.isValidRootForHeight(r, h),
    getTx: port.getTx,
    isRejected: port.isRejected,
    headerAtHeight: (h: number) => temporal.headerAtHeight(h),
    tipHeight: async () => await port.height()
  }
  // watch intent is a durable record, not an in-memory accident: any tx the
  // journal knows was broadcast-but-not-final gets re-watched on boot, so a
  // crash in the broadcast→watch window can't strand a proof wait
  // late-landing candidates ride the same watch: 'abandoned'/'expired' rows
  // keep their trailing window — the substrate's own horizon bounds the set
  for (const t of await ledger.walletTxs(['submitted', 'observed', 'submission_unknown', 'abandoned', 'expired'])) events.watch(t.txid)
  custody.funds.leased = leasePredicate(ledger)
  // wire the first-party spend probe: a chain-VERIFIED 'spent' burns the
  // candidate at selection (port-verified — never an index's word).
  // 'unknown' can't burn or block: the lease guards concurrency and the
  // broadcaster's reject stays the backstop for a spend the probe missed
  if (port.spendStatus !== undefined) {
    const spendStatus = port.spendStatus
    custody.funds.spendCheck = async (txid, vout) => (await spendStatus(txid, vout).catch(() => 'unknown' as const)) === 'spent'
    // starve-sweep revive oracles: 'unspent' is a real answer only where the
    // port declares a first-party utxo view — a live indexer's 'unknown' can
    // never revive, so wiring it there would run futile retried probes
    // inside the serialized take queue under a degraded index
    if (port.provesUnspent === true)
      custody.funds.liveCheck = async (txid, vout) => (await spendStatus(txid, vout).catch(() => 'unknown' as const)) === 'unspent'
  }
  // on live nets the starve sweep's oracle is the index's address listing —
  // positive membership of a tombstoned outpoint lifts its mark. A listing
  // claim, not SPV — safe for self-bookkeeping: a falsely-revived mark
  // self-corrects at pick-time spendCheck or the broadcaster's reject
  if (live && mc?.indexerUrl !== undefined) {
    const idx = indexFor({ url: mc.indexerUrl, apiKey: mc.indexerApiKey })
    custody.funds.liveSet = async () => {
      const listing = await idx.utxos(custody.fundAddress).catch(() => undefined)
      return listing === undefined ? undefined : new Set(listing.map((u) => `${u.txid}:${u.vout}`))
    }
  }
  if ('dealFloor' in custody.funds) custody.funds.dealFloor = nektarEnvNum('NEKTAR_DEAL_FLOOR_SATS') ?? 0
  chain.broadcast = wrapBroadcast(ledger, chain.broadcast)
  chain.broadcast = txLogBroadcast(chain.broadcast)
  const mem = new NektarMemory(
    chain,
    undefined,
    custody.fundKey,
    {
      ...base,
      funds: custody.funds,
      wallet: custody.wallet,
      txStore,
      statusHint: port.statusHint,
      // counterparty-air path: a refused tx that escaped and landed still
      // spent its sats — the guard's day ledger counts them retroactively
      // through the revive hooks on both wallet ledgers
      recordAired: port.guard === undefined ? undefined : (tx: Transaction) => void port.guard!.recordAired(tx),
      height: () => port.height(),
      proofLookup: port.proofLookup,
      txLookup: port.txLookup,
      watchTx: (txid) => events.watch(txid),
      ledger,
      agentSeed: keys.agentSeed ? PrivateKey.fromHex(keys.agentSeed) : undefined,
      signingKey: keys.logKey ? PrivateKey.fromHex(keys.logKey) : undefined,
      shardKeys,
      maxLevel: Number(nektarEnv('NEKTAR_MAX_LEVEL') ?? 3),
      feeRateSatPerKb: port.feeRateSatPerKb,
      securityStore: new FileSecurityStore(join(cfg.dataDir, 'security.json')),
      stateStore: new FileStateStore(join(cfg.dataDir, 'state.json')),
      finalityDepth: live ? Number(nektarEnv('NEKTAR_FINALITY_DEPTH') ?? 6) : 1
    },
    store
  )
  memRef = mem
  healLedgerOnProof(mem, ledger, net)
  // a reorg invalidates cached proofs at/above the fork: clear them and
  // re-watch so the observe path re-fetches and re-verifies against the new
  // headers. Un-anchoring shard records stays out of scope — finalityDepth
  // already withholds shallow anchors from queries.
  let lastReorgFrom: number | undefined
  port.tracker.onReorg?.((removed, newTip) => {
    if (removed.length === 0) return
    const from = Math.min(...removed.map((h) => h.height))
    // the same fork fires once per witnessed source — the effects are
    // idempotent but the log, the metric and the emit must not double-count
    if (from === lastReorgFrom) return
    lastReorgFrom = from
    nlog(`header reorg — ${removed.length} stale header(s) from height ${from}, new tip ${newTip.height}`)
    mem.metrics.inc('nektar_reorgs_total', {}, 1, 'header-chain reorgs observed')
    for (const txid of txStore.clearProofsFrom(from)) events.watch(txid)
    mem.events.emit('reorg', { from, tip: newTip.height })
  })
  mem.metrics.gauge('nektar_fee_balance_sats', () => utxosOf()?.reduce((s, u) => s + (u.satoshis ?? 0), 0) ?? 0, 'operator fee-key balance (sats)')
  // pushed status hints (the port's SSE channel) → the same handler the
  // webhook uses, plus a substrate note so waiters see the hint
  const stream = port.openHintStream?.(async (j) => {
    const w = j as { txid?: string; txStatus?: string }
    mem.metrics.inc(
      'nektar_broadcaster_stream_events_total',
      { status: String(w.txStatus ?? '?').toLowerCase() },
      1,
      'broadcaster status stream events received'
    )
    await handleBroadcasterStatus(mem, w)
    // the pushed status is a wake hint: note() records it for statusOf/
    // waitFor, but interest only drops when a verified observe delivers — a
    // push whose BUMP failed verification keeps the pull probe's floor
    if (typeof w.txid === 'string') {
      const s = String(w.txStatus ?? '').toUpperCase()
      if (s === 'MINED' || s === 'IMMUTABLE') events.note({ txid: w.txid, status: 'mined' })
      else if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(s)) events.note({ txid: w.txid, status: 'rejected' })
    }
  })
  // funding — the reserve fills per-network: regtest bootstraps a treasury
  // split; live nets run imports/discovery/vault/p2p + the funding wait and
  // expose the /v1 ops surface
  let fundingRefresh: (() => Promise<void>) | undefined
  let topUp: (() => Promise<void>) | undefined
  let ops: Runtime['ops'] | undefined
  if (!injected && live) {
    const lf = await liveFunding({ cfg, mc: mc!, net, custody, txStore, ledger, port, broadcast: chain.broadcast })
    ops = lf.ops
    fundingRefresh = lf.refresh
    topUp = lf.topUp
  }
  // a dead funding mint may free its inputs — reconcile the reserve on the
  // abandon verdict (replaces the old watcher-status refresh, which
  // status-string hints no longer trigger)
  events.on('abandoned', () => void (fundingRefresh?.() ?? custody.funds.refresh?.()?.then(() => undefined) ?? Promise.resolve()))
  // optimistic-claim surface: provider + signer over the funding view,
  // claims registry + proof store in the data dir (the watcher shares it)
  if (port.network === 'regtest' && port.getTx !== undefined) {
    const { ClaimService } = await import('./claims.js')
    const contractsDir = fileURLToPath(new URL('../../../contracts', import.meta.url))
    mem.attachClaims(
      new ClaimService(mem, {
        dataDir: cfg.dataDir,
        contractsDir,
        fetchTx: async (txid: string) => (await port.getTx!(txid))?.toHex()
      })
    )
  }
  // only legacy PrivateKey entries persist — wallet-mode shard keys are
  // deterministic (re-derived from the wallet each boot, nothing to store)
  const persistKeys = () => {
    let changed = false
    for (const [s, k] of shardKeys)
      if (k instanceof PrivateKey && keys.shardKeys[s] !== k.toHex()) {
        keys.shardKeys[s] = k.toHex()
        changed = true
      }
    if (changed) save()
  }
  // confirmer: timed flush + optional auto-mine; proof attachment itself is
  // push-shaped — this loop only registers interest with the events
  // substrate, which delivers via chain.onStatus → mem.deliverTxProof.
  // Parents of unproven anchors are watched too: a funding tree is
  // ancestry-anchored the moment its own inputs prove. Height-gating lives
  // inside the substrate — a static tip skips the batch entirely.
  let busy = false
  let inflight: Promise<void> | undefined
  let proofPullAt = 0
  const tick = async () => {
    if (busy) return await inflight
    busy = true
    inflight = (async () => {
      try {
        persistKeys()
        await mem.flushDue()
        const waiting = mem.unprovenAnchors()
        if (waiting.length > 0 && cfg.autoMine) await port.mine?.()
        for (const { txid } of waiting) {
          events.watch(txid)
          const tx = await mem.fetchTx(txid)
          for (const i of tx?.inputs ?? []) {
            const src = i.sourceTransaction
            if (src !== undefined && src.merklePath === undefined) events.watch(src.id('hex'))
          }
        }
        // ledger txs too: one shared probe per txid serves the watcher's
        // statusOf short-circuit — no second fetch per inflight tx
        for (const w of await ledger.walletTxs(['submitted', 'submission_unknown', 'observed'])) events.watch(w.txid)
        // stalled-submission heal: a row parked missing-parent (or vanished
        // after acceptance — clean 404) can never settle, and nothing else
        // will put the absent ancestor on the wire. Walk stored ancestry —
        // the row's parents first, deeper links via the instance tx graph
        // and the ledger itself — and re-air every ancestor the network
        // honestly answers "not found" for. A refused parent can never be a
        // missing ancestor: died() tombstones its outputs so no child ever
        // chains on it. Bounded per pass; deep chains heal one level a tick.
        {
          let healBudget = 8
          const healSeen = new Set<string>()
          const resolveTx = async (txid: string): Promise<Transaction | undefined> => {
            const inGraph = txStore.get(txid)
            if (inGraph !== undefined) return inGraph
            const inLedger = (await ledger.walletTx({ txid }).catch(() => undefined))?.rawHex
            if (inLedger !== undefined) return Transaction.fromHex(inLedger)
            return await port.txLookup?.(txid).catch(() => undefined)
          }
          const hintOf = async (txid: string): Promise<{ status: string; answered: boolean } | undefined> => port.statusHint?.(txid).catch(() => undefined)
          const reair = async (tx: Transaction): Promise<void> => {
            for (const i of tx.inputs) {
              if (i.sourceTransaction !== undefined || i.sourceTXID === undefined) continue
              i.sourceTransaction = await resolveTx(i.sourceTXID)
            }
            const r = await chain.broadcast(tx)
            if (r.status === 'success') nlog(`re-aired ${tx.id('hex')} — wire gap self-healed`)
          }
          const healWalk = async (tx: Transaction): Promise<void> => {
            for (const i of tx.inputs) {
              const pid = i.sourceTXID
              if (pid === undefined || healSeen.has(pid) || healBudget <= 0) continue
              healSeen.add(pid)
              healBudget--
              const parent = i.sourceTransaction ?? (await resolveTx(pid))
              if (parent === undefined || parent.merklePath !== undefined) continue
              const st = await hintOf(pid)
              if (st?.answered === true && st.status === '') await reair(parent)
              else if (st?.answered !== true || st.status !== '') await healWalk(parent) // parent on the wire (or unanswerable) — its own ancestry may still be parked
            }
          }
          for (const w of await ledger.walletTxs(['submitted', 'submission_unknown'])) {
            if (healBudget <= 0) break
            const st = await hintOf(w.txid)
            if (st?.answered !== true) continue
            const parked = /PENDING|MISSING|RETRY/i.test(st.status)
            if (!parked && st.status !== '') continue
            const tx = Transaction.fromHex(w.rawHex)
            for (const i of tx.inputs) {
              if (i.sourceTransaction !== undefined || i.sourceTXID === undefined) continue
              const hex = w.parents[i.sourceTXID]
              if (hex !== undefined) i.sourceTransaction = Transaction.fromHex(hex)
            }
            await healWalk(tx)
            if (st.status === '') await reair(tx) // the row itself vanished — re-air its own bytes
          }
          // anchors take the same heal: a broadcast the network dropped
          // asynchronously can never prove — the watch alone waits forever.
          // Probe the wire; 'absent' re-airs the same bytes (idempotent: a
          // landed original dedupes, a truly-dropped one just needs airing).
          for (const { txid } of waiting) {
            if (healBudget <= 0) break
            if (healSeen.has(txid)) continue
            const st = await hintOf(txid)
            if (st?.answered !== true || st.status !== '') continue
            const tx = await mem.fetchTx(txid)
            if (tx === undefined) continue
            healSeen.add(txid)
            healBudget--
            await healWalk(tx)
            await reair(tx)
          }
        }
        // a bounded, rotating pull: each tick tries a few unproven txs so a
        // long list can never serialize minutes of lookups inside one pass —
        // push (callbacks/status stream) stays the primary proof channel
        {
          const unproven = txStore.unproven()
          const pull = unproven.length <= 8 ? unproven : Array.from({ length: 8 }, (_, i) => unproven[(proofPullAt + i) % unproven.length])
          proofPullAt = unproven.length > 0 ? (proofPullAt + 8) % unproven.length : 0
          for (const txid of pull) await mem.fetchTx(txid)
        }
        if (waiting.length) nlog(`confirmer: waiting=${waiting.length} (watched=${events.pending})`)
        await topUp?.()
        persistKeys()
      } catch (e) {
        nlog(`confirmer: ${(e as Error).message}`)
      } finally {
        busy = false
        inflight = undefined
      }
    })()
    await inflight
  }
  const timer = setInterval(() => {
    void tick()
  }, cfg.confirmEveryMs ?? 5000)
  timer.unref()
  // a fresh anchor is a producer event — rescan (watch + autoMine) now,
  // not at the floor; the interval stays as the missed-wake backstop
  mem.onAnchorQueued(() => void tick())
  // the ledger watcher — event-driven off the shared substrate's sweeps,
  // height advances and pushed broadcaster statuses; no private interval.
  // Construction + custody effects live in rtsupport (boot plumbing)
  const watcher = await buildRecordsWatcher({
    ledger,
    statusOf: (txid) => events.statusOf(txid),
    port,
    funds: custody.funds,
    fundingRefresh,
    diedFromRow,
    net
  })
  let watching = false
  // journal retention on the records ledger — 'confirmed' is the one tx
  // state with no outgoing edge, so its event journal is complete and
  // prunable past the window. Daily-gated: event bursts must not sweep
  // per tick, and the records store's market tables stay empty anyway.
  let lastPrune = 0
  const pruneJournal = () => {
    const now = Date.now()
    if (now - lastPrune < 86_400_000) return
    lastPrune = now
    const retentionMs = (nektarEnvNum('NEKTAR_JOURNAL_RETENTION_DAYS') ?? 30) * 86_400_000
    void ledger.prune(now - retentionMs, { ops: [], txs: ['confirmed'] }).catch(() => undefined)
  }
  const watchOnce = () => {
    if (watching) return
    watching = true
    const settled = (): void => {
      watching = false
      pruneJournal()
    }
    void watcher
      .tick()
      // the only bare catch in the sweep chain: a failing tick stalls all
      // durable settlement — log it; the next event retries regardless
      .catch((e) => nlog(`ledger watcher: ${(e as Error).message}`))
      .then(settled, settled)
  }
  events.on('tick', watchOnce)
  events.on('height', watchOnce)
  // relay tip advances onto the memory emitter the way txAbandoned/reorg
  // already are — hosted automation wakes height-bounded work off it
  events.on('height', (tip) => mem.events.emit('height', { tip }))
  mem.events.on('broadcasterStatus', watchOnce)
  return {
    mem,
    network: net,
    ops,
    close: async () => {
      stream?.close()
      events.stop()
      mem.events.off('broadcasterStatus', watchOnce)
      clearInterval(timer)
      // every teardown await carries a ceiling — a wedged chain/db call
      // must not hang the exit (bin.ts's hard deadline is the outer backstop)
      const bounded = async (p: Promise<unknown>, ms: number, what: string): Promise<void> => {
        const r = await Promise.race([
          Promise.resolve(p).then(
            () => 'done',
            () => 'done'
          ),
          new Promise<'timeout'>((res) => setTimeout(() => res('timeout'), ms))
        ])
        if (r === 'timeout') nlog(`close: ${what} exceeded ${ms}ms — proceeding`)
      }
      await bounded(inflight ?? Promise.resolve(), 15_000, 'inflight confirmer pass')
      await releaseLock()
      await custody.releaseKeyLock?.()
      await bounded(tick(), 20_000, 'final confirmer pass')
      await bounded(custody.closeWallet?.() ?? Promise.resolve(), 10_000, 'wallet close')
      await bounded(knex.destroy(), 10_000, 'records db')
    }
  }
}

// public surface preserved through index.ts — the helpers moved to
// rtsupport.ts; callers importing them from './runtime.js' keep working.
export { acquireLock, keyLockFile, txLogBroadcast }
