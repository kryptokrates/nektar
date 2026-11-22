// indexer.ts — the chain-index dialects behind every "is this tx on the
// network / what do the headers say / what pays this address" question.
// Bananablocks is the default index on both live nets; the WoC dialect
// remains for the BTC temporal source and explicit legacy configs.
//
// Truth rule (unchanged): the index is a HINT source. Proofs are verified
// against witnessed headers; a spend claim is only believed when the named
// spender's own bytes name the outpoint.
import { withRetry, pacer } from './retry.js'
import { BoundedMap, currentSignal } from '@nektar/core'
import type { BlockHeader } from './interfaces.js'

/** normalized tx view — the fields the money path reads off an index */
export interface IndexTx {
  blockHeight?: number
  confirmations?: number
  /** per-output spend claims — 'spentTxId' is the index's NAMED spender,
   *  evidence to verify, never truth */
  outputs?: Array<{ spent?: boolean; spentTxId?: string; spendScope?: string }>
}

/** normalized TSC proof + the height it was proven at */
export interface IndexProof {
  index: number
  txOrId: string
  targetType?: string
  target: string
  nodes: (string | null)[] | null
  blockHeight: number
}

export interface IndexUtxo {
  txid: string
  vout: number
  satoshis?: number
  blockHeight?: number
}

/** the semantic surface every index dialect must answer */
export interface ChainIndex {
  tip(): Promise<number | undefined>
  header(height: number): Promise<BlockHeader | undefined>
  txJson(txid: string): Promise<IndexTx | undefined>
  txHex(txid: string): Promise<string | undefined>
  proof(txid: string): Promise<IndexProof | undefined>
  utxos(address: string): Promise<IndexUtxo[] | undefined>
  /** indexes are read-only — the broadcaster is the only write path */
}

export type IndexerKind = 'banana' | 'woc'

export interface IndexOpts {
  url: string
  apiKey?: string
  fetchImpl?: typeof fetch
  /** 'banana' | 'woc' — guessed from the URL when unset */
  kind?: IndexerKind
  /** hint-cache TTL; undefined → default, 0 → disable (raw reads) */
  cacheTtlMs?: number
}

export const guessIndexKind = (url: string): IndexerKind => (/whatsonchain/i.test(url) ? 'woc' : 'banana')

/** the index picked for a url/kind pair — one client per dialect, behind the
 *  shared hint cache unless cacheTtlMs:0 asks for raw reads */
export function indexFor(o: IndexOpts): ChainIndex {
  const kind = o.kind ?? guessIndexKind(o.url)
  const base = kind === 'woc' ? new WocIndex(o) : new BananaIndex(o)
  return o.cacheTtlMs === 0 ? base : new CachingIndex(base, o.cacheTtlMs ?? 5_000)
}

/** ordered fallback over independent index deployments — a rate-limited or
 *  down primary (bananablocks 429s under suite load) must not read as "tip
 *  unreachable". undefined counts as a miss worth asking the next index
 *  about: a lagging primary genuinely may not have a tx the secondary has.
 *  If every index throws, the LAST error propagates — callers still get an
 *  honest "unreachable" instead of a silent undefined when all sources die. */
export class FailoverIndex implements ChainIndex {
  constructor(private readonly inner: ChainIndex[]) {}
  private async ask<T>(f: (i: ChainIndex) => Promise<T | undefined>): Promise<T | undefined> {
    let lastErr: unknown
    for (const i of this.inner) {
      try {
        const v = await f(i)
        if (v !== undefined) return v
      } catch (e) {
        lastErr = e
      }
    }
    if (lastErr !== undefined) throw lastErr
    return undefined
  }
  tip(): Promise<number | undefined> {
    return this.ask((i) => i.tip())
  }
  header(height: number): Promise<BlockHeader | undefined> {
    return this.ask((i) => i.header(height))
  }
  txJson(txid: string): Promise<IndexTx | undefined> {
    return this.ask((i) => i.txJson(txid))
  }
  txHex(txid: string): Promise<string | undefined> {
    return this.ask((i) => i.txHex(txid))
  }
  proof(txid: string): Promise<IndexProof | undefined> {
    return this.ask((i) => i.proof(txid))
  }
  utxos(address: string): Promise<IndexUtxo[] | undefined> {
    return this.ask((i) => i.utxos(address))
  }
}

/** multi-url variant — one raw dialect per url (each dialect keeps its own
 *  pacing), failover ordered, ONE hint cache on top. Single url ≡ indexFor. */
export function indexFailover(urls: string[], o?: Omit<IndexOpts, 'url'>): ChainIndex {
  const raw = urls.map((url) => {
    const kind = o?.kind ?? guessIndexKind(url)
    return kind === 'woc' ? new WocIndex({ ...o, url }) : new BananaIndex({ ...o, url })
  })
  const base: ChainIndex = raw.length === 1 ? raw[0] : new FailoverIndex(raw)
  return o?.cacheTtlMs === 0 ? base : new CachingIndex(base, o?.cacheTtlMs ?? 5_000)
}

// --- hint cache ---------------------------------------------------------------
/** read-through memoization over any dialect. An upkeep pass asks the same
 *  questions (tip, the same txids' hex/json/proof, an address' utxos) from
 *  many call sites at once — without a shared promise each ask is a paced
 *  upstream RTT, which is what makes a pass cost ~30s on a slow index.
 *
 *  Honesty: everything cached here is a HINT (see the file's truth rule) —
 *  a miss expires on the TTL because the tx may arrive later, and served
 *  proofs expire too because a reorg can kill a BUMP. Only txHex hits are
 *  cached for the process lifetime: bytes either hash to the txid or they
 *  were never served. Verdicts never come from this layer. */
export class CachingIndex implements ChainIndex {
  private hex = new BoundedMap<string, Promise<string | undefined>>(8192)
  private timed = new BoundedMap<string, { at: number; p: Promise<unknown> }>(4096)
  constructor(
    private readonly inner: ChainIndex,
    private readonly ttlMs = 5_000
  ) {}
  private within<T>(key: string, f: () => Promise<T>): Promise<T> {
    const hit = this.timed.get(key)
    if (hit !== undefined && Date.now() - hit.at < this.ttlMs) return hit.p as Promise<T>
    const p = f().catch((e) => {
      this.timed.delete(key) // a failed call must not poison the window
      throw e
    })
    this.timed.set(key, { at: Date.now(), p })
    return p
  }
  tip(): Promise<number | undefined> {
    return this.within('tip', () => this.inner.tip())
  }
  /** deliberately NOT cached — header reads are the divergence-detection
   *  substrate: refreshHeader + the admit linkage walk re-ask the source so
   *  a stale answer can't masquerade as a chain split (mainnet.ts documents
   *  "never cached" at this seam). Same-height repeats are already served by
   *  the tracker's reorg-aware cache one layer up. */
  header(height: number): Promise<BlockHeader | undefined> {
    return this.inner.header(height)
  }
  txJson(txid: string): Promise<IndexTx | undefined> {
    return this.within(`j:${txid}`, () => this.inner.txJson(txid))
  }
  txHex(txid: string): Promise<string | undefined> {
    const hit = this.hex.get(txid)
    if (hit !== undefined) return hit
    // hits pin forever (immutable bytes); misses ride the short TTL so an
    // arriving tx is seen on the next window
    const live = this.within(`x:${txid}`, () => this.inner.txHex(txid))
    const pinned = live.then((h) => (h === undefined ? undefined : h))
    void pinned.then(
      (h) => {
        if (h !== undefined) this.hex.set(txid, Promise.resolve(h))
      },
      () => undefined
    )
    return pinned
  }
  proof(txid: string): Promise<IndexProof | undefined> {
    return this.within(`p:${txid}`, () => this.inner.proof(txid))
  }
  utxos(address: string): Promise<IndexUtxo[] | undefined> {
    return this.within(`u:${address}`, () => this.inner.utxos(address))
  }
}

// --- shared plumbing ----------------------------------------------------------
const mkreq = (o: IndexOpts) => {
  const f = o.fetchImpl ?? fetch
  return async (path: string, init: RequestInit = {}, ms = 15_000): Promise<Response> => {
    const ctl = new AbortController()
    const t = setTimeout(() => ctl.abort(), ms)
    try {
      const headers: Record<string, string> = { ...(init.headers as Record<string, string> | undefined) }
      if (o.apiKey !== undefined) headers['woc-api-key'] = o.apiKey // harmless on bananablocks
      const ambient = currentSignal()
      return await f(`${o.url}${path}`, { ...init, headers, signal: ambient !== undefined ? AbortSignal.any([ctl.signal, ambient]) : ctl.signal })
    } finally {
      clearTimeout(t)
    }
  }
}

// --- bananablocks -------------------------------------------------------------
/** https://{test.}bananablocks.com/api/v1 — the default BSV index. */
export class BananaIndex implements ChainIndex {
  private req: ReturnType<typeof mkreq>
  private pace = pacer(200)
  constructor(o: IndexOpts) {
    this.req = mkreq(o)
  }
  private async get<T>(path: string): Promise<T | undefined> {
    const r = await withRetry(
      async () => {
        await this.pace()
        return await this.req(path)
      },
      { attempts: 4, delay: (i) => 1000 * (i + 1), retryIf: (x) => x.status === 429 || x.status >= 500 }
    ).catch(() => undefined)
    if (r === undefined || r.status === 404) return undefined
    if (!r.ok) throw new Error(`bananablocks ${path}: HTTP ${r.status}`)
    return (await r.json()) as T
  }
  async tip(): Promise<number | undefined> {
    return (await this.get<{ height?: number }>('/block/tip').catch(() => undefined))?.height
  }
  async header(height: number): Promise<BlockHeader | undefined> {
    const b = await this.get<{
      hash?: string
      prev_hash?: string
      merkle_root?: string
      time?: number
      bits?: string | number
      nonce?: number
      version?: number
    }>(`/block/${height}`)
    if (b?.hash === undefined) return undefined
    // a just-sealed tip is served with prev_hash:"" until the link is
    // backfilled — an unusable parent must miss, never poison the linkage
    // walk (an absent/empty prev_hash is "not yet indexed", not "reorg")
    if (height > 0 && !/^[0-9a-f]{64}$/i.test(b.prev_hash ?? '')) return undefined
    return {
      height,
      hash: b.hash,
      prevHash: b.prev_hash ?? '',
      merkleRoot: b.merkle_root ?? '',
      time: b.time ?? 0,
      bits: typeof b.bits === 'string' ? parseInt(b.bits, 16) || 0 : (b.bits ?? 0),
      nonce: b.nonce ?? 0,
      version: b.version ?? 0
    }
  }
  async txJson(txid: string): Promise<IndexTx | undefined> {
    const j = await this.get<{
      block_height?: number
      confirmations?: number
      outputs?: Array<{ spent?: boolean; spent_txid?: string; spend_scope?: string }>
    }>(`/tx/${txid}`)
    if (j === undefined) return undefined
    return {
      blockHeight: j.block_height,
      confirmations: j.confirmations,
      outputs: j.outputs?.map((o) => ({ spent: o.spent, spentTxId: o.spent_txid, spendScope: o.spend_scope }))
    }
  }
  async txHex(txid: string): Promise<string | undefined> {
    const r = await withRetry(
      async () => {
        await this.pace()
        return await this.req(`/tx/${txid}/hex`)
      },
      { attempts: 4, delay: (i) => 1000 * (i + 1), retryIf: (x) => x.status === 429 || x.status >= 500 }
    ).catch(() => undefined)
    if (r === undefined || !r.ok) return undefined
    return (await r.text()).trim()
  }
  async proof(txid: string): Promise<IndexProof | undefined> {
    const tx = await this.get<{ block_height?: number }>(`/tx/${txid}`)
    const height = tx?.block_height
    if (height === undefined || height <= 0) return undefined
    const p = await this.get<{ index?: number; txOrId?: string; targetType?: string; target?: string; nodes?: (string | null)[] | null }>(`/tx/${txid}/proof`)
    if (p?.index === undefined || p.target === undefined) return undefined
    return { index: p.index, txOrId: p.txOrId ?? txid, targetType: p.targetType, target: p.target, nodes: p.nodes ?? null, blockHeight: height }
  }
  /** first pages of unspents — {items, hasMore}; offset+limit paginate */
  async utxos(address: string): Promise<IndexUtxo[] | undefined> {
    const out: IndexUtxo[] = []
    for (let page = 0; page < 10; page++) {
      const j = await this.get<{ items?: Array<{ txid?: string; vout?: number; value?: number; block_height?: number }>; hasMore?: boolean }>(
        `/address/${address}/utxos?limit=200&offset=${page * 200}`
      )
      if (j === undefined) return page === 0 ? undefined : out
      for (const u of j.items ?? [])
        if (u.txid !== undefined && u.vout !== undefined) out.push({ txid: u.txid, vout: u.vout, satoshis: u.value, blockHeight: u.block_height })
      if (j.hasMore !== true) return out
    }
    return out
  }
}

// --- whatsonchain -------------------------------------------------------------
/** legacy dialect — kept for the BTC temporal source and explicit
 *  NEKTAR_INDEXER_URL=https://api.whatsonchain.com/... configs. */
export class WocIndex implements ChainIndex {
  private req: ReturnType<typeof mkreq>
  private pace = pacer(350) // ~3 req/s unauthenticated
  constructor(o: IndexOpts) {
    this.req = mkreq(o)
  }
  private async get<T>(path: string): Promise<T | undefined> {
    const r = await withRetry(() => this.pace().then(() => this.req(path)), {
      attempts: 4,
      delay: (i) => 1200 * (i + 1),
      retryIf: (x) => x.status === 429
    }).catch(() => undefined)
    if (r === undefined || r.status === 404) return undefined
    if (!r.ok) throw new Error(`woc ${path}: HTTP ${r.status}`)
    return (await r.json()) as T
  }
  async tip(): Promise<number | undefined> {
    return (await this.get<{ blocks?: number }>('/chain/info'))?.blocks
  }
  async header(height: number): Promise<BlockHeader | undefined> {
    const b = await this.get<{
      hash?: string
      previousblockhash?: string
      merkleroot?: string
      time?: number
      bits?: string | number
      nonce?: number
      version?: number
    }>(`/block/height/${height}`)
    if (b?.hash === undefined) return undefined
    return {
      height,
      hash: b.hash,
      prevHash: b.previousblockhash ?? '',
      merkleRoot: b.merkleroot ?? '',
      time: b.time ?? 0,
      bits: typeof b.bits === 'string' ? parseInt(b.bits, 16) || 0 : (b.bits ?? 0),
      nonce: b.nonce ?? 0,
      version: b.version ?? 0
    }
  }
  async txJson(txid: string): Promise<IndexTx | undefined> {
    const j = await this.get<{ blockheight?: number; confirmations?: number; vout?: Array<{ spent?: boolean; spentTxId?: string; spender?: string }> }>(
      `/tx/${txid}`
    )
    if (j === undefined) return undefined
    return {
      blockHeight: typeof j.blockheight === 'number' ? j.blockheight : undefined,
      confirmations: j.confirmations,
      outputs: j.vout?.map((o) => ({ spent: o.spent, spentTxId: o.spentTxId ?? o.spender }))
    }
  }
  async txHex(txid: string): Promise<string | undefined> {
    // paced + retried like get() — the only WoC read that bypassed both,
    // which is why discoverUtxos once carried its own pacing wrapper
    const r = await withRetry(
      async () => {
        await this.pace()
        return await this.req(`/tx/${txid}/hex`)
      },
      { attempts: 4, delay: (i) => 1200 * (i + 1), retryIf: (x) => x.status === 429 }
    ).catch(() => undefined)
    if (r === undefined || !r.ok) return undefined
    return (await r.text()).trim()
  }
  async proof(txid: string): Promise<IndexProof | undefined> {
    const tx = await this.get<{ blockheight?: number }>(`/tx/hash/${txid}`)
    const height = tx?.blockheight
    if (height === undefined || height <= 0) return undefined
    const arr = await this.get<Array<{ index?: number; txOrId?: string; target?: string; nodes?: (string | null)[] | null }>>(`/tx/${txid}/proof/tsc`)
    const p = arr?.[0]
    if (p?.index === undefined || p.target === undefined) return undefined
    return { index: p.index, txOrId: p.txOrId ?? txid, target: p.target, nodes: p.nodes ?? null, blockHeight: height }
  }
  async utxos(address: string): Promise<IndexUtxo[] | undefined> {
    const list = await this.get<Array<{ tx_hash?: string; tx_pos?: number; value?: number }>>(`/address/${address}/unspent`)
    return list?.flatMap((u) => (u.tx_hash !== undefined && u.tx_pos !== undefined ? [{ txid: u.tx_hash, vout: u.tx_pos, satoshis: u.value }] : []))
  }
}
