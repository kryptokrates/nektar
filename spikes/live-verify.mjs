// Verify a live contract anchor's proof against real regtest headers.
// Finds the anchor txid in the mined tip block, builds a MerklePath from the
// block's tx list, and runs merklePath.verify with TeranodeRPCTracker.
import { MerklePath } from './lib/sdk.mjs'
import { RPC, rpc, loadState } from './lib/regtest.mjs'
import { createRequire } from 'node:module'
import crypto from 'node:crypto'
const require2 = createRequire(import.meta.url)
const { TeranodeRPCTracker } = require2('../packages/chain/dist/src/regtest.js')

const state = loadState()
const txids = state.liveContractTxids ?? []
if (!txids.length) {
  console.log('no live txids — run contract-live.mjs first')
  process.exit(1)
}

const auth = 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64')
const readVarint = (b, o) => {
  if (b[o] < 0xfd) return [b[o], o + 1]
  if (b[o] === 0xfd) return [b.readUInt16LE(o + 1), o + 3]
  if (b[o] === 0xfe) return [b.readUInt32LE(o + 1), o + 5]
  return [Number(b.readBigUInt64LE(o + 1)), o + 9]
}
const parseBlock = (raw) => {
  let off = 88 // 80B header + Teranode's 8B sizeInBytes before tx count
  let n
  ;[n, off] = readVarint(raw, off)
  if (raw[off] === 0xbf && raw[off + 1] === 0x00) off += 2
  const txs = []
  for (let t = 0; t < n; t++) {
    const start = off
    off += 4
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
    off += 4
    txs.push(raw.subarray(start, off))
  }
  return txs
}
const sha256d = (b) => crypto.createHash('sha256').update(crypto.createHash('sha256').update(b).digest()).digest()
const txidOf = (txBytes) => Buffer.from(sha256d(txBytes)).reverse().toString('hex')

// tip block = coinbase + our 4 anchors in topological (broadcast) order.
// coinbase txid: parse the single-tx stub from /block/ (fund.mjs path).
const info = await rpc('getblockchaininfo')
const hash = await rpc('getblockhash', info.blocks)
const hdr = await rpc('getblockheader', hash)
const res = await fetch(`http://localhost:8090/api/v1/block_legacy/${hash}`, { headers: { Authorization: auth } })
const raw = Buffer.from(await res.arrayBuffer())
const txs = parseBlock(raw)
const ids = txs.map(txidOf)
console.log('block txids:', ids.map((id) => id.slice(0, 12)).join(', '))
const merkleRoot = (() => {
  let level = ids.map((id) => Buffer.from(id, 'hex').reverse().toString('hex'))
  while (level.length > 1) {
    const next = []
    for (let i = 0; i < level.length; i += 2) {
      const a = level[i],
        b = level[i + 1] ?? level[i]
      next.push(sha256d(Buffer.concat([Buffer.from(a, 'hex'), Buffer.from(b, 'hex')])).toString('hex'))
    }
    level = next
  }
  return Buffer.from(level[0], 'hex').reverse().toString('hex')
})()
console.log('computed root:', merkleRoot)
console.log('header root  :', hdr.merkleroot)
if (merkleRoot !== hdr.merkleroot) {
  console.log('txid list does not match block — abort')
  process.exit(1)
}
const found = { height: info.blocks, ids, idx: ids.findIndex((id) => txids.includes(id)) }
if (!found) {
  console.log('no live anchor found in last 6 blocks')
  process.exit(1)
}
console.log(`anchor ${found.ids[found.idx].slice(0, 16)}… in block ${found.height} (index ${found.idx}, ${found.ids.length} txs)`)

// build merkle path for tx index idx
const buildPath = (ids, idx) => {
  // all path hashes in display order; concat works on internal (reversed) bytes
  const disp = (h) => Buffer.from(h, 'hex').reverse().toString('hex')
  const leaves = ids.map((id) => Buffer.from(id, 'hex').reverse().toString('hex'))
  const path = []
  let level = leaves,
    pos = idx,
    first = true
  while (level.length > 1) {
    const sibling = pos % 2 === 0 ? pos + 1 : pos - 1
    const lvl = []
    if (first) {
      lvl.push({ offset: pos, hash: disp(leaves[pos]), txid: true })
      first = false
    }
    lvl.push({ offset: sibling, hash: disp(sibling < level.length ? level[sibling] : level[pos]) })
    path.push(lvl)
    const next = []
    for (let i = 0; i < level.length; i += 2) {
      const a = level[i],
        b = level[i + 1] ?? level[i]
      next.push(sha256d(Buffer.concat([Buffer.from(a, 'hex'), Buffer.from(b, 'hex')])).toString('hex'))
    }
    level = next
    pos = Math.floor(pos / 2)
  }
  return path
}
const mp = new MerklePath(found.height, buildPath(found.ids, found.idx))
console.log(
  'mp.computeRoot:',
  (() => {
    try {
      return mp.computeRoot()
    } catch (e) {
      return 'ERR ' + e.message.slice(0, 120)
    }
  })()
)
const tracker = new TeranodeRPCTracker({
  rpcUrl: RPC.url,
  rpcUser: RPC.user,
  rpcPass: RPC.pass,
  propagationUrl: 'http://localhost:8833/tx',
  assetUrl: 'http://localhost:8090/api/v1'
})
const ok = await mp.verify(found.ids[found.idx], tracker)
console.log('merklePath.verify:', ok ? 'PASS' : 'FAIL')
