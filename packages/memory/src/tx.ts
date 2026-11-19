// MemTx — the transaction surface: TxOps delegates, broadcast, and the
// chain helpers (height/tracker/tipRef) covenant and attest paths call.
import type { Transaction } from '@bsv/sdk'
import type { BroadcastResult, Ingested, TxFacts } from '@nektar/chain'
import { MemKeys } from './keys.js'

export abstract class MemTx extends MemKeys {
  async fetchTx(txid: string): Promise<Transaction | undefined> {
    return await this.txOps.fetchTx(txid)
  }
  /** keep a transaction (and the ancestry it carries) in the instance's graph */
  rememberTx(tx: Transaction): void {
    this.txOps.rememberTx(tx)
  }
  /** hex to hand a counterparty: BEEF when the ancestry is proven, else EF, else raw */
  txWire(txid: string, maxBytes?: number): string | undefined {
    return this.txOps.txWire(txid, maxBytes)
  }
  /** read a counterparty's wire transaction and run SPV against our own headers */
  async readTxWire(hex: string): Promise<Ingested> {
    return await this.txOps.readTxWire(hex)
  }
  /** subscribe to verified tx proofs — fed by verified broadcaster callbacks
   *  (http layer checks the BUMP first) and by chain 'mined' status events */
  onTxProof(cb: (txid: string, mp: import('@bsv/sdk').MerklePath) => void): void {
    this.txOps.onTxProof(cb)
  }
  /** a caller-verified or chain-observed merkle proof: attach to its anchor,
   *  then notify subscribers */
  deliverTxProof(txid: string, mp: import('@bsv/sdk').MerklePath): void {
    this.txOps.deliverTxProof(txid, mp)
  }

  /** subscribe to rejection/abandonment wakes — verified chain events,
   *  first-party refusals, abandon-horizon verdicts, and pushed status
   *  hints all fire it; a wake is a poke to re-ask txFacts, not a verdict */
  onTxRejected(cb: (txid: string) => void): void {
    this.txOps.onTxRejected(cb)
  }
  /** record a FIRST-PARTY verdict — verified chain 'tx' events and
   *  definitive broadcast refusals only; pushed status strings go through
   *  hintTxRejected (a hint carries no proof object) */
  deliverTxRejected(txid: string): void {
    this.txOps.deliverTxRejected(txid)
  }
  /** a pushed rejection STATUS (broadcaster webhook / SSE): wake-only —
   *  pokes subscribers into re-asking txFacts, writes no durable verdict */
  hintTxRejected(txid: string): void {
    this.txOps.hintTxRejected(txid)
  }

  /** the chain observer's abandon horizon fired for a txid — the terminal
   *  verdict for a mint that was resubmitted but never mined (regtest and
   *  mainnet runtimes emit it on `events`) */
  onTxAbandoned(cb: (txid: string) => void): void {
    this.events.on('txAbandoned', ({ txid }: { txid: string }) => cb(txid))
  }

  /** a header reorg orphaned everything proven at ≥ fromHeight — emitted
   *  on `events` by runtimes whose tracker detects a fork */
  onReorg(cb: (fromHeight: number) => void): void {
    this.events.on('reorg', ({ from }: { from: number }) => cb(from))
  }

  /** the one answer about a transaction — bytes, proof, depth, verdict with
   *  its source, and the outside status word as a separate hint */
  async txFacts(txid: string): Promise<TxFacts> {
    return await this.txOps.txFacts(txid)
  }

  /** any tx this instance broadcast and remembers — anchor or attestation
   *  (model-verifier deploy/spend land in attestationTxs too) */
  knownTx(txid: string) {
    return this.txOps.knownTx(txid)
  }

  /** broadcast a fully-signed tx through the instance's broadcaster (SpendGuard stays in front) — covenant host tools only */
  async broadcastTx(tx: Transaction): Promise<BroadcastResult> {
    const r = await this.chain.broadcast(tx)
    if (r.status === 'success') this.txs.put(tx)
    return r
  }
  /** a tx our own guard refused still aired — the counterparty re-broadcast
   *  the signed bytes it was handed. Retro-counts the spend in the guard's
   *  day ledger; the wallet-ledger revive calls it on the counterparty-air
   *  path so the cap measures real outflow, not just our submits. */
  recordAired(tx: Transaction): void {
    this.opts.recordAired?.(tx)
  }

  /** current chain height for locktime-gated covenant calls */
  async chainHeight(): Promise<number> {
    return this.chain.tipHeight ? await this.chain.tipHeight() : await this.chain.currentHeight()
  }
  /** the instance's chain tracker in @bsv/sdk ChainTracker shape (bstack checkCarrier/verify) */
  sdkTracker(): import('@bsv/sdk').ChainTracker {
    return this.chain
  }
  /** {hash,height} pair near the current tip — walks back a few blocks because
   *  the header index lags the tip; the chain-bound identity an artifact claims */
  async tipRef(): Promise<{ tip: string; tipHeight: number } | undefined> {
    const h0 = await this.chainHeight()
    for (let h = h0; h > h0 - 3 && h > 0; h--) {
      const hdr = await this.chain.headerAtHeight(h).catch(() => undefined)
      if (hdr) return { tip: hdr.hash, tipHeight: h }
    }
    return undefined
  }
}
