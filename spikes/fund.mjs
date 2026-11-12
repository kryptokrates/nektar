// Fund the regtest wallet: mine blocks to our address, record a mature
// coinbase UTXO in spikes/.state.json. Usage: node spikes/fund.mjs
import { PrivateKey, Transaction } from '@bsv/sdk'
import { rpc, loadState, saveState, WIF, RPC } from './lib/regtest.mjs'

const key = PrivateKey.fromWif(WIF)
const address = key.toPublicKey().toAddress([0x6f]) // regtest/testnet prefix
console.log('funding address:', address)

// Mine 110 blocks so the first coinbases are mature (>100 conf).
const height = (await rpc('getblockchaininfo')).blocks
console.log('current height:', height)
const hashes = await rpc('generatetoaddress', 110, address)
console.log('mined', hashes.length, 'blocks, new height:', (await rpc('getblockchaininfo')).blocks)

// Take the first mined block's coinbase as our funding UTXO.
// Teranode getblock does not list txids; fetch the raw block from the asset
// API (80-byte header + varint txcount + coinbase tx, single-tx block).
const blockRes = await fetch(`http://localhost:8090/api/v1/block/${hashes[0]}`, {
  headers: { Authorization: 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64') }
})
const raw = Buffer.from(await blockRes.arrayBuffer())
let off = 80
const readVarint = (b, o) => {
  if (b[o] < 0xfd) return [b[o], o + 1]
  if (b[o] === 0xfd) return [b.readUInt16LE(o + 1), o + 3]
  if (b[o] === 0xfe) return [b.readUInt32LE(o + 1), o + 5]
  return [Number(b.readBigUInt64LE(o + 1)), o + 9]
}
let n
;[n, off] = readVarint(raw, off) // tx count
if (raw[off] === 0xbf && raw[off + 1] === 0x00) off += 2 // teranode EF block marker
const txStart = off
off += 4 // version
let vin
;[vin, off] = readVarint(raw, off)
for (let i = 0; i < vin; i++) {
  off += 36
  let sl
  ;[sl, off] = readVarint(raw, off)
  off += sl + 4
}
let vout
;[vout, off] = readVarint(raw, off)
for (let i = 0; i < vout; i++) {
  off += 8
  let sl
  ;[sl, off] = readVarint(raw, off)
  off += sl
}
off += 4 // locktime
const cb = Transaction.fromHex(raw.subarray(txStart, off).toString('hex'))
const cbTxid = cb.id('hex')
const pkHex = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const idx = cb.outputs.findIndex((o) => o.lockingScript.toHex().includes(pkHex))
if (idx < 0) throw new Error('coinbase output to our key not found')

const state = loadState()
state.fundingUtxo = {
  txid: cbTxid,
  vout: idx,
  satoshis: cb.outputs[idx].satoshis,
  height: (await rpc('getblock', hashes[0])).height,
  cbHex: raw.subarray(txStart, off).toString('hex')
}
saveState(state)
console.log('funding UTXO:', state.fundingUtxo)
