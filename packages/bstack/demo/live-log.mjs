// live regtest demo: devchain coinbase -> blogs funding tree -> 3 batches via
// the real tm_log -> ls_log read -> mined anchor token -> anchoredThrough.
import { PrivateKey, P2PKH, Transaction, MerklePath } from '@bsv/sdk'
import { mintTree, MemoryTreeLedger, MemoryCarrierJournal } from '../dist/src/index.js'
import { MemoryLogState, appendLog, readLog, anchorLog, publishMinedLog, logFundingLock, LOG_KIND } from '../dist/src/log.js'
import { stripEF } from './devchain.mjs'

const CHAIN = process.env.CHAIN_URL ?? 'http://127.0.0.1:18344'
const HOSTS = [process.env.HOST_URL ?? 'http://127.0.0.1:18091']
const STREAM = process.env.NEKTAR_LOG_STREAM ?? 'nektar_abcdefghij'
const rpc = (m, p = []) => fetch(`${CHAIN}/rpc`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify({ method: m, params: p }) }).then(r => r.json())
const tracker = {
  isValidRootForHeight: async (root, h) => (await (await fetch(`${CHAIN}/v1/root/${h}`)).json()).merkleRoot === root,
  currentHeight: async () => (await (await fetch(`${CHAIN}/v1/tip`)).json()).height
}
const log = []
const ok = (k, v) => log.push(`${v ? 'PASS' : 'FAIL'} ${k}`)
const info = s => log.push(`INFO ${s}`)

const producer = PrivateKey.fromRandom()
const addr = producer.toPublicKey().toAddress([0x6f])
const gen = await rpc('generatetoaddress', [1, addr])
const bh = Array.isArray(gen.result) ? gen.result[0] : gen.result
const bj = await (await fetch(`${CHAIN}/api/v1/block/${bh}/json`)).json()
const cbTxid = bj.coinbase_tx.txid
info(`coinbase ${cbTxid} -> ${addr}`)
await rpc('generatetoaddress', [101, addr])
const ef = Buffer.from(await (await fetch(`${CHAIN}/api/v1/tx/${cbTxid}`)).arrayBuffer())
const parent = Transaction.fromHex(Array.from(stripEF(ef)))
const addrScript = new P2PKH().lock(addr).toHex()
const vout = parent.outputs.findIndex(o => o.lockingScript.toHex() === addrScript)
const mkPath = (txid, h) => new MerklePath(h, [[{ offset: 0, hash: '33'.repeat(32) }, { offset: 1, hash: txid, txid: true }]])
const minedHeight = async (txid) => (await (await fetch(`${CHAIN}/api/v1/txmeta/${txid}/json`)).json()).blockHeights[0]
const fetchTx = async (txid) => {
  const res = await fetch(`${CHAIN}/api/v1/tx/${txid}`).catch(() => undefined)
  if (res === undefined || !res.ok) return undefined
  const ef2 = Buffer.from(await res.arrayBuffer())
  if (ef2.length === 0) return undefined
  let tx
  try { tx = Transaction.fromHex(Array.from(stripEF(ef2))) } catch { return undefined }
  const h = await minedHeight(txid).catch(() => undefined)
  if (h === undefined) return tx
  tx.merklePath = mkPath(tx.id('hex'), h)
  return tx
}
parent.merklePath = mkPath(parent.id('hex'), await minedHeight(parent.id('hex')))

// blogs funding tree: derive(batch)-locked funding outputs, kind 'log'
const tree = await mintTree({ key: producer, funding: { tx: parent, vout, satoshis: parent.outputs[vout].satoshis }, n: 8, satsPerOutput: 100, lock: await logFundingLock(producer) })
const br = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tree.toEF()) })
info(`broadcast log tree ${tree.id('hex')} -> ${br.status}`)
await rpc('generatetoaddress', [1, addr])
tree.merklePath = mkPath(tree.id('hex'), await minedHeight(tree.id('hex')))
const ledger = new MemoryTreeLedger(); await ledger.add(tree, 8, LOG_KIND); await ledger.markProven(tree.id('hex'))
const journal = new MemoryCarrierJournal()
const state = new MemoryLogState()
ok('funding tree BEEF published to tm_log', await publishMinedLog({ journal, hosts: HOSTS, stream: STREAM, tx: tree, quorum: 'all' }))

const seqs = []
for (const n of [1, 2, 3]) {
  const r = await appendLog({ key: producer, ledger, journal, state, hosts: HOSTS, stream: STREAM, lines: [`{"v":1,"ev":"live-${n}"}`], now: () => Date.now(), quorum: 'all' })
  seqs.push(r.seq)
}
info(`appended seqs ${seqs.join(',')} (took ${HOSTS.length} host(s))`)
ok('three batches appended seq 1,2,3', seqs.join(',') === '1,2,3')

const read = await readLog({ hosts: HOSTS, stream: STREAM, producer: producer.toPublicKey().toString(), tracker })
info(`readLog batches=${read.batches.length} head=${read.head} gaps=${JSON.stringify(read.gaps)} withheld=${JSON.stringify(read.withheld)} unreachable=${JSON.stringify(read.unreachable)}`)
ok('ls_log returns all 3 batches in order', read.batches.length === 3 && read.batches.every((b, i) => b.seq === i + 1))
ok('no gaps, nothing withheld', read.gaps.length === 0 && read.withheld.length === 0)
ok('lines round-trip', read.batches[0]?.lines[0] === '{"v":1,"ev":"live-1"}' && read.batches[2]?.lines[0] === '{"v":1,"ev":"live-3"}')

// anchor: broadcast the mined token, publish its BEEF, anchoredThrough 3
const broadcast = async (tx) => {
  const r = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tx.toEF()) })
  if (r.status !== 200) return { status: 'error', description: `http ${r.status}` }
  await rpc('generatetoaddress', [1, addr])
  return { status: 'success' }
}
const a1 = await anchorLog({ key: producer, ledger, journal, state, hosts: HOSTS, stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
info(`anchor interval=${a1?.interval} first=${a1?.first} count=${a1?.count} txid=${a1?.txid}`)
ok('anchor token broadcast', a1 !== undefined)
const a2 = await anchorLog({ key: producer, ledger, journal, state, hosts: HOSTS, stream: STREAM, broadcast, fetchTx, minBatches: 1, maxAgeMs: 0 })
ok('mined anchor published to tm_log', a2 !== undefined)
const read2 = await readLog({ hosts: HOSTS, stream: STREAM, producer: producer.toPublicKey().toString(), tracker })
info(`anchoredThrough=${read2.anchoredThrough} (of head ${read2.head})`)
ok('anchoredThrough === 3', read2.anchoredThrough === 3)

console.log(log.join('\n'))
process.exit(log.some(l => l.startsWith('FAIL')) ? 1 : 0)
