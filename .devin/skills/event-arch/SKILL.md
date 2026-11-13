---
name: event-arch
description: >
  Use to audit or design the realtime/event layer of the UTXO on-chain
  architecture — wait sites, subscriptions, outbox/inbox flows, watch/wake
  machinery, broadcaster status handling, overlay/topic wiring, lanes/
  transport/counterparty delivery, finger records, bstack plane code.
  Suggests ranked fixes toward: as-realtime-as-possible (events drive,
  polls are ceilings), maximum interoperability + seamlessness (standard
  envelopes, content-addressed artifacts, conformance), no
  timeout-as-mechanism (bounded ceilings only), and resilience (fanout,
  degrade-to-poll, SPV-verify-never-trust, numbered+repaired streams).
  Grounded in Wright/BSV-stack principles, the 1BSV/Lightweb bstack
  papers + BRCs (the repo vendors @lightwebinc/bcommon + bfinger-host),
  and the repo's own seams. For general module/design rules use
  design-guardrails; for money-path invariants use code-review's
  nektar.md.
---

# event-arch — realtime UTXO event-layer audit + fix advisor

You are auditing (or designing) code on the event path: how the system
learns that the world changed — a UTXO spent, a tx sealed, a host back
up, a counterparty's offer landing — and how it reacts. The target
architecture, per the BSV stack model:

- **Overlays own context**: apps subscribe to _topics_, not raw chain
  noise — a topic manager decides which outputs belong (our `lanes`,
  `market`, watch-shim services).
- **Broadcast managers push status**: RECEIVED → ACCEPTED → SEEN is
  delivered as events (`Broadcaster.onStatus`, `ChainEvents`), never
  discovered by re-polling.
- **Block-header service verifies**: pushed statuses are _hints_;
  proofs/headers are _truth_ (`note()` records hints, `observe()` emits
  verified 'tx' — that split is the SPV trust model).
- **Counterparties exchange directly**: P2P for the present peer,
  store-and-forward MessageBox for the absent one — never a shared bus,
  never a timeout while a peer is merely offline.
- **Content-addressed = seamless**: the artifact's hash IS its address;
  on-chain carries the commitment (`hash + url + expiry`), never the
  payload. Anyone holding `{endpoint, bucket, digest}` self-verifies.

## Audit mode — process

### 1. Scope

Same detection as design-guardrails: `--files`, `--pkg`, `--last [n]`,
or default → in-flight diff + untracked `packages/*/src`.

### 2. Inventory the event surface

List every wait/sleep/poll/subscription/outbox-row/fetch-loop the scope
touches. For each, classify:

- **What is it waiting on?** (chain event, host recovery, counterparty
  artifact, local state, timer-only)
- **What wakes it?** (a producer signal on every path? a bounded poll?
  nothing = hang?)
- **What proves it?** (verified read/proof vs. trusted hint — statuses
  from hosts/broadcasters are hints until `observe`-verified)
- **What bounds it?** (ceiling is fine — ceiling AS the mechanism is
  `ev/timeout-as-mechanism`)

### 3. Walk the principle tables

Load [references/principles.md](references/principles.md) always; load
[references/fabric.md](references/fabric.md) when the scope touches
bstack/transport/delivery (lanes, finger, tree, journal, market-wire,
watch-shim, host fanout, artifact publish/fetch).

For each wait/subscription/delivery — the event-loop questions:

1. Does a producer exist that could signal this, but doesn't on some
   path? → `ev/lost-wakeup`
2. Is a fixed poll waiting on something that already emits? →
   `ev/poll-for-event`
3. Is a timer doing notification's job (vs. guarding a leak)? →
   `ev/timeout-as-mechanism`
4. Does a pushed status drive a money decision without `observe`/proof
   verification? → `ev/unverified-status` (BLOCKER on money paths)
5. Does a read treat host silence as nonexistence? → `ev/absence-as-proof`
6. Is a fanout write tracked per-destination, or "sent to the set"? →
   `ev/untracked-fanout`
7. Does a subscription die unrecoverably on disconnect — no cursor
   resume, no poll floor? → `ev/no-fallback`
8. Does a wire format carry location-references instead of digests? →
   `ev/location-coupled`
9. Is an applied effect non-idempotent under re-delivery? →
   `ev/no-dedup`
10. Is there a second scheduling substrate next to an existing one? →
    `ev/scheduler-sprawl`

… and the fabric questions (delivery/transport scope):

11. Does the stream number its items or carry a cursor — or is "missed
    #47" invisible? → `fab/unnumbered-stream`
12. Can a detected gap actually be repaired by name (cursor resume,
    sweep, NACK), or only logged? → `fab/gap-unrepaired`
13. Could a repaired/replayed item pose as fresh delivery — repair bound
    to {source, position}? → `fab/resend-forgeable`
14. Is the subscription narrower than the interest set — firehose join,
    or watch outliving its need? → `fab/firehose-subscription`
15. Does a counterparty flow announce-and-wait-for-pull when it could
    deliver the artifact in the same step? → `fab/announce-pull`
16. Is a host deciding truth instead of serving-and-proving — can't
    rebuild its state from chain/store/digest? → `fab/custody-host`
17. Can a payload exceed the transport's frame/size ceiling and vanish
    silently? → `fab/fat-frame`
18. Does the join name a source where the principal is known, or accept
    any-source? → `fab/source-ambiguity`
19. Does bulk data ride a mined lane instead of the carrier plane? →
    `fab/mineable-carrier`
20. Does a subscriber re-pull per-read what its own watch delivered? →
    `fab/pull-what-you-subscribed`

### 4. Suggest fixes — concrete, repo-seamed, ranked

Every finding names: the principle violated, the in-repo precedent to
copy, and the smallest structural move. See
[references/patterns.md](references/patterns.md) for the fix catalog —
prefer the existing seam (`ChainEvents.watch`, `unblockOutbox`,
watch-shim `?cursor` resume, `hotUntil` hot-window, `note()`-hint vs
`observe()`-truth, `notifyPeer` fire-and-forget) over a new mechanism.

### 5. Report

```
## Event-arch audit — <scope>

**Verdict: ALIGNED | MINOR DRIFT | STRUCTURAL DRIFT | BROKEN**

### Findings → suggested fixes
| # | Sev | Rule | Where | Principle violated | Suggested fix (seam to reuse) |

### Event-surface map
<waits/subscriptions/timers found, each classified: signaler / bound / verifier>
<fabric scope only: streams and their sequence/cursor, lanes joined,
repair paths>

### Realtime score
- Wake coverage: N/M waits have a producer signal on all paths
- Trust discipline: hints vs verified reads (any hint driving money?)
- Fallbacks: every push path degrade to bounded poll? every poll bounded?
- Fabric (if in scope): every stream numbered/cursored? every gap
  repairable by name? subscriptions derived from live interest?
```

Suggest only — do not apply fixes unless asked. For each suggestion,
state the smallest move that preserves the load path (wakes are hints;
the poll floor stays until the event path is proven).

## Design mode — writing new event-path code

Before writing a wait/subscription/delivery:

1. Name the EVENT (UTXO transition, host recovery, counterparty message)
   — if you can't name the event, you're polling for state.
2. Name the SIGNALER — which producer announces it, on EVERY path
   (success and failure)?
3. Name the FLOOR — the bounded poll that covers a missed wake.
4. Name the VERIFIER — what proves the event (`observe`, BUMP verify,
   digest check)? Hints route through `note()`, never emit 'tx'.
5. Name the POSITION — the stream's sequence/cursor the consumer
   persists (missed items must be nameable, then repairable by name).
6. Name the DEGRADATION — shim down / host down / peer offline → what
   still works? (MessageBox semantics: hold encrypted, deliver on wake.)
7. Name the IDEMPOTENCY — applied effects dedup by op-ID at the edge.
8. Name the LANE — which interest-set/topic this delivery belongs to,
   and who computes the join (narrow subscriptions, dead subs aborted).
9. Name the STANDARD — the BRC/upstream spec the wire shape implements,
   or the one it deliberately diverges from.

## nektar-specific calibration

- The trust split is load-bearing: `ChainEvents.note()` records pushed
  statuses for waiters/dedup but never emits 'tx' — only `observe()`
  (our own chain reads) does. Any code that promotes a hint to truth
  without verification is a BLOCKER-class finding.
- "A wake is only ever a hint, never the load path" — the watch-shim's
  own rule. Wakes shorten latency; polls carry correctness. Removing a
  poll floor because "the watch exists" is the classic breakage.
- Long-poll protocol contract: `/watch?service=…&query=…&cursor=…&waitMs`
  → `{changed, cursor}` — new watch services must honor cursor-resume
  and stay degenerate (return quickly when the query is cheap-empty).
- Interop means another implementation plays: envelopes stay boring and
  versioned (`market-wire.ts` `v:`/`kind:` fields), artifacts resolve by
  digest (`chirp/<commit>`, `proofStore`), and shared conformance/test
  vectors are the proof — new wire shapes get vector coverage.
- Host-money paths keep their invariants: SpendGuard on every funded
  spend; the chain arbitrates; local stores cache intent.

## Sources

BSV stack model (overlays/topic managers, broadcast-manager status push
RECEIVED→ACCEPTED→SEEN, block-header verification, MessageBox
store-and-forward, SHA-256-addressed storage, 402/header payments,
open-by-design shared test vectors) — the "BSV Distributed Applications
Stack" deck + Wright's SPV/P2P/first-seen positions.

1BSV/Lightweb bstack family (1bsv.net + github.com/lightwebinc — this
repo _implements_ it: `@lightwebinc/bcommon` + `bfinger-host` are
vendored): Papers 001–016 (pushed Teranode delivery, announce-then-pull
economics, shard-bit extraction, overlay object plane BRC-148/149,
bstack pattern language, topic-lane sharding, the data carrier, frame
sizing, single-source joins, NACK loss repair, provenance at scale);
pattern briefs bfinger/bbox/borg/bsecret/blogs/bstore. See
[references/fabric.md](references/fabric.md) for the paper→rule map.
Wright's Substack essays (singulargrit.substack.com) are cited as
posture only — see the posture section in
[references/principles.md](references/principles.md).

Repo mechanisms: `packages/chain/src/events.ts`, `interfaces.ts`
(`onStatus`, `txStatus`, `onReorg`), `automation/src/runtime.ts`
(watch-shim, hotUntil, unblockOutbox, notifyPeer),
`bstack/src/{lanes,finger,tree,journal,market-wire,testing}.ts`. Rule
IDs `ev/*` + `fab/*` are local to this skill; cross-map to `data/*`,
`stab/*`, `struct/*` in `../code-review/references/*` where they
overlap.
