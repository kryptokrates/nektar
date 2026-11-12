// ZK clearance THROUGH the MCP tool layer (callTool = what MCP/HTTP execute):
// identity + wire-encoded clearance reach recall; every negative is refused.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { stackWorld, stackChain, StackFunder } from '@nektar/chain'
import { NektarMemory, callTool, ClientWallet, ClearanceAuthority } from '../src/index.js'

test('MCP recall with wallet identity + clearance: level-2 read only when cleared; negatives refused', async t => {
  const w = await stackWorld(t, 10)
  if (w === undefined) return
  const { chain, funder } = w
  const authority = new ClearanceAuthority()
  const mem = new NektarMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, clearanceAuthorities: [authority.pubkey] })
  const call = (n: string, a: Record<string, unknown>) => callTool(mem, n, a)
  await call('remember', { shard: 'c', data: 'public fact' })
  await call('remember', { shard: 'c', data: 'secret price 12,400 EUR', level: 2 })
  await chain.mine(1)
  mem.confirmAnchor('c', mem.tailLink('c')!.txid)
  const wallet = new ClientWallet()
  const cred = authority.issue(wallet.pubkey, 3n)
  const read = async (zk?: unknown, identity?: unknown) =>
    (await call('recall', { shard: 'c', seq: 1, identity: identity ?? (await wallet.identify(call)), zk })).items[0]
  assert.match((await read()).reason, /zkaccess required/, 'no clearance → withheld')
  const zk = wallet.presentClearance(cred, 'c', 1, 2n)
  const ok = await read(zk)
  assert.equal(ok.text, 'secret price 12,400 EUR')
  assert.match((await read(zk)).reason, /replayed/, 'same presentation twice → replay rejected')
  const rogue = new ClearanceAuthority().issue(wallet.pubkey, 3n)
  assert.match((await read(wallet.presentClearance(rogue, 'c', 1, 2n))).reason, /authority|not trusted|untrusted/i, 'untrusted authority')
  assert.throws(() => wallet.presentClearance(authority.issue(wallet.pubkey, 1n), 'c', 1, 2n), /below 2/, 'level-1 credential cannot prove level 2')
  const other = new ClientWallet()
  assert.match(
    (await read(wallet.presentClearance(cred, 'c', 1, 2n), await other.identify(call))).reason,
    /holder|caller/i,
    'presentation by a different caller'
  )
  assert.match((await read(wallet.presentClearance(cred, 'c', 0, 2n))).reason, /scope/, 'transcript scoped to another record')
  await assert.rejects(() => call('recall', { shard: 'c', seq: 1, zk: { bogus: true } }), /zk\./, 'malformed wire → specific error')
})
