import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Transaction, PrivateKey, P2PKH, LockingScript } from '@bsv/sdk'
import { MockChain, MockFunder } from '@nektar/chain'
import { createNektarOverlay } from '../src/node.js'
import { signPresence, presenceOutputScript, parsePresence, verifyPresence } from '../src/presence.js'

const ad = async (key: PrivateKey, fields: { endpoint: string, shards: string[], ts: number }) =>
  signPresence(key, fields)

async function mkAdTx (funder: MockFunder, scriptHex: string): Promise<Transaction> {
  const tx = new Transaction()
  const fund = await funder.take(1500)
  tx.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  tx.addOutput({ lockingScript: LockingScript.fromHex(scriptHex), satoshis: 0 })
  tx.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await tx.fee(); await tx.sign()
  return tx
}

async function setup () {
  const chain = new MockChain()
  const funder = new MockFunder()
  funder.topup(50)
  const utxos = (funder as any).utxos as Array<{ tx: Transaction }>
  for (const u of utxos) chain.mempool.set(u.tx.id('hex'), u.tx)
  chain.mine(1)
  const overlay = await createNektarOverlay(chain, chain)
  const submitAd = async (tx: Transaction) =>
    overlay.engine.submit({ beef: tx.toBEEF(), topics: ['tm_presence'] }, undefined, 'current-tx')
  const look = (query: any) => overlay.engine.lookup({ service: 'ls_presence', query })
  const decode = (o: any) => JSON.parse(Buffer.from(o.context ?? []).toString())
  return { chain, funder, overlay, submitAd, look, decode }
}

test('tm_presence: signed ad admitted; directory answers all/shard/pub', async (t) => {
  const { funder, overlay, submitAd, look, decode } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const agentA = PrivateKey.fromRandom()
  const sA = await ad(agentA, { endpoint: 'http://a.example:8080/mcp', shards: ['manual', 'tnet'], ts: 1_700_000_000_000 })
  assert.ok(verifyPresence(sA.ad, sA.sig), 'self-signed ad verifies')
  const txA = await mkAdTx(funder, presenceOutputScript(sA))
  const st = await submitAd(txA)
  assert.equal(st.tm_presence?.outputsToAdmit?.length, 1, 'ad admitted')

  const agentB = PrivateKey.fromRandom()
  const sB = await ad(agentB, { endpoint: 'http://b.example:8080/mcp', shards: ['telemetry'], ts: 1_700_000_001_000 })
  await submitAd(await mkAdTx(funder, presenceOutputScript(sB)))

  const dir = await look({ all: true })
  assert.equal(dir.outputs.length, 2)
  const who = await look({ shard: 'manual' })
  assert.equal(who.outputs.length, 1)
  assert.equal(decode(who.outputs[0]).endpoint, 'http://a.example:8080/mcp')
  assert.equal(decode(who.outputs[0]).pub, agentA.toPublicKey().toString())
  const one = await look({ pub: agentB.toPublicKey().toString() })
  assert.equal(decode(one.outputs[0]).endpoint, 'http://b.example:8080/mcp')

  // newer ad replaces the older one for the same pubkey
  const sA2 = await ad(agentA, { endpoint: 'http://a2.example/mcp', shards: ['telemetry'], ts: 1_700_000_002_000 })
  await submitAd(await mkAdTx(funder, presenceOutputScript(sA2)))
  const again = await look({ pub: agentA.toPublicKey().toString() })
  assert.equal(decode(again.outputs[0]).endpoint, 'http://a2.example/mcp')
  assert.equal((await look({ shard: 'manual' })).outputs.length, 0, 'shard index updated on replace')
  assert.equal((await look({ all: true })).outputs.length, 2)
})

test('tm_presence: forged/unsigned/malformed ads rejected', async (t) => {
  const { funder, overlay, submitAd, look } = await setup()
  t.after(async () => { await overlay.knex.destroy() })
  const key = PrivateKey.fromRandom()
  const forged = await ad(key, { endpoint: 'http://victim.example/mcp', shards: ['s'], ts: 1 })
  forged.sig = '30' + '44'.repeat(35) // mangled signature
  const st = await submitAd(await mkAdTx(funder, presenceOutputScript(forged)))
  assert.equal(st.tm_presence?.outputsToAdmit?.length ?? 0, 0, 'bad sig not admitted')
  const junk = new Transaction()
  const fund = await funder.take(1500)
  junk.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(funder.keyPair) })
  junk.addOutput({ lockingScript: LockingScript.fromHex('6a03787878'), satoshis: 0 }) // OP_RETURN 'xxx'
  junk.addOutput({ lockingScript: new P2PKH().lock(funder.keyPair.toPublicKey().toAddress([0x6f])), change: true })
  await junk.fee(); await junk.sign()
  const st2 = await submitAd(junk)
  assert.equal(st2.tm_presence?.outputsToAdmit?.length ?? 0, 0, 'non-presence OP_RETURN not admitted')
  assert.equal((await look({ all: true })).outputs.length, 0)
})

test('presence script round-trip: parse(build(ad)) === ad', () => {
  const key = PrivateKey.fromRandom()
  const s = signPresence(key, { endpoint: 'http://x/mcp', shards: ['a', 'b'], ts: 42 })
  const back = parsePresence(presenceOutputScript(s))!
  assert.deepEqual(back.ad, s.ad)
  assert.ok(verifyPresence(back.ad, back.sig))
})
