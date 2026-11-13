// env.ts — the only place `process.env.NEKTAR_*` reads and writes may live.
// check-arch grep-gates stray access elsewhere. Typed helpers normalize the
// three shapes this codebase actually uses: string (empty→undefined),
// number, and tri-state flag.
export type EnvSource = Record<string, string | undefined>

/** raw read — undefined when unset or empty ('' is never a value here) */
export function nektarEnv(key: string, env: EnvSource = process.env): string | undefined {
  const v = env[key]
  return v === undefined || v === '' ? undefined : v
}

/** numeric read — undefined when unset, empty, or non-finite */
export function nektarEnvNum(key: string, env: EnvSource = process.env): number | undefined {
  const v = nektarEnv(key, env)
  if (v === undefined) return undefined
  const n = Number(v)
  return Number.isFinite(n) ? n : undefined
}

/** flag read — '1'/'true'/'yes'/'on' → true; unset/empty → undefined so
 *  callers can still distinguish "explicitly off" from "unset" */
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
