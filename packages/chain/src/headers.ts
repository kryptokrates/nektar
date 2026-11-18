// headers.ts — self-authenticating block headers: the 80-byte serialization,
// the proof-of-work check that makes a served header self-verifying, and the
// witnessed tracker that makes two independent sources attest the same chain.
import { Hash } from '@bsv/sdk'
import type { BlockHeader, ChainTracker } from './interfaces.js'

const HEX64 = /^[0-9a-fA-F]{64}$/

/** the consensus serialization — 80 bytes: version LE32, prevHash and
 *  merkleRoot in internal (reversed) order, then time/bits/nonce LE32.
 *  `undefined` when the header lacks what serialization needs (no version —
 *  old sources didn't carry it — or malformed hashes). */
export function serializeHeader(h: BlockHeader): Uint8Array | undefined {
  if (h.version === undefined || !HEX64.test(h.prevHash) || !HEX64.test(h.merkleRoot)) return undefined
  const b = new Uint8Array(80)
  const v = new DataView(b.buffer)
  v.setUint32(0, h.version, true)
  const rev = (hex: string) => Uint8Array.from(Buffer.from(hex, 'hex')).reverse()
  b.set(rev(h.prevHash), 4)
  b.set(rev(h.merkleRoot), 36)
  v.setUint32(68, h.time, true)
  v.setUint32(72, h.bits, true)
  v.setUint32(76, h.nonce, true)
  return b
}

/** compact-target (nBits) expansion — the 256-bit target a valid header hash
 *  must not exceed. `undefined` for negative/zero/overflowing mantissas. */
const expandBits = (bits: number): bigint | undefined => {
  const exp = bits >>> 24
  const mant = bits & 0x007fffff
  if ((bits & 0x00800000) !== 0 || mant === 0 || exp === 0) return undefined
  const target = exp >= 3 ? BigInt(mant) << BigInt(8 * (exp - 3)) : BigInt(mant) >> BigInt(8 * (3 - exp))
  if (target <= 0n || target > (1n << 256n) - 1n) return undefined
  return target
}

/** a served header proves itself: hash256 of its serialization must equal its
 *  claimed hash AND satisfy its own declared difficulty. What this does NOT
 *  prove: that the header sits on the most-work chain — a minority fork's
 *  headers pass PoW equally. That is what a second-source witness is for. */
export function headerPowValid(h: BlockHeader): boolean {
  const raw = serializeHeader(h)
  if (raw === undefined) return false
  const hash = Buffer.from(Hash.hash256([...raw]))
    .reverse()
    .toString('hex')
  if (hash.toLowerCase() !== h.hash.toLowerCase()) return false
  const target = expandBits(h.bits)
  return target !== undefined && BigInt('0x' + hash) <= target
}

/** parent-child linkage — the only chain rule a single cached pair can check */
export function linksTo(child: BlockHeader, parent: BlockHeader): boolean {
  return child.prevHash === parent.hash && child.height === parent.height + 1
}

/** the per-tracker header cache + reorg machinery — ONE implementation so
 *  every header source shares the same rules: linkage-gated admission, and
 *  on a mismatch a bounded walk down to the common ancestor before
 *  subscribers hear of it. */
export class HeaderCache {
  private headers = new Map<number, BlockHeader>()
  private reorgCbs: Array<(removed: BlockHeader[], newTip: BlockHeader) => void> = []

  get(height: number): BlockHeader | undefined {
    return this.headers.get(height)
  }
  has(height: number): boolean {
    return this.headers.has(height)
  }
  set(h: BlockHeader): void {
    this.headers.set(h.height, h)
  }
  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.reorgCbs.push(cb)
  }

  /** linkage gate — true when the fresh header sits cleanly beside what the
   *  cache already holds; a mismatch runs the reorg walk and the caller must
   *  NOT cache or silently serve the newcomer */
  async admit(h: BlockHeader, refetch: (height: number) => Promise<BlockHeader | undefined>): Promise<boolean> {
    const parent = this.headers.get(h.height - 1)
    const child = this.headers.get(h.height + 1)
    if ((parent !== undefined && !linksTo(h, parent)) || (child !== undefined && !linksTo(child, h))) {
      await this.reorgAt(h, refetch)
      return false
    }
    return true
  }

  /** walk DOWN from just under the mismatching header: each cached header is
   *  re-fetched, and evicted while the fresh answer differs — the first
   *  agreement is the common ancestor and stays cached. Everything above the
   *  mismatch height is descendant-stale. `fresh` is the highest header we
   *  could fetch, reported to subscribers as newTip. */
  async reorgAt(fresh: BlockHeader, refetch: (height: number) => Promise<BlockHeader | undefined>): Promise<void> {
    const removed: BlockHeader[] = []
    for (let h = fresh.height - 1, n = 0; n < 12; h--, n++) {
      const cur = this.headers.get(h)
      if (cur === undefined) break // cache gap — can't verify deeper
      const ref = await refetch(h)
      if (ref === undefined) break // couldn't confirm deeper
      if (ref.hash === cur.hash) break // common ancestor — keep it
      this.headers.delete(h)
      removed.push(cur)
    }
    for (const [h, cur] of this.headers) {
      if (h >= fresh.height) {
        this.headers.delete(h)
        removed.push(cur)
      }
    }
    if (removed.length === 0) return
    removed.sort((a, b) => b.height - a.height)
    for (const cb of this.reorgCbs)
      try {
        cb(removed, fresh)
      } catch {
        /* listener errors are theirs */
      }
  }
}

interface WitMark {
  at: number
  state: 'ok' | 'mismatch' | 'uncorroborated'
  hash: string
}

/** two independent header sources, required to agree before a root is served.
 *  PoW per header proves the answer is real work, NOT that it is on the
 *  most-work chain — the witness is what rules out a sourced-but-minority
 *  (or simply different) fork. Fail closed on disagreement: a height the two
 *  sources split on serves no root until they agree. Non-strict mode degrades
 *  to the reachable source and marks the height uncorroborated — re-asked at
 *  most once per `retryMs` until the witness lands an agreement. */
export class WitnessedTracker implements ChainTracker {
  private marks = new Map<number, WitMark>()
  private asks = new Map<number, { at: number; p: Promise<BlockHeader | undefined> }>()

  constructor(
    private primary: ChainTracker,
    private witness: ChainTracker,
    private opts: {
      strict?: boolean
      witnessTimeoutMs?: number
      retryMs?: number
      onDisagree?: (height: number, primaryHash: string, witnessHash: string) => void
    } = {}
  ) {}

  private get retryMs(): number {
    return this.opts.retryMs ?? 30_000
  }

  /** one witness request per height per retry window — a timed-out ask keeps
   *  running in the background and later reads pick up its landed result */
  private askWitness(height: number): Promise<BlockHeader | undefined> {
    const cur = this.asks.get(height)
    if (cur !== undefined && Date.now() - cur.at < this.retryMs) return cur.p
    const p = this.witness.headerAtHeight(height).catch(() => undefined)
    this.asks.set(height, { at: Date.now(), p })
    return p
  }

  /** bounded wait on the witness — a slow witness never stalls a read */
  private async bounded(p: Promise<BlockHeader | undefined>): Promise<BlockHeader | undefined> {
    const ms = this.opts.witnessTimeoutMs ?? 4_000
    return await new Promise((resolve) => {
      const t = setTimeout(() => resolve(undefined), ms)
      void p.then((h) => {
        clearTimeout(t)
        resolve(h)
      })
    })
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const p = await this.primary.headerAtHeight(height).catch(() => undefined)
    // a corroborated height stays served without re-asking — but only for
    // the SAME hash: a primary answer that changed (reorg) re-verifies
    const m = this.marks.get(height)
    if (p !== undefined && m?.state === 'ok' && m.hash === p.hash) return p
    // inside the retry window a known split fails closed immediately — the
    // refresh path below re-runs only once per retryMs so a persistent fork
    // can't turn every read into two forced re-fetches
    if (m?.state === 'mismatch' && Date.now() - m.at <= this.retryMs) return undefined
    const w = await this.bounded(this.askWitness(height))
    if (p !== undefined && w !== undefined) {
      if (p.hash === w.hash) {
        this.marks.set(height, { at: Date.now(), state: 'ok', hash: p.hash })
        return p
      }
      // a hash split can be a STALE CACHE on either side, not a chain split —
      // force both to re-ask their source; only a disagreement that survives
      // the refresh fails closed
      const [p2, w2] = await Promise.all([
        this.primary.refreshHeader?.(height).catch(() => undefined),
        this.witness.refreshHeader?.(height).catch(() => undefined)
      ])
      const p3 = p2 ?? p
      const w3 = w2 ?? w
      if (p3.hash === w3.hash) {
        this.marks.set(height, { at: Date.now(), state: 'ok', hash: p3.hash })
        return p3
      }
      // fail closed: disagreement at a height serves no root until it resolves
      this.marks.set(height, { at: Date.now(), state: 'mismatch', hash: `${p3.hash}|${w3.hash}` })
      this.opts.onDisagree?.(height, p3.hash, w3.hash)
      return undefined
    }
    if (p !== undefined) {
      if (this.opts.strict === true) return undefined
      this.marks.set(height, { at: Date.now(), state: 'uncorroborated', hash: p.hash })
      return p
    }
    if (w !== undefined && this.opts.strict !== true) {
      this.marks.set(height, { at: Date.now(), state: 'uncorroborated', hash: w.hash })
      return w
    }
    return undefined
  }

  async isValidRootForHeight(root: string, height: number): Promise<boolean> {
    return (await this.headerAtHeight(height))?.merkleRoot === root
  }

  async currentHeight(): Promise<number> {
    return await this.primary.currentHeight().catch(() => this.witness.currentHeight())
  }

  async tipHeight(): Promise<number> {
    const tip = (t: ChainTracker) => (t.tipHeight !== undefined ? t.tipHeight() : t.currentHeight())
    return await tip(this.primary).catch(() => tip(this.witness))
  }

  /** force both sources to re-ask a height — clears the corroboration mark
   *  and the pending witness ask, so the next read re-attests rather than
   *  serving the previous verdict */
  async refreshHeader(height: number): Promise<BlockHeader | undefined> {
    this.marks.delete(height)
    this.asks.delete(height)
    const [p, w] = await Promise.all([this.primary.refreshHeader?.(height).catch(() => undefined), this.witness.refreshHeader?.(height).catch(() => undefined)])
    return p ?? w
  }

  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.primary.onReorg?.(cb)
    this.witness.onReorg?.(cb)
  }

  /** true = both sources agreed at this height; false = served uncorroborated
   *  (or disagreeing — that verdict ages out after `retryMs`); undefined =
   *  never asked */
  corroborated(height: number): boolean | undefined {
    const m = this.marks.get(height)
    if (m === undefined) return undefined
    if (m.state === 'ok') return true
    if (m.state === 'mismatch' && Date.now() - m.at > this.retryMs) return undefined
    return false
  }
}
