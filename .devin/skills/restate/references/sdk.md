# sdk.md — Restate TypeScript semantics cheatsheet

Load when authoring or auditing handler/object/service code. Pinned to
docs.restate.dev as of writing — verify against the live docs for new
versions (the surface moves fast: `PauseError`, `attemptCompletedSignal`,
`onJournalMismatchErrors`, `signal`, scope/limitKey are all recent).

## Service shapes

| Type           | Construct          | Concurrency                                                       | State                 | Use for                                                   |
| -------------- | ------------------ | ----------------------------------------------------------------- | --------------------- | --------------------------------------------------------- |
| Basic Service  | `restate.service`  | unlimited parallel                                                | none                  | ETL, fan-out, background jobs, sagas                      |
| Virtual Object | `restate.object`   | exclusive handlers: serial per key; `shared` handlers: concurrent | K/V per key           | entities, per-row serialization, state machines, sessions |
| Workflow       | `restate.workflow` | one `run` per workflow ID + shared handlers                       | K/V per ID + promises | multi-step once-per-ID processes                          |

- Exclusive handlers on a key execute in **arrival order**; sends from
  other sources interleave between them.
- `restate.handlers.object.shared(fn)` — read-only, runs concurrently
  against the exclusive queue. Reads that don't need serialization
  belong here, not on the exclusive path.
- `ctx.key` — the object/workflow key inside the handler.

## The replay contract

- **Journal**: every SDK action (`ctx.run`, sends, state ops, sleeps,
  signals) appends an entry. On retry, completed entries replay their
  recorded result; the first unjournaled point resumes live execution.
- **`ctx.run(name?, fn, retryPolicy?)`** — the only wrapper that records
  an arbitrary side-effect's result. Inside the closure: NO ctx ops
  (no nested run, no `ctx.get`, no calls — structure error). Result
  serialized JSON by default.
- **Determinism requirements**: no `Math.random()`/`uuid`/`new Date()`/
  unordered-map iteration in handler code paths feeding journal ops —
  use `ctx.rand.uuidv4()`, `ctx.rand.random()`, `await ctx.date.now()`
  (seeded by invocation ID — identical across retries).
- **Mutating outer-scope state inside `ctx.run`** then reading it later
  = non-determinism (the mutation doesn't replay). Return it instead.
- **Journal mismatch = RT0016.** Replay produced different ops than the
  journal — non-deterministic code OR in-place code change that
  reordered/added/removed ops or changed their inputs (state keys,
  payloads). `options.onJournalMismatchErrors: 'retry'|'pause'|'fail'`
  chooses the outcome (default retry).
- **In-place edits**: only safe inside an existing `ctx.run` body, or
  for deterministically-reproducing bugs. Reordering ops, changing
  inputs, adding/removing ops = new deployment required.

## ctx — the ops surface

```ts
// state (Object/Workflow contexts only)
await ctx.get<T>(key)          // K/V read
ctx.set(key, value)            // journaled write
ctx.clear(key) / ctx.clearAll()

// calls — request-response, journaled, cancelled with caller
await ctx.serviceClient(svc).handler(arg, restate.rpc.opts({ idempotencyKey }))
await ctx.objectClient(obj, key).handler(arg)
await ctx.workflowClient(wf, id).run(arg)
await ctx.genericCall({ service, method, parameter, key, inputSerde, outputSerde })

// sends — detached one-way; SURVIVE caller cancel+kill, at-least-once
ctx.serviceSendClient(svc).handler(arg, restate.rpc.sendOpts({ delay, idempotencyKey }))
ctx.objectSendClient(obj, key).handler(arg)
ctx.genericSend({ service, method, parameter, key, inputSerde, delay })

// delayed — durable timer; PREFERRED over ctx.sleep+send:
// handler completes (no key blocking, no pinned deployment)
sendOpts({ delay: { seconds: 5 } })   // or genericSend's delay field

// attach/cancel
const h = ctx.serviceSendClient(svc).handler(arg, sendOpts({ idempotencyKey }))
const id = await h.invocationId
await ctx.attach(id)     // result needs the send's idempotency key
ctx.cancel(id)           // cooperative; propagates at await points

// signals / awakeables / promises
await ctx.signal<T>('name')                    // named, multi-resolve, per-invocation
const { id, promise } = ctx.awakeable<T>()     // one-shot external token (HTTP-resolvable)
ctx.resolveAwakeable(id, val) / ctx.rejectAwakeable(id, err)
await ctx.promise<T>('name')                   // workflow-scoped, resolve once
await ctx.promise<T>('name').resolve(val)

// sleep — durable timer; INSIDE AN EXCLUSIVE HANDLER IT BLOCKS THE KEY
await ctx.sleep({ seconds: 10 })
await promise.orTimeout({ seconds: 5 })        // TimeoutError on expiry

// request metadata
ctx.request().id                       // invocation ID — stable idempotency source
ctx.request().attemptCompletedSignal   // AbortSignal: old attempt ended (zombie work)
ctx.request().scope / .limitKey        // flow-control routing

// deterministic helpers
ctx.rand.uuidv4() / ctx.rand.random()  // seeded per invocation — replay-stable
await ctx.date.now()                   // journaled wall clock
```

## Error taxonomy — what each throw means

| Throw                                       | Effect                                                                            | Use for                                             |
| ------------------------------------------- | --------------------------------------------------------------------------------- | --------------------------------------------------- |
| `Error` (any)                               | infinite retry, exponential backoff — suspends on the key, blocks its queue       | transient faults only                               |
| `TerminalError(msg, {errorCode, metadata})` | ends invocation; propagates code+metadata to callers                              | verdicts — record the terminal domain outcome FIRST |
| `RetryableError(msg, {retryAfter})`         | retry after the given delay                                                       | `Retry-After`/rate-limit legs (inside `ctx.run`)    |
| `PauseError`                                | pauses the invocation for operator fix — inside `ctx.run` only; outside = ignored | "human must intervene"                              |
| `TimeoutError` (from `orTimeout`)           | throws to the handler — a delivery event, NOT a verdict                           | bounding a wait                                     |

- Per-run retry policy: `ctx.run(name, fn, { initialRetryInterval, retryIntervalFactor, maxRetryInterval, maxRetryAttempts, maxRetryDuration })` — exhaustion throws `TerminalError`.
- `options.asTerminalError: (err) => TerminalError | undefined` — maps library errors to terminal.
- Inactivity timeout (~1min default, service config): no new journal entries → invocation suspends+retries. Long legs (LLM calls) need the config raised or the leg re-shaped.
- **`attemptCompletedSignal`**: when Restate retries, the OLD attempt keeps running — its result is discarded, its side-effects are not. Pass the signal to `fetch`/long ops so zombie work stops. Does not un-send a committed effect — non-idempotent effects still need an idempotency key.

## Deployment + ops

- `restate.serve({ services: [...], port: 9080 })` — the endpoint; register it (`restate deployments register http://host:9080`, `--use-http1.1` if needed, `--force` for dev overwrite).
- **Immutable deployments**: retries pin to the deployment that started the invocation. Version the endpoint (new URL → `register`) for real upgrades; old deployment drains then `deployments remove <id>`.
- **`--force` re-register** = same endpoint, new code — in-flight invocations hit RT0011/RT0016: armed sends queue against a deployment that no longer serves them. Dev-only or accept the orphans (the domain floor re-arms what it owns).
- Managing invocations: `restate invocations list|cancel|kill|pause|resume|restart-as-new`, `cancel <Service>/<handler>` bulk, `resume <id> --deployment <new_id>` migrates stuck work. Cancel = cooperative compensation at await points; **kill skips compensation and leaves detached sends alive** — last resort.
- Introspection: SQL over `sys_invocation`, `sys_service_state`; admin API `:9070`; the UI shows per-key queues + journals.
- **Clock sync**: delayed sends' wake time is computed by the SDK then re-read by the server — skewed SDK/server clocks fire early/late. Same rule inside the app: delay math must subtract the clock that stamped the deadline.

## Ordering + delivery guarantees

- Same `(service, key)` sends: arrival order, serial execution. Different keys: any interleave.
- At-least-once everywhere — idempotent handlers or explicit `idempotencyKey`.
- Within ONE handler execution, identical sends dedupe automatically; across independent invocations they do not.
- `orTimeout` bounds a wait, not the work — the callee still runs.
- Cancel surfaces at the next await point as `TerminalError`; sends already emitted still fire.
- **Same-key request-response calls deadlock** — A↔B on the same keys, or A→B→C→A cycles, are the documented deadlock class. nektar's escape is `inRtScope`/`rtScopeKey` (ALS scope bypasses the object hop for the owning key).

## Service-level configuration (`options:` on the service def)

| option                                                                     | default                               | the audit question                                                                                                                          |
| -------------------------------------------------------------------------- | ------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------- |
| `retryPolicy` `{initialInterval, maxInterval, maxAttempts, onMaxAttempts}` | ~`50ms → 60s`, ~70 attempts → `pause` | does the leg's failure deserve unbounded retry, a bounded pause, or a TerminalError verdict?                                                |
| `inactivityTimeout`                                                        | **1 min**                             | any `ctx.run` leg or pre-journal stretch longer than this suspends mid-flight (LLM calls, lane fanouts, big scans — split legs or raise it) |
| `abortTimeout`                                                             | 10 min                                | ceiling after inactivity before the attempt is killed                                                                                       |
| `journalRetention`                                                         | 24 h                                  | how long completed journals stay inspectable — the audit/debug window                                                                       |
| `idempotencyRetention`                                                     | 24 h                                  | dedup window for ingress keys — financial effects may need longer                                                                           |
| `workflowRetention`                                                        | 24 h                                  | shared-handler/promise lifetime after `run` completes                                                                                       |
| `ingressPrivate`                                                           | false (public)                        | internal objects must not answer the public ingress — set true on custody/tx/deal-class services                                            |
| `enableLazyState`                                                          | false (eager)                         | lazy K/V loads per-key on read vs full snapshot per invocation                                                                              |

Server-side knobs (`restate.toml` / env): `[invocation.default-retry-policy]` (incl. `on-max-attempts = "pause"|"kill"`, `max-attempts = "unlimited"` for true-forever), `worker.invoker.inactivity-timeout`, `default-journal-retention`.

## Request lifecycle — what actually happens

1. Ingress persists the request + creates the journal — durability starts BEFORE the handler runs.
2. Server opens a bidi HTTP/2 stream to the endpoint; journal entries stream back per ctx op.
3. Handler runs; each ctx op appends to the journal.
4. Failure/retry: the server replays the journal to the service — journaled ops return recorded results, live execution resumes at the first gap.
5. Inter-handler calls route THROUGH the server (not service-to-service) — end-to-end idempotency.
6. **Non-bidi transports** (Lambda, request-response mode): the SDK runs to the first wait point, suspends, ships the journal, gets re-invoked with it — a replay per wait, visible latency per hop.

## Testing surfaces

`@restatedev/restate-sdk-testcontainers` — `RestateTestEnvironment.start({ services, alwaysReplay?, disableRetries? })`:

- `alwaysReplay: true` — forces journal replay at EVERY suspension point; the non-determinism hunter (surfaces exactly the `rt/replay-fork`/`rt/non-determinism` classes)
- `disableRetries: true` — failures surface immediately, no backoff hang (error-path tests)
- `stateOf(obj, key)` — direct K/V read/mutate for arrange/assert
- vs our `Dispatcher`: testcontainers exercises the REAL plane (journal, dedup, retry routing, suspend/resume); the Dispatcher models the subset it can (serialization, delay, sends). `rt/drift` claims a plane semantic — verify on one of these, not the emulator.

## Flow control (scopes + limit keys — recent, moving)

- `ctx.scope('name')` routes an invocation into a named concurrency scope; `rpc.opts({limitKey: 'l1/l2'})` subdivides it (two levels, all levels count simultaneously).
- Scope = sharding + idempotency + object-key namespace: `Cart "a"` under scope `bob` ≠ under `joe`. A scope is one partition — low-cardinality scopes are hot spots.
- Rule book (cluster-wide): `restate rules set "scope[/l1[/l2]]" --concurrency N`, wildcard `*` = per-scope default (NOT global).
- Ordering caveat: same-object sends with the SAME limitKey keep arrival order; different limitKeys each queue independently — a held-back key lets a later send jump ahead.
- Map: the `sentRecently`/`armedDue` dedup bounds identical-send churn inside one key's FIFO; scopes bound concurrency across a CLASS of invocations (per-tenant, per-downstream — e.g. a `broadcaster` scope capping chain-call legs).

## Adjacent ecosystem (nektar-relevant)

- **`restatedev/xstate`** (`@restatedev/xstate`) — runs XState machines as Virtual Objects: machine = object key, events = sends, transitions journaled. Our machines stay the spec/audit surface (SQLite authority + shadow replay); this is the library if a machine ever needs to BE the executor.
- **`@restatedev/restate-sdk-tunnel`** — endpoint→Restate Cloud/BYOC outbound tunnel, no inbound port.
- **Sagas** — the compensation-list pattern: push a `ctx.run` compensation before each step, run reversed on `TerminalError`. Our custody rollback (releaseBudget/leases, abandon-prepared) is the same shape, hand-rolled — the pattern validates the shape and names the rule: compensate only on terminal verdicts, never on retried transient throws.
- **`durableCalls(ctx)` middleware** (`@restatedev/vercel-ai-middleware`) — journals LLM responses for agent replay; `AIExecutor` legs wrapped the same way.
- **Awakeable approvals** — `ctx.awakeable` + `/restate/awakeables/<id>/resolve` over HTTP = the policy-escalation/approval-gate pattern with zero polling.
- **Ingress auto-idempotency** (new clusters): the ingress assigns a random idempotency key to keyless calls so ITS retries don't double-execute — retention then governed by `idempotencyRetention`.
