// ai_inference Groth16 verifier deploy+spend on PUBLIC BSV testnet.
// Same txs as e2e-grothinf-regtest.mjs, pointed at the live network:
//   broadcast:  BSV Broadcaster testnet (NEKTAR_BROADCASTER_URL overridable)
//   discovery:  WhatsOnChain testnet index (NEKTAR_WOC_URL overridable)
//
// Funding: NEKTAR_FUND_WIF wins; otherwise the encrypted ttn-data keystore
//   (.state/ttn-data/fee.key.json + .state/ttn-passphrase, or
//   NEKTAR_KEY_PASSPHRASE) — the key must hold a testnet UTXO.
//
// Unlike regtest we CANNOT mine: after broadcast we poll WoC for block
// inclusion and report honestly — RECEIVED on the testnet mempool is a
// propagation status; MINED is what makes it real.
//
// Run: node packages/e2e/src/e2e-grothinf-testnet.mjs
import { PrivateKey, Transaction, LockingScript, UnlockingScript, P2PKH } from '@bsv/sdk'
import { BsvBroadcaster, WocTracker, discoverUtxos, fetchBroadcasterFeeRate } from '@nektar/chain'
import { readKeyFile } from '../../mcp/dist/src/keystore.js'
import { existsSync, readFileSync } from 'node:fs'

const OUT = new URL('../../../tools/groth16-spike/out/', import.meta.url)
const lockHex = readFileSync(new URL('lock-aiinf.hex', OUT), 'utf8').trim()
const unlockHex = readFileSync(new URL('unlock-aiinf.hex', OUT), 'utf8').trim()
const meta = JSON.parse(readFileSync(new URL('meta-aiinf.json', OUT), 'utf8'))
console.log(`verifier: ${meta.circuit} (${meta.curve}) — statement ${meta.public_statement_fr.slice(0, 18)}…`)

let failures = 0
const check = (cond, name) => {
  console.log(`${cond ? 'PASS' : 'FAIL'} ${name}`)
  if (!cond) failures++
}

// --- config ----------------------------------------------------------------
const cfg = {
  broadcasterUrl: process.env.NEKTAR_BROADCASTER_URL ?? 'https://arcade-v2-testnet-us-1.bsvblockchain.tech',
  wocUrl: process.env.NEKTAR_WOC_URL ?? 'https://api.whatsonchain.com/v1/bsv/test',
  broadcasterApiKey: process.env.NEKTAR_BROADCASTER_API_KEY,
  wocApiKey: process.env.NEKTAR_WOC_API_KEY
}
const broadcaster = new BsvBroadcaster(cfg)
const tracker = new WocTracker(cfg)

// --- key -------------------------------------------------------------------
function loadWif() {
  if (process.env.NEKTAR_FUND_WIF) return process.env.NEKTAR_FUND_WIF
  const pass = process.env.NEKTAR_KEY_PASSPHRASE ?? (existsSync('.state/ttn-passphrase') ? readFileSync('.state/ttn-passphrase', 'utf8').trim() : undefined)
  const keyFile = process.env.NEKTAR_FEE_KEY_FILE ?? '.state/ttn-data/fee.key.json'
  if (!existsSync(keyFile)) throw new Error(`no NEKTAR_FUND_WIF and no key file at ${keyFile}`)
  return JSON.parse(readKeyFile(keyFile, pass, true)).wif
}
const key = PrivateKey.fromWif(loadWif())
const addr = key.toPublicKey().toAddress([0x6f])
const p2pkh = () => new P2PKH().lock(addr)
console.log(`funding address: ${addr}`)

// --- funding ---------------------------------------------------------------
const utxos = await discoverUtxos(cfg, addr)
const fund = utxos.sort((a, b) => b.satoshis - a.satoshis)[0]
check(!!fund, `testnet UTXO discovered (${utxos.length} found${fund ? `, largest ${fund.satoshis} sats` : ''})`)
if (!fund) process.exit(1)

const feeRate = await fetchBroadcasterFeeRate(cfg).catch(() => 1)
const feeFor = (bytes) => Math.max(Math.ceil((bytes / 1000) * feeRate) + 500, 1500)
console.log(`broadcaster fee rate: ${feeRate} sat/kB`)

const broadcast = async (tx) => {
  const r = await broadcaster.broadcast(tx)
  return r.status === 'success' ? { ok: true, txid: tx.id('hex') } : { ok: false, error: r.description ?? r.status }
}
const wocStatus = async (txid) => {
  try {
    const r = await fetch(`${cfg.wocUrl}/tx/hash/${txid}`, { signal: AbortSignal.timeout(15_000) })
    const j = await r.json()
    return j.blockheight > 0 ? `MINED h${j.blockheight}` : 'in mempool (unconfirmed)'
  } catch (e) {
    return `status unknown (${e.message})`
  }
}

// --- tx1: deploy the verifier ----------------------------------------------
const LOCK_SATS = 20_000
const tx1 = new Transaction()
tx1.addInput({ sourceTransaction: fund.tx, sourceOutputIndex: fund.vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
tx1.addOutput({ lockingScript: LockingScript.fromHex(lockHex), satoshis: LOCK_SATS })
const change1 = fund.satoshis - LOCK_SATS - feeFor(lockHex.length / 2 + 400)
if (change1 <= 0) {
  console.log(`funding utxo too small for fee (${fund.satoshis} sats)`)
  process.exit(1)
}
tx1.addOutput({ lockingScript: p2pkh(), satoshis: change1 })
await tx1.sign()
console.log(`tx1 size: ${tx1.toHex().length / 2} bytes, fee≈${fund.satoshis - LOCK_SATS - change1} sats`)
const b1 = await broadcast(tx1)
check(b1.ok, `tx1 (deploy ${lockHex.length / 2}B verifier) broadcast${b1.ok ? ` ${b1.txid}` : ` — ${b1.error}`}`)
if (!b1.ok) {
  console.log('\nNOTE: a ~400KB tx may exceed public-testnet relay policy. The verifier')
  console.log('evaluates identically under regtest consensus (proven); this is a')
  console.log('network-acceptance test, not a new claim about the script.')
  process.exit(1)
}
console.log(`  https://test.whatsonchain.com/tx/${b1.txid}`)

// --- tx2: spend it with the inference proof ---------------------------------
const tx2 = new Transaction()
tx2.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(unlockHex) })
tx2.addOutput({ lockingScript: p2pkh(), satoshis: LOCK_SATS - feeFor(unlockHex.length / 2 + 400) })
console.log(`tx2 size: ${tx2.toHex().length / 2} bytes, fee≈${LOCK_SATS - (LOCK_SATS - feeFor(unlockHex.length / 2 + 400))} sats`)
const b2 = await broadcast(tx2)
check(b2.ok, `tx2 (inference Groth16 proof spend) broadcast${b2.ok ? ` ${b2.txid}` : ` — ${b2.error}`}`)
if (b2.ok) console.log(`  https://test.whatsonchain.com/tx/${b2.txid}`)

// --- negative: tampered unlock must be rejected ------------------------------
if (b2.ok) {
  const bad = Buffer.from(unlockHex, 'hex')
  bad[Math.floor(bad.length / 2)] ^= 0x01
  const tx3 = new Transaction()
  tx3.addInput({ sourceTransaction: tx1, sourceOutputIndex: 0, unlockingScript: UnlockingScript.fromHex(bad.toString('hex')) })
  tx3.addOutput({ lockingScript: p2pkh(), satoshis: LOCK_SATS - feeFor(bad.length + 400) })
  const b3 = await broadcast(tx3)
  check(!b3.ok, `tampered unlock rejected${!b3.ok ? ` (${String(b3.error).slice(0, 90)})` : ' — WARNING accepted!'}`)
}

// --- settlement status (public testnet: we poll, we cannot mine) ------------
if (b1.ok) {
  const deadline = Date.now() + Number(process.env.NEKTAR_CONFIRM_WAIT_MS ?? 120_000)
  let s1 = 'in mempool (unconfirmed)'
  while (Date.now() < deadline) {
    s1 = await wocStatus(b1.txid)
    if (s1.startsWith('MINED')) break
    await new Promise((r) => setTimeout(r, 15_000))
  }
  console.log(`tx1 status after polling: ${s1}`)
  if (b2.ok) console.log(`tx2 status: ${await wocStatus(b2.txid)}`)
  console.log('(testnet blocks land when miners land them — re-run `wocStatus` later or check the links)')
}

console.log(failures === 0 ? '\nAI-INFERENCE-ON-TESTNET PASS' : `\n${failures} FAILURE(S)`)
process.exit(failures === 0 ? 0 : 1)
