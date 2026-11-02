// mainnet.ts — read-only mainnet access (policy, headers, chain info, UTXO
// discovery, merkle paths) + the ARC broadcaster. NO spend happens here:
// ArcBroadcaster is the only write path and it is wrapped by SpendGuard in
// the runtime before any tx reaches it.
import { Transaction, MerklePath } from '@bsv/sdk'
import type { BlockHeader, Broadcaster, BroadcastResult, ChainTracker, FundingOut } from './interfaces.js'

export interface MainnetConfig {
  arcUrl?: string // BSV Broadcaster (ex-Arcade) or classic ARC base (no trailing slash)
  arcApiKey?: string
  wocUrl?: string // WhatsOnChain base (no trailing slash)
  wocApiKey?: string
  fetchImpl?: typeof fetch // injectable — tests pass a recorded stub
}

const def = (c?: MainnetConfig) => ({
  // BSV Broadcaster (formerly Arcade; the ARC successor) — GorillaPool instance
  arcUrl: c?.arcUrl ?? 'https://arcade.gorillapool.io',
  wocUrl: c?.wocUrl ?? 'https://api.whatsonchain.com/v1/bsv/main',
  fetch: c?.fetchImpl ?? fetch,
  arcApiKey: c?.arcApiKey,
  wocApiKey: c?.wocApiKey
})

const req = async (c: ReturnType<typeof def>, url: string, init: RequestInit = {}, ms = 20_000): Promise<Response> => {
  const ctl = new AbortController()
  const t = setTimeout(() => ctl.abort(), ms)
  try { return await c.fetch(url, { ...init, signal: ctl.signal }) } finally { clearTimeout(t) }
}
const j = async (r: Response): Promise<any> => { if (!r.ok) throw new Error(`HTTP ${r.status}`); return await r.json() }

// --- ARC broadcaster ---------------------------------------------------------
const txPaths = new Map<string, string>() // arcUrl → '/v1/tx' (ARC) | '/tx' (Arcade)
// BSV Broadcaster / Arcade hosts serve /tx; classic ARC /v1/tx (still learned on 404)
const txPathFor = (arcUrl: string) => txPaths.get(arcUrl) ?? (/arcade|broadcaster/.test(arcUrl) ? '/tx' : '/v1/tx')
export class ArcBroadcaster implements Broadcaster {
  private cfg
  constructor (c?: MainnetConfig) { this.cfg = def(c) }
  async broadcast (tx: Transaction): Promise<BroadcastResult> {
    const { arcUrl, wocUrl } = this.cfg
    const headers: Record<string, string> = { 'Content-Type': 'application/octet-stream' }
    if (this.cfg.arcApiKey) headers.Authorization = `Bearer ${this.cfg.arcApiKey}`
    try {
      // classic ARC: /v1/tx; BSVA Arcade: /tx (404 on /v1/tx) — learn once per URL
      // Extended Format when every input carries its source tx (Arcade requires it
      // for unconfirmed parents); plain raw otherwise
      const ef = tx.inputs.every(i => i.sourceTransaction !== undefined)
      const payload = new Uint8Array(ef ? tx.toEF() : tx.toBinary())
      const post = (p: string) => req(this.cfg, `${arcUrl}${p}`, { method: 'POST', headers, body: payload })
      let r = await post(txPathFor(arcUrl))
      if (r.status === 404 && txPathFor(arcUrl) === '/v1/tx') { txPaths.set(arcUrl, '/tx'); r = await post('/tx') }
      if (r.status >= 500) throw new Error(`ARC ${r.status}`) // → WoC fallback
      const body = await r.json().catch(() => ({}))
      const st = String(body.txStatus ?? '').toUpperCase()
      if (r.ok && st !== 'REJECTED' && st !== 'DOUBLE_SPEND_ATTEMPTED') {
        return { status: 'success', txid: tx.id('hex') }
      }
      return { status: 'error', description: `ARC ${st || `HTTP ${r.status}`}: ${body.extraInfo ?? body.detail ?? ''}`.trim() }
    } catch (e) {
      // network error / 5xx → WoC fallback (policy rejections already returned)
      try {
        const wh: Record<string, string> = { 'Content-Type': 'application/json' }
        if (this.cfg.wocApiKey) wh['woc-api-key'] = this.cfg.wocApiKey
        const wr = await req(this.cfg, `${wocUrl}/tx/raw`, { method: 'POST', headers: wh, body: JSON.stringify({ txhex: tx.toHex() }) })
        if (!wr.ok) return { status: 'error', description: `WoC HTTP ${wr.status}: ${await wr.text().catch(() => '')}`.slice(0, 200) }
        return { status: 'success', txid: tx.id('hex') }
      } catch (we) {
        return { status: 'error', description: `broadcast failed: ARC ${(e as Error).message}; WoC ${(we as Error).message}` }
      }
    }
  }
}

/** ARC /v1/policy → mining fee in sat/kB (ceil). */
export async function fetchArcFeeRate (cfg?: MainnetConfig): Promise<number> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.arcApiKey) h.Authorization = `Bearer ${c.arcApiKey}`
  const p = await j(await req(c, `${c.arcUrl}/v1/policy`, { headers: h }))
  const f = p.policy?.miningFee ?? p.miningFee
  return Math.ceil((f.satoshis * 1000) / f.bytes)
}

// --- WoC tracker (rate-limited, cached headers) -------------------------------
export class WocTracker implements ChainTracker {
  private cfg
  private headers = new Map<number, BlockHeader>()
  private lastReq = 0
  constructor (c?: MainnetConfig) { this.cfg = def(c) }

  private async wocGet (path: string, retries = 3): Promise<any> {
    for (let i = 0; i <= retries; i++) {
      // ≥350 ms spacing — WoC is ~3 req/s unauthenticated
      const wait = 350 - (Date.now() - this.lastReq)
      if (wait > 0) await new Promise(r => setTimeout(r, wait))
      const h: Record<string, string> = {}
      if (this.cfg.wocApiKey) h['woc-api-key'] = this.cfg.wocApiKey
      this.lastReq = Date.now()
      const r = await req(this.cfg, `${this.cfg.wocUrl}${path}`, { headers: h }, 15_000)
      if (r.status === 429 && i < retries) { await new Promise(rs => setTimeout(rs, 1200 * (i + 1))); continue }
      if (r.status === 404) return undefined
      return await j(r)
    }
  }

  async currentHeight (): Promise<number> { return (await this.wocGet('/chain/info')).blocks }
  async tipHeight (): Promise<number> { return await this.currentHeight() }

  async headerAtHeight (height: number): Promise<BlockHeader | undefined> {
    const hit = this.headers.get(height); if (hit) return hit
    const b = await this.wocGet(`/block/height/${height}`)
    if (!b) return undefined
    const h: BlockHeader = {
      height, hash: b.hash, prevHash: b.previousblockhash ?? '',
      merkleRoot: b.merkleroot, time: b.time, bits: parseInt(String(b.bits), 16) || 0, nonce: b.nonce ?? 0
    }
    this.headers.set(height, h)
    return h
  }

  async isValidRootForHeight (root: string, height: number, signal?: AbortSignal): Promise<boolean> {
    const h = await this.headerAtHeight(height)
    return h?.merkleRoot === root
  }
}

// --- merkle paths -------------------------------------------------------------
/** TSC proof JSON → SDK MerklePath. nodes['*'] = duplicate of the neighbour
 *  (odd leaf self-pair). Pure + exported for tests. */
export function tscToMerklePath (tsc: { index: number, txOrId: string, target: string, nodes: (string | null)[] | null }, txid: string, blockHeight: number): MerklePath {
  // SDK MerklePath rows: each level lists the working node (level 0 = the txid
  // leaf) plus its sibling; '*' in TSC = odd-leaf self-pair → duplicate.
  // TSC `nodes` may be null entirely (single-tx block — common on TTN):
  // offset 0 leaf alone is the whole path.
  const nodes = tsc.nodes ?? []
  let idx = nodes.length === 0 ? 0 : tsc.index
  const path: { offset: number, hash?: string, txid?: boolean, duplicate?: boolean }[][] = []
  for (let lvl = 0; lvl < nodes.length; lvl++) {
    const row: { offset: number, hash?: string, txid?: boolean, duplicate?: boolean }[] = []
    if (lvl === 0) row.push({ offset: idx, hash: txid, txid: true })
    const raw = nodes[lvl]!
    if (raw === '*' || raw === null) row.push({ offset: idx % 2 === 0 ? idx + 1 : idx - 1, duplicate: true })
    else row.push({ offset: idx % 2 === 0 ? idx + 1 : idx - 1, hash: raw })
    path.push(row)
    idx = Math.floor(idx / 2)
  }
  if (path.length === 0) path.push([{ offset: 0, hash: txid, txid: true }]) // single-tx block
  return new MerklePath(blockHeight, path)
}

export type PathResult = { merklePath: MerklePath, status: string }

/** ARC /v1/tx (BUMP hex) → MerklePath; else WoC TSC proof → MerklePath. */
export async function merklePathMainnet (cfg: MainnetConfig | undefined, txid: string): Promise<PathResult | null> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.arcApiKey) h.Authorization = `Bearer ${c.arcApiKey}`
  let ar = await req(c, `${c.arcUrl}${txPathFor(c.arcUrl)}/${txid}`, { headers: h })
  if (ar.status === 404 && txPathFor(c.arcUrl) === '/v1/tx') {
    const alt = await req(c, `${c.arcUrl}/tx/${txid}`, { headers: h })
    if (alt.status !== 404 || (await alt.clone().text()).includes('transaction not found')) { txPaths.set(c.arcUrl, '/tx'); ar = alt }
  }
  if (ar.ok) {
    const b = await ar.json()
    if (b.merklePath) {
      try { return { merklePath: MerklePath.fromHex(b.merklePath), status: String(b.txStatus ?? 'mined') } } catch { /* fall through to WoC */ }
    }
    if (b.txStatus && String(b.txStatus).toUpperCase() !== 'MINED' && String(b.txStatus).toUpperCase() !== 'CONFIRMED') {
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
async function getOk (c: ReturnType<typeof def>, url: string, headers: Record<string, string>): Promise<Response> {
  for (let i = 0; ; i++) {
    const r = await req(c, url, { headers })
    if (r.ok) return r
    if ((r.status === 429 || r.status >= 500) && i < 4) { await new Promise(res => setTimeout(res, 800 * 2 ** i)); continue }
    throw new Error(`${url.split('/').slice(-3).join('/')}: HTTP ${r.status}`)
  }
}

// --- funding import by txid (address indexes can be unavailable, e.g. WoC-TTN) ---
/** fetch a funding tx and return its P2PKH outputs locked to `pkhHex` */
export async function importFundingTx (cfg: MainnetConfig | undefined, ref: string, pkhHex: string): Promise<FundingOut[]> {
  // 'hex:<rawtx>' = fully offline import — the operator hands us the funding
  // transaction itself, no network fetch and no index involved.
  if (ref.startsWith('hex:')) return fundingOutputs(Transaction.fromHex(ref.slice(4)), pkhHex)
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.wocApiKey) h['woc-api-key'] = c.wocApiKey
  const r = await getOk(c, `${c.wocUrl}/tx/${ref}/hex`, h)
  return fundingOutputs(Transaction.fromHex((await r.text()).trim()), pkhHex)
}

function fundingOutputs (tx: Transaction, pkhHex: string): FundingOut[] {
  const want = `76a914${pkhHex}88ac`
  return tx.outputs.flatMap((o, vout) => o.lockingScript.toHex() === want ? [{ tx, vout, satoshis: o.satoshis ?? 0 }] : [])
}

// --- UTXO discovery -----------------------------------------------------------
export async function discoverUtxos (cfg: MainnetConfig | undefined, address: string): Promise<FundingOut[]> {
  const c = def(cfg)
  const h: Record<string, string> = {}
  if (c.wocApiKey) h['woc-api-key'] = c.wocApiKey
  const list = await (await getOk(c, `${c.wocUrl}/address/${address}/unspent`, h)).json()
  const out: FundingOut[] = []
  for (const u of list) {
    await new Promise(res => setTimeout(res, 350))
    // a just-broadcast tx may not be served as hex yet (404): skip it this round
    const hex = await getOk(c, `${c.wocUrl}/tx/${u.tx_hash}/hex`, h).then(r => r.text()).catch(() => null)
    if (hex === null) continue
    out.push({ tx: Transaction.fromHex(hex.trim()), vout: u.tx_pos, satoshis: u.value })
  }
  return out
}
