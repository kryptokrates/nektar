#!/usr/bin/env node
// sweep-agents.mjs — recover a finished run's agent leftovers back into the
// real-stack treasury. Each agent's records.sqlite tx_store holds every tx it
// broadcast, so the full UTXO set under its key is derivable: outputs paying
// the agent's P2PKH script minus every stored input's source outpoint.
// Candidates are confirmed live via the asset API's utxo endpoint (which names
// the spending txid for spent outputs), then swept to the treasury address and
// combined with the current file tip into a single outpoint written back to
// .state/real-stack-treasury.json.
//
//   node packages/e2e/src/scenarios/sweep-agents.mjs <runDir>
//
// Requires NEKTAR_REGTEST_WIF + rpc user/pass (repo .env provides them).

import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { DatabaseSync } from 'node:sqlite'
import { TeranodeBroadcaster } from '../../../chain/dist/src/regtest.js'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const runDir = resolve(process.argv[2])
const envFile = join(root, '.env')
const env = existsSync(envFile)
  ? Object.fromEntries(
      readFileSync(envFile, 'utf8')
        .split(/\r?\n/)
        .map((l) => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/))
        .filter(Boolean)
        .map((m) => [m[1], m[2].trim()])
    )
  : {}
const E = (k) => process.env[k] ?? env[k]
const treasuryKey = PrivateKey.fromWif(E('NEKTAR_REGTEST_WIF'))
const treasuryAddr = treasuryKey.toPublicKey().toAddress([0x6f])
const treasuryScript = new P2PKH().lock(treasuryAddr)
const auth = 'Basic ' + Buffer.from(`${E('NEKTAR_RPC_USER')}:${E('NEKTAR_RPC_PASS')}`).toString('base64')
const cfg = {
  rpcUrl: E('NEKTAR_RPC_URL') ?? 'http://localhost:9292/',
  assetUrl: E('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1',
  propagationUrl: E('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx'
}
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const TREASURY_NS = 'real-stack'
const TREASURY_FILE = join(root, '.state', 'real-stack-treasury.json')
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
// /utxo/{txid}?vout=N answers a binary record: live outpoints are an all-zero
// 12-byte stub; spent ones carry a 48-byte record naming the spending txid.
const utxoLive = async (txid, vout) => {
  for (let i = 0; i < 10; i++) {
    try {
      const r = await fetch(`${cfg.assetUrl}/utxo/${txid}?vout=${vout}`, { signal: AbortSignal.timeout(10_000) })
      if (r.status === 429) {
        await delay(400 * (i + 1))
        continue
      }
      if (!r.ok) return false
      const buf = Buffer.from(await r.arrayBuffer())
      return buf.every((b) => b === 0)
    } catch {
      if (i === 9) return false
      await delay(400 * (i + 1))
    }
  }
  return false
}

const keysPath = join(runDir, 'agent-keys.json')
if (!existsSync(keysPath)) {
  console.log('  no agent-keys.json — run died before funding')
  process.exit(0)
}
const keys = JSON.parse(readFileSync(keysPath, 'utf8'))
const swept = [] // {tx: Transaction, vout, sats} — treasury-bound outputs
for (const [name, wif] of Object.entries(keys)) {
  const dbPath = join(runDir, name, 'records.sqlite')
  if (!existsSync(dbPath)) continue
  const key = PrivateKey.fromWif(wif)
  const target = new P2PKH().lock(key.toPublicKey().toAddress([0x6f])).toHex()
  const db = new DatabaseSync(dbPath, { readOnly: true })
  let rows
  try {
    rows = db.prepare('select txid, raw from tx_store').all()
  } catch {
    continue
  } finally {
    db.close()
  }
  const spent = new Set()
  const outputs = []
  const parents = new Map()
  for (const { txid, raw } of rows) {
    const tx = Transaction.fromHex(raw)
    parents.set(txid, tx)
    for (const inp of tx.inputs) if (inp.sourceTXID) spent.add(`${inp.sourceTXID}:${inp.sourceOutputIndex}`)
    tx.outputs.forEach((o, i) => {
      if (o.lockingScript.toHex() === target) outputs.push({ txid, vout: i, sats: o.satoshis })
    })
  }
  const live = []
  for (const c of outputs.filter((o) => !spent.has(`${o.txid}:${o.vout}`))) {
    if (await utxoLive(c.txid, c.vout)) live.push(c)
  }
  const sats = live.reduce((a, c) => a + c.sats, 0)
  if (live.length === 0) continue
  const tx = new Transaction()
  for (const c of live) {
    tx.addInput({ sourceTransaction: parents.get(c.txid), sourceOutputIndex: c.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  }
  tx.addOutput({ lockingScript: treasuryScript, change: true })
  await tx.fee(new SatoshisPerKilobyte(100))
  await tx.sign()
  const r = await broadcaster.broadcast(tx)
  if (r.status !== 'success') {
    console.log(`  ${name}: sweep refused: ${r.description}`)
    continue
  }
  const fee = tx.getFee()
  console.log(`  ${name}: swept ${sats - fee} sats (${live.length} utxos) → ${tx.id('hex').slice(0, 12)}…`)
  swept.push({ tx, vout: 0, sats: sats - fee })
}
if (swept.length === 0) {
  console.log('  nothing to sweep')
  process.exit(0)
}

// combine: prior file tip + all sweep outputs → the new single tip
const file = JSON.parse(readFileSync(TREASURY_FILE, 'utf8'))
if (file.namespace !== undefined && file.namespace !== TREASURY_NS)
  throw new Error(`treasury file ${TREASURY_FILE} is namespaced '${file.namespace}' — refusing to consume another consumer's treasury`)
const tx = new Transaction()
const fileParent = Transaction.fromHex(file.treasury.txHex)
tx.addInput({ sourceTransaction: fileParent, sourceOutputIndex: file.treasury.vout, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
for (const s of swept) {
  tx.addInput({ sourceTransaction: s.tx, sourceOutputIndex: s.vout, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
}
tx.addOutput({ lockingScript: treasuryScript, change: true })
await tx.fee(new SatoshisPerKilobyte(100))
await tx.sign()
const r = await broadcaster.broadcast(tx)
if (r.status !== 'success') throw new Error(`combine refused: ${r.description}`)
await rpc('generatetoaddress', 1, treasuryAddr)
const total = fileParent.outputs[file.treasury.vout].satoshis + swept.reduce((a, s) => a + s.sats, 0) - tx.getFee()
writeFileSync(TREASURY_FILE, JSON.stringify({ namespace: TREASURY_NS, treasury: { txid: tx.id('hex'), vout: 0, txHex: tx.toHex() } }, null, 1))
console.log(`  treasury: ${fileParent.outputs[file.treasury.vout].satoshis} + ${swept.reduce((a, s) => a + s.sats, 0)} swept → ${total} @ ${tx.id('hex').slice(0, 12)}…`)
