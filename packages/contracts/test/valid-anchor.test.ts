import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'
import { RunarContract, MockProvider, LocalSigner } from 'runar-sdk'
import { decodeMamemAnchor, isValidAnchor, contractRp, TAG_RPRIME, META_FLAG_HEX } from '../src/index.js'
import { chainRp, hash160 } from '@mamem/core'

const art = JSON.parse(readFileSync(path.join(fileURLToPath(import.meta.url), '..', '..', '..', 'MamemAnchor.json'), 'utf8'))
const signer = new LocalSigner('11'.repeat(32))

test('isValidAnchor: deployed anchor decodes with TAG_RPRIME', async () => {
  const prov = new MockProvider('testnet')
  const pk = await signer.getPublicKey()
  prov.addUtxo(await signer.getAddress(), { txid: 'ab'.repeat(32), outputIndex: 0, satoshis: 1e6, script: '' })
  const tag = Buffer.from(TAG_RPRIME).toString('hex')
  const Rp0 = contractRp(new Uint8Array(32), Uint8Array.from({ length: 32 }, () => 0x11), 10)
  const c = new RunarContract(art, [pk, Buffer.from(hash160(Uint8Array.from(Buffer.from(pk, 'hex')))).toString('hex'), META_FLAG_HEX, 0n, 10n, '11'.repeat(32), Buffer.from(Rp0).toString('hex'), tag])
  c.connect(prov, signer)
  await c.deploy({ satoshis: 1 })
  const d = decodeMamemAnchor(c.getLockingScript())
  assert.ok(d, 'decodes')
  assert.equal(d!.n, 0); assert.equal(d!.size, 10); assert.equal(d!.ownerPub, pk)
  assert.ok(isValidAnchor(d!), 'tag == TAG_RPRIME')
  assert.ok(!isValidAnchor({ ...d!, tag: new Uint8Array([0x00]) }), 'wrong tag rejected')
})

test('contractRp == chainRp on vectors', () => {
  for (const [size, Rb, Rpb] of [[10, 0x11, 0x22], [20, 0x33, 0x44], [1, 0xab, 0xcd]] as const) {
    const a = contractRp(Uint8Array.from({ length: 32 }, () => Rpb), Uint8Array.from({ length: 32 }, () => Rb), size)
    const b = chainRp(Uint8Array.from({ length: 32 }, () => Rpb), Uint8Array.from({ length: 32 }, () => Rb), size)
    assert.deepEqual([...a], [...b])
  }
})
