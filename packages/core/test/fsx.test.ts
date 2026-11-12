import { test } from 'node:test'
import assert from 'node:assert/strict'
import { existsSync, mkdtempSync, readFileSync, readdirSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join } from 'node:path'
import { writeFileAtomic, writeJsonAtomic, utf8 } from '../src/index.js'

test('fsx: writeFileAtomic lands bytes via tmp+rename; writeJsonAtomic round-trips', () => {
  const dir = mkdtempSync(join(tmpdir(), 'nektar-fsx-'))
  const f = join(dir, 'sub', 'state.json')
  writeJsonAtomic(f, { a: 1 })
  assert.deepEqual(JSON.parse(readFileSync(f, 'utf8')), { a: 1 }, 'dir created + json lands')
  const f2 = join(dir, 'blob.bin')
  writeFileAtomic(f2, utf8('payload'))
  assert.equal(readFileSync(f2).toString(), 'payload')
  // a second write replaces atomically — the tmp sibling never survives
  writeJsonAtomic(f, { a: 2 })
  assert.deepEqual(JSON.parse(readFileSync(f, 'utf8')), { a: 2 })
  assert.equal(readdirSync(dir).filter((x) => x.endsWith('.tmp')).length, 0, 'no tmp files left behind')
  assert.equal(existsSync(`${f}.tmp`), false)
})
