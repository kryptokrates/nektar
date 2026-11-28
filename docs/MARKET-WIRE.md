# MARKET-WIRE — the Nektar v0.1 Agent-Exchange Wire Contract

Everything a foreign implementation needs to speak wants, offers, escrow,
capsular delivery, and overlay propagation with Nektar agents over stock
BRC-22 / BRC-24 / BRC-148 / BRC-149 / BRC-167 / BRC-169 / BRC-180 / BRC-223
overlay hosts.

**Canonical Pure-Go v0.1 Reference Implementation:**
- `internal/core/{cbor,hash,schema,spendguard}.go`
- `internal/bstack/{wire,wire_cbor,module,module_lookup,bbox_native,bbox_crypto,bstore_tier,chirp}.go`
- `internal/memrepo/{objects,objects_cbor,capsule,capsule_codec,capsule_verify}.go`
- `internal/contracts/{sale,covenant,artifacts/DisclosureSale.json}.go`
- `internal/hub/{hub_overlay,hub_submit,hub_sse,plane_brc149,plane_brc149_codec,store_bbox}.go`
- `internal/market/{fast_journal,workflows*.go,broker_events.go}`

**Companion Normative RFCs:**
- [RFC-001](rfcs/RFC-001-jevgraph-ontology.md) — JevGraph Native Ontology & Bridge Shards
- [RFC-002](rfcs/RFC-002-nmu1-commodity-standard.md) — NMU-1 Standardized Knowledge Commodity
- [RFC-003](rfcs/RFC-003-wire-evolution-and-durability.md) — Wire Evolution, Greasing, Topic Extensibility & Durability Invariants

---

## 0. Conventions and Normative Language (RFC 2119 / RFC 8174)

The key words **"MUST"**, **"MUST NOT"**, **"REQUIRED"**, **"SHALL"**, **"SHALL NOT"**,
**"SHOULD"**, **"SHOULD NOT"**, **"RECOMMENDED"**, **"NOT RECOMMENDED"**, **"MAY"**, and
**"OPTIONAL"** in this document are to be interpreted as described in BCP 14
([RFC 2119](https://www.rfc-editor.org/rfc/rfc2119), [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174))
when, and only when, they appear in all capitals, as shown here.

- **Wire invariants** (framing, canonical CBOR encoding, domain-separation tags, signature verification, critical extension handling, and SPV checks) use **`MUST` / `MUST NOT`**.
- **Operational safety & resilience behaviors** (polling fallbacks, rate ceilings, greasing cadence, clock-skew guards) use **`SHOULD` / `SHOULD NOT`**.
- **Optional protocol extensions** (extra odd CBOR keys, optional transport optimizations, optional BRC-223 fabric hints) use **`MAY`**.

---

## 1. Transport and Overlay Admission

- Overlay hosts expose stock BRC-22 `POST /submit` (topic + Atomic BEEF) and BRC-24 `POST /lookup` (`{service, query}` → `{outputs | result}`).
- On live networks (`main`, `testnet`), every carrier transaction **MUST** be a valid Atomic BEEF carrying SPV ancestry anchored in witnessed headers (`overlay-bridge/pkg/spv`). Unmineable `nkt1` PushDrop carriers (`bcommon/carrier`, locktime ≥ `carrier.LockTime`, non-final input sequence) **MUST** satisfy all `REFUSED-*` carrier rules and bind the PushDrop signing key via BRC-42 (`[1, "nektar market"]`, keyID `"1"`) to the payload's declared 33-byte compressed identity public key.
- A mineable (final) transaction spending a carrier's funding outpoint permanently retracts that carrier across every conforming Hub (1BSV kill-switch sweep) and **MUST** evict the outpoint from `ls_nektar`.
- Zero-confirmation trades under the counterparty's `ZeroConfMaxSats` cap are permitted when SPV ancestry verifies cleanly; above `ZeroConfMaxSats`, sellers **MUST** withhold key release (`Status: "awaiting_confirmation"`) until `settleDepth` confirmations are proved by a witnessed BUMP.
- Implementations **MAY** also consume numbered binary BRC-149 object plane streams (`GET /plane/stream`, `GET /plane/nack`, `Content-Type: application/vnd.nektar.brc149-stream`) or cursored SSE (`GET /events?cursor=<seq>`).

---

## 2. Market Lanes and Topic Evolution (RFC 6709)

- **Legacy Nibble Lanes (MTI):** `tm_nektar_<net>_<nibble>` where `nibble = hex(sha256(utf8(lower(trim(domain))))[0])[0]` (`0..f`, 16 lanes per network).
- **Dynamic Semantic & Ontology Topics (BRC-223):**
  - Domain topic: `tm_nektar_<net>_dom_<slug>` (`[a-z0-9_-]{1,32}`)
  - Ontology topic: `tm_nektar_<net>_ont_<id16>` (first 16 lowercase hex chars of 32-byte `OntologyID`)
- **Co-Located LightBSV Topics:** Conforming Hubs **MAY** mount `tm_finger`, `tm_bbox_<office>`, `tm_bstore_<bucket>`, `tm_blogs_*`, `tm_borg_*`, `tm_bsecret_*`, `tm_bdns_*`, and future `tm_*` topics alongside `tm_nektar_*`.
- **RFC 6709 Unknown-Topic Isolation:** Relays, Brokers, and Hubs **MUST NOT** crash, stall, or drop known-topic processing when encountering an unrecognized topic name or `TopicID` on `/manifest.json`, `/plane/stream`, or BRC-223 fabric multicast; unknown topics **MUST** be ignored safely unless the local node explicitly elected them.
- **Lookup Service:** `ls_nektar` accepts `MarketQuery` JSON (`kind`, `wantId`, `offerId`, `domain`, `ontologyId`, `targetOntologyId`, `minGrade`, `pricingMode`, `usageRights`, `freshnessClass`, `maxSatsPer1kUnits`, `pub`, `buyerPub`, `subjectPub`, `sinceMs`, `sinceSeq`, `limit`).

---

## 3. Canonical Encoding & Schema Evolution (`nkt1`, RFC 9413 / RFC 9170 / DDIA Ch. 4)

All Nektar wire records **MUST** use strict **RFC 8949 §4.2.1 Core Deterministic Encoding (CDE) CBOR** (`bcommon/cbor`):
- Map keys **MUST** be unsigned integers (`uint64`) in canonical CBOR bytewise order (no duplicate keys, no indefinite-length items, no CBOR tags / major type 6, no floating-point numbers).
- Every CBOR map **MUST** contain at most **`MaxCBORMapKeys = 64`** entries (matching `bcommon/record.MaxKeys`), checked before field decoding.
- Decoders **MUST** enforce deterministic refusal precedence:
  1. Size cap (`MaxPayload = 16384` for `nkt1`)
  2. Canonical RFC 8949 CDE CBOR map validity
  3. Entry count ≤ `64`
  4. Key type (`uint64`) and duplicate-key rejection
  5. Magic check (key `0`)
  6. Unknown-key parity rule (below) and per-field type/range checks

### 3.1 Unknown-Key & Extension Parity Rule (CHIRP / RFC 9413)

For every uint-keyed CBOR map in Nektar (`nkt1` outer record, signed envelopes, `WantAd`, `OfferAd`, `AnnounceAd`, `PackRef`, `HubFeeAdvert`, `AttestationAd`, `.nkt` capsule records, `BlobObject`, `TreeObject`, `CommitObject`, `TransitionRecord`, `RefinementEdge`, `PurchaseRequest`, `PurchaseResponse`, `DealWAL`):

- **Unknown EVEN keys** (`k % 2 == 0`): **CRITICAL.** Receivers **MUST** reject the record fail-closed (`core.ErrUnknownCriticalKey`).
- **Unknown ODD keys** (`k % 2 == 1`): **NON-CRITICAL.** Receivers **MUST** accept the record, ignore the semantics locally, and **SHOULD** preserve the exact `(key, value)` pairs verbatim in `Extra` so decode→re-encode round-trips never destroy newer peers' optional metadata (DDIA Ch. 4 Fig. 4-7).
- **Reserved Extension Key Space:** To prevent accidental version lock-in, all v0.1 frozen fields occupy contiguous low keys (documented below). Any **future** schema addition:
  - **MUST** use an **ODD integer key `≥ 101`** if optional / forward-compatible.
  - **MUST** use an **EVEN integer key `≥ 100`** if mandatory / breaking.
  - **MUST NOT** reuse a previously retired key number or change the wire type of an existing key.
- **RFC 9170 Greasing Space:** Odd keys `65501..65535` (specifically `65535`) are reserved for non-critical protocol greasing. Decoders **MUST** accept and preserve grease keys; nodes **MAY** emit grease keys in test/canary modes (`NEKTAR_WIRE_GREASE=1`).

### 3.2 Outer `nkt1` Record Framing (`bcommon/record`)

| Key | Type | Required | Description |
|---|---|---|---|
| `0` | `bstr` | **MUST** | Magic bytes `"nkt1"` (`0x6e6b7431`) |
| `1` | `uint` (`1..255`) | **MUST** | Artifact kind: `1` = `want`, `2` = `offer`, `3` = `announce`, `4` = `attestation`. Unknown **even** kinds `≥ 6` **MUST** fail closed (`ErrUnknownCriticalKind`); unknown **odd** kinds `≥ 5` **MUST** decode cleanly as `ext:<n>` preserving raw `Body` + `Extra` (RFC 6709). |
| `2` | `bstr` | **MUST** | Exact canonical CDE CBOR bytes of the inner `Signed*` envelope (`{1: rawAdCBOR, 2: sigDER}`). |
| `≥ 3` (odd) | any CDE | **MAY** | Non-critical extension keys (preserved verbatim in `MarketPayload.Extra`). |

### 3.3 Signed Envelope Framing (`SignedWant`, `SignedOffer`, `SignedAnnounce`, `SignedAttestation`)

| Key | Type | Required | Description |
|---|---|---|---|
| `1` | `bstr` | **MUST** | Exact raw canonical CDE CBOR bytes of the inner artifact (`rawAdCBOR`). |
| `2` | `bstr` | **MUST** | Strict low-S DER-encoded ECDSA signature bytes over `DSHA256(domainTag ‖ rawAdCBOR)`. |

Verifiers **MUST** verify signatures over the exact received `rawAdCBOR` bytes (NOT over a re-encoded struct) so unknown odd keys added by newer writers remain cryptographically authenticated across older verifiers.

---

## 4. Domain-Separation Tags (Exact v0.1 Values)

Every signature and hash commitment **MUST** use an explicit domain-separation tag and **DSHA-256** (`SHA256(SHA256(tag ‖ payload))`) with strict canonical compressed 33-byte secp256k1 public keys and low-S DER ECDSA signatures. Unregistered types **MUST** fail closed (zero generic fallback tag).

| Artifact / Protocol | Exact ASCII Tag Bytes | Source Constant |
|---|---|---|
| `WantAd` | `nektar:artifact:want:v2` | `internal/bstack/wire_cbor.go` |
| `OfferAd` | `nektar:artifact:offer:v2` | `internal/bstack/wire_cbor.go` |
| `AnnounceAd` | `nektar:artifact:announce:v2` | `internal/bstack/wire_cbor.go` |
| `AttestationAd` | `nektar:artifact:attestation:v2` | `internal/bstack/wire_cbor.go` |
| `KeyedSaleOffer` (v2) | `nektar:sale-offer:v2` | `core.TagSaleOfferV2` |
| `DealReceipt` | `nektar:deal-receipt:v1` | `core.TagDealReceiptV1` |
| `PurchaseRequest` | `nektar:market:purchase:v1` | `core.TagMarketPurchaseV1` |
| `HubEgressAuth` | `nektar:hub:egress:v1` | `core.TagHubEgressV1` |
| `BboxSignablePayload` | `nektar:bbox:send:v1` | `core.TagBboxSendV1` |
| `BboxInboxAuth` | `nektar:bbox:inbox:v1` | `core.TagBboxInboxV1` |
| Shard SSE Feed Auth | `nektar:feed-auth:v1` | `core.TagFeedAuthV1` |
| `.nkt` Capsule Trailer | `nektar:capsule:trailer:v1` | `core.TagCapsuleTrailerV1` |
| `.nkt` Capsule AEAD | `nektar:capsule:aead:v1` | `core.TagCapsuleAEADV1` |
| `TransitionRecord` | `nektar:transition:v1` | `core.TagTransitionV1` |

---

## 5. Exact CBOR Key Registries (`nkt1` Artifacts)

### 5.1 `WantAd` (Frozen Keys `1..14`; Extensions `≥ 100`)

| Key | Field | Type | Req | Notes |
|---|---|---|---|---|
| `1` | `wantId` | `tstr` | **MUST** | Non-empty unique want identifier |
| `2` | `pub` | `tstr` | **MUST** | 33-byte compressed hex buyer pubkey |
| `3` | `net` | `tstr` | **MUST** | `main` \| `testnet` \| `regtest` \| `memory` |
| `4` | `domains` | `[tstr]` | **MUST** | `1..3` domain tags (`MaxDomains = 3`) |
| `5` | `query` | `tstr` | **MUST** | ≤ 1024 bytes |
| `6` | `ontologyId` | `tstr` | **MAY** | 64-hex `TagOntologyV1` commitment (RFC-001) |
| `7` | `maxSats` | `uint` | **MUST** | Maximum price in satoshis |
| `8` | `minGrade` | `tstr` | **MAY** | `C` \| `B` \| `A` \| `S` (RFC-002) |
| `9` | `maxSatsPer1kUnits` | `uint` | **MAY** | Normalized milli-sats/triple cap (RFC-002) |
| `10` | `freshnessClass` | `tstr` | **MAY** | `ephemeral` \| `versioned` \| `evergreen` |
| `11` | `requiredRights` | `tstr` | **MAY** | `inference_only` \| `distill_resellable` |
| `12` | `pricingMode` | `tstr` | **MAY** | `spot` \| `per_query` \| `subscription` |
| `13` | `revision` | `int` | **MUST** | Monotonic want revision |
| `14` | `exp` | `int` | **MUST** | Unix ms expiry; **MUST** satisfy `nowMs ≤ exp ≤ nowMs + 7d + 5m` |

### 5.2 `OfferAd` (Frozen Keys `1..37`; Extensions `≥ 100`)

| Key | Field | Type | Req | Key | Field | Type | Req |
|---|---|---|---|---|---|---|---|
| `1` | `offerId` | `tstr` | **MUST** | `20` | `ontologyId` | `tstr` | **MAY** |
| `2` | `wantId` | `tstr` | **MUST** | `21` | `edgesRoot` | `tstr` | **MAY** |
| `3` | `pub` | `tstr` | **MUST** | `22` | `chirpUrl` | `tstr` | **MAY** |
| `4` | `buyerPub` | `tstr` | **MUST** | `23` | `keyCommitHex` | `tstr` | **MAY** |
| `5` | `net` | `tstr` | **MUST** | `24` | `payAddress` | `tstr` | **MAY** |
| `6` | `domains` | `[tstr]` | **MUST** | `25` | `payIdentityKey` | `tstr` | **MAY** |
| `7` | `priceSats` | `uint` | **MUST** | `26` | `sellerUrl` | `tstr` | **MAY** |
| `8` | `grade` | `tstr` | **MAY** | `27` | `bboxOffice` | `tstr` | **MAY** |
| `9` | `unitCount` | `uint` | **MAY** | `28` | `saleOffer` | `bstr` | **MAY** |
| `10` | `satsPer1kUnits` | `uint` | **MAY** | `29` | `description` | `tstr` | **MAY** (≤ 280B) |
| `11` | `freshnessClass` | `tstr` | **MAY** | `30` | `tags` | `[tstr]` | **MAY** (≤ 8 × 32B) |
| `12` | `validUntilUnix` | `int` | **MAY** | `31` | `bondOutpoint` | `tstr` | **MAY** |
| `13` | `subjectVersion` | `tstr` | **MAY** | `32` | `bondBeefHex` | `tstr` | **MAY** |
| `14` | `usageRights` | `tstr` | **MAY** | `33` | `isHub` | `bool` | **MAY** |
| `15` | `pricingMode` | `tstr` | **MAY** | `34` | `direct` | `bool` | **MAY** |
| `16` | `subscriptionSec` | `int` | **MAY** | `35` | `revision` | `int` | **MUST** |
| `17` | `feedUrl` | `tstr` | **MAY** | `36` | `exp` | `int` | **MUST** |
| `18` | `shard` | `tstr` | **MAY** | `37` | `payloadFormat` | `tstr` | **MAY** (`"nkt"`) |
| `19` | `leaf` | `tstr` | **MUST** (64-hex) | | | | |

### 5.3 `AnnounceAd` (Frozen Keys `1..18`), `PackRef` (`1..20`), `HubFeeAdvert` (`1..6`)

- **`AnnounceAd`** lands on `tm_nektar_<net>_<nibble('dir')>` (and matching domain/ontology lanes):
  `1: pub`, `2: net`, `3: handle`, `4: publicUrl`, `5: bboxOffice`, `6: hosts[]` (**MUST**, 1..8 SSRF-screened URLs), `7: domains[]`, `8: sellDomains[]`, `9: bountyDomains[]`, `10: packs[]` (`PackRef`), `11: bondOutpoint`, `12: bondBeefHex`, `13: isHub`, `14: hubRole` (`spine|leaf`), `15: hubPayIdentityKey`, `16: hubTermsUrl`, `17: hubFees` (`HubFeeAdvert`), `18: exp`.
- **`PackRef`:** `1: shard`, `2: domains[]`, `3: chirpUrl`, `4: rootHex`, `5: ontologyId`, `6: targetOntologyId`, `7: count`, `8: grade`, `9: unitCount`, `10: pricingMode`, `11: enabledModes[]`, `12: priceSats`, `13: satsPer1kUnits`, `14: queryPriceSats`, `15: subscriptionPriceSats`, `16: subscriptionSec`, `17: freshnessClass`, `18: usageRights`, `19: queryEndpoint`, `20: feedEndpoint`.
- **`HubFeeAdvert`:** `1: submitWantSats`, `2: submitOfferSats`, `3: submitAnnounceSats`, `4: bboxSendSats`, `5: egressSatsPer100Events`, `6: egressMinTopupSats`.

### 5.4 `AttestationAd` (Frozen Keys `1..12`)

`1: attestHash` (64-hex), `2: role` (`buyer_rates_seller|seller_rates_buyer`), `3: pub` (issuer, **MUST NOT** equal `subjectPub`), `4: subjectPub`, `5: net`, `6: domains[]`, `7: ontologyId`, `8: dealId`, `9: dealReceiptHash` (64-hex), `10: token` (`nektarrep1.*`, ≤ 12288B), `11: compositeBp` (`0..10000`), `12: exp`.

---

## 6. Sale Quote (`KeyedSaleOffer` v2) and Escrow Covenant

Canonical Go source: `internal/contracts/sale.go` and `internal/contracts/artifacts/DisclosureSale.json`.

- `KeyedSaleOffer` (`version >= 2` **REQUIRED**): carries `cfg` (`sellerPkh`, `price`, `hWrapped`, `refundAfter`, `maxFee`), `sellerPub`, `keyCommitment`, `saltHex` (4B), `ciphertextHex` (or `ciphertextSha256` when CHIRP-offloaded), `expiresAt`, `offerSig`, `artifact` (`{artifactId: "DisclosureSale", scriptHash}`), `direct`, and `payloadFormat` (`"nkt"`).
- **Unified BRC-369 Fair Exchange Invariant:** `cfg.hWrapped` **MUST** equal `keyCommitment` (`keyed.Commitment(k)`).
- **`OfferDigestV2`:** `DSHA256("nektar:sale-offer:v2" ‖ Field(sellerPkh) ‖ Field(hWrapped) ‖ u64le(price) ‖ u64le(refundAfter) ‖ u64le(maxFee) ‖ Field(buyerPub) ‖ Field(leaf) ‖ u64le(expiresAt) ‖ Field(keyCommit) ‖ Field(salt) ‖ Field(ctHash))` where `Field(b) = u32le(len(b)) ‖ b`.
- **Covenant Pinning:** Buyers **MUST** refuse any quote whose `artifact.scriptHash` differs from the locally compiled `DisclosureSale` script hash.
- **Direct Rail (`direct: true`):** Buyer pays BRC-29 P2PKH directly; no covenant refund path exists. Buyers **SHOULD** restrict direct rail to prices ≤ configured `ZeroConfMaxSats` / `directMaxSats`.
- **Escrow Rail (`DisclosureSale`):** Buyer funds the deterministic Rúnar covenant script; Seller claims `price` by revealing BRC-369 scalar `k` on-chain (`Buy(k)`); Buyer reclaims after `refundAfter` via `Refund()` (supervised durably by `EscrowGuardianWorkflow`).

---

## 7. Universal `.nkt` Memory Capsule Wire Format (`NKTCAP1\n`)

Canonical Go source: `internal/memrepo/{capsule,capsule_codec,objects_cbor}.go`.

- **Container Header:** 8-byte ASCII magic `"NKTCAP1\n"` + 1-byte mode (`0x00` = plain zstd RFC 8742 CBOR Sequence; `0x01` = outer BRC-42/ECDH AES-256-GCM chunked envelope over zstd CBOR Sequence). Unknown mode bytes **MUST** fail closed.
- **Record Sequence (key `0` = `recType`):**
  `1` Header · `2` BlobMeta · `3` BlobChunk (≤ 1 MiB) · `4` Tree · `5` Commit · `6` Transition · `7` Anchor · `8` FragmentMeta · `9` BatchProof · `10` Trailer (mandatory final record with 32B DSHA-256 `streamDigest` + `recordCounts` + optional low-S DER signature).
- **Unknown Record Types:** Unknown **even** `recType` **MUST** fail closed; unknown **odd** `recType` **MUST** be hashed into `streamDigest`, counted in `recordCounts`, and ignored cleanly.
- **Unknown Keys Inside Records:** Every capsule record map **MUST** enforce `SplitCBORMap` (unknown even keys fail closed; unknown odd keys accepted/preserved).

---

## 8. LightBSV Interop: `boxrec.Envelope`, `storerec.Entry` & `BboxMessage`

- **Tier-1 NAT Messaging (`boxrec.Envelope` / `BboxMessage`):**
  - Native BRC-169 + BRC-78 envelopes (`tm_bbox_<office>`, `boxrec.Envelope`) reserve keys `1..7` and preserve unknown keys `> 7` verbatim in `Extra cbor.Map`.
  - Oversized payloads (`> 16 KiB` `boxrec.MaxContent`) fall back to `BboxSealedBody` v2 (ephemeral secp256k1 ECDH + RFC 5869 HKDF-SHA256 + AES-256-GCM with length-framed `BboxAAD`). Non-v2 `BboxSealedBody` envelopes **MUST** fail closed.
  - Relayed `BboxMessage` HTTP submissions (`POST /bbox/send`) **MUST** sign `BboxSignablePayload` (`keys 1..9`, tag `nektar:bbox:send:v1`) with `±60s` timestamp and single-use nonce replay checks.
- **Tier-2 Volumetric Storage (`storerec.Entry`, `ste\x01` + BRC-167 CHIRP):**
  - Capsules/ciphertexts `> 16 KiB` are offloaded to content-addressed CHIRP (`chirp://<sha256>`, `PUT/GET /chirp/v1/objects/{hash}`) and described by Lightweb's `storerec.Entry` (`tm_bstore_<bucket>`), which preserves unknown `Kind` (`3..255`) in `Unknown cbor.Map` and unknown keys `≥ 16` in `Extra cbor.Map`.
- **Purchase Wire (`POST /v1/market/purchase`, `Content-Type: application/cbor` or `application/json`):**
  - `PurchaseRequest` CBOR (`keys 1..16`), `PurchaseResponse` CBOR (`keys 1..13`), `ProofBundle` (`keys 1..10`), `AnchorLink` (`keys 1..9`). Decoders **MUST** fail closed on field type mismatches and unknown even keys.

---

## 9. Object Plane (`BRC-149`), SSE (`/events`) & Manifest Capabilities

- **BRC-149 Plane Framing (`internal/hub/plane_brc149_codec.go`):**
  - Delivery record: `[32B TopicID = SHA256(topic)][4B uint32 BE len][BEEF]` (`application/vnd.nektar.brc149`).
  - Numbered relay frame: `[8B uint64 BE FlowSeq][32B TopicID][4B uint32 BE len][BEEF]` (`application/vnd.nektar.brc149-stream`).
  - Future framing changes **MUST NOT** alter this fixed binary header; extensions **MUST** be negotiated via HTTP `Accept`/`Content-Type` media types and `/manifest.json`.
- **Gap Repair & Backpressure:**
  - BRC-149 clients detecting `FlowSeq > cursor + 1` **MUST** invoke `GET /plane/nack?from=<a >&to=<b>` before advancing their cursor.
  - SSE `/events` servers detecting slow-consumer channel overflow **MUST** emit `event: gap` (and close/resync) rather than silently dropping events; clients **MUST** resync from their persisted cursor upon receiving `event: gap` or `event: cursor_reset`.
- **BRC-180 `/manifest.json`:** Hubs serve standard `metanet.{overlays, trust, handles, bstore, brc223}` plus an optional backward-compatible `metanet.nektar` capability object (`wireVersion`, `supportedKinds`, `cborExtensionRule`, `planeMediaTypes`) for RFC 6709 / RFC 9170 capability discovery.
- **Loop Suppression (`LoopGuard`):** Hubs **MUST** key `LoopGuard` on `SHA256(SHA256(rawBEEF) ‖ SHA256(topic))` across both `/submit` and BRC-149 relay paths so byte-identical echoes are suppressed while proof-refreshed BUMP updates pass cleanly.

---

## 10. Multi-Mode HTTP Contracts & Ticker (RFC-002)

- `POST /v1/market/query` — `per_query` metered recall over BRC-105 HTTP 402 returning verified items + signed `nektardr1.*` receipt.
- `POST /v1/market/subscriptions` — `subscription` activation over BRC-105 HTTP 402 returning `subId`, `feedUrl`, and signed `nektardr1.*` receipt.
- `GET /v1/market/feeds/{shard}` — authenticated SSE Shard Feed requiring `X-Nektar-Sub-ID`, `X-Nektar-Pub`, `X-Nektar-Ts` (`±60s` replay window), `X-Nektar-Nonce` (single-use per-host replay cache), and `X-Nektar-Sig` over `DSHA256("nektar:feed-auth:v1" ‖ CanonicalCBOR({1: shard, 2: subId, 3: tsUnix, 4: nonce, 5: audience}))`.
- `GET /v1/market/ticker` — zero-float Commodity Ticker aggregating live quotes by `(ontologyId, grade, pricingMode)`.
