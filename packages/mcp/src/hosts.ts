// hosts.ts — the operator's saved market hosts. NEKTAR_MARKET_HOSTS and the
// baked-in seeds are the boot set; the dashboard's join flow appends to
// hosts.json's `added`, disconnect marks `blocked` (so a seed or env host can
// be left persistently, not just muted for the run). Every reader of "which
// hosts does this instance trade through" goes through marketHosts().
import { existsSync, readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { join, dirname } from 'node:path'
import { nektarEnv, marketHostsFromEnv, type EnvSource } from '@nektar/core'

interface HostFile {
  added: string[]
  blocked: string[]
}

const hostsFile = (env: EnvSource = process.env): string => join(nektarEnv('NEKTAR_DATA_DIR', env) ?? join(process.cwd(), '.nektar'), 'hosts.json')

const norm = (h: unknown): string[] => (typeof h === 'string' ? [h.replace(/\/+$/, '')] : [])

function readHosts(env: EnvSource): HostFile {
  const f = hostsFile(env)
  if (!existsSync(f)) return { added: [], blocked: [] }
  try {
    const j = JSON.parse(readFileSync(f, 'utf8')) as unknown
    // v1 shape was a bare string array — treat it as `added`
    if (Array.isArray(j)) return { added: j.flatMap(norm), blocked: [] }
    const o = j as Partial<Record<'added' | 'blocked', unknown>>
    return { added: Array.isArray(o.added) ? o.added.flatMap(norm) : [], blocked: Array.isArray(o.blocked) ? o.blocked.flatMap(norm) : [] }
  } catch {
    return { added: [], blocked: [] }
  }
}

export function savedHosts(env: EnvSource = process.env): string[] {
  return readHosts(env).added
}

export function blockedHosts(env: EnvSource = process.env): string[] {
  return readHosts(env).blocked
}

/** the effective host set — (env ∪ added) − blocked, deduped. The dashboard
 *  /state, the well-known doc's market.hosts, and the boot cfg all read
 *  through here. */
export function marketHosts(env: EnvSource = process.env): string[] {
  const blocked = new Set(readHosts(env).blocked)
  return [...new Set([...marketHostsFromEnv(env), ...savedHosts(env)])].filter((h) => !blocked.has(h))
}

const write = (hf: HostFile, env: EnvSource): void => {
  const f = hostsFile(env)
  mkdirSync(dirname(f), { recursive: true })
  writeFileSync(f, JSON.stringify(hf, null, 2))
}

/** persist an origin-normalized join (deduped, unblocks a prior disconnect). */
export function addSavedHost(url: string, env: EnvSource = process.env): void {
  const hf = readHosts(env)
  if (!hf.added.includes(url)) hf.added.push(url)
  hf.blocked = hf.blocked.filter((h) => h !== url)
  write(hf, env)
}

/** persist a disconnect — an operator-added host leaves `added`; an env or
 *  seed host lands in `blocked` so the leave survives the next boot. */
export function removeSavedHost(url: string, env: EnvSource = process.env): void {
  const hf = readHosts(env)
  const wasAdded = hf.added.includes(url)
  hf.added = hf.added.filter((h) => h !== url)
  if (!wasAdded && !hf.blocked.includes(url)) hf.blocked.push(url)
  write(hf, env)
}
