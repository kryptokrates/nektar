// Spend the funded coinbase via propagation /tx — proves sign+broadcast+script-eval on regtest.
import { PrivateKey, Transaction, P2PKH } from '@bsv/sdk'
import { rpc, loadState, WIF } from './lib/regtest.mjs'

const key = PrivateKey.fromWif(WIF)
const s = loadState()
const u = s.fundingUtxo
const parent = Transaction.fromHex(u.cbHex)
console.log('parent txid:', parent.id('hex'), 'satoshis:', parent.outputs[u.vout]?.satoshis)

const tx = new Transaction()
tx.addInput({ sourceTransaction: parent, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx.addOutput({ lockingScript: new P2PKH().lock(key.toPublicKey().toAddress([0x6f])), change: true })
await tx.fee(); await tx.sign()
const txid = tx.id('hex')
console.log('signed txid:', txid)
for (const [name, url, body] of [
  ['propagation-raw', 'http://127.0.0.1:8833/tx', Buffer.from(tx.toHex(), 'hex')],
]) {
  try {
    const r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body })
    console.log(name, r.status, (await r.text()).slice(0, 200))
  } catch (e) { console.log(name, 'ERR', e.message) }
}
