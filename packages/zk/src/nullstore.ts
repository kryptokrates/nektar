// NullifierStore — replay protection for membership proofs.
import { readFileSync, writeFileSync, existsSync, mkdirSync } from 'node:fs'
import { dirname } from 'node:path'

export interface NullifierStore {
  has (n: bigint): boolean
  add (n: bigint): void // throws on replay
  seen (): number
}

export class FileNullifierStore implements NullifierStore {
  private set = new Set<string>()
  constructor (private file: string) {
    if (existsSync(file)) for (const n of JSON.parse(readFileSync(file, 'utf8'))) this.set.add(n)
  }
  has (n: bigint): boolean { return this.set.has(n.toString()) }
  add (n: bigint): void {
    const k = n.toString()
    if (this.set.has(k)) throw new Error(`nullifier replay: ${k.slice(0, 16)}…`)
    this.set.add(k)
    mkdirSync(dirname(this.file), { recursive: true })
    writeFileSync(this.file, JSON.stringify([...this.set]))
  }
  seen (): number { return this.set.size }
}
export class MemoryNullifierStore implements NullifierStore {
  private set = new Set<string>()
  has (n: bigint): boolean { return this.set.has(n.toString()) }
  add (n: bigint): void {
    if (this.set.has(n.toString())) throw new Error('nullifier replay')
    this.set.add(n.toString())
  }
  seen (): number { return this.set.size }
}
