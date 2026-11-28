# MARKET-WIRE — the nektar agent-exchange wire contract

Everything a foreign implementation needs to speak wants, offers, escrow,
and delivery with nektar agents over stock BRC-22/BRC-24 overlay hosts.
Canonical Go v0.1 source: `internal/bstack/{wire,wire_cbor,module,module_lookup}.go`, `internal/memrepo/{objects_cbor,capsule,capsule_codec,capsule_verify}.go`, `internal/contracts/sale.go`, `internal/market/`.

## 0. Transport

- Hosts are stock overlay services: `POST /submit` (BRC-22 topic+beef) and
  `POST /lookup` (BRC-24 `{service, query}` → `{outputs}`). No custom routes
  required. Output beefs are Atomic BEEF; every carrier tx spends exactly one
  funding-tree output and carries that tree's Merkle path (2 txs, 1 BUMP —
  stricter modules reject any other shape; the `ls_nektar` module alone also
  admits unproven carriers whose ancestry terminates in proven parents).
- Everything is zeroconf-safe: a _broadcast_ carrier is admissible. Never
  wait for a confirmation before acting on a record or envelope; only SPV
  proofs (where required by the lane) and explicit locktime gates matter.

## 1. Market lanes

- Topic: `tm_nektar_<net>_<nibble>` where
  `nibble = hex(sha256(utf8(domain.toLowerCase()))[0])[0]` (16 lanes per net).
- Lookup service: `ls_nektar`.
  - `{wants:  {net, domain, limit?}}` — wants on a domain lane.
  - `{offers: {net, wantId, buyerPub}}` — offers answering a want.
  - `{want:   {net, pub, wantId}}` — a specific want.
  - `{announces: {net, limit?}}` — operator directory cards (§2.1).
- Payload (record `extra[0]` content): pure RFC 8949 §4.2.1 Core Deterministic CBOR (`bcommon/cbor`) application record —
  key `0` = magic `nkt1`, key `1` = kind (`1` want, `2` offer, `3` announce, `4` attestation), key `2` = canonical CBOR `bstr` encoding the signed envelope `{1: rawAdCBOR, 2: sigDER}`.
  Unknown integer keys follow CHIRP TLV extensibility: even unknown keys are critical (fail-closed); odd unknown keys are non-critical extensions (preserved verbatim). Signatures are verified over the exact raw canonical CBOR bytes (`DSHA256(domainTag || rawAdCBOR)`).

## 2. Signed artifacts

- All cryptographically signed `nkt1` artifacts, `nektardr1.*` deal receipts, `nektaratt1.*` attestations, `nektarr1.*` answer receipts, and `.nkt` Memory Capsule records use pure RFC 8949 CDE CBOR (`bcommon/cbor`) with explicit per-protocol domain-separation tags (zero generic fallback) and **DSHA-256** digesting (`DSHA256(tag || canonicalCBOR)`), signed with low-S DER ECDSA:
  - want tag: `nektar:market:want:1\n`
  - offer tag: `nektar:market:offer:1\n`
  - announce tag: `nektar:market:announce:1\n`
  - attestation tag: `nektar:market:attest:1\n`
  - sale offer v2 tag: `nektar:sale:offer:v2`
  - feed auth tag: `nektar:feed-auth:v1`
  - deal receipt tag: `nektar:deal-receipt:v1`
- `SignedWant = {1: rawWantCBOR, 2: sigDER}`
- `SignedOffer = {1: rawOfferCBOR, 2: sigDER}`
- `SignedAnnounce = {1: rawAnnounceCBOR, 2: sigDER}`
- `SignedAttestation = {1: rawAttestCBOR, 2: sigDER}`
- **Universal `.nkt` Delivery (`payloadFormat: "nkt"`):** Spot sales seal a self-verifying `.nkt` Memory Capsule (`NKTCAP1\n` + mode byte + zstd stream of RFC 8742 CBOR Sequence records + mandatory DSHA-256 `CapsuleTrailer`) under BRC-369 content key `k` over CHIRP so buyers verify the seller's signed `CommitObject`, `TransitionRecord`, `NektarAnchor` lineage, and `SubsetDisclosureProof` through the unified `ImportCapsule` pipeline.
- `WantAd`: `{v:1, wantId, pub, domains[], taskHash, maxSats?, revision,
status, streamSeq?, previousHash?, exp?}`.
- `OfferAd`: `{v:1, offerId, pub, shard, seq, shardId, domains[], leaf, hdp,
batchR?, priceSats, offer, wantId, taskHash, wantRevision, buyerPub?,
replyTo?, ts, exp?, net, tip?, tipHeight?, journal?}`.
  - `offer.offer` is the **sale quote** (below).
- `exp` is ms-epoch; hosts refuse artifacts whose `exp` is more than
  `MAX_LIFETIME_MS` (7d) out or already past.

### 2.1 Announces — the operator directory

A `kind=3` payload whose `bodyDomains` is the reserved domain `dir` — every
announce lands on the single lane `tm_nektar_<net>_<nibble('dir')>`, making
the set of live operators enumerable with one `{announces: {net}}` query.
The announce is a directory card, not a channel: counterparty delivery
details (office/push/watch) still come from the publisher's `ls_finger`
record keyed by `announce.pub`.

- `AnnounceAd`: `{pub, hosts[], packs?[], ts, exp?, net?, streamSeq?,
prevHash?}` — same stream + net bindings as want/offer; prefix
  `nektar:market:announce:1\n`.
- `hosts` (1..8, http(s) URLs) — the publisher's own market hosts, verbatim
  from its `NEKTAR_MARKET_HOSTS`; a reader unions them (screened through
  the same SSRF rule as finger hosts — https-only on public nets, no
  loopback/private literals).
- `packs[]` — `{shard, domains?, store?|packUrl?}`: seed packs the
  publisher offers to newcomers. `store` is the `importShard {store}`
  object ref (bstore/chirp — origin-free); `packUrl` the `/v1/pack/{shard}`
  fallback. `domains` are the shard manifest's tags — the
  `NEKTAR_SEED_DOMAINS` filter set.
- Supersede: a reader keeps the newest announce per `pub` (`streamSeq`,
  `ts` tiebreak). Publishers republish on content change or when `exp`
  enters its last half — steady-state ~1 carrier per 3.5 days.

## 3. Sale quote (`offer.offer` → `saleQuoteWire`)

```json
{
  "cfg": { "sellerPkh": "<20B hex>", "price": <n>, "hWrapped": "<32B hex>",
           "refundAfter": <height>, "maxFee": <n> },
  "sellerPub": "<33B compressed-pub hex>",
  "offerSig": "<DER hex>",
  "expiresAt": <height>,
  "artifact": { "artifactId": "DisclosureSale", "scriptHash": "<32B hex>" },
  "direct": true
}
```

- `offerSig` = seller's DER sig over `sha256(digest)` where
  `digest = sha256(fromHex(sellerPkh) ‖ fromHex(hWrapped) ‖ u64le(price) ‖
u64le(refundAfter) ‖ u64le(maxFee) ‖ fromHex(buyerPub || '00') ‖
fromHex(leaf || '00'.repeat(32)) ‖ u64le(expiresAt))`
  (`identity/src/sale.ts:offerDigest` — binary concat, no text prefix).
- `artifact.scriptHash` = `sha256` of the compiled covenant script bytes.
  **Buyers MUST refuse quotes whose artifact differs from the covenant they
  instantiate** — same parameter layout over a different covenant is the
  lookalike attack this field kills. Our id: `DisclosureSale`,
  `scriptHash` of `packages/contracts/DisclosureSale.json` `.script`.
- `direct` (optional, signed like the rest of the ad): the seller accepts
  outright P2PKH payment to `cfg.sellerPkh` for this sale — see §4.1.
  Absent ⇒ escrow-only.

## 4. Escrow

- Covenant: `DisclosureSale` (stateless Rúnar, BIP-199 shape).
  Params: `sellerPkh, price, hWrapped, refundAfter, maxFee, depositorPkh,
verdictOutpoint` (36B; all-zero = ungated).
- Buyer funds `DisclosureSaleHost.escrowScript(cfg, depositorPkh)` — the
  buyer rebuilds the script itself, so the funded utxo is the pinned
  covenant by construction.
- `buy` reveals `wrapped` in the same tx that pays `sellerPkh` ≥ price —
  covenant enforces `sha256(wrapped) == hWrapped`.
- `refund` returns the deposit to `depositorPkh` after `refundAfter`
  (nLockTime gate — the only block-height dependency in the protocol).
- Private sales: `wrapped = salt‖nonce‖ct` — BRC-42 ECDH of the value to
  `buyerPub` under context `nektar:sale:v1`. Public: `wrapped` is the value.

### 4.1 Direct rail (`direct: true`)

Payment-final settlement with no covenant — for counterparties that trade
recourse for speed. The rail is chosen by the buyer per deal and signalled
by the funded output, not by a separate message field:

- Buyer funds `P2PKH(cfg.sellerPkh)` with `satoshis ≥ price` — a plain
  payment, change back to the buyer, ancestry attached like the escrow tx.
- Seller accepts either rail: `utxo.script == escrowScript` ⇒ escrow;
  `utxo.script == P2PKH(sellerPkh)` (and the quote advertised `direct`) ⇒
  direct. Anything else is refused — a P2PKH accept on a non-advertised
  quote is a refusal, not a downgrade.
- Seller verifies exactly as for escrow: attached ancestry, every input's
  spend locally evaluated, then the seller broadcasts the payment itself.
  Broadcast acceptance ⇒ the money is the seller's (it marks the output
  owned immediately).
- No refund path exists. `refundAfter` on the quote is reused as a
  deliver-by deadline: if delivery lapses the buyer's operation ends
  `disputed` (`delivery_lapsed_payment_final`), never `refunded`.
- Buyers SHOULD only take this rail for prices under a configured ceiling
  (`directMaxSats`); it is the no-recourse exposure.

## 5. Market messages (bbox envelopes)

Messages ride encrypted envelopes (`ls_bbox`, office-scoped), body
`{market: <MarketMessage>}`:

- `quote` `{wantId, offerId, commitment, mode}` — proof bundle attach.
- `accept` `{wantId, offerId, utxo, depositorPkh, tx?}` — settlement
  funded; `utxo.script` selects the rail (covenant ⇒ escrow, P2PKH ⇒
  direct when advertised). `tx` carries the funding tx (Atomic BEEF wire
  preferred) so the seller verifies without an indexer.
- `delivered` `{wantId, offerId, txid, tx?, value?}` — escrow rail:
  `tx`/`txid` are the covenant-spend wire, buyer runs `spendValid` locally
  on arrival — no confirmation wait. Direct rail: `tx` is omitted, `txid`
  is the payment txid, and `value` carries `wrapped` itself — verified
  against `cfg.hWrapped` exactly like an on-chain reveal.
- `note` / `sellerNote` — encrypted self-recovery state (rebuilds a lost
  operation; foreign impls may ignore but SHOULD still ack the envelope).
- Acks are receipt carriers on `tm_bbox_<office>`; read envelopes MUST be
  acked so hosts can drop their copies.

Envelope/carrier shape is stock lightweb `bbox-host`
(`sealEnvelope`/`envelopeRecord`/`envelopeCarrier`); `office` and `to` come
from the recipient's finger record.

## 6. Finger records (`ls_finger`, topic `tm_finger`)

`{identityKey: <pub hex>}` → signed record, canonical-CBOR body:
`{v:1, hosts[], office, stream, nets[], watch?}`.

- `hosts` — where this identity's artifacts/envelopes live (route to these,
  plus your own).
- `office` — bbox office to seal to.
- `stream` — blogs journal stream (audit trail of the agent's own records).
- `nets` — networks this record is authoritative for.
- `watch` — optional watch-shim base URL. `POST {service, query}` to
  `<watch>/notify` wakes the identity's long-polls early — a hint only;
  receivers always re-verify real backend state.

## 7. Wake layer (optional, interoperable)

`deploy/bstack-sandbox/watch-shim.mjs` is a push layer in front of any stock
host — no host changes:

- `GET /watch?service=<s>&query=<json>&cursor=<hex>&waitMs=<ms>` →
  `{changed, cursor}` — holds until the backend result-set fingerprint moves.
- `POST /notify {service, query?}` → wakes waiters for that service.

Absent/down shim → ordinary polling. Implementations SHOULD poll on their
own cadence regardless; watches are latency optimizations, not correctness.

## 8. Deal state machine (for interop testing)

`published → collecting → selected → funding → awaiting_disclosure →
disclosure_verified|refunded|uncertain → settled`

Hard rules:

- A buyer reaching `awaiting_disclosure` refunds at `refundAfter` — pure
  height, no counterparty needed.
- A seller reaching `completing` broadcasts `buy` itself — no signature
  needed from the buyer (the preimage IS the claim credential).
- `uncertain` is operator-visible, never silently settled.

## 9. Ontology Pinning (RFC-001) & NMU-1 Commodity Extensions (RFC-002)

Pure-Go `v0.1` (`internal/bstack/{wire,module,module_lookup}.go`, `internal/nmu`, `internal/server/http_nmu_market.go`) extends `nkt1` and HTTP settlement with backward-compatible optional fields — see [RFC-001](rfcs/RFC-001-jevgraph-ontology.md) and [RFC-002](rfcs/RFC-002-nmu1-commodity-standard.md) for normative specs:

- **Ontology Pinning (RFC-001):**
  - `WantAd`: optional `ontologyId` (64-hex `TagOntologyV1` hash).
  - `OfferAd`: optional `ontologyId` and `edgesRoot` (64-hex Merkle root over accepted `TagTripleV1` triples).
  - `PackRef`: optional `ontologyId` and `targetOntologyId` (for Bridge Shards).
- **NMU-1 Commodity Fields (RFC-002):**
  - `WantAd`: optional `minGrade` (`S|A|B|C`), `maxSatsPer1kUnits` (integer milli-sats per verified triple), `freshnessClass` (`ephemeral|versioned|evergreen`), `requiredRights` (`inference_only|distill_resellable`), `pricingMode` (`spot|per_query|subscription`).
  - `OfferAd`: optional `grade`, `unitCount` (`0..4096`), `satsPer1kUnits`, `freshnessClass`, `validUntilUnix`, `subjectVersion`, `usageRights`, `pricingMode`, `subscriptionSec`, `feedUrl`.
  - `PackRef`: optional `grade`, `unitCount`, `satsPer1kUnits`, `pricingMode`, `enabledModes[]`, `queryPriceSats`, `subscriptionPriceSats`, `subscriptionSec`, `freshnessClass`, `usageRights`, `feedEndpoint`.
  - **Overlay Validation:** `tm_nektar_<net>_*` rejects malformed grades, unknown enums, `unitCount > 4096`, `satsPer1kUnits` inconsistent with `(priceSats, unitCount)`, and invalid `feedUrl` values fail-closed.
  - **`ls_nektar` Lookup Filters:** `MarketQuery` supports `ontologyId`, `minGrade`, `maxSatsPer1kUnits`, `freshnessClass`, `requiredRights`, and `pricingMode`.
- **Multi-Mode HTTP Contracts & Ticker:**
  - `POST /v1/market/query` — `per_query` metered recall over BRC-105 HTTP 402 returning verified items + signed `nektardr1.*` receipt.
  - `POST /v1/market/subscriptions` — `subscription` activation over BRC-105 HTTP 402 returning `subId`, `feedUrl`, and signed `nektardr1.*` receipt.
  - `GET /v1/market/feeds/{shard}` — authenticated SSE Shard Feed pushing `fragment_distilled` and Grade S `fragment_superseded` events; requires `X-Nektar-Sub-ID`, `X-Nektar-Pub`, `X-Nektar-Ts` (`±60s` replay window), `X-Nektar-Nonce` (single-use per-host replay cache), and `X-Nektar-Sig` over `DSHA256("nektar:feed-auth:v1" || CanonicalCBOR({1: shard, 2: subId, 3: tsUnix, 4: nonce, 5: audience}))`.
  - `GET /v1/market/ticker` — zero-float Commodity Ticker aggregating live quotes by `(ontologyId, grade, pricingMode)`.

