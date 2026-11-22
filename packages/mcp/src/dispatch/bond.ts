// bond covenant tools — extracted verbatim from the server.ts dispatchTool switch
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  bondDeploy: async (mem, args) => {
    const r = await mem.bondDeploy(args.sats, args.releaseHeight === undefined ? undefined : BigInt(args.releaseHeight), args.force === true)
    return { ...r, releaseHeight: r.releaseHeight.toString(), receipt: { decision: 'allow', action: 'bondDeploy' } }
  },
  bondStatus: (mem) => {
    const s = mem.bondStatus()
    return Promise.resolve(s ? { ...s, releaseHeight: s.releaseHeight.toString(), deadline: s.deadline.toString() } : { deployed: false })
  },
  bondSeize: async (mem, args) => {
    const bond = args.bond
      ? {
          ...args.bond,
          cfg: {
            ...args.bond.cfg,
            bondReleaseHeight: BigInt(args.bond.cfg.bondReleaseHeight),
            maxFee: BigInt(args.bond.cfg.maxFee),
            epochLen: BigInt(args.bond.cfg.epochLen)
          }
        }
      : undefined
    const r = await mem.bondSeize(args.payToScript, bond)
    return r ? { ...r, receipt: { decision: 'allow', action: 'bondSeize' } } : { deployed: false }
  },
  bondCommit: async (mem, args) => {
    const r = await mem.bondCommit(args.root)
    return r ? { ...r, n: r.n.toString(), receipt: { decision: 'allow', action: 'bondCommit' } } : { deployed: false }
  },
  bondClose: async (mem) => {
    const r = await mem.bondClose()
    return r ? { ...r, receipt: { decision: 'allow', action: 'bondClose' } } : { deployed: false }
  }
}
