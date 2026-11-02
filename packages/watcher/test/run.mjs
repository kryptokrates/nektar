// CI gate: the watcher's only suite is a live --once smoke against local
// regtest (it pays its own challenge bonds, so it needs MAMEM_REGTEST_WIF).
// When no WIF is configured — real CI — skip cleanly instead of failing.
import { existsSync, readFileSync } from 'node:fs'
import { spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'
import { join } from 'node:path'

const here = fileURLToPath(new URL('.', import.meta.url))
const readEnv = p => {
  const out = {}
  if (!existsSync(p)) return out
  for (const line of readFileSync(p, 'utf8').split(/\r?\n/)) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}
// same lookup chain as watcher.mjs: package .env, packages/.env, repo .env
const local = {
  ...readEnv(join(here, '../.env')),
  ...readEnv(join(here, '../../.env')),
  ...readEnv(join(here, '../../../.env'))
}
if (!process.env.MAMEM_REGTEST_WIF && !local.MAMEM_REGTEST_WIF) {
  console.log('# SKIP MAMEM_REGTEST_WIF unset — watcher smoke needs a live regtest env')
  process.exit(0)
}
const r = spawnSync(process.execPath, ['src/watcher.mjs', '--once', '--claims', '.state/watcher-claims.json'], {
  cwd: join(here, '..'), stdio: 'inherit'
})
process.exit(r.status ?? 1)
