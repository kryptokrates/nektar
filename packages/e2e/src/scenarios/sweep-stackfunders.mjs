#!/usr/bin/env node
// sweep-stackfunders.mjs — return sats parked in dead StackFunder worlds to
// the testkit treasury. Worlds record {wif, fundTxid?, outputs?} in
// .state/stackfunder-keys.jsonl; fund txs leave the txmeta index ~60s after
// their parents are spent, so discovery is a bounded BLOCK SCAN, not a tx
// lookup: every output paying a known world key (or the treasury key) in the
// scan window is a candidate, confirmed live via the utxo endpoint.
//
//   node packages/e2e/src/scenarios/sweep-stackfunders.mjs [fromHeight]
//
// Requires NEKTAR_REGTEST_WIF + rpc user/pass (repo .env provides them).

import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte } from '@bsv/sdk'
import { readFileSync, writeFileSync, existsSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { setTimeout as delay } from 'node:timers/promises'
import { TeranodeBroadcaster, parseLegacyBlock } from '../../../chain/dist/src/regtest.js'

const root = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const envFile = join(root, '.env')
/** @type {Record<string, string | undefined>} */
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
const auth = 'Basic ' + Buffer.from(`${E('NEKTAR_RPC_USER')}:${E('NEKTAR_RPC_PASS')}`).toString('base64')
const rpcUrl = E('NEKTAR_RPC_URL') ?? 'http://localhost:9292/'
const assetUrl = E('NEKTAR_ASSET_URL') ?? 'http://localhost:8090/api/v1'
const rpc = async (method, ...params) => {
  const r = await fetch(rpcUrl, {
    method: 'POST',
    headers: { 'content-type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params })
  })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const blockLegacy = async (hash) => {
  for (let i = 0; i < 4; i++) {
    try {
      const res = await fetch(`${assetUrl}/block_legacy/${hash}`, { signal: AbortSignal.timeout(20_000) })
      if (res.status === 429) {
        await delay(300 * (i + 1))
        continue
      }
      if (!res.ok) return null
      return new Uint8Array(await res.arrayBuffer())
    } catch {
      if (i === 3) return null
      await delay(300 * (i + 1))
    }
  }
  return null
}
const utxoLive = async (txid, vout) => {
  for (let i = 0; i < 6; i++) {
    try {
      const r = await fetch(`${assetUrl}/utxo/${txid}?vout=${vout}`, { signal: AbortSignal.timeout(8_000) })
      if (r.status === 429) {
        await delay(300 * (i + 1))
        continue
      }
      if (!r.ok) return false
      const buf = Buffer.from(await r.arrayBuffer())
      return buf.every((b) => b === 0)
    } catch {
      if (i === 5) return false
      await delay(300 * (i + 1))
    }
  }
  return false
}

// ---- known keys -----------------------------------------------------------
const keysFile = join(root, '.state', 'stackfunder-keys.jsonl')
const rows = existsSync(keysFile)
  ? readFileSync(keysFile, 'utf8')
      .split(/\r?\n/)
      .filter(Boolean)
      .map((l) => {
        try {
          return JSON.parse(l)
        } catch {
          return null
        }
      })
      .filter((r) => r?.wif)
  : []
const byPkh = new Map() // pkhHex → PrivateKey
for (const r of rows) {
  const k = PrivateKey.fromWif(r.wif)
  byPkh.set(Buffer.from(k.toPublicKey().toHash()).toString('hex'), k)
}
byPkh.set(Buffer.from(treasuryKey.toPublicKey().toHash()).toString('hex'), treasuryKey)

// ---- scan window -----------------------------------------------------------
const tip = (await rpc('getblockchaininfo')).blocks
let from = Number(process.argv[2])
if (!Number.isFinite(from)) {
  // default: last ~3h — binary-search the first block at that boundary
  const target = Math.floor(Date.now() / 1000) - 3 * 3600
  let lo = 1,
    hi = tip
  while (lo < hi) {
    const mid = (lo + hi) >> 1
    const b = await rpc('getblock', await rpc('getblockhash', mid), 1)
    if (b.time < target) lo = mid + 1
    else hi = mid
  }
  from = lo
}
console.log(`${byPkh.size} known keys; scanning heights ${from}..${tip}`)

// ---- collect candidate outputs --------------------------------------------
const found = [] // {tx, vout, sats, key}
const CONC = 8
let h = from
const worker = async () => {
  for (;;) {
    const my = h++
    if (my > tip) return
    const hash = await rpc('getblockhash', my).catch(() => null)
    if (hash === null) continue
    const raw = await blockLegacy(hash)
    if (raw === null) continue
    let parsed
    try {
      parsed = parseLegacyBlock(raw)
    } catch {
      continue
    }
    for (const rawTx of parsed.txs) {
      let tx
      try {
        tx = Transaction.fromHex(Buffer.from(rawTx).toString('hex'))
      } catch {
        continue
      }
      const txid = tx.id('hex')
      for (const [v, o] of tx.outputs.entries()) {
        const hex = o.lockingScript.toHex()
        if (!hex.startsWith('76a914') || !hex.endsWith('88ac') || (o.satoshis ?? 0) < 546) continue
        const key = byPkh.get(hex.slice(6, 46))
        if (key !== undefined) found.push({ tx, vout: v, sats: o.satoshis, key, txid })
      }
    }
  }
}
await Promise.all(Array.from({ length: CONC }, worker))
console.log(`found ${found.length} candidate outputs paying known keys`)

// ---- liveness + sweep ------------------------------------------------------
// never sweep outpoints a state file still points at: namespaced treasury
// tips (all history) and every reserve entry not marked spent — spending a
// live tip out from under its file would fork the accounting
const excluded = new Set()
const stateDir = join(root, '.state')
for (const f of ['treasury.json', 'real-stack-treasury.json', 'testkit-treasury-chain.json']) {
  try {
    const j = JSON.parse(readFileSync(join(stateDir, f), 'utf8'))
    for (const t of [j.treasury, ...(j.history ?? [])]) if (t?.txid) excluded.add(`${t.txid}:${t.vout}`)
  } catch {
    /* absent/corrupt file → nothing to exclude */
  }
}
for (const f of ['reserve.json']) {
  try {
    const j = JSON.parse(readFileSync(join(stateDir, f), 'utf8'))
    const spent = new Set(j.spent ?? [])
    for (const e of j.entries ?? []) if (!spent.has(`${e.txid}:${e.vout}`)) excluded.add(`${e.txid}:${e.vout}`)
  } catch {
    /* absent/corrupt */
  }
}
const live = []
for (const c of found) {
  if (!excluded.has(`${c.txid}:${c.vout}`) && (await utxoLive(c.txid, c.vout))) live.push(c)
}
console.log(`${live.length} live (${live.reduce((a, c) => a + c.sats, 0)} sats)`)
if (live.length === 0) process.exit(0)
const broadcaster = new TeranodeBroadcaster(E('NEKTAR_PROPAGATION_URL') ?? 'http://localhost:8833/tx')
const CHUNK = 120
const sweepTxs = [] // one output each, all paying treasuryAddr
for (let i = 0; i < live.length; i += CHUNK) {
  const part = live.slice(i, i + CHUNK)
  const tx = new Transaction()
  for (const c of part) {
    tx.addInput({ sourceTransaction: c.tx, sourceOutputIndex: c.vout, unlockingScriptTemplate: new P2PKH().unlock(c.key) })
  }
  tx.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), change: true })
  await tx.fee(new SatoshisPerKilobyte(100))
  await tx.sign()
  const r = await broadcaster.broadcast(tx)
  if (r.status !== 'success') {
    console.log(`chunk ${i / CHUNK}: refused — ${r.description}; continuing`)
    continue
  }
  sweepTxs.push(tx)
  if (sweepTxs.length % 10 === 0) console.log(`  ${sweepTxs.length} sweep txs broadcast…`)
}
if (sweepTxs.length === 0) throw new Error('every sweep chunk was refused')
console.log(`${sweepTxs.length} sweep txs broadcast; mining before combine`)
await rpc('generatetoaddress', 2, treasuryAddr)
// combine: every sweep output → one tip
const combine = new Transaction()
for (const s of sweepTxs) {
  combine.addInput({ sourceTransaction: s, sourceOutputIndex: 0, unlockingScriptTemplate: new P2PKH().unlock(treasuryKey) })
}
combine.addOutput({ lockingScript: new P2PKH().lock(treasuryAddr), change: true })
await combine.fee(new SatoshisPerKilobyte(100))
await combine.sign()
const rc = await broadcaster.broadcast(combine)
if (rc.status !== 'success') throw new Error(`combine refused: ${rc.description}`)
await rpc('generatetoaddress', 1, treasuryAddr)
const chainFile = join(root, '.state', 'testkit-treasury-chain.json')
writeFileSync(chainFile, JSON.stringify({ namespace: 'testkit', treasury: { txid: combine.id('hex'), vout: 0, txHex: combine.toHex() } }, null, 1))
console.log(`swept ${sweepTxs.length} chunks → ${combine.outputs[0].satoshis} sats; testkit tip → ${combine.id('hex').slice(0, 16)}…`)
