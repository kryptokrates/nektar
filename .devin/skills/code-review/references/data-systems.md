# data-systems — correctness under distribution, persistence & replay

Load for diffs touching persistence (SQLite, `*.json` ledgers, journals),
messaging (outbox/inbox/envelopes/lanes), chain interaction (broadcast,
proofs, reconciliation), or anything restartable. Distilled from Kleppmann's
_Designing Data-Intensive Applications_ (adapted under MIT from
ciembor/agent-rules-books) and mapped onto nektar's actual stores. The bias
being corrected: **nothing here is local, ordered, fresh, or exactly-once.**

## nektar's sources of truth (know them before reviewing)

| Store                               | Owns                                      | Durability             |
| ----------------------------------- | ----------------------------------------- | ---------------------- |
| `reserve.json` + chain              | UTXO set the agent can spend              | File + chain reconcile |
| `automation.sqlite` (`market_*`)    | Op state machine, outbox, inbox, policies | SQLite                 |
| `records.sqlite`, `journal`         | Anchored fragments, log trees             | SQLite + chain anchors |
| `tx-log.jsonl`, `spend-ledger.json` | Broadcast evidence, guard accounting      | Append files           |
| chain (testnet/mainnet)             | Escrow UTXOs, carriers, proofs            | The final arbiter      |

Rule of thumb: **the chain arbitrates; local stores cache intent.** Any code
that treats a local row as proof of an on-chain fact is a finding.

## The catalog

| Rule ID                        | Severity | Rule                                                                                                                                                                                                                                                                 |
| ------------------------------ | -------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `data/hidden-source-of-truth`  | MAJOR    | Two stores can disagree and nothing says which wins (local `state` vs chain reality, reserve vs discovered UTXOs). The reconciliation loop must exist AND name its arbiter — don't assume the newer write.                                                           |
| `data/durability-point`        | BLOCKER  | A side effect (broadcast, send, spend) executes before the intent is journaled. Order is: persist intent → act → record outcome. After-crash state must distinguish "never sent" from "sent, outcome unknown".                                                       |
| `data/at-least-once-victim`    | BLOCKER  | A consumer that applies a delivered message without dedup — outbox/inbox delivery is at-least-once by construction. Effects must be idempotent (same `action_id`, same result) or gated by a processed-set.                                                          |
| `data/lost-wakeup`             | MAJOR    | A wait that sleeps the full interval because the wake signal fires only inside the happy path (e.g. `unblockOutbox` missed on one branch, slot freed but no nudge sent). Every producer that can unblock a waiter must signal on every path.                         |
| `data/schema-drift`            | MAJOR    | Persisted shape changes without a migration or reader tolerance — SQLite rows, journal entries, and `reserve.json`/`keys.json` payloads written by older builds are real inputs. Persisted payloads outlive the code that made them (the oversized-outbox incident). |
| `data/derived-unrebuildable`   | MAJOR    | A cache/projection/fingerprint (`refsHash`, merkle cache, `known` tx map, index) with no defined rebuild path from its source — or code treating derived state as durable truth.                                                                                     |
| `data/ordering-scope`          | MAJOR    | Assuming global ordering where only per-entity order exists (op events, lane messages, journal seq vs wall time). Order-sensitive logic must scope to its key and say what it does with gaps/reorder.                                                                |
| `data/exactly-once-fantasy`    | BLOCKER  | A path whose correctness requires an action to happen exactly once (broadcast, charge, reveal) with no dedup, idempotency key, or reconcile. At-least-once + dedup OR at-most-once + reconcile — pick one, name it.                                                  |
| `data/read-your-writes`        | MAJOR    | Reading back a write immediately through a lagging path (quorum read, host lookup, reserve scan) and treating absence as nonexistence. Our writes are not visible until the medium commits — the want-expired-with-sellers-funded incident class.                    |
| `data/cross-store-transaction` | MAJOR    | Two stores updated assuming atomicity (sqlite + file, ledger + reserve, outbox row + broadcast). If they can't commit atomically, one is the intent log and the other the result — and reconcile must cover the gap.                                                 |
| `data/lag-unobserved`          | MINOR    | Async propagation (host lookup, finger publish, quorum read) with no way to observe lag — a silent staleness source that looks identical to failure. Surface the lag or bound it explicitly.                                                                         |
| `data/partial-fanout`          | MAJOR    | A write to N hosts/stores where failure mid-fanout leaves permanent disagreement (publish to 2 of 3 hosts). Needs a repair sweep or per-destination tracking (outbox per-destination rows already do this — reuse the pattern).                                      |

## Restart & replay drill (apply to every new write path)

1. Die before persist → clean no-op?
2. Die after persist, before act → journal replays safely?
3. Die after act, before outcome recorded → act repeats → dedup'd or
   reconciled, not doubled?
4. Die after outcome → no replay at all?

Any answer of "unknown" is a finding. `market_outbox`, `take()` journaling,
and the op event log are the reference implementations — new paths should
reuse them, not invent a fourth scheme.

## nektar-mapped examples (the incidents these rules encode)

- `data/schema-drift` — persisted oversized accept payloads looping on
  `range: content` after the size rule changed; fix shrunk at read+send.
- `data/durability-point` — `take()` vs `takeAll` journal replay; stale
  carriers replayed indefinitely until replay switched to `take()`.
- `data/read-your-writes` — sellers funded, wants published, but hosts hadn't
  ingested → offers never arrived; read paths must treat lag as normal.
- `data/exactly-once-fantasy` — double-pay across restart is the nightmare
  case; the op journal + outbox `action_id` are the guard.
