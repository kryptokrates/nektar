// tm_nektar — TopicManager for nektar anchors.
// Admits a PushDrop anchor output only if:
//   (a) genesis: no previousCoins and n === 0 AND the shard has no live head, or
//   (b) successor: spends the shard's currently-live head output, n = prev.n+1,
//       and R'_n == dsha256(R'_{n-1} | R_n | size_le8).
// Everything else is rejected.
import { Transaction } from '@bsv/sdk'
import { parseAnchorFields, chainRp, ZERO32 } from '../lib/anchor.mjs'

// sharedHeads: the ls_nektar instance (or any object with getHead(shard) ->
// {txid, outputIndex, n, Rp} | undefined) holding live head state.
export function createTmNektar (sharedHeads) {
  return {
    identifyAdmissibleOutputs: async (beef, previousCoins) => {
      const tx = Transaction.fromBEEF(beef)
      const admit = { outputsToAdmit: [], coinsToRetain: [] }

      // Find candidate anchor outputs (exactly one per tx allowed).
      const anchors = []
      tx.outputs.forEach((o, i) => {
        const a = parseAnchorFields(o.lockingScript)
        if (a) anchors.push({ ...a, outputIndex: i })
      })
      if (anchors.length !== 1) return admit
      const a = anchors[0]

      // Collect the nektar anchor inputs (inputs spending previously-admitted
      // topical outputs) — must be exactly the head spend, or none (genesis).
      const headInputs = []
      for (const inIdx of previousCoins) {
        const inp = tx.inputs[inIdx]
        const prevTx = inp?.sourceTransaction
        if (!prevTx) continue
        const prevOut = prevTx.outputs[inp.sourceOutputIndex]
        const pf = prevOut && parseAnchorFields(prevOut.lockingScript)
        if (pf) headInputs.push({ inIdx, prevTx, pf, prevOutIndex: inp.sourceOutputIndex })
      }

      if (headInputs.length === 0) {
        // Genesis path: n must be 0 and the shard must have no live head.
        if (a.n !== 0) return admit
        if (sharedHeads.getHead(a.shard)) return admit // second genesis = fork attempt
        const expectRp = chainRp(ZERO32, a.R, a.size)
        if (!eq(a.Rp, expectRp)) return admit
        admit.outputsToAdmit.push(a.outputIndex)
        return admit
      }

      if (headInputs.length !== 1) return admit
      const h = headInputs[0]

      // The spent output must be the shard's CURRENT head.
      const head = sharedHeads.getHead(a.shard)
      if (!head) return admit
      if (h.pf.shard !== a.shard) return admit
      if (h.prevTx.id('hex') !== head.txid || h.prevOutIndex !== head.outputIndex) return admit

      // n = prev.n + 1 and chained root valid.
      if (a.n !== h.pf.n + 1) return admit
      const expectRp = chainRp(h.pf.Rp, a.R, a.size)
      if (!eq(a.Rp, expectRp)) return admit

      admit.outputsToAdmit.push(a.outputIndex)
      admit.coinsToRetain.push(h.inIdx) // keep the spent head for anchor-by-n history
      return admit
    },
    identifyNeededInputs: async (beef) => {
      const tx = Transaction.fromBEEF(beef)
      const anchors = tx.outputs.map(o => parseAnchorFields(o.lockingScript)).filter(Boolean)
      if (anchors.length === 0) throw new Error('no nektar anchor outputs')
      const head = sharedHeads.getHead(anchors[0].shard)
      return head ? [{ txid: head.txid, outputIndex: head.outputIndex }] : []
    },
    getDocumentation: async () => 'Admits nektar PushDrop anchors: genesis n=0, else spends current head with n+1 and valid chained root.',
    getMetaData: async () => ({ name: 'tm_nektar', shortDescription: 'nektar anchor chain topic manager' })
  }
}

function eq (a, b) {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}
