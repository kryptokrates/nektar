// kill.ts — the operator's hard stop for NEW spends. A flag (env or file)
// is consulted at every new-deal/new-escrow entry point; in-flight deals are
// untouched — refunds and settlements keep running so money is never stranded
// by the stop itself.
//
// FAILS CLOSED: a flag file that exists counts as stopped, and a configured
// flag that cannot be read counts as stopped too — an operator who cannot
// create the flag file has bigger problems, and an attacker who can delete
// files must not be able to un-stop a killed agent by removing it.
import { statSync } from 'node:fs'
import { join } from 'node:path'

export interface KillState {
  killed: boolean
  /** where the stop came from ('NEKTAR_KILL', the flag path, an unreadable flag) */
  source?: string
}

export function killSwitchActive(dataDir?: string, env = process.env, stat: (p: string) => unknown = statSync): KillState {
  if (/^(1|true|yes)$/i.test(env.NEKTAR_KILL ?? '')) return { killed: true, source: 'NEKTAR_KILL' }
  const file = env.NEKTAR_KILL_FILE ?? (dataDir !== undefined ? join(dataDir, 'KILL') : undefined)
  if (file === undefined) return { killed: false }
  try {
    stat(file)
    return { killed: true, source: file }
  } catch (e) {
    const code = (e as NodeJS.ErrnoException).code
    // ENOENT = no flag — the normal running state. Anything else means we
    // cannot determine the flag's state → stopped (fail closed).
    if (code === 'ENOENT') return { killed: false }
    return { killed: true, source: `${file} (unreadable: ${code ?? 'error'})` }
  }
}
