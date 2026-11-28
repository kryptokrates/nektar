# RFC-003: Wire Evolution, Protocol Greasing, Topic Extensibility & Durability Invariants

- **Status:** Implemented (`v0.1`)
- **Normative References:**
  - IETF BCP 14 ([RFC 2119](https://www.rfc-editor.org/rfc/rfc2119), [RFC 8174](https://www.rfc-editor.org/rfc/rfc8174)) — Normative Key Words
  - IAB [RFC 9413](https://www.rfc-editor.org/rfc/rfc9413) — Maintaining Robust Protocols
  - IAB [RFC 9170](https://www.rfc-editor.org/rfc/rfc9170) — Long-Term Viability of Protocol Extension Mechanisms
  - IAB [RFC 6709](https://www.rfc-editor.org/rfc/rfc6709) — Design Considerations for Protocol Extensions
  - IETF [RFC 8949 §4.2.1](https://www.rfc-editor.org/rfc/rfc8949) — CBOR Core Deterministic Encoding (CDE)
  - IETF [RFC 8742](https://www.rfc-editor.org/rfc/rfc8742) — CBOR Sequences
  - Martin Kleppmann, *Designing Data-Intensive Applications* (DDIA), Chapters 4, 8, and 9
- **Reference Implementation:** `internal/core`, `internal/bstack`, `internal/memrepo`, `internal/hub`, `internal/eventbus`, `internal/store`, `internal/deals`, `internal/market`

---

## 1. Normative Language (RFC 2119 / RFC 8174)

The key words **"MUST"**, **"MUST NOT"**, **"REQUIRED"**, **"SHALL"**, **"SHALL NOT"**,
**"SHOULD"**, **"SHOULD NOT"**, **"RECOMMENDED"**, **"NOT RECOMMENDED"**, **"MAY"**, and
**"OPTIONAL"** in this document are to be interpreted as described in BCP 14
(RFC 2119, RFC 8174) when, and only when, they appear in all capitals, as shown here.

---

## 2. Canonical Encoding & Schema Evolution (DDIA Ch. 4 / RFC 9413)

### 2.1 Deterministic CBOR & Bounded Maps
1. Every cryptographically signed or content-addressed Nektar wire message **MUST** be encoded in RFC 8949 §4.2.1 Core Deterministic CBOR (`core.EncodeCBOR` / `bcommon/cbor`).
2. All uint-keyed CBOR maps **MUST** enforce `core.MaxCBORMapKeys` (`64`, matching `bcommon/record.MaxKeys`) before allocating or indexing fields.
3. Decoders **MUST NOT** silently ignore per-field type mismatch errors; any mismatched CBOR type on a defined key **MUST** fail closed.

### 2.2 Forward & Backward Compatibility Rules
1. **Never reuse a key number** and **never change the CBOR major type** of an existing key.
2. **Never add a new REQUIRED field** to an existing schema version; new fields added after initial deployment **MUST** be optional or have well-defined defaults when absent (DDIA Ch. 4).
3. **Unknown-Key Parity Rule:**
   - Unknown **EVEN** keys (`k % 2 == 0`) are **critical**; decoders **MUST** reject the message fail-closed (`core.ErrUnknownCriticalKey`).
   - Unknown **ODD** keys (`k % 2 == 1`) are **non-critical**; decoders **MUST** accept the message and **SHOULD** preserve the `(key, value)` pairs verbatim in `Extra` so read-modify-write / re-encode paths never strip unknown fields written by newer peers (DDIA Ch. 4 Fig. 4-7).
4. **Reserved Extension Key Space:** All future optional fields **MUST** allocate odd keys `≥ 101`; all future mandatory/breaking fields **MUST** allocate even keys `≥ 100`.
5. **Exact-Bytes Signature Verification:** Signature verification on `SignedWant`, `SignedOffer`, `SignedAnnounce`, and `SignedAttestation` **MUST** hash the exact received `rawAdCBOR` bytes verbatim, never a re-encoded Go struct, so unknown odd fields remain authenticated.

### 2.3 LightBSV Schema Compatibility Matrix

| Wire Record | Magic / Container | Unknown Key Rule | Unknown Kind Rule | Preservation Mechanism |
|---|---|---|---|---|
| `nkt1` (`bstack.MarketPayload`) | `b"nkt1"` (`bcommon/record`) | Even → fail closed; Odd → preserved | `1..4` core; Odd `≥ 5` → `ext:<n>` passthrough; Even `≥ 6` → fail closed | `MarketPayload.Extra` + `Signed*.RawCBOR` + struct `Extra` |
| `boxrec.Envelope` (`tm_bbox_*`) | `bb\x01` (`bcommon/record`) | Keys `> 7` preserved verbatim | `TxEnvelope` / `TxAck` | `boxrec.Envelope.Extra cbor.Map` |
| `storerec.Entry` (`tm_bstore_*`) | `ste\x01` (`bcommon/record`) | Keys `≥ 16` preserved verbatim | `Kind` `3..255` preserved unchecked | `storerec.Entry.Unknown` + `storerec.Entry.Extra` |
| `.nkt` Capsule (`NKTCAP1\n`) | RFC 8742 CBOR Sequence | `SplitCBORMap` on EVERY record map (even fail-closed, odd preserved) | `1..10` core; Odd `≥ 11` ignored+hashed; Even `≥ 12` fail closed | `Extra` on `BlobObject`, `TreeObject`, `CommitObject`, `CapsuleHeader` + `Trailer.StreamDigest` |

---

## 3. Extension Viability & Protocol Greasing (RFC 9170)

To prevent middleboxes, Hubs, and foreign implementations from ossifying on exact v0.1 field sets:

1. **Mandatory-to-Implement (MTI) Core vs. Optional Extensions:**
   - MTI: `nkt1` kinds `1..4`, 16 nibble lanes `tm_nektar_<net>_0..f`, `ls_nektar`, `DisclosureSale` v2, `.nkt` mode `0x00` & `0x01`, `boxrec.Envelope`, `storerec.Entry`, BRC-149 binary framing.
   - Optional: NMU-1 commodity filters, ontology pinning, dynamic `dom_*`/`ont_*` topics, BRC-223 UDP multicast fabric, odd CBOR extension keys.
2. **Grease Key Space:** Odd CBOR keys `65501..65535` (canonical grease key `core.GreaseKey = 65535`) are permanently reserved for protocol greasing.
   - Implementations **MUST NOT** assign semantic meaning to keys `65501..65535`.
   - Implementations **MUST** accept and preserve grease keys in all CBOR decoders and overlay validators.
   - When `NEKTAR_WIRE_GREASE=1` is set (RECOMMENDED in CI, testnet, and canary deployments; OFF by default on mainnet to minimize byte fees), encoders **MAY** attach a small deterministic odd grease entry (`{65535: b"gr"}`) to exercise extension paths continuously.
3. **Capability Advertisement:** Hubs **SHOULD** expose `metanet.nektar` inside BRC-180 `/manifest.json` (`wireVersion`, `supportedKinds`, `cborExtensionRule`, `planeMediaTypes`) so peers negotiate new capabilities explicitly rather than probing or overloading fields.

---

## 4. Topic & Message-Type Independence (RFC 6709)

1. Adding new topic families (`tm_bbox_<office>`, `tm_bstore_<bucket>`, `tm_nektar_<net>_dom_<slug>`, `tm_nektar_<net>_ont_<id16>`, or future `tm_*`) **MUST NOT** break older Hubs or Brokers.
2. Hubs receiving `POST /submit` for an unrecognized topic **MUST** skip that topic without failing admitted known topics in the same request.
3. Clients consuming `/plane/stream` or BRC-223 datagrams **MUST** ignore unrecognized `TopicID`s while still advancing their flow cursor cleanly.
4. BRC-149 `PlaneFrame` (`[8B FlowSeq][32B TopicID][4B len][BEEF]`) is frozen for 1BSV Paper 010 §8.2 compatibility; future framing revisions **MUST** use a distinct HTTP media type (`Accept` / `Content-Type`) negotiated via `/manifest.json`.

---

## 5. Distributed Clocks, Fencing, Idempotency & Replay (DDIA Ch. 8–9)

### 5.1 Clocks and Monotonicity
1. **Wall-clock jumps (NTP steps backward):**
   - `SpendGuard` daily rollover **MUST** enforce a monotonic UTC day floor (`today > dayKey`), never resetting `spentToday` on a backward midnight step.
   - `SpendGuard` `MinInterval` pacing **MUST** treat negative `now.Sub(lastActivity)` from a backward clock step safely (never deadlocking broadcasts).
   - Live nodes **SHOULD** run `internal/autonomy.Watchdog` SNTP median skew checks (tripping autonomous spend before the `±60s` HTTP replay window is breached).
2. **Workflow Timers:** Durable sagas (`go-workflows` behind `RestateClient`) **MUST** use deterministic `workflow.Now(ctx)` and `workflow.ScheduleTimer` inside workflow functions, never `time.Now()` or `time.Sleep()`.

### 5.2 Fencing Tokens on Mutable Deal State
1. `deals.revision` acts as a monotonic fencing token per `deal_id`.
2. Direct SQLite writes (`SaveDeal`, `SaveDealWithObligations`, `SettleDealAndFulfillObligations`, `ProjectSettledDealsBatch`) **MUST NOT** blindly `INSERT OR REPLACE` over higher-revision rows; upserts **MUST** fence on `excluded.revision >= deals.revision` (incrementing `revision`) and **MUST NOT** regress a `settled` deal to an earlier status except via explicit chain-reorg rollback (`RollbackDealsFromHeight`).
3. `FastMarketJournal` spend reservations (`ReserveSpendAtomic`) **MUST** persist to Pebble WAL (not RAM-only) and reload on restart so crashes never reset daily spend caps.

### 5.3 Idempotency, Loop Suppression & Bounded Replay
1. **`LoopGuard` Key Consistency:** Both `Hub.submitInternal` (for `nkt1` and module outputs) and `plane_brc149_client` **MUST** key `LoopGuard` on `SHA256(SHA256(rawBEEF) ‖ SHA256(topic))` so echoes are suppressed while proof-refreshed BUMPs pass.
2. **Zero Silent Drops on Slow Consumers:**
   - `EventBus.WaitFor` **MUST** observe `sub.Wake()` / `sub.TakeGap()` and return `eventbus.ErrEventGap` on buffer overflow rather than hanging until timeout.
   - Hub SSE `/events` **MUST** emit `event: gap` (and close cleanly) when a subscriber buffer overflows so clients immediately resync from their persisted cursor.
3. **Durable Workflow Lifecycle (`go-workflows` / `RestateClient`):**
   - `deals.Runtime.CreateOrAttach` **MUST** attach to an existing instance ONLY on `backend.ErrInstanceAlreadyExists`, propagating real SQLite/storage errors fail-closed.
   - Long-running loop workflows (`StandingOrderWorkflow`, `EscrowGuardianWorkflow`) **MUST** bound `workflows.sqlite` history growth using `workflow.ContinueAsNew` after a fixed iteration budget (`256`), carrying forward state and bounded deduplication sets cleanly.
