#!/usr/bin/env node
// custody.mjs — read-only custody inventory + toolbox→client migration checklist.
// Answers "what does this instance still depend on, and what moves under
// NEKTAR_WALLET=client" without touching a key or broadcasting anything.
//
//   node tools/custody.mjs [--data <dir>] [--json]
import { existsSync, readFileSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { createRequire } from 'node:module'

const root = join(fileURLToPath(import.meta.url), '..', '..')
const { parseDotEnv } = await import('./dotenv.mjs')
const argv = process.argv.slice(2)
const flag = (n) => {
  const i = argv.indexOf(n)
  return i >= 0 ? argv[i + 1] : undefined
}
const json = argv.includes('--json')

const env = existsSync(join(root, '.env')) ? parseDotEnv(readFileSync(join(root, '.env'), 'utf8')) : {}
const get = (k) => process.env[k] ?? env[k]
const dataDir = resolve(flag('--data') ?? get('NEKTAR_DATA_DIR') ?? join(root, '.nektar'))

// ── substrate state — the JSON API answers on :3321 even when locked ──
const probe = async () => {
  try {
    const r = await fetch('http://localhost:3321/getVersion', {
      method: 'POST',
      headers: { 'content-type': 'application/json', origin: 'http://localhost' },
      body: '{}'
    })
    const body = await r.json().catch(() => ({}))
    return body?.code === 'WALLET_LOCKED' || (body?.description ?? '').includes('locked') ? 'locked' : 'live'
  } catch {
    return 'absent'
  }
}
const substrate = await probe()

// ── derived addresses (only derivable when the substrate answers) ──
let derived = null
if (substrate === 'live') {
  const { WalletClient, PublicKey } = await import('@bsv/sdk')
  const wallet = new WalletClient('auto', 'nektar')
  const FUND = { protocolID: [2, 'nektar'], keyID: 'nektar-fund', counterparty: 'self' }
  const pub = (await wallet.getPublicKey(FUND)).publicKey
  const pk = PublicKey.fromString(pub)
  derived = { pub, pkhHex: Buffer.from(pk.toHash()).toString('hex'), testnet: pk.toAddress([0x6f]), mainnet: pk.toAddress() }
}

// ── data-dir artifacts ──
const artifacts = {}
for (const f of ['keys.json', 'fee.key.json', 'reserve.json', 'wallet.sqlite', 'wallet.sqlite.migrated', 'records.sqlite']) {
  const p = join(dataDir, f)
  if (existsSync(p)) artifacts[f] = p
}
// keys.json: plaintext {agentSeed,logKey,shardKeys} vs encrypted {v,kdf,...}
let keysShape = 'absent'
if (artifacts['keys.json']) {
  try {
    const k = JSON.parse(readFileSync(artifacts['keys.json'], 'utf8'))
    keysShape = k.ct !== undefined ? `encrypted (${Object.keys(k).length} fields)` : `plaintext — legacy key material readable`
  } catch {
    keysShape = 'unreadable'
  }
}
// reserve.json: entries minus spent = live toolbox reserve stock
let reserve = { live: 0, sats: 0 }
if (artifacts['reserve.json']) {
  try {
    const r = JSON.parse(readFileSync(artifacts['reserve.json'], 'utf8'))
    const spent = new Set(r.spent ?? [])
    const live = (r.entries ?? []).filter((e) => !spent.has(`${e.txid}:${e.vout}`))
    reserve = { live: live.length, sats: live.reduce((a, b) => a + (b.satoshis ?? 0), 0) }
  } catch {
    reserve = { live: -1, sats: 0 }
  }
}
// records.sqlite coin book — live rows and which pkh they lock to (needs tx bytes)
const book = { live: 0, sats: 0, toDerived: 0, toOther: 0, undecodable: 0 }
if (artifacts['records.sqlite']) {
  const req = createRequire(join(root, 'packages/mcp/package.json'))
  const knex = req('knex')({ client: 'sqlite3', connection: { filename: artifacts['records.sqlite'] }, useNullAsDefault: true })
  try {
    if (await knex.schema.hasTable('utxos')) {
      const rows = await knex('utxos').where({ state: 'live' }).select('txid', 'vout', 'satoshis')
      book.live = rows.length
      book.sats = rows.reduce((a, r) => a + r.satoshis, 0)
      if (derived !== null && (await knex.schema.hasTable('tx_store'))) {
        const { Transaction } = await import('@bsv/sdk')
        for (const r of rows) {
          const t = await knex('tx_store').where({ txid: r.txid }).first()
          const out = t !== undefined ? Transaction.fromHex(t.raw).outputs[r.vout] : undefined
          const pkh = out?.lockingScript?.chunks?.[2]?.data !== undefined ? Buffer.from(out.lockingScript.chunks[2].data).toString('hex') : undefined
          if (pkh === derived.pkhHex) book.toDerived++
          else if (pkh !== undefined) book.toOther++
          else book.undecodable++
        }
      }
    }
  } finally {
    await knex.destroy()
  }
}

const report = {
  substrate,
  derived,
  env: {
    NEKTAR_NETWORK: get('NEKTAR_NETWORK'),
    NEKTAR_WALLET: get('NEKTAR_WALLET') ?? '(unset → toolbox)',
    NEKTAR_FUND_WIF: get('NEKTAR_FUND_WIF') !== undefined,
    NEKTAR_FUND_MNEMONIC: get('NEKTAR_FUND_MNEMONIC') !== undefined,
    NEKTAR_KEY_PASSPHRASE: get('NEKTAR_KEY_PASSPHRASE') !== undefined
  },
  dataDir,
  artifacts: Object.fromEntries(Object.entries(artifacts).map(([k, v]) => [k, v.replace(root, '.')])),
  keysShape,
  reserve,
  book
}

// ── the checklist the inventory implies ──
const steps = []
if (substrate === 'absent') steps.push('START BSV Desktop (unlocked) — nothing under client custody signs until :3321 answers')
if (substrate === 'locked') steps.push('UNLOCK the BSV Desktop vault — a locked wallet boots nektar into the misleading "Install a BSV wallet" error')
if (derived !== null) {
  steps.push(`fund the derived reserve: testnet ${derived.testnet} / mainnet ${derived.mainnet} (NEKTAR_WAIT_FUNDING=1 holds for it)`)
}
if (reserve.live > 0)
  steps.push(`sweep the toolbox reserve (${reserve.live} outpoint(s), ${reserve.sats} sats) to the derived address — it does NOT migrate itself`)
if (book.toOther > 0)
  steps.push(`coin book has ${book.toOther} live row(s) locked to a non-derived key — unspendable under client; sweep under toolbox first or mark exported`)
if (artifacts['keys.json'] !== undefined)
  steps.push(
    `keep keys.json + NEKTAR_KEY_PASSPHRASE — legacy shard owners are still READ under client mode; retire only after those shards rotate to nektar-shard: owners or are retired`
  )
if (env.NEKTAR_FUND_WIF !== undefined || env.NEKTAR_FUND_MNEMONIC !== undefined)
  steps.push('NEKTAR_FUND_WIF/_MNEMONIC are ignored under client — remove once the old reserve is swept')
report.checklist = steps

if (json) console.log(JSON.stringify(report, null, 2))
else {
  console.log(`substrate : ${substrate}`)
  if (derived !== null) {
    console.log(`derived   : testnet ${derived.testnet}`)
    console.log(`            mainnet ${derived.mainnet}`)
  }
  console.log(`env       : NEKTAR_WALLET=${report.env.NEKTAR_WALLET} NEKTAR_NETWORK=${report.env.NEKTAR_NETWORK}`)
  console.log(`data dir  : ${dataDir}`)
  console.log(`artifacts : ${Object.keys(artifacts).join(', ') || 'none'}`)
  console.log(`keys.json : ${keysShape}`)
  console.log(`reserve   : ${reserve.live} live / ${reserve.sats} sats (toolbox tracker)`)
  console.log(`coin book : ${book.live} live / ${book.sats} sats — ${book.toDerived} to derived key, ${book.toOther} to other keys`)
  console.log('\nchecklist:')
  for (const s of steps) console.log(`  · ${s}`)
}
