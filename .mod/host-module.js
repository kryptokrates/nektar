import { createTmPresence, createLsPresence } from './presence.js';
import { Script } from '@bsv/sdk';
const body = (req) => new Promise((resolve, reject) => {
    const chunks = [];
    req.on('data', (c) => chunks.push(c));
    req.on('end', () => resolve(Buffer.concat(chunks).toString('utf8')));
    req.on('error', reject);
});
const json = (res, status, obj) => {
    res.writeHead(status, { 'content-type': 'application/json' });
    res.end(JSON.stringify(obj));
};
const gated = (req) => {
    const t = process.env.NEKTAR_OVERLAY_TOKEN;
    if (!t)
        return true;
    return (req.headers.authorization ?? '') === `Bearer ${t}`;
};
const waitForSeq = async (ls, seq, ms) => {
    const t0 = Date.now();
    while (Date.now() - t0 < ms) {
        const evs = await ls.feed({ since: seq, limit: 1 });
        if (evs.length)
            return true;
        await new Promise(r => setTimeout(r, 250));
    }
    return false;
};
export default async function create(host) {
    const dbFile = process.env.NEKTAR_PRESENCE_DB;
    // knex is resolved lazily — an in-memory module needs no sqlite driver,
    // which keeps this module loadable in a container that lacks the native dep
    const kx = dbFile ? (await import('knex')).default({ client: 'sqlite3', connection: { filename: dbFile }, useNullAsDefault: true }) : undefined;
    const ls = createLsPresence(kx);
    // --- HTTP surface under /ls_presence/* -----------------------------------
    const dec = (o) => { try {
        return JSON.parse(Buffer.from(o.context ?? []).toString());
    }
    catch {
        return null;
    } };
    host.route('/ls_presence/directory', async (req, res) => {
        const u = new URL(req.url ?? '/', 'http://x');
        const dom = u.searchParams.get('domain');
        const freetext = u.searchParams.get('q');
        if (dom || freetext) {
            const out = await ls.lookup({ service: 'ls_presence', query: dom ? { domain: dom } : { q: freetext } });
            const ads = (await Promise.all(out.map(async (o) => {
                const sc = dec(o);
                const [a] = await ls.lookup({ service: 'ls_presence', query: { pub: sc?.catalog?.pub } });
                return { sc, ad: a ? dec(a) : undefined };
            }))).map(({ sc, ad }) => ({
                ...(ad ?? {}), pub: sc?.catalog?.pub, endpoint: ad?.endpoint ?? sc?.catalog?.endpoint,
                catalog: sc?.catalog, catalogSig: sc?.sig
            })).filter((a) => a.pub);
            return json(res, 200, { ads });
        }
        const query = u.searchParams.get('all') !== null ? { all: true }
            : u.searchParams.get('shard') ? { shard: u.searchParams.get('shard') }
                : u.searchParams.get('pub') ? { pub: u.searchParams.get('pub') }
                    : {};
        const out = await ls.lookup({ service: 'ls_presence', query });
        const ads = out.map((o) => ({ ...dec(o), txid: o.txid })).filter((a) => a.pub)
            .map((a) => { const sc = ls.catalogFor(a.pub); return sc ? { ...a, catalog: sc.catalog, catalogSig: sc.sig } : a; });
        json(res, 200, { ads });
    });
    host.route('/ls_presence/catalogs', async (req, res) => {
        const pub = new URL(req.url ?? '/', 'http://x').searchParams.get('pub');
        json(res, 200, pub ? { catalog: ls.catalogFor(pub) ?? null } : { catalogs: [] });
    });
    host.route('/ls_presence/events', async (req, res) => {
        const u = new URL(req.url ?? '/', 'http://x');
        const since = Number(u.searchParams.get('since') ?? 0);
        const wait = Number(u.searchParams.get('wait') ?? 0);
        const domains = (u.searchParams.get('domains') ?? '').split(',').filter(Boolean);
        const kind = u.searchParams.get('kind') ?? undefined;
        if (wait > 0)
            await waitForSeq(ls, since, Math.min(wait, 30_000));
        const events = await ls.feed({ since, ...(domains.length ? { domains } : {}), ...(kind ? { kind: kind } : {}) });
        json(res, 200, { cursor: events.length ? events[events.length - 1].seq : since, events });
    });
    host.route('/ls_presence/offers', (req, res) => {
        const domain = new URL(req.url ?? '/', 'http://x').searchParams.get('domain') ?? undefined;
        json(res, 200, { offers: ls.offersFor(domain ?? undefined) });
    });
    host.route('/ls_presence/prices', (req, res) => {
        const u = new URL(req.url ?? '/', 'http://x');
        json(res, 200, ls.prices({ domain: u.searchParams.get('domain') ?? undefined, tool: u.searchParams.get('tool') ?? undefined }));
    });
    host.route('/ls_presence/reputation', (req, res) => {
        const u = new URL(req.url ?? '/', 'http://x');
        const pub = u.searchParams.get('pub');
        const domain = u.searchParams.get('domain');
        json(res, 200, pub ? { reputation: ls.reputationFor(pub) ?? null } : { reputations: ls.reputationsFor(domain ?? undefined) });
    });
    host.route('/ls_presence/announce', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announce(j), pub: j?.ad?.pub });
    });
    host.route('/ls_presence/catalog', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announceCatalog(j), pub: j?.catalog?.pub });
    });
    host.route('/ls_presence/want', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announceWant(j), pub: j?.want?.pub });
    });
    host.route('/ls_presence/offer', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announceOffer(j), pub: j?.offer?.pub });
    });
    host.route('/ls_presence/settlement', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announceSettlement(j), pub: j?.settlement?.pub });
    });
    host.route('/ls_presence/reputationCard', async (req, res) => {
        if (!gated(req))
            return json(res, 403, { error: 'forbidden' });
        const j = JSON.parse(await body(req) || '{}');
        json(res, 200, { ok: await ls.announceReputation(j), pub: j?.card?.pub });
    });
    host.onShutdown(async () => { await kx?.destroy(); });
    return {
        topics: { tm_presence: createTmPresence() },
        lookups: {
            ls_presence: Object.assign(ls, {
                // rebuild the index from admitted outputs after a restart — the host
                // replays unspent outputs of our topics; our own knex already
                // rehydrates, so this reconciles anything the engine holds that we
                // missed (crash between admit and persist)
                restore: async (outputs) => {
                    let n = 0;
                    for (const o of outputs) {
                        await ls.outputAdmittedByTopic({ mode: 'locking-script', txid: o.txid, outputIndex: o.outputIndex, topic: o.topic, satoshis: o.satoshis, lockingScript: Script.fromBinary(o.outputScript) });
                        n++;
                    }
                    await ls.init();
                    host.log('ls_presence restored', { outputs: n });
                    return n;
                }
            })
        }
    };
}
