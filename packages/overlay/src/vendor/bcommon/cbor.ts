/**
 * The deterministic CBOR that committed records are written in: the twin of
 * this repository's Go package cbor, held to the same vectors.
 *
 * The codec is the subset of RFC 8949 a record needs, with the core
 * deterministic encoding rules of section 4.2.1 enforced on BOTH sides. The
 * encoder only ever writes canonical bytes; the decoder refuses anything that
 * is not canonical or is outside the subset, so a record that decodes is byte
 * for byte the record its encoder produced, and a hostile publisher cannot
 * hand a host two different byte strings for one value.
 *
 * Supported: unsigned integers (major type 0), negative integers (1), byte
 * strings (2), text strings (3, valid UTF-8), arrays (4), maps (5, keys in
 * bytewise lexicographic order of their encodings, no duplicates), and the
 * simple values false, true and null (7). Refused: indefinite lengths, tags,
 * floats, undefined, every other simple value, and nesting deeper than
 * MaxDepth. Nothing here allocates from a declared length before the bytes
 * are known to be present.
 *
 * Integers are bigint on both sides, because a record's integers may be as
 * wide as uint64 and a JS number stops being exact at 2^53. A map is a
 * list of pairs in wire order rather than a JS Map keyed by string, so keys
 * of any supported type can be carried and unknown keys keep their position.
 */

/**
 * MaxDepth bounds nesting. A committed record is two levels deep (a map
 * whose body is a map); sixteen leaves room for a body that nests without letting a
 * hostile item recurse without limit.
 */
export const MaxDepth = 16

export type CborErrorCode =
  | 'not-canonical'
  | 'unsupported'
  | 'truncated'
  | 'trailing'
  | 'depth'
  | 'duplicate-key'
  | 'key-order'
  | 'utf8'

export class CborError extends Error {
  constructor(
    readonly code: CborErrorCode,
    detail?: string,
  ) {
    super(detail === undefined ? `cbor: ${code}` : `cbor: ${code}: ${detail}`)
    this.name = 'CborError'
  }
}

/**
 * One decoded item. A negative integer is a bigint below zero; a decoder
 * never produces a non-negative value through the negative path.
 */
export type Value = bigint | Uint8Array | string | Value[] | CborMap | boolean | null

export interface Pair {
  key: Value
  val: Value
}

/**
 * A CBOR map: entries in the order given. encode sorts them; decode returns
 * them in the (already canonical) wire order.
 */
export class CborMap {
  constructor(readonly entries: Pair[] = []) {}

  /** The value for key, matching by encoded key bytes. */
  get(key: Value): Value | undefined {
    let kb: Uint8Array
    try {
      kb = encode(key)
    } catch {
      return undefined
    }
    for (const p of this.entries) {
      try {
        if (bytesEqual(encode(p.key), kb)) return p.val
      } catch {
        // An entry whose key does not encode cannot match anything.
      }
    }
    return undefined
  }
}

export function bytesEqual(a: Uint8Array, b: Uint8Array): boolean {
  if (a.length !== b.length) return false
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false
  return true
}

/** Bytewise lexicographic order; a proper prefix sorts first. */
export function compareBytes(a: Uint8Array, b: Uint8Array): number {
  const n = Math.min(a.length, b.length)
  for (let i = 0; i < n; i++) {
    const x = a[i] ?? 0
    const y = b[i] ?? 0
    if (x !== y) return x < y ? -1 : 1
  }
  return a.length === b.length ? 0 : a.length < b.length ? -1 : 1
}

const MaxUint64 = (1n << 64n) - 1n
const MaxInt64 = (1n << 63n) - 1n

// A JS string may hold a lone surrogate, which TextEncoder would quietly turn
// into U+FFFD; Go's utf8.ValidString refuses it, so this does too.
const loneSurrogate = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/

const utf8Encoder = new TextEncoder()
// ignoreBOM keeps a leading U+FEFF in the decoded string; the default would
// strip it and a re-encode would then differ from the bytes decoded.
const utf8Decoder = new TextDecoder('utf-8', { fatal: true, ignoreBOM: true })

/** Encode v in core deterministic encoding. */
export function encode(v: Value): Uint8Array {
  const out: number[] = []
  encodeInto(out, v, 0)
  return Uint8Array.from(out)
}

/** The initial byte and the shortest argument encoding. */
function head(out: number[], mt: number, n: bigint): void {
  const m = mt << 5
  if (n < 24n) {
    out.push(m | Number(n))
  } else if (n <= 0xffn) {
    out.push(m | 24, Number(n))
  } else if (n <= 0xffffn) {
    out.push(m | 25, Number(n >> 8n), Number(n & 0xffn))
  } else if (n <= 0xffffffffn) {
    out.push(m | 26, Number(n >> 24n), Number((n >> 16n) & 0xffn), Number((n >> 8n) & 0xffn), Number(n & 0xffn))
  } else {
    out.push(m | 27)
    for (let s = 56n; s >= 0n; s -= 8n) out.push(Number((n >> s) & 0xffn))
  }
}

function encodeInto(out: number[], v: Value, depth: number): void {
  if (depth > MaxDepth) throw new CborError('depth')
  if (v === null) {
    out.push(0xf6)
  } else if (typeof v === 'boolean') {
    out.push(v ? 0xf5 : 0xf4)
  } else if (typeof v === 'bigint') {
    if (v >= 0n) {
      if (v > MaxUint64) throw new CborError('unsupported', 'integer above uint64')
      head(out, 0, v)
    } else {
      const n = -1n - v
      if (n > MaxUint64) throw new CborError('unsupported', 'integer below -2^64')
      head(out, 1, n)
    }
  } else if (v instanceof Uint8Array) {
    head(out, 2, BigInt(v.length))
    for (const b of v) out.push(b)
  } else if (typeof v === 'string') {
    if (loneSurrogate.test(v)) throw new CborError('utf8')
    const b = utf8Encoder.encode(v)
    head(out, 3, BigInt(b.length))
    for (const x of b) out.push(x)
  } else if (Array.isArray(v)) {
    head(out, 4, BigInt(v.length))
    for (const e of v) encodeInto(out, e, depth + 1)
  } else if (v instanceof CborMap) {
    const entries = v.entries.map((p) => {
      const k: number[] = []
      const val: number[] = []
      encodeInto(k, p.key, depth + 1)
      encodeInto(val, p.val, depth + 1)
      return { k: Uint8Array.from(k), v: val }
    })
    // Array.prototype.sort is stable, as sort.SliceStable is on the Go side.
    entries.sort((a, b) => compareBytes(a.k, b.k))
    for (let i = 1; i < entries.length; i++) {
      const prev = entries[i - 1]
      const cur = entries[i]
      if (prev !== undefined && cur !== undefined && bytesEqual(prev.k, cur.k)) throw new CborError('duplicate-key')
    }
    head(out, 5, BigInt(entries.length))
    for (const e of entries) {
      for (const x of e.k) out.push(x)
      for (const x of e.v) out.push(x)
    }
  } else {
    // A JS number is deliberately not a Value: it cannot say whether it is an
    // integer or a float, and floats are outside the subset.
    throw new CborError('unsupported', typeof v)
  }
}

/** Parse exactly one canonical item and refuse trailing bytes. */
export function decodeValue(b: Uint8Array): Value {
  const d = new Decoder(b)
  const v = d.item(0)
  if (d.i !== b.length) throw new CborError('trailing')
  return v
}

class Decoder {
  i = 0

  constructor(readonly b: Uint8Array) {}

  private byte(at: number): number {
    const v = this.b[at]
    if (v === undefined) throw new CborError('truncated')
    return v
  }

  /**
   * The argument for an initial byte whose additional information is ai,
   * enforcing the shortest encoding.
   */
  private arg(ai: number): bigint {
    if (ai < 24) return BigInt(ai)
    if (ai === 24) {
      if (this.i + 1 > this.b.length) throw new CborError('truncated')
      const n = this.byte(this.i)
      this.i += 1
      if (n < 24) throw new CborError('not-canonical')
      return BigInt(n)
    }
    if (ai === 25) {
      if (this.i + 2 > this.b.length) throw new CborError('truncated')
      const n = (this.byte(this.i) << 8) | this.byte(this.i + 1)
      this.i += 2
      if (n <= 0xff) throw new CborError('not-canonical')
      return BigInt(n)
    }
    if (ai === 26) {
      if (this.i + 4 > this.b.length) throw new CborError('truncated')
      let n = 0n
      for (let k = 0; k < 4; k++) n = (n << 8n) | BigInt(this.byte(this.i + k))
      this.i += 4
      if (n <= 0xffffn) throw new CborError('not-canonical')
      return n
    }
    if (ai === 27) {
      if (this.i + 8 > this.b.length) throw new CborError('truncated')
      let n = 0n
      for (let k = 0; k < 8; k++) n = (n << 8n) | BigInt(this.byte(this.i + k))
      this.i += 8
      if (n <= 0xffffffffn) throw new CborError('not-canonical')
      return n
    }
    // 28..30 are reserved, 31 is an indefinite length; neither is canonical.
    throw new CborError('unsupported', `additional information ${ai}`)
  }

  item(depth: number): Value {
    if (depth > MaxDepth) throw new CborError('depth')
    if (this.i >= this.b.length) throw new CborError('truncated')
    const ib = this.byte(this.i)
    this.i += 1
    const mt = ib >> 5
    const ai = ib & 0x1f

    if (mt === 7) {
      // Simple values carry their meaning in the additional information;
      // floats and the one-byte simple-value form are not in the subset.
      switch (ai) {
        case 20:
          return false
        case 21:
          return true
        case 22:
          return null
        default:
          throw new CborError('unsupported', `simple ${ai}`)
      }
    }

    const n = this.arg(ai)
    const remaining = BigInt(this.b.length - this.i)

    switch (mt) {
      case 0:
        return n
      case 1:
        if (n > MaxInt64) throw new CborError('unsupported', 'negative integer below int64')
        return -1n - n
      case 2: {
        if (n > remaining) throw new CborError('truncated')
        const len = Number(n)
        const out = this.b.slice(this.i, this.i + len)
        this.i += len
        return out
      }
      case 3: {
        if (n > remaining) throw new CborError('truncated')
        const len = Number(n)
        const s = this.b.subarray(this.i, this.i + len)
        let text: string
        try {
          text = utf8Decoder.decode(s)
        } catch {
          throw new CborError('utf8')
        }
        this.i += len
        return text
      }
      case 4: {
        // Every element is at least one byte, so a count beyond the bytes
        // left is refused before anything is allocated for it.
        if (n > remaining) throw new CborError('truncated')
        const len = Number(n)
        const out: Value[] = []
        for (let k = 0; k < len; k++) out.push(this.item(depth + 1))
        return out
      }
      case 5: {
        if (n > remaining / 2n) throw new CborError('truncated')
        const len = Number(n)
        const out: Pair[] = []
        let prev: Uint8Array | undefined
        for (let k = 0; k < len; k++) {
          const start = this.i
          const key = this.item(depth + 1)
          const kb = this.b.subarray(start, this.i)
          if (prev !== undefined) {
            const c = compareBytes(prev, kb)
            if (c === 0) throw new CborError('duplicate-key')
            if (c > 0) throw new CborError('key-order')
          }
          prev = kb
          const val = this.item(depth + 1)
          out.push({ key, val })
        }
        return new CborMap(out)
      }
      default:
        // 6, tags
        throw new CborError('unsupported', 'tag')
    }
  }
}
