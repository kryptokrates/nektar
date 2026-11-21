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
