// Friction-audit regression + TTFV/TTFC measurement tests across J1–J5:
//   - J1: deploy/{hub,agent}/.env.example keys forwarded in compose.yaml; JOIN-TESTNET Caddyfile proxies :8080
//   - J2a/J3: recall with omitted shard or {"shard":"welcome"} (ADAPTERS.md curl) returns seeded content + measured TTFV;
//             missing shard returns available shard hint; private shards stay hidden
//   - J4: `nektar policy` CLI approves/lists/revokes starter policies without Restate or git checkout; refuses main without ack
//   - J2b/J3/J5: doctor shardNames/standalone/transport/faucet hints; tools/doctor.mjs, tools/automation.mjs, setup-claude/codex
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { existsSync, mkdirSync, mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { PassThrough } from 'node:stream'
import { fileURLToPath } from 'node:url'
import type { AddressInfo } from 'node:net'
import { PrivateKey } from '@bsv/sdk'
import { testWorld } from '@nektar/chain'
import { policyError } from '@nektar/automation'
import { NektarMemory, createHttpApi, callTool } from '../src/index.js'
import { seedWelcome } from '../src/welcome.js'
import { runPolicyCli } from '../src/policy-cli.js'
import { clearDaemonInfo, discoverDaemon, resolveDefaultNetwork, writeDaemonInfo } from '../src/daemon.js'
import { serveStdioBridge } from '../src/mcp-bridge.js'
import { runHookCli } from '../src/hooks-cli.js'
import { runDockCli } from '../src/dock-cli.js'

const REPO_ROOT = resolve(fileURLToPath(new URL('../../../..', import.meta.url)))
const mkTmp = (prefix: string) => mkdtempSync(join(tmpdir(), prefix))

void test('J2a/J3: omitted-shard recall & ADAPTERS curl return seeded welcome content; empty shard hints available shards', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-fr-j2-')
  const t0 = Date.now()
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })
  await mem.remember('secret', 'classified note', { source: 'test' }, 0)
  mem.setShardPrivate('secret')
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`
  try {
    // exact ADAPTERS.md curl payload
    const r1 = await fetch(`${base}/v1/tools/recall`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({ shard: 'welcome' })
    })
    assert.equal(r1.status, 200)
    const j1 = (await r1.json()) as { items: Array<{ text?: string }> }
    assert.ok(j1.items.length >= 3, 'welcome shard returns seeded items')
    const ttfvMs = Date.now() - t0
    assert.ok(ttfvMs < 30_000, `measured J2a/J3 TTFV=${ttfvMs}ms`)

    // omitted shard lists and recalls non-private shards only
    const r2 = await fetch(`${base}/v1/tools/recall`, {
      method: 'POST',
      headers: { 'content-type': 'application/json' },
      body: JSON.stringify({})
    })
    assert.equal(r2.status, 200)
    const j2 = (await r2.json()) as { shards: string[]; items: Array<{ shard: string; text?: string }> }
    assert.deepEqual(j2.shards, ['welcome'])
    assert.ok(j2.items.length >= 3 && j2.items.every((i) => i.shard === 'welcome'))

    // wrong shard name returns a hint listing available shards
    const r3 = (await callTool(mem, 'recall', { shard: 'default' })) as { items: unknown[]; shards?: string[]; hint?: string }
    assert.equal(r3.items.length, 0)
    assert.deepEqual(r3.shards, ['welcome'])
    assert.match(r3.hint ?? '', /available shards: welcome/)
  } finally {
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J5: doctor exposes shardNames, reports standalone when automation is off, and gives transport/faucet-aware funding hints', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-fr-doc-')
  const prevPort = process.env.NEKTAR_HTTP_PORT
  try {
    const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
    await seedWelcome({ mem, dataDir: dir })
    const fakeUnfundedOps = {
      wallet: () =>
        Promise.resolve({
          address: 'mtTestAddr111111111111111111111111',
          balanceSats: 0,
          utxos: 0,
          spentTodaySats: 0,
          caps: { maxFeePerTx: 1000, maxPerDay: 20000, minIntervalMs: 250 },
          avgFeePerAnchorSats: 0,
          runwayAnchors: null
        })
    }
    delete process.env.NEKTAR_HTTP_PORT
    const stdioDoc = (await callTool(mem, 'doctor', {}, { ops: fakeUnfundedOps })) as {
      bootstrap: string
      shardNames: string[]
      hints?: string[]
    }
    assert.equal(stdioDoc.bootstrap, 'standalone')
    assert.deepEqual(stdioDoc.shardNames, ['welcome'])
    assert.ok(!(stdioDoc.hints ?? []).some((h) => h.includes('unsealed')), 'standalone must not emit unsealed hint')
    const stdioFundHint = (stdioDoc.hints ?? []).find((h) => h.startsWith('unfunded')) ?? ''
    assert.match(stdioFundHint, /nektar\.run\/v1\/faucet/)
    assert.match(stdioFundHint, /call fundingImport/)

    process.env.NEKTAR_HTTP_PORT = '8080'
    const httpDoc = (await callTool(mem, 'doctor', {}, { ops: fakeUnfundedOps })) as { hints?: string[] }
    const httpFundHint = (httpDoc.hints ?? []).find((h) => h.startsWith('unfunded')) ?? ''
    assert.match(httpFundHint, /POST \/v1\/tools\/fundingImport/)
  } finally {
    if (prevPort === undefined) delete process.env.NEKTAR_HTTP_PORT
    else process.env.NEKTAR_HTTP_PORT = prevPort
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J4: nektar policy CLI approves, lists, and revokes starter policies without Restate; gates mainnet', async () => {
  const dir = mkTmp('nektar-fr-pol-')
  const prevWallet = process.env.NEKTAR_WALLET
  process.env.NEKTAR_WALLET = 'toolbox'
  const capture = async (argv: string[]) => {
    let stdout = ''
    let stderr = ''
    const code = await runPolicyCli(argv, {
      out: (s) => (stdout += s),
      err: (s) => (stderr += s)
    })
    return { code, stdout, stderr }
  }
  try {
    const init = await capture(['init', '--preset', 'buyer', '--network', 'testnet', '--data', dir])
    assert.equal(init.code, 0, init.stderr)
    const draft = JSON.parse(init.stdout) as { network: string; roles: string[]; agentPub: string }
    assert.equal(draft.network, 'testnet')
    assert.deepEqual(draft.roles, ['buyer'])
    assert.equal(policyError(draft), undefined)

    const app = await capture(['approve', '--preset', 'both', '--domains', 'libfoo', '--shards', 'welcome', '--network', 'testnet', '--data', dir, '--json'])
    assert.equal(app.code, 0, app.stderr)
    const approved = JSON.parse(app.stdout) as { policyHash: string; roles: string[] }
    assert.ok(approved.policyHash.length === 64)
    assert.deepEqual(approved.roles, ['buyer', 'seller'])

    const listed = await capture(['list', '--data', dir, '--json'])
    assert.equal(listed.code, 0, listed.stderr)
    const listJson = JSON.parse(listed.stdout) as { policies: Array<{ policyHash: string; revokedAt?: number }> }
    assert.ok(listJson.policies.some((p) => p.policyHash === approved.policyHash && p.revokedAt === undefined))

    const rev = await capture(['revoke', approved.policyHash, '--data', dir, '--json'])
    assert.equal(rev.code, 0, rev.stderr)

    // mainnet refusal without explicit confirmation
    const deniedMain = await capture(['approve', '--preset', 'buyer', '--network', 'main', '--data', dir, '--agent-pub', draft.agentPub])
    assert.notEqual(deniedMain.code, 0)
    assert.match(deniedMain.stderr, /I_UNDERSTAND/)
  } finally {
    if (prevWallet === undefined) delete process.env.NEKTAR_WALLET
    else process.env.NEKTAR_WALLET = prevWallet
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J1: deploy/{hub,agent} compose.yaml forwards every NEKTAR_* key in .env.example and JOIN-TESTNET Caddyfile proxies :8080', () => {
  const envKeys = (text: string): string[] =>
    text
      .split(/\r?\n/)
      .map((l) => /^([A-Za-z_][A-Za-z0-9_]*)=/.exec(l.trim()))
      .filter((m): m is RegExpExecArray => m !== null)
      .map((m) => m[1])
      .filter((k) => k.startsWith('NEKTAR_'))

  for (const dir of ['deploy/hub', 'deploy/agent']) {
    const envEx = readFileSync(join(REPO_ROOT, dir, '.env.example'), 'utf8')
    const compose = readFileSync(join(REPO_ROOT, dir, 'compose.yaml'), 'utf8')
    const missing = envKeys(envEx).filter((k) => !compose.includes(`${k}:`) && !compose.includes(`\${${k}`))
    assert.deepEqual(missing, [], `${dir}/compose.yaml is missing .env.example keys: ${missing.join(', ')}`)
  }
  const joinDoc = readFileSync(join(REPO_ROOT, 'docs/JOIN-TESTNET.md'), 'utf8')
  assert.match(joinDoc, /reverse_proxy localhost:8080/, 'JOIN-TESTNET Caddyfile must proxy /mcp and /v1/* to :8080')
  assert.match(joinDoc, /NEKTAR_PUBLIC_URL/, 'JOIN-TESTNET install uses NEKTAR_PUBLIC_URL')

  // example policy file validates once agentPub is substituted
  const ex = JSON.parse(readFileSync(join(REPO_ROOT, '.devin/automation.policy.example.json'), 'utf8')) as Record<string, unknown>
  ex.agentPub = PrivateKey.fromRandom().toPublicKey().toString()
  assert.equal(policyError(ex), undefined, '.devin/automation.policy.example.json must satisfy policyError')
})

void test('J2b/J3/J5: developer CLIs handle unset NEKTAR_DATA_DIR, check Restate, and register MCP with token/port', () => {
  const dir = mkTmp('nektar-fr-cli-')
  const cleanEnv = { ...process.env }
  delete cleanEnv.NEKTAR_DATA_DIR
  delete cleanEnv.NEKTAR_RESTATE_URL
  try {
    // doctor.mjs must not throw ERR_INVALID_ARG_TYPE and must report Restate status
    const doc = spawnSync(process.execPath, [join(REPO_ROOT, 'tools/doctor.mjs')], { cwd: dir, env: cleanEnv, encoding: 'utf8' })
    assert.doesNotMatch(doc.stderr, /ERR_INVALID_ARG_TYPE/)
    assert.match(doc.stdout, /executor plane/)

    // automation.mjs help must not throw ERR_INVALID_ARG_TYPE
    const auto = spawnSync(process.execPath, [join(REPO_ROOT, 'tools/automation.mjs'), 'help'], { cwd: dir, env: cleanEnv, encoding: 'utf8' })
    assert.equal(auto.status, 0, auto.stderr)

    // setup-claude registers .mcp.json + hooks + CLAUDE.md
    const cl = spawnSync(process.execPath, [join(REPO_ROOT, 'tools/setup-claude.mjs'), '--dir', dir, '--port', '9091', '--token', 'tok123'], {
      env: cleanEnv,
      encoding: 'utf8'
    })
    assert.equal(cl.status, 0, cl.stderr)
    const mcpJson = JSON.parse(readFileSync(join(dir, '.mcp.json'), 'utf8')) as {
      mcpServers: { nektar: { url: string; headers?: { Authorization?: string } } }
    }
    assert.equal(mcpJson.mcpServers.nektar.url, 'http://localhost:9091/mcp')
    assert.equal(mcpJson.mcpServers.nektar.headers?.Authorization, 'Bearer tok123')

    // setup-codex writes config.toml with custom port + bearer
    const cx = spawnSync(process.execPath, [join(REPO_ROOT, 'tools/setup-codex.mjs'), '--dir', dir, '--port', '9091', '--token', 'tok123'], {
      env: cleanEnv,
      encoding: 'utf8'
    })
    assert.equal(cx.status, 0, cx.stderr)
    const toml = readFileSync(join(dir, '.codex/config.toml'), 'utf8')
    assert.match(toml, /url = "http:\/\/localhost:9091\/mcp"/)
    assert.match(toml, /Bearer tok123/)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J2/J6: zero-env network default picks testnet when unconfigured and preserves regtest when regtest creds exist', () => {
  assert.deepEqual(resolveDefaultNetwork({ env: {}, dot: {} }), { network: 'testnet', defaulted: true })
  assert.deepEqual(resolveDefaultNetwork({ env: { NEKTAR_RPC_PASS: 'secret' }, dot: {} }), { network: 'regtest', defaulted: true })
  assert.deepEqual(resolveDefaultNetwork({ env: {}, dot: { NEKTAR_REGTEST_WIF: 'cTest' } }), { network: 'regtest', defaulted: true })
  assert.deepEqual(resolveDefaultNetwork({ flagNet: 'main', env: {}, dot: {} }), { network: 'main', defaulted: false })
  assert.deepEqual(resolveDefaultNetwork({ envNet: 'regtest', flagNet: 'testnet', env: {}, dot: {} }), { network: 'regtest', defaulted: false })
})

void test('J6: multi-client stdio ↔ HTTP bridge connects two concurrent MCP clients to one brain with zero lock collisions', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-fr-bridge-')
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const port = (srv.address() as AddressInfo).port
  const base = `http://127.0.0.1:${port}`
  writeDaemonInfo(dir, { url: base, pid: process.pid, network: 'testnet', startedAt: Date.now() })

  const callViaBridge = async (clientName: string) => {
    const clientIn = new PassThrough()
    const clientOut = new PassThrough()
    const bridge = await serveStdioBridge({ mcpUrl: `${base}/mcp`, stdin: clientIn, stdout: clientOut })
    const responses = new Map<number, Record<string, unknown>>()
    const waiters = new Map<number, (v: Record<string, unknown>) => void>()
    let buf = ''
    clientOut.on('data', (chunk) => {
      buf += String(chunk)
      for (let nl = buf.indexOf('\n'); nl !== -1; nl = buf.indexOf('\n')) {
        const line = buf.slice(0, nl).trim()
        buf = buf.slice(nl + 1)
        if (!line) continue
        const msg = JSON.parse(line) as { id?: number }
        if (typeof msg.id === 'number') {
          const wFn = waiters.get(msg.id)
          if (wFn) {
            waiters.delete(msg.id)
            wFn(msg)
          } else responses.set(msg.id, msg)
        }
      }
    })
    const rpc = (id: number, method: string, params: Record<string, unknown>) =>
      new Promise<Record<string, unknown>>((res, rej) => {
        const timer = setTimeout(() => rej(new Error(`${clientName} timeout on ${method}`)), 5_000)
        const done = (v: Record<string, unknown>) => {
          clearTimeout(timer)
          res(v)
        }
        if (responses.has(id)) done(responses.get(id)!)
        else waiters.set(id, done)
        clientIn.write(JSON.stringify({ jsonrpc: '2.0', id, method, params }) + '\n')
      })
    try {
      const init = await rpc(1, 'initialize', {
        protocolVersion: '2025-03-26',
        capabilities: {},
        clientInfo: { name: clientName, version: '1.0.0' }
      })
      assert.ok(init.result, `${clientName} initialized`)
      clientIn.write(JSON.stringify({ jsonrpc: '2.0', method: 'notifications/initialized' }) + '\n')
      const recalled = (await rpc(2, 'tools/call', { name: 'recall', arguments: {} })) as {
        result?: { structuredContent?: { shards?: string[]; items?: unknown[] } }
      }
      const sc = recalled.result?.structuredContent
      assert.deepEqual(sc?.shards, ['welcome'])
      assert.ok((sc?.items?.length ?? 0) >= 3)
    } finally {
      await bridge.close()
    }
  }

  try {
    const found = await discoverDaemon({ dataDir: dir, env: {} })
    assert.equal(found?.url, base)
    const t0 = Date.now()
    await Promise.all([callViaBridge('hermes-client'), callViaBridge('claude-client')])
    const elapsedMs = Date.now() - t0
    assert.ok(elapsedMs < 10_000, `concurrent 2-client bridge recall took ${elapsedMs}ms`)
  } finally {
    clearDaemonInfo(dir, process.pid)
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J6: portable nektar hook CLI handles session-start, prefetch, and receipt verification (pass + block-once)', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-fr-hook-')
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    let out = ''
    let err = ''
    const code1 = await runHookCli(['session-start'], {
      out: (s) => (out += s),
      err: (s) => (err += s),
      env: { NEKTAR_URL: base }
    })
    assert.equal(code1, 0, err)
    assert.match(out, /nektar ready/)

    let preOut = ''
    const code2 = await runHookCli(['prefetch'], {
      out: (s) => (preOut += s),
      stdinText: () => Promise.resolve(JSON.stringify({ prompt: 'How does groundedAnswer work?' })),
      env: { NEKTAR_URL: base }
    })
    assert.equal(code2, 0)
    assert.match(preOut, /\[welcome:\d+\]/)
    assert.match(preOut, /groundedAnswer/)

    // Fake token fails verification and blocks stop once (and does not loop when stop_hook_active=true)
    const fakeTok = 'nektarr1.' + 'A'.repeat(44)
    let blockOut = ''
    let blockErr = ''
    const code3 = await runHookCli(['verify-receipt'], {
      out: (s) => (blockOut += s),
      err: (s) => (blockErr += s),
      stdinText: () =>
        Promise.resolve(
          JSON.stringify({
            stop_hook_active: false,
            last_assistant_message: `The answer is 42.\nreceipt: ${fakeTok}\nverify: ${base}/v1/verify?r=${fakeTok}`
          })
        ),
      env: { NEKTAR_URL: base }
    })
    assert.equal(code3, 0)
    assert.match(blockErr, /FAILED/)
    assert.equal((JSON.parse(blockOut) as { decision?: string }).decision, 'block')

    let loopOut = ''
    await runHookCli(['verify-receipt'], {
      out: (s) => (loopOut += s),
      err: () => {},
      stdinText: () =>
        Promise.resolve(
          JSON.stringify({
            stop_hook_active: true,
            last_assistant_message: `The answer is 42.\nreceipt: ${fakeTok}\nverify: ${base}/v1/verify?r=${fakeTok}`
          })
        ),
      env: { NEKTAR_URL: base }
    })
    assert.equal(loopOut, '', 'stop_hook_active=true must never re-block')
  } finally {
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('J3/J6: nektar dock auto detects & wires Hermes, OpenClaw, Claude, Codex, Cursor non-clobbering + cleanly uninstalls', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const fakeHome = mkTmp('nektar-fr-home-')
  const projDir = mkTmp('nektar-fr-proj-')
  const dataDir = mkTmp('nektar-fr-dockdata-')
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    // Seed existing user configs across 5 AI apps to prove non-clobbering merge
    mkdirSync(join(fakeHome, '.hermes'), { recursive: true })
    writeFileSync(join(fakeHome, '.hermes', 'config.yaml'), 'model: claude\nmcp_servers:\n  sibling:\n    url: http://127.0.0.1:9999/mcp\n')
    mkdirSync(join(fakeHome, '.openclaw'), { recursive: true })
    writeFileSync(join(fakeHome, '.openclaw', 'openclaw.json'), JSON.stringify({ plugins: { entries: { keepme: { enabled: true } } } }))
    mkdirSync(join(projDir, '.claude'), { recursive: true })
    mkdirSync(join(fakeHome, '.codex'), { recursive: true })
    mkdirSync(join(fakeHome, '.cursor'), { recursive: true })
    writeFileSync(join(fakeHome, '.cursor', 'mcp.json'), JSON.stringify({ mcpServers: { other: { url: 'http://127.0.0.1:1234/mcp' } } }))

    let jsonOut = ''
    const code = await runDockCli(['auto', '--url', base, '--dir', projDir, '--json'], {
      out: (s) => (jsonOut += s),
      homeDir: fakeHome,
      cwd: projDir,
      osPlatform: 'linux',
      repoRoot: REPO_ROOT,
      env: {}
    })
    assert.equal(code, 0)
    const report = JSON.parse(jsonOut) as {
      clients: Array<{ client: string; changed: boolean }>
      probe: { ok: boolean; items?: number }
    }
    const wired = new Set(report.clients.map((c) => c.client))
    for (const c of ['hermes', 'openclaw', 'claude', 'codex', 'cursor']) assert.ok(wired.has(c), `expected ${c} docked`)
    assert.equal(report.probe.ok, true, 'live recall probe must succeed')
    assert.ok((report.probe.items ?? 0) >= 3)

    // Verify Hermes native plugin + SKILL.md + non-clobbering config.yaml
    assert.ok(existsSync(join(fakeHome, '.hermes', 'plugins', 'memory', 'nektar', 'plugin.yaml')))
    assert.ok(existsSync(join(fakeHome, '.hermes', 'skills', 'nektar-memory', 'SKILL.md')))
    const hYaml = readFileSync(join(fakeHome, '.hermes', 'config.yaml'), 'utf8')
    assert.match(hYaml, /sibling:/)
    assert.match(hYaml, /nektar:/)

    // Verify OpenClaw native memory slot + sibling preserved
    const ocJson = JSON.parse(readFileSync(join(fakeHome, '.openclaw', 'openclaw.json'), 'utf8')) as {
      plugins: { slots: { memory: string }; entries: Record<string, unknown> }
    }
    assert.equal(ocJson.plugins.slots.memory, 'nektar-memory')
    assert.ok(Object.hasOwn(ocJson.plugins.entries, 'keepme'))

    // Verify Cursor sibling preserved
    const curJson = JSON.parse(readFileSync(join(fakeHome, '.cursor', 'mcp.json'), 'utf8')) as { mcpServers: Record<string, unknown> }
    assert.ok(Object.hasOwn(curJson.mcpServers, 'other'))
    assert.ok(Object.hasOwn(curJson.mcpServers, 'nektar'))

    // Clean uninstall removes nektar while keeping sibling configs intact
    const uCode = await runDockCli(['auto', '--uninstall', '--dir', projDir, '--json'], {
      out: () => {},
      homeDir: fakeHome,
      cwd: projDir,
      osPlatform: 'linux',
      repoRoot: REPO_ROOT,
      env: {}
    })
    assert.equal(uCode, 0)
    const curAfter = JSON.parse(readFileSync(join(fakeHome, '.cursor', 'mcp.json'), 'utf8')) as { mcpServers: Record<string, unknown> }
    assert.ok(Object.hasOwn(curAfter.mcpServers, 'other'))
    assert.ok(!Object.hasOwn(curAfter.mcpServers, 'nektar'))
    const ocAfter = JSON.parse(readFileSync(join(fakeHome, '.openclaw', 'openclaw.json'), 'utf8')) as {
      plugins: { slots: { memory: string }; entries: Record<string, unknown> }
    }
    assert.equal(ocAfter.plugins.slots.memory, 'memory-core')
    assert.ok(Object.hasOwn(ocAfter.plugins.entries, 'keepme'))
  } finally {
    srv.close()
    rmSync(fakeHome, { recursive: true, force: true })
    rmSync(projDir, { recursive: true, force: true })
    rmSync(dataDir, { recursive: true, force: true })
  }
})

void test('J3/J6: ecosystem manifests (MCP server.json, Claude plugin, OpenClaw plugin, Hermes plugin, GHCR YAML) are valid and aligned', () => {
  const srvJson = JSON.parse(readFileSync(join(REPO_ROOT, 'server.json'), 'utf8')) as { name: string; packages: Array<{ identifier: string }> }
  assert.equal(srvJson.name, 'io.github.nedzof/nektar')
  assert.match(srvJson.packages[0].identifier, /ghcr\.io\/kryptokrates\/nektar/)

  const claudePlugin = JSON.parse(readFileSync(join(REPO_ROOT, '.claude-plugin/plugin.json'), 'utf8')) as { name: string; skills: string[] }
  assert.equal(claudePlugin.name, 'nektar')
  assert.ok(existsSync(join(REPO_ROOT, 'skills/nektar-memory/SKILL.md')))

  const market = JSON.parse(readFileSync(join(REPO_ROOT, '.claude-plugin/marketplace.json'), 'utf8')) as { plugins: Array<{ name: string }> }
  assert.equal(market.plugins[0].name, 'nektar')

  const ocManifest = JSON.parse(readFileSync(join(REPO_ROOT, 'integrations/openclaw/openclaw.plugin.json'), 'utf8')) as { kind: string }
  assert.equal(ocManifest.kind, 'memory')
  assert.ok(existsSync(join(REPO_ROOT, 'integrations/openclaw/index.mjs')))

  const hermesYaml = readFileSync(join(REPO_ROOT, 'integrations/hermes/plugin.yaml'), 'utf8')
  assert.match(hermesYaml, /name:\s*nektar/)

  const ghcrYaml = readFileSync(join(REPO_ROOT, 'docker-mcp/nektar.ghcr.yaml'), 'utf8')
  assert.match(ghcrYaml, /NEKTAR_TOOL_PROFILE[\s\S]*?value:\s*agent/)
})

void test('J6: OpenClaw native memory plugin prefetches verified fragments, registers tools, and verifies receipts', async (t) => {
  const w = await testWorld(t, 4)
  if (w === undefined) return
  const dir = mkTmp('nektar-fr-oc-')
  const mem = new NektarMemory(w.chain, w.funder, w.funder.keyPair, { batchPolicy: { maxLeaves: 2 }, network: 'testnet' })
  await seedWelcome({ mem, dataDir: dir })
  const srv = createHttpApi(mem).listen(0, '127.0.0.1')
  await new Promise((r) => srv.once('listening', r))
  const base = `http://127.0.0.1:${(srv.address() as AddressInfo).port}`

  try {
    const modPath = new URL('../../../../integrations/openclaw/index.mjs', import.meta.url).href
    const { createNektarOpenClawPlugin } = (await import(modPath)) as {
      createNektarOpenClawPlugin: () => { register: (api: Record<string, unknown>) => void }
    }
    const hooks = new Map<string, (ev: Record<string, unknown>) => Promise<unknown>>()
    const tools = new Map<string, { execute: (args: Record<string, unknown>) => Promise<unknown> }>()
    const logs: string[] = []
    const plugin = createNektarOpenClawPlugin()
    plugin.register({
      pluginConfig: { url: base, defaultShard: 'openclaw', autoRecall: true, verifyReceipts: true },
      on: (name: string, fn: (ev: Record<string, unknown>) => Promise<unknown>) => hooks.set(name, fn),
      registerTool: (def: { name: string; execute: (args: Record<string, unknown>) => Promise<unknown> }) => tools.set(def.name, def),
      logger: { info: (m: string) => logs.push(m) }
    })

    const before = hooks.get('before_prompt_build')
    assert.ok(before, 'before_prompt_build hook registered')
    const injected = (await before({ prompt: 'How does groundedAnswer work?' })) as { prependContext?: string } | undefined
    assert.match(injected?.prependContext ?? '', /\[welcome:\d+\]/)

    const recallTool = tools.get('nektar_recall')
    assert.ok(recallTool, 'nektar_recall tool registered')
    const recalled = (await recallTool.execute({})) as { shards: string[] }
    assert.deepEqual(recalled.shards, ['welcome'])

    const after = hooks.get('agent_end')
    assert.ok(after, 'agent_end hook registered')
    const fakeTok = 'nektarr1.' + 'B'.repeat(44)
    await after({ assistantMessage: `Answer\nreceipt: ${fakeTok}\nverify: ${base}/v1/verify?r=${fakeTok}` })
    assert.ok(
      logs.some((l) => l.includes('FAILED')),
      'agent_end logs failed receipt check'
    )
  } finally {
    srv.close()
    rmSync(dir, { recursive: true, force: true })
  }
})
