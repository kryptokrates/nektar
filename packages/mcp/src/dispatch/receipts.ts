// answer-grounding + receipt tools — extracted verbatim from the server.ts dispatchTool switch
import { nektarEnv, sparse } from '@nektar/core'
import { paymentsEnabled } from '../pay.js'
import { receiptToken } from '../receipt.js'
import { shareBlock, verifyBase } from './shared.js'
import type { ToolHandler } from './shared.js'

const UNFUNDED_RE = /no funding|insufficient|reserve empty|no utxo|empty reserve|0 utxo/i

export const handlers: Record<string, ToolHandler> = {
  groundedAnswer: async (mem, args, d) => {
    const { ground, commitReceipt } = await import('@nektar/reasoning')
    const { Buffer } = await import('node:buffer')
    const allowLocal = !d.ctx.external && args?.requireAnchored !== true
    let allCitedAnchored = true
    const fetch: import('@nektar/reasoning').FragmentFetch = async (ref) => {
      const r = await mem.recall(ref.shard, { seq: ref.seq, allowLocal })
      const it = sparse(r.items[0])
      if (it && it.anchored === false) allCitedAnchored = false
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
    for (const rawClaim of args.claims ?? []) {
      const c = { ...rawClaim, cites: Array.isArray(rawClaim.cites) ? rawClaim.cites : [] }
      const allRefs = [...c.cites, ...(Array.isArray(c.quotes) ? c.quotes : [])]
      const priv = allRefs.filter((r: any) => mem.shardPrivate(r.shard))
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
    const keptReasons = new Set(report.claims.filter((c) => c.grounded).map((c) => c.reason))
    const groundingTier: 'exact-quote' | 'quote-backed-synthesis' | 'datalog-entailed' | 'mixed' | undefined =
      keptReasons.size > 1
        ? 'mixed'
        : keptReasons.has('grounded:quote')
          ? 'exact-quote'
          : keptReasons.has('grounded:quote-backed')
            ? 'quote-backed-synthesis'
            : keptReasons.has('grounded:entailed')
              ? 'datalog-entailed'
              : undefined
    // the receipt commits only cites of KEPT claims (both cites and quotes) —
    // private cites are redacted to {private, hdp, anchorTxid}: the shard name
    // never enters the anchored record.
    const refs: any[] = []
    const seenRef = new Set<string>()
    for (const c of report.kept) {
      const claimRefs = [...(c.cites ?? []), ...(c.quotes ?? []).map((q) => ({ shard: q.shard, seq: q.seq }))]
      for (const r of claimRefs) {
        const key = `${r.shard}:${r.seq}`
        if (seenRef.has(key)) continue
        seenRef.add(key)
        refs.push(
          mem.shardPrivate(r.shard)
            ? {
                private: true,
                proofCommit: proofCommits.get(key)?.commit,
                ...(proofCommits.get(key)?.store !== undefined ? { proofStore: proofCommits.get(key)!.store } : {}),
                ...((await mem.fragmentRef(r.shard, r.seq)) ?? { hdp: '' })
              }
            : r
        )
      }
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
    const canLocalFallback = allowLocal && !paymentsEnabled()
    const hasFunds = (await mem.funds.outs().catch(() => [])).length > 0
    const initialCheckResult: 'verified' | 'unverifiable' = allCitedAnchored && hasFunds ? 'verified' : 'unverifiable'
    const anchorMem: import('@nektar/reasoning').AnchorMem = {
      remember: (shard, data) => mem.remember(shard, data),
      anchorShard: async (shard) => {
        if (canLocalFallback && !hasFunds) return null
        try {
          return await mem.anchorShard(shard)
        } catch (e) {
          if (canLocalFallback && UNFUNDED_RE.test((e as Error).message)) return null
          throw e
        }
      }
    }
    const { receipt, receiptHash, anchor } = await commitReceipt(
      {
        answer,
        fragmentRefs: refs,
        checkResult: initialCheckResult,
        timestamp: Date.now(),
        modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
        groundingReportHash: report.reportHash,
        ...(groundingTier ? { groundingTier } : {}),
        ...(attestation ? { modelAttestation: attestation } : {}),
        ...(d.paid ? { payment: d.paid } : {}),
        ...(typeof args?.caller === 'string' ? { caller: args.caller } : {}),
        ...(typeof args?.delegationId === 'string' ? { delegationId: args.delegationId } : {}),
        ...(typeof (args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST')) === 'string'
          ? { manifestId: args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST') }
          : {})
      },
      anchorMem
    )
    const anchored = allCitedAnchored && Boolean(anchor?.txid)
    const token = receiptToken(receiptHash)
    return {
      answer,
      committed: true,
      anchored,
      verification: anchored ? 'spv-anchored' : 'local-signed-unanchored',
      ...(groundingTier ? { groundingTier } : {}),
      kept: report.kept,
      flagged: report.flagged,
      grounding: report,
      receipt,
      receiptHash,
      receiptToken: token,
      verifyUrl: `${verifyBase()}/v1/verify?r=${token}`,
      share: shareBlock(token),
      anchor,
      checkResult: anchored ? 'verified' : 'unverifiable'
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
