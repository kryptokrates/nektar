// Optional @bsv/wallet-toolbox wallet backend (NEKTAR_WALLET=toolbox).
//
// What this is: a BRC-100 wallet implementation — real UTXO storage (SQLite
// via StorageKnex), `createAction` funding, `internalizeAction` for foreign
// BEEF, and a pluggable `WalletServices` the wallet consults for headers,
// raw txs, merkle paths and broadcast.
//
// What it is NOT: a replacement for the automation chain. `RegtestWalletServices`
// maps the WalletServices interface onto the local Teranode stack so the
// wallet's view of the world is the same node everything else uses.
// `ToolboxFunder` only READS wallet outputs; slot leasing stays in
// `TreeLedger`, covenant/escrow construction stays in market.ts, and
// unmineable carriers never become wallet actions — the wallet only signs.
//
// Dual-build boundary: @bsv/sdk ships separate esm/cjs builds; our packages
// resolve esm, the toolbox (CJS) resolves cjs, and v4 classes carry #private
// fields — objects CANNOT cross raw. This file loads the SDK through
// createRequire (the same cjs instance the toolbox uses, `sdkc`) and converts
// across the boundary by serialization only.

import { createRequire } from 'node:module'
import { Beef, Hash, MerklePath, PrivateKey, Transaction, Utils, type ChainTracker } from '@bsv/sdk'
import { Services, Setup, StorageKnex, Wallet, WalletStorageManager, randomBytesHex, sdk } from '@bsv/wallet-toolbox'
import {
  DEFAULT_INDEXER_URL,
  TeranodeRPCTracker,
  headerPowValid,
  indexFor,
  merklePathFor,
  merklePathAtHeight,
  merklePathMainnet,
  regtestMerkleProof,
  utxoStatus,
  type BroadcastResult,
  type ChainIndex,
  type Funder,
  type FundingOut,
  type MainnetConfig,
  type RegtestConfig
} from '@nektar/chain'
import { join } from 'node:path'

const NAME = 'teranode-regtest'
// the toolbox's own SDK instance — cjs build. Every object handed INTO the
// wallet (keyDeriver, MerklePath, PrivateKey) must come from this class set.
const sdkc = createRequire(import.meta.url)('@bsv/sdk') as typeof import('@bsv/sdk')
// BRC-100 reserves 'default' for wallet-balance outputs — funding UTXOs live
// in their own basket so custody and spend-source selection stay explicit.
export const NEKTAR_BASKET = 'nektar-funding'

const werr = (e: unknown): sdk.WalletError => ({ code: 'WERR_INTERNAL', description: String(e) }) as sdk.WalletError

/** WalletServices over the local Teranode stack — regtest only. Live nets use
 *  the toolbox's default `Services` (ARC/WoC/chaintracks) instead. */
export class RegtestWalletServices implements sdk.WalletServices {
  chain: sdk.Chain = 'regtest'
  /** same seam LiveWalletServices.txLookup uses — runtime binds txStore;
   *  serves txs the node never saw (unmineable carriers, adopted ancestry) */
  txLookup?: (txid: string) => Promise<Transaction | undefined>
  constructor(
    private cfg: RegtestConfig,
    private tracker: TeranodeRPCTracker,
    // D2: wallet-relayed txs are a spend entry point — callers pass the
    // SpendGuard-wrapped broadcaster so createAction/signAction relays count
    // against the same caps; absent it falls back to sendrawtransaction
    private broadcast?: (tx: Transaction) => Promise<BroadcastResult>
  ) {}

  private async rpc(method: string, ...params: unknown[]): Promise<any> {
    const auth = `Basic ${Buffer.from(`${this.cfg.rpcUser}:${this.cfg.rpcPass}`).toString('base64')}`
    const r = await fetch(this.cfg.rpcUrl, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json', Authorization: auth },
      body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }),
      signal: AbortSignal.timeout(15_000)
    })
    const j = (await r.json()) as { result?: unknown; error?: { message?: string } }
    if (j.error !== undefined && j.error !== null) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    return j.result
  }

  private async headerOf(height: number, hash?: string): Promise<sdk.BlockHeader> {
    const h = hash ?? ((await this.rpc('getblockhash', height)) as string)
    const b = (await this.rpc('getblockheader', h, true)) as {
      version: number
      previousblockhash?: string
      merkleroot: string
      time: number
      bits: string
      nonce: number
      height: number
    }
    return {
      version: b.version,
      previousHash: b.previousblockhash ?? '0'.repeat(64),
      merkleRoot: b.merkleroot,
      time: b.time,
      bits: parseInt(b.bits, 16),
      nonce: b.nonce,
      height: b.height,
      hash: h
    }
  }

  getChainTracker = async (): Promise<ChainTracker> => this.tracker
  getHeight = async (): Promise<number> => ((await this.rpc('getblockchaininfo')) as { blocks: number }).blocks
  getHeaderForHeight = async (height: number): Promise<number[]> => {
    const h = (await this.rpc('getblockhash', height)) as string
    return Array.from(Utils.toArray((await this.rpc('getblockheader', h, false)) as string, 'hex'))
  }
  // regtest coins are valueless — the wallet only reads these for display
  getBsvExchangeRate = async (): Promise<number> => 0
  getFiatExchangeRate = async (): Promise<number> => 0

  getRawTx = async (txid: string): Promise<sdk.GetRawTxResult> => {
    // txStore first — unmineable carriers and adopted ancestry never reach
    // the node's mempool, and our own txs save an RPC round-trip
    const local = await this.txLookup?.(txid).catch(() => undefined)
    if (local !== undefined) {
      const t = sdkc.Transaction.fromHex(local.toHex())
      return { txid, name: 'txstore', rawTx: Array.from(t.toBinary()) }
    }
    try {
      // Teranode's verbose flag is an int — a bool fails with -32602
      const hex = (await this.rpc('getrawtransaction', txid, 0)) as string
      return { txid, name: NAME, rawTx: Array.from(Utils.toArray(hex, 'hex')) }
    } catch (e) {
      return { txid, name: NAME, error: werr(e) }
    }
  }

  getMerklePath = async (txid: string): Promise<sdk.GetMerklePathResult> => {
    // proof-service read first — one /merkle_proof request. Block-walking is
    // the fallback for when the asset endpoint can't answer (then the deep
    // fallback builds the path at the height verbose rawtx names).
    const served = await regtestMerkleProof(this.cfg, txid)
    let r = served !== undefined ? served : await merklePathFor(this.cfg, txid, this.tracker, 50)
    if (r === null) {
      // deep fallback: lookback-50 misses old mined txs (coinbases, early
      // treasury splits) — a coinbase ancestor without a bump leaves the
      // null-prevout input dangling and the wallet's no-txidOnly validator
      // rejects the whole internalize. Verbose rawtx names the containing
      // block; build the path at that exact height instead.
      const v = (await this.rpc('getrawtransaction', txid, 1).catch(() => undefined)) as { blockhash?: string } | undefined
      if (v?.blockhash !== undefined) {
        const bh = await this.headerOf(0, v.blockhash)
        r = await merklePathAtHeight(this.cfg, txid, this.tracker, bh.height)
      }
    }
    if (r === null) return { name: NAME, error: werr(`no merkle path for ${txid}`) }
    // cjs-built MerklePath — the wallet consumes it (dual-build boundary)
    return {
      name: NAME,
      merklePath: new sdkc.MerklePath(r.blockHeight, r.path) as unknown as sdk.GetMerklePathResult['merklePath'],
      header: await this.headerOf(r.blockHeight)
    }
  }

  postBeef: sdk.WalletServices['postBeef'] = async (beef, txids) => {
    const txidResults: sdk.PostTxResultForTxid[] = []
    for (const txid of txids) {
      try {
        const tx = beef.findAtomicTransaction(txid)
        if (tx === undefined) throw new Error('txid not in BEEF')
        if (this.broadcast !== undefined) {
          const r = await this.broadcast(Transaction.fromHex(tx.toHex()))
          if (r.status !== 'success') throw new Error(r.description ?? 'broadcast failed')
        } else await this.rpc('sendrawtransaction', tx.toHex())
        txidResults.push({ txid, status: 'success' })
      } catch (e) {
        txidResults.push({ txid, status: 'error', data: { error: String(e) } })
      }
    }
    return [{ name: NAME, status: txidResults.every((r) => r.status === 'success') ? 'success' : 'error', txidResults }]
  }

  hashOutputScript = (script: string): string => Utils.toHex(Hash.sha256(Utils.toArray(script, 'hex')))

  // Teranode's verbose getrawtransaction returns no `confirmations` — mined
  // means a real merkle path, known means the node serves the tx at all.
  getStatusForTxids = async (txids: string[]): Promise<sdk.GetStatusForTxidsResult> => {
    const results: sdk.StatusForTxidResult[] = []
    for (const txid of txids) {
      const r = (await regtestMerkleProof(this.cfg, txid)) ?? (await merklePathFor(this.cfg, txid, this.tracker, 50))
      if (r !== null) {
        results.push({ txid, depth: (await this.getHeight()) - r.blockHeight + 1, status: 'mined' })
        continue
      }
      try {
        await this.rpc('getrawtransaction', txid, 0)
        results.push({ txid, depth: 0, status: 'known' })
      } catch {
        results.push({ txid, depth: undefined, status: 'unknown' })
      }
    }
    return { name: NAME, status: 'success', results }
  }

  isUtxo = async (o: { txid?: string; vout: number }): Promise<boolean> => o.txid !== undefined && (await utxoStatus(this.cfg, o.txid, o.vout)) === 'unspent'

  getUtxoStatus = async (_output: string, _format?: sdk.GetUtxoStatusOutputFormat, outpoint?: string): Promise<sdk.GetUtxoStatusResult> => {
    const [txid, v] = (outpoint ?? '').split('.')
    if (txid === undefined || v === undefined) return { name: NAME, status: 'error', details: [] }
    const st = await utxoStatus(this.cfg, txid, Number(v))
    return { name: NAME, status: 'success', isUtxo: st === 'unspent', details: [{ txid, index: Number(v) }] }
  }

  // no script-hash index on regtest — the wallet uses this only for sweep
  // recovery, which the automation ledger owns anyway
  getScriptHashHistory = async (_hash: string): Promise<sdk.GetScriptHashHistoryResult> => ({ name: NAME, status: 'success', history: [] })
  hashToHeader = async (hash: string): Promise<sdk.BlockHeader> => await this.headerOf(0, hash)

  /** nLockTime finality against the current tip — the honest check for an
   *  unmineable carrier is `false`: lockTime 2100 + non-final sequences. */
  nLockTimeIsFinal: sdk.WalletServices['nLockTimeIsFinal'] = async (txOrLockTime) => {
    let lockTime: number
    let allSeqFinal = true
    if (typeof txOrLockTime === 'number') {
      lockTime = txOrLockTime
    } else {
      const tx =
        typeof txOrLockTime === 'string'
          ? sdkc.Transaction.fromHex(txOrLockTime)
          : Array.isArray(txOrLockTime)
            ? sdkc.Transaction.fromBinary(txOrLockTime)
            : txOrLockTime
      lockTime = (tx as { lockTime: number }).lockTime
      for (const i of (tx as { inputs: Array<{ sequence?: number }> }).inputs) if ((i.sequence ?? 0xffffffff) !== 0xffffffff) allSeqFinal = false
    }
    if (lockTime === 0) return allSeqFinal
    if (lockTime < 500_000_000) return allSeqFinal && (await this.getHeight()) >= lockTime
    return allSeqFinal && Date.now() / 1000 >= lockTime
  }

  /** Beef for a txid: raw tx + merkle path when mined — enough for the wallet
   *  to internalize or verify. */
  getBeefForTxid: sdk.WalletServices['getBeefForTxid'] = async (txid) => {
    const hex = (await this.rpc('getrawtransaction', txid, 0)) as string
    const tx = sdkc.Transaction.fromHex(hex)
    const r = (await regtestMerkleProof(this.cfg, txid)) ?? (await merklePathFor(this.cfg, txid, this.tracker, 50))
    if (r !== null) tx.merklePath = new sdkc.MerklePath(r.blockHeight, r.path)
    const beef = new sdkc.Beef()
    beef.mergeTransaction(tx)
    return beef as unknown as Awaited<ReturnType<sdk.WalletServices['getBeefForTxid']>>
  }

  getServicesCallHistory = (_reset?: boolean): sdk.ServicesCallHistory => {
    const h = { serviceName: NAME, historyByProvider: {} }
    return {
      version: 1,
      getMerklePath: h,
      getRawTx: h,
      postBeef: h,
      getUtxoStatus: h,
      getStatusForTxids: h,
      getScriptHashHistory: h,
      updateFiatExchangeRates: h
    }
  }
}

export interface ToolboxHandle {
  wallet: Wallet
  services: sdk.WalletServices
  storage: WalletStorageManager
  activeStorage: StorageKnex
  funder: ToolboxFunder
  /** drains the wallet.sqlite knex pool — without it the process can't exit */
  close: () => Promise<void>
}

/** Wallet + SQLite storage over `dataDir/wallet.sqlite`, keyed by `fundKey`.
 *  `services` injects the chain view — pass `RegtestWalletServices` on regtest,
 *  omit on live nets (toolbox defaults: ARC/WoC/chaintracks). */
export async function openToolboxWallet(args: {
  fundKey: PrivateKey
  dataDir: string
  chain: sdk.Chain
  services?: sdk.WalletServices
}): Promise<ToolboxHandle> {
  // the deriver must see a cjs PrivateKey — the wallet consumes it
  const keyDeriver = new sdkc.CachedKeyDeriver(sdkc.PrivateKey.fromWif(args.fundKey.toWif()))
  const services = args.services ?? new Services(args.chain)
  const storage = new WalletStorageManager(keyDeriver.identityKey)
  const activeStorage = new StorageKnex({
    chain: args.chain,
    knex: Setup.createSQLiteKnex(join(args.dataDir, 'wallet.sqlite')),
    commissionSatoshis: 0,
    commissionPubKeyHex: undefined,
    feeModel: { model: 'sat/kb', value: 100 }
  })
  await activeStorage.migrate('nektar_wallet', randomBytesHex(33))
  await activeStorage.makeAvailable()
  await storage.addWalletStorageProvider(activeStorage)
  if (storage.canMakeAvailable()) await storage.makeAvailable()
  const wallet = new Wallet({ chain: args.chain, keyDeriver: keyDeriver as never, storage, services })
  return { wallet, services, storage, activeStorage, funder: new ToolboxFunder(wallet), close: async () => await activeStorage.destroy() }
}

/** Funder over wallet outputs — read-only. `take` picks the smallest spendable
 *  output ≥ sats and materializes its source transaction from the wallet's
 *  own BEEF store; the returned tx is re-parsed into OUR sdk build so
 *  downstream code sees one class identity. */
// findAtomicTransaction returns the BEEF tx with its merkle path already
// attached (_Beef_addInputProof) — rebuilding through bare hex strips it and
// a pathless funding parent can never anchor a tree. Re-attach by copy, the
// same pattern buildAtomicBeef uses.
const withPath = (found: Transaction): Transaction => {
  const tx = Transaction.fromHex(found.toHex())
  if (found.merklePath !== undefined) tx.merklePath = MerklePath.fromBinary(found.merklePath.toBinary())
  return tx
}

export class ToolboxFunder implements Funder {
  constructor(private wallet: Wallet) {}
  async address(): Promise<string> {
    const pub = await this.wallet.getPublicKey({ identityKey: true }, 'nektar')
    return sdkc.PublicKey.fromString(pub.publicKey).toAddress()
  }
  async take(satoshis: number): Promise<FundingOut> {
    // page the basket — listOutputs truncates at its limit, so a single
    // 1000-row read would miss candidates past it (fails as "no spendable"
    // while funded). Per-page smallest-sufficient keeps the common case
    // to one call; the cap bounds pathological baskets.
    for (let page = 0; page < 20; page++) {
      const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, include: 'entire transactions', limit: 1000, offset: page * 1000 })
      const beef = r.BEEF !== undefined ? sdkc.Beef.fromBinary(r.BEEF) : undefined
      const candidates = r.outputs.filter((o) => o.spendable && o.satoshis >= satoshis).sort((a, b) => a.satoshis - b.satoshis)
      for (const o of candidates) {
        const [txid, vout] = o.outpoint.split('.')
        const tx = beef?.findAtomicTransaction(txid)
        if (tx !== undefined) return { tx: withPath(tx), vout: Number(vout), satoshis: o.satoshis }
      }
      if (r.outputs.length < 1000) break
    }
    throw new Error(`toolbox funder: no spendable output >= ${satoshis} sats — top up via fundToolbox()`)
  }
}

/** Top-up: spends `outpoints` (txid.vout, P2PKH, owned by `srcKey`) into the
 *  wallet via its own createAction — the wallet's canonical way to ingest
 *  foreign funding. `inputBEEF` carries the source txs for input signing.
 *  PRECONDITION: each source tx must carry a merklePath — createAction
 *  validates inputBEEF under `trustSelf:'known'`. */
export async function fundToolbox(
  tb: ToolboxHandle,
  outpoints: Array<{ txid: string; vout: number; tx: Transaction }>,
  srcKey: PrivateKey
): Promise<Array<{ outpoint: string; txid?: string; success: boolean; error?: string }>> {
  // build the beef with OUR sdk (source txs keep their merklePaths), then
  // re-parse with the cjs build — bytes are the identity-safe boundary
  const esmBeef = new Beef()
  for (const o of outpoints) esmBeef.mergeTransaction(o.tx)
  const beef = sdkc.Beef.fromBinaryStrict(esmBeef.toBinary())
  // getKeyPair takes a hex secret (or a cjs PrivateKey) — never a WIF
  const kp = Setup.getKeyPair(srcKey.toHex())
  const id = await tb.wallet.getPublicKey({ identityKey: true }, 'nektar')
  const inSats = outpoints.reduce((s, o) => s + (o.tx.outputs[o.vout].satoshis ?? 0), 0)
  // The funding output is declared with `basket` at creation — change can
  // never be reclassified after the fact (the toolbox rejects basket
  // insertion over wallet-managed change), so custody has to start here.
  // inSats - 1000 leaves headroom for the network fee; rounding dust lands
  // as ordinary 'default' change.
  const car = await tb.wallet.createAction({
    inputBEEF: beef.toBinary(),
    inputs: outpoints.map((o) => ({ outpoint: `${o.txid}.${o.vout}`, unlockingScriptLength: 108, inputDescription: 'fund wallet from P2PKH' })),
    outputs: [
      {
        satoshis: Math.max(1, inSats - 1000),
        lockingScript: new sdkc.P2PKH().lock(sdkc.PublicKey.fromString(id.publicKey).toAddress()).toHex(),
        basket: NEKTAR_BASKET,
        outputDescription: 'nektar funding output',
        tags: ['nektar']
      }
    ],
    labels: ['nektar-funding'],
    description: `nektar funding top-up (${outpoints.length} outpoints)`,
    options: { trustSelf: 'known' }
  })
  const st = car.signableTransaction
  if (st === undefined) return outpoints.map((o) => ({ outpoint: `${o.txid}.${o.vout}`, txid: car.txid, success: true }))
  // the wallet can't sign foreign P2PKH inputs — signable flow: sign each
  // input with srcKey (same getUnlockP2PKH template the Setup helper uses),
  // then hand the unlock scripts back via signAction
  const unsignedTx = sdkc.Transaction.fromAtomicBEEF(st.tx)
  for (const o of outpoints) {
    const idx = unsignedTx.inputs.findIndex((i) => String(i.sourceTXID).toLowerCase() === o.txid && i.sourceOutputIndex === o.vout)
    if (idx < 0) throw new Error(`signable tx missing input for ${o.txid}.${o.vout}`)
    // same cjs instance at runtime — the cast only bridges the dual-build
    // type split (sdkc is typed through the esm surface)
    unsignedTx.inputs[idx].unlockingScriptTemplate = Setup.getUnlockP2PKH(kp.privateKey, o.tx.outputs[o.vout].satoshis ?? 0) as never
  }
  await unsignedTx.sign()
  const spends: Record<number, { unlockingScript: string }> = {}
  for (let i = 0; i < unsignedTx.inputs.length; i++) spends[i] = { unlockingScript: unsignedTx.inputs[i].unlockingScript!.toHex() }
  const sar = await tb.wallet.signAction({ reference: st.reference, spends })
  return outpoints.map((o) => ({ outpoint: `${o.txid}.${o.vout}`, txid: sar.txid, success: sar.txid !== undefined }))
}

// ── live-net (testnet/main) ──────────────────────────────────────────────────

/** the chain-index dialect for this config — bananablocks by default;
 *  normalization + per-dialect pacing live in @nektar/chain's indexer */
const indexOf = (mc: Pick<MainnetConfig, 'indexerUrl' | 'indexerApiKey' | 'indexerKind' | 'wocUrl' | 'wocApiKey' | 'fetchImpl'>): ChainIndex =>
  indexFor({
    url: mc.indexerUrl ?? mc.wocUrl ?? DEFAULT_INDEXER_URL,
    apiKey: mc.indexerApiKey ?? mc.wocApiKey,
    kind: mc.indexerKind,
    fetchImpl: mc.fetchImpl
  })

/** WalletServices over WoC + the configured broadcaster — same endpoints the
 *  rest of the runtime trusts, so the wallet's view inherits our outage and
 *  fallback semantics instead of the toolbox's default provider mix. */
export class LiveWalletServices implements sdk.WalletServices {
  chain: sdk.Chain
  constructor(
    private opts: {
      chain: 'main' | 'test'
      mc: MainnetConfig
      tracker: { currentHeight(): Promise<number>; isValidRootForHeight(root: string, h: number): Promise<boolean> }
      broadcast: (tx: Transaction) => Promise<BroadcastResult>
    }
  ) {
    this.chain = opts.chain
  }

  private index?: ChainIndex
  private get idx(): ChainIndex {
    this.index ??= indexOf(this.opts.mc)
    return this.index
  }

  /** our own tx store — txs we broadcast or adopted seconds ago exist but
   *  the index hasn't caught up yet. Same seam WalletFunds.txLookup uses;
   *  runtime binds both to txStore. Answers through it are LOCAL truth
   *  (existence, bytes) — mined/spent status still comes from the index. */
  txLookup?: (txid: string) => Promise<Transaction | undefined>

  /** txid → cjs tx from our own store. Bytes cross the dual-build boundary;
   *  hex drops merklePath, so a proven parent re-attaches its BUMP by copy —
   *  the same pattern the `withPath` comment in this file documents. */
  private async localTx(txid: string): Promise<Transaction | undefined> {
    const local = await this.txLookup?.(txid).catch(() => undefined)
    if (local === undefined) return undefined
    const t = sdkc.Transaction.fromHex(local.toHex())
    if (local.merklePath !== undefined) t.merklePath = sdkc.MerklePath.fromBinary(local.merklePath.toBinary())
    return t
  }

  /** txid → cjs tx from the index */
  private async indexTx(txid: string): Promise<Transaction | undefined> {
    const hex = await this.idx.txHex(txid)
    return hex === undefined ? undefined : sdkc.Transaction.fromHex(hex)
  }

  /** block hash → header memo: every header we serve is recorded so a later
   *  hash-keyed lookup (merkle-proof resolution) resolves locally */
  private servedHeaders = new Map<string, sdk.BlockHeader>()

  private async headerAtHeight(height: number): Promise<sdk.BlockHeader> {
    const h = await this.idx.header(height)
    if (h === undefined) throw new Error(`index header ${height}: unavailable`)
    // a served header must prove itself — the index is a source, not an
    // authority: hash256(serialization) must equal the claimed hash and sit
    // under its own declared difficulty before the SDK sees it
    if (!headerPowValid(h)) throw new Error(`index header ${height}: fails PoW check`)
    const header = {
      version: h.version ?? 0, // headerPowValid above already rejected a missing version
      previousHash: h.prevHash,
      merkleRoot: h.merkleRoot,
      time: h.time,
      bits: h.bits,
      nonce: h.nonce,
      height: h.height ?? height,
      hash: h.hash
    }
    this.servedHeaders.set(h.hash, header)
    return header
  }

  getChainTracker = async (): Promise<ChainTracker> => this.opts.tracker as ChainTracker
  getHeight = async (): Promise<number> => await this.opts.tracker.currentHeight()
  getBsvExchangeRate = async (): Promise<number> => 0
  getFiatExchangeRate = async (): Promise<number> => 0
  getScriptHashHistory = async (_hash: string): Promise<sdk.GetScriptHashHistoryResult> => ({ name: 'woc-live', status: 'success', history: [] })
  hashToHeader = async (hash: string): Promise<sdk.BlockHeader> => {
    const served = this.servedHeaders.get(hash)
    if (served !== undefined) return served
    // the index has no hash-keyed block lookup — callers here are resolving
    // a merkle proof, which names a recent block, so a shallow tip-anchored
    // scan covers the reachable call shape without pretending the index
    // can do the lookup
    const tip = await this.getHeight().catch(() => 0)
    for (let h = tip; h > tip - 8 && h >= 0; h--) {
      const hdr = await this.idx.header(h).catch(() => undefined)
      if (hdr?.hash === hash) return await this.headerAtHeight(h)
    }
    throw new Error(`hashToHeader: ${hash} outside served-header memo + 8-block tip window — index has no hash key`)
  }
  getServicesCallHistory = (_reset?: boolean): sdk.ServicesCallHistory => {
    const h = { serviceName: 'woc-live', historyByProvider: {} }
    return {
      version: 1,
      getMerklePath: h,
      getRawTx: h,
      postBeef: h,
      getUtxoStatus: h,
      getStatusForTxids: h,
      getScriptHashHistory: h,
      updateFiatExchangeRates: h
    }
  }

  getHeaderForHeight = async (height: number): Promise<number[]> => {
    const h = await this.headerAtHeight(height)
    // compose the raw 80-byte header: version/prev/root LE-encoded hashes
    const be = (hex: string) => Buffer.from(hex, 'hex').reverse()
    const buf = Buffer.alloc(80)
    buf.writeUInt32LE(h.version, 0)
    be(h.previousHash).copy(buf, 4)
    be(h.merkleRoot).copy(buf, 36)
    buf.writeUInt32LE(h.time, 68)
    buf.writeUInt32LE(h.bits, 72)
    buf.writeUInt32LE(h.nonce, 76)
    return Array.from(buf)
  }

  getRawTx = async (txid: string): Promise<sdk.GetRawTxResult> => {
    // txStore first — a tx we broadcast or adopted moments ago exists but the
    // index answers 404 for minutes; 'index could not serve' on our own tx
    // is self-inflicted lag
    const local = await this.localTx(txid)
    if (local !== undefined) return { txid, name: 'txstore', rawTx: Array.from(local.toBinary()) }
    try {
      const hex = await this.idx.txHex(txid)
      if (hex === undefined) throw new Error('index could not serve the tx')
      return { txid, name: 'index-live', rawTx: Array.from(Utils.toArray(hex, 'hex')) }
    } catch (e) {
      return { txid, name: 'index-live', error: werr(e) }
    }
  }

  getMerklePath = async (txid: string): Promise<sdk.GetMerklePathResult> => {
    const r = await merklePathMainnet(this.opts.mc, txid)
    if (r === null) return { name: 'woc-live', error: werr(`no merkle path for ${txid}`) }
    return {
      name: 'woc-live',
      merklePath: sdkc.MerklePath.fromBinary(r.merklePath.toBinary()) as unknown as sdk.GetMerklePathResult['merklePath'],
      // the header is supplementary — a rate-limited index must not throw
      // away a path we already hold (the wallet anchors it via its tracker)
      header: await this.headerAtHeight(r.merklePath.blockHeight).catch(() => undefined)
    }
  }

  postBeef: sdk.WalletServices['postBeef'] = async (beef, txids) => {
    const txidResults: sdk.PostTxResultForTxid[] = []
    for (const txid of txids) {
      try {
        const tx = beef.findAtomicTransaction(txid)
        if (tx === undefined) throw new Error('txid not in BEEF')
        // rebuilt from hex the inputs carry no source outputs, and the guard
        // refuses a spend it cannot price — link each parent the BEEF holds
        const relay = Transaction.fromHex(tx.toHex())
        for (const inp of relay.inputs) {
          const src = inp.sourceTXID === undefined ? undefined : beef.findTxid(inp.sourceTXID)?.tx
          if (src !== undefined) inp.sourceTransaction = Transaction.fromHex(src.toHex())
        }
        const r = await this.opts.broadcast(relay)
        if (r.status !== 'success') throw new Error(r.description ?? 'broadcast failed')
        txidResults.push({ txid, status: 'success' })
      } catch (e) {
        const msg = e instanceof Error ? e.message : String(e)
        // the aggregator poisons on misclassification: a plain {status:'error'}
        // lands as 'invalidTx' and the tx is marked 'failed' permanently —
        // un-retryable even though nothing about the tx is invalid. A real
        // double-spend gets the competingTxs machinery; a genuine rejection is
        // the only case that should poison. Local SpendGuard refusals and
        // transport failures are service errors — retryable by definition.
        const doubleSpend = /double.?spend/i.test(msg)
        const invalid = !doubleSpend && /\bREJECTED\b|missing input|malformed|invalid/i.test(msg) && !/SpendGuard|HTTP|fetch|timeout|abort/i.test(msg)
        txidResults.push({
          txid,
          status: 'error',
          ...(doubleSpend ? { doubleSpend: true } : invalid ? {} : { serviceError: true }),
          data: { error: msg }
        })
      }
    }
    return [{ name: 'woc-live', status: txidResults.every((r) => r.status === 'success') ? 'success' : 'error', txidResults }]
  }

  hashOutputScript = (script: string): string => Utils.toHex(Hash.sha256(Utils.toArray(script, 'hex')))

  getStatusForTxids = async (txids: string[]): Promise<sdk.GetStatusForTxidsResult> => {
    const results: sdk.StatusForTxidResult[] = []
    for (const txid of txids) {
      try {
        const j = await this.idx.txJson(txid)
        const depth = j?.confirmations ?? 0
        let status: 'known' | 'mined' | 'unknown' = j === undefined ? 'unknown' : depth > 0 ? 'mined' : 'known'
        // index-silent is not 'does not exist' — our own broadcast/adopted
        // txs sit in txStore long before the index catches up
        if (status === 'unknown' && (await this.localTx(txid)) !== undefined) status = 'known'
        results.push({ txid, depth, status })
      } catch {
        results.push({ txid, depth: undefined, status: 'unknown' })
      }
    }
    return { name: 'woc-live', status: 'success', results }
  }

  isUtxo = async (o: { txid?: string; vout: number }): Promise<boolean> => {
    if (o.txid === undefined) return false
    // the index's per-output `spent` flag answers the utxo question — a HINT
    // for the wallet's own bookkeeping, not proof (utxoSpendStatus is the
    // positive-evidence path when the answer decides money)
    const j = await this.idx.txJson(o.txid)
    if (j === undefined) {
      // index doesn't know the tx: if WE hold it, its outputs answer like
      // any fresh unconfirmed tx would — the same thing the index reports
      // once it catches up. A still-unindexed parent can't have an indexed
      // spend either.
      const local = await this.localTx(o.txid)
      return local !== undefined && local.outputs[o.vout] !== undefined
    }
    const out = j.outputs?.[o.vout]
    return out !== undefined && out.spent !== true
  }

  getUtxoStatus = async (_output: string, _format?: sdk.GetUtxoStatusOutputFormat, outpoint?: string): Promise<sdk.GetUtxoStatusResult> => {
    const [txid, v] = (outpoint ?? '').split('.')
    if (txid === undefined || v === undefined) return { name: 'woc-live', status: 'error', details: [] }
    const isUtxo = await this.isUtxo({ txid, vout: Number(v) })
    return { name: 'woc-live', status: 'success', isUtxo, details: [{ txid, index: Number(v) }] }
  }

  nLockTimeIsFinal: sdk.WalletServices['nLockTimeIsFinal'] = async (txOrLockTime) => {
    let lockTime: number
    let allSeqFinal = true
    if (typeof txOrLockTime === 'number') {
      lockTime = txOrLockTime
    } else {
      const tx =
        typeof txOrLockTime === 'string'
          ? sdkc.Transaction.fromHex(txOrLockTime)
          : Array.isArray(txOrLockTime)
            ? sdkc.Transaction.fromBinary(txOrLockTime)
            : txOrLockTime
      lockTime = (tx as { lockTime: number }).lockTime
      for (const i of (tx as { inputs: Array<{ sequence?: number }> }).inputs) if ((i.sequence ?? 0xffffffff) !== 0xffffffff) allSeqFinal = false
    }
    if (lockTime === 0) return allSeqFinal
    if (lockTime < 500_000_000) return allSeqFinal && (await this.getHeight()) >= lockTime
    return allSeqFinal && Date.now() / 1000 >= lockTime
  }

  /** Beef for a txid: whole-ancestry, not a bare tx — the wallet's own beef
   *  consumers (sendWith relay, sync, storage validators) apply the same
   *  closure rule as internalizeAction: every unproven entry must carry its
   *  inputs back to a BUMP. A single-tx beef on an unconfirmed subject is a
   *  rejection in a bag, so serve the proven form outright. Same walk
   *  WalletFunds.buildAtomicBeef runs, on the cjs side of the boundary. */
  getBeefForTxid: sdk.WalletServices['getBeefForTxid'] = async (txid) => {
    const subject = (await this.localTx(txid)) ?? (await this.indexTx(txid))
    if (subject === undefined) throw new Error(`no raw tx for ${txid}`)
    const beef = new sdkc.Beef()
    beef.mergeTransaction(subject)
    // a mined subject anchors itself — its BUMP is the whole proof
    if (subject.merklePath === undefined) {
      const mp = await this.getMerklePath(txid)
      if (mp.merklePath !== undefined) {
        // same cjs instance at runtime — the cast only bridges the
        // dual-build type split (built via sdkc in getMerklePath)
        subject.merklePath = mp.merklePath as unknown as typeof subject.merklePath
        beef.mergeTransaction(subject)
      }
    }
    if (subject.merklePath === undefined) {
      // closure walk: every unproven carried tx resolves its inputs —
      // txStore first, index second — and every mined entry gets its BUMP.
      // Fixpoint, bounded like WalletFunds.parent() (8 hops).
      for (let hops = 0; hops < 8; hops++) {
        let progress = false
        for (const bt of [...beef.txs]) {
          if (bt.isTxidOnly || bt.hasProof || bt.tx === undefined) continue
          const mp = await this.getMerklePath(bt.txid)
          if (mp.merklePath !== undefined) {
            bt.tx.merklePath = mp.merklePath as unknown as typeof bt.tx.merklePath
            beef.mergeTransaction(bt.tx)
            progress = true
            continue
          }
          for (const dep of bt.inputTxids) {
            if (beef.findTxid(dep) == null) {
              const p = (await this.localTx(dep)) ?? (await this.indexTx(dep))
              if (p === undefined) throw new Error(`beef ancestry: unproven ${bt.txid} has unresolvable input ${dep}`)
              beef.mergeTransaction(p)
              progress = true
            }
          }
        }
        if (!progress) break
      }
    }
    return beef as unknown as Awaited<ReturnType<sdk.WalletServices['getBeefForTxid']>>
  }
}

// ── custody ─────────────────────────────────────────────────────────────────
// NEKTAR_FUND_MNEMONIC derivation lives in keystore.ts (fundKeyFromMnemonic) —
// kept out of this module so importing it never drags the toolbox in eagerly.
