# fabric — the 1BSV / Lightweb bstack model → rules for the delivery layer

Source family: **1bsv.net** (16 engineering papers, 6 pattern briefs,
BRC-124…149 standards) + **github.com/lightwebinc** (39 repos). This repo
already implements the family: `packages/bstack` vendors
`@lightwebinc/bcommon` (carriers, funding trees, unlock scripts) and
`@lightwebinc/bfinger-host` (the committed-record spec), and `finger.ts`,
`tree.ts`, `lanes.ts`, `journal.ts`, `market-wire.ts` are the local
instances of their machinery. Findings use `fab/*` IDs.

## The three bstack ideas

1. **State proves itself** — every change is a BSV transaction signed by
   its owner; only a few hundred bytes are mined; the data rides beside
   them provable and retractable (Paper 011 — the data carrier).
2. **Publish once** — each publication is delivered once to every
   subscribed host at the same moment; loss is repaired by the network;
   the hundredth subscriber costs the publisher nothing (Paper 006/009).
3. **Readers verify, hosts compete** — every answer checks against block
   headers the reader holds; a host is a replaceable service, never the
   keeper of truth (Papers 009/016).

## Paper → rule map

| Paper                            | Principle                                                                                          | Rule                           |
| -------------------------------- | -------------------------------------------------------------------------------------------------- | ------------------------------ |
| 015 Loss on the Internet         | every frame carries a sequence from the door; a gap is seen the instant the next frame arrives     | `fab/unnumbered-stream`        |
| 015 + BRC-126                    | detected gaps get repaired by name through a cache (frame / not-yet / never-had)                   | `fab/gap-unrepaired`           |
| 015                              | a resend can never pose as a new frame — repair binds seq+source                                   | `fab/resend-forgeable`         |
| 010 Sharding the object plane    | topics are addresses (leading hash bits = lane); "a lane nobody joined is never sent"; join budget | `fab/firehose-subscription`    |
| 002 Asset service at scale       | announce-then-pull breaks: origin egress, concurrency, the block race                              | `fab/announce-pull`            |
| 009 + 016                        | hosts are interchangeable replicas competing on service, never custody                             | `fab/custody-host`             |
| 013 Frames on 1500-byte Internet | payloads over the path's frame ceiling are _silently lost_ — fragment or fail loud                 | `fab/fat-frame`                |
| 014 One source, not any source   | single-source join beats any-source: identity survives every tier                                  | `fab/source-ambiguity`         |
| 011 The data carrier             | data needing no mining rides carrier txs that never mine                                           | `fab/mineable-carrier`         |
| 006/009                          | deliver-once: subscribers don't re-pull what the plane already delivered                           | `fab/pull-what-you-subscribed` |
| 003 Ingress economics            | admission is free, delivery is the product — sender never configures                               | `fab/sender-pays-config`       |

## What each rule demands in nektar

### `fab/unnumbered-stream` — a feed without sequence/cursor

Any delivered stream (watch responses, lane publications, outbox rows,
blogs-style batches) needs a monotonically numbered position the
consumer persists — the cheap form is the watch-shim's `cursor`. A
stream you can't name a position in can't tell "nothing new" from
"missed item #47". blogs' rule: signed, _numbered_ batches.

### `fab/gap-unrepaired` — gap detected, no repair path

Once a stream is numbered, a skipped position must route to repair:
re-pull by name (cursor resume), a reconcile sweep, or a NACK-style
endpoint. "Noticed the gap" without "repaired the item" is telemetry,
not resilience. The retry-endpoint repo is the reference shape.

### `fab/resend-forgeable` — repaired/replayed data accepted unbound

A repair response must carry the same authentication as live delivery —
the item is bound to {source, position, payload} — or the repair path is
an injection vector. Locally: replayed BEEF/envelopes verify the same
signatures as first delivery; cursor results re-validate.

### `fab/firehose-subscription` — joins wider than the interest set

Subscriptions derive from live interest — `engine.watchList()` feeds
`refreshWatches`, and dead entries get aborted (runtime.ts:1007). A
subscription that outlives its need costs the producer _and_ hides where
attention actually is. Every watch carries an owner/why; every watch has
an expiry or a re-derive sweep.

### `fab/announce-pull` — announce-then-pull where push exists

The pattern paper 002 prices: the recipient re-fetching each
announcement through the origin is the scaling break. Locally the
wake-hint + publish-to-their-hosts shape is already right — a finding is
a counterparty flow that announces-and-waits-for-pull when it could
deliver the artifact (sealed envelope, BEEF) in the same step.

### `fab/custody-host` — host as keeper of truth

A host stores, serves, and proves — it never _decides_. If a flow treats
host-held state as authoritative (can't rebuild it from chain/
store/digest), the host has custody and the reader stopped verifying.
The blogs/bstore family proves listings/completeness against anchors —
copies carry proof, not trust.

### `fab/fat-frame` — payload over the delivery ceiling

Anything shipped through a transport with a datagram/size ceiling needs
a declared fragmentation story or a size guard that fails loudly —
"silently lost" is the literal failure mode Paper 013 names. Local:
envelope `maxBytes` params exist (`txWire(txid, maxBytes)`) — a path
that ignores them is this finding.

### `fab/source-ambiguity` — any-source where single-source fits

A subscription keyed only on topic accepts anything the plane emits;
keyed on {source, topic} it inherits the source's identity. Prefer
single-source joins for lanes whose publications must be signed by a
known principal — the finger record already names which hosts carry an
identity's artifacts.

### `fab/mineable-carrier` — data committed to blocks that only needed delivery

Only the commitment needs mining (a few hundred bytes); bulk data rides
the carrier/object plane and verifies against it. Writing bulk payload
into mined outputs is fee + latency + permanence for data that wanted
"provable, retractable" instead. The anchor-vs-payload split is already
the repo's shape — this flags drift from it.

### `fab/pull-what-you-subscribed` — re-pulling delivered data

If a subscription delivered an object, the consumer holding a copy must
not re-pull it per-use — deliver-once means the _host_ materializes it
from the plane once. A path that fetches per-read what its own watch
already pushed has rebuilt announce-pull inside the subscriber.

### `fab/sender-pays-config` — sender needs account/config to publish

"Admission is free, delivery is the product": the publish path takes a
signed artifact and nothing else. A sender needing endpoint accounts,
registered identities, or host-specific config to _send_ is a seam leak
— receivers carry delivery complexity.

## The repo's fabric inventory (audit anchors)

- **Interest → subscription**: `engine.watchList()` → `refreshWatches`
  (15s re-derive; abort dead subs)
- **Numbered/cursor streams**: `?cursor=` on watch-shim responses
- **Deliver-once + verify**: `publishArtifact` (lanes) → `missed[]`;
  BEEF envelopes in `market-wire.ts`; `onTxProof` → `markProven`
- **Repair**: `unblockOutbox` patterns; watchdog host-recovery; poll floor
- **Records**: `finger.ts` — bfinger committed-record spec (hosts,
  office, stream, watch, retract); `/.well-known/nektar`
- **Carrier/funding**: `tree.ts` (funding trees, MAX_TREE_OUTPUTS=1023),
  `journal.ts` (carrier journal), `lanes.ts` (lane-table ownership)
- **Loss injection**: `testing.ts` + Faulthost — the local NACK/withhold
  harness; new fabric paths need fault coverage here

## Standards reference (when checking wire conformance)

BRC-12/30 (tx formats) · BRC-124/128/130/142 (tx + EF frames,
fragmentation, bundles) · BRC-129/139 (group addressing, shard manifest)
· BRC-126 (NACK retransmission) · BRC-131/135/133/134/127/132/143/144
(block/header/subtree/anchor distribution) · BRC-148/149 (shard domains

- BEEF object plane). New wire shapes should name the BRC they implement
  or the one they deliberately diverge from — divergence without a note is
  `ev/bespoke-format` upstream of `fab/*`.

## Upstream reference repos (github.com/lightwebinc)

`bcommon` + `bfinger`/`bfinger-host` (vendored here) · `retry-endpoint`
(NACK cache — the repair reference) · `shard-proxy` (ingress sharding) ·
`teranode-bridge` (pushed delivery into unmodified clusters) ·
`overlay-bridge` (unmodified engine on the object plane) · `bstore` /
`bstore-bridge` (storage market + UHRP front) · `multicast-test`
(component fault harnesses — the Faulthost analog) · `interop` (TPNIL —
versioned interop programs + durable evidence; our conformance story).
