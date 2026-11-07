// Remote paid-flow e2e — drives a DEPLOYED nektar HTTP server end-to-end over
// the public wire: MCP tools/call → 402 invoice → locally-signed settlement
// tx (payer WIF, BEEF) → paid call → receipt commits the txid → public
// /v1/verify → the payment tx resolvable on the real network.
//   node packages/e2e/src/e2e-pay-remote.mjs
// env: NEKTAR_REMOTE_URL (default https://nektar.run),
//      NEKTAR_HTTP_TOKEN (bearer), NEKTAR_FUND_WIF or the ttn-data keystore +
//      NEKTAR_KEY_PASSPHRASE for the payer, NEKTAR_ARC_URL for the visibility check.
import { PrivateKey, P2PKH, Transaction } from '@bsv/sdk'
import { readFileSync, existsSync } from 'node:fs'
import { buildPaymentTx } from '../../mcp/dist/src/payclient.js'
import { readKeyFile } from '../../mcp/dist/src/keystore.js'

const BASE = process.env.NEKTAR_REMOTE_URL ?? 'https://nektar.run'
const TOKEN = process.env.NEKTAR_HTTP_TOKEN ?? ''
const ARC = process.env.NEKTAR_ARC_URL ?? 'https://arcade-v2-ttn-us-1.bsvblockchain.tech'
const check = (ok, label) => { console.log(` ${ok ? '✔' : '✗ FAIL'} ${label}`); if (!ok) process.exit(1) }
const sleep = ms => new Promise(r => setTimeout(r, ms))

// ── MCP Streamable HTTP — JSON-RPC, SSE-or-JSON response
let sid
const mcp = async (method, params) => {
  const res = await fetch(`${BASE}/mcp`, {
    method: 'POST',
    headers: { 'content-type': 'application/json', accept: 'application/json, text/event-stream', authorization: `Bearer ${TOKEN}`, ...(sid ? { 'mcp-session-id': sid } : {}) },
    body: JSON.stringify(method.startsWith('notifications/') ? { jsonrpc: '2.0', method, params } : { jsonrpc: '2.0', id: 1, method, params })
  })
  sid = res.headers.get('mcp-session-id') ?? sid
  if (res.status === 202) return undefined
  const text = await res.text()
  const lines = text.split('\n').filter(l => l.startsWith('data:'))
  const j = JSON.parse(lines.length ? lines.map(l => l.slice(5)).join('\n') : text)
  if (j.error) throw new Error(`${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const call = async (name, args) => {
  const r = await mcp('tools/call', { name, arguments: args })
  const text = r?.content?.[0]?.text ?? JSON.stringify(r)
  try { return JSON.parse(text) } catch { return { raw: text } }
}

console.log(`═══ remote paid-flow e2e → ${BASE} ═══`)

// MCP Streamable HTTP session: initialize → notifications/initialized → calls
await mcp('initialize', { protocolVersion: '2025-03-26', capabilities: {}, clientInfo: { name: 'e2e-pay-remote', version: '1' } })
await mcp('notifications/initialized', {}).catch(() => {})

// ── 0) the server is on a real network (not mock) and payments are on
const x402 = await fetch(`${BASE}/.well-known/x402`).then(r => r.ok ? r.json() : null).catch(() => null)
check(!!x402, 'x402 discovery catalog served')
if (x402) { console.log(` network=${x402.network} scheme=${x402.scheme} priced=${x402.resources.map(r => r.url).join(',')}`); check(!/mock/.test(x402.network), 'server is NOT on mock — real network required for honest settlement') }

// ── 1) payer = the local ttn fund key's spendable utxo (a real public tx spends it)
const pass = process.env.NEKTAR_KEY_PASSPHRASE ?? readFileSync('.state/ttn-passphrase', 'utf8').trim()
const wif = process.env.NEKTAR_FUND_WIF ?? JSON.parse(readKeyFile('.state/ttn-data/fee.key.json', pass, true)).wif
const payer = PrivateKey.fromWif(wif)
const res = JSON.parse(readFileSync('.state/ttn-data/reserve.json', 'utf8'))
const spent = new Set(res.spent ?? [])
const entry = res.entries.filter(e => !spent.has(`${e.txid}:${e.vout}`)).sort((a, b) => b.satoshis - a.satoshis)[0]
check(!!entry, 'payer has a spendable utxo in the local ttn reserve')
const payerUtxos = [{ tx: Transaction.fromHex(entry.txHex), vout: entry.vout, satoshis: entry.satoshis }]
console.log(` payer ${payer.toPublicKey().toAddress([0x6f])} spends ${entry.satoshis} sats (${entry.txid.slice(0, 16)}…)`)

// ── 2) remember a record on the box; its shard auto-anchors on ttn
const SHARD = `remote-${Date.now().toString(36)}`
const claims = ['the deployed box settles a real ttn payment', 'the receipt names the settlement txid']
for (const c of claims) {
  const r = await call('remember', { shard: SHARD, data: c })
  check(!!r, `remember "${c.slice(0, 40)}…"`)
}
console.log(` batch anchors automatically; waiting for the ttn proof to attach…`)

// ── 3) wait until recall verifies seq 0 — i.e. the anchor is proven on the real chain
let proven = false
for (let i = 0; i < 40 && !proven; i++) {
  await sleep(15_000)
  const r = await call('recall', { shard: SHARD, seq: 0 }).catch(() => null)
  proven = !!(r?.items?.[0] && !r.items[0].unverifiable)
  if (i % 4 === 3) console.log(` …still waiting (${(i + 1) * 15}s)`)
}
check(proven, 'fragment anchored + provable on the real network')

// ── 4) unpaid call → 402 invoice
const ga = { claims: claims.map((text, i) => ({ text, cites: [{ shard: SHARD, seq: i }] })) }
const denied = await call('groundedAnswer', ga)
check(denied.ok === false && !!denied.accepts?.[0]?.extra?.invoiceId, 'unpaid call refused with x402 invoice')
const inv = denied.accepts[0]
console.log(` invoice ${inv.extra.invoiceId} — ${inv.amount} sats → ${inv.payTo} + tag "${inv.extra.opReturn}"`)

// ── 5) sign the settlement tx locally (WIF only), retry paid
const { hex, txid } = await buildPaymentTx({ amount: inv.amount, lockingScriptHex: inv.extra.lockingScriptHex, opReturn: inv.extra.opReturn }, payer, payerUtxos)
const r = await call('groundedAnswer', { ...ga, payment: { invoiceId: inv.extra.invoiceId, transaction: hex } })
// keep the local reserve honest — the spent utxo is gone, the change is ours
if (existsSync('.state/ttn-data/reserve.json')) {
  const { Beef } = await import('@bsv/sdk')
  const { writeFileSync } = await import('node:fs')
  const subj = Beef.fromBinary(Array.from(Buffer.from(hex, 'hex'))).txs.at(-1)
  const txo = subj.tx ?? Transaction.fromBinary(subj.rawTx)
  spent.add(`${entry.txid}:${entry.vout}`)
  res.entries.push({ txid, vout: txo.outputs.length - 1, satoshis: txo.outputs.at(-1).satoshis, txHex: txo.toHex() })
  writeFileSync('.state/ttn-data/reserve.json', JSON.stringify({ spent: [...spent], entries: res.entries }))
}
check(r.committed === true, `paid call committed — settlement tx ${txid.slice(0, 16)}… broadcast by the box`)
check(r.receipt?.payment?.txid === txid, 'receipt commits the settlement txid')
check(r.receipt?.payment?.payTo === inv.payTo, `payment pays the operator ${inv.payTo}`)

// ── 6) the tx exists on the real public network
const st = await fetch(`${ARC}/tx/${txid}`).then(x => x.ok ? x.json() : null).catch(() => null)
check(!!st?.txid || !!st?.txStatus, `settlement tx visible on the network (${st?.txStatus ?? 'pending'})`)

// ── 7) public verify — no auth, what a phone sees
const v = await fetch(`${BASE}/v1/verify?r=${r.receiptToken}`).then(x => x.json())
check(v.ok === true, 'receipt verifies publicly')
check(v.checks.find(c => c.name === 'paid')?.ok === true, `'paid' check green — ${v.checks.find(c => c.name === 'paid')?.detail}`)

console.log(`\n receipt: ${r.receiptToken}\n verify:  ${r.verifyUrl}\n tx:      ${txid}\n`)
process.exit(0)
