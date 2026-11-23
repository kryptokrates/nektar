// rtsupport.ts — openRuntime boot plumbing: key files, instance/fund-key
// locks, the records db opener, the tx-store persister, the broadcast log
// hook and the ledger proof-heal. No chain or custody decisions live here.
import { existsSync, readFileSync, writeFileSync, appendFileSync, rmSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { detached, nektarEnv, nektarEnvNum, mkLog, nap, sparse } from '@nektar/core'
import { TxStore, type ChainPort, type Funds, type TxStatusEvent, type ChainEvents, type BroadcastResult } from '@nektar/chain'
import { executor, cronObject, legObject, EXEC_SVC, RT_OPTS_HEAVY, type InvokeTarget } from '@nektar/automation'
import { KnexShardStore, type ShardStore } from '@nektar/log'
import { readKeyFile, writeKeyFile, fundKeyFromMnemonic } from './keystore.js'
import { watcherObservation } from './txops.js'
import type { NektarMemory } from './memory.js'

interface KeyFile {
  agentSeed: string
  logKey: string
  shardKeys: Record<string, string>
}

/** state of the local BRC-100 substrate (BSV Desktop's JSON API on :3321).
 *  WalletClient('auto') collapses locked-vault and no-service into the same
 *  "Install a BSV wallet today!" error — custody setup probes here first so
 *  the operator gets the true reason. 'live' = the API answered at all (a
 *  locked vault answers 503 with code WALLET_LOCKED — that IS a live wallet). */
export type SubstrateState = 'live' | 'locked' | 'absent'
export async function probeWalletSubstrate(fetchImpl: typeof fetch = fetch): Promise<SubstrateState> {
  try {
    const r = await fetchImpl('http://localhost:3321/getVersion', {
      method: 'POST',
      // the JSON API refuses requests with no Origin header (HTTP 400)
      headers: { 'content-type': 'application/json', origin: 'http://localhost' },
      body: '{}'
    })
    const body = (await r.json().catch(() => ({}))) as { code?: string; description?: string }
    if (body.code === 'WALLET_LOCKED' || (body.description ?? '').includes('locked')) return 'locked'
    return 'live'
  } catch {
    return 'absent'
  }
}
/** keys are generated once into the data dir (0600) unless supplied; never logged.
 *  Encrypted at rest when a passphrase is set; mandatory on main.
 *  `create=false` (external-wallet custody): an absent file is NOT generated —
 *  the wallet covers seed/log/shard custody; an existing file is still read
 *  for legacy shard owners and old records. */
export function loadKeys(dataDir: string, pass: string | undefined, requireEnc = false, create = true): { keys: KeyFile; save: () => void } {
  const file = join(dataDir, 'keys.json')
  if (!existsSync(file) && !create) return { keys: { agentSeed: '', logKey: '', shardKeys: {} }, save: () => undefined }
  const keys: KeyFile = existsSync(file)
    ? (JSON.parse(readKeyFile(file, pass, requireEnc)) as KeyFile)
    : { agentSeed: PrivateKey.fromRandom().toHex(), logKey: PrivateKey.fromRandom().toHex(), shardKeys: {} }
  const save = () => writeKeyFile(file, JSON.stringify(keys), pass)
  if (!existsSync(file)) save()
  return { keys, save }
}

/** operator fee key (pays all anchor fees): NEKTAR_FUND_WIF wins; otherwise a
 *  dedicated fee.key.json generated once in the data dir — encrypted on main. */
export function loadFeeKey(dataDir: string, wif: string | undefined, pass: string | undefined, requireEnc: boolean): PrivateKey {
  if (wif) return PrivateKey.fromWif(wif)
  // NEKTAR_FUND_MNEMONIC (or _FILE): BIP-39 custody-of-record — recoverable,
  // one string to protect. Derived at m/44'/236'/0'/0/0; feeds the same
  // keyDeriver the WIF path uses.
  const mn =
    nektarEnv('NEKTAR_FUND_MNEMONIC') ??
    (existsSync(nektarEnv('NEKTAR_FUND_MNEMONIC_FILE') ?? '') ? readFileSync(nektarEnv('NEKTAR_FUND_MNEMONIC_FILE')!, 'utf8').trim() : undefined)
  if (mn) return fundKeyFromMnemonic(mn)
  const file = join(dataDir, 'fee.key.json')
  if (existsSync(file)) {
    const j = JSON.parse(readKeyFile(file, pass, requireEnc)) as { wif: string }
    return PrivateKey.fromWif(j.wif)
  }
  const key = PrivateKey.fromRandom()
  writeKeyFile(file, JSON.stringify({ wif: key.toWif() }), pass)
  return key
}

// NEKTAR_TX_LOG: when set to a file path, append one JSON line per SUCCESSFUL
// broadcast — {t, txid, bytes, inSats, outSats, feeSats} — so operator cost
// accounting comes from the wire, not from estimates. inSats/feeSats are null
// when a source transaction is missing. Write failures are ignored; no
// behaviour change otherwise.
export function txLogBroadcast<B extends (tx: Transaction) => Promise<import('@nektar/chain').BroadcastResult>>(
  inner: B,
  logPath: string | undefined = nektarEnv('NEKTAR_TX_LOG')
): B {
  if (logPath === undefined || logPath === '') return inner
  return (async (tx: Transaction) => {
    const r = await inner(tx)
    if (r.status === 'success') {
      try {
        let inSats: number | null = 0
        for (const i of tx.inputs) {
          const src = i.sourceTransaction?.outputs[i.sourceOutputIndex]
          if (src?.satoshis === undefined) {
            inSats = null
            break
          }
          inSats += src.satoshis
        }
        const outSats = tx.outputs.reduce((s, o) => s + (o.satoshis ?? 0), 0)
        appendFileSync(
          logPath,
          JSON.stringify({
            t: Date.now(),
            txid: tx.id('hex'),
            bytes: tx.toBinary().length,
            inSats,
            outSats,
            feeSats: inSats === null ? null : inSats - outSats
          }) + '\n'
        )
      } catch {
        /* logging must never break a broadcast */
      }
    }
    return r
  }) as B
}

/** single holder per lock file. Acquisition has two shapes:
 *  - free path: `wx` create — atomic, no delete involved.
 *  - displacement path: a stale/dead holder's file is removed ONLY by the
 *    process holding the `.breaking` sidecar token (itself wx-claimed) —
 *    without it, two concurrent "remove the corpse, then create" runs
 *    delete each other's fresh locks and BOTH end up believing they hold.
 *  A torn mid-beat read never yields on its own: its mtime is the freshest
 *  evidence the writer could leave, so only mtime staleness counts —
 *  matching the conservative stance the old initial check took but the
 *  wait-loop contradicted.
 *  `opts.staleWaitMs`: how long to sit on a live-looking holder before
 *  refusing. The fund-key lock leaves it 0 — custody fails closed NOW
 *  rather than waiting out a maybe-live second writer. The instance lock
 *  waits: a container recreate's bind mount carries the predecessor's
 *  frozen file forward and waiting out its decay beats a crash-loop. */
export async function acquireLock(file: string, label: string, opts?: { staleWaitMs?: number }): Promise<() => Promise<void>> {
  const host = (await import('node:os')).hostname()
  const me = `${process.pid}@${host}`
  const breakFile = `${file}.breaking`
  /** true/false on this host (kill(pid,0)); undefined when the owner is
   *  remote — a shared-fs lock has no probeable pid, the beat decides.
   *  EPERM = alive but unsignalable by us (different uid/integrity level) —
   *  only ESRCH proves the pid is gone. */
  const pidAlive = (owner: string): boolean | undefined => {
    const m = /^(\d+)@(.*)$/.exec(owner)
    if (m === null || m[2] !== host) return undefined
    try {
      process.kill(Number(m[1]), 0)
      return true
    } catch (e) {
      return (e as NodeJS.ErrnoException).code !== 'ESRCH'
    }
  }
  const inUse = (owner: string) => new Error(`${label} is in use by another nektar process (${owner}) — stop it first`)
  type Probe = { vanished: true } | { vanished?: false; owner?: string; beatAge: number }
  const probe = (): Probe => {
    try {
      const l = JSON.parse(readFileSync(file, 'utf8')) as { owner?: string; beat?: number }
      const beat = typeof l.beat === 'number' ? l.beat : statSync(file).mtimeMs
      return { owner: l.owner, beatAge: Date.now() - beat }
    } catch {
      try {
        return { beatAge: Date.now() - statSync(file).mtimeMs }
      } catch {
        return { vanished: true }
      }
    }
  }
  // displaceable = heartbeat silent past the bound, or a local owner pid
  // PROVEN dead (ESRCH). A torn file answers only through its mtime.
  const breakable = (p: Exclude<Probe, { vanished: true }>): boolean => p.beatAge >= 20_000 || (p.owner !== undefined && pidAlive(p.owner) === false)
  const tryCreate = (): boolean => {
    try {
      writeFileSync(file, JSON.stringify({ owner: me, beat: Date.now() }), { flag: 'wx' })
      return true
    } catch {
      return false
    }
  }
  // a live-looking holder refuses by default (staleWaitMs=0 — the custody
  // lock must never wait on a second writer). Opt-in callers wait for the
  // beat to decay instead of crash-looping.
  const until = Date.now() + (opts?.staleWaitMs ?? 0)
  for (;;) {
    if (tryCreate()) break
    const p = probe()
    if (p.vanished === true) continue // vanished between create and probe — the create path decides next pass
    if (p.owner === me) break // a recycled pid adopted our identity — beating rewrites it anyway
    if (!breakable(p)) {
      if (Date.now() >= until) throw inUse(p.owner ?? 'unknown')
      await nap(1_000)
      continue
    }
    // stale/dead — arbitrate the displacement through the breaker token so
    // exactly one acquirer removes the corpse
    try {
      writeFileSync(breakFile, me, { flag: 'wx' })
    } catch {
      await nap(300) // another breaker mid-displacement — its outcome lands first
      continue
    }
    try {
      // re-probe inside arbitration: a heartbeat that just landed is a live
      // holder — backing off here is the whole point of the second look
      const p2 = probe()
      if (p2.vanished !== true && !breakable(p2)) continue
      try {
        rmSync(file)
      } catch {
        /* the corpse vanished on its own — the wx create below decides */
      }
      if (tryCreate()) break
      // a fresh acquirer won the recreate race — fair, re-probe it
      continue
    } finally {
      try {
        rmSync(breakFile)
      } catch {
        /* another breaker cleaned it */
      }
    }
  }
  // every beat verifies ownership first: a DISPLACED owner that kept
  // beating would overwrite the winner's file, and its release would then
  // delete it. A lapsed owner stops writing and stops deleting.
  let owned = true
  const beatTimer = setInterval(() => {
    try {
      const l = JSON.parse(readFileSync(file, 'utf8')) as { owner?: string }
      if (l.owner !== me) {
        owned = false
        clearInterval(beatTimer)
        return
      }
      writeFileSync(file, JSON.stringify({ owner: me, beat: Date.now() }))
    } catch {
      /* torn or vanished — the next beat re-checks (or stops) */
    }
  }, 5_000)
  beatTimer.unref()
  return () => {
    clearInterval(beatTimer)
    if (!owned) return Promise.resolve()
    try {
      const l = JSON.parse(readFileSync(file, 'utf8')) as { owner?: string }
      if (l.owner === me) rmSync(file)
    } catch {
      /* already gone or torn — a corpse decays by mtime either way */
    }
    return Promise.resolve()
  }
}

/** tmpdir, not the data dir — the same fund key must not be live in any
 *  two nektar processes on this machine, wherever their data dirs sit. */
export const keyLockFile = (pkhHex: string): string => join(tmpdir(), `nektar-key-${pkhHex}.lock`)

export const acquireInstanceLock = async (dataDir: string): Promise<{ release: () => Promise<void> }> => ({
  // the data-dir lock is the one a container recreate collides on: a fast
  // recreate reads the predecessor's not-yet-stale beat — waiting it out
  // here beats crash-looping on the restart policy
  release: await acquireLock(join(dataDir, 'instance.lock'), `data dir ${dataDir}`, { staleWaitMs: 30_000 })
})

/** records.sqlite + shard store + automation ledger — the one opener for the
 *  data dir's durable state (WAL + busy_timeout), was two verbatim copies. */
export async function openRecordsDb(dataDir: string) {
  const knexMod = (await import('knex')).default
  const knex = knexMod({ client: 'sqlite3', connection: { filename: join(dataDir, 'records.sqlite') }, useNullAsDefault: true })
  // WAL: readers don't queue behind the writer; busy_timeout turns a
  // contended SQLITE_BUSY into a wait instead of a surfaced error
  await knex.raw('PRAGMA journal_mode=WAL')
  await knex.raw('PRAGMA busy_timeout=5000')
  const store: ShardStore = new KnexShardStore(knex)
  await store.init?.()
  const { AutomationStore, wrapBroadcast, leasePredicate } = await import('@nektar/automation')
  const ledger = new AutomationStore(knex)
  await ledger.init()
  return { knex, store, ledger, wrapBroadcast, leasePredicate }
}

/** heal hook: a verified proof arriving after a terminal-looking ledger
 *  verdict ('conflicted' from the abandon horizon, 'rejected' from a
 *  first-party refusal) flips the row back to 'confirmed' — those verdicts
 *  are ceilings, not truth. Errors log to stderr, never crash. */
export function healLedgerOnProof(mem: NektarMemory, ledger: import('@nektar/automation').AutomationStore, tag: string): () => void {
  const nlog = mkLog(`nektar ${tag}`)
  // onTxProof has no removal API — the stop flag is the unsubscribe: a
  // proof landing between close() and store teardown must not launch an
  // ambient-executor leg against closing stores
  let live = true
  mem.onTxProof((txid) => {
    if (!live) return
    // detached(): the heal is spawned work — a proof emitted inside an
    // automation leg must not hand that leg's deadline to the repair
    void detached(() =>
      (async () => {
        try {
          const w = await ledger.walletTx({ txid })
          if (w !== undefined && (w.state === 'conflicted' || w.state === 'rejected')) {
            const { txEmit, executor } = await import('@nektar/automation')
            // the machine owns the revive: obs.proven@proof carries tx-spent
            // + leases->spent + record-aired; with no fx here the intents
            // commit uneffected and the next watcher sweep replays them
            await txEmit(executor(), ledger, w, 'obs.proven', { guard: 'proof', observation: { at: Date.now(), healed: true } }, {})
            nlog(`${txid} proved after a terminal verdict — ledger healed to confirmed`)
          }
        } catch (e) {
          nlog(`proof-heal ${txid}: ${(e as Error).message}`)
        }
      })()
    )
  })
  return () => {
    live = false
  }
}

/** The instance's transaction graph — parents for spends, BEEF for
 *  counterparties, proofs as they are pushed — persisted next to the records
 *  once the database is open (nothing is broadcast before that). */
export function persistedTxStore(): { txStore: TxStore; open: (knex: import('knex').Knex) => Promise<void> } {
  let kx: import('knex').Knex | undefined
  const txStore = new TxStore({
    load: async () =>
      kx === undefined
        ? []
        : (await kx('tx_store').select('txid', 'raw', 'proof')).map((r: { txid: string; raw: string; proof: string | null }) => ({
            txid: r.txid,
            raw: r.raw,
            ...(r.proof ? { proof: r.proof } : {})
          })),
    put: async (row) => {
      if (kx !== undefined)
        await kx('tx_store')
          .insert({ txid: row.txid, raw: row.raw, proof: row.proof ?? null, at: Date.now() })
          .onConflict('txid')
          .merge(['proof'])
    }
  })
  return {
    txStore,
    open: async (knex) => {
      if (!(await knex.schema.hasTable('tx_store'))) {
        await knex.schema.createTable('tx_store', (t) => {
          t.string('txid', 64).primary()
          t.text('raw').notNullable()
          t.text('proof')
          t.bigInteger('at').notNullable()
        })
      }
      kx = knex
      await txStore.load()
    }
  }
}

/** The records-ledger watcher — the tx.machine effector wired to custody.
 *  txFromRow rehydrates parent txs for funds.spent/revive; exportExpired
 *  returns provably-live inputs on a timed-out export; the effects map runs
 *  the machine-declared intents ('watch'/'watch-trailing' map to nothing:
 *  this watcher already rescans live + recently-dead rows every pass). */
async function buildRecordsWatcher(d: {
  ledger: import('@nektar/automation').AutomationStore
  statusOf: (txid: string) => { ev: TxStatusEvent; verified: boolean } | undefined
  port: Pick<ChainPort, 'statusHint' | 'spendStatus' | 'txLookup' | 'guard'>
  funds: Funds
  fundingRefresh: (() => Promise<unknown>) | undefined
  diedFromRow: (w: import('@nektar/automation').WalletTx, tag: string) => Promise<void>
  net: string
}): Promise<import('@nektar/automation').TransactionWatcher> {
  const { TransactionWatcher } = await import('@nektar/automation')
  const { ledger, statusOf, port, funds, fundingRefresh, diedFromRow, net } = d
  const nlog = mkLog(`nektar ${net}`)
  const txFromRow = (w: { rawHex: string; parents: Record<string, string> }): Transaction => {
    const tx = Transaction.fromHex(w.rawHex)
    for (const inp of tx.inputs) {
      const hex = inp.sourceTXID !== undefined ? w.parents[inp.sourceTXID] : undefined
      if (hex !== undefined) inp.sourceTransaction = Transaction.fromHex(hex)
    }
    return tx
  }
  const exportExpired = async (w: { txid: string; rawHex: string; parents: Record<string, string> }): Promise<void> => {
    let freed = 0
    let inputs = 0
    try {
      const tx = Transaction.fromHex(w.rawHex)
      inputs = tx.inputs.length
      for (const inp of tx.inputs) {
        const srcTxid = inp.sourceTXID
        if (srcTxid === undefined) continue
        const vout = inp.sourceOutputIndex
        // provably-spent inputs stay dead — the export landed late or
        // something else consumed them. 'unknown' still frees: the
        // export had its window and the take-time probe burns a
        // later-landing spend anyway
        if ((await port.spendStatus?.(srcTxid, vout).catch(() => 'unknown' as const)) === 'spent') continue
        const parentHex: string | undefined = sparse(w.parents[srcTxid])
        const parent = parentHex !== undefined ? Transaction.fromHex(parentHex) : await port.txLookup?.(srcTxid).catch(() => undefined)
        if (parent === undefined) continue
        await funds.revive(parent, vout)
        freed++
      }
    } catch (e) {
      nlog(`expired export ${w.txid}: revive failed: ${(e as Error).message}`)
      return
    }
    nlog(`export ${w.txid} expired unaired — ${freed}/${inputs} input(s) returned to custody`)
  }
  return new TransactionWatcher(
    ledger,
    async (txid) => {
      // the shared substrate already probed this txid — use its answer
      // instead of a second fetch per watched tx per pass; a VERIFIED
      // terminal record settles it without the network call at all
      const seen = statusOf(txid)
      const verdict = seen?.verified === true ? watcherObservation(seen, '') : undefined
      if (verdict !== undefined) return verdict
      const hint = await port.statusHint?.(txid).catch(() => ({ status: '', answered: false }))
      return watcherObservation(seen, hint?.status ?? '')
    },
    {
      // the counterparty's window to air a signed payment (x402 export);
      // past it the watcher releases the leases and hands custody back
      // every input the chain still shows unspent — the one-way-door fix
      exportedTtlMs: nektarEnvNum('NEKTAR_EXPORT_TTL_MS') ?? 24 * 60 * 60_000,
      // the same custody semantics as declared intents — the table decides
      // WHICH fire, the effector just executes. 'watch' / 'watch-trailing'
      // map to nothing here: this watcher already rescans live +
      // recently-dead rows every pass
      effects: {
        txDied: async (w) => diedFromRow(w, 'rejected'),
        txSpent: async (w) => {
          try {
            await funds.spent(txFromRow(w))
            nlog(`${w.txid.slice(0, 16)}… aired after our ${w.state} verdict — custody re-marked`)
          } catch (e) {
            nlog(`revived tx ${w.txid}: spent() failed: ${(e as Error).message}`)
          }
          await (fundingRefresh?.() ?? (funds.refresh?.() ?? Promise.resolve()).then(() => undefined))
        },
        recordAired: (w) => {
          try {
            port.guard?.recordAired(txFromRow(w))
          } catch {
            /* day-ledger accounting is best-effort — the custody mark above is the durable one */
          }
        },
        exportExpired: async (w) => {
          await exportExpired(w)
        }
      }
    }
  )
}

/** the confirmer + records-watcher plane: one ChainEvents substrate fans out
 *  to (a) the confirmer pass — persist, flush, watch anchors+ledger txs,
 *  bounded ancestry heal, proof pull, topUp — and (b) the event-driven
 *  records watcher + journal prune. Both ride as journaled objects on the
 *  shared restate endpoint (defs + boot kicks returned for openAutomation
 *  to register). Boot plumbing — lives here, not in runtime.ts. */
export interface ConfirmerWatchDeps {
  cfg: { confirmEveryMs?: number; autoMine?: boolean }
  mem: NektarMemory
  port: ChainPort
  events: ChainEvents
  ledger: import('@nektar/automation').AutomationStore
  chain: { broadcast: (tx: Transaction) => Promise<BroadcastResult> }
  txStore: TxStore
  funds: Funds
  fundingRefresh: (() => Promise<void>) | undefined
  diedFromRow: (w: import('@nektar/automation').WalletTx, tag: string) => Promise<void>
  net: string
  nlog: (line: string) => void
  persistKeys: () => void
  topUp: (() => Promise<void>) | undefined
}

export async function openConfirmerWatch(d: ConfirmerWatchDeps): Promise<{
  rtDefs: unknown[]
  rtKicks: InvokeTarget[]
  /** final-flush seam for close() — one last confirmer pass */
  tick: () => Promise<void>
  /** the running pass's promise while one is in flight */
  inflight: () => Promise<void> | undefined
  close: () => void
}> {
  // confirmer: timed flush + optional auto-mine; proof attachment itself is
  // push-shaped — this loop only registers interest with the events
  // substrate, which delivers via d.chain.onStatus → d.mem.deliverTxProof.
  // Parents of unproven anchors are watched too: a funding tree is
  // ancestry-anchored the moment its own inputs prove. Height-gating lives
  // inside the substrate — a static tip skips the batch entirely.
  // both loops are journaled objects on the shared endpoint — defs/kicks
  // returned to bin.ts and registered by openAutomation.
  const rtDefs: unknown[] = []
  const rtKicks: InvokeTarget[] = []
  let busy = false
  let inflight: Promise<void> | undefined
  let proofPullAt = 0
  const tick = async () => {
    if (busy) return await inflight
    busy = true
    inflight = (async () => {
      try {
        d.persistKeys()
        await d.mem.flushDue()
        const waiting = d.mem.unprovenAnchors()
        if (waiting.length > 0 && d.cfg.autoMine) await d.port.mine?.()
        for (const { txid } of waiting) {
          d.events.watch(txid)
          const tx = await d.mem.fetchTx(txid)
          for (const i of tx?.inputs ?? []) {
            const src = i.sourceTransaction
            if (src !== undefined && src.merklePath === undefined) d.events.watch(src.id('hex'))
          }
        }
        // ledger txs too: one shared probe per txid serves the watcher's
        // statusOf short-circuit — no second fetch per inflight tx
        for (const w of await d.ledger.walletTxs(['submitted', 'submission_unknown', 'observed'])) d.events.watch(w.txid)
        // stalled-submission heal: a row parked missing-parent (or vanished
        // after acceptance — clean 404) can never settle, and nothing else
        // will put the absent ancestor on the wire. Walk stored ancestry —
        // the row's parents first, deeper links via the instance tx graph
        // and the ledger itself — and re-air every ancestor the network
        // honestly answers "not found" for. A refused parent can never be a
        // missing ancestor: died() tombstones its outputs so no child ever
        // chains on it. Bounded per pass; deep chains heal one level a tick.
        {
          let healBudget = 8
          const healSeen = new Set<string>()
          const resolveTx = async (txid: string): Promise<Transaction | undefined> => {
            const inGraph = d.txStore.get(txid)
            if (inGraph !== undefined) return inGraph
            const inLedger = (await d.ledger.walletTx({ txid }).catch(() => undefined))?.rawHex
            if (inLedger !== undefined) return Transaction.fromHex(inLedger)
            return await d.port.txLookup?.(txid).catch(() => undefined)
          }
          const hintOf = async (txid: string): Promise<{ status: string; answered: boolean } | undefined> => d.port.statusHint?.(txid).catch(() => undefined)
          const reair = async (tx: Transaction): Promise<void> => {
            for (const i of tx.inputs) {
              const pid = sparse(i.sourceTXID)
              if (i.sourceTransaction !== undefined || pid === undefined) continue
              i.sourceTransaction = await resolveTx(pid)
            }
            const r = await d.chain.broadcast(tx)
            if (r.status === 'success') d.nlog(`re-aired ${tx.id('hex')} — wire gap self-healed`)
          }
          const healWalk = async (tx: Transaction): Promise<void> => {
            for (const i of tx.inputs) {
              const pid = i.sourceTXID
              if (pid === undefined || healSeen.has(pid) || healBudget <= 0) continue
              healSeen.add(pid)
              healBudget--
              const parent = i.sourceTransaction ?? (await resolveTx(pid))
              if (parent === undefined || parent.merklePath !== undefined) continue
              const st = await hintOf(pid)
              if (st?.answered === true && st.status === '') await reair(parent)
              else if (st?.answered !== true || st.status !== '') await healWalk(parent) // parent on the wire (or unanswerable) — its own ancestry may still be parked
            }
          }
          for (const w of await d.ledger.walletTxs(['submitted', 'submission_unknown'])) {
            if (healBudget <= 0) break
            const st = await hintOf(w.txid)
            if (st?.answered !== true) continue
            const parked = /PENDING|MISSING|RETRY/i.test(st.status)
            if (!parked && st.status !== '') continue
            const tx = Transaction.fromHex(w.rawHex)
            for (const i of tx.inputs) {
              const pid = sparse(i.sourceTXID)
              if (i.sourceTransaction !== undefined || pid === undefined) continue
              const hex = sparse(w.parents[pid])
              if (hex !== undefined) i.sourceTransaction = Transaction.fromHex(hex)
            }
            await healWalk(tx)
            if (st.status === '') await reair(tx) // the row itself vanished — re-air its own bytes
          }
          // anchors take the same heal: a broadcast the network dropped
          // asynchronously can never prove — the watch alone waits forever.
          // Probe the wire; 'absent' re-airs the same bytes (idempotent: a
          // landed original dedupes, a truly-dropped one just needs airing).
          for (const { txid } of waiting) {
            if (healBudget <= 0) break
            if (healSeen.has(txid)) continue
            const st = await hintOf(txid)
            if (st?.answered !== true || st.status !== '') continue
            const tx = await d.mem.fetchTx(txid)
            if (tx === undefined) continue
            healSeen.add(txid)
            healBudget--
            await healWalk(tx)
            await reair(tx)
          }
        }
        // a bounded, rotating pull: each tick tries a few unproven txs so a
        // long list can never serialize minutes of lookups inside one pass —
        // push (callbacks/status stream) stays the primary proof channel
        {
          const unproven = d.txStore.unproven()
          const pull = unproven.length <= 8 ? unproven : Array.from({ length: 8 }, (_, i) => unproven[(proofPullAt + i) % unproven.length])
          proofPullAt = unproven.length > 0 ? (proofPullAt + 8) % unproven.length : 0
          for (const txid of pull) await d.mem.fetchTx(txid)
        }
        if (waiting.length) d.nlog(`confirmer: waiting=${waiting.length} (watched=${d.events.pending})`)
        await d.topUp?.()
        d.persistKeys()
      } catch (e) {
        d.nlog(`confirmer: ${(e as Error).message}`)
      } finally {
        busy = false
        inflight = undefined
      }
    })()
    await inflight
  }
  // journaled self-arming cadence — AnchorQueued wakes become calls, not
  // sends: at most one queued pass rides behind a running one (the busy
  // flag's coalescing, kept)
  rtDefs.push(await cronObject(EXEC_SVC.confirmer, { tick, ceilingMs: () => d.cfg.confirmEveryMs ?? 5000, log: d.nlog }, RT_OPTS_HEAVY))
  rtKicks.push({ service: EXEC_SVC.confirmer, handler: 'tick', key: 'main' })
  // every wake gates on `closed` — mem/ChainEvents emitters outlive this
  // region's teardown and several of them expose no unsubscribe, so the
  // flag is the only removal that works on every subscription shape
  let closed = false
  let anchorWake = false
  d.mem.onAnchorQueued(() => {
    if (anchorWake || closed) return
    anchorWake = true
    void executor()
      .call({ service: EXEC_SVC.confirmer, handler: 'tick', key: 'main' }, {})
      .catch((e: unknown) => d.nlog(`confirmer wake: ${(e as Error).message}`))
      .finally(() => {
        anchorWake = false
      })
  })
  // the ledger watcher — event-driven off the shared substrate's sweeps,
  // height advances and pushed broadcaster statuses; no private interval.
  // Construction + custody effects live in rtsupport (boot plumbing)
  const watcher = await buildRecordsWatcher({
    ledger: d.ledger,
    statusOf: (txid) => d.events.statusOf(txid),
    port: d.port,
    funds: d.funds,
    fundingRefresh: d.fundingRefresh,
    diedFromRow: d.diedFromRow,
    net: d.net
  })
  let watching = false
  // journal retention on the records ledger — 'confirmed' is the one tx
  // state with no outgoing edge, so its event journal is complete and
  // prunable past the window. Daily-gated: event bursts must not sweep
  // per tick, and the records store's market tables stay empty anyway.
  let lastPrune = 0
  const pruneJournal = () => {
    const now = Date.now()
    if (now - lastPrune < 86_400_000) return
    lastPrune = now
    const retentionMs = (nektarEnvNum('NEKTAR_JOURNAL_RETENTION_DAYS') ?? 30) * 86_400_000
    void d.ledger.prune(now - retentionMs, { ops: [], txs: ['confirmed'] }).catch(() => undefined)
  }
  const watchLeg = async () => {
    await watcher.tick()
    pruneJournal()
  }
  // the leg object serializes on 'main'; event wakes become calls so an
  // event burst still coalesces to at most one queued pass
  rtDefs.push(await legObject(EXEC_SVC.watcher, watchLeg, RT_OPTS_HEAVY))
  const watchOnce = (): void => {
    if (watching || closed) return
    watching = true
    void executor()
      .call({ service: EXEC_SVC.watcher, handler: 'tick', key: 'main' }, {})
      .catch((e) => d.nlog(`ledger watcher: ${(e as Error).message}`))
      .finally(() => {
        watching = false
      })
  }
  const relayHeight = (tip: number) => {
    if (!closed) d.mem.events.emit('height', { tip })
  }
  d.events.on('tick', watchOnce)
  d.events.on('height', watchOnce)
  // relay tip advances onto the memory emitter the way txAbandoned/reorg
  // already are — hosted automation wakes height-bounded work off it
  d.events.on('height', relayHeight)
  d.mem.events.on('broadcasterStatus', watchOnce)

  return {
    rtDefs,
    rtKicks,
    tick,
    inflight: () => inflight,
    close: () => {
      closed = true
      // every subscription we took out comes off — a leaked wake fires
      // ambient-executor calls against torn-down stores after close()
      d.events.off('tick', watchOnce)
      d.events.off('height', watchOnce)
      d.events.off('height', relayHeight)
      d.mem.events.off('broadcasterStatus', watchOnce)
    }
  }
}
