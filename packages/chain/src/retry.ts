// retry.ts — the chain package's single wait/retry policy: sleep, withRetry
// (bounded attempts + caller-chosen delay curve), pacer (minimum-spacing
// rate limiter). Replaces the hand-rolled backoff dialects that grew
// site-by-site in mainnet.ts/regtest.ts — sites declare intent, not loops.

export const sleep = (ms: number): Promise<void> => new Promise((r) => setTimeout(r, ms))

export interface RetryOpts<T> {
  /** total tries (1 = no retry) */
  attempts: number
  /** backoff before the next try; `attempt` = index of the FAILED try */
  delay: (attempt: number) => number
  /** retry when the result satisfies this */
  retryIf: (r: T) => boolean
  /** retry when fn throws this (default: rethrow at once) */
  retryErr?: (e: unknown) => boolean
}

export async function withRetry<T>(fn: () => Promise<T>, opts: RetryOpts<T>): Promise<T> {
  for (let i = 0; ; i++) {
    try {
      const r = await fn()
      if (i + 1 >= opts.attempts || !opts.retryIf(r)) return r
      await sleep(opts.delay(i))
    } catch (e) {
      if (i + 1 >= opts.attempts || !(opts.retryErr?.(e) ?? false)) throw e
      await sleep(opts.delay(i))
    }
  }
}

/** minimum-spacing limiter — each call waits until `minMs` since the last
 *  release (e.g. WoC ~3 req/s unauthenticated). */
export const pacer = (minMs: number): (() => Promise<void>) => {
  let last = 0
  return async () => {
    const wait = minMs - (Date.now() - last)
    if (wait > 0) await sleep(wait)
    last = Date.now()
  }
}
