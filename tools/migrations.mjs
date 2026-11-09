// tools/migrations.mjs — data-dir migration registry (Hermes _config_version
// pattern). `tools/update.mjs` runs pending entries in order and bumps
// <dataDir>/VERSION. `doctor{fix:true}` writes the v1 baseline when missing.
//
//   { v: N, name: 'what it does', run: async ({ dataDir, env }) => 'note' }
//
// Rules: migrations are additive/repair-only — never delete user records.
// A migration that can't complete throws; update stops and reports.
import { existsSync, readFileSync } from 'node:fs'
import { join } from 'node:path'

export const MIGRATIONS = [
  {
    v: 1,
    name: 'baseline — VERSION marker established',
    run: async () => 'marker written'
  }
]

export function currentVersion (dataDir) {
  const f = join(dataDir, 'VERSION')
  if (!existsSync(f)) return 0
  return Number(readFileSync(f, 'utf8').trim()) || 0
}
