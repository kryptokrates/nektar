# bsv recipes — the fast paths, mapped to repo seams

Each recipe states the goal, the canonical shape, and where it lives in
this codebase. Cross-references: `spv-strict` for verdict rules,
`event-arch` for wake/ceiling rules.

## R1 — Broadcast a tx at network speed

Goal: ACCEPTED in one RTT, statuses pushed, proof verified on arrival.

1. Build + sign with `sourceTransaction` on every input.
2. `port.broadcast(tx)` — `BsvBroadcaster` sends **EF** to every
   `broadcasterUrls` endpoint concurrently; first acceptance wins.
   (A refused verdict from any route outranks a bare transport failure
   in the merged error.)
3. Statuses arrive pushed — webhook (`X-CallbackUrl`) or SSE
   (`statusStream`); both route through `handleBroadcasterStatus` →
   `ChainEvents.note()` as **hints**.
4. `MINED` push carries the BUMP → `deliverTxProof` verifies the root
   vs. `tracker` → `observe()` records the verdict.
5. The tick only recovers missed pushes (proof-gated poll as ceiling).

Never: a `status === 'success'` short-circuit to a durable claim;
broadcast returns "accepted by a route", not "proven".

## R2 — Never wait for funding stock

Goal: a funding slot exists the moment a lane asks.

1. `ensureTrees` keeps `lowWater` per lane; refill happens in upkeep,
   ahead of demand — never mint inside the deal path.
2. `anchoredTree` stock serves immediately where
   `NEKTAR_ANCHORED_LANES` names the lane (one-unconfirmed-hop rule;
   'market' always). `KnexTreeLedger` and `MemoryTreeLedger` serve by
   the same `anchoredKinds` predicate — stock counts only servable
   leaves.
3. `attachParentPath` heals ancestry as funding parents mine; a tree
   becomes admissible without waiting for its own BUMP.
4. `markProven` attaches the BUMP and strips unproven parents —
   a BUMP-anchored tx terminates BEEF ancestry.

## R3 — Deal flow without a block wait

Goal: deliver on carried evidence; depth only for settlement.

- Buyer carries the **escrow tx wire** in the note (`m.tx`) — the seller
  verifies `spendValid` per input + `deadAncestry` locally, no fetch.
- Seller rebroadcasts **in parallel** with bookkeeping
  (`escrowBroadcastP` started before the `completing` transition) — a
  refused escrow reverts to `quoted`, never produces a false claim.
- Disclosure happens on verified ancestry — **not** on a mined event.
- `settleDepth` gates the money move only; that wait is the security
  parameter, not a defect.

## R4 — Discovery and reconciliation fast

- `discoverUtxos`: `utxos(address)` list → 4 workers verify candidates —
  each entry counts only when its served bytes hash to the claimed txid
  and the vout pays the address. The index dialect owns pacing
  (`pacer` + 429-retry inside `BananaIndex`/`WocIndex`).
- `utxoSpendStatus`: an index "spent" claim is verified only when the
  named spender's own bytes name the outpoint as an input; a mined
  spender's BUMP is checked against witnessed headers.
  `'unspent'` is unprovable by a third party — it answers
  `'spent' | 'unknown'` and never burns on `unknown`.

## R5 — Counterparty zeroconf policy

Different rules for our txs vs theirs:

| Side                                   | Evidence needed to proceed                                                                                                                                     |
| -------------------------------------- | -------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| First-party (we shaped + broadcast it) | `SEEN_ON_NETWORK` hint + our own record of the bytes — we know exactly what was sent                                                                           |
| Counterparty (they sent it)            | carried ancestry verified locally (scripts + amounts + resolvable parents) — `spendValid` + `deadAncestry` — and it must not exceed your ancestor-policy depth |

A counterparty tx that skips inputs is dead ancestry — refuse before
the network is asked, because `TX_MISSING_PARENT` makes it
unbroadcastable forever.

## R6 — Reorg discipline

- `MINED` pushes again on re-mine → `deliverTxProof` replaces the path.
- `unproveFrom(height)` demotes dead-fork BUMPs on trees AND inside
  carried ancestry (mirrors `CarrierJournal.clearPaths`).
- Nothing is final until `IMMUTABLE` — re-announce stock that lost its
  anchor rather than serving stale-proof carriers hosts will refuse.

## Directions (not yet built — do not pretend they exist)

- **F5** Adopt the payment-evidence-statement BRC shape for the seller's
  `evidence: {spv, spends}` record (interop).
- **F6** An `ls_spend` overlay/watch-shim service pushing "outpoint X
  spent" events — removes the last `utxoSpendStatus` probes on the deal
  path. The watch-shim protocol is the natural seam.
- `POST /v1/txs` batch submit for multi-mint `ensureTrees` rounds.
