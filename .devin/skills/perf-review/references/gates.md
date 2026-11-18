# Fast verification — change → minimal gate routing

"Run everything" is the default that burns minutes. Route by what the
change _can_ break. Tests run from `dist/` — **build the package first**
(`pnpm --filter @nektar/X build` or `pnpm -r build`).

## Source changes

| You changed                                            | Minimal verify                                                                                              |
| ------------------------------------------------------ | ----------------------------------------------------------------------------------------------------------- |
| `packages/X/src/**` (no API change)                    | `pnpm --filter @nektar/X build && pnpm --filter @nektar/X test`                                             |
| `packages/X/src/**` exported API/type change           | build + test X **and its dependents** (`rg -l "@nektar/X" packages/*/package.json`)                         |
| New `NEKTAR_*` env read, broadcaster site, funds marks | `pnpm check:arch` — its grep gates own raw `process.env`, `new TeranodeBroadcaster`, `markSpent`/`addOwned` |
| New file, new module, new storage shape                | `pnpm check:arch && pnpm check:dupes`                                                                       |
| `package.json` / `pnpm-workspace.yaml` / dep version   | `pnpm check:deps && pnpm check:cruise && pnpm check:knip && pnpm audit`                                     |
| Rúnar `*.ts` contract source                           | `pnpm --filter @nektar/contracts compile:contract` — commit `script` bytes; `buildTimestamp` churn is noise |
| `.github/workflows/**`                                 | nothing local — CI runs actionlint/zizmor/hadolint; keep `pnpm check:format` honest                         |
| Markdown / docs / `.devin/**`                          | `pnpm check:format` (`.devin/skills/**` is lint/knip-exempt)                                                |
| Test-only change (`test/**`)                           | that package's test; `check:knip` if you added an entrypoint                                                |

## Test tiers — never pay for what you don't need

- **`memory` (default)** — in-process chain, no Docker. `pnpm --filter <pkg> test`.
- **`stack`** — real regtest node: `pnpm test:stack` (or `test:smoke` for
  the subset). Only when chain/broadcast/regtest code or ledger
  persistence is touched.
- **`testnet`** — real coins, mcp `market-testnet` only. Never unless asked.

## Op-level regression

`pnpm bench` → `BENCHMARKS.md` (needs the local Teranode stack up). The
right answer to "did my crypto/verify change get slower" — never eyeball
EC math.

## Before push / when in doubt

`pnpm check:hygiene` — the full suite (lint + type-aware lint + format +
syncpack + deps/cruise/knip + dupes + leftovers + arch + secrets).
Cheaper than a red CI round-trip, always.

## The one rule

If the minimal gate passes but the change _could_ have broken a neighbor,
run the neighbor's test — routing is about _knowing_ what you skipped,
not skipping.
