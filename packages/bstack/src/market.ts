import { sha256, utf8, toHex } from '@nektar/core'
import { marketJson, verifyWant, verifyOffer, type SignedWant, type SignedOffer } from '@nektar/overlay/presence'

export const MAX_PAYLOAD = 16384
export const MAX_DOMAINS = 3
export const LS_NAME = 'ls_nektar'

export type MarketPayload = { v: 1, kind: 'want' | 'offer', body: SignedWant | SignedOffer }

export function encodePayload (m: MarketPayload): Uint8Array {
  const b = utf8(marketJson(m))
  if (b.length > MAX_PAYLOAD) throw new Error(`payload ${b.length} over ${MAX_PAYLOAD}`)
  return b
}
export function decodePayload (b: Uint8Array): MarketPayload {
  const m = JSON.parse(new TextDecoder().decode(b)) as MarketPayload
  if (m?.v !== 1 || (m.kind !== 'want' && m.kind !== 'offer') || m.body === undefined) throw new Error('bad market payload')
  return m
}

export function laneNibble (domain: string): string {
  return toHex(Uint8Array.of(sha256(utf8(domain.toLowerCase()))[0]!))[0]!
}
export function laneOf (net: string, domain: string): string { return `tm_nektar_${net}_${laneNibble(domain)}` }
export function allLanes (net: string): string[] { return '0123456789abcdef'.split('').map(n => `tm_nektar_${net}_${n}`) }
export function lanesFor (net: string, domains: string[]): string[] {
  const d = [...new Set(domains.map(x => x.toLowerCase()))]
  if (d.length === 0 || d.length > MAX_DOMAINS) throw new Error(`want needs 1..${MAX_DOMAINS} domains`)
  return d.map(x => laneOf(net, x))
}

export function verifyBody (m: MarketPayload): boolean {
  const b = m.body as { want?: unknown, offer?: unknown, sig: string }
  return m.kind === 'want' ? verifyWant(b.want as never, b.sig) : verifyOffer(b.offer as never, b.sig)
}
export function bodyPub (m: MarketPayload): string {
  const b = m.body as { want?: { pub: string }, offer?: { pub: string } }
  return (m.kind === 'want' ? b.want!.pub : b.offer!.pub)
}
export function bodyDomains (m: MarketPayload): string[] {
  const b = m.body as { want?: { domains: string[] }, offer?: { domains: string[] } }
  return m.kind === 'want' ? b.want!.domains : b.offer!.domains
}
/** ms-epoch expiry of the signed artifact (want.exp / offer.exp); undefined when absent */
export function bodyExp (m: MarketPayload): number | undefined {
  const b = m.body as { want?: { exp?: number }, offer?: { exp?: number } }
  return m.kind === 'want' ? b.want?.exp : b.offer?.exp
}
/** hosts refuse artifacts expiring further out than this */
export const MAX_LIFETIME_MS = 7 * 86_400_000
