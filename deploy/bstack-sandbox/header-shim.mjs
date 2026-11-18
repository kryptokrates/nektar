// header-shim.mjs — chain header/asset bridge for overlay hosts.
//   GET /v1/tip                    -> { height }
//   GET /v1/root/<h>               -> { merkleRoot }   (404 = unknown height)
//   GET /api/v1/utxos/<txid>/json  -> [{vout,status,spendingData?}]
//   GET /api/v1/merkle_proof/<txid>-> binary MerklePath (404 while unmined)
// SHIM_MODE=regtest (default): headers via Teranode legacy RPC; asset paths
// proxied to the Teranode asset API with basic-auth injected.
// SHIM_MODE=live (testnet): headers from the broadcaster's chaintracks
// (/height, /header/height/<h>) AND the chain index — the two are independent
// sources and must AGREE before an answer is served (this shim is the header
// anchor behind host-side SPV admission, and the same shim serves the merkle
// proofs those roots check: first-success-wins would make one lying source a
// forged-root oracle). Disagreement or silence from either -> 404; set
// SHIM_SINGLE_OK=1 to serve a lone source's answer (non-strict, logged).
// proofs from broadcaster GET /tx/<txid> merklePath (BUMP hex) or the index's
// TSC proof; the utxos view is assembled per-output from the index's tx
// record (outputs[].spent/spent_txid — true=SPENT, false=OK, absent=NOT_FOUND).
// env: SHIM_PORT (8080), SHIM_MODE (regtest|live); regtest: SHIM_RPC_URL,
// SHIM_RPC_USER, SHIM_RPC_PASS, SHIM_ASSET_URL; live: SHIM_BROADCASTER,
// SHIM_INDEX, SHIM_CHAINTRACKS (default $SHIM_BROADCASTER/chaintracks/v2),
// SHIM_SINGLE_OK=1 to allow uncorroborated live answers
import { createServer } from 'node:http'

const PORT = Number(process.env.SHIM_PORT ?? 8080)
const MODE = process.env.SHIM_MODE ?? 'regtest'
const RPC = (process.env.SHIM_RPC_URL ?? 'http://host.docker.internal:9292/').replace(/\/+$/, '')
const USER = process.env.SHIM_RPC_USER ?? ''
const PASS = process.env.SHIM_RPC_PASS ?? ''
const ASSET = (process.env.SHIM_ASSET_URL ?? 'http://host.docker.internal:8090/api/v1').replace(/\/+$/, '')
const AUTH = 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64')
const BCAST = (process.env.SHIM_BROADCASTER ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech').replace(/\/+$/, '')
const INDEX = (process.env.SHIM_INDEX ?? 'https://test.bananablocks.com/api/v1').replace(/\/+$/, '')
const CT = (process.env.SHIM_CHAINTRACKS ?? `${BCAST}/chaintracks/v2`).replace(/\/+$/, '')
// live header answers are only as strong as their corroboration: with both
// sources configured an answer must agree across them; a lone-source answer
// is a 404 unless the operator explicitly opts in to non-strict service
const SINGLE_OK = process.env.SHIM_SINGLE_OK === '1'
const ctJson = async (path) =>
  await fetch(`${CT}${path}`, { signal: AbortSignal.timeout(15_000) })
    .then((r) => (r.ok ? r.json() : undefined))
    .catch(() => undefined)
let rpcId = 0

async function rpcOnce(method, ...params) {
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
// retry transport/5xx-style failures (never a "not found" answer). Refused
// connections are cheap, and observed blackouts run ~30-120s — cap the
// backoff and give transport failures real coverage instead of ~6s.
async function rpc(method, ...params) {
  let last
  for (let t = 0; t < 60; t++) {
    try {
      return await rpcOnce(method, ...params)
    } catch (e) {
      if (e.code === -5 || e.code === -8) throw e
      last = e
      console.log(`${new Date().toISOString()} rpc ${method}${params.length ? ' ' + params : ''} retry ${t + 1}: ${String(e.message).slice(0, 160)}`)
      await new Promise((r) => setTimeout(r, Math.min(400 * (t + 1), 3000)))
    }
  }
  throw last
}

// free index tiers rate-limit — space reads and retry 429/5xx; 404 answers
// undefined (never an error — absence is an answer, not a failure)
let idxLast = 0
async function idxGet(path, retries = 4) {
  for (let i = 0; i <= retries; i++) {
    const wait = 300 - (Date.now() - idxLast)
    if (wait > 0) await new Promise((r) => setTimeout(r, wait))
    idxLast = Date.now()
    const r = await fetch(`${INDEX}${path}`, { signal: AbortSignal.timeout(15_000) }).catch(() => undefined)
    if (r === undefined) {
      if (i < retries) {
        await new Promise((rs) => setTimeout(rs, 1200 * (i + 1)))
        continue
      }
      return undefined
    }
    if ((r.status === 429 || r.status >= 500) && i < retries) {
      await new Promise((rs) => setTimeout(rs, 1200 * (i + 1)))
      continue
    }
    if (r.status === 404 || r.status === 400) return undefined
    if (!r.ok) throw new Error(`index ${path}: HTTP ${r.status}`)
    return await r.json()
  }
  return undefined
}

// bitcoin varint (BUMP leaf count/offset/block-height encoding)
function varint(n) {
  if (n < 0xfd) return [n]
  if (n <= 0xffff) return [0xfd, n & 0xff, n >> 8]
  if (n <= 0xffffffff) {
    const b = Buffer.alloc(5)
    b[0] = 0xfe
    b.writeUInt32LE(n, 1)
    return [...b]
  }
  const b = Buffer.alloc(9)
  b[0] = 0xff
  b.writeBigUInt64LE(BigInt(n), 1)
  return [...b]
}

// TSC proof JSON → BUMP binary (sdk MerklePath.toWriter): nodes['*'] is an
// odd-leaf self-pair (duplicate → flags 1, no hash); the level-0 working leaf
// carries the txid flag (2). nodes may be null in a single-transaction block.
function tscToBump(tsc, txid, blockHeight) {
  const nodes = tsc.nodes ?? []
  let idx = nodes.length === 0 ? 0 : tsc.index
  const rows = []
  for (let lvl = 0; lvl < nodes.length; lvl++) {
    const row = []
    if (lvl === 0) row.push({ offset: idx, hash: txid, flags: 2 })
    const raw = nodes[lvl]
    row.push(
      raw === '*' || raw === null ? { offset: idx % 2 === 0 ? idx + 1 : idx - 1, flags: 1 } : { offset: idx % 2 === 0 ? idx + 1 : idx - 1, hash: raw, flags: 0 }
    )
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

// live mode: txid → binary BUMP from the broadcaster, else the index TSC proof
async function liveProof(txid) {
  const r = await fetch(`${BCAST}/tx/${txid}`, { signal: AbortSignal.timeout(15_000) })
  if (r.ok) {
    const b = await r.json().catch(() => ({}))
    if (typeof b.merklePath === 'string' && b.merklePath.length > 0) return Buffer.from(b.merklePath, 'hex')
    if (!['MINED', 'IMMUTABLE'].includes(String(b.txStatus ?? '').toUpperCase())) return undefined
  }
  const t = await idxGet(`/tx/${txid}`)
  if (!t?.block_height || t.block_height <= 0) return undefined
  const p = await idxGet(`/tx/${txid}/proof`)
  if (!p || !Array.isArray(p.nodes)) return undefined
  return tscToBump(p, txid, t.block_height)
}

// live mode: per-output spend view — one index tx read answers every output
// (bb serves spent=true + spent_txid per output; absent flag = undecided)
async function liveUtxos(txid) {
  const t = await idxGet(`/tx/${txid}`)
  if (t === undefined) return undefined
  const outs = []
  for (const [i, o] of (t.outputs ?? []).entries()) {
    const vout = o?.index ?? i
    if (o?.spent === true) outs.push({ txid, vout, status: 'SPENT', spendingData: { txId: o.spent_txid } })
    else if (o?.spent === false) outs.push({ txid, vout, status: 'OK' })
    else outs.push({ txid, vout, status: 'NOT_FOUND' }) // undecided — not evidence, per bcommon/nodeapi
  }
  return outs
}

const json = (res, code, body) => {
  const b = JSON.stringify(body)
  res.writeHead(code, { 'content-type': 'application/json' })
  res.end(b)
}

async function handle(req, res) {
  const url = new URL(req.url ?? '/', 'http://x')
  if (req.method !== 'GET') {
    json(res, 405, { error: 'method' })
    return
  }
  try {
    if (url.pathname === '/v1/tip') {
      if (MODE === 'live') {
        const [ct, idx] = await Promise.all([ctJson('/height'), idxGet('/block/tip')])
        const a = ct?.height
        const b = idx?.height
        // both answered: serve the lowest — the strongest claim EITHER source
        // corroborates (a skewed tip is propagation lag or a stuck source;
        // never report height one of them has not seen)
        const height = a !== undefined && b !== undefined ? Math.min(a, b) : SINGLE_OK ? (a ?? b) : undefined
        if (height === undefined) {
          if (a !== undefined || b !== undefined)
            console.log(`tip: only ${a !== undefined ? 'chaintracks' : 'index'} answered — refusing uncorroborated answer`)
          json(res, 404, { error: 'no corroborated tip' })
          return
        }
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
        const [ct, idx] = await Promise.all([ctJson(`/header/height/${h}`), idxGet(`/block/${h}`)])
        const a = ct?.merkleRoot
        const b = idx?.merkle_root
        // same chain -> same root: two independent answers that differ mean a
        // fork or a lying source — refuse; silence from one side corroborates
        // nothing either (SHIM_SINGLE_OK=1 serves a lone answer, logged)
        const root = a !== undefined && b !== undefined ? (a === b ? a : undefined) : SINGLE_OK ? (a ?? b) : undefined
        if (root === undefined) {
          if (a !== undefined && b !== undefined) console.log(`root ${h}: chaintracks ${a} != index ${b} — refusing`)
          else if (a !== undefined || b !== undefined)
            console.log(`root ${h}: only ${a !== undefined ? 'chaintracks' : 'index'} answered — refusing uncorroborated answer`)
          json(res, 404, { error: 'unknown height' })
          return
        }
        json(res, 200, { merkleRoot: root })
        return
      }
      const hash = await rpc('getblockhash', h).catch((e) => {
        if (e.code === -5 || e.code === -8) return undefined
        throw e
      })
      if (hash === undefined) {
        json(res, 404, { error: 'unknown height' })
        return
      }
      const hd = await rpc('getblockheader', hash)
      json(res, 200, { merkleRoot: hd.merkleroot })
      return
    }
    m = /^\/api\/v1\/merkle_proof\/([0-9a-fA-F]{64})$/.exec(url.pathname)
    if (m !== null && MODE === 'live') {
      const bump = await liveProof(m[1])
      if (bump === undefined) {
        json(res, 404, { error: 'no proof' })
        return
      }
      res.writeHead(200, { 'content-type': 'application/octet-stream' })
      res.end(bump)
      return
    }
    m = /^\/api\/v1\/utxos\/([0-9a-fA-F]{64})\/json$/.exec(url.pathname)
    if (m !== null && MODE === 'live') {
      const outs = await liveUtxos(m[1])
      if (outs === undefined) {
        json(res, 404, { error: 'unknown tx' })
        return
      }
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

createServer((req, res) => {
  handle(req, res).catch(() => {
    try {
      res.writeHead(500)
      res.end()
    } catch {
      /* closed */
    }
  })
}).listen(PORT, () => console.log(`header-shim on :${PORT} mode=${MODE} -> ${MODE === 'live' ? `${CT} / ${INDEX}` : `${RPC} / ${ASSET}`}`))
