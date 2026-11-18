// @lightwebinc/bbox-host 0.6.4: tm_bbox_<office> and ls_bbox, with @lightwebinc/bcommon 0.24.0 inlined; imports @bsv/sdk (built against 2.7.1)

// src/module.ts
import { isAbsolute } from "node:path";
import { Transaction as Transaction5, Utils as Utils4 } from "@bsv/sdk";
import { PrivateKey, ProtoWallet } from "@bsv/sdk";

// node_modules/@lightwebinc/bcommon/dist/host/accept.js
import { appendFileSync, mkdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { MerklePath as MerklePath2 } from "@bsv/sdk";

// node_modules/@lightwebinc/bcommon/dist/acceptance.js
var DefaultThresholdSats = 25e6;
var SatsPerCoin = 1e8;
function defaultAcceptancePolicy() {
  return {
    thresholdSats: DefaultThresholdSats,
    thresholdCents: 0,
    maxPriceAge: 36e5,
    window: 36e5,
    payerLimit: DefaultThresholdSats,
    totalLimit: 10 * DefaultThresholdSats
  };
}
async function paymentThreshold(p, now = Date.now()) {
  if (p.thresholdCents <= 0)
    return { sats: p.thresholdSats };
  if (p.price === void 0)
    return { sats: 0, reason: "price-unknown" };
  let pr;
  try {
    pr = await p.price();
  } catch {
    return { sats: 0, reason: "price-unknown" };
  }
  if (!Number.isSafeInteger(pr.centsPerCoin) || pr.centsPerCoin <= 0)
    return { sats: 0, reason: "price-unknown" };
  if (p.maxPriceAge > 0 && pr.at !== void 0 && now - pr.at > p.maxPriceAge)
    return { sats: 0, reason: "price-stale" };
  const sats = Number(BigInt(p.thresholdCents) * BigInt(SatsPerCoin) / BigInt(pr.centsPerCoin));
  const conv = Number.isSafeInteger(sats) ? sats : Number.MAX_SAFE_INTEGER;
  return { sats: p.thresholdSats > 0 && p.thresholdSats < conv ? p.thresholdSats : conv };
}
var PaymentExposure = class {
  charges = /* @__PURE__ */ new Map();
  flagged = /* @__PURE__ */ new Map();
  /** Charges r if the limits allow it, or names the one that does not. */
  reserve(p, payer, txid, sats, now = Date.now()) {
    if (this.flagged.has(payer))
      return "payer-flagged";
    if (this.charges.has(txid))
      return "already-charged";
    let mine = 0;
    let total = 0;
    for (const [id, c] of this.charges) {
      if (p.window > 0 && now - c.at > p.window) {
        this.charges.delete(id);
        continue;
      }
      total += c.sats;
      if (c.payer === payer)
        mine += c.sats;
    }
    if (mine + sats > p.payerLimit)
      return "payer-limit";
    if (total + sats > p.totalLimit)
      return "total-limit";
    this.charges.set(txid, { payer, sats, at: now });
    return void 0;
  }
  charge(payer, txid, sats, at) {
    this.charges.set(txid, { payer, sats, at });
  }
  release(txid) {
    this.charges.delete(txid);
  }
  flag(payer, why) {
    this.flagged.set(payer, why);
  }
  unflag(payer) {
    this.flagged.delete(payer);
  }
  isFlagged(payer) {
    return this.flagged.has(payer);
  }
  unmined(payer, window = 0, now = Date.now()) {
    let mine = 0;
    let total = 0;
    for (const c of this.charges.values()) {
      if (window > 0 && now - c.at > window)
        continue;
      total += c.sats;
      if (c.payer === payer)
        mine += c.sats;
    }
    return { payer: mine, total };
  }
};
async function decidePayment(p, x, r, now = Date.now()) {
  if (!Number.isSafeInteger(r.sats) || r.sats <= 0)
    return { decision: "refuse", reason: "pays-nothing", sats: 0, threshold: 0 };
  const th = await paymentThreshold(p, now);
  const hold = (reason) => ({ decision: "hold", reason, sats: r.sats, threshold: th.sats });
  if (th.reason !== void 0)
    return hold(th.reason);
  if (r.ask === "hold")
    return hold("payer-asked-hold");
  if (r.sats > th.sats)
    return hold("above-threshold");
  if (x === void 0)
    return hold("no-exposure-ledger");
  if (r.txid === "")
    return hold("no-txid");
  const why = x.reserve(p, r.payer, r.txid, r.sats, now);
  if (why !== void 0)
    return hold(why);
  return { decision: "fast", reason: "at-or-below-threshold", sats: r.sats, threshold: th.sats };
}
function arcadeVerdict(st) {
  const s = st.txStatus ?? "";
  if (s === "REJECTED")
    return "refused";
  if (s === "DOUBLE_SPEND_ATTEMPTED" || (st.competingTxs?.length ?? 0) > 0)
    return "conflict";
  switch (s) {
    case "ACCEPTED_BY_NETWORK":
    case "SEEN_ON_NETWORK":
    case "SEEN_MULTIPLE_NODES":
    case "STUMP_PROCESSING":
    case "MINED":
    case "IMMUTABLE":
      return "accepted";
  }
  return "pending";
}

// node_modules/@lightwebinc/bcommon/dist/woc.js
import { MerklePath, Transaction } from "@bsv/sdk";
var isTxid = (s) => typeof s === "string" && /^[0-9a-fA-F]{64}$/.test(s);
var timeout = 1e4;
var sleep = (ms) => new Promise((r) => setTimeout(r, ms).unref?.());
var MaxTxBytes = 8 << 20;
var WocFreeRate = 3;
function tscMerklePath(p, txid, height) {
  if (typeof p.txOrId !== "string" || p.txOrId.toLowerCase() !== txid.toLowerCase())
    throw new Error(`the tsc proof is for ${String(p.txOrId)}, not ${txid}`);
  if (!Array.isArray(p.nodes) || p.nodes.length > 64 || !Number.isSafeInteger(p.index) || p.index < 0)
    throw new Error("the tsc proof is malformed");
  if (p.nodes.length < 53 && p.index >= 2 ** p.nodes.length)
    throw new Error(`tsc index ${p.index} does not fit ${p.nodes.length} levels`);
  const path = [[{ offset: p.index, hash: txid.toLowerCase(), txid: true }]];
  p.nodes.forEach((n, level) => {
    const offset = Math.floor(p.index / 2 ** level) ^ 1;
    if (n !== "*" && !isTxid(n))
      throw new Error(`tsc node ${level} is not a hash`);
    const el = n === "*" ? { offset, duplicate: true } : { offset, hash: n.toLowerCase() };
    path[level] = [...path[level] ?? [], el].sort((a, b) => a.offset - b.offset);
  });
  return new MerklePath(height, path);
}
var WocHttp = class {
  key;
  rate;
  next = 0;
  base;
  constructor(network, key, base, rate2 = WocFreeRate) {
    this.key = key;
    this.rate = rate2;
    if (network !== "main" && network !== "test")
      throw new Error(`WhatsOnChain serves main or test, not ${String(network)}`);
    this.base = (base ?? `https://api.whatsonchain.com/v1/bsv/${network}`).replace(/\/+$/, "");
  }
  async get(p) {
    const gap = 1e3 / (this.rate > 0 ? this.rate : WocFreeRate);
    for (let attempt = 0; ; attempt++) {
      const now = Date.now();
      const slot = Math.max(now, this.next);
      this.next = slot + gap;
      if (slot > now)
        await sleep(slot - now);
      const headers = {};
      if (this.key !== void 0 && this.key !== "")
        headers.authorization = this.key;
      const res = await fetch(`${this.base}${p}`, { headers, signal: AbortSignal.timeout(timeout) });
      if (res.status === 429 && attempt < 3) {
        await sleep([200, 1e3, 3e3][attempt] ?? 3e3);
        continue;
      }
      return res;
    }
  }
  async spender(txid, vout) {
    if (!isTxid(txid) || !Number.isSafeInteger(vout) || vout < 0)
      throw new Error("not an outpoint");
    const res = await this.get(`/tx/${txid}/${vout}/spent`);
    if (res.status === 404)
      return "";
    if (res.status === 400)
      throw new Error(`WhatsOnChain does not know ${txid}.${vout}`);
    if (res.status !== 200)
      throw new Error(`WhatsOnChain answers ${res.status} for ${txid}.${vout}`);
    const s = await res.json();
    if (!isTxid(s?.txid))
      throw new Error("WhatsOnChain names a spender that is not a txid");
    return s.txid.toLowerCase();
  }
  /** GET /tx/{txid}/hex: the raw transaction, which must hash to txid (Go nodeapi.WoC.TxRaw); a 404 throws. */
  async raw(txid) {
    if (!isTxid(txid))
      throw new Error("not a txid");
    const res = await this.get(`/tx/${txid}/hex`);
    if (res.status === 404)
      throw new Error(`WhatsOnChain does not hold transaction ${txid}`);
    if (res.status !== 200)
      throw new Error(`WhatsOnChain answers ${res.status} for transaction ${txid}`);
    const text2 = (await res.text()).trim();
    if (text2.length > 2 * MaxTxBytes || !/^([0-9a-fA-F]{2})*$/.test(text2))
      throw new Error(`transaction ${txid}: the answer is not hex within its bound`);
    const tx = Transaction.fromHex(text2);
    if (tx.id("hex") !== txid.toLowerCase())
      throw new Error(`asked for transaction ${txid}, answered ${tx.id("hex")}`);
    return Uint8Array.from(tx.toBinary());
  }
  async proof(txid) {
    if (!isTxid(txid))
      throw new Error("not a txid");
    let failed;
    const res = await this.get(`/tx/${txid}/beef`);
    const body = await res.text();
    if (res.status === 422 || res.status === 404 || res.status === 500 && body.includes("No such mempool or blockchain transaction"))
      return void 0;
    if (res.status === 200) {
      try {
        const tx = Transaction.fromHexBEEF(body.trim());
        if (tx.id("hex") !== txid.toLowerCase())
          throw new Error(`the BEEF is transaction ${tx.id("hex")}`);
        if (tx.merklePath === void 0)
          return void 0;
        return held(tx.merklePath, txid);
      } catch (e) {
        failed = `beef: ${e.message}`;
      }
    } else {
      failed = `beef: ${res.status}`;
    }
    const tsc = await this.get(`/tx/${txid}/proof/tsc`);
    if (tsc.status !== 200)
      throw new Error(`WhatsOnChain proof of ${txid}: ${failed}; tsc: ${tsc.status}`);
    const answer = await tsc.json();
    if (answer === null)
      return void 0;
    const p = (Array.isArray(answer) ? answer : [answer]).find((x) => typeof x?.txOrId === "string" && x.txOrId.toLowerCase() === txid.toLowerCase());
    if (p === void 0 || !isTxid(p.target))
      throw new Error(`WhatsOnChain's tsc answer holds no proof of ${txid}`);
    const hdr = await this.get(`/block/${p.target}/header`);
    if (hdr.status !== 200)
      throw new Error(`block ${p.target}: ${hdr.status}`);
    const h = await hdr.json();
    if (h.hash?.toLowerCase() !== p.target.toLowerCase() || !Number.isSafeInteger(h.height))
      throw new Error(`block ${p.target}: the answer is another block`);
    return held(tscMerklePath(p, txid, h.height), txid);
  }
};
function held(mp, txid) {
  if (!(mp.path[0] ?? []).some((l) => l.hash === txid.toLowerCase()))
    throw new Error("the proof does not contain the txid");
  return mp;
}

// node_modules/@lightwebinc/bcommon/dist/host/accept.js
var BroadcastRefused = class extends Error {
};
var timeout2 = 1e4;
var ArcadeHttp = class {
  base;
  constructor(base) {
    this.base = base;
  }
  url(p) {
    return `${this.base.replace(/\/+$/, "")}${p}`;
  }
  async submit(ef) {
    for (let attempt = 0; ; attempt++) {
      const res = await fetch(this.url("/tx"), { method: "POST", headers: { "content-type": "application/octet-stream" }, body: Buffer.from(ef), signal: AbortSignal.timeout(timeout2) });
      const body = await res.text();
      if (res.status === 200 || res.status === 202)
        return JSON.parse(body);
      if (res.status === 503 && attempt < 3) {
        const s = Number(res.headers.get("retry-after"));
        await sleep2(s > 0 && s <= 10 ? s * 1e3 : 1e3);
        continue;
      }
      const why = `arcade answered ${res.status}: ${body.slice(0, 300)}`;
      if (res.status === 422 || res.status >= 460 && res.status <= 475)
        throw new BroadcastRefused(why);
      if (/already (known|in (the )?mempool)|txn-already-known/i.test(body) && !/spent/i.test(body))
        return { txStatus: "RECEIVED", extraInfo: "already known" };
      throw new Error(why);
    }
  }
  async status(txid) {
    const res = await fetch(this.url(`/tx/${txid}`), { headers: { accept: "application/json" }, signal: AbortSignal.timeout(timeout2) });
    if (res.status === 404)
      return void 0;
    if (res.status !== 200)
      throw new Error(`arcade status ${txid}: ${res.status}`);
    return await res.json();
  }
};
var isTxid2 = (s) => typeof s === "string" && /^[0-9a-fA-F]{64}$/.test(s);
var AssetHttp = class {
  base;
  constructor(base) {
    this.base = base;
  }
  url(p) {
    return `${this.base.replace(/\/+$/, "")}${p}`;
  }
  async spender(txid, vout) {
    const res = await fetch(this.url(`/api/v1/utxos/${txid}/json`), { headers: { accept: "application/json" }, signal: AbortSignal.timeout(timeout2) });
    if (res.status !== 200)
      throw new Error(`the node answers ${res.status} for ${txid}`);
    const outs = await res.json();
    if (!Array.isArray(outs))
      throw new Error("the node answers no list");
    let found;
    for (const o of outs) {
      if (o === null || typeof o.vout !== "number")
        throw new Error("an entry names no output");
      if (o.vout !== vout)
        continue;
      if (found !== void 0)
        throw new Error("the node names the output twice");
      if (o.txid !== void 0 && o.txid !== "" && o.txid.toLowerCase() !== txid.toLowerCase())
        throw new Error(`the answer is for ${o.txid}`);
      const st = (o.status ?? "").toUpperCase();
      if (st === "OK")
        found = "";
      else if (st === "SPENT" && isTxid2(o.spendingData?.txId))
        found = o.spendingData.txId.toLowerCase();
      else
        throw new Error(`the node answers status "${o.status}"`);
    }
    if (found === void 0)
      throw new Error(`the answer has no output ${vout}`);
    return found;
  }
  async proof(txid) {
    const res = await fetch(this.url(`/api/v1/merkle_proof/${txid}`), { signal: AbortSignal.timeout(timeout2) });
    if (res.status === 404)
      return void 0;
    if (res.status !== 200)
      throw new Error(`merkle_proof ${txid}: ${res.status}`);
    const raw = new Uint8Array(await res.arrayBuffer());
    if (raw.length > 1 << 20)
      throw new Error("merkle_proof: too large");
    const mp = MerklePath2.fromBinary(Array.from(raw));
    if (!(mp.path[0] ?? []).some((l) => l.hash === txid))
      throw new Error("the proof does not contain the txid");
    return mp;
  }
};
var sleep2 = (ms) => new Promise((r) => setTimeout(r, ms).unref?.());
var PaymentLabels = [
  ["requested", "no-payment"],
  ["fast", "at-or-below-threshold"],
  ["fast", "mined"],
  ["hold", "above-threshold"],
  ["hold", "payer-limit"],
  ["hold", "total-limit"],
  ["hold", "payer-flagged"],
  ["hold", "no-broadcast-leg"],
  ["hold", "broadcast-unconfirmed"],
  ["hold", "no-network-verdict"],
  ["hold", "double-spend-attempted"],
  ["hold", "spend-view-unknown"],
  ["hold", "already-charged"],
  ["hold", "payer-asked-hold"],
  ["hold", "price-unknown"],
  ["hold", "price-stale"],
  ["hold", "no-txid"],
  ["hold", "watch-limit"],
  ["refuse", "pays-nothing"],
  ["refuse", "malformed"],
  ["refuse", "wrong-script"],
  ["refuse", "spv-failed"],
  ["refuse", "not-final"],
  ["refuse", "outputs-exceed-inputs"],
  ["refuse", "network-refused"],
  ["refuse", "double-spent"],
  ["refuse", "replayed"],
  ["refuse", "conflict"]
];
var EventKinds = ["confirmed", "double-spent", "refused", "unmined"];
function isFinal(tx) {
  return tx.lockTime === 0 || tx.inputs.every((i) => (i.sequence ?? 4294967295) === 4294967295);
}
function overspends(tx) {
  let inSats = 0;
  for (const [n, i] of tx.inputs.entries()) {
    const src = i.sourceTransaction?.outputs[i.sourceOutputIndex];
    if (src?.satoshis === void 0)
      return `input ${n} carries no source transaction`;
    inSats += src.satoshis;
  }
  const outSats = tx.outputs.reduce((a, o) => a + (o.satoshis ?? 0), 0);
  return outSats > inSats ? `outputs ${outSats} sat, inputs ${inSats} sat` : void 0;
}
function unminedAncestors(tx) {
  const out = [];
  const seen = /* @__PURE__ */ new Set();
  const walk = (t) => {
    for (const i of t.inputs) {
      const s = i.sourceTransaction;
      if (s === void 0 || s.merklePath !== void 0 || seen.has(s))
        continue;
      seen.add(s);
      walk(s);
      out.push(s);
    }
  };
  walk(tx);
  return out;
}
var PaymentGate = class {
  o;
  exposure = new PaymentExposure();
  watched = /* @__PURE__ */ new Map();
  now;
  constructor(o) {
    this.o = o;
    this.now = o.now ?? Date.now;
    for (const [decision, reason] of PaymentLabels)
      o.host.metrics.preset(`${o.app}_payments_total`, { decision, reason });
    for (const kind of EventKinds)
      o.host.metrics.preset(`${o.app}_payment_events_total`, { kind });
    o.host.metrics.gauge(`${o.app}_payments_unmined`, () => this.watched.size);
    this.reflag();
  }
  /** Flags again every payer unsettleable.jsonl names: a flag outlives a restart. */
  reflag() {
    if (this.o.stateDir === void 0)
      return;
    let text2 = "";
    try {
      text2 = readFileSync(join(this.o.stateDir, "unsettleable.jsonl"), "utf8");
    } catch {
      return;
    }
    for (const l of text2.split("\n")) {
      try {
        const v = JSON.parse(l);
        if (typeof v.payer === "string" && typeof v.kind === "string" && typeof v.txid === "string")
          this.exposure.flag(v.payer, `${v.kind}: ${v.txid}`);
      } catch {
      }
    }
  }
  /** Releases and stops watching a payment the route did not take after all. */
  forget(txid) {
    this.exposure.release(txid);
    this.watched.delete(txid);
  }
  /** True when the host can take a payment on the fast path at all. */
  get networked() {
    return this.o.arcade !== void 0 && this.o.node !== void 0;
  }
  count(decision, reason) {
    this.o.host.metrics.inc(`${this.o.app}_payments_total`, { decision, reason });
  }
  /** Decides tx, which pays sats and passed the route's own checks, for payer. */
  async accept(tx, payer, sats) {
    const v = await this.decide(tx, payer, sats);
    this.count(v.decision, v.reason);
    return v;
  }
  async decide(tx, payer, sats) {
    const txid = tx.id("hex");
    if (tx.merklePath !== void 0)
      return { decision: "fast", reason: "mined" };
    if (!isFinal(tx))
      return { decision: "refuse", reason: "not-final", detail: "the payment can be replaced before it mines" };
    const over = overspends(tx);
    if (over !== void 0)
      return { decision: "refuse", reason: "outputs-exceed-inputs", detail: over };
    const { arcade, node } = this.o;
    if (arcade === void 0 || node === void 0) {
      if (arcade !== void 0) {
        try {
          for (const a of unminedAncestors(tx))
            await arcade.submit(Uint8Array.from(a.toEF())).catch(() => void 0);
          const st2 = await arcade.submit(Uint8Array.from(tx.toEF()));
          if (arcadeVerdict(st2) === "refused")
            return { decision: "refuse", reason: "network-refused", detail: `${st2.txStatus} ${st2.extraInfo ?? ""}`.trim() };
        } catch (e) {
          if (e instanceof BroadcastRefused)
            return { decision: "refuse", reason: "network-refused", detail: e.message };
        }
      }
      if (await this.mined(tx))
        return { decision: "fast", reason: "mined" };
      return { decision: "hold", reason: "no-broadcast-leg", detail: "this host has no arcade or no node to take a payment fast" };
    }
    if (this.watched.size >= (this.o.maxWatched ?? 1e5) && !await this.mined(tx))
      return { decision: "hold", reason: "watch-limit", detail: "too many fast payments are waiting to mine" };
    const d = await decidePayment(this.o.policy, this.exposure, { payer, txid, sats }, this.now());
    if (d.decision === "refuse")
      return { decision: "refuse", reason: d.reason };
    const fast = d.decision === "fast";
    const settle = async (v2) => {
      if (fast && v2.decision !== "fast")
        this.exposure.release(txid);
      if (v2.decision === "hold" && await this.mined(tx))
        return { decision: "fast", reason: "mined" };
      return v2;
    };
    let st;
    try {
      for (const a of unminedAncestors(tx))
        await arcade.submit(Uint8Array.from(a.toEF())).catch(() => void 0);
      st = await arcade.submit(Uint8Array.from(tx.toEF()));
    } catch (e) {
      if (e instanceof BroadcastRefused)
        return await settle({ decision: "refuse", reason: "network-refused", detail: e.message });
      return await settle({ decision: "hold", reason: "broadcast-unconfirmed", detail: String(e) });
    }
    if (st.txid !== void 0 && st.txid !== "" && st.txid !== txid)
      return await settle({ decision: "hold", reason: "broadcast-unconfirmed", detail: `arcade acknowledged ${st.txid}` });
    const first = arcadeVerdict(st);
    if (first === "refused")
      return await settle({ decision: "refuse", reason: "network-refused", detail: `${st.txStatus} ${st.extraInfo ?? ""}`.trim() });
    if (first === "conflict")
      return await settle({ decision: "hold", reason: "double-spend-attempted", detail: (st.competingTxs ?? []).join(",") });
    if (!fast)
      return await settle({ decision: "hold", reason: d.reason });
    const v = await this.evidence(tx, txid, first);
    if (v.decision !== "fast")
      return await settle(v);
    this.watched.set(txid, { tx, payer, sats, since: this.now() });
    return { decision: "fast", reason: "at-or-below-threshold" };
  }
  /** arcade's verdict and the node's spend view, until both agree or the wait ends. */
  async evidence(tx, txid, first) {
    const wait = this.o.waitMs ?? 1e4;
    const watch = this.o.watchMs ?? 0;
    const poll = this.o.pollMs ?? 500;
    const start = this.now();
    let verdict = first;
    for (; ; ) {
      const elapsed = this.now() - start;
      if (verdict === "refused")
        return { decision: "refuse", reason: "network-refused" };
      if (verdict === "conflict")
        return { decision: "hold", reason: "double-spend-attempted" };
      if (verdict === "accepted") {
        const spends = await this.spends(tx, txid);
        if (spends !== void 0)
          return spends;
        if (elapsed >= watch)
          return { decision: "fast", reason: "at-or-below-threshold" };
      }
      if (elapsed >= Math.max(wait, watch))
        return { decision: "hold", reason: "no-network-verdict", detail: `arcade had not answered it accepted after ${wait} ms` };
      await sleep2(poll);
      try {
        const st = await this.o.arcade.status(txid);
        verdict = st === void 0 ? "pending" : arcadeVerdict(st);
      } catch {
        verdict = "pending";
      }
    }
  }
  /** undefined when every input is unspent or spent by txid; a hold or a refusal otherwise. */
  async spends(tx, txid) {
    for (const [n, i] of tx.inputs.entries()) {
      const src = i.sourceTXID ?? i.sourceTransaction?.id("hex");
      if (src === void 0)
        return { decision: "hold", reason: "spend-view-unknown", detail: `input ${n} names no source` };
      let by;
      try {
        by = await this.o.node.spender(src, i.sourceOutputIndex);
      } catch (e) {
        return { decision: "hold", reason: "spend-view-unknown", detail: String(e) };
      }
      if (by !== "" && by !== txid)
        return { decision: "refuse", reason: "double-spent", detail: `input ${n} (${src}.${i.sourceOutputIndex}) is spent by ${by}` };
    }
    return void 0;
  }
  /** True when the node serves a proof of tx that verifies against the host's headers. */
  async mined(tx) {
    if (this.o.node === void 0)
      return false;
    const txid = tx.id("hex");
    try {
      const mp = await this.o.node.proof(txid);
      return mp !== void 0 && await mp.verify(txid, this.o.headers);
    } catch (e) {
      this.o.host.log(`${this.o.app} proof read failed`, { txid, err: String(e) });
      return false;
    }
  }
  /** Watches a fast payment taken before a restart, charged as it was. */
  restore(tx, payer, sats, at) {
    const txid = tx.id("hex");
    const age = this.now() - at;
    if (age > (this.o.maxAgeMs ?? 864e5))
      return;
    const window = this.o.policy.window;
    if (window <= 0 || age <= window)
      this.exposure.charge(payer, txid, sats, at);
    this.watched.set(txid, { tx, payer, sats, since: at });
  }
  /** The txids being watched. */
  get watching() {
    return [...this.watched.keys()].sort();
  }
  /** One pass over the fast payments not yet mined. */
  async sweep() {
    const events = [];
    const maxAge = this.o.maxAgeMs ?? 864e5;
    const batch = [...this.watched].slice(0, this.o.sweepBatch ?? 500);
    for (const [txid, w] of batch) {
      this.watched.delete(txid);
      this.watched.set(txid, w);
    }
    let next = 0;
    const worker = async () => {
      while (next < batch.length) {
        const [txid, w] = batch[next++];
        const ev = await this.check(txid, w, maxAge).catch(() => void 0);
        if (ev === void 0 || !this.watched.has(txid))
          continue;
        this.watched.delete(txid);
        if (ev.kind === "confirmed")
          this.exposure.release(txid);
        else
          this.exposure.flag(w.payer, `${ev.kind}: ${txid}`);
        events.push(ev);
      }
    };
    await Promise.all(Array.from({ length: Math.min(8, batch.length) }, worker));
    events.sort((a, b) => a.txid < b.txid ? -1 : 1);
    for (const ev of events)
      this.record(ev);
    return events;
  }
  async check(txid, w, maxAge) {
    const ev = { txid, payer: w.payer, sats: w.sats };
    if (await this.mined(w.tx))
      return { ...ev, kind: "confirmed" };
    if (this.o.node !== void 0) {
      for (const [n, i] of w.tx.inputs.entries()) {
        const src = i.sourceTXID ?? i.sourceTransaction?.id("hex");
        if (src === void 0)
          continue;
        const by = await this.o.node.spender(src, i.sourceOutputIndex).catch(() => "");
        if (by !== "" && by !== txid)
          return { ...ev, kind: "double-spent", why: `input ${n} (${src}.${i.sourceOutputIndex}) is spent by ${by}` };
      }
    }
    const st = await this.o.arcade?.status(txid).catch(() => void 0);
    if (st?.txStatus === "REJECTED")
      return { ...ev, kind: "refused", why: `${st.txStatus} ${st.extraInfo ?? ""}`.trim() };
    if (maxAge > 0 && this.now() - w.since > maxAge)
      return { ...ev, kind: "unmined", why: `not mined within ${Math.round(maxAge / 1e3)} s` };
    return void 0;
  }
  record(ev) {
    this.o.host.metrics.inc(`${this.o.app}_payment_events_total`, { kind: ev.kind });
    if (ev.kind === "confirmed")
      return;
    this.o.host.log(`${this.o.app} fast payment lost; its payer is held for confirmation`, { ...ev });
    if (this.o.stateDir === void 0)
      return;
    try {
      mkdirSync(this.o.stateDir, { recursive: true });
      appendFileSync(join(this.o.stateDir, "unsettleable.jsonl"), JSON.stringify({ ...ev, at: Math.floor(this.now() / 1e3) }) + "\n");
    } catch (e) {
      this.o.host.log(`${this.o.app} unsettleable record failed`, { err: String(e) });
    }
  }
};
function parseDuration(name, raw) {
  const m = /^([0-9]+)(ms|s|m|h)$/.exec(raw.trim());
  if (m === null || !Number.isSafeInteger(Number(m[1])))
    throw new Error(`${name}: "${raw}" is not a duration like 2s, 10m or 1h`);
  return Number(m[1]) * { ms: 1, s: 1e3, m: 6e4, h: 36e5 }[m[2]];
}
function parseAcceptConfig(prefix, env) {
  const policy = defaultAcceptancePolicy();
  const sats = (key, def) => {
    const raw = env[`${prefix}_${key}`]?.trim();
    if (raw === void 0 || raw === "")
      return def;
    if (!/^(0|[1-9][0-9]*)$/.test(raw) || !Number.isSafeInteger(Number(raw)))
      throw new Error(`${prefix}_${key}: "${raw}" is not a whole number of satoshis`);
    return Number(raw);
  };
  const dur = (key, def) => {
    const raw = env[`${prefix}_${key}`]?.trim();
    return raw === void 0 || raw === "" ? def : parseDuration(`${prefix}_${key}`, raw);
  };
  policy.thresholdSats = sats("ACCEPT_THRESHOLD_SATS", policy.thresholdSats);
  policy.payerLimit = sats("ACCEPT_PAYER_LIMIT", policy.thresholdSats);
  policy.totalLimit = sats("ACCEPT_TOTAL_LIMIT", 10 * policy.thresholdSats);
  policy.window = dur("ACCEPT_WINDOW", policy.window);
  const url = (key) => {
    const raw = env[`${prefix}_${key}`]?.trim();
    if (raw === void 0 || raw === "")
      return void 0;
    if (!/^https?:\/\/[^\s]+$/.test(raw))
      throw new Error(`${prefix}_${key}: "${raw}" is not an http(s) URL`);
    return raw;
  };
  const c = { arcadeURL: url("ARCADE_URL"), assetURL: url("ASSET_URL"), policy, watchMs: dur("ACCEPT_WATCH", 0) };
  const chain = env[`${prefix}_CHAIN`]?.trim();
  if (chain !== void 0 && chain !== "") {
    if (chain !== "woc:main" && chain !== "woc:test")
      throw new Error(`${prefix}_CHAIN: "${chain}" is woc:main or woc:test`);
    if (c.assetURL !== void 0)
      throw new Error(`${prefix}_CHAIN and ${prefix}_ASSET_URL are both set; keep one`);
    c.chain = chain === "woc:main" ? "main" : "test";
    const key = env[`${prefix}_WOC_KEY`]?.trim();
    if (key !== void 0 && key !== "")
      c.wocKey = key;
  }
  return c;
}
function gateFor(app, host, headers, c, stateDir) {
  return new PaymentGate({
    app,
    host,
    headers,
    policy: c.policy,
    arcade: c.arcadeURL === void 0 ? void 0 : new ArcadeHttp(c.arcadeURL),
    node: c.assetURL !== void 0 ? new AssetHttp(c.assetURL) : c.chain !== void 0 ? new WocHttp(c.chain, c.wocKey) : void 0,
    watchMs: c.watchMs,
    stateDir
  });
}

// node_modules/@lightwebinc/bcommon/dist/host/budget.js
import { isIPv6 } from "node:net";
var DefaultBudget = { perSec: 4, burst: 8, perAddressPerSec: 1, addressBurst: 4 };
var MaxAddresses = 1e4;
var Bucket = class {
  tokens;
  at;
  constructor(tokens, at) {
    this.tokens = tokens;
    this.at = at;
  }
  refill(rate2, burst, now) {
    if (now > this.at)
      this.tokens = Math.min(burst, this.tokens + (now - this.at) / 1e3 * rate2);
    this.at = now;
  }
  /** Seconds until one token is there, at least 1. */
  wait(rate2) {
    return Math.max(1, Math.ceil((1 - this.tokens) / rate2));
  }
};
function addressKey(address) {
  const a = address.trim().toLowerCase();
  const mapped = /^::ffff:([0-9]+\.[0-9]+\.[0-9]+\.[0-9]+)$/.exec(a);
  if (mapped !== null)
    return mapped[1];
  const bare = a.replace(/%.*$/, "");
  if (!isIPv6(bare))
    return a;
  const [head2, tail] = bare.includes("::") ? bare.split("::") : [bare, void 0];
  const left = head2 === "" ? [] : head2.split(":");
  const right = tail === void 0 || tail === "" ? [] : tail.split(":");
  const groups = tail === void 0 ? left : [...left, ...Array(8 - left.length - right.length).fill("0"), ...right];
  return `${groups.slice(0, 4).map((g) => g.replace(/^0+(?=.)/, "")).join(":")}::/64`;
}
var HandshakeBudget = class {
  c;
  now;
  global;
  /** By address key, least recently seen first. */
  byAddress = /* @__PURE__ */ new Map();
  constructor(c, now = Date.now) {
    this.c = c;
    this.now = now;
    for (const [k, v] of Object.entries(c)) {
      if (!(Number.isFinite(v) && v > 0))
        throw new Error(`HandshakeBudget: ${k} must be positive`);
    }
    if (c.burst < 1 || c.addressBurst < 1)
      throw new Error("HandshakeBudget: a burst must be at least 1");
    this.global = new Bucket(c.burst, now());
  }
  /** Addresses with a bucket. */
  get addresses() {
    return this.byAddress.size;
  }
  /** Takes one handshake for address from both buckets, or says which refused and how long to wait. */
  take(address) {
    const now = this.now();
    const key = addressKey(address);
    let a = this.byAddress.get(key);
    if (a === void 0)
      a = new Bucket(this.c.addressBurst, now);
    else
      this.byAddress.delete(key);
    this.prune(now);
    this.byAddress.set(key, a);
    a.refill(this.c.perAddressPerSec, this.c.addressBurst, now);
    if (a.tokens < 1)
      return { ok: false, limit: "address", retryAfter: a.wait(this.c.perAddressPerSec) };
    this.global.refill(this.c.perSec, this.c.burst, now);
    if (this.global.tokens < 1)
      return { ok: false, limit: "global", retryAfter: this.global.wait(this.c.perSec) };
    a.tokens -= 1;
    this.global.tokens -= 1;
    return { ok: true };
  }
  /** Forgets addresses whose buckets refilled, oldest first, and any past the cap, leaving room for one. */
  prune(now) {
    for (const [key, b] of this.byAddress) {
      if (this.byAddress.size < MaxAddresses) {
        b.refill(this.c.perAddressPerSec, this.c.addressBurst, now);
        if (b.tokens < this.c.addressBurst)
          break;
      }
      this.byAddress.delete(key);
    }
  }
};
var DefaultResponseBudget = { perSec: 5, burst: 20 };
var KeyedBudget = class {
  c;
  max;
  now;
  byKey = /* @__PURE__ */ new Map();
  constructor(c, max = MaxAddresses, now = Date.now) {
    this.c = c;
    this.max = max;
    this.now = now;
    if (!(Number.isFinite(c.perSec) && c.perSec > 0))
      throw new Error("KeyedBudget: perSec must be positive");
    if (!(Number.isFinite(c.burst) && c.burst >= 1))
      throw new Error("KeyedBudget: burst must be at least 1");
    if (!Number.isSafeInteger(max) || max < 1)
      throw new Error("KeyedBudget: max must be at least 1");
  }
  get size() {
    return this.byKey.size;
  }
  /** Takes one from key's bucket, or says how long until one is there. */
  take(key) {
    const now = this.now();
    const b = this.byKey.get(key) ?? new Bucket(this.c.burst, now);
    this.byKey.delete(key);
    this.byKey.set(key, b);
    while (this.byKey.size > this.max)
      this.byKey.delete(this.byKey.keys().next().value);
    b.refill(this.c.perSec, this.c.burst, now);
    if (b.tokens < 1)
      return { ok: false, retryAfter: b.wait(this.c.perSec) };
    b.tokens -= 1;
    return { ok: true };
  }
};
var MaxSeenRequests = 65536;
var SeenRequests = class {
  max;
  seen = /* @__PURE__ */ new Set();
  constructor(max = MaxSeenRequests) {
    this.max = max;
    if (!Number.isSafeInteger(max) || max < 1)
      throw new Error("SeenRequests: max must be at least 1");
  }
  get size() {
    return this.seen.size;
  }
  has(key) {
    return this.seen.has(key);
  }
  add(key) {
    this.seen.add(key);
    while (this.seen.size > this.max)
      this.seen.delete(this.seen.values().next().value);
  }
};

// node_modules/@lightwebinc/bcommon/dist/host/front.js
import { createServer } from "node:http";
import { Hash, Peer, Transaction as Transaction3, Utils as Utils2, createNonce, normalizeBRC100ByteFields, stringifyBRC100, verifyNonce } from "@bsv/sdk";

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
        let text2;
        try {
          text2 = utf8Decoder.decode(s);
        } catch {
          throw new CborError("utf8");
        }
        this.i += len;
        return text2;
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

// node_modules/@lightwebinc/bcommon/dist/host/ledger.js
import { closeSync, fdatasyncSync, mkdirSync as mkdirSync2, openSync, readFileSync as readFileSync2, writeSync } from "node:fs";
import { join as join2 } from "node:path";
import { Transaction as Transaction2, Utils } from "@bsv/sdk";
var LedgerFile = "payments.jsonl";
function spentOutpoints(tx) {
  return tx.inputs.map((i) => `${i.sourceTXID ?? i.sourceTransaction?.id("hex") ?? ""}.${i.sourceOutputIndex}`);
}
function ledgerLine(p, at, layout = "payee") {
  if (layout === "spread")
    return JSON.stringify({ ...p, beef: Utils.toBase64(Array.from(Uint8Array.from(p.beef))), at }) + "\n";
  const line = {
    txid: p.txid,
    beef: Utils.toBase64(Array.from(Uint8Array.from(p.beef))),
    outputIndex: p.outputIndex,
    satoshis: p.satoshis,
    derivationPrefix: p.derivationPrefix,
    derivationSuffix: p.derivationSuffix,
    senderIdentityKey: p.senderIdentityKey,
    class: p.class,
    inputs: p.inputs,
    at,
    // What the host decided; a line without it is the payee's own line, byte for byte.
    ...p.decision === void 0 ? {} : { decision: p.decision, reason: p.reason }
  };
  return JSON.stringify(line) + "\n";
}
function readLedgerLine(l) {
  let v;
  try {
    v = JSON.parse(l);
  } catch {
    return void 0;
  }
  if (typeof v?.txid !== "string")
    return void 0;
  let inputs = [];
  if (Array.isArray(v.inputs) && v.inputs.every((i) => typeof i === "string")) {
    inputs = v.inputs;
  } else if (typeof v.beef === "string") {
    try {
      inputs = spentOutpoints(Transaction2.fromAtomicBEEF(Utils.toArray(v.beef, "base64")));
    } catch {
    }
  }
  const out = { txid: v.txid, inputs };
  if (v.decision === "fast" || v.decision === "hold" || v.decision === "mined")
    out.decision = v.decision;
  if (typeof v.beef === "string")
    out.beef = v.beef;
  if (typeof v.senderIdentityKey === "string")
    out.senderIdentityKey = v.senderIdentityKey;
  if (typeof v.satoshis === "number")
    out.satoshis = v.satoshis;
  if (typeof v.at === "number")
    out.at = v.at;
  return out;
}
var TakenPayments = class {
  answered = /* @__PURE__ */ new Set();
  held = /* @__PURE__ */ new Set();
  coins = /* @__PURE__ */ new Map();
  /** Why p cannot be taken, or undefined. */
  refusal(p) {
    if (this.answered.has(p.txid))
      return "replayed";
    if (p.inputs.length === 0 || p.inputs.some((i) => (this.coins.get(i) ?? p.txid) !== p.txid))
      return "conflict";
    return void 0;
  }
  take(p) {
    if (p.decision === "hold") {
      this.held.add(p.txid);
    } else {
      this.held.delete(p.txid);
      this.answered.add(p.txid);
    }
    for (const i of p.inputs)
      this.coins.set(i, p.txid);
  }
  isHeld(txid) {
    return this.held.has(txid);
  }
};
var LedgerReceiver = class {
  path;
  taken = new TakenPayments();
  fd;
  now;
  layout;
  /** The fast lines read at start, newest last: watched again until they mine. */
  fast = [];
  constructor(dir, o = {}) {
    this.now = o.now ?? Date.now;
    this.layout = o.layout ?? "payee";
    mkdirSync2(dir, { recursive: true });
    this.path = join2(dir, LedgerFile);
    let text2 = "";
    try {
      text2 = readFileSync2(this.path, "utf8");
    } catch (e) {
      if (e.code !== "ENOENT")
        throw e;
    }
    for (const l of text2.split("\n")) {
      const v = readLedgerLine(l);
      if (v === void 0)
        continue;
      this.taken.take({ txid: v.txid, inputs: v.inputs, decision: v.decision === "hold" ? "hold" : void 0 });
      if (v.decision === "fast" && v.beef !== void 0 && v.senderIdentityKey !== void 0 && v.satoshis !== void 0 && v.at !== void 0) {
        this.fast.push({ beef: v.beef, senderIdentityKey: v.senderIdentityKey, satoshis: v.satoshis, at: v.at * 1e3 });
      }
    }
    if (text2.length > 0 && !text2.endsWith("\n"))
      this.write("\n");
  }
  write(s) {
    this.fd ??= openSync(this.path, "a");
    writeSync(this.fd, s);
    fdatasyncSync(this.fd);
  }
  refusal(p) {
    return this.taken.refusal(p);
  }
  claim(p) {
    const no = this.taken.refusal(p);
    if (no !== void 0)
      return no;
    const again = this.taken.isHeld(p.txid) && p.decision === "hold";
    this.taken.take(p);
    if (again)
      return "accepted";
    this.write(ledgerLine(p, Math.floor(this.now() / 1e3), this.layout));
    return "accepted";
  }
  close() {
    if (this.fd !== void 0)
      closeSync(this.fd);
    this.fd = void 0;
  }
};

// node_modules/@lightwebinc/bcommon/dist/host/sessions.js
var DefaultMaxSessions = 1e4;
var DefaultSessionTTL = 600;
var BoundedSessions = class {
  max;
  ttlMs;
  now;
  evicted;
  /** By session nonce, least recently used first. */
  byNonce = /* @__PURE__ */ new Map();
  byKey = /* @__PURE__ */ new Map();
  constructor(max, ttlMs, now = Date.now, evicted = () => {
  }) {
    this.max = max;
    this.ttlMs = ttlMs;
    this.now = now;
    this.evicted = evicted;
    if (!Number.isSafeInteger(max) || max < 1)
      throw new Error("BoundedSessions: max must be at least 1");
    if (!(ttlMs > 0))
      throw new Error("BoundedSessions: ttlMs must be positive");
  }
  get size() {
    return this.byNonce.size;
  }
  addSession(session) {
    const nonce = session.sessionNonce;
    if (typeof nonce !== "string")
      throw new TypeError("Invalid session: sessionNonce is required to add a session.");
    this.forget(nonce);
    this.byNonce.set(nonce, { s: session, at: this.now() });
    if (typeof session.peerIdentityKey === "string") {
      let set = this.byKey.get(session.peerIdentityKey);
      if (set === void 0)
        this.byKey.set(session.peerIdentityKey, set = /* @__PURE__ */ new Set());
      set.add(nonce);
    }
    this.prune();
  }
  updateSession(session) {
    this.addSession(session);
  }
  getSession(identifier) {
    this.expire();
    const direct = this.byNonce.get(identifier);
    if (direct !== void 0)
      return this.touch(identifier, direct.s);
    let best;
    for (const nonce of this.byKey.get(identifier) ?? []) {
      const e = this.byNonce.get(nonce);
      if (e !== void 0 && (best === void 0 || (e.s.lastUpdate ?? 0) > (best.lastUpdate ?? 0)))
        best = e.s;
    }
    return best === void 0 ? void 0 : this.touch(best.sessionNonce, best);
  }
  removeSession(session) {
    if (typeof session.sessionNonce === "string")
      this.forget(session.sessionNonce);
  }
  hasSession(identifier) {
    return this.getSession(identifier) !== void 0;
  }
  touch(nonce, s) {
    this.byNonce.delete(nonce);
    this.byNonce.set(nonce, { s, at: this.now() });
    return s;
  }
  forget(nonce) {
    const e = this.byNonce.get(nonce);
    if (e === void 0)
      return;
    this.byNonce.delete(nonce);
    const key = e.s.peerIdentityKey;
    if (typeof key === "string") {
      const set = this.byKey.get(key);
      set?.delete(nonce);
      if (set?.size === 0)
        this.byKey.delete(key);
    }
  }
  /** Forgets every session idle for ttlMs: the oldest come first. */
  expire() {
    const cutoff = this.now() - this.ttlMs;
    for (const [nonce, e] of this.byNonce) {
      if (e.at > cutoff)
        break;
      this.forget(nonce);
      this.evicted("idle");
    }
  }
  prune() {
    this.expire();
    for (const nonce of this.byNonce.keys()) {
      if (this.byNonce.size <= this.max)
        break;
      this.forget(nonce);
      this.evicted("cap");
    }
  }
};

// node_modules/@lightwebinc/bcommon/dist/host/front.js
var PaymentVersion = "1.0";
var MaxBody = 64 << 10;
var MaxHeaders = 256 << 10;
var AuthTimeout = 3e4;
var termsPath = (service) => `/${service}/terms`;
function parsePrices(raw, classes, variable) {
  const out = /* @__PURE__ */ new Map();
  if (raw === void 0 || raw.trim() === "")
    return out;
  for (const entry of raw.split(",")) {
    const e = entry.trim();
    const eq = e.indexOf("=");
    const name = eq < 0 ? e : e.slice(0, eq);
    const cls = classes.find((c) => c.name === name);
    if (eq < 0 || cls === void 0)
      throw new Error(`${variable}: "${e}" is not <class>=<satoshis>`);
    if (cls.free)
      throw new Error(`${variable}: ${name} is a free class and never has a price`);
    if (out.has(name))
      throw new Error(`${variable} names ${name} twice`);
    const v = e.slice(eq + 1);
    if (!/^(0|[1-9][0-9]*)$/.test(v) || !Number.isSafeInteger(Number(v)))
      throw new Error(`${variable}: ${name}: "${v}" is not an integer from 0 to 2^53 - 1`);
    out.set(name, Number(v));
  }
  for (const c of classes) {
    const after = `${c.name}-after`;
    if (!classes.some((x) => x.name === after))
      continue;
    if ((out.get(c.name) ?? 0) > 0 !== (out.get(after) ?? 0) > 0)
      throw new Error(`${variable}: ${c.name} and ${after} are priced together or not at all`);
  }
  return out;
}
function termsDocument(service, classes, prices) {
  return {
    service,
    terms: 1,
    classes: classes.filter((c) => prices.has(c.name)).map((c) => ({ class: c.name, satoshis: prices.get(c.name) }))
  };
}
function p2pkh(key) {
  return Uint8Array.from([118, 169, 20, ...Hash.hash160(Array.from(key)), 136, 172]);
}
var fromHex = (s) => Uint8Array.from(s.match(/../g) ?? [], (b) => parseInt(b, 16));
var b64 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
function strictBase64(s) {
  if (s.length === 0 || !b64.test(s))
    return void 0;
  const b = Uint8Array.from(Utils2.toArray(s, "base64"));
  return Utils2.toBase64(Array.from(b)) === s ? b : void 0;
}
var text = new TextEncoder();
var json = (status, v, headers = {}) => ({
  status,
  headers: { "content-type": "application/json", ...headers },
  body: text.encode(JSON.stringify(v))
});
var failure = (status, code, description) => json(status, { status: "error", code, description });
var ServerTransport = class {
  callback;
  waiting = /* @__PURE__ */ new Map();
  async send(m) {
    const key = m.messageType === "general" ? `g:${Utils2.toBase64(m.payload.slice(0, 32))}` : `h:${m.yourNonce}`;
    const w = this.waiting.get(key);
    if (w === void 0)
      throw new Error("no open request for this message");
    this.waiting.delete(key);
    w(m);
  }
  async onData(callback) {
    this.callback = callback;
  }
  /** What the peer will send for key, once it sends it. */
  expect(key) {
    return new Promise((resolve, reject) => {
      const t = setTimeout(() => {
        this.waiting.delete(key);
        reject(new Error("authentication timed out"));
      }, AuthTimeout);
      t.unref();
      this.waiting.set(key, (m) => {
        clearTimeout(t);
        resolve(m);
      });
    });
  }
  forget(key) {
    this.waiting.delete(key);
  }
  async deliver(m) {
    if (this.callback === void 0)
      throw new Error("no peer");
    await this.callback(m);
  }
};
function header(h, name) {
  const v = h[name];
  return typeof v === "string" ? v : void 0;
}
function optionalText(w, s) {
  if (s.length === 0) {
    w.writeVarIntNum(-1);
    return;
  }
  const b = Utils2.toArray(s, "utf8");
  w.writeVarIntNum(b.length);
  w.write(b);
}
function writePairs(w, pairs) {
  pairs.sort(([a], [b]) => a.localeCompare(b));
  w.writeVarIntNum(pairs.length);
  for (const [k, v] of pairs) {
    const kb = Utils2.toArray(k, "utf8");
    const vb = Utils2.toArray(v, "utf8");
    w.writeVarIntNum(kb.length);
    w.write(kb);
    w.writeVarIntNum(vb.length);
    w.write(vb);
  }
}
var signedHeader = (k) => k.startsWith("x-bsv-") && !k.startsWith("x-bsv-auth") || k === "authorization";
function requestPayload(requestId, method, url, headers, body) {
  const w = new Utils2.Writer();
  w.write(requestId);
  w.writeVarIntNum(method.length);
  w.write(Utils2.toArray(method));
  optionalText(w, url.pathname);
  optionalText(w, url.search);
  const pairs = [];
  for (const [k, v] of Object.entries(headers)) {
    if (typeof v !== "string")
      continue;
    if (signedHeader(k))
      pairs.push([k, v]);
    else if (k === "content-type")
      pairs.push([k, v.split(";")[0].trim()]);
  }
  writePairs(w, pairs);
  if (body.length === 0) {
    w.writeVarIntNum(-1);
  } else {
    w.writeVarIntNum(body.length);
    w.write(Array.from(body));
  }
  return w.toArray();
}
function responsePayload(requestId, r) {
  const w = new Utils2.Writer();
  w.write(requestId);
  w.writeVarIntNum(r.status);
  writePairs(w, Object.entries(r.headers).map(([k, v]) => [k.toLowerCase(), v]).filter(([k]) => signedHeader(k)));
  w.writeVarIntNum(r.body.length);
  if (r.body.length > 0)
    w.write(Array.from(r.body));
  return w.toArray();
}
async function readBody(req) {
  const chunks = [];
  let n = 0;
  for await (const c of req) {
    n += c.length;
    if (n > MaxBody)
      throw new Error("the request body is too large");
    chunks.push(c);
  }
  return new Uint8Array(Buffer.concat(chunks));
}
var RequestResults = ["replayed", "limited"];
var HandshakeResults = ["accepted", "failed", "limited_address", "limited_global"];
var LookupFront = class {
  o;
  peer;
  transport = new ServerTransport();
  /** The BRC-104 sessions the route keeps. */
  sessions;
  /** What handshakes the route answers. */
  budget;
  /** What signed responses each session is given. */
  responses;
  /** The authenticated requests answered lately: one sent twice is refused. */
  seen = new SeenRequests();
  /** Requests whose signature is being checked now. */
  verifying = /* @__PURE__ */ new Set();
  /** Payment acceptance. */
  gate;
  constructor(o) {
    this.o = o;
    for (const [name, price] of o.prices) {
      if (price > 0 && (o.wallet === void 0 || o.receiver === void 0 || o.headers === void 0)) {
        throw new Error(`${o.app}: ${name} has a price, which needs a payee wallet, a payment receiver and a header source`);
      }
    }
    if (o.gate !== void 0)
      this.gate = o.gate;
    else if (o.headers !== void 0 && [...o.prices.values()].some((p) => p > 0))
      this.gate = new PaymentGate({ app: o.app, host: o.host, headers: o.headers, policy: defaultAcceptancePolicy() });
    const bound = o.sessions ?? { max: DefaultMaxSessions, ttlSeconds: DefaultSessionTTL };
    this.sessions = new BoundedSessions(bound.max, bound.ttlSeconds * 1e3, Date.now, (why) => o.host.metrics.inc(`${o.app}_sessions_evicted_total`, { why }));
    o.host.metrics.gauge(`${o.app}_sessions`, () => this.sessions.size);
    this.budget = new HandshakeBudget(o.budget ?? DefaultBudget);
    this.responses = new KeyedBudget(o.responses ?? DefaultResponseBudget, bound.max);
    for (const result of RequestResults)
      o.host.metrics.preset(`${o.app}_requests_total`, { result });
    for (const result of HandshakeResults)
      o.host.metrics.preset(`${o.app}_handshakes_total`, { result });
    if (o.wallet !== void 0)
      this.peer = new Peer(o.wallet, this.transport, void 0, this.sessions, false);
  }
  /** The node:http request handler. */
  handler = (req, res) => {
    this.handle(req, res).catch((e) => {
      this.o.host.log(`${this.o.app} terms route failed`, { err: String(e) });
      if (!res.headersSent)
        send(res, failure(500, "ERR_INTERNAL", "the request could not be served"));
      else
        res.destroy();
    });
  };
  /** Listens on host:port; resolves with the server once it listens. */
  async listen(port, hostname) {
    const server = createServer({ maxHeaderSize: MaxHeaders }, this.handler);
    await new Promise((resolve, reject) => {
      server.once("error", reject);
      server.listen(port, hostname, () => {
        server.off("error", reject);
        resolve();
      });
    });
    return server;
  }
  async handle(req, res) {
    const handshake = req.method === "POST" && new URL(req.url ?? "/", "http://front").pathname === "/.well-known/auth";
    if (handshake && this.peer !== void 0) {
      const v = this.budget.take(req.socket.remoteAddress ?? "");
      if (!v.ok) {
        this.o.host.metrics.inc(`${this.o.app}_handshakes_total`, { result: `limited_${v.limit}` });
        send(res, json(429, { status: "error", code: "ERR_RATE_LIMITED", description: "too many handshakes; try again later" }, { "retry-after": String(v.retryAfter) }));
        return;
      }
    }
    let body;
    try {
      body = await readBody(req);
    } catch {
      send(res, failure(413, "ERR_TOO_LARGE", "the request body is too large"));
      return;
    }
    const url = new URL(req.url ?? "/", "http://front");
    const method = req.method ?? "GET";
    if (handshake) {
      const status = await this.handshake(res, body);
      if (this.peer !== void 0)
        this.o.host.metrics.inc(`${this.o.app}_handshakes_total`, { result: status === 200 ? "accepted" : "failed" });
      return;
    }
    const requestId = header(req.headers, "x-bsv-auth-request-id");
    if (requestId === void 0) {
      if (Object.keys(req.headers).some((k) => k.startsWith("x-bsv-auth-"))) {
        send(res, failure(400, "ERR_AUTH_PARTIAL", "partial BRC-104 authentication headers"));
        return;
      }
      send(res, await this.route(method, url, req.headers, body, void 0));
      return;
    }
    await this.authenticated(req, res, method, url, body, requestId);
  }
  /** Answers a handshake; resolves with the status answered. */
  async handshake(res, body) {
    if (this.peer === void 0) {
      send(res, failure(404, "ERR_NOT_FOUND", "this host offers no BRC-104 authentication"));
      return 404;
    }
    let m;
    try {
      m = normalizeBRC100ByteFields(JSON.parse(new TextDecoder().decode(body)), ["payload", "signature"]);
    } catch {
      send(res, failure(400, "ERR_AUTH_MALFORMED", "the handshake is not JSON"));
      return 400;
    }
    if (m.messageType !== "initialRequest" || typeof m.initialNonce !== "string") {
      send(res, failure(400, "ERR_AUTH_MALFORMED", "only an initial request is accepted here"));
      return 400;
    }
    const key = `h:${m.initialNonce}`;
    const reply = this.transport.expect(key);
    try {
      await this.transport.deliver(m);
    } catch (e) {
      this.transport.forget(key);
      reply.catch(() => {
      });
      send(res, failure(401, "ERR_AUTH_FAILED", String(e.message)));
      return 401;
    }
    const r = await reply;
    const headers = {
      "content-type": "application/json",
      "x-bsv-auth-version": r.version,
      "x-bsv-auth-message-type": r.messageType,
      "x-bsv-auth-identity-key": r.identityKey
    };
    if (r.yourNonce !== void 0)
      headers["x-bsv-auth-your-nonce"] = r.yourNonce;
    if (r.signature !== void 0)
      headers["x-bsv-auth-signature"] = Utils2.toHex(r.signature);
    send(res, { status: 200, headers, body: text.encode(stringifyBRC100(r)) });
    return 200;
  }
  async authenticated(req, res, method, url, body, requestId) {
    const h = (n) => header(req.headers, n);
    const version = h("x-bsv-auth-version");
    const identityKey = h("x-bsv-auth-identity-key");
    const nonce = h("x-bsv-auth-nonce");
    const yourNonce = h("x-bsv-auth-your-nonce");
    const signature = h("x-bsv-auth-signature");
    const id = strictBase64(requestId);
    if (this.peer === void 0 || version === void 0 || identityKey === void 0 || nonce === void 0 || yourNonce === void 0 || signature === void 0 || id?.length !== 32 || !/^([0-9a-f]{2})+$/.test(signature)) {
      send(res, failure(401, "ERR_AUTH_FAILED", "the BRC-104 authentication headers are incomplete"));
      return;
    }
    const idBytes = Array.from(id);
    const once = `${identityKey}:${requestId}`;
    if (this.seen.has(once) || this.verifying.has(once)) {
      this.o.host.metrics.inc(`${this.o.app}_requests_total`, { result: "replayed" });
      send(res, failure(401, "ERR_AUTH_REPLAYED", "this request was already answered"));
      return;
    }
    if (this.o.responseBudget === "before-verify" && !this.allowed(res, yourNonce))
      return;
    this.verifying.add(once);
    try {
      await this.transport.deliver({
        version,
        messageType: "general",
        identityKey,
        nonce,
        yourNonce,
        payload: requestPayload(idBytes, method, url, req.headers, body),
        signature: Array.from(fromHex(signature))
      });
    } catch {
      send(res, failure(401, "ERR_AUTH_FAILED", "the request does not verify"));
      return;
    } finally {
      this.verifying.delete(once);
    }
    this.seen.add(once);
    if (this.o.responseBudget !== "before-verify" && !this.allowed(res, yourNonce))
      return;
    const reply = await this.route(method, url, req.headers, body, identityKey);
    const key = `g:${requestId}`;
    const signed = this.transport.expect(key);
    try {
      await this.peer.toPeer(responsePayload(idBytes, reply), yourNonce);
    } catch (e) {
      this.transport.forget(key);
      signed.catch(() => {
      });
      throw e;
    }
    const m = await signed;
    send(res, {
      ...reply,
      headers: {
        ...reply.headers,
        "x-bsv-auth-version": m.version,
        "x-bsv-auth-identity-key": m.identityKey,
        "x-bsv-auth-nonce": m.nonce,
        "x-bsv-auth-your-nonce": m.yourNonce,
        "x-bsv-auth-signature": Utils2.toHex(m.signature),
        "x-bsv-auth-request-id": requestId
      }
    });
  }
  /** Takes one signed response from session's budget, or answers 429 and says so. */
  allowed(res, session) {
    const allowed = this.responses.take(session);
    if (allowed.ok)
      return true;
    this.o.host.metrics.inc(`${this.o.app}_requests_total`, { result: "limited" });
    send(res, json(429, { status: "error", code: "ERR_RATE_LIMITED", description: "too many requests on this session; try again later" }, { "retry-after": String(allowed.retryAfter) }));
    return false;
  }
  /** The application: terms and lookups. identity is the asker's key when authenticated. */
  async route(method, url, headers, body, identity) {
    const { ls, prices } = this.o;
    if (url.pathname === termsPath(this.o.service) && method === "GET") {
      if (prices.size === 0)
        return failure(404, "ERR_NO_TERMS", "this host prices nothing");
      return json(200, termsDocument(this.o.service, this.o.classes, prices));
    }
    if (url.pathname !== "/lookup" || method !== "POST")
      return failure(404, "ERR_NOT_FOUND", "no such route");
    let question;
    try {
      question = JSON.parse(new TextDecoder().decode(body));
      if (typeof question !== "object" || question === null)
        throw new Error("not an object");
    } catch {
      return json(400, { error: "the question is not a JSON object" });
    }
    let cls;
    try {
      cls = ls.classify(question);
    } catch (e) {
      return json(400, { error: e.message });
    }
    if (!ls.restored)
      return failure(503, "ERR_NOT_READY", "the lookup service is not restored yet");
    const price = prices.get(cls);
    const extra = {};
    if (price !== void 0 && price > 0) {
      if (identity === void 0)
        return failure(401, "ERR_AUTH_REQUIRED", `${cls} is priced; ask it over BRC-104 authentication`);
      const raw = header(headers, "x-bsv-payment");
      if (raw === void 0) {
        this.gate?.count("requested", "no-payment");
        const prefix = await createNonce(this.o.wallet);
        return json(402, { status: "error", code: "ERR_PAYMENT_REQUIRED", satoshisRequired: price, description: `${cls} costs ${price} satoshis a question` }, { "x-bsv-payment-version": PaymentVersion, "x-bsv-payment-satoshis-required": String(price), "x-bsv-payment-derivation-prefix": prefix });
      }
      const paid = await this.pay(raw, identity, price, cls);
      if (!paid.ok)
        return paid.reply;
      extra["x-bsv-payment-satoshis-paid"] = String(paid.satoshis);
    }
    try {
      return json(200, await ls.answer(question), extra);
    } catch (e) {
      return json(400, { error: e.message });
    }
  }
  /**
   * Checks a BRC-105 payment of price for one question of cls from identity:
   * a prefix this server issued, an Atomic BEEF whose output 0 holds at least
   * the price and pays the key BRC-29 derives for the prefix, the suffix and
   * the asker, a transaction that verifies against the host's headers (full
   * SPV, no fee check), a txid never claimed before, and no input an
   * accepted payment already spent.
   */
  async pay(raw, identity, price, cls) {
    const bad = (status, code, d, reason = "malformed") => {
      this.gate?.count("refuse", reason);
      return { ok: false, reply: failure(status, code, d) };
    };
    const wallet = this.o.wallet;
    let p;
    try {
      p = JSON.parse(raw);
    } catch {
      return bad(400, "ERR_MALFORMED_PAYMENT", "the payment header is not JSON");
    }
    const { derivationPrefix: prefix, derivationSuffix: suffix, transaction } = p ?? {};
    if (typeof prefix !== "string" || typeof suffix !== "string" || typeof transaction !== "string" || strictBase64(prefix) === void 0 || strictBase64(suffix) === void 0) {
      return bad(400, "ERR_MALFORMED_PAYMENT", "the payment header is malformed");
    }
    const beef = strictBase64(transaction);
    if (beef === void 0)
      return bad(400, "ERR_MALFORMED_PAYMENT", "the payment transaction is not standard base64");
    let issued = false;
    try {
      issued = await verifyNonce(prefix, wallet);
    } catch {
      issued = false;
    }
    if (!issued)
      return bad(400, "ERR_INVALID_DERIVATION_PREFIX", "this server did not issue that derivation prefix");
    let tx;
    try {
      tx = Transaction3.fromAtomicBEEF(Array.from(beef));
    } catch {
      return bad(400, "ERR_INVALID_PAYMENT", "the payment is not Atomic BEEF");
    }
    const out = tx.outputs[0];
    if (out === void 0 || (out.satoshis ?? 0) < price)
      return bad(400, "ERR_INVALID_PAYMENT", `output 0 does not hold ${price} satoshis`);
    const { publicKey } = await wallet.getPublicKey({ protocolID: this.o.paymentProtocol, keyID: `${prefix} ${suffix}`, counterparty: identity, forSelf: true });
    if (!bytesEqual(Uint8Array.from(out.lockingScript.toBinary()), p2pkh(fromHex(publicKey)))) {
      return bad(400, "ERR_INVALID_PAYMENT", "output 0 does not pay the key derived for this prefix, suffix and asker", "wrong-script");
    }
    let verified = false;
    try {
      verified = await tx.verify(this.o.headers);
    } catch {
      verified = false;
    }
    if (!verified)
      return bad(400, "ERR_PAYMENT_SPV", "the payment does not verify against this host's headers", "spv-failed");
    const accepted = {
      txid: tx.id("hex"),
      beef: Array.from(beef),
      outputIndex: 0,
      satoshis: out.satoshis ?? 0,
      derivationPrefix: prefix,
      derivationSuffix: suffix,
      senderIdentityKey: identity,
      class: cls,
      inputs: spentOutpoints(tx)
    };
    const replayed = () => bad(409, "ERR_PAYMENT_REPLAYED", "this payment was already used", "replayed");
    const conflict = () => bad(409, "ERR_PAYMENT_CONFLICT", "this payment spends a coin an earlier payment spent", "conflict");
    const before = this.o.receiver.refusal(accepted);
    if (before === "replayed")
      return replayed();
    if (before === "conflict")
      return conflict();
    const v = await this.gate.accept(tx, identity, accepted.satoshis);
    if (v.decision === "refuse") {
      this.o.host.log(`${this.o.app} payment refused`, { txid: accepted.txid, reason: v.reason, detail: v.detail ?? "" });
      return { ok: false, reply: failure(400, "ERR_PAYMENT_REFUSED", `the payment is refused (${v.reason})${v.detail === void 0 ? "" : `: ${v.detail}`}`) };
    }
    accepted.decision = v.decision === "hold" ? "hold" : v.reason === "mined" ? "mined" : "fast";
    accepted.reason = v.reason;
    const claim = this.o.receiver.claim(accepted);
    if (claim !== "accepted" && v.decision === "fast")
      this.gate.forget(accepted.txid);
    if (claim === "replayed")
      return replayed();
    if (claim === "conflict")
      return conflict();
    if (v.decision === "hold") {
      this.o.host.log(`${this.o.app} payment held for confirmation`, { txid: accepted.txid, satoshis: accepted.satoshis, class: cls, reason: v.reason });
      return {
        ok: false,
        reply: json(
          // 202, not 402: a BRC-105 client (go-sdk's and the TS SDK's
          // AuthFetch) takes every 402 for a price to pay, so a 402 without
          // the BRC-105 headers fails inside the client and never reaches its
          // caller, and one with them would be paid a second time.
          202,
          { status: "error", code: "ERR_PAYMENT_HELD", txid: accepted.txid, reason: v.reason, description: `the payment is held for confirmation (${v.reason}); send it again once it is mined` }
        )
      };
    }
    this.o.host.log(`${this.o.app} payment accepted`, { txid: accepted.txid, satoshis: accepted.satoshis, class: cls, decision: accepted.decision });
    return { ok: true, satoshis: accepted.satoshis };
  }
};
function send(res, r) {
  res.writeHead(r.status, { ...r.headers, "content-length": String(r.body.length) });
  res.end(Buffer.from(r.body));
}

// src/beef.ts
var DefaultMaxBEEF = 256 << 10;
function beefCounts(b) {
  let i = 0;
  const byte = () => i < b.length ? b[i++] : void 0;
  const varInt = () => {
    const h = byte();
    if (h === void 0) return void 0;
    const n = h === 253 ? 2 : h === 254 ? 4 : h === 255 ? 8 : 0;
    if (n === 0) return h;
    if (i + n > b.length) return void 0;
    let v = 0;
    for (let k = n - 1; k >= 0; k--) v = v * 256 + b[i + k];
    i += n;
    return v;
  };
  if (b.length >= 4 && b[0] === 1 && b[1] === 1 && b[2] === 1 && b[3] === 1) i = 4 + 32;
  i += 4;
  const bumps = varInt();
  if (bumps === void 0) return void 0;
  for (let n = 0; n < bumps; n++) {
    if (varInt() === void 0) return void 0;
    const levels = byte();
    if (levels === void 0) return void 0;
    for (let l = 0; l < levels; l++) {
      const leaves = varInt();
      if (leaves === void 0) return void 0;
      for (let k = 0; k < leaves; k++) {
        if (varInt() === void 0) return void 0;
        const flags = byte();
        if (flags === void 0) return void 0;
        if ((flags & 1) === 0) i += 32;
      }
    }
  }
  const txs = varInt();
  if (txs === void 0 || i > b.length) return void 0;
  return { bumps, txs };
}
function minimalPath(mp, txid) {
  const level0 = mp.path[0];
  if (level0 === void 0 || level0.length !== 2) return false;
  const at = level0.find((e) => e.hash === txid && e.txid === true);
  if (at === void 0) return false;
  for (const [level, leaves] of mp.path.entries()) {
    const node = Math.floor(at.offset / 2 ** level);
    const want = node % 2 === 0 ? node + 1 : node - 1;
    let n = 0;
    for (const e of leaves) {
      if (e === at) continue;
      if (e.offset !== want || e.txid === true) return false;
      n++;
    }
    if (n !== 1) return false;
  }
  return true;
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
function firstPush(s) {
  if (s.length < 36 || s[0] !== 33 || s[34] !== OP_CHECKSIG)
    return void 0;
  const p = s.subarray(35);
  const op = p[0];
  let n;
  let off;
  if (op >= 1 && op <= 75) {
    n = op;
    off = 1;
  } else if (op === OP_PUSHDATA1 && p.length >= 2) {
    n = p[1];
    off = 2;
  } else if (op === OP_PUSHDATA2 && p.length >= 3) {
    n = p[1] | p[2] << 8;
    off = 3;
  } else if (op === OP_PUSHDATA4 && p.length >= 5) {
    n = (p[1] | p[2] << 8 | p[3] << 16) + p[4] * 16777216;
    if (n > p.length)
      return void 0;
    off = 5;
  } else {
    return void 0;
  }
  if (p.length < off + n)
    return void 0;
  return p.subarray(off, off + n);
}
function parseScript(s) {
  const out = [];
  let i = 0;
  while (i < s.length) {
    const c = s[i++];
    let n;
    if (c === OP_0) {
      out.push({ code: c, data: new Uint8Array(0) });
      continue;
    } else if (c >= 1 && c <= 75) {
      n = c;
    } else if (c === OP_PUSHDATA1) {
      if (i + 1 > s.length)
        return void 0;
      n = s[i];
      i += 1;
    } else if (c === OP_PUSHDATA2) {
      if (i + 2 > s.length)
        return void 0;
      n = s[i] | s[i + 1] << 8;
      i += 2;
    } else if (c === OP_PUSHDATA4) {
      if (i + 4 > s.length)
        return void 0;
      n = (s[i] | s[i + 1] << 8 | s[i + 2] << 16) + s[i + 3] * 16777216;
      i += 4;
    } else {
      out.push({ code: c });
      continue;
    }
    if (n > s.length - i)
      return void 0;
    out.push({ code: c, data: s.subarray(i, i + n) });
    i += n;
  }
  return out;
}
function pushFields(s) {
  const ops = parseScript(s);
  if (ops === void 0 || ops.length < 3 || ops[0].data?.length !== 33 || ops[1].code !== OP_CHECKSIG)
    return void 0;
  const fields = [];
  for (const o of ops.slice(2)) {
    if (o.data === void 0)
      break;
    fields.push(o.data);
  }
  return fields.length > 0 ? fields : void 0;
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
  const split = (m, last) => {
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
    split,
    checkMagic: checkMagic2,
    decode(b, max, last, magic) {
      const f = split(decodeMap2(b, max), last);
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

// src/boxrec.ts
var ProtocolName = "bbox message";
var Protocol = [1, ProtocolName];
var KeyEnvelope = "envelope";
var KeySignature = "signature";
var TopicPrefix = "tm_bbox_";
var LookupService = "ls_bbox";
var TagFunding = Uint8Array.of(98, 98, 2);
var MagicEnvelope = Uint8Array.of(98, 98, 101, 1);
var MagicReceipt = Uint8Array.of(98, 98, 114, 1);
var MaxSafe = Number.MAX_SAFE_INTEGER;
var MaxTime = 253402300799;
var SuffixLen = 10;
var MaxOffice = 50 - TopicPrefix.length;
var MaxOfficeName = MaxOffice - 1 - SuffixLen;
var MinOffice = 1 + 1 + SuffixLen;
var MaxBox = 50;
var MaxContent = 16 << 10;
var MaxEnvelopeRecord = MaxContent + 4096;
var MaxAcks = 64;
var MaxReceiptRecord = 4096;
var EK = { magic: 0, office: 1, to: 2, box: 3, from: 4, created: 5, expires: 6, content: 7 };
var ekLast = 7n;
var RK = { magic: 0, office: 1, by: 2, acks: 3, created: 4 };
var rkLast = 4n;
var Refusal = class extends Error {
  constructor(reason, detail) {
    super(detail === void 0 ? `boxrec: ${reason}` : `boxrec: ${reason}: ${detail}`);
    this.reason = reason;
    this.name = "Refusal";
  }
  reason;
};
function checkIdentity(k) {
  if (strictPublicKey(k) === void 0) throw new Refusal("identity");
}
function checkName(s, max, digits2, fail) {
  if (s.length === 0 || s.length > max) throw new Refusal(fail);
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c >= 97 && c <= 122) continue;
    if (digits2 && c >= 48 && c <= 57 && i > 0) continue;
    if (c === 95 && i !== 0 && i !== s.length - 1 && s.charCodeAt(i - 1) !== 95) continue;
    throw new Refusal(fail);
  }
}
function checkOfficeName(name) {
  checkName(name, MaxOfficeName, false, "office");
}
function checkOffice(s) {
  if (s.length < MinOffice || s.length > MaxOffice) throw new Refusal("office");
  const cut = s.length - SuffixLen - 1;
  if (s.charCodeAt(cut) !== 95) throw new Refusal("office");
  for (let i = cut + 1; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c < 97 || c > 122) throw new Refusal("office");
  }
  checkOfficeName(s.slice(0, cut));
}
function topic(office) {
  checkOffice(office);
  return TopicPrefix + office;
}
function checkBox(s) {
  checkName(s, MaxBox, true, "box");
}
var rec = recordReader((reason, detail) => new Refusal(reason, detail));
var decodeMap = rec.decodeMap;
var intKeys = rec.split;
var checkMagic = rec.checkMagic;
var present = rec.need;
var needBytes = rec.bytes;
var needText = rec.text;
var needUint = rec.uint;
var inRange = rec.inRange;
var checkExtra = rec.checkExtraKeys;
function envelopeCross(e) {
  if (e.expires !== 0 && e.expires <= e.created) throw new Refusal("expires");
}
function decodeEnvelope(b) {
  const f = intKeys(decodeMap(b, MaxEnvelopeRecord), ekLast);
  checkMagic(f, MagicEnvelope);
  const office = needText(f, EK.office);
  checkOffice(office);
  const to = needBytes(f, EK.to);
  checkIdentity(to);
  const box = needText(f, EK.box);
  checkBox(box);
  const from = needBytes(f, EK.from);
  checkIdentity(from);
  const created = needUint(f, EK.created, 1, MaxTime);
  const expires = needUint(f, EK.expires, 0, MaxTime);
  const content = needBytes(f, EK.content);
  inRange(content.length, 1, MaxContent, "content");
  const e = { office, to, box, from, created, expires, content, extra: f.extra };
  envelopeCross(e);
  return e;
}
function compare(a, b) {
  for (let i = 0; i < Math.min(a.length, b.length); i++) if (a[i] !== b[i]) return a[i] - b[i];
  return a.length - b.length;
}
function checkAcks(acks) {
  if (acks.length < 1 || acks.length > MaxAcks) throw new Refusal("acks");
  for (const [i, a] of acks.entries()) {
    if (a.length !== 32) throw new Refusal("acks");
    if (i > 0 && compare(acks[i - 1], a) >= 0) throw new Refusal("acks");
  }
}
function decodeReceipt(b) {
  const f = intKeys(decodeMap(b, MaxReceiptRecord), rkLast);
  checkMagic(f, MagicReceipt);
  const office = needText(f, RK.office);
  checkOffice(office);
  const by = needBytes(f, RK.by);
  checkIdentity(by);
  const v = present(f, RK.acks);
  if (!Array.isArray(v)) throw new Refusal("type", `key ${RK.acks}`);
  if (v.length < 1 || v.length > MaxAcks) throw new Refusal("acks");
  const acks = [];
  for (const a of v) {
    if (!(a instanceof Uint8Array) || a.length !== 32) throw new Refusal("acks");
    acks.push(a);
  }
  checkAcks(acks);
  const created = needUint(f, RK.created, 1, MaxTime);
  return { office, by, acks, created, extra: f.extra };
}
var claims = claimsRecord;
var isEnvelope = (p) => claims(p, MagicEnvelope);
var isReceipt = (p) => claims(p, MagicReceipt);
function claimOfField(p) {
  if (isEnvelope(p)) return "envelope";
  if (isReceipt(p)) return "receipt";
  return "none";
}
function claimOf(s) {
  const f = firstPush(s);
  return f === void 0 ? "none" : claimOfField(f);
}
var firstPush2 = firstPush;

// src/journal.ts
import { closeSync as closeSync2, fdatasyncSync as fdatasyncSync2, mkdirSync as mkdirSync3, openSync as openSync2, readFileSync as readFileSync3, truncateSync, writeSync as writeSync2 } from "node:fs";
import { join as join3 } from "node:path";
function isLine(v) {
  if (typeof v !== "object" || v === null) return false;
  const o = v;
  const s = (k) => typeof o[k] === "string";
  const n = (k) => typeof o[k] === "number" && Number.isSafeInteger(o[k]);
  switch (o["k"]) {
    case "c":
      return s("op") && s("c") && s("txid") && (o["kind"] === "e" || o["kind"] === "r") && s("office") && n("at");
    case "d":
      return s("op") && s("c") && typeof o["w"] === "boolean";
    case "s":
      return s("op") && s("s") && s("office") && n("h") && n("at");
  }
  return false;
}
var FileJournal = class {
  path;
  fd;
  constructor(dir) {
    mkdirSync3(dir, { recursive: true });
    this.path = join3(dir, "outpoints.jsonl");
  }
  load() {
    let text2;
    try {
      text2 = readFileSync3(this.path, "utf8");
    } catch (e) {
      if (e.code === "ENOENT") return [];
      throw e;
    }
    if (text2.length > 0 && !text2.endsWith("\n")) {
      const keep = text2.lastIndexOf("\n") + 1;
      truncateSync(this.path, Buffer.byteLength(text2.slice(0, keep), "utf8"));
      text2 = text2.slice(0, keep);
    }
    const raw = text2.split("\n");
    const out = [];
    for (const [i, l] of raw.entries()) {
      if (l === "") continue;
      let v;
      try {
        v = JSON.parse(l);
      } catch {
        throw new Error(`${this.path}: line ${i + 1} is not JSON`);
      }
      if (!isLine(v)) throw new Error(`${this.path}: line ${i + 1} is not an outpoint row`);
      out.push(v);
    }
    return out;
  }
  append(l) {
    this.fd ??= openSync2(this.path, "a");
    writeSync2(this.fd, JSON.stringify(l) + "\n");
    fdatasyncSync2(this.fd);
  }
  close() {
    if (this.fd !== void 0) closeSync2(this.fd);
    this.fd = void 0;
  }
};

// src/ls_bbox.ts
import { Transaction as Transaction4 } from "@bsv/sdk";

// src/util.ts
import { Utils as Utils3 } from "@bsv/sdk";
var fromHex2 = (s) => Uint8Array.from(s.match(/../g) ?? [], (b) => parseInt(b, 16));
var toHex2 = (b) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
function bytesEq(a, b) {
  return a.length === b.length && a.every((x, i) => x === b[i]);
}
var isLowerHex = (s, n) => s.length === n && /^[0-9a-f]*$/.test(s);
var b642 = /^(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;
function strictBase642(s) {
  if (s.length === 0 || !b642.test(s)) return void 0;
  const b = Uint8Array.from(Utils3.toArray(s, "base64"));
  return Utils3.toBase64(Array.from(b)) === s ? b : void 0;
}

// src/query.ts
var PageEnvelopes = 64;
var PageReceipts = 8;
var PageSweeps = 8;
var Classes = [
  { name: "inbox", members: ["office", "to"], free: true, page: PageEnvelopes },
  { name: "inbox-after", members: ["after", "office", "to"], free: true, page: PageEnvelopes },
  { name: "box", members: ["box", "office", "to"], free: true, page: PageEnvelopes },
  { name: "box-after", members: ["after", "box", "office", "to"], free: true, page: PageEnvelopes },
  { name: "sender", members: ["from", "office", "to"], free: true, page: PageEnvelopes },
  { name: "sender-after", members: ["after", "from", "office", "to"], free: true, page: PageEnvelopes },
  { name: "receipt", members: ["by", "office", "receiptFor"], free: true, page: PageReceipts },
  { name: "sweep", members: ["office", "spent"], free: true, page: PageSweeps },
  { name: "history", members: ["history", "office"], free: false, page: PageEnvelopes },
  { name: "history-after", members: ["after", "history", "office"], free: false, page: PageEnvelopes }
];
function keyHex(s) {
  if (!isLowerHex(s, 66)) return void 0;
  const b = fromHex2(s);
  try {
    checkIdentity(b);
  } catch {
    return void 0;
  }
  return b;
}
var digits = /^[0-9]+$/;
function parseAfter(s) {
  const at = s.indexOf(":");
  if (at < 0) return void 0;
  const c = s.slice(0, at);
  const t = s.slice(at + 1);
  if (c.length === 0 || c.length > 12 || c[0] === "0" || !digits.test(c) || !isLowerHex(t, 64)) return void 0;
  const n = Number(c);
  return n > MaxTime ? void 0 : { created: n, txid: t };
}
function parseOutpoint(s) {
  const at = s.indexOf(".");
  if (at < 0) return void 0;
  const t = s.slice(0, at);
  const v = s.slice(at + 1);
  if (!isLowerHex(t, 64) || v.length === 0 || v.length > 10 || v.length > 1 && v[0] === "0" || !digits.test(v)) return void 0;
  const n = Number(v);
  return n > 4294967295 ? void 0 : { txid: t, vout: n };
}
function parseQuery(q) {
  const names = Object.keys(q).sort();
  const cls = Classes.find((c) => c.members.join(",") === names.join(","));
  if (cls === void 0) throw new Refusal("query", `members ${names.join(",")}`);
  const out = { class: cls, office: "" };
  for (const n of names) {
    const s = q[n];
    if (typeof s !== "string") throw new Refusal("query", `${n} is not a string`);
    const fail = () => {
      throw new Refusal("query", n);
    };
    switch (n) {
      case "office":
        try {
          checkOffice(s);
        } catch {
          fail();
        }
        out.office = s;
        break;
      case "to":
      case "by":
      case "history":
        out.key = keyHex(s) ?? fail();
        break;
      case "from":
        out.from = keyHex(s) ?? fail();
        break;
      case "spent": {
        const o = parseOutpoint(s) ?? fail();
        out.spentTxid = o.txid;
        out.spentVout = o.vout;
        break;
      }
      case "box":
        try {
          checkBox(s);
        } catch {
          fail();
        }
        out.box = s;
        break;
      case "after": {
        const a = parseAfter(s) ?? fail();
        out.afterCreated = a.created;
        out.afterTxid = a.txid;
        break;
      }
      case "receiptFor":
        if (!isLowerHex(s, 64)) fail();
        out.receiptFor = s;
        break;
    }
  }
  return out;
}

// src/script.ts
import { Hash as Hash2 } from "@bsv/sdk";
function fundingScript(key) {
  return pushDropScript(key, [TagFunding]);
}
function isFundingShape(s) {
  if (s.length !== 1 + 33 + 1 + 1 + 3 + 1 || s[0] !== 33) return false;
  const key = s.subarray(1, 34);
  if (strictPublicKey(key) === void 0) return false;
  return bytesEq(s, fundingScript(key));
}

// src/ls_bbox.ts
var WindowBefore = 30 * 86400;
var WindowAfter = 3600;
var FloorDays = 31;
var Evidence = 8;
var EvidenceAcknowledged = 64;
var outpoint = (txid, vout) => `${txid}.${vout}`;
var hashOrder = (display) => toHex2(fromHex2(display).reverse());
var cmp = (a, b) => a < b ? -1 : a > b ? 1 : 0;
function addTo(m, k, v) {
  let s = m.get(k);
  if (s === void 0) m.set(k, s = /* @__PURE__ */ new Set());
  s.add(v);
}
function removeFrom(m, k, v) {
  const s = m.get(k);
  s?.delete(v);
  if (s?.size === 0) m.delete(k);
}
var BboxLookupService = class {
  /** The whole transaction: a carrier's funding outpoint and a sweep's spends are its inputs. */
  admissionMode = "whole-tx";
  /** Spends are read from admitted transactions, never from the engine's notices. */
  spendNotificationMode = "none";
  offices;
  host;
  journal;
  priced;
  retention;
  now;
  // Outpoint rows, from the journal.
  rows = /* @__PURE__ */ new Map();
  /** Commitment -> its carrier line. */
  lines = /* @__PURE__ */ new Map();
  /** Envelope commitment -> receipt commitments listing it. */
  ackers = /* @__PURE__ */ new Map();
  /** Sweep txid -> first sight. */
  sweepSeen = /* @__PURE__ */ new Map();
  /** Sweep txid -> the offices it was admitted in, as the journal's `s` lines name them. */
  sweepOffices = /* @__PURE__ */ new Map();
  // What the host holds, from storage.
  envelopes = /* @__PURE__ */ new Map();
  envByTo = /* @__PURE__ */ new Map();
  receipts = /* @__PURE__ */ new Map();
  rcptByBy = /* @__PURE__ */ new Map();
  sweeps = /* @__PURE__ */ new Map();
  sweepsByOp = /* @__PURE__ */ new Map();
  /** Outputs dropped from the index and still to be deleted from storage. */
  pending = /* @__PURE__ */ new Map();
  storage;
  /** The restore in flight: an admission waits for it. */
  inflight;
  warnedNoDelete = false;
  // Memo of the status functions, cleared on any change.
  winners = /* @__PURE__ */ new Map();
  acked = /* @__PURE__ */ new Map();
  computing = /* @__PURE__ */ new Set();
  constructor(o) {
    this.offices = o.offices;
    this.host = o.host;
    this.journal = o.journal;
    this.priced = o.priced ?? /* @__PURE__ */ new Set();
    const days = o.retentionDays ?? FloorDays;
    if (!Number.isSafeInteger(days) || days < FloorDays) throw new Error(`ls_bbox: retention of ${days} days is under the ${FloorDays}-day floor`);
    this.retention = days * 86400;
    this.now = o.now ?? (() => Math.floor(Date.now() / 1e3));
    for (const l of this.journal.load()) this.apply(l);
  }
  // ---- gauges
  get envelopeCount() {
    return this.envelopes.size;
  }
  get receiptCount() {
    return this.receipts.size;
  }
  get sweepCount() {
    return this.sweeps.size;
  }
  get rowCount() {
    return this.rows.size;
  }
  /** Carriers ever admitted: one journal line, and one entry in memory, each, for the host's life. */
  get lineCount() {
    return this.lines.size;
  }
  get pendingDeletes() {
    return this.pending.size;
  }
  // ---- outpoint rows
  row(op) {
    let r = this.rows.get(op);
    if (r === void 0) this.rows.set(op, r = { carriers: /* @__PURE__ */ new Map(), dropped: /* @__PURE__ */ new Set(), closed: false, sweeps: /* @__PURE__ */ new Set() });
    return r;
  }
  apply(l) {
    const r = this.row(l.op);
    if (l.k === "c") {
      r.carriers.set(l.c, l);
      this.lines.set(l.c, l);
      for (const a of l.acks ?? []) addTo(this.ackers, a, l.c);
    } else if (l.k === "d") {
      r.dropped.add(l.c);
      if (l.w) r.closed = true;
    } else {
      r.sweeps.add(l.s);
      if (!this.sweepSeen.has(l.s)) this.sweepSeen.set(l.s, l.at);
      addTo(this.sweepOffices, l.s, l.office);
    }
    this.changed();
  }
  queueDelete(txid, office) {
    const t = topic(office);
    this.pending.set(`${t}|${txid}`, { txid, topic: t });
  }
  write(l) {
    this.journal.append(l);
    this.apply(l);
  }
  changed() {
    this.winners = /* @__PURE__ */ new Map();
    this.acked = /* @__PURE__ */ new Map();
  }
  // ---- statuses (spec section 8.2)
  /** The outpoint's winner: the lowest C among its acknowledged envelopes, else among all its carriers. */
  winnerOf(op) {
    const memo = this.winners.get(op);
    if (memo !== void 0) return memo;
    const r = this.rows.get(op);
    if (r === void 0 || r.carriers.size === 0) return void 0;
    const all = [...r.carriers.keys()].sort(cmp);
    if (this.computing.has(op)) return all[0];
    this.computing.add(op);
    let w;
    try {
      w = all.find((c) => this.isAcknowledged(c)) ?? all[0];
    } finally {
      this.computing.delete(op);
    }
    this.winners.set(op, w);
    return w;
  }
  /** Whether a receipt it is not retracted and is its outpoint's winner, so it acknowledges. */
  acknowledges(l) {
    return this.rows.get(l.op).sweeps.size === 0 && this.winnerOf(l.op) === l.c;
  }
  /** Whether an acknowledging receipt by the envelope's `to` lists envelope c. */
  isAcknowledged(c) {
    const memo = this.acked.get(c);
    if (memo !== void 0) return memo;
    const e = this.lines.get(c);
    let yes = false;
    if (e?.kind === "e") {
      for (const rc of this.ackers.get(c) ?? []) {
        const r = this.lines.get(rc);
        if (r.by === e.to && this.acknowledges(r)) {
          yes = true;
          break;
        }
      }
    }
    this.acked.set(c, yes);
    return yes;
  }
  /** The status of the carrier with commitment c (hash byte order hex). */
  status(c) {
    const l = this.lines.get(c);
    if (l === void 0) return void 0;
    const r = this.rows.get(l.op);
    if (r.sweeps.size > 0) return "retracted";
    if (r.closed || this.winnerOf(l.op) !== c) return "superseded";
    if (l.kind === "e" && this.isAcknowledged(c)) return "acknowledged";
    return "held";
  }
  /** Whether the carrier with commitment c was dropped. */
  dropped(c) {
    const l = this.lines.get(c);
    return l !== void 0 && this.rows.get(l.op).dropped.has(c);
  }
  open(e, now) {
    return this.status(e.c) === "held" && (e.expires === 0 || now < e.expires) && e.created >= now - WindowBefore && e.created <= now + WindowAfter;
  }
  // ---- admission
  /**
   * Indexes an admitted output. The index is changed before the first
   * await; what follows deletes what was dropped from storage. An admission
   * that meets a restore in flight waits for it, so that it is decided as
   * any other.
   */
  async outputAdmittedByTopic(payload) {
    if (payload.mode !== "whole-tx") return;
    const office = this.officeOf(payload.topic);
    if (office === void 0) return;
    let tx;
    try {
      tx = Transaction4.fromAtomicBEEF(payload.atomicBEEF);
    } catch {
      this.host.log("ls_bbox ignored an admission whose BEEF did not parse", { topic: payload.topic });
      return;
    }
    while (this.inflight !== void 0) await this.inflight.catch(() => {
    });
    this.index(office, tx, payload.outputIndex);
    await this.flush();
  }
  outputEvicted(txid, outputIndex) {
    if (outputIndex === 0) this.forget(txid);
  }
  outputNoLongerRetainedInHistory(txid, outputIndex, t) {
    const office = this.officeOf(t);
    if (outputIndex === 0 && office !== void 0) this.forget(txid, office);
  }
  officeOf(t) {
    if (!t.startsWith(TopicPrefix)) return void 0;
    const office = t.slice(TopicPrefix.length);
    return this.offices.has(office) ? office : void 0;
  }
  /**
   * Indexes output outputIndex of an admitted transaction on office, writing
   * its rows the first time it is seen. A carrier the rows say was dropped
   * is not indexed again, and its output is deleted.
   */
  index(office, tx, outputIndex) {
    const out = tx.outputs[outputIndex];
    if (out === void 0 || outputIndex !== 0) return false;
    const script = Uint8Array.from(out.lockingScript.toBinary());
    const txid = tx.id("hex");
    const kind = claimOf(script);
    const now = this.now();
    if (kind !== "none") {
      const input = tx.inputs[0];
      const src = input?.sourceTXID ?? input?.sourceTransaction?.id("hex");
      if (input === void 0 || src === void 0) return false;
      const op = outpoint(src, input.sourceOutputIndex);
      const c = toHex2(Uint8Array.from(tx.hash()));
      const record = firstPush2(script);
      let line;
      let entry;
      if (kind === "envelope") {
        const e2 = decodeEnvelope(record);
        const to = toHex2(e2.to);
        line = { k: "c", op, c, txid, kind: "e", office, to, at: now };
        entry = { e: { txid, c, office, to, box: e2.box, from: toHex2(e2.from), created: e2.created, expires: e2.expires } };
      } else {
        const r = decodeReceipt(record);
        const by = toHex2(r.by);
        const acks = r.acks.map(toHex2);
        line = { k: "c", op, c, txid, kind: "r", office, by, acks, at: now };
        entry = { r: { txid, c, office, by, acks: new Set(acks), created: r.created } };
      }
      if (!this.lines.has(c)) this.write(line);
      if (this.rows.get(op).dropped.has(c)) {
        this.queueDelete(txid, office);
        return false;
      }
      if (entry.e !== void 0) {
        this.envelopes.set(txid, entry.e);
        addTo(this.envByTo, `${office}|${entry.e.to}`, txid);
      } else {
        this.receipts.set(txid, entry.r);
        addTo(this.rcptByBy, `${office}|${entry.r.by}`, txid);
      }
      this.changed();
      this.enforceEvidence(op);
      return true;
    }
    if (!isFundingShape(script)) {
      this.host.log("ls_bbox indexed an output of no known kind", { office, txid });
      return false;
    }
    const height = tx.merklePath?.blockHeight ?? 0;
    const spent = [];
    for (const input of tx.inputs) {
      const src = input.sourceTXID ?? input.sourceTransaction?.id("hex");
      if (src === void 0) continue;
      const op = outpoint(src, input.sourceOutputIndex);
      spent.push(op);
      if (!this.rows.get(op)?.sweeps.has(txid)) {
        this.write({ k: "s", op, s: txid, office, h: height, at: this.sweepSeen.get(txid) ?? now });
        this.host.metrics.inc("bbox_retractions_total");
      }
    }
    if (spent.length > 0 && this.sweepOffices.get(txid)?.has(office) !== true) {
      this.write({ k: "s", op: spent[0], s: txid, office, h: height, at: this.sweepSeen.get(txid) ?? now });
    }
    let e = this.sweeps.get(txid);
    if (e === void 0) this.sweeps.set(txid, e = { txid, offices: /* @__PURE__ */ new Set(), spent, height });
    if (!e.offices.has(office)) {
      e.offices.add(office);
      for (const op of e.spent) addTo(this.sweepsByOp, `${office}|${op}`, txid);
    }
    return true;
  }
  /** Removes a held output from the index, in one office or in all; its rows stay. */
  forget(txid, office) {
    const e = this.envelopes.get(txid);
    if (e !== void 0 && (office === void 0 || e.office === office)) {
      this.envelopes.delete(txid);
      removeFrom(this.envByTo, `${e.office}|${e.to}`, txid);
    }
    const r = this.receipts.get(txid);
    if (r !== void 0 && (office === void 0 || r.office === office)) {
      this.receipts.delete(txid);
      removeFrom(this.rcptByBy, `${r.office}|${r.by}`, txid);
    }
    const s = this.sweeps.get(txid);
    if (s !== void 0) {
      for (const o of office === void 0 ? [...s.offices] : [office]) {
        if (!s.offices.delete(o)) continue;
        for (const op of s.spent) removeFrom(this.sweepsByOp, `${o}|${op}`, txid);
      }
      if (s.offices.size === 0) this.sweeps.delete(txid);
    }
  }
  // ---- drops (spec section 8.4)
  /**
   * Drops the carrier with commitment c: marks its row (and closes the row
   * when c is its winner), takes it out of every answer, and queues its
   * output for deletion from storage.
   */
  drop(c, why) {
    const l = this.lines.get(c);
    const r = this.rows.get(l.op);
    if (r.dropped.has(c)) return;
    this.write({ k: "d", op: l.op, c, w: this.winnerOf(l.op) === c });
    this.forget(l.txid);
    this.queueDelete(l.txid, l.office);
    this.host.metrics.inc("bbox_dropped_total", { why });
    this.host.log("ls_bbox dropped a carrier", { txid: l.txid, outpoint: l.op, why });
  }
  /**
   * Keeps at most 8 superseded carriers on an outpoint (64 when its winner
   * is acknowledged) and drops the rest, highest commitment first.
   */
  enforceEvidence(op) {
    const r = this.rows.get(op);
    const w = this.winnerOf(op);
    const cap = w !== void 0 && this.isAcknowledged(w) ? EvidenceAcknowledged : Evidence;
    const kept = [...r.carriers.keys()].filter((c) => !r.dropped.has(c) && this.status(c) === "superseded").sort(cmp);
    for (const c of kept.slice(cap).reverse()) this.drop(c, "evidence");
  }
  /**
   * Applies retention at the host's time: drops every carrier and sweep
   * past its keep, then deletes what was dropped from storage. What is kept:
   * an open envelope, and one dated ahead of the host's clock, always; any
   * other carrier or sweep for the retention period from first sight, at
   * least 31 days. Returns how many were dropped.
   */
  async prune() {
    const now = this.now();
    let n = 0;
    const old = (at) => at !== void 0 && now - at >= this.retention;
    for (const e of [...this.envelopes.values()]) {
      if (this.open(e, now) || e.created > now + WindowAfter && this.status(e.c) !== "retracted") continue;
      if (old(this.lines.get(e.c)?.at)) {
        this.drop(e.c, "retention");
        n++;
      }
    }
    for (const r of [...this.receipts.values()]) {
      if (old(this.lines.get(r.c)?.at)) {
        this.drop(r.c, "retention");
        n++;
      }
    }
    for (const s of [...this.sweeps.values()]) {
      if (old(this.sweepSeen.get(s.txid))) {
        for (const o of s.offices) this.queueDelete(s.txid, o);
        this.forget(s.txid);
        this.host.metrics.inc("bbox_dropped_total", { why: "retention" });
        n++;
      }
    }
    await this.flush();
    return n;
  }
  /**
   * Deletes dropped outputs from storage. It runs after every admission and
   * after retention, so a dropped output is gone at once. A store that
   * cannot delete keeps what was dropped; the queue is not kept for it.
   */
  async flush() {
    if (this.storage === void 0 || this.pending.size === 0) return;
    const del = this.storage.deleteOutput?.bind(this.storage);
    if (del === void 0) {
      if (!this.warnedNoDelete) this.host.log("ls_bbox cannot delete dropped outputs: the storage has no deleteOutput", {});
      this.warnedNoDelete = true;
      this.pending.clear();
      return;
    }
    for (const [k, p] of [...this.pending]) {
      try {
        await del(p.txid, 0, p.topic);
        this.pending.delete(k);
      } catch (e) {
        this.host.log("ls_bbox could not delete a dropped output", { txid: p.txid, err: String(e) });
      }
    }
  }
  // ---- questions (spec sections 7.2, 7.3 and 8.3)
  /** Asked through the engine: a class the host prices is refused here. */
  async lookup(question) {
    return this.answer(question, false);
  }
  /** The class of a question, validated, and whether this host prices it. */
  classify(question) {
    if (question.service !== LookupService) throw new Error(`ls_bbox: this service is ${LookupService}, not ${String(question.service)}`);
    const raw = question.query;
    if (typeof raw !== "object" || raw === null || Array.isArray(raw)) throw new Error("ls_bbox: the query is not an object");
    let q;
    try {
      q = parseQuery(raw);
    } catch (e) {
      if (e instanceof Refusal) throw new Error(`ls_bbox: refused: ${e.message.replace(/^boxrec: /, "")}`);
      throw e;
    }
    if (!this.offices.has(q.office)) throw new Error(`ls_bbox: this host does not carry office ${q.office}`);
    return { q, priced: !q.class.free && this.priced.has(q.class.name) };
  }
  /**
   * Answers a question. paid says the question was paid for (or asked where
   * no payment is due); a class this host prices is refused otherwise.
   */
  async answer(question, paid) {
    const { q, priced } = this.classify(question);
    if (priced && !paid) {
      throw new Error(`ls_bbox: ${q.class.name} is priced on this host; ask its terms route's /lookup over BRC-104`);
    }
    this.host.metrics.inc("bbox_lookups_total", { class: q.class.name });
    const now = this.now();
    const key = q.key === void 0 ? "" : toHex2(q.key);
    const f = (txid) => ({ txid, outputIndex: 0 });
    const envs = () => [...this.envByTo.get(`${q.office}|${key}`) ?? []].map((t) => this.envelopes.get(t));
    const page = (list) => {
      list.sort((a, b) => a.created - b.created || cmp(a.txid, b.txid));
      if (q.afterCreated !== void 0) {
        const ac = q.afterCreated;
        const at = q.afterTxid;
        list = list.filter((e) => e.created > ac || e.created === ac && e.txid > at);
      }
      return list.slice(0, q.class.page).map((e) => f(e.txid));
    };
    switch (q.class.name) {
      case "inbox":
      case "inbox-after":
        return page(envs().filter((e) => this.open(e, now)));
      case "box":
      case "box-after":
        return page(envs().filter((e) => e.box === q.box && this.open(e, now)));
      case "sender":
      case "sender-after": {
        const from = toHex2(q.from);
        return page(envs().filter((e) => e.from === from && this.open(e, now)));
      }
      case "history":
      case "history-after":
        return page(
          envs().filter((e) => {
            const s = this.status(e.c);
            return (s === "held" || s === "acknowledged") && !this.open(e, now) && e.created <= now + WindowAfter;
          })
        );
      case "receipt": {
        const t = hashOrder(q.receiptFor);
        const list = [...this.rcptByBy.get(`${q.office}|${key}`) ?? []].map((x) => this.receipts.get(x)).filter((r) => r.acks.has(t) && this.status(r.c) === "held").sort((a, b) => a.created - b.created || cmp(a.txid, b.txid));
        return list.slice(0, q.class.page).map((r) => f(r.txid));
      }
      case "sweep": {
        const op = outpoint(q.spentTxid, q.spentVout);
        const list = [...this.sweepsByOp.get(`${q.office}|${op}`) ?? []].map((x) => this.sweeps.get(x)).sort((a, b) => a.height - b.height || cmp(a.txid, b.txid));
        return list.slice(0, q.class.page).map((s) => f(s.txid));
      }
    }
    throw new Error(`ls_bbox: no answer for class ${q.class.name}`);
  }
  /**
   * An answer with each output's BEEF, as the engine hydrates one, read from
   * the host's storage: for the terms route, which answers outside the
   * engine.
   */
  async hydrate(formula) {
    const storage = this.storage;
    if (storage === void 0) throw new Error("ls_bbox: storage is not restored yet");
    const outputs = [];
    for (const { txid, outputIndex } of formula) {
      const o = await storage.findOutput(txid, outputIndex, void 0, void 0, true);
      if (o?.beef !== void 0) outputs.push({ beef: o.beef, outputIndex });
    }
    return { type: "output-list", outputs };
  }
  get restored() {
    return this.storage !== void 0;
  }
  // ---- restore (spec section 8.5)
  /**
   * Rebuilds the index: the rows from the journal, then every output of
   * this module's topics the host hands over (a row handed without its BEEF
   * is read again from storage with it), then every carrier and sweep the
   * rows name that the unspent rows did not carry (a spent tombstone, say),
   * read from storage. A carrier the rows say was dropped is not indexed,
   * and its output is deleted. Statuses are functions of the set, so the
   * order storage returns rows in changes nothing. A storage failure
   * propagates: a restore that skipped a sweep would serve retracted
   * envelopes.
   */
  async restore(outputs, storage) {
    while (this.inflight !== void 0) await this.inflight.catch(() => {
    });
    this.inflight = Promise.resolve().then(async () => await this.restoreAll(outputs, storage));
    try {
      return await this.inflight;
    } finally {
      this.inflight = void 0;
    }
  }
  async restoreAll(outputs, storage) {
    for (const m of [this.rows, this.lines, this.ackers, this.sweepSeen, this.sweepOffices, this.envelopes, this.envByTo, this.receipts, this.rcptByBy, this.sweeps, this.sweepsByOp, this.pending]) {
      m.clear();
    }
    for (const l of this.journal.load()) this.apply(l);
    const full = storage;
    this.storage = full;
    const seen = /* @__PURE__ */ new Set();
    const queue = [];
    for (const o of outputs) {
      if (this.officeOf(o.topic) === void 0 || o.outputIndex !== 0 || seen.has(`${o.topic}|${o.txid}`)) continue;
      seen.add(`${o.topic}|${o.txid}`);
      queue.push(o);
    }
    const named = [];
    for (const l of this.lines.values()) if (this.offices.has(l.office)) named.push({ topic: topic(l.office), txid: l.txid });
    for (const [txid, offices] of this.sweepOffices) for (const office of offices) if (this.offices.has(office)) named.push({ topic: topic(office), txid });
    let indexed = 0;
    const take = async (o) => {
      let row = o;
      if (row.beef === void 0) row = await full.findOutput(o.txid, 0, o.topic, void 0, true) ?? row;
      let tx;
      try {
        tx = row.beef === void 0 ? void 0 : Transaction4.fromBEEF(row.beef, row.txid);
      } catch {
        tx = void 0;
      }
      if (tx === void 0) {
        this.host.log("ls_bbox could not restore an output without its transaction", { topic: o.topic, txid: o.txid });
        return;
      }
      try {
        if (this.index(this.officeOf(o.topic), tx, 0)) indexed++;
      } catch (e) {
        this.host.log("ls_bbox could not restore an output", { topic: o.topic, txid: o.txid, err: String(e) });
      }
    };
    for (const o of queue) await take(o);
    for (const n of named) {
      if (seen.has(`${n.topic}|${n.txid}`)) continue;
      seen.add(`${n.topic}|${n.txid}`);
      const o = await full.findOutput(n.txid, 0, n.topic, void 0, true);
      if (o !== null) await take(o);
    }
    await this.flush();
    return indexed;
  }
  async getDocumentation() {
    return [
      "# ls_bbox",
      "",
      "Answers every office this host carries. Free classes: `inbox`, `inbox-after`,",
      "`box`, `box-after`, `sender`, `sender-after` (open envelopes to a key, 64 a page,",
      "by created then txid), `receipt` (answered receipts by a key listing a txid, at",
      "most 8) and `sweep` (sweeps spending an outpoint, at most 8). Priceable:",
      "`history`, `history-after` (envelopes to a key no longer open). A question must",
      "carry exactly the members of one class; anything else is refused with an error.",
      "An empty answer means only that this host holds nothing that answers."
    ].join("\n");
  }
  async getMetaData() {
    return { name: LookupService, shortDescription: "Answers bbox envelopes, receipts and sweeps by office and key." };
  }
};

// src/payment.ts
import { Beef } from "@bsv/sdk";

// src/jcs.ts
var maxDepth = 16;
var JObject = class _JObject {
  constructor(members = []) {
    this.members = members;
  }
  members;
  get(name) {
    return this.members.find((m) => m.name === name)?.value;
  }
  has(name) {
    return this.members.some((m) => m.name === name);
  }
  set(name, value) {
    const m = this.members.find((x) => x.name === name);
    if (m !== void 0) m.value = value;
    else this.members.push({ name, value });
    return this;
  }
  /** A shallow copy without the named members. */
  without(...names) {
    return new _JObject(this.members.filter((m) => !names.includes(m.name)));
  }
};
var JSONSubsetError = class extends Error {
  constructor() {
    super("not in the JSON subset");
    this.name = "JSONSubsetError";
  }
};
var utf8 = new TextDecoder("utf-8", { fatal: true, ignoreBOM: true });
var hex4 = /^[0-9a-fA-F]{4}$/;
var Parser = class {
  constructor(s) {
    this.s = s;
  }
  s;
  pos = 0;
  ws() {
    while (this.pos < this.s.length) {
      const c = this.s[this.pos];
      if (c !== " " && c !== "	" && c !== "\n" && c !== "\r") return;
      this.pos++;
    }
  }
  lit(t) {
    if (this.s.startsWith(t, this.pos)) {
      this.pos += t.length;
      return true;
    }
    return false;
  }
  value(depth) {
    if (depth > maxDepth || this.pos >= this.s.length) throw new JSONSubsetError();
    const c = this.s[this.pos];
    if (c === "{") return this.object(depth + 1);
    if (c === "[") return this.array(depth + 1);
    if (c === '"') return this.str();
    if (c === "-" || c >= "0" && c <= "9") return this.number();
    if (this.lit("true")) return true;
    if (this.lit("false")) return false;
    if (this.lit("null")) return null;
    throw new JSONSubsetError();
  }
  object(depth) {
    if (depth > maxDepth) throw new JSONSubsetError();
    this.pos++;
    const o = new JObject();
    const seen = /* @__PURE__ */ new Set();
    this.ws();
    if (this.lit("}")) return o;
    for (; ; ) {
      this.ws();
      if (this.s[this.pos] !== '"') throw new JSONSubsetError();
      const k = this.str();
      if (seen.has(k)) throw new JSONSubsetError();
      seen.add(k);
      this.ws();
      if (!this.lit(":")) throw new JSONSubsetError();
      this.ws();
      o.members.push({ name: k, value: this.value(depth) });
      this.ws();
      if (this.lit("}")) return o;
      if (!this.lit(",")) throw new JSONSubsetError();
    }
  }
  array(depth) {
    if (depth > maxDepth) throw new JSONSubsetError();
    this.pos++;
    const out = [];
    this.ws();
    if (this.lit("]")) return out;
    for (; ; ) {
      this.ws();
      out.push(this.value(depth));
      this.ws();
      if (this.lit("]")) return out;
      if (!this.lit(",")) throw new JSONSubsetError();
    }
  }
  hex4(at) {
    const h = this.s.slice(at, at + 4);
    return hex4.test(h) ? parseInt(h, 16) : void 0;
  }
  str() {
    this.pos++;
    let out = "";
    while (this.pos < this.s.length) {
      const c = this.s[this.pos];
      if (c === '"') {
        this.pos++;
        return out;
      }
      if (c.charCodeAt(0) < 32) throw new JSONSubsetError();
      if (c !== "\\") {
        out += c;
        this.pos++;
        continue;
      }
      if (this.pos + 1 >= this.s.length) throw new JSONSubsetError();
      const e = this.s[this.pos + 1];
      this.pos += 2;
      switch (e) {
        case '"':
        case "\\":
        case "/":
          out += e;
          break;
        case "b":
          out += "\b";
          break;
        case "f":
          out += "\f";
          break;
        case "n":
          out += "\n";
          break;
        case "r":
          out += "\r";
          break;
        case "t":
          out += "	";
          break;
        case "u": {
          const r = this.hex4(this.pos);
          if (r === void 0) throw new JSONSubsetError();
          this.pos += 4;
          if (r >= 55296 && r <= 57343) {
            if (r >= 56320 || !this.s.startsWith("\\u", this.pos)) throw new JSONSubsetError();
            const r2 = this.hex4(this.pos + 2);
            if (r2 === void 0 || r2 < 56320 || r2 > 57343) throw new JSONSubsetError();
            this.pos += 6;
            out += String.fromCharCode(r, r2);
          } else {
            out += String.fromCharCode(r);
          }
          break;
        }
        default:
          throw new JSONSubsetError();
      }
    }
    throw new JSONSubsetError();
  }
  number() {
    const start = this.pos;
    const neg = this.lit("-");
    const digits2 = this.pos;
    while (this.pos < this.s.length && this.s[this.pos] >= "0" && this.s[this.pos] <= "9") this.pos++;
    const d = this.s.slice(digits2, this.pos);
    if (d.length === 0 || d.length > 1 && d[0] === "0" || neg && d === "0") throw new JSONSubsetError();
    const next = this.s[this.pos];
    if (next === "." || next === "e" || next === "E") throw new JSONSubsetError();
    const n = BigInt(this.s.slice(start, this.pos));
    if (n > BigInt(Number.MAX_SAFE_INTEGER) || n < -BigInt(Number.MAX_SAFE_INTEGER)) throw new JSONSubsetError();
    return Number(n);
  }
};
function parseJSON(b) {
  let s;
  try {
    s = utf8.decode(b);
  } catch {
    throw new JSONSubsetError();
  }
  const p = new Parser(s);
  p.ws();
  const v = p.value(0);
  p.ws();
  if (p.pos !== s.length) throw new JSONSubsetError();
  return v;
}
var loneSurrogate2 = /[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/;
function canonString(s) {
  if (loneSurrogate2.test(s)) throw new JSONSubsetError();
  let out = '"';
  for (const ch of s) {
    const c = ch.charCodeAt(0);
    if (ch === '"') out += '\\"';
    else if (ch === "\\") out += "\\\\";
    else if (ch === "\b") out += "\\b";
    else if (ch === "\f") out += "\\f";
    else if (ch === "\n") out += "\\n";
    else if (ch === "\r") out += "\\r";
    else if (ch === "	") out += "\\t";
    else if (c < 32) out += "\\u00" + c.toString(16).padStart(2, "0");
    else out += ch;
  }
  return out + '"';
}
function canon(v) {
  if (v === null) return "null";
  if (v === true) return "true";
  if (v === false) return "false";
  if (typeof v === "number") {
    if (!Number.isSafeInteger(v)) throw new JSONSubsetError();
    return String(v);
  }
  if (typeof v === "string") return canonString(v);
  if (Array.isArray(v)) return "[" + v.map(canon).join(",") + "]";
  const ms = [...v.members].sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0);
  for (let i = 1; i < ms.length; i++) if (ms[i - 1].name === ms[i].name) throw new JSONSubsetError();
  return "{" + ms.map((m) => canonString(m.name) + ":" + canon(m.value)).join(",") + "}";
}
var enc = new TextEncoder();
function canonical(v) {
  return enc.encode(canon(v));
}

// src/content.ts
var MetanetHandles = "1.0";
var BRC78Version = Uint8Array.of(66, 66, 16, 51);
var BRC78Min = 4 + 33 + 33 + 32 + 32 + 16;
function rfc3339(t) {
  return new Date(t * 1e3).toISOString().replace(/\.\d{3}Z$/, "Z");
}
function str(o, name) {
  const v = o.get(name);
  return typeof v === "string" ? v : void 0;
}
function optionalString(o, name) {
  return !o.has(name) || typeof o.get(name) === "string";
}
function checkParty(doc, name, key, optional) {
  const p = doc.get(name);
  if (!(p instanceof JObject)) throw new Refusal("content-shape", name);
  if (str(p, "identityKey") !== toHex2(key)) throw new Refusal("content-shape", `${name}.identityKey`);
  for (const o of optional) if (!optionalString(p, o)) throw new Refusal("content-shape", `${name}.${o}`);
}
function parseCanonicalObject(b) {
  let v;
  try {
    v = parseJSON(b);
  } catch (e) {
    if (e instanceof JSONSubsetError) return void 0;
    throw e;
  }
  if (!(v instanceof JObject)) return void 0;
  let c;
  try {
    c = canonical(v);
  } catch (e) {
    if (e instanceof JSONSubsetError) return void 0;
    throw e;
  }
  return bytesEq(c, b) ? v : void 0;
}
function parseContent(e) {
  const doc = parseCanonicalObject(e.content);
  if (doc === void 0) throw new Refusal("content-json");
  if (str(doc, "metanetHandles") !== MetanetHandles) throw new Refusal("content-shape", "metanetHandles");
  checkParty(doc, "recipient", e.to, ["handle", "tag", "domain"]);
  checkParty(doc, "sender", e.from, ["handle", "domain"]);
  if (str(doc, "created") !== rfc3339(e.created)) throw new Refusal("content-shape", "created");
  if (!optionalString(doc, "quoteId")) throw new Refusal("content-shape", "quoteId");
  if (!doc.has("payment") || doc.get("payment") !== null) throw new Refusal("content-shape", "payment must be null");
  const b643 = str(doc, "content");
  if (b643 === void 0) throw new Refusal("content-shape", "content");
  const sigHex = str(doc, "signature");
  if (sigHex === void 0) throw new Refusal("content-shape", "signature");
  const cipher = strictBase642(b643);
  if (cipher === void 0 || cipher.length < BRC78Min) throw new Refusal("content-cipher", "not a BRC-78 message in standard base64");
  if (!bytesEq(cipher.subarray(0, 4), BRC78Version) || !bytesEq(cipher.subarray(4, 37), e.from) || !bytesEq(cipher.subarray(37, 70), e.to)) {
    throw new Refusal("content-cipher", "header does not name the record's sender and recipient");
  }
  const signed = canonical(doc.without("content", "signature"));
  const signature = /^(?:[0-9a-f]{2})*$/.test(sigHex) ? Uint8Array.from(sigHex.match(/../g) ?? [], (x) => parseInt(x, 16)) : new Uint8Array(0);
  return { doc, signed, cipher, signature };
}
function verifyContentSignature(c, from) {
  if (!strictSignature(c.signature)) throw new Refusal("content-signature", "not lowercase hex of a strict low-S DER signature");
  const key = readerLockingKey(Protocol, KeySignature, toHex2(from));
  if (!verifyFieldSignature(key, Array.from(c.signed), Array.from(c.signature))) throw new Refusal("content-signature");
}
function checkContent(e) {
  const c = parseContent(e);
  verifyContentSignature(c, e.from);
  return c;
}

// src/payment.ts
var PaymentProtocol = [2, "3241645161d8"];
var MaxPaymentBEEF = 16 << 10;

// src/paid.ts
var TermsPath = termsPath(LookupService);
function parsePrices2(raw) {
  return parsePrices(raw, Classes, "BBOX_PRICES");
}
function termsDocument2(prices) {
  return termsDocument(LookupService, Classes, prices);
}
function answerer(ls) {
  return {
    get restored() {
      return ls.restored;
    },
    classify: (q) => ls.classify(q).q.class.name,
    answer: async (q) => ls.hydrate(await ls.answer(q, true))
  };
}
function bboxRoute(ls) {
  return { app: "bbox", service: LookupService, classes: Classes, paymentProtocol: PaymentProtocol, ls: answerer(ls), responseBudget: "after-verify" };
}
var Ledger = { layout: "spread" };
var HeaderTracker = class {
  constructor(base, timeoutMs = 1e4) {
    this.base = base;
    this.timeoutMs = timeoutMs;
  }
  base;
  timeoutMs;
  async get(path) {
    const res = await fetch(`${this.base.replace(/\/+$/, "")}${path}`, { headers: { accept: "application/json" }, signal: AbortSignal.timeout(this.timeoutMs) });
    return { status: res.status, body: res.status === 200 ? await res.json() : void 0 };
  }
  async isValidRootForHeight(root, height) {
    const { status, body } = await this.get(`/v1/root/${height}`);
    if (status === 404) return false;
    if (status !== 200) throw new Error(`headers: root for height ${height}: status ${status}`);
    return body.merkleRoot === root;
  }
  async currentHeight() {
    const { status, body } = await this.get("/v1/tip");
    const height = body?.height;
    if (status !== 200 || typeof height !== "number") throw new Error(`headers: tip: status ${status}`);
    return height;
  }
};

// src/admit.ts
import { Beef as Beef2 } from "@bsv/sdk";
var bin = (s) => Uint8Array.from(s.toBinary());
async function mined(tx, headers) {
  if (tx.merklePath === void 0 || headers === void 0) return false;
  try {
    return await tx.merklePath.verify(tx.id("hex"), headers);
  } catch {
    return false;
  }
}
function parseBEEF(raw, bound) {
  if (raw.length > bound) throw new Refusal("beef", `${raw.length} bytes, max ${bound}`);
  let beef;
  try {
    beef = Beef2.fromBinaryView(Uint8Array.from(raw));
  } catch (e) {
    throw new Refusal("beef", e.message);
  }
  if (beef.txs.length === 0 || beef.bumps.some((b) => b.path.length === 0) || beef.txs.some((t) => t.tx !== void 0 && t.tx.inputs.length === 0)) {
    throw new Refusal("beef", "an empty BEEF, proof or transaction");
  }
  const target = beef.atomicTxid ?? beef.txs.at(-1).txid;
  const tx = beef.findAtomicTransaction(target);
  if (tx === void 0) throw new Refusal("beef", "no subject transaction");
  return { beef, tx };
}
function retained(tx, held2) {
  for (const i of held2) {
    if (!Number.isInteger(i) || i < 0 || i >= tx.inputs.length) throw new Error(`boxrec: previous coin names input ${i} of ${tx.inputs.length}`);
  }
  return [...new Set(held2)].sort((a, b) => a - b);
}
async function admit(raw, held2, h) {
  try {
    checkOffice(h.office);
  } catch {
    throw new Error(`boxrec: host office ${JSON.stringify(h.office)} breaks the grammar`);
  }
  const bytes = Uint8Array.from(raw);
  const { beef, tx } = parseBEEF(bytes, h.maxBEEF ?? DefaultMaxBEEF);
  const retain = retained(tx, held2);
  const a = { kind: "spend", txid: Uint8Array.from(tx.hash()), outputs: [], retain };
  let claim = "none";
  let claims2 = 0;
  for (const o of tx.outputs) {
    const k = claimOf(bin(o.lockingScript));
    if (k !== "none") {
      claim = k;
      claims2++;
    }
  }
  if (claims2 > 1) throw new Refusal("carrier-shape", `${claims2} outputs claim a record`);
  if (claims2 === 1) {
    a.carrier = checkCarrier(bytes, beef, tx, claim, h);
    a.kind = claim === "envelope" ? "envelope" : "receipt";
    a.outputs = [0];
    return a;
  }
  const first = tx.outputs[0];
  if (first !== void 0 && isFundingShape(bin(first.lockingScript))) {
    if (!await mined(tx, h.headers)) throw new Refusal("unmined");
    a.kind = "sweep";
    a.outputs = [0];
    a.spent = tx.inputs.map((i) => ({ txid: i.sourceTXID ?? i.sourceTransaction.id("hex"), vout: i.sourceOutputIndex }));
    return a;
  }
  if (retain.length > 0) return a;
  throw new Refusal("not-bbox");
}
function derived(owner) {
  return Uint8Array.from(readerLockingKey(Protocol, KeyEnvelope, toHex2(owner)).encode(true));
}
function checkCarrier(raw, beef, tx, claim, h) {
  if (tx.inputs.length !== 1 || tx.outputs.length !== 1) throw new Refusal("carrier-shape", `${tx.inputs.length} inputs, ${tx.outputs.length} outputs`);
  const input = tx.inputs[0];
  const out = tx.outputs[0];
  const src = input.sourceTransaction?.outputs[input.sourceOutputIndex];
  if (src === void 0) throw new Refusal("beef", "the spent output is not in the BEEF");
  if ((src.satoshis ?? 0) !== (out.satoshis ?? 0)) throw new Refusal("carrier-shape", "value");
  checkCarrierBEEF(raw, beef, tx);
  const lock = bin(out.lockingScript);
  const record = firstPush2(lock);
  let c;
  let office;
  const parent = input.sourceTXID ?? input.sourceTransaction.id("hex");
  const base = { record, commitment: Uint8Array.from(tx.hash()), funding: { txid: parent, vout: input.sourceOutputIndex } };
  if (claim === "envelope") {
    const e = decodeEnvelope(record);
    c = { ...base, envelope: e, owner: e.from };
    office = e.office;
  } else {
    const r = decodeReceipt(record);
    c = { ...base, receipt: r, owner: r.by };
    office = r.office;
  }
  if (office !== h.office) throw new Refusal("office", "not this topic's office");
  if (tx.lockTime < LockTime || (input.sequence ?? MaxSequence) === MaxSequence) throw new Refusal("mineable");
  if (unlockingRefusal(tx) !== void 0) throw new Refusal("unlock");
  const key = derived(c.owner);
  const fields = pushFields(lock);
  if (fields?.length !== 2) throw new Refusal("lock", "want the record and a signature");
  const sig = fields[1];
  if (!bytesEq(lock, pushDropScript(key, [record], sig))) throw new Refusal("lock");
  if (!strictSignature(sig) || !verifyFieldSignature(readerLockingKey(Protocol, KeyEnvelope, toHex2(c.owner)), Array.from(record), Array.from(sig))) {
    throw new Refusal("signature");
  }
  if (!bytesEq(bin(src.lockingScript), fundingScript(key))) throw new Refusal("funding");
  if (c.envelope !== void 0) c.content = checkContent(c.envelope);
  return c;
}
function checkCarrierBEEF(raw, beef, tx) {
  const n = beefCounts(raw);
  if (n === void 0 || n.bumps !== 1 || n.txs !== beef.txs.length || beef.bumps.length !== 1 || beef.txs.length < 2) {
    throw new Refusal("beef", `${n?.bumps} BUMPs and ${n?.txs} transactions`);
  }
  const self = beef.findTxid(tx.id("hex"));
  if (self?.tx === void 0 || self.bumpIndex !== void 0) throw new Refusal("beef", "not the carrier and its funding tree");
  const bumped = beef.txs.filter((e) => e.bumpIndex !== void 0);
  if (bumped.length !== 1 || bumped[0].bumpIndex !== 0 || bumped[0].tx === void 0) throw new Refusal("beef", "the proof is not the funding tree's alone");
  if (!minimalPath(beef.bumps[0], bumped[0].txid)) throw new Refusal("beef", "the proof holds more than the funding tree's path");
  const seen = /* @__PURE__ */ new Set([tx.id("hex")]);
  let frontier = [tx];
  while (frontier.length !== 0) {
    const next = [];
    for (const c of frontier) {
      if (c.merklePath !== void 0) continue;
      for (const i of c.inputs) {
        const p = i.sourceTXID ?? i.sourceTransaction?.id("hex");
        if (p === void 0 || p === "" || seen.has(p)) continue;
        seen.add(p);
        const e = beef.findTxid(p);
        if (e?.tx === void 0) throw new Refusal("beef", "the funding ancestry is not carried whole");
        next.push(e.tx);
      }
    }
    frontier = next;
  }
  if (seen.size !== beef.txs.length) throw new Refusal("beef", "transactions the carrier does not descend from");
}

// src/tm_bbox.ts
var Reasons = [
  "too-large",
  "cbor",
  "key-type",
  "missing",
  "type",
  "range",
  "identity",
  "office",
  "box",
  "expires",
  "acks",
  "content-json",
  "content-shape",
  "content-cipher",
  "content-signature",
  "carrier-shape",
  "beef",
  "mineable",
  "unlock",
  "lock",
  "signature",
  "funding",
  "unmined",
  "not-bbox"
];
var RecentAdmissions = 4096;
var AdmitKinds = ["envelope", "receipt", "sweep", "spend"];
var Refused = class extends Error {
  constructor(reason, detail) {
    super(`bbox: refused, ${reason}: ${detail}`);
    this.reason = reason;
    this.name = "Refused";
  }
  reason;
};
var engineVerified = {
  isValidRootForHeight: async () => true,
  currentHeight: async () => 0
};
var BboxTopicManager = class {
  constructor(topic2, host, headers = engineVerified, maxBEEF = DefaultMaxBEEF) {
    this.topic = topic2;
    this.host = host;
    this.headers = headers;
    this.maxBEEF = maxBEEF;
    if (!topic2.startsWith(TopicPrefix)) throw new Error(`bbox: ${topic2} is not a tm_bbox_ topic`);
    this.office = topic2.slice(TopicPrefix.length);
    checkOffice(this.office);
    if (!Number.isSafeInteger(maxBEEF) || maxBEEF < DefaultMaxBEEF) throw new Error(`bbox: a BEEF bound of ${maxBEEF} is under the ${DefaultMaxBEEF}-byte floor`);
  }
  topic;
  host;
  headers;
  maxBEEF;
  office;
  async identifyAdmissibleOutputs(beef, previousCoins) {
    try {
      const a = await admit(beef, previousCoins, { office: this.office, headers: this.headers, maxBEEF: this.maxBEEF });
      this.count(a.kind, toHex2(a.txid));
      return { outputsToAdmit: a.outputs, coinsToRetain: a.retain };
    } catch (e) {
      if (!(e instanceof Refusal)) throw e;
      this.host.metrics.inc("bbox_refused_total", { reason: e.reason });
      this.host.log("tm_bbox refused", { topic: this.topic, reason: e.reason, detail: e.message });
      throw new Refused(e.reason, e.message.replace(/^boxrec: /, ""));
    }
  }
  /**
   * Counts an admission once per transaction. The engine shows a manager a
   * txid it already applied only when two submissions of it race past its
   * duplicate check (in the lab: a sweep a client sends to the facade and
   * then directly to the same host), so `bbox_admitted_total` counts
   * distinct transactions and `bbox_admitted_repeats_total` the rest.
   * The memory is the last RecentAdmissions txids.
   */
  count(kind, txid) {
    if (this.recent.has(txid)) {
      this.host.metrics.inc("bbox_admitted_repeats_total", { kind });
      return;
    }
    this.recent.add(txid);
    if (this.recent.size > RecentAdmissions) this.recent.delete(this.recent.values().next().value);
    this.host.metrics.inc("bbox_admitted_total", { kind });
  }
  recent = /* @__PURE__ */ new Set();
  /** Nothing is needed: a carrier carries its funding tree, and a sweep names what it spends. */
  async identifyNeededInputs() {
    return [];
  }
  async getDocumentation() {
    return [
      `# ${this.topic}`,
      "",
      `The bbox office ${this.office}: envelope and receipt carriers, each spending one`,
      "output of its owner's mined funding tree, and the tombstones of mined sweeps,",
      "each admitted on its own validity (spec section 8.1). A transaction that spends",
      "held outputs and claims nothing is accepted for its inputs alone. Every refusal",
      "is raised, so nothing refused is recorded."
    ].join("\n");
  }
  async getMetaData() {
    return { name: this.topic, shortDescription: `Admits the bbox office ${this.office}.` };
  }
};

// src/module.ts
var PruneEvery = 3600;
var TickEvery = 30;
function parseOffices(raw) {
  if (raw === void 0 || raw.trim() === "") throw new Error("BBOX_OFFICES is required: name each office this host carries");
  const out = [];
  for (const entry of raw.split(",")) {
    const office = entry.trim();
    try {
      checkOffice(office);
    } catch {
      throw new Error(`BBOX_OFFICES: "${office}" is not an office identifier <name>_<suffix>`);
    }
    if (out.includes(office)) throw new Error(`BBOX_OFFICES names ${office} twice`);
    out.push(office);
  }
  return out;
}
function integer(name, raw, def, min) {
  if (raw === void 0 || raw.trim() === "") return def;
  const v = raw.trim();
  if (!/^[0-9]+$/.test(v) || !Number.isSafeInteger(Number(v)) || Number(v) < min) throw new Error(`${name}: "${raw}" is not an integer of at least ${min}`);
  return Number(v);
}
function rate(name, raw, def) {
  if (raw === void 0 || raw.trim() === "") return def;
  const v = raw.trim();
  if (!/^[0-9]+(\.[0-9]+)?$/.test(v) || !(Number(v) > 0) || Number(v) > 1e6) throw new Error(`${name}: "${raw}" is not a positive number of at most 1000000`);
  return Number(v);
}
function parseConfig(env) {
  const offices = parseOffices(env["BBOX_OFFICES"]);
  const stateDir = env["BBOX_STATE_DIR"];
  if (stateDir === void 0 || !isAbsolute(stateDir)) {
    throw new Error("BBOX_STATE_DIR is required: an absolute directory kept for the host's life, where the outpoint rows live");
  }
  const c = {
    offices,
    stateDir,
    retentionDays: integer("BBOX_RETENTION_DAYS", env["BBOX_RETENTION_DAYS"], FloorDays, FloorDays),
    maxBEEF: integer("BBOX_MAX_BEEF", env["BBOX_MAX_BEEF"], DefaultMaxBEEF, DefaultMaxBEEF),
    prices: parsePrices2(env["BBOX_PRICES"]),
    sessions: {
      max: integer("BBOX_SESSIONS", env["BBOX_SESSIONS"], DefaultMaxSessions, 1),
      ttlSeconds: integer("BBOX_SESSION_TTL", env["BBOX_SESSION_TTL"], DefaultSessionTTL, 1)
    },
    handshakes: {
      perSec: rate("BBOX_HANDSHAKES_PER_SEC", env["BBOX_HANDSHAKES_PER_SEC"], DefaultBudget.perSec),
      burst: integer("BBOX_HANDSHAKE_BURST", env["BBOX_HANDSHAKE_BURST"], DefaultBudget.burst, 1),
      perAddressPerSec: rate("BBOX_HANDSHAKES_PER_ADDR_PER_SEC", env["BBOX_HANDSHAKES_PER_ADDR_PER_SEC"], DefaultBudget.perAddressPerSec),
      addressBurst: integer("BBOX_HANDSHAKE_ADDR_BURST", env["BBOX_HANDSHAKE_ADDR_BURST"], DefaultBudget.addressBurst, 1)
    },
    responses: {
      perSec: rate("BBOX_RESPONSES_PER_SEC", env["BBOX_RESPONSES_PER_SEC"], DefaultResponseBudget.perSec),
      burst: integer("BBOX_RESPONSE_BURST", env["BBOX_RESPONSE_BURST"], DefaultResponseBudget.burst, 1)
    },
    accept: parseAcceptConfig("BBOX", env)
  };
  const listen = env["BBOX_LISTEN"]?.trim();
  if (listen !== void 0 && listen !== "") {
    const m = /^(.+):([0-9]{1,5})$/.exec(listen);
    const port = m === null ? NaN : Number(m[2]);
    if (m === null || port < 1 || port > 65535) throw new Error(`BBOX_LISTEN: "${listen}" is not host:port`);
    c.listen = { host: m[1].replace(/^\[(.*)\]$/, "$1"), port };
  }
  const key = env["BBOX_PAYEE_KEY"]?.trim();
  if (key !== void 0 && key !== "") {
    if (!/^[0-9a-f]{64}$/.test(key)) throw new Error("BBOX_PAYEE_KEY is not 64 lowercase hex characters");
    c.payeeKey = PrivateKey.fromHex(key);
  }
  const headers = (env["BBOX_HEADERS_URL"] ?? env["OVERLAY_CHAIN_TRACKER_URL"])?.trim();
  if (headers !== void 0 && headers !== "") c.headersURL = headers;
  const charged = [...c.prices.values()].some((p) => p > 0);
  if (charged && (c.listen === void 0 || c.payeeKey === void 0 || c.headersURL === void 0)) {
    throw new Error("BBOX_PRICES prices a class: BBOX_LISTEN, BBOX_PAYEE_KEY and a header source (BBOX_HEADERS_URL or OVERLAY_CHAIN_TRACKER_URL) are required");
  }
  if (c.prices.size > 0 && c.listen === void 0) throw new Error("BBOX_PRICES needs BBOX_LISTEN, where the terms document is served");
  return c;
}
function bboxModule(host, c, overlayTopics, journal) {
  const names = new Set(c.offices.map(topic));
  for (const t of overlayTopics ?? []) {
    if (t.startsWith(TopicPrefix) && !names.has(t)) {
      throw new Error(`OVERLAY_TOPICS names ${t} but BBOX_OFFICES does not configure it; it would be admitted by the default manager`);
    }
  }
  for (const kind of AdmitKinds) host.metrics.preset("bbox_admitted_total", { kind });
  for (const kind of AdmitKinds) host.metrics.preset("bbox_admitted_repeats_total", { kind });
  for (const why of ["cap", "idle"]) host.metrics.preset("bbox_sessions_evicted_total", { why });
  for (const reason of Reasons) host.metrics.preset("bbox_refused_total", { reason });
  for (const why of ["evidence", "retention"]) host.metrics.preset("bbox_dropped_total", { why });
  host.metrics.preset("bbox_retractions_total");
  const priced = new Set([...c.prices].filter(([, p]) => p > 0).map(([name]) => name));
  const ls = new BboxLookupService({
    offices: new Set(c.offices),
    host,
    journal: journal ?? new FileJournal(c.stateDir),
    priced,
    retentionDays: c.retentionDays
  });
  host.metrics.gauge("bbox_offices", () => c.offices.length);
  host.metrics.gauge("bbox_envelopes", () => ls.envelopeCount);
  host.metrics.gauge("bbox_receipts", () => ls.receiptCount);
  host.metrics.gauge("bbox_sweeps", () => ls.sweepCount);
  host.metrics.gauge("bbox_outpoint_rows", () => ls.rowCount);
  host.metrics.gauge("bbox_carrier_lines", () => ls.lineCount);
  const topics = {};
  for (const office of c.offices) topics[topic(office)] = new BboxTopicManager(topic(office), host, void 0, c.maxBEEF);
  let front;
  const headers = c.headersURL === void 0 ? void 0 : new HeaderTracker(c.headersURL);
  const accept = c.accept ?? parseAcceptConfig("BBOX", {});
  const receiver = c.listen !== void 0 && priced.size > 0 ? new LedgerReceiver(c.stateDir, Ledger) : void 0;
  const gate = receiver !== void 0 && headers !== void 0 ? gateFor("bbox", host, headers, accept, c.stateDir) : void 0;
  if (gate !== void 0 && receiver !== void 0) {
    for (const f of receiver.fast) {
      try {
        gate.restore(Transaction5.fromAtomicBEEF(Utils4.toArray(f.beef, "base64")), f.senderIdentityKey, f.satoshis, f.at);
      } catch {
      }
    }
  }
  if (c.listen !== void 0) {
    front = new LookupFront({
      ...bboxRoute(ls),
      host,
      prices: c.prices,
      wallet: c.payeeKey === void 0 ? void 0 : new ProtoWallet(c.payeeKey),
      receiver,
      headers,
      gate,
      sessions: c.sessions,
      budget: c.handshakes,
      responses: c.responses
    });
  }
  const start = async () => {
    let pruned = Date.now();
    let sweeping = false;
    const timer = setInterval(() => {
      if (gate !== void 0 && !sweeping) {
        sweeping = true;
        gate.sweep().catch((e) => host.log("bbox payment watch failed", { err: String(e) })).finally(() => sweeping = false);
      }
      if (!ls.restored || Date.now() - pruned < PruneEvery * 1e3) return;
      pruned = Date.now();
      ls.prune().catch((e) => host.log("ls_bbox retention failed", { err: String(e) }));
    }, TickEvery * 1e3);
    timer.unref();
    if (front === void 0 || c.listen === void 0) return void 0;
    const server = await front.listen(c.listen.port, c.listen.host);
    server.unref();
    return server;
  };
  const build = true ? "@lightwebinc/bbox-host 0.6.4: tm_bbox_<office> and ls_bbox, with @lightwebinc/bcommon 0.24.0 inlined; imports @bsv/sdk (built against 2.7.1)" : "unbundled";
  host.log("bbox module mounted", {
    topics: [...names].join(","),
    lookup: LookupService,
    terms: c.listen === void 0 ? "none" : `${c.listen.host}:${c.listen.port}`,
    priced: [...priced].join(",") || "none",
    sessions: c.listen === void 0 ? "none" : `${c.sessions.max}, idle ${c.sessions.ttlSeconds}s`,
    handshakes: c.listen === void 0 ? "none" : `${c.handshakes.perSec}/s burst ${c.handshakes.burst}, per address ${c.handshakes.perAddressPerSec}/s burst ${c.handshakes.addressBurst}`,
    responses: c.listen === void 0 ? "none" : `${c.responses.perSec}/s burst ${c.responses.burst} a session`,
    payments: gate === void 0 ? "none" : gate.networked ? `fast to ${accept.policy.thresholdSats} sat, payer ${accept.policy.payerLimit}, total ${accept.policy.totalLimit}` : "held until mined (no arcade, or no asset URL or chain)",
    build
  });
  return { module: { topics, lookups: { [LookupService]: ls } }, ls, front, start };
}
async function create(host) {
  const overlay = process.env["OVERLAY_TOPICS"];
  const topics = overlay === void 0 ? void 0 : overlay.split(",").map((t) => t.trim()).filter((t) => t !== "");
  const m = bboxModule(host, parseConfig(process.env), topics);
  await m.start();
  return m.module;
}
export {
  BboxLookupService,
  BboxTopicManager,
  LookupFront,
  Refused,
  bboxModule,
  create as default,
  parseConfig,
  parseOffices,
  termsDocument2 as termsDocument
};
