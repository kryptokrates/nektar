// GOAL-v7 V3: paid-disclosure MCP tools — env gating + a full
// offer → fund → buy → claim flow against the real covenant (MockChain
// verifyScripts runs the locking script for keeps).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey, Transaction, LockingScript } from '@bsv/sdk'
import { MockChain, MockFunder } from '@mamem/chain'
import { MamemMemory } from '../src/memory.js'
import { visibleTools, callTool } from '../src/server.js'
import { DisclosureSaleHost } from '@mamem/anchor'

const SALE_TOOLS = ['saleOffer', 'saleBuy', 'saleClaim']

function setup () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(50)
  for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 }, modelId: 'test-model-1' })
  return { chain, funder, mem }
}

test('paid disclosure tools: hidden + refused unless MAMEM_PAID_DISCLOSURE is set', async () => {
  const { mem } = setup()
  delete process.env.MAMEM_PAID_DISCLOSURE
  const vis: string[] = visibleTools().map(t => t.name as string)
  for (const n of SALE_TOOLS) assert.ok(!vis.includes(n), `${n} must be hidden when unset`)
  await assert.rejects(
    () => callTool(mem, 'saleOffer', { value: 'aa', price: 100, refundAfter: 1, maxFee: 10 }),
    /paid disclosure is disabled/)
  process.env.MAMEM_PAID_DISCLOSURE = '1'
  try {
    const vis2: string[] = visibleTools().map(t => t.name as string)
    for (const n of SALE_TOOLS) assert.ok(vis2.includes(n), `${n} must be listed when enabled`)
  } finally {
    delete process.env.MAMEM_PAID_DISCLOSURE
  }
})

test('paid disclosure: saleOffer → funded listing → saleBuy → saleClaim (private, real covenant)', async () => {
  process.env.MAMEM_PAID_DISCLOSURE = '1'
  try {
    const { chain, mem } = setup()
    const operatorPub = mem.operatorKey().toPublicKey().toString()
    const VALUE = Buffer.from('field:clearance=2')
    // 1. offer: wraps VALUE for the operator key (private mode), returns cfg + wrapped
    const offer = await callTool(mem, 'saleOffer', {
      value: VALUE.toString('hex'), price: 3_000, refundAfter: 1_000_000, maxFee: 10, buyerPub: operatorPub
    })
    assert.equal(offer.cfg.hWrapped.length, 64)
    assert.equal(offer.cfg.price, 3_000)
    // 2. fund the listing utxo on the mock chain
    const host = new DisclosureSaleHost(offer.cfg)
    const parent = new Transaction()
    parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new LockingScript() })
    parent.addOutput({ satoshis: 5_000, lockingScript: LockingScript.fromHex(host.lockingScript()) })
    chain.registerParent(parent)
    // 3. buy: covenant spend — the mock chain runs real script eval; output 0 pays the seller exactly `price`
    const buy = await callTool(mem, 'saleBuy', {
      cfg: offer.cfg,
      utxo: { txid: parent.id('hex'), vout: 0, satoshis: 5_000, script: host.lockingScript() },
      wrapped: offer.wrapped, changeSats: 1_990
    })
    assert.equal(buy.accepted, true, `buy must be accepted: ${buy.reason ?? ''}`)
    assert.equal(buy.sellerSats, 3_000)
    // 4. claim: parse the revealed `wrapped` from the real buy tx and unwrap it
    const tx = chain.mempool.get(buy.txid)!
    assert.ok(tx, 'buy tx must be in the mempool')
    const claim = await callTool(mem, 'saleClaim', {
      txHex: tx.toHex(), hWrapped: offer.cfg.hWrapped, sellerPub: operatorPub
    })
    assert.equal(claim.mode, 'private')
    assert.equal(claim.valueHex, VALUE.toString('hex'), 'unwrapped payload round-trips')
    // 5. wrong commitment is refused
    await assert.rejects(
      () => callTool(mem, 'saleClaim', { txHex: tx.toHex(), hWrapped: 'ff'.repeat(32) }),
      /does not match/)
  } finally {
    delete process.env.MAMEM_PAID_DISCLOSURE
  }
})

test('paid disclosure: public mode returns the value unencrypted', async () => {
  process.env.MAMEM_PAID_DISCLOSURE = '1'
  try {
    const { chain, mem } = setup()
    const VALUE = Buffer.from('{"field":7,"salt":"aa","path":[]}')
    const offer = await callTool(mem, 'saleOffer', {
      value: VALUE.toString('hex'), price: 500, refundAfter: 1_000_000, maxFee: 10
      // no buyerPub → public/plaintext commitment
    })
    const host = new DisclosureSaleHost(offer.cfg)
    const parent = new Transaction()
    parent.addInput({ sourceTXID: '00'.repeat(32), sourceOutputIndex: 0xffffffff, unlockingScript: new LockingScript() })
    parent.addOutput({ satoshis: 1_500, lockingScript: LockingScript.fromHex(host.lockingScript()) })
    chain.registerParent(parent)
    const buy = await callTool(mem, 'saleBuy', {
      cfg: offer.cfg,
      utxo: { txid: parent.id('hex'), vout: 0, satoshis: 1_500, script: host.lockingScript() },
      wrapped: offer.wrapped, changeSats: 990
    })
    assert.equal(buy.accepted, true, `buy must be accepted: ${buy.reason ?? ''}`)
    const tx = chain.mempool.get(buy.txid)!
    const claim = await callTool(mem, 'saleClaim', { txHex: tx.toHex(), hWrapped: offer.cfg.hWrapped })
    assert.equal(claim.mode, 'public')
    assert.equal(claim.valueHex, VALUE.toString('hex'))
  } finally {
    delete process.env.MAMEM_PAID_DISCLOSURE
  }
})
