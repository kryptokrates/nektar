// bonds.ts — §12 SLA bond (NektarCheckpointBond) lifecycle extracted from
// NektarMemory: deploy, status, seize, commit, close, the checkpoint-mirror
// commit and the deadline watchdog. The covenant spends the REAL parent — a
// synthetic sourceTransaction mismatches the outpoint txid in the preimage —
// so parents come through the caller's fetchTx (graph first, network last).
import type { PrivateKey, Transaction } from '@bsv/sdk'
import { CheckpointBond, type BondUtxo } from '@nektar/anchor'
import { toHex } from '@nektar/core'

export interface BondOpsDeps {
  chain: import('@nektar/chain').Broadcaster & import('@nektar/chain').ChainTracker
  txs: () => import('@nektar/chain').TxStore
  fetchTx: (txid: string) => Promise<Transaction | undefined>
  takeFunding: (satoshis: number) => Promise<import('@nektar/chain').FundingOut>
  fundKey: PrivateKey
  bondOpts: () => { ownerKey?: PrivateKey, sats?: number, horizonBlocks?: number, maxFee?: bigint, epochBlocks?: number } | undefined
  network?: string
  persist: () => void
}

/** serialized into state.json — the public constructor args + head utxo. */
export interface BondSnapshot { txid: string, vout: number, satoshis: number, script: string, operatorPkh: string, releaseHeight: string, maxFee: string, epochLen?: string }

export class BondOps {
  contract?: CheckpointBond
  head?: BondUtxo
  // serializes covenant spends — two in-flight ops (manual call + background
  // poll) racing the same parent produce one rejected double-spend
  busy = false
  constructor (private deps: BondOpsDeps) {}

  private key (): PrivateKey { return this.deps.bondOpts()?.ownerKey ?? this.deps.fundKey }

  get active (): boolean { return this.contract !== undefined && this.head !== undefined }

  /** deploy the checkpoint-bond covenant: `sats` locked, releasable after
   *  `releaseHeight` (default: current height + horizonBlocks). From then on
   *  every anchor commits its checkpoint digest — the on-chain history is
   *  unforkable and timestamped, so promise/SLA violations are provable by
   *  anyone replaying the commit chain. */
  async deploy (sats = this.deps.bondOpts()?.sats ?? 5_000, releaseHeight?: bigint, force = false): Promise<{ txid: string, releaseHeight: bigint }> {
    if (this.head && !force) throw new Error('bond already deployed')
    // force: drop the local bond state — the old bond stays on-chain and
    // remains seizeable under its own deadline; we just stop tracking it.
    if (force) { this.contract = undefined; this.head = undefined }
    const key = this.key()
    const release = releaseHeight ?? BigInt(await this.deps.chain.currentHeight()) + BigInt(this.deps.bondOpts()?.horizonBlocks ?? 1008)
    const pkh = key.toPublicKey().toHash()
    // mainnet's 100 sat/kB relay floor needs ~300-sat fees for a ~3KB covenant
    // tx — the covenant's own residual bound (fee+2 ≤ maxFee+1) must allow it.
    const mainnet = this.deps.network === 'main'
    // mainnet's 100 sat/kB relay floor needs ~400-500-sat fees for ~4KB covenant
    // spends — maxFee must leave headroom (residual = fee+2 ≤ maxFee+1).
    const cfg = { operatorPkh: typeof pkh === 'string' ? pkh : toHex(Uint8Array.from(pkh)), bondReleaseHeight: release, maxFee: this.deps.bondOpts()?.maxFee ?? (mainnet ? 1500n : 10n), epochLen: BigInt(this.deps.bondOpts()?.epochBlocks ?? 144) }
    this.contract = new CheckpointBond(cfg)
    const fund = await this.deps.takeFunding(sats + 600)
    const { tx, sats: bondSats, script } = this.contract.buildDeploy({ tx: fund.tx, vout: fund.vout, satoshis: fund.satoshis }, key, sats, mainnet ? 300n : 1n)
    await tx.sign() // P2PKH funding input — txid serialization needs the signed unlock
    const utxo = this.contract.deployUtxo(tx, bondSats, script)
    const br = await this.deps.chain.broadcast(tx)
    if (br.status !== 'success') { this.contract = undefined; throw new Error(`bond deploy broadcast failed: ${br.description ?? 'unknown'}`) }
    this.deps.txs().put(tx)
    this.head = utxo
    this.deps.persist()
    return { txid: tx.id('hex'), releaseHeight: release }
  }

  /** current on-chain bond state + head — absent when no bond was deployed.
   *  `deadline` is the seize threshold: if chain height reaches it without a
   *  commit, ANYONE may take the bond (§12 deadline-slashing). */
  status (): { n: number, lastRoot: string, headTxid: string, sats: number, releaseHeight: bigint, deadline: bigint, seized: boolean, cfg?: { operatorPkh: string, bondReleaseHeight: bigint, maxFee: bigint, epochLen: bigint }, utxo?: BondUtxo } | null {
    if (!this.contract || !this.head) return null
    const s = this.contract.stateOf(this.head.script)
    return { n: Number(s.n), lastRoot: s.lastRoot, headTxid: this.head.txid, sats: this.head.satoshis, releaseHeight: this.contract.params.bondReleaseHeight, deadline: s.deadline, seized: s.seized === 1n,
      // the observer's reconstruction kit — the bond's public constructor
      // args + its on-chain utxo. Anyone holding these can seize after the
      // deadline; nothing secret is revealed.
      cfg: { operatorPkh: this.contract.params.operatorPkh, bondReleaseHeight: this.contract.params.bondReleaseHeight, maxFee: this.contract.params.maxFee, epochLen: this.contract.params.epochLen },
      utxo: this.head }
  }

  /** seize an overdue bond — permissionless: valid only once the on-chain
   *  deadline has passed. With `bond: {cfg, utxo}` an OBSERVER seizes a bond
   *  it never deployed — cfg is the bond's public constructor args (posted in
   *  the bond's own manifest/attestation), utxo is read from the chain. No
   *  local bond state or operator key is needed. */
  async seize (payToScript?: string, bond?: { cfg: ConstructorParameters<typeof CheckpointBond>[0], utxo: BondUtxo }): Promise<{ txid: string } | null> {
    const contract = bond ? new CheckpointBond(bond.cfg) : this.contract
    const head = bond ? bond.utxo : this.head
    if (!contract || !head) return null
    if (this.busy) throw new Error('bond op in flight')
    this.busy = true
    try {
      const height = await this.deps.chain.currentHeight()
      const s = contract.stateOf(head.script)
      if (s.seized !== 0n) throw new Error('bond already seized')
      if (BigInt(height) < s.deadline) throw new Error(`bond not yet seizable — deadline ${s.deadline}, height ${height}`)
      const { buildP2PKHScript } = await import('@nektar/anchor')
      const pkh = contract.params.operatorPkh
      // the seize spends the REAL parent — a synthetic sourceTransaction
      // mismatches the outpoint txid in the covenant's preimage and produces
      // an invalid unlock on-chain. Fetch the actual bond deploy tx — on the
      // INSTANCE's own network (never a silent mainnet fallback).
      const parentTx = await this.parentTx(head.txid)
      // covenant bound: input − payoutSats (= fee + husk + change) ≤ maxFee+1
      // → fee ≤ maxFee−1. Use exactly maxFee−1 so the residual equals the bound.
      // fee: relay needs ~100sat/kB on main (~500 for a ~4KB covenant tx);
      // the covenant bounds residual = fee+2 ≤ maxFee+1. Pick the lower of the
      // two — 500 on main, maxFee−1 on test (where 1-sat relays pass anyway).
      const spendFee = this.deps.network === 'main' ? 500n : BigInt(contract.params.maxFee) - 1n
      const { tx } = await contract.buildSeize(head, payToScript ?? buildP2PKHScript(pkh), height, this.deps.chain, spendFee, parentTx)
      this.deps.txs().put(tx)
      if (!bond) { // only update local state for the bond WE deployed
        this.head = { txid: tx.id('hex'), vout: 1, satoshis: tx.outputs[1]!.satoshis!, script: tx.outputs[1]!.lockingScript.toHex() }
        this.deps.persist()
      }
      return { txid: tx.id('hex') }
    } finally { this.busy = false }
  }

  /** the covenant preimage needs the REAL parent tx — a synthetic
   *  sourceTransaction mismatches the outpoint txid and produces an invalid
   *  unlock on-chain. Fetch it on the instance's own network. */
  private async parentTx (txid: string): Promise<Transaction> {
    const tx = await this.deps.fetchTx(txid)
    if (tx === undefined) throw new Error(`parent tx ${txid} is not known to this instance`)
    return tx
  }

  /** commit a checkpoint root — keeps the bond alive past its current deadline. */
  async commit (root: string): Promise<{ txid: string, n: bigint } | null> {
    if (!this.contract || !this.head) return null
    if (this.busy) throw new Error('bond op in flight')
    this.busy = true
    try {
      const head = this.head
      const height = await this.deps.chain.currentHeight()
      const parentTx = await this.parentTx(head.txid)
      const commitFee = this.deps.network === 'main' ? 500n : BigInt(this.contract.params.maxFee) - 1n
      const { tx, state } = await this.contract.buildCommit(head, this.key(), root, height, this.deps.chain, commitFee, parentTx)
      this.deps.txs().put(tx)
      this.head = { txid: tx.id('hex'), vout: 0, satoshis: tx.outputs[0]!.satoshis!, script: tx.outputs[0]!.lockingScript.toHex() }
      this.deps.persist()
      return { txid: tx.id('hex'), n: state.n }
    } finally { this.busy = false }
  }

  /** close the bond after its release height — operator reclaims the funds. */
  async close (): Promise<{ txid: string } | null> {
    if (!this.contract || !this.head) return null
    const head = this.head
    const s = this.contract.stateOf(head.script)
    const height = await this.deps.chain.currentHeight()
    if (this.busy) throw new Error('bond op in flight')
    this.busy = true
    try {
      if (BigInt(height) < BigInt(this.contract.params.bondReleaseHeight)) throw new Error(`bond not yet releasable — release ${this.contract.params.bondReleaseHeight}, height ${height}`)
      const parentTx = await this.parentTx(head.txid)
      const closeFee = this.deps.network === 'main' ? 500n : BigInt(this.contract.params.maxFee) - 1n
      const { tx } = await this.contract.buildClose(head, this.key(), height, this.deps.chain, closeFee, parentTx)
      this.deps.txs().put(tx)
      this.contract = undefined
      this.head = undefined
      this.deps.persist()
      return { txid: tx.id('hex') }
    } finally { this.busy = false }
  }

  /** mirror an anchored checkpoint digest on-chain when idle — the covenant
   *  commits to the FULL signed checkpoint, so the history is unforkable and
   *  publicly replayable. Returns false when the commit could not be built. */
  async mirror (lastRoot: string): Promise<boolean> {
    if (!this.contract || !this.head || this.busy) return false
    this.busy = true
    try {
      const c = await this.contract.buildCommit(this.head, this.key(), lastRoot, await this.deps.chain.currentHeight(), this.deps.chain)
      this.head = { txid: c.tx.id('hex'), vout: 0, satoshis: c.tx.outputs[0]!.satoshis!, script: c.tx.outputs[0]!.lockingScript.toHex() }
      this.deps.persist()
      return true
    } catch { return false } finally { this.busy = false }
  }

  /** watchdog: if no anchor renewed the bond for half an epoch, commit the
   *  current root again — a lapsed deadline makes the bond seizable by
   *  anyone, so the operator must refresh before it passes. */
  async renewIfDue (): Promise<'committed' | 'skipped' | 'failed'> {
    if (!this.contract || !this.head) return 'skipped'
    const height = await this.deps.chain.currentHeight()
    const s = this.contract.stateOf(this.head.script)
    const margin = this.contract.params.epochLen / 2n > 0n ? this.contract.params.epochLen / 2n : 1n
    if (s.seized !== 0n || this.busy || BigInt(height) + margin < s.deadline) return 'skipped'
    this.busy = true
    try {
      const c = await this.contract.buildCommit(this.head, this.key(), s.lastRoot, height, this.deps.chain)
      this.head = { txid: c.tx.id('hex'), vout: 0, satoshis: c.tx.outputs[0]!.satoshis!, script: c.tx.outputs[0]!.lockingScript.toHex() }
      this.deps.persist()
      return 'committed'
    } catch { return 'failed' } finally { this.busy = false }
  }

  snapshot (): BondSnapshot | undefined {
    if (!this.contract || !this.head) return undefined
    return { txid: this.head.txid, vout: this.head.vout, satoshis: this.head.satoshis, script: this.head.script,
      operatorPkh: this.contract.params.operatorPkh, releaseHeight: this.contract.params.bondReleaseHeight.toString(), maxFee: this.contract.params.maxFee.toString(), epochLen: this.contract.params.epochLen.toString() }
  }

  restore (s: BondSnapshot): void {
    this.contract = new CheckpointBond({ operatorPkh: s.operatorPkh, bondReleaseHeight: BigInt(s.releaseHeight), maxFee: BigInt(s.maxFee), epochLen: BigInt(s.epochLen ?? this.deps.bondOpts()?.epochBlocks ?? 144) })
    this.head = { txid: s.txid, vout: s.vout, satoshis: s.satoshis, script: s.script }
  }
}
