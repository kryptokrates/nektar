// focused repro of the e2e reorg lane — one shard, one anchor, invalidate the
// sealing block, verify stale-BUMP clearing + pull-floor re-attachment.
import { StackChain, stackTreasury, stackCfgFromEnv, stackUp, assetUp, StackFunder } from '@nektar/chain'
import { NektarMemory } from '@nektar/mcp'

const stackCfg = stackCfgFromEnv()
if (stackCfg === undefined || !(await stackUp(stackCfg)) || !(await assetUp(stackCfg, 3_000))) {
  console.error('stack or asset plane down')
  process.exit(1)
}
const treasury = await stackTreasury()
const rc = new StackChain(stackCfg)
const rf = await StackFunder.fund(treasury, 10)
await rc.mine(1)
const rm = new NektarMemory(rc, rf, rf.keyPair, {
  batchPolicy: { maxLeaves: 1 },
  proofLookup: (t) => rc.proofOf(t)
})
await rm.remember('reorg', 'to be reorged')
await rm.anchorShard('reorg')
await rc.mine(1)
const rac = (rm as any).anchors.get('reorg')
rac.confirm(rac.getHead().tx.id('hex'), rac.getHead().tx.merklePath)
const { result: before } = await rm.prove('reorg', 0)
console.log(`pre-reorg ok: ${before.ok} state: ${(before as any).anchorState}`)
const lostTxid = rac.getHead().tx.id('hex')
console.log(`lostTxid: ${lostTxid}`)
const tipBefore = await rc.currentHeight()
const hdr = await rc.headerAtHeight(tipBefore)
await rc.rpc('invalidateblock', hdr!.hash)
console.log(`invalidated h${tipBefore}`)
await rac.handleReorg([lostTxid])
const rh = rac.getHead()
console.log(`re-broadcast; head pending: ${rh.status === 'pending'} merklePath cleared: ${rh.tx.merklePath === undefined}`)
await rc.mine(1)
const t0 = Date.now()
for (let i = 0; i < 60 && rh.tx.merklePath === undefined; i++) {
  await rm.fetchTx(rh.tx.id('hex')).catch(() => undefined)
  if (rh.tx.merklePath === undefined) await new Promise((r) => setTimeout(r, 5_000))
}
const secs = Math.round((Date.now() - t0) / 1000)
console.log(`proof attached after ${secs}s; merklePath: ${rh.tx.merklePath !== undefined}`)
rac.confirm(rh.tx.id('hex'), rh.tx.merklePath)
const { result: after } = await rm.prove('reorg', 0)
console.log(`after.ok: ${after.ok} anchorState: ${(after as any).anchorState}`)
