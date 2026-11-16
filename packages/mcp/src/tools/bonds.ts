// bonds.ts — the operator checkpoint bond: deploy, status, seize, commit and
// close. BigInt cfg fields cross the wire as numbers and re-widen here.
import type { ToolHandler } from './ctx.js'

export const BOND_HANDLERS: Record<string, ToolHandler> = {
  bondDeploy: async (d) => {
    const { mem, args } = d
    const r = await mem.bondDeploy(args.sats, args.releaseHeight === undefined ? undefined : BigInt(args.releaseHeight), args.force === true)
    return { ...r, releaseHeight: r.releaseHeight.toString(), receipt: { decision: 'allow', action: 'bondDeploy' } }
  },
  bondStatus: async (d) => {
    const { mem } = d
    const s = mem.bondStatus()
    return s ? { ...s, releaseHeight: s.releaseHeight.toString(), deadline: s.deadline.toString() } : { deployed: false }
  },
  bondSeize: async (d) => {
    const { mem, args } = d
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
  bondCommit: async (d) => {
    const { mem, args } = d
    const r = await mem.bondCommit(args.root)
    return r ? { ...r, n: r.n.toString(), receipt: { decision: 'allow', action: 'bondCommit' } } : { deployed: false }
  },
  bondClose: async (d) => {
    const { mem } = d
    const r = await mem.bondClose()
    return r ? { ...r, receipt: { decision: 'allow', action: 'bondClose' } } : { deployed: false }
  }
}
