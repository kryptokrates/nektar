# nektar.md — the audit surface

Load with state-audit for nektar reviews. The stores, unions, and projection
splits below are the map the authority worksheet fills in; the incident table
maps each past failure class to the phase that hunts it. Verify against the
code before citing — schemas move.

## Stores & unions — with machine candidacy

| Store / union                                                 | Owns                                 | States                                                                                                                                                                    | Machine candidate?                                                                                                                                                             |
| ------------------------------------------------------------- | ------------------------------------ | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `wallet_transactions` / `TxState` (`automation/src/store.ts`) | ledger of tx intents + outcomes      | `prepared`, `exported`, `submission_unknown`, `submitted`, `observed`, `confirmed`, `rejected`, `conflicted`, `expired`                                                   | **y** — clean union, unknown-state already modeled, boot replay exists (`runtime.ts` replays `prepared`/`submission_unknown`); `expired` is deliberately "not a chain verdict" |
| `market_operations` op states (`automation/src/market/`)      | deal/op state machine                | incl. `created→preparing→quoted→escrow_verified→disclosure_*→settled`, plus `funding_deferred`, `refund_pending`, escalation exits via `market/types.ts` `maxAgeMs` table | **y** — the late-landing fixes live here; the `maxAgeMs`→`escalate:human` table is already a shadow machine declaration                                                        |
| `utxos` rows (`automation/src/sqlfunds.ts`)                   | owned coin lifecycle                 | `live`, `dropped` (dropped IS the tombstone); reservation overlay via `wallet_reservations` (durable) + in-memory `reserved`                                              | **y** — adopt/reserve/spend/died/revive transitions; the tombstone-vs-live split is a two-state machine with a revival edge                                                    |
| `bstack_trees` (`bstack/src/trees.ts`)                        | funding-tree stock                   | `proven` / unproven (`unproven===0` mint gate; unproven drop-on-rejected hints)                                                                                           | partial — `proven` is an external verdict (chain), the local side is stock accounting; candidate for the stock lifecycle not the proof state                                   |
| `market_outbox` (`automation/src/store.ts`)                   | intent journal + delivery ledger     | pending/delivered per destination                                                                                                                                         | **n** — already the machine's effect ledger; audit it as the intents column, not as a resource                                                                                 |
| `market_events`                                               | append-only evidence log             | —                                                                                                                                                                         | **n** — the replay corpus for shadow mode, not state                                                                                                                           |
| `spend-ledger.json`, `tx-log.jsonl`                           | guard accounting, broadcast evidence | —                                                                                                                                                                         | **n** — append evidence; authority for "what did we attempt"                                                                                                                   |

## Authority vs projections

**Authority** (writes win): `TxState`, `market_operations.state`, `utxos`
rows, `market_outbox` intent rows. **Projections** (derive, don't store):
`deals` tool labels (computed from `walletTxs` + op state — if a label is
persisted anywhere it drifts), `funds.utxos` in-memory cache, tree
`remaining` counts, any `lastStatus`/summary column denormalized onto a
parent row. The fix pattern for projection drift is deletion: compute the
projection from the authority at read time — don't add a third writer that
"tries to keep them in sync."

## Incident anchors → the phase that hunts them

| Incident class                                                                                                  | Phase                                               | Rule ID that names it                                                   |
| --------------------------------------------------------------------------------------------------------------- | --------------------------------------------------- | ----------------------------------------------------------------------- |
| phantom-parent — change adopted into the book before the tx had any witness; refused-tx change stayed spendable | 2 (optimism without compensation)                   | `nektar/optimistic-adopt`                                               |
| refused-escrow-landed — chain applied what the guard refused; no edge from `failed`                             | structural check 1 (terminal-without-revival)       | fix shipped as `refund_pending`                                         |
| daily-cap refusal treated as terminal — burned the refusal threshold on a policy window                         | 5 (failure vocabulary conflated)                    | `nektar/refusal-classified`; fix shipped as `funding_deferred`          |
| stale sweep list — in-memory list vs durable table diverged after concurrent drops                              | 3 (restart inventory)                               | F11 state-list drift                                                    |
| sealed-envelope overflow — size checked on plaintext, bound lives on the sealed artifact                        | 5 (limit on wrong representation)                   | `nektar/sealed-budget`, `nektar/envelope-bound`                         |
| submitted≠landed — own broadcast status treated as wire presence                                                | 2 (event-source column)                             | `nektar/write-side-verify`, `spv/self-submitted`                        |
| silent adopt refusal — `void funds.adopt(...)` ate the refusal reason                                           | 3 (unowned failure mode — "logs" is not a detector) | observability fix; the sensor asks "who notices"                        |
| want-expiry race — want TTL died while escrow broadcast was in flight; no component owned the wait              | 3 (owner-of-the-wait)                               | surfaced via live-test; TTL config now exists (`NEKTAR_WANT_EXTEND_MS`) |
| oversized outbox payloads persisting past the rule change                                                       | 5 (persisted shape outlives writer)                 | `data/schema-drift`                                                     |

## Shadow-replay procedure (for any future machine rollout)

1. **Corpus**: `market_events` + `tx-log.jsonl` + `wallet_transactions`
   transitions from a real run — the events are the replay input, the
   durable rows are the expected outputs.
2. **Replay**: drive the candidate machine (pure transition function) with
   the recorded events, beside the live FSM. Persist nothing — compare the
   produced `(nextState, intents)` against what actually happened.
3. **Diff**: every divergence needs a named cause — missing event, wrong
   guard, drifted projection, or a real bug the machine exposed. "Close
   enough" is not an explanation; unexplained diffs block switchover.
4. **Switchover**: machine output becomes authoritative only after diffs
   are explained; keep the intent-journal contract (`state + intent` in one
   transaction, effects after commit — see `statecharts.md`).
5. **Rebuild before reading signal**: mcp tests execute
   `dist/test/*.test.js` — a red or green test on stale `dist/` is noise.
   `pnpm -r build` before trusting results.
