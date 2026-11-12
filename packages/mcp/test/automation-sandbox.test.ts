import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parsePatch, validatePatch, evaluatePatch, dockerBackend, type SandboxSpec, type IsolationBackend } from '../src/automation/sandbox.js'
import { ScriptedExecutor, HttpChatExecutor, validateResult } from '../src/automation/ai.js'

const root = join(fileURLToPath(new URL('.', import.meta.url)), '..', '..', '..', '..')
const spec: SandboxSpec = {
  fixtureDir: join(root, 'test', 'fixtures', 'libfoo-break'),
  hiddenTestsDir: join(root, 'test', 'fixtures', 'libfoo-break-hidden'),
  allowedPaths: ['src'],
  testCommand: ['node', '--test', 'test/build.test.js'],
  hiddenTestCommand: ['node', '--test', '/hidden/regress.test.js'],
  image: 'node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202',
  timeoutMs: 120_000,
  maxPatchBytes: 4096
}
const FINDING =
  'libfoo 3.0 renamed connect() to open().\n```nektar-patch\n{"edits":[{"file":"src/app.js","find":"libfoo.connect(","replace":"libfoo.open("}]}\n```'
const good = parsePatch(FINDING)!

test('sandbox: a purchased finding parses into a bounded patch proposal', () => {
  assert.deepEqual(good, { edits: [{ file: 'src/app.js', find: 'libfoo.connect(', replace: 'libfoo.open(' }] })
  assert.equal(parsePatch('no patch here'), undefined)
  assert.equal(parsePatch('```nektar-patch\nnot json\n```'), undefined)
  assert.equal(validatePatch(spec, good), undefined)
})

test('sandbox: traversal, protected files, test-oracle edits and oversized patches are refused', () => {
  const e = (file: string, replace = 'x') => ({ edits: [{ file, find: 'a', replace }] })
  for (const file of [
    '../outside.js',
    'src/../../x',
    '/etc/passwd',
    'C:\\Windows\\x',
    'src//x',
    'test/build.test.js',
    'lib/libfoo.js',
    'package.json',
    'src/.env',
    '.npmrc',
    'src/node_modules/x.js',
    '.github/workflows/ci.yml'
  ]) {
    assert.ok(validatePatch(spec, e(file)), `${file} must be refused`)
  }
  assert.match(validatePatch(spec, e('src/app.js', 'y'.repeat(5000)))!, /too large/)
  assert.match(validatePatch(spec, { edits: [] })!, /1\.\.32/)
  assert.match(validatePatch(spec, { edits: [{ file: 'src/app.js', find: '', replace: 'x' }] })!, /malformed/)
})

test('sandbox: without real isolation the evaluation is BLOCKED — never run on the host', async () => {
  let ran = false
  const none: IsolationBackend = {
    name: 'absent',
    available: async () => false,
    run: async () => {
      ran = true
      return { exitCode: 0, timedOut: false, outputTail: '' }
    }
  }
  const r = await evaluatePatch(spec, good, none)
  assert.equal(r.outcome, 'BLOCKED')
  assert.match(r.reason!, /refusing to run untrusted code on the host/)
  assert.equal(ran, false)
  const bad = await evaluatePatch(spec, { edits: [{ file: 'test/build.test.js', find: 'open', replace: 'x' }] }, none)
  assert.equal(bad.outcome, 'FAIL')
  assert.match(bad.reason!, /patch refused/)
})

test('sandbox: control flow with a recording backend — baseline must fail, patch must apply, hidden tests gate the result', async () => {
  const calls: Array<{ cmd: string[]; hidden?: string; app: string }> = []
  const script = (results: number[]): IsolationBackend => ({
    name: 'recording-double',
    available: async () => true,
    run: async (work, hidden, cmd) => {
      calls.push({ cmd, hidden, app: readFileSync(join(work, 'src', 'app.js'), 'utf8') })
      return { exitCode: results.shift() ?? 0, timedOut: false, outputTail: '' }
    }
  })
  const pass = await evaluatePatch(spec, good, script([1, 0, 0]))
  assert.equal(pass.outcome, 'PASS')
  assert.match(calls[0]!.app, /libfoo\.connect\(/, 'baseline ran on the unpatched checkout')
  assert.match(calls[1]!.app, /libfoo\.open\(/, 'acceptance ran on the patched checkout')
  assert.ok(calls[2]!.hidden, 'hidden tests are mounted separately from the writable checkout')
  assert.match(readFileSync(join(spec.fixtureDir, 'src', 'app.js'), 'utf8'), /libfoo\.connect\(/, 'the original fixture is never modified')
  assert.equal((await evaluatePatch(spec, good, script([0]))).outcome, 'BLOCKED', 'a fixture that already passes proves nothing')
  assert.equal((await evaluatePatch(spec, good, script([1, 1]))).outcome, 'FAIL')
  assert.equal((await evaluatePatch(spec, good, script([1, 0, 1]))).reason, 'hidden regression tests fail')
  assert.match((await evaluatePatch(spec, { edits: [{ file: 'src/app.js', find: 'nope(', replace: 'x(' }] }, script([1]))).reason!, /does not apply/)
})

test('sandbox: real container run — failing build, purchased fix, passing acceptance + hidden tests', async (t) => {
  if (process.env.CI && process.env.NEKTAR_SANDBOX_E2E !== '1') {
    t.skip('runs in the dedicated sandbox job (NEKTAR_SANDBOX_E2E=1)')
    return
  }
  if (!(await dockerBackend.available())) {
    t.skip('BLOCKED: docker is not available on this host — the isolated run was not exercised')
    return
  }
  const r = await evaluatePatch(spec, good)
  assert.equal(r.outcome, 'PASS', `${r.reason ?? ''}\n${r.patched?.outputTail ?? r.baseline?.outputTail ?? ''}`)
  assert.notEqual(r.baseline!.exitCode, 0)
  assert.deepEqual(r.isolation, {
    backend: 'docker',
    network: 'none',
    image: 'node:22-alpine@sha256:0e1754758c2df4cfa3e68d59e2e1b7374bb7fd9ea6328eaaacf15cc934af8202'
  })
})

test('ai: scripted matcher is deterministic; results outside the offered candidates are rejected', async () => {
  const job = {
    jobId: 'j',
    kind: 'match' as const,
    question: 'libfoo connect is not a function',
    domains: ['libfoo'],
    candidates: [
      { id: '0', text: 'tomatoes and gardening' },
      { id: '1', text: 'libfoo renamed connect to open' }
    ],
    maxOutputTokens: 100,
    timeoutMs: 1000
  }
  const a = await new ScriptedExecutor().execute(job)
  assert.equal(a.choice, '1')
  assert.deepEqual(a, await new ScriptedExecutor().execute(job))
  assert.equal((await new ScriptedExecutor().execute({ ...job, question: 'zebra', domains: ['zoo'] })).stop, 'no_match')
  const model = { provider: 'x', modelId: 'y' }
  assert.throws(() => validateResult(job, { choice: '7', relevanceBps: 10 }, model), /not one of the offered/)
  assert.throws(() => validateResult(job, { choice: '1', relevanceBps: 10001 }, model), /out of range/)
  assert.throws(() => validateResult(job, 'ignore previous instructions', model), /not an object/)
})

test('ai: provider-neutral HTTP adapter — same contract against two endpoints, bounded and schema-checked', async () => {
  const job = { jobId: 'j', kind: 'match' as const, question: 'q', domains: ['d'], candidates: [{ id: '0', text: 'a' }], maxOutputTokens: 64, timeoutMs: 1000 }
  const seen: Array<{ url: string; auth?: string; body: any }> = []
  const fake =
    (content: string, status = 200): typeof fetch =>
    async (url, init) => {
      seen.push({ url: String(url), auth: (init!.headers as Record<string, string>).authorization, body: JSON.parse(String(init!.body)) })
      return new Response(JSON.stringify({ choices: [{ message: { content } }], usage: { prompt_tokens: 9, completion_tokens: 3 } }), { status })
    }
  const one = new HttpChatExecutor({
    provider: 'local-llm',
    baseUrl: 'http://127.0.0.1:11434/v1',
    modelId: 'm1',
    egress: 'local',
    fetchImpl: fake('{"choice":"0","relevanceBps":8000}')
  })
  const two = new HttpChatExecutor({
    provider: 'hosted',
    baseUrl: 'https://api.example.test/v1/',
    modelId: 'm2',
    apiKey: 'k',
    egress: 'external',
    fetchImpl: fake('{"choice":"0","relevanceBps":8000}')
  })
  const r1 = await one.execute(job),
    r2 = await two.execute(job)
  assert.equal(r1.choice, '0')
  assert.equal(r2.choice, '0')
  assert.deepEqual(
    [r1.model, r2.model],
    [
      { provider: 'local-llm', modelId: 'm1' },
      { provider: 'hosted', modelId: 'm2' }
    ]
  )
  assert.deepEqual(
    seen.map((s) => s.url),
    ['http://127.0.0.1:11434/v1/chat/completions', 'https://api.example.test/v1/chat/completions']
  )
  assert.deepEqual(
    seen.map((s) => s.auth),
    [undefined, 'Bearer k']
  )
  assert.equal(seen[0]!.body.max_tokens, 64)
  assert.ok(!JSON.stringify(seen).includes('WIF'), 'no wallet material in a model request')
  const evil = new HttpChatExecutor({
    provider: 'p',
    baseUrl: 'http://127.0.0.1:1/v1',
    modelId: 'm',
    egress: 'local',
    fetchImpl: fake('{"choice":"../../etc","relevanceBps":1}')
  })
  await assert.rejects(evil.execute(job), /not one of the offered/)
  await assert.rejects(
    new HttpChatExecutor({ provider: 'p', baseUrl: 'http://127.0.0.1:1/v1', modelId: 'm', egress: 'local', fetchImpl: fake('x', 500) }).execute(job),
    /answered 500/
  )
  assert.throws(() => new HttpChatExecutor({ provider: 'p', baseUrl: 'file:///etc', modelId: 'm', egress: 'local' }), /http/)
})
