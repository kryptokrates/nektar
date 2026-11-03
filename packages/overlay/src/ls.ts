// ls_nektar — LookupService for nektar anchors.
// Queries: {shard} → current head; {shard,n} → anchor n; {shard,all:true} → history.
// State is persisted in a knex table (nektar_ls_anchors) so heads survive
// overlay restarts — without it a fresh n=0 genesis would fork a shard.
import { decodeNektarAnchor, isValidAnchor } from '@nektar/contracts'
import { toHex, utf8, type Bytes } from '@nektar/core'
import type { Knex } from 'knex'
import type { LookupService, LookupFormula, OutputAdmittedByTopic, OutputSpent } from '@bsv/overlay'

export interface AnchorRecord {
  txid: string
  outputIndex: number
  shard: string
  n: number
  size: number
  R: Bytes
  Rp: Bytes
  pub: string // genesisOwner pubkey hex — never changes for a shard
  ownerPkh?: Bytes // hash160(current owner); rotates on-chain
  spent: boolean
}

export interface NektarHeads {
  getHead: (shard: string) => AnchorRecord | undefined
  getByOutpoint: (txid: string, vout: number) => AnchorRecord | undefined
}

const TABLE = 'nektar_ls_anchors'

export function createLsNektar (kx?: Knex): LookupService & NektarHeads & { init: () => Promise<void> } {
  const anchors = new Map<string, AnchorRecord[]>()
  const outpointMap = new Map<string, AnchorRecord>()
  // The Engine notifies outputSpent even for transactions the topic rejects —
  // only apply a spend once its spender has itself been admitted. Spends
  // arrive before the admit, so they are buffered.
  const pendingSpends = new Map<string, string>()

  const getHead = (shard: string): AnchorRecord | undefined => {
    let head: AnchorRecord | undefined
    for (const a of anchors.get(shard) ?? []) {
      if (!a.spent && (!head || a.n > head.n)) head = a
    }
    return head
  }

  const pub = (a: AnchorRecord) => ({
    txid: a.txid,
    outputIndex: a.outputIndex,
    context: [...utf8(JSON.stringify({
      n: a.n, size: a.size, R: toHex(a.R), Rp: toHex(a.Rp), spent: a.spent
    }))]
  })

  const persist = async (r: AnchorRecord) => {
    if (!kx) return
    await kx(TABLE).insert({
      shard: r.shard, txid: r.txid, output_index: r.outputIndex,
      n: r.n, size: r.size, R: Buffer.from(r.R), Rp: Buffer.from(r.Rp),
      pub: r.pub, pkh: r.ownerPkh ? Buffer.from(r.ownerPkh) : null, spent: r.spent ? 1 : 0
    })
  }
  const persistSpent = async (r: AnchorRecord) => {
    if (!kx) return
    await kx(TABLE).where({ txid: r.txid, output_index: r.outputIndex }).update({ spent: 1 })
  }

  return {
    admissionMode: 'locking-script',
    spendNotificationMode: 'txid',
    getHead,
    getByOutpoint: (txid: string, vout: number) => outpointMap.get(`${txid}:${vout}`),

    init: async () => {
      if (!kx) return
      if (!(await kx.schema.hasTable(TABLE))) {
        await kx.schema.createTable(TABLE, t => {
          t.string('shard').notNullable()
          t.string('txid').notNullable()
          t.integer('output_index').notNullable()
          t.integer('n').notNullable()
          t.integer('size').notNullable()
          t.binary('R').notNullable()
          t.binary('Rp').notNullable()
          t.string('pub').notNullable()
          t.binary('pkh').nullable()
          t.integer('spent').notNullable().defaultTo(0)
          t.primary(['txid', 'output_index'])
        })
      }
      const rows = await kx(TABLE).select('*')
      for (const r of rows) {
        const rec: AnchorRecord = {
          txid: r.txid, outputIndex: r.output_index, shard: r.shard,
          n: r.n, size: r.size, R: new Uint8Array(r.R), Rp: new Uint8Array(r.Rp),
          pub: r.pub, ownerPkh: r.pkh ? new Uint8Array(r.pkh) : undefined, spent: !!r.spent
        }
        const list = anchors.get(rec.shard) ?? []
        list.push(rec); anchors.set(rec.shard, list)
        outpointMap.set(`${rec.txid}:${rec.outputIndex}`, rec)
      }
    },

    outputAdmittedByTopic: (payload: OutputAdmittedByTopic) => {
      if (payload.mode !== 'locking-script') return
      const d0 = decodeNektarAnchor(typeof payload.lockingScript === 'string' ? payload.lockingScript : payload.lockingScript?.toHex?.() ?? '')
      if (!d0 || !isValidAnchor(d0)) return
      const a = d0
      const rec: AnchorRecord = {
        txid: payload.txid, outputIndex: payload.outputIndex, shard: a.shard,
        n: a.n, size: a.size, R: a.R, Rp: a.Rp,
        pub: d0.ownerPub, ownerPkh: d0.ownerPkh,
        spent: false
      }
      const list = anchors.get(a.shard) ?? []
      list.push(rec); anchors.set(a.shard, list)
      outpointMap.set(`${payload.txid}:${payload.outputIndex}`, rec)
      void persist(rec)
      for (const [op, spender] of pendingSpends) {
        if (spender === payload.txid) {
          const r = outpointMap.get(op)
          if (r) { r.spent = true; void persistSpent(r) }
          pendingSpends.delete(op)
        }
      }
    },

    outputSpent: (payload: OutputSpent) => {
      if (payload.mode === 'none') return
      const spendingTxid = 'spendingTxid' in payload ? payload.spendingTxid : undefined
      if (!spendingTxid) return
      const rec = outpointMap.get(`${payload.txid}:${payload.outputIndex}`)
      if (!rec) return
      const admitted = [...outpointMap.values()].some(r => r.txid === spendingTxid)
      if (admitted) { rec.spent = true; void persistSpent(rec) }
      else pendingSpends.set(`${payload.txid}:${payload.outputIndex}`, spendingTxid)
    },

    outputEvicted: (txid: string, outputIndex: number) => {
      const rec = outpointMap.get(`${txid}:${outputIndex}`)
      if (rec) { rec.spent = true; void persistSpent(rec) }
      outpointMap.delete(`${txid}:${outputIndex}`)
    },

    lookup: async ({ query }: { query?: any }): Promise<LookupFormula> => {
      const { shard, n, all } = (query ?? {}) as { shard?: string, n?: number, all?: boolean }
      if (typeof shard !== 'string') return []
      const list = anchors.get(shard) ?? []
      if (all === true) return list.map(pub)
      if (n !== undefined) return list.filter(a => a.n === Number(n)).map(pub)
      const head = getHead(shard)
      return head ? [pub(head)] : []
    },

    getDocumentation: async () => 'nektar anchor lookup: head of shard, anchor by n, anchors by shard.',
    getMetaData: async () => ({ name: 'ls_nektar', shortDescription: 'nektar anchor lookup service' })
  }
}
