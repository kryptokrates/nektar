import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { dsha256, utf8, chainRp, GENESIS_PREV_RP, eq } from '@mamem/core'
import { decodeMamemAnchor } from '@mamem/contracts'
import { MockChain, MockFunder } from '@mamem/chain'
import { AnchorChain } from '../src/index.js'

const setup = (burstLimit = Infinity) => {
  const chain = new MockChain()
  const funder = new MockFunder(); funder.topup(200); for (const u of (funder as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('s1', PrivateKey.fromRandom(), funder, funder.keyPair, chain, { burstLimit })
  return { chain, funder, ac }
}

test('regression: confirming a descendant never gives its BUMP to earlier pending heads', async () => {
  const { ac } = setup()
  const g = await ac.anchor(1, dsha256(utf8('c0')))
  const a1 = await ac.anchor(1, dsha256(utf8('c1')))
  const { MerklePath } = await import('@bsv/sdk')
  // a BUMP that proves only a1 (single-leaf block)
  const mp = new MerklePath(7, [[{ offset: 0, hash: a1.tx.id('hex'), txid: true }]])
  ac.confirm(a1.tx.id('hex'), mp)
  assert.equal(a1.tx.merklePath, mp)
  assert.notEqual(g.tx.merklePath, mp, 'ancestor keeps its own (absent) proof')
  assert.equal(ac.getHead()!.data.n, 1)
})

test('anchor: genesis then successor spends the head UTXO', async () => {
  const { chain, ac } = setup()
  const g = await ac.anchor(10, dsha256(utf8('b0')))
  const gd = decodeMamemAnchor(g.tx.outputs[0]!.lockingScript.toHex())
  assert.equal(gd!.n, 0)

  const a1 = await ac.anchor(20, dsha256(utf8('b1')))
  // input 0 must spend genesis head outpoint
  const cIn = a1.tx.inputs.find(i => (i.sourceTXID ?? i.sourceTransaction?.id('hex')) === g.tx.id('hex'))!
  assert.equal(cIn.sourceOutputIndex, 0)
  const a1d = decodeMamemAnchor(a1.tx.outputs[0]!.lockingScript.toHex())
  assert.equal(a1d!.n, 1)
  assert.ok(eq(a1d!.Rp, chainRp(gd!.Rp, a1d!.R, 20)))

  chain.mine(1)
  ac.confirm(a1.tx.id('hex'), a1.tx.merklePath)
  assert.equal(ac.getHead()!.data.n, 1)
  assert.ok(ac.headAtomicBEEF()!.length > 0)
})

test('anchor: failed broadcast does not advance the head', async () => {
  const chain = new MockChain()
  const funder = new MockFunder(); funder.topup(200); for (const u of (funder as any).utxos) chain.registerParent(u.tx)
  // wrap the broadcaster: fail exactly once (the 3rd broadcast = 2nd append)
  let calls = 0
  const flaky = {
    broadcast: async (tx: any) => {
      calls++
      if (calls === 3) return { status: 'error' as const, description: 'simulated drop' }
      return chain.broadcast(tx)
    }
  }
  const ac = new AnchorChain('s1', PrivateKey.fromRandom(), funder, funder.keyPair, flaky as any)
  const R = (i: number) => dsha256(utf8(`rb${i}`))
  const g = await ac.anchor(1, R(0))
  const a1 = await ac.anchor(1, R(1))
  assert.equal(ac.getHead()!.data.n, 1)
  await assert.rejects(() => ac.anchor(1, R(2))) // the failed append
  assert.equal(ac.getHead()!.data.n, 1, 'head must not advance on failed broadcast')
  // retry after the drop — must produce a valid successor of n=1 (not a phantom)
  const a3 = await ac.anchor(1, R(2))
  const d = decodeMamemAnchor(a3.tx.outputs[0]!.lockingScript.toHex())
  assert.equal(d!.n, 2)
  const cIn = a3.tx.inputs.find(i => (i.sourceTXID ?? i.sourceTransaction?.id('hex')) === a1.tx.id('hex'))!
  assert.ok(cIn, 'successor spends the n=1 head')
  const a4 = await ac.anchor(1, R(3))
  assert.equal(decodeMamemAnchor(a4.tx.outputs[0]!.lockingScript.toHex())!.n, 3)
})

test('anchor: unconfirmed chain + burst back-pressure', async () => {
  const { chain, ac } = setup(3)
  for (let i = 0; i < 3; i++) await ac.anchor(1, dsha256(utf8(`b${i}`)))
  await assert.rejects(() => ac.anchor(1, dsha256(utf8('x'))), /back-pressure/)
  chain.mine(1)
  // confirm the whole chain (all mined in one block)
  for (const h of [...(ac as any).pendingHeads]) ac.confirm(h.tx.id('hex'), h.tx.merklePath)
  await ac.anchor(1, dsha256(utf8('b3')))
  assert.equal(ac.getHead()!.data.n, 3)
})

test('burst: 20 anchors with reserve-style funder pass script verification', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(30); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  // emulate ReserveFunder: hand out each parent exactly once
  const pool = [...(f as any).utxos]
  const spentSet = new Set<string>()
  const bf: any = {
    keyPair: f.keyPair,
    takeReserve: async () => pool.find(u => !spentSet.has(u.tx.id('hex')+':'+u.vout)) ?? null,
    markSpent: (t: string, v: number) => { spentSet.add(t+':'+v) },
    get utxos () { return pool }
  }
  const ac = new AnchorChain('b', f.keyPair, bf, f.keyPair, chain)
  for (let i = 0; i < 20; i++) {
    try {
      await ac.anchor(1, new Uint8Array(32).fill(i))
    } catch (e) {
      const ltx = (ac as any).provider?.lastTx
      ltx?.inputs.forEach((inp: any, ix: number) => {
        const st = inp.sourceTransaction; const out = st?.outputs?.[inp.sourceOutputIndex ?? 0]
        console.log(`i=${i} in${ix}: src=${inp.sourceTXID ?? st?.id('hex')} sats=${out?.satoshis} srcSats=${inp.sourceSatoshis} unlockLen=${inp.unlockingScript?.toHex().length}`)
      })
      throw e
    }
  }
  assert.equal(true, true)
})

test('retry after TX_MISSING_PARENT resolves the parent (no stale lastTx)', async () => {
  // A broadcaster that rejects the SECOND broadcast once with TX_MISSING_PARENT
  // (regtest propagation race). Before the fix, lastTx was set to the FAILED
  // child, so the retry's parent fetch returned '' → partial sourceTransaction
  // → preimage mismatch → TX_INVALID on the retry.
  const chain = new MockChain(); chain.verifyScripts = true
  const inner = chain
  const flaky: any = {
    broadcast: async (tx: import('@bsv/sdk').Transaction) => {
      if ((flaky as any)._n === 1) { (flaky as any)._n = 2; return { status: 'error', description: 'TX_MISSING_PARENT (34): error getting parent' } }
      (flaky as any)._n = ((flaky as any)._n ?? 0) + 1
      return inner.broadcast(tx)
    },

  }
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1)) // genesis ok
  await ac.anchor(1, new Uint8Array(32).fill(2)) // n=1: first broadcast fails, retry must succeed
  assert.equal(ac.getHead()!.data.n, 1, 'chain advanced despite the failed attempt')
})

test('retry: ambiguous TX_MISSING_PARENT re-broadcasts the IDENTICAL tx', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  let n = 0; let rejectedHex: string | undefined; let secondHex: string | undefined
  const flaky: any = {
    broadcast: async (tx: import('@bsv/sdk').Transaction) => {
      n++
      if (n === 2) { rejectedHex = tx.toHex(); return { status: 'error', description: 'TX_MISSING_PARENT (34): error getting parent' } }
      if (n === 3) { secondHex = tx.toHex() }
      return chain.broadcast(tx)
    }
  }
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1))
  await ac.anchor(1, new Uint8Array(32).fill(2)) // n=1: first broadcast rejected, re-broadcast identical
  assert.equal(ac.getHead()!.data.n, 1)
  assert.equal(secondHex, rejectedHex, 're-broadcast must be byte-identical')
})

test('retry: UTXO_SPENT on head spent by our own attempt is adopted', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  let n = 0; let lostTxid = ''; let headOp = ''
  const flaky: any = {
    broadcast: async (tx: import('@bsv/sdk').Transaction) => {
      n++
      if (n === 2) {
        // "accepted but reported missing-parent": accept the tx, then tell the
        // caller it failed — the retry's REBUILT tx then double-spends the head
        const res = await chain.broadcast(tx)
        lostTxid = tx.id('hex')
        return { status: 'error', description: 'TX_MISSING_PARENT (34)' }
      }
      if (n === 3) {
        // the rebuilt tx hits UTXO_SPENT naming the head outpoint + our txid
        return { status: 'error', description: `UTXO_SPENT (70): ${headOp} utxo already spent by tx ${lostTxid}[0]` }
      }
      return chain.broadcast(tx)
    }
  }
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1))
  headOp = ac.getHead()!.tx.id('hex') + ':0'
  const r = await ac.anchor(1, new Uint8Array(32).fill(2))
  assert.equal(r.head.data.n, 1, 'adopted the earlier attempt as the head')
  assert.equal(r.tx.id('hex'), lostTxid)
})

test('anchorOnce fails fast on dry funding — never broadcasts a contract-only tx', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder() // no topup — dry
  const ac = new AnchorChain('d', f.keyPair, f, f.keyPair, chain)
  await assert.rejects(() => ac.anchor(1, new Uint8Array(32).fill(1)), /reserve dry/)
  assert.equal(chain['mempool'].size + (chain['spent'].size ?? 0), 0, 'nothing was broadcast')
})

test('every AnchorChain tx produces a BEEF with all inputs resolved', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(8); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('b', f.keyPair, f, f.keyPair, chain)
  const { Transaction } = await import('@bsv/sdk')
  for (let i = 0; i < 4; i++) {
    const r = await ac.anchor(1, new Uint8Array(32).fill(i + 1))
    const ntx = Transaction.fromBEEF(ac.beefOf(r.tx) as any)
    for (const [ix, inp] of ntx.inputs.entries()) {
      assert.ok(inp.sourceTXID !== undefined || inp.sourceTransaction !== undefined,
        `anchor ${i} in${ix}: parent unresolvable in BEEF`)
    }
  }
})

test('MamemAnchor v3: state is exactly (owner, n, size, R, R\', tag, ownerPkh) — no ZK companion field', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const f = new MockFunder(); f.topup(6); for (const u of (f as any).utxos) chain.registerParent(u.tx)
  const ac = new AnchorChain('v3', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(3, dsha256(utf8('batch')))
  await ac.anchor(2, dsha256(utf8('batch2'))) // append passes script verification
  const d = decodeMamemAnchor(ac.getHead()!.tx.outputs[0]!.lockingScript.toHex())!
  assert.ok(d, 'v3 anchor decodes')
  assert.deepEqual(Object.keys(d).sort(), ['R', 'Rp', 'n', 'ownerPkh', 'ownerPub', 'shard', 'size', 'tag'])
  assert.equal(d.n, 1); assert.equal(d.size, 2)
  assert.equal(Buffer.from(d.R).toString('hex'), Buffer.from(dsha256(utf8('batch2'))).toString('hex'))
  // a v2-shaped state (extra length-prefixed 32-byte field after R') is not a v3 anchor
  const hex = ac.getHead()!.tx.outputs[0]!.lockingScript.toHex()
  const at = hex.lastIndexOf('6a') + 2 + 226 // after owner|n|size|R|Rp
  assert.equal(decodeMamemAnchor(hex.slice(0, at) + '20' + '42'.repeat(32) + hex.slice(at)), null, 'v2 layout refused')
})
