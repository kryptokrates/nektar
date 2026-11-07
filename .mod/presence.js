// tm_presence / ls_presence — the agent-directory topic.
// Agents "find each other" by publishing a signed OP_RETURN ad:
//   OP_RETURN 'nektar-agent' <pub 33B> <json{endpoint,shards,ts}> <sig DER>
// The signature binds endpoint+shards+ts to the advertiser's key — the same
// key that owns the anchor chain, so 'who serves shard X' is answered by the
// key that anchors X, not by whoever shouts loudest. Ads are 0-sat data
// outputs; latest-by-ts wins per pubkey.
import { PublicKey, Signature, Script, Transaction } from '@bsv/sdk';
import { sha256, utf8, toHex } from '@nektar/core';
const TAG = 'nektar-agent';
const TABLE = 'nektar_ls_presence';
const canonical = (ad) => `${ad.endpoint}|${[...ad.shards].sort().join(',')}|${ad.ts}|${ad.exp ?? ''}`;
const digest = (ad) => sha256(utf8(canonical(ad)));
/** deterministic JSON: object keys sorted recursively, array order kept */
const stableJson = (v) => JSON.stringify(v, (_k, x) => (x !== null && typeof x === 'object' && !Array.isArray(x)
    ? Object.fromEntries(Object.keys(x).sort().map(k => [k, x[k]]))
    : x));
const catalogCanonical = (c) => `${c.pub}|${c.ts}|${stableJson(c.shards)}`;
const catalogDigest = (c) => sha256(utf8(catalogCanonical(c)));
const DOMAIN_RE = /^[a-z0-9][a-z0-9._-]{0,47}$/;
const validEntry = (e) => typeof e?.shard === 'string' && e.shard.length > 0 && e.shard.length <= 64 &&
    Array.isArray(e.domains) && e.domains.length > 0 && e.domains.length <= 32 &&
    e.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d)) &&
    (e.desc === undefined || (typeof e.desc === 'string' && e.desc.length <= 560)) &&
    (e.shardId === undefined || (typeof e.shardId === 'string' && /^[0-9a-f]{40}$/i.test(e.shardId))) &&
    (e.offers === undefined || (Array.isArray(e.offers) && e.offers.length <= 64 &&
        e.offers.every(o => o && (o.kind === 'answer' || o.kind === 'pack' || o.kind === 'record'))));
export function signCatalog(key, fields) {
    const catalog = { ...fields, pub: key.toPublicKey().toString() };
    return { catalog, sig: toHex(Uint8Array.from(key.sign(Array.from(catalogDigest(catalog))).toDER())) };
}
export function verifyCatalog(catalog, sigHex) {
    try {
        if (typeof catalog?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(catalog.pub))
            return false;
        if (!Number.isSafeInteger(catalog.ts) || catalog.ts <= 0)
            return false;
        if (catalog.endpoint !== undefined && (typeof catalog.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(catalog.endpoint)))
            return false;
        if (!Array.isArray(catalog.shards) || catalog.shards.length === 0 || catalog.shards.length > 64 || !catalog.shards.every(validEntry))
            return false;
        return PublicKey.fromString(catalog.pub).verify(Array.from(catalogDigest(catalog)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
const wantCanonical = (w) => `${[...w.domains].sort().join(',')}|${w.question ?? ''}|${w.maxSats ?? ''}|${w.endpoint ?? ''}|${w.ts}|${w.exp ?? ''}`;
const wantDigest = (w) => sha256(utf8(wantCanonical(w)));
export function signWant(key, fields) {
    const want = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() };
    return { want, sig: toHex(Uint8Array.from(key.sign(Array.from(wantDigest(want))).toDER())) };
}
export function verifyWant(want, sigHex) {
    try {
        if (typeof want?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(want.pub))
            return false;
        if (!Array.isArray(want.domains) || want.domains.length === 0 || want.domains.length > 32 || !want.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d)))
            return false;
        if (want.question !== undefined && (typeof want.question !== 'string' || want.question.length > 560))
            return false;
        if (want.maxSats !== undefined && (!Number.isSafeInteger(want.maxSats) || want.maxSats < 0))
            return false;
        if (want.endpoint !== undefined && (typeof want.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(want.endpoint)))
            return false;
        if (!Number.isSafeInteger(want.ts) || want.ts <= 0)
            return false;
        if (want.exp !== undefined && (!Number.isSafeInteger(want.exp) || want.exp <= Date.now()))
            return false;
        return PublicKey.fromString(want.pub).verify(Array.from(wantDigest(want)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
const offerCanonical = (o) => `${o.shard}|${o.seq}|${o.shardId ?? ''}|${o.leaf ?? ''}|${o.hdp ?? ''}|${o.batchR ?? ''}|${o.priceSats}|${[...o.domains].sort().join(',')}|${o.ts}|${o.exp ?? ''}|${o.replyTo ?? ''}`;
const offerDigest = (o) => sha256(utf8(offerCanonical(o)));
const offerKey = (o) => `${o.pub}:${o.shard}:${o.seq}`;
export function signOffer(key, fields) {
    const offer = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() };
    return { offer, sig: toHex(Uint8Array.from(key.sign(Array.from(offerDigest(offer))).toDER())) };
}
export function verifyOffer(offer, sigHex) {
    try {
        if (typeof offer?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(offer.pub))
            return false;
        if (typeof offer.shard !== 'string' || offer.shard.length === 0 || offer.shard.length > 64)
            return false;
        if (!Number.isSafeInteger(offer.seq) || offer.seq < 0)
            return false;
        if (!Number.isSafeInteger(offer.priceSats) || offer.priceSats < 0)
            return false;
        if (!Array.isArray(offer.domains) || offer.domains.length > 32 || !offer.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d)))
            return false;
        for (const h of [offer.shardId, offer.leaf, offer.hdp, offer.batchR])
            if (h !== undefined && (typeof h !== 'string' || !/^[0-9a-f]{40,64}$/i.test(h)))
                return false;
        if (!Number.isSafeInteger(offer.ts) || offer.ts <= 0)
            return false;
        if (offer.exp !== undefined && (!Number.isSafeInteger(offer.exp) || offer.exp <= Date.now()))
            return false;
        return PublicKey.fromString(offer.pub).verify(Array.from(offerDigest(offer)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
const settlementCanonical = (s) => `${s.invoiceId}|${s.tool}|${s.sats}|${s.txid}|${[...s.domains].sort().join(',')}|${s.ts}`;
const settlementDigest = (s) => sha256(utf8(settlementCanonical(s)));
export function signSettlement(key, fields) {
    const ad = { ...fields, domains: [...fields.domains].sort(), pub: key.toPublicKey().toString() };
    return { ad, sig: toHex(Uint8Array.from(key.sign(Array.from(settlementDigest(ad))).toDER())) };
}
export function verifySettlement(ad, sigHex) {
    try {
        if (typeof ad?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(ad.pub))
            return false;
        if (typeof ad.invoiceId !== 'string' || ad.invoiceId.length < 4 || ad.invoiceId.length > 64)
            return false;
        if (typeof ad.txid !== 'string' || !/^[0-9a-f]{64}$/i.test(ad.txid))
            return false;
        if (!Number.isSafeInteger(ad.sats) || ad.sats < 0)
            return false;
        if (typeof ad.tool !== 'string' || ad.tool.length === 0 || ad.tool.length > 48)
            return false;
        if (!Array.isArray(ad.domains) || !ad.domains.every(d => typeof d === 'string' && DOMAIN_RE.test(d)))
            return false;
        if (!Number.isSafeInteger(ad.ts) || ad.ts <= 0)
            return false;
        return PublicKey.fromString(ad.pub).verify(Array.from(settlementDigest(ad)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
const repCanonical = (c) => `${c.shardId ?? ''}|${JSON.stringify(c.entries)}|${c.since ?? ''}|${c.asOf}`;
const repDigest = (c) => sha256(utf8(repCanonical(c)));
export function signReputation(key, fields) {
    const card = { ...fields, pub: key.toPublicKey().toString() };
    return { card, sig: toHex(Uint8Array.from(key.sign(Array.from(repDigest(card))).toDER())) };
}
export function verifyReputationCard(card, sigHex) {
    try {
        if (typeof card?.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(card.pub))
            return false;
        if (!Array.isArray(card.entries) || card.entries.length > 256 || !card.entries.every(e => e && typeof e.domain === 'string' && DOMAIN_RE.test(e.domain)))
            return false;
        if (!Number.isSafeInteger(card.asOf) || card.asOf <= 0)
            return false;
        return PublicKey.fromString(card.pub).verify(Array.from(repDigest(card)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
export function signPresence(key, fields) {
    const ad = { ...fields, shards: [...fields.shards].sort(), pub: key.toPublicKey().toString() };
    return { ad, sig: toHex(Uint8Array.from(key.sign(Array.from(digest(ad))).toDER())) };
}
export function verifyPresence(ad, sigHex) {
    try {
        if (typeof ad.pub !== 'string' || !/^[0-9a-f]{66}$/i.test(ad.pub))
            return false;
        if (typeof ad.endpoint !== 'string' || !/^https?:\/\/\S{2,200}$/.test(ad.endpoint))
            return false;
        if (!Array.isArray(ad.shards) || ad.shards.length > 64 || !ad.shards.every(s => typeof s === 'string' && s.length > 0 && s.length <= 64))
            return false;
        if (!Number.isSafeInteger(ad.ts) || ad.ts <= 0)
            return false;
        if (ad.exp !== undefined && (!Number.isSafeInteger(ad.exp) || ad.exp <= Date.now()))
            return false; // expired
        return PublicKey.fromString(ad.pub).verify(Array.from(digest(ad)), Signature.fromDER(sigHex, 'hex'));
    }
    catch {
        return false;
    }
}
export function presenceOutputScript({ ad, sig }) {
    const payload = JSON.stringify({ endpoint: ad.endpoint, shards: ad.shards, ts: ad.ts, ...(ad.exp !== undefined ? { exp: ad.exp } : {}) });
    return Script.fromASM(`OP_RETURN ${toHex(utf8(TAG))} ${ad.pub} ${toHex(utf8(payload))} ${sig}`).toHex();
}
const CAT_TAG = 'nektar-catalog';
const WANT_TAG = 'nektar-want';
const OFFER_TAG = 'nektar-offer';
/** on-chain catalog — same 4-push shape as presence ads; the payload carries
 *  everything except pub (push 1) so the canonical digest rebuilds. 0-sat
 *  data outputs ride tm_presence / the object plane like any admitted tx. */
export function catalogOutputScript({ catalog, sig }) {
    const payload = JSON.stringify({ ...(catalog.endpoint !== undefined ? { endpoint: catalog.endpoint } : {}), shards: catalog.shards, ts: catalog.ts });
    return Script.fromASM(`OP_RETURN ${toHex(utf8(CAT_TAG))} ${catalog.pub} ${toHex(utf8(payload))} ${sig}`).toHex();
}
export function wantOutputScript({ want, sig }) {
    const payload = JSON.stringify({ domains: want.domains, ...(want.question !== undefined ? { question: want.question } : {}), ...(want.maxSats !== undefined ? { maxSats: want.maxSats } : {}), ...(want.endpoint !== undefined ? { endpoint: want.endpoint } : {}), ts: want.ts, ...(want.exp !== undefined ? { exp: want.exp } : {}) });
    return Script.fromASM(`OP_RETURN ${toHex(utf8(WANT_TAG))} ${want.pub} ${toHex(utf8(payload))} ${sig}`).toHex();
}
export function offerOutputScript({ offer, sig }) {
    const payload = JSON.stringify({ shard: offer.shard, seq: offer.seq, ...(offer.shardId ? { shardId: offer.shardId } : {}), ...(offer.leaf ? { leaf: offer.leaf } : {}), ...(offer.hdp ? { hdp: offer.hdp } : {}), ...(offer.batchR ? { batchR: offer.batchR } : {}), priceSats: offer.priceSats, domains: offer.domains, ...(offer.offer ? { offer: offer.offer } : {}), ts: offer.ts, ...(offer.exp !== undefined ? { exp: offer.exp } : {}) });
    return Script.fromASM(`OP_RETURN ${toHex(utf8(OFFER_TAG))} ${offer.pub} ${toHex(utf8(payload))} ${sig}`).toHex();
}
// The SDK's Script.chunks collapses OP_RETURN data into one chunk — decode the
// pushes ourselves. Everything after the leading 0x6a must be pushdata or it
// isn't our format.
function readPushes(hex) {
    const b = Buffer.from(hex, 'hex');
    if (b[0] !== 0x6a)
        return null;
    const out = [];
    let i = 1;
    while (i < b.length) {
        const op = b[i++];
        let len;
        if (op === 0) {
            out.push(Buffer.alloc(0));
            continue;
        }
        if (op <= 75)
            len = op;
        else if (op === 0x4c) {
            len = b[i] ?? -1;
            i += 1;
        }
        else if (op === 0x4d) {
            len = i + 2 <= b.length ? b.readUInt16LE(i) : -1;
            i += 2;
        }
        else if (op === 0x4e) {
            len = i + 4 <= b.length ? b.readUInt32LE(i) : -1;
            i += 4;
        }
        else
            return null;
        if (len < 0 || i + len > b.length)
            return null;
        out.push(Buffer.from(b.subarray(i, i + len)));
        i += len;
    }
    return out;
}
export function parsePresence(scriptHex) {
    try {
        const pushes = readPushes(scriptHex);
        if (!pushes || pushes.length !== 4)
            return null;
        if (pushes[0].toString() !== TAG)
            return null;
        const pub = pushes[1].toString('hex');
        const p = JSON.parse(pushes[2].toString());
        const ad = { pub, endpoint: p.endpoint, shards: p.shards, ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) };
        return { ad, sig: pushes[3].toString('hex') };
    }
    catch {
        return null;
    }
}
export function parseCatalogOutput(scriptHex) {
    try {
        const pushes = readPushes(scriptHex);
        if (!pushes || pushes.length !== 4)
            return null;
        if (pushes[0].toString() !== CAT_TAG)
            return null;
        const pub = pushes[1].toString('hex');
        const p = JSON.parse(pushes[2].toString());
        const catalog = { pub, ...(p.endpoint !== undefined ? { endpoint: p.endpoint } : {}), shards: p.shards, ts: p.ts };
        return { catalog, sig: pushes[3].toString('hex') };
    }
    catch {
        return null;
    }
}
export function parseWantOutput(scriptHex) {
    try {
        const pushes = readPushes(scriptHex);
        if (!pushes || pushes.length !== 4)
            return null;
        if (pushes[0].toString() !== WANT_TAG)
            return null;
        const pub = pushes[1].toString('hex');
        const p = JSON.parse(pushes[2].toString());
        const want = { pub, domains: p.domains, ...(p.question !== undefined ? { question: p.question } : {}), ...(p.maxSats !== undefined ? { maxSats: p.maxSats } : {}), ...(p.endpoint !== undefined ? { endpoint: p.endpoint } : {}), ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) };
        return { want, sig: pushes[3].toString('hex') };
    }
    catch {
        return null;
    }
}
export function parseOfferOutput(scriptHex) {
    try {
        const pushes = readPushes(scriptHex);
        if (!pushes || pushes.length !== 4)
            return null;
        if (pushes[0].toString() !== OFFER_TAG)
            return null;
        const pub = pushes[1].toString('hex');
        const p = JSON.parse(pushes[2].toString());
        const offer = { pub, shard: p.shard, seq: p.seq, ...(p.shardId ? { shardId: p.shardId } : {}), ...(p.leaf ? { leaf: p.leaf } : {}), ...(p.hdp ? { hdp: p.hdp } : {}), ...(p.batchR ? { batchR: p.batchR } : {}), priceSats: p.priceSats, domains: p.domains, ...(p.offer ? { offer: p.offer } : {}), ts: p.ts, ...(p.exp !== undefined ? { exp: p.exp } : {}) };
        return { offer, sig: pushes[3].toString('hex') };
    }
    catch {
        return null;
    }
}
export function parseAnyPresence(scriptHex) {
    const ad = parsePresence(scriptHex);
    if (ad)
        return { kind: 'ad', ad };
    const sc = parseCatalogOutput(scriptHex);
    if (sc)
        return { kind: 'catalog', sc };
    const sw = parseWantOutput(scriptHex);
    if (sw)
        return { kind: 'want', sw };
    const so = parseOfferOutput(scriptHex);
    if (so)
        return { kind: 'offer', so };
    return null;
}
export function createTmPresence() {
    return {
        identifyAdmissibleOutputs: async (beef, _previousCoins) => {
            const tx = Transaction.fromBEEF(beef);
            const admit = { outputsToAdmit: [], coinsToRetain: [] };
            for (const [i, o] of tx.outputs.entries()) {
                const hex = o.lockingScript.toHex();
                const p = parseAnyPresence(hex);
                if (p === null)
                    continue;
                const ok = p.kind === 'ad' ? verifyPresence(p.ad.ad, p.ad.sig)
                    : p.kind === 'catalog' ? verifyCatalog(p.sc.catalog, p.sc.sig)
                        : p.kind === 'want' ? verifyWant(p.sw.want, p.sw.sig)
                            : verifyOffer(p.so.offer, p.so.sig);
                if (ok)
                    admit.outputsToAdmit.push(i);
            }
            return admit;
        },
        identifyNeededInputs: async () => [],
        getDocumentation: async () => "Admits OP_RETURN outputs 'nektar-agent'|'nektar-catalog'|'nektar-want' <pub> <payload> <sig> where sig is a valid signature by pub over that kind's canonical digest. Stateless — no chain ordering required; latest-by-ts wins per kind in the lookup service.",
        getMetaData: async () => ({ name: 'tm_presence', shortDescription: 'nektar agent-directory: signed presence/catalog/want ads' })
    };
}
export function createLsPresence(kx) {
    // latest ad per pub; shard → set of pubs advertising it
    const ads = new Map();
    const shardIndex = new Map();
    // latest signed catalog per pub; domain tag → set of pubs claiming it
    const catalogs = new Map();
    const domainIndex = new Map();
    // latest signed want per pub
    const wants = new Map();
    // live offers keyed pub:shard:seq (one seller lists many records)
    const offers = new Map();
    // signed settlement announcements (append — history IS the price feed)
    const settlements = [];
    // latest reputation card per pub
    const reps = new Map();
    // the durable event log — seq is the consumer cursor
    const events = [];
    let nextSeq = 0;
    const appendEvent = (kind, pub, domains, payload) => {
        const e = { seq: nextSeq++, kind, pub, domains, ts: Date.now(), payload };
        events.push(e);
        if (kx)
            void (async () => { if (await kx.schema.hasTable(EVENTS_TABLE))
                await kx(EVENTS_TABLE).insert({ seq: e.seq, kind: e.kind, pub: e.pub, domains: JSON.stringify(e.domains), ts: e.ts, payload: JSON.stringify(e.payload) }); })().catch(() => { });
        return e;
    };
    const EVENTS_TABLE = 'nektar_ls_events';
    const WANTS_TABLE = 'nektar_ls_wants';
    const reindex = (pub, ad, prev) => {
        if (prev)
            for (const s of prev.shards) {
                const set = shardIndex.get(s);
                set?.delete(pub);
                if (set && set.size === 0)
                    shardIndex.delete(s);
            }
        if (ad)
            for (const s of ad.shards) {
                const set = shardIndex.get(s) ?? new Set();
                set.add(pub);
                shardIndex.set(s, set);
            }
    };
    const reindexCatalog = (pub, cat, prev) => {
        const doms = (c) => new Set((c?.shards ?? []).flatMap(e => e.domains.map(d => d.toLowerCase())));
        if (prev)
            for (const d of doms(prev)) {
                const set = domainIndex.get(d);
                set?.delete(pub);
                if (set && set.size === 0)
                    domainIndex.delete(d);
            }
        if (cat)
            for (const d of doms(cat)) {
                const set = domainIndex.get(d) ?? new Set();
                set.add(pub);
                domainIndex.set(d, set);
            }
    };
    const persist = async (s) => {
        if (!kx)
            return;
        const row = { pub: s.ad.pub, txid: s.txid, output_index: s.outputIndex, endpoint: s.ad.endpoint, shards: JSON.stringify(s.ad.shards), ts: s.ad.ts };
        if (await kx.schema.hasTable(TABLE))
            await kx(TABLE).insert(row).onConflict('pub').merge(row);
    };
    const unpersist = async (txid, vout) => {
        if (!kx || !(await kx.schema.hasTable(TABLE)))
            return;
        await kx(TABLE).where({ txid, output_index: vout }).del();
    };
    const CAT_TABLE = 'nektar_ls_catalog';
    const persistCatalog = async (sc) => {
        if (!kx || !(await kx.schema.hasTable(CAT_TABLE)))
            return;
        const row = { pub: sc.catalog.pub, ts: sc.catalog.ts, catalog: JSON.stringify(sc.catalog), sig: sc.sig };
        await kx(CAT_TABLE).insert(row).onConflict('pub').merge(row);
    };
    const persistWant = async (sw) => {
        if (!kx || !(await kx.schema.hasTable(WANTS_TABLE)))
            return;
        const row = { pub: sw.want.pub, ts: sw.want.ts, want: JSON.stringify(sw.want), sig: sw.sig };
        await kx(WANTS_TABLE).insert(row).onConflict('pub').merge(row);
    };
    const OFFERS_TABLE = 'nektar_ls_offers';
    const persistOffer = async (so) => {
        if (!kx || !(await kx.schema.hasTable(OFFERS_TABLE)))
            return;
        const row = { key: offerKey(so.offer), pub: so.offer.pub, ts: so.offer.ts, offer: JSON.stringify(so.offer), sig: so.sig };
        await kx(OFFERS_TABLE).insert(row).onConflict('key').merge(row);
    };
    const SETTLE_TABLE = 'nektar_ls_settlements';
    const persistSettlement = async (s) => {
        if (!kx || !(await kx.schema.hasTable(SETTLE_TABLE)))
            return;
        await kx(SETTLE_TABLE).insert({ pub: s.ad.pub, invoice_id: s.ad.invoiceId, txid: s.ad.txid, tool: s.ad.tool, sats: s.ad.sats, domains: JSON.stringify(s.ad.domains), ts: s.ad.ts, ad: JSON.stringify(s.ad), sig: s.sig });
    };
    const REPS_TABLE = 'nektar_ls_reputation';
    const persistRep = async (sr) => {
        if (!kx || !(await kx.schema.hasTable(REPS_TABLE)))
            return;
        const row = { pub: sr.card.pub, as_of: sr.card.asOf, card: JSON.stringify(sr.card), sig: sr.sig };
        await kx(REPS_TABLE).insert(row).onConflict('pub').merge(row);
    };
    const formula = (s) => ({
        txid: s.txid, outputIndex: s.outputIndex,
        context: [...utf8(JSON.stringify(s.ad))]
    });
    return {
        admissionMode: 'locking-script',
        spendNotificationMode: 'txid',
        init: async () => {
            if (!kx)
                return;
            if (!(await kx.schema.hasTable(TABLE))) {
                await kx.schema.createTable(TABLE, t => {
                    t.string('pub').primary();
                    t.string('txid').notNullable();
                    t.integer('output_index').notNullable();
                    t.string('endpoint').notNullable();
                    t.string('shards').notNullable();
                    t.bigInteger('ts').notNullable();
                });
            }
            for (const r of await kx(TABLE).select('*')) {
                const s = { ad: { pub: r.pub, endpoint: r.endpoint, shards: JSON.parse(r.shards), ts: Number(r.ts) }, txid: r.txid, outputIndex: r.output_index };
                ads.set(r.pub, s);
                reindex(r.pub, s.ad);
            }
            if (!(await kx.schema.hasTable(CAT_TABLE))) {
                await kx.schema.createTable(CAT_TABLE, t => {
                    t.string('pub').primary();
                    t.bigInteger('ts').notNullable();
                    t.string('catalog').notNullable();
                    t.string('sig').notNullable();
                });
            }
            for (const r of await kx(CAT_TABLE).select('*')) {
                const sc = { catalog: JSON.parse(r.catalog), sig: r.sig };
                const prev = catalogs.get(r.pub);
                if (prev && prev.sc.catalog.ts >= sc.catalog.ts)
                    continue;
                catalogs.set(r.pub, { sc });
                reindexCatalog(r.pub, sc.catalog, prev?.sc.catalog);
            }
            if (!(await kx.schema.hasTable(WANTS_TABLE))) {
                await kx.schema.createTable(WANTS_TABLE, t => {
                    t.string('pub').primary();
                    t.bigInteger('ts').notNullable();
                    t.string('want').notNullable();
                    t.string('sig').notNullable();
                });
            }
            for (const r of await kx(WANTS_TABLE).select('*')) {
                const sw = { want: JSON.parse(r.want), sig: r.sig };
                const prev = wants.get(r.pub);
                if (!prev || prev.want.ts < sw.want.ts)
                    wants.set(r.pub, sw);
            }
            if (!(await kx.schema.hasTable(EVENTS_TABLE))) {
                await kx.schema.createTable(EVENTS_TABLE, t => {
                    t.integer('seq').primary();
                    t.string('kind').notNullable();
                    t.string('pub').notNullable();
                    t.string('domains').notNullable();
                    t.bigInteger('ts').notNullable();
                    t.string('payload').notNullable();
                });
            }
            for (const r of await kx(EVENTS_TABLE).select('*').orderBy('seq')) {
                events.push({ seq: Number(r.seq), kind: r.kind, pub: r.pub, domains: JSON.parse(r.domains), ts: Number(r.ts), payload: JSON.parse(r.payload) });
            }
            nextSeq = events.length ? events.at(-1).seq + 1 : 0;
            if (!(await kx.schema.hasTable(OFFERS_TABLE))) {
                await kx.schema.createTable(OFFERS_TABLE, t => {
                    t.string('key').primary();
                    t.string('pub').notNullable();
                    t.bigInteger('ts').notNullable();
                    t.string('offer').notNullable();
                    t.string('sig').notNullable();
                });
            }
            for (const r of await kx(OFFERS_TABLE).select('*')) {
                const so = { offer: JSON.parse(r.offer), sig: r.sig };
                const prev = offers.get(r.key);
                if (!prev || prev.offer.ts < so.offer.ts)
                    offers.set(r.key, so);
            }
            if (!(await kx.schema.hasTable(SETTLE_TABLE))) {
                await kx.schema.createTable(SETTLE_TABLE, t => {
                    t.increments('id');
                    t.string('pub').notNullable();
                    t.string('invoice_id').notNullable();
                    t.string('txid').notNullable();
                    t.string('tool').notNullable();
                    t.integer('sats').notNullable();
                    t.string('domains').notNullable();
                    t.bigInteger('ts').notNullable();
                    t.string('ad').notNullable();
                    t.string('sig').notNullable();
                });
            }
            for (const r of await kx(SETTLE_TABLE).select('*'))
                settlements.push({ ad: JSON.parse(r.ad), sig: r.sig });
            if (!(await kx.schema.hasTable(REPS_TABLE))) {
                await kx.schema.createTable(REPS_TABLE, t => {
                    t.string('pub').primary();
                    t.bigInteger('as_of').notNullable();
                    t.string('card').notNullable();
                    t.string('sig').notNullable();
                });
            }
            for (const r of await kx(REPS_TABLE).select('*')) {
                const sr = { card: JSON.parse(r.card), sig: r.sig };
                const prev = reps.get(r.pub);
                if (!prev || prev.card.asOf < sr.card.asOf)
                    reps.set(r.pub, sr);
            }
        },
        outputAdmittedByTopic: (payload) => {
            if (payload.mode !== 'locking-script')
                return;
            const hex = typeof payload.lockingScript === 'string' ? payload.lockingScript : payload.lockingScript?.toHex?.() ?? '';
            const p = parseAnyPresence(hex);
            if (!p)
                return;
            if (p.kind === 'ad') {
                const parsed = p.ad;
                if (!verifyPresence(parsed.ad, parsed.sig))
                    return;
                const prev = ads.get(parsed.ad.pub);
                if (prev && prev.ad.ts >= parsed.ad.ts)
                    return; // stale ad — latest wins
                const s = { ad: parsed.ad, txid: payload.txid, outputIndex: payload.outputIndex };
                ads.set(parsed.ad.pub, s);
                reindex(parsed.ad.pub, parsed.ad, prev?.ad);
                void persist(s);
                appendEvent('ad', parsed.ad.pub, parsed.ad.shards, parsed);
                return;
            }
            if (p.kind === 'catalog') {
                const sc = p.sc;
                if (!verifyCatalog(sc.catalog, sc.sig))
                    return;
                const prev = catalogs.get(sc.catalog.pub);
                if (prev && prev.sc.catalog.ts >= sc.catalog.ts)
                    return;
                catalogs.set(sc.catalog.pub, { sc });
                reindexCatalog(sc.catalog.pub, sc.catalog, prev?.sc.catalog);
                void persistCatalog(sc);
                appendEvent('catalog', sc.catalog.pub, sc.catalog.shards.flatMap(e => e.domains), sc);
                return;
            }
            if (p.kind === 'want') {
                const sw = p.sw;
                if (!verifyWant(sw.want, sw.sig))
                    return;
                const prevW = wants.get(sw.want.pub);
                if (prevW && prevW.want.ts >= sw.want.ts)
                    return;
                wants.set(sw.want.pub, sw);
                void persistWant(sw);
                appendEvent('want', sw.want.pub, sw.want.domains, sw);
                return;
            }
            const so = p.so;
            if (!verifyOffer(so.offer, so.sig))
                return;
            const ok_ = offerKey(so.offer);
            const prevO = offers.get(ok_);
            if (prevO && prevO.offer.ts >= so.offer.ts)
                return;
            offers.set(ok_, so);
            void persistOffer(so);
            appendEvent('offer', so.offer.pub, so.offer.domains, so);
        },
        // Off-chain rendezvous: same verification, same latest-wins rule, no UTXO.
        // Synthetic outpoint = sha256 of the ad (32B hex — the Engine validates
        // output txids); a newer ad (on- or off-chain) replaces it by ts.
        announce: async (s) => {
            if (!verifyPresence(s.ad, s.sig))
                return false;
            const prev = ads.get(s.ad.pub);
            if (prev && prev.ad.ts >= s.ad.ts)
                return true;
            const rec = { ad: s.ad, txid: toHex(digest(s.ad)), outputIndex: 0 };
            ads.set(s.ad.pub, rec);
            reindex(s.ad.pub, s.ad, prev?.ad);
            await persist(rec);
            appendEvent('ad', s.ad.pub, s.ad.shards, s);
            return true;
        },
        // Signed catalog — same off-channel path as announce(): verified, latest-
        // wins per pub. The directory's content index: domain tag → claimants.
        announceCatalog: async (sc) => {
            if (!verifyCatalog(sc.catalog, sc.sig))
                return false;
            const prev = catalogs.get(sc.catalog.pub);
            if (prev && prev.sc.catalog.ts >= sc.catalog.ts)
                return true;
            catalogs.set(sc.catalog.pub, { sc });
            reindexCatalog(sc.catalog.pub, sc.catalog, prev?.sc.catalog);
            await persistCatalog(sc);
            appendEvent('catalog', sc.catalog.pub, sc.catalog.shards.flatMap(e => e.domains), sc);
            return true;
        },
        // Signed want — the demand side. Latest-per-pub wins; exp honoured at
        // read time. Every accepted want lands on the event log so agents
        // subscribed to its domains see it on their next cursor pull.
        announceWant: async (sw) => {
            if (!verifyWant(sw.want, sw.sig))
                return false;
            const prev = wants.get(sw.want.pub);
            if (prev && prev.want.ts >= sw.want.ts)
                return true;
            wants.set(sw.want.pub, sw);
            await persistWant(sw);
            appendEvent('want', sw.want.pub, sw.want.domains, sw);
            return true;
        },
        announceOffer: async (so) => {
            if (!verifyOffer(so.offer, so.sig))
                return false;
            const k = offerKey(so.offer);
            const prev = offers.get(k);
            if (prev && prev.offer.ts >= so.offer.ts)
                return true;
            offers.set(k, so);
            await persistOffer(so);
            appendEvent('offer', so.offer.pub, so.offer.domains, so);
            return true;
        },
        announceSettlement: async (s) => {
            if (!verifySettlement(s.ad, s.sig))
                return false;
            if (settlements.some(x => x.ad.pub === s.ad.pub && x.ad.invoiceId === s.ad.invoiceId))
                return true; // idempotent
            settlements.push(s);
            await persistSettlement(s);
            appendEvent('settlement', s.ad.pub, s.ad.domains, s);
            return true;
        },
        announceReputation: async (sr) => {
            if (!verifyReputationCard(sr.card, sr.sig))
                return false;
            const prev = reps.get(sr.card.pub);
            if (prev && prev.card.asOf >= sr.card.asOf)
                return true;
            reps.set(sr.card.pub, sr);
            await persistRep(sr);
            appendEvent('reputation', sr.card.pub, sr.card.entries.map(e => e.domain), sr);
            return true;
        },
        catalogFor: (pub) => catalogs.get(pub)?.sc,
        wantFor: (pub) => wants.get(pub),
        offersFor: (domain) => [...offers.values()].filter(so => so.offer.exp === undefined || so.offer.exp > Date.now()).filter(so => domain === undefined || so.offer.domains.some(d => d.toLowerCase() === domain.toLowerCase())),
        reputationFor: (pub) => reps.get(pub),
        reputationsFor: (domain) => [...reps.values()].filter(sr => domain === undefined || sr.card.entries.some(e => e.domain === domain.toLowerCase())),
        // realized-price index: settled sales per (domain, tool) — count + sats
        // distribution. Signed announcements; txids checkable on-chain.
        prices: (q) => {
            const dom = q.domain?.toLowerCase();
            const rows = settlements.filter(s => (dom === undefined || s.ad.domains.some(d => d.toLowerCase() === dom)) &&
                (q.tool === undefined || s.ad.tool === q.tool));
            const sats = rows.map(r => r.ad.sats).sort((a, b) => a - b);
            return {
                count: rows.length,
                sats,
                minSats: sats[0] ?? null,
                maxSats: sats.at(-1) ?? null,
                medianSats: sats.length ? sats[Math.floor(sats.length / 2)] : null,
                totalSats: sats.reduce((a, b) => a + b, 0),
                settlements: rows.map(r => r.ad)
            };
        },
        // JetStream-pull view: events after `since` cursor, optionally narrowed
        // by kind and/or domain tags (a consumer subscribed to 'soccer' sees any
        // event whose domains contain it).
        feed: async (q) => {
            const since = q.since ?? -1;
            const doms = q.domains?.length ? new Set(q.domains.map(d => d.toLowerCase())) : undefined;
            const out = [];
            for (const e of events) {
                if (e.seq <= since)
                    continue;
                if (q.kind && e.kind !== q.kind)
                    continue;
                if (doms && !e.domains.some(d => doms.has(d.toLowerCase())))
                    continue;
                out.push(e);
                if (q.limit !== undefined && out.length >= q.limit)
                    break;
            }
            return out;
        },
        outputSpent: (payload) => {
            if (payload.mode === 'none')
                return;
            for (const [pub, s] of [...ads]) {
                if (s.txid === payload.txid && s.outputIndex === payload.outputIndex) {
                    ads.delete(pub);
                    reindex(pub, undefined, s.ad);
                    void unpersist(s.txid, s.outputIndex);
                }
            }
        },
        outputEvicted: (txid, outputIndex) => {
            for (const [pub, s] of [...ads]) {
                if (s.txid === txid && s.outputIndex === outputIndex) {
                    ads.delete(pub);
                    reindex(pub, undefined, s.ad);
                    void unpersist(s.txid, s.outputIndex);
                }
            }
        },
        lookup: async ({ query }) => {
            const live = (s) => s.ad.exp === undefined || s.ad.exp > Date.now();
            const q = (query ?? {});
            if (typeof q.domain === 'string') {
                // content query: domain tag → claimants' signed catalogs (context =
                // the signed catalog JSON so callers verify the sig themselves)
                const pubs = domainIndex.get(q.domain.toLowerCase()) ?? new Set();
                return [...pubs].map(p => catalogs.get(p).sc).filter(sc => { const a = ads.get(sc.catalog.pub); return a === undefined || live(a); })
                    .map(sc => ({ txid: toHex(catalogDigest(sc.catalog)), outputIndex: 0, context: [...utf8(JSON.stringify(sc))] }));
            }
            if (typeof q.q === 'string') {
                // free-text: substring over each entry's domains + description
                const needle = q.q.toLowerCase();
                const matches = (e) => e.domains.some(d => d.toLowerCase().includes(needle)) || e.desc?.toLowerCase().includes(needle);
                return [...catalogs.values()].map(c => c.sc)
                    .filter(sc => sc.catalog.shards.some(matches))
                    .filter(sc => (ads.get(sc.catalog.pub)?.ad.exp ?? Infinity) > Date.now())
                    .map(sc => ({ txid: toHex(catalogDigest(sc.catalog)), outputIndex: 0, context: [...utf8(JSON.stringify(sc))] }));
            }
            if (q.all === true)
                return [...ads.values()].filter(live).map(formula);
            if (typeof q.shard === 'string')
                return [...(shardIndex.get(q.shard) ?? [])].map(p => ads.get(p)).filter(live).map(formula);
            if (typeof q.pub === 'string') {
                const s = ads.get(q.pub);
                return s && live(s) ? [formula(s)] : [];
            }
            return [];
        },
        getDocumentation: async () => "nektar agent-directory lookup. Queries: {all:true} → latest signed ad per agent; {shard:'x'} → ads of agents serving shard x; {pub:'<66hex>'} → one agent's ad; {domain:'soccer'} → signed catalogs of agents claiming that domain tag; {q:'text'} → free-text match over catalog domains+descriptions. Context = the ad JSON (or the signed catalog for domain/q).",
        getMetaData: async () => ({ name: 'ls_presence', shortDescription: 'nektar agent-directory lookup service' })
    };
}
