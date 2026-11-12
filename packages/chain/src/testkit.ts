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
import { existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { dirname, join } from 'node:path'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut, Funder } from './interfaces.js'
import {
  TeranodeRPCTracker,
  TeranodeBroadcaster,
  regtestTx,
  merklePathFor,
  utxoStatus,
  ReserveFunder,
  bootstrapReserve,
  type RegtestConfig
} from './regtest.js'

// ---------------------------------------------------------------------------
// stack connection

export function stackCfgFromEnv(): RegtestConfig | undefined {
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
  const j = (await r.json()) as { result?: any; error?: { message?: string } }
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

/** cheap liveness probe — the gate behind skipUnlessStack */
export async function stackUp(cfg: RegtestConfig = stackCfgFromEnv() ?? ({} as RegtestConfig)): Promise<boolean> {
  if (cfg.rpcUrl === undefined) return false
  try {
    await stackRpc(cfg, 'getblockchaininfo')
    return true
  } catch {
    return false
  }
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
// chain surface — the same shape tests used on MockChain, backed by the node

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
  /** mempool + mined, with the node's merkle proof attached once sealed */
  async getTx(txid: string): Promise<Transaction | undefined> {
    return await regtestTx(this.cfg, txid)
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
  /** generatetoaddress to the treasury address; waits for the tip to catch up */
  async mine(n = 1): Promise<void> {
    const addr = this.treasuryAddr ?? new PrivateKey(1).toPublicKey().toAddress([0x6f])
    const before = await this.currentHeight()
    await stackRpc(this.cfg, 'generatetoaddress', n, addr)
    // getblockchaininfo.blocks lags generatetoaddress on Teranode — poll the
    // header index until the tip reflects the generate call
    for (let i = 0; i < 40 && (await this.currentHeight()) < before + n; i++) await new Promise((r) => setTimeout(r, 250))
  }
}

// ---------------------------------------------------------------------------
// treasury + funder — real funded keys on the real chain

const STATE_DIR = join(process.cwd(), '.state')

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
    this.key =
      process.env.NEKTAR_REGTEST_WIF !== undefined && process.env.NEKTAR_REGTEST_WIF !== ''
        ? PrivateKey.fromWif(process.env.NEKTAR_REGTEST_WIF)
        : this.loadOrMakeKey(file)
    this.chain = new StackChain(cfg, this.key)
    this.reserve = new ReserveFunder(this.key, join(STATE_DIR, 'reserve.json'))
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
    await t.ensureFunded()
    return t
  }
  /** reserve entries exist → nothing to do; else mature coinbases are
   *  generated to the treasury key and split into the reserve pool. */
  async ensureFunded(): Promise<void> {
    if (this.reserve.utxos.length > 0) return
    const addr = this.key.toPublicKey().toAddress([0x6f])
    const tip = await this.chain.currentHeight()
    // coinbases mature at +100 on regtest — generate enough to have ≥1 mature
    if (tip < 120) await this.chain.mine(120 - tip + 1)
    const r = await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), {
      treasuryFile: join(STATE_DIR, 'treasury.json')
    })
    if (this.reserve.utxos.length === 0 && r === null) {
      // no mature coinbase found for our key — generate 110 to it, then split
      await this.chain.mine(110)
      await this.reserve.scanCoinbases(this.cfg, (m, ...p) => stackRpc(this.cfg, m, ...p), 1, (await this.chain.currentHeight()) - 101)
      await bootstrapReserve(this.cfg, this.key, this.reserve, (m, ...p) => stackRpc(this.cfg, m, ...p), { treasuryFile: join(STATE_DIR, 'treasury.json') })
    }
    if (this.reserve.utxos.length === 0) throw new Error('testkit: treasury could not be funded — no mature coinbases')
  }
  async mine(n = 1): Promise<void> {
    await this.chain.mine(n)
  }
}

let treasuryPromise: Promise<StackTreasury> | undefined
/** per-process shared treasury — lazily opened, bootstrapped once */
export function stackTreasury(cfg?: RegtestConfig): Promise<StackTreasury> {
  const c = cfg ?? stackCfgFromEnv()
  if (c === undefined) throw new Error('testkit: no regtest stack env (NEKTAR_RPC_*)')
  return (treasuryPromise ??= StackTreasury.open(c))
}

/** MockFunder's surface on real money: a fresh identity key holding `outputs`
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
  static async fund(t: StackTreasury, outputs: number, sats = 10_000): Promise<StackFunder> {
    const f = new StackFunder(PrivateKey.fromRandom())
    const addr = f.keyPair.toPublicKey().toAddress([0x6f])
    // stale reserve entries (never-broadcast parents, already-spent) prune on
    // broadcast failure — allow enough attempts to walk past a rotten pool
    const need = outputs * sats + 5_000
    for (let attempt = 0; attempt < 8; attempt++) {
      // accumulate reserve entries until the spend is covered — the ~50k-sat
      // entry cap means bigger funds take several inputs
      const srcs: Array<{ tx: Transaction, vout: number, satoshis: number }> = []
      let inSats = 0
      while (inSats < need && srcs.length < 16) {
        const src = await t.reserve.takeReserve(async (txid, vout) => (await utxoStatus(t.cfg, txid, vout)) === 'spent', Math.min(50_000, need))
        if (src === null) break
        srcs.push(src); inSats += src.satoshis
      }
      if (inSats < need) throw new Error('testkit: reserve empty — bootstrap failed')
      const tx = new Transaction()
      for (const src of srcs) tx.addInput({ sourceTransaction: src.tx, sourceOutputIndex: src.vout, unlockingScriptTemplate: new P2PKH().unlock(t.key) })
      for (let i = 0; i < outputs; i++) tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
      tx.addOutput({ lockingScript: new P2PKH().lock(t.key.toPublicKey().toAddress([0x6f])), change: true })
      await tx.fee()
      await tx.sign()
      const r = await t.chain.broadcast(tx)
      if (r.status === 'success') {
        for (const src of srcs) t.reserve.markSpent(src.tx.id('hex'), src.vout)
        const changeVout = tx.outputs.length - 1
        if ((tx.outputs[changeVout]!.satoshis ?? 0) > 0) t.reserve.addOwned(tx, changeVout)
        f.fundingTx = tx
        for (let i = 0; i < outputs; i++) f.utxos.push({ tx, vout: i, satoshis: sats })
        await t.mine(1)
        return f
      }
      for (const src of srcs) t.reserve.markSpent(src.tx.id('hex'), src.vout)
      if (!/UTXO_SPENT|already.*spent|missing.*(input|parent)/i.test(r.description ?? ''))
        throw new Error(`testkit: fund broadcast failed ${r.description ?? ''}`.slice(0, 160))
    }
    throw new Error('testkit: fund broadcast raced 8 times')
  }
  async take(satoshis: number): Promise<FundingOut> {
    const i = this.utxos.findIndex((u) => u.satoshis >= satoshis)
    if (i < 0) throw new Error('StackFunder: no utxo of that size')
    const u = this.utxos.splice(i, 1)[0]!
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
