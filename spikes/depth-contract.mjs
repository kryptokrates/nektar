// Spike B (contract anchors): chain unconfirmed appends on regtest until the
// node rejects or 30 s cap. Measures the real unconfirmed-depth limit for the
// Rúnar contract path.
import { AnchorChain } from '../packages/anchor/dist/src/index.js'
import { Transaction, PrivateKey, P2PKH } from './lib/sdk.mjs'
import { WIF, RPC, rpc, broadcastEF } from './lib/regtest.mjs'
import { dsha256, utf8 } from '../packages/core/dist/src/index.js'
import { createRequire } from 'node:module'
const require2 = createRequire(import.meta.url)
const { TeranodeBroadcaster } = require2('../packages/chain/dist/src/regtest.js')

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const auth = 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64')
const readV = (b, o) => {
  if (b[o] < 0xfd) return [b[o], o + 1]
  if (b[o] === 0xfd) return [b.readUInt16LE(o + 1), o + 3]
  if (b[o] === 0xfe) return [b.readUInt32LE(o + 1), o + 5]
  return [Number(b.readBigUInt64LE(o + 1)), o + 9]
}
// fresh coinbase → funding split
const hashes = await rpc('generatetoaddress', 110, addr)
const br = await fetch(`http://localhost:8090/api/v1/block/${hashes[0]}`, { headers: { Authorization: auth } })
const raw = Buffer.from(await br.arrayBuffer())
let off = 80
let n; [n, off] = readV(raw, off)
if (raw[off] === 0xbf && raw[off + 1] === 0x00) off += 2
const ts = off
off += 4
let vin; [vin, off] = readV(raw, off)
for (let i = 0; i < vin; i++) { off += 36; let sl; [sl, off] = readV(raw, off); off += sl + 4 }
let vo; [vo, off] = readV(raw, off)
for (let i = 0; i < vo; i++) { off += 8; let sl; [sl, off] = readV(raw, off); off += sl }
off += 4
const cb = Transaction.fromHex(raw.subarray(ts, off).toString('hex'))
const pkHex = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const cbv = cb.outputs.findIndex(o => o.lockingScript.toHex().includes(pkHex))
const N = 120
const split = new Transaction()
split.addInput({ sourceTransaction: cb, sourceOutputIndex: cbv, unlockingScriptTemplate: new P2PKH().unlock(key) })
const sats = Math.floor((cb.outputs[cbv].satoshis - 2000) / (N + 1))
for (let i = 0; i < N; i++) split.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
split.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
await split.fee(); await split.sign()
await broadcastEF(split)
await rpc('generatetoaddress', 1, addr)
const funder = { keyPair: key, utxos: [] }
for (let i = 0; i < N; i++) funder.utxos.push({ tx: split, vout: i, satoshis: sats })

const ac = new AnchorChain('depth', key, funder, key, new TeranodeBroadcaster('http://localhost:8833/tx'))
const t0 = Date.now()
let count = 0, lastErr = ''
while (Date.now() - t0 < 30000 && count < 100) {
  try {
    await ac.anchor(1, dsha256(utf8(`d-${count}`)))
    count++
    if (count % 10 === 0) console.log(`${count} anchors ${Date.now() - t0}ms`)
  } catch (e) { lastErr = e.message; break }
}
console.log(`RESULT: ${count} unconfirmed contract anchors chained in ${Date.now() - t0}ms`)
console.log(lastErr ? `stopped at ${count}: ${lastErr.slice(0, 160)}` : 'no limit hit')
