// mainnet.ts — read-only mainnet access (policy, headers, chain info, UTXO
// discovery, merkle paths) + the BSV Broadcaster client. NO spend happens
// here: BsvBroadcaster is the only write path and it is wrapped by SpendGuard
// in the runtime before any tx reaches it.
import { TxStore } from './txstore.js'
import { Transaction, MerklePath, Hash } from '@bsv/sdk'
import { tscMerklePath } from '@lightwebinc/bcommon'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut } from './interfaces.js'

export interface MainnetConfig {
  broadcasterUrl?: string // BSV Broadcaster base (no trailing slash)
  broadcasterApiKey?: string
  callbackUrl?: string // our public base — the broadcaster pushes tx-status updates here (X-CallbackUrl)
  callbackToken?: string // sent as X-CallbackToken; webhooks carry it as Authorization: Bearer
  wocUrl?: string // WhatsOnChain base (no trailing slash)
  wocApiKey?: string
  chaintracksUrl?: string // chaintracks base incl. /v1|/v2 — header source for BUMP verification
  fetchImpl?: typeof fetch // injectable — tests pass a recorded stub
}

const def = (c?: MainnetConfig) => ({
  // BSV Broadcaster (formerly Arcade; the ARC successor) — GorillaPool instance
  broadcasterUrl: c?.broadcasterUrl ?? 'https://arcade.gorillapool.io',
  callbackUrl: c?.callbackUrl ?? nektarEnv('NEKTAR_CALLBACK_URL'),
  callbackToken: c?.callbackToken ?? nektarEnv('NEKTAR_CALLBACK_TOKEN'),
  wocUrl: c?.wocUrl ?? 'https://api.whatsonchain.com/v1/bsv/main',
  fetch: c?.fetchImpl ?? fetch,
  broadcasterApiKey: c?.broadcasterApiKey,
  wocApiKey: c?.wocApiKey
})

const req = async (c: ReturnType<typeof def>, url: string, init: RequestInit = {}, ms = 20_000): Promise<Response> => {
  const ctl = new AbortController()
  const t = setTimeout(() => ctl.abort(), ms)
  try {
    return await c.fetch(url, { ...init, signal: ctl.signal })
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
  constructor(c?: MainnetConfig) {
    this.cfg = def(c)
  }
  async broadcast(tx: Transaction): Promise<BroadcastResult> {
    const { broadcasterUrl, wocUrl } = this.cfg
    const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
    if (this.cfg.broadcasterApiKey) headers.Authorization = `Bearer ${this.cfg.broadcasterApiKey}`
    // tx-status updates: pushed to our webhook when a callback URL is
    // configured, or pulled by our own SSE reader on /events — either way the
    // token tags the submissions so only our statuses reach us
    if (this.cfg.callbackUrl) headers['X-CallbackUrl'] = `${this.cfg.callbackUrl}/v1/broadcaster-callback`
    if (this.cfg.callbackUrl || this.cfg.callbackToken) headers['X-FullStatusUpdates'] = 'true'
    if (this.cfg.callbackToken) headers['X-CallbackToken'] = this.cfg.callbackToken
    try {
      // Extended Format when every input carries its source tx (the broadcaster
      // resolves unconfirmed parents from it); plain raw otherwise
      const payload = new Uint8Array(tx.inputs.every((i) => i.sourceTransaction !== undefined) ? tx.toEF() : tx.toBinary())
      const post = () => req(this.cfg, `${broadcasterUrl}/tx`, { method: 'POST', headers, body: payload })
      // 503 = backpressure, nothing queued — retry the same instance shortly
      let r: Response | undefined
      for (let i = 0; ; i++) {
        r = await post()
        if (r.status !== 503 || i >= 3) break
        await new Promise((rs) => setTimeout(rs, 500 * 2 ** i))
      }
      if (r.status >= 500) throw new Error(`broadcaster HTTP ${r.status}`) // → WoC fallback
      const body = await r.json().catch(() => ({}))
      const st = String(body.txStatus ?? '').toUpperCase()
      if (r.ok && st !== 'REJECTED' && st !== 'DOUBLE_SPEND_ATTEMPTED') {
        return { status: 'success', txid: tx.id('hex') }
      }
      return { status: 'error', description: `broadcaster ${st || `HTTP ${r.status}`}: ${body.reason ?? body.detail ?? body.extraInfo ?? ''}`.trim() }
    } catch (e) {
      // network error / 5xx → WoC fallback (policy rejections already returned)
      try {
        const wh: Record<string, string> = { 'Content-Type': 'application/json' }
        if (this.cfg.wocApiKey) wh['woc-api-key'] = this.cfg.wocApiKey
        const wr = await req(this.cfg, `${wocUrl}/tx/raw`, { method: 'POST', headers: wh, body: JSON.stringify({ txhex: tx.toHex() }) })
        if (!wr.ok) return { status: 'error', description: `WoC HTTP ${wr.status}: ${await wr.text().catch(() => '')}`.slice(0, 200) }
        return { status: 'success', txid: tx.id('hex') }
      } catch (we) {
        return { status: 'error', description: `broadcast failed: broadcaster ${(e as Error).message}; WoC ${(we as Error).message}` }
      }
    }
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
 *  last delivered id is persisted through get/set so a process restart
 *  resumes exactly where it left off. */
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
  let closed = false
  let lastId: string | undefined
  let abort: AbortController | undefined
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
          if (e.id !== undefined) {
            lastId = e.id
            await opts.cursor?.set(e.id)
          }
          await onEvent(e)
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
        if (closed) return
        opts.onError?.(e as Error)
        await new Promise((r) => setTimeout(r, backoff))
        backoff = Math.min(maxB, backoff * 2)
      }
    }
  }
  const p = run()
  return {
    close: () => {
      closed = true
      abort?.abort()
      void p.catch(() => {})
    }
  }
}

/** BSV Broadcaster /policy → mining fee in sat/kB (ceil). */
export async function fetchBroadcasterFeeRate(cfg?: MainnetConfig): Promise<number> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.broadcasterApiKey) h.Authorization = `Bearer ${c.broadcasterApiKey}`
  const p = await j(await req(c, `${c.broadcasterUrl}/policy`, { headers: h }))
  const f = p.policy?.miningFee ?? p.miningFee
  return Math.ceil((f.satoshis * 1000) / f.bytes)
}

// --- WoC tracker (rate-limited, cached headers) -------------------------------
export class WocTracker implements ChainTracker {
  private cfg
  private headers = new Map<number, BlockHeader>()
  private lastReq = 0
  constructor(c?: MainnetConfig) {
    this.cfg = def(c)
  }

  private async wocGet(path: string, retries = 3): Promise<any> {
    for (let i = 0; i <= retries; i++) {
      // ≥350 ms spacing — WoC is ~3 req/s unauthenticated
      const wait = 350 - (Date.now() - this.lastReq)
      if (wait > 0) await new Promise((r) => setTimeout(r, wait))
      const h: Record<string, string> = {}
      if (this.cfg.wocApiKey) h['woc-api-key'] = this.cfg.wocApiKey
      this.lastReq = Date.now()
      const r = await req(this.cfg, `${this.cfg.wocUrl}${path}`, { headers: h }, 15_000)
      if (r.status === 429 && i < retries) {
        await new Promise((rs) => setTimeout(rs, 1200 * (i + 1)))
        continue
      }
      if (r.status === 404) return undefined
      return await j(r)
    }
  }

  async currentHeight(): Promise<number> {
    return (await this.wocGet('/chain/info')).blocks
  }
  async tipHeight(): Promise<number> {
    return await this.currentHeight()
  }

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const hit = this.headers.get(height)
    if (hit) return hit
    const b = await this.wocGet(`/block/height/${height}`)
    if (!b) return undefined
    const h: BlockHeader = {
      height,
      hash: b.hash,
      prevHash: b.previousblockhash ?? '',
      merkleRoot: b.merkleroot,
      time: b.time,
      bits: parseInt(String(b.bits), 16) || 0,
      nonce: b.nonce ?? 0
    }
    this.headers.set(height, h)
    return h
  }

  async isValidRootForHeight(root: string, height: number, signal?: AbortSignal): Promise<boolean> {
    const h = await this.headerAtHeight(height)
    return h?.merkleRoot === root
  }
}

// --- Chaintracks header source (public, no key — PoW-verifiable data) --------
/** Chaintracks serves the longest-chain block headers — the ONLY global chain
 *  data SPV verification needs, and self-authenticating via proof-of-work, so
 *  this is not an indexer dependency. v2 (`/chaintracks/v2`) returns bare JSON
 *  headers; v1 (`/chaintracks/v1`) wraps values and serves 80-byte hex.
 *  `fallback` (e.g. WocTracker) is tried only when chaintracks itself fails. */
export class ChaintracksTracker implements ChainTracker {
  private headers = new Map<number, BlockHeader>()
  private base: string
  private readonly v1: boolean
  private cfg: ReturnType<typeof def>
  constructor(
    c?: MainnetConfig,
    base?: string,
    private fallback?: ChainTracker
  ) {
    this.cfg = def(c)
    this.base = (base ?? `${this.cfg.broadcasterUrl}/chaintracks/v2`).replace(/\/$/, '')
    this.v1 = this.base.endsWith('/v1')
  }

  private async j(path: string): Promise<any> {
    const r = await req(this.cfg, `${this.base}${path}`, {}, 15_000)
    if (!r.ok) throw new Error(`chaintracks ${path}: HTTP ${r.status}`)
    return await r.json()
  }

  async currentHeight(): Promise<number> {
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

  async headerAtHeight(height: number): Promise<BlockHeader | undefined> {
    const hit = this.headers.get(height)
    if (hit) return hit
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
            nonce: b.nonce ?? 0
          }
        }
      }
    } catch {
      /* fall through to fallback */
    }
    if (!h && this.fallback) h = await this.fallback.headerAtHeight(height)
    if (h) this.headers.set(height, h)
    return h
  }

  async isValidRootForHeight(root: string, height: number, signal?: AbortSignal): Promise<boolean> {
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
    nonce: b.readUInt32LE(76)
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

/** BSV Broadcaster /tx (BUMP hex) → MerklePath; else WoC TSC proof → MerklePath. */
export async function merklePathMainnet(cfg: MainnetConfig | undefined, txid: string): Promise<PathResult | null> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.broadcasterApiKey) h.Authorization = `Bearer ${c.broadcasterApiKey}`
  const ar = await req(c, `${c.broadcasterUrl}/tx/${txid}`, { headers: h })
  if (ar.ok) {
    const b = await ar.json()
    if (b.merklePath) {
      try {
        return { merklePath: MerklePath.fromHex(b.merklePath), status: String(b.txStatus ?? 'mined') }
      } catch {
        /* fall through to WoC */
      }
    }
    const st = String(b.txStatus ?? '').toUpperCase()
    if (st !== 'MINED' && st !== 'IMMUTABLE') {
      return null // still pending / rejected upstream
    }
  }
  // WoC TSC fallback
  const wh: Record<string, string> = {}
  if (c.wocApiKey) wh['woc-api-key'] = c.wocApiKey
  const tr = await req(c, `${c.wocUrl}/tx/hash/${txid}`, { headers: wh })
  if (!tr.ok) return null
  const txj = await tr.json()
  const height = txj.blockheight
  if (!height || height <= 0) return null
  const pr = await req(c, `${c.wocUrl}/tx/${txid}/proof/tsc`, { headers: wh })
  if (!pr.ok) return null
  const tsc = (await pr.json())[0]
  if (!tsc) return null
  return { merklePath: tscToMerklePath(tsc, txid, height), status: 'mined' }
}

/** GET with 429/5xx backoff (WoC rate-limits ~3 req/s); throws on a final non-2xx */
async function getOk(c: ReturnType<typeof def>, url: string, headers: Record<string, string>): Promise<Response> {
  for (let i = 0; ; i++) {
    const r = await req(c, url, { headers })
    if (r.ok) return r
    if ((r.status === 429 || r.status >= 500) && i < 4) {
      await new Promise((res) => setTimeout(res, 800 * 2 ** i))
      continue
    }
    throw new Error(`${url.split('/').slice(-3).join('/')}: HTTP ${r.status}`)
  }
}

// --- funding import by txid (address indexes can be unavailable, e.g. WoC-TTN) ---
/** fetch a funding tx and return its P2PKH outputs locked to `pkhHex` */
export async function importFundingTx(cfg: MainnetConfig | undefined, ref: string, pkhHex: string): Promise<FundingOut[]> {
  // 'hex:<tx>' = fully offline import — the operator hands us the funding
  // transaction itself, no network fetch and no index involved. Handed as
  // BEEF it brings its proof or proven ancestry, so what is paid from it can
  // be verified by a counterparty at once.
  if (ref.startsWith('hex:')) {
    const wire = ref.slice(4).replace(/\s+/g, '')
    if (wire.length === 0) throw new Error('empty transaction payload — check $HEX / the funding file')
    if (wire.length % 2 !== 0 || !/^[0-9a-fA-F]+$/.test(wire)) throw new Error(`malformed transaction hex (${wire.length} chars)`)
    return fundingOutputs((await TxStore.read(wire, undefined)).tx, pkhHex)
  }
  if (!/^[0-9a-fA-F]{64}$/.test(ref)) throw new Error(`malformed txid — expected 64 hex chars, 'hex:<rawtx>', got "${ref.slice(0, 24)}${ref.length > 24 ? '…' : ''}"`)
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.wocApiKey) h['woc-api-key'] = c.wocApiKey
  const r = await getOk(c, `${c.wocUrl}/tx/${ref}/hex`, h)
  return fundingOutputs(Transaction.fromHex((await r.text()).trim()), pkhHex)
}

function fundingOutputs(tx: Transaction, pkhHex: string): FundingOut[] {
  const want = `76a914${pkhHex}88ac`
  return tx.outputs.flatMap((o, vout) => (o.lockingScript.toHex() === want ? [{ tx, vout, satoshis: o.satoshis ?? 0 }] : []))
}

// --- UTXO discovery -----------------------------------------------------------
export async function discoverUtxos(cfg: MainnetConfig | undefined, address: string): Promise<FundingOut[]> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.wocApiKey) h['woc-api-key'] = c.wocApiKey
  const list = await (await getOk(c, `${c.wocUrl}/address/${address}/unspent`, h)).json()
  const out: FundingOut[] = []
  for (const u of list) {
    await new Promise((res) => setTimeout(res, 350))
    // a just-broadcast tx may not be served as hex yet (404): skip it this round
    const hex = await getOk(c, `${c.wocUrl}/tx/${u.tx_hash}/hex`, h)
      .then((r) => r.text())
      .catch(() => null)
    if (hex === null) continue
    out.push({ tx: Transaction.fromHex(hex.trim()), vout: u.tx_pos, satoshis: u.value })
  }
  return out
}
