// endpoint.ts — serve the service handlers in-process (the restate server
// invokes them over HTTP/2) and register the deployment with the server's
// admin API. Registration is idempotent — the server dedupes by URI.
import { createServer, type Http2Server, type ServerHttp2Session } from 'node:http2'
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

const manifestMatches = async (admin: string, uri: string, expected: string[]): Promise<boolean> => {
  try {
    const r = await fetch(`${admin}/deployments`, { signal: AbortSignal.timeout(WAITS.registerIo.ms) })
    if (!r.ok) return false
    const body = (await r.json()) as { deployments?: AdminDeployment[] } | AdminDeployment[]
    const list = Array.isArray(body) ? body : (body.deployments ?? [])
    const mine = list.filter((d) => d.uri === uri)
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
  const expected = manifestOf(services) ?? []
  const force = expected.length === 0 || !(await manifestMatches(admin, uri, expected))
  if (!(await registerDeployment(admin, uri, log, force))) {
    server.close()
    throw new Error(`automation: restate deployment registration failed ${uri} — the executor plane is required infrastructure, refusing to start without it`)
  }
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
        for (const s of sessions) s.destroy()
        server.close(() => resolve())
      })
  }
}
