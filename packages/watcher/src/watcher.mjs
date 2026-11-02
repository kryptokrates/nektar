// mamem-watcher — the 1-of-N honest challenger in the optimistic-claim
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
import { PrivateKey, Transaction, LockingScript, Hash } from '@bsv/sdk'
import { RunarContract, LocalSigner, deserializeState } from 'runar-sdk'
import { MamemProvider } from '@mamem/anchor'
import { TeranodeBroadcaster, ReserveFunder, bootstrapReserve } from '@mamem/chain'
import { verifyZkm2 } from '@mamem/zk'
import { loadClaims, proofFile } from '@mamem/chain'
import { existsSync, readFileSync, writeFileSync, readdirSync } from 'node:fs'

const ARG = (k, d = null) => { const i = process.argv.indexOf('--' + k); return i >= 0 ? process.argv[i + 1] : d }
const ONCE = process.argv.includes('--once')
const WATCH_MS = Number(ARG('watch-ms', '5000'))

function readEnv (p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = { ...readEnv(new URL('../../.env', import.meta.url)), ...readEnv(new URL('../../../.env', import.meta.url)) }
const qsEnv = readEnv(new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.MAMEM_REGTEST_WIF ?? localEnv.MAMEM_REGTEST_WIF
if (!WIF) throw new Error('MAMEM_REGTEST_WIF missing (watcher pays its own challenge bonds)')

const cfg = {
  rpcUrl: process.env.MAMEM_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.MAMEM_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.MAMEM_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.MAMEM_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.MAMEM_ASSET_URL ?? 'http://localhost:8090/api/v1',
  claimsFile: ARG('claims', '.state/watcher-claims.json'),
  proofDir: ARG('proof-dir', '.state/proofs'),
  stateFile: ARG('reserve', '.state/watcher-reserve.json')
}
const art = JSON.parse(readFileSync(new URL('../../contracts/MamemZkClaim.json', import.meta.url), 'utf8'))

const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
const rpc = async (method, ...params) => {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
const reserve = new ReserveFunder(key, cfg.stateFile)
const provider = new MamemProvider(broadcaster, reserve, addr)
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

// ── claim registry: [{txid, vout}] — each = a deployed MamemZkClaim UTXO.
// Seeded by deployments (the e2e writes entries); the watcher tracks the
// tip per entry while it stays unspent (chain-tracking beyond the tip
// needs a tx-index — documented gap, see GOAL-v9). ──
// deserialize the claim's state tail (codePart ‖ '6a' ‖ fields)
const readClaimState = scriptHex => {
  const tail = scriptHex.slice(art.script.length + 2) // skip '6a'
  return deserializeState(art.stateFields, tail)
}
const proofFileFor = receiptHash => proofFile(cfg.proofDir, receiptHash)

async function checkClaim (entry) {
  const { txid, vout } = entry
  const raw = await rpc('getrawtransaction', txid)
  const tx = Transaction.fromHex(raw)
  const out = tx.outputs[vout]
  if (!out) return console.log(`  ${txid.slice(0, 8)}:${vout} — no such output`)
  const script = out.lockingScript.toHex()
  if (!script.startsWith(art.script)) { if (process.env.MAMEM_WATCH_DEBUG) console.log(`  ${txid.slice(0, 8)}:${vout} — not a MamemZkClaim script`); return }
  const st = readClaimState(script)
  if (st.status !== 0n) { if (process.env.MAMEM_WATCH_DEBUG) console.log(`  ${txid.slice(0, 8)}:${vout} — status=${st.status}, skipping`); return }
  // deadline check: nLockTime is time-or-height; treat < 500M as height
  const tip = await rpc('getblockchaininfo').then(b => b.blocks).catch(() => 0)
  const open = st.deadline < 500000000n ? tip < Number(st.deadline) : Math.floor(Date.now() / 1000) < Number(st.deadline)
  if (!open) return // window closed — settle will handle it

  // re-verify the proof off-chain
  const pf = proofFileFor(st.receiptHash)
  let verdict = { ok: false, reason: 'no proof bundle in store' }
  if (existsSync(pf)) verdict = verifyZkm2(new Uint8Array(readFileSync(pf)))
  const fieldsOk = verdict.ok &&
    verdict.journal.root === st.journalRoot &&
    verdict.journal.size === Number(st.journalSize) &&
    verdict.journal.queryId === st.queryId
  if (fieldsOk) return console.log(`  ${txid.slice(0, 8)}:${vout} — claim verifies, no action`)
  console.log(`  ${txid.slice(0, 8)}:${vout} — FRAUD: ${verdict.ok ? 'journal fields mismatch' : verdict.reason} → challenging`)

  // broadcast challenge: status 0→1, lock challengerPkh, start the window
  const ctor = [st.status, st.receiptHash, st.journalRoot, st.journalSize, st.queryId, st.bondSats, st.deadline, st.responseDeadline, st.challengerPkh, st.bisectLo, st.bisectHi, st.midCommit, st.proverPkh, st.maxFee, st.window, st.stages, st.verdictCode, st.anchorOutpoint]
  const c = new RunarContract(art, ctor)
  c.connect(provider, signer)
  c.currentUtxo = { txid, outputIndex: vout, satoshis: out.satoshis, script }
  const newState = { ...st, status: 1n, challengerPkh: watcherPkh, responseDeadline: st.window, bisectLo: 0n, bisectHi: st.stages }
  await c.call('challenge', [null, watcherPk], provider, signer, { newState })
  console.log(`  ✔ challenge broadcast for ${txid.slice(0, 8)}:${vout}`)
}

async function tick () {
  const claims = loadClaims(cfg.claimsFile)
  if (process.env.MAMEM_WATCH_DEBUG) console.log(`  tick: ${claims.length} registry entries`)
  for (const e of claims) {
    try { await checkClaim(e) } catch (err) { console.log(`  ${e.txid?.slice(0, 8)}:${e.vout} — error: ${err.message.slice(0, 200)}`) }
  }
}
console.log(`mamem-watcher up — claims: ${cfg.claimsFile}, proofs: ${cfg.proofDir}, every ${WATCH_MS}ms${ONCE ? ' (once)' : ''}`)
await tick()
if (!ONCE) setInterval(tick, WATCH_MS)
