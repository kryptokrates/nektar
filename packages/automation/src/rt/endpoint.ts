// endpoint.ts — serve the service handlers in-process (the restate server
// invokes them over HTTP/2) and register the deployment with the server's
// admin API. Registration is idempotent — the server dedupes by URI.
import { createServer, type Http2Server } from 'node:http2'
import { nektarEnv, nektarEnvNum, nap, type EnvSource } from '@nektar/core'
import { WAITS } from '../waits.js'

export interface RtEndpoint {
  /** the URI the server invokes handlers at */
  uri: string
  close: () => Promise<void>
}

/** POST /deployments to the admin API — retries while the server warms up.
 *  force: true re-discovers the manifest every boot — force:false + a known
 *  URI returns the OLD deployment without looking at new services, which is
 *  how a newly added handler can silently never register. */
export async function registerDeployment(admin: string, uri: string, log: (line: string) => void): Promise<boolean> {
  for (let i = 0; i < 20; i++) {
    try {
      const r = await fetch(`${admin}/deployments`, {
        method: 'POST',
        headers: { 'content-type': 'application/json' },
        body: JSON.stringify({ uri, force: true }),
        signal: AbortSignal.timeout(WAITS.registerIo.ms)
      })
      if (r.ok || r.status === 409) {
        log(`rt: restate deployment registered ${uri}`)
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
  const handler = restate.createEndpointHandler({ services: services as never })
  const server: Http2Server = createServer(handler)
  await new Promise<void>((resolve, reject) => {
    server.once('error', reject)
    server.listen(port, () => resolve())
  })
  await registerDeployment(admin, uri, log)
  return {
    uri,
    close: () => new Promise((resolve) => server.close(() => resolve()))
  }
}
