import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Hash, PrivateKey, ProtoWallet, Signature, Utils, WalletClient, type WalletInterface } from '@bsv/sdk'
import { keySigner, walletSigner } from '../src/signer.js'

// NEKTAR_WALLET=client runs every signing surface through WalletClient — an
// external BRC-100 substrate. A ProtoWallet stands in as the local substrate
// so the full call path (WalletClient delegation → walletSigner seam) is
// exercised the way production uses it.
//
// Known substrate semantics (sdk 4.x): createSignature signs under a derived
// key for {protocolID,keyID,counterparty} — `identityKey` selects the pub for
// getPublicKey but cannot root-sign — so a wallet sig validates through the
// wallet's own verifySignature, not raw pub.verify against getPublicKey.
// Anything needing an ECDSA-verifiable sig under the reported pub (P2PKH
// unlocks, artifact sigs) therefore stays on key custody or a wallet path
// that root-signs (e.g. createAction spends) — asserted by the tests below.
test('signer: walletSigner over a WalletClient substrate — identity pub is the root key', async () => {
  const key = new PrivateKey(7)
  const wallet = new WalletClient(new ProtoWallet(key) as unknown as WalletInterface, 'nektar')
  const s = walletSigner(wallet)
  assert.equal(await s.pubKeyHex(), key.toPublicKey().toString())
})

test('signer: walletSigner signatures verify through the same client surface', async () => {
  const key = new PrivateKey(7)
  const wallet = new WalletClient(new ProtoWallet(key) as unknown as WalletInterface, 'nektar')
  const keyArgs = { protocolID: [2, 'nektar'] as [number, string], keyID: 'nektar-log', counterparty: 'self' }
  const s = walletSigner(wallet, keyArgs)
  const digest = Hash.sha256(Utils.toArray('digest', 'utf8'))
  const sig = await s.sign(digest)
  const r = await wallet.verifySignature({
    ...keyArgs,
    hashToDirectlyVerify: Array.from(digest),
    signature: Array.from(sig.toDER() as number[]),
    forSelf: true
  } as never)
  assert.equal(r.valid, true)
})

test('signer: walletSigner deriveSecret is deterministic and wallet-scoped', async () => {
  const mk = (n: number) =>
    walletSigner(new WalletClient(new ProtoWallet(new PrivateKey(n)) as unknown as WalletInterface, 'nektar'), {
      protocolID: [2, 'nektar fragment'],
      keyID: 'shard:guide:0',
      forSelf: true
    })
  assert.deepEqual(await mk(7).deriveSecret!('pre', 'post'), await mk(7).deriveSecret!('pre', 'post'))
  assert.notDeepEqual(await mk(7).deriveSecret!('pre', 'post'), await mk(8).deriveSecret!('pre', 'post'))
})

test('signer: keySigner produces a raw ECDSA-verifiable signature', async () => {
  const key = new PrivateKey(11)
  const s = keySigner(key)
  const digest = Hash.sha256(Utils.toArray('same digest', 'utf8'))
  const sig = await s.sign(digest)
  assert.ok(key.toPublicKey().verify(Array.from(digest), sig))
  assert.equal(await s.pubKeyHex(), key.toPublicKey().toString())
})
