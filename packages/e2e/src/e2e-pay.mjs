// Real-chain payment e2e. Spins the full runtime on regtest, funds an
// INDEPENDENT payer key on-chain, runs the paid-call flow:
//   402 invoice → signed tx paying the operator + OP_RETURN tag → settled by
//   real consensus → receipt commits the settlement txid → /v1/verify green.
// Run:
//   regtest: node packages/e2e/src/e2e-pay.mjs           (local Teranode stack)
import { PrivateKey } from '@bsv/sdk'
import { configFromEnv, openRuntime } from '../../mcp/dist/src/runtime.js'
import { createHttpApi } from '../../mcp/dist/src/http.js'
import { buildPaymentTx } from '../../mcp/dist/src/payclient.js'
import { existsSync, readFileSync, writeFileSync } from 'node:fs'

const net = process.env.NEKTAR_NETWORK ?? 'regtest'
const check = (ok, label) => {
  console.log(` ${ok ? '✔' : '✗ FAIL'} ${label}`)
  if (!ok) process.exit(1)
}
const _t0 = Date.now()
let _lap = _t0
const lap = (n) => {
  const now = Date.now()
  console.log(`  [${n}: ${now - _lap}ms]`)
  _lap = now
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

// ── regtest funding runs BEFORE the runtime boots. The toolbox wallet does
// not read reserve.json (that file is the pre-toolbox format): regtestSeed →
// bootstrapReserve reads dataDir/treasury.json and splits it into wallet
// utxos. So: keep .state/treasury.json live, spend it once for the payer and
// write the change back as the next treasury, then hand that fresh file to
// the runtime whose seed funds the wallet.
const payer = PrivateKey.fromRandom()
const payerAddr = payer.toPublicKey().toAddress([0x6f])
const payerUtxos = []
if (net === 'regtest') {
  const { TeranodeBroadcaster, utxoStatus, bootstrapReserve } = await import('@nektar/chain')
  const { Transaction, P2PKH } = await import('@bsv/sdk')
  const fundKey = PrivateKey.fromWif(process.env.NEKTAR_REGTEST_WIF)
  const fundAddr = fundKey.toPublicKey().toAddress([0x6f])
  const TFILE = '.state/treasury.json'
  const treasuryLive = async (f) => {
    if (!existsSync(f)) return false
    const t = JSON.parse(readFileSync(f, 'utf8')).treasury
    return !!t && (await utxoStatus(rc, t.txid, t.vout)) === 'unspent'
  }
  // the file must name a live output — re-split from mature coinbases when a
  // previous consumer already spent it
  if (!(await treasuryLive(TFILE))) {
    await bootstrapReserve(rc, fundKey, { addOwned: () => {} }, rpc, { treasuryFile: TFILE, namespace: 'e2e', outputs: 8 })
    await rpc('generatetoaddress', 1, fundAddr)
  }
  check(await treasuryLive(TFILE), 'e2e treasury is live (bootstrapReserve refilled it)')
  const t = JSON.parse(readFileSync(TFILE, 'utf8')).treasury
  const srcTx = Transaction.fromHex(t.txHex)
  check((srcTx.outputs[t.vout]?.satoshis ?? 0) >= 20_000, `treasury has spendable sats (${srcTx.outputs[t.vout]?.satoshis})`)
  const paySats = Math.min(50_000, srcTx.outputs[t.vout].satoshis - 5000)
  const send = new Transaction()
  send.addInput({ sourceTransaction: srcTx, sourceOutputIndex: t.vout, unlockingScriptTemplate: new P2PKH().unlock(fundKey) })
  send.addOutput({ lockingScript: new P2PKH().lock(payerAddr), satoshis: paySats })
  send.addOutput({ lockingScript: new P2PKH().lock(fundAddr), change: true })
  await send.fee()
  await send.sign()
  const br = await new TeranodeBroadcaster(rc.propagationUrl).broadcast(send)
  check(br.status === 'success', `payer funding broadcast — ${send.id('hex').slice(0, 16)}… (real spend of the fund treasury)`)
  // the change output IS the next treasury — keep the file live across runs
  writeFileSync(TFILE, JSON.stringify({ namespace: 'e2e', treasury: { txid: send.id('hex'), vout: 1, txHex: send.toHex() } }))
  await rpc('generatetoaddress', 1, fundAddr)
  payerUtxos.push({ tx: send, vout: 0, satoshis: paySats })
  console.log(` payer ${payerAddr} funded by regtest tx ${send.id('hex').slice(0, 20)}…`)
  lap('payer funded on-chain')
  // hand the runtime a LIVE treasury — its seed splits it into the wallet
  // (also replaces a stale data-dir treasury a killed earlier run left)
  const dir = process.env.NEKTAR_DATA_DIR
  const dt = `${dir}/treasury.json`
  const { mkdirSync, copyFileSync } = await import('node:fs')
  mkdirSync(dir, { recursive: true })
  if (!(await treasuryLive(dt))) copyFileSync(TFILE, dt)
} else {
  // live-network payer funding is not wired — the toolbox wallet path holds
  // no operator-readable key to build the send from. Regtest is the funded
  // tier for this e2e.
  throw new Error(`e2e-pay: payer funding unsupported on ${net} — run on regtest`)
}

const rt = await openRuntime(configFromEnv())
const srv = createHttpApi(rt.mem).listen(0)
await new Promise((r) => srv.once('listening', r))
const base = `http://127.0.0.1:${srv.address().port}`
const call = async (tool, body) =>
  await (await fetch(`${base}/v1/tools/${tool}`, { method: 'POST', headers: { 'content-type': 'application/json' }, body: JSON.stringify(body) })).json()
console.log(`\n═══ payment e2e on ${net.toUpperCase()} (real chain) ═══`)

// ── a fragment to cite, anchored for real
const SHARD = 'pay-e2e'
for (let i = 0; i < 4; i++) await call('remember', { shard: SHARD, data: `paid answers settle on a real chain — record ${i}` })
console.log(' fragment committed — waiting for anchor…')
const anchorT0 = Date.now()
let f
for (let i = 0; i < 60; i++) {
  const r = await call('recall', { shard: SHARD, seq: 0 })
  f = r.items?.[0]
  if (f && !f.unverifiable) break
  await new Promise((r) => setTimeout(r, 5000))
}
const anchorLag = Date.now() - anchorT0
check(
  f && !f.unverifiable,
  `${SHARD}:0 anchored + verified on ${net} (anchor lag ${Math.round(anchorLag / 1000)}s${f && !f.unverifiable ? '' : ` — TIMEOUT, unverifiable=${f?.unverifiable}`})`
)
lap('anchor→proof wait')

// ── 1) catalog + unpaid call → the 402 invoice
const catalog = await (await fetch(`${base}/.well-known/x402`)).json()
check(
  catalog.resources?.some((r) => r.url === 'tool://groundedAnswer'),
  'discovery catalog lists priced tools'
)
const denied = await call('groundedAnswer', { claims: [{ text: 'paid answers settle on a real chain — record 0', cites: [{ shard: SHARD, seq: 0 }] }] })
check(denied.ok === false && denied.accepts?.[0]?.scheme === 'exact', 'unpaid call refused with x402 invoice')
lap('402 invoice')
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
lap('sign+broadcast+commit')

// ── 3) the payment tx exists ON THE REAL CHAIN
{
  const res = await fetch(`${rc.assetUrl}/tx/${txid}`).catch(() => null)
  check(!!res?.ok, `payment tx resolvable via Teranode asset API (${res?.status ?? 'unreachable'})`)
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
lap('verify+replay')
console.log(`\n receipt: ${r.receiptToken}\n verify:  ${r.verifyUrl}\n  [total: ${Date.now() - _t0}ms — anchor lag ${Math.round(anchorLag / 1000)}s]`)
srv.close()
await rt.close()
process.exit(0)
