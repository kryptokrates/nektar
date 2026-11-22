// answer-grounding + receipt tools — extracted verbatim from the server.ts dispatchTool switch
import { nektarEnv, sparse } from '@nektar/core'
import { receiptToken } from '../receipt.js'
import { shareBlock, verifyBase } from './shared.js'
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  groundedAnswer: async (mem, args, d) => {
    const { ground, commitReceipt } = await import('@nektar/reasoning')
    const { Buffer } = await import('node:buffer')
    const fetch: import('@nektar/reasoning').FragmentFetch = async (ref) => {
      const r = await mem.recall(ref.shard, { seq: ref.seq })
      const it = sparse(r.items[0])
      return { verified: !!it && !it.unverifiable, text: it?.text }
    }
    // private shards (remember …, private:true): a plain cite leaks the shard
    // name into the public receipt — the record must be cited via a ZK
    // membership proof. Caller may supply {zk:{queryId,proof}} from an
    // externally-issued zkQuery; otherwise the service AUTO-PROVES (self-issue
    // → prove → verify) — a real proof either way: a verifier re-runs it via
    // proofCommit. Prover absent/failed → flagged and stripped — fail-closed.
    const privRefused: any[] = []
    const claimsForGround: any[] = []
    const proofCommits = new Map<string, { commit: string; store?: { endpoint: string; bucket: string } }>() // `${shard}:${seq}` → proof commit + store locator
    for (const c of args.claims ?? []) {
      const priv = (c.cites ?? []).filter((r: any) => mem.shardPrivate(r.shard))
      if (priv.length === 0) {
        claimsForGround.push(c)
        continue
      }
      let zkok = true
      for (const r of priv) {
        const zk = r.zk ?? c.zk
        let bytes: Uint8Array
        if (typeof zk?.queryId === 'string' && typeof zk?.proof === 'string') {
          bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
        } else {
          try {
            const q = mem.issueZkQuery('nektar:auto-cite')
            bytes = (await mem.proveMembership(r.shard, r.seq, q.queryId)).bytes
          } catch {
            zkok = false
            break
          }
        }
        const v = await mem.verifyMembership(r.shard, r.seq, bytes)
        if (!v.ok) {
          zkok = false
          break
        }
        // the proof becomes a public artifact: committed in the receipt and
        // fetchable at /v1/proof — a verifier re-runs it, no trust in us.
        const pc = mem.putProof(bytes)
        proofCommits.set(`${r.shard}:${r.seq}`, { commit: pc, store: mem.proofStoreRef(pc) })
      }
      if (zkok) claimsForGround.push(c)
      else privRefused.push(c)
    }
    const report = await ground(claimsForGround, fetch, { mode: args.mode === 'flag' ? 'flag' : 'remove' })
    for (const c of privRefused)
      report.flagged.push({
        ...c,
        grounded: false,
        reason:
          'cite targets a private shard — the ZK membership proof could not be produced (record unanchored or prover unavailable); supply {zk:{queryId,proof}} to cite explicitly'
      })
    const answer = report.kept.map((c) => c.text).join(' ')
    if (report.kept.length === 0) {
      return {
        answer: '',
        committed: false,
        kept: [],
        flagged: report.flagged,
        grounding: report,
        checkResult: 'failed',
        note: 'no claim survived grounding — nothing committed (an underivable answer is invalid by construction)'
      }
    }
    // the receipt commits only cites of KEPT claims — and private cites are
    // redacted to {private, hdp, anchorTxid}: the shard name never enters
    // the anchored record.
    const refs: any[] = []
    for (const c of report.kept)
      for (const r of c.cites) {
        refs.push(
          mem.shardPrivate(r.shard)
            ? {
                private: true,
                proofCommit: proofCommits.get(`${r.shard}:${r.seq}`)?.commit,
                ...(proofCommits.get(`${r.shard}:${r.seq}`)?.store !== undefined ? { proofStore: proofCommits.get(`${r.shard}:${r.seq}`)!.store } : {}),
                ...((await mem.fragmentRef(r.shard, r.seq)) ?? { hdp: '' })
              }
            : r
        )
      }
    // modelAttestation (from attestInference): the statement MUST re-derive
    // from (input‖output‖modelPoseidon) before it enters the anchored record —
    // fail closed, never commit an unverifiable attestation.
    let attestation = args.modelAttestation
    if (args.attestInference !== undefined) {
      if (attestation !== undefined) throw new Error('pass attestInference OR modelAttestation, not both')
      const { attestInference } = await import('../inference.js')
      attestation = (await attestInference(mem, args.attestInference ?? {})).attestation
    }
    if (attestation !== undefined) {
      const { validateAttestation } = await import('@nektar/zk/aiinf-demo')
      const v = validateAttestation(attestation)
      if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
    }
    const { receipt, receiptHash, anchor } = await commitReceipt(
      {
        answer,
        fragmentRefs: refs,
        checkResult: 'verified',
        timestamp: Date.now(),
        modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
        groundingReportHash: report.reportHash,
        ...(attestation ? { modelAttestation: attestation } : {}),
        ...(d.paid ? { payment: d.paid } : {}),
        ...(typeof args?.caller === 'string' ? { caller: args.caller } : {}),
        ...(typeof args?.delegationId === 'string' ? { delegationId: args.delegationId } : {}),
        ...(typeof (args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST')) === 'string'
          ? { manifestId: args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST') }
          : {})
      },
      mem
    )
    const token = receiptToken(receiptHash)
    return {
      answer,
      committed: true,
      kept: report.kept,
      flagged: report.flagged,
      grounding: report,
      receipt,
      receiptHash,
      receiptToken: token,
      verifyUrl: `${verifyBase()}/v1/verify?r=${token}`,
      share: shareBlock(token),
      anchor,
      checkResult: 'verified'
    }
  },
  commitReceipt: async (mem, args, d) => {
    const { commitReceipt } = await import('@nektar/reasoning')
    const { Buffer } = await import('node:buffer')
    // private shards: same fail-closed rule as groundedAnswer — a private
    // cite needs a ZK membership proof the service verifies, then commits
    // redacted with the proof's sha256 so a verifier can re-run it.
    const refs: any[] = []
    for (const r of args.fragmentRefs ?? []) {
      if (!mem.shardPrivate(r.shard)) {
        refs.push(r)
        continue
      }
      const zk = r.zk
      let bytes: Uint8Array
      if (typeof zk?.queryId === 'string' && typeof zk?.proof === 'string') {
        bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
      } else {
        // auto-prove: self-issue → prove → verify (same artifact, no caller choreography)
        let q: { queryId: string }
        try {
          q = mem.issueZkQuery('nektar:auto-cite')
          bytes = (await mem.proveMembership(r.shard, r.seq, q.queryId)).bytes
        } catch (e) {
          throw new Error(`private cite for "${r.shard}" could not be auto-proven (${(e as Error).message}) — supply zk:{queryId,proof} to cite explicitly`, {
            cause: e
          })
        }
      }
      const v = await mem.verifyMembership(r.shard, r.seq, bytes)
      if (!v.ok) throw new Error(`ZK membership proof for private cite failed: ${v.reason}`)
      const pc = mem.putProof(bytes)
      refs.push({
        private: true,
        proofCommit: pc,
        ...(mem.proofStoreRef(pc) !== undefined ? { proofStore: mem.proofStoreRef(pc) } : {}),
        ...((await mem.fragmentRef(r.shard, r.seq)) ?? { hdp: '' })
      })
    }
    let attestation = args.modelAttestation
    if (args.attestInference !== undefined) {
      if (attestation !== undefined) throw new Error('pass attestInference OR modelAttestation, not both')
      const { attestInference } = await import('../inference.js')
      attestation = (await attestInference(mem, args.attestInference ?? {})).attestation
    }
    if (attestation !== undefined) {
      const { validateAttestation } = await import('@nektar/zk/aiinf-demo')
      const v = validateAttestation(attestation)
      if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
    }
    const out = await commitReceipt(
      {
        answer: args.answer,
        fragmentRefs: refs,
        checkResult: args.checkResult,
        timestamp: Date.now(),
        modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
        groundingReportHash: args.groundingReportHash,
        ...(attestation ? { modelAttestation: attestation } : {}),
        ...(d.paid ? { payment: d.paid } : {})
      },
      mem
    )
    const token = receiptToken(out.receiptHash)
    return { ...out, receiptToken: token, verifyUrl: `${verifyBase()}/v1/verify?r=${token}`, share: shareBlock(token) }
  },
  attestInference: async (mem, args) => {
    const { attestInference } = await import('../inference.js')
    return await attestInference(mem, args)
  },
  verifyReceipt: async (mem, args) => {
    const { verifyReceipt } = await import('../receipt.js')
    return await verifyReceipt(mem, args)
  }
}
