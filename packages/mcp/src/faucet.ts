// faucet.ts — a self-serve testnet drip: the stranger's one step that cannot
// be done without outside action becomes a POST. NEKTAR_FAUCET=1 opts the
// route in (it is unauthenticated by design — a stranger can't hold our
// bearer); it is hard-refused on main. The spend rides the instance's own
// funded path (sendSats → SqlFunds → SpendGuard), so these caps are the
// whole extra defense: one drip per address, a daily sats ceiling, both in a
// persisted ledger a restart can't reset, plus a single-flight lock so a
// double-click can't race two drips onto one UTXO set.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
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

export const FAUCET_DEFAULT_SATS = 250_000
export const FAUCET_DEFAULT_DAILY_SATS = 5_000_000

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

const inflight = new Set<string>()

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
  const file = join(p.dataDir, 'faucet-drips.json')
  const ledger = readLedger(file)
  if (ledger.drips.some((d) => d.address === address)) return { ok: false, status: 429, error: 'address already received a drip' }
  const sats = nektarEnvNum('NEKTAR_FAUCET_SATS', env) ?? FAUCET_DEFAULT_SATS
  const dailyCap = nektarEnvNum('NEKTAR_FAUCET_DAILY_SATS', env) ?? FAUCET_DEFAULT_DAILY_SATS
  const today = ledger.drips.filter((d) => d.ts >= dayStart(now)).reduce((a, d) => a + d.sats, 0)
  if (today + sats > dailyCap) return { ok: false, status: 429, error: 'faucet daily limit reached — retry tomorrow' }
  if (inflight.has(address)) return { ok: false, status: 409, error: 'drip already in flight for this address' }
  inflight.add(address)
  try {
    const out = await p.mem.sendSats({ amount: sats, address })
    ledger.drips.push({ address, txid: out.txid, sats, ts: now })
    mkdirSync(p.dataDir, { recursive: true })
    writeFileSync(file, JSON.stringify(ledger, null, 2))
    return { ok: true, txid: out.txid, sats }
  } finally {
    inflight.delete(address)
  }
}
