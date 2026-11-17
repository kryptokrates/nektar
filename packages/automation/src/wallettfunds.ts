// Reserve-funder shape over toolbox wallet storage (extracted from
// toolbox.ts — see that file's header for the dual-build boundary note: every
// object handed INTO the wallet must come from the cjs `sdkc` class set).

import { createRequire } from 'node:module'
import { Beef, MerklePath, Transaction, type WalletInterface } from '@bsv/sdk'
import { sdk } from '@bsv/wallet-toolbox'
import { FundsBase, type FundingOut, type FundsDeps, type FundsStore } from '@nektar/chain'
import { NEKTAR_BASKET } from './toolbox.js'

const sdkc = createRequire(import.meta.url)('@bsv/sdk') as typeof import('@bsv/sdk')

// a pathless funding parent can never anchor a tree. Re-attach by copy, the
// same pattern buildAtomicBeef uses.
const withPath = (found: Transaction): Transaction => {
  const tx = Transaction.fromHex(found.toHex())
  if (found.merklePath !== undefined) tx.merklePath = MerklePath.fromBinary(found.merklePath.toBinary())
  return tx
}

/** The wallet-backed `Funds` owner: `take` reserves a non-destructive peek
 *  (smallest sufficient, lease-filtered, optional spendCheck) over the
 *  wallet's default basket; `spent(tx)` relinquishes consumed outputs and
 *  internalizes our change via `internalizeAction`; `adopt`/`revive`
 *  internalize live outputs; `drop` relinquishes dead weight. `utxos` is a
 *  sync cache refreshed on every `take` — the async wallet list is the
 *  truth. */
/** basket listing page size + cap — listOutputs truncates silently at its
 *  limit, so every basket read pages until a short page or the cap; the
 *  cap is a load ceiling, never a correct cut-off, so hitting it logs */
const PAGE = 1000
const MAX_PAGES = 20

export class WalletFunds extends FundsBase {
  private cache: FundingOut[] = []
  /** outpoints we dropped this process — lets `died` tell "consumed by the
   *  dead spend" (revive) vs "never ours" (leave). Restart honesty comes
   *  from the first-party spentByUs check, not a persisted tombstone. */
  private readonly dropped = new Set<string>()
  protected readonly store: FundsStore

  constructor(
    private wallet: Pick<WalletInterface, 'listOutputs' | 'internalizeAction' | 'relinquishOutput' | 'getPublicKey'>,
    private services: Pick<sdk.WalletServices, 'getRawTx' | 'getMerklePath'> | undefined,
    deps: FundsDeps & { log?: (s: string) => void }
  ) {
    super(deps)
    const log = deps.log ?? (() => {})
    this.store = {
      list: async () => {
        await this.refresh()
        return this.cache
      },
      has: async (txid, vout) => this.cache.some((u) => u.tx.id('hex') === txid && u.vout === vout) || (await this.has(txid, vout)),
      add: (tx, vout) => this.internalizePush(tx, vout, log),
      drop: (txid, vout) => {
        this.cache = this.cache.filter((u) => !(u.tx.id('hex') === txid && u.vout === vout))
        this.dropped.add(`${txid}:${vout}`)
        // basket removal is the honest equivalent — a spent wallet outpoint
        // must never be offered again. Fire-and-forget: the automation
        // ledger's outpoint lease covers the async gap.
        void this.wallet.relinquishOutput({ basket: NEKTAR_BASKET, output: `${txid}.${vout}` }).catch(() => {})
      },
      revive: async (tx, vout) => {
        this.dropped.delete(`${tx.id('hex')}:${vout}`)
        await this.internalizePush(tx, vout, log)
      },
      tombstoned: (txid, vout) => this.dropped.has(`${txid}:${vout}`)
    }
  }

  /** local store of already-seen transactions — funding imports arrive with
   *  their ancestry attached and `TxStore` keeps it, so a descendant
   *  internalize resolves parents locally instead of re-fetching what the
   *  import already carried (one indexer call saved per ancestor, and a
   *  rate-limited indexer can no longer fail a self-contained import) */
  txLookup: ((txid: string) => Transaction | undefined) | undefined

  get utxos(): FundingOut[] {
    return this.cache
  }

  /** identity comes from the wallet itself — the BRC-100 identity key
   *  derives our receive address and the P2PKH lock that marks change */
  async address(): Promise<string> {
    const pub = await this.wallet.getPublicKey({ identityKey: true }, 'nektar')
    return sdkc.PublicKey.fromString(pub.publicKey).toAddress()
  }

  private lockCache?: string
  protected async ourLock(): Promise<string> {
    if (this.lockCache === undefined) {
      const pub = await this.wallet.getPublicKey({ identityKey: true }, 'nektar')
      this.lockCache = new sdkc.P2PKH().lock(sdkc.PublicKey.fromString(pub.publicKey).toAddress()).toHex()
    }
    return this.lockCache
  }

  async refresh(): Promise<void> {
    const outs: FundingOut[] = []
    let truncated = true
    for (let page = 0; page < MAX_PAGES; page++) {
      const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, include: 'entire transactions', limit: PAGE, offset: page * PAGE })
      const beef = r.BEEF !== undefined ? sdkc.Beef.fromBinary(r.BEEF) : undefined
      for (const o of r.outputs) {
        if (!o.spendable) continue
        const [txid, vout] = o.outpoint.split('.')
        const tx = beef?.findAtomicTransaction(txid)
        if (tx !== undefined) outs.push({ tx: withPath(tx), vout: Number(vout), satoshis: o.satoshis })
      }
      if (r.outputs.length < PAGE) {
        truncated = false
        break
      }
    }
    if (truncated) this.deps.log?.(`walletfunds: basket listing hit the ${PAGE * MAX_PAGES}-output page cap — refresh sees a subset`)
    this.cache = outs
  }

  /** Whether the wallet already custodies this outpoint — lets boot seeding
   *  skip re-internalizing outputs that survived a restart. */
  async has(txid: string, vout: number): Promise<boolean> {
    for (let page = 0; page < MAX_PAGES; page++) {
      const r = await this.wallet.listOutputs({ basket: NEKTAR_BASKET, limit: PAGE, offset: page * PAGE })
      if (r.outputs.some((o) => o.outpoint === `${txid}.${vout}`)) return true
      if (r.outputs.length < PAGE) return false
    }
    return false
  }

  private internalizePush(tx: Transaction, vout: number, log: (s: string) => void): Promise<void> {
    if (this.cache.some((u) => u.tx.id('hex') === tx.id('hex') && u.vout === vout)) return Promise.resolve()
    // cache only what the wallet actually accepted — an optimistic add made
    // utxos report funding the wallet never held (the 60-utxo silent-fail
    // class: internalize errors were logged while the cache lied)
    return this.internalize(tx, vout)
      .then(() => {
        this.dropped.delete(`${tx.id('hex')}:${vout}`)
        this.cache.push({ tx, vout, satoshis: tx.outputs[vout].satoshis ?? 0 })
      })
      .catch((e) => log(`walletfunds internalize ${tx.id('hex')}.${vout}: ${e instanceof Error ? e.message : String(e)}`))
  }

  private async internalize(tx: Transaction, vout: number): Promise<void> {
    // BRC-100 wants a self-contained AtomicBEEF: txid-only deps are rejected
    // and every unproven tx must chain through included ancestors to a BUMP.
    // `parent()` therefore recurses: an unconfirmed input parent carries its
    // own ancestry with it until the chain bottoms out at proven roots —
    // zeroconf spends internalize whole, not one level deep.
    const txid = tx.id('hex')
    const atomic = this.atomicBeefCache.get(txid) ?? (await this.buildAtomicBeef(tx))
    this.atomicBeefCache.set(txid, atomic)
    try {
      await this.wallet.internalizeAction({
        tx: atomic,
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

  /** txid → completed atomic BEEF: outputs of one tx share the identical
   *  ancestry closure, so seeding 60 utxos pays for the deep walk once. */
  private atomicBeefCache = new Map<string, number[]>()

  private async buildAtomicBeef(tx: Transaction): Promise<number[]> {
    // a mined subject anchors itself: its own BUMP is the whole proof, so no
    // input ancestry is needed. Walking the parents of a many-input tx was
    // pure indexer load — and one rate-limited parent failed the whole import
    if (tx.merklePath === undefined) {
      const own = await this.services?.getMerklePath(tx.id('hex')).catch(() => undefined)
      if (own?.merklePath !== undefined) tx.merklePath = MerklePath.fromBinary((own.merklePath as unknown as MerklePath).toBinary())
    }
    const beef = new Beef()
    beef.mergeTransaction(tx)
    if (tx.merklePath !== undefined) return beef.toBinaryAtomic(tx.id('hex'))
    for (const i of tx.inputs) {
      if (i.sourceTXID === undefined) continue
      const parent = i.sourceTransaction ?? (await this.parent(i.sourceTXID))
      if (parent === undefined) throw new Error(`internalize: no source tx for input ${i.sourceTXID}`)
      beef.mergeTransaction(parent)
    }
    // closure completion: the wallet's validators demand every real entry be
    // proven (BUMP) or have all inputs resolved — txid-only stubs are
    // rejected and a dangling input rejects the whole atomic beef. Object-
    // linked ancestry arrives with no merklePath, so walk the merged set:
    // give every mined entry its BUMP (getMerklePath's deep fallback covers
    // old coinbases), and fetch+merge whatever a still-unproven entry
    // references. Fixpoint until nothing changes.
    for (;;) {
      let progress = false
      for (const bt of [...beef.txs]) {
        if (bt.isTxidOnly || bt.hasProof || bt.tx === undefined) continue
        const mp = await this.services?.getMerklePath(bt.txid)
        if (mp?.merklePath !== undefined) {
          bt.tx.merklePath = MerklePath.fromBinary((mp.merklePath as unknown as MerklePath).toBinary())
          beef.mergeTransaction(bt.tx)
          progress = true
          continue
        }
        for (const dep of bt.inputTxids) {
          if (beef.findTxid(dep) == null) {
            const p = await this.parent(dep)
            if (p === undefined) throw new Error(`internalize: unproven ancestor ${bt.txid} has unresolvable input ${dep}`)
            beef.mergeTransaction(p)
            progress = true
          }
        }
      }
      if (!progress) break
    }
    return beef.toBinaryAtomic(tx.id('hex'))
  }

  /** Fetch a parent tx with its merkle path attached when mined — what the
   *  AtomicBEEF validator needs to accept the dependency. An unmined parent
   *  has no proof, so its own inputs are fetched and linked recursively until
   *  the ancestry anchors at proven roots (`depth` bounds pathological
   *  zeroconf chains). */
  private async parent(txid: string, depth = 8): Promise<Transaction | undefined> {
    const local = this.txLookup?.(txid)
    if (local !== undefined) return local
    if (this.services === undefined) return undefined
    const r = await this.services.getRawTx(txid)
    if (r.rawTx === undefined) return undefined
    const tx = Transaction.fromBinary(r.rawTx)
    const mp = await this.services.getMerklePath(txid)
    if (mp.merklePath !== undefined) tx.merklePath = MerklePath.fromBinary((mp.merklePath as unknown as MerklePath).toBinary())
    else if (depth > 0)
      for (const i of tx.inputs)
        if (i.sourceTransaction === undefined && i.sourceTXID !== undefined) i.sourceTransaction = await this.parent(i.sourceTXID, depth - 1)
    return tx
  }
}
