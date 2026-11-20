// rtsupport.ts — openRuntime boot plumbing: key files, instance/fund-key
// locks, the records db opener, the tx-store persister, the broadcast log
// hook and the ledger proof-heal. No chain or custody decisions live here.
import { existsSync, readFileSync, writeFileSync, appendFileSync, rmSync, statSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { PrivateKey, Transaction } from '@bsv/sdk'
import { nektarEnv } from '@nektar/core'
import { TxStore } from '@nektar/chain'
import { KnexShardStore, type ShardStore } from '@nektar/log'
import { readKeyFile, writeKeyFile, fundKeyFromMnemonic } from './keystore.js'
import type { NektarMemory } from './memory.js'

interface KeyFile {
  agentSeed: string
  logKey: string
  shardKeys: Record<string, string>
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
          const src = i.sourceTransaction?.outputs[i.sourceOutputIndex ?? 0]
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

/** single holder per lock file: PID liveness proves a dead local holder at
 *  once (no 20s wait on a stale-looking beat); a live/unverifiable holder
 *  yields only when its heartbeat goes stale (>20s). A corrupt file's mtime
 *  stands in for the beat — a live process rewrites it every 5s. */
export async function acquireLock(file: string, label: string): Promise<() => Promise<void>> {
  const host = (await import('node:os')).hostname()
  const me = `${process.pid}@${host}`
  /** true/false on this host (kill(pid,0)); undefined when the owner is
   *  remote — a shared-fs lock has no probeable pid, the beat decides */
  const pidAlive = (owner: string): boolean | undefined => {
    const m = /^(\d+)@(.*)$/.exec(owner)
    if (m === null || m[2] !== host) return undefined
    try {
      process.kill(Number(m[1]), 0)
      return true
    } catch {
      return false
    }
  }
  const inUse = (owner: string) => new Error(`${label} is in use by another nektar process (${owner}) — stop it first`)
  if (existsSync(file)) {
    let refuse = false
    let owner = 'unknown'
    try {
      const l = JSON.parse(readFileSync(file, 'utf8')) as { owner: string; beat: number }
      owner = l.owner
      // a dead local pid is displaced now; a live or unverifiable one only
      // after the heartbeat went silent
      refuse = l.owner !== me && pidAlive(l.owner) !== false && Date.now() - l.beat < 20_000
    } catch {
      // unreadable or corrupt — the file mtime is the freshest beat a live
      // writer could have left; only an old corpse yields the lock. A file
      // that vanished mid-check is no lock at all
      try {
        refuse = Date.now() - statSync(file).mtimeMs < 20_000
      } catch {
        refuse = false
      }
    }
    if (refuse) throw inUse(owner)
    try {
      rmSync(file)
    } catch {
      /* a racing acquirer removed it first — the wx create below decides */
    }
  }
  const beat = () => writeFileSync(file, JSON.stringify({ owner: me, beat: Date.now() }))
  try {
    writeFileSync(file, JSON.stringify({ owner: me, beat: Date.now() }), { flag: 'wx' })
  } catch {
    throw inUse('lock race')
  }
  const beatTimer = setInterval(beat, 5_000)
  beatTimer.unref()
  return async () => {
    clearInterval(beatTimer)
    try {
      rmSync(file)
    } catch {
      /* already gone */
    }
  }
}

/** tmpdir, not the data dir — the same fund key must not be live in any
 *  two nektar processes on this machine, wherever their data dirs sit. */
export const keyLockFile = (pkhHex: string): string => join(tmpdir(), `nektar-key-${pkhHex}.lock`)

export const acquireInstanceLock = async (dataDir: string): Promise<{ release: () => Promise<void> }> => ({
  release: await acquireLock(join(dataDir, 'instance.lock'), `data dir ${dataDir}`)
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
export function healLedgerOnProof(mem: NektarMemory, ledger: import('@nektar/automation').AutomationStore, tag: string): void {
  mem.onTxProof((txid) => {
    void (async () => {
      try {
        const w = await ledger.walletTx({ txid })
        if (w !== undefined && (w.state === 'conflicted' || w.state === 'rejected')) {
          const { emitTxEvent, txFsmLegacy } = await import('@nektar/automation')
          if (!txFsmLegacy()) {
            // the machine owns the revive: obs.proven@proof carries tx-spent
            // + leases->spent + record-aired; with no fx here the intents
            // commit uneffected and the next watcher sweep replays them
            await emitTxEvent(ledger, w, 'obs.proven', { guard: 'proof', observation: { at: Date.now(), healed: true } }, {})
          } else await ledger.setTxState(w.actionId, 'confirmed', { at: Date.now(), healed: true })
          process.stderr.write(`nektar ${tag}: ${txid} proved after a terminal verdict — ledger healed to confirmed\n`)
        }
      } catch (e) {
        process.stderr.write(`nektar ${tag}: proof-heal ${txid}: ${(e as Error).message}\n`)
      }
    })()
  })
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
