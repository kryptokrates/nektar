import { test } from 'node:test'
import assert from 'node:assert/strict'
import { mkdtempSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import { tmpdir } from 'node:os'

// G7: one process per fund key — the heartbeat lock refuses a live second
// holder and takes over a stale one
const mod = async () =>
  (await import('../src/runtime.js')) as {
    acquireLock?: (file: string, label: string) => Promise<() => Promise<void>>
    keyLockFile?: (pkhHex: string) => string
  }

test('key lock: a live holder refuses the second acquire; release frees it; a stale beat is taken over', async () => {
  const { acquireLock, keyLockFile } = await mod()
  assert.ok(acquireLock !== undefined, 'runtime must export acquireLock')
  assert.ok(keyLockFile !== undefined, 'runtime must export keyLockFile')
  const dir = mkdtempSync(join(tmpdir(), 'nektar-keylock-'))
  const file = join(dir, 'k.lock')
  // a live foreign holder refuses; the holder is a different pid@host
  writeFileSync(file, JSON.stringify({ owner: '99999@elsewhere', beat: Date.now() }))
  await assert.rejects(acquireLock(file, 'key'), /in use by another nektar/)
  // free file → acquire, release, re-acquire
  const release = await acquireLock(join(dir, 'k2.lock'), 'key')
  await release()
  const release2 = await acquireLock(join(dir, 'k2.lock'), 'key')
  await release2()
  // a stale heartbeat is taken over
  writeFileSync(file, JSON.stringify({ owner: '99999@elsewhere', beat: Date.now() - 60_000 }))
  const release3 = await acquireLock(file, 'key')
  await release3()
  // a dead LOCAL pid is displaced at once even with a fresh-looking beat —
  // the killed-process case a 20s heartbeat cannot see
  const deadLocal = join(dir, 'dead.lock')
  writeFileSync(deadLocal, JSON.stringify({ owner: `99999999@${(await import('node:os')).hostname()}`, beat: Date.now() }))
  const release4 = await acquireLock(deadLocal, 'key')
  await release4()
  // a corrupt lock file: fresh mtime refuses (a live writer could have left
  // it), an old corpse yields
  const corrupt = join(dir, 'corrupt.lock')
  writeFileSync(corrupt, 'not json {{{')
  const { utimesSync } = await import('node:fs')
  const old = new Date(Date.now() - 120_000)
  utimesSync(corrupt, old, old)
  const release5 = await acquireLock(corrupt, 'key')
  await release5()
  assert.ok(keyLockFile('ab'.repeat(20)).includes('nektar-key-'), 'lock file names the key')
})
