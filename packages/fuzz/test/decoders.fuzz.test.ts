// P16 fuzzing — every untrusted-input decoder/verifier, seeded and bounded.
// Properties: (1) never crashes with an untyped error, (2) a mutated encoding
// is never accepted as a different valid value (byte-authenticated decoders
// either reject or round-trip exactly), (3) verifiers return {ok:false}.
// Seed with MAMEM_FUZZ_SEED; iterations with MAMEM_FUZZ_N (default 300).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { PrivateKey } from '@bsv/sdk'
import { dsha256, utf8, toHex } from '@mamem/core'
import { MockChain, MockFunder } from '@mamem/chain'
import { decodeMamemAnchor } from '@mamem/contracts'
import { verifyFragment } from '@mamem/verify'
import { encodeZkm2, decodeZkm2, ZkmCodecError, verifyZkm2 } from '@mamem/zk'
import { parseProgram, ParseError } from '@mamem/datalog'
import { issueDisclosure, verifyDisclosure, fieldPath, fieldRoot, randSalt } from '@mamem/identity'
import { AutomatonChain, decodeMamemAutomaton } from '@mamem/automaton'
import { MamemMemory } from '@mamem/mcp'
import { findLastOpReturn } from 'runar-sdk'

const SEED = Number(process.env.MAMEM_FUZZ_SEED ?? 0x6d616d)
const N = Number(process.env.MAMEM_FUZZ_N ?? 300)
function rng (seed: number) { let s = seed >>> 0; return () => { s ^= s << 13; s ^= s >>> 17; s ^= s << 5; return (s >>> 0) / 2 ** 32 } }
const R = rng(SEED)
const int = (n: number) => Math.floor(R() * n)

/** one random structural mutation of a byte string */
function mutate (b: Uint8Array): Uint8Array {
  const c = Uint8Array.from(b)
  switch (int(5)) {
    case 0: { const i = int(c.length); c[i] = c[i]! ^ (1 << int(8)); return c } // bit flip
    case 1: return c.subarray(0, int(c.length)) // truncate
    case 2: return new Uint8Array([...c, ...Array.from({ length: 1 + int(8) }, () => int(256))]) // extend
    case 3: { const i = int(c.length); c[i] = int(256); return c } // byte replace
    default: { const i = int(c.length), j = int(c.length); [c[i], c[j]] = [c[j]!, c[i]!]; return c } // swap
  }
}
const hex = (b: Uint8Array) => Buffer.from(b).toString('hex')
const unhex = (s: string) => new Uint8Array(Buffer.from(s, 'hex'))

async function anchoredWorld () {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(12); for (const t of funder.parents) chain.registerParent(t)
  const mem = new MamemMemory(chain, funder, funder.keyPair, { batchPolicy: { maxLeaves: 2 } })
  await mem.remember('f', 'a'); await mem.remember('f', 'b')
  chain.mine(1); mem.confirmAnchor('f', mem.tailLink('f')!.txid)
  return { chain, funder, mem }
}

test(`fuzz: MamemAnchor decoder is byte-authenticated (seed ${SEED}, n ${N})`, async () => {
  const { mem } = await anchoredWorld()
  const script = mem.anchorTx(mem.tailLink('f')!.txid)!.outputs[0]!.lockingScript.toHex()
  const orig = decodeMamemAnchor(script)!
  const code = script.slice(0, findLastOpReturn(script))
  let stateOnly = 0
  for (let i = 0; i < N; i++) {
    const m = hex(mutate(unhex(script)))
    if (m === script) continue
    const d = decodeMamemAnchor(m) // must never throw
    if (!d) continue
    // an accepted mutation can only be a well-formed DIFFERENT state under the
    // identical contract code — never altered code, never the same state
    assert.equal(m.slice(0, code.length), code, `mutation ${i}: code changed yet decoded`)
    assert.notDeepEqual(d, orig)
    stateOnly++
  }
  console.log(`  anchor: ${stateOnly}/${N} mutations were well-formed state-only rewrites (authentic only if on-chain)`)
})

test('fuzz: proof bundle verifier never throws, never accepts a mutated bundle', async () => {
  const { chain, mem } = await anchoredWorld()
  const { bundle } = await mem.prove('f', 0)
  assert.deepEqual(await verifyFragment(bundle!, chain), { ok: true })
  for (let i = 0; i < Math.min(N, 120); i++) {
    const b = JSON.parse(JSON.stringify(bundle))
    switch (int(6)) {
      case 0: b.leaf = hex(mutate(unhex(b.leaf))); break
      case 1: b.anchorBEEF = Array.from(mutate(Uint8Array.from(b.anchorBEEF))); break
      case 2: b.leafIndex = int(5) - 1; break
      case 3: b.size = int(5); break
      case 4: if (b.path.length) b.path[0] = hex(mutate(unhex(b.path[0]))); else b.path.push('00'); break
      default: b.chainTail[0].Rp = hex(mutate(unhex(b.chainTail[0].Rp)))
    }
    if (JSON.stringify(b) === JSON.stringify(bundle)) continue
    const r = await verifyFragment(b, chain)
    assert.equal(r.ok, false, `mutation ${i} accepted`)
  }
})

test('fuzz: L_ZKMem v2 codec + STARK verifier never throw and never accept a mutation', async () => {
  const { readFileSync } = await import('node:fs')
  const fx = JSON.parse(readFileSync(new URL('../../../zk/test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
  const H = (h: string) => Uint8Array.from(Buffer.from(h, 'hex'))
  const valid = encodeZkm2({ imageId: H(fx.imageId), root: H(fx.journal.root), size: fx.journal.size, queryId: H(fx.journal.queryId), receipt: H(fx.receipt) })
  assert.ok(verifyZkm2(valid).ok, 'fixture verifies')
  for (let i = 0; i < Math.ceil(N / 3); i++) {
    const m = mutate(valid)
    try {
      const d = decodeZkm2(m)
      assert.deepEqual(encodeZkm2(d), m, 'accepted bytes re-encode identically (canonical)')
    } catch (e) { assert.ok(e instanceof ZkmCodecError, `untyped error: ${String(e)}`) }
    const v = verifyZkm2(m) // must return, never throw
    if (v.ok) assert.deepEqual(m, valid, `mutation ${i} accepted as a different proof`)
  }
})

test('fuzz: L_pol parser throws only ParseError on garbage', () => {
  const src = '@id r1\nallowed(P, F) :- member(P, verified), fragment(F), not revoked(P).\nmember(a, verified).\nfragment(f1).'
  const alphabet = '()[],.:-_ "@\nabcXYZ019\\\'notNOT'
  for (let i = 0; i < N * 3; i++) {
    const chars = [...src]
    for (let k = 0; k <= int(4); k++) chars.splice(int(chars.length + 1), int(3), alphabet[int(alphabet.length)]!)
    try { parseProgram(chars.join('')) } catch (e) { assert.ok(e instanceof ParseError, `untyped error: ${String(e)} for ${JSON.stringify(chars.join(''))}`) }
  }
})

test('fuzz: disclosure envelope verifier rejects every byte-level mutation', () => {
  const owner = PrivateKey.fromRandom(); const verifier = PrivateKey.fromRandom().toPublicKey().toString()
  const fields = ['serial', 'torque', 'inspector'].map(n => ({ name: n, value: utf8(`${n}-value`), salt: randSalt() }))
  const env = issueDisclosure(owner, fields, 1, verifier, 2_000_000_000)
  const root = fieldRoot(fields); const sibs = fieldPath(fields, 1).siblings
  const ownerPub = owner.toPublicKey().toString()
  assert.equal(verifyDisclosure(env, ownerPub, verifier, 3, root, sibs, 1).ok, true)
  for (let i = 0; i < N; i++) {
    const e = { ...env, field: { ...env.field } }
    switch (int(5)) {
      case 0: e.field.value = mutate(e.field.value); break
      case 1: e.field.salt = mutate(e.field.salt); break
      case 2: e.nonce = mutate(e.nonce); break
      case 3: e.sig = hex(mutate(unhex(e.sig))); break
      default: e.fieldIndex = int(3) === 1 ? 0 : int(3)
    }
    const same = toHex(e.field.value) === toHex(env.field.value) && toHex(e.field.salt) === toHex(env.field.salt) && toHex(e.nonce) === toHex(env.nonce) && e.sig === env.sig && e.fieldIndex === env.fieldIndex
    if (same) continue
    assert.equal(verifyDisclosure(e, ownerPub, verifier, 3, root, sibs, 1).ok, false, `mutation ${i} accepted`)
  }
})

test('fuzz: MamemAutomaton decoder is byte-authenticated', async () => {
  const chain = new MockChain(); chain.verifyScripts = true
  const funder = new MockFunder(); funder.topup(4); for (const t of funder.parents) chain.registerParent(t)
  const ac = new AutomatonChain({ Q: ['a', 'b'], Sigma: ['x'], delta: [[0, 0, 1]], q0: 0, F: [1], O: [] }, PrivateKey.fromRandom(), funder, funder.keyPair, chain)
  const script = (await ac.deploy()).tx.outputs[0]!.lockingScript.toHex()
  const code = script.slice(0, findLastOpReturn(script))
  const orig = decodeMamemAutomaton(script)
  for (let i = 0; i < N; i++) {
    const m = hex(mutate(unhex(script)))
    if (m === script) continue
    const d = decodeMamemAutomaton(m)
    if (!d) continue
    assert.equal(m.slice(0, code.length), code, `mutation ${i}: code changed yet decoded`)
    assert.notDeepEqual(d, orig)
  }
})
