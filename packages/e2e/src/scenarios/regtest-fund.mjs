#!/usr/bin/env node
// Build a usable regtest treasury on a long-lived chain whose recent coinbase
// outputs are small: aggregate batches of mature coinbases into one large UTXO.
// Writes the treasury.json shape openRuntime's bootstrapReserve consumes.
import { writeFileSync, readFileSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'
import { PrivateKey, Transaction, P2PKH } from '@bsv/sdk'
import { parseLegacyBlock, TeranodeBroadcaster } from '../../../chain/dist/src/regtest.js'

const env = process.env
const key = PrivateKey.fromWif(env.NEKTAR_REGTEST_WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const auth = 'Basic ' + Buffer.from(`${env.NEKTAR_RPC_USER}:${env.NEKTAR_RPC_PASS}`).toString('base64')
const cfg = {
  rpcUrl: env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  assetUrl: env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1',
  propagationUrl: env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx'
}
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(JSON.stringify(j.error))
  return j.result
}
const want = Buffer.from(key.toPublicKey().toHash()).toString('hex')
const target = Number(env.NEKTAR_TREASURY_SATS ?? 2_000_000)

async function coinbases(from, to) {
  const out = []
  for (let h = from; h <= to; h++) {
    const hash = await rpc('getblockhash', h)
    const res = await fetch(`${cfg.assetUrl}/block_legacy/${hash}`, { headers: { Authorization: auth } })
    if (!res.ok) continue
    const { txs } = parseLegacyBlock(new Uint8Array(await res.arrayBuffer()))
    const cb = Transaction.fromHex(Buffer.from(txs[0]).toString('hex'))
    cb.outputs.forEach((o, vout) => {
      if (o.lockingScript.toHex().includes(want) && (o.satoshis ?? 0) > 0) out.push({ tx: cb, vout, satoshis: o.satoshis })
    })
  }
  return out
}

async function combine(inputs, label) {
  const tx = new Transaction()
  let total = 0
  for (const u of inputs) {
    tx.addInput({ sourceTransaction: u.tx, sourceOutputIndex: u.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
    total += u.satoshis
  }
  const fee = Math.min(2000, Math.max(100, Math.floor(inputs.length * 20)))
  tx.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: total - fee })
  await tx.sign()
  const r = await broadcaster.broadcast(tx)
  if (r.status !== 'success') throw new Error(`${label} broadcast failed: ${r.description}`)
  console.log(`${label}: ${tx.id('hex').slice(0, 16)}… ${total - fee} sats`)
  return { tx, vout: 0, satoshis: total - fee }
}

const tip = (await rpc('getblockchaininfo')).blocks
const needBlocks = Math.min(1500, Math.ceil(target / 700) + 150)
console.log(`tip ${tip}; generating ${needBlocks} blocks to ${addr}`)
for (let left = needBlocks; left > 0;) {
  const n = Math.min(100, left)
  for (let tries = 0; tries < 5; tries++) {
    try {
      await rpc('generatetoaddress', n, addr)
      left -= n
      break
    } catch (e) {
      if (tries === 4) throw e
      await new Promise((r) => setTimeout(r, 2000))
    }
  }
}
const newTip = (await rpc('getblockchaininfo')).blocks
const matureTo = newTip - 101
const coins = await coinbases(tip + 1, matureTo)
console.log(`collected ${coins.length} mature coinbase outputs`)
if (coins.every((c) => c.satoshis === 0)) {
  throw new Error(
    'this regtest chain has no spendable coinbase subsidy left (long halving history). Reset the LOCAL test chain with infra/teranode-quickstart/clean.sh --data-only, or point at a fresh regtest stack. No treasury was created.'
  )
}
const batches = []
for (let i = 0; i < coins.length; i += 100) batches.push(await combine(coins.slice(i, i + 100), `batch ${i / 100}`))
let final = batches
while (final.length > 1) {
  const next = []
  for (let i = 0; i < final.length; i += 12) next.push(await combine(final.slice(i, i + 12), `combine ${i}`))
  final = next
}
const treasury = final[0]
if (!treasury || treasury.satoshis < 1_000_000) throw new Error(`treasury too small: ${treasury?.satoshis}`)
const file = env.NEKTAR_TREASURY_FILE
mkdirSync(dirname(file), { recursive: true })
// preserve the target file's namespace tag (or NEKTAR_TREASURY_NS when seeding
// a fresh file) so consumer-side isolation checks keep working across refills
let namespace = env.NEKTAR_TREASURY_NS
try {
  namespace ??= JSON.parse(readFileSync(file, 'utf8')).namespace
} catch {}
writeFileSync(file, JSON.stringify({ namespace, treasury: { txid: treasury.tx.id('hex'), vout: treasury.vout, txHex: treasury.tx.toHex() } }))
console.log(`treasury written to ${file}: ${treasury.satoshis} sats`)
