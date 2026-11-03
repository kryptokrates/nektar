// ls_nektar — LookupService for nektar anchors.
// Answers: {shard} -> current head; {shard, n} -> anchor n; {shard, all:true} -> history.
import { parseAnchorFields } from '../lib/anchor.mjs'

export function createLsNektar () {
  // anchors: Map<shard, Array<{txid, outputIndex, n, size, R, Rp, spent}>>
  const anchors = new Map()
  const outpointMap = new Map() // 'txid:idx' -> record
  // The Engine notifies outputSpent even for transactions the topic rejects.
  // Only apply a spend once its spending tx was itself admitted; spends arrive
  // before the corresponding admit, so they are buffered here.
  const pendingSpends = new Map() // 'txid:idx' -> spendingTxid

  const record = (shard) => {
    if (!anchors.has(shard)) anchors.set(shard, [])
    return anchors.get(shard)
  }

  const svc = {
    admissionMode: 'locking-script',
    spendNotificationMode: 'txid',

    getHead (shard) {
      const list = anchors.get(shard) ?? []
      let head
      for (const a of list) if (!a.spent && (!head || a.n > head.n)) head = a
      return head
    },

    outputAdmittedByTopic: ({ txid, outputIndex, lockingScript }) => {
      const a = parseAnchorFields(lockingScript)
      if (!a) return
      const rec = { txid, outputIndex, n: a.n, size: a.size, R: a.R, Rp: a.Rp, spent: false }
      record(a.shard).push(rec)
      outpointMap.set(`${txid}:${outputIndex}`, rec)
      // Apply any buffered spends whose spender just got admitted.
      for (const [op, spender] of pendingSpends) {
        if (spender === txid) {
          const r = outpointMap.get(op)
          if (r) r.spent = true
          pendingSpends.delete(op)
        }
      }
    },

    outputSpent: ({ txid, outputIndex, spendingTxid }) => {
      const rec = outpointMap.get(`${txid}:${outputIndex}`)
      if (!rec) return
      const admitted = [...outpointMap.values()].some(r => r.txid === spendingTxid)
      if (admitted) rec.spent = true
      else pendingSpends.set(`${txid}:${outputIndex}`, spendingTxid)
    },

    outputEvicted: (txid, outputIndex) => {
      const rec = outpointMap.get(`${txid}:${outputIndex}`)
      if (rec) rec.spent = true
      outpointMap.delete(`${txid}:${outputIndex}`)
    },

    lookup: async ({ query }) => {
      const { shard, n, all } = query ?? {}
      if (typeof shard !== 'string') return []
      const list = anchors.get(shard) ?? []
      if (all === true) return list.map(pub)
      if (n !== undefined) return list.filter(a => a.n === Number(n)).map(pub)
      const head = svc.getHead(shard)
      return head ? [pub(head)] : []
    },

    getDocumentation: async () => 'nektar anchor lookup: head of shard, anchor by n, anchors by shard.',
    getMetaData: async () => ({ name: 'ls_nektar', shortDescription: 'nektar anchor lookup service' }),

    _anchors: anchors
  }
  return svc
}

function pub (a) {
  return {
    txid: a.txid,
    outputIndex: a.outputIndex,
    context: [
      ...Buffer.from(JSON.stringify({ n: a.n, size: a.size, R: Buffer.from(a.R).toString('hex'), Rp: Buffer.from(a.Rp).toString('hex'), spent: a.spent }), 'utf8')
    ]
  }
}
