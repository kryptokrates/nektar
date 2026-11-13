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
import { nektarEnv, setNektarEnv } from '@nektar/core'
import { PrivateKey, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { existsSync, readFileSync, writeFileSync, mkdirSync, statSync, rmSync, appendFileSync } from 'node:fs'
import { dirname, join } from 'node:path'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut, Funder, TxStatusEvent } from './interfaces.js'
import { ChainEvents } from './events.js'
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
import { TxStore } from './txstore.js'

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
        if (m !== null && nektarEnv(m[1]) === undefined) setNektarEnv(m[1], m[2].replace(/^["']|["']$/g, ''))
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
  const pass = nektarEnv('NEKTAR_RPC_PASS')
  if (pass === undefined || pass === '') return undefined
  return {
    rpcUrl: nektarEnv('NEKTAR_RPC_URL') ?? 'http://localhost:9292/',
    rpcUser: nektarEnv('NEKTAR_RPC_USER') ?? 'teranode',
    rpcPass: pass,
    propagationUrl: nektarEnv('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx',
    assetUrl: nektarEnv('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1'
  }
}

let rpcCounter = 0

// timing instrumentation — NEKTAR_DEBUG_TIMING=1 logs where a test's wallclock
// actually goes: mine() rounds, treasury-lock waits, proof-push latency.
/** watched broadcasts that never seal are dropped after this — regtest txs
 *  only prove once something mines, and tests often broadcast without mining. */
const WATCH_TTL_MS = 60_000
const dbgTimings = (): boolean => nektarEnv('NEKTAR_DEBUG_TIMING') === '1'
const dbgLog = (m: string): void => {
  process.stderr.write(`[testkit ${process.pid}] ${m}\n`)
}
const dbg = { mineMs: 0, mineN: 0 }
function dbgMine(ms: number): void {
  if (!dbgTimings()) return
  dbg.mineMs += ms
  dbg.mineN++
  dbgLog(`mine() ${ms}ms — total ${dbg.mineN} mines, ${dbg.mineMs}ms`)
}

async function stackRpc(cfg: RegtestConfig, method: string, ...params: unknown[]): Promise<any> {
  // last param may be a timeout-seconds override for slow verbs
  // (generatetoaddress for n blocks legitimately outlives the 15s default)
  let timeoutMs = 15_000
  if (typeof params[params.length - 1] === 'object' && params[params.length - 1] !== null && '__timeoutMs' in (params[params.length - 1] as object)) {
    timeoutMs = (params.pop() as { __timeoutMs: number }).__timeoutMs
  }
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64') },
    body: JSON.stringify({ jsonrpc: '1.0', id: ++rpcCounter, method, params }),
    signal: AbortSignal.timeout(timeoutMs)
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

/** cheap liveness probe — the gate behind skipUnlessStack. `probe` bounds
 *  the check for callers where a down stack is a skip, not a failure
 *  (the `pnpm -r test` entry — it must not stall on 3×15s RPC timeouts). */
export async function stackUp(
  cfg: RegtestConfig = stackCfgFromEnv() ?? ({} as RegtestConfig),
  probe?: { attempts?: number; timeoutMs?: number }
): Promise<boolean> {
  if (cfg.rpcUrl === undefined) return false
  const attempts = probe?.attempts ?? 3
  const timeoutMs = probe?.timeoutMs ?? 15_000
  for (let i = 0; i < attempts; i++) {
    try {
      await stackRpc(cfg, 'getblockchaininfo', { __timeoutMs: timeoutMs })
      return true
    } catch {
      if (i + 1 < attempts) await new Promise((r) => setTimeout(r, Math.min(500 * (i + 1), timeoutMs)))
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
  private known = new Map<string, Transaction>()
  private rejectedSet = new Set<string>()
  /** the shared chain-event substrate — one poll loop serving every consumer;
   *  txid TTL keeps never-sealing regtest txs from pinning the event loop. */
  private events: ChainEvents
  constructor(
    private cfg: RegtestConfig,
    treasuryKey?: PrivateKey
  ) {
    this.tracker = new TeranodeRPCTracker(cfg)
    this.submit = new TeranodeBroadcaster(cfg.propagationUrl)
    if (treasuryKey !== undefined) this.treasuryAddr = treasuryKey.toPublicKey().toAddress([0x6f])
    this.events = new ChainEvents({
      intervalMs: 500,
      ttlMs: WATCH_TTL_MS,
      height: () => this.tracker.currentHeight(),
      observe: async (txid) => {
        const auth = 'Basic ' + Buffer.from(`${this.cfg.rpcUser}:${this.cfg.rpcPass}`).toString('base64')
        const r = await fetch(`${this.cfg.assetUrl}/merkle_proof/${txid}`, {
          headers: { Authorization: auth },
          signal: AbortSignal.timeout(4_000)
        }).catch(() => null)
        if (r?.ok !== true) return undefined
        try {
          const merklePath = MerklePath.fromBinary([...new Uint8Array(await r.arrayBuffer())])
          // attach the proof to the cached tx so the sync getTx view answers
          // 'mined', not 'seen' — txState consumers rely on it
          const cached = this.known.get(txid)
          if (cached !== undefined) cached.merklePath = merklePath
          if (dbgTimings()) dbgLog(`proof pushed for ${txid.slice(0, 12)}…`)
          return { txid, status: 'mined', blockHeight: merklePath.blockHeight, merklePath }
        } catch {
          return undefined /* unparseable proof → keep watching */
        }
      }
    })
  }
  /** broadcast; the txid joins the status watch so 'mined' + proof push on seal */
  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const r = await this.submit.broadcast(tx)
    const txid = tx.id('hex')
    if (r.status === 'success') {
      this.known.set(txid, tx)
      this.events.watch(txid)
    } else if (/double.?spend|conflict|already.*spent|UTXO_SPENT|mandatory|invalid|reject/i.test(r.description ?? '')) {
      // definitive rejects only — missing-parent and non-final are transient
      // (lagging ingestion, locktime before height): retried, never terminal.
      // A broadcast-time refusal is a final state — push it like 'mined' so
      // onTxRejected → engine.hint clears the reconcile gate immediately.
      this.rejectedSet.add(txid)
      this.events.report({ txid, status: 'rejected' })
    }
    return r
  }
  /** sync chain view — the same contract the in-memory stack carried: txs
   *  this chain broadcast, merklePath attached when the subtree seals.
   *  NektarMemory.txState calls this WITHOUT await — an async signature
   *  returns a Promise (≠undefined) and every query would read 'seen',
   *  which is exactly the regtest reconcile failure this must not hide. */
  getTx(txid: string): Transaction | undefined {
    return this.known.get(txid)
  }
  /** definitive broadcast rejection, sync — feed to engine reconcile gates */
  isRejected(txid: string): boolean {
    return this.rejectedSet.has(txid)
  }
  /** status push, the same shape statusStream gives live networks: Teranode
   *  has no per-tx event feed, so the shared events substrate polls watched
   *  broadcasts against /merkle_proof and 'mined' fires with the real BUMP
   *  once the subtree seals — markProven runs push-side, not on a timer. */
  onStatus(cb: (ev: TxStatusEvent) => void): void {
    this.events.on('tx', cb)
    this.events.start()
  }
  /** expose the substrate — runtimes share this instance's poller for
   *  confirmer/ledger watches instead of running private proof loops */
  get chainEvents(): ChainEvents {
    return this.events
  }
  /** release the status watch — the poll loop's fetches hold keep-alive sockets
   *  that pin the event loop open, so stackWorld's t.after teardown calls this. */
  stopWatching(): void {
    this.events.stop()
  }
  /** wait until a broadcast tx carries its MerklePath — subtrees seal on a
   *  ~10s cadence, so proofs are inherently async; callers that NEED one
   *  synchronously (BEEF payloads) say so explicitly rather than mine()
   *  stalling for every consumer. */
  async waitProof(txid: string, timeoutMs = 30_000): Promise<Transaction | undefined> {
    await this.events.waitFor(txid, timeoutMs)
    return this.known.get(txid)
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
  /** node-fetched variant — for txids this chain did not itself broadcast
   *  (foreign parents, lookups). The sync getTx above is the chain-view the
   *  engine consumes; this one hits getrawtransaction + the proof endpoint. */
  async fetchTx(txid: string): Promise<Transaction | undefined> {
    for (let i = 0; i < 12; i++) {
      const tx = await regtestTx(this.cfg, txid)
      if (tx !== undefined) return tx
      await new Promise((r) => setTimeout(r, 250))
    }
    return undefined
  }
  async merklePathOf(txid: string): Promise<MerklePath | undefined> {
    const tx = this.getTx(txid)
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
  /** mempool txs awaiting a seal — lets test drivers skip rounds that would
   *  mine an empty block (one ~15ms RPC vs ~0.2-3s for generate+seal waits).
   *  Teranode keeps an all-ones sentinel entry in getrawmempool — not a tx. */
  async pendingBroadcasts(): Promise<number> {
    const mp: unknown = await stackRpc(this.cfg, 'getrawmempool').catch(() => undefined)
    if (!Array.isArray(mp)) return 1 // mempool unreadable → mine anyway
    return mp.filter((t) => t !== 'f'.repeat(64)).length
  }
  /** generatetoaddress to the treasury address; waits for the tip to catch up */
  async mine(n = 1): Promise<void> {
    const t0 = Date.now()
    const addr = this.treasuryAddr ?? new PrivateKey(1).toPublicKey().toAddress([0x6f])
    // Teranode throttles concurrent generate calls ("Too busy") — parallel test
    // files legitimately collide here, so retry the throttle with backoff.
    // Batched n>10 is chunked: the node's own RPC layer aborts generate calls
    // that run past ~30s, so big batches go in 10-block pieces.
    // capture the pre-generate tip so the post-wait below can converge on a
    // real height: generatetoaddress returns before Teranode seals, and under
    // load the seal can lag tens of seconds — callers that read the height
    // right after mine() (refund deadlines!) must see the new blocks
    const h0 = await this.currentHeight().catch(() => undefined)
    const hashes: string[] = []
    for (let done = 0; done < n;) {
      const step = Math.min(10, n - done)
      let got: string[] | undefined
      for (let i = 0; i < 20 && got === undefined; i++) {
        got = await stackRpc(this.cfg, 'generatetoaddress', step, addr, { __timeoutMs: 25_000 })
          .then((h) => h as string[])
          .catch(async (e: unknown) => {
            // parallel test files collide two ways: the throttle ("too busy") and
            // stale candidates (a peer's block sealed first) — both are retriable
            if (!/too busy|overload|queue.*full|503|job not found|stale|timed out/i.test(String(e))) throw e
            await new Promise((r) => setTimeout(r, 200 + Math.random() * 300))
            return undefined
          })
      }
      if (got === undefined) throw new Error('testkit: generatetoaddress stayed busy')
      hashes.push(...got)
      done += step
    }
    // Teranode's getblockchaininfo.blocks lags seconds behind generate; the
    // header index serves the new blocks almost immediately — wait on that.
    // Sealing is ordered: the tip hash being indexed implies all ancestors are,
    // so only it needs the wait (mine(40) would otherwise pay 40 round-trips).
    const tip = hashes[hashes.length - 1]
    for (let i = 0; i < 40; i++) {
      const hdr = await stackRpc(this.cfg, 'getblockheader', tip).catch(() => undefined)
      if (hdr !== undefined) break
      await new Promise((r) => setTimeout(r, 100))
    }
    // generatetoaddress can report before the chain serves the new height —
    // the fixed header wait above can pass while currentHeight() still trails.
    // Converge on the real tip (h0+n) — this is a condition, not a timeout:
    // if the node never reports the generated blocks, mine() is right to wait
    // rather than return a chain state callers can't act on.
    if (h0 !== undefined) {
      const target = h0 + n
      for (let i = 0; i < 100; i++) {
        const h = await this.currentHeight().catch(() => 0)
        if (h >= target) break
        await new Promise((r) => setTimeout(r, 300))
      }
    }
    // asset index catch-up: /merkle_proof only answers once the block's
    // subtree is sealed. Wait briefly for it (a sealed tip means proofs are
    // pushable now), but bound the wait — under suite load the asset index
    // lags ~30s and stalling every mine on it costs more than the async
    // watcher's poll tick. Beyond the bound the push path carries proofs.
    // Nothing watched ⇒ no proof consumer can exist for this block — under
    // load the bound was paid on every mine for nobody, so skip the wait.
    const auth = 'Basic ' + Buffer.from(`${this.cfg.rpcUser}:${this.cfg.rpcPass}`).toString('base64')
    if (this.events.pending > 0) {
      const deadline = Date.now() + 3_000
      let coinbase: string | undefined
      while (coinbase === undefined && Date.now() < deadline) {
        const r = await fetch(`${this.cfg.assetUrl}/block_legacy/${tip}`, { signal: AbortSignal.timeout(1_500) }).catch(() => null)
        if (r?.ok === true) {
          const body = await r.arrayBuffer().catch(() => null)
          if (body !== null) coinbase = parseLegacyBlock(new Uint8Array(body)).txids[0]
          else await new Promise((r) => setTimeout(r, 200))
        } else await new Promise((r) => setTimeout(r, 200))
      }
      while (coinbase !== undefined && Date.now() < deadline) {
        const r = await fetch(`${this.cfg.assetUrl}/merkle_proof/${coinbase}`, {
          headers: { Authorization: auth },
          signal: AbortSignal.timeout(1_500)
        }).catch(() => null)
        if (r?.ok === true) break
        await new Promise((r) => setTimeout(r, 200))
      }
    }
    // the subtree is sealed by here — push watched proofs now rather than
    // leaving consumers to the next poll tick (mine(); confirm() determinism)
    for (let i = 0; i < 5 && this.events.pending > 0; i++) {
      await this.events.tick()
      if (this.events.pending > 0) await new Promise((r) => setTimeout(r, 100))
    }
    dbgMine(Date.now() - t0)
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

/** shared treasury: the same KEY as real-stack + e2e-regtest, but its own
 *  chain file (.state/testkit-treasury-chain.json) — each consumer tracks an
 *  outpoint chain independently so a concurrent spend can't race the file.
 *  Opens once per process; stackFunder splits reserve outputs into fresh keys. */
export class StackTreasury {
  key: PrivateKey
  chain: StackChain
  reserve: ReserveFunder
  private constructor(
    readonly cfg: RegtestConfig,
    file = join(STATE_DIR, 'treasury.json')
  ) {
    loadRepoEnv()
    const wif = nektarEnv('NEKTAR_REGTEST_WIF')
    this.key = wif !== undefined ? PrivateKey.fromWif(wif) : this.loadOrMakeKey(file)
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
      treasuryFile: join(STATE_DIR, 'testkit-treasury-chain.json'), namespace: 'testkit'
    })
    if (this.reserve.utxos.length === 0 && r === null) {
      // no mature coinbase found for our key — generate 110 to it, then split.
      // Only the freshly generated range can hold usable coinbases — the
      // bootstrap scan above already ruled the older mature heights out.
      const t0 = await this.chain.currentHeight()
      await this.chain.mine(110)
      // bound the scan by what was generated, not the (lagging) reported tip —
      // getblockchaininfo.blocks trails generatetoaddress by seconds under load,
      // which can leave toHeight < t0 and scan nothing. The asset index and its
      // throttle also lag the generate — retry briefly: both settle fast.
      for (let i = 0; i < 20 && this.reserve.utxos.length === 0; i++) {
        await this.reserve.scanCoinbases(this.cfg, (m, ...p) => stackRpc(this.cfg, m, ...p), t0, t0 + 10)
        if (this.reserve.utxos.length === 0) await new Promise((r) => setTimeout(r, 500))
      }
      await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), { treasuryFile: join(STATE_DIR, 'testkit-treasury-chain.json'), namespace: 'testkit' })
    }
    if (this.reserve.utxos.length === 0) throw new Error('testkit: treasury could not be funded — no mature coinbases')
  }
  async mine(n = 1): Promise<void> {
    await this.chain.mine(n)
  }
  /** split the treasury tail into another ~60 reserve entries when the pool
   *  can't cover `need` — entries merely existing is not enough: they may all
   *  be leased by other processes or too small to cover this spend. */
  async refill(need: number): Promise<void> {
    await withTreasuryLock(async () => {
      // another process may have refilled while this one waited on the lock —
      // splitting the treasury tail again would burn it for nothing
      this.reserve.reload()
      const leases = readLeases(join(STATE_DIR, 'leases.json'))
      const op = (u: { tx: Transaction; vout: number }) => `${u.tx.id('hex')}:${u.vout}`
      const free = this.reserve.utxos.filter((u) => u.satoshis >= 546 && leases[op(u)] === undefined).sort((a, b) => b.satoshis - a.satoshis)
      // same coverage predicate leaseReserve applies: single ≥need or 16 largest
      const coverable = free.some((u) => u.satoshis >= need) || free.slice(0, 16).reduce((a, u) => a + u.satoshis, 0) >= need
      if (coverable) return
      const r = await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), {
        treasuryFile: join(STATE_DIR, 'testkit-treasury-chain.json'), namespace: 'testkit'
      })
      if (r === null) {
        // name the real cause: the file tip either drained below the split
        // floor or is spent — the fix is different (top the file up vs re-point it)
        let why = 'no usable coinbase in the recent window'
        try {
          const t = JSON.parse(readFileSync(join(STATE_DIR, 'testkit-treasury-chain.json'), 'utf8')) as {
            treasury?: { txid: string; vout: number; txHex: string }
          }
          if (t.treasury !== undefined) {
            const parent = (await TxStore.read(t.treasury.txHex, undefined)).tx
            const sats = parent.outputs[t.treasury.vout]?.satoshis ?? 0
            const status = await this.chain.utxoStatus(t.treasury.txid, t.treasury.vout)
            why = sats < 1e6 ? `treasury tail under the 1M-sat split floor (${sats} sats) — top up ${join(STATE_DIR, 'testkit-treasury-chain.json')}` : `treasury tail ${status}`
          }
        } catch {
          /* fall through to the generic cause */
        }
        throw new Error(`testkit: cannot refill reserve — ${why}`)
      }
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
  const tw = Date.now()
  const res = await withTreasuryLock(async () => {
    t.reserve.reload()
    const leases = readLeases(leasesFile)
    const op = (u: ReserveSrc) => `${u.tx.id('hex')}:${u.vout}`
    const free = t.reserve.utxos
      .filter((u) => !t.reserve.isSpent(u.tx.id('hex'), u.vout) && u.satoshis >= 546 && leases[op(u)] === undefined)
      .sort((a, b) => b.satoshis - a.satoshis)
    // prefer ONE entry covering the whole spend; else accumulate largest-first —
    // the 16-input cap must not cap coverage at the 16 smallest entries
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
  if (dbgTimings()) dbgLog(`leaseReserve need=${need} → ${res === null ? 'null' : `${res.length} srcs`} (lock wait ${Date.now() - tw}ms)`)
  return res
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
/** per-process shared treasury — lazily opened, bootstrapped once. The
 *  singleton is bound to the cfg that opened it: a caller passing a
 *  DIFFERENT stack would silently fund itself from the wrong treasury —
 *  refuse instead of mixing. */
export function stackTreasury(cfg?: RegtestConfig): Promise<StackTreasury> {
  const c = cfg ?? stackCfgFromEnv()
  if (c === undefined) throw new Error('testkit: no regtest stack env (NEKTAR_RPC_*)')
  treasuryPromise ??= StackTreasury.open(c)
  return treasuryPromise.then((t) => {
    if (t.cfg.rpcUrl !== c.rpcUrl || t.cfg.assetUrl !== c.assetUrl || t.cfg.propagationUrl !== c.propagationUrl)
      throw new Error('testkit: stackTreasury() given a different stack config — the singleton is bound to the first cfg it opened with')
    return t
  })
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
    // the suite mints a key per world and discards it — coins parked in dead
    // worlds are unrecoverable on a zero-subsidy chain. Persist the WIF so a
    // sweep can return them to the treasury.
    appendFileSync(join(STATE_DIR, 'stackfunder-keys.jsonl'), JSON.stringify({ wif: f.keyPair.toWif() }) + '\n')
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
          await t.refill(need)
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
      if (/UTXO_SPENT|already.*spent|missing.*(input|parent)/i.test(r.description ?? '')) {
        // spent-class failure: the parents really are consumed on-chain — mark
        // them so the pool stops offering them (leases may stay; entries are dead)
        await withTreasuryLock(async () => {
          t.reserve.reload()
          for (const src of srcs) t.reserve.markSpent(src.tx.id('hex'), src.vout)
        })
        continue
      }
      // any other failure never consumed the inputs — release the leases so a
      // later fund() can still draw them, then surface the real error
      await withTreasuryLock(async () => {
        const leasesFile = join(STATE_DIR, 'leases.json')
        const leases = readLeases(leasesFile)
        const op = (u: ReserveSrc) => `${u.tx.id('hex')}:${u.vout}`
        for (const s of srcs) delete leases[op(s)]
        writeFileSync(leasesFile, JSON.stringify(leases))
      })
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
/** the chain inside the shared treasury is per-process — a test that shadows
 *  `broadcast` (wrapBroadcast) would otherwise route later tests' broadcasts
 *  through a destroyed store. Tests restore on teardown via t.after. */
const unshadowBroadcast = (chain: StackChain): void => {
  delete (chain as Partial<StackChain>).broadcast
}
export async function stackChain(t: { skip: (m?: string) => void; after?: (fn: () => void) => void }): Promise<StackChain | undefined> {
  const cfg = await skipUnlessStack(t)
  if (cfg === undefined) return undefined
  const chain = (await stackTreasury(cfg)).chain
  t.after?.(() => {
    chain.stopWatching()
    unshadowBroadcast(chain)
  })
  return chain
}

/** one-call convenience: `const { chain, funder } = await stackWorld(t, 30)` */
export async function stackWorld(
  t: { skip: (m?: string) => void; after?: (fn: () => void) => void },
  outputs = 30,
  sats = 10_000
): Promise<{ chain: StackChain; funder: StackFunder; treasury: StackTreasury } | undefined> {
  const cfg = await skipUnlessStack(t)
  if (cfg === undefined) return undefined
  const treasury = await stackTreasury(cfg)
  t.after?.(() => {
    treasury.chain.stopWatching()
    unshadowBroadcast(treasury.chain)
  })
  const funder = await StackFunder.fund(treasury, outputs, sats)
  return { chain: treasury.chain, funder, treasury }
}
