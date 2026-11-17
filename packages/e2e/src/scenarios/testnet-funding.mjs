// testnet-funding.mjs — live-net agent provisioning for real-stack.mjs:
// treasury-split → atomic-BEEF funding payload, extracted so live-ops tools
// can reuse the machinery without importing the whole scenario.
//
// Two testnet-specific disciplines live here:
//  - the split accumulates top treasury outpoints as inputs (no single UTXO
//    covers ~150k/agent runway) — each candidate is pre-filtered against a
//    first-party spend ledger (spentFile) because WoC keeps listing our own
//    prior splits' inputs as unspent for minutes after the mempool conflict
//  - the funding payload ships Atomic BEEF (split + every parent with its
//    BUMP where mined) so the wallet's internalize never walks ancestry
import { mkdirSync, rmSync, writeFileSync, readFileSync, existsSync } from 'node:fs'
import { join } from 'node:path'
import { setTimeout as delay } from 'node:timers/promises'
import { PrivateKey, Transaction, P2PKH, SatoshisPerKilobyte, Beef } from '@bsv/sdk'
import { merklePathMainnet } from '../../../chain/dist/src/mainnet.js'

// agents: [{name, key?, dataDir?, pass?, fundHex?}] — key/dataDir/fundHex are
// written back. treasury = { addr, key }. testnet = { wocUrl, broadcasterUrl }.
// spentFile: JSON map outpoint→splitTxid of inputs OUR accepted splits
// consumed — survives the agentsDir wipe, filters the stale WoC listings.
export async function fundAgentsLive({ agents, reuse, agentsDir, treasury, testnet, broadcaster, spentFile, log }) {
  if (reuse) {
    const keys = JSON.parse(readFileSync(join(agentsDir, 'agent-keys.json'), 'utf8'))
    const passFile = join(agentsDir, 'agent-pass.json')
    const pass = existsSync(passFile) ? JSON.parse(readFileSync(passFile, 'utf8')) : {}
    for (const a of agents) {
      a.key = PrivateKey.fromWif(keys[a.name])
      a.dataDir = join(agentsDir, a.name)
      a.pass = pass[a.name]
    }
    log(`reusing provisioned agents in ${agentsDir}`)
    return
  }
  rmSync(agentsDir, { recursive: true, force: true })
  mkdirSync(agentsDir, { recursive: true })
  // WoC lists a fresh unconfirmed output a few seconds before it serves its hex
  const woc = async (path) => {
    for (let i = 0; ; i++) {
      const r = await fetch(`${testnet.wocUrl}${path}`, { signal: AbortSignal.timeout(20_000) })
      if (r.ok) return r
      if (i >= 8) throw new Error(`woc ${path}: ${r.status}`)
      await delay(3000)
    }
  }
  const conf = await (await woc(`/address/${treasury.addr}/unspent`)).json()
  const unconf = (await (await woc(`/address/${treasury.addr}/unconfirmed/unspent`)).json()).result ?? []
  // the first-party ledger: an aborted run can leave one of our own prior
  // splits holding an outpoint WoC still lists — the broadcaster accepts the
  // submit then REJECTS once the mempool conflict resolves. Filtering known-
  // spent inputs up front skips those rejection rounds entirely; the retry
  // loop below still covers a stale record (it's a hint, not correctness).
  const spent = existsSync(spentFile) ? JSON.parse(readFileSync(spentFile, 'utf8')) : {}
  const candidates = [...conf.sort((x, y) => y.value - x.value), ...unconf.sort((x, y) => y.value - x.value)].filter(
    (c) => spent[`${c.tx_hash}:${c.tx_pos}`] === undefined
  )
  const skipped = conf.length + unconf.length - candidates.length
  if (skipped > 0) log(`dropped ${skipped} treasury candidate(s) our prior splits already spent`)
  // 150k/agent runway: the lane stock model pre-mints ~8 trees (fee+pad each)
  // plus the deal's escrow/disclosure/collect — 60k starved the finger publish
  // below a 5k take. No single treasury UTXO covers it (~297k max), so the
  // split accumulates top outpoints as inputs until the need is met.
  const per = 25_000
  const outs = 6
  const need = agents.length * per * outs + 10_000
  for (const a of agents) a.key = PrivateKey.fromRandom()
  writeFileSync(join(agentsDir, 'agent-keys.json'), JSON.stringify(Object.fromEntries(agents.map((a) => [a.name, a.key.toWif([0xef])]))), { mode: 0o600 })
  // confirmed outpoints first: an unconfirmed input's ancestors must all
  // resolve at internalize time, and a stale unconfirmed ancestor can be
  // unservable forever. Unconfirmed candidates are a coverage fallback only.
  for (let skip = 0; skip < candidates.length; skip++) {
    const inputs = []
    let covered = 0
    for (const c of candidates.slice(skip)) {
      inputs.push(c)
      covered += c.value
      if (covered >= need) break
    }
    if (covered < need) break
    const split = new Transaction()
    const parents = []
    for (const c of inputs) {
      const parent = Transaction.fromHex((await (await woc(`/tx/${c.tx_hash}/hex`)).text()).trim())
      // attach the parent's BUMP where it's already mined — a proven parent
      // anchors the BEEF closure so the wallet's fixpoint never has to walk
      // (and fetch) deeper ancestry
      if (parent.merklePath === undefined) {
        const mp = await merklePathMainnet(testnet, c.tx_hash).catch(() => null)
        if (mp?.merklePath !== undefined) parent.merklePath = mp.merklePath
      }
      parents.push(parent)
      split.addInput({ sourceTransaction: parent, sourceOutputIndex: c.tx_pos, unlockingScriptTemplate: new P2PKH().unlock(treasury.key) })
    }
    for (const a of agents)
      for (let i = 0; i < outs; i++) split.addOutput({ lockingScript: new P2PKH().lock(a.key.toPublicKey().toAddress([0x6f])), satoshis: per })
    split.addOutput({ lockingScript: new P2PKH().lock(treasury.addr), change: true })
    await split.fee(new SatoshisPerKilobyte(100))
    await split.sign()
    const r = await broadcaster.broadcast(split)
    if (r.status !== 'success') {
      log(`treasury split refused (${r.description}) — next candidate`)
      continue
    }
    let rejected = ''
    for (let i = 0; i < 3 && rejected === ''; i++) {
      await delay(5000)
      const st = await fetch(`${testnet.broadcasterUrl}/tx/${split.id('hex')}`, { signal: AbortSignal.timeout(10_000) })
        .then((x) => (x.ok ? x.json() : null))
        .catch(() => null)
      if (st?.txStatus === 'REJECTED') rejected = st.extraInfo ?? ''
    }
    if (rejected !== '') {
      log(`split ${split.id('hex').slice(0, 12)}… rejected: ${rejected.slice(0, 100)} — next candidate`)
      continue
    }
    // only an accepted split's inputs go in the ledger — a rejected tx may
    // not have consumed them (the conflict winner isn't knowable here)
    for (const c of inputs) spent[`${c.tx_hash}:${c.tx_pos}`] = split.id('hex')
    writeFileSync(spentFile, JSON.stringify(spent, null, 1))
    // the funding payload must carry its ancestry: a raw multi-input tx
    // leaves the wallet's internalize with no source tx for each input.
    // BEEF ships the split + every parent inline, self-contained (same
    // reason the regtest branch hands beef, not raw hex).
    const beef = new Beef()
    beef.mergeTransaction(split)
    for (const p of parents) beef.mergeTransaction(p)
    const fundHex = Buffer.from(beef.toBinaryAtomic(split.id('hex'))).toString('hex')
    for (const a of agents) {
      a.dataDir = join(agentsDir, a.name)
      mkdirSync(a.dataDir, { recursive: true })
      a.fundHex = fundHex
    }
    log(`treasury split ${split.id('hex')} (${split.toBinary().length} bytes, fee ${split.getFee()} sats) → ${agents.length}×${outs}×${per} sats`)
    return split.id('hex')
  }
  throw new Error(`testnet treasury ${treasury.addr} underfunded or every candidate UTXO contested`)
}
