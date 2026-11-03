// GOAL-v6 S3 — §12 multi-chain consensus time.
// T_C*(i) = f(T_C1(i),…,T_Cm(i)): when several chains are consulted for the
// timestamp of block i, the consensus time is the weighted median over the
// sources that answer. The default configuration is a single BSV source,
// which is today's behaviour bit-identical (median of one = that one).
//
// TemporalSources wraps the sources as ONE ChainTracker: header reads return
// the PRIMARY source's header with `time` replaced by the cross-chain
// weighted median; every non-time function (merkle roots, heights, reorgs)
// stays strictly on the primary BSV chain — SPV correctness never mixes
// chains. Wiring: NEKTAR_TEMPORAL_SOURCES=bsv[,btc] (packages/mcp runtime).
import type { BlockHeader, ChainTracker } from './interfaces.js'

/** a named header/height source; weight defaults to 1 */
export interface TemporalSource { name: string, tracker: ChainTracker, weight?: number }

/**
 * Weighted median of {time,weight}: sort ascending and take the first
 * timestamp whose cumulative weight reaches half the total (the "lower"
 * weighted median). Equal weights + n=2 therefore pick the SMALLER time —
 * a skewed-high source can never push consensus time up, which is the
 * conservative direction for §12 existence-by ("no later than") bounds.
 * n=1 returns that timestamp, so a single source is bit-identical to today.
 */
export function weightedMedianTime (entries: Array<{ time: number, weight?: number }>): number {
  if (entries.length === 0) throw new Error('weightedMedianTime: no timestamps')
  const s = entries.map(e => ({ t: e.time, w: e.weight ?? 1 })).filter(e => Number.isFinite(e.t) && e.w > 0)
    .sort((a, b) => a.t - b.t)
  if (s.length === 0) throw new Error('weightedMedianTime: no positive-weight timestamps')
  const total = s.reduce((a, e) => a + e.w, 0)
  let cum = 0
  for (const e of s) { cum += e.w; if (cum >= total / 2) return e.t }
  return s[s.length - 1]!.t
}

/** pure median over raw timestamps (all weights 1) */
export function medianTime (timestamps: number[]): number {
  return weightedMedianTime(timestamps.map(t => ({ time: t })))
}

/**
 * A ChainTracker whose header TIMES are the weighted median across sources
 * at the requested height; hash/roots/heights come from the primary source
 * (sources[0] — always the BSV tracker). A source that has no header at the
 * height (or errors) simply does not vote.
 */
export class TemporalSources implements ChainTracker {
  constructor (private sources: TemporalSource[]) {
    if (sources.length === 0) throw new Error('TemporalSources: at least one source required')
  }

  /** f(T_C1(i),…,T_Cm(i)) — weighted median over sources answering height i */
  async consensusTime (height: number): Promise<number | undefined> {
    const entries: Array<{ time: number, weight?: number }> = []
    await Promise.all(this.sources.map(async s => {
      const h = await s.tracker.headerAtHeight(height).catch(() => undefined)
      if (h) entries.push({ time: h.time, weight: s.weight })
    }))
    return entries.length === 0 ? undefined : weightedMedianTime(entries)
  }

  async headerAtHeight (height: number): Promise<BlockHeader | undefined> {
    const head = await this.sources[0]!.tracker.headerAtHeight(height)
    if (!head) return undefined
    if (this.sources.length === 1) return head // single source: unchanged
    const t = await this.consensusTime(height)
    return t === undefined ? head : { ...head, time: t }
  }

  currentHeight (): Promise<number> { return this.sources[0]!.tracker.currentHeight() }
  tipHeight (): Promise<number> {
    const p = this.sources[0]!.tracker
    return p.tipHeight ? p.tipHeight() : p.currentHeight()
  }
  isValidRootForHeight (root: string, height: number, signal?: AbortSignal): Promise<boolean> {
    return this.sources[0]!.tracker.isValidRootForHeight(root, height, signal)
  }
  onReorg (cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.sources[0]!.tracker.onReorg?.(cb)
  }
}
