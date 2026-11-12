// Test backend on the real regtest stack — one code path for tests and
// production: broadcasts go through the Teranode propagation API, BUMPs are
// the node's own proofs, script validation happens at admission, not in a
// simulator. Helpers resolve to undefined / tests call skipUnlessStack when
// no stack is configured, so suites degrade to skips rather than failing
// where a node is unreachable.
//
// Env (repo .env supplies these):
//   NEKTAR_RPC_URL / NEKTAR_RPC_USER / NEKTAR_RPC_PASS  — Teranode JSON-RPC
//   NEKTAR_PROPAGATION_URL / NEKTAR_ASSET_URL          — submit + utxo/proofs
//   NEKTAR_REGTEST_WIF                                  — treasury key; without
//     it a generated key is persisted to .state/testkit-treasury.json and the
//     first stackFunder() generates ~120 blocks to it for mature coinbases
import { PrivateKey, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { existsSync, readFileSync, writeFileSync, mkdirSync, statSync, rmSync } from 'node:fs'
import { dirname, join } from 'node:path'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut, Funder } from './interfaces.js'
import {
  TeranodeRPCTracker,
  TeranodeBroadcaster,
  regtestTx,
  merklePathFor,
  parseLegacyBlock,
  utxoStatus,
  ReserveFunder,
  bootstrapReserve,
  type RegtestConfig
} from './regtest.js'

// ---------------------------------------------------------------------------
// stack connection

// Repo .env carries NEKTAR_RPC_*/NEKTAR_REGTEST_WIF for local runs — load it
// once so `pnpm test` works from any package dir without manual exports.
// Existing process.env values always win.
let envLoaded = false
function loadRepoEnv(): void {
  if (envLoaded) return
  envLoaded = true
  let dir = process.cwd()
  for (let up = 0; up < 5; up++) {
    const f = join(dir, '.env')
    if (existsSync(f)) {
      for (const line of readFileSync(f, 'utf8').split(/\r?\n/)) {
        const m = /^\s*([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)\s*$/.exec(line)
        if (m !== null && process.env[m[1]] === undefined) process.env[m[1]] = m[2].replace(/^["']|["']$/g, '')
      }
      return
    }
    const parent = dirname(dir)
    if (parent === dir) return
    dir = parent
  }
}

export function stackCfgFromEnv(): RegtestConfig | undefined {
  loadRepoEnv()
  const pass = process.env.NEKTAR_RPC_PASS
  if (pass === undefined || pass === '') return undefined
  return {
    rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
    rpcUser: process.env.NEKTAR_RPC_USER ?? 'teranode',
    rpcPass: pass,
    propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
    assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
  }
}

let rpcCounter = 0
async function stackRpc(cfg: RegtestConfig, method: string, ...params: unknown[]): Promise<any> {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64') },
    body: JSON.stringify({ jsonrpc: '1.0', id: ++rpcCounter, method, params }),
    signal: AbortSignal.timeout(15_000)
  })
  const text = await r.text()
  let j: { result?: any; error?: { message?: string } }
  try {
    j = JSON.parse(text) as typeof j
  } catch {
    throw new Error(`rpc ${method}: non-JSON response ${r.status}: ${text.slice(0, 140)}`)
  }
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

/** cheap liveness probe — the gate behind skipUnlessStack */
export async function stackUp(cfg: RegtestConfig = stackCfgFromEnv() ?? ({} as RegtestConfig)): Promise<boolean> {
  if (cfg.rpcUrl === undefined) return false
  for (let i = 0; i < 3; i++) {
    try {
      await stackRpc(cfg, 'getblockchaininfo')
      return true
    } catch {
      await new Promise((r) => setTimeout(r, 500 * (i + 1)))
    }
  }
  return false
}

/** node:test gate — call first thing in a test that needs the chain */
export async function skipUnlessStack(t: { skip: (msg?: string) => void }, cfg?: RegtestConfig): Promise<RegtestConfig | undefined> {
  const c = cfg ?? stackCfgFromEnv()
  if (c === undefined || !(await stackUp(c))) {
    t.skip('regtest stack unreachable (NEKTAR_RPC_*)')
    return undefined
  }
  return c
}

// ---------------------------------------------------------------------------
// chain surface — the same shape tests used on the in-memory stub, backed by the node

export class StackChain implements ChainTracker, Broadcaster {
  private tracker: TeranodeRPCTracker
  private submit: TeranodeBroadcaster
  private treasuryAddr?: string
  constructor(
    private cfg: RegtestConfig,
    treasuryKey?: PrivateKey
  ) {
    this.tracker = new TeranodeRPCTracker(cfg)
    this.submit = new TeranodeBroadcaster(cfg.propagationUrl)
    if (treasuryKey !== undefined) this.treasuryAddr = treasuryKey.toPublicKey().toAddress([0x6f])
  }
  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    return await this.submit.broadcast(tx)
  }
  async currentHeight(): Promise<number> {
    return await this.tracker.currentHeight()
  }
  async tipHeight(): Promise<number> {
    return await this.tracker.currentHeight()
  }
  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    return await this.tracker.isValidRootForHeight(root, height)
  }
  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    return await this.tracker.headerAtHeight(height)
  }
  /** mempool + mined, with the node's merkle proof attached once sealed.
   *  Retries briefly on not-found: propagation ingestion lags the broadcast
   *  ACK under load, so a just-aired tx can 404 for a beat. */
  async getTx(txid: string): Promise<Transaction | undefined> {
    for (let i = 0; i < 12; i++) {
      const tx = await regtestTx(this.cfg, txid)
      if (tx !== undefined) return tx
      await new Promise((r) => setTimeout(r, 250))
    }
    return undefined
  }
  async merklePathOf(txid: string): Promise<MerklePath | undefined> {
    const tx = await this.getTx(txid)
    if (tx?.merklePath !== undefined) return tx.merklePath
    const r = await merklePathFor(this.cfg, txid, this.tracker)
    if (r === null) return undefined
    return new MerklePath(r.blockHeight, r.path)
  }
  /** utxo live-status — 'unspent' | 'spent' | 'unknown' */
  async utxoStatus(txid: string, vout: number): Promise<'unspent' | 'spent' | 'unknown'> {
    return await utxoStatus(this.cfg, txid, vout)
  }
  /** raw JSON-RPC passthrough for stack-only verbs (invalidateblock, etc.) */
  async rpc(method: string, ...params: unknown[]): Promise<any> {
    return await stackRpc(this.cfg, method, ...params)
  }
  /** generatetoaddress to the treasury address; waits for the tip to catch up */
  async mine(n = 1): Promise<void> {
    const addr = this.treasuryAddr ?? new PrivateKey(1).toPublicKey().toAddress([0x6f])
    const hashes = (await stackRpc(this.cfg, 'generatetoaddress', n, addr)) as string[]
    // Teranode's getblockchaininfo.blocks lags seconds behind generate; the
    // header index serves the new blocks almost immediately — wait on that.
    for (const h of hashes) {
      for (let i = 0; i < 40; i++) {
        const hdr = await stackRpc(this.cfg, 'getblockheader', h).catch(() => undefined)
        if (hdr !== undefined) break
        await new Promise((r) => setTimeout(r, 100))
      }
    }
    // asset index catch-up: /merkle_proof only answers once the block's
    // subtree is sealed — block_legacy serving the generated hash is not
    // enough (subtree sealing lags it), so fetch the block, take its coinbase
    // txid, and poll /merkle_proof/{coinbase}: sealed for one member means
    // sealed for the whole block.
    const auth = 'Basic ' + Buffer.from(`${this.cfg.rpcUser}:${this.cfg.rpcPass}`).toString('base64')
    for (const h of hashes) {
      let coinbase: string | undefined
      for (let i = 0; i < 120 && coinbase === undefined; i++) {
        const r = await fetch(`${this.cfg.assetUrl}/block_legacy/${h}`, { signal: AbortSignal.timeout(4_000) }).catch(() => null)
        if (r?.ok === true) {
          coinbase = parseLegacyBlock(new Uint8Array(await r.arrayBuffer())).txids[0]
        } else await new Promise((r) => setTimeout(r, 250))
      }
      if (coinbase === undefined) continue
      for (let i = 0; i < 120; i++) {
        const r = await fetch(`${this.cfg.assetUrl}/merkle_proof/${coinbase}`, { headers: { Authorization: auth }, signal: AbortSignal.timeout(4_000) }).catch(
          () => null
        )
        if (r?.ok === true) break
        await new Promise((r) => setTimeout(r, 250))
      }
    }
  }
}

// ---------------------------------------------------------------------------
// treasury + funder — real funded keys on the real chain

// .state lives beside the repo-root .env (the same upward walk loadRepoEnv
// does) — resolving it per-cwd meant each package dir kept its own reserve,
// and a package without one re-scanned ~2900 coinbase blocks under the lock.
function stateDir(): string {
  let dir = process.cwd()
  for (let up = 0; up < 5; up++) {
    if (existsSync(join(dir, '.env'))) return join(dir, '.state')
    const parent = dirname(dir)
    if (parent === dir) break
    dir = parent
  }
  return join(process.cwd(), '.state')
}
const STATE_DIR = stateDir()

/** shared treasury: the same key/file real-stack + e2e-regtest use. Opens
 *  once per process; stackFunder splits reserve outputs into fresh keys. */
export class StackTreasury {
  key: PrivateKey
  chain: StackChain
  reserve: ReserveFunder
  private constructor(
    readonly cfg: RegtestConfig,
    file = join(STATE_DIR, 'treasury.json')
  ) {
    loadRepoEnv()
    this.key =
      process.env.NEKTAR_REGTEST_WIF !== undefined && process.env.NEKTAR_REGTEST_WIF !== ''
        ? PrivateKey.fromWif(process.env.NEKTAR_REGTEST_WIF)
        : this.loadOrMakeKey(file)
    this.chain = new StackChain(cfg, this.key)
    this.reserve = new ReserveFunder(this.key, join(STATE_DIR, 'reserve.json'))
    // cross-process lease: takeReserve consults .state/leases.json so two
    // parallel test files never pick the same entry (same-entry picks were
    // the UTXO_SPENT retry storm). Leases carry an expiry so dead processes
    // release theirs.
    this.reserve.leased = async (txid, vout) => {
      const f = join(STATE_DIR, 'leases.json')
      return await withTreasuryLock(async () => {
        let leases: Record<string, number> = {}
        try {
          leases = JSON.parse(readFileSync(f, 'utf8'))
        } catch {
          /* missing/corrupt → fresh */
        }
        const now = Date.now()
        for (const k of Object.keys(leases)) if (leases[k] < now) delete leases[k]
        const key = `${txid}:${vout}`
        if (leases[key] !== undefined) {
          writeFileSync(f, JSON.stringify(leases))
          return true
        }
        leases[key] = now + 300_000
        writeFileSync(f, JSON.stringify(leases))
        return false
      })
    }
  }
  private loadOrMakeKey(file: string): PrivateKey {
    const kf = join(STATE_DIR, 'testkit-treasury.json')
    if (existsSync(kf)) return PrivateKey.fromWif(JSON.parse(readFileSync(kf, 'utf8')).wif)
    const key = PrivateKey.fromRandom()
    mkdirSync(dirname(kf), { recursive: true })
    writeFileSync(kf, JSON.stringify({ wif: key.toWif() }))
    return key
  }
  static async open(cfg: RegtestConfig): Promise<StackTreasury> {
    const t = new StackTreasury(cfg)
    await withTreasuryLock(() => t.ensureFunded())
    return t
  }
  /** reserve entries exist → nothing to do; else mature coinbases are
   *  generated to the treasury key and split into the reserve pool. */
  async ensureFunded(): Promise<void> {
    // runs under the treasury lock — re-read the reserve so a process that
    // waited sees the state the lock holder just produced instead of
    // bootstrapping all over again
    this.reserve.reload()
    if (this.reserve.utxos.length > 0) return
    const addr = this.key.toPublicKey().toAddress([0x6f])
    const tip = await this.chain.currentHeight()
    // coinbases mature at +100 on regtest — generate enough to have ≥1 mature
    if (tip < 120) await this.chain.mine(120 - tip + 1)
    const r = await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), {
      treasuryFile: join(STATE_DIR, 'treasury.json')
    })
    if (this.reserve.utxos.length === 0 && r === null) {
      // no mature coinbase found for our key — generate 110 to it, then split.
      // Only the freshly generated range can hold usable coinbases — the
      // bootstrap scan above already ruled the older mature heights out.
      const t0 = await this.chain.currentHeight()
      await this.chain.mine(110)
      await this.reserve.scanCoinbases(this.cfg, (m, ...p) => stackRpc(this.cfg, m, ...p), t0, (await this.chain.currentHeight()) - 101)
      await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), { treasuryFile: join(STATE_DIR, 'treasury.json') })
    }
    if (this.reserve.utxos.length === 0) throw new Error('testkit: treasury could not be funded — no mature coinbases')
  }
  async mine(n = 1): Promise<void> {
    await this.chain.mine(n)
  }
  /** split the treasury tail into another ~60 reserve entries when the pool
   *  runs dry mid-suite — the split chain is self-funding on regtest. */
  async refill(): Promise<void> {
    await withTreasuryLock(async () => {
      // another process may have refilled while this one waited on the lock —
      // splitting the treasury tail again would burn it for nothing
      this.reserve.reload()
      if (this.reserve.utxos.length > 0) return
      const r = await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), {
        treasuryFile: join(STATE_DIR, 'treasury.json')
      })
      if (r === null) throw new Error('testkit: treasury tail spent — cannot refill reserve')
      // zeroconf: the fresh reserve outputs are spendable as mempool children —
      // no block needed before the next fund() can draw on them
    })
  }
}

type ReserveSrc = { tx: Transaction; vout: number; satoshis: number }
const LEASE_MS = 300_000

/** Read leases.json, drop expired rows. Caller holds the treasury lock. */
function readLeases(file: string): Record<string, number> {
  let leases: Record<string, number> = {}
  try {
    leases = JSON.parse(readFileSync(file, 'utf8'))
  } catch {
    /* missing/corrupt → fresh */
  }
  const now = Date.now()
  for (const k of Object.keys(leases)) if (leases[k] < now) delete leases[k]
  return leases
}

/** Pick reserve inputs covering `need` sats and lease ONLY those picks —
 *  reload + selection + lease write happen in one critical section, so two
 *  parallel processes can never pick the same entry (the UTXO_SPENT retry
 *  storm) nor lease entries they didn't take. Returns null when the pool
 *  can't cover — caller refills under the same protocol. */
async function leaseReserve(t: StackTreasury, need: number): Promise<ReserveSrc[] | null> {
  const leasesFile = join(STATE_DIR, 'leases.json')
  return await withTreasuryLock(async () => {
    t.reserve.reload()
    const leases = readLeases(leasesFile)
    const op = (u: ReserveSrc) => `${u.tx.id('hex')}:${u.vout}`
    const free = t.reserve.utxos
      .filter((u) => !t.reserve.isSpent(u.tx.id('hex'), u.vout) && u.satoshis >= 546 && leases[op(u)] === undefined)
      .sort((a, b) => a.satoshis - b.satoshis)
    // prefer ONE entry covering the whole spend; else accumulate smallest-first
    const single = free.find((u) => u.satoshis >= need)
    const srcs: ReserveSrc[] = single !== undefined ? [single] : []
    let inSats = single?.satoshis ?? 0
    if (single === undefined) {
      for (const u of free) {
        if (inSats >= need || srcs.length >= 16) break
        srcs.push(u)
        inSats += u.satoshis
      }
    }
    if (inSats < need) {
      writeFileSync(leasesFile, JSON.stringify(leases))
      return null
    }
    for (const s of srcs) leases[op(s)] = Date.now() + LEASE_MS
    writeFileSync(leasesFile, JSON.stringify(leases))
    return srcs
  })
}

// `node --test` runs files in parallel processes sharing .state/*.json —
// serialize bootstrap/refill (treasury-tail splits) across processes.
async function withTreasuryLock<T>(fn: () => Promise<T>): Promise<T> {
  const lock = join(STATE_DIR, 'treasury.lock')
  const deadline = Date.now() + 180_000
  for (;;) {
    try {
      mkdirSync(lock)
      break
    } catch {
      if (Date.now() > deadline) throw new Error('testkit: treasury lock wait exceeded')
      try {
        const st = statSync(lock)
        if (Date.now() - st.mtimeMs > 120_000) {
          rmSync(lock, { recursive: true })
          continue
        }
      } catch {
        /* raced */
      }
      await new Promise((r) => setTimeout(r, 150))
    }
  }
  try {
    return await fn()
  } finally {
    rmSync(lock, { recursive: true })
  }
}

let treasuryPromise: Promise<StackTreasury> | undefined
/** per-process shared treasury — lazily opened, bootstrapped once */
export function stackTreasury(cfg?: RegtestConfig): Promise<StackTreasury> {
  const c = cfg ?? stackCfgFromEnv()
  if (c === undefined) throw new Error('testkit: no regtest stack env (NEKTAR_RPC_*)')
  return (treasuryPromise ??= StackTreasury.open(c))
}

/** the test funder's surface on real money: a fresh identity key holding `outputs`
 *  mined P2PKH utxos of `sats` each, created by one split tx from the shared
 *  reserve. `parents` returns the real funding txs for API parity — nothing
 *  needs registering, the node already validated and mined them. */
export class StackFunder implements Funder {
  keyPair: PrivateKey
  private utxos: FundingOut[] = []
  private fundingTx?: Transaction
  private constructor(key: PrivateKey) {
    this.keyPair = key
  }
  /** a funder with no utxos — take() always throws; no stack needed */
  static dry(key: PrivateKey = PrivateKey.fromRandom()): StackFunder {
    return new StackFunder(key)
  }
  static async fund(t: StackTreasury, outputs: number, sats = 10_000): Promise<StackFunder> {
    const f = new StackFunder(PrivateKey.fromRandom())
    const addr = f.keyPair.toPublicKey().toAddress([0x6f])
    // stale reserve entries (never-broadcast parents, already-spent) prune on
    // broadcast failure — allow enough attempts to walk past a rotten pool
    const need = outputs * sats + 5_000
    for (let attempt = 0; attempt < 8; attempt++) {
      // pick + lease under ONE lock: the old per-candidate leased() hook took
      // the treasury lock AND leased every scanned entry for 5 min — the
      // first process to scan starved all others into refill(), serializing
      // the suite behind the lock. Batch selection fixes both.
      const srcs = await leaseReserve(t, need)
      if (srcs === null) {
        // pool drained below the floor — split another ~60 entries off the
        // treasury tail and retry rather than failing the suite
        if (attempt < 6) {
          await t.refill()
          continue
        }
        throw new Error('testkit: reserve empty — bootstrap failed')
      }
      const tx = new Transaction()
      for (const src of srcs) tx.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(t.key) })
      for (let i = 0; i < outputs; i++) tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
      tx.addOutput({ lockingScript: new P2PKH().lock(t.key.toPublicKey().toAddress([0x6f])), change: true })
      await tx.fee()
      await tx.sign()
      const r = await t.chain.broadcast(tx)
      // reserve accounting under the lock: save() writes the whole in-memory
      // file, so unlocked markSpent/addOwned from parallel processes clobber
      // each other (lost entries, resurrected spends → UTXO_SPENT storms and
      // phantom "reserve empty"). reload first so this process sees the union.
      if (r.status === 'success') {
        await withTreasuryLock(async () => {
          t.reserve.reload()
          for (const src of srcs) t.reserve.markSpent(src.tx.id('hex'), src.vout)
          const changeVout = tx.outputs.length - 1
          if ((tx.outputs[changeVout].satoshis ?? 0) > 0) t.reserve.addOwned(tx, changeVout)
        })
        f.fundingTx = tx
        for (let i = 0; i < outputs; i++) f.utxos.push({ tx, vout: i, satoshis: sats })
        // zeroconf: outputs are spendable while unconfirmed — tests mine only
        // where a proof is actually asserted
        return f
      }
      await withTreasuryLock(async () => {
        t.reserve.reload()
        for (const src of srcs) t.reserve.markSpent(src.tx.id('hex'), src.vout)
      })
      if (!/UTXO_SPENT|already.*spent|missing.*(input|parent)/i.test(r.description ?? ''))
        throw new Error(`testkit: fund broadcast failed ${r.description ?? ''}`.slice(0, 160))
    }
    throw new Error('testkit: fund broadcast raced 8 times')
  }
  async take(satoshis: number): Promise<FundingOut> {
    const i = this.utxos.findIndex((u) => u.satoshis >= satoshis)
    if (i < 0) throw new Error('StackFunder: no utxo of that size')
    const u = this.utxos.splice(i, 1)[0]
    return u
  }
  markSpent(txid: string, vout: number): void {
    const i = this.utxos.findIndex((u) => u.tx.id('hex') === txid && u.vout === vout)
    if (i >= 0) this.utxos.splice(i, 1)
  }
  /** return a broadcast tx's output to the pool (change/escrow outs) */
  addOwned(tx: Transaction, vout: number): void {
    const o = tx.outputs[vout]
    if (o !== undefined) this.utxos.push({ tx, vout, satoshis: o.satoshis ?? 0 })
  }
  address(): string {
    return this.keyPair.toPublicKey().toAddress([0x6f])
  }
  get parents(): Transaction[] {
    return this.fundingTx !== undefined ? [this.fundingTx] : []
  }
}

/** a bare chain handle — for tests that only broadcast/track, no funder */
export async function stackChain(t: { skip: (m?: string) => void }): Promise<StackChain | undefined> {
  const cfg = await skipUnlessStack(t)
  if (cfg === undefined) return undefined
  return (await stackTreasury(cfg)).chain
}

/** one-call convenience: `const { chain, funder } = await stackWorld(t, 30)` */
export async function stackWorld(
  t: { skip: (m?: string) => void },
  outputs = 30,
  sats = 10_000
): Promise<{ chain: StackChain; funder: StackFunder; treasury: StackTreasury } | undefined> {
  const cfg = await skipUnlessStack(t)
  if (cfg === undefined) return undefined
  const treasury = await stackTreasury(cfg)
  const funder = await StackFunder.fund(treasury, outputs, sats)
  return { chain: treasury.chain, funder, treasury }
}
