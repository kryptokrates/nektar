# module-design — Ousterhout rules at authoring time

Distilled from _A Philosophy of Software Design_ (2nd ed.) for the moment
you're writing code — before review ever sees it. Each entry: the rule,
the red flag that means you're violating it, and how the fix looks in
this codebase. Rule IDs match `../../code-review/references/structure.md`
where they overlap, so a miss here becomes a named finding there.

## Depth: the unit-testable question

A module is **deep** when its interface is much simpler than what it
hides; **shallow** when the interface costs nearly as much to learn as
the implementation saves. The authoring test: _could a caller inline
this in fewer lines than the interface costs to learn?_

| Flag                     | Symptom while writing                                                                | Fix                                                                                                                                                            |
| ------------------------ | ------------------------------------------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `struct/shallow-module`  | new helper/service/wrapper whose signature reads like its body (`doX` → calls `doX`) | Inline it, merge it into the module that owns the knowledge, or push the real complexity inside so the interface earns its keep.                               |
| `struct/push-down`       | every call site repeats the same setup/validation/orchestration ceremony             | The knowledge lives below — pull the ceremony into the module. A messier implementation behind a simpler contract is a win: modules have more users than devs. |
| `struct/leaky-interface` | interface exposes storage shape, wire format, protocol staging, custody details      | Callers must sequence or decode internals → re-draw the boundary around what callers actually mean ("what", not "how").                                        |
| `struct/pass-through`    | args/state transit a function untouched en route to the real callee                  | Route around the middle layer or give it real work. Pass-throughs couple every layer in the chain to the signature.                                            |

## Information hiding: what each module may keep secret

Every module hides a **design decision** — a fact callers must NOT know.
Write down which decision(s) your module owns before writing its exports.

| Flag                          | Symptom while writing                                                                 | Fix                                                                                                            |
| ----------------------------- | ------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| information leakage           | the same decision (a layout, an order, a constant, a policy) is reflected in N places | Co-locate the decision in one module; let everyone else depend on the module, not the fact.                    |
| `struct/change-amplification` | one conceptual change → edits in N unrelated files                                    | The knowledge is scattered, not the work — extract it. Every extra site is a coinvestor in your next change.   |
| `struct/temporal-coupling`    | correctness needs calls in an order types don't enforce (`init`/`prepare`/`process`)  | Restructure so the invalid order is unrepresentable — a builder, a token, an explicit state type.              |
| temporal decomposition        | package/file organized by WHEN code runs (`step1.ts`, `phase2.ts`)                    | Organize by what each piece KNOWS. Order-of-execution files force readers to hold the whole pipeline mentally. |
| `struct/obscured-dependency`  | a dependency invisible in the code — module-level mutable state, ambient singletons   | Make it a parameter/field. If a reader can't see it, it's an unknown unknown — the worst complexity class.     |
| `struct/obscured-obscurity`   | a call is only safe if you already know an invariant from another file                | Contract belongs at the interface — doc it in the signature's comment, or enforce it in the types.             |

## Layers and abstractions

| Flag                             | Symptom while writing                                                          | Fix                                                                                                                               |
| -------------------------------- | ------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------- |
| different-layer-same-abstraction | a higher layer speaks the same vocabulary as the layer below (façade thinking) | Each layer should raise the abstraction level. If layer N+1 = layer N with forwarding, merge or reassign.                         |
| `struct/mode-flag`               | `doThing(x, {persist: false})` — boolean picks between behaviors               | Two functions. A flag param is two functions in a trench coat and a hidden coupling for every caller.                             |
| `struct/speculative-layer`       | interface/factory/plugin-point built for a variant nobody needs yet            | Earn abstractions with the second caller. YAGNI is a depth rule — speculative layers are shallow-by-design.                       |
| `struct/special-general-mix`     | special-purpose names inside a general mechanism (or vice versa)               | General mechanism below; specialization above. Mixing them makes the general part hard to reuse and the special part hard to see. |

## "Somewhat general-purpose" — the sweet spot

Interface general enough for multiple uses; implementation scoped to
today's actual needs. The POSD lesson that surprises people: the
general-purpose **interface** is usually simpler AND smaller than the
special-purpose one — `insert(range, text)` beats `backspace(cursor)` +
`delete(cursor)` + `deleteSelection(sel)`. Ask: _what's the simplest
interface that covers today's use plus the obvious neighbors?_ — then
implement only today's path.

## Error semantics — define errors out of existence

Every exception thrown is complexity exported to every caller. Order of
preference:

1. **Define it out of existence** — make the "error" a normal case:
   clamp, default, no-op semantics (unset of an absent key is a no-op;
   a partial write returns bytes-written).
2. **Mask it inside the module** — retry/aggregate/degrade where the
   knowledge to handle it lives; don't forward it through layers that
   can't act on it.
3. **Aggregate at the boundary** — one handler for a whole class of
   failures (per-destination outbox dead-letter is the repo's shape).
4. **Crash loudly** — for "can't happen" invariants, abort with context
   instead of propagating a recoverable-looking failure upward.

| Flag                    | Symptom while writing                                                    | Fix                                                                                    |
| ----------------------- | ------------------------------------------------------------------------ | -------------------------------------------------------------------------------------- |
| exception proliferation | a throw added because the caller "should know" — no handler design       | If you can't name who handles it and how, re-design until it can't occur.              |
| `common/erased-failure` | returning `false`/`null`/`[]` where the distinction matters for recovery | Failures need shape — a typed error or a boundary that owns the decision.              |
| fail-slow               | continuing after an unrecoverable condition is detected                  | Fail fast at the boundary (`stab/fail-slow`); holding resources during death is worse. |

## Names, comments, obviousness

| Flag                   | Symptom while writing                                                         | Rule                                                                                                     |
| ---------------------- | ----------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------------- |
| `vague name`           | `data`, `result`, `tmp`, `handle` — the name doesn't answer "what concept"    | Name the concept, not the mechanism (`shrinkWire`, not `handleOversizePayloadRetryLogic`).               |
| `hard to pick name`    | you can't name it precisely — usually because the entity has no crisp purpose | Take the difficulty as a design signal, not a vocabulary problem — reshape the entity.                   |
| `hard to describe`     | the doc comment needs to be long to be complete                               | The contract is too complicated — simplify the interface, not the prose.                                 |
| comment repeats code   | comment restates what the adjacent lines plainly do                           | Comments exist for what code can't say: intent, invariants, why-nots, units, preconditions.              |
| impl docs in interface | interface comment describes internals callers don't need                      | Interface comments = contract only; implementation notes belong inside.                                  |
| `nonobvious code`      | a reader must hold >~4 unstated facts to verify the line                      | Software is designed for ease of READING — allocate the reader's effort to the hard parts, not ceremony. |

**Write the interface comments first** (POSD ch.15): if the contract
comment is painful to write, the design is painful to use — fix the
design while it's still cheap. When modifying existing code, check the
diff for comments your change just invalidated — stale comments create
unknown unknowns, the worst symptom class.

## Consistency and the "match the file" trap

Consistency is cognitive leverage: same problem → same shape. But
`struct/deep-vs-flat-mismatch` — matching an existing **bad** pattern
propagates it. When the file's convention is itself the problem, flag
the pattern and improve where you touch; don't grow the mistake. The
`check:leftovers`/`check:arch` ratchets exist precisely so cleanup
counts can only shrink.
