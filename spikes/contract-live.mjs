// Live regtest proof using the PRODUCTION AnchorChain — fully self-contained:
// mine a fresh coinbase, split it into funding outputs to self, fund only
// from that fresh split (each output marked used when handed out).
import { AnchorChain } from '../packages/anchor/dist/src/index.js'
import { Transaction, PrivateKey, P2PKH } from './lib/sdk.mjs'
import { WIF, RPC, rpc, broadcastEF, loadState, saveState } from './lib/regtest.mjs'
import { dsha256, utf8, toHex } from '../packages/core/dist/src/index.js'
import { createRequire } from 'node:module'
const require2 = createRequire(import.meta.url)
const { TeranodeBroadcaster } = require2('../packages/chain/dist/src/regtest.js')

const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const state = loadState()

// --- 1) fresh mature coinbase (from the raw block, like fund.mjs) ----------
const auth = 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64')
const hashes = await rpc('generatetoaddress', 110, addr)
const blockRes = await fetch(`http://localhost:8090/api/v1/block/${hashes[0]}`, { headers: { Authorization: auth } })
const raw = Buffer.from(await blockRes.arrayBuffer())
let off = 80
const readVarint = (b, o) => {
  if (b[o] < 0xfd) return [b[o], o + 1]
  if (b[o] === 0xfd) return [b.readUInt16LE(o + 1), o + 3]
  if (b[o] === 0xfe) return [b.readUInt32LE(o + 1), o + 5]
  return [Number(b.readBigUInt64LE(o + 1)), o + 9]
}
let n; [n, off] = readVarint(raw, off)
if (raw[off] === 0xbf && raw[off + 1] === 0x00) off += 2
const txStart = off
off += 4
let vin; [vin, off] = readVarint(raw, off)
for (let i = 0; i < vin; i++) { off += 36; let sl; [sl, off] = readVarint(raw, off); off += sl + 4 }
let vout; [vout, off] = readVarint(raw, off)
for (let i = 0; i < vout; i++) { off += 8; let sl; [sl, off] = readVarint(raw, off); off += sl }
off += 4
const cb = Transaction.fromHex(raw.subarray(txStart, off).toString('hex'))
const pkHex = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const cbv = cb.outputs.findIndex(o => o.lockingScript.toHex().includes(pkHex))

// --- 2) fresh split tx: 50 outputs to self ---------------------------------
const split = new Transaction()
split.addInput({ sourceTransaction: cb, sourceOutputIndex: cbv, unlockingScriptTemplate: new P2PKH().unlock(key) })
const N = 50, sats = Math.floor((cb.outputs[cbv].satoshis - 2000) / (N + 1))
for (let i = 0; i < N; i++) split.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats })
split.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
await split.fee(); await split.sign()
const br = await broadcastEF(split)
if (br.status !== 200 && br.status !== 204) throw new Error(`split broadcast ${br.status}: ${br.body.slice(0, 200)}`)
await rpc('generatetoaddress', 1, addr)
console.log('split:', split.id('hex'))

// --- 3) Funder over the fresh split ----------------------------------------
let cursor = 0
const funder = {
  keyPair: key,
  utxos: [],
  async take () {
    const vout = cursor++
    const u = { tx: split, vout, satoshis: split.outputs[vout].satoshis }
    this.utxos.push(u)
    return u
  }
}
// runar's getUtxos reads funder.utxos — prefill all
for (let i = 0; i < N; i++) funder.utxos.push({ tx: split, vout: i, satoshis: split.outputs[i].satoshis })

const broadcaster = new TeranodeBroadcaster('http://localhost:8833/tx')
const ac = new AnchorChain('live', key, funder, key, broadcaster)

const R = (i) => dsha256(utf8(`live-${i}`))
const txids = []
try {
  for (let i = 0; i < 4; i++) {
    const r = await ac.anchor(1, R(i))
    txids.push(r.tx.id('hex'))
    console.log(`${i === 0 ? 'genesis' : 'append ' + i}:`, r.tx.id('hex'))
  }
} catch (e) { console.log('ERR:', e.message.slice(0, 300)) }
try { await rpc('generatetoaddress', 1, addr); console.log('mined 1 block') } catch (e) { console.log('mine:', e.message.slice(0, 100)) }
state.liveContractTxids = txids
state.liveSplitTxid = split.id('hex')
saveState(state)
console.log('DONE')
