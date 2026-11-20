---
name: event-arch
description: >
  Use to audit or design the realtime/event layer of the UTXO on-chain
  architecture — wait sites, subscriptions, outbox/inbox flows, watch/wake
  machinery, broadcaster status handling, overlay/topic wiring, lanes/
  transport/counterparty delivery, finger records, bstack plane code.
  Also the wait-site hygiene audit: bounded-but-still-held resources,
  wedged mutexes behind abandoned promises, unjittered cadences, stacked
  deadlines, unarmed deadlines, unmeasured wake paths — "why is this
  slow/wedged/starved" questions route here too.
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

## The wait lifecycle — the audit's organizing model

Every wait is a six-step lifecycle, not a front-half question. The four
classification fields (waits-on / wakes-it / proves-it / bounds-it) cover
steps 1–3; the failure modes that survive a clean-looking audit live in
steps 4–6. Audit the whole arc:

1. **arm** — when interest begins: is the deadline derived at commit
   (`stamp + bound` stored, `dueAt`-style) or scanned later? Is the
   cadence jittered, or do N restarted processes fire on the same tick?
2. **wait** — what resource is held while waiting? (mutex link, queue
   slot, connection, lease — the thing an orphan will still pin.)
3. **fire** — what resolves it (signal / poll / ceiling)? If bounds
   nest, do inner awaits share one operation deadline or each take a
   fresh budget?
4. **abandon** — the bound fires first: what does the still-running
   promise still hold? `Promise.race` resolves the _waiter_; it never
   cancels the work — whatever it held, it still holds.
5. **detect** — who notices a wedge/orphan? A mutex poisoned by an
   abandoned promise starves every later acquirer _silently_ — a log
   line or acquire-bound is the difference between a wedge and an alert.
6. **measure** — what records which source fired? A dead push path
   degrades to ceiling speed — correct and invisible. "Events drive,
   polls are ceilings" is an assertion until runtime can report it.

The failure taxonomy this catches: the `bound()` in `runtime.ts` that
abandons `engine.tick()` while the hung leg still owns `this.chain`
(every later `run()` starves — abandon+detect), a wake registry that is
a static contract with no runtime accounting (measure), fixed retry
cadences with no jitter (arm), nested fresh bounds that sum instead of
sharing a budget (fire), a computable stuck deadline scanned per pass
(arm), and a coverage test that greps `setInterval` while `setTimeout`/
`Promise.race` sites land unmapped (the census is blind to a wait class).

## Operating modes

Audit runs **inline** by default — one pass over the scope's event
surface. It runs as a **swarm** on whole-repo/subsystem runs and
explicit full audits per
[references/orchestration.md](references/orchestration.md): inventory
agents enumerate the surface per slice, one auditor per question family
(`ev`/`fab`/`spv`) walks the merged inventory, and fresh verifiers
challenge every BLOCKER and every absence claim — including the
lifecycle absence claims ("nothing holds a resource across this bound",
"no jitter anywhere", "no runtime wake accounting"). Design mode never
delegates — it is pre-writing reflection, not a review.

## Audit mode — process

### 1. Scope

Same detection as design-guardrails: `--files`, `--pkg`, `--last [n]`,
or default → in-flight diff + untracked `packages/*/src`.

### 2. Inventory the event surface

In swarm mode this inventory is produced by delegated agents per slice
(orchestration.md stage 1) and merged by the parent. Inline mode: list
every wait/sleep/poll/subscription/outbox-row/fetch-loop the scope
touches. For each, classify:

- **What is it waiting on?** (chain event, host recovery, counterparty
  artifact, local state, timer-only)
- **What wakes it?** (a producer signal on every path? a bounded poll?
  nothing = hang?)
- **What proves it?** (verified read/proof vs. trusted hint — statuses
  from hosts/broadcasters are hints until `observe`-verified)
- **What bounds it?** (ceiling is fine — ceiling AS the mechanism is
  `ev/timeout-as-mechanism`. On bound-fire: _cancels_, _abandons_, or
  _just resolves_? `Promise.race` resolves the waiter — the work keeps
  running and keeps its holds)
- **What does it hold while waiting?** (mutex link, queue slot,
  connection, lease — the resource an abandoned promise still pins. A
  promise-chain `this.chain = p.then(fn)` is the serializing shape)
- **What measures it?** (runtime accounting of which source fired — a
  wake tag, a counter, nothing? A static registry is the contract; the
  measure is the truth)

### 3. Walk the principle tables

In swarm mode each question family (`ev`, `fab`, `spv`) is a delegated
auditor walking the merged inventory (orchestration.md stage 2). Inline
mode: load [references/principles.md](references/principles.md) always; load
[references/fabric.md](references/fabric.md) when the scope touches
bstack/transport/delivery (lanes, finger, tree, journal, market-wire,
watch-shim, host fanout, artifact publish/fetch); load
[references/spv.md](references/spv.md) when the scope touches
proofs/status/inclusion (events observe paths, BUMP/BEEF handling,
ChainTracker/header use, tx fetch/status, anything calling "is it
mined/proven").

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

… and the wait-hygiene questions (the lifecycle's back half — the
failures that survive a front-half-clean audit):

- A bound fires — does the abandoned promise still hold a serialized
  resource (mutex chain, queue slot, lease, connection) with nothing
  detecting the orphan? → `ev/abandon-holds-resource` (**HIGH** when the
  resource serializes work — `this.chain = p.then(fn)` in `base.ts` is
  the live exemplar: a hung `engine.tick()` inside `bound()` starves
  every later `run()` silently)
- Do push paths exist but nothing records at runtime which source drove
  work — a dead push degrades to ceiling speed invisibly? →
  `ev/unmeasured-wake`
- Is a retry/interval cadence a fixed literal with no jitter — N
  processes restarted together fire on the same ticks? →
  `ev/phase-aligned-cadence` (`watches.ts` `* (0.5 + Math.random())` is
  the in-repo jitter precedent — today the only one)
- Do nested bounds each take a fresh budget instead of sharing one
  operation deadline — a leg can burn N× its intended cost? →
  `ev/stacked-deadline`
- Is a deadline derivable at commit (`stamp + bound` stored) scanned
  per-pass instead of armed as a scheduled wake (`dueAt`/`dueAtArmed`
  seam)? → `ev/unarmed-deadline` (latency finding, not correctness)
- Does a coverage gate/registry enumerate only one wait primitive
  (`setInterval`) while `setTimeout`/`Promise.race`/
  `AbortSignal.timeout`/`sleep` sites exist unmapped? →
  `ev/wait-class-blind`

### The second question — the root-cause discipline

After the surface classification, walk six follow-up lenses — this is
what turns "is it bounded?" into "what is it really?":

1. **Follow the resource** — the bound fires → what's still held?
   Trace the abandoned promise's holds, not the waiter's resolution.
2. **Follow the measurement** — the contract exists → how does runtime
   prove it fired? A registry nobody reads at runtime is decoration.
3. **Follow the deadline** — computable at commit → why is it scanned?
   And nested bounds: one budget shared inward, or summed?
4. **Follow the cadence** — a fixed interval → do N restarts fire it on
   the same tick? (thundering herd: jitter is the fix, not a longer
   interval)
5. **Follow the registry** — a coverage gate → what wait class can't it
   see? Enumerate every primitive: `setInterval`, `setTimeout`,
   `Promise.race`, `AbortSignal.timeout`, `sleep`, deferred `dueAt`s.
6. **Follow the silence** — the ceiling fires → who hears it? A silent
   ceiling-hit is a degraded push path nobody sees.

### Detection recipes

- `Promise.race` / `setTimeout` bound / `AbortSignal.timeout` /
  `sleep(` → for each site, trace what the abandoned arm still holds
  (mutex links, leases, queue slots); a `.then` chain assigned back
  (`this.chain = p.catch(...)`) is the serializing shape — check for an
  acquire-bound or a held-too-long detector
- retry/backoff literals (`* 2`, `\d+_000` near `next_at`/`backoff`/
  `interval`) with no `Math.random` nearby → `ev/phase-aligned-cadence`
- `now() - <stamp> > <bound>` inside a scan/advance loop →
  `ev/unarmed-deadline` candidate — could the bound have been armed at
  the write that set `stamp`?
- a `bound(`/`deliverBounded`/timeout wrapper whose inner work itself
  calls a fresh bound → `ev/stacked-deadline` — budgets sum; pass one
  deadline in
- coverage tests/registries that enumerate `setInterval` only →
  `ev/wait-class-blind` — extend the primitive list or the gap is
  declared

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

… and the SPV questions (proof/status/inclusion scope):

21. Does app code fetch or scan a full block to answer "is it mined"? →
    `spv/block-scan` — the answer arrives as BUMP + header check;
    block-walking is proof-service work
22. Does a 'mined' claim emit without a merkle path in hand (indexer
    `blockheight>0`, "known", presence)? → `spv/pathless-mined`
23. Does a status claim ride the verified path ('tx' emit, watch-drop,
    money decision) without a proof object — including rejection
    verdicts, which nothing can prove? → `spv/hint-as-verified`
24. Is a txid trusted without the bytes (`hash(bytes)=txid` can't be
    checked)? → `spv/txid-only`
25. Is a path anchored to a header from the same party that served the
    path? → `spv/circular-anchor` — header anchor must be independent
26. Is a counterparty handed raw tx/txid where BEEF/EF would travel
    self-verifying? → `spv/beefless-handoff`
27. Is an indexer's presence/blockheight treated as chain truth, or is
    global state reconstructed to serve one relationship? →
    `spv/indexer-dependency`, `spv/world-state`
28. Is 'mined' binary where the stake demands a depth policy? →
    `spv/depth-blind` (advisory)
29. Does an unconfirmed-chain handoff carry its full ancestry BEEF with
    every input chain reaching a BUMP (or a named issuer-class bound) —
    and does a step stall on the subject's own block when verified
    first-party ancestry would advance the flow? → `spv/thin-ancestry`,
    `spv/zeroconf-overreach`, `spv/zeroconf-unused`

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
- Wait hygiene: every bound names what it abandons + who detects the
  orphan? every cadence jittered? nested bounds share one budget?
  deadlines armed at commit or declared-as-scanned? every wait primitive
  (`setInterval`/`setTimeout`/`Promise.race`/`AbortSignal`/`sleep`) inside
  the census? does runtime record which source fired each pass?
- Fabric (if in scope): every stream numbered/cursored? every gap
  repairable by name? subscriptions derived from live interest?
- SPV (if in scope): every 'mined' claim path-anchored to OUR headers?
  every rejection provable or hint-only? zero block-fetches in client
  code (proof-service work)? handoffs carry BEEF/EF, not bare txids?
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
10. Name the PROOF OBJECT — what physical evidence carries the claim
    (BUMP, BEEF, the conflicting tx's own proof)? If the answer is "a
    status string", it is a hint and must route through `note()` — only
    'mined' (BUMP) and 'double-spent' (the conflict's BUMP) are
    provable claims.
11. Name the HEADER ANCHOR — which `ChainTracker` checks the root, and
    is it independent of whoever served the proof? Verifying against
    the proof's own provider is circular trust.
12. Name the EVIDENCE SHAPE — what the counterparty is handed:
    AtomicBEEF > BEEF > EF > raw+lookup > txid-alone. Every rung down
    moves work from sender-held proofs to receiver network queries.
13. Name the ABANDONMENT — when the bound fires before the work settles,
    what does the orphan still hold (mutex, lease, connection) and who
    detects it? A race-swallowed promise that pins a serialized resource
    is a silent wedge — bound the acquire, or detect the held-too-long.
14. Name the BUDGET — one deadline per operation, passed inward. Nested
    bounds that each take a fresh budget sum; a `deliver` containing
    submit+lookup+fetch burns 3× what you wrote.
15. Name the JITTER — a fixed retry cadence is a phase-aligned stampede
    the day N processes restart together. `* (0.5 + Math.random())`
    (watches.ts) is the in-repo shape.
16. Name the WITNESS — what records at runtime which source fired the
    wake? The registry is the contract; the counter is the truth. A push
    path that dies degrades to ceiling speed — correct, and invisible
    without the witness.

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
Wright's Substack essays (singulargrit.substack.com): the philosophical
essays are cited as posture in
[references/principles.md](references/principles.md); the SPV papers —
"Safe Low Bandwidth SPV: A Formal Treatment", "The Protocol No
Gatekeeper Wants" (evidence travels, MF-SPV), "A Mechanism of Honour"
(txid-only proves nothing) — are the doctrine in
[references/spv.md](references/spv.md) alongside the 1BSV SPV model
(3 things: bytes + path + headers; inclusion ≠ currency; apps take
BEEF+headers, never blocks) and BRC-62/74 (BEEF/BUMP).

Repo mechanisms: `packages/chain/src/events.ts`, `interfaces.ts`
(`onStatus`, `txStatus`, `onReorg`, `ChainTracker` = header service),
`mainnet.ts` (`merklePathMainnet`, `ChaintracksTracker`,
`statusStream`), `regtest.ts` (`merklePathFor`/`merklePathAtHeight` —
contained proof-service stand-in), `txstore.ts` (evidence store,
BEEF/EF wire), `automation/src/runtime.ts` (watch-shim, hotUntil,
unblockOutbox, notifyPeer), `automation/src/toolbox.ts` (wallet
services = SPV services shape), `mcp/src/{runtime,http,txops}.ts`
(observe/proofLookup/handleBroadcasterStatus/deliverTxProof),
`anchor/src/runarchain.ts` (BEEF ancestry assembly),
`bstack/src/{lanes,finger,tree,journal,market-wire,testing}.ts`. Rule
IDs `ev/*` + `fab/*` + `spv/*` are local to this skill; cross-map to
`data/*`, `stab/*`, `struct/*` in `../code-review/references/*` where
they overlap.
