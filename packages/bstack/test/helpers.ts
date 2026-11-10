import { PrivateKey, Transaction, P2PKH, MerklePath } from '@bsv/sdk'
import { mintTree } from '../src/tree.js'
import type { FundingOut } from '@nektar/chain'

export function fakeParent (key: PrivateKey, sats = 1_000_000): Transaction {
  const p = new Transaction()
  p.addOutput({ satoshis: sats, lockingScript: new P2PKH().lock(key.toPublicKey().toAddress()) })
  return p
}
export function fund (key: PrivateKey, sats = 1_000_000): FundingOut { return { tx: fakeParent(key, sats), vout: 0, satoshis: sats } }
export function prove (tx: Transaction, sibling = '00'.repeat(32)): void {
  const h = tx.id('hex')
  tx.merklePath = new MerklePath(1, [[{ offset: 0, hash: sibling }, { offset: 1, hash: h, txid: true }]])
}
export async function provenTree (key: PrivateKey, n = 8): Promise<Transaction> {
  const t = await mintTree({ key, funding: fund(key), n, satsPerOutput: 1000 })
  prove(t)
  return t
}
