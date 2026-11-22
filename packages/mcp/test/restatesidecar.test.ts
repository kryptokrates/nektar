import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import type { ChildProcess, SpawnOptions, SpawnSyncReturns } from 'node:child_process'
import { ensureRestate } from '../src/restatesidecar.js'

// the sidecar's resolution order — every external seam (fetch, spawn, spawnSync,
// platform, restateDir) is injected so the tests never touch a real restate

const fakeChild = (): { c: ChildProcess; killed: boolean[] } => {
  const killed: boolean[] = []
  const c = {
    kill: () => {
      killed.push(true)
      return true
    },
    unref: () => {},
    on: () => c
  } as unknown as ChildProcess
  return { c, killed }
}

const okFetch =
  (aliveAdmins: string[]): typeof fetch =>
  async (url: string | URL | Request) => {
    const u = String(url)
    if (aliveAdmins.some((a) => u.startsWith(a))) return new Response('{}', { status: 200 })
    throw new Error('ECONNREFUSED')
  }

const deadFetch: typeof fetch = async () => {
  throw new Error('ECONNREFUSED')
}

type SpawnSyncImpl = NonNullable<NonNullable<Parameters<typeof ensureRestate>[0]>['spawnSyncImpl']>

const wslStub = (cmds: string[][]) =>
  ((cmd: string, args?: readonly string[], _o?: SpawnOptions) => {
    cmds.push([cmd, ...(args ?? [])])
    if (cmd === 'wsl.exe' && args?.[0] === '--status') return { status: 0 } as SpawnSyncReturns<string>
    if (cmd === 'wsl.exe' && args?.[1] === 'wslpath') {
      const win = String(args[3])
      return { status: 0, stdout: `/mnt/c/${win.replace(/\\/g, '/').replace(/^C:/i, '').replace(/^\//, '')}` } as SpawnSyncReturns<string>
    }
    if (cmd === 'wsl.exe' && args?.[1] === 'sh') return { status: 0, stdout: 'default via 172.21.112.1 dev eth0' } as SpawnSyncReturns<string>
    return { status: 0 } as SpawnSyncReturns<string>
  }) as unknown as SpawnSyncImpl

void test('sidecar: NEKTAR_RESTATE_URL set → configured, nothing probed or spawned', async () => {
  const env: NodeJS.ProcessEnv = { NEKTAR_RESTATE_URL: 'http://x:9' }
  const st = await ensureRestate({ env, fetchImpl: deadFetch, spawnImpl: () => assert.fail('must not spawn') })
  assert.equal(st, 'configured')
  assert.equal(env.NEKTAR_RESTATE_URL, 'http://x:9')
  assert.equal(env.NEKTAR_RESTATE_ADMIN, undefined)
})

void test('sidecar: live admin on :19070 → adopted, ingress/admin envs set', async () => {
  const env: NodeJS.ProcessEnv = {}
  const st = await ensureRestate({
    env,
    platform: 'linux',
    fetchImpl: okFetch(['http://127.0.0.1:19070']),
    spawnImpl: () => assert.fail('must not spawn')
  })
  assert.equal(st, 'adopted')
  assert.equal(env.NEKTAR_RESTATE_URL, 'http://127.0.0.1:18080')
  assert.equal(env.NEKTAR_RESTATE_ADMIN, 'http://127.0.0.1:19070')
})

void test('sidecar: standard ports :9070/:8080 adopted when :19070 is dark', async () => {
  const env: NodeJS.ProcessEnv = {}
  const st = await ensureRestate({
    env,
    platform: 'linux',
    fetchImpl: okFetch(['http://127.0.0.1:9070']),
    spawnImpl: () => assert.fail('must not spawn')
  })
  assert.equal(st, 'adopted')
  assert.equal(env.NEKTAR_RESTATE_URL, 'http://127.0.0.1:8080')
  assert.equal(env.NEKTAR_RESTATE_ADMIN, 'http://127.0.0.1:9070')
})

void test('sidecar: no bundled binary → degraded with a log, nothing spawned', async () => {
  const env: NodeJS.ProcessEnv = {}
  const logs: string[] = []
  const st = await ensureRestate({
    env,
    log: (m) => logs.push(m),
    fetchImpl: deadFetch,
    spawnImpl: () => assert.fail('must not spawn'),
    restateDir: mkdtempSync(join(tmpdir(), 'nektar-nors-'))
  })
  assert.equal(st, 'degraded')
  assert.ok(logs.some((m) => m.includes('NEKTAR_RESTATE_URL')))
  assert.equal(env.NEKTAR_RESTATE_URL, undefined)
})

void test('sidecar: spawn failure degrades — memory plane still boots', async () => {
  const env: NodeJS.ProcessEnv = { NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'nektar-dd-')) }
  const logs: string[] = []
  const dir = mkdtempSync(join(tmpdir(), 'nektar-rs-'))
  writeFileSync(join(dir, 'restate-server'), 'x')
  const st = await ensureRestate({
    env,
    log: (m) => logs.push(m),
    platform: 'linux',
    fetchImpl: deadFetch,
    spawnImpl: () => {
      throw new Error('execve failed')
    },
    restateDir: dir
  })
  assert.equal(st, 'degraded')
  assert.ok(logs.some((m) => m.includes('spawn failed')))
})

void test('sidecar: native spawn + admin answers → spawned, cleanup kills the child', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-rs-'))
  writeFileSync(join(dir, 'restate-server'), 'x')
  const env: NodeJS.ProcessEnv = { NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'nektar-dd-')) }
  const { c, killed } = fakeChild()
  const cleanups: (() => void)[] = []
  // dead until spawnImpl "boots" the server — the adoption probe must lose
  let up = false
  const st = await ensureRestate({
    env,
    platform: 'linux',
    fetchImpl: (async (url: string | URL | Request) => {
      if (up && String(url).startsWith('http://127.0.0.1:19070')) return new Response('{}', { status: 200 })
      throw new Error('ECONNREFUSED')
    }) as typeof fetch,
    spawnImpl: () => {
      up = true
      return c
    },
    restateDir: dir,
    onCleanup: (fn) => cleanups.push(fn)
  })
  assert.equal(st, 'spawned')
  assert.equal(env.NEKTAR_RESTATE_URL, 'http://127.0.0.1:18080')
  assert.equal(env.NEKTAR_RESTATE_ADMIN, 'http://127.0.0.1:19070')
  assert.equal(cleanups.length, 1)
  cleanups[0]()
  assert.equal(killed.length, 1)
})

void test('sidecar: win32 spawns through wsl.exe, endpoint URI advertises the WSL gateway, cleanup pkills inside the VM', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-rs-'))
  writeFileSync(join(dir, 'restate-server'), 'x')
  const env: NodeJS.ProcessEnv = { NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'nektar-dd-')) }
  const cmds: string[][] = []
  const { c, killed } = fakeChild()
  const spawned: [string, string[]][] = []
  const cleanups: (() => void)[] = []
  let up = false
  const st = await ensureRestate({
    env,
    platform: 'win32',
    fetchImpl: (async (url: string | URL | Request) => {
      if (up && String(url).startsWith('http://127.0.0.1:19070')) return new Response('{}', { status: 200 })
      throw new Error('ECONNREFUSED')
    }) as typeof fetch,
    spawnSyncImpl: wslStub(cmds),
    spawnImpl: (cmd, args) => {
      up = true
      spawned.push([cmd, args])
      return c
    },
    restateDir: dir,
    onCleanup: (fn) => cleanups.push(fn)
  })
  assert.equal(st, 'spawned')
  assert.equal(spawned[0]?.[0], 'wsl.exe')
  assert.ok(
    spawned[0]?.[1].some((a) => a.startsWith('/mnt/')),
    'binary must be a /mnt path'
  )
  assert.equal(env.NEKTAR_RESTATE_ENDPOINT_URI, 'http://172.21.112.1:9080')
  // cleanup: child.kill + pkill -f <bin> inside the VM — wsl.exe's death alone
  // orphans the linux process (verified live)
  cleanups[0]()
  assert.equal(killed.length, 1)
  assert.ok(cmds.some((a) => a[0] === 'wsl.exe' && a.includes('pkill') && a.includes('-f')))
})

void test('sidecar: explicit NEKTAR_RESTATE_ENDPOINT_URI is never overwritten', async () => {
  const env: NodeJS.ProcessEnv = { NEKTAR_RESTATE_ENDPOINT_URI: 'http://mine:9999' }
  const st = await ensureRestate({
    env,
    platform: 'win32',
    fetchImpl: okFetch(['http://127.0.0.1:19070']),
    spawnImpl: () => assert.fail('must not spawn')
  })
  assert.equal(st, 'adopted')
  assert.equal(env.NEKTAR_RESTATE_ENDPOINT_URI, 'http://mine:9999')
})
