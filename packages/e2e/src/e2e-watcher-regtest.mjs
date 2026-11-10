// GOAL-v9 watcher e2e — the honest challenger running against live Teranode.
// Deploys a FRAUDULENT claim (committed journalRoot lies about the real
// proof's journal), seeds the watcher registry + proof store, runs the
// daemon once → the watcher must broadcast `challenge`.
import { PrivateKey, Transaction, LockingScript, P2PKH, Hash } from '@bsv/sdk'
import { RunarContract, LocalSigner } from 'runar-sdk'
import { NektarProvider } from '@nektar/anchor'
import { TeranodeBroadcaster, ReserveFunder, bootstrapReserve, utxoStatus } from '@nektar/chain'
import { verifyZkm2, encodeZkm2 } from '@nektar/zk'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import { execFileSync } from 'node:child_process'

let failures = 0
const check = (cond, name) => { console.log(`${cond ? '✔' : '✖'} ${name}`); if (!cond) failures++ }

function readEnv (p) {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
const localEnv = { ...readEnv(new URL('../../../.env', import.meta.url)), ...readEnv(new URL('../../../../.env', import.meta.url)) }
const qsEnv = readEnv(new URL('../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.NEKTAR_REGTEST_WIF ?? localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing')
const cfg = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
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
const reserve = new ReserveFunder(key, '.state/watcher-e2e-reserve.json')
const rec = await reserve.reconcile(async (t, v) => await utxoStatus(cfg, t, v))
const tip = (await rpc('getblockchaininfo')).blocks
const scanned = await reserve.scanCoinbases(cfg, rpc, Math.max(1, tip - 150), tip - 101)
console.log(`reserve: ${reserve.utxos.length} live (revived ${rec.revived}, pruned ${rec.pruned}) + scanned ${scanned} mature coinbases (tip ${tip})`)
if (reserve.utxos.length < 4) {
  await bootstrapReserve(cfg, key, reserve, rpc, { treasuryFile: '.state/watcher-e2e-treasury.json', outputs: 16 })
}
const provider = new NektarProvider(broadcaster, reserve, addr)
const signer = new LocalSigner(WIF)
const pubKeyHex = await signer.getPublicKey()
const opPkh = Buffer.from(Hash.hash160(Buffer.from(pubKeyHex, 'hex'))).toString('hex')

const art = JSON.parse(readFileSync(new URL('../../contracts/NektarZkClaim.json', import.meta.url), 'utf8'))
const vArt = JSON.parse(readFileSync(new URL('../../contracts/ClaimVerdict.json', import.meta.url), 'utf8'))
const VCR = vArt.script
// real RISC Zero proof bundle: encode the fixture into L_ZKMem bytes
const fx = JSON.parse(readFileSync(new URL('../../zk/test/fixtures/zkm2-n5i4.json', import.meta.url), 'utf8'))
const bundle = encodeZkm2({ imageId: Buffer.from(fx.imageId, 'hex'), root: Buffer.from(fx.journal.root, 'hex'), size: fx.journal.size, queryId: Buffer.from(fx.journal.queryId, 'hex'), receipt: Buffer.from(fx.receipt, 'hex') })
const receiptHash = Buffer.from(Hash.sha256(bundle)).toString('hex')
const PROOFS = fileURLToPath(new URL('../../../.state/watcher-proofs', import.meta.url))
mkdirSync(PROOFS, { recursive: true })
writeFileSync(`${PROOFS}/${receiptHash}.bin`, Buffer.from(bundle))

// FRAUDULENT claim: committed journalRoot LIES (real journal.root is the fixture's)
const LIE = 'ff'.repeat(32)
const st0 = { status: 0n, receiptHash, journalRoot: LIE, journalSize: BigInt(fx.journal.size), queryId: fx.journal.queryId, bondSats: 5000n, deadline: 100000n, responseDeadline: 0n, challengerPkh: '00'.repeat(32), bisectLo: 0n, bisectHi: 4n, midCommit: '00'.repeat(32), proverPkh: opPkh, maxFee: 1000n, window: 100n, stages: 4n, verdictCode: VCR, anchorOutpoint: '' }
const ctor = [st0.status, receiptHash, LIE, st0.journalSize, st0.queryId, st0.bondSats, st0.deadline, 0n, st0.challengerPkh, 0n, 4n, st0.midCommit, opPkh, 1000n, 100n, 4n, VCR, '']
const c = new RunarContract(art, ctor)
c.connect(provider, signer)
await c.deploy({ satoshis: 5000 })
const claimOp = c.currentUtxo
console.log(`fraudulent claim deployed ${claimOp.txid.slice(0, 12)}…:${claimOp.outputIndex} (deadline 100000, journalRoot=LIE)`)

// sanity: the proof itself verifies off-chain — the claim just lies about it
const v = verifyZkm2(bundle)
check(v.ok && v.journal.root !== LIE, `fixture verifies; claim's journalRoot is indeed a lie`)

// seed the watcher registry, run the daemon once
const CLAIMS = fileURLToPath(new URL('../../../.state/watcher-claims.json', import.meta.url))
writeFileSync(CLAIMS, JSON.stringify([{ txid: claimOp.txid, vout: claimOp.outputIndex }]))
// NEKTAR_SEED_ONLY=1: stop after seeding — used to hand the claim to an
// already-running daemon (e.g. the docker-compose watcher service) instead
// of spawning a one-shot watcher here.
if (process.env.NEKTAR_SEED_ONLY) { console.log('seeded only (NEKTAR_SEED_ONLY)'); process.exit(failures ? 1 : 0) }
const out = execFileSync('node', ['../../watcher/src/watcher.mjs', '--once', '--claims', CLAIMS, '--proof-dir', PROOFS], { cwd: new URL('.', import.meta.url), encoding: 'utf8', timeout: 120000 })
console.log(out.trim())
check(/challenge/i.test(out) || /FRAUD/i.test(out), 'watcher detected fraud and broadcast challenge')
// second pass: the challenged outpoint must NOT be retried — the watcher
// marks it spent in the registry instead of spamming doomed broadcasts.
const out2 = execFileSync('node', ['../../watcher/src/watcher.mjs', '--once', '--claims', CLAIMS, '--proof-dir', PROOFS], { cwd: new URL('.', import.meta.url), encoding: 'utf8', timeout: 120000 })
check(!/challenge broadcast/i.test(out2), 'second pass: spent outpoint skipped (no double challenge)')
process.exit(failures ? 1 : 0)
