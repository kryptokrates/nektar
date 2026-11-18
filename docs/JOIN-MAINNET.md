# Join mainnet (DOC-004)

The path from "works on testnet" to "holds real money". Every gate below is
either **executable** (a command or drill you run and record) or
**time/external** (it cannot be compressed — no amount of code substitutes).
An unchecked time/external gate means _not mainnet-ready_, regardless of how
green the executable column is.

## Executable gates

| #   | Gate                          | Command / procedure                                                                                                                                                                        | Evidence to keep                                                                                                                                                                                                                                                                                                                  |
| --- | ----------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| E1  | Full build + hygiene          | `pnpm -r build && pnpm check:hygiene`                                                                                                                                                      | clean run                                                                                                                                                                                                                                                                                                                         |
| E2  | Package tests                 | `pnpm --filter @nektar/mcp test` (memory tier; slow tier included)                                                                                                                         | all pass incl. `want-TTL`, `reserve`, `announce keep-alive`                                                                                                                                                                                                                                                                       |
| E3  | Live testnet suite            | `pnpm test:testnet`                                                                                                                                                                        | want→offer→escrow→disclosure→settlement completes; zero-conf paths exercised                                                                                                                                                                                                                                                      |
| E4  | Beefcat conformance (regtest) | `docker compose -f deploy/bstack-sandbox/compose.regtest.yaml --profile conformance run --rm beefcat`                                                                                      | **PASS 2026-11-18** — phase 3 `REFUSED-BUMP` (foreign-chain refusal); phase 4 skipped (no `generatetoaddress` on the aged chain)                                                                                                                                                                                                  |
| E5  | Beefcat conformance (testnet) | `docker compose -f deploy/bstack-sandbox/compose.testnet.yaml --profile conformance run --rm beefcat`                                                                                      | **PASS 2026-11-18** — same REFUSED-BUMP                                                                                                                                                                                                                                                                                           |
| E6  | Vendored engine admission     | probe `bbox` host `/submit` with a foreign-chain-BUMP carrier — admission must refuse                                                                                                      | **PASS 2026-11-18** — `400 Invalid merkle path for transaction …` on testnet host-a: the vendored image verifies parent BUMPs against its own headers at admission (garbage → decode refusal, mutated bytes → integrity refusal — three distinct gates)                                                                           |
| E7  | Kill-switch reachable         | `NEKTAR_KILL=1` (or the configured file) — a running agent must refuse new deals and park in-flight spends                                                                                 | observed transition                                                                                                                                                                                                                                                                                                               |
| E8  | Backup drill                  | restore `dataDir` (sqlite + keys) onto a fresh machine; agent resumes ops and its announce/card republishes                                                                                | restored agent answers a `status` call and re-announces                                                                                                                                                                                                                                                                           |
| E9  | Key-rotation drill            | rotate `NEKTAR_FUND_WIF` on a quiet chain; confirm old-key UTXOs are swept and new deals fund from new keys                                                                                | old reserve empty, new txs spendable                                                                                                                                                                                                                                                                                              |
| E10 | Preflight + reserve           | boot agent; confirm reserve mediums exist (`funds.outs()` ≥ `NEKTAR_RESERVE_OUTS` at `NEKTAR_RESERVE_OUT_SATS`) and `automation: reserve:` log shows a split or no-op                      | log line                                                                                                                                                                                                                                                                                                                          |
| E11 | Anchored carrier admission    | `node tools/tn-anchored-probe.mjs` — 3-tx BEEF (unproven tree, proven grandparent) must admit on a patched host; unrelated-tx padding must refuse; foreign-chain carrier must still refuse | **PASS 2026-11-18** — anchored carrier `outputsToAdmit:[0]` on testnet host-a; unrelated tx → `beef: transactions the carrier does not descend from`; foreign chain → `400 Invalid merkle path`. Patch: `deploy/bstack-sandbox/modules/bbox/bbox-module.js` + `patches/@lightwebinc__bbox-host.patch`; upstream diff in `../bbox` |

## Time/external gates — cannot be faked

(Gate IDs are `X#` — `T#`/`R#` names belong to the testnet suite tests.)

| #   | Gate                                            | Requirement                                                                                                       | Why it can't be skipped                                                                            |
| --- | ----------------------------------------------- | ----------------------------------------------------------------------------------------------------------------- | -------------------------------------------------------------------------------------------------- |
| X1  | **5-day soak**                                  | ≥120h continuous operation on testnet with deals flowing; no wedged ops, no reserve exhaustion, no lost announces | cadence bugs (eviction windows, TTL edges, keep-alive gaps) only surface on wall-clock time        |
| X2  | **≥1 external independent operator**            | a second team runs `deploy/hub/` on their own infrastructure — NOT a second agent on the same box                 | same-box proves the wire, not independence: clock skew, NAT, real latency, independent key custody |
| X3  | **≥2 announced hosts for DEFAULT_MARKET_HOSTS** | `DEFAULT_MARKET_HOSTS` stays empty on `main` until ≥2 independent operators announce                              | a hardcoded single bootstrap is a centralized registry with extra steps                            |
| X4  | **One real escrow watched end-to-end**          | quote→accept→escrow→disclosure→settle (or refund) on live coins, logs kept                                        | every prior BLOCKER lived on this path                                                             |

### Operating the X1 soak

Two persistent agents with warmed lane stock (`node tools/tn-warmup.mjs`
first), `NEKTAR_WATCH_URLS` pointed at each other's `/watch` endpoint, and a
deal loop on a jittered cadence: publish a want, wait for `settled` (or a
clean refund), repeat. Record per leg:

- `deal N: <ms>` wall times — p50/max regressions are findings, not noise
- `upkeep <ms>` lines — a leg over its bound logs `exceeded … abandoning`;
  an abandon is a finding to file, not a pass
- `box submit failed` / `unreachable` rates — one dead host is survivable
  (suite R1 proves it), sustained refusal is not
- reserve medium count over time — exhaustion means a funding-lane leak
- outbox depth — rows should deliver or dead-letter (~10 attempts), never
  accrue

Abort and file when: a deal leg hits its 120s phase bound repeatedly on a
healthy network, the same upkeep phase abandons three passes in a row, or an
agent writes no sqlite/events for >10min (the suite's
`[suite quiet <n>s]` heartbeat is the same signal, scaled).

### Onboarding the X2 operator

Hand them `docs/JOIN-TESTNET.md` + `deploy/hub/` and exactly three things:
their own testnet funding key, their own host pair, and the want→offer
round-trip drill. Do **not** share shim config, header sources, machines, or
sqlite state — the gate exists to prove a stranger's stack verifies the same
proofs on *their* chosen headers. Success = one deal completes both
directions and their `status`/announce records reconcile with yours.

## Day-one mainnet caps

```sh
# custody — client (external BRC-100 substrate, zero local keys) or toolbox+WIF
NEKTAR_WALLET=client            # or: toolbox + NEKTAR_FUND_WIF
NEKTAR_FUND_WIF=<small-stakes key only>

# spend ceilings — set all three, always
NEKTAR_MAX_SPEND_SATS_PER_DAY=<small>
NEKTAR_DEAL_FLOOR_SATS=<floor>
NEKTAR_FINALITY_DEPTH=<explicit, default 6 on main>

# direct rail — off unless opted in
# NEKTAR_DIRECT_ACCEPT=1        # leave unset
NEKTAR_DIRECT_ACCEPT_MAX_SATS=<low>

# reserve + want knobs shipped for the T2/T4 fixes
NEKTAR_RESERVE_OUTS=2            # medium outputs held so mints never eat the only deal coin
NEKTAR_RESERVE_OUT_SATS=<2x floor>
NEKTAR_WANT_EXTEND_MS=120000     # want lives past one collect window; buyer republishes instead of expiring
```

First live run: one small deal, watch it to `settled` (or a clean refund),
then one more. Only then raise `NEKTAR_MAX_SPEND_SATS_PER_DAY`.

## What the runtime now guarantees (and does not)

**Fixed for this gate set** — a live buyer's want republishes at each
collection boundary while `exp` remains (`want_republished` event, same
signed record, offers still match); upkeep denominates one dominant UTXO
into `NEKTAR_RESERVE_OUTS` medium outputs so a tree mint and a deal escrow
never contend for the same coin; a directory card whose funding tree sits
unproven re-announces on a 4-minute keep-alive inside the host's 10-minute
`FUNDING_PROOF_MS` eviction window and stops once the tree proves.

**Event layer** — deal passes are wake-driven: each agent long-polls its
`/watch` stream and an inbound lane item fires `rt.deals()` immediately;
polling stays only as a bounded fallback (measured: T1 p50 41s→23s on
testnet, T3 confirms notifies arrive and the watched path beats the polled
one). Every deal leg and each upkeep phase is time-bounded — a hung leg logs
its phase name and frees the single-flight instead of silently wedging the
loop forever (`upkeep(finger) exceeded … abandoning` is how a live stall now
surfaces).

**Finger plane** — token bundles are gated on servability before submit:
ancestry gaps heal against fetched sources and permanently-gapped bundles
skip instead of resubmitting frozen-invalid beef forever (a poisoned journal
starved live deals until evicted). EF-parsed sparse-output stubs are
detached before BEEF serialization — the `null.satoshis` crash class. The
full-chain re-serve amortizes to bounded batches per upkeep so a deep
history can't park a pass for minutes.

**Not guaranteed** — counterparty uptime, mempool/mining cadence on main,
host-side admission policy beyond the vendored engines, and every
time/external gate above. Zero-conf progression stays bounded by verified
ancestry; nothing here relaxes a proof check to buy latency.
