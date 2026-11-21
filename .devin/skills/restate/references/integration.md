# integration.md — setup, architecture fit, and the no-polling rules

Load when wiring Restate into a service, designing where it sits in the
stack, or auditing a path that waits. nektar's shape is the worked
example throughout.

## 1. Setup — the moving parts and who owns them

| part             | what                                                                                                                                                                                | nektar's instance                                                                                                                          |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------ |
| **Server**       | the durable executor + journal store + scheduler + ingress/admin APIs (`:8080`/`:9070`)                                                                                             | `restatedev/restate:latest` in `compose.yml` — `depends_on: [restate]` marks it required infra; `restate-data` volume persists the journal |
| **Endpoint**     | the in-process HTTP/2 handler the server invokes                                                                                                                                    | `openEndpoint` → `restate.createEndpointHandler({services})` on `NEKTAR_RESTATE_ENDPOINT_PORT` (9080)                                      |
| **Registration** | telling the server which URI serves which service defs                                                                                                                              | auto at boot: `openEndpoint` POSTs to `NEKTAR_RESTATE_ADMIN` — a failed registration means invocations never arrive (log line, not silent) |
| **Env seam**     | `NEKTAR_RESTATE_URL` (ingress :8080), `NEKTAR_RESTATE_ADMIN` (:9070), `NEKTAR_RESTATE_ENDPOINT_URI` (what the server dials back — container DNS name in compose, `127.0.0.1` local) | compose sets all three; tests skip without `NEKTAR_RESTATE_URL`                                                                            |

Setup rules from the docs:

- **Registration is a deployment.** Immutable once made — retries of an
  invocation pin to it. `--force` re-register is dev-only; production
  upgrades register a NEW endpoint URI and let the old drain.
- **`--use-http1.1`** when registering endpoints that can't speak h2;
  `createEndpointHandler` auto-detects per request.
- **Lock the endpoint down** — `identityKeys: ["publickeyv1_…"]` on
  `serve`/`createEndpointHandler` validates requests are signed by your
  Restate instance. An open :9080 accepts invocations from anyone who
  can reach it. `rt/open-endpoint` if a production endpoint lacks them.
- **Mark internal objects private** — `options.ingressPrivate: true`
  keeps a service callable service-to-service but unreachable over the
  public ingress. Custody/tx/deal objects are internal mechanics — only
  the door services belong on ingress. `rt/public-internal` when an
  internal object is HTTP-callable.
- **One server per trust domain, one endpoint per process.** Two
  in-process runtimes binding :9080 collide (EADDRINUSE) — production is
  one dock per process; tests inject `cfg.exec` Dispatchers instead.
  The e2e real-stack scenario gives each agent its own server sidecar —
  that's the honest multi-process shape.

## 2. Where it sits — the architecture map

```
external producers          the plane                 domain truth
(lane polls, host watches,   (Restate)                 (SQLite)
 tx hints, tool calls)
      │                       │                           ▲
      ▼                       ▼                           │
 wake sends ────► journal ──► handlers ──ctx.run──► store/engine legs
 (detached,        (delivery    (journaled          (verdict writes,
  at-least-once)    truth)       replay)             CAS-guarded)
```

- **Producers stay outside.** The lane long-poll, host watches, tx
  status hints, MCP tool calls — real I/O that _generates_ events. They
  don't run inside handlers; they emit sends into the plane. Restate
  can't make a 25s HTTP long-poll durable — the poll is the producer,
  the send is the durable artifact.
- **The journal is delivery truth; the row is verdict truth.** Never
  read `sys_invocation` status as domain status (worksheet 3 in
  audit.md maps this).
- **Domain writes stay in SQLite.** Handlers are thin `ctx.run` wrappers
  around store/engine legs — the journal records that a leg ran, the
  CAS (`revision`) records that the row accepted it. A handler that
  writes to both Restate K/V and the row creates the hidden-second-
  authority class (state-audit `data/hidden-source-of-truth`).
- **The cycle glue is allowed to stay outside** — `rt.cycle()` is host
  orchestration: it polls producers then _kicks_ the plane (dealfan
  call, sweep send). Its legs are journaled sends; the loop itself is
  not a durable artifact and doesn't need to be.

## 3. The no-polling rules — event-driven on this plane

Every wait in the system names its producer signal. On Restate the
signals are: **sends** (incl. delayed), **signals/awakeables/promises**,
and **calls**. Polling survives only as a bounded ceiling behind them —
the event-arch rules, translated:

| instead of…                                      | use                                                                                                                                                            | why                                                                                                                                         |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `setInterval` tick that scans for work           | **self-arm at the next real deadline**: drain re-sends itself at `nextOutboxAt`, dealfan at `ceilingMs`                                                        | the arm IS the wait — durable, exact-instant, zero idle burn. A fixed cadence is a poll: it wakes to find nothing, or sleeps past the event |
| `ctx.sleep` inside a handler waiting on an event | **delayed send to self** or an **awakeable**                                                                                                                   | sleep inside an exclusive handler blocks the whole key's queue; a delayed send frees the key AND survives deploys better                    |
| Poll an approval/status until it flips           | **`ctx.awakeable` / `ctx.promise` / `ctx.signal`** — the approver resolves via HTTP (`/restate/awakeables/<id>/resolve`) or another handler resolves           | our `funding_deferred`/approval gates are this shape — the wait is a suspended durable promise, not a re-scan                               |
| Poll a downstream job for completion             | **call** (request-response — the response IS the completion) or send + `ctx.attach(invocationId)`                                                              | attach suspends until the callee completes; polling re-enters at a cadence                                                                  |
| Re-poke "is there outbox work"                   | **producer pokes**: `outboxReady` fires on every enqueue's commit → drain send                                                                                 | the enqueue IS the event; the drain's self-arm at `next_at` is the ceiling for pre-committed rows                                           |
| Wait for a chain height / block                  | **delayed send at due-height via the wake service**, armed when the op writes `dueHeight` — plus the height-relay producer sending `wake.height` on new blocks | the block event pokes; the arm is the leak-guard ceiling                                                                                    |
| A second poll loop "in case a send was missed"   | **the self-arm ceiling**: each cadence service re-arms from the store on every fire — a missed poke degrades to ceiling speed, never silence                   | redundancy lives in the floor, not a parallel mechanism                                                                                     |

Rules of thumb:

- **Arms are computed from the row's deadline, never a fixed interval** —
  `delayMs = next_at - now(domain clock)`. A cadence that fires "every N"
  is a poll; a cadence that fires "at the earliest due" is the ceiling.
- **A send is cheap; a poll is a mechanism.** Emitting a wake costs a
  journaled line; polling costs a whole leg on a cadence and still
  arrives late.
- **Suspension is free.** A handler parked on an awakeable/promise holds
  no thread, no timer, no budget — it's a journal entry. Never re-check
  "did it resolve yet" from outside; resolve it and the plane resumes.
- **Shared handlers for reads** — a status query that queues behind the
  exclusive key invents latency the plane doesn't have.
  `restate.handlers.object.shared` answers against the live object
  without waiting for the queue.
- **The floor exists but is not the mechanism** — `ceilingMs`/`idleMs`
  arms bound the damage of a lost poke; they are leak guards. event-arch
  owns the wait-site classification; this skill owns that the arm is a
  delayed send on the domain clock.

## 4. Latency traps specific to this plane

- **`inactivityTimeout` (default 1 min)** — a leg inside `ctx.run` that
  emits no journal entries for >1min suspends+retries the whole
  invocation (the old attempt keeps running — see the zombie rule).
  Long legs (lane HTTP fanout, LLM calls, big store scans) either fit
  under it, split into smaller `ctx.run` steps, or raise
  `options.inactivityTimeout` on the service. `rt/inactivity-suspend`.
- **A `call` waits — budget it.** Request-response holds the caller's
  journal position until the callee completes on ITS key's queue. A
  call into a busy key is queue-latency stacked on work-latency; a send
  - attach is the same wait without the deadlock surface.
- **Per-key FIFO is the latency budget.** Everything queued behind a
  slow handler on the same key waits for it. Key choice is a latency
  decision: `deal` keyed per want id (parallel across wants) vs
  `TRANSPORT_DRAIN_KEY` singleton (serializes all delivery — accepted
  because drain legs are tiny scans+fan-outs, never the delivery
  itself — `deliver` lands per destination key instead).
- **Re-fan dedup is a queue-hygiene tool, not a correctness tool** —
  identical sends pile onto the same key's FIFO; the row guard makes
  them harmless but the QUEUE still pays (`sentRecently`/`armedDue`
  exist for this). Under flow control, scope/limitKey can bound a class
  of invocations natively — but a limitKey re-orders across keys:
  same-object sends needing order share the limitKey.
- **Bidi vs request-response**: our endpoint is HTTP/2
  `createEndpointHandler` → bidi streaming (progress without
  suspend/re-invoke). On non-bidi transports every wait point suspends
  and replays — visible latency per hop. Keep the endpoint h2.

## 5. Test surfaces

- **`@restatedev/restate-sdk-testcontainers`** — `RestateTestEnvironment.start({services, alwaysReplay: true, disableRetries: true})`; `stateOf(obj, key)` reads/mutates K/V directly. `alwaysReplay` forces replay at every suspension — the non-determinism hunter. The rt-spike tests gate on `NEKTAR_RESTATE_URL`; this package is the no-server-needed version of the same honesty.
- **The `Dispatcher` is an emulator, not Restate** — it models per-key serialization, delay, at-least-once-ish redelivery and close-drains. It does NOT model: the journal (no replay), ingress idempotency dedup, Terminal/Retryable/Pause routing, suspend/resume, deployment pinning. A green Dispatcher run proves handler logic; `rt/drift` findings claim plane semantics.
