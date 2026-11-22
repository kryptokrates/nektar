// mainnet.ts — read-only mainnet access (policy, headers, chain info, UTXO
// discovery, merkle paths) + the BSV Broadcaster client. NO spend happens
// here: BsvBroadcaster is the only write path and it is wrapped by SpendGuard
// in the runtime before any tx reaches it.
import { currentSignal, nektarEnv, sparse, spread } from '@nektar/core'
import { sleep, withRetry } from './retry.js'
import { TxStore } from './txstore.js'
import { Transaction, MerklePath, Hash, Utils } from '@bsv/sdk'
import { tscMerklePath } from '@lightwebinc/bcommon'
import { headerPowValid, HeaderCache } from './headers.js'
import { indexFailover, type ChainIndex, type IndexUtxo } from './indexer.js'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut } from './interfaces.js'
import type { StatusHint } from './txfacts.js'

export interface MainnetConfig {
  broadcasterUrl?: string // primary BSV Broadcaster base (no trailing slash)
  /** additional broadcaster endpoints the same tx is submitted to
   *  concurrently — first acceptance wins ('have a second route ready'
   *  per the ARC docs). broadcasterUrl stays primary: /events, /policy
   *  and reads prefer it; statuses merge under the one callback token */
  broadcasterUrls?: string[]
  broadcasterApiKey?: string
  callbackUrl?: string // our public base — the broadcaster pushes tx-status updates here (X-CallbackUrl)
  callbackToken?: string // sent as X-CallbackToken; webhooks carry it as Authorization: Bearer
  /** chain-index base (no trailing slash) — bananablocks /api/v1 is the
   *  default index on both live nets; kind is guessed from the URL */
  indexerUrl?: string
  /** extra index bases, queried in order when the primary errors or misses —
   *  one rate-limited index must not read as chain-unreachable */
  indexerUrls?: string[]
  indexerApiKey?: string
  indexerKind?: 'banana' | 'woc'
  /** @deprecated use indexerUrl — honoured as an alias */
  wocUrl?: string
  /** @deprecated use indexerApiKey — honoured as an alias */
  wocApiKey?: string
  chaintracksUrl?: string // chaintracks base incl. /v1|/v2 — header source for BUMP verification
  tipCacheMs?: number // currentHeight() cache TTL — heights only grow, so a stale-low answer only delays gates by ≤TTL; NEKTAR_TIP_CACHE_MS overrides (default 3000)
  feeRateSatPerKb?: number // caller-pinned fee rate — otherwise livePort takes max(floor, broadcaster-policy quote)
  fetchImpl?: typeof fetch // injectable — tests pass a recorded stub
}

/** default BSV Broadcaster instance — the single place the fallback URL
 *  lives (was redeclared at five call sites in mcp/chain). */
export const DEFAULT_BROADCASTER_URL = 'https://arcade.gorillapool.io'
/** default chain index — bananablocks /api/v1 on main; testnet configs pass
 *  indexerUrl: 'https://test.bananablocks.com/api/v1' */
export const DEFAULT_INDEXER_URL = 'https://bananablocks.com/api/v1'

/** baked-in public ARC-family endpoints — the runtime's fan-out defaults
 *  plus the GorillaPool primary. Membership = 'public-net config': the
 *  index cross-check on status reads is safe to engage. A CUSTOM primary
 *  (the only other case) is a private deployment — no public index can
 *  vouch for its view, so the cross-check stays off there. */
export const PUBLIC_BROADCASTER_URLS = new Set([
  DEFAULT_BROADCASTER_URL,
  'https://testnet.arcade.gorillapool.io',
  'https://arc.taal.com',
  'https://arc-test.taal.com',
  'https://arcade-v2-us-1.bsvblockchain.tech',
  'https://arcade-v2-testnet-us-1.bsvblockchain.tech',
  'https://arcade-v2-testnet-eu-1.bsvblockchain.tech'
])

/** the index-cross-check predicate, shared by the constructor flag and the
 *  standalone probe — 'public' = explicit index URL, no broadcaster
 *  configured at all, or a baked-in public primary */
const publicIndexOk = (c?: MainnetConfig): boolean => {
  // eslint-disable-next-line typescript/no-deprecated -- wocUrl stays readable here; this IS the alias implementation
  if (c?.indexerUrl !== undefined || c?.wocUrl !== undefined) return true
  const primary = c?.broadcasterUrl ?? c?.broadcasterUrls?.[0]
  return primary === undefined || PUBLIC_BROADCASTER_URLS.has(primary)
}

const def = (c?: MainnetConfig) => {
  // eslint-disable-next-line typescript/no-deprecated -- wocUrl stays readable here; this IS the alias implementation
  const indexerUrl = c?.indexerUrl ?? c?.wocUrl ?? DEFAULT_INDEXER_URL
  // primary = explicit broadcasterUrl, else the first listed extra, else
  // the GorillaPool default; extras append in order, deduped
  const broadcasterUrls = [...new Set([c?.broadcasterUrl ?? c?.broadcasterUrls?.[0] ?? DEFAULT_BROADCASTER_URL, ...(c?.broadcasterUrls ?? [])])]
  return {
    // BSV Broadcaster (formerly Arcade; the ARC successor) — GorillaPool instance
    broadcasterUrl: broadcasterUrls[0],
    broadcasterUrls,
    callbackUrl: c?.callbackUrl ?? nektarEnv('NEKTAR_CALLBACK_URL'),
    callbackToken: c?.callbackToken ?? nektarEnv('NEKTAR_CALLBACK_TOKEN'),
    indexerUrl,
    index: indexFailover([...new Set([indexerUrl, ...(c?.indexerUrls ?? [])])], {
      // eslint-disable-next-line typescript/no-deprecated -- wocApiKey stays readable here; this IS the alias implementation
      apiKey: c?.indexerApiKey ?? c?.wocApiKey,
      kind: c?.indexerKind,
      fetchImpl: c?.fetchImpl
    }),
    fetch: c?.fetchImpl ?? fetch,
    broadcasterApiKey: c?.broadcasterApiKey,
    // eslint-disable-next-line typescript/no-deprecated -- wocApiKey stays readable here; this IS the alias implementation
    indexerApiKey: c?.indexerApiKey ?? c?.wocApiKey,
    tipCacheMs: c?.tipCacheMs ?? Number(nektarEnv('NEKTAR_TIP_CACHE_MS') ?? 3_000)
  }
}

const req = async (c: ReturnType<typeof def>, url: string, init: RequestInit = {}, ms = 20_000): Promise<Response> => {
  const ctl = new AbortController()
  const t = setTimeout(() => ctl.abort(), ms)
  try {
    // the ambient leg context merges by construction — a submit/probe issued
    // inside a deadline()-bound leg dies with the leg instead of lingering
    // as an orphaned socket (an abort mid-submit classifies as
    // submission_unknown upstream: bytes may have left, never a verdict)
    const ambient = currentSignal()
    return await c.fetch(url, { ...init, signal: ambient !== undefined ? AbortSignal.any([ctl.signal, ambient]) : ctl.signal })
  } finally {
    clearTimeout(t)
  }
}
const j = async (r: Response): Promise<any> => {
  if (!r.ok) throw new Error(`HTTP ${r.status}`)
  return await r.json()
}

// --- BSV Broadcaster client --------------------------------------------------
// Own client on purpose: the sdk ARC class posts /v1/tx which BSV Broadcaster
// does not serve, and the sdk Teranode class has no callback support.
export class BsvBroadcaster implements Broadcaster {
  private cfg
  private readonly indexFallback: boolean
  constructor(c?: MainnetConfig) {
    this.cfg = def(c)
    // index cross-check on status reads engages for public-default configs or
    // an explicit index URL — a custom broadcaster alone isn't assumed public-net
    // 'public-default' means the EFFECTIVE primary is a baked-in URL —
    // a custom first entry in broadcasterUrls counts as a private config
    this.indexFallback = publicIndexOk(c)
  }
  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
    if (this.cfg.broadcasterApiKey) headers.Authorization = `Bearer ${this.cfg.broadcasterApiKey}`
    // tx-status updates: pushed to our webhook when a callback URL is
    // configured, or pulled by our own SSE reader on /events — either way the
    // token tags the submissions so only our statuses reach us
    if (this.cfg.callbackUrl) headers['X-CallbackUrl'] = `${this.cfg.callbackUrl}/v1/broadcaster-callback`
    if (this.cfg.callbackUrl || this.cfg.callbackToken) headers['X-FullStatusUpdates'] = 'true'
    if (this.cfg.callbackToken) headers['X-CallbackToken'] = this.cfg.callbackToken
    // Extended Format when every input carries its source tx (the broadcaster
    // resolves unconfirmed parents from it); plain raw otherwise. One payload
    // serves every endpoint — same bytes, same txid, resubmission idempotent.
    const payload = new Uint8Array(tx.inputs.every((i) => i.sourceTransaction !== undefined) ? tx.toEF() : tx.toBinary())
    const submit = async (base: string): Promise<BroadcastResult> => {
      try {
        // EF submits under load can queue past the default 20s window — the
        // post is idempotent (same bytes, same txid), so a slow answer and a
        // transport timeout both earn one retry rather than a starvation
        // verdict for the caller
        const post = () => req(this.cfg, `${base}/tx`, { method: 'POST', headers, body: payload }, 60_000)
        // 503 = backpressure, nothing queued — retry the same instance shortly
        const r = await withRetry(post, {
          attempts: 4,
          delay: (i) => 500 * 2 ** i,
          retryIf: (x) => x.status === 503
        }).catch(async (_e) => {
          await new Promise((r2) => setTimeout(r2, 1_000))
          return await post()
        })
        if (r.status >= 500) throw new Error(`broadcaster HTTP ${r.status}`)
        const body = await r.json().catch(() => ({}))
        const st = String(body.txStatus ?? '').toUpperCase()
        if (r.ok && st !== 'REJECTED' && st !== 'DOUBLE_SPEND_ATTEMPTED') {
          return { status: 'success', txid: tx.id('hex') }
        }
        return { status: 'error', description: `broadcaster ${st || `HTTP ${r.status}`}: ${body.reason ?? body.detail ?? body.extraInfo ?? ''}`.trim() }
      } catch (e) {
        // the broadcaster is the only write path — a transport failure is an
        // error answer, never a detour through the index
        return { status: 'error', description: `broadcast failed: ${(e as Error).message}` }
      }
    }
    // every configured endpoint gets the same bytes at once — first
    // acceptance wins. Among all-failed answers a wire verdict ('broadcaster
    // REJECTED: …' / a 4xx reason) outranks a bare transport failure: it
    // names why the tx was refused instead of just that a route was down.
    const results = await Promise.all(this.cfg.broadcasterUrls.map(submit))
    return (
      results.find((r) => r.status === 'success') ??
      results.find((r) => r.description?.startsWith('broadcaster ')) ??
      sparse(results[0]) ?? { status: 'error', description: 'broadcast failed: no broadcaster endpoints configured' }
    )
  }

  /** the broadcaster's current word for a txid — a HINT, never a verdict:
   *  routes through the shared observer seam incl. the index mined-fallback */
  async statusHint(txid: string): Promise<StatusHint> {
    return await probeTxStatus(this.cfg, txid, { indexerFallback: this.indexFallback })
  }
}

export interface StatusStreamEvent {
  id?: string
  event?: string
  data?: string
}
export interface StatusStreamOpts {
  cursor?: { get: () => Promise<string | undefined> | string | undefined; set: (id: string) => Promise<void> | void }
  onError?: (e: Error) => void
  minBackoffMs?: number // default 1000
  maxBackoffMs?: number // default 30000
}

/** BSV Broadcaster SSE status stream — GET {base}/events?callbackToken=<t>.
 *  Pull-mode equivalent of the webhook: same status payloads, outbound only.
 *  Parses `id:`/`event:`/`data:` fields (multi-line data joined with '\n'),
 *  skips `:` comments/keepalives, and reconnects with exponential backoff
 *  (1 s → 30 s) resuming via `Last-Event-ID`. When opts.cursor is given the
 *  cursor advances only AFTER onEvent resolves — a process crash mid-handler
 *  redelivers the event on restart. An onEvent throw routes to opts.onError
 *  and the cursor still advances: a poison event must not wedge the stream. */
export function statusStream(
  cfg: MainnetConfig | undefined,
  onEvent: (e: StatusStreamEvent) => void | Promise<void>,
  opts: StatusStreamOpts = {}
): { close: () => void } {
  const c = def(cfg)
  const f = c.fetch
  const url = `${c.broadcasterUrl}/events?callbackToken=${encodeURIComponent(c.callbackToken ?? '')}`
  const minB = opts.minBackoffMs ?? 1000,
    maxB = opts.maxBackoffMs ?? 30_000
  let closed = false as boolean
  const done = (): boolean => closed
  let lastId: string | undefined
  let abort: AbortController | undefined
  let wake: (() => void) | undefined
  const ready = (async () => {
    lastId = await opts.cursor?.get()
  })()
  let backoff = minB
  const run = async (): Promise<void> => {
    await ready
    while (!closed) {
      abort = new AbortController()
      try {
        const headers: Record<string, string> = { accept: 'text/event-stream' }
        if (lastId !== undefined) headers['Last-Event-ID'] = lastId
        if (c.broadcasterApiKey) headers.Authorization = `Bearer ${c.broadcasterApiKey}`
        const res = await f(url, { headers, signal: abort.signal })
        if (!res.ok || res.body === null) throw new Error(`events HTTP ${res.status}`)
        backoff = minB
        const dec = new TextDecoder()
        const reader = res.body.getReader()
        let buf = ''
        let ev: { id?: string; event?: string; data: string[] } = { data: [] }
        const dispatch = async () => {
          if (ev.data.length === 0) {
            ev = { data: [] }
            return
          }
          const e = { id: ev.id, event: ev.event, data: ev.data.join('\n') }
          ev = { data: [] }
          try {
            await onEvent(e)
          } catch (err) {
            opts.onError?.(err as Error)
          }
          if (e.id !== undefined) {
            lastId = e.id
            await opts.cursor?.set(e.id)
          }
        }
        for (;;) {
          const { done, value } = await reader.read()
          if (done) break
          buf += dec.decode(value, { stream: true })
          let nl
          while ((nl = buf.indexOf('\n')) >= 0) {
            let line = buf.slice(0, nl)
            buf = buf.slice(nl + 1)
            if (line.endsWith('\r')) line = line.slice(0, -1)
            if (line === '') {
              await dispatch()
              continue
            }
            if (line.startsWith(':')) continue // comment / keepalive
            const colon = line.indexOf(':')
            const field = colon < 0 ? line : line.slice(0, colon)
            let val = colon < 0 ? '' : line.slice(colon + 1)
            if (val.startsWith(' ')) val = val.slice(1)
            if (field === 'id') ev.id = val
            else if (field === 'event') ev.event = val
            else if (field === 'data') ev.data.push(val)
          }
        }
        await dispatch() // flush a trailing unterminated event
        throw new Error('events stream ended')
      } catch (e) {
        if (done()) return
        opts.onError?.(e as Error)
        // spread() on the sleep, not the curve — the doubling stays exact,
        // the reconnect lands desynchronized across reconnecting agents.
        // close() must interrupt the backoff: an un-abortable ~30s sleep
        // lingers past teardown (and pins the loop — unref'd or not)
        await new Promise<void>((r) => {
          const t = setTimeout(r, spread(backoff))
          wake = () => {
            clearTimeout(t)
            r()
          }
        })
        wake = undefined
        backoff = Math.min(maxB, backoff * 2)
      }
    }
  }
  const p = run()
  return {
    close: () => {
      closed = true
      abort?.abort()
      wake?.()
      void p.catch(() => {})
    }
  }
}

/** BSV Broadcaster /policy → mining fee in sat/kB (ceil). */
export async function fetchBroadcasterFeeRate(cfg?: MainnetConfig): Promise<number> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.broadcasterApiKey) h.Authorization = `Bearer ${c.broadcasterApiKey}`
  // primary first; a dead route falls through to the next configured one
  for (const base of c.broadcasterUrls) {
    try {
      const p = await j(await req(c, `${base}/policy`, { headers: h }))
      const f = p.policy?.miningFee ?? p.miningFee
      return Math.ceil((f.satoshis * 1000) / f.bytes)
    } catch {
      /* next endpoint */
    }
  }
  throw new Error('broadcaster /policy unreachable on every configured endpoint')
}

/** the broadcaster's current word for a txid — the single status-hint seam:
 *  uppercase txStatus ('' when undecidable) plus an `answered` flag, true
 *  when some source gave a DEFINITIVE reply (a 2xx verdict, or a clean 404 —
 *  the host knows it does not carry the tx); false when every reachable
 *  source errored, rate-limited or timed out. "Couldn't ask" is not "not on
 *  the network" — callers that age watches (the abandon clock) may only
 *  count answered reads. `wocFallback` (default: public-default or
 *  explicitly-WoC configs) cross-checks a dead/erroring broadcaster against
 *  WoC /tx/{txid}: blockheight > 0 → 'MINED' — rejection verdicts stay
 *  broadcaster-only (WoC serves no rejection semantics). Adapter-internal:
 *  callers consume it through BsvBroadcaster.statusHint / TxFacts.hint. */
export async function probeTxStatus(
  cfg: MainnetConfig | undefined,
  txid: string,
  opts?: { wocFallback?: boolean; indexerFallback?: boolean }
): Promise<{ status: string; answered: boolean }> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.broadcasterApiKey) h.Authorization = `Bearer ${c.broadcasterApiKey}`
  // every configured endpoint is asked at once — the primary's answer is
  // preferred (list order), a clean 404 on any route is still a real answer
  const answers = await Promise.all(
    c.broadcasterUrls.map(async (base) => {
      try {
        const r = await req(c, `${base}/tx/${txid}`, { headers: h }, 10_000)
        if (r.ok) {
          const s = String(((await r.json().catch(() => undefined)) as { txStatus?: string } | undefined)?.txStatus ?? '').toUpperCase()
          if (s !== '') return { status: s, answered: true }
          // 2xx without a txStatus → not answered by the broadcaster
        } else if (r.status === 404) {
          return { status: '', answered: true } // a clean "not found" is a real answer
        }
        // 429 / 5xx / other non-ok → the broadcaster never answered
      } catch {
        /* endpoint undecidable — another route may still answer */
      }
      return { status: '', answered: false }
    })
  )
  // a nonempty status from any route short-circuits; 'not found' answers
  // only mark the question answered and still let the index say MINED —
  // a mined tx ages out of broadcaster memory
  const verdict = answers.find((a) => a.answered && a.status !== '')
  if (verdict !== undefined) return verdict
  const answered = answers.some((a) => a.answered)
  const indexOk = opts?.indexerFallback ?? opts?.wocFallback ?? publicIndexOk(cfg)
  if (!indexOk) return { status: '', answered }
  try {
    const w = await c.index.txJson(txid)
    if (w === undefined) return { status: '', answered } // couldn't-ask / not found per dialect semantics
    return typeof w.blockHeight === 'number' && w.blockHeight > 0 ? { status: 'MINED', answered: true } : { status: '', answered: true }
  } catch {
    return { status: '', answered }
  }
}

/** Raw tx bytes from the best available source — WoC `/tx/{txid}/hex`
 *  first, then the broadcaster's `GET /tx/{txid}` rawTx (plain hex, or
 *  Extended Format when the 0000000000ef marker is present). Whatever the
 *  source, the bytes are returned only when they hash to `txid`. */
export async function fetchRawTx(cfg: MainnetConfig | undefined, txid: string): Promise<Transaction | undefined> {
  const c = def(cfg)
  const f = c.fetch
  const parse = (hex: string | undefined): Transaction | undefined => {
    if (hex === undefined || hex.length < 20) return undefined
    try {
      const tx = hex.slice(8, 20) === '0000000000ef' ? Transaction.fromHexEF(hex) : Transaction.fromHex(hex)
      return tx.id('hex') === txid ? tx : undefined
    } catch {
      return undefined
    }
  }
  try {
    const hex = await c.index.txHex(txid)
    if (hex !== undefined) {
      const tx = parse(hex)
      if (tx !== undefined) return tx
    }
  } catch {
    /* fall through to the broadcaster source */
  }
  for (const base of c.broadcasterUrls) {
    try {
      const r = await f(`${base}/tx/${txid}`, { signal: AbortSignal.timeout(10_000) })
      if (r.ok) {
        const j = (await r.json()) as { rawTx?: string } | null
        const tx = parse(j?.rawTx)
        if (tx !== undefined) return tx
      }
    } catch {
      /* next endpoint */
    }
  }
  return undefined
}

// --- WoC tracker (rate-limited, cached headers) -------------------------------
/** the index-backed tracker — the dialect (bananablocks default, WoC for the
 *  BTC temporal source/legacy configs) comes from the MainnetConfig URL. */
export class IndexTracker implements ChainTracker {
  private cfg
  private index: ChainIndex
  private cache = new HeaderCache()
  private readonly requirePow: boolean
  constructor(c?: MainnetConfig, opts?: { requirePow?: boolean }) {
    this.cfg = def(c)
    this.index = this.cfg.index
    // PoW-per-header is on by default; a private non-PoW net opts out via the
    // construction site (NEKTAR_HEADER_POW=0) — it is not a runtime flag here
    this.requirePow = opts?.requirePow !== false
  }

  private tip = { at: 0, height: 0 }
  async currentHeight(): Promise<number> {
    const ms = this.cfg.tipCacheMs
    if (ms > 0 && Date.now() - this.tip.at < ms) return this.tip.height
    const height = await this.index.tip()
    if (height === undefined) throw new Error('index tip unreachable')
    // a tip advance over a cached previous tip is where a silent reorg shows
    // up — the fresh tip's linkage check runs against the cached parent; no
    // request is added when the height is unchanged
    if (height > this.tip.height && this.cache.has(this.tip.height)) void this.headerAtHeight(height).catch(() => undefined)
    this.tip = { at: Date.now(), height }
    return height
  }
  async tipHeight(): Promise<number> {
    return await this.currentHeight()
  }

  /** raw fetch + self-authentication — a PoW-invalid header is a miss, not
   *  truth; never cached and never linkage-checked */
  private async fetchHeader(height: number): Promise<BlockHeader | undefined> {
    const h = await this.index.header(height)
    if (h === undefined) return undefined
    if (this.requirePow && !headerPowValid(h)) return undefined
    return h
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const hit = this.cache.get(height)
    if (hit) return hit
    const h = await this.fetchHeader(height)
    if (h === undefined) return undefined
    if (!(await this.cache.admit(h, (x) => this.fetchHeader(x)))) return h
    this.cache.set(h)
    return h
  }

  /** bypass the cache and re-ask — a witnessed pair calls this when sources
   *  disagree, so a stale cache can't masquerade as a chain split. A fresh
   *  header that reorgs a same-height cached one runs the walk (onReorg
   *  fires, neighbours evict) before the answer is served. */
  async refreshHeader(height: number): Promise<BlockHeader | undefined> {
    const h = await this.fetchHeader(height)
    if (h === undefined) return undefined // source down — leave the cache alone
    const cur = this.cache.get(height)
    if (cur !== undefined && cur.hash !== h.hash) {
      await this.cache.reorgAt(h, (x) => this.fetchHeader(x))
      this.cache.set(h)
    } else if (await this.cache.admit(h, (x) => this.fetchHeader(x))) this.cache.set(h)
    return h
  }

  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.cache.onReorg(cb)
  }

  async isValidRootForHeight(root: string, height: number, _signal?: AbortSignal): Promise<boolean> {
    const h = await this.headerAtHeight(height)
    return h?.merkleRoot === root
  }
}

// --- Chaintracks header source (public, no key — PoW-verifiable data) --------
/** Chaintracks serves the longest-chain block headers — the ONLY global chain
 *  data SPV verification needs, and each answer is now CHECKED to be
 *  self-authenticating: hash256 over its serialization must equal the served
 *  hash and satisfy its own declared difficulty, or the header is treated as
 *  not served. PoW-per-header does NOT prove the answer sits on the most-work
 *  chain — a sourced minority fork passes it too; corroboration against a
 *  second source is WitnessedTracker's job. v2 (`/chaintracks/v2`) returns
 *  bare JSON headers; v1 (`/chaintracks/v1`) wraps values and serves 80-byte
 *  hex. `fallback` (e.g. WocTracker) is tried only when chaintracks itself
 *  fails — prefer a WitnessedTracker over a silent fallback pair. */
export class ChaintracksTracker implements ChainTracker {
  private cache = new HeaderCache()
  private base: string
  private readonly v1: boolean
  private readonly requirePow: boolean
  private cfg: ReturnType<typeof def>
  constructor(
    c?: MainnetConfig,
    base?: string,
    private fallback?: ChainTracker,
    opts?: { requirePow?: boolean }
  ) {
    this.cfg = def(c)
    this.base = (base ?? `${this.cfg.broadcasterUrl}/chaintracks/v2`).replace(/\/$/, '')
    this.v1 = this.base.endsWith('/v1')
    this.requirePow = opts?.requirePow !== false
  }

  private async j(path: string): Promise<any> {
    const r = await req(this.cfg, `${this.base}${path}`, {}, 15_000)
    if (!r.ok) throw new Error(`chaintracks ${path}: HTTP ${r.status}`)
    return await r.json()
  }

  private tip = { at: 0, height: 0 }
  async currentHeight(): Promise<number> {
    const ms = this.cfg.tipCacheMs
    if (ms > 0 && Date.now() - this.tip.at < ms) return this.tip.height
    const height = await this.loadHeight()
    // same reorg trigger as WocTracker: a tip advance over a cached previous
    // tip runs the linkage gate — silent when the height is unchanged
    if (height > this.tip.height && this.cache.has(this.tip.height)) void this.headerAtHeight(height).catch(() => undefined)
    this.tip = { at: Date.now(), height }
    return height
  }
  private async loadHeight(): Promise<number> {
    try {
      if (this.v1) {
        const j = await this.j('/getPresentHeight')
        return j.value
      }
      return (await this.j('/height')).height
    } catch (e) {
      if (this.fallback) return await this.fallback.currentHeight()
      throw e
    }
  }
  async tipHeight(): Promise<number> {
    return await this.currentHeight()
  }

  /** raw fetch + self-authentication — a PoW-invalid header counts as a miss
   *  and falls through to `fallback` like any other primary failure */
  private async fetchHeader(height: number): Promise<BlockHeader | undefined> {
    let h: BlockHeader | undefined
    try {
      if (this.v1) {
        const j = await this.j(`/findHeaderHexForHeight?height=${height}`)
        if (j.value) h = parseHeaderHex(j.value, height)
      } else {
        const b = await this.j(`/header/height/${height}`)
        if (b?.merkleRoot) {
          h = {
            height: b.height ?? height,
            hash: b.hash ?? '',
            prevHash: b.previousHash ?? '',
            merkleRoot: b.merkleRoot,
            time: b.time ?? 0,
            bits: b.bits ?? 0,
            nonce: b.nonce ?? 0,
            version: b.version
          }
        }
      }
    } catch {
      /* fall through to fallback */
    }
    if (h !== undefined && this.requirePow && !headerPowValid(h)) h = undefined
    if (h === undefined && this.fallback) h = await this.fallback.headerAtHeight(height)
    return h
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const hit = this.cache.get(height)
    if (hit) return hit
    const h = await this.fetchHeader(height)
    if (h === undefined) return undefined
    if (!(await this.cache.admit(h, (x) => this.fetchHeader(x)))) return h
    this.cache.set(h)
    return h
  }

  /** bypass the cache and re-ask — a witnessed pair calls this when sources
   *  disagree, so a stale cache can't masquerade as a chain split. A fresh
   *  header that reorgs a same-height cached one runs the walk (onReorg
   *  fires, neighbours evict) before the answer is served. */
  async refreshHeader(height: number): Promise<BlockHeader | undefined> {
    const h = await this.fetchHeader(height)
    if (h === undefined) return undefined // source down — leave the cache alone
    const cur = this.cache.get(height)
    if (cur !== undefined && cur.hash !== h.hash) {
      await this.cache.reorgAt(h, (x) => this.fetchHeader(x))
      this.cache.set(h)
    } else if (await this.cache.admit(h, (x) => this.fetchHeader(x))) this.cache.set(h)
    return h
  }

  onReorg(cb: (removed: BlockHeader[], newTip: BlockHeader) => void): void {
    this.cache.onReorg(cb)
  }

  async isValidRootForHeight(root: string, height: number, _signal?: AbortSignal): Promise<boolean> {
    const h = await this.headerAtHeight(height)
    return h?.merkleRoot === root
  }
}

/** parse an 80-byte block header (hex) — v1 serves headers serialized. */
export function parseHeaderHex(hex: string, height: number): BlockHeader {
  const b = Buffer.from(hex.trim(), 'hex')
  if (b.length !== 80) throw new Error(`header hex is ${b.length}B, want 80`)
  const hash = Buffer.from(Hash.hash256([...b]))
    .reverse()
    .toString('hex')
  return {
    height,
    hash,
    prevHash: b.slice(4, 36).reverse().toString('hex'),
    merkleRoot: b.slice(36, 68).reverse().toString('hex'),
    time: b.readUInt32LE(68),
    bits: b.readUInt32LE(72),
    nonce: b.readUInt32LE(76),
    version: b.readUInt32LE(0)
  }
}

// --- merkle paths -------------------------------------------------------------
/** TSC proof JSON → SDK MerklePath. nodes['*'] = duplicate of the neighbour
 *  (odd leaf self-pair). Pure + exported for tests. The upstream
 *  `tscMerklePath` does the conversion and the malformed-proof refusals;
 *  this adapter only normalises TSC `nodes`, which a source may send as
 *  null for a single-tx block (common on TTN): an empty sibling list, not
 *  a malformed proof. A null *inside* the list stays a refusal upstream. */
export function tscToMerklePath(
  tsc: { index: number; txOrId: string; target: string; nodes: (string | null)[] | null },
  txid: string,
  blockHeight: number
): MerklePath {
  return tscMerklePath({ ...tsc, nodes: (tsc.nodes ?? []) as string[] }, txid, blockHeight)
}

export type PathResult = { merklePath: MerklePath; status: string }

/** BSV Broadcaster /tx (BUMP hex) → MerklePath; else index TSC proof → MerklePath. */
export async function merklePathMainnet(cfg: MainnetConfig | undefined, txid: string): Promise<PathResult | null> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.broadcasterApiKey) h.Authorization = `Bearer ${c.broadcasterApiKey}`
  for (const base of c.broadcasterUrls) {
    const ar = await req(c, `${base}/tx/${txid}`, { headers: h }).catch(() => undefined)
    if (ar === undefined || !ar.ok) continue // dead route — try the next one
    const b = await ar.json()
    if (b.merklePath) {
      try {
        return { merklePath: MerklePath.fromHex(b.merklePath), status: String(b.txStatus ?? 'mined') }
      } catch {
        break // a malformed path is a claim, not a proof — index fallback
      }
    }
    const st = String(b.txStatus ?? '').toUpperCase()
    if (st !== 'MINED' && st !== 'IMMUTABLE') {
      return null // still pending / rejected upstream
    }
    break // an answering endpoint that reports mined-without-path → TSC proof
  }
  // index TSC-proof fallback — the root it claims is still verified against
  // the witnessed tracker by the caller before the proof is trusted
  try {
    const pr = await c.index.proof(txid)
    if (pr === undefined) return null
    const p = sparse(tscToMerklePath({ index: pr.index, txOrId: pr.txOrId, target: pr.target, nodes: pr.nodes }, txid, pr.blockHeight))
    if (p === undefined) return null
    return { merklePath: p, status: 'mined' }
  } catch {
    return null
  }
}

// --- funding import by txid (address indexes can be unavailable) ---
/** fetch a funding tx and return its P2PKH outputs locked to `pkhHex`.
 *  `tracker` verifies any carried proof against our own headers — without it a
 *  BEEF import's BUMPs are unverifiable and get stripped at `TxStore.read`. */
export async function importFundingTx(cfg: MainnetConfig | undefined, ref: string, pkhHex: string, tracker?: ChainTracker): Promise<FundingOut[]> {
  // 'hex:<tx>' = fully offline import — the operator hands us the funding
  // transaction itself, no network fetch and no index involved. Handed as
  // BEEF it brings its proof or proven ancestry, so what is paid from it can
  // be verified by a counterparty at once.
  if (ref.startsWith('hex:')) {
    const wire = ref.slice(4).replace(/\s+/g, '')
    if (wire.length === 0) throw new Error('empty transaction payload — check $HEX / the funding file')
    if (wire.length % 2 !== 0 || !/^[0-9a-fA-F]+$/.test(wire)) throw new Error(`malformed transaction hex (${wire.length} chars)`)
    return fundingOutputs((await TxStore.read(wire, tracker)).tx, pkhHex)
  }
  if (!/^[0-9a-fA-F]{64}$/.test(ref))
    throw new Error(`malformed txid — expected 64 hex chars, 'hex:<rawtx>', got "${ref.slice(0, 24)}${ref.length > 24 ? '…' : ''}"`)
  const c = def(cfg)
  const hex = await c.index.txHex(ref)
  if (hex === undefined) throw new Error(`${ref}: index could not serve the tx`)
  const tx = Transaction.fromHex(hex)
  // the index is discovery, not truth: the served bytes must name the ref —
  // same gate discoverUtxos applies to its candidates
  if (tx.id('hex') !== ref) throw new Error(`${ref}: index served a transaction hashing to a different txid`)
  return fundingOutputs(tx, pkhHex)
}

function fundingOutputs(tx: Transaction, pkhHex: string): FundingOut[] {
  const want = `76a914${pkhHex}88ac`
  return tx.outputs.flatMap((o, vout) => (o.lockingScript.toHex() === want ? [{ tx, vout, satoshis: o.satoshis ?? 0 }] : []))
}

// --- UTXO discovery -----------------------------------------------------------
export async function discoverUtxos(cfg: MainnetConfig | undefined, address: string, onSkip?: (txid: string, reason: string) => void): Promise<FundingOut[]> {
  const c = def(cfg)
  const list = await c.index.utxos(address)
  if (list === undefined) throw new Error(`${address}: index could not serve unspents`)
  const out: Array<FundingOut | undefined> = Array.from({ length: list.length })
  // the index is discovery, not truth: an entry counts only once its own
  // bytes confirm the txid, the vout exists, and the script pays `address`.
  // satoshis come from the tx output, never from the index's `value`.
  // Request pacing lives inside the index dialects (pace + 429-retry) —
  // bounded workers overlap each candidate's RTT; a funded address with
  // N coins no longer pays N × (pace + fetch) inside WAIT_FUNDING.
  const pkh = Utils.fromBase58Check(address).data
  const want = `76a914${typeof pkh === 'string' ? pkh : Utils.toHex(Uint8Array.from(pkh))}88ac`
  const verify = async (u: IndexUtxo, i: number): Promise<void> => {
    // a just-broadcast tx may not be served as hex yet: skip it this round
    const hex = await c.index.txHex(u.txid).catch(() => undefined)
    if (hex === undefined) return
    let tx: Transaction
    try {
      tx = Transaction.fromHex(hex)
    } catch {
      onSkip?.(u.txid, 'served tx hex does not parse')
      return
    }
    if (tx.id('hex') !== u.txid) {
      onSkip?.(u.txid, 'served hex hashes to a different txid')
      return
    }
    const o = sparse(tx.outputs[u.vout])
    if (o === undefined) {
      onSkip?.(u.txid, `index vout ${u.vout} does not exist`)
      return
    }
    if (o.lockingScript.toHex() !== want) {
      onSkip?.(u.txid, `vout ${u.vout} does not pay the address`)
      return
    }
    out[i] = { tx, vout: u.vout, satoshis: o.satoshis ?? 0 }
  }
  let next = 0
  const WORKERS = 4
  await Promise.all(
    Array.from({ length: Math.min(WORKERS, list.length) }, async () => {
      while (next < list.length) {
        const i = next++
        await verify(list[i], i)
      }
    })
  )
  return out.flatMap((o) => (o === undefined ? [] : [o]))
}

/** The spend question answered with evidence, never trust: an index's
 *  "spent" flag is only a CLAIM — 'spent' is reported once the claimed
 *  spending transaction's own bytes name the outpoint as an input, hash to
 *  the claimed txid, and (when mined) its BUMP computes a root our
 *  witnessed tracker confirms. Mempool spends count as 'spent': a signed
 *  spend that provably exists will land, and offering the outpoint under
 *  it only buys conflicts. 'unspent' is unprovable by a third party —
 *  every negative and every couldn't-ask is 'unknown'. */
export async function utxoSpendStatus(
  cfg: MainnetConfig | undefined,
  tracker: Pick<ChainTracker, 'isValidRootForHeight'>,
  txid: string,
  vout: number
): Promise<'spent' | 'unknown'> {
  const c = def(cfg)
  // a hint probe answers fast or not at all — 'unknown' is the designed
  // negative, so a degraded index costs one bounded wait instead of the
  // full retry ladder (~18s across three retried calls) inside a
  // serialized take. 4s covers a healthy RTT plus one pacing retry and
  // stays under the take-side sweep ceiling
  return await Promise.race([spendProbe(c, tracker, txid, vout), sleep(4_000).then(() => 'unknown' as const)])
}

/** same evidence bar as utxoSpendStatus, but answers with the verified
 *  spender's txid — callers that need to classify WHO moved an outpoint
 *  (refund-vs-completion) take the named tx, not just the verdict */
export async function utxoSpender(
  cfg: MainnetConfig | undefined,
  tracker: Pick<ChainTracker, 'isValidRootForHeight'>,
  txid: string,
  vout: number
): Promise<string | undefined> {
  const c = def(cfg)
  return await Promise.race([spenderProbe(c, tracker, txid, vout), sleep(4_000).then(() => undefined)])
}

async function spendProbe(
  c: ReturnType<typeof def>,
  tracker: Pick<ChainTracker, 'isValidRootForHeight'>,
  txid: string,
  vout: number
): Promise<'spent' | 'unknown'> {
  return (await spenderProbe(c, tracker, txid, vout)) !== undefined ? 'spent' : 'unknown'
}

async function spenderProbe(
  c: ReturnType<typeof def>,
  tracker: Pick<ChainTracker, 'isValidRootForHeight'>,
  txid: string,
  vout: number
): Promise<string | undefined> {
  try {
    const j = await c.index.txJson(txid)
    if (j === undefined) return undefined
    const claim = j.outputs?.[vout]?.spentTxId
    if (typeof claim !== 'string' || !/^[0-9a-fA-F]{64}$/.test(claim)) return undefined
    const spending = await fetchRawTx(c, claim)
    if (spending === undefined) return undefined
    // the spend claim is only evidence when the served bytes name OUR
    // outpoint as an input — anything else is the index's word, ignored
    if (!spending.inputs.some((i) => String(i.sourceTXID).toLowerCase() === txid && i.sourceOutputIndex === vout)) return undefined
    const proof = sparse(await merklePathMainnet(c, claim))
    if (proof !== undefined && proof !== null) {
      const root = proof.merklePath.computeRoot(claim)
      if (!(await tracker.isValidRootForHeight(root, proof.merklePath.blockHeight))) return undefined
    }
    return claim
  } catch {
    return undefined
  }
}
