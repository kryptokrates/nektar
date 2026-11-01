// tm_mamem — TopicManager for mamem anchors.
// Admits a contract anchor output only if:
//   (a) genesis: previousCoins === [] AND n === 0 AND the shard has no live
//       head AND shard == hash160(genesisOwner) AND ownerPkh ==
//       hash160(genesisOwner) AND valid R'_0; or
//   (b) successor (append or rotate): spends exactly one topical input whose
//       parent output is an authentic MamemAnchor of the same shard — append:
//       n = parent.n+1, chained R', same ownerPkh; rotate: same n/size/R/R',
//       new ownerPkh. Parent validation is STATELESS (the parent output is
//       decoded from the BEEF), so a fresh host validates a replayed chain
//       correctly; the local head is used only as an extra fork guard when the
//       shard is already known.
import { Transaction } from '@bsv/sdk'
import { decodeMamemAnchor, isValidAnchor, type DecodedAnchor } from '@mamem/contracts'
import { chainRp, GENESIS_PREV_RP, eq, hash160, toHex, type AnchorData } from '@mamem/core'
import type { TopicManager, AdmittanceInstructions } from '@bsv/overlay'
import type { MamemHeads } from './ls.js'

function decode (lockingScript: any): { data: DecodedAnchor, pub: string } | null {
  const dec = decodeMamemAnchor(lockingScript?.toHex?.() ?? String(lockingScript))
  if (!dec || !isValidAnchor(dec)) return null
  return { data: dec, pub: dec.ownerPub }
}

// Funding inputs are standard P2PKH unlocks: exactly <sig> <pubkey> (pubkey
// 33 or 65 bytes). Anything else is treated as the contract (runar call)
// input. (A byte-length threshold is wrong: a P2PKH unlock is ~107 bytes.)
const isP2PKHUnlock = (i: any): boolean => {
  const chunks = i.unlockingScript?.chunks
  if (!Array.isArray(chunks) || chunks.length !== 2) return false
  const sigLen = chunks[0]?.data?.length ?? 0
  const pubLen = chunks[1]?.data?.length ?? 0
  return sigLen >= 9 && sigLen <= 73 && (pubLen === 33 || pubLen === 65)
}
const isContractInput = (i: any) => i.unlockingScript !== undefined && !isP2PKHUnlock(i)

export function createTmMamem (sharedHeads: MamemHeads): TopicManager {
  // Decoded anchor outputs of every tx this manager has parsed, keyed by
  // outpoint. Content-addressed (a txid fixes the tx's outputs), so entries
  // cannot be forged. Needed because GASP graph validation replays ancestors
  // as dry runs (no ls updates) and sdk BEEFs carry proven parents as
  // txid+BUMP only, so the parent output is otherwise unavailable.
  const seen = new Map<string, DecodedAnchor>()
  return {
    identifyAdmissibleOutputs: async (beef: number[], previousCoins: number[]): Promise<AdmittanceInstructions> => {
      const tx = Transaction.fromBEEF(beef)
      const admit: AdmittanceInstructions = { outputsToAdmit: [], coinsToRetain: [] }

      const txid = tx.id('hex')
      const anchors: Array<DecodedAnchor & { outputIndex: number, pub: string }> = []
      tx.outputs.forEach((o, i) => {
        const d = decode(o.lockingScript)
        if (d) {
          anchors.push({ ...d.data, outputIndex: i, pub: d.pub })
          seen.set(`${txid}:${i}`, d.data)
        }
      })
      if (anchors.length !== 1) return admit
      const a = anchors[0]!
      const head = sharedHeads.getHead(a.shard)

      if (previousCoins.length === 0) {
        // genesis: strictly no topical coins spent
        if (a.n !== 0) return admit
        if (head) return admit
        // shard = hash160(genesisOwner) and at genesis ownerPkh must equal it
        // too — otherwise anyone could deploy with a victim's genesisOwner and
        // squat their shard id
        if (a.shard !== toHex(hash160(Uint8Array.from(Buffer.from(a.pub, 'hex'))))) return admit
        if (!a.ownerPkh || !eq(a.ownerPkh, hash160(Uint8Array.from(Buffer.from(a.pub, 'hex'))))) return admit
        if (!eq(a.Rp, chainRp(GENESIS_PREV_RP, a.R, a.size))) return admit
        admit.outputsToAdmit.push(a.outputIndex)
        return admit
      }

      // successor: exactly one topical coin, whose spent parent output decodes
      // to an authentic anchor of the same shard
      if (previousCoins.length !== 1) return admit
      const inIdx = previousCoins[0]!
      const inp = tx.inputs[inIdx]
      if (!inp || !isContractInput(inp)) return admit
      // parent = decoded BEEF output when present; when previousCoins is
      // non-empty the parent is an already-admitted topical output and is in
      // the ls store (GASP replays ancestors first) — fall back to it.
      const pTx = inp.sourceTransaction
      const pTxid = inp.sourceTXID ?? pTx?.id('hex')
      const p = (pTx ? decode(pTx.outputs[inp.sourceOutputIndex!]?.lockingScript)?.data : undefined) ??
        (pTxid !== undefined ? seen.get(`${pTxid}:${inp.sourceOutputIndex!}`) : undefined) ??
        (pTxid !== undefined ? sharedHeads.getByOutpoint(pTxid, inp.sourceOutputIndex!) : undefined)
      if (!p || p.shard !== a.shard) return admit
      // genesisOwner is carried in state — same shard ⟺ same genesis owner

      // fork guard: if the shard is known locally, the successor must extend
      // the current head, not an old branch
      if (head) {
        if (pTxid !== head.txid || inp.sourceOutputIndex !== head.outputIndex) return admit
      }

      const pd = p
      const isAppend = a.n === pd.n + 1 && eq(a.Rp, chainRp(pd.Rp, a.R, a.size)) &&
        !!a.ownerPkh && !!pd.ownerPkh && eq(a.ownerPkh, pd.ownerPkh)
      const isRotate = a.n === pd.n && a.size === pd.size && eq(a.R, pd.R) &&
        eq(a.Rp, pd.Rp) && !!a.ownerPkh && !!pd.ownerPkh && !eq(a.ownerPkh, pd.ownerPkh)
      if (!isAppend && !isRotate) return admit

      admit.outputsToAdmit.push(a.outputIndex)
      admit.coinsToRetain.push(inIdx)
      return admit
    },

    identifyNeededInputs: async (beef: number[]) => {
      const tx = Transaction.fromBEEF(beef)
      const d = tx.outputs.map(o => decode(o.lockingScript)).find(Boolean)
      if (!d) throw new Error('no mamem anchor outputs')
      // ask for the contract input's parent when it isn't already in the BEEF —
      // never a local head (a fresh host has none)
      const needed: Array<{ txid: string, outputIndex: number }> = []
      for (const i of tx.inputs) {
        if (!isContractInput(i)) continue
        const txid = i.sourceTXID ?? i.sourceTransaction?.id('hex')
        if (!txid) continue
        const p = i.sourceTransaction?.outputs[i.sourceOutputIndex!]
        if (!p || !decode(p.lockingScript)) needed.push({ txid, outputIndex: i.sourceOutputIndex! })
      }
      return needed
    },

    getDocumentation: async () =>
      'Admits mamem contract anchors: genesis n=0 with shard=hash160(genesisOwner), ownerPkh=hash160(genesisOwner) and valid R\'_0; else a successor spending exactly one topical parent: append n+1 with chained R\' and same ownerPkh, or rotate (same n/size/R/R\', new ownerPkh). Validation is stateless against the parent output in the BEEF; the local head is only a fork guard.',
    getMetaData: async () => ({ name: 'tm_mamem', shortDescription: 'mamem anchor-chain topic manager' })
  }
}
