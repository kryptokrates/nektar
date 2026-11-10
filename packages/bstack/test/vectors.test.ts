import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { PrivateKey, ProtoWallet, Transaction } from '@bsv/sdk'
import { fundingLock, mintCarrier, sweep } from '@lightwebinc/bcommon'

const hex = (s: string): Uint8Array => new Uint8Array(Buffer.from(s, 'hex'))
const v = JSON.parse(readFileSync(new URL('../../test/vectors/transactions-v1.json', import.meta.url), 'utf8'))

test('golden: bcommon writer is byte-equal to the Go vector under @bsv/sdk 4.0.1', async () => {
  const w = new ProtoWallet(PrivateKey.fromHex(v.privateKeyHex))
  const d = { protocol: [v.securityLevel, v.protocol] as import('@bsv/sdk').WalletProtocol, keyID: v.objectKeyId as string }
  const tag = hex(v.fundingTagHex)
  const tree = Transaction.fromHex(v.fundingTree.txHex)
  assert.equal((await fundingLock(w, v.originator, d, tag)).toHex(), v.fundingLockHex)
  for (const c of v.carriers) assert.equal((await mintCarrier(w, v.originator, d, hex(c.payloadHex), tree, c.vout)).toHex(), c.txHex)
  const sw = await sweep(w, v.originator, d, tag, tree, v.sweep.vouts, { sats: v.satPerByte, bytes: 1, floor: v.floor })
  assert.equal(sw.toHex(), v.sweep.txHex)
})
