# RFC-002: Nektar Memory Unit Standard (NMU-1)

- **Status:** Implemented (`v0.1` Pure-Go)
- **Authors:** Nektar Core
- **Requires:** [RFC-001](RFC-001-jevgraph-ontology.md), `internal/bstack` (`nkt1`), `internal/market`, `internal/reputation`, BRC-29 / BRC-105 / BRC-369
- **Canonical Code:** `internal/nmu/{nmu,verify,auth}.go`, `internal/bstack/{wire,module,module_lookup}.go`, `internal/store/db_nmu.go`, `internal/distill/distill.go`, `internal/market/{pricing,policy_ranking,deal_receipt,workflows_buyer_verify,feed_client}.go`, `internal/server/http_nmu_market.go`

---

## 1. Motivation

Cryptographic market plumbing (`nkt1` CBOR, AtomicBEEF, `DisclosureSale`, `nektardr1.*`, `nektarrep1.*`) guarantees fair settlement, but without a standardized unit of account a "fragment" can range from a one-line note to a 30-triple verified breakdown. **NMU-1** standardizes AI memory as a comparable, machine-verifiable commodity — analogous to standardized token pricing (`¥ / 1M tokens`) in compute markets — using zero-float integer arithmetic across four primitives:

1. **Canonical Unit of Account:** 1 Verified Triple (`satsPer1kUnits` integer milli-sats per triple).
2. **Machine-Verifiable Commodity Grades:** `C < B < A < S`.
3. **Lifecycle & Usage Rights:** `freshnessClass` (`ephemeral`, `versioned`, `evergreen`) + `usageRights` (`inference_only`, `distill_resellable`).
4. **Multi-Mode Market Contracts:** `spot`, `per_query` (BRC-105 HTTP 402), and `subscription` (time-windowed SSE Shard Feed).

---

## 2. Canonical Unit & Integer Pricing Arithmetic (Zero Floats)

### 2.1 Anti-Stuffing Verified Triple Counting (`CountVerifiedTriples`)

A fragment's `UnitCount` (`0..4096`, `MaxUnitCount = 4096`) MUST be computed deterministically over its triples as follows:

1. **Canonical Deduplication:** Compute each triple's `TagTripleV1` CBOR digest; exact duplicates are counted once.
2. **Declared Predicate Filtering:** When an `OntologyID` is pinned and its `OntologyManifest` / `CompiledOntology` predicate set is available, only triples whose `Predicate` is declared in the pinned TBox are counted. Unknown/noise predicates contribute `0` units.
3. **Per-`(Subject, Predicate)` Cap:** At most `DefaultMaxTriplesPerSubjectPredicate = 8` distinct triples sharing the same `(Subject, Predicate)` pair are counted, preventing single-entity attribute stuffing.

### 2.2 Normalized Unit Rate (`satsPer1kUnits`)

To avoid floating-point non-determinism in consensus, ranking, and receipts, unit prices are represented as integer **milli-sats per verified triple** (`satsPer1kUnits`):

```text
satsPer1kUnits = floor((priceSats * 1000) / max(1, unitCount))   // 128-bit overflow-safe
```

- If `priceSats > 0` and integer division floors to `0`, `ComputeSatsPer1kUnits` clamps to `1` milli-sat so paid units never appear free.
- `ComputeTotalSatsForUnits(satsPer1kUnits, unitCount)` computes `ceil((satsPer1kUnits * max(1, unitCount)) / 1000)` with 128-bit overflow protection.
- Human display formats `satsPer1kUnits` as decimal `sats/triple` (e.g., `2500` → `2.5` sats/triple).

---

## 3. Machine-Verifiable Commodity Grades (`C < B < A < S`)

Grades are strictly ordered (`GradeRank`: `C=1`, `B=2`, `A=3`, `S=4`) and deterministically computed by `ClassifyFragmentGrade` and independently re-verified by buyers post-reveal via `VerifyDeliveredGrade`:

| Grade | Requirements |
|---|---|
| **Grade C** *(Raw Scrubbed)* | Non-empty text that passed the fail-closed PII/secret scrub gate (`distill.Scrub`). No ontology triples required. |
| **Grade B** *(Distilled & Ontology-Pinned)* | Grade C + `Distilled == true` + valid 64-hex `OntologyID` + `UnitCount >= 1` verified unique TBox triples + deterministic Google Mangle TBox consistency. |
| **Grade A** *(Bonded & Cited)* | Grade B + verbatim source spans committed in 64-hex `EdgesRoot` bound inside L1 MPE (`ontologyId` + `edgesRoot`) + active, unseized `NektarCheckpointBond` collateral (`BondOutpoint`). |
| **Grade S** *(Live Maintained)* | Grade A + active live SSE supersession feed (`FeedURL` serving bi-temporal `core.RefinementSupersedes` push updates). |

### 3.1 Grade S "Dead Feed" Anti-Gaming Guard

To prevent sellers from advertising a dummy `FeedURL` to claim Grade S ranking bonuses without serving updates:
- `RankOffersWith6DReputationAt` awards the full Grade S ranking bonus (`+1300 bp`) **only** if the seller has an active bond (`BondOutpoint != ""`), non-empty `FeedURL`, AND non-penalized reputation (`DispBP >= 5000` and `CollabBP >= 5000`).
- Buyers streaming a paid subscription via `market.Engine.StreamShardSubscription` automatically emit a signed `nektarrep1.*` slashing attestation (`DispBP <= 2500`, `CollabBP <= 3500`) if the feed returns HTTP `404`/`410`/`5xx` or drops prematurely before `expires_at`.

---

## 4. Lifecycle, Freshness & Usage Rights

### 4.1 Freshness Classes (`freshnessClass`)

- `ephemeral`: Rapidly decaying operational/incident facts; typically carries `validUntilUnix`.
- `versioned`: Pinned to a software/protocol version via `subjectVersion` (max 64 chars, e.g. `go-1.27`, `bsv-sdk-v1.7.8`); valid until superseded.
- `evergreen`: Durable invariants (math, RFCs, protocol rules).
- **Fail-Closed Expiry:** Any fragment or offer with `validUntilUnix > 0 && nowUnix >= validUntilUnix` is treated as expired (`IsExpired == true`) and excluded from quoting, `per_query` recall, and buyer ranking.

### 4.2 Usage Rights (`usageRights`)

- `inference_only` *(default, fail-closed)*: Buyer may recall and ground local reasoning, but **MUST NOT** distill or resell the fragment into market shards.
- `distill_resellable`: Buyer may merge and distill the fragment into derived `market-*` shards, recording an explicit `core.RefinementRefines` provenance edge.
- **Enforcement in SleepCycle (`internal/distill`):** Every `Tainted: true` (purchased) fragment is blocked from distillation if `GradeMismatch == true` or `EffectiveRights(usageRights) != distill_resellable`.

---

## 5. Multi-Mode Market Contracts

A single shard listing (`sell_listings`) MAY enable any combination of the three contract modes independently (`PriceSats > 0`, `QueryPriceSats > 0`, `SubscriptionPriceSats > 0`):

### 5.1 `spot` — Atomic FactPack Sale

- Standard `nkt1` WantAd/OfferAd + `DisclosureSale` / BRC-29 + BRC-369 atomic key reveal.
- Sellers may set `PricePerUnit: true` so `DynamicPriceState.QuoteForFactPack` scales the ask linearly by verified `UnitCount`.

### 5.2 `per_query` — Metered Recall over BRC-105 HTTP 402

- **Endpoint:** `POST /v1/market/query`
- **Request:** `{shard, query?, ontologyId?, minGrade?, buyerPub?, limit?}`
- **Payment Gate:** Requires BRC-105 HTTP 402 payment (`verifyBRC105Payment`) of `listing.QueryPriceSats` with atomic `txid` replay protection.
- **Response:** Cedar-authorized, PII-scrubbed, non-expired `items[]` (each with `nmu` spec and `proofBundle`) plus a seller-signed `nektardr1.*` `DealReceipt` binding `pricingMode = "per_query"`.

### 5.3 `subscription` — Time-Windowed Shard Feed & Grade S SLA

- **Activation Endpoint:** `POST /v1/market/subscriptions` (`{shard, buyerPub, durationSec?}`) gated by BRC-105 HTTP 402 (`listing.SubscriptionPriceSats`), creating a durable `shard_subscriptions` row (unique `payment_txid`) and returning `{subId, feedUrl, startedAt, expiresAt, dealReceipt, dealReceiptToken}`.
- **Live SSE Stream:** `GET /v1/market/feeds/{shard}?sinceSeq=<n>`
- **Replay-Protected Subscriber Authentication (`±60s` Window):**
  - Required HTTP headers: `X-Nektar-Sub-ID`, `X-Nektar-Pub` (33-byte compressed hex), `X-Nektar-TS` (Unix seconds), `X-Nektar-Sig` (strict low-S DER ECDSA hex).
  - Canonical signed digest:
    ```text
    msg    = "GET|/v1/market/feeds/" + shard + "|" + subId + "|" + tsUnix
    digest = DSHA256("nektar:feed-auth:v1" || msg)
    ```
  - Server MUST reject any request where `|nowUnix - tsUnix| > 60` (`FeedAuthMaxClockSkewSec = 60`, `ErrFeedAuthExpired`).
- **Stream Events:** `connected`, `fragment_distilled` (replayed for `seq > sinceSeq`), `fragment_superseded` (real-time push wired from `memory.Service.SetOnSupersede`), and `subscription_expired`.

---

## 6. Wire Format (`nkt1`), Deal Receipts & Post-Reveal Enforcement

### 6.1 Backward-Compatible `nkt1` Fields

All NMU-1 wire fields are optional (`omitempty`) so legacy ungraded nodes interoperate seamlessly:
- **`WantAd`:** `minGrade`, `maxSatsPer1kUnits`, `freshnessClass`, `requiredRights`, `pricingMode`.
- **`OfferAd`:** `grade`, `unitCount`, `satsPer1kUnits`, `freshnessClass`, `validUntilUnix`, `subjectVersion`, `usageRights`, `pricingMode`, `subscriptionSec`, `feedUrl`.
- **`PackRef`:** `grade`, `unitCount`, `satsPer1kUnits`, `pricingMode`, `enabledModes`, `queryPriceSats`, `subscriptionPriceSats`, `subscriptionSec`, `freshnessClass`, `usageRights`, `feedEndpoint`.
- **Fail-Closed Overlay Validation (`bstack.ValidateMarketPayload`):** Rejects unknown grades, unknown freshness/rights/modes, `unitCount > 4096`, `satsPer1kUnits` inconsistent with `(priceSats, unitCount)`, and `feedUrl` exceeding 256 bytes or non-HTTP(S).

### 6.2 `nektardr1.*` NMU Binding

When `DealReceipt.NMU` is present, `DealReceipt.CanonicalFields()` appends `nmu_version`, `nmu_grade`, `nmu_unit_count`, `nmu_sats_per_1k_units`, `nmu_freshness_class`, `nmu_valid_until`, `nmu_subject_version`, `nmu_usage_rights`, `nmu_pricing_mode`, `nmu_subscription_sec`, and `nmu_feed_url` into the receipt's MPE `FieldsRoot`, making the seller's grade and pricing terms cryptographically non-repudiable.

### 6.3 Post-Reveal Verification, Default Quarantine & Slashing

Upon decrypting a spot FactPack, the buyer runs `nmu.VerifyDeliveredGrade`:
- If `achievedGrade < advertisedGrade` or delivered `verifiedUnits < advertisedUnitCount`:
  1. The buyer still persists the paid plaintext for auditability, tagged with `AchievedGrade` and `GradeMismatch: true`.
  2. Untrusted edges are **quarantined out of `knowledge_edges` by default** (`QuarantineEdges: true`) so dishonest triples never pollute local Mangle reasoning.
  3. The buyer automatically emits a signed `nektarrep1.*` slashing attestation with capped `AccBP`, `DispBP`, and `ProvBP` citing the `nektardr1.*` receipt hash.

---

## 7. Commodity Ticker & Interfaces

| Surface | Interface |
|---|---|
| **HTTP API** | `GET /v1/market/ticker?ontologyId=&grade=&pricingMode=&domain=` |
| **CLI** | `nektar ticker [-ontology ...] [-grade S\|A\|B\|C] [-mode spot\|per_query\|subscription] [-domain ...] [-json]` |
| **CLI Flags** | `nektar sell` (`-per-unit`, `-query-price`, `-sub-price`, `-sub-days`, `-freshness`, `-rights`, `-min-grade`), `nektar want` (`-min-grade`, `-max-sats-per-1k`, `-freshness`, `-rights`, `-mode`), `nektar remember` (`-freshness`, `-valid-until`, `-subject-version`, `-rights`) |
| **MCP / REST Tool** | `marketContract` (`action: "ticker" \| "subscriptions" \| "revokeSubscription"`) |
| **Dashboard UI** | `CommodityTicker.svelte` (embedded in `/state` `ticker` field) + Grade/mode pills on local listings |

---

## 8. Honest Boundaries (Normative)

Every NMU-1 `Spec`, `DealReceipt`, and `CommodityTickerResponse` MUST preserve `AdvisoryNMUNote`:

> *"NMU-1 grade proves recorded, unaltered, schema-conformant, evidence-cited, bonded, and live-maintained status — never real-world factual truth."*

- Usage rights (`inference_only`) are enforced locally by honest Nektar nodes (SleepCycle distillation gate) and evidenced in signed `nektardr1.*` receipts, but cryptographic decryption cannot physically prevent an off-protocol buyer from copying plaintext after reveal.
