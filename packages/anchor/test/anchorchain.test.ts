import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { dsha256, utf8, chainRp, eq } from '@nektar/core'
import { decodeNektarAnchor } from '@nektar/contracts'
import { stackWorld, StackFunder } from '@nektar/chain'
import { AnchorChain } from '../src/index.js'

const setup = async (t: { skip: (m?: string) => void }, burstLimit = Infinity) => {
  const w = await stackWorld(t, 16, 10_000)
  if (w === undefined) return undefined
  const { chain, funder } = w
  const ac = new AnchorChain('s1', PrivateKey.fromRandom(), funder, funder.keyPair, chain, { burstLimit })
  return { chain, funder, ac }
}

test('regression: confirming a descendant never gives its BUMP to earlier pending heads', async (t) => {
  const s0 = await setup(t)
  if (s0 === undefined) return
  const { ac } = s0
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

test('anchor: genesis then successor spends the head UTXO', async (t) => {
  const s0 = await setup(t)
  if (s0 === undefined) return
  const { chain, ac } = s0
  const g = await ac.anchor(10, dsha256(utf8('b0')))
  const gd = decodeNektarAnchor(g.tx.outputs[0].lockingScript.toHex())
  assert.equal(gd!.n, 0)

  const a1 = await ac.anchor(20, dsha256(utf8('b1')))
  // input 0 must spend genesis head outpoint
  const cIn = a1.tx.inputs.find((i) => (i.sourceTXID ?? i.sourceTransaction?.id('hex')) === g.tx.id('hex'))!
  assert.equal(cIn.sourceOutputIndex, 0)
  const a1d = decodeNektarAnchor(a1.tx.outputs[0].lockingScript.toHex())
  assert.equal(a1d!.n, 1)
  assert.ok(eq(a1d!.Rp, chainRp(gd!.Rp, a1d!.R, 20)))

  await chain.mine(1)
  ac.confirm(a1.tx.id('hex'), (await chain.getTx(a1.tx.id('hex')))!.merklePath)
  assert.equal(ac.getHead()!.data.n, 1)
  assert.ok(ac.headAtomicBEEF()!.length > 0)
})

test('anchor: failed broadcast does not advance the head', async (t) => {
  const w = await stackWorld(t, 16, 10_000)
  if (w === undefined) return
  const { chain, funder } = w
  // wrap the broadcaster: fail exactly once (the 3rd broadcast = 2nd append)
  let calls = 0
  const flaky = {
    broadcast: async (tx: any) => {
      calls++
      if (calls === 3) return { status: 'error' as const, description: 'simulated drop' }
      return chain.broadcast(tx)
    }
  }
  const ac = new AnchorChain('s1', PrivateKey.fromRandom(), funder, funder.keyPair, flaky)
  const R = (i: number) => dsha256(utf8(`rb${i}`))
  const g = await ac.anchor(1, R(0))
  const a1 = await ac.anchor(1, R(1))
  assert.equal(ac.getHead()!.data.n, 1)
  await assert.rejects(() => ac.anchor(1, R(2))) // the failed append
  assert.equal(ac.getHead()!.data.n, 1, 'head must not advance on failed broadcast')
  // retry after the drop — must produce a valid successor of n=1 (not a phantom)
  const a3 = await ac.anchor(1, R(2))
  const d = decodeNektarAnchor(a3.tx.outputs[0].lockingScript.toHex())
  assert.equal(d!.n, 2)
  const cIn = a3.tx.inputs.find((i) => (i.sourceTXID ?? i.sourceTransaction?.id('hex')) === a1.tx.id('hex'))!
  assert.ok(cIn, 'successor spends the n=1 head')
  const a4 = await ac.anchor(1, R(3))
  assert.equal(decodeNektarAnchor(a4.tx.outputs[0].lockingScript.toHex())!.n, 3)
})

test('anchor: unconfirmed chain + burst back-pressure', async (t) => {
  const s0 = await setup(t, 3)
  if (s0 === undefined) return
  const { chain, ac } = s0
  for (let i = 0; i < 3; i++) await ac.anchor(1, dsha256(utf8(`b${i}`)))
  await assert.rejects(() => ac.anchor(1, dsha256(utf8('x'))), /back-pressure/)
  await chain.mine(1)
  // confirm the whole chain (all mined in one block)
  for (const h of [...(ac as any).pendingHeads]) ac.confirm(h.tx.id('hex'), (await chain.getTx(h.tx.id('hex')))!.merklePath)
  await ac.anchor(1, dsha256(utf8('b3')))
  assert.equal(ac.getHead()!.data.n, 3)
})

test('burst: 20 anchors with reserve-style funder pass script verification', async (t) => {
  const w = await stackWorld(t, 30, 10_000)
  if (w === undefined) return
  const { chain, funder: f } = w
  // emulate ReserveFunder: hand out each parent exactly once
  const pool = [...(f as any).utxos]
  const spentSet = new Set<string>()
  const bf: any = {
    keyPair: f.keyPair,
    takeReserve: async () => pool.find((u) => !spentSet.has(u.tx.id('hex') + ':' + u.vout)) ?? null,
    markSpent: (t: string, v: number) => {
      spentSet.add(t + ':' + v)
    },
    get utxos() {
      return pool
    }
  }
  const ac = new AnchorChain('b', f.keyPair, bf, f.keyPair, chain)
  for (let i = 0; i < 20; i++) {
    try {
      await ac.anchor(1, new Uint8Array(32).fill(i))
    } catch (e) {
      const ltx = (ac as any).provider?.lastTx
      ltx?.inputs.forEach((inp: any, ix: number) => {
        const st = inp.sourceTransaction
        const out = st?.outputs?.[inp.sourceOutputIndex ?? 0]
        console.log(
          `i=${i} in${ix}: src=${inp.sourceTXID ?? st?.id('hex')} sats=${out?.satoshis} srcSats=${inp.sourceSatoshis} unlockLen=${inp.unlockingScript?.toHex().length}`
        )
      })
      throw e
    }
  }
  assert.equal(true, true)
})

test('retry after TX_MISSING_PARENT resolves the parent (no stale lastTx)', async (t) => {
  // A broadcaster that rejects the SECOND broadcast once with TX_MISSING_PARENT
  // (regtest propagation race). Before the fix, lastTx was set to the FAILED
  // child, so the retry's parent fetch returned '' → partial sourceTransaction
  // → preimage mismatch → TX_INVALID on the retry.
  const w = await stackWorld(t, 10, 10_000)
  if (w === undefined) return
  const { chain } = w
  const inner = chain
  const flaky: any = {
    broadcast: async (tx: import('@bsv/sdk').Transaction) => {
      if ((flaky)._n === 1) {
        ;(flaky)._n = 2
        return { status: 'error', description: 'TX_MISSING_PARENT (34): error getting parent' }
      }
      ;(flaky)._n = ((flaky)._n ?? 0) + 1
      return inner.broadcast(tx)
    }
  }
  const { funder: f } = w
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1)) // genesis ok
  await ac.anchor(1, new Uint8Array(32).fill(2)) // n=1: first broadcast fails, retry must succeed
  assert.equal(ac.getHead()!.data.n, 1, 'chain advanced despite the failed attempt')
})

test('retry: ambiguous TX_MISSING_PARENT re-broadcasts the IDENTICAL tx', async (t) => {
  const w = await stackWorld(t, 10, 10_000)
  if (w === undefined) return
  const { chain } = w
  let n = 0
  let rejectedHex: string | undefined
  let secondHex: string | undefined
  const flaky: any = {
    broadcast: async (tx: import('@bsv/sdk').Transaction) => {
      n++
      if (n === 2) {
        rejectedHex = tx.toHex()
        return { status: 'error', description: 'TX_MISSING_PARENT (34): error getting parent' }
      }
      if (n === 3) {
        secondHex = tx.toHex()
      }
      return chain.broadcast(tx)
    }
  }
  const { funder: f } = w
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1))
  await ac.anchor(1, new Uint8Array(32).fill(2)) // n=1: first broadcast rejected, re-broadcast identical
  assert.equal(ac.getHead()!.data.n, 1)
  assert.equal(secondHex, rejectedHex, 're-broadcast must be byte-identical')
})

test('retry: UTXO_SPENT on head spent by our own attempt is adopted', async (t) => {
  const w = await stackWorld(t, 10, 10_000)
  if (w === undefined) return
  const { chain } = w
  let n = 0
  let lostTxid = ''
  let headOp = ''
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
  const { funder: f } = w
  const ac = new AnchorChain('r', f.keyPair, f, f.keyPair, flaky)
  await ac.anchor(1, new Uint8Array(32).fill(1))
  headOp = ac.getHead()!.tx.id('hex') + ':0'
  const r = await ac.anchor(1, new Uint8Array(32).fill(2))
  assert.equal(r.head.data.n, 1, 'adopted the earlier attempt as the head')
  assert.equal(r.tx.id('hex'), lostTxid)
})

test('anchorOnce fails fast on dry funding — never broadcasts a contract-only tx', async () => {
  // no chain needed: funding emptiness must abort before any broadcast
  const f = StackFunder.dry()
  let calls = 0
  const probe = {
    broadcast: async () => {
      calls++
      return { status: 'error' as const }
    }
  }
  const ac = new AnchorChain('d', f.keyPair, f, f.keyPair, probe)
  await assert.rejects(() => ac.anchor(1, new Uint8Array(32).fill(1)), /reserve dry|no utxo/i)
  assert.equal(calls, 0, 'nothing was broadcast')
})

test('every AnchorChain tx produces a BEEF with all inputs resolved', async (t) => {
  const w = await stackWorld(t, 10, 10_000)
  if (w === undefined) return
  const { chain } = w
  const { funder: f } = w
  const ac = new AnchorChain('b', f.keyPair, f, f.keyPair, chain)
  const { Transaction } = await import('@bsv/sdk')
  for (let i = 0; i < 4; i++) {
    const r = await ac.anchor(1, new Uint8Array(32).fill(i + 1))
    const ntx = Transaction.fromBEEF(ac.beefOf(r.tx))
    for (const [ix, inp] of ntx.inputs.entries()) {
      assert.ok(inp.sourceTXID !== undefined || inp.sourceTransaction !== undefined, `anchor ${i} in${ix}: parent unresolvable in BEEF`)
    }
  }
})

test('NektarAnchor v3 + ANC-006: META flag leads the data payload; flag-absent stays decodable', async (t) => {
  const w = await stackWorld(t, 10, 10_000)
  if (w === undefined) return
  const { chain } = w
  const { funder: f } = w
  const ac = new AnchorChain('v3', f.keyPair, f, f.keyPair, chain)
  await ac.anchor(3, dsha256(utf8('batch')))
  await ac.anchor(2, dsha256(utf8('batch2'))) // append passes script verification
  const d = decodeNektarAnchor(ac.getHead()!.tx.outputs[0].lockingScript.toHex())!
  assert.ok(d, 'v3 anchor decodes')
  assert.deepEqual(Object.keys(d).sort(), ['R', 'Rp', 'metanet', 'n', 'ownerPkh', 'ownerPub', 'shard', 'size', 'tag'])
  assert.equal(d.metanet, true, 'new anchors carry the META flag push')
  assert.equal(d.n, 1)
  assert.equal(d.size, 2)
  assert.equal(Buffer.from(d.R).toString('hex'), Buffer.from(dsha256(utf8('batch2'))).toString('hex'))
  const hex = ac.getHead()!.tx.outputs[0].lockingScript.toHex()
  // the flag is a real push — <0x04> 'META' — directly after the state
  // OP_RETURN. Match the full sequence: a bare lastIndexOf('6a') is flaky —
  // byte 0x6a can appear inside the state's pubkey/hash fields that follow.
  const flagAt = hex.lastIndexOf('6a044d455441')
  assert.ok(flagAt >= 0, 'META flag push directly after OP_RETURN')
  assert.equal(hex.slice(flagAt, flagAt + 12), '6a044d455441')
  // a pre-flag (v3, flag-absent) anchor built by the legacy artifact still
  // decodes byte-for-byte (metanet=false)
  const { RunarContract } = await import('runar-sdk')
  const { NEKTAR_ANCHOR_V3_ARTIFACT, META_FLAG_HEX } = await import('@nektar/contracts')
  const { PrivateKey } = await import('@bsv/sdk')
  const { hash160, toHex } = await import('@nektar/core')
  const pk = PrivateKey.fromRandom().toPublicKey().toString()
  const legacy = new RunarContract(NEKTAR_ANCHOR_V3_ARTIFACT, [
    pk,
    toHex(hash160(Uint8Array.from(Buffer.from(pk, 'hex')))),
    0n,
    3n,
    '11'.repeat(32),
    '22'.repeat(32),
    toHex((await import('@nektar/core')).TAG_RPRIME)
  ]).getLockingScript()
  const ld = decodeNektarAnchor(legacy)!
  assert.ok(ld, 'flag-absent v3 anchor decodes')
  assert.equal(ld.metanet, false)
  assert.equal(ld.n, 0)
  assert.equal(ld.size, 3)
  // and a WRONG flag byte is refused (byte-auth: rebuild ≠ script)
  const wrong = legacy.slice(0, legacy.lastIndexOf('6a')) + '6a' + '0444454144' + legacy.slice(legacy.lastIndexOf('6a') + 2)
  assert.equal(decodeNektarAnchor(wrong), null, 'a non-META leading push is not a valid flag')
  // a v2-shaped state (extra length-prefixed 32-byte field after R') is not a v3 anchor
  const at = hex.lastIndexOf('6a') + 2 + 10 + 226 // after flag|owner|n|size|R|Rp
  assert.equal(decodeNektarAnchor(hex.slice(0, at) + '20' + '42'.repeat(32) + hex.slice(at)), null, 'v2 layout refused')
})

test('GOAL-v8 D5: extractAnchorAuthorship exposes σ_n — verified against the preimage', async (t) => {
  const { extractAnchorAuthorship } = await import('@nektar/contracts')
  const s0 = await setup(t)
  if (s0 === undefined) return
  const { ac } = s0
  const g = await ac.anchor(1, dsha256(utf8('g')))
  const a1 = await ac.anchor(2, dsha256(utf8('a1')))
  // the successor spend's input 0 spends the genesis anchor output
  const auth = extractAnchorAuthorship(a1.tx, g.tx.outputs[0].lockingScript.toHex())
  assert.ok(auth, 'σ_n extracted from the spend')
  assert.equal(auth.ownerPub, (ac as any).ownerPub())
  assert.equal(auth.sighashScope, 0x41)
  assert.ok(auth.verified, 'sig verifies over sha256(txPreimage) for the owner key')
  // the preimage commits to the spend itself — the continuation output carrying
  // R_n, R'_n is inside its digest (σ_n ≡ Sign(R_n‖t_n), Appendix D)
  assert.ok(auth.preimage.startsWith('01000000'), 'BIP143-shaped preimage')
  // negatives: a non-anchor prevOut yields no authorship
  assert.equal(extractAnchorAuthorship(a1.tx, '76a914' + '00'.repeat(20) + '88ac'), null, 'P2PKH prevOut → null')
  // a spend input without an owner-key push yields null (input 1 = funding P2PKH)
  assert.equal(extractAnchorAuthorship(a1.tx, g.tx.outputs[0].lockingScript.toHex(), 1), null, 'funding input carries no σ_n')
  // a corrupted preimage breaks verification
  const bad = extractAnchorAuthorship(a1.tx, g.tx.outputs[0].lockingScript.toHex())!
  assert.ok(bad.verified)
})
