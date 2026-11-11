// live regtest demo: devchain coinbase -> mint tree -> submit want carrier -> read -> sweep -> gone.
// devchain: /rpc (getinfo, generatetoaddress, sendrawtransaction), /v1/tip, /v1/root/<h>,
//           /api/v1/block/<hash>/json, /api/v1/txmeta/<txid>/json, /api/v1/merkle_proof/<txid>, /arcade/tx (POST EF, GET /arcade/tx/<id>)
import { PrivateKey, P2PKH, Transaction, MerklePath } from '@bsv/sdk'

import { signWant, mintTree, buildCarrier, buildSweep, encodePayload, lanesFor, submitAll, readWants, FUNDING_TAG } from '../dist/src/index.js'
import { tscMerklePath } from '@lightwebinc/bcommon'

import { stripEF } from './devchain.mjs'

const CHAIN = process.env.CHAIN_URL ?? 'http://127.0.0.1:18344'
const HOSTS = [process.env.HOST_URL ?? 'http://127.0.0.1:18091']
const NET = 'regtest'
const rpc = (m, p = []) => fetch(`${CHAIN}/rpc`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ method: m, params: p }) }).then(r => r.json())
const tracker = {
  isValidRootForHeight: async (root, h) => (await (await fetch(`${CHAIN}/v1/root/${h}`)).json()).merkleRoot === root,
  currentHeight: async () => (await (await fetch(`${CHAIN}/v1/tip`)).json()).height
}
const log = []
const ok = (k, v) => log.push(`${v ? 'PASS' : 'FAIL'} ${k}`)
const info = s => log.push(`INFO ${s}`)

const key = PrivateKey.fromRandom()
const addr = key.toPublicKey().toAddress([0x6f])
const gen = await rpc('generatetoaddress', [1, addr])
const bh = Array.isArray(gen.result) ? gen.result[0] : gen.result
const bj = await (await fetch(`${CHAIN}/api/v1/block/${bh}/json`)).json()
const cbTxid = bj.coinbase_tx.txid
info(`coinbase ${cbTxid} in block ${bh} -> ${addr}`)
// coinbase must mature on this chain: generate 101 more blocks first
await rpc('generatetoaddress', [101, addr])
const ef = Buffer.from(await (await fetch(`${CHAIN}/api/v1/tx/${cbTxid}`)).arrayBuffer())
const parent = Transaction.fromHex(Array.from(stripEF(ef)))
info(`parent ${parent.id('hex')} == ${cbTxid}: ${parent.id('hex') === cbTxid}`)
const addrScript = new P2PKH().lock(addr).toHex()
const vout = parent.outputs.findIndex(o => o.lockingScript.toHex() === addrScript)
info(`coinbase vout ${vout} sats ${parent.outputs[vout].satoshis}`)
const ptxid = parent.id('hex')
// devchain: every proof is one level — sibling 0x33*32, tx at offset 1 (testchain/chain.go proof());
// the mined height is txmeta's, not the broadcast answer's blockHeight (block is mined later)
const mkPath = (txid, h) => new MerklePath(h, [[{ offset: 0, hash: '33'.repeat(32) }, { offset: 1, hash: txid, txid: true }]])
const minedHeight = async (txid) => (await (await fetch(`${CHAIN}/api/v1/txmeta/${txid}/json`)).json()).blockHeights[0]
parent.merklePath = mkPath(ptxid, await minedHeight(ptxid))

const tree = await mintTree({ key, funding: { tx: parent, vout, satoshis: parent.outputs[vout].satoshis }, n: 8, satsPerOutput: 500 })
const br = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tree.toEF()) })
info(`broadcast tree ${tree.id('hex')} -> ${br.status} ${(await br.text()).slice(0, 160)}`)
await rpc('generatetoaddress', [1, addr])
const tj = await (await fetch(`${CHAIN}/api/v1/txmeta/${tree.id('hex')}/json`)).json()
info(`tree mined: ${JSON.stringify(tj).slice(0, 160)}`)
tree.merklePath = mkPath(tree.id('hex'), await minedHeight(tree.id('hex')))

const sw = signWant(key, { domains: ['compute'], question: 'live', net: NET, ts: Date.now(), exp: Date.now() + 60000 })
const { carrier, beef } = await buildCarrier({ key, payload: encodePayload({ v: 1, kind: 'want', body: sw }), slot: { tree, vout: 0 } })
const lanes = lanesFor(NET, ['compute'])
info(`carrier ${carrier.id('hex')} -> lanes ${lanes}`)
const sub = await submitAll(HOSTS, beef, lanes)
info(`submit -> ${JSON.stringify(sub)}`)
ok('submit accepted', sub.ok)
const r = await readWants(HOSTS, { net: NET, domain: 'compute' }, { tracker, net: NET })
ok(`readWants returns the want (items=${r.items.length})`, r.items.length === 1)

const swp = await buildSweep({ key, tree, vouts: [0], fees: { sats: 1, bytes: 1, floor: 1 } })
const br2 = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(swp.toEF()) })
info(`broadcast sweep ${swp.id('hex')} -> ${br2.status} ${(await br2.text()).slice(0, 120)}`)
await rpc('generatetoaddress', [1, addr])
// a retraction must be a MINED sweep: embed the sweep's own proof at its mined height
swp.merklePath = mkPath(swp.id('hex'), await minedHeight(swp.id('hex')))
const sub2 = await submitAll(HOSTS, swp.toAtomicBEEF(), lanes)
info(`submit sweep -> ${JSON.stringify(sub2)}`)
const r2 = await readWants(HOSTS, { net: NET, domain: 'compute' }, { tracker, net: NET })
ok(`after sweep the want is gone (items=${r2.items.length})`, r2.items.length === 0)

console.log(log.join('\n'))
