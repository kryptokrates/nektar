export const SCORE_DIMENSIONS = ['relevance', 'provenance', 'history', 'price', 'latency'] as const
export type ScoreDimension = typeof SCORE_DIMENSIONS[number]
export type ScoreWeights = Record<ScoreDimension, number>
export type OfferFeatures = Record<Exclude<ScoreDimension, 'price'>, number>

export const DEFAULT_SCORE_WEIGHTS: Readonly<ScoreWeights> = Object.freeze({
  relevance: 3000,
  provenance: 2500,
  history: 1500,
  price: 2000,
  latency: 1000
})

export interface ScoringPolicy {
  maxAllInSats: number
  minimumScoreBps: number
  weights: ScoreWeights
}

export interface ScoreCandidate {
  offerKey: string
  eligible: boolean
  maxAllInSats: number
  features: OfferFeatures
}

export interface RankedOffer {
  offerKey: string
  maxAllInSats: number
  scoreBps: number
  featuresBps: Record<ScoreDimension, number>
  weightsBps: ScoreWeights
  weightedNumerators: Record<ScoreDimension, number>
  denominator: 10000
}

export type ScoreRejection = 'invalid_offer' | 'ineligible' | 'over_budget' | 'below_minimum' | 'conflicting_offer'

export interface OfferRanking {
  ranked: RankedOffer[]
  rejected: Array<{ offerKey: string; reason: ScoreRejection }>
}

const FEATURE_DIMENSIONS = ['relevance', 'provenance', 'history', 'latency'] as const
const integer = (n: unknown, min: number, max: number): n is number =>
  typeof n === 'number' && Number.isSafeInteger(n) && n >= min && n <= max
const keysAre = (value: unknown, keys: readonly string[]): value is Record<string, unknown> => {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) return false
  const proto = Object.getPrototypeOf(value)
  return (proto === Object.prototype || proto === null) &&
    Object.keys(value).length === keys.length && keys.every(k => Object.hasOwn(value, k))
}
const compareKeys = (a: string, b: string) => a < b ? -1 : a > b ? 1 : 0

function validatePolicy (policy: ScoringPolicy): void {
  if (!keysAre(policy, ['maxAllInSats', 'minimumScoreBps', 'weights']) ||
    !integer(policy.maxAllInSats, 1, Number.MAX_SAFE_INTEGER) ||
    !integer(policy.minimumScoreBps, 0, 10000) ||
    !keysAre(policy.weights, SCORE_DIMENSIONS) ||
    !SCORE_DIMENSIONS.every(k => integer(policy.weights[k], 0, 10000)) ||
    SCORE_DIMENSIONS.reduce((sum, k) => sum + policy.weights[k], 0) !== 10000) {
    throw new Error('invalid scoring policy')
  }
}

function validCandidate (candidate: ScoreCandidate): boolean {
  return keysAre(candidate, ['offerKey', 'eligible', 'maxAllInSats', 'features']) &&
    typeof candidate.offerKey === 'string' && /^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$/.test(candidate.offerKey) &&
    typeof candidate.eligible === 'boolean' &&
    integer(candidate.maxAllInSats, 0, Number.MAX_SAFE_INTEGER) &&
    keysAre(candidate.features, FEATURE_DIMENSIONS) &&
    FEATURE_DIMENSIONS.every(k => integer(candidate.features[k], 0, 10000))
}

function sameCandidate (a: ScoreCandidate, b: ScoreCandidate): boolean {
  return a.eligible === b.eligible && a.maxAllInSats === b.maxAllInSats &&
    FEATURE_DIMENSIONS.every(k => a.features[k] === b.features[k])
}

function score (candidate: ScoreCandidate, policy: ScoringPolicy): RankedOffer {
  const price = Number(BigInt(policy.maxAllInSats - candidate.maxAllInSats) * 10000n / BigInt(policy.maxAllInSats))
  const featuresBps = { ...candidate.features, price }
  const weightsBps = { ...policy.weights }
  const weightedNumerators = Object.fromEntries(SCORE_DIMENSIONS.map(k =>
    [k, featuresBps[k] * weightsBps[k]])) as Record<ScoreDimension, number>
  const numerator = SCORE_DIMENSIONS.reduce((sum, k) => sum + weightedNumerators[k], 0)
  return {
    offerKey: candidate.offerKey,
    maxAllInSats: candidate.maxAllInSats,
    scoreBps: Math.floor(numerator / 10000),
    featuresBps,
    weightsBps,
    weightedNumerators,
    denominator: 10000
  }
}

export function rankOffers (candidates: readonly ScoreCandidate[], policy: ScoringPolicy): OfferRanking {
  validatePolicy(policy)
  if (!Array.isArray(candidates)) throw new Error('candidates must be an array')
  const rejected: OfferRanking['rejected'] = []
  const groups = new Map<string, ScoreCandidate[]>()
  const invalidKeys = new Set<string>()
  for (const candidate of candidates) {
    const offerKey = typeof candidate?.offerKey === 'string' ? candidate.offerKey : ''
    if (!validCandidate(candidate)) {
      rejected.push({ offerKey, reason: 'invalid_offer' })
      invalidKeys.add(offerKey)
      continue
    }
    const group = groups.get(offerKey) ?? []
    group.push(candidate)
    groups.set(offerKey, group)
  }
  const ranked: RankedOffer[] = []
  for (const [offerKey, group] of groups) {
    const candidate = group[0]!
    let reason: ScoreRejection | undefined
    if (invalidKeys.has(offerKey) || !group.every(c => sameCandidate(candidate, c))) reason = 'conflicting_offer'
    else if (!candidate.eligible) reason = 'ineligible'
    else if (candidate.maxAllInSats > policy.maxAllInSats) reason = 'over_budget'
    if (reason) { rejected.push({ offerKey, reason }); continue }
    const result = score(candidate, policy)
    if (result.scoreBps < policy.minimumScoreBps) rejected.push({ offerKey, reason: 'below_minimum' })
    else ranked.push(result)
  }
  ranked.sort((a, b) => b.scoreBps - a.scoreBps ||
    (a.maxAllInSats < b.maxAllInSats ? -1 : a.maxAllInSats > b.maxAllInSats ? 1 : 0) ||
    compareKeys(a.offerKey, b.offerKey))
  rejected.sort((a, b) => compareKeys(a.offerKey, b.offerKey) || compareKeys(a.reason, b.reason))
  return { ranked, rejected }
}
