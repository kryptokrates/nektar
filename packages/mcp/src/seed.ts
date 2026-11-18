// seed.ts — zero-config join, layer 2: enumerate operator announces on the
// dir lane, union the announced hosts, then importShard every offered pack
// matching NEKTAR_SEED_DOMAINS. One bounded boot pass — announces arrive
// signed and every imported pack re-verifies its records, so the seed hosts
// are untrusted entry points: withholding is the worst they can do.
import { readAnnounces, fingerHostAllowed, type AnnounceAd, type AnnouncePack } from '@nektar/bstack'
import type { ChainTracker } from '@bsv/sdk'

export interface SeedResult {
  announces: number
  hostsAdded: number
  imported: number
  skipped: number
  failed: number
}

/** one directory read against a host set — injectable so tests exercise the
 *  orchestration without a live overlay */
export type SeedRead = (hosts: string[]) => Promise<{ items: Array<{ txid: string; payload: { body: unknown } }> }>

const MAX_EXTRA_HOSTS = 8
const MAX_ATTEMPTS = 64

const packMatches = (pack: AnnouncePack, domainSet: Set<string>, all: boolean): boolean =>
  all || (pack.domains ?? []).some((d) => domainSet.has(d.toLowerCase()))

/** newest announce per publisher — streamSeq orders a producer's stream
 *  (ts is the fallback for a missing seq, which verifyAnnounce refuses
 *  anyway, so every accepted announce carries it) */
function latestAnnounces(items: Array<{ payload: { body: unknown } }>): AnnounceAd[] {
  const byPub = new Map<string, AnnounceAd>()
  for (const i of items) {
    const a = (i.payload.body as { announce?: AnnounceAd }).announce
    if (a === undefined) continue
    const cur = byPub.get(a.pub)
    if (cur === undefined || (a.streamSeq ?? 0) > (cur.streamSeq ?? 0) || ((a.streamSeq ?? 0) === (cur.streamSeq ?? 0) && a.ts > cur.ts)) byPub.set(a.pub, a)
  }
  return [...byPub.values()]
}

export async function seedFromDirectory(p: {
  mem: { sdkTracker(): ChainTracker }
  call: (tool: string, args: Record<string, unknown>) => Promise<unknown>
  hosts: string[]
  net: string
  /** lowercase domain set from NEKTAR_SEED_DOMAINS ('*' sets `all`) */
  domains: string[]
  all?: boolean
  allowHttp?: boolean
  log?: (line: string) => void
  timeoutMs?: number
  /** defaults to a verified readAnnounces pass — a test injects its own */
  read?: SeedRead
}): Promise<SeedResult> {
  const log = p.log ?? (() => {})
  const read: SeedRead =
    p.read ?? (async (hosts) => await readAnnounces(hosts, { net: p.net }, { tracker: p.mem.sdkTracker(), net: p.net, timeoutMs: p.timeoutMs ?? 8_000 }))
  const first = await read(p.hosts)
  // one hop of union: announced hosts (screened like a finger record's)
  // get their own directory read — a fresh instance finds operators the
  // baked-in seeds never announced
  const extra = new Set<string>()
  for (const a of latestAnnounces(first.items))
    for (const h of a.hosts)
      if (extra.size < MAX_EXTRA_HOSTS && !p.hosts.includes(h) && fingerHostAllowed(h, { allowHttp: p.allowHttp, allowLocal: p.allowHttp })) extra.add(h)
  const items = [...first.items]
  if (extra.size > 0) items.push(...(await read([...extra])).items)
  const announces = latestAnnounces(items)
  const domainSet = new Set(p.domains)
  const all = p.all === true
  const res: SeedResult = { announces: announces.length, hostsAdded: extra.size, imported: 0, skipped: 0, failed: 0 }
  let attempts = 0
  for (const a of announces) {
    for (const pack of a.packs ?? []) {
      if (attempts >= MAX_ATTEMPTS) return res
      if (!packMatches(pack, domainSet, all)) continue
      const args = pack.store !== undefined ? { store: pack.store } : typeof pack.packUrl === 'string' ? { packUrl: pack.packUrl } : undefined
      if (args === undefined) continue
      attempts++
      try {
        await p.call('importShard', args)
        res.imported++
      } catch (e) {
        if ((e as Error).message.includes('already has records')) res.skipped++
        else {
          res.failed++
          log(`seed: importShard ${pack.shard}: ${(e as Error).message.slice(0, 120)}`)
        }
      }
    }
  }
  return res
}
