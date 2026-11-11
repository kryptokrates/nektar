// live regtest demo: devchain coinbase -> bfinger funding tree -> publish a
// create record via the real tm_finger -> mine the state token -> resolve it
// through the real ls_finger -> publish an update -> resolve the newer seq.
import { PrivateKey, P2PKH, Transaction, MerklePath } from '@bsv/sdk'
import { mintTree, MemoryTreeLedger, MemoryCarrierJournal, submitAll } from '../dist/src/index.js'
import { MemoryFingerState, publishFinger, resolveFinger, fingerFundingLock, FINGER_KIND } from '../dist/src/finger.js'
import { stripEF } from './devchain.mjs'

const CHAIN = process.env.CHAIN_URL ?? 'http://127.0.0.1:18344'
const HOSTS = [process.env.HOST_URL ?? 'http://127.0.0.1:18091']
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
info(`coinbase ${cbTxid} -> ${addr}`)
await rpc('generatetoaddress', [101, addr])
const ef = Buffer.from(await (await fetch(`${CHAIN}/api/v1/tx/${cbTxid}`)).arrayBuffer())
const coinbase = Transaction.fromHex(Array.from(stripEF(ef)))
const addrScript = new P2PKH().lock(addr).toHex()
const cbVout = coinbase.outputs.findIndex(o => o.lockingScript.toHex() === addrScript)
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
coinbase.merklePath = mkPath(coinbase.id('hex'), await minedHeight(coinbase.id('hex')))

// one funding tx: vout 0 feeds the finger tree, vouts 1+2 pay token fees
const fundTx = new Transaction()
fundTx.addInput({ sourceTransaction: coinbase, sourceOutputIndex: cbVout, unlockingScriptTemplate: new P2PKH().unlock(key) })
fundTx.addOutput({ satoshis: 200_000, lockingScript: new P2PKH().lock(addr) })
fundTx.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(addr) })
fundTx.addOutput({ satoshis: 100_000, lockingScript: new P2PKH().lock(addr) })
fundTx.addOutput({ lockingScript: new P2PKH().lock(addr), change: true })
await fundTx.fee(); await fundTx.sign()
await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(fundTx.toEF()) })
await rpc('generatetoaddress', [1, addr])
fundTx.merklePath = mkPath(fundTx.id('hex'), await minedHeight(fundTx.id('hex')))

// bfinger funding tree: record-derivation-locked funding leaves, kind 'finger'
const tree = await mintTree({ key, funding: { tx: fundTx, vout: 0, satoshis: 200_000 }, n: 8, satsPerOutput: 100, lock: await fingerFundingLock(key) })
const br = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tree.toEF()) })
info(`broadcast finger tree ${tree.id('hex')} -> ${br.status}`)
await rpc('generatetoaddress', [1, addr])
tree.merklePath = mkPath(tree.id('hex'), await minedHeight(tree.id('hex')))
const ledger = new MemoryTreeLedger(); await ledger.add(tree, 8, FINGER_KIND); await ledger.markProven(tree.id('hex'))
const journal = new MemoryCarrierJournal()
const state = new MemoryFingerState()

// the proven tree must reach tm_finger: its funding leaves are admitted as
// 'funding' outputs so storage can vouch for the carrier's parent
const tr = await submitAll(HOSTS, tree.toAtomicBEEF(), ['tm_finger'])
info(`tree -> tm_finger took=${tr.took.length} missed=${JSON.stringify(tr.missed)}`)
ok('funding tree BEEF published to tm_finger', tr.took.length === HOSTS.length)

const queue = [{ tx: fundTx, vout: 1 }, { tx: fundTx, vout: 2 }]
const takeFunding = async () => queue.shift()
const broadcast = async (tx) => {
  const r = await fetch(`${CHAIN}/arcade/tx`, { method: 'POST', body: Buffer.from(tx.toEF()) })
  if (r.status !== 200) return { status: 'error', description: `http ${r.status}` }
  await rpc('generatetoaddress', [1, addr])
  return { status: 'success' }
}
const publish = async (body) =>
  await publishFinger({ key, state, ledger, journal, hosts: HOSTS, body, takeFunding, broadcast, fetchTx, allowHttp: true })

const body1 = { v: 1, hosts: HOSTS, office: 'nektar_abcdefghij', stream: 'nektar_abcdefghij', nets: ['regtest'] }
const r1 = await publish(body1)
info(`create seq=${r1.seq} carrier=${r1.carrierTxid} token=${r1.tokenTxid}`)
ok('create published seq 1', r1.changed === true && r1.seq === 1)
// the second call (changed:false) propagates the now-proven token BEEF
const r1b = await publish(body1)
ok('same body is a no-op', r1b.changed === false)

const pub = key.toPublicKey().toString()
const got1 = await resolveFinger({ hosts: HOSTS, pub, tracker, allowHttp: true })
info(`resolve seq=${got1?.seq} answered=${JSON.stringify(got1?.hostsAnswered)} unreachable=${JSON.stringify(got1?.unreachable)}`)
ok('ls_finger resolves the record', got1 !== undefined)
ok('body round-trips', got1 !== undefined && JSON.stringify(got1.body) === JSON.stringify(body1))

const body2 = { ...body1, hosts: [...HOSTS, 'https://finger2.example.com'] }
const r2 = await publish(body2)
info(`update seq=${r2.seq} carrier=${r2.carrierTxid} token=${r2.tokenTxid}`)
ok('update published seq 2', r2.changed === true && r2.seq === 2)
await publish(body2) // propagate the proven update token
const got2 = await resolveFinger({ hosts: HOSTS, pub, tracker, allowHttp: true })
info(`resolve seq=${got2?.seq} token=${got2?.tokenTxid}`)
ok('the newer seq wins', got2 !== undefined && got2.seq === 2 && JSON.stringify(got2.body) === JSON.stringify(body2))

console.log(log.join('\n'))
process.exit(log.some(l => l.startsWith('FAIL')) ? 1 : 0)
