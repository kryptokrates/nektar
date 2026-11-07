/**
 * The bounded, ordered reading of one application record, the twin of the Go
 * package record: a canonical CBOR map with unsigned-integer keys, whose key
 * 0 is the record's magic and whose keys above the last one a version
 * defines are preserved and ignored.
 *
 * A record is refused for the first rule it breaks, in one order, so that
 * the two languages refuse the same bytes for the same reason:
 *
 *   1. too-large: the bytes exceed the record's bound, checked before
 *      anything is decoded;
 *   2. cbor: not one canonical CBOR item, or not a map;
 *   3. too-large: more than MaxKeys entries;
 *   4. key-type: a key that is not an unsigned integer;
 *   5. magic: key 0 is not the byte string the caller names;
 *   6. then each defined key as the caller reads it, in the caller's order:
 *      missing, type, and range or list for the key's own rule.
 *
 * The application supplies the bound, the last defined key and the magic,
 * reads its own fields, and supplies the error a refusal is thrown as, so
 * that a record refusal is one of its own refusals and not a second kind
 * its callers must tell apart.
 */
import { CborError, CborMap, bytesEqual, compareBytes, decodeValue, encode, type Pair, type Value } from './cbor.js'

/** The entries a record map may hold, unknown keys included. */
export const MaxKeys = 64

/** The fixed labels of the record steps, the Go record.Reason. */
export type RecordReason = 'too-large' | 'cbor' | 'key-type' | 'magic' | 'missing' | 'type' | 'range' | 'list'

/** A decoded record: its known integer keys, and the ones above the last defined key. */
export interface RecordFields {
  known: Map<number, Value>
  /** The pairs whose keys are above the last defined key, in the record's order, as decoded. */
  extra: Pair[]
}

/** The reader an application gets for its own refusal type. */
export interface RecordReader {
  /** Steps 1 to 3: the bound, one canonical CBOR map, at most MaxKeys entries. */
  decodeMap(b: Uint8Array, max: number): CborMap
  /** Step 4: every key an unsigned integer; those above last are preserved. */
  split(m: CborMap, last: number | bigint): RecordFields
  /** Step 5: key 0 present, a byte string, and equal to magic. */
  checkMagic(f: RecordFields, magic: Uint8Array): void
  /** Steps 1 to 5 in order. */
  decode(b: Uint8Array, max: number, last: number | bigint, magic: Uint8Array): RecordFields
  /** Key k's value, or the refusal missing. */
  need(f: RecordFields, k: number): Value
  /** Key k as a byte string of any length. */
  bytes(f: RecordFields, k: number): Uint8Array
  /** Key k as a byte string of exactly n bytes; a negative n takes any length. */
  bytesN(f: RecordFields, k: number, n: number): Uint8Array
  /** Key k as a byte string of lo to hi bytes. */
  bytesRange(f: RecordFields, k: number, lo: number, hi: number): Uint8Array
  /** Key k as a text string. */
  text(f: RecordFields, k: number): string
  /** Key k as an unsigned integer within lo to hi, which must be safe integers. */
  uint(f: RecordFields, k: number, lo: number, hi: number): number
  /** Key k as a boolean. */
  bool(f: RecordFields, k: number): boolean
  /** Key k as an array of at most max elements. */
  array(f: RecordFields, k: number, max: number): Value[]
  /** Key k as an array of at most max 32-byte strings in strictly ascending byte order. */
  list32(f: RecordFields, k: number, max: number): Uint8Array[]
  /** n held to lo through hi, or the refusal range naming what. */
  inRange(n: number, lo: number, hi: number, what: string): void
  /** Preserved pairs held to unsigned-integer keys above last. */
  checkExtraKeys(extra: Pair[], last: number | bigint): void
  /** checkExtraKeys, and the preserved pairs and the defined ones are at most MaxKeys together. */
  checkExtra(extra: Pair[], last: number | bigint, defined: number): void
  /** The defined pairs then the preserved ones as one canonical CBOR map, held to max. */
  encode(m: Pair[], extra: Pair[], max: number): Uint8Array
}

/**
 * A record reader whose every refusal is the error refuse returns for the
 * step's label and a detail for people.
 */
export function recordReader(refuse: (reason: RecordReason, detail?: string) => Error): RecordReader {
  const decodeMap = (b: Uint8Array, max: number): CborMap => {
    if (b.length > max) throw refuse('too-large')
    let v: Value
    try {
      v = decodeValue(b)
    } catch (e) {
      if (e instanceof CborError) throw refuse('cbor', e.message)
      throw e
    }
    if (!(v instanceof CborMap)) throw refuse('cbor', 'not a map')
    if (v.entries.length > MaxKeys) throw refuse('too-large', `more than ${MaxKeys} keys`)
    return v
  }
  const split = (m: CborMap, last: number | bigint): RecordFields => {
    const top = BigInt(last)
    const known = new Map<number, Value>()
    const extra: Pair[] = []
    for (const p of m.entries) {
      if (typeof p.key !== 'bigint' || p.key < 0n) throw refuse('key-type')
      if (p.key > top) {
        extra.push(p)
        continue
      }
      known.set(Number(p.key), p.val)
    }
    return { known, extra }
  }
  const checkMagic = (f: RecordFields, magic: Uint8Array): void => {
    const b = f.known.get(0)
    if (!(b instanceof Uint8Array) || !bytesEqual(b, magic)) throw refuse('magic')
  }
  const need = (f: RecordFields, k: number): Value => {
    const v = f.known.get(k)
    if (v === undefined) throw refuse('missing', `key ${k}`)
    return v
  }
  const bytes = (f: RecordFields, k: number): Uint8Array => {
    const v = need(f, k)
    if (!(v instanceof Uint8Array)) throw refuse('type', `key ${k}`)
    return v
  }
  const array = (f: RecordFields, k: number, max: number): Value[] => {
    const v = need(f, k)
    if (!Array.isArray(v)) throw refuse('type', `key ${k}`)
    if (v.length > max) throw refuse('list', `key ${k}`)
    return v
  }
  const checkExtraKeys = (extra: Pair[], last: number | bigint): void => {
    const top = BigInt(last)
    for (const p of extra) {
      if (typeof p.key !== 'bigint' || p.key <= top) throw refuse('key-type')
    }
  }
  return {
    decodeMap,
    split,
    checkMagic,
    decode(b, max, last, magic) {
      const f = split(decodeMap(b, max), last)
      checkMagic(f, magic)
      return f
    },
    need,
    bytes,
    bytesN(f, k, n) {
      const v = bytes(f, k)
      if (n >= 0 && v.length !== n) throw refuse('range', `key ${k}`)
      return v
    },
    bytesRange(f, k, lo, hi) {
      const v = bytes(f, k)
      if (v.length < lo || v.length > hi) throw refuse('range', `key ${k}`)
      return v
    },
    text(f, k) {
      const v = need(f, k)
      if (typeof v !== 'string') throw refuse('type', `key ${k}`)
      return v
    },
    uint(f, k, lo, hi) {
      const v = need(f, k)
      if (typeof v !== 'bigint' || v < 0n) throw refuse('type', `key ${k}`)
      if (v < BigInt(lo) || v > BigInt(hi)) throw refuse('range', `key ${k}`)
      return Number(v)
    },
    bool(f, k) {
      const v = need(f, k)
      if (typeof v !== 'boolean') throw refuse('type', `key ${k}`)
      return v
    },
    array,
    list32(f, k, max) {
      const out = array(f, k, max).map((e) => {
        if (!(e instanceof Uint8Array) || e.length !== 32) throw refuse('list', `key ${k}`)
        return e
      })
      if (!ascending(out)) throw refuse('list', `key ${k}`)
      return out
    },
    inRange(n, lo, hi, what) {
      if (!Number.isSafeInteger(n) || n < lo || n > hi) throw refuse('range', what)
    },
    checkExtraKeys,
    checkExtra(extra, last, defined) {
      checkExtraKeys(extra, last)
      if (extra.length + defined > MaxKeys) throw refuse('too-large')
    },
    encode(m, extra, max) {
      const out = encode(new CborMap([...m, ...extra]))
      if (out.length > max) throw refuse('too-large')
      return out
    },
  }
}

/** Whether a list of byte strings is in strictly ascending byte order. */
export function ascending(a: Uint8Array[]): boolean {
  for (let i = 1; i < a.length; i++) if (compareBytes(a[i - 1]!, a[i]!) >= 0) return false
  return true
}

/**
 * Whether payload claims to be a record under magic, reading only its head:
 * a definite-length map head (0xa0 to 0xbb), key 0, then a byte-string head
 * of four bytes and magic itself. Key 0 always sorts first in a canonical
 * map with unsigned-integer keys. It allocates nothing and decides nothing
 * else: a payload that claims a record is then held to the reader's decode.
 */
export function claimsRecord(payload: Uint8Array, magic: Uint8Array): boolean {
  if (payload.length < 1 || payload[0]! >> 5 !== 5) return false
  const ai = payload[0]! & 0x1f
  let i: number
  if (ai < 24) i = 1
  else if (ai <= 27) i = 1 + 2 ** (ai - 24)
  else return false
  if (payload.length < i + 2 + magic.length || payload[i] !== 0x00 || payload[i + 1] !== 0x44) return false
  return bytesEqual(payload.subarray(i + 2, i + 2 + magic.length), magic)
}
