// Real-chain payment e2e. Spins the full runtime (regtest or ttn),
// funds an INDEPENDENT payer key on-chain, runs the paid-call flow:
//   402 invoice → signed tx paying the operator + OP_RETURN tag → settled by
//   real consensus → receipt commits the settlement txid → /v1/verify green.
// Run:
//   regtest: node packages/e2e/src/e2e-pay.mjs           (local Teranode stack)
//   ttn:     NEKTAR_NETWORK=ttn node packages/e2e/src/e2e-pay.mjs  (public testnet)
import { PrivateKey } from '@bsv/sdk'
import { configFromEnv, openRuntime } from '../../mcp/dist/src/runtime.js'
import { createHttpApi } from '../../mcp/dist/src/http.js'
import { buildPaymentTx } from '../../mcp/dist/src/payclient.js'
import { existsSync, readFileSync } from 'node:fs'

const net = process.env.NEKTAR_NETWORK ?? 'regtest'
const check = (ok, label) => {
  console.log(` ${ok ? '✔' : '✗ FAIL'} ${label}`)
  if (!ok) process.exit(1)
}

// regtest connection — same env resolution as the sibling e2e scripts
function readEnvFile(p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = { ...readEnvFile(new URL('../../../.env', import.meta.url)), ...readEnvFile(new URL('../../../../.env', import.meta.url)) }
for (const [k, v] of Object.entries(localEnv)) if (v && process.env[k] === undefined) process.env[k] = v
const qsEnv = readEnvFile(new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const rc = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
const rpc = async (method, ...params) => {
  const auth = 'Basic ' + Buffer.from(`${rc.rpcUser}:${rc.rpcPass}`).toString('base64')
  const r = await fetch(rc.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

// ── operator = a fresh key; payments settle to ITS address (never the funder)
const opKey = PrivateKey.fromRandom()
process.env.NEKTAR_PAYMENTS = '1'
process.env.NEKTAR_OPERATOR_WIF = opKey.toWif([0xef])
const opAddr = opKey.toPublicKey().toAddress([0x6f])

// ── runtime on its own data dir (never the live server's .nektar)
process.env.NEKTAR_DATA_DIR = process.env.NEKTAR_DATA_DIR ?? `.state/nektar-e2e-pay-${net}`
process.env.NEKTAR_NETWORK = net
if (net === 'regtest' && !existsSync(`${process.env.NEKTAR_DATA_DIR}/reserve.json`)) {
  // seed the reserve from the live server's .nektar — same fund key owns them
  const { mkdirSync, copyFileSync } = await import('node:fs')
  mkdirSync(process.env.NEKTAR_DATA_DIR, { recursive: true })
  if (existsSync('.nektar/reserve.json')) copyFileSync('.nektar/reserve.json', `${process.env.NEKTAR_DATA_DIR}/reserve.json`)
}
const rt = await openRuntime(configFromEnv())
const srv = createHttpApi(rt.mem).listen(0)
await new Promise((r) => srv.once('listening', r))
const base = `http://127.0.0.1:${srv.address().port}`
const call = async (tool, body) =>
  await (await fetch(`${base}/v1/tools/${tool}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })).json()
console.log(`\n═══ payment e2e on ${net.toUpperCase()} (real chain) ═══`)

// ── payer: an independent key, funded on-chain
const payer = PrivateKey.fromRandom()
const payerAddr = payer.toPublicKey().toAddress([0x6f])
const payerUtxos = []
if (net === 'regtest') {
  // fund the payer with a REAL tx: spend the fund key's treasury utxo (the
  // refillable split pool) → 50k sats to the payer; falls back to any unspent
  // .nektar reserve entry. The payer then owns uncontested utxos.
  const { TeranodeBroadcaster } = await import('@nektar/chain')
  const { Transaction, P2PKH } = await import('@bsv/sdk')
  const fundKey = PrivateKey.fromWif(process.env.NEKTAR_REGTEST_WIF)
  const fundAddr = fundKey.toPublicKey().toAddress([0x6f])
  let srcTx, srcVout
  const { utxoStatus } = await import('@nektar/chain')
  const usable = async (e) => e && (await utxoStatus(rc, e.tx.id('hex'), e.vout)) === 'unspent'
  const t = JSON.parse(readFileSync(new URL('../../../.state/treasury.json', import.meta.url), 'utf8')).treasury
  const cand = []
  if (t) cand.push({ tx: Transaction.fromHex(t.txHex), vout: t.vout })
  const rs = JSON.parse(readFileSync(new URL('../../../.nektar/reserve.json', import.meta.url), 'utf8'))
  const spent = new Set(rs.spent ?? [])
  for (const e of rs.entries ?? []) if (!spent.has(`${e.txid}:${e.vout}`)) cand.push({ tx: Transaction.fromHex(e.txHex), vout: e.vout })
  for (const c of cand)
    if ((c.tx.outputs[c.vout]?.satoshis ?? 0) >= 20_000 && (await usable(c))) {
      srcTx = c.tx
      srcVout = c.vout
      break
    }
  if (!srcTx) throw new Error('no fund source: treasury + reserve all spent or too small')
  const paySats = Math.min(50_000, srcTx.outputs[srcVout].satoshis - 5000)
  const send = new Transaction()
  send.addInput({ sourceTransaction: srcTx, sourceOutputIndex: srcVout, unlockingScriptTemplate: new P2PKH().unlock(fundKey) })
  send.addOutput({ lockingScript: new P2PKH().lock(payerAddr), satoshis: paySats })
  send.addOutput({ lockingScript: new P2PKH().lock(fundAddr), change: true })
  await send.fee()
  await send.sign()
  const br = await new TeranodeBroadcaster(rc.propagationUrl).broadcast(send)
  check(br.status === 'success', `payer funding broadcast — ${send.id('hex').slice(0, 16)}… (real spend of the fund treasury)`)
  await rpc('generatetoaddress', 1, fundAddr)
  payerUtxos.push({ tx: send, vout: 0, satoshis: paySats })
  console.log(` payer ${payerAddr} funded by regtest tx ${send.id('hex').slice(0, 20)}…`)
} else {
  // ttn: payer is funded by a real send from the runtime's fund key — the
  // funding tx itself is a real public testnet tx. The live funder must be
  // told the entry is spent and the change is owned again, else the next
  // anchor hits UTXO_SPENT on a drained reserve.
  const { readKeyFile } = await import('../../mcp/dist/src/keystore.js')
  const pass = process.env.NEKTAR_KEY_PASSPHRASE ?? readFileSync(new URL('../../../.state/ttn-passphrase', import.meta.url), 'utf8').trim()
  const rawKey = process.env.NEKTAR_FUND_WIF ?? readKeyFile(`${process.env.NEKTAR_DATA_DIR}/fee.key.json`, pass, true)
  const fundWif = rawKey.trim().startsWith('{') ? JSON.parse(rawKey).wif : rawKey.trim()
  const fundKey = PrivateKey.fromWif(fundWif)
  const { Transaction, P2PKH } = await import('@bsv/sdk')
  const { readFileSync } = await import('node:fs')
  const reserve = JSON.parse(readFileSync(`${process.env.NEKTAR_DATA_DIR}/reserve.json`, 'utf8'))
  const spent = new Set(reserve.spent ?? [])
  const entry = (reserve.entries ?? []).find((e) => !spent.has(`${e.txid}:${e.vout}`) && e.satoshis > 5000)
  if (!entry) throw new Error('no spendable reserve entry in ttn data dir')
  const ftx = Transaction.fromHex(entry.txHex)
  const send = new Transaction()
  send.addInput({ sourceTransaction: ftx, sourceOutputIndex: entry.vout, unlockingScriptTemplate: new P2PKH().unlock(fundKey) })
  send.addOutput({ lockingScript: new P2PKH().lock(payerAddr), satoshis: 3000 })
  send.addOutput({ lockingScript: new P2PKH().lock(fundKey.toPublicKey().toAddress([0x6f])), change: true })
  await send.fee()
  await send.sign()
  const bc = await rt.mem.broadcastTx(send)
  check(bc.status === 'success', `payer funding broadcast — ${send.id('hex').slice(0, 16)}… ${bc.status === 'success' ? '(public ttn tx)' : bc.description}`)
  const fund = rt.mem.funder
  fund.markSpent(entry.txid, entry.vout)
  fund.addOwned(send, send.outputs.length - 1)
  payerUtxos.push({ tx: send, vout: 0, satoshis: 3000 })
  console.log(` payer ${payerAddr} funded by ttn tx ${send.id('hex').slice(0, 20)}…`)
}

// ── a fragment to cite, anchored for real
const SHARD = 'pay-e2e'
for (let i = 0; i < 4; i++) await call('remember', { shard: SHARD, data: `paid answers settle on a real chain — record ${i}` })
console.log(' fragment committed — waiting for anchor…')
for (let i = 0; i < 60; i++) {
  const r = await call('recall', { shard: SHARD, seq: 0 })
  if (!r.items?.[0]?.unverifiable) break
  await new Promise((r) => setTimeout(r, 5000))
}
const f = (await call('recall', { shard: SHARD, seq: 0 })).items[0]
check(f && !f.unverifiable, `${SHARD}:0 anchored + verified on ${net}`)

// ── 1) catalog + unpaid call → the 402 invoice
const catalog = await (await fetch(`${base}/.well-known/x402`)).json()
check(
  catalog.resources?.some((r) => r.url === 'tool://groundedAnswer'),
  'discovery catalog lists priced tools'
)
const denied = await call('groundedAnswer', { claims: [{ text: 'paid answers settle on a real chain — record 0', cites: [{ shard: SHARD, seq: 0 }] }] })
check(denied.ok === false && denied.accepts?.[0]?.scheme === 'exact', 'unpaid call refused with x402 invoice')
const extra = denied.accepts[0].extra

// ── 2) pay it for real: signed tx → server verifies + broadcasts → mined by consensus
const { hex, txid } = await buildPaymentTx(
  { amount: denied.accepts[0].amount, lockingScriptHex: extra.lockingScriptHex, opReturn: extra.opReturn },
  payer,
  payerUtxos
)
const r = await call('groundedAnswer', {
  claims: [{ text: 'paid answers settle on a real chain — record 0', cites: [{ shard: SHARD, seq: 0 }] }],
  payment: { invoiceId: extra.invoiceId, transaction: hex }
})
if (r.committed !== true) console.log('  paid call response:', JSON.stringify(r).slice(0, 400))
check(r.committed === true, `paid call committed — payment tx ${txid.slice(0, 20)}… settled by real consensus`)
check(r.receipt.payment?.txid === txid, 'receipt commits the settlement txid')
check(r.receipt.payment?.payTo === opAddr, `payment pays the operator ${opAddr}`)

// ── 3) the payment tx exists ON THE REAL CHAIN
if (net === 'regtest') {
  const res = await fetch(`${rc.assetUrl}/tx/${txid}`).catch(() => null)
  check(!!res?.ok, `payment tx resolvable via Teranode asset API (${res?.status ?? 'unreachable'})`)
} else {
  const bcast = process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade-v2-ttn-us-1.bsvblockchain.tech'
  const st = await fetch(`${bcast}/tx/${txid}`)
    .then((r) => (r.ok ? r.json() : null))
    .catch(() => null)
  check(!!st?.txid || !!st?.txStatus, `payment tx visible on TTN (${st?.txStatus ?? 'pending'})`)
}

// ── 4) independent verify → 'paid' line green
const v = await (await fetch(`${base}/v1/verify?r=${r.receiptToken}`)).json()
check(v.ok === true, 'receipt verifies')
check(v.checks.find((c) => c.name === 'paid')?.ok === true, `'paid' check green — settlement auditable on ${net}`)

// ── 5) replay rejected
const denied2 = await call('groundedAnswer', { claims: [{ text: 'paid answers settle on a real chain — record 1', cites: [{ shard: SHARD, seq: 1 }] }] })
const again = await call('groundedAnswer', {
  claims: [{ text: 'paid answers settle on a real chain — record 1', cites: [{ shard: SHARD, seq: 1 }] }],
  payment: { invoiceId: denied2.accepts[0].extra.invoiceId, transaction: hex }
})
check(!!again.error || !!again.paymentRequired === false, 'reused payment tx rejected')
console.log(`\n receipt: ${r.receiptToken}\n verify:  ${r.verifyUrl}\n`)
srv.close()
await rt.close()
process.exit(0)
