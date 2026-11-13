// conformance.ts — the unattended gate. A green scenarios:verify run issues a
// `conformance.token`: {gitCommit, configHash, suiteDigest, issuedAt,
// expiresAt} signed by an OPERATOR/CI key that lives outside the agent's
// keystore. The agent verifies — it can never mint: it has no access to the
// signing key, only the public half (NEKTAR_OPERATOR_PUB or dataDir/
// operator.pub). Preflight fails closed on missing, stale, wrong-commit,
// wrong-config or bad-signature tokens, so NEKTAR_UNATTENDED=1 is honored
// only while a verify run ≤7d old attests THIS commit + THIS config.
import { existsSync, readFileSync, readdirSync, statSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { execFileSync } from 'node:child_process'
import { createHash } from 'node:crypto'
import { fileURLToPath } from 'node:url'
import { PrivateKey, PublicKey, Signature } from '@bsv/sdk'
import { sha256, utf8, toHex } from '@nektar/core'

export const TOKEN_TTL_MS = 7 * 24 * 3600 * 1000 // exactly 7d — expiry forces a verify re-run
export interface ConformanceClaims {
  v: 1
  gitCommit: string
  configHash: string
  suiteDigest: string
  issuedAt: number
  expiresAt: number
}
export interface ConformanceToken extends ConformanceClaims {
  pub: string
  sig: string
}

const canon = (c: ConformanceClaims): string =>
  JSON.stringify({ v: 1, gitCommit: c.gitCommit, configHash: c.configHash, suiteDigest: c.suiteDigest, issuedAt: c.issuedAt, expiresAt: c.expiresAt })

/** the config the token is bound to — what scenarios:verify actually ran
 *  against. Sorted host list so ordering noise can't mint a different hash. */
export function conformanceConfigHash(cfg: { network: string; marketHosts?: string[] }): string {
  return toHex(
    Uint8Array.from(
      sha256(utf8(JSON.stringify({ v: 1, network: cfg.network, marketHosts: [...(cfg.marketHosts ?? [])].map((h) => h.replace(/\/+$/, '')).sort() })))
    )
  )
}

/** digest of the scenario suite itself — sorted file names + bytes under the
 *  suite dir, so a changed scenario invalidates tokens issued before it. */
export function suiteDigest(suiteDir: string): string {
  const h = createHash('sha256')
  const files = readdirSync(suiteDir)
    .filter((f) => statSync(join(suiteDir, f)).isFile())
    .sort()
  for (const f of files) {
    h.update(f)
    h.update('\0')
    h.update(readFileSync(join(suiteDir, f)))
    h.update('\0')
  }
  return h.digest('hex')
}

/** the commit the running code claims — env first (CI/Docker builds set it),
 *  then the checkout itself. 'unknown' can never match a token claim. */
export function currentCommit(env = process.env, cwd = process.cwd()): string {
  if (env.NEKTAR_GIT_COMMIT) return env.NEKTAR_GIT_COMMIT.trim()
  try {
    return execFileSync('git', ['rev-parse', 'HEAD'], { cwd, stdio: ['ignore', 'pipe', 'ignore'] })
      .toString()
      .trim()
  } catch {
    return 'unknown'
  }
}

/** public keys allowed to certify conformance — never the agent's own key:
 *  NEKTAR_OPERATOR_PUB (comma-separated) or one pub hex per line in
 *  <dataDir>/operator.pub. Empty = no token can verify = fail closed. */
export function operatorPubs(dataDir?: string, env = process.env): string[] {
  const pubs = new Set<string>()
  for (const p of (env.NEKTAR_OPERATOR_PUB ?? '')
    .split(',')
    .map((s) => s.trim())
    .filter(Boolean))
    pubs.add(p.toLowerCase())
  if (dataDir !== undefined) {
    try {
      for (const l of readFileSync(join(dataDir, 'operator.pub'), 'utf8').split(/\r?\n/))
        if (l.trim() !== '' && !l.trim().startsWith('#')) pubs.add(l.trim().toLowerCase())
    } catch {
      /* no operator.pub */
    }
  }
  return [...pubs]
}

/** verify a token's own internal validity: signature, expiry, shape. Does
 *  not check commit/config/suite — that binding is the caller's job. */
export function verifyToken(token: ConformanceToken, allowedPubs: string[], now = Date.now()): { ok: boolean; reason?: string } {
  try {
    if (token.v !== 1 || typeof token.gitCommit !== 'string' || typeof token.configHash !== 'string' || typeof token.suiteDigest !== 'string')
      return { ok: false, reason: 'malformed claims' }
    if (!Number.isSafeInteger(token.issuedAt) || token.expiresAt !== token.issuedAt + TOKEN_TTL_MS) return { ok: false, reason: 'expiry is not issuedAt+7d' }
    if (token.issuedAt > now + 60_000) return { ok: false, reason: 'issued in the future' }
    if (now >= token.expiresAt) return { ok: false, reason: 'stale — scenarios:verify must re-run' }
    const pub = String(token.pub).toLowerCase()
    if (!allowedPubs.includes(pub)) return { ok: false, reason: 'signer is not an operator key' }
    const ok = PublicKey.fromString(token.pub).verify(Array.from(sha256(utf8(canon(token)))), Signature.fromDER(token.sig, 'hex'))
    return ok ? { ok: true } : { ok: false, reason: 'bad signature' }
  } catch (e) {
    return { ok: false, reason: `unreadable token (${(e as Error).message.slice(0, 80)})` }
  }
}

/** issue a token — called by scenarios:verify with the OPERATOR key (the
 *  agent's keystore is never involved). Fails before writing when the signer
 *  is not listed as an operator, so a test key can't forge confidence. */
export function issueToken(
  key: PrivateKey,
  claims: Omit<ConformanceClaims, 'v' | 'issuedAt' | 'expiresAt'>,
  allowedPubs: string[],
  now = Date.now()
): ConformanceToken {
  const pub = key.toPublicKey().toString()
  if (!allowedPubs.includes(pub.toLowerCase())) throw new Error('signer is not an operator key (NEKTAR_OPERATOR_PUB / operator.pub)')
  const c: ConformanceClaims = {
    v: 1,
    gitCommit: claims.gitCommit,
    configHash: claims.configHash,
    suiteDigest: claims.suiteDigest,
    issuedAt: now,
    expiresAt: now + TOKEN_TTL_MS
  }
  const sig = toHex(Uint8Array.from(key.sign(Array.from(sha256(utf8(canon(c))))).toDER() as number[]))
  return { ...c, pub, sig }
}

/** the full unattended check — token file → signature → commit/config/suite
 *  binding. Every failure mode names itself for the preflight detail line. */
export function conformanceStatus(cfg: {
  dataDir?: string
  network: string
  marketHosts?: string[]
  suiteDir?: string
  tokenFile?: string
  now?: number
  env?: NodeJS.ProcessEnv
}): { pass: boolean; detail: string; token?: ConformanceToken } {
  const env = cfg.env ?? process.env
  const file = cfg.tokenFile ?? (cfg.dataDir !== undefined ? join(cfg.dataDir, 'conformance.token') : undefined)
  if (file === undefined || !existsSync(file)) return { pass: false, detail: 'no conformance.token — run scenarios:verify to certify unattended operation' }
  let token: ConformanceToken
  try {
    token = JSON.parse(readFileSync(file, 'utf8')) as ConformanceToken
  } catch {
    return { pass: false, detail: 'conformance.token is unreadable JSON' }
  }
  const pubs = operatorPubs(cfg.dataDir, env)
  if (pubs.length === 0) return { pass: false, detail: 'no operator keys configured (NEKTAR_OPERATOR_PUB / operator.pub) — token cannot be trusted' }
  const v = verifyToken(token, pubs, cfg.now)
  if (!v.ok) return { pass: false, detail: `token invalid: ${v.reason}` }
  const commit = currentCommit(env)
  if (token.gitCommit !== commit)
    return { pass: false, detail: `token certifies commit ${token.gitCommit.slice(0, 12)} but this instance runs ${commit.slice(0, 12)}` }
  const ch = conformanceConfigHash(cfg)
  if (token.configHash !== ch) return { pass: false, detail: 'token was issued for a different config (network/hosts changed since)' }
  if (cfg.suiteDir !== undefined) {
    let sd = ''
    try {
      sd = suiteDigest(cfg.suiteDir)
    } catch {
      sd = ''
    }
    if (sd === '' || token.suiteDigest !== sd) return { pass: false, detail: 'token was issued against a different scenario suite' }
  }
  return { pass: true, detail: `valid until ${new Date(token.expiresAt).toISOString()} (commit ${token.gitCommit.slice(0, 12)})`, token }
}

/** the scenarios suite dir this repo ships — verify.mjs passes it explicitly;
 *  the default probes both the source-tree and compiled-dist depths. */
export function defaultSuiteDir(): string {
  const here = fileURLToPath(new URL('.', import.meta.url))
  for (const rel of ['../../e2e/src/scenarios', '../../../e2e/src/scenarios', '../../../../e2e/src/scenarios']) {
    const d = resolve(here, rel)
    if (existsSync(d)) return d
  }
  return resolve(here, '../../e2e/src/scenarios')
}
