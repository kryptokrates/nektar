// Contract-level tests: deploy NektarAnchor, call append with correct and
// incorrect args, on runar-sdk MockProvider (evaluates the real compiled
// script — same bytes Teranode will execute).
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { contractRp, TAG_RPRIME, META_FLAG_HEX } from '../src/index.js'
import { hash160 } from '@nektar/core'
import { Buffer } from 'node:buffer'

const pkh = (pk: string) => Buffer.from(hash160(Uint8Array.from(Buffer.from(pk, 'hex')))).toString('hex')

const artifact = JSON.parse(readFileSync(path.join(fileURLToPath(import.meta.url), '..', '..', '..', 'NektarAnchor.json'), 'utf8'))
const signer = new LocalSigner('11'.repeat(32))

function setup(n = 0n, size = 10n) {
  const prov = new MockProvider('testnet')
  return signer.getPublicKey().then((pk) => {
    prov.addUtxo('1Q1pE5vPGEEMqRcVRMbtBK842Y6Pzo6nK9', { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
    const c = new RunarContract(artifact, [pk, pkh(pk), META_FLAG_HEX, n, size, '11'.repeat(32), '22'.repeat(32), Buffer.from(TAG_RPRIME).toString('hex')])
    c.connect(prov, signer)
    return c
  })
}

test('contract: genesisOwner carried unchanged across append and rotate', async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk = await signer.getPublicKey()
  const lock0 = c.getLockingScript()
  assert.ok(lock0.includes(pk), 'genesisOwner in state suffix')
  await c.call('append', ['00'.repeat(72), pk, '33'.repeat(32), 20n])
  const lock1 = c.getLockingScript()
  assert.ok(lock1.includes(pk), 'genesisOwner unchanged after append')
  const pkh = (x: string) => Buffer.from(hash160(Uint8Array.from(Buffer.from(x, 'hex')))).toString('hex')
  const pk2 = '03' + '5a'.repeat(32)
  await c.call('rotate', ['00'.repeat(72), pk, pkh(pk2)])
  const lock2 = c.getLockingScript()
  assert.ok(lock2.includes(pk), 'genesisOwner unchanged after rotate')
  assert.ok(lock2.includes(pkh(pk2).slice(0, 20)), 'ownerPkh rotated')
  // decode keeps the same shard id across the rotate
  const { decodeNektarAnchor } = await import('../src/index.js')
  const d0 = decodeNektarAnchor(lock1)!,
    d1 = decodeNektarAnchor(lock2)!
  assert.equal(d0.shard, d1.shard, 'shard = hash160(genesisOwner) stable')
})

test("contract: deploy + append produces n+1 and correct R'", async () => {
  const c = await setup()
  await c.deploy({ satoshis: 1 })
  const pk = await signer.getPublicKey()
  const r = await c.call('append', ['00'.repeat(72), pk, '33'.repeat(32), 20n])
  assert.ok(r.txid)
  const st: any = c.state
  assert.equal(BigInt(st.n), 1n)
  assert.equal(BigInt(st.size), 20n)
  // R' must equal hash256(tag‖Rp‖R‖size_le8) = contractRp
  const expected = contractRp(
    Uint8Array.from({ length: 32 }, () => 0x22),
    Uint8Array.from({ length: 32 }, () => 0x33),
    20
  )
  assert.equal(String(st.Rp), Buffer.from(expected).toString('hex'))
})
