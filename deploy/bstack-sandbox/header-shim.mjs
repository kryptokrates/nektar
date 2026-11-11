// header-shim.mjs — chain header/asset bridge for overlay hosts.
//   GET /v1/tip                    -> { height }
//   GET /v1/root/<h>               -> { merkleRoot }   (404 = unknown height)
//   GET /api/v1/utxos/<txid>/json  -> [{vout,status,spendingData?}]
//   GET /api/v1/merkle_proof/<txid>-> binary MerklePath (404 while unmined)
// SHIM_MODE=regtest (default): headers via Teranode legacy RPC; asset paths
// proxied to the Teranode asset API with basic-auth injected.
// SHIM_MODE=live (testnet): headers from the broadcaster's chaintracks
// (/height, /header/height/<h>) with WoC fallback; proofs from broadcaster
// GET /tx/<txid> merklePath (BUMP hex) or the WoC TSC proof; the utxos view is
// assembled per-output from WoC /tx/<txid>/<vout>/spent (200=SPENT, 404=OK).
// env: SHIM_PORT (8080), SHIM_MODE (regtest|live); regtest: SHIM_RPC_URL,
// SHIM_RPC_USER, SHIM_RPC_PASS, SHIM_ASSET_URL; live: SHIM_BROADCASTER,
// SHIM_WOC, SHIM_CHAINTRACKS (default $SHIM_BROADCASTER/chaintracks/v2)
import { createServer } from 'node:http'

const PORT = Number(process.env.SHIM_PORT ?? 8080)
const MODE = process.env.SHIM_MODE ?? 'regtest'
const RPC = (process.env.SHIM_RPC_URL ?? 'http://host.docker.internal:9292/').replace(/\/+$/, '')
const USER = process.env.SHIM_RPC_USER ?? ''
const PASS = process.env.SHIM_RPC_PASS ?? ''
const ASSET = (process.env.SHIM_ASSET_URL ?? 'http://host.docker.internal:8090/api/v1').replace(/\/+$/, '')
const AUTH = 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64')
const BCAST = (process.env.SHIM_BROADCASTER ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech').replace(/\/+$/, '')
const WOC = (process.env.SHIM_WOC ?? 'https://api.whatsonchain.com/v1/bsv/test').replace(/\/+$/, '')
const CT = (process.env.SHIM_CHAINTRACKS ?? `${BCAST}/chaintracks/v2`).replace(/\/+$/, '')
let rpcId = 0

async function rpcOnce (method, ...params) {
  const res = await fetch(`${RPC}/`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: AUTH },
    body: JSON.stringify({ jsonrpc: '1.0', id: ++rpcId, method, params }),
    signal: AbortSignal.timeout(15_000)
  })
  const j = await res.json()
  if (j.error) {
    const e = new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
    e.code = j.error.code
    throw e
  }
  return j.result
}

// Teranode's legacy RPC fails transiently while the 2s-block rig churns —
// retry transport/5xx-style failures (never a "not found" answer) a few times
async function rpc (method, ...params) {
  let last
  for (let t = 0; t < 6; t++) {
    try { return await rpcOnce(method, ...params) } catch (e) {
      if (e.code === -5 || e.code === -8) throw e
      last = e
      console.log(`${new Date().toISOString()} rpc ${method}${params.length ? ' ' + params : ''} retry ${t + 1}: ${String(e.message).slice(0, 160)}`)
      await new Promise(r => setTimeout(r, 400 * (t + 1)))
    }
  }
  throw last
}

// WoC free tier is ~3 req/s — space reads and retry 429s; 404 answers undefined
let wocLast = 0
async function wocGet (path, retries = 3) {
  for (let i = 0; i <= retries; i++) {
    const wait = 350 - (Date.now() - wocLast)
    if (wait > 0) await new Promise(r => setTimeout(r, wait))
    wocLast = Date.now()
    const r = await fetch(`${WOC}${path}`, { signal: AbortSignal.timeout(15_000) })
    if (r.status === 429 && i < retries) { await new Promise(rs => setTimeout(rs, 1200 * (i + 1))); continue }
    if (r.status === 404 || r.status === 400) return undefined
    if (!r.ok) throw new Error(`woc ${path}: HTTP ${r.status}`)
    return await r.json()
  }
  return undefined
}

// bitcoin varint (BUMP leaf count/offset/block-height encoding)
function varint (n) {
  if (n < 0xfd) return [n]
  if (n <= 0xffff) return [0xfd, n & 0xff, n >> 8]
  if (n <= 0xffffffff) { const b = Buffer.alloc(5); b[0] = 0xfe; b.writeUInt32LE(n, 1); return [...b] }
  const b = Buffer.alloc(9); b[0] = 0xff; b.writeBigUInt64LE(BigInt(n), 1); return [...b]
}

// TSC proof JSON → BUMP binary (sdk MerklePath.toWriter): nodes['*'] is an
// odd-leaf self-pair (duplicate → flags 1, no hash); the level-0 working leaf
// carries the txid flag (2). nodes may be null in a single-transaction block.
function tscToBump (tsc, txid, blockHeight) {
  const nodes = tsc.nodes ?? []
  let idx = nodes.length === 0 ? 0 : tsc.index
  const rows = []
  for (let lvl = 0; lvl < nodes.length; lvl++) {
    const row = []
    if (lvl === 0) row.push({ offset: idx, hash: txid, flags: 2 })
    const raw = nodes[lvl]
    row.push(raw === '*' || raw === null
      ? { offset: idx % 2 === 0 ? idx + 1 : idx - 1, flags: 1 }
      : { offset: idx % 2 === 0 ? idx + 1 : idx - 1, hash: raw, flags: 0 })
    rows.push(row)
    idx = Math.floor(idx / 2)
  }
  if (rows.length === 0) rows.push([{ offset: 0, hash: txid, flags: 2 }])
  const out = [...varint(blockHeight), rows.length]
  for (const row of rows) {
    out.push(...varint(row.length))
    for (const leaf of row) {
      out.push(...varint(leaf.offset), leaf.flags)
      if ((leaf.flags & 1) === 0) out.push(...Buffer.from(leaf.hash, 'hex').reverse())
    }
  }
  return Buffer.from(out)
}

// live mode: txid → binary BUMP from the broadcaster, else the WoC TSC proof
async function liveProof (txid) {
  const r = await fetch(`${BCAST}/tx/${txid}`, { signal: AbortSignal.timeout(15_000) })
  if (r.ok) {
    const b = await r.json().catch(() => ({}))
    if (typeof b.merklePath === 'string' && b.merklePath.length > 0) return Buffer.from(b.merklePath, 'hex')
    if (!['MINED', 'IMMUTABLE'].includes(String(b.txStatus ?? '').toUpperCase())) return undefined
  }
  const t = await wocGet(`/tx/hash/${txid}`)
  if (!t?.blockheight || t.blockheight <= 0) return undefined
  const p = await wocGet(`/tx/${txid}/proof/tsc`)
  if (!p?.[0]) return undefined
  return tscToBump(p[0], txid, t.blockheight)
}

// live mode: per-output spend view — one WoC /spent read per output
async function liveUtxos (txid) {
  const t = await wocGet(`/tx/hash/${txid}`)
  if (t === undefined) return undefined
  const n = Array.isArray(t.vout) ? t.vout.length : 0
  const outs = []
  for (let vout = 0; vout < n; vout++) {
    const s = await fetch(`${WOC}/tx/${txid}/${vout}/spent`, { signal: AbortSignal.timeout(15_000) }).catch(() => undefined)
    if (s?.status === 200) {
      const j = await s.json().catch(() => ({}))
      outs.push({ txid, vout, status: 'SPENT', spendingData: { txId: j.txid } })
    } else if (s?.status === 404) outs.push({ txid, vout, status: 'OK' })
    else outs.push({ txid, vout, status: 'NOT_FOUND' }) // undecided — not evidence, per bcommon/nodeapi
  }
  return outs
}

const json = (res, code, body) => {
  const b = JSON.stringify(body)
  res.writeHead(code, { 'content-type': 'application/json' })
  res.end(b)
}

async function handle (req, res) {
  const url = new URL(req.url ?? '/', 'http://x')
  if (req.method !== 'GET') { json(res, 405, { error: 'method' }); return }
  try {
    if (url.pathname === '/v1/tip') {
      if (MODE === 'live') {
        const h = await fetch(`${CT}/height`, { signal: AbortSignal.timeout(15_000) }).then(r => r.ok ? r.json() : undefined).catch(() => undefined)
        const height = h?.height ?? (await wocGet('/chain/info'))?.blocks
        if (height === undefined) throw new Error('no header source answered')
        json(res, 200, { height })
        return
      }
      const info = await rpc('getblockchaininfo')
      json(res, 200, { height: info.blocks })
      return
    }
    let m = /^\/v1\/root\/(\d+)$/.exec(url.pathname)
    if (m !== null) {
      const h = Number(m[1])
      if (MODE === 'live') {
        const b = await fetch(`${CT}/header/height/${h}`, { signal: AbortSignal.timeout(15_000) }).then(r => r.ok ? r.json() : undefined).catch(() => undefined)
        const root = b?.merkleRoot ?? (await wocGet(`/block/height/${h}`))?.merkleroot
        if (root === undefined) { json(res, 404, { error: 'unknown height' }); return }
        json(res, 200, { merkleRoot: root })
        return
      }
      const hash = await rpc('getblockhash', h).catch(e => {
        if (e.code === -5 || e.code === -8) return undefined
        throw e
      })
      if (hash === undefined) { json(res, 404, { error: 'unknown height' }); return }
      const hd = await rpc('getblockheader', hash)
      json(res, 200, { merkleRoot: hd.merkleroot })
      return
    }
    m = /^\/api\/v1\/merkle_proof\/([0-9a-fA-F]{64})$/.exec(url.pathname)
    if (m !== null && MODE === 'live') {
      const bump = await liveProof(m[1])
      if (bump === undefined) { json(res, 404, { error: 'no proof' }); return }
      res.writeHead(200, { 'content-type': 'application/octet-stream' }); res.end(bump)
      return
    }
    m = /^\/api\/v1\/utxos\/([0-9a-fA-F]{64})\/json$/.exec(url.pathname)
    if (m !== null && MODE === 'live') {
      const outs = await liveUtxos(m[1])
      if (outs === undefined) { json(res, 404, { error: 'unknown tx' }); return }
      json(res, 200, outs)
      return
    }
    m = /^\/api\/v1\/(utxos\/[0-9a-fA-F]{64}\/json|merkle_proof\/[0-9a-fA-F]{64})$/.exec(url.pathname)
    if (m !== null && MODE !== 'live') {
      const up = await fetch(`${ASSET}/${m[1]}`, { headers: { Authorization: AUTH }, signal: AbortSignal.timeout(20_000) })
      res.writeHead(up.status, { 'content-type': up.headers.get('content-type') ?? 'application/octet-stream' })
      res.end(Buffer.from(await up.arrayBuffer()))
      return
    }
    json(res, 404, { error: 'not found' })
  } catch (e) {
    json(res, 502, { error: String((e ?? {}).message ?? e).slice(0, 200) })
  }
}

createServer((req, res) => { handle(req, res).catch(() => { try { res.writeHead(500); res.end() } catch { /* closed */ } }) })
  .listen(PORT, () => console.log(`header-shim on :${PORT} mode=${MODE} -> ${MODE === 'live' ? `${CT} / ${WOC}` : `${RPC} / ${ASSET}`}`))
