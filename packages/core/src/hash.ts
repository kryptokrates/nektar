import { createHash } from 'node:crypto'

export type Bytes = Uint8Array

export function sha256(b: Bytes): Bytes {
  return new Uint8Array(createHash('sha256').update(Buffer.from(b)).digest())
}

export function dsha256(b: Bytes): Bytes {
  return sha256(sha256(b))
}

export function concat(...parts: Bytes[]): Bytes {
  return new Uint8Array(Buffer.concat(parts.map((p) => Buffer.from(p))))
}

export function toHex(b: Bytes): string {
  return Buffer.from(b).toString('hex')
}

export function fromHex(s: string): Bytes {
  return new Uint8Array(Buffer.from(s, 'hex'))
}

export function utf8(s: string): Bytes {
  return new Uint8Array(Buffer.from(s, 'utf8'))
}

export function u8(n: number): Bytes {
  return Uint8Array.of(n & 0xff)
}

export function u64le(n: number | bigint): Bytes {
  const b = Buffer.alloc(8)
  b.writeBigUInt64LE(BigInt(n))
  return new Uint8Array(b)
}

export function readU64le(b: Bytes): number {
  return Number(Buffer.from(b).readBigUInt64LE())
}

export function eq(a: Bytes, b: Bytes): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

export const ZERO32: Bytes = new Uint8Array(32)

export function hash160(b: Bytes): Bytes {
  return new Uint8Array(
    createHash('ripemd160')
      .update(Buffer.from(sha256(b)))
      .digest()
  )
}
