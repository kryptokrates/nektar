// MemAnchor — anchor/head state and everything built on it: AnchorOps,
// AuthorshipOps (rotate/authorship/cert chain), RevocationOps, BondOps
// delegates, the promise/checkpoint monitors, flushDue/anchorStatus, and
// state persistence (persistState/restoreState own anchors+tails+cps+flags).
import { Transaction } from '@bsv/sdk'
import { AnchorChain, type CheckpointBond, type BondUtxo } from '@nektar/anchor'
import { PromiseMonitor, type Checkpoint } from '@nektar/log'
import type { AnchorLink } from '@nektar/verify'
import type { SignerLike } from '@nektar/core'
import * as idbind from '@nektar/identity'
import type { BondOpsPort } from './ports.js'
import type { PersistedMemoryState } from './types.js'
import { MemTx } from './tx.js'

export abstract class MemAnchor extends MemTx {
  protected anchorChain(shard: string): AnchorChain {
    if (!this.anchors.has(shard)) {
      this.anchors.set(
        shard,
        new AnchorChain(shard, this.shardKey(shard), this.funds, this.fundSigner(), this.chain, {
          burstLimit: this.opts.burstLimit,
          feeRateSatPerKb: this.opts.feeRateSatPerKb,
          network: this.opts.network,
          shardPub: this._shardPubs.get(shard)
        })
      )
    }
    return this.anchors.get(shard)!
  }

  /** the shard's current anchor head (undefined before the first anchor) */
  anchorHead(shard: string) {
    return this.anchorOps.anchorHead(shard)
  }
  /** an anchor tx by txid (as broadcast, with BUMP when attached) */
  anchorTx(txid: string) {
    return this.anchorOps.anchorTx(txid)
  }
  /** subscribe to fresh unproven anchors — emitted the moment an anchor
   *  broadcast lands in the tail set, so the confirmer watches/mines now
   *  rather than at its interval floor */
  onAnchorQueued(cb: (txid: string, shard: string) => void): void {
    this.anchorOps.onAnchorQueued(cb)
  }
  /** the shard's latest anchor link (tail of the on-chain succession) */
  tailLink(shard: string) {
    return this.anchorOps.tailLink(shard)
  }
  /** anchors still waiting for their BUMP */
  unprovenAnchors(): Array<{ shard: string; txid: string }> {
    return this.anchorOps.unprovenAnchors()
  }
  /** attach a BUMP to an anchor (must prove THIS txid) and mark the chain confirmed */
  attachProof(shard: string, txid: string, mp: import('@bsv/sdk').MerklePath): void {
    this.anchorOps.attachProof(shard, txid, mp)
    this.clearProofCache()
  }
  /** shards this instance has written or anchored */
  shards(): string[] {
    return [...new Set([...this.anchors.keys(), ...this.touched])]
  }
  /** every anchor link of the shard, genesis first (read-only copy) */
  anchorLinks(shard: string): AnchorLink[] {
    return this.anchorOps.anchorLinks(shard)
  }

  /** Close the pending batch of a shard and broadcast the anchor. */
  async anchorShard(shard: string): Promise<{ txid: string; n: number } | null> {
    return await this.anchorOps.anchorShard(shard)
  }

  /** rotate a shard's owner key on-chain (rotation cert emitted by AnchorChain).
   *  D4 (§19): the cert is also written to the `__identity__` shard and anchored
   *  — a third party reconstructs the pk(0)→pk(i) ledger from chain data alone. */
  async rotateShard(shard: string, newKey: SignerLike): Promise<{ txid: string; n: number }> {
    return await this.authorshipOps.rotateShard(shard, newKey)
  }

  /** resolve the shard's owner cert chain: genesis pub from the genesis
   *  anchor's decoded ownerPub + live rotation certs. Cached per shard. */
  protected async authorChain(shard: string): Promise<{ genesisPub: string; certs: import('@nektar/identity').RotationCert[] } | null> {
    return await this.authorshipOps.authorChain(shard)
  }

  /** §19: was record seq written by the key of its recorded epoch, inside that key's window? */
  async authorship(shard: string, seq: number, genesisPub: string, certs: import('@nektar/identity').RotationCert[]) {
    return await this.authorshipOps.authorship(shard, seq, genesisPub, certs)
  }

  /** D4 (§19 M_U): the shard's anchored rotation ledger — genesis owner pub
   *  plus every `rotation-cert` record in `__identity__` with its inclusion
   *  bundle. A verifier replays verifyCertChain over certs it can each prove
   *  were anchored; a withheld cert can only make the chain look older. */
  async rotationChainProof(shard: string): Promise<{
    genesisPub: string | null
    certs: Array<{
      cert: import('@nektar/identity').RotationCert
      seq: number
      data: string
      prov?: import('@nektar/core').Provenance
      E?: import('@nektar/core').Bytes
      leaf: string
      bundle?: import('@nektar/verify').ProofBundle
      anchored: boolean
    }>
  } | null> {
    return await this.authorshipOps.rotationChainProof(shard)
  }

  /** revocation registry (IDN-007): lives in revocations.ts — SMT keyed by
   *  derive_key(pubkey); tests/server read the SMT directly via this getter */
  get revocations(): idbind.Smt {
    return this.revocationOps.smt
  }
  /** revoke a 33-byte pubkey: SMT key = derive_key(pubkey); persisted */
  revoke(pubHex: string): Uint8Array {
    return this.revocationOps.revoke(pubHex)
  }
  isRevoked(pubHex: string): boolean {
    return this.revocationOps.isRevoked(pubHex)
  }

  /** §19 third-party revocation proof — implemented in revocations.ts */
  async revocationProof(pubHex: string) {
    return await this.revocationOps.revocationProof(pubHex)
  }

  /** §19 CRL status — implemented in revocations.ts */
  async revocationStatus(pubHex: string) {
    return await this.revocationOps.revocationStatus(pubHex)
  }

  // ── §12 SLA bond (NektarCheckpointBond) — lifecycle lives in bonds.ts ──

  async bondDeploy(sats = this.opts.bond?.sats ?? 5_000, releaseHeight?: bigint, force = false): Promise<{ txid: string; releaseHeight: bigint }> {
    return await this.bonds.deploy(sats, releaseHeight, force)
  }
  bondStatus(): ReturnType<BondOpsPort['status']> {
    return this.bonds.status()
  }
  async bondSeize(payToScript?: string, bond?: { cfg: ConstructorParameters<typeof CheckpointBond>[0]; utxo: BondUtxo }): Promise<{ txid: string } | null> {
    return await this.bonds.seize(payToScript, bond)
  }
  async bondCommit(root: string): Promise<{ txid: string; n: bigint } | null> {
    return await this.bonds.commit(root)
  }
  async bondClose(): Promise<{ txid: string } | null> {
    return await this.bonds.close()
  }

  /** broadcaster-callback path: a MINED push names only the txid — resolve the
   *  shard and confirm; no-op for txids we didn't anchor. Never a poll, always push. */
  confirmByTxid(txid: string): boolean {
    const shard = this.txidShard.get(txid)
    if (!shard) return false
    this.confirmAnchor(shard, txid)
    return true
  }

  /** Mark anchor txs mined (drive from broadcaster status events). */
  confirmAnchor(shard: string, txid: string): void {
    this.anchorOps.confirmAnchor(shard, txid)
  }

  protected covered() {
    return (p: { shard: string; seq: number }) => (this.cps.get(p.shard) ?? []).some((c) => c.size > p.seq)
  }
  /** LOG-005: own promises not covered by a valid checkpoint within the delay */
  async promiseViolations(maxMergeDelayMs: number, now = Date.now()) {
    const mon = new PromiseMonitor(await this.logPublicKey(), (s) => this.cps.get(s)?.[0]?.logId)
    for (const p of this.promises) mon.observePromise(p)
    for (const list of this.cps.values()) for (const c of list) mon.observeCheckpoint(c)
    return mon.violations(now, maxMergeDelayMs)
  }
  /** LOG-007: signed checkpoints issued for the shard, oldest first */
  checkpoints(shard: string): Checkpoint[] {
    return [...(this.cps.get(shard) ?? [])]
  }

  /** LOG-006: anchor every shard whose close policy fires now (interval /
   *  bytes / cost thresholds also fire without a new write — call periodically) */
  async flushDue(): Promise<Array<{ shard: string; txid: string; n: number }>> {
    const out: Array<{ shard: string; txid: string; n: number }> = []
    for (const s of this.touched) {
      if (await this.log.closeReason(s)) {
        const a = await this.anchorShard(s)
        if (a) out.push({ shard: s, ...a })
      }
    }
    // §12 SLA watchdog: if no anchor renewed the bond for half an epoch,
    // commit the current root again — a lapsed deadline makes the bond
    // seizable by anyone, so the operator must refresh before it passes.
    {
      const renewed = await this.bonds.renewIfDue()
      if (renewed === 'committed') this.metrics.inc('nektar_bond_renewals_total', {}, 1, 'bond deadline renewals committed by the SLA watchdog')
      else if (renewed === 'failed') this.metrics.inc('nektar_bond_commit_failures_total', {}, 1, 'bond commit broadcasts that failed')
    }
    return out
  }

  /** CHN-006: pending (not batched/anchored or not mined), mined (k < depth), final */
  async anchorStatus(shard: string, seq: number): Promise<{ state: 'pending' | 'mined' | 'final'; confirmations: number; depth: number }> {
    const depth = this.opts.finalityDepth ?? 1
    const rec = await this.store.bySeq(shard, seq)
    const mp = rec?.anchorTxid ? this.anchorTxs.get(rec.anchorTxid)?.merklePath : undefined
    if (!mp) return { state: 'pending', confirmations: 0, depth }
    const tip = this.chain.tipHeight ? await this.chain.tipHeight() : await this.chain.currentHeight()
    const confirmations = Math.max(0, tip - mp.blockHeight + 1)
    return { state: confirmations >= depth ? 'final' : 'mined', confirmations, depth }
  }

  // ── persisted anchor state (restart without forking the shard) ───────────
  protected persistState(): void {
    const st = this.opts.stateStore
    if (!st) return
    const shards: PersistedMemoryState['shards'] = {}
    for (const [shard, ac] of this.anchors) {
      const head = ac.getHead()
      if (!head) continue
      const anchors: Record<string, string> = {}
      for (const l of this.tails.get(shard) ?? []) {
        const tx = this.anchorTxs.get(l.txid)
        if (tx) anchors[l.txid] = Buffer.from(ac.beefOf(tx, true)).toString('hex')
      }
      // the head may be a rotate spend that adds no tail link
      anchors[head.tx.id('hex')] = Buffer.from(ac.beefOf(head.tx, true)).toString('hex')
      shards[shard] = {
        headTxid: head.tx.id('hex'),
        confirmed: !!head.tx.merklePath,
        tails: this.tails.get(shard) ?? [],
        checkpoints: this.cps.get(shard) ?? [],
        certs: [...ac.rotationCerts],
        anchors
      }
    }
    const bond = this.bonds.snapshot()
    st.save({
      version: 1,
      shards,
      // shards with records but no anchor yet live nowhere else — persist
      // the write set itself or a restart orphans them (sell/describeShard
      // resolve shards() = anchors ∪ touched)
      touched: [...this.touched],
      promises: this.promises,
      flags: Object.fromEntries(this.flags),
      importedBundles: Object.fromEntries(this.importedBundles),
      bond,
      packStores: Object.fromEntries(this.packStores),
      proofStores: Object.fromEntries(this.proofStore.stores),
      packsStore: this.opts.packsStore?.snapshot() ?? this.savedPacksStore
    })
  }
  protected restoreState(): void {
    // the durable truth is the log itself — a state snapshot can lag the
    // last write (persistState only runs on chain-write ops), so written
    // shards rejoin from the store's own name set whether or not a
    // snapshot exists: a record that exists is a shard we own
    for (const shard of this.store.shardNames?.() ?? []) this.touched.add(shard)
    const s = this.opts.stateStore?.load()
    if (!s) return
    for (const [shard, p] of Object.entries(s.shards)) {
      for (const [txid, hex] of Object.entries(p.anchors)) {
        const tx = Transaction.fromAtomicBEEF(Array.from(Buffer.from(hex, 'hex')))
        if (tx.id('hex') !== txid) throw new Error(`restore: anchor ${txid} BEEF mismatch`)
        this.anchorTxs.set(txid, tx)
        this.txidShard.set(txid, shard)
      }
      const headTx = this.anchorTxs.get(p.headTxid)
      if (!headTx) throw new Error(`restore: head ${p.headTxid} of shard ${shard} missing`)
      this.anchorChain(shard).resume(headTx, { confirmed: p.confirmed, certs: p.certs })
      this.tails.set(shard, p.tails)
      this.cps.set(shard, p.checkpoints)
      this.touched.add(shard)
    }
    this.promises.push(...s.promises)
    // written-but-never-anchored shards — the shard store still holds their
    // records; rejoin the set so shards() can name them again
    for (const shard of s.touched ?? []) this.touched.add(shard)
    // state files written before the transaction graph kept parents here
    for (const hex of Object.values(s.txHex ?? {})) {
      try {
        this.txs.put(Transaction.fromHex(hex))
      } catch {
        /* unreadable entry */
      }
    }
    for (const [k, b] of Object.entries(s.importedBundles ?? {})) this.importedBundles.set(k, b)
    for (const [shard, f] of Object.entries(s.flags ?? {})) this.flags.set(shard, f)
    for (const [shard, r] of Object.entries(s.packStores ?? {})) this.packStores.set(shard, r)
    for (const [commit, r] of Object.entries(s.proofStores ?? {})) this.proofStore.stores.set(commit, r)
    this.savedPacksStore = s.packsStore
    if (s.bond) this.bonds.restore(s.bond)
  }
}
