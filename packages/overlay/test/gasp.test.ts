import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, LockingScript } from '@bsv/sdk'
import { MAMEM_ANCHOR_ARTIFACT, TAG_RPRIME, META_FLAG_HEX } from '@mamem/contracts'
import { RunarContract } from 'runar-sdk'
import { chainRp, GENESIS_PREV_RP, dsha256, utf8, hash160, toHex } from '@mamem/core'
import { MockChain, MockFunder } from '@mamem/chain'
import { createMamemOverlay } from '../src/node.js'
import { AnchorChain } from '@mamem/anchor'
import { GASP } from '@bsv/gasp'
// OverlayGASPStorage is not exported from the package root; import the file
// directly (same class Engine.syncGASPWithPeer uses for its local storage).
// @ts-ignore deep import
// package subpath export "./*.ts" → dist/esm/src/*.js (no hoisting-dependent relative path)
import { OverlayGASPStorage } from '@bsv/overlay/GASP/OverlayGASPStorage.ts'

// A GASPRemote whose "transport" is a direct call into the peer engine's
// provideForeignSyncResponse / provideForeignGASPNode — the same methods the
// overlay-express /requestSyncResponse + /requestForeignGASPNode routes wrap.
class DirectRemote {
  constructor (private engine: any, private topic: string) {}
  async getInitialResponse (request: any) { return await this.engine.provideForeignSyncResponse(request, this.topic) }
  async requestNode (graphID: string, txid: string, outputIndex: number, metadata: boolean) {
    return await this.engine.provideForeignGASPNode(graphID, txid, outputIndex, this.topic)
  }
  async getInitialReply (_r: any) { return { UTXOList: [] } }
  async submitNode (_n: any) { return undefined }
}
// fake remote serving arbitrary nodes — forged-chain test
class FakeRemote {
  constructor (private nodes: Array<{ txid: string, outputIndex: number, rawTx: string }>) {}
  async getInitialResponse () { return { UTXOList: this.nodes.map(n => ({ txid: n.txid, outputIndex: n.outputIndex, score: 1 })), since: 0 } }
  async requestNode (_g: string, txid: string, outputIndex: number) {
    const n = this.nodes.find(n => n.txid === txid && n.outputIndex === outputIndex) ?? this.nodes[0]
    if (!n) throw new Error('node not found')
    return { graphID: _g, txid: n.txid, outputIndex: n.outputIndex, rawTx: n.rawTx }
  }
  async getInitialReply () { return { UTXOList: [] } }
  async submitNode () { return undefined }
}

// The same flow Engine.syncGASPWithPeer runs, with the remote wired in-process:
// `new GASP(storage, remote, lastInteraction, prefix, true, true).sync(peer)`
async function syncFrom (host: any, remote: any, label: string) {
  const gasp = new (GASP as any)(new OverlayGASPStorage('tm_mamem', host.engine), remote, 0, label, true, true)
  await gasp.sync(label)
}

const submit = async (ov: any, tx: Transaction) =>
  ov.engine.submit({ beef: tx.toBEEF(), topics: ['tm_mamem'] }, undefined, 'current-tx')

test('GASP sync: empty host converges to host1 heads for 2 shards incl. rotate', async (t) => {
  const chain = new MockChain()
  const f1 = new MockFunder(); f1.topup(30)
  for (const u of (f1 as any).utxos as Array<{ tx: Transaction }>) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  const host1 = await createMamemOverlay(chain, chain)
  const host2 = await createMamemOverlay(chain, chain)
  t.after(async () => { await host1.knex.destroy(); await host2.knex.destroy() })

  // shard A: genesis + append + rotate + post-rotate append on host1
  const acA = new AnchorChain('shardA', f1.keyPair, f1, f1.keyPair, chain)
  await acA.anchor(1, dsha256(utf8('a0')))
  await acA.anchor(1, dsha256(utf8('a1')))
  const newKey = PrivateKey.fromRandom()
  await acA.rotate(newKey)
  // funding stays with f1's fund key — the new owner only authorizes
  await acA.anchor(1, dsha256(utf8('a2')))

  // shard B: genesis + append on host1
  const fB = new MockFunder(); fB.topup(10)
  for (const u of (fB as any).utxos as Array<{ tx: Transaction }>) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  const acB = new AnchorChain('shardB', fB.keyPair, fB, fB.keyPair, chain)
  await acB.anchor(1, dsha256(utf8('b0')))
  await acB.anchor(1, dsha256(utf8('b1')))

  // mine so the stored BEEFs carry merkle proofs — GASP serves anchors with
  // proofs; unconfirmed anchors propagate via direct submit, not GASP
  chain.mine(1)

  // admit the whole chains on host1
  for (const [name, ac] of [['A', acA], ['B', acB]] as const) {
    for (const h of [...((ac as any).pendingHeads ?? [])]) {
      const s = await submit(host1, h.tx)
      const n = s.tm_mamem?.outputsToAdmit?.length
      if (n !== 1) console.log(`${name} link NOT admitted: n=${h.data?.n} txid=${h.tx.id('hex').slice(0,12)} res=${JSON.stringify(s.tm_mamem)}`)
      assert.equal(n, 1, `chain link admitted on host1: ${name} n=${h.data?.n} txid=${h.tx.id('hex').slice(0,12)}`)
    }
    if ((ac as any).head) {
      const s = await submit(host1, (ac as any).head.tx)
      assert.equal(s.tm_mamem?.outputsToAdmit?.length, 1, 'genesis admitted on host1')
    }
  }

  const shardA = (acA as any).getShardId(), shardB = (acB as any).getShardId()
  assert.equal(host2.ls.getHead(shardA), undefined, 'host2 starts empty')

  await syncFrom(host2, new DirectRemote(host1.engine, 'tm_mamem'), 'host1')

  const h1A = host1.ls.getHead(shardA), h2A = host2.ls.getHead(shardA)
  const h1B = host1.ls.getHead(shardB), h2B = host2.ls.getHead(shardB)
  assert.ok(h1A && h2A && h1A.txid === h2A.txid, 'shardA head converged')
  // genesis n=0, append n=1, rotate keeps n=1, append n=2
  assert.equal(h2A!.n, 2, 'shardA head n=2 after append+rotate+append')
  assert.equal(h2A!.txid, (acA as any).getHead().tx.id('hex'), 'shardA head is the post-rotate append')
  assert.ok(h1B && h2B && h1B.txid === h2B.txid, 'shardB head converged')
  assert.equal(h2B!.n, 1, 'shardB head n=1')
})

test('GASP sync: forged chain pushed through sync is rejected (tm still applies)', async (t) => {
  const chain = new MockChain()
  const host2 = await createMamemOverlay(chain, chain)
  t.after(async () => { await host2.knex.destroy() })
  // forged anchor: genesis whose ownerPkh ≠ hash160(genesisOwner) — a shard
  // squat that tm_mamem must reject even when it arrives via sync
  const victim = PrivateKey.fromRandom(), attacker = PrivateKey.fromRandom()
  const forged = new Transaction()
  const f = new MockFunder(attacker); f.topup(5)
  const fu = await f.take(2000)
  forged.addInput({ sourceTransaction: fu.tx, sourceOutputIndex: fu.vout, unlockingScriptTemplate: new P2PKH().unlock(attacker) })
  const victimPub = victim.toPublicKey().toString()
  const atkPkh = toHex(hash160(Uint8Array.from(Buffer.from(attacker.toPublicKey().toString(), 'hex'))))
  const R = dsha256(utf8('forged'))
  const lock = LockingScript.fromHex(new RunarContract(MAMEM_ANCHOR_ARTIFACT, [
    victimPub, atkPkh, META_FLAG_HEX, 0n, 1n, Buffer.from(R).toString('hex'),
    Buffer.from(chainRp(GENESIS_PREV_RP, R, 1)).toString('hex'), Buffer.from(TAG_RPRIME).toString('hex')
  ]).getLockingScript())
  forged.addOutput({ lockingScript: lock, satoshis: 1 })
  forged.addOutput({ lockingScript: new P2PKH().lock(attacker.toPublicKey().toAddress([0x6f])), change: true })
  await forged.fee(); await forged.sign()

  await syncFrom(host2, new FakeRemote([{ txid: forged.id('hex'), outputIndex: 0, rawTx: forged.toHex() }]), 'attacker')
  const squatShard = toHex(hash160(Uint8Array.from(Buffer.from(victimPub, 'hex'))))
  assert.equal(host2.ls.getHead(squatShard), undefined, 'squatting genesis rejected through sync')
})
