// @lightwebinc/blogs-host 0.6.3: tm_log_<stream> and ls_log, with @lightwebinc/bcommon 0.33.13 inlined; imports @bsv/sdk (built against 2.7.1)

// src/admit.ts
import { PublicKey as PublicKey2, Transaction } from "@bsv/sdk";

// node_modules/@lightwebinc/bcommon/dist/cbor.js
var MaxDepth = 16;
var CborError = class extends Error {
  code;
  constructor(code, detail) {
    super(detail === void 0 ? `cbor: ${code}` : `cbor: ${code}: ${detail}`);
    this.code = code;
    this.name = "CborError";
  }
};
var CborMap = class {
  entries;
  constructor(entries = []) {
    this.entries = entries;
  }
  /** The value for key, matching by encoded key bytes. */
  get(key) {
    let kb;
    try {
      kb = encode(key);
    } catch {
      return void 0;
    }
    for (const p of this.entries) {
      try {
        if (bytesEqual(encode(p.key), kb))
          return p.val;
      } catch {
      }
    }
    return void 0;
  }
};
function bytesEqual(a, b) {
  if (a.length !== b.length)
    return false;
  for (let i = 0; i < a.length; i++)
    if (a[i] !== b[i])
      return false;
  return true;
}
function compareBytes(a, b) {
  const n = Math.min(a.length, b.length);
  for (let i = 0; i < n; i++) {
    const x = a[i] ?? 0;
    const y = b[i] ?? 0;
    if (x !== y)
      return x < y ? -1 : 1;
  }
  return a.length === b.length ? 0 : a.length < b.length ? -1 : 1;
}
var MaxUint64 = (1n << 64n) - 1n;
var MaxInt64 = (1n << 63n) - 1n;
var loneSurrogate = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
var utf8Encoder = new TextEncoder();
var utf8Decoder = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
function encode(v) {
  const out = [];
  encodeInto(out, v, 0);
  return Uint8Array.from(out);
}
function head(out, mt, n) {
  const m = mt << 5;
  if (n < 24n) {
    out.push(m | Number(n));
  } else if (n <= 0xffn) {
    out.push(m | 24, Number(n));
  } else if (n <= 0xffffn) {
    out.push(m | 25, Number(n >> 8n), Number(n & 0xffn));
  } else if (n <= 0xffffffffn) {
    out.push(m | 26, Number(n >> 24n), Number(n >> 16n & 0xffn), Number(n >> 8n & 0xffn), Number(n & 0xffn));
  } else {
    out.push(m | 27);
    for (let s = 56n; s >= 0n; s -= 8n)
      out.push(Number(n >> s & 0xffn));
  }
}
function encodeInto(out, v, depth) {
  if (depth > MaxDepth)
    throw new CborError("depth");
  if (v === null) {
    out.push(246);
  } else if (typeof v === "boolean") {
    out.push(v ? 245 : 244);
  } else if (typeof v === "bigint") {
    if (v >= 0n) {
      if (v > MaxUint64)
        throw new CborError("unsupported", "integer above uint64");
      head(out, 0, v);
    } else {
      const n = -1n - v;
      if (n > MaxUint64)
        throw new CborError("unsupported", "integer below -2^64");
      head(out, 1, n);
    }
  } else if (v instanceof Uint8Array) {
    head(out, 2, BigInt(v.length));
    for (const b of v)
      out.push(b);
  } else if (typeof v === "string") {
    if (loneSurrogate.test(v))
      throw new CborError("utf8");
    const b = utf8Encoder.encode(v);
    head(out, 3, BigInt(b.length));
    for (const x of b)
      out.push(x);
  } else if (Array.isArray(v)) {
    head(out, 4, BigInt(v.length));
    for (const e of v)
      encodeInto(out, e, depth + 1);
  } else if (v instanceof CborMap) {
    const entries = v.entries.map((p) => {
      const k = [];
      const val = [];
      encodeInto(k, p.key, depth + 1);
      encodeInto(val, p.val, depth + 1);
      return { k: Uint8Array.from(k), v: val };
    });
    entries.sort((a, b) => compareBytes(a.k, b.k));
    for (let i = 1; i < entries.length; i++) {
      const prev = entries[i - 1];
      const cur = entries[i];
      if (prev !== void 0 && cur !== void 0 && bytesEqual(prev.k, cur.k))
        throw new CborError("duplicate-key");
    }
    head(out, 5, BigInt(entries.length));
    for (const e of entries) {
      for (const x of e.k)
        out.push(x);
      for (const x of e.v)
        out.push(x);
    }
  } else {
    throw new CborError("unsupported", typeof v);
  }
}
function decodeValue(b) {
  const d = new Decoder(b);
  const v = d.item(0);
  if (d.i !== b.length)
    throw new CborError("trailing");
  return v;
}
var Decoder = class {
  b;
  i = 0;
  constructor(b) {
    this.b = b;
  }
  byte(at) {
    const v = this.b[at];
    if (v === void 0)
      throw new CborError("truncated");
    return v;
  }
  /**
   * The argument for an initial byte whose additional information is ai,
   * enforcing the shortest encoding.
   */
  arg(ai) {
    if (ai < 24)
      return BigInt(ai);
    if (ai === 24) {
      if (this.i + 1 > this.b.length)
        throw new CborError("truncated");
      const n = this.byte(this.i);
      this.i += 1;
      if (n < 24)
        throw new CborError("not-canonical");
      return BigInt(n);
    }
    if (ai === 25) {
      if (this.i + 2 > this.b.length)
        throw new CborError("truncated");
      const n = this.byte(this.i) << 8 | this.byte(this.i + 1);
      this.i += 2;
      if (n <= 255)
        throw new CborError("not-canonical");
      return BigInt(n);
    }
    if (ai === 26) {
      if (this.i + 4 > this.b.length)
        throw new CborError("truncated");
      let n = 0n;
      for (let k = 0; k < 4; k++)
        n = n << 8n | BigInt(this.byte(this.i + k));
      this.i += 4;
      if (n <= 0xffffn)
        throw new CborError("not-canonical");
      return n;
    }
    if (ai === 27) {
      if (this.i + 8 > this.b.length)
        throw new CborError("truncated");
      let n = 0n;
      for (let k = 0; k < 8; k++)
        n = n << 8n | BigInt(this.byte(this.i + k));
      this.i += 8;
      if (n <= 0xffffffffn)
        throw new CborError("not-canonical");
      return n;
    }
    throw new CborError("unsupported", `additional information ${ai}`);
  }
  item(depth) {
    if (depth > MaxDepth)
      throw new CborError("depth");
    if (this.i >= this.b.length)
      throw new CborError("truncated");
    const ib = this.byte(this.i);
    this.i += 1;
    const mt = ib >> 5;
    const ai = ib & 31;
    if (mt === 7) {
      switch (ai) {
        case 20:
          return false;
        case 21:
          return true;
        case 22:
          return null;
        default:
          throw new CborError("unsupported", `simple ${ai}`);
      }
    }
    const n = this.arg(ai);
    const remaining = BigInt(this.b.length - this.i);
    switch (mt) {
      case 0:
        return n;
      case 1:
        if (n > MaxInt64)
          throw new CborError("unsupported", "negative integer below int64");
        return -1n - n;
      case 2: {
        if (n > remaining)
          throw new CborError("truncated");
        const len = Number(n);
        const out = this.b.slice(this.i, this.i + len);
        this.i += len;
        return out;
      }
      case 3: {
        if (n > remaining)
          throw new CborError("truncated");
        const len = Number(n);
        const s = this.b.subarray(this.i, this.i + len);
        let text;
        try {
          text = utf8Decoder.decode(s);
        } catch {
          throw new CborError("utf8");
        }
        this.i += len;
        return text;
      }
      case 4: {
        if (n > remaining)
          throw new CborError("truncated");
        const len = Number(n);
        const out = [];
        for (let k = 0; k < len; k++)
          out.push(this.item(depth + 1));
        return out;
      }
      case 5: {
        if (n > remaining / 2n)
          throw new CborError("truncated");
        const len = Number(n);
        const out = [];
        let prev;
        for (let k = 0; k < len; k++) {
          const start = this.i;
          const key = this.item(depth + 1);
          const kb = this.b.subarray(start, this.i);
          if (prev !== void 0) {
            const c = compareBytes(prev, kb);
            if (c === 0)
              throw new CborError("duplicate-key");
            if (c > 0)
              throw new CborError("key-order");
          }
          prev = kb;
          const val = this.item(depth + 1);
          out.push({ key, val });
        }
        return new CborMap(out);
      }
      default:
        throw new CborError("unsupported", "tag");
    }
  }
};

// node_modules/@lightwebinc/bcommon/dist/derive.js
import { KeyDeriver } from "@bsv/sdk";

// node_modules/@lightwebinc/bcommon/dist/pubkey.js
import { PublicKey } from "@bsv/sdk";
var fieldPrime = 0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2fn;
function toHex(b) {
  let s = "";
  for (let i = 0; i < b.length; i++)
    s += (b[i] & 255).toString(16).padStart(2, "0");
  return s;
}
function strictPublicKey(b) {
  if (b.length !== 33 || b[0] !== 2 && b[0] !== 3)
    return void 0;
  let x = 0n;
  for (let i = 1; i < 33; i++)
    x = x << 8n | BigInt(b[i] & 255);
  if (x >= fieldPrime)
    return void 0;
  const hex = toHex(b);
  let key;
  try {
    key = PublicKey.fromString(hex);
  } catch {
    return void 0;
  }
  if (!key.validate() || key.toString() !== hex)
    return void 0;
  return key;
}
function strictPublicKeyHex(hex) {
  if (!/^[0-9a-fA-F]{66}$/.test(hex))
    return void 0;
  const b = [];
  for (let i = 0; i < 66; i += 2)
    b.push(parseInt(hex.slice(i, i + 2), 16));
  return strictPublicKey(b);
}

// node_modules/@lightwebinc/bcommon/dist/derive.js
var anyone = new KeyDeriver("anyone");
function readerLockingKey(protocol, keyID, identityHex) {
  if (strictPublicKeyHex(identityHex) === void 0)
    throw new Error("identity key is not a canonical compressed public key");
  return anyone.derivePublicKey(protocol, keyID, identityHex, false);
}

// node_modules/@lightwebinc/bcommon/dist/script.js
var OP_0 = 0;
var OP_PUSHDATA1 = 76;
var OP_PUSHDATA2 = 77;
var OP_PUSHDATA4 = 78;
var OP_1NEGATE = 79;
var OP_DROP = 117;
var OP_2DROP = 109;
var OP_CHECKSIG = 172;
function concat(parts) {
  const out = new Uint8Array(parts.reduce((n, p) => n + p.length, 0));
  let at = 0;
  for (const p of parts) {
    out.set(p, at);
    at += p.length;
  }
  return out;
}
function minimalPushBytes(d) {
  const n = d.length;
  if (n === 0 || n === 1 && d[0] === 0)
    return Uint8Array.of(OP_0);
  if (n === 1 && d[0] >= 1 && d[0] <= 16)
    return Uint8Array.of(80 + d[0]);
  if (n === 1 && d[0] === 129)
    return Uint8Array.of(OP_1NEGATE);
  let head2;
  if (n <= 75)
    head2 = [n];
  else if (n <= 255)
    head2 = [OP_PUSHDATA1, n];
  else if (n <= 65535)
    head2 = [OP_PUSHDATA2, n & 255, n >> 8];
  else
    head2 = [OP_PUSHDATA4, n & 255, n >> 8 & 255, n >> 16 & 255, n >>> 24 & 255];
  return concat([Uint8Array.from(head2), d]);
}
function pushDropScript(key, fields, sig) {
  const all = sig === void 0 ? fields : [...fields, sig];
  const parts = [Uint8Array.of(33), key, Uint8Array.of(OP_CHECKSIG), ...all.map(minimalPushBytes)];
  for (let n = all.length; n > 0; n -= 2)
    parts.push(Uint8Array.of(n === 1 ? OP_DROP : OP_2DROP));
  return concat(parts);
}

// node_modules/@lightwebinc/bcommon/dist/record.js
var MaxKeys = 64;
function recordReader(refuse) {
  const decodeMap2 = (b, max) => {
    if (b.length > max)
      throw refuse("too-large");
    let v;
    try {
      v = decodeValue(b);
    } catch (e) {
      if (e instanceof CborError)
        throw refuse("cbor", e.message);
      throw e;
    }
    if (!(v instanceof CborMap))
      throw refuse("cbor", "not a map");
    if (v.entries.length > MaxKeys)
      throw refuse("too-large", `more than ${MaxKeys} keys`);
    return v;
  };
  const split2 = (m, last) => {
    const top = BigInt(last);
    const known = /* @__PURE__ */ new Map();
    const extra = [];
    for (const p of m.entries) {
      if (typeof p.key !== "bigint" || p.key < 0n)
        throw refuse("key-type");
      if (p.key > top) {
        extra.push(p);
        continue;
      }
      known.set(Number(p.key), p.val);
    }
    return { known, extra };
  };
  const checkMagic2 = (f, magic) => {
    const b = f.known.get(0);
    if (!(b instanceof Uint8Array) || !bytesEqual(b, magic))
      throw refuse("magic");
  };
  const need = (f, k) => {
    const v = f.known.get(k);
    if (v === void 0)
      throw refuse("missing", `key ${k}`);
    return v;
  };
  const bytes = (f, k) => {
    const v = need(f, k);
    if (!(v instanceof Uint8Array))
      throw refuse("type", `key ${k}`);
    return v;
  };
  const array = (f, k, max) => {
    const v = need(f, k);
    if (!Array.isArray(v))
      throw refuse("type", `key ${k}`);
    if (v.length > max)
      throw refuse("list", `key ${k}`);
    return v;
  };
  const checkExtraKeys = (extra, last) => {
    const top = BigInt(last);
    for (const p of extra) {
      if (typeof p.key !== "bigint" || p.key <= top)
        throw refuse("key-type");
    }
  };
  return {
    decodeMap: decodeMap2,
    split: split2,
    checkMagic: checkMagic2,
    decode(b, max, last, magic) {
      const f = split2(decodeMap2(b, max), last);
      checkMagic2(f, magic);
      return f;
    },
    need,
    bytes,
    bytesN(f, k, n) {
      const v = bytes(f, k);
      if (n >= 0 && v.length !== n)
        throw refuse("range", `key ${k}`);
      return v;
    },
    bytesRange(f, k, lo, hi) {
      const v = bytes(f, k);
      if (v.length < lo || v.length > hi)
        throw refuse("range", `key ${k}`);
      return v;
    },
    text(f, k) {
      const v = need(f, k);
      if (typeof v !== "string")
        throw refuse("type", `key ${k}`);
      return v;
    },
    uint(f, k, lo, hi) {
      const v = need(f, k);
      if (typeof v !== "bigint" || v < 0n)
        throw refuse("type", `key ${k}`);
      if (v < BigInt(lo) || v > BigInt(hi))
        throw refuse("range", `key ${k}`);
      return Number(v);
    },
    bool(f, k) {
      const v = need(f, k);
      if (typeof v !== "boolean")
        throw refuse("type", `key ${k}`);
      return v;
    },
    array,
    list32(f, k, max) {
      const out = array(f, k, max).map((e) => {
        if (!(e instanceof Uint8Array) || e.length !== 32)
          throw refuse("list", `key ${k}`);
        return e;
      });
      if (!ascending(out))
        throw refuse("list", `key ${k}`);
      return out;
    },
    inRange(n, lo, hi, what) {
      if (!Number.isSafeInteger(n) || n < lo || n > hi)
        throw refuse("range", what);
    },
    checkExtraKeys,
    checkExtra(extra, last, defined) {
      checkExtraKeys(extra, last);
      if (extra.length + defined > MaxKeys)
        throw refuse("too-large");
    },
    encode(m, extra, max) {
      const out = encode(new CborMap([...m, ...extra]));
      if (out.length > max)
        throw refuse("too-large");
      return out;
    }
  };
}
function ascending(a) {
  for (let i = 1; i < a.length; i++)
    if (compareBytes(a[i - 1], a[i]) >= 0)
      return false;
  return true;
}
function claimsRecord(payload, magic) {
  if (payload.length < 1 || payload[0] >> 5 !== 5)
    return false;
  const ai = payload[0] & 31;
  let i;
  if (ai < 24)
    i = 1;
  else if (ai <= 27)
    i = 1 + 2 ** (ai - 24);
  else
    return false;
  if (payload.length < i + 2 + magic.length || payload[i] !== 0 || payload[i + 1] !== 68)
    return false;
  return bytesEqual(payload.subarray(i + 2, i + 2 + magic.length), magic);
}

// node_modules/@lightwebinc/bcommon/dist/carrier.js
import { PushDrop } from "@bsv/sdk";

// node_modules/@lightwebinc/bcommon/dist/fieldsig.js
import { Signature } from "@bsv/sdk";
function verifyFieldSignature(key, signed, der) {
  try {
    return key.verify(signed, Signature.fromDER(der));
  } catch {
    return false;
  }
}

// node_modules/@lightwebinc/bcommon/dist/carrier.js
var LockTime = 4102444800;
var MaxSequence = 4294967295;
var SigHashType = 65;
var order = 0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141n;
function mineableRefusal(tx) {
  if (tx.lockTime < LockTime)
    return "mineable";
  if (tx.inputs.length === 0)
    return "other";
  for (const input of tx.inputs) {
    if ((input.sequence ?? MaxSequence) === MaxSequence)
      return "mineable";
  }
  return void 0;
}
function strictInt(b) {
  if (b.length === 0 || (b[0] & 128) !== 0)
    return false;
  return !(b.length > 1 && b[0] === 0 && (b[1] & 128) === 0);
}
function toBigInt(b) {
  let x = 0n;
  for (const byte of b)
    x = x << 8n | BigInt(byte);
  return x;
}
function strictSignature(der) {
  if (der.length < 8 || der.length > 72)
    return false;
  if (der[0] !== 48 || der[1] !== der.length - 2)
    return false;
  const lenR = der[3];
  if (der[2] !== 2 || lenR === 0 || 6 + lenR > der.length)
    return false;
  const lenS = der[5 + lenR];
  if (der[4 + lenR] !== 2 || lenS === 0 || 6 + lenR + lenS !== der.length)
    return false;
  const r = der.subarray(4, 4 + lenR);
  const s = der.subarray(6 + lenR);
  if (!strictInt(r) || !strictInt(s))
    return false;
  const R = toBigInt(r);
  const S = toBigInt(s);
  return R > 0n && R < order && S > 0n && S <= order >> 1n;
}
function unlockingRefusal(tx) {
  if (tx.inputs.length !== 1)
    return "non-canonical-unlocking";
  const u = tx.inputs[0]?.unlockingScript;
  if (u === void 0)
    return "non-canonical-unlocking";
  const s = Uint8Array.from(u.toBinary());
  const op = s[0];
  if (op === void 0 || op < 1 || op > 75 || s.length !== 1 + op)
    return "non-canonical-unlocking";
  const sig = s.subarray(1);
  if (sig[sig.length - 1] !== SigHashType)
    return "non-canonical-unlocking";
  return strictSignature(sig.subarray(0, sig.length - 1)) ? void 0 : "non-canonical-unlocking";
}

// src/logrec.ts
var ProtocolName = "blogs";
var Protocol = [1, ProtocolName];
var KeyBatch = "batch";
var KeyAnchor = "anchor";
var TopicPrefix = "tm_log_";
var LookupService = "ls_log";
var TagAnchor = Uint8Array.of(98, 108, 1);
var TagFunding = Uint8Array.of(98, 108, 2);
var MagicBatch = Uint8Array.of(98, 108, 98, 1);
var MagicAnchor = Uint8Array.of(98, 108, 97, 1);
var MaxSafe = Number.MAX_SAFE_INTEGER;
var SuffixLen = 10;
var MaxStream = 50 - TopicPrefix.length;
var MaxStreamName = MaxStream - 1 - SuffixLen;
var MinStream = 1 + 1 + SuffixLen;
var MaxContent = 256 << 10;
var MaxExpanded = 16 << 20;
var MaxLocated = MaxExpanded;
var MaxBatchRecord = MaxContent + 4096;
var MaxAnchorRecord = 1024;
var MaxIntervalBatches = 1 << 16;
var MaxURLs = 4;
var MaxURL = 256;
var BK = { magic: 0, producer: 1, stream: 2, seq: 3, prev: 4, timestamp: 5, count: 6, encoding: 7, profile: 8, content: 9, locator: 10, size: 11 };
var bkLast = 11n;
var AK = { magic: 0, producer: 1, stream: 2, interval: 3, prev: 4, first: 5, count: 6, root: 7, timestamp: 8 };
var akLast = 8n;
var EncodingLines = 1;
var ProfileFull = 1;
var ProfileDigest = 2;
var Refusal = class extends Error {
  constructor(reason, detail) {
    super(detail === void 0 ? `logrec: ${reason}` : `logrec: ${reason}: ${detail}`);
    this.reason = reason;
    this.name = "Refusal";
  }
  reason;
};
var fieldP = 0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2fn;
function modPow(b, e, m) {
  let r = 1n;
  b %= m;
  while (e > 0n) {
    if (e & 1n) r = r * b % m;
    b = b * b % m;
    e >>= 1n;
  }
  return r;
}
function toBigInt2(b) {
  let x = 0n;
  for (const byte of b) x = x << 8n | BigInt(byte);
  return x;
}
function checkProducer(k) {
  if (k.length !== 33 || k[0] !== 2 && k[0] !== 3) throw new Refusal("producer");
  const x = toBigInt2(k.subarray(1));
  if (x >= fieldP) throw new Refusal("producer");
  const rhs = (modPow(x, 3n, fieldP) + 7n) % fieldP;
  if (rhs !== 0n && modPow(rhs, (fieldP - 1n) / 2n, fieldP) !== 1n) throw new Refusal("producer");
}
function checkName(s, max) {
  if (s.length === 0 || s.length > max) throw new Refusal("stream");
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 97 && c <= 122) continue;
    if (c === 95 && i !== 0 && i !== s.length - 1 && s.charCodeAt(i - 1) !== 95) continue;
    throw new Refusal("stream");
  }
}
function checkStreamName(name) {
  checkName(name, MaxStreamName);
}
function checkStream(s) {
  if (s.length < MinStream || s.length > MaxStream) throw new Refusal("stream");
  const cut = s.length - SuffixLen - 1;
  if (s.charCodeAt(cut) !== 95) throw new Refusal("stream");
  for (let i = cut + 1; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 97 || c > 122) throw new Refusal("stream");
  }
  checkStreamName(s.slice(0, cut));
}
function topic(stream) {
  checkStream(stream);
  return TopicPrefix + stream;
}
var rec = recordReader((reason, detail) => new Refusal(reason, detail));
var decodeMap = rec.decodeMap;
var intKeys = rec.split;
var checkMagic = rec.checkMagic;
var getBytes = rec.bytes;
var needBytes = rec.bytesRange;
var needUint = rec.uint;
var needText = rec.text;
var inRange = rec.inRange;
var checkExtra = rec.checkExtraKeys;
function isZero(b) {
  return b.every((x) => x === 0);
}
function validURL(u) {
  if (u.length < "https://x".length || u.length > MaxURL || !u.startsWith("https://")) return false;
  for (let i = 0; i < u.length; i++) {
    const c = u.charCodeAt(i);
    if (c <= 32 || c >= 127) return false;
  }
  return true;
}
var locatorNames = /* @__PURE__ */ new Set(["digest", "length", "urls"]);
function validateLocator(l) {
  if (!Number.isSafeInteger(l.length) || l.length < 1 || l.length > MaxLocated) throw new Refusal("locator", "length");
  if (l.digest.length !== 32) throw new Refusal("locator", "digest");
  if (l.urls.length > MaxURLs) throw new Refusal("locator", "urls");
  for (const u of l.urls) if (!validURL(u)) throw new Refusal("locator", "url");
  for (const p of l.extra) {
    if (typeof p.key === "string" && locatorNames.has(p.key)) throw new Refusal("locator", "member name");
  }
}
function decodeLocatorValue(v) {
  if (!(v instanceof CborMap)) throw new Refusal("locator", "not a map");
  let digest;
  let length;
  let urls;
  let seen = 0;
  const extra = [];
  for (const p of v.entries) {
    const k = typeof p.key === "string" ? p.key : "";
    switch (k) {
      case "digest":
        if (!(p.val instanceof Uint8Array) || p.val.length !== 32) throw new Refusal("locator", "digest");
        digest = p.val;
        seen++;
        break;
      case "length":
        if (typeof p.val !== "bigint" || p.val < 0n) throw new Refusal("locator", "length");
        if (p.val < 1n || p.val > BigInt(MaxLocated)) throw new Refusal("locator", "length");
        length = Number(p.val);
        seen++;
        break;
      case "urls": {
        if (!Array.isArray(p.val) || p.val.length > MaxURLs) throw new Refusal("locator", "urls");
        const us = [];
        for (const e of p.val) {
          if (typeof e !== "string") throw new Refusal("locator", "url");
          us.push(e);
        }
        urls = us;
        seen++;
        break;
      }
      default:
        extra.push(p);
    }
  }
  if (seen !== 3 || digest === void 0 || length === void 0 || urls === void 0) throw new Refusal("locator", "a member is missing");
  const l = { digest, length, urls, extra };
  validateLocator(l);
  return l;
}
function batchCrossRules(b) {
  if (b.seq === 1 !== isZero(b.prev)) throw new Refusal("prev");
  if (b.count > b.size) throw new Refusal("range", "count above size");
  if (b.profile === ProfileFull) {
    if (b.content === void 0 || b.locator !== void 0) throw new Refusal("profile-shape");
    if (b.encoding === EncodingLines && b.size !== b.content.length) throw new Refusal("content");
  } else if (b.profile === ProfileDigest) {
    if (b.content !== void 0 || b.locator === void 0) throw new Refusal("profile-shape");
    if (b.encoding === EncodingLines && b.size !== b.locator.length) throw new Refusal("content");
  }
}
var claims = claimsRecord;
function isBatch(payload) {
  return claims(payload, MagicBatch);
}
function decodeBatch(p) {
  const f = intKeys(decodeMap(p, MaxBatchRecord), bkLast);
  checkMagic(f, MagicBatch);
  const producer = getBytes(f, BK.producer);
  checkProducer(producer);
  const stream = needText(f, BK.stream);
  checkStream(stream);
  const seq = needUint(f, BK.seq, 1, MaxSafe);
  const prev = needBytes(f, BK.prev, 32, 32);
  const timestamp = needUint(f, BK.timestamp, 1, MaxSafe);
  const count = needUint(f, BK.count, 1, MaxSafe);
  const encoding = needUint(f, BK.encoding, 1, MaxSafe);
  const profile = needUint(f, BK.profile, 1, MaxSafe);
  let content;
  if (f.known.has(BK.content)) content = needBytes(f, BK.content, 1, MaxContent);
  let locator;
  if (f.known.has(BK.locator)) {
    const v = f.known.get(BK.locator);
    if (!(v instanceof CborMap)) throw new Refusal("type", `key ${BK.locator}`);
    locator = decodeLocatorValue(v);
  }
  const size = needUint(f, BK.size, 1, MaxExpanded);
  const b = { producer, stream, seq, prev, timestamp, count, encoding, profile, size, extra: f.extra };
  if (content !== void 0) b.content = content;
  if (locator !== void 0) b.locator = locator;
  batchCrossRules(b);
  return b;
}
function anchorCrossRules(a) {
  if (a.interval === 1 !== isZero(a.prev)) throw new Refusal("prev");
  if (a.interval === 1 && a.first !== 1) throw new Refusal("range", "first on interval 1");
  if (a.first + a.count - 1 > MaxSafe) throw new Refusal("range", "last seq");
}
function decodeAnchor(p) {
  const f = intKeys(decodeMap(p, MaxAnchorRecord), akLast);
  checkMagic(f, MagicAnchor);
  const producer = getBytes(f, AK.producer);
  checkProducer(producer);
  const stream = needText(f, AK.stream);
  checkStream(stream);
  const interval = needUint(f, AK.interval, 1, MaxSafe);
  const prev = needBytes(f, AK.prev, 32, 32);
  const first = needUint(f, AK.first, 1, MaxSafe);
  const count = needUint(f, AK.count, 1, MaxIntervalBatches);
  const root = needBytes(f, AK.root, 32, 32);
  const timestamp = needUint(f, AK.timestamp, 1, MaxSafe);
  const a = { producer, stream, interval, prev, first, count, root, timestamp, extra: f.extra };
  anchorCrossRules(a);
  return a;
}

// src/script.ts
var OP_02 = 0;
var OP_PUSHDATA12 = 76;
var OP_PUSHDATA22 = 77;
var OP_PUSHDATA42 = 78;
var OP_CHECKSIG2 = 172;
function parseScript(s) {
  const out = [];
  let i = 0;
  while (i < s.length) {
    const c = s[i++];
    let n;
    if (c === OP_02) {
      out.push({ code: c, data: new Uint8Array(0) });
      continue;
    } else if (c >= 1 && c <= 75) {
      n = c;
    } else if (c === OP_PUSHDATA12) {
      if (i + 1 > s.length) return void 0;
      n = s[i];
      i += 1;
    } else if (c === OP_PUSHDATA22) {
      if (i + 2 > s.length) return void 0;
      n = s[i] | s[i + 1] << 8;
      i += 2;
    } else if (c === OP_PUSHDATA42) {
      if (i + 4 > s.length) return void 0;
      n = (s[i] | s[i + 1] << 8 | s[i + 2] << 16) + s[i + 3] * 16777216;
      i += 4;
    } else {
      out.push({ code: c });
      continue;
    }
    if (n > s.length - i) return void 0;
    out.push({ code: c, data: s.subarray(i, i + n) });
    i += n;
  }
  return out;
}
function readPushDrop(s) {
  const ops = parseScript(s);
  if (ops === void 0 || ops.length < 3) return void 0;
  const k = ops[0].data;
  if (k === void 0 || k.length !== 33 || k[0] !== 2 && k[0] !== 3) return void 0;
  if (ops[1].code !== OP_CHECKSIG2) return void 0;
  const fields = [];
  for (const o of ops.slice(2)) {
    if (o.data === void 0) break;
    fields.push(o.data);
  }
  if (fields.length === 0) return void 0;
  return { key: k, fields };
}
function fundingScript(key) {
  return pushDropScript(key, [TagFunding]);
}
function isFundingShape(s) {
  if (s.length !== 1 + 33 + 1 + 1 + 3 + 1 || s[0] !== 33) return false;
  const key = s.subarray(1, 34);
  try {
    checkProducer(key);
  } catch {
    return false;
  }
  return bytesEq(s, fundingScript(key));
}
function bytesEq(a, b) {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}

// src/admit.ts
function validatePolicy(p) {
  if ("open" in p ? p.open !== true || "producers" in p : p.producers.length === 0) {
    throw new Error("logrec: a policy is open or names producers, exactly one");
  }
}
function admits(p, producer) {
  return "open" in p || p.producers.some((k) => bytesEq(k, producer));
}
var outpointKey = (txid, vout) => `${txid}.${vout}`;
function spentCoin(tx, i, coins) {
  const input = tx.inputs[i];
  const txid = input.sourceTXID ?? input.sourceTransaction?.id("hex");
  if (txid === void 0) return void 0;
  const coin = coins.get(outpointKey(txid, input.sourceOutputIndex));
  return coin === void 0 ? void 0 : { coin, txid };
}
var fromHex = (s) => Uint8Array.from(s.match(/../g) ?? [], (b) => parseInt(b, 16));
var toHex2 = (b) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
function script(s) {
  return Uint8Array.from(s.toBinary());
}
function spent(tx, i) {
  const input = tx.inputs[i];
  const src = input?.sourceTransaction;
  const out = src?.outputs[input.sourceOutputIndex];
  if (src === void 0 || out === void 0) return void 0;
  return { satoshis: out.satoshis ?? 0, script: script(out.lockingScript), txid: Uint8Array.from(src.hash()) };
}
function classify(tx) {
  let kind = "none";
  let at = -1;
  let batches = 0;
  let anchors = 0;
  tx.outputs.forEach((o, i) => {
    const p = readPushDrop(script(o.lockingScript));
    if (p === void 0) return;
    if (isBatch(p.fields[0])) {
      batches++;
      kind = "batch";
      at = i;
    } else if (bytesEq(p.fields[0], TagAnchor)) {
      anchors++;
      if (kind !== "batch") {
        kind = "anchor";
        at = i;
      }
    }
  });
  if (batches > 0 && batches + anchors > 1) throw new Refusal("carrier-shape", `${batches + anchors} claiming outputs`);
  if (anchors > 1) throw new Refusal("anchor-shape", `${anchors} claiming outputs`);
  return { kind, at };
}
function derived(keyID, producer) {
  return Uint8Array.from(readerLockingKey(Protocol, keyID, toHex2(producer)).encode(true));
}
function verifyField(key, signed, der) {
  if (!strictSignature(der)) return false;
  return verifyFieldSignature(readerKey(key), Array.from(signed), Array.from(der));
}
function readerKey(k) {
  return PublicKey2.fromString(toHex2(k));
}
function checkCarrier(tx, h) {
  if (tx.inputs.length !== 1 || tx.outputs.length !== 1) throw new Refusal("carrier-shape", `${tx.inputs.length} inputs, ${tx.outputs.length} outputs`);
  const src = spent(tx, 0);
  const out = tx.outputs[0];
  if (src === void 0) throw new Refusal("carrier-shape", "the spent output is not in the BEEF");
  if (src.satoshis !== (out.satoshis ?? 0)) throw new Refusal("carrier-shape", "value");
  const lock = script(out.lockingScript);
  const pd = readPushDrop(lock);
  const record = pd.fields[0];
  const b = decodeBatch(record);
  if (b.stream !== h.stream) throw new Refusal("stream", "not this topic's stream");
  if (!admits(h.policy, b.producer)) throw new Refusal("producer-policy");
  if (mineableRefusal(tx) !== void 0) throw new Refusal("mineable");
  if (unlockingRefusal(tx) !== void 0) throw new Refusal("unlock");
  const key = derived(KeyBatch, b.producer);
  if (pd.fields.length !== 2) throw new Refusal("lock", "want the record and a signature");
  const sig = pd.fields[1];
  if (!bytesEq(lock, pushDropScript(key, [record], sig))) throw new Refusal("lock");
  if (!verifyField(key, record, sig)) throw new Refusal("signature");
  if (!bytesEq(src.script, fundingScript(key))) throw new Refusal("funding");
  return {
    batch: b,
    record,
    commitment: Uint8Array.from(tx.hash()),
    funding: { txid: src.txid, vout: tx.inputs[0].sourceOutputIndex }
  };
}
function readToken(s) {
  const pd = readPushDrop(s);
  if (pd === void 0 || !bytesEq(pd.fields[0], TagAnchor) || pd.fields.length !== 3) return void 0;
  let a;
  try {
    a = decodeAnchor(pd.fields[1]);
  } catch {
    return void 0;
  }
  const key = derived(KeyAnchor, a.producer);
  return bytesEq(s, pushDropScript(key, [TagAnchor, pd.fields[1]], pd.fields[2])) ? a : void 0;
}
function anchorInput(tx, a, coins) {
  const funding = fundingScript(derived(KeyBatch, a.producer));
  const prev = toHex2(Uint8Array.from(a.prev).reverse());
  let found = false;
  for (let i = 0; i < tx.inputs.length; i++) {
    const spent2 = spentCoin(tx, i, coins);
    if (spent2 === void 0) return false;
    if (bytesEq(spent2.coin.lockingScript, funding)) {
      found ||= a.interval === 1;
      continue;
    }
    if (a.interval === 1 || spent2.txid !== prev) return false;
    const p = readToken(spent2.coin.lockingScript);
    if (p === void 0 || !bytesEq(p.producer, a.producer) || p.stream !== a.stream || p.interval !== a.interval - 1) return false;
    found = true;
  }
  return found;
}
async function mined(tx, headers) {
  if (tx.merklePath === void 0 || headers === void 0) return false;
  try {
    return await tx.merklePath.verify(tx.id("hex"), headers);
  } catch {
    return false;
  }
}
async function checkToken(tx, at, coins, h) {
  const out = tx.outputs[at];
  const lock = script(out.lockingScript);
  const pd = readPushDrop(lock);
  if (pd.fields.length !== 3) throw new Refusal("anchor-shape", "want the tag, the record and a signature");
  if ((out.satoshis ?? 0) !== 1) throw new Refusal("anchor-shape", "satoshis");
  const record = pd.fields[1];
  const sig = pd.fields[2];
  const a = decodeAnchor(record);
  if (a.stream !== h.stream) throw new Refusal("stream", "not this topic's stream");
  if (!admits(h.policy, a.producer)) throw new Refusal("producer-policy");
  const key = derived(KeyAnchor, a.producer);
  if (!bytesEq(lock, pushDropScript(key, [TagAnchor, record], sig))) throw new Refusal("lock");
  const signed = new Uint8Array(TagAnchor.length + record.length);
  signed.set(TagAnchor);
  signed.set(record, TagAnchor.length);
  if (!verifyField(key, signed, sig)) throw new Refusal("signature");
  if (!anchorInput(tx, a, coins)) throw new Refusal("anchor-input");
  if (!await mined(tx, h.headers)) throw new Refusal("unmined");
  tx.inputs.forEach((input, i) => {
    if (unlockingRefusal(new Transaction(tx.version, [input], [], 0)) !== void 0) throw new Refusal("unlock", `input ${i}`);
  });
  return { anchor: a, record, outputIndex: at, txid: Uint8Array.from(tx.hash()) };
}
async function fundingOutputs(tx, headers) {
  const outs = [];
  tx.outputs.forEach((o, i) => {
    if (isFundingShape(script(o.lockingScript))) outs.push(i);
  });
  if (outs.length === 0 || !await mined(tx, headers)) return [];
  return outs;
}
async function admit(tx, coins, h) {
  validatePolicy(h.policy);
  try {
    checkStream(h.stream);
  } catch {
    throw new Error(`logrec: host stream ${JSON.stringify(h.stream)} breaks the grammar`);
  }
  const { kind, at } = classify(tx);
  if (kind === "batch") return { kind, carrier: checkCarrier(tx, h), funding: [] };
  if (kind === "anchor") return { kind, token: await checkToken(tx, at, coins, h), funding: [] };
  return { kind, funding: await fundingOutputs(tx, h.headers) };
}

// src/ls_log.ts
import { Transaction as Transaction2 } from "@bsv/sdk";

// src/merkle.ts
import { Hash } from "@bsv/sdk";
function sha256(parts) {
  const all = [];
  for (const p of parts) all.push(...p);
  return Uint8Array.from(Hash.sha256(all));
}
function leafHash(c) {
  return sha256([Uint8Array.of(0), c]);
}
function nodeHash(l, r) {
  return sha256([Uint8Array.of(1), l, r]);
}
function split(n) {
  let k = 1;
  while (k * 2 < n) k *= 2;
  return k;
}
function subtree(leaves) {
  if (leaves.length === 1) return leafHash(leaves[0]);
  const k = split(leaves.length);
  return nodeHash(subtree(leaves.slice(0, k)), subtree(leaves.slice(k)));
}
function intervalRoot(commitments) {
  return commitments.length === 0 ? sha256([]) : subtree(commitments);
}

// src/ls_log.ts
var MaxCandidates = 8;
var Stream = class {
  constructor(producer, stream) {
    this.producer = producer;
    this.stream = stream;
  }
  producer;
  stream;
  batches = /* @__PURE__ */ new Map();
  tokens = /* @__PURE__ */ new Map();
  highest = 0;
  /** Dropped by retention: intervals 1 to retiredInterval, seqs 1 to retiredSeq; retiredC is C at retiredSeq (hex). */
  retiredInterval = 0;
  retiredSeq = 0;
  retiredC = "";
  /** Cached canonical chain, index k - 1 for interval k; cleared on any token change. */
  chain;
};
var compareKey = (a, b) => a < b ? -1 : a > b ? 1 : 0;
var hashKey = (display) => toHex2(fromHex(display).reverse());
function position(tx, txid) {
  const p = tx.merklePath;
  const leaf = p?.path[0]?.find((l) => l.hash === txid);
  if (p === void 0 || leaf === void 0) return { height: Infinity, index: Infinity };
  return { height: p.blockHeight, index: leaf.offset };
}
function inputsOf(tx) {
  const out = [];
  for (const input of tx.inputs) {
    const src = input.sourceTXID ?? input.sourceTransaction?.id("hex");
    if (src !== void 0) out.push(outpointKey(src, input.sourceOutputIndex));
  }
  return out;
}
var LogLookupService = class {
  constructor(topics, host, retention) {
    this.topics = topics;
    this.host = host;
    this.retention = retention;
    if (retention !== void 0) {
      const st = retention.store.load();
      for (const m of st.marks) this.marks.set(`${m.producer}|${m.stream}`, m);
      for (const [k, v] of st.trees) this.treeLow.set(k, v);
    }
  }
  topics;
  host;
  retention;
  /** The whole transaction: a carrier's funding outpoint and every spend are its inputs. */
  admissionMode = "whole-tx";
  /** Spends are read from admitted transactions, never from the engine's notices. */
  spendNotificationMode = "none";
  /** producer hex | stream -> index. */
  streams = /* @__PURE__ */ new Map();
  /** Outpoint -> what was indexed there, for eviction; an outpoint may be held on several topics. */
  byOutpoint = /* @__PURE__ */ new Map();
  /** topic | outpoint -> the admitted output, for the topic manager's previous coins. */
  held = /* @__PURE__ */ new Map();
  /** topic | txid -> the funding outputs the topic holds of that transaction, for the tree class. */
  trees = /* @__PURE__ */ new Map();
  /** Funding outpoint -> the carriers (display txid) that spend it. */
  carriersByFunding = /* @__PURE__ */ new Map();
  /** Outpoint -> txid -> whether that spender is mined. */
  spenders = /* @__PURE__ */ new Map();
  /** Carrier txids retracted by a mined spend of their funding outpoint. */
  retracted = /* @__PURE__ */ new Set();
  noted = /* @__PURE__ */ new Set();
  forkSeqs = 0;
  storage;
  /** producer | stream -> the retained prefix's mark, from the state file. */
  marks = /* @__PURE__ */ new Map();
  /** topic | tree txid -> the lowest funding output whose carrier retention dropped. */
  treeLow = /* @__PURE__ */ new Map();
  /** Carrier outputs to delete from storage. */
  drops = [];
  restoring = false;
  /** The marks or treeLow changed since the state file was last written. */
  stateDirty = false;
  warned = /* @__PURE__ */ new Set();
  // ---- the topic manager's previous coins
  /**
   * The output the topic holds at an outpoint: from the engine's storage
   * once restore has handed it over (the host calls restore before it
   * accepts a submission), otherwise from what this service indexed.
   */
  coin = async (topic2, txid, vout) => {
    if (this.storage !== void 0) {
      const o = await this.storage.findOutput(txid, vout, topic2);
      return o === null ? void 0 : { lockingScript: Uint8Array.from(o.outputScript), satoshis: o.satoshis };
    }
    return this.held.get(`${topic2}|${outpointKey(txid, vout)}`);
  };
  // ---- gauges
  get streamCount() {
    return this.streams.size;
  }
  get batchCount() {
    let n = 0;
    for (const s of this.streams.values()) for (const l of s.batches.values()) n += l.length;
    return n;
  }
  get tokenCount() {
    let n = 0;
    for (const s of this.streams.values()) for (const l of s.tokens.values()) n += l.length;
    return n;
  }
  get retractedCount() {
    return this.retracted.size;
  }
  /** Seqs below each stream's highest that nothing holds and retention did not drop. */
  get missingCount() {
    let n = 0;
    for (const s of this.streams.values()) n += Math.max(0, s.highest - s.retiredSeq - s.batches.size);
    return n;
  }
  /** Seqs whose carriers retention dropped, over every stream. */
  get retiredCount() {
    let n = 0;
    for (const m of this.marks.values()) n += m.seq;
    return n;
  }
  /** Seqs holding more than one candidate. */
  get forkSeqCount() {
    return this.forkSeqs;
  }
  // ---- admission
  outputAdmittedByTopic(payload) {
    if (payload.mode !== "whole-tx" || !this.topics.has(payload.topic)) return;
    let tx;
    try {
      tx = Transaction2.fromAtomicBEEF(payload.atomicBEEF);
    } catch {
      this.host.log("ls_log ignored an admission whose BEEF did not parse", { topic: payload.topic });
      return;
    }
    if (this.index(payload.topic, tx, payload.outputIndex)) this.noteSpends(tx);
  }
  outputEvicted(txid, outputIndex) {
    this.drop(outpointKey(txid, outputIndex));
  }
  outputNoLongerRetainedInHistory(txid, outputIndex, topic2) {
    this.drop(outpointKey(txid, outputIndex), topic2);
  }
  /**
   * Index one admitted output of tx on topic. False for a carrier at a
   * position retention has dropped: it is not indexed, and its output is
   * deleted from storage at the next pass.
   */
  index(topic2, tx, outputIndex) {
    const out = tx.outputs[outputIndex];
    if (out === void 0) return true;
    const txid = tx.id("hex");
    const script2 = Uint8Array.from(out.lockingScript.toBinary());
    const op = outpointKey(txid, outputIndex);
    const stream = topic2.slice(TopicPrefix.length);
    const pd = readPushDrop(script2);
    if (pd !== void 0 && isBatch(pd.fields[0])) {
      const b = decodeBatch(pd.fields[0]);
      if (b.stream !== stream) return true;
      const funding = inputsOf(tx)[0];
      if (funding === void 0) return true;
      const s = this.streamOf(toHex2(b.producer), b.stream);
      if (b.seq <= s.retiredSeq) {
        this.drops.push({ txid, vout: outputIndex, topic: topic2 });
        return false;
      }
      const c = Uint8Array.from(tx.hash());
      this.hold(topic2, op, { lockingScript: new Uint8Array(0), satoshis: out.satoshis ?? 0 });
      this.addBatch(topic2, s, { seq: b.seq, prev: toHex2(b.prev), c, cKey: toHex2(c), txid, outputIndex, funding });
      return true;
    }
    this.hold(topic2, op, { lockingScript: script2, satoshis: out.satoshis ?? 0 });
    if (pd !== void 0 && pd.fields.length === 3 && bytesEq(pd.fields[0], TagAnchor)) {
      const a = decodeAnchor(pd.fields[1]);
      if (a.stream !== stream) return true;
      const s = this.streamOf(toHex2(a.producer), a.stream);
      const at = position(tx, txid);
      this.addToken(topic2, s, { anchor: a, txid, txidKey: hashKey(txid), outputIndex, outpoint: op, ...at, inputs: inputsOf(tx) });
      return true;
    }
    if (!isFundingShape(script2)) {
      this.host.log("ls_log indexed an output of no known kind", { topic: topic2, outpoint: op });
      return true;
    }
    const k = `${topic2}|${txid}`;
    let vouts = this.trees.get(k);
    if (vouts === void 0) this.trees.set(k, vouts = /* @__PURE__ */ new Set());
    vouts.add(outputIndex);
    return true;
  }
  hold(topic2, op, coin) {
    this.held.set(`${topic2}|${op}`, coin);
    this.ref(op, { topic: topic2, kind: "coin" });
  }
  ref(op, r) {
    const l = this.byOutpoint.get(op);
    if (l === void 0) this.byOutpoint.set(op, [r]);
    else if (!l.some((x) => x.topic === r.topic && x.kind === r.kind)) l.push(r);
  }
  streamOf(producer, stream) {
    const k = `${producer}|${stream}`;
    let s = this.streams.get(k);
    if (s === void 0) {
      this.streams.set(k, s = new Stream(producer, stream));
      const m = this.marks.get(k);
      if (m !== void 0) {
        s.retiredInterval = m.interval;
        s.retiredSeq = m.seq;
        s.retiredC = m.c;
        s.highest = m.seq;
      }
    }
    return s;
  }
  addBatch(topic2, s, e) {
    const list = s.batches.get(e.seq) ?? [];
    if (list.some((x) => x.cKey === e.cKey)) return;
    list.push(e);
    list.sort((a, b) => compareKey(a.cKey, b.cKey));
    if (list.length > MaxCandidates) {
      const cut = list.pop();
      this.host.metrics.inc("blogs_candidates_dropped_total");
      this.host.log("ls_log dropped a fork candidate over the cap", { stream: s.stream, seq: e.seq, txid: cut.txid });
      if (cut === e) return;
      this.forget(cut);
    }
    if (list.length === 2) this.forkSeqs++;
    s.batches.set(e.seq, list);
    if (e.seq > s.highest) s.highest = e.seq;
    this.ref(outpointKey(e.txid, e.outputIndex), { topic: topic2, kind: "batch", stream: s });
    let set = this.carriersByFunding.get(e.funding);
    if (set === void 0) this.carriersByFunding.set(e.funding, set = /* @__PURE__ */ new Set());
    set.add(e.txid);
    this.checkRetracted(e.funding);
  }
  forget(e) {
    const set = this.carriersByFunding.get(e.funding);
    set?.delete(e.txid);
    if (set?.size === 0) this.carriersByFunding.delete(e.funding);
    this.retracted.delete(e.txid);
  }
  addToken(topic2, s, t) {
    const k = t.anchor.interval;
    const list = s.tokens.get(k) ?? [];
    if (list.some((x) => x.txid === t.txid)) return;
    list.push(t);
    s.tokens.set(k, list);
    s.chain = void 0;
    this.ref(t.outpoint, { topic: topic2, kind: "token", stream: s });
    if (list.length > MaxCandidates) {
      const canonical = this.chainOf(s).tokens[k - 1];
      list.sort((a, b) => a === canonical ? -1 : b === canonical ? 1 : compareKey(a.txidKey, b.txidKey));
      const cut = list.pop();
      s.chain = void 0;
      this.host.metrics.inc("blogs_tokens_dropped_total");
      this.host.log("ls_log dropped an anchor token over the cap", { stream: s.stream, interval: k, txid: cut.txid });
    }
  }
  /**
   * Record every input of an admitted transaction as a spend. Only a mined
   * spend retracts: a carrier never mines, and an unmined spend of a funding
   * output may never mine either (spec section 8.4).
   */
  noteSpends(tx) {
    const txid = tx.id("hex");
    if (this.noted.has(txid)) return;
    this.noted.add(txid);
    const mined2 = tx.merklePath !== void 0;
    for (const op of inputsOf(tx)) {
      let m = this.spenders.get(op);
      if (m === void 0) this.spenders.set(op, m = /* @__PURE__ */ new Map());
      m.set(txid, mined2 || m.get(txid) === true);
      if (mined2) this.checkRetracted(op);
    }
  }
  checkRetracted(funding) {
    const carriers = this.carriersByFunding.get(funding);
    const spent2 = this.spenders.get(funding);
    if (carriers === void 0 || spent2 === void 0) return;
    for (const c of carriers) {
      if (this.retracted.has(c)) continue;
      for (const [by, mined2] of spent2) {
        if (!mined2 || by === c) continue;
        this.retracted.add(c);
        this.host.metrics.inc("blogs_retractions_total");
        this.host.log("ls_log retracted a batch", { carrier: c, funding, by });
        break;
      }
    }
  }
  drop(op, topic2) {
    const refs = this.byOutpoint.get(op);
    if (refs === void 0) return;
    const keep = refs.filter((r) => topic2 !== void 0 && r.topic !== topic2);
    for (const r of refs) {
      if (topic2 !== void 0 && r.topic !== topic2) continue;
      this.held.delete(`${r.topic}|${op}`);
      if (r.kind === "coin") this.dropTree(r.topic, op);
      if (r.kind === "batch" && r.stream !== void 0) this.dropBatch(r.stream, op);
      if (r.kind === "token" && r.stream !== void 0) this.dropToken(r.stream, op);
    }
    if (keep.length === 0) this.byOutpoint.delete(op);
    else this.byOutpoint.set(op, keep);
  }
  dropTree(topic2, op) {
    const dot = op.lastIndexOf(".");
    const k = `${topic2}|${op.slice(0, dot)}`;
    const vouts = this.trees.get(k);
    if (vouts === void 0) return;
    vouts.delete(Number(op.slice(dot + 1)));
    if (vouts.size === 0) this.trees.delete(k);
  }
  /** How many funding trees (and sweeps, by their tombstones) the tree class can answer. */
  get treeCount() {
    return this.trees.size;
  }
  dropBatch(s, op) {
    for (const [seq, list] of s.batches) {
      const i = list.findIndex((e) => outpointKey(e.txid, e.outputIndex) === op);
      if (i < 0) continue;
      this.forget(list[i]);
      list.splice(i, 1);
      if (list.length === 1) this.forkSeqs--;
      if (list.length === 0) {
        s.batches.delete(seq);
        if (seq === s.highest) {
          s.highest = s.retiredSeq;
          for (const k of s.batches.keys()) if (k > s.highest) s.highest = k;
        }
      }
      return;
    }
  }
  dropToken(s, op) {
    for (const [k, list] of s.tokens) {
      const i = list.findIndex((t) => t.outpoint === op);
      if (i < 0) continue;
      list.splice(i, 1);
      if (list.length === 0) s.tokens.delete(k);
      s.chain = void 0;
      return;
    }
  }
  // ---- statuses
  isRetracted(e) {
    return this.retracted.has(e.txid);
  }
  /** A batch's status (spec section 8.2): the first of the table that applies. */
  batchStatus(s, e) {
    if (this.isRetracted(e)) return "retracted";
    if ((s.batches.get(e.seq) ?? []).some((o) => o.cKey !== e.cKey)) return "fork";
    if (e.seq === 1) return "linked";
    if (e.seq - 1 === s.retiredSeq) return e.prev === s.retiredC ? "linked" : "fork";
    const before = s.batches.get(e.seq - 1);
    if (before === void 0 || before.length === 0) return "gap";
    return before.some((p) => p.cKey === e.prev) ? "linked" : "fork";
  }
  /**
   * The canonical chain (spec section 8.3): interval 1's token mined first,
   * then for each k the token that spends the canonical k - 1 token, if its
   * first continues the predecessor's. A token that fails that join is kept
   * and reported, and the chain stops before it.
   */
  chainOf(s) {
    if (s.chain !== void 0) return s.chain;
    const tokens = [];
    const joinFailed = /* @__PURE__ */ new Set();
    const firsts = [...s.tokens.get(1) ?? []].sort((a, b) => a.height - b.height || a.index - b.index || compareKey(a.txidKey, b.txidKey));
    let pred = firsts[0];
    while (pred !== void 0) {
      tokens.push(pred);
      const p = pred;
      const next = (s.tokens.get(p.anchor.interval + 1) ?? []).find((t) => t.inputs.includes(p.outpoint));
      if (next === void 0) break;
      if (next.anchor.first !== p.anchor.first + p.anchor.count) {
        joinFailed.add(next.txid);
        break;
      }
      pred = next;
    }
    s.chain = { tokens, joinFailed };
    return s.chain;
  }
  tokenStatus(s, t) {
    const { tokens, joinFailed } = this.chainOf(s);
    if (tokens[t.anchor.interval - 1] === t) return "canonical";
    if (joinFailed.has(t.txid)) return "anchor-join";
    if (t.anchor.interval > 1) {
      const prev = toHex2(t.anchor.prev);
      const held = (s.tokens.get(t.anchor.interval - 1) ?? []).some((p) => p.txidKey === prev);
      if (!held) return "pending";
    }
    return "anchor-branch";
  }
  /**
   * An interval's completeness (spec section 8.3): every covered seq held by
   * a batch or a receipt, and one choice of candidates whose root is the
   * token's. Each choice starts from a candidate at the last seq and follows
   * prev back; a position with one candidate that prev does not select is
   * tried anyway, so the root, not the link, decides.
   */
  intervalReport(s, t) {
    return this.intervalChoice(s, t).report;
  }
  /** The interval's report, and when it is complete the candidates whose root is the token's, in seq order. */
  intervalChoice(s, t) {
    const a = t.anchor;
    const last = a.first + a.count - 1;
    const base = { interval: a.interval, first: a.first, count: a.count, txid: t.txid };
    if (a.interval <= s.retiredInterval) return { report: { ...base, status: "complete", missing: [], retired: true } };
    const missing = [];
    for (let seq = a.first; seq <= last; seq++) if ((s.batches.get(seq)?.length ?? 0) === 0) missing.push(seq);
    if (missing.length > 0) return { report: { ...base, status: "incomplete", missing } };
    for (const end of s.batches.get(last)) {
      const chosen = [end];
      for (let seq = last - 1; seq >= a.first; seq--) {
        const cands = s.batches.get(seq);
        const pick = cands.find((c) => c.cKey === chosen[0].prev) ?? (cands.length === 1 ? cands[0] : void 0);
        if (pick === void 0) break;
        chosen.unshift(pick);
      }
      if (chosen.length === a.count && bytesEq(intervalRoot(chosen.map((e) => e.c)), a.root)) return { report: { ...base, status: "complete", missing }, chosen };
    }
    return { report: { ...base, status: "root-mismatch", missing } };
  }
  /** Statuses of every (producer, stream) held, or of one stream. */
  report(stream) {
    const out = [];
    for (const s of this.streams.values()) {
      if (stream !== void 0 && s.stream !== stream) continue;
      const holes = [];
      const forks = [];
      const retracted = [];
      let last = s.retiredSeq;
      for (const seq of [...s.batches.keys()].sort((a, b) => a - b)) {
        if (seq > last + 1) holes.push([last + 1, seq - 1]);
        last = seq;
        const statuses = s.batches.get(seq).map((e) => this.batchStatus(s, e));
        if (statuses.includes("fork")) forks.push(seq);
        if (statuses.every((x) => x === "retracted")) retracted.push(seq);
      }
      const { tokens } = this.chainOf(s);
      const others = [];
      for (const list of s.tokens.values()) {
        for (const t of list) {
          const st = this.tokenStatus(s, t);
          if (st !== "canonical") others.push({ interval: t.anchor.interval, txid: t.txid, status: st });
        }
      }
      out.push({
        producer: s.producer,
        stream: s.stream,
        highest: s.highest,
        holes,
        forks,
        retracted,
        intervals: tokens.map((t) => this.intervalReport(s, t)),
        tokens: others.sort((x, y) => x.interval - y.interval || compareKey(x.txid, y.txid))
      });
    }
    return out;
  }
  /** The status of the batch with commitment C (display txid) on a stream, if held. */
  batchStatusOf(producer, stream, txid) {
    const s = this.streams.get(`${producer}|${stream}`);
    if (s === void 0) return void 0;
    for (const list of s.batches.values()) {
      const e = list.find((x) => x.txid === txid);
      if (e !== void 0) return this.batchStatus(s, e);
    }
    return void 0;
  }
  // ---- questions
  async lookup(question) {
    if (question.service !== LookupService) throw new Error(`ls_log: this service is ${LookupService}, not ${String(question.service)}`);
    const q = parseQuery(question.query);
    if ("tree" in q) {
      const topic2 = `${TopicPrefix}${q.stream}`;
      const k = `${topic2}|${q.tree}`;
      const held = [...this.trees.get(k) ?? []];
      const low = this.treeLow.get(k);
      if (low !== void 0) held.push(low);
      if (held.length > 0) return [{ txid: q.tree, outputIndex: Math.min(...held) }];
      if (this.retention !== void 0 && this.storage !== void 0) {
        const o = await this.storage.findOutput(q.tree, 0, topic2);
        if (o !== null && isFundingShape(Uint8Array.from(o.outputScript))) return [{ txid: q.tree, outputIndex: 0 }];
      }
      return [];
    }
    const s = this.streams.get(`${q.producer}|${q.stream}`);
    if (s === void 0) return [];
    if ("seq" in q) {
      return (s.batches.get(q.seq) ?? []).filter((e) => !this.isRetracted(e)).map((e) => ({ txid: e.txid, outputIndex: e.outputIndex }));
    }
    const canonical = this.chainOf(s).tokens[q.interval - 1];
    const list = [...s.tokens.get(q.interval) ?? []].sort(
      (a, b) => a === canonical ? -1 : b === canonical ? 1 : compareKey(a.txidKey, b.txidKey)
    );
    return list.map((t) => ({ txid: t.txid, outputIndex: t.outputIndex }));
  }
  // ---- restore
  /**
   * Rebuilds the index from storage (spec section 8.5). The host hands the
   * unspent outputs of this module's topics, with their BEEF (a row handed
   * without it is read again from storage with it); the index also needs
   * spent ones (each anchor token but the newest is spent by the next, and
   * a carrier's funding output by the carrier and by any sweep), so it
   * walks from every row to the outputs its transaction spent
   * (outputsConsumed), the outputs that spent it (consumedBy), and its
   * transaction's other outputs, asking storage for each. Every output
   * connected to an unspent one is found, whatever order storage returns
   * them in; statuses are functions of the set. Storage is kept for the
   * topic managers' previous coins. A storage failure propagates: a restore that
   * skipped a retraction would serve retracted batches.
   */
  async restore(outputs, storage) {
    this.restoring = true;
    try {
      return await this.restoreIndex(outputs, storage);
    } finally {
      this.restoring = false;
      if (this.retention !== void 0) {
        this.retain().catch((e) => this.host.log("ls_log retention pass failed", { err: String(e) }));
      }
    }
  }
  async restoreIndex(outputs, storage) {
    this.streams.clear();
    this.byOutpoint.clear();
    this.held.clear();
    this.trees.clear();
    this.carriersByFunding.clear();
    this.spenders.clear();
    this.retracted.clear();
    this.noted.clear();
    this.forkSeqs = 0;
    this.drops = [];
    const full = storage;
    this.storage = full;
    const key = (topic2, txid, vout) => `${topic2}|${outpointKey(txid, vout)}`;
    const seen = /* @__PURE__ */ new Set();
    const queue = [];
    for (const o of outputs) {
      if (!this.topics.has(o.topic)) continue;
      seen.add(key(o.topic, o.txid, o.outputIndex));
      queue.push(o);
    }
    const txs = /* @__PURE__ */ new Map();
    const absent = /* @__PURE__ */ new Set();
    const fundings = [];
    const visit = async (topic2, txid, vout) => {
      const k = key(topic2, txid, vout);
      if (seen.has(k)) return;
      seen.add(k);
      const o = await full.findOutput(txid, vout, topic2, void 0, true);
      if (o !== null) queue.push(o);
      else absent.add(k);
    };
    let indexed = 0;
    while (queue.length > 0) {
      let o = queue.shift();
      if (o.beef === void 0 && !txs.has(o.txid)) {
        o = await full.findOutput(o.txid, o.outputIndex, o.topic, void 0, true) ?? o;
      }
      let tx = txs.get(o.txid);
      const first = !txs.has(o.txid);
      if (first) {
        try {
          tx = o.beef === void 0 ? void 0 : Transaction2.fromBEEF(o.beef, o.txid);
        } catch {
          tx = void 0;
        }
        txs.set(o.txid, tx);
      }
      if (tx === void 0) {
        this.host.log("ls_log could not restore an output without its transaction", { topic: o.topic, txid: o.txid, outputIndex: o.outputIndex });
      } else {
        try {
          if (this.index(o.topic, tx, o.outputIndex)) {
            this.noteSpends(tx);
            indexed++;
            if (this.retention !== void 0 && o.consumedBy.length > 0 && this.trees.get(`${o.topic}|${o.txid}`)?.has(o.outputIndex) === true) {
              fundings.push({ topic: o.topic, op: outpointKey(o.txid, o.outputIndex), spenders: o.consumedBy.map((c) => key(o.topic, c.txid, c.outputIndex)) });
            }
          }
        } catch (e) {
          this.host.log("ls_log could not restore an output", { topic: o.topic, txid: o.txid, outputIndex: o.outputIndex, err: String(e) });
        }
      }
      for (const c of o.outputsConsumed) await visit(o.topic, c.txid, c.outputIndex);
      for (const c of o.consumedBy) await visit(o.topic, c.txid, c.outputIndex);
      if (first && tx !== void 0) for (let j = 0; j < tx.outputs.length; j++) await visit(o.topic, o.txid, j);
    }
    for (const f of fundings) {
      if (f.spenders.every((k) => absent.has(k)) && (this.carriersByFunding.get(f.op)?.size ?? 0) === 0) this.retireFunding(f.topic, f.op);
    }
    return indexed;
  }
  // ---- retention (retention.ts)
  running;
  /**
   * One retention pass at the host's time: for each stream, its oldest
   * intervals not yet dropped, in order, while each is canonical, complete
   * and older than the policy, have every carrier dropped from the index;
   * the state file is written; then the dropped carriers' outputs are
   * deleted from storage. Never while a restore is in flight; a pass asked
   * for while one runs is that pass.
   */
  retain() {
    if (this.running !== void 0) return this.running;
    this.running = this.retainPass().finally(() => {
      this.running = void 0;
    });
    return this.running;
  }
  async retainPass() {
    const out = { intervals: 0, carriers: 0, deleted: 0 };
    const r = this.retention;
    if (r === void 0 || this.restoring || this.storage === void 0) return out;
    const now = (r.now ?? Date.now)();
    for (const s of this.streams.values()) {
      const chain = this.chainOf(s).tokens;
      for (let k = s.retiredInterval + 1; k <= chain.length; k++) {
        const t = chain[k - 1];
        if (now - t.anchor.timestamp < r.keepMs) break;
        const { report, chosen } = this.intervalChoice(s, t);
        if (report.status !== "complete" || chosen === void 0) break;
        const topic2 = `${TopicPrefix}${s.stream}`;
        for (let seq = t.anchor.first; seq <= t.anchor.first + t.anchor.count - 1; seq++) {
          const list = s.batches.get(seq) ?? [];
          if (list.length > 1) this.forkSeqs--;
          for (const e of list) {
            this.retireBatch(topic2, e);
            out.carriers++;
          }
          s.batches.delete(seq);
        }
        s.retiredInterval = k;
        s.retiredSeq = t.anchor.first + t.anchor.count - 1;
        s.retiredC = chosen[chosen.length - 1].cKey;
        this.marks.set(`${s.producer}|${s.stream}`, { producer: s.producer, stream: s.stream, interval: k, seq: s.retiredSeq, c: s.retiredC });
        this.stateDirty = true;
        out.intervals++;
      }
    }
    if (out.carriers > 0) {
      this.host.metrics.inc("blogs_retired_total", void 0, out.carriers);
      this.host.log("ls_log retention dropped proven carriers", { intervals: out.intervals, carriers: out.carriers });
    }
    if (this.stateDirty) {
      try {
        r.store.save({ marks: [...this.marks.values()], trees: [...this.treeLow] });
        this.stateDirty = false;
      } catch (e) {
        this.host.log("ls_log could not write the retention state; nothing is deleted from storage until it is", { err: String(e) });
        return out;
      }
    }
    out.deleted = await this.deleteDrops();
    return out;
  }
  /** Takes a carrier out of the index, with its funding output when no other carrier spends it, and queues its output for deletion. */
  retireBatch(topic2, e) {
    this.forget(e);
    const op = outpointKey(e.txid, e.outputIndex);
    this.held.delete(`${topic2}|${op}`);
    this.unref(op, topic2);
    this.noted.delete(e.txid);
    this.drops.push({ txid: e.txid, vout: e.outputIndex, topic: topic2 });
    if ((this.carriersByFunding.get(e.funding)?.size ?? 0) === 0) this.retireFunding(topic2, e.funding);
  }
  /**
   * Takes a funding output out of the index. Storage keeps it, spent, with
   * its transaction (spec section 8.1); the tree class answers its tree
   * from what is left of it in the index, from treeLow, or from storage.
   */
  retireFunding(topic2, op) {
    this.held.delete(`${topic2}|${op}`);
    this.unref(op, topic2);
    this.spenders.delete(op);
    const dot = op.lastIndexOf(".");
    const k = `${topic2}|${op.slice(0, dot)}`;
    const vout = Number(op.slice(dot + 1));
    const vouts = this.trees.get(k);
    if (vouts === void 0 || !vouts.has(vout)) return;
    vouts.delete(vout);
    const low = Math.min(this.treeLow.get(k) ?? Infinity, vout);
    if (vouts.size > 0 || low !== 0) this.treeLow.set(k, low);
    else this.treeLow.delete(k);
    if (vouts.size === 0) this.trees.delete(k);
    this.stateDirty = true;
  }
  unref(op, topic2) {
    const refs = this.byOutpoint.get(op);
    if (refs === void 0) return;
    const keep = refs.filter((r) => r.topic !== topic2);
    if (keep.length === 0) this.byOutpoint.delete(op);
    else this.byOutpoint.set(op, keep);
  }
  /** Deletes the queued carrier outputs from storage; one that fails stays queued. */
  async deleteDrops() {
    const s = this.storage;
    if (this.drops.length === 0 || s === void 0) return 0;
    if (s.deleteOutput === void 0) {
      if (!this.warned.has("delete")) {
        this.warned.add("delete");
        this.host.log("ls_log cannot delete dropped carriers: the storage has no deleteOutput; they stay in storage and restore skips them");
      }
      this.drops = [];
      return 0;
    }
    const queue = this.drops;
    this.drops = [];
    let n = 0;
    for (const d of queue) {
      try {
        await s.deleteOutput(d.txid, d.vout, d.topic);
        n++;
      } catch (e) {
        this.drops.push(d);
        this.host.log("ls_log could not delete a dropped carrier; the next pass tries again", { txid: d.txid, err: String(e) });
      }
    }
    return n;
  }
  async getDocumentation() {
    return [
      "# ls_log",
      "",
      "Three free question classes, for every blogs stream this host carries:",
      "",
      '- `{"stream": s, "producer": p, "seq": n}`: the admitted, unretracted batch',
      "  carriers of p on s at seq n (at most 8, by commitment);",
      '- `{"stream": s, "producer": p, "interval": k}`: the admitted anchor tokens of',
      "  p on s for interval k (at most 8, canonical first);",
      '- `{"stream": s, "tree": t}`: the mined funding tree with txid t whose funding',
      "  outputs the topic of s holds, spent or not (one output, the lowest held; its",
      "  BEEF is the tree with its proof).",
      "",
      "Any other member, a missing member or a value out of range is refused with",
      "an error. An empty answer means only that this host holds nothing that answers."
    ].join("\n");
  }
  async getMetaData() {
    return { name: LookupService, shortDescription: "Answers blogs batches by seq, anchor tokens by interval and funding trees by txid." };
  }
};
function parseQuery(query) {
  if (typeof query !== "object" || query === null || Array.isArray(query)) throw new Error("ls_log: the query is not an object");
  const q = query;
  const members = Object.keys(q).sort();
  const cls = members.includes("tree") ? "tree" : members.includes("seq") ? "seq" : "interval";
  const want = (cls === "tree" ? ["stream", "tree"] : ["producer", cls, "stream"]).sort();
  if (members.length !== want.length || members.some((m, i) => m !== want[i])) {
    throw new Error(
      `ls_log: a query has exactly stream, producer and one of seq or interval, or exactly stream and tree; got ${members.join(", ") || "nothing"}`
    );
  }
  const stream = q["stream"];
  if (typeof stream !== "string") throw new Error("ls_log: stream is not a string");
  try {
    checkStream(stream);
  } catch {
    throw new Error("ls_log: stream is not a stream identifier <name>_<suffix>");
  }
  if (cls === "tree") {
    const tree = q["tree"];
    if (typeof tree !== "string" || !/^[0-9a-f]{64}$/.test(tree)) throw new Error("ls_log: tree is not a txid of 64 lowercase hex characters");
    return { stream, tree };
  }
  const producer = q["producer"];
  if (typeof producer !== "string" || !/^[0-9a-f]{66}$/.test(producer)) throw new Error("ls_log: producer is not 66 lowercase hex characters");
  try {
    checkProducer(fromHex(producer));
  } catch {
    throw new Error("ls_log: producer is not a compressed secp256k1 key");
  }
  const n = q[cls];
  if (typeof n !== "number" || !Number.isInteger(n) || n < 1 || n > MaxSafe) throw new Error(`ls_log: ${cls} is not an integer from 1 to 2^53 - 1`);
  return cls === "seq" ? { stream, producer, seq: n } : { stream, producer, interval: n };
}

// src/retention.ts
import { closeSync, fsyncSync, mkdirSync, openSync, readFileSync, renameSync, writeSync } from "node:fs";
import { isAbsolute, join } from "node:path";
var MinKeepDays = 1;
var RetentionEvery = 60 * 60 * 1e3;
var StateFile = "blogs-retention.json";
function parseRetention(env) {
  const raw = env["BLOGS_KEEP_DAYS"]?.trim() ?? "";
  if (raw === "") {
    if ((env["BLOGS_STATE_DIR"] ?? "") !== "") throw new Error("BLOGS_STATE_DIR is set but BLOGS_KEEP_DAYS is not: set both to keep carriers for a bounded time, or neither to keep everything");
    return void 0;
  }
  if (!/^[0-9]+(\.[0-9]+)?$/.test(raw)) throw new Error(`BLOGS_KEEP_DAYS: "${raw}" is not a number of days`);
  const days = Number(raw);
  if (!(days >= MinKeepDays)) throw new Error(`BLOGS_KEEP_DAYS: ${raw} is below the floor of ${MinKeepDays} day`);
  const dir = env["BLOGS_STATE_DIR"] ?? "";
  if (dir === "" || !isAbsolute(dir)) throw new Error("BLOGS_KEEP_DAYS needs BLOGS_STATE_DIR, an absolute directory for the retention state");
  return { keepMs: days * 24 * 60 * 60 * 1e3, stateDir: dir };
}
var isCount = (n) => typeof n === "number" && Number.isSafeInteger(n) && n >= 0;
function fileStore(dir) {
  const path = join(dir, StateFile);
  return {
    load() {
      let raw;
      try {
        raw = readFileSync(path, "utf8");
      } catch (e) {
        if (e.code === "ENOENT") return { marks: [], trees: [] };
        throw e;
      }
      const j = JSON.parse(raw);
      const ok = j.v === 1 && Array.isArray(j.marks) && Array.isArray(j.trees) && j.marks.every(
        (m) => typeof m.producer === "string" && typeof m.stream === "string" && isCount(m.interval) && isCount(m.seq) && typeof m.c === "string" && /^[0-9a-f]{64}$/.test(m.c)
      ) && j.trees.every((t) => Array.isArray(t) && t.length === 2 && typeof t[0] === "string" && isCount(t[1]));
      if (!ok) throw new Error(`${path} is not a blogs retention state; a position read as held again would be counted a hole`);
      return { marks: j.marks, trees: j.trees };
    },
    save(s) {
      mkdirSync(dir, { recursive: true });
      const tmp = `${path}.tmp`;
      const fd = openSync(tmp, "w", 384);
      try {
        writeSync(fd, JSON.stringify({ v: 1, marks: s.marks, trees: s.trees }));
        fsyncSync(fd);
      } finally {
        closeSync(fd);
      }
      renameSync(tmp, path);
    }
  };
}

// src/tm_log.ts
import { Transaction as Transaction3 } from "@bsv/sdk";
var Reasons = [
  "too-large",
  "cbor",
  "key-type",
  "magic",
  "missing",
  "type",
  "range",
  "producer",
  "stream",
  "prev",
  "profile-shape",
  "locator",
  "content",
  "carrier-shape",
  "anchor-shape",
  "producer-policy",
  "mineable",
  "lock",
  "signature",
  "funding",
  "unlock",
  "anchor-input",
  "unmined"
];
var DeferredReasons = ["anchor-input", "unmined"];
var AdmitKinds = ["batch", "anchor", "funding", "spend", "none"];
var Unrecorded = class extends Error {
  constructor(reason, detail) {
    super(`blogs: ${reason}, not recorded: ${detail}`);
    this.reason = reason;
    this.name = "Unrecorded";
  }
  reason;
};
var engineVerified = {
  isValidRootForHeight: async () => true,
  currentHeight: async () => 0
};
var LogTopicManager = class {
  constructor(topic2, policy, coins, host, headers = engineVerified) {
    this.topic = topic2;
    this.policy = policy;
    this.coins = coins;
    this.host = host;
    this.headers = headers;
    if (!topic2.startsWith(TopicPrefix)) throw new Error(`blogs: ${topic2} is not a tm_log_ topic`);
    this.stream = topic2.slice(TopicPrefix.length);
    checkStream(this.stream);
    validatePolicy(policy);
  }
  topic;
  policy;
  coins;
  host;
  headers;
  stream;
  async identifyAdmissibleOutputs(beef, previousCoins) {
    const tx = Transaction3.fromBEEF(beef);
    const txid = tx.id("hex");
    const coins = /* @__PURE__ */ new Map();
    for (const i of previousCoins) {
      const input = tx.inputs[i];
      const src = input?.sourceTXID ?? input?.sourceTransaction?.id("hex");
      if (input === void 0 || src === void 0) continue;
      const coin = await this.coins(this.topic, src, input.sourceOutputIndex);
      if (coin !== void 0) coins.set(outpointKey(src, input.sourceOutputIndex), coin);
    }
    let kind;
    let outputsToAdmit;
    try {
      const a = await admit(tx, coins, { stream: this.stream, policy: this.policy, headers: this.headers });
      if (a.kind === "batch") {
        kind = "batch";
        outputsToAdmit = [0];
      } else if (a.kind === "anchor") {
        kind = "anchor";
        outputsToAdmit = [a.token.outputIndex];
      } else if (a.funding.length > 0) {
        kind = "funding";
        outputsToAdmit = a.funding;
      } else {
        kind = previousCoins.length > 0 ? "spend" : "none";
        outputsToAdmit = [];
      }
    } catch (e) {
      if (!(e instanceof Refusal)) throw e;
      const recorded = previousCoins.length === 0 && !DeferredReasons.includes(e.reason);
      this.host.metrics.inc("blogs_refused_total", { reason: e.reason, recorded: recorded ? "yes" : "no" });
      this.host.log("tm_log refused", { topic: this.topic, txid, reason: e.reason, detail: e.message, recorded });
      if (!recorded) {
        throw new Unrecorded(e.reason, previousCoins.length > 0 ? `${txid} spends held outputs` : `${txid} may be decided later`);
      }
      return { outputsToAdmit: [], coinsToRetain: [] };
    }
    this.host.metrics.inc("blogs_admitted_total", { kind });
    return { outputsToAdmit, coinsToRetain: [...previousCoins] };
  }
  /** Nothing is needed: a carrier carries its ancestry, an anchor's spent outputs are held. */
  async identifyNeededInputs() {
    return [];
  }
  async getDocumentation() {
    return [
      `# ${this.topic}`,
      "",
      `The blogs stream ${this.stream}: batch carriers, mined anchor tokens, and the`,
      "funding outputs of mined funding trees and sweeps, each admitted on its own",
      "validity (spec section 8.1). A transaction that spends held outputs and claims",
      "nothing is accepted for its inputs alone. Gaps and forks between batches are",
      "reported by ls_log, never refused."
    ].join("\n");
  }
  async getMetaData() {
    return { name: this.topic, shortDescription: `Admits the blogs log stream ${this.stream}.` };
  }
};

// src/module.ts
function parseStreams(raw) {
  if (raw === void 0 || raw.trim() === "") throw new Error("BLOGS_STREAMS is required: name each stream and its producer policy");
  const out = [];
  for (const entry of raw.split(",")) {
    const e = entry.trim();
    const eq = e.indexOf("=");
    if (eq < 0) throw new Error(`BLOGS_STREAMS: "${e}" is not <stream>=open or <stream>=<key>+<key>`);
    const stream = e.slice(0, eq);
    const value = e.slice(eq + 1);
    try {
      checkStream(stream);
    } catch {
      throw new Error(`BLOGS_STREAMS: "${stream}" is not a stream identifier <name>_<suffix>`);
    }
    if (out.some((s) => s.stream === stream)) throw new Error(`BLOGS_STREAMS names ${stream} twice`);
    let policy;
    if (value === "open") {
      policy = { open: true };
    } else {
      const producers = value.split("+").map((k) => {
        if (!/^[0-9a-f]{66}$/.test(k)) throw new Error(`BLOGS_STREAMS: ${stream}: "${k}" is not 66 lowercase hex characters`);
        const b = fromHex(k);
        try {
          checkProducer(b);
        } catch {
          throw new Error(`BLOGS_STREAMS: ${stream}: ${k} is not a compressed secp256k1 key`);
        }
        return b;
      });
      policy = { producers };
    }
    validatePolicy(policy);
    out.push({ stream, policy });
  }
  return out;
}
function blogsModule(host, streams, overlayTopics, retention) {
  if (streams.length === 0) throw new Error("blogs: no streams configured");
  const names = new Set(streams.map((s) => topic(s.stream)));
  for (const t of overlayTopics ?? []) {
    if (t.startsWith(TopicPrefix) && !names.has(t)) {
      throw new Error(`OVERLAY_TOPICS names ${t} but BLOGS_STREAMS does not configure it; it would be admitted by the default manager`);
    }
  }
  for (const kind of AdmitKinds) host.metrics.preset("blogs_admitted_total", { kind });
  for (const reason of Reasons) {
    host.metrics.preset("blogs_refused_total", { reason, recorded: "yes" });
    host.metrics.preset("blogs_refused_total", { reason, recorded: "no" });
  }
  for (const c of ["blogs_retractions_total", "blogs_candidates_dropped_total", "blogs_tokens_dropped_total", "blogs_retired_total"]) host.metrics.preset(c);
  const ls = new LogLookupService(names, host, retention);
  host.metrics.gauge("blogs_streams", () => ls.streamCount);
  host.metrics.gauge("blogs_batches", () => ls.batchCount);
  host.metrics.gauge("blogs_anchor_tokens", () => ls.tokenCount);
  host.metrics.gauge("blogs_funding_trees", () => ls.treeCount);
  host.metrics.gauge("blogs_retracted_batches", () => ls.retractedCount);
  host.metrics.gauge("blogs_missing_positions", () => ls.missingCount);
  host.metrics.gauge("blogs_fork_positions", () => ls.forkSeqCount);
  host.metrics.gauge("blogs_retired_positions", () => ls.retiredCount);
  if (retention !== void 0) {
    setInterval(() => {
      ls.retain().catch((e) => host.log("ls_log retention pass failed", { err: String(e) }));
    }, RetentionEvery).unref();
  }
  const topics = {};
  for (const s of streams) topics[topic(s.stream)] = new LogTopicManager(topic(s.stream), s.policy, ls.coin, host);
  const build = true ? "@lightwebinc/blogs-host 0.6.3: tm_log_<stream> and ls_log, with @lightwebinc/bcommon 0.33.13 inlined; imports @bsv/sdk (built against 2.7.1)" : "unbundled";
  host.log("blogs module mounted", {
    topics: [...names].join(","),
    lookup: LookupService,
    build,
    retention: retention === void 0 ? "off" : `${retention.keepMs / 864e5} days`
  });
  return { topics, lookups: { [LookupService]: ls } };
}
function create(host) {
  const overlay = process.env["OVERLAY_TOPICS"];
  const topics = overlay === void 0 ? void 0 : overlay.split(",").map((t) => t.trim()).filter((t) => t !== "");
  const streams = parseStreams(process.env["BLOGS_STREAMS"]);
  const r = parseRetention(process.env);
  return blogsModule(host, streams, topics, r === void 0 ? void 0 : { keepMs: r.keepMs, store: fileStore(r.stateDir) });
}
export {
  LogLookupService,
  LogTopicManager,
  Unrecorded,
  blogsModule,
  create as default,
  parseQuery,
  parseRetention,
  parseStreams
};
