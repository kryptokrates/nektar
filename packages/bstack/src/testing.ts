import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { Transaction, type ChainTracker } from '@bsv/sdk'
import { createNektarModule } from './module.js'

export class FakeTracker implements ChainTracker {
  ok = true
  validRoots = new Set<string>()
  strict = false
  async isValidRootForHeight (root: string): Promise<boolean> { return this.ok && (!this.strict || this.validRoots.has(root)) }
  async currentHeight (): Promise<number> { return 1 }
}

export interface MinihostOpts { tracker: ChainTracker, evil?: boolean, failNext?: () => boolean, drop?: (txid: string) => boolean }
export interface MinihostHandle { url: string, close: () => Promise<void>, has: (txid: string) => boolean, txids: () => string[] }
export async function minihost (nets: string[], opts: MinihostOpts | FakeTracker): Promise<MinihostHandle> {
  const tracker: ChainTracker = 'tracker' in opts ? opts.tracker : opts
  const evil = 'evil' in opts ? (opts.evil ?? false) : false
  const failNext = 'failNext' in opts ? opts.failNext : undefined
  const drop = 'drop' in opts ? opts.drop : undefined
  const host = { log: () => {}, chainTracker: tracker as unknown as ChainTracker, engine: { submit: async () => ({}) } }
  const { topics, lookups } = createNektarModule(host as never, nets)
  const stored = new Map<string, number[]>()
  const srv = http.createServer(async (req, res) => {
    const chunks: Buffer[] = []
    req.on('data', c => chunks.push(c))
    await new Promise(r => req.on('end', r))
    const body = Buffer.concat(chunks)
    try {
      if (req.method === 'POST' && req.url === '/submit') {
        if (failNext !== undefined && failNext()) { res.writeHead(503); res.end('down'); return }
        const names = JSON.parse(String(req.headers['x-topics'] ?? '[]'))
        const beef = Array.from(body)
        let admitted: number[] = []
        for (const t of names) {
          if (topics[t] === undefined) { res.writeHead(400, { 'content-type': 'application/json' }); res.end(JSON.stringify({ error: `unknown topic ${t}` })); return }
          const r = await topics[t]!.identifyAdmissibleOutputs(beef, [])
          admitted = admitted.concat(r.outputsToAdmit)
          try { stored.set(Transaction.fromAtomicBEEF(beef).id('hex'), beef) } catch { /* */ }
          for (const o of r.outputsToAdmit) await lookups['ls_nektar']!.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: beef, outputIndex: o, topic: t })
        }
        res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(admitted.length ? { [names[0]]: { outputsToAdmit: admitted } } : {}))
      } else if (req.method === 'POST' && req.url === '/lookup') {
        const q = JSON.parse(body.toString('utf8'))
        let a = evil
          ? [...stored.keys()].map(txid => ({ txid, outputIndex: 0 }))
          : await lookups[q.service]!.lookup(q) as Array<{ txid: string, outputIndex: number }>
        if (drop !== undefined) a = a.filter(x => !drop(x.txid))
        res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ type: 'output-list', outputs: a.map(o => ({ beef: stored.get(o.txid) ?? [], outputIndex: o.outputIndex })) }))
      } else { res.writeHead(404); res.end() }
    } catch (e) { res.writeHead(500); res.end(String((e as Error).message)) }
  })
  await new Promise<void>(r => srv.listen(0, '127.0.0.1', r))
  const { port } = srv.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${port}`,
    close: async () => { await new Promise<void>(r => srv.close(() => r())) },
    has: (txid: string) => stored.has(txid),
    txids: () => [...stored.keys()]
  }
}
