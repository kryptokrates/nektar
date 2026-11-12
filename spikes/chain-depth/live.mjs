// Spike B (live): split the funded coinbase into N funding outputs, then chain
// successive unconfirmed nektar anchors on regtest until propagation rejects.
import { PrivateKey, Transaction, P2PKH } from '@bsv/sdk'
import { AnchorChain } from '../../packages/anchor/dist/src/anchorchain.js'
import { TeranodeBroadcaster, TeranodeRPCTracker } from '../../packages/chain/dist/src/regtest.js'
import { dsha256, utf8 } from '../../packages/core/dist/src/index.js'
import { loadState, saveState, WIF, PROPAGATION_URL, RPC } from './../lib/regtest.mjs'

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const N = Number(process.env.SPLIT ?? 1100)
const SPLITTX_PATH = new URL('./split-tx.json', import.meta.url).pathname

// 1) funding: split coinbase into N ~8000sat outputs to self
let splitTx
const s = loadState()
if (s.splitTxid) {
  const raw = await fetch(`http://localhost:8090/api/v1/tx/${s.splitTxid}`, {
    headers: { Authorization: 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64') }
  })
  splitTx = Transaction.fromEF(new Uint8Array(await raw.arrayBuffer()))
  console.log('reusing split tx', s.splitTxid)
} else {
  const u = s.fundingUtxo
  splitTx = new Transaction()
  splitTx.addInput({ sourceTransaction: Transaction.fromHex(u.cbHex), sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  const sats = 8000
  for (let i = 0; i < N; i++) splitTx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
  splitTx.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
  await splitTx.fee()
  await splitTx.sign()
  const r = await fetch(PROPAGATION_URL, { method: 'POST', headers: { 'Content-Type': 'application/octet-stream' }, body: Buffer.from(splitTx.toHex(), 'hex') })
  const t = await r.text()
  console.log('split broadcast:', r.status, t.slice(0, 120), 'txid', splitTx.id('hex'))
  if (!r.ok) process.exit(1)
  s.splitTxid = splitTx.id('hex')
  saveState(s)
}

// 2) chain anchors until rejected
class SplitFunder {
  constructor(tx) {
    this.tx = tx
    this.i = 0
  }
  async take(n) {
    if (this.i >= this.tx.outputs.length - 1) throw new Error('funder exhausted')
    return { tx: this.tx, vout: this.i++ }
  }
}
const tracker = new TeranodeRPCTracker({
  rpcUrl: RPC.url,
  rpcUser: RPC.user,
  rpcPass: RPC.pass,
  propagationUrl: PROPAGATION_URL,
  assetUrl: 'http://localhost:8090/api/v1'
})
const broadcaster = new TeranodeBroadcaster(PROPAGATION_URL)
const ac = new AnchorChain('depth', PrivateKey.fromRandom(), new SplitFunder(splitTx), key, broadcaster)
let i = 0,
  lastErr = null
try {
  for (; i < 1000; i++) {
    await ac.anchor(1, dsha256(utf8(`depth-${i}`)))
    if (i % 25 === 0) console.log('anchored', i)
  }
} catch (e) {
  lastErr = e.message
}
console.log(`UNCONFIRMED DEPTH LIMIT: ${i} anchors accepted before rejection`)
console.log('error:', lastErr)
