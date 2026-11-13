// Crash-safe file writes — the tmp+rename idiom. A kill mid-write leaves the
// pre-write file intact instead of a torn file that a load() silently parses
// as empty or corrupt. rename is atomic within one filesystem; all our state
// files put the tmp sibling next to the target for exactly that reason.
import { writeFileSync, renameSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'

/** write `data` to `file` via a `<file>.tmp` sibling + rename. */
export function writeFileAtomic(file: string, data: Uint8Array | string): void {
  mkdirSync(dirname(file), { recursive: true })
  const tmp = `${file}.tmp`
  writeFileSync(tmp, data)
  // Windows: renameSync hits transient EPERM/EEXIST/EBUSY/EACCES while an AV
  // scan or indexer briefly holds the destination — retry over that window
  // instead of aborting the process on a ~50ms lock.
  for (let i = 0; ; i++) {
    try {
      renameSync(tmp, file)
      return
    } catch (e) {
      const code = (e as NodeJS.ErrnoException).code
      if (i >= 5 || !['EPERM', 'EEXIST', 'EBUSY', 'EACCES'].includes(code ?? '')) throw e
      Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, 50)
    }
  }
}

/** JSON-serializing convenience over writeFileAtomic. */
export function writeJsonAtomic(file: string, value: unknown, pretty = false): void {
  writeFileAtomic(file, pretty ? JSON.stringify(value, null, 1) : JSON.stringify(value))
}
