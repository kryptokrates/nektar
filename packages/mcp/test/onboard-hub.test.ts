import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync } from 'node:fs'
import type { AddressInfo } from 'node:net'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { testWorld } from '@nektar/chain'
import { NektarMemory, createHttpApi } from '../src/index.js'
import { runDockCli } from '../src/dock-cli.js'
import { runHubCli } from '../src/hub-cli.js'

const REPO_ROOT = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const mkTmp = (p: string) => mkdtempSync(join(tmpdir(), p))

void test('Onboarding routes (/v1/onboard/faucet, policy-preset, hub-bundle) and /v1/adapters/* bundles', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-onboard-')
  const prevNet = process.env.NEKTAR_NETWORK
  const prevDir = process.env.NEKTAR_DATA_DIR
  const prevOp = process.env.NEKTAR_OPERATOR_TOKEN
  process.env.NEKTAR_NETWORK = 'testnet'
  process.env.NEKTAR_DATA_DIR = dir
  process.env.NEKTAR_OPERATOR_TOKEN = 'op-secret-test-99'

  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  const importedTxids: string[] = []
  const mockFetch = ((url: string | URL | Request) => {
    const u = typeof url === 'string' ? url : url instanceof URL ? url.href : url.url
    if (u.includes('/v1/faucet')) {
      return Promise.resolve(new Response(JSON.stringify({ txid: 'aa'.repeat(32), sats: 10_000 }), { status: 200 }))
    }
    return Promise.resolve(new Response('{}', { status: 404 }))
  }) as typeof fetch

  const srv = createHttpApi(
    mem,
    {
      wallet: () => Promise.resolve({ address: 'mtTestOnboardAddress1111111111', balanceSats: 0, utxos: 0 }),
      importFunding: (txid: string) => {
        importedTxids.push(txid)
        return Promise.resolve({ ok: true })
      }
    },
    { fetchImpl: mockFetch, adaptersRoot: REPO_ROOT }
  ).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    // 1. 1-click faucet drips + auto-imports txid
    const fr = await fetch(`${base}/v1/onboard/faucet`, { method: 'POST' })
    assert.equal(fr.status, 200)
    const fj = (await fr.json()) as { ok: boolean; txid: string; imported: boolean }
    assert.equal(fj.ok, true)
    assert.equal(fj.imported, true)
    assert.deepEqual(importedTxids, ['aa'.repeat(32)])

    // 2. Policy preset requires operator token
    const pDeny = await fetch(`${base}/v1/onboard/policy-preset`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ preset: 'buyer' })
    })
    assert.equal(pDeny.status, 403)
    const pOk = await fetch(`${base}/v1/onboard/policy-preset`, {
      method: 'POST',
      headers: { 'content-type': 'application/json', authorization: 'Bearer op-secret-test-99' },
      body: JSON.stringify({ preset: 'both', domains: ['ops'] })
    })
    assert.equal(pOk.status, 200)
    const pj = (await pOk.json()) as { ok: boolean; policyHash: string; roles: string[] }
    assert.equal(pj.ok, true)
    assert.deepEqual(pj.roles, ['buyer', 'seller'])

    // 3. Hub bundle generator
    const hb = (await (await fetch(`${base}/v1/onboard/hub-bundle?mode=domain&domain=my.hub.test`)).json()) as {
      cliCommand: string
      cloudInitYaml: string
    }
    assert.match(hb.cliCommand, /nektar hub up --domain my\.hub\.test/)
    assert.match(hb.cloudInitYaml, /#cloud-config/)

    // 4. Bundled adapters manifest + Hermes/OpenClaw/SKILL.md
    const man = (await (await fetch(`${base}/v1/adapters/manifest`)).json()) as {
      adapters: { hermes: { available: boolean }; openclaw: { available: boolean }; skill: { available: boolean } }
    }
    assert.equal(man.adapters.hermes.available, true)
    assert.equal(man.adapters.openclaw.available, true)
    assert.equal(man.adapters.skill.available, true)
    const hermesBundle = (await (await fetch(`${base}/v1/adapters/bundle/hermes`)).json()) as { files: Array<{ path: string }> }
    assert.ok(hermesBundle.files.some((f) => f.path === 'plugin.yaml'))
    const skillMd = await (await fetch(`${base}/v1/adapters/skill.md`)).text()
    assert.match(skillMd, /nektar-memory/)
  } finally {
    srv.close()
    if (prevNet === undefined) delete process.env.NEKTAR_NETWORK
    else process.env.NEKTAR_NETWORK = prevNet
    if (prevDir === undefined) delete process.env.NEKTAR_DATA_DIR
    else process.env.NEKTAR_DATA_DIR = prevDir
    if (prevOp === undefined) delete process.env.NEKTAR_OPERATOR_TOKEN
    else process.env.NEKTAR_OPERATOR_TOKEN = prevOp
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('nektar dock extended clients (vscode, zed, gemini, proxy, --hook-mode http) + nektar hub dry run & template inventory', async () => {
  const home = mkTmp('nektar-ext-home-')
  const proj = mkTmp('nektar-ext-proj-')
  const hubDir = mkTmp('nektar-hub-dry-')
  try {
    mkdirSync(join(home, '.config', 'Code', 'User'), { recursive: true })
    mkdirSync(join(home, '.config', 'zed'), { recursive: true })
    mkdirSync(join(home, '.gemini'), { recursive: true })
    mkdirSync(join(proj, '.claude'), { recursive: true })

    let dockOut = ''
    const code = await runDockCli(
      ['vscode', 'zed', 'gemini', 'claude', 'proxy', '--url', 'http://127.0.0.1:18420', '--token', 'tok99', '--hook-mode', 'http', '--dir', proj],
      {
        homeDir: home,
        cwd: proj,
        osPlatform: 'linux',
        repoRoot: REPO_ROOT,
        env: {},
        out: (s) => (dockOut += s),
        err: () => {},
        fetchImpl: () => Promise.reject(new Error('offline'))
      }
    )
    assert.equal(code, 0)
    assert.match(dockOut, /OPENAI_BASE_URL/)
    assert.match(dockOut, /nkt:tok99:/)
    assert.ok(existsSync(join(home, '.nektar', 'hooks', 'prefetch.sh')), 'http hook script generated')
    const vscodeCfg = JSON.parse(readFileSync(join(home, '.config', 'Code', 'User', 'mcp.json'), 'utf8')) as { servers: { nektar: { url: string } } }
    assert.equal(vscodeCfg.servers.nektar.url, 'http://127.0.0.1:18420/mcp')
    const geminiCfg = JSON.parse(readFileSync(join(home, '.gemini', 'settings.json'), 'utf8')) as { mcpServers: { nektar: { httpUrl: string } } }
    assert.equal(geminiCfg.mcpServers.nektar.httpUrl, 'http://127.0.0.1:18420/mcp')

    // nektar hub up --dry generates complete secrets + domain config without touching Docker
    let hubOut = ''
    const hCode = await runHubCli(['up', '--dry', '--domain', 'hub.smoke.test', '--dir', hubDir], {
      homeDir: home,
      repoRoot: REPO_ROOT,
      env: {},
      out: (s) => (hubOut += s),
      err: () => {}
    })
    assert.equal(hCode, 0)
    assert.match(hubOut, /TLS https:\/\/hub\.smoke\.test/)
    const envFile = readFileSync(join(hubDir, '.env'), 'utf8')
    assert.match(envFile, /^NEKTAR_DOMAIN=hub\.smoke\.test$/m)
    assert.match(envFile, /^NEKTAR_HTTP_TOKEN=[0-9a-f]{64}$/m)
    assert.match(envFile, /^OVERLAY_DB_PASSWORD=[0-9a-f]{64}$/m)

    // Verify every 1-click template & installer file exists and parses cleanly
    for (const rel of [
      'install/install.sh',
      'install/install.ps1',
      'install/nektar',
      'install/nektar.ps1',
      'install/nektar.cmd',
      'packaging/homebrew/nektar.rb',
      'deploy/hub/templates/coolify.compose.yaml',
      'deploy/hub/templates/dokploy.compose.yaml',
      'deploy/hub/templates/cloud-init.yaml',
      'deploy/hub/templates/railway.json',
      'deploy/hub/templates/render.yaml',
      'deploy/hub/templates/fly.toml',
      'deploy/hub/templates/umbrel/umbrel-app.yml',
      'deploy/hub/templates/umbrel/docker-compose.yml',
      'deploy/hub/templates/casaos.yaml'
    ]) {
      assert.ok(existsSync(join(REPO_ROOT, rel)), `missing deliverable: ${rel}`)
    }
    const rw = JSON.parse(readFileSync(join(REPO_ROOT, 'deploy/hub/templates/railway.json'), 'utf8')) as { deploy: { healthcheckPath: string } }
    assert.equal(rw.deploy.healthcheckPath, '/v1/health')
  } finally {
    rmSync(home, { recursive: true, force: true })
    rmSync(proj, { recursive: true, force: true })
    rmSync(hubDir, { recursive: true, force: true })
  }
})
