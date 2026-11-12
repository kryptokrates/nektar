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
  renameSync(tmp, file)
}

/** JSON-serializing convenience over writeFileAtomic. */
export function writeJsonAtomic(file: string, value: unknown, pretty = false): void {
  writeFileAtomic(file, pretty ? JSON.stringify(value, null, 1) : JSON.stringify(value))
}
