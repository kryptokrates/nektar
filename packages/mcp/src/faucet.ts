// faucet.ts — a self-serve testnet drip: the stranger's one step that cannot
// be done without outside action becomes a POST. NEKTAR_FAUCET=1 opts the
// route in (it is unauthenticated by design — a stranger can't hold our
// bearer); it is hard-refused on main. The spend rides the instance's own
// funded path (sendSats → SqlFunds → SpendGuard), so these caps are the
// whole extra defense: one drip per address, a daily sats ceiling, both in a
// persisted ledger a restart can't reset, plus a single-flight lock so a
// double-click can't race two drips onto one UTXO set.
import { mkdirSync, readFileSync, renameSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { P2PKH } from '@bsv/sdk'
import { nektarEnvFlag, nektarEnvNum, nektarEnv, type EnvSource } from '@nektar/core'

interface Drip {
  address: string
  txid: string
  sats: number
  ts: number
}

interface FaucetLedger {
  v: 1
  drips: Drip[]
}

// the drip rides sendSats → SpendGuard — the default must sit under the
// guard's shipped daily cap (20_000) or the feature refuses by construction;
// operators raising NEKTAR_MAX_SPEND_SATS_PER_DAY can raise this to match
export const FAUCET_DEFAULT_SATS = 10_000
const FAUCET_DEFAULT_DAILY_SATS = 5_000_000

/** the route gate, evaluated per request so env changes take effect live:
 *  opted in, and never on main — a faucet is a testnet artifact; a mainnet
 *  instance that opens this is a drain, so the refusal is unconditional. */
export const faucetEnabled = (env: EnvSource = process.env): boolean =>
  nektarEnvFlag('NEKTAR_FAUCET', env) === true && (nektarEnv('NEKTAR_NETWORK', env) ?? 'regtest') !== 'main'

const readLedger = (file: string): FaucetLedger => {
  try {
    const j = JSON.parse(readFileSync(file, 'utf8')) as { drips?: Drip[] }
    return { v: 1, drips: Array.isArray(j.drips) ? j.drips : [] }
  } catch {
    return { v: 1, drips: [] }
  }
}

const dayStart = (now: number): number => now - (now % 86_400_000)

// drips serialize GLOBALLY, not per address: the check-spend-write sequence
// is a read-modify-write on one shared ledger, and concurrent distinct-
// address drips read the same stale ledger — caps bypassed, the last write
// wins and forgets the other's record. Faucet volume is low by definition;
// a queued drip costs a stranger nothing.
let dripChain: Promise<unknown> = Promise.resolve()

/** tmp+rename: a crash mid-writeFileSync must not leave a truncated ledger
 *  that forgets every prior drip (and re-opens the addresses it covered). */
const writeLedger = (file: string, ledger: FaucetLedger): void => {
  const tmp = `${file}.${process.pid}.tmp`
  writeFileSync(tmp, JSON.stringify(ledger, null, 2))
  renameSync(tmp, file)
}

export async function faucetDrip(p: {
  mem: { sendSats(args: { amount: number; address: string }): Promise<{ txid: string }> }
  address: string
  dataDir: string
  env?: EnvSource
  now?: number
}): Promise<{ ok: true; txid: string; sats: number } | { ok: false; status: number; error: string }> {
  const env = p.env ?? process.env
  const now = p.now ?? Date.now()
  const address = p.address.trim()
  // validate by building the lock — a bad address throws before any cap work
  try {
    new P2PKH().lock(address)
  } catch {
    return { ok: false, status: 400, error: 'not a valid address' }
  }
  const run = async (): Promise<{ ok: true; txid: string; sats: number } | { ok: false; status: number; error: string }> => {
    const file = join(p.dataDir, 'faucet-drips.json')
    const ledger = readLedger(file)
    if (ledger.drips.some((d) => d.address === address)) return { ok: false, status: 429, error: 'address already received a drip' }
    const sats = nektarEnvNum('NEKTAR_FAUCET_SATS', env) ?? FAUCET_DEFAULT_SATS
    const dailyCap = nektarEnvNum('NEKTAR_FAUCET_DAILY_SATS', env) ?? FAUCET_DEFAULT_DAILY_SATS
    const today = ledger.drips.filter((d) => d.ts >= dayStart(now)).reduce((a, d) => a + d.sats, 0)
    if (today + sats > dailyCap) return { ok: false, status: 429, error: 'faucet daily limit reached — retry tomorrow' }
    const out = await p.mem.sendSats({ amount: sats, address })
    ledger.drips.push({ address, txid: out.txid, sats, ts: now })
    mkdirSync(p.dataDir, { recursive: true })
    writeLedger(file, ledger)
    return { ok: true, txid: out.txid, sats }
  }
  // the queued drip evaluates the ledger AFTER the in-flight one commits —
  // same-address dedupe and the daily cap both see the freshest state
  const r = dripChain.then(run, run)
  dripChain = r.then(
    () => undefined,
    () => undefined
  )
  return await r
}
