import http from 'node:http'
import type { AddressInfo } from 'node:net'
import { Transaction, Utils, type ChainTracker } from '@bsv/sdk'
import { createNektarModule } from './module.js'

export class FakeTracker implements ChainTracker {
  ok = true
  validRoots = new Set<string>()
  strict = false
  async isValidRootForHeight(root: string): Promise<boolean> {
    return this.ok && (!this.strict || this.validRoots.has(root))
  }
  async currentHeight(): Promise<number> {
    return 1
  }
}

export interface MinihostOpts {
  tracker: ChainTracker
  evil?: boolean
  failNext?: (topics?: string[]) => boolean
  drop?: (txid: string) => boolean
  office?: string
  corrupt?: boolean
  port?: number
  now?: () => number
  logStream?: string
  finger?: boolean
}
export interface MinihostHandle {
  url: string
  close: () => Promise<void>
  has: (txid: string) => boolean
  txids: () => string[]
  raw: () => number[][]
  envelopes: () => Array<{ to: string; txid: string }>
}
export async function minihost(nets: string[], opts: MinihostOpts | FakeTracker): Promise<MinihostHandle> {
  const tracker: ChainTracker = 'tracker' in opts ? opts.tracker : opts
  const evil = 'evil' in opts ? (opts.evil ?? false) : false
  const failNext = 'failNext' in opts ? opts.failNext : undefined
  const drop = 'drop' in opts ? opts.drop : undefined
  const office = 'office' in opts ? opts.office : undefined
  const logStream = 'logStream' in opts ? opts.logStream : undefined
  const finger = 'finger' in opts ? (opts.finger ?? false) : false
  const corrupt = 'corrupt' in opts ? (opts.corrupt ?? false) : false
  const port = 'port' in opts ? opts.port : undefined
  const now = 'now' in opts ? opts.now : undefined
  const host = { log: () => {}, chainTracker: tracker, engine: { submit: async () => ({}) } }
  const { topics, lookups } = createNektarModule(host, nets, now !== undefined ? { now } : undefined)
  const stored = new Map<string, number[]>()
  // ls_bbox below is a spec-shaped double: it implements only {office,to} and
  // inbox-after; admission on tm_bbox_<office> runs the REAL codec admit() verdict,
  // and a recipient receipt removes its acked commitments from the inbox index.
  const boxIndex = new Map<string, Array<{ txid: string; created: number; expires: number; commitment: string }>>()
  const boxEnvelopes: Array<{ to: string; txid: string }> = []
  // tm_log/ls_log are the REAL upstream module (blogsModule wires the topic
  // manager to the lookup service's coin view): admission is the reference
  // verdict and every free question class behaves exactly as on a
  // reference host. What this double does not implement: retention passes,
  // priced question classes, engine-side storage/restore, and GASP catch-up.
  let logTM: { identifyAdmissibleOutputs: (beef: number[], coins: number[]) => Promise<{ outputsToAdmit: number[]; coinsToRetain: number[] }> } | undefined
  let logLS:
    | {
        coin: (topic: string, txid: string, vout: number) => Promise<unknown>
        outputAdmittedByTopic: (p: { mode: string; atomicBEEF: number[]; outputIndex: number; topic: string }) => Promise<void> | void
        lookup: (q: unknown) => Promise<Array<{ txid: string; outputIndex: number }>>
      }
    | undefined
  if (logStream !== undefined) {
    const mod = await import('@lightwebinc/blogs-host/dist/module.js')
    const mhost = { metrics: { preset: () => {}, inc: () => {}, gauge: () => {} }, log: () => {}, chainTracker: tracker }
    const m = mod.blogsModule(mhost, [{ stream: logStream, policy: { open: true } }], [`tm_log_${logStream}`])
    logTM = m.topics![`tm_log_${logStream}`] as never
    logLS = m.lookups!['ls_log'] as never
  }
  // tm_finger runs the REAL upstream admission verdict, and ls_finger is the
  // REAL upstream join: admitted outputs and spends are fed to the vendored
  // FingerLookupService, so {identityKey}, {identityKey,pending} and
  // {identityKey,carrier} behave exactly as on a reference host. What this
  // double does not implement: priced question classes, engine-side storage
  // retention/eviction, and GASP catch-up.
  const fingerHeld = new Set<string>()
  let fingerTM: { identifyAdmissibleOutputs: (beef: number[], coins: number[]) => Promise<{ outputsToAdmit: number[]; coinsToRetain: number[] }> } | undefined
  let fingerLS:
    | {
        outputAdmittedByTopic: (p: { mode: string; atomicBEEF: number[]; txid: string; outputIndex: number; topic: string }) => void
        outputSpent: (p: { mode: string; txid: string; outputIndex: number; spendingTxid: string; topic: string }) => void
        lookup: (q: unknown) => Promise<Array<{ txid: string; outputIndex: number }>>
      }
    | undefined
  if (finger) {
    const mod = await import('@lightwebinc/bfinger-host/dist/index.js')
    const mhost = { metrics: { preset: () => {}, inc: () => {}, gauge: () => {} }, log: () => {}, chainTracker: tracker }
    fingerTM = new mod.FingerTopicManager(mhost) as never
    fingerLS = new mod.FingerLookupService(mhost) as never
  }
  const srv = http.createServer(async (req, res) => {
    const chunks: Buffer[] = []
    req.on('data', (c) => chunks.push(c))
    await new Promise((r) => req.on('end', r))
    const body = Buffer.concat(chunks)
    try {
      if (req.method === 'POST' && req.url === '/submit') {
        const names = JSON.parse(String(req.headers['x-topics'] ?? '[]'))
        if (failNext !== undefined && failNext(names)) {
          res.writeHead(503)
          res.end('down')
          return
        }
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
              boxEnvelopes.push({ to: k, txid })
              admitted = admitted.concat(adm.outputs)
              continue
            }
            if (adm.kind === 'receipt') {
              const rec = decodeReceipt(new Uint8Array(adm.carrier!.record))
              const byHex = Utils.toHex(Array.from(rec.by))
              stored.set(Utils.toHex(Array.from(adm.txid)), beefArr)
              const list = boxIndex.get(byHex)
              if (list !== undefined) {
                const acks = new Set(rec.acks.map((a) => Utils.toHex(Array.from(a))))
                boxIndex.set(
                  byHex,
                  list.filter((e) => !acks.has(e.commitment))
                )
              }
              admitted = admitted.concat(adm.outputs)
              continue
            }
            throw new Error(`box admit kind ${adm.kind}`)
          }
          if (logTM !== undefined && t === `tm_log_${logStream}`) {
            const beefArr = Array.from(body)
            let tx: Transaction
            try {
              tx = Transaction.fromAtomicBEEF(beefArr)
            } catch {
              tx = Transaction.fromBEEF(beefArr)
            }
            // an engine always hands whole-tx admissions the ATOMIC form —
            // a non-atomic BEEF does not parse inside the lookup service
            const atomic = tx.toAtomicBEEF()
            const prevCoins: number[] = []
            for (const [i, inp] of tx.inputs.entries()) {
              const src = inp.sourceTXID ?? inp.sourceTransaction?.id('hex')
              if (src !== undefined && (await logLS!.coin(t, src, inp.sourceOutputIndex)) !== undefined) prevCoins.push(i)
            }
            const r = await logTM.identifyAdmissibleOutputs(beefArr, prevCoins)
            admitted = admitted.concat(r.outputsToAdmit)
            stored.set(tx.id('hex'), beefArr)
            for (const o of r.outputsToAdmit) await logLS!.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: atomic, outputIndex: o, topic: t })
            continue
          }
          if (finger && t === 'tm_finger') {
            let tx: Transaction
            try {
              tx = Transaction.fromAtomicBEEF(beef)
            } catch {
              tx = Transaction.fromBEEF(beef)
            }
            const atomic = tx.toAtomicBEEF()
            const txid = tx.id('hex')
            const prevCoins = tx.inputs
              .filter((i) => fingerHeld.has(`${i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? ''}.${i.sourceOutputIndex}`))
              .map((_, i) => i)
            const r = await fingerTM!.identifyAdmissibleOutputs(beef, prevCoins)
            admitted = admitted.concat(r.outputsToAdmit)
            stored.set(txid, beef)
            for (const i of tx.inputs) {
              const src = i.sourceTXID ?? i.sourceTransaction?.id('hex')
              if (src !== undefined && fingerHeld.has(`${src}.${i.sourceOutputIndex}`)) {
                fingerLS!.outputSpent({ mode: 'txid', txid: src, outputIndex: i.sourceOutputIndex, spendingTxid: txid, topic: 'tm_finger' })
              }
            }
            for (const o of r.outputsToAdmit) {
              fingerHeld.add(`${txid}.${o}`)
              fingerLS!.outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: atomic, txid, outputIndex: o, topic: 'tm_finger' })
            }
            continue
          }
          if (topics[t] === undefined) {
            res.writeHead(400, { 'content-type': 'application/json' })
            res.end(JSON.stringify({ error: `unknown topic ${t}` }))
            return
          }
          const r = await topics[t].identifyAdmissibleOutputs(beef, [])
          admitted = admitted.concat(r.outputsToAdmit)
          try {
            stored.set(Transaction.fromAtomicBEEF(beef).id('hex'), beef)
          } catch {
            /* */
          }
          for (const o of r.outputsToAdmit) await lookups['ls_nektar'].outputAdmittedByTopic({ mode: 'whole-tx', atomicBEEF: beef, outputIndex: o, topic: t })
        }
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(JSON.stringify(admitted.length ? { [names[0]]: { outputsToAdmit: admitted } } : {}))
      } else if (req.method === 'POST' && req.url === '/lookup') {
        const q = JSON.parse(body.toString('utf8'))
        let a: Array<{ txid: string; outputIndex: number }>
        if (q.service === 'ls_bbox') {
          const want = q.query as Record<string, string>
          const names = Object.keys(want).sort().join(',')
          if (names !== 'office,to' && names !== 'after,office,to') throw new Error('box double: only inbox questions')
          if (office === undefined || want.office !== office) throw new Error('box double: unknown office')
          a = (boxIndex.get(want.to) ?? [])
            .filter((e) => e.expires === 0 || e.expires > Math.floor(Date.now() / 1000))
            .map((e) => ({ txid: e.txid, outputIndex: 0 }))
        } else if (q.service === 'ls_finger') {
          if (fingerLS === undefined) throw new Error('finger double: not enabled')
          a = await fingerLS.lookup(q)
        } else if (q.service === 'ls_log') {
          if (logLS === undefined) throw new Error('log double: not enabled')
          a = await logLS.lookup(q)
        } else a = evil ? [...stored.keys()].map((txid) => ({ txid, outputIndex: 0 })) : await lookups[q.service].lookup(q)
        if (drop !== undefined) a = a.filter((x) => !drop(x.txid))
        res.writeHead(200, { 'content-type': 'application/json' })
        res.end(
          JSON.stringify({
            type: 'output-list',
            outputs: a.map((o) => {
              const b = [...(stored.get(o.txid) ?? [])]
              if (corrupt && b.length > 10) b[10] = b[10] ^ 0xff
              return { beef: b, outputIndex: o.outputIndex }
            })
          })
        )
      } else {
        res.writeHead(404)
        res.end()
      }
    } catch (e) {
      res.writeHead(500)
      res.end(String((e as Error).message))
    }
  })
  await new Promise<void>((r) => srv.listen(port ?? 0, '127.0.0.1', r))
  const { port: bound } = srv.address() as AddressInfo
  return {
    url: `http://127.0.0.1:${bound}`,
    close: async () => {
      await new Promise<void>((r) => srv.close(() => r()))
    },
    has: (txid: string) => stored.has(txid),
    txids: () => [...stored.keys()],
    raw: () => [...stored.values()],
    envelopes: () => [...boxEnvelopes]
  }
}
