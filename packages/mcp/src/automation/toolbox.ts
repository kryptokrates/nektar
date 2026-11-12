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
import { TeranodeRPCTracker, merklePathFor, merklePathMainnet, utxoStatus, type BroadcastResult, type Funder, type FundingOut, type MainnetConfig, type RegtestConfig } from '@nektar/chain'
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
  constructor(
    private cfg: RegtestConfig,
    private tracker: TeranodeRPCTracker
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
      version: number; previousblockhash?: string; merkleroot: string; time: number; bits: string; nonce: number; height: number
    }
    return { version: b.version, previousHash: b.previousblockhash ?? '0'.repeat(64), merkleRoot: b.merkleroot, time: b.time, bits: parseInt(b.bits, 16), nonce: b.nonce, height: b.height, hash: h }
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
    try {
      // Teranode's verbose flag is an int — a bool fails with -32602
      const hex = (await this.rpc('getrawtransaction', txid, 0)) as string
      return { txid, name: NAME, rawTx: Array.from(Utils.toArray(hex, 'hex')) }
    } catch (e) {
      return { txid, name: NAME, error: werr(e) }
    }
  }

  getMerklePath = async (txid: string): Promise<sdk.GetMerklePathResult> => {
    const r = await merklePathFor(this.cfg, txid, this.tracker, 50)
    if (r === null) return { name: NAME, error: werr(`no merkle path for ${txid}`) }
    // cjs-built MerklePath — the wallet consumes it (dual-build boundary)
    return { name: NAME, merklePath: new sdkc.MerklePath(r.blockHeight, r.path) as unknown as sdk.GetMerklePathResult['merklePath'], header: await this.headerOf(r.blockHeight) }
  }

  postBeef: sdk.WalletServices['postBeef'] = async (beef, txids) => {
    const txidResults: sdk.PostTxResultForTxid[] = []
    for (const txid of txids) {
      try {
        const tx = beef.findAtomicTransaction(txid)
        if (tx === undefined) throw new Error('txid not in BEEF')
        await this.rpc('sendrawtransaction', tx.toHex())
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
      const r = await merklePathFor(this.cfg, txid, this.tracker, 50)
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
      const tx = typeof txOrLockTime === 'string' ? sdkc.Transaction.fromHex(txOrLockTime) : Array.isArray(txOrLockTime) ? sdkc.Transaction.fromBinary(txOrLockTime) : txOrLockTime
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
    const r = await merklePathFor(this.cfg, txid, this.tracker, 50)
    if (r !== null) tx.merklePath = new sdkc.MerklePath(r.blockHeight, r.path)
    const beef = new sdkc.Beef()
    beef.mergeTransaction(tx)
    return beef as unknown as Awaited<ReturnType<sdk.WalletServices['getBeefForTxid']>>
  }

  getServicesCallHistory = (_reset?: boolean): sdk.ServicesCallHistory => {
    const h = { serviceName: NAME, historyByProvider: {} }
    return { version: 1, getMerklePath: h, getRawTx: h, postBeef: h, getUtxoStatus: h, getStatusForTxids: h, getScriptHashHistory: h, updateFiatExchangeRates: h }
  }
}

export interface ToolboxHandle {
  wallet: Wallet
  services: sdk.WalletServices
  storage: WalletStorageManager
  activeStorage: StorageKnex
  funder: ToolboxFunder
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
  return { wallet, services, storage, activeStorage, funder: new ToolboxFunder(wallet) }
}

/** Funder over wallet outputs — read-only. `take` picks the smallest spendable
 *  output ≥ sats and materializes its source transaction from the wallet's
 *  own BEEF store; the returned tx is re-parsed into OUR sdk build so
 *  downstream code sees one class identity. */
export class ToolboxFunder implements Funder {
  constructor(private wallet: Wallet) {}
  async address(): Promise<string> {
    const pub = await this.wallet.getPublicKey({ identityKey: true }, 'nektar')
    return sdkc.PublicKey.fromString(pub.publicKey).toAddress()
  }
  async take(satoshis: number): Promise<FundingOut> {
    const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, include: 'entire transactions', limit: 1000 })
    const beef = r.BEEF !== undefined ? sdkc.Beef.fromBinary(r.BEEF) : undefined
    const candidates = r.outputs.filter((o) => o.spendable && o.satoshis >= satoshis).sort((a, b) => a.satoshis - b.satoshis)
    for (const o of candidates) {
      const [txid, vout] = o.outpoint.split('.')
      const tx = beef?.findAtomicTransaction(txid)
      if (tx !== undefined) return { tx: Transaction.fromHex(tx.toHex()), vout: Number(vout), satoshis: o.satoshis }
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
    outputs: [{ satoshis: Math.max(1, inSats - 1000), lockingScript: new sdkc.P2PKH().lock(sdkc.PublicKey.fromString(id.publicKey).toAddress()).toHex(), basket: NEKTAR_BASKET, outputDescription: 'nektar funding output', tags: ['nektar'] }],
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

interface WocHeader {
  hash: string
  height: number
  version: number
  previousblockhash?: string
  merkleroot: string
  time: number
  bits: string
  nonce: number
}

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

  private woc(path: string): Promise<Response> {
    const h: Record<string, string> = {}
    if (this.opts.mc.wocApiKey) h['woc-api-key'] = this.opts.mc.wocApiKey
    return (this.opts.mc.fetchImpl ?? fetch)(`${this.opts.mc.wocUrl ?? 'https://api.whatsonchain.com/v1/bsv/main'}${path}`, { headers: h, signal: AbortSignal.timeout(15_000) })
  }

  private async headerAtHeight(height: number): Promise<sdk.BlockHeader> {
    const r = await this.woc(`/block/height/${height}`)
    if (!r.ok) throw new Error(`woc header ${height}: ${r.status}`)
    const j = (await r.json()) as WocHeader | WocHeader[]
    const h = Array.isArray(j) ? j[0] : j
    if (h === undefined || h.merkleroot === undefined) throw new Error(`woc header ${height}: empty`)
    return { version: h.version, previousHash: h.previousblockhash ?? '0'.repeat(64), merkleRoot: h.merkleroot, time: h.time, bits: parseInt(h.bits, 16), nonce: h.nonce, height: h.height, hash: h.hash }
  }

  getChainTracker = async (): Promise<ChainTracker> => this.opts.tracker as ChainTracker
  getHeight = async (): Promise<number> => await this.opts.tracker.currentHeight()
  getBsvExchangeRate = async (): Promise<number> => 0
  getFiatExchangeRate = async (): Promise<number> => 0
  getScriptHashHistory = async (_hash: string): Promise<sdk.GetScriptHashHistoryResult> => ({ name: 'woc-live', status: 'success', history: [] })
  hashToHeader = async (_hash: string): Promise<sdk.BlockHeader> => {
    throw new Error('hashToHeader: not indexed by hash — use getHeaderForHeight')
  }
  getServicesCallHistory = (_reset?: boolean): sdk.ServicesCallHistory => {
    const h = { serviceName: 'woc-live', historyByProvider: {} }
    return { version: 1, getMerklePath: h, getRawTx: h, postBeef: h, getUtxoStatus: h, getStatusForTxids: h, getScriptHashHistory: h, updateFiatExchangeRates: h }
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
    try {
      const r = await this.woc(`/tx/${txid}/hex`)
      if (!r.ok) throw new Error(`${r.status}`)
      return { txid, name: 'woc-live', rawTx: Array.from(Utils.toArray((await r.text()).trim(), 'hex')) }
    } catch (e) {
      return { txid, name: 'woc-live', error: werr(e) }
    }
  }

  getMerklePath = async (txid: string): Promise<sdk.GetMerklePathResult> => {
    const r = await merklePathMainnet(this.opts.mc, txid)
    if (r === null) return { name: 'woc-live', error: werr(`no merkle path for ${txid}`) }
    return {
      name: 'woc-live',
      merklePath: sdkc.MerklePath.fromBinary(r.merklePath.toBinary()) as unknown as sdk.GetMerklePathResult['merklePath'],
      header: await this.headerAtHeight(r.merklePath.blockHeight)
    }
  }

  postBeef: sdk.WalletServices['postBeef'] = async (beef, txids) => {
    const txidResults: sdk.PostTxResultForTxid[] = []
    for (const txid of txids) {
      try {
        const tx = beef.findAtomicTransaction(txid)
        if (tx === undefined) throw new Error('txid not in BEEF')
        const r = await this.opts.broadcast(Transaction.fromHex(tx.toHex()))
        if (r.status !== 'success') throw new Error(r.description ?? 'broadcast failed')
        txidResults.push({ txid, status: 'success' })
      } catch (e) {
        txidResults.push({ txid, status: 'error', data: { error: String(e) } })
      }
    }
    return [{ name: 'woc-live', status: txidResults.every((r) => r.status === 'success') ? 'success' : 'error', txidResults }]
  }

  hashOutputScript = (script: string): string => Utils.toHex(Hash.sha256(Utils.toArray(script, 'hex')))

  getStatusForTxids = async (txids: string[]): Promise<sdk.GetStatusForTxidsResult> => {
    const results: sdk.StatusForTxidResult[] = []
    for (const txid of txids) {
      try {
        const r = await this.woc(`/tx/${txid}`)
        if (!r.ok) throw new Error(`${r.status}`)
        const j = (await r.json()) as { confirmations?: number }
        const depth = j.confirmations ?? 0
        results.push({ txid, depth, status: depth > 0 ? 'mined' : 'known' })
      } catch {
        results.push({ txid, depth: undefined, status: 'unknown' })
      }
    }
    return { name: 'woc-live', status: 'success', results }
  }

  isUtxo = async (o: { txid?: string; vout: number }): Promise<boolean> => {
    if (o.txid === undefined) return false
    const r = await this.woc(`/tx/${o.txid}/oindex/${o.vout}`)
    if (!r.ok) return false
    return ((await r.json()) as { spendable?: boolean }).spendable === true
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
      const tx = typeof txOrLockTime === 'string' ? sdkc.Transaction.fromHex(txOrLockTime) : Array.isArray(txOrLockTime) ? sdkc.Transaction.fromBinary(txOrLockTime) : txOrLockTime
      lockTime = (tx as { lockTime: number }).lockTime
      for (const i of (tx as { inputs: Array<{ sequence?: number }> }).inputs) if ((i.sequence ?? 0xffffffff) !== 0xffffffff) allSeqFinal = false
    }
    if (lockTime === 0) return allSeqFinal
    if (lockTime < 500_000_000) return allSeqFinal && (await this.getHeight()) >= lockTime
    return allSeqFinal && Date.now() / 1000 >= lockTime
  }

  getBeefForTxid: sdk.WalletServices['getBeefForTxid'] = async (txid) => {
    const r = await this.getRawTx(txid)
    if (r.rawTx === undefined) throw new Error(`no raw tx for ${txid}`)
    const tx = sdkc.Transaction.fromBinary(r.rawTx)
    const mp = await merklePathMainnet(this.opts.mc, txid)
    if (mp !== null) tx.merklePath = sdkc.MerklePath.fromBinary(mp.merklePath.toBinary())
    const beef = new sdkc.Beef()
    beef.mergeTransaction(tx)
    return beef as unknown as Awaited<ReturnType<sdk.WalletServices['getBeefForTxid']>>
  }
}

// ── reserve-funder shape over wallet storage ────────────────────────────────

/** Drop-in for the `funder` object openLiveRuntime builds over reserve.json.
 *  Same semantics: `take`/`takeReserve` is a NON-destructive peek (smallest
 *  sufficient, lease-filtered, optional spendCheck); `markSpent` lands after
 *  broadcast and relinquishes the output from the wallet's default basket;
 *  `addOwned` internalizes a freshly-created own output. `utxos` is a sync
 *  cache refreshed on every `take` — the async wallet list is the truth. */
export class ToolboxReserveFunder {
  leased?: (txid: string, vout: number) => Promise<boolean>
  private cache: Array<{ tx: Transaction; vout: number; satoshis: number }> = []
  constructor(
    private wallet: Wallet,
    private services?: LiveWalletServices,
    private log: (s: string) => void = () => {}
  ) {}

  get utxos(): Array<{ tx: Transaction; vout: number; satoshis: number }> {
    return this.cache
  }

  async refresh(): Promise<void> {
    const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, include: 'entire transactions', limit: 1000 })
    const beef = r.BEEF !== undefined ? sdkc.Beef.fromBinary(r.BEEF) : undefined
    this.cache = r.outputs
      .filter((o) => o.spendable)
      .flatMap((o) => {
        const [txid, vout] = o.outpoint.split('.')
        const tx = beef?.findAtomicTransaction(txid)
        return tx === undefined ? [] : [{ tx: Transaction.fromHex(tx.toHex()), vout: Number(vout), satoshis: o.satoshis }]
      })
  }

  async takeReserve(
    spendCheck?: (txid: string, vout: number) => Promise<boolean>,
    minSats?: number
  ): Promise<{ tx: Transaction; vout: number; satoshis: number } | null> {
    const floor = minSats ?? 546
    await this.refresh()
    for (const u of this.cache.filter((x) => x.satoshis >= floor).sort((a, b) => a.satoshis - b.satoshis)) {
      const txid = u.tx.id('hex')
      if (this.leased !== undefined && (await this.leased(txid, u.vout))) continue
      if (spendCheck !== undefined && (await spendCheck(txid, u.vout))) {
        this.markSpent(txid, u.vout)
        continue
      }
      return u
    }
    return null
  }

  markSpent(txid: string, vout: number): void {
    this.cache = this.cache.filter((u) => !(u.tx.id('hex') === txid && u.vout === vout))
    // basket removal is the honest equivalent — a spent wallet outpoint must
    // never be offered again. Fire-and-forget: the automation ledger's
    // outpoint lease covers the async gap.
    void this.wallet.relinquishOutput({ basket: NEKTAR_BASKET, output: `${txid}.${vout}` }).catch(() => {})
  }

  /** Whether the wallet already custodies this outpoint — lets boot seeding
   *  skip re-internalizing outputs that survived a restart. */
  async has(txid: string, vout: number): Promise<boolean> {
    const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, limit: 1000 })
    return r.outputs.some((o) => o.outpoint === `${txid}.${vout}`)
  }

  /** Returns the internalize promise so boot-seeding can serialize (WoC rate
   *  limits) and callers that don't care can ignore it fire-and-forget. */
  addOwned(tx: Transaction, vout: number): Promise<void> {
    if (this.cache.some((u) => u.tx.id('hex') === tx.id('hex') && u.vout === vout)) return Promise.resolve()
    this.cache.push({ tx, vout, satoshis: tx.outputs[vout].satoshis ?? 0 })
    return this.internalize(tx, vout).catch((e) => this.log(`toolbox internalize ${tx.id('hex')}.${vout}: ${e instanceof Error ? e.message : String(e)}`))
  }

  private async internalize(tx: Transaction, vout: number): Promise<void> {
    // BRC-100 wants a self-contained AtomicBEEF: txid-only deps are rejected,
    // so input parents ride along — carrying their own merkle proofs when
    // mined (unmined parents recurse one level back to a proven ancestor or
    // fail honestly; funding UTXOs in practice spend confirmed treasury).
    const beef = new Beef()
    beef.mergeTransaction(tx)
    for (const i of tx.inputs) {
      if (i.sourceTXID === undefined) continue
      const parent = i.sourceTransaction ?? (await this.parent(i.sourceTXID))
      if (parent === undefined) throw new Error(`internalize: no source tx for input ${i.sourceTXID}`)
      beef.mergeTransaction(parent)
    }
    try {
      await this.wallet.internalizeAction({
        tx: beef.toBinaryAtomic(tx.id('hex')),
        outputs: [{ outputIndex: vout, protocol: 'basket insertion', insertionRemittance: { basket: NEKTAR_BASKET } }],
        description: 'nektar funding output'
      })
    } catch (e) {
      // WERR_REVIEW_ACTIONS: the wallet re-broadcasts internalized txs — for
      // outputs already on chain the sendWith result isn't 'unproven' and it
      // demands review. The custody write happens either way; trust only the
      // end state: if the output landed, the internalize succeeded.
      if ((e as { code?: string }).code !== 'WERR_REVIEW_ACTIONS' || !(await this.has(tx.id('hex'), vout))) throw e
    }
  }

  /** Fetch a parent tx with its merkle path attached when mined — what the
   *  AtomicBEEF validator needs to accept the dependency. */
  private async parent(txid: string): Promise<Transaction | undefined> {
    if (this.services === undefined) return undefined
    const r = await this.services.getRawTx(txid)
    if (r.rawTx === undefined) return undefined
    const tx = Transaction.fromBinary(r.rawTx)
    const mp = await this.services.getMerklePath(txid)
    if (mp.merklePath !== undefined) tx.merklePath = MerklePath.fromBinary((mp.merklePath as unknown as MerklePath).toBinary())
    return tx
  }
}

// ── custody ─────────────────────────────────────────────────────────────────
// NEKTAR_FUND_MNEMONIC derivation lives in keystore.ts (fundKeyFromMnemonic) —
// kept out of this module so importing it never drags the toolbox in eagerly.
