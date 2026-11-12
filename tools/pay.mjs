#!/usr/bin/env node
// nektar-pay — sign a settlement tx for a 402 invoice and print the retry arg.
// The whole point: a WIF key and one spendable utxo are all it takes.
//
//   node tools/pay.mjs '<the paymentRequired JSON the tool returned>' \
//     --wif Kx… --parent-tx-hex <funding tx hex> --parent-vout 0 --parent-sats 5000
//
// Paste the printed JSON as the `payment` argument on the retried tool call.
// --beef (default) carries the funding parent inline — raw hex with --raw.
import { PrivateKey, Transaction } from '@bsv/sdk'
import { buildPaymentTx } from '../packages/mcp/dist/src/payclient.js'

const arg = (name, def) => {
  const i = process.argv.indexOf(`--${name}`)
  return i > 0 ? process.argv[i + 1] : def
}
const json = process.argv.slice(2).find((a) => !a.startsWith('--'))
if (!json) {
  console.error("usage: node tools/pay.mjs '<paymentRequired JSON>' --wif … --parent-tx-hex … --parent-vout N --parent-sats N")
  process.exit(2)
}
const pr = JSON.parse(json)
const acc = pr.accepts?.[0] ?? {}
const inv = {
  amount: Number(acc.amount ?? pr.invoice?.amount),
  lockingScriptHex: acc.extra?.lockingScriptHex ?? pr.invoice?.lockingScriptHex,
  opReturn: acc.extra?.opReturn ?? pr.invoice?.opReturn,
  invoiceId: acc.extra?.invoiceId ?? pr.invoice?.invoiceId
}
if (!inv.invoiceId || !inv.lockingScriptHex || !inv.opReturn) {
  console.error('could not find invoiceId/lockingScriptHex/opReturn in the paymentRequired JSON')
  process.exit(2)
}
const wif = arg('wif', process.env.NEKTAR_PAYER_WIF)
const parentHex = arg('parent-tx-hex', process.env.NEKTAR_PARENT_TX_HEX)
if (!wif || !parentHex) {
  console.error('need --wif and --parent-tx-hex (the funding tx that pays your key)')
  process.exit(2)
}
const payer = PrivateKey.fromWif(wif)
const parent = Transaction.fromHex(parentHex)
const vout = Number(arg('parent-vout', process.env.NEKTAR_PARENT_VOUT ?? 0))
const satoshis = Number(arg('parent-sats', process.env.NEKTAR_PARENT_SATS ?? parent.outputs[vout]?.satoshis))
const { hex, txid } = await buildPaymentTx(inv, payer, [{ tx: parent, vout, satoshis }])
console.log(JSON.stringify({ invoiceId: inv.invoiceId, transaction: hex }, null, 2))
console.error(`payment tx ${txid} — ${inv.amount} sats + tag "${inv.opReturn}"`)
