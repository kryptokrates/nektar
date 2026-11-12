import { test } from 'node:test'
import assert from 'node:assert/strict'
import { DEFAULT_SCORE_WEIGHTS, rankOffers, type ScoreCandidate, type ScoringPolicy } from '../src/automation/scoring.js'

const policy = (extra: Partial<ScoringPolicy> = {}): ScoringPolicy => ({
  maxAllInSats: 1000,
  minimumScoreBps: 0,
  weights: { ...DEFAULT_SCORE_WEIGHTS },
  ...extra
})
const candidate = (offerKey: string, maxAllInSats: number, extra: Partial<ScoreCandidate> = {}): ScoreCandidate => ({
  offerKey,
  eligible: true,
  maxAllInSats,
  features: { relevance: 10000, provenance: 10000, history: 10000, latency: 10000 },
  ...extra
})

test('scoring: equal-quality S1 offers prefer 200 over 300 and 450 with replayable exact scores', () => {
  const result = rankOffers([candidate('seller:a', 450), candidate('seller:b', 300), candidate('seller:c', 200)], policy())
  assert.deepEqual(
    result.ranked.map((o) => [o.offerKey, o.scoreBps]),
    [
      ['seller:c', 9600],
      ['seller:b', 9400],
      ['seller:a', 9100]
    ]
  )
  assert.deepEqual(result.rejected, [])
  for (const offer of result.ranked) {
    const numerator = Object.values(offer.weightedNumerators).reduce((a, b) => a + b, 0)
    assert.equal(offer.scoreBps, Math.floor(numerator / offer.denominator))
  }
})

test('scoring: stronger expensive offer wins; price never overrides eligibility', () => {
  const cheap = candidate('cheap', 200, { features: { relevance: 4000, provenance: 10000, history: 10000, latency: 10000 } })
  const result = rankOffers([cheap, candidate('strong', 450), candidate('invalid', 0, { eligible: false })], policy())
  assert.deepEqual(
    result.ranked.map((o) => [o.offerKey, o.scoreBps]),
    [
      ['strong', 9100],
      ['cheap', 7800]
    ]
  )
  assert.deepEqual(result.rejected, [{ offerKey: 'invalid', reason: 'ineligible' }])
})

test('scoring: bounds, threshold, and deterministic key tie-break', () => {
  const result = rankOffers([candidate('z', 200), candidate('a', 200), candidate('over', 1001), candidate('low', 1000)], policy({ minimumScoreBps: 9000 }))
  assert.deepEqual(
    result.ranked.map((o) => o.offerKey),
    ['a', 'z']
  )
  assert.deepEqual(result.rejected, [
    { offerKey: 'low', reason: 'below_minimum' },
    { offerKey: 'over', reason: 'over_budget' }
  ])
  assert.equal(rankOffers([candidate('boundary', 1000)], policy({ minimumScoreBps: 8000 })).ranked[0].scoreBps, 8000)
  assert.equal(rankOffers([candidate('free', 0)], policy()).ranked[0].scoreBps, 10000)
})

test('scoring: exact duplicate copies collapse; conflicting same-key copies fail closed regardless of order', () => {
  const a = candidate('same', 200),
    b = candidate('same', 300)
  assert.equal(rankOffers([a, a], policy()).ranked.length, 1)
  assert.deepEqual(rankOffers([a, b], policy()), rankOffers([b, a], policy()))
  assert.deepEqual(rankOffers([a, b], policy()).rejected, [{ offerKey: 'same', reason: 'conflicting_offer' }])
})

test('scoring: malicious numeric features and unknown policy constraints are rejected', () => {
  for (const value of [NaN, Infinity, -1, 10001, 0.1]) {
    const item = candidate('bad', 100, { features: { relevance: value, provenance: 10000, history: 10000, latency: 10000 } })
    assert.deepEqual(rankOffers([item], policy()).rejected, [{ offerKey: 'bad', reason: 'invalid_offer' }])
  }
  for (const invalid of [
    policy({ maxAllInSats: 0 }),
    policy({ minimumScoreBps: -1 }),
    policy({ weights: { ...DEFAULT_SCORE_WEIGHTS, price: 2001 } }),
    { ...policy(), unknownConstraint: true }
  ])
    assert.throws(() => rankOffers([], invalid), /invalid scoring policy/)
  assert.equal(rankOffers([null as unknown as ScoreCandidate], policy()).rejected[0].reason, 'invalid_offer')
})

test('scoring: malformed duplicate cannot shadow a valid offer; result snapshots do not alias inputs', () => {
  const valid = candidate('same', 200)
  const bad = { ...valid, maxAllInSats: -1 }
  assert.deepEqual(rankOffers([valid, bad], policy()), rankOffers([bad, valid], policy()))
  assert.equal(rankOffers([valid, bad], policy()).ranked.length, 0)
  const p = policy()
  const snapshot = rankOffers([valid], p).ranked[0]
  valid.features.relevance = 0
  p.weights.relevance = 0
  assert.equal(snapshot.featuresBps.relevance, 10000)
  assert.equal(snapshot.weightsBps.relevance, 3000)
})

test('scoring: price normalization is exact at the maximum safe-integer amount', () => {
  const result = rankOffers([candidate('large', Number.MAX_SAFE_INTEGER - 1)], policy({ maxAllInSats: Number.MAX_SAFE_INTEGER }))
  assert.equal(result.ranked[0].featuresBps.price, 0)
  assert.equal(result.ranked[0].scoreBps, 8000)
})
