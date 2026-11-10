// Reduced live-regtest E2E (NEKTAR_NETWORK=regtest):
// fresh coinbase → split → NektarMemory story + correction → ~20 anchors →
// mine → overlay admit with the real ChainTracker → verifyFragment on real
// headers (BUMP via merklePathFor/block_legacy).
import { PrivateKey, Transaction, MerklePath } from '@bsv/sdk'
import { P2PKH } from '@bsv/sdk'
import { utf8, dsha256, toHex, hash160 } from '@nektar/core'
const dsha256utf8 = (s: string) => dsha256(utf8(s))
import { NektarMemory } from '@nektar/mcp'
import { createNektarOverlay } from '@nektar/overlay'
import { verifyFragment } from '@nektar/verify'
import { AnchorChain } from '@nektar/anchor'
import { decodeNektarAnchor } from '@nektar/contracts'
import { TeranodeBroadcaster, TeranodeRPCTracker, merklePathFor, merklePathAtHeight, parseLegacyBlock, type RegtestConfig } from '@nektar/chain'

let failures = 0
const check = (cond: boolean, name: string) => { console.log(`${cond ? '✔' : '✖'} ${name}`); if (!cond) failures++ }

// Load secrets from gitignored env files (never printed): nektar/.env holds
// NEKTAR_REGTEST_WIF; the teranode quickstart .env holds rpc_user/rpc_pass.
import { existsSync, readFileSync } from 'node:fs'
function readEnv (p: URL): Record<string, string> {
  const out: Record<string, string> = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]!] = m[2]!.trim()
  }
  return out
}
const localEnv = readEnv(new URL('../../../../.env', import.meta.url))
const qsEnv = readEnv(new URL('../../../../infra/teranode-quickstart/.env', import.meta.url))
const WIF = process.env.NEKTAR_REGTEST_WIF ?? localEnv.NEKTAR_REGTEST_WIF
if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing: set it in nektar/.env')
const cfg: RegtestConfig = {
  rpcUrl: process.env.NEKTAR_RPC_URL ?? 'http://localhost:9292/',
  rpcUser: process.env.NEKTAR_RPC_USER ?? qsEnv.rpc_user ?? 'teranode',
  rpcPass: process.env.NEKTAR_RPC_PASS ?? qsEnv.rpc_pass ?? '',
  propagationUrl: process.env.NEKTAR_PROPAGATION_URL ?? 'http://localhost:8833/tx',
  assetUrl: process.env.NEKTAR_ASSET_URL ?? 'http://localhost:8090/api/v1'
}
if (!cfg.rpcPass) throw new Error('rpc_pass missing: set it in infra/teranode-quickstart/.env or NEKTAR_RPC_PASS')
const tracker = new TeranodeRPCTracker(cfg)
const broadcaster = new TeranodeBroadcaster(cfg.propagationUrl)
const auth = 'Basic ' + Buffer.from(`${cfg.rpcUser}:${cfg.rpcPass}`).toString('base64')
async function rpc (method: string, ...params: unknown[]): Promise<any> {
  const r = await fetch(cfg.rpcUrl, { method: 'POST', headers: { 'Content-Type': 'application/json', Authorization: auth }, body: JSON.stringify({ jsonrpc: '1.0', id: 1, method, params }) })
  const j = await r.json() as any
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}
const key = PrivateKey.fromWif(WIF)
const addr = key.toPublicKey().toAddress([0x6f])
// fresh coinbase → funding (legacy block: tx[0] is the coinbase)
// funding: a treasury chain — exactly one "treasury" outpoint persisted in
// .state/reserve.json (an output we created ourselves, so it's known-unspent).
// Each run splits it into ~120 fresh 50k-sat outputs + a NEW treasury output,
// mines the split, and persists the new treasury immediately after acceptance.
// This run's reserve is only those fresh outputs — nothing from earlier runs.
import { ReserveFunder } from '@nektar/chain'
import { writeFileSync, mkdirSync, rmSync } from 'node:fs'
// Two separate files: the treasury (the ONLY state carried across runs) and
// the per-run reserve pool, which is recreated empty on every run so stale
// entries from earlier runs can never be handed out.
const TREASURY_FILE = '.state/treasury.json'
const RS_FILE = '.state/reserve-run.json'
mkdirSync('.state', { recursive: true })
rmSync(RS_FILE, { force: true })
const loadRS = (): { treasury?: { txid: string, vout: number, txHex: string } } => {
  try { return JSON.parse(readFileSync(TREASURY_FILE, 'utf8')) } catch { return {} }
}
const saveRS = (t: { txid: string, vout: number, txHex: string }) => {
  writeFileSync(TREASURY_FILE, JSON.stringify({ treasury: t }, null, 1))
}
/** Mine one block and return ITS height, read from the mined block's own
 *  header (tracker.currentHeight() can lag right after generatetoaddress). */
// getblockchaininfo.blocks lags generatetoaddress on Teranode; the mined
// block's own header height is authoritative for the tip we just made
let lastMinedHeight = 0
async function mineOne (): Promise<number> {
  const [hash] = await rpc('generatetoaddress', 1, addr) as string[]
  const hdr = await rpc('getblockheader', hash, true)
  if (typeof hdr?.height !== 'number') throw new Error(`getblockheader ${hash}: no height`)
  lastMinedHeight = Math.max(lastMinedHeight, hdr.height)
  return hdr.height
}
/** spend `u` into ~120×50k outputs + a new treasury output. Returns the split tx. */
async function splitTreasury (parentTx: Transaction, vout: number, sats: number): Promise<Transaction | null> {
  const split = new Transaction()
  split.addInput({ sourceTransaction: parentTx, sourceOutputIndex: vout, unlockingScriptTemplate: new P2PKH().unlock(key) })
  const per = Math.min(50_000, Math.floor((sats - 2000) / 121))
  for (let k = 0; k < 120; k++) split.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: per })
  split.addOutput({ lockingScript: new P2PKH().lock(addr), satoshis: sats - 120 * per - 1000 }) // new treasury
  await split.sign()
  // deterministic signing re-creates the SAME split from the same parent; if
  // the node already knows it, its outputs belong to an earlier run (likely
  // spent) — skip this candidate instead of trusting the idempotent "success"
  const known = await fetch(`${cfg.assetUrl}/tx/${split.id('hex')}`, { headers: { Authorization: auth } })
  if (known.ok) return null
  const r = await broadcaster.broadcast(split)
  return r.status === 'success' ? split : null
}
const reserve = new ReserveFunder(key, RS_FILE)
let treasuryTx: Transaction | null = null
{
  const rs = loadRS()
  if (rs.treasury) treasuryTx = Transaction.fromHex(rs.treasury.txHex)
  // attempt 1: spend the persisted treasury
  if (rs.treasury) {
    treasuryTx = Transaction.fromHex(rs.treasury.txHex)
    const split = await splitTreasury(treasuryTx, rs.treasury.vout, treasuryTx.outputs[rs.treasury.vout]!.satoshis ?? 0)
    if (split) {
      reserve.heights.set(split.id('hex'), await mineOne())
      saveRS({ txid: split.id('hex'), vout: 120, txHex: split.toHex() })
      for (let k = 0; k < 120; k++) reserve.addOwned(split, k)
      console.log(`treasury split: ${split.id('hex').slice(0, 12)}… → 120 fresh outputs`)
    }
  }
  // bootstrap/recovery: treasury missing or already spent → scan early
  // coinbases, one candidate at a time, until one split succeeds (≤100).
  if (reserve.utxos.length === 0) {
    console.log('treasury missing/spent — scanning early coinbases')
    const want = Buffer.from(key.toPublicKey().toHash()).toString('hex')
    let done = false
    for (let h = 1; h <= 800 && !done; h++) {
      try {
        const hash = await rpc('getblockhash', h)
        const res = await fetch(`${cfg.assetUrl}/block_legacy/${hash}`)
        if (!res.ok) break
        const { txs } = parseLegacyBlock(new Uint8Array(await res.arrayBuffer()))
        const cb = Transaction.fromHex(Buffer.from(txs[0]!).toString('hex'))
        for (const [v, o] of cb.outputs.entries()) {
          if (done || !o.lockingScript.toHex().includes(want) || (o.satoshis ?? 0) < 1e7) continue
          const split = await splitTreasury(cb, v, o.satoshis!)
          if (split) {
            reserve.heights.set(split.id('hex'), await mineOne())
            saveRS({ txid: split.id('hex'), vout: 120, txHex: split.toHex() })
            for (let k = 0; k < 120; k++) reserve.addOwned(split, k)
            console.log(`treasury bootstrapped from coinbase h=${h} → 120 outputs`)
            done = true
          }
        }
      } catch { break }
      if (h > 100) break // bounded: ≤100 candidates
    }
  }
}
const funder: any = {
  keyPair: key,
  get utxos () { return reserve.utxos },
  markSpent: (t: string, v: number) => reserve.markSpent(t, v),
  takeReserve: (sc?: unknown, min?: number) => reserve.takeReserve(sc as any, min)
}
const burstPool = reserve.utxos.slice(40) // own half of the pool

// real chain facade: broadcast→propagation, tracker→asset/rpc; stubs for the
// mock-only bits NektarMemory/overlay may touch
const chain: any = {
  broadcast: (tx: Transaction) => broadcaster.broadcast(tx),
  mempool: new Map(),
  mine: async () => { await rpc('generatetoaddress', 1, addr) },
  currentHeight: () => tracker.currentHeight(),
  isValidRootForHeight: (r: string, h: number) => tracker.isValidRootForHeight(r, h),
  headerAtHeight: (h: number) => tracker.headerAtHeight(h),
  tipHeight: async () => Math.max(lastMinedHeight, await tracker.currentHeight())
}
for (const u of reserve.utxos) chain.mempool.set(u.tx.id('hex'), u.tx)
// attach real BUMPs to the reserve's mined parents — BEEF verification
// rejects unmined coinbase spends (heights recorded at scan time)
{
  const seen = new Set<string>()
  for (const u of reserve.utxos) {
    const id = u.tx.id('hex')
    if (seen.has(id)) continue
    seen.add(id)
    const h = reserve.heights.get(id)
    if (!h) continue
    // the asset API indexes a just-mined block with a short lag — retry briefly,
    // and fail fast: an unproven funding parent breaks every BEEF built on it
    let mp = null as Awaited<ReturnType<typeof merklePathAtHeight>>
    for (let a = 0; a < 10 && !mp; a++) {
      mp = await merklePathAtHeight(cfg, id, tracker, h).catch(() => null)
      if (!mp) await new Promise(r => setTimeout(r, 300))
    }
    if (!mp) throw new Error(`funding parent ${id} has no merkle proof at height ${h}`)
    u.tx.merklePath = new MerklePath(mp.blockHeight, mp.path)
  }
}

// --- story + correction ---------------------------------------------------
console.log('— story + correction —')
// shard key = fund key: funding UTXOs are P2PKH-locked to `key`, and
// LocalSigner signs the funding inputs with the shard key
const shardKeys = new Map([['manual', key]])
const mem = new NektarMemory(chain, funder as any, key, { batchPolicy: { maxLeaves: 2 }, modelId: 'reg-e2e', shardKeys })
await mem.remember('manual', 'drain port 4 torque 45Nm')
await mem.anchorShard('manual')
const corr = await mem.remember('manual', 'CORRECTION: torque is 42Nm (supersedes seq 0)')
await mem.anchorShard('manual')
await rpc('generatetoaddress', 1, addr)
console.log('anchors broadcast + block mined')

// --- ~20 anchor burst -------------------------------------------------------
console.log('— 20-anchor burst —')
const bF: any = {
  keyPair: key,
  get utxos () { return reserve.utxos.slice(40) },
  markSpent: (t: string, v: number) => reserve.markSpent(t, v),
  takeReserve: (sc?: unknown, min?: number) => reserve.takeReserve(sc as any, min)
}
const burst = new AnchorChain('burst', key, bF as any, key, chain)
const t0 = Date.now()
const burstIds: string[] = []
for (let i = 0; i < 20; i++) {
  try {
    const r = await burst.anchor(1, dsha256utf8(`rb-${i}`)); burstIds.push(r.tx.id('hex'))
  } catch (e) {
    console.log(`burst FAIL at i=${i}: ${(e as Error).message.split('\n')[0]}`)
    throw e
  }
}
await rpc('generatetoaddress', 1, addr)
check(burstIds.length === 20, `20 contract anchors broadcast+mined (${Date.now() - t0}ms)`)
console.log('  burst genesis:', burstIds[0])

// --- owner rotation mid-chain on regtest ------------------------------------
console.log('— rotate —')
const newOwner = PrivateKey.fromRandom()
const rot = await burst.rotate(newOwner)
await rpc('generatetoaddress', 1, addr)
const rotDec = decodeNektarAnchor(rot.tx.outputs[0]!.lockingScript.toHex())!
check(rotDec.n === 19 && rotDec.shard === decodeNektarAnchor(burst.getHead()!.tx.outputs[0]!.lockingScript.toHex())!.shard,
  `rotate broadcast+mined: n=${rotDec.n} shard stable ${rotDec.shard.slice(0, 16)}`)

// --- overlay admit with the real tracker -----------------------------------
console.log('— overlay admit —')
const ov = await createNektarOverlay(tracker, chain)
const tails: Map<string, any[]> = (mem as any).tails
const txs: Map<string, Transaction> = (mem as any).anchorTxs
const prev: Record<string, string | undefined> = {}
for (const [shard, tail] of tails) {
  for (const link of [...tail].sort((a, b) => a.n - b.n)) {
    const tx = txs.get(link.txid)!
    const acFor = (mem as any).anchors.get(shard)
    // attach any missing parent txs (contract parents are unmined → the BEEF
    // needs the full source, not a txid+BUMP) then BUMP the mined ones
    const prevTxid = prev[shard]
    for (const [ix, i] of tx.inputs.entries()) {
      const ptxid = i.sourceTXID ?? i.sourceTransaction?.id('hex') ?? (ix === 0 ? prevTxid : undefined)
      if (!i.sourceTransaction && ptxid) {
        const e = (reserve as any).entries?.find?.((x: any) => x.txid === ptxid) ?? reserve.utxos.find(u => u.tx.id('hex') === ptxid)
        i.sourceTransaction = txs.get(ptxid) ?? (acFor as any).txById?.get(ptxid) ?? (acFor as any).provider?.taken?.get(ptxid) ?? (e?.tx ?? (e?.txHex ? Transaction.fromHex(e.txHex) : undefined))
      }
      const src = i.sourceTransaction
      if (!src) console.log(`  ${shard} n=${link.n} in${ix}: parent ${ptxid ?? '?'} unresolvable (txs=${!!txs.get(ptxid ?? '')}, txById=${!!(acFor as any).txById?.get(ptxid ?? '')}, taken=${!!(acFor as any).provider?.taken?.get(ptxid ?? '')}, reserve=${!!reserve.utxos.find(u => u.tx.id('hex') === ptxid)})`)
      if (src && !src.merklePath) {
        const h = reserve.heights.get(src.id('hex'))
        if (h) { const m = await merklePathAtHeight(cfg, src.id('hex'), tracker, h); if (m) src.merklePath = new MerklePath(m.blockHeight, m.path) }
      }
    }
    const s = await ov.engine.submit({ beef: acFor.beefOf(tx), topics: ['tm_nektar'] }, undefined, 'current-tx')
    check((s.tm_nektar?.outputsToAdmit?.length ?? 0) === 1, `engine admits ${shard} n=${link.n}`)
    prev[shard] = link.txid
  }
}

// --- verifyFragment on real headers -----------------------------------------
console.log('— verifyFragment (real headers) —')
// attach real BUMPs to every broadcast anchor tx (offline mine() does this).
// The asset API lags briefly indexing a just-mined block — retry a few times.
const txMap: Map<string, Transaction> = (mem as any).anchorTxs
await new Promise(r => setTimeout(r, 2500))
for (const [txid, tx] of txMap) {
  let mp0 = null
  for (let t = 0; t < 6 && !mp0; t++) {
    mp0 = await merklePathFor(cfg, txid, tracker).catch(() => null)
    if (!mp0) await new Promise(r => setTimeout(r, 1000))
  }
  if (mp0) tx.merklePath = new MerklePath(mp0.blockHeight, mp0.path)
}
const { result } = await mem.prove('manual', corr.seq)
check(result.ok === true, `correction proof verifies against real headers ${JSON.stringify(result)}`)
// SPV check on the burst chain too
const btx = burstIds[burstIds.length - 1]!
let mp = null as Awaited<ReturnType<typeof merklePathFor>>
for (let t = 0; t < 6 && !mp; t++) {
  mp = await merklePathFor(cfg, btx, tracker).catch(() => null)
  if (!mp) await new Promise(r => setTimeout(r, 1000))
}
check(!!mp, `BUMP built for ${btx.slice(0, 12)}… (block ${mp?.blockHeight ?? '?'})`)
if (mp) {
  const btxRaw = await rpc('getrawtransaction', btx)
  const btxObj = Transaction.fromHex(btxRaw)
  btxObj.merklePath = new MerklePath(mp.blockHeight, mp.path)
  check(await btxObj.merklePath.verify(btx, tracker as any), 'burst anchor SPV verified against real header')
}

// --- v2 scenario on regtest -------------------------------------------------
console.log('— v2 scenario (regtest) —')
{
  const { canonicalizePolicySet } = await import('@nektar/policy')
  const { signBinding, issueChallenge, answerChallenge, issueClearance, proveAccess, verifyAccess, Smt, issueDisclosure, verifyDisclosureAnchored, fieldRoot, fieldPath } = await import('@nektar/identity')
  const B = PrivateKey.fromRandom(); const pubB = B.toPublicKey().toString()
  const policyText = `permit(principal == Agent::"${toHex(hash160(Buffer.from(pubB, 'hex')))}", action, resource);`
  const ps = canonicalizePolicySet('v2r', policyText)
  const vm = new NektarMemory(chain, funder as any, key, {
    batchPolicy: { maxLeaves: 2 }, shardKeys: new Map([['v2r', key]]),
    policy: { policyText, anchoredPolicySetHash: ps.hash }, agentSeed: key, maxLevel: 2,
    clearanceAuthorities: [key.toPublicKey().toString()]
  })
  // A writes level-2 data
  await vm.remember('v2r', 'level-2 secret', { source: 'agent-a' }, 2)
  await vm.anchorShard('v2r'); await mineOne()
  // attach the real BUMP to the just-mined anchor + confirm it, like the
  // other mined-anchor paths in this file
  {
    const txid = (vm as any).tails.get('v2r')?.at(-1)?.txid
    let mp = null
    for (let t = 0; t < 20 && !mp; t++) { mp = await merklePathFor(cfg, txid, tracker).catch(() => null); if (!mp) await new Promise(r => setTimeout(r, 1500)) }
    const tx = (vm as any).anchorTxs.get(txid)
    if (mp && tx) { tx.merklePath = new MerklePath(mp.blockHeight, mp.path); (vm as any).confirmAnchor?.('v2r', txid) }
  }
  // B reads: verified identity + ZK clearance ≥2 + Cedar ALLOW
  const binding = signBinding(B, 'uuid-b', '00'.repeat(32), 0)
  const chal = vm.issueChallenge(); const csig = answerChallenge(B, chal)
  const iss = issueClearance(key, 2n, pubB)
  const tr = { queryId: 'q1', fragmentId: 'v2r/0', nonce: new Uint8Array(32).fill(9) }
  const proof = proveAccess(iss, 2n, 8, tr)!
  const ident = { pubkey: pubB, binding, challengeNonce: chal, challengeSig: csig }
  const rb = await vm.recall('v2r', { seq: 0, zk:  { commitment: iss.commitment, proof, transcript: tr, requiredLevel: 2n }, identity: ident })
  if (rb.items[0]?.text !== 'level-2 secret') console.log('  Brecall:', JSON.stringify(rb.items[0]).slice(0,220), JSON.stringify(rb.receipt).slice(0,120))
  check(rb.items[0]?.text === 'level-2 secret' && rb.receipt.zkaccess === 'verified' && rb.receipt.cedar === 'allow', 'regtest: B reads level-2 with identity+zk+cedar')
  // C denied: Cedar DENY (valid identity, not in policy)
  const C = PrivateKey.fromRandom()
  const cchal = vm.issueChallenge()
  const cident = { pubkey: C.toPublicKey().toString(), binding: signBinding(C, 'uuid-c', '00'.repeat(32), 0), challengeNonce: cchal, challengeSig: answerChallenge(C, cchal) }
  const dc = await vm.recall('v2r', { seq: 0, zk: { commitment: iss.commitment, proof, transcript: tr, requiredLevel: 2n }, identity: cident })
  check(dc.items[0]?.unverifiable === true && dc.receipt.cedar === 'deny', 'regtest: C denied by Cedar')
  // B without ZK → denied on level-2
  const dz = await vm.recall('v2r', { seq: 0, identity: ident })
  check(dz.items[0]?.unverifiable === true, 'regtest: B without ZK denied on level-2')
  // mid-chain rotate — earlier proofs still verify; capture the rotate txid
  const newOwner = PrivateKey.fromRandom()
  const ac2 = (vm as any).anchors.get('v2r')
  const rot = await ac2.rotate(newOwner)
  await mineOne()
  const ROT_TXID = rot.tx.id('hex')
  check(Number(rot.head.data.n) === 0, `regtest: rotate n=${rot.head.data.n} txid=${ROT_TXID.slice(0, 12)}…`)
  check(verifyAccess(iss.commitment, 2n, proof, tr) === true, 'regtest: ZK proof still verifies after rotate')
  // revoke old key in SMT, anchor the root on regtest, non-revocation for new key
  const smt = new Smt()
  smt.insert(dsha256(Buffer.from(key.toPublicKey().toString())), new Uint8Array(32).fill(1))
  const rootRev = smt.root()
  await vm.remember('__rev__', `root:${Buffer.from(rootRev).toString('hex')}`)
  const ra = await vm.anchorShard('__rev__')
  await mineOne()
  const REV_TXID = (vm as any).tails.get('__rev__')?.at(-1)?.txid ?? ra?.txid ?? 'unknown'
  check(REV_TXID !== 'unknown', `regtest: revocation root anchored txid=${String(REV_TXID).slice(0, 12)}…`)
  const nip = smt.prove(dsha256(Buffer.from(newOwner.toPublicKey().toString())))
  const { verifyProof: vsp } = await import('@nektar/identity')
  check(vsp(nip, { present: false }, rootRev) === true, 'regtest: new key non-inclusion vs anchored root')
  // auditor envelope on the anchored 'v2r' fragment: verify, then expiry fails
  const fld = [{ name: 'secret', value: utf8('level-2 secret'), salt: new Uint8Array(32).fill(3) }]
  const exp = Math.floor(Date.now() / 1000) + 60
  const env = issueDisclosure(key, fld, 0, C.toPublicKey().toString(), exp, 'audit')
  const fp = fieldPath(fld, 0)
  const verifyAnchor = async () => {
    for (let t = 0; t < 10; t++) {
      // re-attach the BUMP if the anchor tx lacks one (asset index lag)
      const txid = (vm as any).tails.get('v2r')?.find?.(() => true)?.txid ?? (vm as any).tails.get('v2r')?.at(-1)?.txid
      const atx = (vm as any).anchorTxs.get(txid)
      if (atx && !atx.merklePath) {
        const mp = await merklePathFor(cfg, txid, tracker).catch(() => null)
        if (mp) { atx.merklePath = new MerklePath(mp.blockHeight, mp.path); (vm as any).confirmAnchor?.('v2r', txid) }
      }
      const pr = await vm.prove('v2r', 0)
      if (pr.result.ok === true) return true
      await new Promise(r => setTimeout(r, 800))
    }
    return false
  }
  const now = Math.floor(Date.now() / 1000)
  const okD = await verifyDisclosureAnchored(env, key.toPublicKey().toString(), C.toPublicKey().toString(), fld.length, fieldRoot(fld), fp.siblings ?? fp, now, verifyAnchor)
  check(okD.ok === true, 'regtest: auditor envelope verifies anchored')
  const badD = await verifyDisclosureAnchored(env, key.toPublicKey().toString(), C.toPublicKey().toString(), fld.length, fieldRoot(fld), fp.siblings ?? fp, exp + 61, verifyAnchor)
  check(badD.ok === false, 'regtest: auditor envelope fails after expiry')
  // disclosure-event anchor txid (anchored via the __rev__ shard pattern — the
  // envelope itself is off-chain; the anchor carrying it is the v2r fragment's)
  const DIS_TXID = (vm as any).tails.get('v2r')?.at(-1)?.txid ?? 'unknown'
  console.log(`  TXIDS rotate=${ROT_TXID} revoke=${REV_TXID} disclose=${DIS_TXID}`)

  // --- P10: ZK membership on a live-anchored regtest batch + transcript leaf ---
  console.log('— P10 zk membership (regtest, STARK) —')
  if (process.env.NEKTAR_E2E_STARK === '0') console.log('  (skipped: NEKTAR_E2E_STARK=0)')
  else try {
    await vm.remember('zkm', 'p10 regtest frag 0', {}, 1)
    await vm.remember('zkm', 'p10 regtest frag 1', {}, 1)
    const za = await vm.anchorShard('zkm')
    await mineOne()
    const ZKM_TXID = za?.txid ?? vm.tailLink('zkm')?.txid ?? 'unknown'
    check(ZKM_TXID !== 'unknown', `regtest: zkm batch anchored txid=${String(ZKM_TXID).slice(0, 12)}…`)
    // attach the BUMP like verifyAnchor does — asset index lags behind mining
    const atx = vm.anchorTx(ZKM_TXID)
    if (atx && !atx.merklePath) {
      for (let t = 0; t < 20 && !atx.merklePath; t++) {
        const mp = await merklePathFor(cfg, ZKM_TXID, tracker).catch(() => null)
        if (mp) { atx.merklePath = new MerklePath(mp.blockHeight, mp.path); vm.confirmAnchor('zkm', ZKM_TXID) }
        else await new Promise(r => setTimeout(r, 1500))
      }
    }
    const q = vm.issueZkQuery('regtest-verifier')
    const { bytes, ms } = await vm.proveMembership('zkm', 1, q.queryId)
    const res = await vm.verifyMembership('zkm', 1, bytes)
    check(res.ok === true, `regtest: STARK membership (${bytes.length} B, ${ms} ms) verifies vs the live SHA-256 anchor (${res.reason ?? 'ok'})`)
    const replay = await vm.verifyMembership('zkm', 1, bytes)
    check(replay.ok === false, `regtest: replay rejected (${replay.reason})`)
    console.log(`  TXIDS zkm-anchor=${ZKM_TXID}`)
  } catch (e) {
    check(false, `regtest P10 zk block: ${String((e as Error).message).slice(0, 160)}`)
  }
}

// — v4 AORG / policy DAG on regtest —
console.log('— v4 AORG / policy DAG (regtest) —')
{
  try {
    const { AORGraph, GraphAnchor, PolicyDAG, instanceOf, delegate, verifyInstance } = await import('@nektar/graph')
    const { parseProgram } = await import('@nektar/datalog')
    const cat = parseProgram(`@id r-spec
spec(S, V) :- requires(P, V), applies(S, P).`).rules
    const g = new AORGraph(cat)
    const ep = { kind: 'episode' as const, sigma: [] as string[], context: ['manual'] }
    const e1 = g.append({ ...ep, phi: 'requires(torque, 45)', t: { logPos: 1 } }, [])
    const e2 = g.append({ ...ep, phi: 'applies(port4, torque)', t: { logPos: 2 } }, [])
    const d45 = g.append({ kind: 'derived', phi: 'spec(port4, 45)', t: { logPos: 3 }, context: ['manual'], sigma: [] }, [e1.h, e2.h], 'r-spec')
    const e3 = g.append({ ...ep, phi: 'requires(torque, 55)', t: { logPos: 4 } }, [])
    const rf = g.append({ kind: 'refinement', phi: 'spec(port4, 55)', t: { logPos: 5 }, context: ['manual'], sigma: [], refines: d45.h }, [e3.h, e2.h], 'r-spec')
    check(!g.currentView(6).some(v => v.phi === 'spec(port4, 45)'), 'regtest: refinement changes currentView')
    // anchor graph + policy roots on the real chain (fresh memory on the shared chain/funder)
    const v4m = new NektarMemory(chain, funder as any, key, { batchPolicy: { maxLeaves: 10 } })
    const ga = new GraphAnchor(v4m as any, 'aorg')
    for (const h of [e1.h, e2.h, e3.h, d45.h, rf.h]) await ga.record(h)
    const gtx = await ga.anchor(); await mineOne()
    const atx = ga.proveVertex(d45.h).anchorTx
    if (atx && !atx.merklePath) {
      for (let t = 0; t < 20 && !atx.merklePath; t++) {
        const mp = await merklePathFor(cfg, gtx, tracker).catch(() => null)
        if (mp) { atx.merklePath = new MerklePath(mp.blockHeight, mp.path); v4m.confirmAnchor('aorg', gtx) }
        else await new Promise(r => setTimeout(r, 1500))
      }
    }
    const GRAPH_TXID = gtx
    check(GRAPH_TXID !== 'unknown' && await ga.verifyVertex(d45.h), `regtest: graph root anchored txid=${String(GRAPH_TXID).slice(0, 12)}…`)
    const dag = new PolicyDAG([{ name: 'org', rules: parseProgram(`@id pol
permit(P, read, R) :- member(P, verified), fragment(R).`).rules }])
    const proot = Buffer.from(dag.policyRoot()).toString('hex')
    await v4m.remember('pol', `policyroot:${proot}`)
    const pa = await v4m.anchorShard('pol'); await mineOne()
    const POL_TXID = pa?.txid ?? 'unknown'
    check(POL_TXID !== 'unknown', `regtest: policy root anchored txid=${String(POL_TXID).slice(0, 12)}…`)
    console.log(`  TXIDS graph-anchor=${GRAPH_TXID} policy-root-anchor=${POL_TXID}`)
    // delegated instance verified by a third party
    const inst = instanceOf(g, ['manual'])
    const dk = PrivateKey.fromRandom(); const expiry = Date.now() + 60000
    const anchored = new Map([...inst.members].map(h => [h, { anchorTxid: GRAPH_TXID }]))
    const vi = verifyInstance(g, inst, anchored as any, { pub: dk.toPublicKey(), expiry, sig: delegate(dk, inst, expiry) })
    check(vi.ok, 'regtest: delegated instance verifies')
  } catch (e) {
    check(false, `regtest v4 block: ${String((e as Error).message).slice(0, 160)}`)
  }
}

// — P14 Merkle Automaton on regtest: Teranode/Chronicle enforces δ —
console.log('— P14 Merkle Automaton (regtest) —')
{
  try {
    const { NEKTAR_AUTOMATON_ARTIFACT } = await import('@nektar/automaton')
    const { automatonCosts } = await import('./automaton-costs.js')
    const def = { Q: ['draft', 'review', 'published'], Sigma: ['submit', 'approve'], delta: [[0, 0, 1], [1, 1, 2]] as [number, number, number][], q0: 0, F: [2], O: ['ok'] }
    const out = Buffer.from(dsha256(utf8('regtest manual published'))).toString('hex')
    const am = new NektarMemory(chain, funder as any, key, { batchPolicy: { maxLeaves: 10 } })
    const d = await am.automatonDeploy('lifecycle', def)
    const ac = am.automaton('lifecycle')
    const s1 = await ac.step(0, 1, out)
    // an off-δ transition built without local guards: Teranode must reject it
    ac.unguarded = true
    let rejected = ''
    try { await ac.halt({ asState: 2 }) } catch (e) { rejected = (e as Error).message }
    ac.unguarded = false
    check(rejected.length > 0 && ac.getHead()!.state.n === 1, `regtest: Teranode rejects halt from non-final state (${rejected.slice(0, 60)})`)
    const s2 = await ac.step(1, 2, out)
    const h = await ac.halt()
    await mineOne()
    check(h.state.halted && h.state.q === 2, 'regtest: automaton draft→review→published, halted on-chain')
    const c = automatonCosts(NEKTAR_AUTOMATON_ARTIFACT, s2.tx)
    console.log(`  AUT-009 lockingScript=${c.scriptBytes}B stepTx=${c.txBytes}B fee=${c.feeSats}sat`)
    console.log(`  TXIDS automaton-deploy=${d.tx.id('hex')} step1=${s1.tx.id('hex')} step2=${s2.tx.id('hex')} halt=${h.tx.id('hex')}`)
  } catch (e) {
    check(false, `regtest P14 block: ${String((e as Error).message).slice(0, 160)}`)
  }
}

// — full paper scenario on regtest (same code as offline; real headers/BUMPs) —
console.log('— full paper scenario (regtest) —')
{
  try {
    const { runFullPaper } = await import('./full-paper.js')
    const txids = await runFullPaper({
      chain, funder: funder as any, fundKey: key, check, label: 'regtest paper',
      settle: async (m, shards) => {
        await mineOne()
        // every not-yet-proven anchor of the shard (batches also close
        // automatically mid-flow), not only the latest one
        for (const s of shards) {
          for (const l of m.anchorLinks(s)) {
            const atx = m.anchorTx(l.txid)
            for (let t = 0; t < 20 && atx && !atx.merklePath; t++) {
              const mp = await merklePathFor(cfg, l.txid, tracker).catch(() => null)
              if (mp) atx.merklePath = new MerklePath(mp.blockHeight, mp.path)
              else await new Promise(r => setTimeout(r, 500))
            }
            if (atx && !atx.merklePath) throw new Error(`settle: no BUMP for ${s} anchor ${l.txid.slice(0, 12)}…`)
          }
          const last = m.tailLink(s); if (last) m.confirmAnchor(s, last.txid)
        }
      }
    })
    console.log(`  TXIDS ${Object.entries(txids).map(([k, v]) => `${k}=${v}`).join(' ')}`)
  } catch (e) {
    check(false, `regtest full paper block: ${String((e as Error).stack ?? e).split('\n').slice(0, 3).join(' | ')}`)
  }
}

// — GOAL-v4 production block on regtest —
console.log('— v4 production block (regtest) —')
{
  try {
    const { runV4Block } = await import('./v4-block.js')
    const settleV4 = async (m: InstanceType<typeof NektarMemory>, shards: string[]) => {
      await mineOne()
      for (const s of shards) {
        for (const l of m.anchorLinks(s)) {
          const atx = m.anchorTx(l.txid)
          for (let t = 0; t < 20 && atx && !atx.merklePath; t++) {
            const mp = await merklePathFor(cfg, l.txid, tracker).catch(() => null)
            if (mp) atx.merklePath = new MerklePath(mp.blockHeight, mp.path)
            else await new Promise(r => setTimeout(r, 500))
          }
          if (atx && !atx.merklePath) throw new Error(`settle: no BUMP for ${s} anchor ${l.txid.slice(0, 12)}…`)
        }
        const last = m.tailLink(s); if (last) m.confirmAnchor(s, last.txid)
      }
    }
    const tx = await runV4Block({ chain, funder: funder as any, fundKey: key, check, label: 'regtest v4', settle: settleV4, mineMore: async n => { for (let i = 0; i < n; i++) await mineOne() } })
    console.log(`  TXIDS ${Object.entries(tx).map(([k, v]) => `${k}=${v}`).join(' ')}`)
  } catch (e) {
    check(false, `regtest v4 block: ${String((e as Error).stack ?? e).split('\n').slice(0, 3).join(' | ')}`)
  }
}

await ov.knex.destroy()
console.log(failures === 0 ? '\nREGTEST E2E PASS' : `\nREGTEST E2E FAIL: ${failures} checks failed`)
process.exit(failures === 0 ? 0 : 1)
