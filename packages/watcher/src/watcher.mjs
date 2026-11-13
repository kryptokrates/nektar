// nektar-watcher — the 1-of-N honest challenger in the optimistic-claim
// security model (GOAL-v9 §4.5). Polls a claims registry, and for every
// OPEN claim (status=0, deadline not yet passed):
//   1. locates the L_ZKMem proof bundle in the proof store by its
//      committed receiptHash,
//   2. re-verifies the RISC Zero STARK off-chain (verifyZkm2 — free, ms),
//   3. checks the claim's committed journal fields (journalRoot/journalSize/
//      queryId) against the proven journal,
//   4. broadcasts `challenge` on ANY mismatch — the dispute game decides
//      on-chain; the challenger's bond is at stake only on a wrong call.
// Honest claims are left untouched. This is what makes "a false claim
// finalizes only if zero watchers act" a deployed guarantee, not paper.
import { PrivateKey, Transaction, Hash } from '@bsv/sdk'
import { RunarContract, LocalSigner, deserializeState } from 'runar-sdk'
import { NektarProvider } from '@nektar/anchor'
import { TeranodeBroadcaster, ReserveFunder, bootstrapReserve } from '@nektar/chain'
import { loadClaims, proofFile, markClaimSpent, registerClaim } from '@nektar/chain'
import { existsSync, readFileSync, readdirSync } from 'node:fs'
import { spawn } from 'node:child_process'
import { fileURLToPath } from 'node:url'

// The RISC Zero verifier is a Rust wasm-bindgen module — a panic inside it
// fail-fasts the host process (observed intermittently as exit 0xC0000409).
// It therefore runs in a dedicated worker process (verify-worker.mjs); this
// daemon only ever sees a JSON verdict on stdout. A crashed worker yields
// INCONCLUSIVE — never a challenge: only a confirmed-invalid verdict may
// risk the challenger bond, and an attacker must not be able to DoS the
// 1-of-N assumption with a verifier-crashing claim.
const VERIFY_WORKER = fileURLToPath(new URL('./verify-worker.mjs', import.meta.url))
const VERIFY_TIMEOUT_MS = 60000
function verifyProof(file) {
  return new Promise((resolve) => {
    const child = spawn(process.execPath, [VERIFY_WORKER, file], { stdio: ['ignore', 'pipe', 'pipe'] })
    let out = ''
    let err = ''
    const kill = setTimeout(() => {
      child.kill('SIGKILL')
      resolve({ inconclusive: 'timeout' })
    }, VERIFY_TIMEOUT_MS)
    child.stdout.on('data', (d) => {
      out += d
    })
    child.stderr.on('data', (d) => {
      err += d
    })
    child.on('error', (e) => {
      clearTimeout(kill)
      resolve({ inconclusive: `spawn: ${e.message}` })
    })
    child.on('close', (code) => {
      clearTimeout(kill)
      const line = out.trim().split('\n').pop() ?? ''
      try {
        resolve(JSON.parse(line))
      } catch {
        resolve({ inconclusive: `exit ${code}${err ? ' stderr: ' + err.slice(0, 120) : ''}` })
      }
    })
  })
}

const ARG = (k, d = null) => {
  const i = process.argv.indexOf('--' + k)
  return i >= 0 ? process.argv[i + 1] : d
}
const ONCE = process.argv.includes('--once')
const WATCH_MS = Number(ARG('watch-ms', '5000'))

function readEnv(p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = { ...readEnv(new URL('../../.env', import.meta.url)), ...readEnv(new URL('../../../.env', import.meta.url)) }
// quickstart creds: repo path locally, mounted file in containers (NEKTAR_QS_ENV)
const qsEnv = readEnv(process.env.NEKTAR_QS_ENV ?? new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.NEKTAR_REGTEST_WIF || localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing (watcher pays its own challenge bonds)')

const cfg = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER || qsEnv.rpc_user || 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS || qsEnv.rpc_pass || '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1',
  claimsFile: ARG('claims', '.state/watcher-claims.json'),
  proofDir: ARG('proof-dir', '.state/proofs'),
  stateFile: ARG('reserve', '.state/watcher-reserve.json')
}
const art = JSON.parse(readFileSync(new URL('../../contracts/NektarZkClaim.json', import.meta.url), 'utf8'))

const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json', Authorization: auth },
    body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }),
    // a hung RPC (stale port proxy, saturated rpc_max_clients) must not freeze
    // the tick forever — the deadline keeps the challenger live
    signal: AbortSignal.timeout(15_000)
  })
  const text = await r.text()
  let j
  try {
    j = JSON.parse(text)
  } catch {
    throw new Error(`rpc ${method}: HTTP ${r.status} — non-JSON response (bad credentials?): ${text.slice(0, 120)}`)
  }
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const reserve = new ReserveFunder(key, cfg.stateFile)
const provider = new NektarProvider(broadcaster, reserve, addr)
const signer = new LocalSigner(WIF)
const watcherPk = await signer.getPublicKey()
const watcherPkh = Buffer.from(Hash.hash160(Buffer.from(watcherPk, 'hex'))).toString('hex')

// fund the challenger: purge stale entries, rescan mature coinbases
// (gettxout is unimplemented on Teranode — local marks go stale)
for (const u of [...reserve.utxos]) reserve.markSpent(u.tx.id('hex'), u.vout)
const tipH = (await rpc('getblockchaininfo')).blocks
await reserve.scanCoinbases(cfg, rpc, Math.max(1, tipH - 150), tipH - 101)
if (reserve.utxos.length < 2) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: cfg.stateFile.replace('.json', '-treasury.json'), outputs: 8 })
}

// ── claim registry: [{txid, vout}] — each = a deployed NektarZkClaim UTXO.
// Seeded by deployments (the e2e writes entries); the watcher tracks the
// tip per entry while it stays unspent (chain-tracking beyond the tip
// needs a tx-index — documented gap, see GOAL-v9). ──
// deserialize the claim's state tail (codePart ‖ '6a' ‖ fields)
const readClaimState = (scriptHex) => {
  const tail = scriptHex.slice(art.script.length + 2) // skip '6a'
  return deserializeState(art.stateFields, tail)
}
const proofFileFor = (receiptHash) => proofFile(cfg.proofDir, receiptHash)

async function checkClaim(entry) {
  const { txid, vout } = entry
  const raw = await rpc('getrawtransaction', txid)
  const tx = Transaction.fromHex(raw)
  const out = tx.outputs[vout]
  if (!out) return console.log(`  ${txid.slice(0, 8)}:${vout} — no such output`)
  const script = out.lockingScript.toHex()
  if (!script.startsWith(art.script)) {
    if (process.env.NEKTAR_WATCH_DEBUG) console.log(`  ${txid.slice(0, 8)}:${vout} — not a NektarZkClaim script`)
    return
  }
  const st = readClaimState(script)
  if (st.status !== 0n) {
    if (process.env.NEKTAR_WATCH_DEBUG) console.log(`  ${txid.slice(0, 8)}:${vout} — status=${st.status.toString()}, skipping`)
    return
  }
  // deadline check: nLockTime is time-or-height; treat < 500M as height
  const tip = await rpc('getblockchaininfo')
    .then((b) => b.blocks)
    .catch(() => 0)
  const open = st.deadline < 500000000n ? tip < Number(st.deadline) : Math.floor(Date.now() / 1000) < Number(st.deadline)
  if (!open) return // window closed — settle will handle it

  // re-verify the proof off-chain — in a worker process (see top of file)
  const pf = proofFileFor(st.receiptHash)
  const verdict = existsSync(pf) ? await verifyProof(pf) : { ok: false, reason: 'no proof bundle in store' }
  if (verdict.inconclusive) {
    const n = (inconclusiveStreaks.get(txid) ?? 0) + 1
    inconclusiveStreaks.set(txid, n)
    return console.log(`  ${txid.slice(0, 8)}:${vout} — WARN verifier inconclusive (${verdict.inconclusive}), streak ${n} — no action`)
  }
  inconclusiveStreaks.delete(txid)
  const fieldsOk =
    verdict.ok && verdict.journal.root === st.journalRoot && verdict.journal.size === Number(st.journalSize) && verdict.journal.queryId === st.queryId
  if (fieldsOk) return console.log(`  ${txid.slice(0, 8)}:${vout} — claim verifies, no action`)
  console.log(`  ${txid.slice(0, 8)}:${vout} — FRAUD: ${verdict.ok ? 'journal fields mismatch' : verdict.reason} → challenging`)

  // broadcast challenge: status 0→1, lock challengerPkh, start the window
  const ctor = [
    st.status,
    st.receiptHash,
    st.journalRoot,
    st.journalSize,
    st.queryId,
    st.bondSats,
    st.deadline,
    st.responseDeadline,
    st.challengerPkh,
    st.bisectLo,
    st.bisectHi,
    st.midCommit,
    st.proverPkh,
    st.maxFee,
    st.window,
    st.stages,
    st.verdictCode,
    st.anchorOutpoint
  ]
  const c = new RunarContract(art, ctor)
  c.connect(provider, signer)
  c.currentUtxo = { txid, outputIndex: vout, satoshis: out.satoshis, script }
  const newState = { ...st, status: 1n, challengerPkh: watcherPkh, responseDeadline: st.window, bisectLo: 0n, bisectHi: st.stages }
  let res
  try {
    res = await c.call('challenge', [null, watcherPk], provider, signer, { newState })
    console.log(`  ✔ challenge broadcast for ${txid.slice(0, 8)}:${vout}`)
  } catch (e) {
    if (/UTXO_SPENT/.test(e.message)) {
      markClaimSpent(cfg.claimsFile, txid, vout)
      return console.log(`  ${txid.slice(0, 8)}:${vout} — outpoint already spent (likely challenged), marked`)
    }
    throw e
  }
  markClaimSpent(cfg.claimsFile, txid, vout) // consumed by our challenge tx
  // continuation tracking: the challenged claim lives on as the challenge
  // tx's output 0 — register it so the watcher keeps following the bisection
  // game instead of losing the covenant at its first spend
  if (res?.txid) registerClaim(cfg.claimsFile, { txid: res.txid, vout: 0 })
}

const inconclusiveStreaks = new Map() // txid → consecutive inconclusive verifications

let ticking = false // no overlapping ticks: a 60s verify outlives the 5s interval
async function tick() {
  if (ticking) return
  ticking = true
  try {
    const claims = loadClaims(cfg.claimsFile)
    if (process.env.NEKTAR_WATCH_DEBUG) console.log(`  tick: ${claims.length} registry entries`)
    for (const e of claims) {
      if (e.spent) continue
      try {
        await checkClaim(e)
      } catch (err) {
        console.log(`  ${e.txid?.slice(0, 8)}:${e.vout} — error: ${err.message.slice(0, 200)}`)
      }
    }
  } finally {
    ticking = false
  }
}
console.log(`nektar-watcher up — claims: ${cfg.claimsFile}, proofs: ${cfg.proofDir}, every ${WATCH_MS}ms${ONCE ? ' (once)' : ''}`)
await tick()
if (!ONCE) setInterval(() => void tick(), WATCH_MS)
