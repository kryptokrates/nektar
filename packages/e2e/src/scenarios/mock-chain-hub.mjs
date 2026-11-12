// Shared MockChain over HTTP — lets scenario agents run as real OS processes
// on the mock lane while sharing one chain view. No funds, no network.
import { createServer } from 'node:http'
import { Transaction } from '@bsv/sdk'
import { MockChain } from '../../../chain/dist/src/index.js'

const chain = new MockChain()
chain.verifyScripts = true
const json = (res, code, obj) => {
  res.writeHead(code, { 'content-type': 'application/json' })
  res.end(JSON.stringify(obj))
}
const body = (req) =>
  new Promise((resolve, reject) => {
    const chunks = []
    req.on('data', (c) => chunks.push(c))
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')))
    req.on('error', reject)
  })

export function startHub(port = 0) {
  const srv = createServer(async (req, res) => {
    try {
      const url = new URL(req.url ?? '/', 'http://x')
      if (req.method === 'GET' && url.pathname === '/height') return json(res, 200, { height: await chain.currentHeight(), tip: await chain.tipHeight() })
      if (req.method === 'GET' && url.pathname === '/header')
        return json(res, 200, { header: (await chain.headerAtHeight(Number(url.searchParams.get('h')))) ?? null })
      if (req.method === 'GET' && url.pathname === '/tx') {
        const tx = chain.getTx(url.searchParams.get('txid') ?? '')
        return json(res, 200, { txHex: tx?.toHex() ?? null })
      }
      if (req.method === 'GET' && url.pathname === '/merkle') {
        const mp = chain.merklePathOf(url.searchParams.get('txid') ?? '')
        return json(res, 200, { path: mp ? { blockHeight: mp.blockHeight, path: mp.path } : null })
      }
      if (req.method === 'POST' && url.pathname === '/register-parent') {
        const { hex } = JSON.parse(await body(req))
        chain.registerParent(Transaction.fromHex(hex))
        return json(res, 200, { ok: true })
      }
      if (req.method === 'POST' && url.pathname === '/broadcast') {
        const { hex } = JSON.parse(await body(req))
        const r = await chain.broadcast(Transaction.fromHex(hex))
        return json(res, 200, r)
      }
      if (req.method === 'POST' && url.pathname === '/root') {
        const { root, height } = JSON.parse(await body(req))
        return json(res, 200, { ok: await chain.isValidRootForHeight(root, height) })
      }
      if (req.method === 'POST' && url.pathname === '/mine') {
        const { count = 1 } = JSON.parse(await body(req))
        return json(res, 200, { headers: chain.mine(count) })
      }
      return json(res, 404, { error: 'not found' })
    } catch (e) {
      return json(res, 500, { error: e.message })
    }
  })
  return new Promise((resolve) => srv.listen(port, '127.0.0.1', () => resolve({ srv, port: srv.address().port, chain })))
}
