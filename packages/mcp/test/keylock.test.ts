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
  assert.ok(keyLockFile('ab'.repeat(20)).includes('nektar-key-'), 'lock file names the key')
})
