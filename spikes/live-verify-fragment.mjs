// Full live verifyFragment on regtest data: bundle (leaf=R, size-1 batch) →
// anchor tx (real bytes via getrawtransaction) with real MerklePath →
// verifyFragment(bundle, TeranodeRPCTracker) → ok.
// Then a tampered-leaf negative on the same live data.
import { Transaction, MerklePath } from './lib/sdk.mjs'
import { RPC, rpc, loadState } from './lib/regtest.mjs'
import { createRequire } from 'node:module'
const require2 = createRequire(import.meta.url)
const { TeranodeRPCTracker, merklePathFor } = require2('../packages/chain/dist/src/regtest.js')
const { verifyFragment, buildProofBundle } = require2('../packages/verify/dist/src/bundle.js')
const { decodeMamemAnchor } = require2('../packages/contracts/dist/src/index.js')

const state = loadState()
const txid = state.liveContractTxids[0] // genesis anchor (size=1, n=0)
const cfg = { rpcUrl: RPC.url, rpcUser: RPC.user, rpcPass: RPC.pass, propagationUrl: 'http://localhost:8833/tx', assetUrl: 'http://localhost:8090/api/v1' }
const tracker = new TeranodeRPCTracker(cfg)

const hex = await rpc('getrawtransaction', txid)
const tx = Transaction.fromHex(hex)
const d = decodeMamemAnchor(tx.outputs[0].lockingScript.toHex())
if (!d) throw new Error('anchor did not decode')
console.log(`anchor n=${d.n} size=${d.size} shard=${d.shard.slice(0, 12)}…`)

const mp = await merklePathFor(cfg, txid, tracker)
if (!mp) throw new Error('anchor not found in recent blocks')
tx.merklePath = new MerklePath(mp.blockHeight, mp.path)
console.log(`BUMP: block ${mp.blockHeight}, path depth ${mp.path.length}`)

// bundle: leaf = R (the committed batch root; verifier checks reconstructRoot==R)
const Rhex = Buffer.from(d.R).toString('hex')
const bundle = {
  version: 1, shard: d.shard, n: d.n, size: d.size,
  leaf: Rhex, leafIndex: 0, path: [],
  anchor: { txid, outputIndex: 0, n: d.n, size: d.size, R: Rhex, Rp: Buffer.from(d.Rp).toString('hex') },
  chainTail: [{ txid, outputIndex: 0, n: d.n, size: d.size, R: Rhex, Rp: Buffer.from(d.Rp).toString('hex') }],
  anchorBEEF: tx.toAtomicBEEF()
}
const res = await verifyFragment(bundle, tracker)
console.log('verifyFragment:', JSON.stringify(res))
// tamper: flip a leaf byte → must fail at LeafPathMismatch
const bad = { ...bundle, leaf: '00' + Rhex.slice(2) }
const badRes = await verifyFragment(bad, tracker)
console.log('tampered leaf  :', JSON.stringify(badRes), '(expect LeafPathMismatch)')
