import { sha256, utf8, toHex, nektarEnv, sparse } from '@nektar/core'
import { claimsRecord, recordReader } from '@lightwebinc/bcommon'
import { marketJson, verifyWant, verifyOffer, verifyAnnounce, type SignedWant, type SignedOffer, type SignedAnnounce } from './artifacts.js'

export const MAX_PAYLOAD = 16384
export const MAX_DOMAINS = 3
export const LS_NAME = 'ls_nektar'
/** reserved domain tagging operator announces — every announce lands on the
 *  single `tm_nektar_<net>_<nibble('dir')>` lane, the enumerable directory */
export const DIR_DOMAIN = 'dir'

export type MarketPayload = { v: 1; kind: 'want' | 'offer' | 'announce'; body: SignedWant | SignedOffer | SignedAnnounce }

// ── wire form ──────────────────────────────────────────────────────────
// v2: the stock application-record envelope — a canonical CBOR map with a
// 'nkt1' magic, key 1 the kind (1 want, 2 offer), key 2 the signed artifact
// as canonical marketJson text. The artifact inside is byte-identical to
// v1, so signatures and the whole verify path are unchanged; a reader that
// knows only the record layer can still name the record and lift the body.
// Keys above 2 are preserved-but-ignored headroom for a later schema.
// v1 was the same {v,kind,body} as JSON text; decode accepts both forever.
const MAGIC = new TextEncoder().encode('nkt1')
const LAST_KEY = 2n
const KIND: Record<MarketPayload['kind'], bigint> = { want: 1n, offer: 2n, announce: 3n }
const KIND_NAME: Record<string, MarketPayload['kind']> = { '1': 'want', '2': 'offer', '3': 'announce' }
const reader = recordReader((reason, detail) => new Error(`market payload ${reason}${detail !== undefined ? ` ${detail}` : ''}`))

/** Emit the legacy v1 JSON envelope (pre-record wire form for talking to
 *  hosts running an older module — also NEKTAR_MARKET_V1=1). */
export function encodePayload(m: MarketPayload, opts?: { legacy?: boolean }): Uint8Array {
  if (opts?.legacy === true || nektarEnv('NEKTAR_MARKET_V1') === '1') {
    const b = utf8(marketJson(m))
    if (b.length > MAX_PAYLOAD) throw new Error(`payload ${b.length} over ${MAX_PAYLOAD}`)
    return b
  }
  const body = marketJson(m.body)
  const pairs = [
    { key: 0n, val: MAGIC },
    { key: 1n, val: KIND[m.kind] },
    { key: 2n, val: body }
  ]
  return reader.encode(pairs, [], MAX_PAYLOAD)
}
export function decodePayload(b: Uint8Array): MarketPayload {
  if (claimsRecord(b, MAGIC)) {
    const f = reader.decode(b, MAX_PAYLOAD, LAST_KEY, MAGIC)
    const kind = sparse(KIND_NAME[String(reader.uint(f, 1, 1, 3))])
    if (kind === undefined) throw new Error('market payload: unknown kind')
    let body: MarketPayload['body'] | undefined
    try {
      body = sparse(JSON.parse(reader.text(f, 2)) as MarketPayload['body'])
    } catch {
      throw new Error('market payload: body is not valid JSON')
    }
    if (body === undefined || typeof body !== 'object') throw new Error('market payload: empty body')
    return { v: 1, kind, body }
  }
  let m: MarketPayload | null
  try {
    m = JSON.parse(new TextDecoder().decode(b)) as MarketPayload | null
  } catch {
    throw new Error('bad market payload')
  }
  if (m?.v !== 1 || !(['want', 'offer', 'announce'] as string[]).includes(m.kind) || sparse(m.body) === undefined) throw new Error('bad market payload')
  return m
}

export function laneNibble(domain: string): string {
  return toHex(Uint8Array.of(sha256(utf8(domain.toLowerCase()))[0]))[0]
}
export function laneOf(net: string, domain: string): string {
  return `tm_nektar_${net}_${laneNibble(domain)}`
}
export function allLanes(net: string): string[] {
  return '0123456789abcdef'.split('').map((n) => `tm_nektar_${net}_${n}`)
}
export function lanesFor(net: string, domains: string[]): string[] {
  const d = [...new Set(domains.map((x) => x.toLowerCase()))]
  if (d.length === 0 || d.length > MAX_DOMAINS) throw new Error(`want needs 1..${MAX_DOMAINS} domains`)
  return d.map((x) => laneOf(net, x))
}

export function verifyBody(m: MarketPayload, now?: number): boolean {
  const b = m.body as { want?: unknown; offer?: unknown; announce?: unknown; sig: string }
  return m.kind === 'want'
    ? verifyWant(b.want as never, b.sig, { now })
    : m.kind === 'offer'
      ? verifyOffer(b.offer as never, b.sig, { now })
      : verifyAnnounce(b.announce as never, b.sig, { now })
}
export function bodyPub(m: MarketPayload): string {
  const b = m.body as { want?: { pub: string }; offer?: { pub: string }; announce?: { pub: string } }
  return m.kind === 'want' ? b.want!.pub : m.kind === 'offer' ? b.offer!.pub : b.announce!.pub
}
export function bodyDomains(m: MarketPayload): string[] {
  const b = m.body as { want?: { domains: string[] }; offer?: { domains: string[] } }
  // announces don't tag content domains — the reserved directory domain
  // puts every announce on the one enumerable lane
  return m.kind === 'announce' ? [DIR_DOMAIN] : m.kind === 'want' ? b.want!.domains : b.offer!.domains
}
/** ms-epoch expiry of the signed artifact (want.exp / offer.exp / announce.exp); undefined when absent */
export function bodyExp(m: MarketPayload): number | undefined {
  const b = m.body as { want?: { exp?: number }; offer?: { exp?: number }; announce?: { exp?: number } }
  return m.kind === 'want' ? b.want?.exp : m.kind === 'offer' ? b.offer?.exp : b.announce?.exp
}
/** the artifact's signed network binding — one extraction for every kind */
export function bodyNet(m: MarketPayload): string | undefined {
  const b = m.body as { want?: { net?: string }; offer?: { net?: string }; announce?: { net?: string } }
  return m.kind === 'want' ? b.want?.net : m.kind === 'offer' ? b.offer?.net : b.announce?.net
}
/** hosts refuse artifacts expiring further out than this */
export const MAX_LIFETIME_MS = 7 * 86_400_000
