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
import { TeranodeRPCTracker, merklePathFor, utxoStatus, type Funder, type FundingOut, type RegtestConfig } from '@nektar/chain'
import { join } from 'node:path'

const NAME = 'teranode-regtest'
// the toolbox's own SDK instance — cjs build. Every object handed INTO the
// wallet (keyDeriver, MerklePath, PrivateKey) must come from this class set.
const sdkc = createRequire(import.meta.url)('@bsv/sdk') as typeof import('@bsv/sdk')

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
      const hex = (await this.rpc('getrawtransaction', txid, false)) as string
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

  getStatusForTxids = async (txids: string[]): Promise<sdk.GetStatusForTxidsResult> => {
    const results: sdk.StatusForTxidResult[] = []
    for (const txid of txids) {
      try {
        const v = (await this.rpc('getrawtransaction', txid, true)) as { confirmations?: number }
        const depth = v.confirmations ?? 0
        results.push({ txid, depth, status: depth > 0 ? 'mined' : 'known' })
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
    const hex = (await this.rpc('getrawtransaction', txid, false)) as string
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
    const r = await this.wallet.listOutputs({ basket: 'default', include: 'entire transactions', limit: 1000 })
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
  return await Setup.fundWalletFromP2PKHOutpoints(tb.wallet, outpoints.map((o) => `${o.txid}.${o.vout}`), kp, beef.toBinary())
}
