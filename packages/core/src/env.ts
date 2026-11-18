// env.ts — the only place `process.env.NEKTAR_*` reads and writes may live.
// check-arch grep-gates stray access elsewhere. Typed helpers normalize the
// three shapes this codebase actually uses: string (empty→undefined),
// number, and tri-state flag.
export type EnvSource = Record<string, string | undefined>

/** raw read — undefined when unset or empty ('' is never a value here).
 *  '<UNKNOWN>' is the Docker MCP toolkit's sentinel for a declared-but-
 *  unset secret — an optional secret it injects verbatim would otherwise
 *  be parsed as a real value (a '<UNKNOWN>' WIF crashes the wallet) */
export function nektarEnv(key: string, env: EnvSource = process.env): string | undefined {
  const v = env[key]
  return v === undefined || v === '' || v === '<UNKNOWN>' ? undefined : v
}

/** numeric read — undefined when unset, empty, or non-finite */
export function nektarEnvNum(key: string, env: EnvSource = process.env): number | undefined {
  const v = nektarEnv(key, env)
  if (v === undefined) return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

/** flag read — '1'/'true'/'yes'/'on' → true; unset/empty → undefined so
 *  callers can still distinguish "explicitly off" vs "unset" */
export function nektarEnvFlag(key: string, env: EnvSource = process.env): boolean | undefined {
  const v = nektarEnv(key, env)
  return v === undefined ? undefined : /^(1|true|yes|on)$/i.test(v)
}

/** the only NEKTAR_* write — for composition code that materializes a
 *  derived value back into the process surface (callback tokens, resolved
 *  file paths) and for .env-file loaders */
export function setNektarEnv(key: string, value: string | undefined, env: EnvSource = process.env): void {
  if (value === undefined) delete env[key]
  else env[key] = value
}

/** snapshot of every NEKTAR_* variable present — config modules and doctor
 *  tools that need the whole surface take it once, here */
export function readNektarEnv(env: EnvSource = process.env): EnvSource {
  const out: EnvSource = {}
  for (const [k, v] of Object.entries(env)) if (k.startsWith('NEKTAR_')) out[k] = v
  return out
}

/** baked-in bootstrap hosts — the plural Layer-0 entry points of a network
 *  whose lanes verify everything they serve (signature + SPV + funding
 *  binding), so a seed host is untrusted by design: the worst it does is
 *  withhold, which quorum reads detect — it can never forge. Operators who
 *  run a public host pair on a given network can be added here. */
export const DEFAULT_MARKET_HOSTS: Record<string, string[]> = {
  testnet: ['https://nektar.run/hosts/a', 'https://nektar.run/hosts/b']
}

/** the effective market-host set: an explicit NEKTAR_MARKET_HOSTS wins
 *  outright — a private silo must be able to exclude the seeds — else the
 *  baked-in seed hosts for NEKTAR_NETWORK, else [] (regtest always runs
 *  its own hosts; nothing baked in). */
export function marketHostsFromEnv(env: EnvSource = process.env): string[] {
  const explicit = (nektarEnv('NEKTAR_MARKET_HOSTS', env) ?? '')
    .split(',')
    .map((s) => s.trim().replace(/\/+$/, ''))
    .filter(Boolean)
  if (explicit.length > 0) return explicit
  return [...(DEFAULT_MARKET_HOSTS[nektarEnv('NEKTAR_NETWORK', env) ?? 'regtest'] ?? [])]
}
