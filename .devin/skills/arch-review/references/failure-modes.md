# failure-modes — the runtime-structure lens

Load for Phase 2's runtime hunt. The static graph (measurement #1–#8) tells
you where a failure can travel; this catalog tells you what to hunt. The
question has changed: not "is the structure clean" but "where does the
structure break while it's running" — crash, restart, race, lost wakeup,
partial failure, saturation, drift — and how far the blast travels.

**Mine AGENTS.md first.** The "learned the hard way" section is the
empirical catalog — incidents the system already had:

- `rpc_max_clients` defaults to 3 → keep-alive callers saturate it, healthcheck
  stays green while new RPC calls are refused (saturation + silence)
- Kafka `txmeta` ~60s retention → ghost parents: UTXO says unspent, parent
  fetch returns `TX_MISSING_PARENT` forever (external-dependency failure +
  silent unspendability)
- Docker Desktop port proxies go stale → port accepts then drops while the
  container is healthy internally (boundary failure invisble to healthchecks)
- bstack hosts race their db on daemon restart → crash-loop with no restart
  policy (ordering/lifecycle)
- Node 24/libuv `0xC0000409` teardown assert kills processes holding sockets
  at exit (resource lifecycle)
- Subsidy collapses ~every 150 blocks → funding-by-mining silently becomes
  hopeless (time/economics assumption)

A runtime finding that rhymes with a past incident is High confidence by
default — the class is proven live.

## Runtime topology map (build before hunting)

The package graph is compile-time; this is the execution shape. Map:

- **Processes & entry points**: mcp bin, watcher `.mjs`, e2e scenarios,
  bstack hosts (docker), regtest node. Long-lived vs one-shot, restart
  policy per process.
- **Loops & substrates**: every tick/poll/interval owns a duty (ChainEvents
  tick, automation idle/hot tick, watcher duty cycle, outbox drain). Two
  substrates for one duty = scheduling that races itself.
- **Event subscriptions**: every `.on(`/SSE/webhook/callback and the
  producer paths feeding it. An emitter nobody consumes is dead machinery;
  a consumer whose producer can't reach it is a stall.
- **Durable vs in-memory**: every store (SQLite, wallet basket, `.state/`
  files, on-chain) vs every long-lived `Map`/`Set`/cache. For each in-memory
  structure: rebuilt on restart how — or is its loss silent (S4)?
- **External dependencies**: broadcaster, WoC/indexers, RPC node, wallet
  services, Kafka, docker proxies, bstack hosts. For each: degraded
  behavior, and who notices.
- **The satoshi path**: custody chain end-to-end — funds `take` → tx build →
  sign → broadcast → status → `spent`. The hunt runs hardest here.

## Evidence standard (every runtime finding)

| Field          | Question                                                           |
| -------------- | ------------------------------------------------------------------ |
| Code path      | `file:line` of the mechanism — you read it this session            |
| Trigger        | what has to happen (restart, 429, two takes at once, host flap)    |
| Failure path   | what the code actually does next — not what it should do           |
| Blast radius   | what else corrupts or stalls when it fires (use the package graph) |
| Silence reason | why nobody sees it — no log, no metric, swallowed, no waiter       |

Then the **challenge step**: try to kill the finding. Is there a lock,
queue, lease, tombstone, or bounded retry that already prevents it? Does a
test exercise the path? What survives gets a confidence level:

- **High** — whole path traced; nothing prevents it (or it matches a past
  incident).
- **Medium** — mechanism confirmed, trigger plausible, guard unclear.
- **Low** — hypothesis worth a test; report as hypothesis, not finding.

For each hazard, name the guard that prevents it. A guard that is a comment
or convention is an **unenforced guard** — that's the finding (S2-adjacent).

## The catalog

### F1. Concurrency & check-then-act

- **Rule**: read-decide-write across an `await` with nothing serializing it;
  shared mutable state touched from >1 async flow.
- **Detect**: `rg -n "reserved|mutex|queue|busy|locked" packages/*/src` —
  then find the stores WITHOUT a guard. `FundsBase.queue` + `reserved` is
  the exemplar (take() serialized so two spends can't share an outpoint);
  the hunt is for the same shape elsewhere: ledger writes, store updates,
  basket cache refresh.
- **Nektar**: `WalletFunds.cache` is refreshed on every `take` — a
  concurrent `list` mid-refresh sees a partial set unless the queue covers
  it. Ask per store: who serializes?

### F2. Ordering & lifecycle

- **Rule**: consumer starts before producer (lost events in the gap),
  registration after use, teardown that hangs or races in-flight work.
- **Detect**: `rg -n "openRuntime|\.start\(|\.stop\(|\.close\(|ready" packages/*/src`;
  trace one subsystem's boot and one teardown. Unbounded drain-spin =
  teardown hang (Bounded drain pattern, consolidation.md).
- **Nektar**: the bstack restart-race incident is this class. Any subsystem
  whose `open` assumes another is open, with no check.

### F3. Crash & restart (recovery)

- **Rule**: in-memory state the flow treats as durable; half-committed
  writes across the three stores (SQLite ledger, wallet basket, on-chain);
  no replay/reconcile path.
- **Detect**: durable-vs-memory map from the topology; `rg -n "replay|resume|recover|reconcile|restart" packages/*/src`
  — where's the resume story, and does it rebuild or assume?
- **Nektar**: `FundsBase.reserved` is in-memory — a restarted process can
  re-`take` what its dead self reserved. Is the cross-process ledger lease
  the durable cover? Then the finding isn't the memory loss, it's any path
  where the lease ISN'T consulted. The three-store split-brain question:
  marked spent in ledger, basket still holds it, tx never broadcast — which
  store wins on boot?

### F4. Lost wakeup & event gaps

- **Rule**: a waiter whose producer doesn't signal on every path (incl.
  error), or a subscription with a gap between poll and subscribe where
  the event falls through.
- **Detect**: waiter→signaler pair map (measurement #13) — extend to
  reconnection windows: SSE/long-poll gaps, Kafka retention windows, the
  interval between last status poll and subscription arm.
- **Nektar**: the 60s txmeta window IS this class — data written to a
  retained topic the consumer never saw. Status-hint streams have the same
  shape: what arrives between `broadcast` returning and the watch being
  registered?

### F5. Partial failure & silence

- **Rule**: work nobody awaits and nobody observes; catch→default on money
  paths; error paths that leave waiters asleep forever.
- **Detect**: `rg -n "void |\.catch\(\(\) ?=> ?\{\}\)|\.catch\(\(\) ?=> ?(null|undefined)" packages/*/src`;
  then catch→`return null/[]/false` on the custody path — read every site
  on money paths.
- **Nektar**: `WalletFunds.store.drop` does
  `void relinquishOutput().catch(() => {})` — deliberate fire-and-forget,
  covered by the ledger lease. The question is never "is there a void" but
  "what covers the async gap" — a `void` with no covering mechanism is the
  finding.

### F6. Saturation & unbounded growth

- **Rule**: pools/limits sized below the real concurrency; collections that
  only grow; retry layers multiplying; handle/socket leaks at lifecycle
  edges.
- **Detect**: `rg -n "= new (Map|Set)|readonly.*(Map|Set)" packages/*/src` —
  each long-lived collection gets an eviction/bound story or a finding;
  `rg -n "connection:|keep-?alive|agent" packages/*/src`; stacked-retry
  multiplication from measurement #11's axis 4.
- **Nektar**: the `rpc_max_clients=3` incident — healthcheck green while
  callers starved. Any resource limit set by a default nobody chose is a
  finding. The `0xC0000409` teardown crash = sockets held at exit.

### F7. External dependency failure

- **Rule**: an upstream that can lie, lag, rate-limit, or vanish — and the
  boundary treats its answer as fact.
- **Detect**: every fetch/RPC client: timeout bound? failover? degraded
  mode? is a 404/missing answer distinguishable from a negative answer
  (S3 trust-boundary smearing is the type-level cousin)?
- **Nektar**: broadcaster `SEEN_ON_NETWORK` semantics, WoC rate limits, the
  stale docker proxy that accepts-then-drops. Indexer `TX_MISSING_PARENT`
  is an answer that's true-from-the-indexer but false-in-fact — does the
  caller's type know the difference?

### F8. State machine drift

- **Rule**: states and transitions scattered across `if status ==` sites
  instead of one table; two stores holding the same truth and diverging;
  illegal transitions reachable.
- **Detect**: `rg -n "state:|status:" packages/*/src` at the deal/ledger/
  chain-tracker seams; enumerate transitions per entity — is there one
  validator or N scattered checks?
- **Nektar**: basket-vs-cache desync (`WalletFunds.cache` vs `listOutputs`
  truth), tombstone-vs-revive in `died`, tracker `lastStatus` vs actual
  chain state. Any path where the two can diverge silently = the finding.

### F9. Time & economics assumptions

- **Rule**: behavior that degrades as the world ages — retention windows,
  halvings, fee floors, hardcoded "reasonable" durations, timeout-as-
  mechanism (bounded ceilings are fine; a timeout BEING the mechanism is
  not).
- **Detect**: the #11 census's configured/hardcoded axis; constants whose
  validity depends on chain age or chain params; anything that assumes
  "this will have happened by now."
- **Nektar**: subsidy collapse made funding-by-mining silently hopeless;
  `rpc_max_clients` was an unexamined default. Every duration constant on a
  money path: chosen, or inherited?

### F10. Idempotency & replay

- **Rule**: the same operation arriving twice — redelivery, re-broadcast,
  retried internalize, re-take after crash — and the handler isn't
  idempotent.
- **Detect**: for every ingest/apply/mark path: what makes the second
  application a no-op? tombstones, `has()` checks, natural keys — or
  nothing?
- **Nektar**: `internalizePush` caches only what the wallet accepted and
  `has()` guards re-internalize — the exemplar. The hunt: every other
  "apply" (outbox drain, deal transition, spent-mark) — second-delivery
  safe?

### F11. State lists that drift apart

- **Rule**: the same entity's state space enumerated in two places — an
  enum/union type here, a hardcoded "active states" list there — and one
  gains a member the other never learns about. The narrow list quietly
  stops covering reality; whatever only exists in the missing state is
  never seen again.
- **Detect**: take every status/enum union (`TxState`, deal states, lease
  states) and grep each literal — every consumer site gets "does this
  list still cover the whole union?" A `WHERE state IN (...)` or a
  `walletTxs([...])` that names a subset is a drift candidate by
  construction. The fix pattern is deriving the active set from the
  union, or a terminal-set the watcher excludes rather than an
  active-set it includes.
- **Nektar**: the `TransactionWatcher` sweep listed
  `['submitted','submission_unknown','observed']` while `TxState` had
  grown `'exported'` — signed payments handed to a counterparty could
  never be observed settling, stranding their inputs as leased-
  forever. Found 2026-11 review; the sharper rule than generic F8.

## Reporting

Runtime findings go in the report's **Danger zones** table, ranked by
severity × confidence. A High-confidence CRITICAL with a named trigger is
the review's top deliverable — it is the "will hurt first" item the honest
paragraph names. Low-confidence items land in a hypothesis list with the
test that would settle them.
