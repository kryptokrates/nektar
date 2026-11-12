// live regtest demo: devchain coinbase -> bbox funding tree -> envelope A->B via real tm_bbox/ls_bbox -> B opens it, third key gets nothing.
import { PrivateKey, P2PKH, Transaction, MerklePath } from '@bsv/sdk'
import { mintTree, sendBox, readBox, ackBox, boxFundingLock, MemoryTreeLedger, MemoryCarrierJournal, DEFAULT_OFFICE } from '../dist/src/index.js'
import { stripEF } from './devchain.mjs'

const CHAIN = process.env.CHAIN_URL ?? 'http://127.0.0.1:18344'
const HOSTS = [process.env.HOST_URL ?? 'http://127.0.0.1:18091']
const OFFICE = process.env.BBOX_OFFICE ?? DEFAULT_OFFICE
const rpc = (m, p = []) =>
  fetch(`${CHAIN}/rpc`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ method: m, params: p }) }).then((r) =>
    r.json()
  )
const tracker = {
  isValidRootForHeight: async (root, h) => (await (await fetch(`${CHAIN}/v1/root/${h}`)).json()).merkleRoot === root,
  currentHeight: async () => (await (await fetch(`${CHAIN}/v1/tip`)).json()).height
}
const log = []
const ok = (k, v) => log.push(`${v ? 'PASS' : 'FAIL'} ${k}`)
const info = (s) => log.push(`INFO ${s}`)

const sender = PrivateKey.fromRandom()
const recip = PrivateKey.fromRandom()
const third = PrivateKey.fromRandom()
const addr = sender.toPublicKey().toAddress([0x6f])
const recipAddr = recip.toPublicKey().toAddress([0x6f])
const gen = await rpc('generatetoaddress', [1, addr])
const bh = Array.isArray(gen.result) ? gen.result[0] : gen.result
const bj = await (await fetch(`${CHAIN}/api/v1/block/${bh}/json`)).json()
const cbTxid = bj.coinbase_tx.txid
const gen2 = await rpc('generatetoaddress', [1, recipAddr])
const bh2 = Array.isArray(gen2.result) ? gen2.result[0] : gen2.result
const bj2 = await (await fetch(`${CHAIN}/api/v1/block/${bh2}/json`)).json()
const cbTxid2 = bj2.coinbase_tx.txid
info(`coinbase ${cbTxid} -> ${addr}`)
await rpc('generatetoaddress', [101, addr])
const ef = Buffer.from(await (await fetch(`${CHAIN}/api/v1/tx/${cbTxid}`)).arrayBuffer())
const parent = Transaction.fromHex(Array.from(stripEF(ef)))
const addrScript = new P2PKH().lock(addr).toHex()
const vout = parent.outputs.findIndex((o) => o.lockingScript.toHex() === addrScript)
const mkPath = (txid, h) =>
  new MerklePath(h, [
    [
      { offset: 0, hash: '33'.repeat(32) },
      { offset: 1, hash: txid, txid: true }
    ]
  ])
const minedHeight = async (txid) => (await (await fetch(`${CHAIN}/api/v1/txmeta/${txid}/json`)).json()).blockHeights[0]
parent.merklePath = mkPath(parent.id('hex'), await minedHeight(parent.id('hex')))

// bbox funding tree: envelopeFundingLock outputs, kind 'bbox'
const tree = await mintTree({
  key: sender,
  funding: { tx: parent, vout, satoshis: parent.outputs[vout].satoshis },
  n: 8,
  satsPerOutput: 500,
  lock: await boxFundingLock(sender)
})
const br = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tree.toEF()) })
info(`broadcast bbox tree ${tree.id('hex')} -> ${br.status}`)
await rpc('generatetoaddress', [1, addr])
tree.merklePath = mkPath(tree.id('hex'), await minedHeight(tree.id('hex')))
const ledger = new MemoryTreeLedger()
await ledger.add(tree, 8, 'bbox')
await ledger.markProven(tree.id('hex'))

const body = { market: { type: 'quote', wantId: 'w-live', priceSats: 250, ts: Date.now() } }
const sent = await sendBox({ key: sender, ledger, journal: new MemoryCarrierJournal(), hosts: HOSTS, office: OFFICE, to: recip.toPublicKey().toString(), body })
info(`envelope ${sent.txid} took=${sent.took.length} admitted=${sent.admitted.length} missed=${JSON.stringify(sent.missed)}`)
ok('envelope admitted by real tm_bbox', sent.admitted.length >= 1)

const read = await readBox({ key: recip, hosts: HOSTS, office: OFFICE, tracker })
info(`readBox items=${read.items.length} withheld=${JSON.stringify(read.withheld)} unreachable=${JSON.stringify(read.unreachable)}`)
ok('recipient reads + decrypts via real ls_bbox', read.items.length === 1)
if (read.items.length === 1) {
  ok('from is sender pub (record-bound)', read.items[0].from === sender.toPublicKey().toString())
  const rm = read.items[0].body.market ?? {}
  ok('body round-trips', rm.type === body.market.type && rm.wantId === body.market.wantId && rm.priceSats === body.market.priceSats)
}
const r3 = await readBox({ key: third, hosts: HOSTS, office: OFFICE, tracker })
ok('third key gets nothing', r3.items.length === 0)

// recipient acknowledges: own bbox funding tree, receipt acking the envelope commitment
const ef2 = Buffer.from(await (await fetch(`${CHAIN}/api/v1/tx/${cbTxid2}`)).arrayBuffer())
const parent2 = Transaction.fromHex(Array.from(stripEF(ef2)))
const recipScript = new P2PKH().lock(recipAddr).toHex()
const vout2 = parent2.outputs.findIndex((o) => o.lockingScript.toHex() === recipScript)
parent2.merklePath = mkPath(parent2.id('hex'), await minedHeight(parent2.id('hex')))
const tree2 = await mintTree({
  key: recip,
  funding: { tx: parent2, vout: vout2, satoshis: parent2.outputs[vout2].satoshis },
  n: 8,
  satsPerOutput: 500,
  lock: await boxFundingLock(recip)
})
const br2 = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tree2.toEF()) })
info(`broadcast recipient bbox tree ${tree2.id('hex')} -> ${br2.status}`)
await rpc('generatetoaddress', [1, addr])
tree2.merklePath = mkPath(tree2.id('hex'), await minedHeight(tree2.id('hex')))
const ledger2 = new MemoryTreeLedger()
await ledger2.add(tree2, 8, 'bbox')
await ledger2.markProven(tree2.id('hex'))
const ack = await ackBox({
  key: recip,
  ledger: ledger2,
  journal: new MemoryCarrierJournal(),
  hosts: HOSTS,
  office: OFFICE,
  items: read.items.map((i) => ({ txid: i.txid, commitment: i.commitment }))
})
info(`ackBox receipts=${ack.receipts.length} acked=${ack.acked} resent=${ack.resent} skipped=${ack.skipped}`)
ok('receipt admitted by real tm_bbox', ack.acked === 1)
const read2 = await readBox({ key: recip, hosts: HOSTS, office: OFFICE, tracker })
ok('after the receipt the inbox is empty (real ls_bbox)', read2.items.length === 0)

console.log(log.join('\n'))
