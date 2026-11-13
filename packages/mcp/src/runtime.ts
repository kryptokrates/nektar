// openRuntime: one production entry for the MCP / HTTP servers.
//   NEKTAR_NETWORK=regtest  Teranode regtest: persistent records (sqlite), anchor
//                          state, security state, funding reserve and keys in
//                          NEKTAR_DATA_DIR; a confirmer attaches BUMPs.
//   NEKTAR_NETWORK=testnet  same shape as regtest; refused until an endpoint and
//                          funding are configured (no real funds without approval).
import { existsSync, readFileSync, writeFileSync, mkdirSync, appendFileSync, rmSync } from 'node:fs'
import { join } from 'node:path'
import { randomBytes } from 'node:crypto'
import { PrivateKey, Transaction } from '@bsv/sdk'
import {
  TeranodeBroadcaster,
  TeranodeRPCTracker,
  ReserveFunder,
  bootstrapReserve,
  regtestTx,
  regtestMerkleProof,
  BsvBroadcaster,
  WocTracker,
  fetchBroadcasterFeeRate,
  merklePathMainnet,
  discoverUtxos,
  importFundingTx,
  utxoStatus,
  ChaintracksTracker,
  SpendGuard,
  TemporalSources,
  statusStream,
  fetchTxStatus,
  DEFAULT_WOC_URL,
  TxStore,
  ChainEvents,
  type TemporalSource,
  type ChainTracker,
  type RegtestConfig,
  type MainnetConfig,
  type TxStatusEvent
} from '@nektar/chain'
import { handleBroadcasterStatus } from './http.js'
import { walletSigner, FUND_SIGN_ARGS, nektarEnv, setNektarEnv, type SignerWallet } from '@nektar/core'
import { wireP2p, setupFeeVault } from './setup.js'
import { KnexShardStore, type ShardStore } from '@nektar/log'
import { NektarMemory, type NektarMemoryOptions } from './memory.js'
import { FileSecurityStore, FileStateStore } from './securitystore.js'
import { readKeyFile, writeKeyFile, passphraseFromEnv, fundKeyFromMnemonic, KeystoreError } from './keystore.js'
import { fileURLToPath } from 'node:url'

export interface RuntimeConfig {
  network: 'regtest' | 'testnet' | 'main' | 'ttn'
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

/** TemporalSources over the primary tracker + optional BTC WoC tracker (read-only).
 *  `NEKTAR_BTC_WOC_URL` overrides the BTC endpoint (tests point it at a stub). */
function temporalSourcesFor(names: string[], primary: ChainTracker, opts: { wocApiKey?: string; fetchImpl?: typeof fetch } = {}): TemporalSources {
  const sources: TemporalSource[] = [{ name: 'bsv', tracker: primary }]
  for (const n of names) {
    if (n === 'bsv') continue
    if (n === 'btc')
      sources.push({
        name: 'btc',
        tracker: new WocTracker({
          wocUrl: nektarEnv('NEKTAR_BTC_WOC_URL') ?? 'https://api.whatsonchain.com/v1/btc/main',
          wocApiKey: opts.wocApiKey,
          fetchImpl: opts.fetchImpl
        })
      })
  }
  return new TemporalSources(sources)
}

export function configFromEnv(env = process.env): RuntimeConfig {
  const network = (env.NEKTAR_NETWORK ?? 'regtest') as RuntimeConfig['network']
  const dataDir = env.NEKTAR_DATA_DIR ?? join(process.cwd(), '.nektar')
  const secret = (k: string) => env[k] ?? (env[`${k}_FILE`] && existsSync(env[`${k}_FILE`]!) ? readFileSync(env[`${k}_FILE`]!, 'utf8').trim() : undefined)
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
  if (network === 'main' || network === 'ttn' || network === 'testnet') {
    if (network === 'main' && env.NEKTAR_MAINNET_CONFIRM !== 'I_UNDERSTAND') {
      throw new Error('mainnet mode spends REAL BSV. Refusing to start. Set NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND to proceed.')
    }
    // "ttn" below = any test network (testnet encoding, test coins, low fee floor)
    const ttn = network !== 'main'
    const urls =
      network === 'testnet'
        ? { broadcaster: 'https://arcade-v2-testnet-us-1.bsvblockchain.tech', woc: 'https://api.whatsonchain.com/v1/bsv/test' }
        : network === 'ttn'
          ? { broadcaster: 'https://arcade-v2-ttn-us-1.bsvblockchain.tech', woc: 'https://api.woc-ttn.bsvblockchain.tech/v1/bsv/test' }
          : undefined
    cfg.confirmEveryMs = Number(env.NEKTAR_CONFIRM_MS ?? (ttn ? 30_000 : 60_000))
    cfg.main = {
      fundWif: secret('NEKTAR_FUND_WIF'),
      passphrase: secret('NEKTAR_KEY_PASSPHRASE'),
      // TTN defaults: BSV Broadcaster + WoC-TTN endpoints (probed live); main
      // keeps the built-in GorillaPool/WoC defaults when env vars are unset
      broadcasterUrl: env.NEKTAR_BROADCASTER_URL ?? urls?.broadcaster,
      broadcasterApiKey: secret('NEKTAR_BROADCASTER_API_KEY'),
      wocUrl: env.NEKTAR_WOC_URL ?? urls?.woc,
      wocApiKey: secret('NEKTAR_WOC_API_KEY'),
      chaintracksUrl: env.NEKTAR_CHAINTRACKS_URL,
      // fee floor: 100 sat/kB main (GorillaPool policy), 1 sat/kB TTN (its
      // Arcade policy is 0 — a zero fee rate would produce free-of-fee txs
      // miners may reject; keep ≥1)
      feeRateSatPerKb: env.NEKTAR_FEE_RATE_SAT_PER_KB ? Number(env.NEKTAR_FEE_RATE_SAT_PER_KB) : ttn ? 1 : undefined,
      maxFeePerTx: Number(env.NEKTAR_MAX_FEE_SATS_PER_TX ?? 1000),
      maxSpendPerDay: Number(env.NEKTAR_MAX_SPEND_SATS_PER_DAY ?? 20_000),
      minBroadcastIntervalMs: Number(env.NEKTAR_MIN_BROADCAST_INTERVAL_MS ?? 250)
    }
  }
  return cfg
}

interface KeyFile {
  agentSeed: string
  logKey: string
  shardKeys: Record<string, string>
}
/** keys are generated once into the data dir (0600) unless supplied; never logged.
 *  Encrypted at rest when a passphrase is set; mandatory on main.
 *  `create=false` (external-wallet custody): an absent file is NOT generated —
 *  the wallet covers seed/log/shard custody; an existing file is still read
 *  for legacy shard owners and old records. */
function loadKeys(dataDir: string, pass: string | undefined, requireEnc = false, create = true): { keys: KeyFile; save: () => void } {
  const file = join(dataDir, 'keys.json')
  if (!existsSync(file) && !create) return { keys: { agentSeed: '', logKey: '', shardKeys: {} }, save: () => undefined }
  const keys: KeyFile = existsSync(file)
    ? (JSON.parse(readKeyFile(file, pass, requireEnc)) as KeyFile)
    : { agentSeed: PrivateKey.fromRandom().toHex(), logKey: PrivateKey.fromRandom().toHex(), shardKeys: {} }
  const save = () => writeKeyFile(file, JSON.stringify(keys), pass)
  if (!existsSync(file)) save()
  return { keys, save }
}

/** operator fee key (pays all anchor fees): NEKTAR_FUND_WIF wins; otherwise a
 *  dedicated fee.key.json generated once in the data dir — encrypted on main. */
function loadFeeKey(dataDir: string, wif: string | undefined, pass: string | undefined, requireEnc: boolean): PrivateKey {
  if (wif) return PrivateKey.fromWif(wif)
  // NEKTAR_FUND_MNEMONIC (or _FILE): BIP-39 custody-of-record — recoverable,
  // one string to protect. Derived at m/44'/236'/0'/0/0; feeds the same
  // keyDeriver the WIF path uses.
  const mn =
    nektarEnv('NEKTAR_FUND_MNEMONIC') ??
    (existsSync(nektarEnv('NEKTAR_FUND_MNEMONIC_FILE') ?? '') ? readFileSync(nektarEnv('NEKTAR_FUND_MNEMONIC_FILE')!, 'utf8').trim() : undefined)
  if (mn) return fundKeyFromMnemonic(mn)
  const file = join(dataDir, 'fee.key.json')
  if (existsSync(file)) {
    const j = JSON.parse(readKeyFile(file, pass, requireEnc)) as { wif: string }
    return PrivateKey.fromWif(j.wif)
  }
  const key = PrivateKey.fromRandom()
  writeKeyFile(file, JSON.stringify({ wif: key.toWif() }), pass)
  return key
}

// NEKTAR_TX_LOG: when set to a file path, append one JSON line per SUCCESSFUL
// broadcast — {t, txid, bytes, inSats, outSats, feeSats} — so operator cost
// accounting comes from the wire, not from estimates. inSats/feeSats are null
// when a source transaction is missing. Write failures are ignored; no
// behaviour change otherwise.
export function txLogBroadcast<B extends (tx: Transaction) => Promise<import('@nektar/chain').BroadcastResult>>(
  inner: B,
  logPath: string | undefined = nektarEnv('NEKTAR_TX_LOG')
): B {
  if (logPath === undefined || logPath === '') return inner
  return (async (tx: Transaction) => {
    const r = await inner(tx)
    if (r.status === 'success') {
      try {
        let inSats: number | null = 0
        for (const i of tx.inputs) {
          const src = i.sourceTransaction?.outputs[i.sourceOutputIndex ?? 0]
          if (src?.satoshis === undefined) {
            inSats = null
            break
          }
          inSats += src.satoshis
        }
        const outSats = tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
        appendFileSync(
          logPath,
          JSON.stringify({
            t: Date.now(),
            txid: tx.id('hex'),
            bytes: tx.toBinary().length,
            inSats,
            outSats,
            feeSats: inSats === null ? null : inSats - outSats
          }) + '\n'
        )
      } catch {
        /* logging must never break a broadcast */
      }
    }
    return r
  }) as B
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
    costs: () => Array<{ txid: string; bytes: number; fee: number; at: string }>
  }
}

/** single writer per data dir: a live heartbeat from another instance
 *  refuses startup. release() stops the beat and removes the lock —
 *  was duplicated verbatim across both open paths. */
async function acquireInstanceLock(dataDir: string): Promise<{ release: () => Promise<void> }> {
  const lockFile = join(dataDir, 'instance.lock')
  const me = `${process.pid}@${(await import('node:os')).hostname()}`
  if (existsSync(lockFile)) {
    const l = JSON.parse(readFileSync(lockFile, 'utf8')) as { owner: string; beat: number }
    if (l.owner !== me && Date.now() - l.beat < 20_000) throw new Error(`data dir ${dataDir} is in use by another nektar instance (${l.owner}); stop it first`)
    rmSync(lockFile)
  }
  const beat = () => writeFileSync(lockFile, JSON.stringify({ owner: me, beat: Date.now() }))
  writeFileSync(lockFile, JSON.stringify({ owner: me, beat: Date.now() }), { flag: 'wx' })
  const beatTimer = setInterval(beat, 5_000)
  beatTimer.unref()
  return {
    release: async () => {
      clearInterval(beatTimer)
      try {
        rmSync(lockFile)
      } catch {
        /* already gone */
      }
    }
  }
}

/** records.sqlite + shard store + automation ledger — the one opener for the
 *  data dir's durable state (WAL + busy_timeout), was two verbatim copies. */
async function openRecordsDb(dataDir: string) {
  const knexMod = (await import('knex')).default
  const knex = knexMod({ client: 'sqlite3', connection: { filename: join(dataDir, 'records.sqlite') }, useNullAsDefault: true })
  // WAL: readers don't queue behind the writer; busy_timeout turns a
  // contended SQLITE_BUSY into a wait instead of a surfaced error
  await knex.raw('PRAGMA journal_mode=WAL')
  await knex.raw('PRAGMA busy_timeout=5000')
  const store: ShardStore = new KnexShardStore(knex)
  await store.init?.()
  const { AutomationStore, wrapBroadcast, leasePredicate } = await import('@nektar/automation')
  const ledger = new AutomationStore(knex)
  await ledger.init()
  return { knex, store, ledger, wrapBroadcast, leasePredicate }
}

export async function openRuntime(cfg: RuntimeConfig): Promise<Runtime> {
  // fail fast: paid tools need a valid operator wallet BEFORE the first 402 —
  // an invoice served to a missing address is a payment that can never settle.
  const { paymentsEnabled, requireOperatorWallet } = await import('./pay.js')
  if (paymentsEnabled()) requireOperatorWallet()
  const { objectStoreFromEnv } = await import('@nektar/store')
  const bondEpoch = nektarEnv('NEKTAR_BOND_EPOCH')
  const bondMaxFee = nektarEnv('NEKTAR_BOND_MAXFEE')
  const base: NektarMemoryOptions = {
    batchPolicy: { maxLeaves: cfg.batchLeaves ?? 4, maxAgeMs: 60_000 },
    modelId: cfg.modelId ?? 'unspecified',
    clearanceAuthorities: cfg.clearanceAuthorities,
    network: cfg.network,
    objectStore: objectStoreFromEnv(),
    bond:
      bondEpoch || bondMaxFee
        ? {
            epochBlocks: Number(bondEpoch ?? 144),
            ...(bondMaxFee !== undefined ? { maxFee: BigInt(bondMaxFee) } : {})
          }
        : undefined
  }
  if (cfg.network === 'main' || cfg.network === 'ttn' || cfg.network === 'testnet') {
    return await openLiveRuntime(cfg, cfg.network, base)
  }
  const rc = cfg.regtest!
  mkdirSync(cfg.dataDir, { recursive: true })
  const { release: releaseLock } = await acquireInstanceLock(cfg.dataDir)
  const { keys, save } = loadKeys(cfg.dataDir, passphraseFromEnv())
  const fundKey = PrivateKey.fromWif(rc.fundWif)
  const tracker = new TeranodeRPCTracker(rc)
  // input-value resolver for the guard's fee check — same closure as the
  // live runtime: reserve entries, guarded broadcasts, anchor txs (fail-closed)
  let memRef: NektarMemory | undefined
  const { txStore, open: openTxStore } = persistedTxStore()
  const valueOf = (txid: string, vout: number): number | undefined =>
    utxoView.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis ??
    txStore.get(txid)?.outputs[vout]?.satoshis ??
    memRef?.anchorTx(txid)?.outputs[vout]?.satoshis
  // D2 hard daily cap: default 10_000 sats/day of fees on regtest too —
  // 'unlimited' is an explicit opt-out for soak/stress runs, never the
  // default, because an unattended agent must always have a provable guard.
  // NEKTAR_DAILY_CAP_SATS is the plan's hard cap; NEKTAR_MAX_SPEND_SATS_PER_DAY
  // (the live env) sets it too — the smaller of the two wins.
  const caps = [nektarEnv('NEKTAR_DAILY_CAP_SATS'), nektarEnv('NEKTAR_MAX_SPEND_SATS_PER_DAY')].filter((x) => x !== undefined)
  // opt-out only when every set value agrees — a stray numeric cap keeps the
  // guard on (mixed signals never silently un-guard the spend path)
  const unlimited = caps.length > 0 && caps.every((x) => x === 'unlimited')
  const maxPerDay = unlimited ? Number.MAX_SAFE_INTEGER : Math.min(...caps.filter((x) => x !== 'unlimited').map((x) => Number(x)), 10_000)
  const broadcaster = new SpendGuard(new TeranodeBroadcaster(rc.propagationUrl), valueOf, {
    unlimited,
    maxPerDay,
    maxFeePerTx: Number(nektarEnv('NEKTAR_MAX_FEE_SATS_PER_TX') ?? 1_000),
    minIntervalMs: Number(nektarEnv('NEKTAR_MIN_BROADCAST_INTERVAL_MS') ?? 0),
    ledgerFile: join(cfg.dataDir, 'spend-ledger.json')
  })
  const auth = 'Basic ' + Buffer.from(`${rc.rpcUser}:${rc.rpcPass}`).toString('base64')
  const rpc = async (method: string, ...params: unknown[]): Promise<any> => {
    const r = await fetch(rc.rpcUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: auth },
      body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }),
      signal: AbortSignal.timeout(15_000)
    })
    const j = (await r.json()) as { result?: unknown; error?: unknown }
    if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    return j.result
  }
  let lastMined = 0
  const mineOne = async () => {
    const [hash] = (await rpc('generatetoaddress', 1, fundKey.toPublicKey().toAddress([0x6f]))) as string[]
    const hdr = (await rpc('getblockheader', hash, true)) as { height: number }
    lastMined = Math.max(lastMined, hdr.height)
  }
  // the one chain-event substrate for this process: every "is it proven?"
  // consumer (confirmer, anchor parents, ledger) registers interest here and
  // receives 'tx' pushes instead of running a private poll loop. Its height
  // probe makes proof fetches impossible-but-cheap misses free: a static tip
  // and no new watches skips the batch entirely. Self-observed BUMPs come
  // from the asset proof endpoint — served bytes are claims until their
  // root anchors to our own header chain (the SPV kernel, not "the node
  // said so").
  const events = new ChainEvents({
    intervalMs: 2_000,
    abandonAfterBlocks: 100,
    height: async () => Math.max(lastMined, await tracker.currentHeight()),
    observe: async (txid) => {
      const mp = await regtestMerkleProof(rc, txid)
      if (mp === undefined) return undefined
      let root: string
      try {
        root = mp.computeRoot(txid)
      } catch {
        return undefined // served path doesn't contain the txid — a claim, not a proof
      }
      if (!(await tracker.isValidRootForHeight(root, mp.blockHeight))) return undefined
      return { txid, status: 'mined', blockHeight: mp.blockHeight, merklePath: mp }
    }
  })
  events.on('abandoned', ({ txid, tip, seenAtHeight }) => {
    process.stderr.write(`nektar confirmer: anchor ${txid} not mined after 100 blocks — abandoned (check for a conflicting spend)\n`)
    // repair-by-name, not just a log line: mark the durable intent
    // 'conflicted' so boot-time re-watch doesn't resurrect a dead tx
    // forever, and surface it on mem.events for live consumers
    void (async () => {
      try {
        const hit = (await ledger.walletTxs(['submitted', 'observed', 'submission_unknown'])).find((w) => w.txid === txid)
        if (hit !== undefined) {
          await ledger.setTxState(hit.actionId, 'conflicted', { reason: `not mined after 100 blocks (tip ${tip}, watched since height ${seenAtHeight})` })
        }
        memRef?.events.emit('txAbandoned', { txid, tip, seenAtHeight })
      } catch (e) {
        process.stderr.write(`nektar confirmer: abandoned ${txid} mark failed: ${(e as Error).message}\n`)
      }
    })()
  })
  const reserve = new ReserveFunder(fundKey, join(cfg.dataDir, 'reserve.json'))
  if (reserve.utxos.length < 5) {
    // own treasury chain in the data dir; split mined before use
    const s = await bootstrapReserve(rc, fundKey, reserve, rpc, { treasuryFile: join(cfg.dataDir, 'treasury.json') })
    if (!s && reserve.utxos.length === 0) throw new Error('regtest: could not fund the reserve (no unspent treasury or early coinbase for the fund key)')
    if (s) await mineOne()
  }
  // NEKTAR_WALLET=toolbox: the BRC-100 wallet owns the signing surface AND
  // the funding-UTXO lifecycle — reserve.json only seeds it once at boot,
  // then the wallet's own storage is the custody view (selection, spent
  // marks, internalized change). Legacy mode keeps the file-backed reserve.
  let toolboxWallet: import('@nektar/automation').ToolboxHandle | undefined
  let tf: import('@nektar/automation').ToolboxReserveFunder | undefined
  if (nektarEnv('NEKTAR_WALLET') === 'toolbox') {
    const tb = await import('@nektar/automation')
    const services = new tb.RegtestWalletServices(rc, tracker)
    toolboxWallet = await tb.openToolboxWallet({ fundKey, dataDir: cfg.dataDir, chain: 'regtest', services })
    tf = new tb.ToolboxReserveFunder(toolboxWallet.wallet, services, (s) => process.stderr.write(`nektar regtest: ${s}\n`))
    await tf.refresh()
    for (const u of reserve.utxos) if (!(await tf.has(u.tx.id('hex'), u.vout))) await tf.addOwned(u.tx, u.vout)
    process.stderr.write(`nektar regtest: wallet backend toolbox (wallet.sqlite) — ${tf.utxos.length} funding utxo(s)\n`)
  }
  // one custody view: consumers read utxoView regardless of backend — the
  // same shape ReserveFunder always had, so nothing downstream changes
  const utxoView = tf ?? reserve
  const funder = {
    keyPair: fundKey,
    get utxos() {
      return utxoView.utxos
    },
    markSpent: (t: string, v: number) => utxoView.markSpent(t, v),
    addOwned: (tx: Transaction, v: number) => {
      void utxoView.addOwned(tx, v)
    },
    takeReserve: async (_sc?: unknown, minSats?: number) =>
      await utxoView.takeReserve(tf === undefined ? async (t, v) => (await utxoStatus(rc, t, v)) === 'spent' : undefined, minSats)
  }
  const temporal = temporalSourcesFor(cfg.temporalSources ?? ['bsv'], tracker)
  const chain = {
    broadcast: async (tx: Transaction) => {
      const r = await broadcaster.broadcast(tx)
      if (r.status === 'success') {
        txStore.put(tx)
        events.watch(tx.id('hex'))
      }
      return r
    },
    // push contract: mem's onStatus listener delivers proofs/rejects the
    // events substrate emits — the same shape statusStream gives live nets
    onStatus: (cb: (ev: TxStatusEvent) => void) => {
      events.on('tx', cb)
      events.start()
    },
    currentHeight: () => tracker.currentHeight(),
    isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
    // mem.fetchTx's only regtest source: mempool/mined tx bytes + sealed BUMP
    // so bstack funding trees (and escrow fetches) can be marked proven
    getTx: async (txid: string) => await regtestTx(rc, txid),
    // §12: header TIMES are the weighted median across temporal sources;
    // single-source (default) is bit-identical to the raw tracker
    headerAtHeight: (h: number) => temporal.headerAtHeight(h),
    tipHeight: async () => Math.max(lastMined, await tracker.currentHeight())
  }
  const { knex, store, ledger, wrapBroadcast, leasePredicate } = await openRecordsDb(cfg.dataDir)
  await openTxStore(knex)
  for (const u of utxoView.utxos) txStore.put(u.tx)
  const shardKeys = new Map(Object.entries(keys.shardKeys).map(([s, h]) => [s, PrivateKey.fromHex(h)]))
  // watch intent is a durable record, not an in-memory accident: any tx the
  // journal knows was broadcast-but-not-final gets re-watched on boot, so a
  // crash in the broadcast→watch window can't strand a proof wait
  for (const t of await ledger.walletTxs(['submitted', 'observed', 'submission_unknown'])) events.watch(t.txid)
  reserve.leased = leasePredicate(ledger)
  if (tf !== undefined) tf.leased = leasePredicate(ledger)
  chain.broadcast = wrapBroadcast(ledger, chain.broadcast)
  chain.broadcast = txLogBroadcast(chain.broadcast)
  const mem = new NektarMemory(
    chain,
    funder as never,
    fundKey,
    {
      ...base,
      wallet: toolboxWallet?.wallet,
      txStore,
      agentSeed: PrivateKey.fromHex(keys.agentSeed),
      signingKey: PrivateKey.fromHex(keys.logKey),
      shardKeys,
      maxLevel: Number(nektarEnv('NEKTAR_MAX_LEVEL') ?? 3),
      securityStore: new FileSecurityStore(join(cfg.dataDir, 'security.json')),
      stateStore: new FileStateStore(join(cfg.dataDir, 'state.json')),
      finalityDepth: 1
    },
    store
  )
  memRef = mem
  mem.ledger = ledger
  // optimistic-claim surface: provider + signer over the regtest funder,
  // claims registry + proof store in the data dir (the watcher shares it)
  {
    const { ClaimService } = await import('./claims.js')
    const contractsDir = fileURLToPath(new URL('../../../contracts', import.meta.url))
    mem.attachClaims(
      new ClaimService(mem, {
        dataDir: cfg.dataDir,
        contractsDir,
        fetchTx: async (txid: string) => await rpc('getrawtransaction', txid).catch(() => undefined)
      })
    )
  }
  // persist newly generated shard keys (rotation replaces the entry)
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
  // substrate, which observes /merkle_proof once for every watched txid and
  // emits 'tx' → chain.onStatus → mem.deliverTxProof. Parents of unproven
  // anchors are watched too: a funding tree is ancestry-anchored the moment
  // its own inputs prove. Height-gating lives inside the substrate — a
  // static tip skips the batch like the old proofScanTip gate did.
  let busy = false
  const rescan = async () => {
    const waiting = mem.unprovenAnchors()
    if (waiting.length === 0) return
    if (cfg.autoMine) await mineOne()
    for (const { txid } of waiting) {
      events.watch(txid)
      const tx = await mem.fetchTx(txid)
      for (const i of tx?.inputs ?? []) {
        const src = i.sourceTransaction
        if (src !== undefined && src.merklePath === undefined) events.watch(src.id('hex'))
      }
    }
    if (waiting.length) process.stderr.write(`nektar confirmer: waiting=${waiting.length} (watched=${events.pending})\n`)
  }
  const tick = async () => {
    if (busy) return
    busy = true
    try {
      persistKeys()
      await mem.flushDue()
      await rescan()
      persistKeys()
    } catch (e) {
      process.stderr.write(`nektar confirmer: ${(e as Error).message}\n`)
    } finally {
      busy = false
    }
  }
  const timer = setInterval(() => {
    void tick()
  }, cfg.confirmEveryMs ?? 5000)
  timer.unref()
  return {
    mem,
    network: 'regtest',
    close: async () => {
      events.stop()
      clearInterval(timer)
      await releaseLock()
      await tick()
      await toolboxWallet?.close()
      await knex.destroy()
    }
  }
}

/** The instance's transaction graph — parents for spends, BEEF for
 *  counterparties, proofs as they are pushed — persisted next to the records
 *  once the database is open (nothing is broadcast before that). */
function persistedTxStore(): { txStore: TxStore; open: (knex: import('knex').Knex) => Promise<void> } {
  let kx: import('knex').Knex | undefined
  const txStore = new TxStore({
    load: async () =>
      kx === undefined
        ? []
        : (await kx('tx_store').select('txid', 'raw', 'proof')).map((r: { txid: string; raw: string; proof: string | null }) => ({
            txid: r.txid,
            raw: r.raw,
            ...(r.proof ? { proof: r.proof } : {})
          })),
    put: async (row) => {
      if (kx !== undefined)
        await kx('tx_store')
          .insert({ txid: row.txid, raw: row.raw, proof: row.proof ?? null, at: Date.now() })
          .onConflict('txid')
          .merge(['proof'])
    }
  })
  return {
    txStore,
    open: async (knex) => {
      if (!(await knex.schema.hasTable('tx_store'))) {
        await knex.schema.createTable('tx_store', (t) => {
          t.string('txid', 64).primary()
          t.text('raw').notNullable()
          t.text('proof')
          t.bigInteger('at').notNullable()
        })
      }
      kx = knex
      await txStore.load()
    }
  }
}

/** Shared live path for `main` (BSV mainnet) and `ttn` (Teranode Test Net —
 * test coins, same code). Net-dependent bits: address prefix (0x6f testnet
 * for ttn, mainnet for main), fee floor (100 vs 1 sat/kB), labels. */
async function openLiveRuntime(cfg: RuntimeConfig, net: 'main' | 'ttn' | 'testnet', base: NektarMemoryOptions): Promise<Runtime> {
  const mc = cfg.main!
  const ttn = net !== 'main' // any test network: testnet encoding, 1 sat/kB floor
  mkdirSync(cfg.dataDir, { recursive: true })
  const pass = mc.passphrase
  if (!pass) throw new KeystoreError(`${net} requires NEKTAR_KEY_PASSPHRASE (or _FILE) for keys at rest`)
  const { release: releaseLock } = await acquireInstanceLock(cfg.dataDir)

  // NEKTAR_WALLET=client: an external BRC-100 wallet (Metanet Desktop or any
  // 'auto'-detected substrate) holds ALL signing material — no fund key, no
  // agent seed, no log key, no new shard keys in-process. An existing
  // keys.json is still READ (legacy shard owners + seed for old records) but
  // nothing new is generated into it. Mutually exclusive with =toolbox.
  const externalWallet = nektarEnv('NEKTAR_WALLET') === 'client'
  const { keys, save } = loadKeys(cfg.dataDir, pass, true, !externalWallet)
  let clientWallet: import('@bsv/sdk').WalletInterface | undefined
  let fundAddress: string
  let pkhHex: string
  let fundKey: PrivateKey | undefined
  if (externalWallet) {
    if (mc.fundWif !== undefined) process.stderr.write(`nektar ${net}: NEKTAR_FUND_WIF ignored under NEKTAR_WALLET=client — the external wallet signs\n`)
    const { WalletClient, PublicKey: SdkPublicKey } = await import('@bsv/sdk')
    clientWallet = new WalletClient('auto', 'nektar')
    // the reserve funds a stable self-derived wallet keyID (nektar-fund) —
    // createSignature under the same derivation produces the matching sig, so
    // P2PKH spends can be wallet-signed without the app holding key material.
    // (identityKey cannot root-sign on sdk-4 — the derived pair is the only
    // spendable surface external custody exposes.)
    const fund = walletSigner(clientWallet, FUND_SIGN_ARGS)
    const pk = SdkPublicKey.fromString(await fund.pubKeyHex())
    fundAddress = pk.toAddress(ttn ? [0x6f] : undefined)
    pkhHex = Buffer.from(pk.toHash()).toString('hex')
    process.stderr.write(`nektar ${net}: NEKTAR_WALLET=client — signing via external wallet substrate (derived funding address ${fundAddress})\n`)
  } else {
    fundKey = loadFeeKey(cfg.dataDir, mc.fundWif, pass, true)
    fundAddress = fundKey.toPublicKey().toAddress(ttn ? [0x6f] : undefined)
    pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  }
  // TTN uses testnet encoding (prefix byte 0x6f → 'm'/'n' addresses); main '1…'
  process.stderr.write(
    `nektar ${net}: operator fee key ${fundAddress} — fund this address to enable anchoring (then set NEKTAR_FUND_TXIDS=<txid|hex:rawtx>, or NEKTAR_WAIT_FUNDING=1 to QR + hold for funds)\n`
  )

  // headers via Chaintracks (public, no key, PoW-verifiable — the only global
  // data SPV needs); WoC stays as fallback. GorillaPool's broadcaster doesn't mount
  // the chaintracks listener, so mainnet uses the association broadcaster that does.
  const tracker = new ChaintracksTracker(
    mc,
    mc.chaintracksUrl ?? (ttn ? `${mc.broadcasterUrl}/chaintracks/v2` : 'https://arcade-us-1.bsvblockchain.tech/chaintracks/v2'),
    new WocTracker(mc)
  )
  // per-instance callback token — tags our submissions so the broadcaster can
  // route their status updates back to us (configFromEnv leaves both fields
  // to def()'s env fallback: NEKTAR_CALLBACK_TOKEN wins; else a persisted
  // random token, also pushed into env so the webhook auth check sees it).
  if (!mc.callbackToken && !nektarEnv('NEKTAR_CALLBACK_TOKEN')) {
    const tokFile = join(cfg.dataDir, 'callback-token')
    mc.callbackToken = existsSync(tokFile)
      ? readFileSync(tokFile, 'utf8').trim()
      : (() => {
          const t = randomBytes(32).toString('hex')
          writeFileSync(tokFile, t, { mode: 0o600 })
          return t
        })()
    setNektarEnv('NEKTAR_CALLBACK_TOKEN', mc.callbackToken)
  }
  const callbackUrl = mc.callbackUrl ?? nektarEnv('NEKTAR_CALLBACK_URL')
  const rawBroadcaster = new BsvBroadcaster(mc)
  const reserve = new ReserveFunder(fundKey ?? { pkhHex }, join(cfg.dataDir, 'reserve.json'))
  // NEKTAR_WALLET=toolbox: the BRC-100 wallet owns the funding-UTXO lifecycle
  // (selection, spent marks, internalized change). reserve.json only seeds it
  // once at boot — every consumer then reads the wallet-backed view through
  // utxoView, never reserve.json directly. Legacy mode keeps the file reserve.
  let liveToolbox: import('@nektar/automation').ToolboxHandle | undefined
  let liveTf: import('@nektar/automation').ToolboxReserveFunder | undefined
  if (nektarEnv('NEKTAR_WALLET') === 'toolbox') {
    if (fundKey === undefined) throw new Error('NEKTAR_WALLET=toolbox embeds its keyDeriver from the fee key — =client is the keyless mode')
    const tb = await import('@nektar/automation')
    const services = new tb.LiveWalletServices({ chain: ttn ? 'test' : 'main', mc, tracker, broadcast: (tx) => rawBroadcaster.broadcast(tx) })
    liveToolbox = await tb.openToolboxWallet({ fundKey, dataDir: cfg.dataDir, chain: ttn ? 'test' : 'main', services })
    liveTf = new tb.ToolboxReserveFunder(liveToolbox.wallet, services, (s) => process.stderr.write(`nektar ${net}: ${s}\n`))
    await liveTf.refresh()
    // one-time mirror: reserve.json's existing UTXOs feed the wallet's own
    // storage — serialized + deduped; each internalize resolves parents.
    for (const u of reserve.utxos) if (!(await liveTf.has(u.tx.id('hex'), u.vout))) await liveTf.addOwned(u.tx, u.vout)
    process.stderr.write(`nektar ${net}: wallet backend toolbox (wallet.sqlite) — ${liveTf.utxos.length} funding utxo(s)\n`)
  }
  /** the one custody view: utxos/markSpent/addOwned/takeReserve/isSpent —
   *  ReserveFunder and ToolboxReserveFunder are deliberately the same shape */
  const utxoView = liveTf ?? reserve
  /** ReserveLike facade for the sync-only consumers (wireP2p, fee vault) —
   *  toolbox addOwned returns a promise; swallow it at the boundary */
  const utxoSync = {
    get utxos() {
      return utxoView.utxos
    },
    isSpent: (t: string, v: number) => utxoView.isSpent(t, v),
    markSpent: (t: string, v: number) => utxoView.markSpent(t, v),
    addOwned: (tx: Transaction, v: number) => {
      void utxoView.addOwned(tx, v)
    }
  }
  const { txStore, open: openTxStore } = persistedTxStore()
  // funding wake: every producer that lands a utxo (p2p 'pay', FUND_TXIDS
  // import, indexer discovery, fee-vault mint) signals through addIfNew so
  // waitForFunding's poll can sleep event-driven instead of fixed-cadence
  let fundingWake: (() => void) | undefined
  const addIfNew = (u: { tx: Transaction; vout: number }) => {
    txStore.put(u.tx)
    // legacy tombstone: a marked-spent outpoint must not be revived. Toolbox
    // has no spent-set — the wallet's spendable flag is the truth there.
    if (liveTf === undefined && reserve.isSpent(u.tx.id('hex'), u.vout)) return
    void utxoView.addOwned(u.tx, u.vout)
    fundingWake?.()
  }
  let memRef: NektarMemory | undefined
  const valueOf = (txid: string, vout: number): number | undefined =>
    utxoView.utxos.find((u) => u.tx.id('hex') === txid && u.vout === vout)?.satoshis ??
    txStore.get(txid)?.outputs[vout]?.satoshis ??
    memRef?.anchorTx(txid)?.outputs[vout]?.satoshis
  const spendGuard = new SpendGuard(rawBroadcaster, valueOf, {
    maxFeePerTx: mc.maxFeePerTx,
    maxPerDay: mc.maxSpendPerDay,
    minIntervalMs: mc.minBroadcastIntervalMs,
    ledgerFile: join(cfg.dataDir, 'spend-ledger.json')
  })
  // optional p2p eyes: NEKTAR_P2P_PEERS='ip:port,...' — first-hand inv stream
  // watching our reserve outpoints (spend events by ANYONE) and the fee-key
  // script (unannounced funding). No index: watched set only, rest is dropped.
  const p2pPeers = (nektarEnv('NEKTAR_P2P_PEERS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  wireP2p({ peers: p2pPeers, pkhHex, reserve: utxoSync, addIfNew, log: (s) => process.stderr.write(s) })
  const floor = ttn ? 1 : 100
  const feeRate = Math.max(mc.feeRateSatPerKb ?? floor, await fetchBroadcasterFeeRate(mc).catch(() => floor))
  const funder = {
    keyPair: fundKey,
    get utxos() {
      return utxoView.utxos
    },
    markSpent: (t: string, v: number) => utxoView.markSpent(t, v),
    addOwned: (tx: Transaction, v: number) => {
      void utxoView.addOwned(tx, v)
    },
    takeReserve: async (_sc?: unknown, minSats?: number) => await utxoView.takeReserve(undefined, minSats)
  }
  const temporal = temporalSourcesFor(cfg.temporalSources ?? ['bsv'], tracker, { wocApiKey: mc.wocApiKey, fetchImpl: mc.fetchImpl })
  const chain = {
    broadcast: async (tx: Transaction) => {
      // attach parent txs so the broadcaster can send Extended Format (Arcade
      // cannot resolve unconfirmed parents from raw txs)
      for (const inp of tx.inputs) {
        if (inp.sourceTransaction || !inp.sourceTXID) continue
        const p = txStore.get(inp.sourceTXID) ?? utxoView.utxos.find((u) => u.tx.id('hex') === inp.sourceTXID)?.tx ?? memRef?.anchorTx(inp.sourceTXID)
        if (p) inp.sourceTransaction = p
      }
      const r = await spendGuard.broadcast(tx)
      if (r.status === 'success') {
        txStore.put(tx)
        events.watch(tx.id('hex'))
        memRef?.metrics.inc('nektar_fee_sats_total', {}, spendGuard.costs().at(-1)?.fee ?? 0, 'total anchor fee spend (sats)')
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
    // events substrate emits — pushed SSE statuses stay hints (handleBroadcasterStatus)
    onStatus: (cb: (ev: TxStatusEvent) => void) => {
      events.on('tx', cb)
      events.start()
    },
    currentHeight: () => tracker.currentHeight(),
    isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
    // §12: header TIMES are the weighted median across temporal sources;
    // single-source (default) is bit-identical to the raw tracker
    headerAtHeight: (h: number) => temporal.headerAtHeight(h),
    tipHeight: () => tracker.currentHeight()
  }
  // the one chain-event substrate: pushed SSE/callback statuses land as
  // notes (unverified hints), this pull probes what a push could not deliver
  // — proofs missed while down, and txs handed rather than broadcast.
  // status reads go through the chain-observer seam; WoC cross-checks a dead
  // broadcaster on main, or whenever the operator configured a WoC URL
  const wocFallback = net === 'main' || mc.wocUrl !== undefined
  const arcTxStatus = (txid: string): Promise<string> => fetchTxStatus(mc, txid, { wocFallback })
  const events = new ChainEvents({
    intervalMs: cfg.confirmEveryMs ?? 30_000,
    height: () => tracker.currentHeight(),
    observe: async (txid) => {
      const tx = await memRef?.fetchTx(txid).catch(() => undefined)
      if (tx?.merklePath !== undefined) return { txid, status: 'mined', blockHeight: tx.merklePath.blockHeight, merklePath: tx.merklePath }
      // a broadcaster rejection verdict is a claim with no possible proof
      // object — only a double-spend is provable (via the conflicting tx's
      // own BUMP). It lands as a hint (resolves waiters, keeps the floor),
      // never on the verified path — same treatment the push path gives it.
      const st = await arcTxStatus(txid)
      if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(st)) events.note({ txid, status: 'rejected' })
      return undefined
    }
  })
  const { knex, store, ledger, wrapBroadcast, leasePredicate } = await openRecordsDb(cfg.dataDir)
  await openTxStore(knex)
  for (const u of utxoView.utxos) txStore.put(u.tx)
  const shardKeys = new Map(Object.entries(keys.shardKeys).map(([s, h]) => [s, PrivateKey.fromHex(h)]))
  // watch intent is a durable record, not an in-memory accident: any tx the
  // journal knows was broadcast-but-not-final gets re-watched on boot, so a
  // crash in the broadcast→watch window can't strand a proof wait
  for (const t of await ledger.walletTxs(['submitted', 'observed', 'submission_unknown'])) events.watch(t.txid)
  reserve.leased = leasePredicate(ledger)
  if (liveTf !== undefined) liveTf.leased = leasePredicate(ledger)
  chain.broadcast = wrapBroadcast(ledger, chain.broadcast)
  chain.broadcast = txLogBroadcast(chain.broadcast)
  const mem = new NektarMemory(
    chain,
    funder as never,
    fundKey,
    {
      ...base,
      wallet: clientWallet ?? liveToolbox?.wallet,
      txStore,
      txStatus: arcTxStatus,
      agentSeed: keys.agentSeed ? PrivateKey.fromHex(keys.agentSeed) : undefined,
      signingKey: keys.logKey ? PrivateKey.fromHex(keys.logKey) : undefined,
      shardKeys,
      maxLevel: Number(nektarEnv('NEKTAR_MAX_LEVEL') ?? 3),
      feeRateSatPerKb: feeRate,
      securityStore: new FileSecurityStore(join(cfg.dataDir, 'security.json')),
      stateStore: new FileStateStore(join(cfg.dataDir, 'state.json')),
      finalityDepth: Number(nektarEnv('NEKTAR_FINALITY_DEPTH') ?? 6)
    },
    store
  )
  memRef = mem
  mem.ledger = ledger
  // restart-recovery pull for proofs that never arrived by push — a fetched
  // BUMP counts only after its root verifies against our own headers (same
  // trust rule as the push path; push stays primary)
  mem.proofLookup = async (txid) => {
    const r = await merklePathMainnet(mc, txid)
    if (!r) return undefined
    const root = r.merklePath.computeRoot(txid)
    return (await tracker.isValidRootForHeight(root, r.merklePath.blockHeight)) ? r.merklePath : undefined
  }
  mem.txLookup = async (txid) => {
    const h: Record<string, string> = {}
    if (mc.wocApiKey) h['woc-api-key'] = mc.wocApiKey
    const r = await (mc.fetchImpl ?? fetch)(`${mc.wocUrl ?? DEFAULT_WOC_URL}/tx/${txid}/hex`, {
      headers: h,
      signal: AbortSignal.timeout(10_000)
    })
    return r.ok ? Transaction.fromHex((await r.text()).trim()) : undefined
  }
  mem.metrics.gauge('nektar_fee_balance_sats', () => utxoView.utxos.reduce((s, u) => s + u.satoshis, 0), 'operator fee-key balance (sats)')
  // outbound-only status push: no callback URL → pull tx statuses from the
  // broadcaster's /events SSE stream into the same handler the webhook uses.
  // Needs no inbound port; Last-Event-ID cursor persists across restarts.
  let stream: { close: () => void } | undefined
  if (!callbackUrl && (mc.callbackToken ?? nektarEnv('NEKTAR_CALLBACK_TOKEN'))) {
    const cursorFile = join(cfg.dataDir, 'sse-cursor')
    stream = statusStream(
      mc,
      async (ev) => {
        if (ev.data === undefined) return
        let j: { txid?: string; txStatus?: string; merklePath?: string; extraInfo?: string }
        try {
          j = JSON.parse(ev.data)
        } catch {
          return
        } // malformed data line — skip
        mem.metrics.inc(
          'nektar_broadcaster_stream_events_total',
          { status: String(j.txStatus ?? '?').toLowerCase() },
          1,
          'broadcaster status stream events received'
        )
        await handleBroadcasterStatus(mem, j)
        // the pushed status is a wake hint: note() records it for
        // statusOf/waitFor, but interest only drops when a verified observe
        // delivers — a push whose BUMP failed verification keeps the pull
        // probe's reconcile floor
        if (typeof j.txid === 'string') {
          const s = String(j.txStatus ?? '').toUpperCase()
          if (s === 'MINED' || s === 'IMMUTABLE') events.note({ txid: j.txid, status: 'mined' })
          else if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(s)) events.note({ txid: j.txid, status: 'rejected' })
        }
      },
      {
        cursor: {
          get: () => {
            try {
              const v = readFileSync(cursorFile, 'utf8').trim()
              return v === '' ? undefined : v
            } catch {
              return undefined
            }
          },
          set: (id) => {
            try {
              writeFileSync(cursorFile, id)
            } catch {
              /* best effort */
            }
          }
        },
        onError: (e) => process.stderr.write(`nektar ${net}: status stream: ${e.message}\n`)
      }
    )
    process.stderr.write(`nektar ${net}: no callback URL — pulling tx statuses from the broadcaster /events stream\n`)
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
  // NEKTAR_FEE_VAULT (testnet/main): anchor funding minted through the
  // fee-vault covenant — setupFeeVault returns the funder's takeReserve
  const vaultSpec = nektarEnv('NEKTAR_FEE_VAULT')
  if (vaultSpec) {
    funder.takeReserve = await setupFeeVault({
      vaultSpec,
      dataDir: cfg.dataDir,
      pkhHex,
      fundKey: fundKey ?? walletSigner(clientWallet as SignerWallet, FUND_SIGN_ARGS),
      fundAddress,
      reserve: utxoSync,
      broadcast: async (tx) => await chain.broadcast(tx),
      currentHeight: async () => await tracker.currentHeight(),
      net,
      log: (s) => process.stderr.write(s)
    })
  }
  // NEKTAR_FUND_TXIDS: funding txs to import directly (no address index needed)
  for (const txid of (nektarEnv('NEKTAR_FUND_TXIDS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)) {
    try {
      for (const u of await importFundingTx(mc, txid, pkhHex)) addIfNew(u)
    } catch (e) {
      process.stderr.write(`nektar ${net}: funding import ${txid}: ${(e as Error).message}\n`)
    }
  }
  const topUp = async () => {
    // tracked-set model: we spend only what we made or were handed. Address-
    // index discovery (WoC) is OPT-IN — NEKTAR_INDEXER_DISCOVERY=1 — because it
    // creates an indexer dependency that doesn't scale. The default funding
    // path is NEKTAR_FUND_TXIDS=<txid|hex:rawtx> (import above).
    if (nektarEnv('NEKTAR_INDEXER_DISCOVERY') === '1') {
      try {
        for (const u of await discoverUtxos(mc, fundAddress)) addIfNew(u)
      } catch (e) {
        process.stderr.write(`nektar ${net}: UTXO discovery unavailable (${(e as Error).message}) — set NEKTAR_FUND_TXIDS=<funding txid>\n`)
      }
    }
    if (utxoView.utxos.length === 0)
      process.stderr.write(
        `nektar ${net}: reserve empty — send coins to ${fundAddress} and set NEKTAR_FUND_TXIDS (or NEKTAR_WAIT_FUNDING=1 to hold for funds)\n`
      )
  }
  // bootstrap UX (NEKTAR_WAIT_FUNDING=1): QR the funding address and hold
  // startup until a few sats land — ONCE, not per tick (the tick loop re-runs
  // topUp every confirmEveryMs and must not block). Detection during the
  // wait: indexer discovery when enabled, p2p 'pay' auto-import, or the
  // operator setting NEKTAR_FUND_TXIDS. NEKTAR_WAIT_FUNDING_MS caps it (30 min).
  const waitForFunding = async () => {
    if (utxoView.utxos.length > 0 || nektarEnv('NEKTAR_WAIT_FUNDING') !== '1') return
    try {
      const QR = await import('qrcode')
      process.stderr.write('\n' + (await QR.toString(fundAddress, { type: 'terminal', small: true })) + '\n')
    } catch {
      /* QR optional — the address prints either way */
    }
    process.stderr.write(`nektar ${net}: waiting for funding — send a few sats to ${fundAddress}\n`)
    const deadline = Date.now() + Number(nektarEnv('NEKTAR_WAIT_FUNDING_MS') ?? 30 * 60_000)
    const pollMs = Number(nektarEnv('NEKTAR_FUND_POLL_MS') ?? 15_000)
    while (utxoView.utxos.length === 0 && Date.now() < deadline) {
      await liveTf?.refresh().catch(() => undefined)
      // event-driven wait: addIfNew resolves the sleep the moment a utxo
      // lands; re-check first so a just-landed utxo can't sleep past us,
      // and a missed signal degrades to the bounded poll, never a hang
      await new Promise<void>((r) => {
        if (utxoView.utxos.length > 0) return r()
        const t = setTimeout(
          () => {
            fundingWake = undefined
            r()
          },
          Math.min(pollMs, Math.max(0, deadline - Date.now()))
        )
        fundingWake = () => {
          fundingWake = undefined
          clearTimeout(t)
          r()
        }
      })
      for (const txid of (nektarEnv('NEKTAR_FUND_TXIDS') ?? '')
        .split(',')
        .map((s) => s.trim())
        .filter(Boolean)) {
        try {
          for (const u of await importFundingTx(mc, txid, pkhHex)) addIfNew(u)
        } catch {
          /* keep polling */
        }
      }
      if (nektarEnv('NEKTAR_INDEXER_DISCOVERY') === '1') {
        try {
          for (const u of await discoverUtxos(mc, fundAddress)) addIfNew(u)
        } catch {
          /* keep polling */
        }
      }
    }
    if (utxoView.utxos.length > 0) {
      process.stderr.write(`nektar ${net}: funded ✓ — ${utxoView.utxos.reduce((s, u) => s + u.satoshis, 0)} sats across ${utxoView.utxos.length} utxo(s)\n`)
    } else {
      process.stderr.write(`nektar ${net}: funding wait timed out — booting unfunded (anchors fail until ${fundAddress} has a utxo; use NEKTAR_FUND_TXIDS)\n`)
    }
  }
  await waitForFunding()
  await topUp()
  let busy = false
  let proofPullAt = 0
  const tick = async () => {
    if (busy) return
    busy = true
    try {
      persistKeys()
      await mem.flushDue()
      // proofs are pushed (webhook or status stream); the events substrate's
      // pull picks up what a push could not deliver — a proof missed while
      // the instance was down, or a tx handed rather than broadcast. This
      // pass registers interest; delivery arrives via chain.onStatus.
      for (const { txid } of mem.unprovenAnchors()) events.watch(txid)
      // ledger txs too: one shared probe per txid serves the watcher's
      // statusOf short-circuit — no second /tx fetch per inflight tx
      for (const w of await ledger.walletTxs(['submitted', 'submission_unknown', 'observed'])) events.watch(w.txid)
      // a bounded, rotating pull: each tick tries a few unproven txs so a
      // long list can never serialize minutes of lookups inside one pass —
      // push (callbacks/status stream) stays the primary proof channel
      {
        const unproven = txStore.unproven()
        const pull = unproven.length <= 8 ? unproven : Array.from({ length: 8 }, (_, i) => unproven[(proofPullAt + i) % unproven.length])
        proofPullAt = unproven.length > 0 ? (proofPullAt + 8) % unproven.length : 0
        for (const txid of pull) await mem.fetchTx(txid)
      }
      await topUp()
      persistKeys()
    } catch (e) {
      process.stderr.write(`nektar confirmer(${net}): ${(e as Error).message}\n`)
    } finally {
      busy = false
    }
  }
  const timer = setInterval(
    () => {
      void tick()
    },
    cfg.confirmEveryMs ?? (ttn ? 30_000 : 60_000)
  )
  timer.unref()
  const ops = {
    wallet: async () => {
      const balanceSats = utxoView.utxos.reduce((s, u) => s + u.satoshis, 0)
      const costs = spendGuard.costs()
      const avg = costs.length ? Math.round(costs.reduce((s, c) => s + c.fee, 0) / costs.length) : 0
      return {
        address: fundAddress,
        balanceSats,
        utxos: utxoView.utxos.length,
        spentTodaySats: spendGuard.spentToday(),
        caps: { maxFeePerTx: mc.maxFeePerTx!, maxPerDay: mc.maxSpendPerDay!, minIntervalMs: mc.minBroadcastIntervalMs! },
        avgFeePerAnchorSats: avg,
        runwayAnchors: avg > 0 ? Math.floor(balanceSats / avg) : null
      }
    },
    costs: () => spendGuard.costs(),
    // fundingImport MCP tool: credit a tx paying the fee address into the
    // reserve without a restart (same path as NEKTAR_FUND_TXIDS)
    importFunding: async (txid: string) => {
      const added = await importFundingTx(mc, txid, pkhHex)
      for (const u of added) addIfNew(u)
      const balanceSats = utxoView.utxos.reduce((s, u) => s + u.satoshis, 0)
      return { imported: added.length, utxos: utxoView.utxos.length, balanceSats, address: fundAddress }
    },
    // fundingRefresh MCP tool: reconcile the reserve against chain truth.
    //   - outputs the unspent index reports live at the fee address are
    //     revived (stale spent-marks from dead unbroadcast txs cleared) or
    //     imported fresh
    //   - entries whose parent tx the broadcaster rejected get dropped, and
    //     any input they consumed that is still live on-chain is revived
    //   - entries the broadcaster knows (mempool or mined) but which are no
    //     longer unspent get marked spent
    // Fixes the phantom-entry/dust-pick class without a restart.
    refreshFunding: async () => {
      // toolbox custody: the wallet's spendable view IS the reconcile — a
      // refresh re-queries listOutputs, then import whatever the index sees.
      // reserve.json bookkeeping (revive/drop/mark) only exists in legacy mode.
      if (liveTf !== undefined) {
        await liveTf.refresh()
        let imported = 0
        try {
          for (const u of await discoverUtxos(mc, fundAddress)) {
            const before = utxoView.utxos.length
            addIfNew(u)
            if (utxoView.utxos.length > before) imported++
          }
        } catch {
          /* discovery down — wallet view still refreshed */
        }
        const balanceSats = utxoView.utxos.reduce((s, u) => s + u.satoshis, 0)
        return { refreshed: true, imported, utxos: utxoView.utxos.length, balanceSats, address: fundAddress }
      }
      const r = reserve as any
      const op = (txid: string, vout: number) => `${txid}:${vout}`
      const revive = (o: string, tx?: Transaction): boolean => {
        const [, v] = o.split(':')
        const vout = Number(v)
        r.spent.delete(o)
        if (r.utxos.some((u: { tx: Transaction; vout: number }) => op(u.tx.id('hex'), u.vout) === o)) return false
        const e = (r.entries as Array<{ txid: string; vout: number; satoshis: number; txHex: string }>).find((x) => op(x.txid, x.vout) === o)
        if (e) {
          r.utxos.push({ tx: Transaction.fromHex(e.txHex), vout: e.vout, satoshis: e.satoshis })
          return true
        }
        if (tx) {
          reserve.addOwned(tx, vout)
          return true
        }
        return false
      }
      let revived = 0,
        dropped = 0,
        marked = 0,
        pending = 0
      let live = new Map<string, { tx: Transaction; vout: number }>()
      try {
        const list = await discoverUtxos(mc, fundAddress)
        live = new Map(list.map((u) => [op(u.tx.id('hex'), u.vout), u]))
      } catch {
        /* discovery down — still can drop rejected entries below */
      }
      for (const [o, u] of live) {
        if (revive(o, u.tx)) revived++
      }
      for (const u of [...reserve.utxos]) {
        const o = op(u.tx.id('hex'), u.vout)
        if (live.has(o)) continue
        const txid = u.tx.id('hex')
        // the broadcaster's verdict decides: known live status (mempool or
        // mined) → the output is spent since it is absent from the unspent
        // set; an explicit rejection → the parent is dead, drop and revive
        // inputs; undecidable → leave for the next reconcile
        const status = await arcTxStatus(txid)
        if (status !== '' && !['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(status)) {
          reserve.markSpent(txid, u.vout)
          marked++
          continue
        }
        if (status === '') {
          pending++
          continue
        }
        // dead tx — drop the entry and revive any of its inputs still live
        r.entries = (r.entries as any[]).filter((e) => op(e.txid, e.vout) !== o)
        r.utxos = (r.utxos as any[]).filter((x) => op(x.tx.id('hex'), x.vout) !== o)
        dropped++
        for (const inp of u.tx.inputs) {
          const io = op(inp.sourceTXID ?? '', inp.sourceOutputIndex ?? 0)
          if (live.has(io) && revive(io, live.get(io)!.tx)) revived++
        }
      }
      r.save?.()
      const balanceSats = reserve.utxos.reduce((s, u) => s + u.satoshis, 0)
      return { revived, dropped, marked, pending, utxos: reserve.utxos.length, balanceSats, address: fundAddress }
    }
  }
  const { TransactionWatcher } = await import('@nektar/automation')
  const watcher = new TransactionWatcher(
    ledger,
    async (txid) => {
      // the shared substrate already probed this txid — use its answer
      // instead of a second /tx fetch per watched tx per pass
      const seen = events.statusOf(txid)
      if (seen?.status === 'rejected' || seen?.status === 'double-spent') return { status: 'rejected', source: 'broadcaster', reason: seen.status }
      if (seen?.status === 'mined') return { status: 'proven', source: 'broadcaster' }
      const status = await arcTxStatus(txid)
      const SEEN = ['RECEIVED', 'SENT_TO_NETWORK', 'ACCEPTED_BY_NETWORK', 'SEEN_ON_NETWORK', 'SEEN_MULTIPLE_NODES', 'STUMP_PROCESSING', 'MINED', 'IMMUTABLE']
      if (['REJECTED', 'DOUBLE_SPEND_ATTEMPTED'].includes(status)) return { status: 'rejected', source: 'broadcaster', reason: status }
      if (SEEN.includes(status)) return { status: 'seen', source: 'broadcaster' }
      return undefined
    },
    async () => {
      await ops.refreshFunding?.().catch(() => {})
    }
  )
  let watching = false
  const watchOnce = () => {
    if (watching) return
    watching = true
    void watcher
      .tick()
      .catch(() => {})
      .finally(() => {
        watching = false
      })
  }
  // event-driven: the shared substrate's sweeps, height advances and pushed
  // broadcaster statuses may each settle a watched tx — no private interval
  events.on('tick', watchOnce)
  events.on('height', watchOnce)
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
      await releaseLock()
      await tick()
      await liveToolbox?.close()
      await knex.destroy()
    }
  }
}
