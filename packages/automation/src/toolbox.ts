// Optional @bsv/wallet-toolbox wallet backend (NEKTAR_WALLET=toolbox).
//
// What this is: a BRC-100 wallet implementation — real UTXO storage (SQLite
// via StorageKnex), `createAction` funding, `internalizeAction` for foreign
// BEEF, and a pluggable `WalletServices` the wallet consults for headers,
// raw txs, merkle paths and broadcast.
//
// What it is NOT: a replacement for the automation chain. The WalletServices
// implementations (`RegtestWalletServices`/`LiveWalletServices`) live in
// toolbox-services.ts — they map the wallet's chain questions onto the same
// node/index/broadcaster everything else uses.
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
import { Beef, MerklePath, PrivateKey, Transaction } from '@bsv/sdk'
import { Services, Setup, StorageKnex, Wallet, WalletStorageManager, randomBytesHex, sdk } from '@bsv/wallet-toolbox'
import { FundsStarvedError, type Funder, type FundingOut } from '@nektar/chain'
import { join } from 'node:path'

// the toolbox's own SDK instance — cjs build. Every object handed INTO the
// wallet (keyDeriver, MerklePath, PrivateKey) must come from this class set.
const sdkc = createRequire(import.meta.url)('@bsv/sdk') as typeof import('@bsv/sdk')
// BRC-100 reserves 'default' for wallet-balance outputs — funding UTXOs live
// in their own basket so custody and spend-source selection stay explicit.
export const NEKTAR_BASKET = 'nektar-funding'

export interface ToolboxHandle {
  wallet: Wallet
  services: sdk.WalletServices
  storage: WalletStorageManager
  activeStorage: StorageKnex
  funder: ToolboxFunder
  /** drains the wallet.sqlite knex pool — without it the process can't exit */
  close: () => Promise<void>
}

export interface ToolboxStoreHandle {
  activeStorage: StorageKnex
  /** drains the wallet.sqlite knex pool — without it the process can't exit */
  close: () => Promise<void>
}

/** Storage-only open of `dataDir/wallet.sqlite` — no Wallet, no services.
 *  The basket sweep enumerates storage-level rows because the wallet-layer
 *  `listOutputs` hides outputs whose parent tx is `unsigned`/`failed`/
 *  `unprocessed` (toolbox utxo-lifecycle doc) — exactly the state a stalled
 *  delayed broadcast leaves, and exactly the rows a migration must not miss. */
export async function openToolboxStore(args: { dataDir: string; chain: sdk.Chain }): Promise<ToolboxStoreHandle> {
  const activeStorage = new StorageKnex({
    chain: args.chain,
    knex: Setup.createSQLiteKnex(join(args.dataDir, 'wallet.sqlite')),
    commissionSatoshis: 0,
    commissionPubKeyHex: undefined,
    feeModel: { model: 'sat/kb', value: 100 }
  })
  await activeStorage.migrate('nektar_wallet', randomBytesHex(33))
  await activeStorage.makeAvailable()
  return { activeStorage, close: async () => await activeStorage.destroy() }
}

export interface BasketRow {
  /** the output's parent txid — the output row's `txid` when set, else the
   *  transaction row's */
  txid: string | undefined
  vout: number
  satoshis: number
  /** parent transaction status (toolbox utxo-lifecycle doc): `unprocessed`/
   *  `nosend`/`sending`/`unfail` mean the parent may never have aired;
   *  `unsigned`/`failed` rows are dead. Parent BYTES are not here — the
   *  transactions.rawTx column is unused; resolve bytes via the storage's
   *  `getProvenOrRawTx(txid)` (proven_txs / proven_tx_reqs), like the
   *  toolbox's own BEEF assembly does. */
  parentStatus: string
}

/** Storage-level basket enumeration — every spendable row in `basket`,
 *  regardless of parent-transaction status. The wallet-layer `listOutputs`
 *  hides outputs under `unsigned`/`failed`/`unprocessed` parents, so a
 *  migration/recovery path must read the tables. */
export async function basketRows(store: Pick<StorageKnex, 'knex'>, basket: string): Promise<BasketRow[]> {
  const rows = (await store
    .knex('outputs as o')
    .join('transactions as t', 'o.transactionId', 't.transactionId')
    .join('output_baskets as b', 'o.basketId', 'b.basketId')
    .where('b.name', basket)
    .where('o.spendable', true)
    .select('o.txid', 'o.vout', 'o.satoshis', 't.status', 't.txid as parentTxid')) as Array<{
    txid: string | null
    vout: number
    satoshis: number
    status: string
    parentTxid: string | null
  }>
  return rows.map((r) => ({
    txid: r.txid ?? r.parentTxid ?? undefined,
    vout: r.vout,
    satoshis: r.satoshis,
    parentStatus: r.status
  }))
}

/** Wallet + SQLite storage over `dataDir/wallet.sqlite`, keyed by `fundKey`.
 *  `services` injects the chain view — pass `RegtestWalletServices` on regtest,
 *  omit on live nets (toolbox defaults: ARC/WoC/chaintracks).
 *  NO MONITOR RUNS: nothing here starts the toolbox Monitor, so —
 *  - every send must pass `acceptDelayedBroadcast: false` (the documented
 *    default would queue it for a Monitor that never broadcasts),
 *  - wallet-side `unproven`→`completed` settlement never runs — our tracker
 *    is the truth source; `unproven` parents still list, so reads are fine,
 *  - `signableTransaction` references live in wallet memory only — create
 *    and sign in the same call (`fundToolbox` already does). */
export async function openToolboxWallet(args: {
  fundKey: PrivateKey
  dataDir: string
  chain: sdk.Chain
  services?: sdk.WalletServices
  /** BRC-100 conformance surface: serve privileged key usages from a real
   *  manager (docs: `PrivilegedKeyManager(async () => rootKey)`). Undefined
   *  keeps the embedded wallet's default derive-from-root behaviour. */
  privilegedKeyManager?: sdk.PrivilegedKeyManager
  /** Certificate-lookup seam: the wallet builds a SLAP-tracker resolver by
   *  default; callers that want deterministic discovery inject their own. */
  lookupResolver?: { query: (question: unknown) => Promise<unknown> }
}): Promise<ToolboxHandle> {
  // the deriver must see a cjs PrivateKey — the wallet consumes it
  const keyDeriver = new sdkc.CachedKeyDeriver(sdkc.PrivateKey.fromWif(args.fundKey.toWif()))
  const services = args.services ?? new Services(args.chain)
  const { activeStorage, close } = await openToolboxStore({ dataDir: args.dataDir, chain: args.chain })
  const storage = new WalletStorageManager(keyDeriver.identityKey)
  await storage.addWalletStorageProvider(activeStorage)
  if (storage.canMakeAvailable()) await storage.makeAvailable()
  const wallet = new Wallet({
    chain: args.chain,
    keyDeriver: keyDeriver as never,
    storage,
    services,
    privilegedKeyManager: args.privilegedKeyManager,
    lookupResolver: args.lookupResolver as never
  })
  return { wallet, services, storage, activeStorage, funder: new ToolboxFunder(wallet), close }
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
    throw new FundsStarvedError(`toolbox funder: no spendable output >= ${satoshis} sats — top up via fundToolbox()`)
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
    // no Monitor runs in this process — the documented default (delayed)
    // would queue the tx `unprocessed`/`unsent` forever, and its basket
    // output stays hidden from listOutputs under an unprocessed parent.
    // Broadcast synchronously through our own services.postBeef instead.
    options: { trustSelf: 'known', acceptDelayedBroadcast: false }
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

// ── custody ─────────────────────────────────────────────────────────────────
// NEKTAR_FUND_MNEMONIC derivation lives in keystore.ts (fundKeyFromMnemonic) —
// kept out of this module so importing it never drags the toolbox in eagerly.
