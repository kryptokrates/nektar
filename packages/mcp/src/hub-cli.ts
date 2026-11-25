// hub-cli.ts — one-command Hub Node promotion & lifecycle (`nektar hub`).
// Lets any user graduate from a personal agent to an independent public hub
// with built-in Caddy Auto-TLS (custom domain, automatic <ip>.sslip.io, or
// zero-port Cloudflare Tunnel), auto-generated secrets, optional testnet
// faucet drip, and automatic join handshake back to their local agent.
import { spawnSync } from 'node:child_process'
import { randomBytes } from 'node:crypto'
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { homedir } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { nektarEnv, parseDotEnv, writeErr, writeOut, type EnvSource } from '@nektar/core'
import { discoverDaemon } from './daemon.js'
import { buildHubBundle } from './onboard.js'

const RAW_BASE = 'https://raw.githubusercontent.com/kryptokrates/nektar/master/deploy/hub'

const USAGE = `nektar hub — 1-command Hub Node deployment & promotion

  nektar hub up [options]               pull GHCR hub stack, auto-configure TLS & start
  nektar hub status [--dir <path>]      inspect hub containers, health & announced card
  nektar hub logs [--dir <path>]        follow hub agent logs
  nektar hub down [--dir <path>]        stop hub containers (volumes & keys preserved)
  nektar hub template <kind> [options]  print a 1-click template (coolify|dokploy|cloud-init|railway|render|fly|umbrel|casaos)

Options for \`up\`:
  --domain <fqdn>          Public domain with automatic Let's Encrypt TLS via Caddy
  --auto-ip                Zero-domain HTTPS via <public-ip>.sslip.io + Caddy Auto-TLS
  --tunnel                 Zero-port HTTPS via Cloudflare Quick Tunnel (works behind NAT)
  --network <testnet|main> Network (default: testnet; main requires --confirm I_UNDERSTAND)
  --confirm I_UNDERSTAND   Required acknowledgement for mainnet
  --dir <path>             Hub directory (default: ~/.nektar-hub)
  --dry                    Write .env/compose preview without starting Docker
  --no-faucet              Skip automatic testnet faucet drip
  --no-join                Skip auto-joining local agent to this new hub
`

const readFlag = (args: string[], name: string): string | undefined => {
  const i = args.indexOf(name)
  if (i !== -1 && i + 1 < args.length) return args[i + 1]
  const eq = args.find((a) => a.startsWith(`${name}=`))
  return eq?.slice(name.length + 1)
}

export interface HubCliIo {
  out?: (s: string) => void
  err?: (s: string) => void
  env?: EnvSource
  fetchImpl?: typeof fetch
  spawnSyncImpl?: typeof spawnSync
  homeDir?: string
  repoRoot?: string
}

function locateHubDeployDir(repoRoot?: string): string | undefined {
  const here = dirname(fileURLToPath(import.meta.url))
  const cands = [
    ...(repoRoot ? [join(repoRoot, 'deploy', 'hub')] : []),
    resolve(here, '..', '..', 'deploy', 'hub'),
    resolve(here, '..', '..', '..', '..', 'deploy', 'hub')
  ]
  return cands.find((c) => existsSync(join(c, 'compose.yaml')))
}

function setKv(content: string, key: string, value: string): string {
  const re = new RegExp(`^${key}=.*$`, 'm')
  if (re.test(content)) return content.replace(re, `${key}=${value}`)
  return content.trimEnd() + `\n${key}=${value}\n`
}

async function detectPublicIp(fetchImpl: typeof fetch): Promise<string | undefined> {
  for (const url of ['https://api.ipify.org', 'https://ifconfig.me/ip']) {
    try {
      const r = await fetchImpl(url, { signal: AbortSignal.timeout(4_000) })
      if (!r.ok) continue
      const ip = (await r.text()).trim()
      if (/^\d{1,3}(\.\d{1,3}){3}$/.test(ip)) return ip
    } catch {
      /* try next */
    }
  }
  return undefined
}

export async function runHubCli(argv: string[], io: HubCliIo = {}): Promise<number> {
  const out = io.out ?? writeOut
  const err = io.err ?? writeErr
  const env = io.env ?? process.env
  const fetchImpl = io.fetchImpl ?? fetch
  const runCmd = io.spawnSyncImpl ?? spawnSync
  const home = io.homeDir ?? homedir()

  const sub = argv[0]
  if (!sub || sub === 'help' || argv.includes('--help') || argv.includes('-h')) {
    out(USAGE)
    return 0
  }

  const hubDir = resolve(readFlag(argv, '--dir') ?? nektarEnv('NEKTAR_HUB_DIR', env) ?? join(home, '.nektar-hub'))
  const localDeploy = locateHubDeployDir(io.repoRoot)

  if (sub === 'template') {
    const kind = (argv[1] ?? '').toLowerCase()
    const domain = readFlag(argv, '--domain')
    const net = readFlag(argv, '--network') ?? 'testnet'
    const mode = argv.includes('--tunnel') ? 'tunnel' : argv.includes('--auto-ip') ? 'auto-ip' : 'domain'
    const map: Record<string, string> = {
      coolify: 'coolify.compose.yaml',
      dokploy: 'dokploy.compose.yaml',
      'cloud-init': 'cloud-init.yaml',
      railway: 'railway.json',
      render: 'render.yaml',
      fly: 'fly.toml',
      umbrel: 'umbrel/docker-compose.yml',
      casaos: 'casaos.yaml'
    }
    const rel = map[kind]
    if (!rel) {
      err(`nektar hub template: unknown kind "${kind}" — pick one of: ${Object.keys(map).join(', ')}\n`)
      return 2
    }
    if (kind === 'cloud-init') {
      out(buildHubBundle({ domain, network: net, mode }).cloudInitYaml)
      return 0
    }
    const localFile = localDeploy ? join(localDeploy, 'templates', rel) : undefined
    if (localFile && existsSync(localFile)) {
      out(readFileSync(localFile, 'utf8'))
      return 0
    }
    try {
      const r = await fetchImpl(`${RAW_BASE}/templates/${rel}`, { signal: AbortSignal.timeout(10_000) })
      if (!r.ok) {
        err(`nektar hub template: fetch failed (HTTP ${r.status})\n`)
        return 1
      }
      out(await r.text())
      return 0
    } catch (e) {
      err(`nektar hub template: ${(e as Error).message}\n`)
      return 1
    }
  }

  if (sub === 'down' || sub === 'logs' || sub === 'status') {
    if (!existsSync(join(hubDir, 'compose.yaml'))) {
      err(`nektar hub: no hub found in ${hubDir} — start one with \`nektar hub up\`\n`)
      return 1
    }
    if (sub === 'down') {
      const r = runCmd('docker', ['compose', '-f', join(hubDir, 'compose.yaml'), 'down'], { stdio: 'inherit' })
      return r.status ?? 1
    }
    if (sub === 'logs') {
      const r = runCmd('docker', ['compose', '-f', join(hubDir, 'compose.yaml'), 'logs', '-f', 'agent'], { stdio: 'inherit' })
      return r.status ?? 1
    }
    const ps = runCmd('docker', ['compose', '-f', join(hubDir, 'compose.yaml'), 'ps'], { stdio: 'inherit' })
    const envMap: Record<string, string | undefined> = existsSync(join(hubDir, '.env')) ? parseDotEnv(readFileSync(join(hubDir, '.env'), 'utf8')) : {}
    out(`\nhub dir     : ${hubDir}\n`)
    out(`network     : ${envMap.NEKTAR_NETWORK ?? 'testnet'}\n`)
    out(`public base : ${envMap.NEKTAR_PUBLIC_URL ?? envMap.NEKTAR_DOMAIN ?? 'localhost'}\n`)
    return ps.status ?? 0
  }

  if (sub !== 'up') {
    err(`nektar hub: unknown subcommand "${sub}"\n${USAGE}`)
    return 2
  }

  const net = readFlag(argv, '--network') ?? nektarEnv('NEKTAR_NETWORK', env) ?? 'testnet'
  if (net !== 'testnet' && net !== 'main') {
    err(`nektar hub up: --network must be testnet|main, got "${net}"\n`)
    return 2
  }
  const confirm = readFlag(argv, '--confirm') ?? nektarEnv('NEKTAR_MAINNET_CONFIRM', env)
  if (net === 'main' && confirm !== 'I_UNDERSTAND') {
    err('nektar hub up: refusing mainnet without --confirm I_UNDERSTAND\n')
    return 2
  }

  const dry = argv.includes('--dry')
  const useTunnel = argv.includes('--tunnel')
  const useAutoIp = argv.includes('--auto-ip')
  let domain = readFlag(argv, '--domain')
    ?.trim()
    .replace(/^https?:\/\//i, '')
    .replace(/\/.*$/, '')

  if (!domain && useAutoIp) {
    const ip = await detectPublicIp(fetchImpl)
    if (!ip) {
      err('nektar hub up: --auto-ip could not detect public IPv4 — pass --domain <fqdn> or --tunnel\n')
      return 1
    }
    domain = `${ip.replace(/\./g, '-')}.sslip.io`
    out(`nektar hub: resolved zero-domain TLS host → https://${domain}\n`)
  }

  mkdirSync(hubDir, { recursive: true })
  const composeDst = join(hubDir, 'compose.yaml')
  const envExDst = join(hubDir, '.env.example')
  if (localDeploy && existsSync(join(localDeploy, 'compose.yaml'))) {
    writeFileSync(composeDst, readFileSync(join(localDeploy, 'compose.yaml'), 'utf8'))
    if (existsSync(join(localDeploy, '.env.example'))) writeFileSync(envExDst, readFileSync(join(localDeploy, '.env.example'), 'utf8'))
  } else {
    for (const [name, dst] of [
      ['compose.yaml', composeDst],
      ['.env.example', envExDst]
    ] as const) {
      const r = await fetchImpl(`${RAW_BASE}/${name}`, { signal: AbortSignal.timeout(15_000) })
      if (!r.ok) {
        err(`nektar hub up: failed to fetch ${name} (HTTP ${r.status})\n`)
        return 1
      }
      writeFileSync(dst, await r.text())
    }
  }

  const envPath = join(hubDir, '.env')
  let envText = existsSync(envPath) ? readFileSync(envPath, 'utf8') : existsSync(envExDst) ? readFileSync(envExDst, 'utf8') : ''
  const parsed = parseDotEnv(envText)
  for (const secretKey of ['NEKTAR_KEY_PASSPHRASE', 'NEKTAR_HTTP_TOKEN', 'NEKTAR_OPERATOR_TOKEN', 'OVERLAY_DB_PASSWORD', 'OVERLAY_ADMIN_TOKEN']) {
    if (!parsed[secretKey]) envText = setKv(envText, secretKey, randomBytes(32).toString('hex'))
  }
  envText = setKv(envText, 'NEKTAR_NETWORK', net)
  if (net === 'main') envText = setKv(envText, 'NEKTAR_MAINNET_CONFIRM', 'I_UNDERSTAND')

  const profiles: string[] = []
  if (domain) {
    const pub = `https://${domain}`
    const hosts = net === 'testnet' ? `${pub}/hosts/a,${pub}/hosts/b,https://nektar.run/hosts/a,https://nektar.run/hosts/b` : `${pub}/hosts/a,${pub}/hosts/b`
    envText = setKv(envText, 'NEKTAR_DOMAIN', domain)
    envText = setKv(envText, 'NEKTAR_PUBLIC_URL', pub)
    envText = setKv(envText, 'NEKTAR_MARKET_HOSTS', hosts)
    envText = setKv(envText, 'NEKTAR_PUSH_PUBLIC', pub)
    envText = setKv(envText, 'NEKTAR_WATCH_URLS', `${pub}/watch/a,${pub}/watch/b`)
    envText = setKv(envText, 'NEKTAR_WATCH_PUBLIC', `${pub}/watch`)
    envText = setKv(envText, 'NEKTAR_ANNOUNCE', '1')
    profiles.push('tls')
  } else if (useTunnel) {
    envText = setKv(envText, 'NEKTAR_DOMAIN', ':80')
    envText = setKv(envText, 'NEKTAR_MARKET_HOSTS', 'http://host-a:8080,http://host-b:8080')
    envText = setKv(envText, 'NEKTAR_ANNOUNCE', '0')
    profiles.push('tls', 'tunnel')
  } else {
    envText = setKv(envText, 'NEKTAR_MARKET_HOSTS', 'http://localhost:18391,http://localhost:18392')
    envText = setKv(envText, 'NEKTAR_ANNOUNCE', '0')
  }

  writeFileSync(envPath, envText, { mode: 0o600 })
  out(`nektar hub: prepared ${hubDir}/.env (${net}, ${domain ? `TLS https://${domain}` : useTunnel ? 'Cloudflare Tunnel' : 'localhost'})\n`)

  if (dry) {
    out(`nektar hub [dry]: skipping docker compose up\n`)
    return 0
  }

  const args = ['compose', '-f', composeDst, '--env-file', envPath]
  for (const p of profiles) args.push('--profile', p)
  args.push('up', '-d')
  const up = runCmd('docker', args, { stdio: 'inherit' })
  if (up.status !== 0) return up.status ?? 1

  // Optional auto-join back to local personal agent
  if (domain && !argv.includes('--no-join')) {
    const localDaemon = await discoverDaemon({ env, fetchImpl, timeoutMs: 500 })
    const opTok = nektarEnv('NEKTAR_OPERATOR_TOKEN', env) ?? localDaemon?.token
    if (localDaemon && opTok) {
      await fetchImpl(`${localDaemon.url}/v1/hosts`, {
        method: 'POST',
        headers: { 'content-type': 'application/json', authorization: `Bearer ${opTok}` },
        body: JSON.stringify({ url: `https://${domain}` }),
        signal: AbortSignal.timeout(5_000)
      }).catch(() => {})
    }
  }

  out(`\nnektar hub UP in ${hubDir}\n`)
  out(`  status : nektar hub status --dir "${hubDir}"\n`)
  out(`  logs   : nektar hub logs --dir "${hubDir}"\n`)
  if (domain) out(`  public : https://${domain}/.well-known/nektar\n`)
  else if (useTunnel) out(`  tunnel : docker compose -f "${composeDst}" logs cloudflared | grep trycloudflare\n`)
  return 0
}
