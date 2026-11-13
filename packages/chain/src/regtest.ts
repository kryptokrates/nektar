// Real regtest adapters — Teranode propagation /tx for broadcast and
// Teranode JSON-RPC for headers. Compiled but only exercised on a live stack
// (NEKTAR_NETWORK=regtest). No tests depend on these.
import { Transaction, MerklePath } from '@bsv/sdk'
import { dsha256 } from '@nektar/core'
import { withRetry } from './retry.js'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker } from './interfaces.js'

export interface RegtestConfig {
  rpcUrl: string // http://localhost:9292/
  rpcUser: string
  rpcPass: string
  propagationUrl: string // http://localhost:8084/tx
  assetUrl: string // http://localhost:8090/api/v1
}

export class TeranodeBroadcaster implements Broadcaster {
  constructor(private url: string) {}
  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const res = await fetch(this.url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/octet-stream' },
      body: Buffer.from(tx.toHex(), 'hex'),
      signal: AbortSignal.timeout(30_000)
    })
    const text = await res.text()
    if (!res.ok) return { status: 'error', description: `${res.status}: ${text}` }
    return { status: 'success', txid: tx.id('hex') }
  }
}

export class TeranodeRPCTracker implements ChainTracker {
  private headers = new Map<number, BlockHeader>()
  private rpcId = 0
  constructor(private cfg: RegtestConfig) {}

  private async rpcOnce(method: string, ...params: unknown[]): Promise<any> {
    const res = await fetch(this.cfg.rpcUrl, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/json',
        Authorization: 'Basic ' + Buffer.from(`${this.cfg.rpcUser}:${this.cfg.rpcPass}`).toString('base64')
      },
      body: JSON.stringify({ jsonrpc: '1.0', id: ++this.rpcId, method, params }),
      signal: AbortSignal.timeout(15_000)
    })
    const j = await res.json()
    if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    return j.result
  }

  // Teranode regtest returns intermittent non-JSON bodies under block churn;
  // retry transport/parse failures (JSON-RPC error replies are real answers
  // and are thrown by rpcOnce immediately without retrying)
  private async rpc(method: string, ...params: unknown[]): Promise<any> {
    return await withRetry(() => this.rpcOnce(method, ...params), {
      attempts: 5,
      delay: (t) => 300 * (t + 1),
      retryIf: () => false,
      retryErr: (e) => !(e instanceof Error && e.message.startsWith(`rpc ${method}:`))
    })
  }

  async currentHeight(): Promise<number> {
    // getblockchaininfo.blocks lags seconds behind generate on Teranode;
    // bestblockhash+getblockheader reflect the real tip almost immediately.
    const best = await this.rpc('getbestblockhash').catch(() => undefined)
    if (best) {
      const h = await this.rpc('getblockheader', best).catch(() => undefined)
      if (typeof h?.height === 'number') return h.height
    }
    return (await this.rpc('getblockchaininfo')).blocks as number
  }

  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    const h = await this.headerAtHeight(height)
    return h?.merkleRoot === root
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const cached = this.headers.get(height)
    if (cached) return cached
    const hash = await this.rpc('getblockhash', height).catch(() => undefined)
    if (!hash) return undefined
    const b = await this.rpc('getblockheader', hash)
    const header: BlockHeader = {
      height,
      hash,
      prevHash: b.previousblockhash ?? '',
      merkleRoot: b.merkleroot,
      time: b.time,
      bits: typeof b.bits === 'string' ? parseInt(b.bits, 16) : b.bits,
      nonce: b.nonce
    }
    this.headers.set(height, header)
    return header
  }
}

export function regtestStackFromEnv(): { tracker: ChainTracker; broadcaster: Broadcaster } {
  const cfg: RegtestConfig = {
    rpcUrl: nektarEnv('NEKTAR_RPC_URL') ?? 'http://localhost:9292/',
    rpcUser: nektarEnv('NEKTAR_RPC_USER') ?? 'teranode',
    rpcPass: nektarEnv('NEKTAR_RPC_PASS') ?? '',
    propagationUrl: nektarEnv('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx',
    assetUrl: nektarEnv('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1'
  }
  return { tracker: new TeranodeRPCTracker(cfg), broadcaster: new TeranodeBroadcaster(cfg.propagationUrl) }
}

// --- asset API helpers (basic auth = rpc creds) ---
async function assetGet(cfg: RegtestConfig, path: string, binary = true): Promise<Response | null> {
  const res = await fetch(`${cfg.assetUrl}/${path}`, {
    headers: { Authorization: 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64') },
    signal: AbortSignal.timeout(20_000)
  })
  if (!res.ok) return null
  return res
}

/** Tx + (when sealed) BUMP lookup for the regtest runtime's chain.getTx —
 * `getrawtransaction` serves mempool and mined txs as hex; the asset API's
 * `/merkle_proof` answers the binary MerklePath once the tx is sealed in a
 * subtree (404 while pending). Without this, mem.fetchTx returns nothing on
 * regtest and bstack funding trees can never be marked proven.
 * (The asset `/tx` endpoint answers EXTENDED-format bytes, not a raw tx.) */
/** the proof-service read: one /merkle_proof request → parsed BUMP, versus
 *  the merklePathFor legacy-block scan (the fallback for when the asset
 *  endpoint can't answer). Served bytes are a claim — a caller that needs
 *  trust anchors computeRoot against its own ChainTracker. */
export async function regtestMerkleProof(cfg: RegtestConfig, txid: string, fetchImpl: typeof fetch = fetch): Promise<MerklePath | undefined> {
  const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
  const res = await fetchImpl(`${cfg.assetUrl}/merkle_proof/${txid}`, { headers: { Authorization: auth }, signal: AbortSignal.timeout(15_000) }).catch(
    () => null
  )
  if (res === null || !res.ok) return undefined
  try {
    return MerklePath.fromBinary([...new Uint8Array(await res.arrayBuffer())])
  } catch {
    /* unparseable proof → treat as unproven */
    return undefined
  }
}

export async function regtestTx(cfg: RegtestConfig, txid: string, fetchImpl: typeof fetch = fetch): Promise<Transaction | undefined> {
  const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
  try {
    const r = await fetchImpl(cfg.rpcUrl, {
      method: 'POST',
      headers: { 'content-type': 'application/json', Authorization: auth },
      body: JSON.stringify({ jsonrpc: '1.0', id: 1, method: 'getrawtransaction', params: [txid] }),
      signal: AbortSignal.timeout(15_000)
    })
    const j = (await r.json()) as { result?: unknown; error?: unknown }
    if (typeof j.result !== 'string' || j.result === '') return undefined
    const tx = Transaction.fromHex(j.result)
    const mp = await regtestMerkleProof(cfg, txid, fetchImpl)
    if (mp !== undefined) tx.merklePath = mp
    return tx
  } catch {
    return undefined
  }
}

// --- legacy-block parsing + MerklePathProvider --------------------------
function readVarint(b: Uint8Array, o: number): [number, number] {
  const dv = new DataView(b.buffer, b.byteOffset)
  if (b[o] < 0xfd) return [b[o], o + 1]
  if (b[o] === 0xfd) return [dv.getUint16(o + 1, true), o + 3]
  if (b[o] === 0xfe) return [dv.getUint32(o + 1, true), o + 5]
  return [Number(dv.getBigUint64(o + 1, true)), o + 9]
}
const sha256d = (b: Uint8Array): Uint8Array => dsha256(b)
/** legacy block: 80B header + 8B sizeInBytes + varint txcount + raw txs */
export function parseLegacyBlock(raw: Uint8Array): { txids: string[]; txs: Uint8Array[] } {
  const hexOf = (b: Uint8Array) => Buffer.from(b).toString('hex')
  let off = 88
  let n
  ;[n, off] = readVarint(raw, off)
  const txids: string[] = []
  const txs: Uint8Array[] = []
  for (let t = 0; t < n; t++) {
    const start = off
    off += 4
    let vin
    ;[vin, off] = readVarint(raw, off)
    for (let i = 0; i < vin; i++) {
      off += 36
      let sl
      ;[sl, off] = readVarint(raw, off)
      off += sl + 4
    }
    let vout
    ;[vout, off] = readVarint(raw, off)
    for (let i = 0; i < vout; i++) {
      off += 8
      let sl
      ;[sl, off] = readVarint(raw, off)
      off += sl
    }
    off += 4
    const tx = raw.subarray(start, off)
    txs.push(tx)
    txids.push(Buffer.from(sha256d(tx)).reverse().toString('hex'))
  }
  return { txids, txs }
}
/** standard merkle root (display order) over txid list */
export function merkleRootOf(txids: string[]): string {
  let level = txids.map((id) => Buffer.from(id, 'hex').reverse().toString('hex'))
  while (level.length > 1) {
    const next: string[] = []
    for (let i = 0; i < level.length; i += 2) {
      const a = level[i],
        b = level[i + 1] ?? level[i]
      next.push(Buffer.from(sha256d(Buffer.concat([Buffer.from(a, 'hex'), Buffer.from(b, 'hex')]))).toString('hex'))
    }
    level = next
  }
  return Buffer.from(level[0], 'hex').reverse().toString('hex')
}
export interface MerklePathResult {
  blockHeight: number
  path: Array<Array<{ offset: number; hash: string; txid?: boolean }>>
}
/** MerklePath for a txid by scanning the last N blocks via /block_legacy. */
export async function merklePathFor(cfg: RegtestConfig, txid: string, tracker: TeranodeRPCTracker, lookback = 10): Promise<MerklePathResult | null> {
  const tip = await tracker.currentHeight()
  const disp = (h: string) => Buffer.from(h, 'hex').reverse().toString('hex')
  for (let h = tip; h > tip - lookback && h > 0; h--) {
    const hdr = await tracker.headerAtHeight(h)
    if (!hdr) continue
    const res = await assetGet(cfg, `block_legacy/${hdr.hash}`)
    if (!res) continue
    // a mid-body socket close (keep-alive race/server restart) must not kill
    // the proof scan — skip the block like any other unreadable one
    const body = await res.arrayBuffer().catch(() => null)
    if (body === null) continue
    const { txids } = parseLegacyBlock(new Uint8Array(body))
    const idx = txids.indexOf(txid)
    if (idx < 0) continue
    // sanity: block's own merkle root must match the header
    if (merkleRootOf(txids) !== hdr.merkleRoot) throw new Error(`block ${h}: merkle root mismatch`)
    const leaves = txids.map((id) => Buffer.from(id, 'hex').reverse().toString('hex'))
    const path: MerklePathResult['path'] = []
    let level = leaves,
      pos = idx,
      first = true
    while (level.length > 1) {
      const sibling = pos % 2 === 0 ? pos + 1 : pos - 1
      const lvl: Array<{ offset: number; hash: string; txid?: boolean }> = []
      if (first) {
        lvl.push({ offset: pos, hash: disp(leaves[pos]), txid: true })
        first = false
      }
      lvl.push({ offset: sibling, hash: disp(sibling < level.length ? level[sibling] : level[pos]) })
      path.push(lvl)
      const next: string[] = []
      for (let i = 0; i < level.length; i += 2) {
        const a = level[i],
          b = level[i + 1] ?? level[i]
        next.push(Buffer.from(sha256d(Buffer.concat([Buffer.from(a, 'hex'), Buffer.from(b, 'hex')]))).toString('hex'))
      }
      level = next
      pos = Math.floor(pos / 2)
    }
    return { blockHeight: h, path }
  }
  return null
}

// --- ReserveFunder ---------------------------------------------------------
// Regtest halves the coinbase subsidy every 150 blocks — fresh mining can't
// fund tests forever. The reserve scans early blocks' coinbases + prior
// broadcast change outputs to our P2PKH, persists in .state/reserve.json,
// marks outputs spent on broadcast (and on UTXO_SPENT rejects), and returns
// change after each run.
import { existsSync, readFileSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'
import { writeFileAtomic, nektarEnv } from '@nektar/core'
import type { PrivateKey } from '@bsv/sdk'

interface ReserveEntry {
  txid: string
  vout: number
  satoshis: number
  txHex: string
  height?: number
}

export class ReserveFunder {
  keyPair: PrivateKey | undefined
  utxos: Array<{ tx: Transaction; vout: number; satoshis: number }> = []
  private entries: ReserveEntry[] = []
  private spent = new Set<string>()
  private file: string
  private pkhHex: string
  heights = new Map<string, number>()

  /** key custody for signing is NOT needed here — the reserve only tracks
   *  ownership by pubkey hash. Pass `{pkhHex}` under external-wallet custody. */
  constructor(key: PrivateKey | { pkhHex: string }, file = '.state.json') {
    this.keyPair = 'pkhHex' in key ? undefined : key
    this.file = file
    this.pkhHex = 'pkhHex' in key ? key.pkhHex : Buffer.from(key.toPublicKey().toHash()).toString('hex')
    this.load()
  }

  private op(txid: string, vout: number) {
    return `${txid}:${vout}`
  }

  private load(): void {
    try {
      const j = JSON.parse(readFileSync(this.file, 'utf8'))
      this.spent = new Set(j.spent ?? [])
      for (const e of (j.entries ?? []) as ReserveEntry[]) {
        try {
          const tx = Transaction.fromHex(e.txHex)
          this.entries.push(e) // spent marks keep their record — reconcile() can revive
          if (e.height) this.heights.set(e.txid, e.height)
          if (!this.spent.has(this.op(e.txid, e.vout))) this.utxos.push({ tx, vout: e.vout, satoshis: e.satoshis })
        } catch {
          /* skip unparseable */
        }
      }
    } catch {
      /* no state file yet */
    }
  }
  /** Re-read the reserve file fresh — another process may have populated or
   *  updated it while this one waited on the treasury lock (node --test runs
   *  files as parallel processes sharing .state). */
  reload(): void {
    this.utxos = []
    this.entries = []
    this.spent = new Set()
    this.heights = new Map()
    this.load()
  }
  private save(): void {
    mkdirSync(dirname(this.file), { recursive: true })
    writeFileAtomic(this.file, JSON.stringify({ spent: [...this.spent], entries: this.entries }, null, 1))
  }

  isSpent(txid: string, vout: number): boolean {
    return this.spent.has(this.op(txid, vout))
  }
  markSpent(txid: string, vout: number): void {
    this.spent.add(this.op(txid, vout))
    this.utxos = this.utxos.filter((u) => this.op(u.tx.id('hex'), u.vout) !== this.op(txid, vout))
    this.save()
  }

  /** Reconcile every recorded entry against a chain spend-status lookup:
   * 'spent' → mark spent; 'unspent' → revive (a stale mark from a dead
   * unbroadcast tx or an older blanket purge must not lose a live output);
   * 'unknown' → left as-is (pending txs may lag the UTXO store). */
  async reconcile(statusOf: (txid: string, vout: number) => Promise<'unspent' | 'spent' | 'unknown'>): Promise<{ revived: number; pruned: number }> {
    let revived = 0,
      pruned = 0
    for (const e of this.entries) {
      const status = await statusOf(e.txid, e.vout)
      const op = this.op(e.txid, e.vout)
      if (status === 'spent' && !this.spent.has(op)) {
        this.spent.add(op)
        this.utxos = this.utxos.filter((u) => this.op(u.tx.id('hex'), u.vout) !== op)
        pruned++
      } else if (status === 'unspent' && this.spent.has(op)) {
        this.spent.delete(op)
        this.utxos.push({ tx: Transaction.fromHex(e.txHex), vout: e.vout, satoshis: e.satoshis })
        revived++
      }
    }
    if (revived || pruned) this.save()
    return { revived, pruned }
  }

  /** Record an output we created (change) as a reserve candidate. */
  addOwned(tx: Transaction, vout: number): void {
    const hex = tx.toHex()
    const e: ReserveEntry = { txid: tx.id('hex'), vout, satoshis: tx.outputs[vout].satoshis ?? 0, txHex: hex }
    if (this.spent.has(this.op(e.txid, e.vout))) return
    if (this.utxos.some((x) => this.op(x.tx.id('hex'), x.vout) === this.op(e.txid, e.vout))) return
    this.entries.push(e)
    this.utxos.push({ tx, vout: e.vout, satoshis: e.satoshis })
    this.save()
  }

  /** Peek at the next unspent candidate — NON-destructive. runar calls
   * getUtxos multiple times per anchor; consuming on hand-out leaked entries
   * through failed broadcasts and drained the pool mid-burst. Entries are
   * marked spent only when their tx actually broadcasts (markSpent).
   * `spendCheck` (optional) is an O(1) chain lookup: when it reports a
   * candidate definitively spent it is pruned and the next is tried —
   * 'unknown' results are kept (pending txs may lag the UTXO store). */
  leased?: (txid: string, vout: number) => Promise<boolean>
  async takeReserve(
    spendCheck?: (txid: string, vout: number) => Promise<boolean>,
    minSats?: number
  ): Promise<{ tx: Transaction; vout: number; satoshis: number } | null> {
    while (true) {
      // smallest-sufficient: skip spent entries AND entries too small for the
      // spend — a first-fit dust pick produces negative-fee/insufficient txs.
      // With no explicit minimum the floor is 546 sats: a sub-dust P2PKH entry
      // (e.g. a 1-sat change output) can never fund a tx, so it is never picked.
      const floor = minSats ?? 546
      const candidates = []
      for (const x of this.utxos.filter((x) => !this.spent.has(this.op(x.tx.id('hex'), x.vout)) && x.satoshis >= floor)) {
        if (this.leased && (await this.leased(x.tx.id('hex'), x.vout))) continue
        candidates.push(x)
      }
      const u = candidates.sort((a, b) => a.satoshis - b.satoshis)[0]
      if (!u) return null
      if (spendCheck && (await spendCheck(u.tx.id('hex'), u.vout))) {
        this.markSpent(u.tx.id('hex'), u.vout)
        continue
      }
      return u
    }
  }

  /** Scan generated block coinbases for outputs to our key; add mature ones. */
  async scanCoinbases(cfg: RegtestConfig, rpc: (m: string, ...p: unknown[]) => Promise<any>, fromHeight: number, toHeight: number): Promise<number> {
    let added = 0
    const want = this.pkhHex
    for (let h = fromHeight; h <= toHeight; h++) {
      let hash: string
      try {
        hash = await rpc('getblockhash', h)
      } catch {
        break
      }
      const res = await assetGet(cfg, `block_legacy/${hash}`)
      if (!res) continue
      try {
        const { txs } = parseLegacyBlock(new Uint8Array(await res.arrayBuffer()))
        const cb = Transaction.fromHex(Buffer.from(txs[0]).toString('hex'))
        cb.outputs.forEach((o, v) => {
          const s = o.satoshis ?? 0
          if (o.lockingScript.toHex().includes(want) && s > 2000 && !this.spent.has(this.op(cb.id('hex'), v))) {
            this.entries.push({ txid: cb.id('hex'), vout: v, satoshis: s, txHex: cb.toHex(), height: h })
            this.heights.set(cb.id('hex'), h)
            this.utxos.push({ tx: cb, vout: v, satoshis: s })
            added++
          }
        })
      } catch {
        /* unparsable block */
      }
    }
    this.save()
    return added
  }
}

/** Fund a reserve on regtest: spend ONE candidate (persisted treasury first,
 *  else an early mature coinbase ≥ 0.1 coin to our key) into `outputs` fresh
 *  outputs + a new treasury output. A split the node already knows came from
 *  an earlier run (its outputs are likely spent) and is skipped. Returns the
 *  split tx or null. Bounded: ≤ maxCandidates coinbases are tried. */
export async function bootstrapReserve(
  cfg: RegtestConfig,
  key: PrivateKey,
  reserve: ReserveFunder,
  rpc: (m: string, ...p: unknown[]) => Promise<any>,
  opts: { treasuryFile: string; outputs?: number; maxCandidates?: number; maxScan?: number }
): Promise<Transaction | null> {
  const { P2PKH } = await import('@bsv/sdk')
  const addr = key.toPublicKey().toAddress([0x6f])
  const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
  const n = opts.outputs ?? 60
  const split = async (parent: Transaction, vout: number): Promise<Transaction | null> => {
    const sats = parent.outputs[vout].satoshis ?? 0
    if (sats < 1e6) return null
    const tx = new Transaction()
    tx.addInput({ sourceTransaction: parent, sourceOutputIndex: vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
    // entries sized to cover a full funder in one input — a 50k cap forced
    // 10-16 inputs + utxo probes per test setup
    const per = Math.min(1_000_000, Math.floor((sats - 2000) / (n + 1)))
    for (let k = 0; k < n; k++) tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: per })
    tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats - n * per - 1000 })
    await tx.sign()
    const known = await fetch(`${cfg.assetUrl}/tx/${tx.id('hex')}`, { headers: { Authorization: auth }, signal: AbortSignal.timeout(15_000) })
    if (known.ok) return null
    const r = await new TeranodeBroadcaster(cfg.propagationUrl).broadcast(tx)
    return r.status === 'success' ? tx : null
  }
  const accept = (tx: Transaction): Transaction => {
    for (let k = 0; k < n; k++) reserve.addOwned(tx, k)
    mkdirSync(dirname(opts.treasuryFile), { recursive: true })
    writeFileAtomic(opts.treasuryFile, JSON.stringify({ treasury: { txid: tx.id('hex'), vout: n, txHex: tx.toHex() } }))
    return tx
  }
  if (existsSync(opts.treasuryFile)) {
    const t = JSON.parse(readFileSync(opts.treasuryFile, 'utf8')) as { treasury?: { vout: number; txHex: string } }
    if (t.treasury) {
      const s = await split(Transaction.fromHex(t.treasury.txHex), t.treasury.vout)
      if (s) return accept(s)
    }
  }
  const want = Buffer.from(key.toPublicKey().toHash()).toString('hex')
  const tip = ((await rpc('getblockchaininfo')) as { blocks: number }).blocks
  let tried = 0
  // Scan mature heights BACKWARDS within a bounded window: the treasury key's
  // usable coinbases cluster near the tip on a live chain, while a from-1
  // walk on a long chain costs a block fetch per height even when the key
  // never mined. A key whose only coinbases lie deeper falls through to the
  // caller's generate-and-split path, which is always correct.
  const last = tip - 101
  const first = Math.max(1, last - (opts.maxScan ?? 800))
  for (let h = last; h >= first && tried < (opts.maxCandidates ?? 200); h--) {
    const hash = (await rpc('getblockhash', h)) as string
    const res = await fetch(`${cfg.assetUrl}/block_legacy/${hash}`, { signal: AbortSignal.timeout(20_000) }).catch(() => null)
    if (res === null || !res.ok) continue
    const body = await res.arrayBuffer().catch(() => null)
    if (body === null) continue
    const cb = Transaction.fromHex(Buffer.from(parseLegacyBlock(new Uint8Array(body)).txs[0]).toString('hex'))
    for (const [v, o] of cb.outputs.entries()) {
      if (!o.lockingScript.toHex().includes(want) || (o.satoshis ?? 0) < 1e7) continue
      tried++
      const s = await split(cb, v)
      if (s) return accept(s)
    }
  }
  return null
}

/** O(1) UTXO spend-status via the asset store — the endpoint answers for a
 *  known outpoint, so no scanning or address index is involved. Response is
 *  binary: unspent → all-zero record; spent → 0x01 followed by the spending
 *  txid (and block data). 404 → the store doesn't know the outpoint
 *  (pending tx lag or pruned record) — reported as 'unknown', never spent. */
export async function utxoStatus(cfg: RegtestConfig, txid: string, vout: number): Promise<'unspent' | 'spent' | 'unknown'> {
  const res = await assetGet(cfg, `utxo/${txid}?vout=${vout}`)
  if (!res) return 'unknown'
  const b = await res.arrayBuffer().catch(() => null)
  if (b === null) return 'unknown'
  return new Uint8Array(b)[0] === 1 ? 'spent' : 'unspent'
}

/** MerklePath for a txid at a known block height. */
export async function merklePathAtHeight(cfg: RegtestConfig, txid: string, tracker: TeranodeRPCTracker, height: number): Promise<MerklePathResult | null> {
  const hdr = await tracker.headerAtHeight(height)
  if (!hdr) return null
  const res = await assetGet(cfg, `block_legacy/${hdr.hash}`)
  if (!res) return null
  const body = await res.arrayBuffer().catch(() => null)
  if (body === null) return null
  const { txids } = parseLegacyBlock(new Uint8Array(body))
  const idx = txids.indexOf(txid)
  if (idx < 0) return null
  if (merkleRootOf(txids) !== hdr.merkleRoot) throw new Error(`block ${height}: merkle root mismatch`)
  const disp = (h: string) => Buffer.from(h, 'hex').reverse().toString('hex')
  const leaves = txids.map((id) => Buffer.from(id, 'hex').reverse().toString('hex'))
  const path: MerklePathResult['path'] = []
  let level = leaves,
    pos = idx,
    first = true
  while (level.length > 1) {
    const sibling = pos % 2 === 0 ? pos + 1 : pos - 1
    const lvl: Array<{ offset: number; hash: string; txid?: boolean }> = []
    if (first) {
      lvl.push({ offset: pos, hash: disp(leaves[pos]), txid: true })
      first = false
    }
    lvl.push({ offset: sibling, hash: disp(sibling < level.length ? level[sibling]! : level[pos]!) })
    path.push(lvl)
    const next: string[] = []
    for (let i = 0; i < level.length; i += 2) {
      const a = level[i]!,
        b = level[i + 1] ?? level[i]!
      next.push(Buffer.from(sha256d(Buffer.concat([Buffer.from(a, 'hex'), Buffer.from(b, 'hex')]))).toString('hex'))
    }
    level = next
    pos = Math.floor(pos / 2)
  }
  if (path.length === 0) path.push([{ offset: 0, hash: disp(leaves[0]), txid: true }])
  return { blockHeight: height, path }
}
