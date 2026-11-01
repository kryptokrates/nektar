// File-backed SecurityStateStore: challenges, used ZK transcripts, nullifiers
// and revocations survive a restart (replay stays rejected). Atomic replace.
import { existsSync, readFileSync, writeFileSync, renameSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'
import type { SecurityStateStore, MemoryStateStore, PersistedMemoryState } from './memory.js'

/** JSON file store for anchor state (atomic replace) */
export class FileStateStore implements MemoryStateStore {
  constructor (private file: string) {}
  load (): PersistedMemoryState | undefined {
    return existsSync(this.file) ? JSON.parse(readFileSync(this.file, 'utf8')) as PersistedMemoryState : undefined
  }
  save (s: PersistedMemoryState): void {
    mkdirSync(dirname(this.file), { recursive: true })
    writeFileSync(`${this.file}.tmp`, JSON.stringify(s)); renameSync(`${this.file}.tmp`, this.file)
  }
}

type State = ReturnType<SecurityStateStore['load']>
const EMPTY: State = { challenges: [], zkSeen: [], nullifiers: {}, revocations: [] }

export class FileSecurityStore implements SecurityStateStore {
  constructor (private file: string) {}
  load (): State {
    if (!existsSync(this.file)) return { ...EMPTY, nullifiers: {} }
    return { ...EMPTY, ...JSON.parse(readFileSync(this.file, 'utf8')) as Partial<State> }
  }
  save (s: State): void {
    mkdirSync(dirname(this.file), { recursive: true })
    const tmp = `${this.file}.tmp`
    writeFileSync(tmp, JSON.stringify(s))
    renameSync(tmp, this.file)
  }
}
