// Friction-audit regression + TTFV/TTFC measurement tests across J1–J5:
//   - J1: deploy/{hub,agent}/.env.example keys forwarded in compose.yaml; JOIN-TESTNET Caddyfile proxies :8080
//   - J2a/J3: recall with omitted shard or {"shard":"welcome"} (ADAPTERS.md curl) returns seeded content + measured TTFV;
//             missing shard returns available shard hint; private shards stay hidden
//   - J4: `nektar policy` CLI approves/lists/revokes starter policies without Restate or git checkout; refuses main without ack
//   - J2b/J3/J5: doctor shardNames/standalone/transport/faucet hints; tools/doctor.mjs, tools/automation.mjs, setup-claude/codex
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { spawnSync } from 'node:child_process'
import { mkdtempSync, readFileSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import type { AddressInfo } from 'node:net'
import { PrivateKey } from '@bsv/sdk'
import { testWorld } from '@nektar/chain'
import { policyError } from '@nektar/automation'
import { NektarMemory, createHttpApi, callTool } from '../src/index.js'
import { seedWelcome } from '../src/welcome.js'
import { runPolicyCli } from '../src/policy-cli.js'

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
