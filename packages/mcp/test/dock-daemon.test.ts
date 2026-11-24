import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { PassThrough } from 'node:stream'
import { resolveDefaultNetwork, discoverDaemon, writeDaemonInfo, clearDaemonInfo } from '../src/daemon.js'
import { runHookCli } from '../src/hooks-cli.js'
import { runDockCli } from '../src/dock-cli.js'
import { serveStdioBridge } from '../src/mcp-bridge.js'

void test('resolveDefaultNetwork: defaults to testnet without regtest creds and honors explicit flags', () => {
  assert.deepEqual(resolveDefaultNetwork({ env: {}, dot: {} }), { network: 'testnet', defaulted: true })
  assert.deepEqual(resolveDefaultNetwork({ env: { NEKTAR_RPC_PASS: 'pw' }, dot: {} }), { network: 'regtest', defaulted: true })
  assert.deepEqual(resolveDefaultNetwork({ flagNet: 'main', env: {}, dot: {} }), { network: 'main', defaulted: false })
})

void test('discoverDaemon + writeDaemonInfo/clearDaemonInfo: verifies nektar card and respects PID ownership', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-daemon-'))
  try {
    writeDaemonInfo(dir, { url: 'http://127.0.0.1:18421', pid: 4242, network: 'testnet', startedAt: Date.now() })
    clearDaemonInfo(dir, 9999) // foreign PID must not clobber
    const probed: string[] = []
    const found = await discoverDaemon({
      dataDir: dir,
      env: {},
      candidates: ['http://127.0.0.1:18421'],
      fetchImpl: (async (url: string | URL | Request) => {
        probed.push(String(url))
        return new Response(JSON.stringify({ name: 'nektar', network: 'testnet' }), { status: 200 })
      }) as typeof fetch
    })
    assert.equal(found?.url, 'http://127.0.0.1:18421')
    assert.equal(found?.mcpUrl, 'http://127.0.0.1:18421/mcp')
    assert.equal(probed[0], 'http://127.0.0.1:18421/.well-known/agent.json')
    clearDaemonInfo(dir, 4242)
  } finally {
    rmSync(dir, { recursive: true, force: true })
  }
})

void test('hooks-cli verify-receipt: ignores non-loopback verify URL (SSRF guard) and blocks on drift/failure', async () => {
  const fetched: string[] = []
  const outLines: string[] = []
  const tok = 'nektarr1.' + 'A'.repeat(44)
  const code = await runHookCli(['verify-receipt'], {
    env: { NEKTAR_HTTP_PORT: '18499' },
    stdinText: () =>
      Promise.resolve(
        JSON.stringify({
          last_assistant_message: `Wrong text.\nreceipt: ${tok}\nverify: https://evil.example/v1/verify?r=${tok}`
        })
      ),
    out: (s) => outLines.push(s),
    err: () => {},
    fetchImpl: (async (url: string | URL | Request) => {
      const u = String(url)
      fetched.push(u)
      if (u.includes('/.well-known/agent.json')) return new Response('nope', { status: 404 })
      return new Response(JSON.stringify({ ok: true, checks: [{ name: 'sig', ok: true }], receipt: { answer: 'Committed text' } }), { status: 200 })
    }) as typeof fetch
  })
  assert.equal(code, 0)
  assert.ok(
    fetched.every((u) => !u.includes('evil.example')),
    `non-loopback verify URL must never be fetched, got: ${fetched.join(', ')}`
  )
  assert.ok(
    fetched.some((u) => u.startsWith('http://127.0.0.1:18499/v1/verify?r=')),
    'fell back to local daemon port'
  )
  const decision = JSON.parse(outLines.join('')) as { decision?: string; reason?: string }
  assert.equal(decision.decision, 'block')
  assert.ok(decision.reason?.includes('answer-drift'))
})

void test('dock-cli: validates --url, escapes YAML/TOML scalars, is idempotent, and uninstalls cleanly', async () => {
  const home = mkdtempSync(join(tmpdir(), 'nektar-dock-home-'))
  const proj = mkdtempSync(join(tmpdir(), 'nektar-dock-proj-'))
  try {
    const errs: string[] = []
    const bad = await runDockCli(['hermes', '--url', 'javascript:alert(1)'], {
      homeDir: home,
      cwd: proj,
      env: {},
      err: (s) => errs.push(s),
      out: () => {},
      fetchImpl: (() => Promise.reject(new Error('offline'))) as typeof fetch
    })
    assert.equal(bad, 2, 'invalid --url scheme must fail with exit 2')

    const out: string[] = []
    const ok = await runDockCli(['hermes', 'goose', '--url', 'http://127.0.0.1:18420', '--token', 'tok"quote\ninjected: true'], {
      homeDir: home,
      cwd: proj,
      env: {},
      out: (s) => out.push(s),
      err: () => {},
      fetchImpl: (() => Promise.reject(new Error('offline'))) as typeof fetch
    })
    assert.equal(ok, 0)
    const hermesYaml = readFileSync(join(home, '.hermes', 'config.yaml'), 'utf8')
    assert.ok(hermesYaml.includes('Authorization: "Bearer tok\\"quoteinjected: true"'), `YAML token must be escaped & newline-stripped: ${hermesYaml}`)
    assert.ok(!hermesYaml.includes('\ninjected: true'), 'newline injection must not create a new YAML key')

    // Re-running is idempotent and --uninstall removes nektar blocks cleanly
    const rm = await runDockCli(['hermes', 'goose', '--uninstall'], {
      homeDir: home,
      cwd: proj,
      env: {},
      out: () => {},
      err: () => {},
      fetchImpl: (() => Promise.reject(new Error('offline'))) as typeof fetch
    })
    assert.equal(rm, 0)
    assert.ok(!readFileSync(join(home, '.hermes', 'config.yaml'), 'utf8').includes('# nektar-dock-start'))
  } finally {
    rmSync(home, { recursive: true, force: true })
    rmSync(proj, { recursive: true, force: true })
  }
})

void test('mcp-bridge: replies with JSON-RPC error when remote send fails and closes cleanly', async () => {
  const stdin = new PassThrough()
  const stdout = new PassThrough()
  const chunks: string[] = []
  stdout.on('data', (c) => chunks.push(String(c)))
  const bridge = await serveStdioBridge({
    mcpUrl: 'http://127.0.0.1:1/mcp',
    stdin,
    stdout,
    log: () => {}
  })
  stdin.write(JSON.stringify({ jsonrpc: '2.0', id: 7, method: 'tools/list', params: {} }) + '\n')
  // Wait briefly for the failed loopback send to emit its JSON-RPC error reply
  for (let i = 0; i < 40 && chunks.length === 0; i++) await new Promise((r) => setTimeout(r, 25))
  await bridge.close()
  const reply = JSON.parse(chunks.join('').trim()) as { id?: number; error?: { code?: number; message?: string } }
  assert.equal(reply.id, 7)
  assert.equal(reply.error?.code, -32000)
  assert.ok(reply.error?.message?.includes('nektar bridge error'))
})
