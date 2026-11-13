import { test } from 'node:test'
import assert from 'node:assert/strict'
import { Hash, P2PKH, PrivateKey, ProtoWallet, PublicKey, Signature, Transaction, Utils, WalletClient, type WalletInterface } from '@bsv/sdk'
import { custodySigner, keySigner, unlockP2PKH, walletSigner, FUND_SIGN_ARGS } from '../src/signer.js'

// NEKTAR_WALLET=client runs every signing surface through WalletClient — an
// external BRC-100 substrate. A ProtoWallet stands in as the local substrate
// so the full call path (WalletClient delegation → walletSigner seam) is
// exercised the way production uses it.
//
// Known substrate semantics (sdk 4.x): createSignature signs under a derived
// key for {protocolID,keyID,counterparty} — `identityKey` selects the pub for
// getPublicKey but cannot root-sign — and on ProtoWallet the derived pub/sig
// pair fails raw-ECDSA parity (a substrate limitation). Funding spends under
// external custody therefore use a consistent derived pair (FUND_SIGN_ARGS):
// on conformant substrates getPublicKey(args) is the pub whose priv signs
// under createSignature(args). unlockP2PKH self-verifies the pair at sign
// time so a non-conformant substrate fails loudly instead of broadcasting a
// script that can never evaluate — asserted below.
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

/** a funding tx (P2PKH → key) + a spend tx wired through unlockP2PKH — the
 *  shape every funding call site builds. */
const spendOf = async (signer: Parameters<typeof unlockP2PKH>[0], payTo: PrivateKey): Promise<Transaction> => {
  const funding = new Transaction()
  funding.addOutput({ satoshis: 2000, lockingScript: new P2PKH().lock(payTo.toAddress()) })
  const tx = new Transaction()
  tx.addInput({ sourceTransaction: funding, sourceOutputIndex: 0, unlockingScriptTemplate: unlockP2PKH(signer) })
  tx.addOutput({ satoshis: 1000, lockingScript: new P2PKH().lock(payTo.toAddress()) })
  await tx.sign()
  return tx
}

test('signer: unlockP2PKH(keySigner) emits a self-consistent unlocking script', async () => {
  const key = new PrivateKey(42)
  const tx = await spendOf(keySigner(key), key)
  const chunks = tx.inputs[0].unlockingScript!.chunks
  const sig = Signature.fromDER(chunks[0].data!.slice(0, -1) as number[])
  const pub = PublicKey.fromString(Utils.toHex(chunks[1].data as number[]))
  const funding = tx.inputs[0].sourceTransaction!
  const sighash = Hash.sha256(tx.preimage(0, 0x41, funding.outputs[0].lockingScript))
  assert.ok(pub.verify(Array.from(sighash), sig))
  assert.equal(pub.toString(), key.toPublicKey().toString())
})

test('signer: unlockP2PKH(walletSigner) rejects when the substrate lacks pub/sig parity', async () => {
  // ProtoWallet's derived createSignature/getPublicKey pair is not raw-ECDSA
  // consistent — the self-verify must throw rather than emit a dead script.
  const key = new PrivateKey(42)
  const wallet = new WalletClient(new ProtoWallet(key) as unknown as WalletInterface, 'nektar')
  await assert.rejects(spendOf(walletSigner(wallet, FUND_SIGN_ARGS), key), /does not verify under its public key/)
})

test('signer: custodySigner uses the held keyDeriver root key when present', async () => {
  const key = new PrivateKey(7)
  const toolboxLike = { keyDeriver: { rootKey: key } } as unknown as Parameters<typeof custodySigner>[0]
  const s = custodySigner(toolboxLike, FUND_SIGN_ARGS)
  assert.equal(await s.pubKeyHex(), key.toPublicKey().toString())
  const digest = Hash.sha256(Utils.toArray('x', 'utf8'))
  assert.ok(key.toPublicKey().verify(Array.from(digest), await s.sign(digest)))
})

test('signer: custodySigner falls back to the wallet derivation without a held key', async () => {
  const key = new PrivateKey(7)
  const wallet = new WalletClient(new ProtoWallet(key) as unknown as WalletInterface, 'nektar')
  const s = custodySigner(wallet, FUND_SIGN_ARGS)
  // external custody: pub is the derived pair pub (not the root), stable across calls
  const pub = await s.pubKeyHex()
  assert.notEqual(pub, key.toPublicKey().toString())
  assert.equal(await s.pubKeyHex(), pub)
})
