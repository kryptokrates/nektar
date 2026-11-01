// openRuntime: one production entry for the MCP / HTTP servers.
//   MAMEM_NETWORK=mock     offline MockChain (mines per tx) — zero setup, in-memory
//   MAMEM_NETWORK=regtest  Teranode regtest: persistent records (sqlite), anchor
//                          state, security state, funding reserve and keys in
//                          MAMEM_DATA_DIR; a confirmer attaches BUMPs.
//   MAMEM_NETWORK=testnet  same shape as regtest; refused until an endpoint and
//                          funding are configured (no real funds without approval).
import { existsSync, readFileSync, writeFileSync, mkdirSync, chmodSync } from 'node:fs'
import { join } from 'node:path'
import { PrivateKey, MerklePath, type Transaction } from '@bsv/sdk'
import { MockChain, MockFunder, TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, merklePathAtHeight, parseLegacyBlock, bootstrapReserve, ArcBroadcaster, WocTracker, fetchArcFeeRate, merklePathMainnet, discoverUtxos, SpendGuard, type RegtestConfig, type MainnetConfig } from '@mamem/chain'
import { KnexShardStore, type ShardStore } from '@mamem/log'
import { MamemMemory, type MamemMemoryOptions } from './memory.js'
import { FileSecurityStore, FileStateStore } from './securitystore.js'
import { readKeyFile, writeKeyFile, passphraseFromEnv, KeystoreError } from './keystore.js'

export interface RuntimeConfig {
  network: 'mock' | 'regtest' | 'testnet' | 'main'
  dataDir: string
  regtest?: RegtestConfig & { fundWif: string }
  main?: MainnetConfig & {
    fundWif?: string // fee key; absent → load/generate fee.key.json
    passphrase?: string // MAMEM_KEY_PASSPHRASE — mandatory on main
    feeRateSatPerKb?: number // default: max(100, ARC policy)
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
}

export function configFromEnv (env = process.env): RuntimeConfig {
  const network = (env.MAMEM_NETWORK ?? 'mock') as RuntimeConfig['network']
  const dataDir = env.MAMEM_DATA_DIR ?? join(process.cwd(), '.mamem')
  const secret = (k: string) => env[k] ?? (env[`${k}_FILE`] && existsSync(env[`${k}_FILE`]!) ? readFileSync(env[`${k}_FILE`]!, 'utf8').trim() : undefined)
  const cfg: RuntimeConfig = { network, dataDir, modelId: env.MAMEM_MODEL_ID, batchLeaves: Number(env.MAMEM_BATCH ?? 4), autoMine: env.MAMEM_AUTOMINE === '1', confirmEveryMs: Number(env.MAMEM_CONFIRM_MS ?? 5000), clearanceAuthorities: (env.MAMEM_CLEARANCE_AUTHORITIES ?? '').split(',').map(s => s.trim()).filter(Boolean) }
  if (network === 'regtest' || network === 'testnet') {
    const fundWif = secret('MAMEM_FUND_WIF') ?? secret('MAMEM_REGTEST_WIF')
    const rpcPass = secret('MAMEM_RPC_PASS')
    if (!fundWif || !rpcPass) throw new Error(`${network}: MAMEM_FUND_WIF (or _FILE) and MAMEM_RPC_PASS (or _FILE) are required`)
    cfg.regtest = {
      fundWif,
      rpcUrl: env.MAMEM_RPC_URL ?? 'http://localhost:9292/',
      rpcUser: env.MAMEM_RPC_USER ?? 'teranode',
      rpcPass,
      propagationUrl: env.MAMEM_PROPAGATION_URL ?? 'http://localhost:8833/tx',
      assetUrl: env.MAMEM_ASSET_URL ?? 'http://localhost:8090/api/v1'
    }
  }
  if (network === 'main') {
    if (env.MAMEM_MAINNET_CONFIRM !== 'I_UNDERSTAND') {
      throw new Error('mainnet mode spends REAL BSV. Refusing to start. Set MAMEM_MAINNET_CONFIRM=I_UNDERSTAND to proceed.')
    }
    cfg.confirmEveryMs = Number(env.MAMEM_CONFIRM_MS ?? 60_000)
    cfg.main = {
      fundWif: secret('MAMEM_FUND_WIF'),
      passphrase: secret('MAMEM_KEY_PASSPHRASE'),
      arcUrl: env.MAMEM_ARC_URL,
      arcApiKey: secret('MAMEM_ARC_API_KEY'),
      wocUrl: env.MAMEM_WOC_URL,
      wocApiKey: secret('MAMEM_WOC_API_KEY'),
      feeRateSatPerKb: env.MAMEM_FEE_RATE_SAT_PER_KB ? Number(env.MAMEM_FEE_RATE_SAT_PER_KB) : undefined,
      maxFeePerTx: Number(env.MAMEM_MAX_FEE_SATS_PER_TX ?? 1000),
      maxSpendPerDay: Number(env.MAMEM_MAX_SPEND_SATS_PER_DAY ?? 20_000),
      minBroadcastIntervalMs: Number(env.MAMEM_MIN_BROADCAST_INTERVAL_MS ?? 10_000)
    }
  }
  return cfg
}

interface KeyFile { agentSeed: string, logKey: string, shardKeys: Record<string, string> }
/** keys are generated once into the data dir (0600) unless supplied; never logged.
 *  Encrypted at rest when a passphrase is set; mandatory on main. */
function loadKeys (dataDir: string, pass: string | undefined, requireEnc = false): { keys: KeyFile, save: () => void } {
  const file = join(dataDir, 'keys.json')
  const keys: KeyFile = existsSync(file)
    ? JSON.parse(readKeyFile(file, pass, requireEnc)) as KeyFile
    : { agentSeed: PrivateKey.fromRandom().toHex(), logKey: PrivateKey.fromRandom().toHex(), shardKeys: {} }
  const save = () => writeKeyFile(file, JSON.stringify(keys), pass)
  if (!existsSync(file)) save()
  return { keys, save }
}

/** operator fee key (pays all anchor fees): MAMEM_FUND_WIF wins; otherwise a
 *  dedicated fee.key.json generated once in the data dir — encrypted on main. */
function loadFeeKey (dataDir: string, wif: string | undefined, pass: string | undefined, requireEnc: boolean): PrivateKey {
  if (wif) return PrivateKey.fromWif(wif)
  const file = join(dataDir, 'fee.key.json')
  if (existsSync(file)) {
    const j = JSON.parse(readKeyFile(file, pass, requireEnc)) as { wif: string }
    return PrivateKey.fromWif(j.wif)
  }
  const key = PrivateKey.fromRandom()
  writeKeyFile(file, JSON.stringify({ wif: key.toWif() }), pass)
  return key
}

export interface Runtime {
  mem: MamemMemory
  close: () => Promise<void>
  network: RuntimeConfig['network']
  /** mainnet-only ops surface for /v1/wallet + /v1/costs */
  ops?: {
    wallet: () => Promise<{ address: string, balanceSats: number, utxos: number, spentTodaySats: number, caps: { maxFeePerTx: number, maxPerDay: number, minIntervalMs: number }, avgFeePerAnchorSats: number, runwayAnchors: number | null }>
    costs: () => Array<{ txid: string, bytes: number, fee: number, at: string }>
  }
}

export async function openRuntime (cfg: RuntimeConfig): Promise<Runtime> {
  if (cfg.network === 'testnet') throw new Error('testnet: not enabled — configure an ARC/Arcade endpoint and approve testnet funding first')
  const base: MamemMemoryOptions = { batchPolicy: { maxLeaves: cfg.batchLeaves ?? 4, maxAgeMs: 60_000 }, modelId: cfg.modelId ?? 'unspecified', clearanceAuthorities: cfg.clearanceAuthorities }
  if (cfg.network === 'mock') {
    const chain = new MockChain(); chain.verifyScripts = true
    const funder = new MockFunder(); funder.topup(Number(process.env.MAMEM_DEMO_UTXOS ?? 200))
    for (const t of funder.parents) chain.registerParent(t)
    const broadcast = chain.broadcast.bind(chain)
    chain.broadcast = async (tx: Transaction) => { const r = await broadcast(tx); if (r.status === 'success') chain.mine(1); return r }
    const mem = new MamemMemory(chain, funder, funder.keyPair, base)
    // auto-mined: every anchor is immediately provable
    const t = setInterval(() => { for (const { shard, txid } of mem.unprovenAnchors()) { const mp = chain.merklePathOf(txid); if (mp) mem.attachProof(shard, txid, mp) } }, 1000); t.unref()
    return { mem, network: 'mock', close: async () => { clearInterval(t) } }
  }
  if (cfg.network === 'main') {
    const mc = cfg.main!
    mkdirSync(cfg.dataDir, { recursive: true })
    const pass = mc.passphrase
    if (!pass) throw new KeystoreError('mainnet requires MAMEM_KEY_PASSPHRASE (or _FILE) for keys at rest')
    // single-writer lock (same as regtest)
    const lockFile = join(cfg.dataDir, 'instance.lock')
    const me = `${process.pid}@${(await import('node:os')).hostname()}`
    if (existsSync(lockFile)) {
      const l = JSON.parse(readFileSync(lockFile, 'utf8')) as { owner: string, beat: number }
      if (l.owner !== me && Date.now() - l.beat < 20_000) throw new Error(`data dir ${cfg.dataDir} is in use by another mamem instance (${l.owner}); stop it first`)
    }
    const beat = () => writeFileSync(lockFile, JSON.stringify({ owner: me, beat: Date.now() }))
    beat()
    const beatTimer = setInterval(beat, 5_000); beatTimer.unref()

    const { keys, save } = loadKeys(cfg.dataDir, pass, true)
    const fundKey = loadFeeKey(cfg.dataDir, mc.fundWif, pass, true)
    const fundAddress = fundKey.toPublicKey().toAddress()
    process.stderr.write(`mamem mainnet: operator fee key ${fundAddress} — fund this address to enable anchoring\n`)

    const tracker = new WocTracker(mc)
    const rawBroadcaster = new ArcBroadcaster(mc)
    const reserve = new ReserveFunder(fundKey, join(cfg.dataDir, 'reserve.json'))
    let memRef: MamemMemory | undefined // hoisted for the guard's valueOf
    const guardedBroadcasts = new Map<string, Transaction>()
    const valueOf = (txid: string, vout: number): number | undefined =>
      reserve.utxos.find(u => u.tx.id('hex') === txid && u.vout === vout)?.satoshis ??
      guardedBroadcasts.get(txid)?.outputs[vout]?.satoshis ??
      memRef?.anchorTx(txid)?.outputs[vout]?.satoshis
    const spendGuard = new SpendGuard(rawBroadcaster, valueOf, {
      maxFeePerTx: mc.maxFeePerTx, maxPerDay: mc.maxSpendPerDay,
      minIntervalMs: mc.minBroadcastIntervalMs, ledgerFile: join(cfg.dataDir, 'spend-ledger.json')
    })
    const feeRate = Math.max(mc.feeRateSatPerKb ?? 100, await fetchArcFeeRate(mc).catch(() => 100))
    const funder = { keyPair: fundKey, get utxos () { return reserve.utxos }, markSpent: (t: string, v: number) => reserve.markSpent(t, v), addOwned: (tx: Transaction, v: number) => reserve.addOwned(tx, v), takeReserve: async () => await reserve.takeReserve() }
    const chain = {
      broadcast: async (tx: Transaction) => {
        const r = await spendGuard.broadcast(tx)
        if (r.status === 'success') {
          guardedBroadcasts.set(tx.id('hex'), tx)
          memRef?.metrics.inc('mamem_fee_sats_total', {}, spendGuard.costs().at(-1)?.fee ?? 0, 'total anchor fee spend (sats)')
        } else if (r.description?.startsWith('SpendGuard refused')) {
          memRef?.metrics.inc('mamem_spend_refused_total', { reason: r.description.split(': ')[1]?.split(' ')[0] ?? 'unknown' }, 1, 'broadcasts refused by SpendGuard')
        }
        return r
      },
      currentHeight: () => tracker.currentHeight(),
      isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
      headerAtHeight: (h: number) => tracker.headerAtHeight(h),
      tipHeight: () => tracker.currentHeight()
    }
    const knexMod = (await import('knex')).default
    const knex = knexMod({ client: 'sqlite3', connection: { filename: join(cfg.dataDir, 'records.sqlite') }, useNullAsDefault: true })
    const store: ShardStore = new KnexShardStore(knex)
    await store.init?.()
    const shardKeys = new Map(Object.entries(keys.shardKeys).map(([s, h]) => [s, PrivateKey.fromHex(h)]))
    const mem = new MamemMemory(chain as never, funder as never, fundKey, {
      ...base,
      agentSeed: PrivateKey.fromHex(keys.agentSeed), signingKey: PrivateKey.fromHex(keys.logKey), shardKeys,
      maxLevel: Number(process.env.MAMEM_MAX_LEVEL ?? 3),
      feeRateSatPerKb: feeRate,
      securityStore: new FileSecurityStore(join(cfg.dataDir, 'security.json')),
      stateStore: new FileStateStore(join(cfg.dataDir, 'state.json')),
      finalityDepth: Number(process.env.MAMEM_FINALITY_DEPTH ?? 6)
    }, store)
    memRef = mem
    mem.metrics.gauge('mamem_fee_balance_sats', () => reserve.utxos.reduce((s, u) => s + u.satoshis, 0), 'operator fee-key balance (sats)')
    const persistKeys = () => {
      let changed = false
      for (const [s, k] of shardKeys) if (keys.shardKeys[s] !== k.toHex()) { keys.shardKeys[s] = k.toHex(); changed = true }
      if (changed) save()
    }
    // reserve top-up: import fee-address UTXOs discovered via WoC
    const topUp = async () => {
      for (const u of await discoverUtxos(mc, fundAddress)) {
        if (!reserve.isSpent(u.tx.id('hex'), u.vout) && !reserve.utxos.some(x => x.tx.id('hex') === u.tx.id('hex') && x.vout === u.vout)) {
          reserve.addOwned(u.tx, u.vout)
        }
      }
      if (reserve.utxos.length === 0) process.stderr.write(`mamem mainnet: reserve empty — send BSV to ${fundAddress}\n`)
    }
    await topUp()
    // confirmer: attach BUMPs for unproven anchors via ARC/WoC
    let busy = false
    const tick = async () => {
      if (busy) return
      busy = true
      try {
        persistKeys()
        await mem.flushDue()
        for (const { shard, txid } of mem.unprovenAnchors()) {
          const mp = await merklePathMainnet(mc, txid)
          if (mp) mem.attachProof(shard, txid, mp.merklePath)
        }
        await topUp()
        persistKeys()
      } catch (e) { process.stderr.write(`mamem confirmer(main): ${(e as Error).message}\n`) } finally { busy = false }
    }
    const timer = setInterval(() => { void tick() }, cfg.confirmEveryMs ?? 60_000); timer.unref()
    const ops = {
      wallet: async () => {
        const balanceSats = reserve.utxos.reduce((s, u) => s + u.satoshis, 0)
        const costs = spendGuard.costs()
        const avg = costs.length ? Math.round(costs.reduce((s, c) => s + c.fee, 0) / costs.length) : 0
        return {
          address: fundAddress, balanceSats, utxos: reserve.utxos.length,
          spentTodaySats: spendGuard.spentToday(),
          caps: { maxFeePerTx: mc.maxFeePerTx!, maxPerDay: mc.maxSpendPerDay!, minIntervalMs: mc.minBroadcastIntervalMs! },
          avgFeePerAnchorSats: avg,
          runwayAnchors: avg > 0 ? Math.floor(balanceSats / avg) : null
        }
      },
      costs: () => spendGuard.costs()
    }
    return {
      mem, network: 'main', ops,
      close: async () => {
        clearInterval(timer); clearInterval(beatTimer)
        await tick(); await knex.destroy()
        try { (await import('node:fs')).rmSync(lockFile) } catch { /* already gone */ }
      }
    }
  }
  const rc = cfg.regtest!
  mkdirSync(cfg.dataDir, { recursive: true })
  // single writer per data dir: a live heartbeat from another instance refuses startup
  const lockFile = join(cfg.dataDir, 'instance.lock')
  const me = `${process.pid}@${(await import('node:os')).hostname()}`
  if (existsSync(lockFile)) {
    const l = JSON.parse(readFileSync(lockFile, 'utf8')) as { owner: string, beat: number }
    if (l.owner !== me && Date.now() - l.beat < 20_000) throw new Error(`data dir ${cfg.dataDir} is in use by another mamem instance (${l.owner}); stop it first`)
  }
  const beat = () => writeFileSync(lockFile, JSON.stringify({ owner: me, beat: Date.now() }))
  beat()
  const beatTimer = setInterval(beat, 5_000); beatTimer.unref()
  const { keys, save } = loadKeys(cfg.dataDir, passphraseFromEnv())
  const fundKey = PrivateKey.fromWif(rc.fundWif)
  const tracker = new TeranodeRPCTracker(rc)
  const broadcaster = new SpendGuard(new TeranodeBroadcaster(rc.propagationUrl), () => 0, { unlimited: true })
  const auth = 'Basic ' + Buffer.from(`${rc.rpcUser}:${rc.rpcPass}`).toString('base64')
  const rpc = async (method: string, ...params: unknown[]): Promise<any> => {
    const r = await fetch(rc.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }), signal: AbortSignal.timeout(15_000) })
    const j = await r.json() as { result?: unknown, error?: unknown }
    if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    return j.result
  }
  let lastMined = 0
  const mineOne = async () => {
    const [hash] = await rpc('generatetoaddress', 1, fundKey.toPublicKey().toAddress([0x6f])) as string[]
    const hdr = await rpc('getblockheader', hash, true) as { height: number }
    lastMined = Math.max(lastMined, hdr.height)
  }
  const reserve = new ReserveFunder(fundKey, join(cfg.dataDir, 'reserve.json'))
  if (reserve.utxos.length < 5) {
    // own treasury chain in the data dir; split mined before use
    const s = await bootstrapReserve(rc, fundKey, reserve, rpc, { treasuryFile: join(cfg.dataDir, 'treasury.json') })
    if (!s && reserve.utxos.length === 0) throw new Error('regtest: could not fund the reserve (no unspent treasury or early coinbase for the fund key)')
    if (s) await mineOne()
  }
  const funder = { keyPair: fundKey, get utxos () { return reserve.utxos }, markSpent: (t: string, v: number) => reserve.markSpent(t, v), addOwned: (tx: Transaction, v: number) => reserve.addOwned(tx, v), takeReserve: async () => await reserve.takeReserve() }
  const chain = {
    broadcast: (tx: Transaction) => broadcaster.broadcast(tx),
    currentHeight: () => tracker.currentHeight(),
    isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
    headerAtHeight: (h: number) => tracker.headerAtHeight(h),
    tipHeight: async () => Math.max(lastMined, await tracker.currentHeight())
  }
  const knexMod = (await import('knex')).default
  const knex = knexMod({ client: 'sqlite3', connection: { filename: join(cfg.dataDir, 'records.sqlite') }, useNullAsDefault: true })
  const store: ShardStore = new KnexShardStore(knex)
  await store.init?.()
  const shardKeys = new Map(Object.entries(keys.shardKeys).map(([s, h]) => [s, PrivateKey.fromHex(h)]))
  const mem = new MamemMemory(chain as never, funder as never, fundKey, {
    ...base,
    agentSeed: PrivateKey.fromHex(keys.agentSeed), signingKey: PrivateKey.fromHex(keys.logKey), shardKeys,
    maxLevel: Number(process.env.MAMEM_MAX_LEVEL ?? 3),
    securityStore: new FileSecurityStore(join(cfg.dataDir, 'security.json')),
    stateStore: new FileStateStore(join(cfg.dataDir, 'state.json')),
    finalityDepth: 1
  }, store)
  // persist newly generated shard keys (rotation replaces the entry)
  const persistKeys = () => {
    let changed = false
    for (const [s, k] of shardKeys) if (keys.shardKeys[s] !== k.toHex()) { keys.shardKeys[s] = k.toHex(); changed = true }
    if (changed) save()
  }
  // confirmer: BUMPs for every unproven anchor; optional auto-mine; timed flush
  let busy = false
  let scannedTo = 0
  const minedAt = new Map<string, number>(); const firstSeen = new Map<string, number>(); const abandoned = new Set<string>()
  const tick = async () => {
    if (busy) return
    busy = true
    try {
      persistKeys()
      await mem.flushDue()
      const waiting = mem.unprovenAnchors().filter(w => !abandoned.has(w.txid))
      if (waiting.length && cfg.autoMine) await mineOne()
      // index each block ONCE (txid → height); never rescan per anchor
      const tip = Math.max(lastMined, await tracker.currentHeight())
      if (scannedTo === 0) scannedTo = Math.max(0, tip - 50)
      for (let h = scannedTo + 1, n = 0; h <= tip && n < 25; h++, n++) {
        const hdr = await tracker.headerAtHeight(h)
        if (!hdr) break
        const res = await fetch(`${rc.assetUrl}/block_legacy/${hdr.hash}`, { headers: { Authorization: auth }, signal: AbortSignal.timeout(20_000) })
        if (!res.ok) break
        for (const t of parseLegacyBlock(new Uint8Array(await res.arrayBuffer())).txids) minedAt.set(t, h)
        scannedTo = h
      }
      let attached = 0
      for (const { shard, txid } of waiting) {
        const h = minedAt.get(txid)
        if (h === undefined) {
          const seen = firstSeen.get(txid) ?? tip; firstSeen.set(txid, seen)
          // never mined within 100 blocks (e.g. replaced by a conflicting spend): stop waiting
          if (scannedTo - seen > 100) { abandoned.add(txid); process.stderr.write(`mamem confirmer: anchor ${txid} not mined after 100 blocks — abandoned (check for a conflicting spend)\n`) }
          continue
        }
        const mp = await merklePathAtHeight(rc, txid, tracker, h)
        if (mp) { mem.attachProof(shard, txid, new MerklePath(mp.blockHeight, mp.path)); attached++ }
      }
      if (waiting.length) process.stderr.write(`mamem confirmer: waiting=${waiting.length} attached=${attached} scanned=${scannedTo}/${tip}\n`)
      persistKeys()
    } catch (e) { process.stderr.write(`mamem confirmer: ${(e as Error).message}\n`) } finally { busy = false }
  }
  const timer = setInterval(() => { void tick() }, cfg.confirmEveryMs ?? 5000); timer.unref()
  return {
    mem, network: 'regtest',
    close: async () => {
      clearInterval(timer); clearInterval(beatTimer)
      await tick(); await knex.destroy()
      try { (await import('node:fs')).rmSync(lockFile) } catch { /* already gone */ }
    }
  }
}
