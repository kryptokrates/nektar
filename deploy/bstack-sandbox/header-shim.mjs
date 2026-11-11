// header-shim.mjs — chain header/asset bridge for overlay hosts on Teranode regtest.
//
// Reference hosts and the nektar/blogs/finger/bbox modules ask a ChainTracker
//   GET /v1/tip          -> { height }
//   GET /v1/root/<h>     -> { merkleRoot }   (404 when the height is unknown)
// and the (priced-payment) bbox gate asks the node asset API
//   GET /api/v1/utxos/<txid>/json    -> [{vout,status,...}]
//   GET /api/v1/merkle_proof/<txid>  -> binary MerklePath (404 while unmined)
//
// The first two translate to Teranode's legacy RPC (getblockchaininfo /
// getblockhash / getblockheader); the asset paths proxy to Teranode's asset
// HTTP API with the RPC basic-auth credentials injected (the host calls it
// unauthenticated). Nothing else is emulated: unknown paths answer 404.
//
// env: SHIM_PORT (8080), SHIM_RPC_URL (http://host.docker.internal:9292/),
//      SHIM_RPC_USER, SHIM_RPC_PASS, SHIM_ASSET_URL
//      (http://host.docker.internal:8090/api/v1)
import { createServer } from 'node:http'

const PORT = Number(process.env.SHIM_PORT ?? 8080)
const RPC = (process.env.SHIM_RPC_URL ?? 'http://host.docker.internal:9292/').replace(/\/+$/, '')
const USER = process.env.SHIM_RPC_USER ?? ''
const PASS = process.env.SHIM_RPC_PASS ?? ''
const ASSET = (process.env.SHIM_ASSET_URL ?? 'http://host.docker.internal:8090/api/v1').replace(/\/+$/, '')
const AUTH = 'Basic ' + Buffer.from(`${USER}:${PASS}`).toString('base64')
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
      const info = await rpc('getblockchaininfo')
      json(res, 200, { height: info.blocks })
      return
    }
    let m = /^\/v1\/root\/(\d+)$/.exec(url.pathname)
    if (m !== null) {
      const hash = await rpc('getblockhash', Number(m[1])).catch(e => {
        if (e.code === -5 || e.code === -8) return undefined // block not found / height out of range
        throw e
      })
      if (hash === undefined) { json(res, 404, { error: 'unknown height' }); return }
      const h = await rpc('getblockheader', hash)
      json(res, 200, { merkleRoot: h.merkleroot })
      return
    }
    m = /^\/api\/v1\/(utxos\/[0-9a-fA-F]{64}\/json|merkle_proof\/[0-9a-fA-F]{64})$/.exec(url.pathname)
    if (m !== null) {
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
  .listen(PORT, () => console.log(`header-shim on :${PORT} -> ${RPC} / ${ASSET}`))
