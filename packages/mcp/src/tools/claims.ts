// claims.ts — selective disclosure verification, action authorization,
// challenge nonces, and the regtest ZK-claim covenant ops.
import type { ToolHandler } from './ctx.js'

// consolidated state machine: op → internal 'claim#<op>' handler (never
// listed in TOOLS — reachable only through the 'claim' dispatcher)
const CLAIM_OPS = new Set(['deploy', 'status', 'settle', 'deploySale', 'buySale'])

export const CLAIM_HANDLERS: Record<string, ToolHandler> = {
  disclose: async (d) => {
    const { mem, args } = d
    const idm = await import('@nektar/identity')
    const verifyAnchor = async () => (await mem.prove(args.shard, args.seq)).result.ok === true
    const r = await idm.verifyDisclosureAnchored(
      args.envelope,
      args.ownerPub,
      args.verifierPub,
      args.fieldCount,
      Buffer.from(args.fieldRoot, 'hex'),
      (args.siblings as string[]).map((s) => Buffer.from(s, 'hex')),
      args.atTime,
      verifyAnchor
    )
    return { verified: r.ok, reason: r.reason ?? null, receipt: { decision: r.ok ? 'allow' : 'deny', action: 'disclose' } }
  },
  authorize: async (d) => {
    const { mem, args } = d
    const { decodePresentation } = await import('@nektar/identity')
    return {
      receipt: {
        ...(await mem.authorizeAction(args.action, {
          identity: args.identity,
          zk: args.zk === undefined ? undefined : decodePresentation(args.zk),
          resource: args.resource
        })),
        action: args.action
      }
    }
  },
  challenge: async (d) => {
    const { mem } = d
    return { nonce: mem.issueChallenge(), statement: 'sign with answerChallenge(key, nonce) within 5 minutes; nonces from anywhere else are refused' }
  },
  claim: async (d) => {
    const { mem, args, ctx, callTool } = d
    const op = String(args?.op ?? '')
    if (!CLAIM_OPS.has(op)) throw new Error('claim: op required — deploy|status|settle|deploySale|buySale')
    return await callTool(mem, `claim#${op}`, args, ctx)
  },
  'claim#deploy': async (d) => {
    const { mem, args } = d
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    const { Buffer } = await import('node:buffer')
    return await mem.claims.deploy(Buffer.from(args.lzkmem, 'hex').toString('hex'), {
      bondSats: args.bondSats,
      deadline: args.deadline,
      anchorOutpoint: args.anchorOutpoint
    })
  },
  'claim#status': async (d) => {
    const { mem, args } = d
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.status(args.txid, args.vout ?? 0)
  },
  'claim#settle': async (d) => {
    const { mem, args } = d
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.settle(args.txid, args.vout ?? 0, args.payoutSats ?? 4980, args.verdictSats ?? 10)
  },
  'claim#deploySale': async (d) => {
    const { mem, args } = d
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.deploySale(args.verdictOutpoint, args.priceSats, args.wrapped)
  },
  'claim#buySale': async (d) => {
    const { mem, args } = d
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.buyGated(args.saleTxid, args.verdictOutpoint, args.wrapped, args.priceSats, args.changeSats ?? 900)
  }
}
