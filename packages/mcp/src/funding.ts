// funding.ts — how the runtime gets coins. Two separable questions live here:
//   custody — which wallet owns the coin book (toolbox embedded, or an
//             external BRC-100 substrate), who signs, where the key lock sits;
//   funding — how the reserve fills: regtest treasury bootstrap, and the live
//             UX (p2p eyes, fee vault, txid imports, index discovery, the
//             funding wait, and the /v1/wallet + /v1/costs ops surface).
// Chain questions are already answered by the ChainPort; this file only sees
// port.broadcast/tracker/mine/guard.
import { join } from 'node:path'
import { PrivateKey, Transaction, WalletClient, PublicKey as SdkPublicKey } from '@bsv/sdk'
import { walletSigner, FUND_SIGN_ARGS, nektarEnv, nektarEnvFlag, nektarEnvNum, type SignerWallet } from '@nektar/core'
import {
  bootstrapReserve,
  regtestRpc,
  importFundingTx,
  discoverUtxos,
  type ChainPort,
  type BroadcastResult,
  type RegtestConfig,
  type MainnetConfig,
  type TxStore,
  type Funds
} from '@nektar/chain'
import { wireP2p, setupFeeVault } from './setup.js'
import { acquireLock, keyLockFile, loadFeeKey } from './rtsupport.js'
import { importable } from './txops.js'

const log = (net: string) => (s: string) => process.stderr.write(`nektar ${net}: ${s}\n`)

/** the coin book a custody path hands the runtime — `Funds` plus the fields
 *  the concrete impls happen to expose (a real wallet path always has all
 *  of them; an injected test-tier Funds may carry none) */
interface CoinBook extends Funds {
  utxos?: Array<{ tx: Transaction; vout: number; satoshis?: number }>
  dealFloor?: number
}

export interface CustodyResult {
  /** the one coin book (WalletFunds on every real wallet path) */
  funds: CoinBook
  /** injected signing surface for lanes/bstack protocol calls */
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
  pass: string | undefined
): Promise<CustodyResult> {
  const tb = await import('@nektar/automation')
  const net = port.network
  const walletMode = nektarEnv('NEKTAR_WALLET') ?? 'toolbox'
  // upkeep/publish takes may not dip the book below what keeps one deal
  // affordable — the impl's take() enforces it on every pick
  const dealFloor = nektarEnvNum('NEKTAR_DEAL_FLOOR_SATS') ?? 0
  if (net === 'regtest') {
    if (walletMode !== 'toolbox')
      throw new Error(`regtest custody is NEKTAR_WALLET=toolbox — got '${walletMode}' (legacy was removed; client has no regtest path)`)
    const rc = cfg.regtest!
    const fundKey = PrivateKey.fromWif(rc.fundWif)
    // D2: wallet-relayed txs go through the SAME SpendGuard — postBeef is a
    // spend entry point, not a side channel around the caps
    const services = new tb.RegtestWalletServices(rc, port.tracker as import('@nektar/chain').TeranodeRPCTracker, (tx) => port.broadcast(tx))
    const toolbox = await tb.openToolboxWallet({ fundKey, dataDir: cfg.dataDir, chain: 'regtest', services })
    const funds = new tb.WalletFunds(toolbox.wallet, services, { log: log(net), dealFloor })
    await funds.refresh()
    const pkhHex = Buffer.from(fundKey.toPublicKey().toHash()).toString('hex')
    return {
      funds,
      wallet: toolbox.wallet,
      fundKey,
      fundAddress: fundKey.toPublicKey().toAddress([0x6f]),
      pkhHex,
      closeWallet: () => toolbox.close()
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
    if (mc.fundWif !== undefined) process.stderr.write(`nektar ${net}: NEKTAR_FUND_WIF ignored under NEKTAR_WALLET=client — the external wallet signs\n`)
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
    process.stderr.write(`nektar ${net}: NEKTAR_WALLET=client — signing via external wallet substrate (derived funding address ${fundAddress})\n`)
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
    process.stderr.write(`nektar ${net}: NEKTAR_ALLOW_SHARED_KEY=1 — fund-key lock skipped; two processes may spend this reserve\n`)
  } else {
    releaseKeyLock = await acquireLock(keyLockFile(pkhHex), `fund key ${fundAddress}`)
  }
  process.stderr.write(
    `nektar ${net}: operator fee key ${fundAddress} — fund this address to enable anchoring (then set NEKTAR_FUND_TXIDS=<txid|hex:rawtx>, or NEKTAR_WAIT_FUNDING=1 to QR + hold for funds)\n`
  )
  // D2: the wallet's own postBeef relays through the SAME SpendGuard — a
  // wallet-signed action is a spend entry point like any other
  const services = new tb.LiveWalletServices({ chain: testNet ? 'test' : 'main', mc, tracker: port.tracker, broadcast: (tx) => port.broadcast(tx) })
  let closeWallet: (() => Promise<void>) | undefined
  let wallet: import('@bsv/sdk').WalletInterface | undefined
  let funds: import('@nektar/automation').WalletFunds
  if (externalWallet) {
    funds = new tb.WalletFunds(clientWallet!, services, { log: log(net), dealFloor })
    wallet = clientWallet
    await funds.refresh()
    process.stderr.write(`nektar ${net}: wallet backend client (external BRC-100) — ${funds.utxos.length} funding utxo(s)\n`)
  } else {
    if (fundKey === undefined) throw new Error('NEKTAR_WALLET=toolbox embeds its keyDeriver from the fee key — =client is the keyless mode')
    const toolbox = await tb.openToolboxWallet({ fundKey, dataDir: cfg.dataDir, chain: testNet ? 'test' : 'main', services })
    funds = new tb.WalletFunds(toolbox.wallet, services, { log: log(net), dealFloor })
    wallet = toolbox.wallet
    closeWallet = () => toolbox.close()
    await funds.refresh()
    process.stderr.write(`nektar ${net}: wallet backend toolbox (wallet.sqlite) — ${funds.utxos.length} funding utxo(s)\n`)
  }
  return { funds, wallet, fundKey, clientWallet, fundAddress, pkhHex, releaseKeyLock, closeWallet }
}

/** regtest treasury bootstrap — the funder's own chain in the data dir; split
 *  mined before use. Bootstrap writes straight into the wallet via the seed's
 *  addOwned hook (internalize). Only runs while the wallet holds <5 utxos. */
export async function regtestSeed(deps: {
  rc: RegtestConfig & { fundWif: string }
  port: ChainPort
  funds: import('@nektar/automation').WalletFunds
  dataDir: string
}): Promise<void> {
  const { rc, port, funds, dataDir } = deps
  if (funds.utxos.length >= 5) return
  const fundKey = PrivateKey.fromWif(rc.fundWif)
  const seed = { addOwned: (tx: Transaction, vout: number) => void funds.adopt(tx, vout) }
  const s = await bootstrapReserve(rc, fundKey, seed, regtestRpc(rc), { treasuryFile: join(dataDir, 'treasury.json') })
  if (!s && funds.utxos.length === 0) throw new Error('regtest: could not fund the reserve (no unspent treasury or early coinbase for the fund key)')
  if (s) await port.mine?.()
  await funds.refresh()
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
    costs: () => Array<{ txid: string; bytes: number; fee: number; at: string }>
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
  // the live path only ever builds the wallet impl — the fields the UX
  // pokes (utxos/refresh/adopt) are its real surface
  const funds = custody.funds as import('@nektar/automation').WalletFunds
  const l = log(net)
  // funding wake: every producer that lands a utxo (p2p 'pay', FUND_TXIDS
  // import, indexer discovery, fee-vault mint) signals through addIfNew so
  // waitForFunding's poll can sleep event-driven instead of fixed-cadence
  let fundingWake: (() => void) | undefined
  const addIfNew = (u: { tx: Transaction; vout: number }) => {
    txStore.put(u.tx)
    // first-party spend record beats any index or operator-named txid: an
    // outpoint a stored tx of ours already spends is never re-imported — a
    // definitively rejected/conflicted spender excepted, since that spend
    // never landed and the outpoint is genuinely unspent again
    const spender = txStore.spender(u.tx.id('hex'), u.vout)
    if (spender === undefined) {
      void funds.adopt(u.tx, u.vout)
      fundingWake?.()
      return
    }
    void ledger
      .walletTx({ txid: spender })
      .then((w) => {
        if (importable(spender, w?.state)) {
          void funds.adopt(u.tx, u.vout)
          fundingWake?.()
        }
      })
      .catch(() => undefined) // unreadable ledger → stay skipped; the next import pass retries
  }
  // optional p2p eyes: NEKTAR_P2P_PEERS='ip:port,...' — first-hand inv stream
  // watching our reserve outpoints (spend events by ANYONE) and the fee-key
  // script (unannounced funding). No index: watched set only, rest is dropped.
  const p2pPeers = (nektarEnv('NEKTAR_P2P_PEERS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)
  wireP2p({ peers: p2pPeers, pkhHex, reserve: funds, addIfNew, log: (s) => process.stderr.write(s) })
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
      log: (s) => process.stderr.write(s)
    })
    funds.takeOverride = async (purpose) => (purpose === 'publish' ? await vaultTake() : undefined)
  }
  // NEKTAR_FUND_TXIDS: funding txs to import directly (no address index needed)
  for (const txid of (nektarEnv('NEKTAR_FUND_TXIDS') ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean)) {
    try {
      for (const u of await importFundingTx(mc, txid, pkhHex)) addIfNew(u)
    } catch (e) {
      l(`funding import ${txid}: ${(e as Error).message}`)
    }
  }
  const discover = async () => {
    // tracked-set model: we spend only what we made or were handed. Address-
    // index discovery (WoC) is OPT-IN — NEKTAR_INDEXER_DISCOVERY=1 — because it
    // creates an indexer dependency that doesn't scale. The default funding
    // path is NEKTAR_FUND_TXIDS=<txid|hex:rawtx> (import above).
    if (nektarEnv('NEKTAR_INDEXER_DISCOVERY') !== '1') return
    try {
      for (const u of await discoverUtxos(mc, fundAddress, (t, why) => l(`discover: index entry ${t.slice(0, 12)} skipped: ${why}`))) addIfNew(u)
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
      process.stderr.write('\n' + (await QR.toString(fundAddress, { type: 'terminal', small: true })) + '\n')
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
          for (const u of await importFundingTx(mc, txid, pkhHex)) addIfNew(u)
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
    wallet: async () => {
      const balanceSats = funds.utxos.reduce((s, u) => s + u.satoshis, 0)
      const costs = port.guard?.costs() ?? []
      const avg = costs.length ? Math.round(costs.reduce((s, c) => s + c.fee, 0) / costs.length) : 0
      return {
        address: fundAddress,
        balanceSats,
        utxos: funds.utxos.length,
        spentTodaySats: port.guard?.spentToday() ?? 0,
        caps: { maxFeePerTx: mc.maxFeePerTx!, maxPerDay: mc.maxSpendPerDay!, minIntervalMs: mc.minBroadcastIntervalMs! },
        avgFeePerAnchorSats: avg,
        runwayAnchors: avg > 0 ? Math.floor(balanceSats / avg) : null
      }
    },
    costs: () => port.guard?.costs() ?? [],
    // fundingImport MCP tool: credit a tx paying the fee address into the
    // reserve without a restart (same path as NEKTAR_FUND_TXIDS)
    importFunding: async (txid: string) => {
      const added = await importFundingTx(mc, txid, pkhHex)
      for (const u of added) addIfNew(u)
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
        for (const u of await discoverUtxos(mc, fundAddress, (t, why) => l(`discover: index entry ${t.slice(0, 12)} skipped: ${why}`))) {
          const before = funds.utxos.length
          addIfNew(u)
          if (funds.utxos.length > before) imported++
        }
      } catch {
        /* discovery down — wallet view still refreshed */
      }
      const balanceSats = funds.utxos.reduce((s, u) => s + u.satoshis, 0)
      return { refreshed: true, imported, utxos: funds.utxos.length, balanceSats, address: fundAddress }
    }
  }
  return { ops, refresh: async () => void (await ops.refreshFunding()), topUp }
}
