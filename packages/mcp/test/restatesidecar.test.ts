import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'
import { EventEmitter } from 'node:events'
import type { ChildProcess, SpawnOptions, SpawnSyncReturns } from 'node:child_process'
import { Dispatcher, EXEC_SVC, setExecutor, inertTxEffects } from '@nektar/automation'
import { ensureRestate } from '../src/restatesidecar.js'
import { openConfirmerWatch } from '../src/rtsupport.js'

// the sidecar's resolution order — every external seam (fetch, spawn, spawnSync,
// platform, restateDir) is injected so the tests never touch a real restate

const fakeChild = (): { c: ChildProcess; killed: boolean[]; fire: (ev: string, arg?: unknown) => void } => {
  const killed: boolean[] = []
  const handlers = new Map<string, (arg?: unknown) => void>()
  const c = {
    kill: () => {
      killed.push(true)
      return true
    },
    unref: () => {},
    on: () => c,
    once: (ev: string, fn: (arg?: unknown) => void) => {
      handlers.set(ev, fn)
      return c
    }
  } as unknown as ChildProcess
  return { c, killed, fire: (ev, arg) => handlers.get(ev)?.(arg) }
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

void test('sidecar: no bundled binary → unavailable with a log naming NEKTAR_RESTATE_URL, nothing spawned', async () => {
  const env: NodeJS.ProcessEnv = {}
  const logs: string[] = []
  const st = await ensureRestate({
    env,
    log: (m) => logs.push(m),
    fetchImpl: deadFetch,
    spawnImpl: () => assert.fail('must not spawn'),
    restateDir: mkdtempSync(join(tmpdir(), 'nektar-nors-'))
  })
  assert.equal(st, 'unavailable')
  assert.ok(logs.some((m) => m.includes('NEKTAR_RESTATE_URL')))
  assert.equal(env.NEKTAR_RESTATE_URL, undefined)
})

void test('sidecar: spawn failure → unavailable — the caller refuses to boot', async () => {
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
  assert.equal(st, 'unavailable')
  assert.ok(logs.some((m) => m.includes('spawn failed')))
})

void test('sidecar: child dying at exec → unavailable immediately — no 30s poll', async () => {
  // the glibc-on-bookworm exec failure seen in the image smoke: the binary
  // can't even start, admin never answers — the child's error/exit event must
  // cut the wait short instead of polling a dead port for the full deadline
  const dir = mkdtempSync(join(tmpdir(), 'nektar-rs-'))
  writeFileSync(join(dir, 'restate-server'), 'x')
  const env: NodeJS.ProcessEnv = { NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'nektar-dd-')) }
  const logs: string[] = []
  const { c, fire } = fakeChild()
  const t0 = Date.now()
  const st = await ensureRestate({
    env,
    log: (m) => logs.push(m),
    platform: 'linux',
    fetchImpl: deadFetch,
    spawnImpl: () => {
      setTimeout(() => fire('error', new Error('ENOENT: no such file or directory')), 0)
      return c
    },
    restateDir: dir
  })
  assert.equal(st, 'unavailable')
  assert.ok(Date.now() - t0 < 5_000)
  assert.ok(logs.some((m) => m.includes('sidecar died')))
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

void test('sidecar: WSL present but no distro → unavailable with the install hint, nothing spawned', async () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-rs-'))
  writeFileSync(join(dir, 'restate-server'), 'x')
  const env: NodeJS.ProcessEnv = { NEKTAR_DATA_DIR: mkdtempSync(join(tmpdir(), 'nektar-dd-')) }
  const logs: string[] = []
  // `wsl --status` passes (feature on) but `wsl -e true` fails — the
  // distro-less shape the windows-latest CI runner actually ships
  const st = await ensureRestate({
    env,
    log: (m) => logs.push(m),
    platform: 'win32',
    fetchImpl: deadFetch,
    spawnSyncImpl: ((cmd: string, args?: readonly string[]) => {
      if (cmd === 'wsl.exe' && args?.[0] === '--status') return { status: 0 }
      return { status: 1 }
    }) as unknown as SpawnSyncImpl,
    spawnImpl: () => assert.fail('must not spawn'),
    restateDir: dir
  })
  assert.equal(st, 'unavailable')
  assert.ok(logs.some((m) => m.includes('wsl --install -d Ubuntu')))
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

void test('openConfirmerWatch: wake arriving mid-pass triggers one follow-up pass (rt/lost-coalesced-wake)', async () => {
  const ex = new Dispatcher()
  const memEvents = new EventEmitter()
  const chainEvents = new EventEmitter()
  let anchorCb: (() => void) | undefined
  let releaseWatch1!: () => void
  const watch1Gate = new Promise<void>((r) => (releaseWatch1 = r))
  let watchRuns = 0
  let releaseConf1!: () => void
  const conf1Gate = new Promise<void>((r) => (releaseConf1 = r))
  let confRuns = 0
  ex.register(EXEC_SVC.watcher, 'tick', async () => {
    watchRuns++
    if (watchRuns === 1) await watch1Gate
    return { done: true }
  })
  ex.register(EXEC_SVC.confirmer, 'tick', async () => {
    confRuns++
    if (confRuns === 1) await conf1Gate
    return { done: true }
  })
  setExecutor(ex)
  try {
    const cw = await openConfirmerWatch({
      cfg: { confirmEveryMs: 60_000 },
      mem: {
        events: memEvents,
        onAnchorQueued: (cb: () => void) => {
          anchorCb = cb
        },
        flushDue: () => Promise.resolve(),
        unprovenAnchors: () => [],
        fetchTx: () => Promise.resolve(undefined)
      } as never,
      port: {} as never,
      events: Object.assign(chainEvents, { watch: () => {}, statusOf: () => undefined, pending: 0 }) as never,
      ledger: { walletTxs: () => Promise.resolve([]), prune: () => Promise.resolve({ inbox: 0, outbox: 0, events: 0, txEvents: 0 }) } as never,
      chain: { broadcast: () => Promise.resolve({ status: 'success' }) } as never,
      txStore: { unproven: () => [], get: () => undefined } as never,
      txEffects: inertTxEffects,
      net: 'regtest',
      nlog: () => {},
      persistKeys: () => {},
      topUp: undefined
    })
    try {
      // two watcher wakes while pass 1 is in-flight → must coalesce to exactly 1 follow-up (total 2)
      memEvents.emit('broadcasterStatus', { txid: '00'.repeat(32), status: 'MINED' })
      memEvents.emit('broadcasterStatus', { txid: '11'.repeat(32), status: 'MINED' })
      memEvents.emit('broadcasterStatus', { txid: '22'.repeat(32), status: 'MINED' })
      // two anchor wakes while pass 1 is in-flight → must coalesce to exactly 1 follow-up (total 2)
      anchorCb?.()
      anchorCb?.()
      anchorCb?.()
      releaseWatch1()
      releaseConf1()
      await new Promise((r) => setTimeout(r, 50))
      assert.equal(watchRuns, 2, 'mid-pass watcher wake must trigger exactly one follow-up pass')
      assert.equal(confRuns, 2, 'mid-pass anchor wake must trigger exactly one follow-up pass')
    } finally {
      cw.close()
    }
  } finally {
    setExecutor(undefined)
    await ex.close()
  }
})
