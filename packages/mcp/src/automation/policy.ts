import { sha256, utf8, toHex } from '@nektar/core'
import { SCORE_DIMENSIONS, type ScoreWeights } from './scoring.js'

export interface ActionContext {
  operationId: string
  actionId: string
  policyHash: string
  networkId: string
}

export type PolicyDecision =
  | { kind: 'allow' }
  | { kind: 'deny', code: string }
  | { kind: 'escalate', code: string }

export interface AutomationPolicy {
  v: 1
  policyId: string
  network: string
  agentPub: string
  roles: Array<'buyer' | 'seller'>
  notBefore: number
  notAfter: number
  domains: string[]
  limits: {
    perPurchaseSats: number
    dailyPurchaseSats: number
    perTxFeeSats: number
    dailyFeeSats: number
    runTotalSats: number
    maxLockedSats: number
    maxUnconfirmedSats: number
    maxUnconfirmedCount: number
  }
  scoring: { minimumScoreBps: number, weights: ScoreWeights, collectMs: number }
  seller: { minPriceSats: number, maxPriceSats: number, maxPreparationsPerDay: number, sellableShards: string[] }
  disclosure: { allowPublic: boolean }
  ai: { providers: string[], allowSensitiveEgress: boolean, maxOutputTokens: number, timeoutMs: number }
  sandbox: { mode: 'sandbox-only' | 'never' }
}

const TOP = ['v', 'policyId', 'network', 'agentPub', 'roles', 'notBefore', 'notAfter', 'domains', 'limits', 'scoring', 'seller', 'disclosure', 'ai', 'sandbox']
const LIMITS = ['perPurchaseSats', 'dailyPurchaseSats', 'perTxFeeSats', 'dailyFeeSats', 'runTotalSats', 'maxLockedSats', 'maxUnconfirmedSats', 'maxUnconfirmedCount']
const ID_RE = /^[A-Za-z0-9][A-Za-z0-9._:-]{0,127}$/
const DOMAIN_RE = /^[a-z0-9][a-z0-9._-]{0,47}$/

const exact = (v: unknown, keys: readonly string[]): v is Record<string, unknown> =>
  v !== null && typeof v === 'object' && !Array.isArray(v) &&
  Object.keys(v).length === keys.length && keys.every(k => Object.hasOwn(v, k))
const nat = (n: unknown, max = Number.MAX_SAFE_INTEGER): n is number =>
  typeof n === 'number' && Number.isSafeInteger(n) && n >= 0 && n <= max
const strings = (v: unknown, re: RegExp, max: number): v is string[] =>
  Array.isArray(v) && v.length <= max && v.every(s => typeof s === 'string' && re.test(s)) && new Set(v).size === v.length

export function policyError (p: unknown): string | undefined {
  if (!exact(p, TOP)) return 'unknown or missing policy field'
  const q = p as unknown as AutomationPolicy
  if (q.v !== 1) return 'unsupported policy version'
  if (typeof q.policyId !== 'string' || !ID_RE.test(q.policyId)) return 'policyId'
  if (typeof q.network !== 'string' || !/^(mock|regtest|testnet|ttn|main)$/.test(q.network)) return 'network'
  if (typeof q.agentPub !== 'string' || !/^0[23][0-9a-f]{64}$/.test(q.agentPub)) return 'agentPub'
  if (!Array.isArray(q.roles) || q.roles.length === 0 || q.roles.length > 2 || !q.roles.every(r => r === 'buyer' || r === 'seller') || new Set(q.roles).size !== q.roles.length) return 'roles'
  if (!nat(q.notBefore) || !nat(q.notAfter) || q.notAfter <= q.notBefore) return 'validity window'
  if (!strings(q.domains, DOMAIN_RE, 64) || q.domains.length === 0) return 'domains'
  if (!exact(q.limits, LIMITS) || !LIMITS.every(k => nat((q.limits as Record<string, unknown>)[k]))) return 'limits'
  if (q.limits.perPurchaseSats > q.limits.dailyPurchaseSats || q.limits.perTxFeeSats > q.limits.dailyFeeSats) return 'limits: per-action cap exceeds daily cap'
  if (!exact(q.scoring, ['minimumScoreBps', 'weights', 'collectMs']) || !nat(q.scoring.minimumScoreBps, 10000) || !nat(q.scoring.collectMs, 86_400_000) ||
    !exact(q.scoring.weights, SCORE_DIMENSIONS) || !SCORE_DIMENSIONS.every(k => nat(q.scoring.weights[k], 10000)) ||
    SCORE_DIMENSIONS.reduce((s, k) => s + q.scoring.weights[k], 0) !== 10000) return 'scoring'
  if (!exact(q.seller, ['minPriceSats', 'maxPriceSats', 'maxPreparationsPerDay', 'sellableShards']) || !nat(q.seller.minPriceSats) || !nat(q.seller.maxPriceSats) ||
    q.seller.maxPriceSats < q.seller.minPriceSats || !nat(q.seller.maxPreparationsPerDay, 100_000) || !strings(q.seller.sellableShards, /^[^\s]{1,64}$/, 256)) return 'seller'
  if (!exact(q.disclosure, ['allowPublic']) || typeof q.disclosure.allowPublic !== 'boolean') return 'disclosure'
  if (!exact(q.ai, ['providers', 'allowSensitiveEgress', 'maxOutputTokens', 'timeoutMs']) || !strings(q.ai.providers, ID_RE, 16) ||
    typeof q.ai.allowSensitiveEgress !== 'boolean' || !nat(q.ai.maxOutputTokens, 1_000_000) || !nat(q.ai.timeoutMs, 3_600_000)) return 'ai'
  if (!exact(q.sandbox, ['mode']) || (q.sandbox.mode !== 'sandbox-only' && q.sandbox.mode !== 'never')) return 'sandbox'
  return undefined
}

export const canonicalJson = (v: unknown): string => JSON.stringify(v, (_k, x) =>
  typeof x === 'bigint'
    ? x.toString()
    : (x !== null && typeof x === 'object' && !Array.isArray(x)
        ? Object.fromEntries(Object.keys(x as object).sort().map(k => [k, (x as Record<string, unknown>)[k]]))
        : x))

export const digestOf = (domain: string, v: unknown): string => toHex(sha256(utf8(`${domain}\n${canonicalJson(v)}`)))

export function policyHash (p: AutomationPolicy): string {
  const e = policyError(p)
  if (e) throw new Error(`invalid automation policy: ${e}`)
  return digestOf('nektar:automation:policy:1', p)
}

export interface PurchaseIntent {
  network: string
  agentPub: string
  domains: string[]
  priceSats: number
  feeSats: number
  lockedSats: number
  now: number
}

export function decidePurchase (p: AutomationPolicy, i: PurchaseIntent): PolicyDecision {
  if (policyError(p)) return { kind: 'deny', code: 'invalid_policy' }
  if (![i.priceSats, i.feeSats, i.lockedSats, i.now].every(n => nat(n))) return { kind: 'deny', code: 'invalid_amount' }
  if (!p.roles.includes('buyer')) return { kind: 'deny', code: 'role_not_authorized' }
  if (i.network !== p.network) return { kind: 'deny', code: 'wrong_network' }
  if (i.agentPub !== p.agentPub) return { kind: 'deny', code: 'wrong_agent' }
  if (i.now < p.notBefore || i.now >= p.notAfter) return { kind: 'deny', code: 'policy_not_valid_now' }
  if (i.domains.length === 0 || !i.domains.every(d => p.domains.includes(d))) return { kind: 'escalate', code: 'domain_outside_policy' }
  if (i.priceSats > p.limits.perPurchaseSats) return { kind: 'escalate', code: 'price_above_policy' }
  if (i.feeSats > p.limits.perTxFeeSats) return { kind: 'escalate', code: 'fee_above_policy' }
  if (i.lockedSats > p.limits.maxLockedSats) return { kind: 'escalate', code: 'locked_capital_above_policy' }
  return { kind: 'allow' }
}

export interface OfferIntent {
  network: string
  agentPub: string
  domains: string[]
  shard: string
  priceSats: number
  publicDisclosure: boolean
  now: number
}

export function decideOffer (p: AutomationPolicy, i: OfferIntent): PolicyDecision {
  if (policyError(p)) return { kind: 'deny', code: 'invalid_policy' }
  if (!nat(i.priceSats) || !nat(i.now)) return { kind: 'deny', code: 'invalid_amount' }
  if (!p.roles.includes('seller')) return { kind: 'deny', code: 'role_not_authorized' }
  if (i.network !== p.network) return { kind: 'deny', code: 'wrong_network' }
  if (i.agentPub !== p.agentPub) return { kind: 'deny', code: 'wrong_agent' }
  if (i.now < p.notBefore || i.now >= p.notAfter) return { kind: 'deny', code: 'policy_not_valid_now' }
  if (!p.seller.sellableShards.includes(i.shard)) return { kind: 'deny', code: 'shard_not_sellable' }
  if (i.publicDisclosure && !p.disclosure.allowPublic) return { kind: 'deny', code: 'public_disclosure_not_authorized' }
  if (i.domains.length === 0 || !i.domains.every(d => p.domains.includes(d))) return { kind: 'deny', code: 'domain_outside_policy' }
  if (i.priceSats < p.seller.minPriceSats || i.priceSats > p.seller.maxPriceSats) return { kind: 'deny', code: 'price_outside_policy' }
  return { kind: 'allow' }
}
