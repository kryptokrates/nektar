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
import { MockChain, MockFunder, TeranodeBroadcaster, TeranodeRPCTracker, ReserveFunder, merklePathFor, bootstrapReserve, type RegtestConfig } from '@mamem/chain'
import { KnexShardStore, type ShardStore } from '@mamem/log'
import { MamemMemory, type MamemMemoryOptions } from './memory.js'
import { FileSecurityStore, FileStateStore } from './securitystore.js'

export interface RuntimeConfig {
  network: 'mock' | 'regtest' | 'testnet'
  dataDir: string
  regtest?: RegtestConfig & { fundWif: string }
  modelId?: string
  batchLeaves?: number
  /** regtest only: mine a block when anchors wait for confirmation */
  autoMine?: boolean
  confirmEveryMs?: number
}

export function configFromEnv (env = process.env): RuntimeConfig {
  const network = (env.MAMEM_NETWORK ?? 'mock') as RuntimeConfig['network']
  const dataDir = env.MAMEM_DATA_DIR ?? join(process.cwd(), '.mamem')
  const secret = (k: string) => env[k] ?? (env[`${k}_FILE`] && existsSync(env[`${k}_FILE`]!) ? readFileSync(env[`${k}_FILE`]!, 'utf8').trim() : undefined)
  const cfg: RuntimeConfig = { network, dataDir, modelId: env.MAMEM_MODEL_ID, batchLeaves: Number(env.MAMEM_BATCH ?? 4), autoMine: env.MAMEM_AUTOMINE === '1', confirmEveryMs: Number(env.MAMEM_CONFIRM_MS ?? 5000) }
  if (network !== 'mock') {
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
  return cfg
}

interface KeyFile { agentSeed: string, logKey: string, shardKeys: Record<string, string> }
/** keys are generated once into the data dir (0600) unless supplied; never logged */
function loadKeys (dataDir: string): { keys: KeyFile, save: () => void } {
  const file = join(dataDir, 'keys.json')
  const keys: KeyFile = existsSync(file)
    ? JSON.parse(readFileSync(file, 'utf8')) as KeyFile
    : { agentSeed: PrivateKey.fromRandom().toHex(), logKey: PrivateKey.fromRandom().toHex(), shardKeys: {} }
  const save = () => { writeFileSync(file, JSON.stringify(keys)); try { chmodSync(file, 0o600) } catch { /* windows */ } }
  if (!existsSync(file)) save()
  return { keys, save }
}

export interface Runtime { mem: MamemMemory, close: () => Promise<void>, network: RuntimeConfig['network'] }

export async function openRuntime (cfg: RuntimeConfig): Promise<Runtime> {
  if (cfg.network === 'testnet') throw new Error('testnet: not enabled — configure an ARC/Arcade endpoint and approve testnet funding first')
  const base: MamemMemoryOptions = { batchPolicy: { maxLeaves: cfg.batchLeaves ?? 4, maxAgeMs: 60_000 }, modelId: cfg.modelId ?? 'unspecified' }
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
  const rc = cfg.regtest!
  mkdirSync(cfg.dataDir, { recursive: true })
  const { keys, save } = loadKeys(cfg.dataDir)
  const fundKey = PrivateKey.fromWif(rc.fundWif)
  const tracker = new TeranodeRPCTracker(rc)
  const broadcaster = new TeranodeBroadcaster(rc.propagationUrl)
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
  const tick = async () => {
    if (busy) return
    busy = true
    try {
      persistKeys()
      await mem.flushDue()
      const waiting = mem.unprovenAnchors()
      if (waiting.length && cfg.autoMine) await mineOne()
      for (const { shard, txid } of waiting) {
        const mp = await merklePathFor(rc, txid, tracker).catch(() => null)
        if (mp) mem.attachProof(shard, txid, new MerklePath(mp.blockHeight, mp.path))
      }
      persistKeys()
    } catch (e) { process.stderr.write(`mamem confirmer: ${(e as Error).message}\n`) } finally { busy = false }
  }
  const timer = setInterval(() => { void tick() }, cfg.confirmEveryMs ?? 5000); timer.unref()
  return { mem, network: 'regtest', close: async () => { clearInterval(timer); await tick(); await knex.destroy() } }
}
