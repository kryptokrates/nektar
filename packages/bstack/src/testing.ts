import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { Transaction, Utils, type ChainTracker } from '@bsv/sdk'
import { createNektarModule } from './module.js'

export class FakeTracker implements ChainTracker {
  ok = true
  validRoots = new Set<string>()
  strict = false
  async isValidRootForHeight (root: string): Promise<boolean> { return this.ok && (!this.strict || this.validRoots.has(root)) }
  async currentHeight (): Promise<number> { return 1 }
}

export interface MinihostOpts { tracker: ChainTracker, evil?: boolean, failNext?: () => boolean, drop?: (txid: string) => boolean, office?: string, corrupt?: boolean, port?: number, now?: () => number }
export interface MinihostHandle { url: string, close: () => Promise<void>, has: (txid: string) => boolean, txids: () => string[], raw: () => number[][] }
export async function minihost (nets: string[], opts: MinihostOpts | FakeTracker): Promise<MinihostHandle> {
  const tracker: ChainTracker = 'tracker' in opts ? opts.tracker : opts
  const evil = 'evil' in opts ? (opts.evil ?? false) : false
  const failNext = 'failNext' in opts ? opts.failNext : undefined
  const drop = 'drop' in opts ? opts.drop : undefined
  const office = 'office' in opts ? opts.office : undefined
  const corrupt = 'corrupt' in opts ? (opts.corrupt ?? false) : false
  const port = 'port' in opts ? opts.port : undefined
  const now = 'now' in opts ? opts.now : undefined
  const host = { log: () => {}, chainTracker: tracker as unknown as ChainTracker, engine: { submit: async () => ({}) } }
  const { topics, lookups } = createNektarModule(host as never, nets, now !== undefined ? { now } : undefined)
  const stored = new Map<string, number[]>()
  // ls_bbox below is a spec-shaped double: it implements only {office,to} and
  // inbox-after; admission on tm_bbox_<office> runs the REAL codec admit() verdict,
  // and a recipient receipt removes its acked commitments from the inbox index.
  const boxIndex = new Map<string, Array<{ txid: string, created: number, expires: number, commitment: string }>>()
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
          if (office !== undefined && t === `tm_bbox_${office}`) {
            const beefArr = Array.from(body)
            const adm = await (await import('@lightwebinc/bbox-host/codec/admit.js')).admit(new Uint8Array(beefArr), [], { office, headers: tracker })
            const { decodeEnvelope, decodeReceipt } = await import('@lightwebinc/bbox-host/codec/boxrec.js')
            if (adm.kind === 'envelope') {
              const env = decodeEnvelope(new Uint8Array(adm.carrier!.record))
              const txid = Utils.toHex(Array.from(adm.txid))
              stored.set(txid, beefArr)
              const k = Utils.toHex(Array.from(env.to))
              if (!boxIndex.has(k)) boxIndex.set(k, [])
              boxIndex.get(k)!.push({ txid, created: env.created, expires: env.expires, commitment: Utils.toHex(Array.from(adm.carrier!.commitment)) })
              admitted = admitted.concat(adm.outputs)
              continue
            }
            if (adm.kind === 'receipt') {
              const rec = decodeReceipt(new Uint8Array(adm.carrier!.record))
              const byHex = Utils.toHex(Array.from(rec.by))
              stored.set(Utils.toHex(Array.from(adm.txid)), beefArr)
              const list = boxIndex.get(byHex)
              if (list !== undefined) {
                const acks = new Set(rec.acks.map(a => Utils.toHex(Array.from(a))))
                boxIndex.set(byHex, list.filter(e => !acks.has(e.commitment)))
              }
              admitted = admitted.concat(adm.outputs)
              continue
            }
            throw new Error(`box admit kind ${adm.kind}`)
          }
          if (topics[t] === undefined) { res.writeHead(400, { 'content-type': 'application/json' }); res.end(JSON.stringify({ error: `unknown topic ${t}` })); return }
          const r = await topics[t]!.identifyAdmissibleOutputs(beef, [])
          admitted = admitted.concat(r.outputsToAdmit)
          try { stored.set(Transaction.fromAtomicBEEF(beef).id('hex'), beef) } catch { /* */ }
          for (const o of r.outputsToAdmit) await lookups['ls_nektar']!.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: beef, outputIndex: o, topic: t })
        }
        res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify(admitted.length ? { [names[0]]: { outputsToAdmit: admitted } } : {}))
      } else if (req.method === 'POST' && req.url === '/lookup') {
        const q = JSON.parse(body.toString('utf8'))
        let a: Array<{ txid: string, outputIndex: number }>
        if (q.service === 'ls_bbox') {
          const want = q.query as Record<string, string>
          const names = Object.keys(want).sort().join(',')
          if (names !== 'office,to' && names !== 'after,office,to') throw new Error('box double: only inbox questions')
          if (office === undefined || want.office !== office) throw new Error('box double: unknown office')
          a = (boxIndex.get(want.to) ?? []).filter(e => e.expires === 0 || e.expires > Math.floor(Date.now() / 1000)).map(e => ({ txid: e.txid, outputIndex: 0 }))
        } else a = evil
          ? [...stored.keys()].map(txid => ({ txid, outputIndex: 0 }))
          : await lookups[q.service]!.lookup(q) as Array<{ txid: string, outputIndex: number }>
        if (drop !== undefined) a = a.filter(x => !drop(x.txid))
        res.writeHead(200, { 'content-type': 'application/json' }); res.end(JSON.stringify({ type: 'output-list', outputs: a.map(o => { const b = [...(stored.get(o.txid) ?? [])]; if (corrupt && b.length > 10) b[10] = b[10]! ^ 0xff; return { beef: b, outputIndex: o.outputIndex } }) }))
      } else { res.writeHead(404); res.end() }
    } catch (e) { res.writeHead(500); res.end(String((e as Error).message)) }
  })
  await new Promise<void>(r => srv.listen(port ?? 0, '127.0.0.1', r))
  const { port: bound } = srv.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${bound}`,
    close: async () => { await new Promise<void>(r => srv.close(() => r())) },
    has: (txid: string) => stored.has(txid),
    txids: () => [...stored.keys()],
    raw: () => [...stored.values()]
  }
}
