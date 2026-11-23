// endpoint.ts — serve the service handlers in-process (the restate server
// invokes them over HTTP/2) and register the deployment with the server's
// admin API. Registration is idempotent — the server dedupes by URI.
// The executor plane is mandatory: openAutomation (market on) and
// openHostPlane (market off) are the two registration paths — exactly one
// runs per process, and there is no Restate-less boot.
import { createServer, type Http2Server, type ServerHttp2Session } from 'node:http2'
import { createHash } from 'node:crypto'
import { readFileSync, writeFileSync, mkdirSync } from 'node:fs'
import { join } from 'node:path'
import { nektarEnv, nektarEnvNum, nap, type EnvSource } from '@nektar/core'
import { WAITS } from '../waits.js'

export interface RtEndpoint {
  /** the URI the server invokes handlers at */
  uri: string
  close: () => Promise<void>
}

interface AdminDeployment {
  id?: string
  uri?: string
  services?: Array<{ name?: string; handlers?: Array<{ name?: string }> }>
}

/** handler names off either shape — the admin manifest carries an array of
 *  {name}, the SDK def a Record keyed by handler name. */
const handlerNames = (h: unknown): string[] => {
  if (h === null || h === undefined || typeof h !== 'object') return []
  if (Array.isArray(h)) return h.map((x) => (x as { name?: string }).name ?? '?').sort()
  return Object.keys(h).sort()
}

/** the endpoint's manifest fingerprint — service names + their handler
 *  names, read off the SDK defs (or the admin deployment's services). A def
 *  that won't yield its name produces a marker, so an unreadable manifest
 *  mismatches (forces registration) instead of crashing the compare. */
const svcManifest = (svc: unknown): string => {
  const s = svc as { name?: string; handlers?: unknown; definition?: unknown }
  const def = (s.definition ?? s) as { name?: string; handlers?: unknown }
  return `${def.name ?? '?'}:${handlerNames(def.handlers).join(',')}`
}

const manifestOf = (services: unknown[] | undefined): string[] | undefined => services?.map(svcManifest).sort()

/** the def's own service names — paused-invocation heal must scope to them:
 *  on a shared restate server an unscoped resume wedges OTHER apps' legs
 *  back into legs we never owned (and our deployment-rebind would re-pin
 *  them onto us). */
/** exported so host-plane tests record service names with the exact
 *  extraction the endpoint itself uses (never a re-derived copy). */
export const svcName = (svc: unknown): string | undefined => {
  const s = svc as { name?: string; definition?: { name?: string } }
  return s.definition?.name ?? s.name
}

/** what we actually serve — service names + handler names + the handler
 *  OPTIONS the def carries (RT_OPTS_* changes are a real manifest change:
 *  a same-name re-register keeps the OLD options, and the admin manifest
 *  doesn't reliably serve options back for a compare). The fingerprint is
 *  stored next to the data dir after a successful registration and a drift
 *  forces a re-register — comparing to our own record needs no admin schema. */
const optionsFingerprint = (services: unknown[]): string => {
  const seen = new Set<unknown>()
  const stable = (v: unknown): string => {
    if (v === null || typeof v === 'number' || typeof v === 'string' || typeof v === 'boolean') return JSON.stringify(v)
    if (typeof v === 'function' || typeof v === 'symbol' || typeof v === 'bigint' || v === undefined) return '∅'
    if (Array.isArray(v)) return `[${v.map(stable).join(',')}]`
    if (typeof v === 'object') {
      if (seen.has(v)) return '∅'
      seen.add(v)
      const o = v as Record<string, unknown>
      // transport bits that exist on a def object but aren't part of the
      // served contract are skipped — they're identity, not options
      return `{${Object.keys(o)
        .filter((k) => !['component', 'discovery', 'logger', 'set'].includes(k))
        .sort()
        .map((k) => `${JSON.stringify(k)}:${stable(o[k])}`)
        .join(',')}}`
    }
    return '∅'
  }
  return createHash('sha256').update(services.map(stable).join('|')).digest('hex')
}

const manifestMatches = async (admin: string, uri: string, expected: string[]): Promise<boolean> => {
  try {
    const r = await fetch(`${admin}/deployments`, { signal: AbortSignal.timeout(WAITS.registerIo.ms) })
    if (!r.ok) return false
    const body = (await r.json()) as { deployments?: AdminDeployment[] } | AdminDeployment[]
    const list = Array.isArray(body) ? body : (body.deployments ?? [])
    // the admin normalizes the registered uri (trailing slash) — a strict
    // compare never matches → force-register every boot → orphaned sends
    const want = uri.replace(/\/+$/, '')
    const mine = list.filter((d) => (d.uri ?? '').replace(/\/+$/, '') === want)
    if (mine.length === 0) return false
    let newest = mine[mine.length - 1]
    // the list response may carry services inline; fetch detail when not
    if (newest.services === undefined && newest.id !== undefined) {
      const d = await fetch(`${admin}/deployments/${newest.id}`, { signal: AbortSignal.timeout(WAITS.registerIo.ms) })
      if (d.ok) newest = (await d.json()) as AdminDeployment
    }
    const served = manifestOf(newest.services)
    // a manifest we can't read mismatches — force:true is the safe answer,
    // never a compare-failure veto
    return served !== undefined && served.join('') === expected.join('')
  } catch {
    return false
  }
}

/** POST /deployments to the admin API — retries while the server warms up.
 *  force: true re-discovers the manifest — required when services/handlers
 *  changed (a same-URI re-register without force returns the OLD deployment
 *  and a new handler silently never registers). force: false keeps the
 *  existing deployment: armed delayed sends survive the restart (a forced
 *  re-register orphans them — RT0011 — and the domain floors then carry
 *  the re-arm). */
export async function registerDeployment(admin: string, uri: string, log: (line: string) => void, force = true): Promise<boolean> {
  for (let i = 0; i < 20; i++) {
    try {
      const r = await fetch(`${admin}/deployments`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ uri, force }),
        signal: AbortSignal.timeout(WAITS.registerIo.ms)
      })
      if (r.ok || r.status === 409) {
        log(`rt: restate deployment registered ${uri} (force=${force})`)
        return true
      }
      log(`rt: deployment registration ${r.status} ${await r.text().catch(() => '')}`)
    } catch {
      // server still starting
    }
    await nap(WAITS.registerRetry.ms)
  }
  log(`rt: restate deployment registration failed ${uri} — invocations will not reach this endpoint`)
  return false
}

/** paused-invocation count on the plane — a paused head wedges its object's
 *  FIFO invisibly (retry-policy 'pause' parks a leg after maxAttempts; every
 *  later send on that key queues behind it forever). The operator surfaces
 *  read this so a wedge is a loud number, not a silent stall. undefined =
 *  admin unreachable (the plane itself may be down — equally loud). */
export const pausedInvocations = async (admin: string, ioMs = 3_000): Promise<number | undefined> => {
  try {
    const r = await fetch(`${admin.replace(/\/+$/, '')}/query`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'application/json' },
      body: JSON.stringify({ query: "select count(*) c from sys_invocation where status = 'paused'" }),
      signal: AbortSignal.timeout(ioMs)
    })
    if (!r.ok) return undefined
    const j = (await r.json()) as { rows?: Array<{ c?: number | string }> }
    const n = Number(j.rows?.[0]?.c ?? 0)
    return Number.isFinite(n) ? n : undefined
  } catch {
    return undefined
  }
}

/** paused invocations the handler never caused. A pause is a circuit
 *  breaker — but two infrastructure shapes pause legs on a fault they never
 *  saw: (a) journal_size <= 1 — paused at delivery/inbox before user code
 *  ran (deployment gap, h2 refuse during restart); (b) a journaled leg
 *  whose last failure is delivery-shaped — replay re-runs the journal and
 *  continues. Both resume clean. A journaled leg with a domain/deterministic
 *  failure stays paused — that IS the breaker's job (the journaled error
 *  would replay identically anyway). */
/** unambiguous wire/deployment failure vocabulary — the ONLY failure text
 *  allowed to defeat the breaker on a journaled leg. 'timed out' and
 *  'temporarily unavailable' are deliberately absent: a handler-side
 *  TimeoutError reads exactly like that, and resuming a domain failure
 *  forever is the breaker never holding. journal_size <= 1 legs need no
 *  text match at all — user code never ran, so no domain failure exists. */
const DEPLOYMENT_PAUSE = /h2 pool|connection refused|network is unreachable|connection reset|econnrefused|cannot connect|deployment.*(not found|unavailable)/i

/** target's service name — sys_invocation.target is 'svc/handler' for
 *  services and 'svc.key/handler' (or 'svc/key/handler') for objects; the
 *  service segment is always first. */
const targetService = (t: string): string => t.split('/')[0].split('.')[0]

/** restart-gap wedge tracking — a paused invocation with journal_size > 1
 *  and NO last_failure is the mid-Run kill class: the process died inside a
 *  journaled leg and restate's resume accepts (200) yet never re-dispatches
 *  (observed live: PATCH resume → running_at frozen, status stays paused).
 *  A journaled DOMAIN failure always records last_failure — those keep the
 *  breaker. So: resume once, and if the leg is still paused on the next
 *  pass, DELETE it — invocations are scheduling, the durable intent
 *  (wallet_tx state, outbox rows) re-drives the work via resubmitPending /
 *  advance pokes. Process-scoped map is enough: the id is only marked while
 *  it stays paused; a cleared or re-paused-after-boot leg resumes once more
 *  before its kill — worst case one extra window, never an instant kill. */
const resumeMarks = new Map<string, number>()
const RESUME_MARK_CAP = 256

export const healPausedInvocations = async (
  admin: string,
  uri: string,
  log: (line: string) => void,
  ioMs = 3_000,
  cap = 32,
  ours?: ReadonlySet<string>
): Promise<number> => {
  const a = admin.replace(/\/+$/, '')
  try {
    const r = await fetch(`${a}/query`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', accept: 'application/json' },
      body: JSON.stringify({
        query: "select id, target, last_failure, journal_size from sys_invocation where status = 'paused' limit 256"
      }),
      signal: AbortSignal.timeout(ioMs)
    })
    if (!r.ok) return 0
    const j = (await r.json()) as {
      rows?: Array<{ id?: string; target?: string; last_failure?: string; journal_size?: number | string }>
    }
    const ours_ = (x: { target?: string }): boolean => ours === undefined || (x.target !== undefined && ours.has(targetService(x.target)))
    const paused = (j.rows ?? []).filter((x) => x.id && ours_(x))
    // two distinct wounds with two distinct cures:
    //  - resumable: never ran (js<=1) or a wire/deployment-shaped failure
    //  - restartGap: journaled (js>1), NO failure recorded — resume no-ops
    //    on this class (the dead Run entry can never re-dispatch), so the
    //    second sighting kills it and the durable intent re-drives the work
    const resumable = paused.filter((x) => Number(x.journal_size ?? 0) <= 1 || DEPLOYMENT_PAUSE.test(x.last_failure ?? '')).slice(0, cap)
    const restartGap = paused.filter((x) => Number(x.journal_size ?? 0) > 1 && !(x.last_failure ?? '').trim()).slice(0, cap)
    if (resumeMarks.size > RESUME_MARK_CAP) resumeMarks.clear()
    for (const id of resumeMarks.keys()) if (!restartGap.some((x) => x.id === id)) resumeMarks.delete(id)
    if (resumable.length === 0 && restartGap.length === 0) return 0
    // our deployment id is the fallback rebind — a paused leg whose pinned
    // deployment was deleted can't plain-resume ('deployment id not pinned')
    let depId: string | undefined
    const dr = await fetch(`${a}/deployments`, { signal: AbortSignal.timeout(ioMs) })
    if (dr.ok) {
      const body = (await dr.json()) as { deployments?: AdminDeployment[] } | AdminDeployment[]
      const list = Array.isArray(body) ? body : (body.deployments ?? [])
      const want = uri.replace(/\/+$/, '')
      depId = [...list].reverse().find((d) => (d.uri ?? '').replace(/\/+$/, '') === want)?.id
    }
    let healed = 0
    for (const x of resumable) {
      const id = encodeURIComponent(x.id as string)
      let ok = false
      try {
        ok = (await fetch(`${a}/invocations/${id}/resume`, { method: 'PATCH', signal: AbortSignal.timeout(ioMs) })).ok
      } catch {
        /* fall through to the rebind attempt */
      }
      if (!ok && depId) {
        try {
          ok = (
            await fetch(`${a}/invocations/${id}/resume?deployment=${depId}`, {
              method: 'PATCH',
              signal: AbortSignal.timeout(ioMs)
            })
          ).ok
        } catch {
          /* next boot tries again */
        }
      }
      if (ok) healed++
      else log(`rt: paused invocation ${x.id} (${x.target}) did not resume — inspect manually`)
    }
    if (healed > 0) log(`rt: resumed ${healed} transport-paused invocation(s) — restart-gap wedge cleared`)
    let killed = 0
    for (const x of restartGap) {
      const id = encodeURIComponent(x.id as string)
      if (!resumeMarks.has(x.id as string)) {
        // first sighting — give resume its chance; if it re-pauses the
        // mark carries to the next pass and the leg is killed below
        try {
          await fetch(`${a}/invocations/${id}/resume`, { method: 'PATCH', signal: AbortSignal.timeout(ioMs) })
        } catch {
          /* mark anyway — a dead admin kills on sighting two as well */
        }
        resumeMarks.set(x.id as string, Date.now())
        continue
      }
      try {
        const kr = await fetch(`${a}/invocations/${id}`, { method: 'DELETE', signal: AbortSignal.timeout(ioMs) })
        if (kr.ok || kr.status === 404) {
          killed++
          resumeMarks.delete(x.id as string)
          log(`rt: killed restart-gap invocation ${x.id} (${x.target}) — resume cannot re-dispatch a dead Run; the durable row re-drives it`)
        }
      } catch {
        /* next pass tries again */
      }
    }
    return healed + killed
  } catch {
    return 0
  }
}

export async function openEndpoint(services: unknown[], env: EnvSource = process.env, log: (line: string) => void = () => {}): Promise<RtEndpoint> {
  // the SDK loads only on this path — an injected Dispatcher never pays its module cost
  const restate = await import('@restatedev/restate-sdk')
  const port = nektarEnvNum('NEKTAR_RESTATE_ENDPOINT_PORT', env) ?? 9080
  const uri = nektarEnv('NEKTAR_RESTATE_ENDPOINT_URI', env) ?? `http://127.0.0.1:${port}`
  const admin = nektarEnv('NEKTAR_RESTATE_ADMIN', env) ?? 'http://127.0.0.1:9070'
  // request-signing lockdown — NEKTAR_RESTATE_IDENTITY_KEYS carries the
  // server's publickeyv1_* keys (comma-separated); set only when the server
  // signs invocations. Off by default: dev/loopback topologies run unsigned.
  const identityKeys = (nektarEnv('NEKTAR_RESTATE_IDENTITY_KEYS', env) ?? '')
    .split(',')
    .map((k) => k.trim())
    .filter((k) => k.length > 0)
  const handler = restate.createEndpointHandler({ services: services as never, ...(identityKeys.length > 0 ? { identityKeys } : {}) })
  const server: Http2Server = createServer(handler)
  // http2 has no closeAllConnections — track sessions so close() can force-
  // destroy in-flight invocations (crash semantics: they retry server-side)
  const sessions = new Set<ServerHttp2Session>()
  server.on('session', (s) => {
    sessions.add(s)
    s.on('close', () => sessions.delete(s))
  })
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, () => resolve())
  })
  // plain restarts keep the existing deployment (armed sends survive); only
  // a changed manifest forces a new one (the RT0011 orphan class the floors
  // cover). A registration that never lands = a dead executor plane — the
  // runtime must NOT start: a live process executing nothing is worse than
  // a refused boot.
  // a loopback advertise is right for a host-process sidecar and silently
  // fatal for a containerized one — the URI resolves inside the container.
  // Registration itself may still succeed (the admin call works; delivery
  // retries are what die), so name the fix before the wedge forms.
  // KUBERNETES_SERVICE_HOST is always set in a pod — there the sidecar is
  // same-pod and loopback is the correct address, so the warning is skipped.
  if (/^https?:\/\/(127\.|localhost|\[?::1\]?)/.test(uri) && !env.KUBERNETES_SERVICE_HOST)
    log(
      `automation: endpoint URI ${uri} is loopback — a containerized sidecar cannot reach it; set NEKTAR_RESTATE_ENDPOINT_URI to a container-routable address (Docker Desktop: http://192.168.65.254:${port} — plain IP, not host.docker.internal, whose DNS can answer an unroutable IPv6)`
    )
  const expected = manifestOf(services) ?? []
  // the served-contract fingerprint includes handler options — a manifest
  // name-match with changed options still forces (an RT_OPTS_* bump that
  // never registered is the silent-stale-deployment class). Compares
  // against our own record: the admin manifest needn't serve options back.
  const fp = optionsFingerprint(services)
  const fpFile = join(nektarEnv('NEKTAR_DATA_DIR', env) ?? process.cwd(), 'restate-deploy.json')
  let lastFp: string | undefined
  try {
    lastFp = (JSON.parse(readFileSync(fpFile, 'utf8')) as { fp?: string }).fp
  } catch {
    /* no prior record — first boot or wiped dir */
  }
  const force = expected.length === 0 || !(await manifestMatches(admin, uri, expected)) || lastFp !== fp
  if (!(await registerDeployment(admin, uri, log, force))) {
    server.close()
    throw new Error(`automation: restate deployment registration failed ${uri} — the executor plane is required infrastructure, refusing to start without it`)
  }
  try {
    mkdirSync(join(fpFile, '..'), { recursive: true })
    writeFileSync(fpFile, JSON.stringify({ fp, uri }))
  } catch {
    /* a lost record forces the next boot — fail-safe direction */
  }
  // heal scope: only OUR services' paused legs are ours to resume
  const ourNames = new Set(services.map(svcName).filter((n): n is string => n !== undefined))
  // a restart gaps this endpoint for seconds; retry-policy 'pause' parks
  // whatever the server was delivering then — wedging whole keys on a
  // fault the handler never saw. Resume the transport-paused set now that
  // delivery works again, then keep a floor cadence: pauses form whenever
  // retries exhaust, which can be seconds after this pass (mid-run network
  // flaps pause later still). Deterministic failures stay paused (the
  // restate-plane preflight keeps reporting them).
  await healPausedInvocations(admin, uri, log, 3_000, 32, ourNames)
  let healing = false
  const healFloor = setInterval(() => {
    if (healing) return
    healing = true
    void healPausedInvocations(admin, uri, log, 3_000, 32, ourNames).finally(() => {
      healing = false
    })
  }, WAITS.healPaused.ms)
  healFloor.unref()
  return {
    uri,
    // crash semantics, not graceful drain: destroying sessions drops
    // in-flight handler work on the spot — the invocations retry on the
    // next endpoint and the journal guarantees no double effect. A
    // graceful close() would wait on every session; a wedged leg could
    // hang shutdown forever (the durable-execution model makes force-close
    // the honest shutdown, not a risk).
    close: () =>
      new Promise((resolve) => {
        clearInterval(healFloor)
        for (const s of sessions) s.destroy()
        server.close(() => resolve())
      })
  }
}
