# signals — the workaround census

Load in Phase 1. Each signature produces **candidates, not findings** —
a count and a location list. The legitimacy test (SKILL.md) runs later;
this file's job is recall, not judgment. Missing a real class is worse
than over-collecting.

Repo note: `rg` is recursive by default — `-r <str>` is replace-mode,
never use it; `-n` prints line numbers. Prefer the built-in grep tool
where the agent has it. Commands below assume repo root.

## 1 — Marker census (the code admits it)

```sh
rg -n "workaround|hack|FIXME|TODO|XXX|temporary|for now|band-?aid|belt|just in case|defensive|paranoid|shouldn'?t happen|in theory|safety ?net|edge case|can'?t hurt|be safe|belt.?and.?(braces|suspenders)" packages/*/src
rg -n "compat|legacy|shim|migration|migrate|transitional|deprecated|for backward" packages/*/src
```

`tools/.leftover-baseline.json` already counts TODO/`as any`/`ts-ignore`
per package — read it for hotspots before grepping. A marker is a
confession; the question is what it's confessing to. Legit version: a
TODO with an issue link and a real plan; a `@deprecated` with a live
migration path (the ratchet tracks cleanup).

## 2 — Catch→default (failure swallowed, not handled)

```sh
rg -n "catch" packages/*/src -A 3
```

Filter for bodies that: return a default (`return null|undefined|[]|false|0|""`),
assign a fallback and continue, or log-only (`dbg`/`writeErr` then fall
through). Each asks: _who upstream let this fail, and is that failure
still possible?_ Legit version: a fallback that IS the contract
(degrade-to-poll, SPV-verify-never-trust) — check whether the fallback
is a designed degraded mode or an accident.

## 3 — Retry dialects

```sh
rg -n "retry|retries|backoff|attempts?|maxAttempts|attempt" packages/*/src
rg -n "while.*await.*(sleep|delay|setTimeout)|for.*attempt" packages/*/src
```

Count distinct implementations: different base/cap/jitter conventions =
the smell even when every loop is individually justified. Also flag a
retry whose awaited call is **deterministic** (pure parse, local read,
shape check) — retrying determinism is compensating for a bug, not
transience. Legit version: one `withRetry` substrate + declared intent
per site; a retry bounded and jittered against a genuinely transient
failure.

## 4 — Re-normalization / re-validation

```sh
rg -n "normaliz|canonical|sanitiz|coerce|trim\(\)|toLowerCase|encodeURI|decodeURI" packages/*/src
rg -n "function (normalize|fix|ensure|coerce|adapt|convert|parse|validate)[A-Z_]" packages/*/src
```

Two shapes: the same fixup applied at N call sites downstream of one
producer, and the same precondition re-checked at successive layers
(`if (!x) throw` where the caller already checked). Trace each to its
producer: if the producer could emit a canonical shape once, every
downstream normalizer is a candidate. Legit version: validation AT the
outermost boundary (config load, wire decode, RPC ingress) — one layer
deep at most, never repeated per consumer.

## 5 — Defensive optionality

```sh
rg -o "\?\." packages/*/src | cut -d: -f1 | sort | uniq -c | sort -rn
rg -n "\?\?|\|\| \[\]|\|\| \{\}|\|\| ''|\|\| \"\"" packages/*/src
```

Density is the tell: files where deep-internal code sprinkles `?.`/`??`
over data the type declares present are compensating for a producer
contract nobody trusts. Check the producing type — if it says `T` and
the consumer treats it as `T | undefined`, one of them is lying; which
one is the cluster's question. Legit version: optionality that mirrors
the declared type at a real boundary.

## 6 — Double-check / sleep-then-check

```sh
rg -n "sleep|delay|setTimeout|setInterval|waitFor|poll" packages/*/src
rg -n "verify.*(after|post)|read.*after.*write|re-?check|re-?read|confirm" packages/*/src
```

Read-after-write verification and poll-until-true both say "the layer
below isn't believed". nektar-specific: every recurring wait must name
its producer signal or be classified `probe-tier`/`floor-only` in the
wake-coverage map — a wait with no producer is either a missing emission
(the fix: producer announces) or a workaround for an event path that
can't be trusted. A raw `setInterval` outside the map already fails CI;
the audit hunts the same shape where the map's classification is the
workaround. Legit version: bounded ceilings guarding a real event path.

## 7 — Compat/legacy shims

```sh
rg -n "compat|legacy|v1|old format|oldFormat|old-style|shape.*detect|Array\.isArray.*\?|typeof.*=== 'string'.*JSON\.parse" packages/*/src
fd "compat|legacy|shim|migrat" packages
```

Shape-detection branches (`is it the old shape or the new shape?`) mean
a migration that never finished. For each: can old-shape records still
be produced, or only read? Read-only legacy + no writer = a migration
away from deletion. Compare with the basket-sweep precedent:
`wallet.sqlite` → `.migrated` is a shim _with_ a completed arc — the
legit version has an end state, not just a beginning.

## 8 — Parallel truth

```sh
rg -n "cache|shadow|mirror|reconcil|bookkeep" packages/*/src
```

Two stores of the same fact (a map + a table, a journal + a domain row,
a flag + a status). Ask which one code believes when they disagree —
the disbelieved one is a workaround for the trusted one's missing
guarantee. Legit version: derived state with a defined rebuild
(cache-from-source, projection-from-journal) — the second copy is a
view, not a second truth.

## 9 — Git archaeology (fix density)

```sh
git log --oneline --grep="fix\|hotfix\|workaround\|patch\|revert" -- packages/<pkg>
git log --format="%h %s" -n 200 -- packages/<pkg>/src/<file>
git log -p -S "<guarded symbol>" -- packages/<pkg>/src/<file>
```

Files where >~30% of recent commits are fix-terms are stratigraphy
sites — each fix layered on the last. `-S` history on the guarded
symbol shows when the guard grew and what fired it. Feed the top
fix-density files into the drill directly; they're clusters that
self-identify.

## Caller tracing (mandatory per candidate)

For each candidate site, enumerate producers/consumers before it can
cluster:

```sh
rg -n "<symbol|helper|pattern>" packages/*/src
```

Classify every caller by the variance it actually passes — not the
type's promise. "Called from 5 sites, 4 pass already-normalized input,
1 passes raw" is a different finding than "5 sites each handle raw".

## Disposition

Every census row gets a disposition before Phase 1 closes:
`clustered` / `insurance` (legit, logged) / `hypothesis` / `out-of-scope`
(with reason). A row with no disposition is a dropped candidate — the
coverage critic in swarm mode checks for exactly that.
