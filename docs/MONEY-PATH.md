# Money path

How sats move through nektar: one coin-state owner, one transaction-status
answer, and a small set of lifecycle verbs every funded spend routes through.
This document is the contract — the gates in `tools/check-arch.mjs` enforce
its seams mechanically.

## Two owners, two answers

**`Funds`** (`packages/chain/src/interfaces.ts`) is the single owner of coin
state. Every implementation shares one state machine — `FundsBase`
(`packages/chain/src/funds.ts`) — over a backend `FundsStore` that only
answers `list`/`add`/`drop`/`revive`/`tombstoned`/`has`:

| implementation                     | store                | custody                           |
| ---------------------------------- | -------------------- | --------------------------------- |
| `MemoryFunds`                      | `listFundsStore()`   | test world, treasury splits       |
| `WalletFunds` (automation/toolbox) | BRC-100 basket       | `toolbox` + `client` wallets      |
| `ReserveFunds` (chain/regtest)     | `ReserveFunder` file | regtest bootstrap, e2e treasuries |
| `AdaptedFunds` (via `fundsOf`)     | utxos-shaped facades | fixtures, bridge call sites       |

`FundsBase` serializes takes (no double-pick races), owns the deal floor,
reservations, holds, tombstones, hint-only filtering and spend checks. No
caller writes bookkeeping — `markSpent`/`addOwned` exist only as store verbs
and test-fixture seeds, and the `funds-bookkeeping` grep gate keeps it that
way.

**`TxFacts`** (`packages/chain/src/txfacts.ts`) is the single answer to "what
is this transaction's status": `{ tx?, proof?, depth?, verdict, source,
hint? }`. `txFactsOf(deps)` composes the read once per consumer. The verdict
is `mined`/`pending`/`dead`, provenanced to `proof | first-party | abandon |
reject` — a held merkle path outranks every outside word. The broadcaster's
status rides separately as `hint` and is fetched lazily, only while the
verdict is pending. Raw probes (`probeTxStatus`) stay adapter-internal and
are not exported from the package index; the `broadcaster-status-read` grep
gate blocks new call sites.

## Lifecycle verbs (whole-transaction)

- `take(purpose, sats)` — reserve an output ≥ sats. Purposes:
  `'deal'` (escrow/payments, exempt from the floor), `'publish'` and
  `'upkeep'` (background anchoring — may not dip owned funds below
  `dealFloor`, default 25 000 sats, `NEKTAR_DEAL_FLOOR_SATS`).
- `spent(tx)` — after a broadcast we aired: consume our inputs, adopt
  outputs that pay our lock (change, escrow returns).
- `exported(tx)` — a signed tx left for someone else to air (BEEF payment):
  inputs consumed, outputs **not** adopted — discovery adopts them if the
  spend lands.
- `died(tx | txid)` — rejection/abandonment: held inputs freed, tombstoned
  parents revived, the dead tx's own outputs tombstoned forever.
- `adopt(tx, vout)` — foreign coins paying us; refuses spent/tombstoned.
- `revive(tx, vout)` — lift a tombstone after first-party verification.
- `burn(txid, vout)` — tombstone a single outpoint (observed on-chain spend
  or first-party 'spent' answer during selection via `spendCheck`).
- `hold`/`release` — named-outpoint reservations (fee vault, imports).
- `takeOverride` — a purpose's take served from a dedicated pool
  (`NEKTAR_FEE_VAULT` mints capped `publish` stock through the covenant).

## Hint discipline

Broadcaster status words are **advisory hints, never verdicts**:

- `hintSkip` may pass a candidate over for the current take only — it writes
  nothing.
- A proof-carrying input is never status-queried (`hintSeen`/`hintRejected`/
  `hintSilent` predicates in txfacts.ts).
- `spendCheck` is the first-party probe: a 'spent' answer burns permanently.
- Durable money decisions (`spent`/`died`/`adopt`) read only `facts.verdict`;
  `facts.hint` may wake reconcilers early (`market.ts` uses `hintSeen`
  before declaring an observable pending tx uncertain).

## Startup adapters (`packages/chain/src/port.ts`)

`ChainPort` is the one chain-side contract the runtime assembles against —
`memoryPort` (test tier), `regtestPort` (local Teranode stack + `mine`),
`livePort` (main/testnet: `BsvBroadcaster` + witnessed headers + SSE hint
stream). Every broadcaster construction sits inside `SpendGuard`; `port.ts`
is the allowlisted composition root, and `openRuntime` converges on this
shape as the remaining startup paths migrate onto it.

## The two deal state tables

Deal state is durable in the automation store (`packages/automation/src/store.ts`) — memory-tier runs use the same schema in-process, so behavior is identical under restart:

- **`ops`** — one row per deal (`role` buyer|seller, `kind` purchase|sale, monotonically `revision`-guarded `state`, JSON `data`, `lastError`). Every transition lands in `events` (`seq`, `from`, `to`, `cause`, `evidence`) — the deal journal is replayable, and optimistic-concurrency on `revision` is what keeps two pumps from double-settling. Buyer states run `collecting → quoted/selected → funding → escrow… → disclosure_verified → useful|not_useful`; seller states run `enabled → quoted → escrow… → completing → completed`, with `refunded`, `disputed`, `uncertain` and `escalated` as the failure vocabulary (`uncertain` means a broadcast may have escaped — only a person may close it).
- **`outbox`** — one row per pending delivery action (`operation_id`, `kind`, `destination`, `payload`, `attempts`, `next_at`, `last_error`, `defer_class`, `status` pending|acked|dead). Retries back off exponentially; `defer_class` is the machine-readable wait cause (`funding-slot` = starved for a funding out — does not burn attempts; `transport` = counterparty/host unreachable). `unblockOutbox(class)` pulls a class forward when its cause clears, so wakes stay event-driven. `store.waitingOn(operationId)` is what `deals {operationId}` renders as its "waiting on" line.

## The schedule

Wake sources are events first, intervals only as ceilings:

- The chain observer (`ChainEvents` in the mcp runtime) is the single chain-side loop — height advances and pushed broadcaster statuses emit `tick`/`height`/`broadcasterStatus`, which fan out to the transaction watcher and the confirmer. The confirmer's `setInterval` is a missed-wake backstop, not the driver.
- The automation pump is a hot `setTimeout` chain while deals are live (wakes and inbound items keep it hot) falling back to `intervalMs` when silent, plus a slower watch-refresh pass — converging on one named scheduler with named jobs (`deals`, `outbox`, `upkeep`, `watches`, `watchdog`), each with its wake sources and a bounded ceiling rather than independent timers competing for the wallet.

## Custody modes

`NEKTAR_WALLET=toolbox` (regtest default) — embedded wallet-toolbox; the
wallet owns the funding-UTXO lifecycle and `reserve.json` only seeds it at
boot. `NEKTAR_WALLET=client` — external BRC-100 substrate, zero private keys
in app space. `legacy`/`ttn` are removed; `NEKTAR_NETWORK` is
`regtest|testnet|main`, and mainnet requires
`NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND`.
