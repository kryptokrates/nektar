// @lightwebinc/bfinger-host 0.8.4: tm_finger and ls_finger, with @lightwebinc/bcommon 0.33.13 inlined

// src/ls_finger.ts
import { Hash, LockingScript, Transaction, Utils as Utils2 } from "@bsv/sdk";

// src/carrier.ts
import { Utils } from "@bsv/sdk";

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

// node_modules/@lightwebinc/bcommon/dist/store.js
var refMembers = /* @__PURE__ */ new Set(["name", "root", "count", "head"]);
var MaxRefMembers = 8;
var MaxRefs = 64;
var MaxRefName = 64;
var StoreError = class extends Error {
  code;
  detail;
  constructor(code, detail) {
    super(`store: ${code}: ${detail}`);
    this.code = code;
    this.detail = detail;
    this.name = "StoreError";
  }
};
var utf8 = new TextEncoder();
function fixed(v, n, name) {
  if (!(v instanceof Uint8Array) || v.length !== n)
    throw new StoreError("field", `${name} wants ${n} bytes`);
  return v.slice();
}
function unsigned(v, name) {
  if (typeof v !== "bigint" || v < 0n)
    throw new StoreError("field", `${name} wants an unsigned integer`);
  return v;
}
function decodeRefs(v) {
  if (!Array.isArray(v))
    throw new StoreError("field", "refs");
  if (v.length > MaxRefs)
    throw new StoreError("field", `${v.length} refs`);
  const refs = [];
  const seenRef = /* @__PURE__ */ new Set();
  for (const e of v) {
    if (!(e instanceof CborMap) || e.entries.length < 3 || e.entries.length > MaxRefMembers) {
      throw new StoreError("field", "ref");
    }
    const head2 = e.get("head");
    const name = e.get("name");
    if (typeof name !== "string" || name === "" || utf8.encode(name).length > MaxRefName) {
      throw new StoreError("field", "ref name");
    }
    if (seenRef.has(name))
      throw new StoreError("dup-ref", name);
    seenRef.add(name);
    const ref = {
      name,
      root: fixed(e.get("root"), 32, "ref root"),
      count: unsigned(e.get("count"), "ref count")
    };
    if (head2 !== void 0)
      ref.head = fixed(head2, 32, "ref head");
    const extra = e.entries.filter((p) => typeof p.key !== "string" || !refMembers.has(p.key));
    for (const p of extra)
      if (typeof p.key !== "string")
        throw new StoreError("field", `ref member key ${typeof p.key}`);
    if (extra.length > 0)
      ref.unknown = extra;
    refs.push(ref);
  }
  return refs;
}

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

// node_modules/@lightwebinc/bcommon/dist/pushdrop.js
import { OP, PushDrop } from "@bsv/sdk";
function minimalPush(d) {
  const n = d.length;
  if (n === 0 || n === 1 && d[0] === 0)
    return [OP.OP_0];
  if (n === 1 && d[0] >= 1 && d[0] <= 16)
    return [80 + d[0]];
  if (n === 1 && d[0] === 129)
    return [OP.OP_1NEGATE];
  if (n <= 75)
    return [n, ...d];
  if (n <= 255)
    return [OP.OP_PUSHDATA1, n, ...d];
  if (n <= 65535)
    return [OP.OP_PUSHDATA2, n & 255, n >> 8, ...d];
  return [OP.OP_PUSHDATA4, n & 255, n >> 8 & 255, n >> 16 & 255, n >>> 24, ...d];
}
function canonicalLock(key, fields) {
  const out = [...minimalPush(key), OP.OP_CHECKSIG];
  for (const f of fields)
    out.push(...minimalPush(f));
  let n = fields.length;
  for (; n > 1; n -= 2)
    out.push(OP.OP_2DROP);
  if (n === 1)
    out.push(OP.OP_DROP);
  return out;
}
function decodeStrictPushDrop(script) {
  const keyBytes = script.chunks[0]?.data;
  if (keyBytes === void 0 || strictPublicKey(keyBytes) === void 0)
    return void 0;
  let decoded;
  try {
    decoded = PushDrop.decode(script, "before");
  } catch {
    return void 0;
  }
  const want = canonicalLock(keyBytes, decoded.fields);
  const got = script.toBinary();
  if (got.length !== want.length || !got.every((x, i) => x === want[i]))
    return void 0;
  return decoded;
}

// node_modules/@lightwebinc/bcommon/dist/carrier.js
import { PushDrop as PushDrop2 } from "@bsv/sdk";

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
function inspectScript(script, codec) {
  let decoded;
  try {
    decoded = PushDrop2.decode(script, "before");
  } catch {
    return { kind: "not-record" };
  }
  if (decoded.fields.length !== 2)
    return { kind: "not-record" };
  const [s, sig] = decoded.fields;
  if (s === void 0 || sig === void 0)
    return { kind: "not-record" };
  const payloadBytes = Uint8Array.from(s);
  const insp = codec.inspect(payloadBytes);
  if (insp.kind === "not-payload")
    return { kind: "not-record" };
  if (insp.kind === "bad-payload")
    return { kind: "bad-record", detail: insp.detail };
  if (decodeStrictPushDrop(script) === void 0)
    return { kind: "bad-record", detail: "the record output is not the canonical PushDrop" };
  return { kind: "carrier", out: { payload: insp.payload, payloadBytes, lockingKey: decoded.lockingPublicKey, signature: [...sig] } };
}
function payloadRefusal(out, codec) {
  try {
    codec.validate(out.payload);
    return void 0;
  } catch {
    return "bad-record";
  }
}
function lockRefusal(out, lockingKeyFor2) {
  let want;
  try {
    want = lockingKeyFor2(out.payload);
  } catch {
    return "bad-lock";
  }
  return want.toString() === out.lockingKey.toString() ? void 0 : "bad-lock";
}
function signatureRefusal(out) {
  return verifyFieldSignature(out.lockingKey, Array.from(out.payloadBytes), out.signature) ? void 0 : "bad-sig";
}
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
function commitment(tx) {
  return tx.hash();
}
function decodeCarrier(tx, codec, lockingKeyFor2) {
  let found;
  for (let i = 0; i < tx.outputs.length; i++) {
    const out = tx.outputs[i];
    if (out === void 0)
      continue;
    const insp = inspectScript(out.lockingScript, codec);
    if (insp.kind === "not-record")
      continue;
    if (insp.kind === "bad-record")
      return "bad-record";
    if (found !== void 0)
      return "bad-record";
    found = { ...insp.out, outputIndex: i };
  }
  if (found === void 0)
    return "not-pushdrop";
  const refused = payloadRefusal(found, codec) ?? mineableRefusal(tx) ?? unlockingRefusal(tx) ?? lockRefusal(found, lockingKeyFor2) ?? signatureRefusal(found);
  if (refused !== void 0)
    return refused;
  return {
    outputIndex: found.outputIndex,
    payload: found.payload,
    payloadBytes: found.payloadBytes,
    lockingKey: found.lockingKey,
    c: commitment(tx)
  };
}

// node_modules/@lightwebinc/bcommon/dist/funding.js
import { OP as OP2 } from "@bsv/sdk";
function sameBytes(a, b) {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
function decodeFunding(script, tag) {
  const chunks = script.chunks;
  if (chunks.length !== 4)
    return void 0;
  const [key, checksig, tagChunk, drop] = chunks;
  if (key?.data === void 0 || key.data.length !== 33)
    return void 0;
  if (checksig?.op !== OP2.OP_CHECKSIG || drop?.op !== OP2.OP_DROP)
    return void 0;
  if (tagChunk?.data === void 0 || !sameBytes(tagChunk.data, tag))
    return void 0;
  const decoded = decodeStrictPushDrop(script);
  if (decoded === void 0 || decoded.fields.length !== 1)
    return void 0;
  return decoded.lockingPublicKey;
}

// src/keys.ts
var Protocol = [1, "bfinger"];
var KeyIDProfile = "profile";
var KeyIDRecord = "record";
function profileLockingKey(identityKeyHex) {
  return readerLockingKey(Protocol, KeyIDProfile, identityKeyHex);
}
function recordLockingKey(identityKeyHex) {
  return readerLockingKey(Protocol, KeyIDRecord, identityKeyHex);
}

// src/record.ts
var keyMagic = 0n;
var keyIdentityKey = 1n;
var keySeq = 2n;
var keyKind = 3n;
var keyPrev = 4n;
var keySalt = 5n;
var keyWC = 6n;
var keyPrevWitness = 7n;
var keyNotBefore = 8n;
var keyNotAfter = 9n;
var keyBody = 10n;
var keyRefs = 11n;
var keySuccessor = 12n;
var KindCreate = 1;
var KindUpdate = 2;
var KindRotate = 3;
var KindRetire = 4;
var KindSub = 5;
var KindManifest = 6;
var MaxBodyBytes = 16384;
var MaxSubBodyBytes = 65536;
function bodyBound(kind) {
  return kind === KindSub || kind === KindManifest ? MaxSubBodyBytes : MaxBodyBytes;
}
var MagicV1 = Uint8Array.from([98, 102, 114, 1]);
var RecordError = class extends Error {
  constructor(code, detail) {
    super(detail === void 0 ? `record: ${code}` : `record: ${code}: ${detail}`);
    this.code = code;
    this.name = "RecordError";
  }
  code;
};
function isZero(b) {
  for (const x of b) if (x !== 0) return false;
  return true;
}
function fixed2(v, n, name) {
  if (!(v instanceof Uint8Array) || v.length !== n) throw new RecordError("field", `${name} wants ${n} bytes`);
  return v.slice();
}
function unsigned2(v, name) {
  if (typeof v !== "bigint" || v < 0n) throw new RecordError("field", `${name} wants an unsigned integer`);
  return v;
}
function checkKeyPrefix(k, name) {
  if (k[0] !== 2 && k[0] !== 3) throw new RecordError("field", `${name} prefix`);
}
function asRecordError(f) {
  try {
    return f();
  } catch (err) {
    if (err instanceof StoreError) throw new RecordError(err.code, err.detail);
    throw err;
  }
}
function decodeRecord(b) {
  const v = decodeValue(b);
  if (!(v instanceof CborMap)) throw new RecordError("shape");
  const seen = /* @__PURE__ */ new Set();
  const r = { unknown: [], refs: [] };
  for (const p of v.entries) {
    if (typeof p.key !== "bigint" || p.key < 0n) throw new RecordError("shape", `key ${typeof p.key}`);
    const k = p.key;
    seen.add(k);
    switch (k) {
      case keyMagic:
        r.magic = fixed2(p.val, 4, "magic");
        if (!bytesEq(r.magic, MagicV1)) throw new RecordError("magic");
        break;
      case keyIdentityKey:
        r.identityKey = fixed2(p.val, 33, "identityKey");
        checkKeyPrefix(r.identityKey, "identityKey");
        break;
      case keySeq:
        r.seq = unsigned2(p.val, "seq");
        break;
      case keyKind: {
        const n = unsigned2(p.val, "kind");
        if (n < BigInt(KindCreate) || n > BigInt(KindManifest)) throw new RecordError("field", `kind ${n}`);
        r.kind = Number(n);
        break;
      }
      case keyPrev:
        r.prev = fixed2(p.val, 32, "prev");
        break;
      case keySalt:
        r.salt = fixed2(p.val, 32, "salt");
        break;
      case keyWC:
        r.wc = fixed2(p.val, 32, "wc");
        break;
      case keyPrevWitness:
        r.prevWitness = fixed2(p.val, 32, "prevWitness");
        break;
      case keyNotBefore:
        r.notBefore = unsigned2(p.val, "notBefore");
        break;
      case keyNotAfter:
        r.notAfter = unsigned2(p.val, "notAfter");
        break;
      case keyBody:
        if (!(p.val instanceof CborMap)) throw new RecordError("field", "body");
        for (const bp of p.val.entries) {
          if (typeof bp.key !== "string") throw new RecordError("field", `body key ${typeof bp.key}`);
        }
        r.body = p.val;
        break;
      case keyRefs:
        r.refs = asRecordError(() => decodeRefs(p.val));
        break;
      case keySuccessor:
        r.successor = fixed2(p.val, 33, "successor");
        checkKeyPrefix(r.successor, "successor");
        break;
      default:
        r.unknown.push(p);
    }
  }
  for (const k of [keyMagic, keyIdentityKey, keySeq, keyKind, keyPrev, keySalt, keyWC, keyNotBefore, keyNotAfter, keyBody, keyRefs]) {
    if (!seen.has(k)) throw new RecordError("missing", `key ${k}`);
  }
  if (encode(r.body).length > bodyBound(r.kind)) throw new RecordError("body-size");
  return r;
}
function bytesEq(a, b) {
  if (a.length !== b.length) return false;
  for (let i = 0; i < a.length; i++) if (a[i] !== b[i]) return false;
  return true;
}
function validateRecord(r) {
  if (!bytesEq(r.magic, MagicV1)) throw new RecordError("magic");
  if (r.seq === 0n) throw new RecordError("field", "seq 0");
  switch (r.kind) {
    case KindCreate:
    case KindSub:
    case KindManifest:
      if (r.seq !== 1n || !isZero(r.prev) || r.prevWitness !== void 0 || r.successor !== void 0) {
        throw new RecordError("kind", `kind ${r.kind} starts a chain: seq 1, zero prev, no witness, no successor`);
      }
      break;
    case KindUpdate:
    case KindRotate:
    case KindRetire:
      if (r.seq < 2n || isZero(r.prev) || r.prevWitness === void 0) {
        throw new RecordError("kind", `kind ${r.kind} needs prev and prevWitness and seq >= 2`);
      }
      if (r.kind === KindRotate !== (r.successor !== void 0)) {
        throw new RecordError("kind", "successor is for rotate only");
      }
      if (r.kind === KindRetire && r.body.entries.length !== 0) {
        throw new RecordError("kind", "retire carries an empty body");
      }
      break;
    default:
      throw new RecordError("field", `kind ${String(r.kind)}`);
  }
  if (r.notBefore !== 0n && r.notAfter !== 0n && r.notBefore > r.notAfter) throw new RecordError("window");
}
function isRecordError(err) {
  return err instanceof RecordError || err instanceof CborError;
}

// src/carrier.ts
var recordCodec = {
  inspect(s) {
    const head2 = s[0];
    if (s.length < 6 || head2 === void 0 || head2 >> 5 !== 5) return { kind: "not-payload" };
    try {
      return { kind: "payload", payload: decodeRecord(s) };
    } catch (err) {
      if (err instanceof RecordError && (err.code === "magic" || err.code === "shape")) return { kind: "not-payload" };
      if (isRecordError(err)) return { kind: "bad-payload", detail: err.message };
      throw err;
    }
  },
  validate(r) {
    validateRecord(r);
    if (strictPublicKey(r.identityKey) === void 0) throw new RecordError("field", "identityKey is not a canonical key");
    if (r.successor !== void 0 && strictPublicKey(r.successor) === void 0) {
      throw new RecordError("field", "successor is not a canonical key");
    }
  }
};
function lockingKeyFor(record) {
  return recordLockingKey(Utils.toHex(record.identityKey));
}
function toFinger(out) {
  return { record: out.payload, recordBytes: out.payloadBytes, lockingKey: out.lockingKey, signature: out.signature };
}
function toLibrary(out) {
  return { payload: out.record, payloadBytes: out.recordBytes, lockingKey: out.lockingKey, signature: out.signature };
}
function inspectCarrierScript(script) {
  const insp = inspectScript(script, recordCodec);
  return insp.kind === "carrier" ? { kind: "carrier", out: toFinger(insp.out) } : insp;
}
function recordRefusal(out) {
  return payloadRefusal(toLibrary(out), recordCodec);
}
function lockRefusal2(out) {
  return lockRefusal(toLibrary(out), lockingKeyFor);
}
function signatureRefusal2(out) {
  return signatureRefusal(toLibrary(out));
}
function decodeCarrier2(tx) {
  const c = decodeCarrier(tx, recordCodec, lockingKeyFor);
  if (typeof c === "string") return c;
  return {
    outputIndex: c.outputIndex,
    record: c.payload,
    recordBytes: c.payloadBytes,
    lockingKey: c.lockingKey,
    c: c.c
  };
}

// src/funding.ts
var FundingTag = [98, 102, 2];
function decodeFunding2(script) {
  return decodeFunding(script, FundingTag);
}

// src/token.ts
import { PushDrop as PushDrop3 } from "@bsv/sdk";
var Tag = [98, 102, 1];
function sameBytes2(a, b) {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
function inspectToken(script) {
  let decoded;
  try {
    decoded = PushDrop3.decode(script, "before");
  } catch {
    return { kind: "not-pushdrop" };
  }
  if (decoded.fields.length !== 3) return { kind: "not-token" };
  const [tag, c, sig] = decoded.fields;
  if (tag === void 0 || c === void 0 || sig === void 0) return { kind: "not-token" };
  if (!sameBytes2(tag, Tag) || c.length !== 32) return { kind: "bad-tag" };
  const strict = decodeStrictPushDrop(script);
  if (strict === void 0) return { kind: "bad-tag" };
  return {
    kind: "token",
    token: { c: [...c], lockingKey: strict.lockingPublicKey, valid: verifyFieldSignature(strict.lockingPublicKey, [...tag, ...c], sig) }
  };
}
function decodeToken(script) {
  const insp = inspectToken(script);
  return insp.kind === "token" ? insp.token : void 0;
}

// src/ls_finger.ts
var JoinFailures = [
  "bad-record",
  "bad-lock",
  "duplicate-create",
  "no-current",
  "retired",
  "rotated",
  "successor-taken",
  "bad-prev",
  "bad-witness",
  "bad-seq",
  "not-spent",
  "killed"
];
var KillReasons = ["funding-spent"];
var outpointKey = (o) => `${o.txid}.${o.outputIndex}`;
function reverseHex(hex) {
  return Utils2.toHex(Utils2.toArray(hex, "hex").reverse());
}
function inputsOf(tx) {
  const out = [];
  for (const input of tx.inputs) {
    const txid = input.sourceTXID ?? input.sourceTransaction?.id("hex");
    if (txid === void 0) continue;
    out.push({ txid, outputIndex: input.sourceOutputIndex });
  }
  return out;
}
var carrierShape = /^[0-9a-f]{64}$/i;
var identityKeyShape = /^0[23][0-9a-fA-F]{64}$/;
var FingerLookupService = class {
  constructor(host) {
    this.host = host;
    for (const reason of JoinFailures) host.metrics.preset("finger_join_failures_total", { reason });
    for (const reason of KillReasons) host.metrics.preset("finger_killed_total", { reason });
  }
  host;
  /**
   * `whole-tx`: a carrier's kill switch is its INPUT, which a locking-script
   * payload does not carry; the atomic BEEF does. `txid` spend notifications
   * are what tell the join which token spent the current one, and what tell
   * the kill switch who spent a funding output.
   */
  admissionMode = "whole-tx";
  spendNotificationMode = "txid";
  /** C hex -> token. */
  /**
   * Tokens by commitment. A LIST per commitment, not one entry.
   *
   * A commitment is public, and a token is admitted on its own validity: the
   * topic manager cannot check whose profile key a token is locked to,
   * because the identity key is not in the token. So anyone may mint a
   * valid token over anyone's commitment. Keeping one token per commitment
   * let a stranger's token displace the real one in the window before the
   * carrier arrives, and the join then failed `bad-lock` and never
   * recovered: the real token was gone from the map and nothing re-added
   * it, so that identity stopped advancing at this host for good.
   *
   * Neither last-writer-wins nor first-writer-wins is safe, because each
   * loses to one ordering. The map holds every candidate and the join picks
   * the one locked to the identity's profile key, which is the only test
   * that distinguishes them and is exactly the test `advance` already made.
   */
  tokens = /* @__PURE__ */ new Map();
  /** C hex (the carrier's txid in hash order) -> carrier. */
  carriers = /* @__PURE__ */ new Map();
  /** Outpoint -> the txid that spent it. Arrives before the spender's admission. */
  spentBy = /* @__PURE__ */ new Map();
  /** Identity key hex -> current state. */
  states = /* @__PURE__ */ new Map();
  /** C hex of transitions that became current. */
  joined = /* @__PURE__ */ new Set();
  /** prev C hex -> C hex of carriers naming it, for out-of-order arrival. */
  byPrev = /* @__PURE__ */ new Map();
  /** Outpoint -> what was indexed there, for eviction. */
  byOutpoint = /* @__PURE__ */ new Map();
  /** Funding outpoint -> its carrier and its spenders. */
  funding = /* @__PURE__ */ new Map();
  /** Carrier txid (display order) -> the identity whose chain it is in. */
  carrierIdentity = /* @__PURE__ */ new Map();
  /** Identities the kill switch retracted. Nothing of theirs is indexed again. */
  killed = /* @__PURE__ */ new Set();
  /** Outpoint -> spenders, for spends heard before the outpoint was known as funding. */
  earlySpends = /* @__PURE__ */ new Map();
  /** Profile locking key hex -> killed identity, so a late token is refused too. */
  killedProfiles = /* @__PURE__ */ new Map();
  /** A scheduled rebuild of the chains from the index (see rebuildChains). */
  rebuildTimer;
  /** Set while rebuildChains replays, so its refusals are not counted twice. */
  rebuilding = false;
  /** How long a burst of arrivals may run before the one rebuild it causes. */
  rebuildDelayMs = 500;
  /** Tokens plus carriers indexed, for the host's gauge. */
  get size() {
    return this.tokenCount + this.carriers.size;
  }
  get tokenCount() {
    let n = 0;
    for (const list of this.tokens.values()) n += list.length;
    return n;
  }
  get carrierCount() {
    return this.carriers.size;
  }
  get identityCount() {
    return this.states.size;
  }
  get killedCount() {
    return this.killed.size;
  }
  /** Funding outpoints known, admitted or named by a carrier. */
  get fundingCount() {
    return this.funding.size;
  }
  /** Tokens whose carrier has not arrived. */
  get pendingTokens() {
    let n = 0;
    for (const [c, list] of this.tokens) if (!this.carriers.has(c)) n += list.length;
    return n;
  }
  outputAdmittedByTopic(payload) {
    if (payload.mode !== "whole-tx") return;
    let tx;
    try {
      tx = Transaction.fromAtomicBEEF(payload.atomicBEEF);
    } catch {
      this.host.log("ls_finger ignored an admission whose BEEF did not parse", { outputIndex: payload.outputIndex });
      return;
    }
    const txid = tx.id("hex");
    const out = tx.outputs[payload.outputIndex];
    if (out === void 0) return;
    this.noteInputs(tx);
    const script = out.lockingScript;
    const outpoint = { txid, outputIndex: payload.outputIndex };
    if (decodeFunding2(script) !== void 0) {
      this.indexFunding(outpoint);
      return;
    }
    const token = decodeToken(script);
    if (token !== void 0) {
      if (!token.valid) return;
      const lockingKeyHex = token.lockingKey.toString();
      const cHex = Utils2.toHex(token.c);
      const victim = this.killedProfiles.get(lockingKeyHex);
      if (victim !== void 0) {
        this.refuseKilled(victim, cHex);
        return;
      }
      this.indexToken({ outpoint, lockingKeyHex, cHex });
      this.join(cHex, false);
      return;
    }
    const entry = this.carrierFromScript(script.toBinary(), outpoint, inputsOf(tx));
    if (entry === void 0) return;
    if (this.killed.has(entry.identityKeyHex)) {
      this.refuseKilled(entry.identityKeyHex, entry.cHex);
      return;
    }
    this.indexCarrier(entry);
    if (!this.linkFunding(entry)) return;
    this.join(entry.cHex, false);
  }
  outputSpent(payload) {
    if (payload.mode !== "txid") return;
    this.noteSpend(outpointKey({ txid: payload.txid, outputIndex: payload.outputIndex }), payload.spendingTxid, false);
  }
  /**
   * Record that `k` was spent by `spendingTxid`. The carrier's own spend of
   * its funding output is the one the design expects. Any other txid is a
   * second spend of the same output: the carrier is now a double spend, and
   * the record it carried is retracted. Idempotent: a kill drops the funding
   * entry, so hearing the same spend twice kills once.
   */
  noteSpend(k, spendingTxid, killing) {
    this.spentBy.set(k, spendingTxid);
    if (!killing) return;
    const f = this.funding.get(k);
    if (f === void 0) {
      let early = this.earlySpends.get(k);
      if (early === void 0) this.earlySpends.set(k, early = /* @__PURE__ */ new Set());
      early.add(spendingTxid);
      return;
    }
    f.spenders.add(spendingTxid);
    if (f.carrierTxid !== void 0 && f.carrierTxid !== spendingTxid) {
      this.kill(f.carrierTxid, "funding-spent", { outpoint: k, spendingTxid });
    }
  }
  /**
   * Every input of an admitted transaction is a spend, whether or not this
   * host holds the output it spends. The engine reports spends only of
   * outputs in storage, so a host that caught up from a peer (GASP sends
   * unspent outputs, graphs in no fixed order) could hold a sweep without
   * the funding outputs it spent and never see the kill. Reading the spends
   * from the transaction itself makes the kill independent of arrival order.
   */
  noteInputs(tx) {
    const txid = tx.id("hex");
    const killing = isFinal(tx);
    for (const input of tx.inputs) {
      const src = input.sourceTXID ?? input.sourceTransaction?.id("hex");
      if (src === void 0) continue;
      this.noteSpend(outpointKey({ txid: src, outputIndex: input.sourceOutputIndex }), txid, killing);
    }
  }
  outputEvicted(txid, outputIndex) {
    const k = outpointKey({ txid, outputIndex });
    const ref = this.byOutpoint.get(k);
    if (ref === void 0) return;
    this.byOutpoint.delete(k);
    if (ref.kind === "funding") {
      this.funding.delete(k);
      return;
    }
    if (ref.kind === "token") {
      this.dropToken(ref.cHex, k);
      return;
    }
    const carrier = this.carriers.get(ref.cHex);
    this.carriers.delete(ref.cHex);
    if (carrier !== void 0) {
      this.byPrev.get(Utils2.toHex(carrier.record.prev))?.delete(ref.cHex);
      this.carrierIdentity.delete(carrier.outpoint.txid);
    }
  }
  /**
   * Decode a carrier from its locking script. The lock and signature are
   * re-verified here although the topic manager refused a carrier failing
   * either: the cost is one derivation, and the enforcement point should not
   * trust that every object reached it through the topic manager.
   */
  carrierFromScript(script, outpoint, funding) {
    const insp = inspectCarrierScript(LockingScript.fromBinary(script));
    if (insp.kind !== "carrier") return void 0;
    const refused = recordRefusal(insp.out) ?? lockRefusal2(insp.out) ?? signatureRefusal2(insp.out);
    if (refused !== void 0) {
      this.host.log("ls_finger ignored a carrier the topic manager should have refused", { txid: outpoint.txid, reason: refused });
      return void 0;
    }
    insp.out.record.body = new CborMap();
    insp.out.record.refs = [];
    return {
      outpoint,
      cHex: reverseHex(outpoint.txid),
      record: insp.out.record,
      identityKeyHex: Utils2.toHex(insp.out.record.identityKey),
      funding
    };
  }
  /** The spenders heard for k before it was known as funding, now claimed. */
  takeEarly(k) {
    const early = this.earlySpends.get(k) ?? /* @__PURE__ */ new Set();
    this.earlySpends.delete(k);
    return early;
  }
  indexFunding(outpoint) {
    const k = outpointKey(outpoint);
    if (!this.funding.has(k)) this.funding.set(k, { spenders: this.takeEarly(k) });
    this.byOutpoint.set(k, { kind: "funding", cHex: "" });
  }
  indexToken(t) {
    if (this.joined.has(t.cHex)) return;
    const list = this.tokens.get(t.cHex);
    if (list === void 0) {
      this.tokens.set(t.cHex, [t]);
    } else {
      const k = outpointKey(t.outpoint);
      if (list.some((e) => outpointKey(e.outpoint) === k)) return;
      list.push(t);
    }
    this.byOutpoint.set(outpointKey(t.outpoint), { kind: "token", cHex: t.cHex });
  }
  /** Forget one token by its outpoint, and the commitment's list with it if that was the last. */
  dropToken(cHex, outpointK) {
    const list = this.tokens.get(cHex);
    if (list === void 0) return;
    const left = list.filter((e) => outpointKey(e.outpoint) !== outpointK);
    if (left.length === 0) this.tokens.delete(cHex);
    else this.tokens.set(cHex, left);
  }
  /**
   * The token for this commitment locked to this identity's profile key,
   * out of however many were minted over it. Undefined when none is, which
   * is a commitment whose real token has not arrived yet rather than a
   * commitment nobody may claim.
   */
  tokenFor(cHex, identity) {
    const list = this.tokens.get(cHex);
    if (list === void 0) return void 0;
    let profile;
    try {
      profile = profileLockingKey(identity).toString();
    } catch {
      return void 0;
    }
    return list.find((t) => t.lockingKeyHex === profile);
  }
  indexCarrier(c) {
    if (this.joined.has(c.cHex)) return;
    this.carriers.set(c.cHex, c);
    this.byOutpoint.set(outpointKey(c.outpoint), { kind: "carrier", cHex: c.cHex });
    this.carrierIdentity.set(c.outpoint.txid, c.identityKeyHex);
    const prev = Utils2.toHex(c.record.prev);
    let set = this.byPrev.get(prev);
    if (set === void 0) {
      set = /* @__PURE__ */ new Set();
      this.byPrev.set(prev, set);
    }
    set.add(c.cHex);
  }
  /**
   * Record the carrier as the spender of each output it spends, whether or
   * not that output was ever admitted here: a carrier names its own kill
   * switch. Returns false when one of them already has a spender that is
   * not this carrier, in which case the carrier was retracted before it
   * arrived and has been killed.
   *
   * An output another carrier already claims is one this carrier spends a
   * second time, so that carrier is killed first, exactly as the engine's
   * spend notification would have done had the output been in storage (it
   * arrives before the admission, so when it is in storage this branch is
   * never reached). Only the identity's own record key can spend a funding
   * output, and the engine's SPV step checks that signature before the
   * topic manager sees the carrier, so this is the owner's doing.
   */
  linkFunding(c) {
    for (const f of c.funding) {
      const held = this.funding.get(outpointKey(f))?.carrierTxid;
      if (held !== void 0 && held !== c.outpoint.txid) {
        this.dropCarrier(c);
        this.host.log("ls_finger dropped a second carrier on a claimed funding output", {
          c: c.cHex,
          funding: outpointKey(f),
          held
        });
        return false;
      }
    }
    let foreign;
    for (const f of c.funding) {
      const k = outpointKey(f);
      let e = this.funding.get(k);
      if (e === void 0) {
        e = { spenders: this.takeEarly(k) };
        this.funding.set(k, e);
      }
      e.carrierTxid = c.outpoint.txid;
      for (const s of e.spenders) if (s !== c.outpoint.txid) foreign ??= { outpoint: k, spendingTxid: s };
    }
    if (foreign === void 0) return true;
    this.kill(c.outpoint.txid, "funding-spent", foreign);
    return false;
  }
  /**
   * Retract a carrier and every state it is part of.
   *
   * The states to drop are the identity whose chain the carrier is in (the
   * record's own identity key, kept in `carrierIdentity` so a carrier deep
   * in the chain still names its identity) and any state whose current or
   * previous carrier it is: a rotation's successor holds the rotation record
   * as its own current state until it publishes. Every carrier and token of
   * a killed identity is dropped with it: the host keeps nothing of a
   * retracted record it would have to serve. A rotation's forward pointer
   * into a killed successor is removed, because it would name a state that
   * no longer exists; the rotated key stays refused by its spent token.
   * Counted once per identity, which is what the gauge counts too.
   */
  kill(carrierTxid, reason, extra) {
    const cHex = reverseHex(carrierTxid);
    const victims = /* @__PURE__ */ new Set();
    const owner = this.carrierIdentity.get(carrierTxid);
    if (owner !== void 0) victims.add(owner);
    for (const [identity, state] of this.states) {
      if (state.carrierC === cHex || state.prevCarrierC === cHex) victims.add(identity);
    }
    for (const identity of victims) {
      this.states.delete(identity);
      if (this.killed.has(identity)) continue;
      this.killed.add(identity);
      try {
        this.killedProfiles.set(profileLockingKey(identity).toString(), identity);
      } catch {
      }
      this.host.metrics.inc("finger_killed_total", { reason });
    }
    for (const state of this.states.values()) {
      if (state.successor !== void 0 && victims.has(state.successor)) delete state.successor;
    }
    for (const carrier of [...this.carriers.values()]) {
      if (carrier.cHex === cHex || victims.has(carrier.identityKeyHex)) this.dropCarrier(carrier);
    }
    this.host.log("ls_finger killed", { carrier: carrierTxid, reason, identities: [...victims], ...extra });
  }
  /** Forget a carrier, its token, and its claim on its funding outputs. */
  dropCarrier(carrier) {
    this.carriers.delete(carrier.cHex);
    this.joined.delete(carrier.cHex);
    this.byOutpoint.delete(outpointKey(carrier.outpoint));
    this.byPrev.get(Utils2.toHex(carrier.record.prev))?.delete(carrier.cHex);
    this.carrierIdentity.delete(carrier.outpoint.txid);
    for (const token of this.tokens.get(carrier.cHex) ?? []) {
      this.byOutpoint.delete(outpointKey(token.outpoint));
    }
    this.tokens.delete(carrier.cHex);
    for (const f of carrier.funding) {
      const k = outpointKey(f);
      if (this.funding.get(k)?.carrierTxid === carrier.outpoint.txid) this.funding.delete(k);
    }
  }
  refuseKilled(identity, cHex) {
    this.host.metrics.inc("finger_join_failures_total", { reason: "killed" });
    this.host.log("ls_finger join refused", { identity, c: cHex, reason: "killed", restoring: false });
  }
  /** The live join: both halves must be present. */
  join(cHex, restoring) {
    const carrier = this.carriers.get(cHex);
    if (carrier === void 0) return false;
    const token = this.tokenFor(cHex, carrier.identityKeyHex);
    if (token === void 0) {
      if ((this.tokens.get(cHex)?.length ?? 0) > 0) {
        this.host.metrics.inc("finger_join_failures_total", { reason: "bad-lock" });
        this.host.log("ls_finger join refused", {
          identity: carrier.identityKeyHex,
          c: cHex,
          reason: "bad-lock",
          restoring
        });
      }
      return false;
    }
    return this.advance(carrier, token, restoring);
  }
  /**
   * Apply one transition to its identity's state, or refuse it. Returns
   * whether it became current.
   *
   * `restoring` relaxes two things, both because a restore is fed unspent
   * outputs only. The spend link is rebuilt from spend notifications, which
   * storage does not replay, so a transition is accepted on the chain checks
   * alone (prev, witness, sequence). And a transition whose token was spent
   * by the next one has no token row at all, so it advances the chain from
   * its carrier alone, with no token outpoint; the token's lock was checked
   * live when it joined, before the engine ever marked it spent. Carriers are
   * never spent, so the chain of records is always whole in storage.
   */
  advance(carrier, token, restoring) {
    const cHex = carrier.cHex;
    if (this.joined.has(cHex)) return false;
    if (carrier.record.kind === KindSub || carrier.record.kind === KindManifest) return false;
    if (token === void 0 && !restoring) return false;
    const identity = carrier.identityKeyHex;
    const fail = (reason) => {
      if (this.rebuilding) return false;
      this.host.metrics.inc("finger_join_failures_total", { reason });
      this.host.log("ls_finger join refused", { identity, c: cHex, reason, restoring });
      if (reason === "no-current" && !restoring) this.scheduleRebuild();
      return false;
    };
    if (this.killed.has(identity)) return fail("killed");
    const record = carrier.record;
    try {
      validateRecord(record);
    } catch {
      return fail("bad-record");
    }
    if (token !== void 0) {
      let profile;
      try {
        profile = profileLockingKey(identity).toString();
      } catch {
        return fail("bad-lock");
      }
      if (profile !== token.lockingKeyHex) return fail("bad-lock");
    }
    const current = this.states.get(identity);
    let next;
    if (record.kind === KindCreate) {
      if (current !== void 0) return fail("duplicate-create");
      next = { tokenOutpoint: token?.outpoint, carrierC: cHex, seq: record.seq, wc: record.wc, terminal: false };
    } else {
      if (current === void 0) return fail("no-current");
      if (current.terminal) return fail("retired");
      if (current.successor !== void 0) return fail("rotated");
      if (Utils2.toHex(record.prev) !== current.carrierC) return fail("bad-prev");
      if (record.prevWitness === void 0 || Utils2.toHex(Hash.sha256(Array.from(record.prevWitness))) !== Utils2.toHex(current.wc)) {
        return fail("bad-witness");
      }
      if (record.seq !== current.seq + 1n) return fail("bad-seq");
      if (!restoring && token !== void 0 && current.tokenOutpoint !== void 0) {
        if (this.spentBy.get(outpointKey(current.tokenOutpoint)) !== token.outpoint.txid) return fail("not-spent");
      }
      next = {
        tokenOutpoint: token?.outpoint,
        carrierC: cHex,
        prevCarrierC: current.carrierC,
        seq: record.seq,
        wc: record.wc,
        terminal: record.kind === KindRetire
      };
      if (record.kind === KindRotate && record.successor !== void 0) {
        const successor = Utils2.toHex(record.successor);
        if (this.states.has(successor)) return fail("successor-taken");
        if (this.killed.has(successor)) {
          this.host.log("ls_finger rotation to a killed successor stands without it", { identity, c: cHex, successor });
        } else {
          next.successor = successor;
          this.states.set(successor, { ...next, successor: void 0 });
        }
      }
    }
    this.states.set(identity, next);
    this.joined.add(cHex);
    if (!restoring) for (const child of this.byPrev.get(cHex) ?? []) this.join(child, false);
    return true;
  }
  /**
   * Rebuild every index from the unspent outputs of the topic, in a
   * deterministic order: every funding output, carrier and token is indexed
   * first; then each carrier's funding outpoints are re-read from storage
   * and a carrier whose funding output was spent by any txid other than its
   * own is killed, exactly as a live spend notification would have; then
   * the carriers left are advanced by ascending sequence of their record
   * (ties by commitment), each with its token when the token row is
   * present, so a create always precedes its updates whatever order storage
   * returned the rows in. Spent tokens are not among the rows and are not
   * needed for the chain: a record's `prev` names the previous carrier, and
   * carriers are never spent.
   *
   * A carrier's funding outpoints come from its row's `outputsConsumed`,
   * which the engine fills with the previous coins the topic manager
   * retained at admission; a carrier admitted while its funding output was
   * not in storage has none, and then there is no spend for storage to have
   * seen either. A storage failure here propagates: a restore that skipped
   * the kill check would bring retracted records back, and the host's own
   * rule is to refuse to start rather than start wrong.
   */
  async restore(outputs, storage) {
    if (this.rebuildTimer !== void 0) clearTimeout(this.rebuildTimer);
    this.rebuildTimer = void 0;
    this.tokens.clear();
    this.carriers.clear();
    this.spentBy.clear();
    this.states.clear();
    this.joined.clear();
    this.byPrev.clear();
    this.byOutpoint.clear();
    this.funding.clear();
    this.carrierIdentity.clear();
    this.killed.clear();
    this.killedProfiles.clear();
    this.earlySpends.clear();
    let fundingRows = 0;
    const carrierRows = [];
    for (const o of outputs) {
      if (o.spent) continue;
      const outpoint = { txid: o.txid, outputIndex: o.outputIndex };
      let script;
      try {
        script = LockingScript.fromBinary(o.outputScript);
      } catch {
        continue;
      }
      if (decodeFunding2(script) !== void 0) {
        this.indexFunding(outpoint);
        fundingRows++;
        continue;
      }
      const token = decodeToken(script);
      if (token !== void 0) {
        if (token.valid) this.indexToken({ outpoint, lockingKeyHex: token.lockingKey.toString(), cHex: Utils2.toHex(token.c) });
        continue;
      }
      const entry = this.carrierFromScript(o.outputScript, outpoint, o.outputsConsumed.map((c) => ({ ...c })));
      if (entry === void 0) continue;
      this.indexCarrier(entry);
      this.linkFunding(entry);
      carrierRows.push({ entry, topic: o.topic });
    }
    for (const o of outputs) {
      if (o.beef === void 0) continue;
      try {
        this.noteInputs(Transaction.fromBEEF(o.beef, o.txid));
      } catch {
      }
    }
    const carrierTxids = new Set(carrierRows.map((r) => r.entry.outpoint.txid));
    const killers = new Set(outputs.filter((o) => !carrierTxids.has(o.txid)).map((o) => o.txid));
    for (const { entry, topic } of carrierRows) {
      if (!this.carriers.has(entry.cHex)) continue;
      for (const f of entry.funding) {
        const stored = await storage.findOutput(f.txid, f.outputIndex, topic);
        if (stored === null || !stored.spent) continue;
        const foreign = stored.consumedBy.find((c) => c.txid !== entry.outpoint.txid && killers.has(c.txid));
        if (foreign === void 0) continue;
        this.kill(entry.outpoint.txid, "funding-spent", { outpoint: outpointKey(f), spendingTxid: foreign.txid, restoring: true });
        break;
      }
    }
    for (const carrier of this.carriersInOrder()) this.advance(carrier, this.tokenFor(carrier.cHex, carrier.identityKeyHex), true);
    return this.tokenCount + this.carriers.size + fundingRows;
  }
  /** Every carrier by ascending record sequence, ties by commitment. */
  carriersInOrder() {
    return [...this.carriers.values()].sort((a, b) => {
      if (a.record.seq !== b.record.seq) return a.record.seq < b.record.seq ? -1 : 1;
      return a.cHex < b.cHex ? -1 : a.cHex > b.cHex ? 1 : 0;
    });
  }
  scheduleRebuild() {
    if (this.rebuildTimer !== void 0) clearTimeout(this.rebuildTimer);
    this.rebuildTimer = setTimeout(() => this.rebuildChains(), this.rebuildDelayMs);
    this.rebuildTimer.unref?.();
  }
  /**
   * Re-derive every identity's chain from what is already indexed, the way
   * restore does, without reading storage.
   *
   * A host that joins late (one that catches up from a peer, or is fed a
   * backlog by hand) receives unspent outputs only: every carrier, since
   * carriers are never spent, but only the CURRENT token of each identity,
   * because every earlier one was spent by its successor. Live, a transition
   * joins only with its token and only after the one before it, so the
   * create, whose token is spent, never joins, and everything after it is
   * refused `no-current` for good. Before this, only a restart repaired such
   * a host. The walk is restore's: carriers by ascending sequence, each with
   * its token when there is one, so the chain is checked by prev, witness
   * and sequence and the current state gets its token.
   *
   * It runs once after a burst of `no-current` refusals settles, and never
   * during a restore. Kills are kept: the killed sets are not touched, and
   * advance refuses a killed identity's carriers as it does live.
   */
  rebuildChains() {
    if (this.rebuildTimer !== void 0) clearTimeout(this.rebuildTimer);
    this.rebuildTimer = void 0;
    const before = this.joined.size;
    this.states.clear();
    this.joined.clear();
    const spenders = /* @__PURE__ */ new Set();
    for (const list of this.tokens.values()) {
      for (const t of list) {
        const by = this.spentBy.get(outpointKey(t.outpoint));
        if (by !== void 0) spenders.add(by);
      }
    }
    const taken = (c) => {
      const t = this.tokenFor(c.cHex, c.identityKeyHex);
      return t !== void 0 && spenders.has(t.outpoint.txid) ? 0 : 1;
    };
    const order2 = [...this.carriers.values()].sort((a, b) => {
      if (a.record.seq !== b.record.seq) return a.record.seq < b.record.seq ? -1 : 1;
      return taken(a) - taken(b) || (a.cHex < b.cHex ? -1 : a.cHex > b.cHex ? 1 : 0);
    });
    this.rebuilding = true;
    try {
      for (const carrier of order2) this.advance(carrier, this.tokenFor(carrier.cHex, carrier.identityKeyHex), true);
    } finally {
      this.rebuilding = false;
    }
    this.host.log("ls_finger rebuilt its chains from the index", { carriers: this.carriers.size, joinedBefore: before, joined: this.joined.size });
  }
  /**
   * Two questions:
   *
   *   { identityKey: "<66 hex>" }                 the current token, the
   *                                               current carrier, and the
   *                                               previous carrier if any
   *   { identityKey: "<66 hex>", pending: true }  the same, plus any token
   *                                               under that identity's
   *                                               profile key whose carrier
   *                                               has not arrived
   *
   * The previous carrier is always answered (spec section 7): a
   * reader checks the witness against it. The engine hydrates each outpoint
   * as BEEF; the record is inside the carrier's. A restored state whose
   * current token was spent by a refused transition answers without the
   * token, and the reader's NO-TOKEN refusal is the right outcome for it.
   * A killed identity answers nothing, pending included.
   */
  async lookup(question) {
    const q = question.query;
    if (typeof q?.identityKey !== "string" || !identityKeyShape.test(q.identityKey)) {
      throw new Error(
        'ls_finger: ask { identityKey: "<66 hex>" }, { identityKey, pending: true } or { identityKey, carrier: "<64 hex>" }'
      );
    }
    for (const member of Object.keys(q)) {
      if (member !== "identityKey" && member !== "pending" && member !== "carrier") {
        throw new Error(`ls_finger: unknown query member ${JSON.stringify(member)}`);
      }
    }
    const identity = q.identityKey.toLowerCase();
    const out = [];
    if (this.killed.has(identity)) return out;
    if (q.carrier !== void 0) {
      if (typeof q.carrier !== "string" || !carrierShape.test(q.carrier)) {
        throw new Error("ls_finger: carrier must be a 64-hex txid in display order");
      }
      const entry = this.carriers.get(reverseHex(q.carrier.toLowerCase()));
      if (entry !== void 0 && entry.identityKeyHex === identity) out.push({ ...entry.outpoint });
      return out;
    }
    const state = this.states.get(identity);
    if (state !== void 0) {
      if (state.tokenOutpoint !== void 0) out.push({ ...state.tokenOutpoint });
      const current = this.carriers.get(state.carrierC);
      if (current !== void 0) out.push({ ...current.outpoint });
      if (state.prevCarrierC !== void 0) {
        const prev = this.carriers.get(state.prevCarrierC);
        if (prev !== void 0) out.push({ ...prev.outpoint });
      }
    }
    if (q.pending === true) {
      let profile;
      try {
        profile = profileLockingKey(identity).toString();
      } catch {
        return out;
      }
      for (const [c, list] of this.tokens) {
        if (this.carriers.has(c)) continue;
        for (const t of list) if (t.lockingKeyHex === profile) out.push({ ...t.outpoint });
      }
    }
    return out;
  }
  async getDocumentation() {
    return [
      "# ls_finger",
      "",
      "Answers `{ identityKey }` with the current state token, the current",
      "carrier and the previous carrier, once a token and the carrier it",
      "commits to have both arrived and the chain checks pass. With",
      "`pending: true` it also lists tokens under that identity whose carrier",
      "has not arrived. A funding output spent by anything other than its",
      "carrier kills the carrier: every state it is part of is dropped and",
      "the identity answers nothing afterwards."
    ].join("\n");
  }
  async getMetaData() {
    return { name: "ls_finger", shortDescription: "Joins committed-record tokens and carriers; answers by identity key." };
  }
};
function isFinal(tx) {
  return tx.lockTime === 0 || tx.inputs.every((i) => (i.sequence ?? 4294967295) === 4294967295);
}

// src/tm_finger.ts
import { Transaction as Transaction2 } from "@bsv/sdk";
var RefuseReasons = ["not-pushdrop", "bad-tag", "bad-sig", "bad-record", "bad-lock", "mineable", "non-canonical-unlocking", "other"];
var AdmitKinds = ["token", "carrier", "funding", "spend"];
var FingerTopicManager = class {
  constructor(host) {
    this.host = host;
    for (const kind of AdmitKinds) host.metrics.preset("finger_admitted_total", { kind });
    for (const reason of RefuseReasons) host.metrics.preset("finger_refused_total", { reason });
  }
  host;
  async identifyAdmissibleOutputs(beef, previousCoins) {
    const coinsToRetain = [...previousCoins];
    const refuse = (reason2, txid2) => {
      this.host.metrics.inc("finger_refused_total", { reason: reason2 });
      this.host.log("tm_finger refused", { txid: txid2 ?? "?", reason: reason2 });
      return { outputsToAdmit: [], coinsToRetain };
    };
    let tx;
    try {
      tx = Transaction2.fromBEEF(beef);
    } catch {
      return refuse("other");
    }
    const txid = tx.id("hex");
    const admitted = /* @__PURE__ */ new Set();
    let tokens = 0;
    let funding = 0;
    let tokenReason;
    for (let i = 0; i < tx.outputs.length; i++) {
      const out = tx.outputs[i];
      if (out === void 0) continue;
      const insp = inspectToken(out.lockingScript);
      if (insp.kind === "token") {
        if (insp.token.valid) {
          admitted.add(i);
          tokens++;
        } else {
          tokenReason ??= "bad-sig";
        }
        continue;
      }
      if (insp.kind === "bad-tag") {
        tokenReason ??= "bad-tag";
        continue;
      }
      if (decodeFunding2(out.lockingScript) !== void 0) {
        admitted.add(i);
        funding++;
      }
    }
    let carrierReason;
    let carriers = 0;
    const carrier = decodeCarrier2(tx);
    if (typeof carrier === "string") {
      carrierReason = carrier;
    } else {
      admitted.add(carrier.outputIndex);
      carriers = 1;
    }
    if (admitted.size > 0) {
      if (tokens > 0) this.host.metrics.inc("finger_admitted_total", { kind: "token" }, tokens);
      if (carriers > 0) this.host.metrics.inc("finger_admitted_total", { kind: "carrier" }, carriers);
      if (funding > 0) this.host.metrics.inc("finger_admitted_total", { kind: "funding" }, funding);
      return { outputsToAdmit: [...admitted].sort((a, b) => a - b), coinsToRetain };
    }
    const nothingFingerShaped = tokenReason === void 0 && carrierReason === "not-pushdrop";
    if (nothingFingerShaped && previousCoins.length > 0) {
      this.host.metrics.inc("finger_admitted_total", { kind: "spend" });
      this.host.log("tm_finger accepted a spend of held outputs", { txid, inputs: previousCoins.length });
      return { outputsToAdmit: [], coinsToRetain };
    }
    const reason = carrierReason !== void 0 && carrierReason !== "not-pushdrop" ? carrierReason : tokenReason ?? carrierReason ?? "not-pushdrop";
    return refuse(reason, txid);
  }
  /** Nothing is needed: each object is admitted on its own validity. */
  async identifyNeededInputs() {
    return [];
  }
  async getDocumentation() {
    return [
      "# tm_finger",
      "",
      "Admits state tokens (PushDrop [tag, C] whose field signature verifies),",
      "carriers (an unmineable transaction with one record output whose",
      "record decodes, whose lock is the identity's record key and whose",
      'signature verifies), and funding outputs (PushDrop [ "bf" + 0x02 ]',
      "with no signature). Each object is admitted on its own validity; the",
      "join, the chain rules and the kill switch are ls_finger's. A",
      "transaction that admits nothing but spends held outputs (the sweep of",
      "a funding tree) is accepted for its inputs alone."
    ].join("\n");
  }
  async getMetaData() {
    return { name: "tm_finger", shortDescription: "Admits committed-record state tokens, carriers and funding outputs." };
  }
};

// src/index.ts
var bcommonVersion = true ? "0.33.13" : "unbundled";
function create(host) {
  host.log("finger module built with bcommon", { bcommon: bcommonVersion });
  const tm = new FingerTopicManager(host);
  const ls = new FingerLookupService(host);
  host.metrics.gauge("finger_pending_tokens", () => ls.pendingTokens);
  host.metrics.gauge("finger_indexed_tokens", () => ls.tokenCount);
  host.metrics.gauge("finger_indexed_carriers", () => ls.carrierCount);
  host.metrics.gauge("finger_identities", () => ls.identityCount);
  host.metrics.gauge("finger_killed_identities", () => ls.killedCount);
  return { topics: { tm_finger: tm }, lookups: { ls_finger: ls } };
}
export {
  FingerLookupService,
  FingerTopicManager,
  create as default
};
