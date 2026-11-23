// funding.ts — how the runtime gets coins. Two separable questions live here:
//   custody — which wallet owns the coin book (toolbox embedded, or an
//             external BRC-100 substrate), who signs, where the key lock sits;
//   funding — how the reserve fills: regtest treasury bootstrap, and the live
//             UX (p2p eyes, fee vault, txid imports, index discovery, the
//             funding wait, and the /v1/wallet + /v1/costs ops surface).
// Chain questions are already answered by the ChainPort; this file only sees
// port.broadcast/tracker/mine/guard.
import { join } from 'node:path'
import { existsSync, renameSync } from 'node:fs'
import { PrivateKey, Transaction, WalletClient, ProtoWallet, PublicKey as SdkPublicKey, MerklePath } from '@bsv/sdk'
import { walletSigner, FUND_SIGN_ARGS, nektarEnv, nektarEnvFlag, nektarEnvNum, mkLog, writeErr, type LogLine, type SignerWallet } from '@nektar/core'
import {
  bootstrapReserve,
  regtestRpc,
  importFundingTx,
  discoverUtxos,
  ALREADY,
  broadcastProvablyDead,
  type ChainPort,
  type BroadcastResult,
  type RegtestConfig,
  type MainnetConfig,
  type TxStore,
  type Funds
} from '@nektar/chain'
import { wireP2p, setupFeeVault } from './setup.js'
import { acquireLock, keyLockFile, loadFeeKey, probeWalletSubstrate } from './rtsupport.js'
import { importable } from './txops.js'

const log = (net: string): LogLine => mkLog(`nektar ${net}`)

/** the coin book a custody path hands the runtime — `Funds` plus the fields
 *  the concrete impls happen to expose (a real wallet path always has all
 *  of them; an injected test-tier Funds may carry none) */
interface CoinBook extends Funds {
  utxos?: Array<{ tx: Transaction; vout: number; satoshis?: number }>
  dealFloor?: number
}

export interface CustodyResult {
  /** the one coin book (SqlFunds on every real wallet path) */
  funds: CoinBook
  /** injected signing surface for lanes/bstack protocol calls — ProtoWallet
   *  under held-key custody, WalletClient under NEKTAR_WALLET=client. Both
   *  expose the same getPublicKey/createSignature/createHmac surface. */
  wallet?: import('@bsv/sdk').WalletInterface | import('./types.js').OperatorWallet
  /** absent under client custody — the external wallet signs instead */
  fundKey?: PrivateKey
  /** external BRC-100 substrate when NEKTAR_WALLET=client */
  clientWallet?: import('@bsv/sdk').WalletInterface
  fundAddress: string
  pkhHex: string
  releaseKeyLock?: () => Promise<void>
  closeWallet?: () => Promise<void>
}

/** which wallet owns the coins: toolbox everywhere except NEKTAR_WALLET=client
 *  on live nets (regtest is toolbox-only — no client path exists there). */
export async function openCustody(
  cfg: { network: string; dataDir: string; regtest?: RegtestConfig & { fundWif: string }; main?: MainnetConfig & { fundWif?: string } },
  port: ChainPort,
  pass: string | undefined,
  persistence: { knex: import('knex').Knex; txStore: TxStore }
): Promise<CustodyResult> {
  const tb = await import('@nektar/automation')
  const net = port.network
  const walletMode = nektarEnv('NEKTAR_WALLET') ?? 'toolbox'
  // upkeep/publish takes may not dip the book below what keeps one deal
  // affordable — the impl's take() enforces it on every pick
  const dealFloor = nektarEnvNum('NEKTAR_DEAL_FLOOR_SATS') ?? 0
  const { knex, txStore } = persistence
  if (net === 'regtest') {
    if (walletMode !== 'toolbox')
      throw new Error(`regtest custody is NEKTAR_WALLET=toolbox — got '${walletMode}' (legacy was removed; client has no regtest path)`)
    const rc = cfg.regtest!
    const fundKey = PrivateKey.fromWif(rc.fundWif)
    const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
    const fundAddress = fundKey.toPublicKey().toAddress([0x6f])
    // one process per fund key — the lock lives in tmpdir keyed on the pkh,
    // so it binds regtest treasuries exactly like live reserves: two booted
    // instances on one WIF is the double-spend class this exists to refuse
    let releaseKeyLock: (() => Promise<void>) | undefined
    if (nektarEnvFlag('NEKTAR_ALLOW_SHARED_KEY')) {
      log(net)('NEKTAR_ALLOW_SHARED_KEY=1 — fund-key lock skipped; two processes may spend this reserve')
    } else {
      releaseKeyLock = await acquireLock(keyLockFile(pkhHex), `fund key ${fundAddress}`)
    }
    const funds = await tb.SqlFunds.open(knex, txStore, { log: log(net), dealFloor, pkhHex, address: fundAddress })
    await sweepBasket({ tb, dataDir: cfg.dataDir, chain: 'regtest', port, funds, log: log(net) })
    return {
      funds,
      wallet: new ProtoWallet(fundKey),
      fundKey,
      fundAddress,
      pkhHex,
      releaseKeyLock
    }
  }
  const mc = cfg.main!
  const testNet = net !== 'main'
  const externalWallet = walletMode === 'client'
  if (walletMode !== 'toolbox' && walletMode !== 'client') throw new Error(`NEKTAR_WALLET must be toolbox|client — got '${walletMode}' (legacy was removed)`)
  let clientWallet: import('@bsv/sdk').WalletInterface | undefined
  let fundAddress: string
  let pkhHex: string
  let fundKey: PrivateKey | undefined
  if (externalWallet) {
    if (mc.fundWif !== undefined) log(net)('NEKTAR_FUND_WIF ignored under NEKTAR_WALLET=client — the external wallet signs')
    // the SDK's auto probe collapses every substrate failure into 'Install a
    // BSV wallet today!' — a LOCKED vault gets that same message. Name the
    // real state first; these are the two operator-visible cases.
    const sub = await probeWalletSubstrate()
    if (sub === 'locked') throw new Error('NEKTAR_WALLET=client: BSV Desktop is running but the vault is LOCKED — unlock it and restart')
    if (sub === 'absent') throw new Error('NEKTAR_WALLET=client: no BRC-100 wallet substrate on localhost:3321 — start BSV Desktop unlocked')
    clientWallet = new WalletClient('auto', 'nektar')
    // the reserve funds a stable self-derived wallet keyID (nektar-fund) —
    // createSignature under the same derivation produces the matching sig, so
    // P2PKH spends can be wallet-signed without the app holding key material.
    // (identityKey cannot root-sign on sdk-4 — the derived pair is the only
    // spendable surface external custody exposes.)
    const fund = walletSigner(clientWallet, FUND_SIGN_ARGS)
    const pk = SdkPublicKey.fromString(await fund.pubKeyHex())
    fundAddress = pk.toAddress(testNet ? [0x6f] : undefined)
    pkhHex = Buffer.from(pk.toHash()).toString('hex')
    log(net)(`NEKTAR_WALLET=client — signing via external wallet substrate (derived funding address ${fundAddress})`)
  } else {
    fundKey = loadFeeKey(cfg.dataDir, mc.fundWif, pass, net === 'main')
    fundAddress = fundKey.toPublicKey().toAddress(testNet ? [0x6f] : undefined)
    pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
  }
  // one process per fund key — a second instance spending the same reserve
  // is how the earlier double spends happened. Harnesses that deliberately
  // co-run set NEKTAR_ALLOW_SHARED_KEY=1 (warned on stderr).
  let releaseKeyLock: (() => Promise<void>) | undefined
  if (nektarEnvFlag('NEKTAR_ALLOW_SHARED_KEY')) {
    log(net)('NEKTAR_ALLOW_SHARED_KEY=1 — fund-key lock skipped; two processes may spend this reserve')
  } else {
    releaseKeyLock = await acquireLock(keyLockFile(pkhHex), `fund key ${fundAddress}`)
  }
  log(net)(
    `operator fee key ${fundAddress} — fund this address to enable anchoring (then set NEKTAR_FUND_TXIDS=<txid|hex:rawtx>, or NEKTAR_WAIT_FUNDING=1 to QR + hold for funds)`
  )
  // the one coin book for both custody modes — signing differs (held key via
  // ProtoWallet, external via WalletClient), coin state does not
  const funds = await tb.SqlFunds.open(knex, txStore, { log: log(net), dealFloor, pkhHex, address: fundAddress })
  const wallet = externalWallet ? clientWallet! : new ProtoWallet(fundKey)
  if (!externalWallet && fundKey !== undefined) {
    await sweepBasket({ tb, dataDir: cfg.dataDir, chain: testNet ? 'test' : 'main', port, funds, log: log(net) })
  }
  log(net)(`coin book sqlite utxos (${funds.utxos.length} funding utxo(s), custody ${externalWallet ? 'client' : 'held key'})`)
  return { funds, wallet, fundKey, clientWallet, fundAddress, pkhHex, releaseKeyLock }
}

/** one-time migration: an existing wallet.sqlite basket is swept into the
 *  `utxos` table, then renamed aside so the sweep never re-runs. Enumerates
 *  STORAGE-level rows (openToolboxStore + basketRows — no Wallet, no
 *  services): the wallet-layer `listOutputs` hides outputs under
 *  `unsigned`/`failed`/`unprocessed` parents (toolbox utxo-lifecycle doc),
 *  which is exactly where a stalled delayed broadcast leaves them. Parents
 *  that never aired get a broadcast recovery before adoption; `unsigned`/
 *  `failed` parents and dead broadcasts are skipped without blocking the
 *  rename; any other blocked row keeps wallet.sqlite for a next-boot retry.
 *  `adopt` respects tombstones, so a re-run is idempotent.
 *  NEKTAR_SKIP_BASKET_SWEEP=1 opts out. Exported for the migration test. */
export async function sweepBasket(deps: {
  tb: typeof import('@nektar/automation')
  dataDir: string
  chain: 'regtest' | 'test' | 'main'
  port: ChainPort
  funds: { adopt: (tx: Transaction, vout: number) => Promise<boolean>; utxos?: unknown[] }
  log: (s: string) => void
}): Promise<void> {
  const { tb, dataDir, chain, port, funds, log: l } = deps
  const file = join(dataDir, 'wallet.sqlite')
  if (nektarEnvFlag('NEKTAR_SKIP_BASKET_SWEEP') || !existsSync(file)) return
  try {
    const store = await tb.openToolboxStore({ dataDir, chain })
    let swept = 0
    let blocked = 0
    try {
      for (const r of await tb.basketRows(store.activeStorage, tb.NEKTAR_BASKET)) {
        // dead parents: never aired or abandoned — the output was never real
        // money; skip without holding the rename
        if (r.parentStatus === 'unsigned' || r.parentStatus === 'failed') continue
        if (r.txid === undefined) {
          blocked++
          continue
        }
        // bytes come from the storage's own resolver first — proven_txs
        // (mined) or proven_tx_reqs (the send queue) — first-party evidence,
        // zero network calls; the port lookups are the fallback for rows
        // whose bytes never landed in wallet storage.
        const { proven, rawTx } = await store.activeStorage.getProvenOrRawTx(r.txid)
        const bytes = rawTx ?? proven?.rawTx
        let tx = bytes !== undefined ? Transaction.fromBinary(bytes) : undefined
        tx ??= (await port.getTx?.(r.txid)) ?? (await port.txLookup?.(r.txid))
        if (tx === undefined) {
          l(`basket sweep: ${r.txid.slice(0, 12)}…:${r.vout} — no tx bytes anywhere; retry next boot`)
          blocked++
          continue
        }
        // `completed`/`unproven` (and any proven record) aired by definition —
        // adopt as-is. The genuinely-stalled send states get one broadcast
        // recovery: the delayed-broadcast casualty this migration exists for.
        const stalled = r.parentStatus !== 'completed' && r.parentStatus !== 'unproven' && proven === undefined
        if (stalled) {
          const br = await port.broadcast(tx)
          const desc = br.description ?? ''
          const ok = br.status === 'success' || ALREADY.test(desc)
          const dead = broadcastProvablyDead(desc)
          if (!ok && !dead) {
            l(`basket sweep: ${r.txid.slice(0, 12)}…:${r.vout} parent broadcast failed (${br.description ?? 'rejected'}) — retry next boot`)
            blocked++
          }
          if (!ok) continue
        }
        if (await funds.adopt(tx, r.vout)) swept++
      }
    } finally {
      await store.close()
    }
    if (swept > 0) l(`basket sweep: ${swept} outpoint(s) migrated wallet.sqlite → utxos`)
    if (blocked > 0) {
      l(`basket sweep: ${blocked} outpoint(s) blocked — wallet.sqlite kept for next-boot retry`)
    } else {
      try {
        renameSync(file, `${file}.migrated`)
      } catch {
        /* rename failed — the sweep is idempotent, next boot re-runs it */
      }
    }
  } catch (e) {
    l(`basket sweep skipped: ${(e as Error).message}`)
  }
}

/** regtest treasury bootstrap — the funder's own chain in the data dir; split
 *  mined before use. Bootstrap writes straight into the wallet via the seed's
 *  addOwned hook (internalize). Only runs while the wallet holds <5 utxos. */
export async function regtestSeed(deps: { rc: RegtestConfig & { fundWif: string }; port: ChainPort; funds: CoinBook; dataDir: string }): Promise<void> {
  const { rc, port, funds, dataDir } = deps
  if ((funds.utxos?.length ?? 0) >= 5) return
  const fundKey = PrivateKey.fromWif(rc.fundWif)
  const seed = { addOwned: (tx: Transaction, vout: number) => void funds.adopt(tx, vout) }
  const s = await bootstrapReserve(rc, fundKey, seed, regtestRpc(rc), { treasuryFile: join(dataDir, 'treasury.json') })
  if (!s && (funds.utxos?.length ?? 0) === 0) throw new Error('regtest: could not fund the reserve (no unspent treasury or early coinbase for the fund key)')
  if (s) await port.mine?.()
  await funds.refresh?.()
}

/** fetch + verify the funding tx's proof at the adoption seam — the one
 *  place imported/discovered funding meets the port's witnessed headers.
 *  The discovery paths (importFundingTx, discoverUtxos, the p2p eyes) stay
 *  bytes-only by design; the proof lands here, in TxStore, where
 *  CustodyOps.takeFunding's held-proof lookup already reads it — so a tree
 *  minted on imported stock anchors without waiting a block for its own
 *  proof. A txid the port can't prove yet is handed to the events floor:
 *  the verified observe attaches it when it lands. Never throws — a
 *  couldn't-ask lookup must not block the adoption of a valid coin. */
export function fundingProofAttacher(d: {
  txStore: Pick<TxStore, 'get' | 'setProof'>
  /** ChainPort.proofLookup — already verified against the port's witnessed
   *  headers; absent on tiers with no proof source (watch-only then) */
  proofLookup?: (txid: string) => Promise<MerklePath | undefined>
  watchTx: (txid: string) => void
  log: (line: string) => void
}): (tx: Transaction) => Promise<void> {
  const inflight = new Map<string, Promise<void>>()
  const watched = new Set<string>()
  return (tx) => {
    const txid = tx.id('hex')
    // held evidence is never re-fetched — carried on the tx or already
    // stored from an earlier seam
    if (tx.merklePath !== undefined || d.txStore.get(txid)?.merklePath !== undefined) return Promise.resolve()
    const pending = inflight.get(txid)
    if (pending !== undefined) return pending
    if (watched.has(txid)) return Promise.resolve()
    const p = (async () => {
      try {
        const mp = await d.proofLookup?.(txid)
        if (mp !== undefined) {
          d.txStore.setProof(txid, mp)
          d.log(`funding ${txid.slice(0, 12)}… proof attached at height ${mp.blockHeight}`)
          return
        }
      } catch {
        /* a lookup failure is "couldn't ask" — the events floor answers when the proof lands */
      }
      watched.add(txid)
      d.watchTx(txid)
    })().finally(() => {
      inflight.delete(txid)
    })
    inflight.set(txid, p)
    return p
  }
}

export interface LiveFundingDeps {
  cfg: { dataDir: string }
  mc: MainnetConfig & { maxFeePerTx?: number; maxSpendPerDay?: number; minBroadcastIntervalMs?: number }
  net: 'main' | 'testnet'
  custody: CustodyResult
  txStore: TxStore
  ledger: import('@nektar/automation').AutomationStore
  port: ChainPort
  /** the fully-wrapped broadcast (ledger + log + watch) */
  broadcast: (tx: Transaction) => Promise<BroadcastResult>
  /** the events floor — a funding tx the port can't prove yet is watched so
   *  its proof attaches when it lands (fundingProofAttacher) */
  watchTx: (txid: string) => void
}

export interface LiveFunding {
  ops: {
    wallet: () => Promise<{
      address: string
      balanceSats: number
      utxos: number
      spentTodaySats: number
      caps: { maxFeePerTx: number; maxPerDay: number; minIntervalMs: number }
      avgFeePerAnchorSats: number
      runwayAnchors: number | null
    }>
    costs: () => Array<{ txid: string; bytes: number; fee: number; spend: number; at: string }>
    importFunding: (txid: string) => Promise<unknown>
    refreshFunding: () => Promise<unknown>
  }
  /** re-read the wallet view + adopt index discoveries — fired on the
   *  abandon verdict (a dead funding mint may free its inputs) */
  refresh: () => Promise<void>
  /** per-tick discovery pass + the empty-reserve warning */
  topUp: () => Promise<void>
}

/** live-net funding UX: p2p eyes, the optional fee-vault mint, FUND_TXIDS and
 *  index discovery, the QR funding wait, and the wallet/costs ops surface. */
export async function liveFunding(deps: LiveFundingDeps): Promise<LiveFunding> {
  const { cfg, mc, net, custody, txStore, ledger, port } = deps
  const { fundAddress, pkhHex } = custody
  // the live path only ever builds the sqlite impl — the fields the UX
  // pokes (utxos/refresh/adopt) are its real surface
  const funds = custody.funds as import('@nektar/automation').SqlFunds
  const l = log(net)
  // funding wake: every producer that lands a utxo (p2p 'pay', FUND_TXIDS
  // import, indexer discovery, fee-vault mint) signals through addIfNew so
  // waitForFunding's poll can sleep event-driven instead of fixed-cadence
  let fundingWake: (() => void) | undefined
  // the adoption seam's proof fetcher — every producer that lands a funding
  // tx goes through addIfNew, so the proof attaches exactly once, at the
  // seam where custody takes the coin (SPV-03)
  const attach = fundingProofAttacher({ txStore, proofLookup: port.proofLookup, watchTx: deps.watchTx, log: l })
  // adopt is fire-and-forget for latency, but never silently: a refusal names
  // the outpoint and (via the chain-verified oracle, not an index) whether the
  // coin is already spent — the difference between "dead funding tx, send
  // another" and "already on the book"
  const adopt = (u: { tx: Transaction; vout: number }): void => {
    const tag = `${u.tx.id('hex').slice(0, 12)}…:${u.vout}`
    void funds
      .adopt(u.tx, u.vout)
      .then(async (ok) => {
        if (ok) return fundingWake?.()
        const why =
          (await funds.spendCheck?.(u.tx.id('hex'), u.vout).catch(() => undefined)) === true ? 'outpoint already spent on-chain' : 'already held or tombstoned'
        l(`funding ${tag} refused — ${why}`)
      })
      .catch((e) => l(`funding ${tag} adopt failed: ${(e as Error).message}`))
  }
  const addIfNew = (u: { tx: Transaction; vout: number }): Promise<void> => {
    txStore.put(u.tx)
    // first-party spend record beats any index or operator-named txid: an
    // outpoint a stored tx of ours already spends is never re-imported — a
    // definitively rejected/conflicted spender excepted, since that spend
    // never landed and the outpoint is genuinely unspent again
    const spender = txStore.spender(u.tx.id('hex'), u.vout)
    if (spender === undefined) {
      adopt(u)
    } else {
      void ledger
        .walletTx({ txid: spender })
        .then((w) => {
          if (importable(spender, w?.state)) adopt(u)
        })
        .catch(() => undefined) // unreadable ledger → stay skipped; the next import pass retries
    }
    // adoption is NOT delayed by the lookup — the caller's Promise.all only
    // bounds the proof fetch, not the coin book
    return attach(u.tx)
  }
  // optional p2p eyes: NEKTAR_P2P_PEERS='ip:port,...' — first-hand inv stream
  // watching our reserve outpoints (spend events by ANYONE) and the fee-key
  // script (unannounced funding). No index: watched set only, rest is dropped.
  const p2pPeers = (nektarEnv('NEKTAR_P2P_PEERS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  wireP2p({
    peers: p2pPeers,
    pkhHex,
    reserve: funds,
    addIfNew,
    // wireP2p emits its own `nektar p2p:` tag — bare stamp at the sink
    log: mkLog(''),
    spendStatus: (txid, vout) => port.spendStatus?.(txid, vout) ?? Promise.resolve('unknown' as const)
  })
  // NEKTAR_FEE_VAULT: anchor funding minted through the fee-vault covenant —
  // setupFeeVault returns a vault-take; the covenant mints capped publish
  // stock while deal and upkeep takes still draw from the shared pool
  const vaultSpec = nektarEnv('NEKTAR_FEE_VAULT')
  if (vaultSpec) {
    const vaultTake = await setupFeeVault({
      vaultSpec,
      dataDir: cfg.dataDir,
      pkhHex,
      fundKey: custody.fundKey ?? walletSigner(custody.clientWallet as SignerWallet, FUND_SIGN_ARGS),
      fundAddress,
      funds,
      broadcast: deps.broadcast,
      currentHeight: async () => await port.tracker.currentHeight(),
      net,
      log: mkLog('')
    })
    funds.takeOverride = async (purpose) => (purpose === 'publish' ? await vaultTake() : undefined)
  }
  // NEKTAR_FUND_TXIDS: funding txs to import directly (no address index needed)
  for (const txid of (nektarEnv('NEKTAR_FUND_TXIDS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)) {
    try {
      await Promise.all((await importFundingTx(mc, txid, pkhHex, port.tracker)).map((u) => addIfNew(u)))
    } catch (e) {
      l(`funding import ${txid}: ${(e as Error).message}`)
    }
  }
  const discover = async () => {
    // tracked-set model: we spend only what we made or were handed. Address-
    // index discovery is OPT-IN — NEKTAR_INDEXER_DISCOVERY=1 — because it
    // creates an indexer dependency that doesn't scale. The default funding
    // path is NEKTAR_FUND_TXIDS=<txid|hex:rawtx> (import above).
    if (nektarEnv('NEKTAR_INDEXER_DISCOVERY') !== '1') return
    try {
      await Promise.all(
        (await discoverUtxos(mc, fundAddress, (t, why) => l(`discover: index entry ${t.slice(0, 12)} skipped: ${why}`))).map((u) => addIfNew(u))
      )
    } catch (e) {
      l(`UTXO discovery unavailable (${(e as Error).message}) — set NEKTAR_FUND_TXIDS=<funding txid>`)
    }
  }
  const topUp = async () => {
    await discover()
    if (funds.utxos.length === 0) l(`reserve empty — send coins to ${fundAddress} and set NEKTAR_FUND_TXIDS (or NEKTAR_WAIT_FUNDING=1 to hold for funds)`)
  }
  // bootstrap UX (NEKTAR_WAIT_FUNDING=1): QR the funding address and hold
  // startup until a few sats land — ONCE, not per tick (the tick loop re-runs
  // topUp every confirmEveryMs and must not block). Detection during the
  // wait: indexer discovery when enabled, p2p 'pay' auto-import, or the
  // operator setting NEKTAR_FUND_TXIDS. NEKTAR_WAIT_FUNDING_MS caps it (30 min).
  const waitForFunding = async () => {
    if (funds.utxos.length > 0 || nektarEnv('NEKTAR_WAIT_FUNDING') !== '1') return
    try {
      const QR = await import('qrcode')
      writeErr('\n' + (await QR.toString(fundAddress, { type: 'terminal', small: true })) + '\n')
    } catch {
      /* QR optional — the address prints either way */
    }
    l(`waiting for funding — send a few sats to ${fundAddress}`)
    const deadline = Date.now() + Number(nektarEnv('NEKTAR_WAIT_FUNDING_MS') ?? 30 * 60_000)
    const pollMs = Number(nektarEnv('NEKTAR_FUND_POLL_MS') ?? 15_000)
    while (funds.utxos.length === 0 && Date.now() < deadline) {
      await funds.refresh().catch(() => undefined)
      // event-driven wait: addIfNew resolves the sleep the moment a utxo
      // lands; re-check first so a just-landed utxo can't sleep past us,
      // and a missed signal degrades to the bounded poll, never a hang
      await new Promise<void>((r) => {
        if (funds.utxos.length > 0) return r()
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
          await Promise.all((await importFundingTx(mc, txid, pkhHex, port.tracker)).map((u) => addIfNew(u)))
        } catch {
          /* keep polling */
        }
      }
      await discover()
    }
    if (funds.utxos.length > 0) {
      l(`funded ✓ — ${funds.utxos.reduce((s, u) => s + u.satoshis, 0)} sats across ${funds.utxos.length} utxo(s)`)
    } else {
      l(`funding wait timed out — booting unfunded (anchors fail until ${fundAddress} has a utxo; use NEKTAR_FUND_TXIDS)`)
    }
  }
  await waitForFunding()
  await topUp()
  const ops = {
    wallet: () => {
      const balanceSats = funds.utxos.reduce((s, u) => s + u.satoshis, 0)
      const costs = port.guard?.costs() ?? []
      const avg = costs.length ? Math.round(costs.reduce((s, c) => s + c.fee, 0) / costs.length) : 0
      return Promise.resolve({
        address: fundAddress,
        balanceSats,
        utxos: funds.utxos.length,
        spentTodaySats: port.guard?.spentToday() ?? 0,
        caps: { maxFeePerTx: mc.maxFeePerTx!, maxPerDay: mc.maxSpendPerDay!, minIntervalMs: mc.minBroadcastIntervalMs! },
        avgFeePerAnchorSats: avg,
        runwayAnchors: avg > 0 ? Math.floor(balanceSats / avg) : null
      })
    },
    costs: () => port.guard?.costs() ?? [],
    // fundingImport MCP tool: credit a tx paying the fee address into the
    // reserve without a restart (same path as NEKTAR_FUND_TXIDS)
    importFunding: async (txid: string) => {
      const added = await importFundingTx(mc, txid, pkhHex, port.tracker)
      await Promise.all(added.map((u) => addIfNew(u)))
      const balanceSats = funds.utxos.reduce((s, u) => s + u.satoshis, 0)
      return { imported: added.length, utxos: funds.utxos.length, balanceSats, address: fundAddress, txids: [...new Set(added.map((u) => u.tx.id('hex')))] }
    },
    // fundingRefresh MCP tool: reconcile the wallet's funding view against
    // chain truth — the wallet's spendable view IS the reconcile; a refresh
    // re-queries listOutputs, then adopts whatever the index reports live
    // at the fee address. Fixes the phantom-entry/dust-pick class.
    refreshFunding: async () => {
      await funds.refresh()
      let imported = 0
      try {
        const found = await discoverUtxos(mc, fundAddress, (t, why) => l(`discover: index entry ${t.slice(0, 12)} skipped: ${why}`))
        const counts = await Promise.all(
          found.map(async (u) => {
            const before = funds.utxos.length
            await addIfNew(u)
            return funds.utxos.length > before ? 1 : 0
          })
        )
        imported = counts.reduce((a: number, b) => a + b, 0)
      } catch {
        /* discovery down — wallet view still refreshed */
      }
      const balanceSats = funds.utxos.reduce((s, u) => s + u.satoshis, 0)
      return { refreshed: true, imported, utxos: funds.utxos.length, balanceSats, address: fundAddress }
    }
  }
  return { ops, refresh: async () => void (await ops.refreshFunding()), topUp }
}
