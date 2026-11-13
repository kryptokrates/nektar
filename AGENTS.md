# nektar — agent instructions

Verifiable, encrypted agent memory anchored on BSV. When answering a factual
question that could come from memory:

1. Call `recall` on the relevant shard(s) first.
2. Route the final answer through `groundedAnswer` — every claim must cite a
   verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

4. If no receipt comes back, say the answer is ungrounded — never fake a
   footer; verifiers check real tokens only.

## Project commands

- Build all: `pnpm -r build` (then `pnpm --filter @nektar/contracts compile:contract` for Rúnar artifacts — timestamp churn in *.json is normal, `script` field is what matters)
- Dependency checks: `pnpm check:deps` (every import must be declared in the right package.json) + `pnpm check:cruise` (dependency-cruiser — no cycles, no orphans, no undeclared imports; generated artifacts like `packages/contracts/Nektar*.json` and `stark/verifier.js` are intentionally unresolvable and allowlisted in `.dependency-cruiser.cjs`) + `pnpm check:knip` (dead files/deps — knip.json entries cover e2e/demo/spike/hook entrypoints) + `pnpm audit` (supply chain — tar/minimatch are overridden to patched floors in pnpm-workspace.yaml)
- Hygiene: `pnpm check:hygiene` = `check:lint` (oxlint — correctness rules fail; warnings advisory, `oxlint --fix-suggestions` clears most) + `check:lint:types` (oxlint `--type-aware` via oxlint-tsgolint — floating/misused promises, await-on-sync leftovers and dead casts fail in src; `require-await`/`no-unnecessary-*`/`no-deprecated` are warnings so `@deprecated` marks track migration cleanup without blocking) + `check:format` (prettier; `pnpm format` to apply) + `check:syncpack` (same dep version everywhere — `@bsv/sdk`-style pin drift is a real incident class) + deps/cruise/knip + `check:dupes` (jscpd — copy-paste leftovers; fails >5% duplicated lines, `.jscpd.json` scopes src+tools and skips tests/demos/generated) + `check:leftovers` (debt ratchet — counts `as any`, `@ts-ignore`, TODO markers, console.* and commented-out code per package against `tools/.leftover-baseline.json`; counts may only shrink, regenerate deliberately with `--update`) + `check:arch` (architecture ratchet — per-package LOC/fanout/instability + god-file thresholds + the cross-package edge matrix + custody/env/layering grep gates, all against `tools/.arch-baseline.json`; same `--update` contract. NOTE: depcruise cannot see `@nektar/*` edges — they resolve into dist/ — so check-arch reads import specifiers directly and is the authoritative layering gate) + `check:secrets` (gitleaks dir-scan of the working tree — binary if on PATH, else the pinned docker image; `.gitleaks.toml` suppresses hex-only crypto material + gitignored local keystores). `pnpm report:cold` ranks source files by last-commit age — the legacy-review inventory. CI hygiene job additionally gates shellcheck, actionlint, hadolint, zizmor (Actions security) and a full-history gitleaks scan.
- Layout notes: market automation lives in `@nektar/automation` (reaches the app only through `AutomationHost` in `src/host.ts`); shared object/chirp/pack storage in `@nektar/store`; `bstack/src/market-wire.ts` is the wire envelope (vs automation's `market.ts` decision engine). `NEKTAR_*` env reads go through `nektarEnv`/`setNektarEnv`/`nektarEnvNum`/`nektarEnvFlag` in `@nektar/core` (`src/env.ts`) — injected `env = process.env` params are the testable seam; raw `process.env.NEKTAR_*` outside env.ts fails check:arch. `packages/watcher` is the `.mjs` executable tier (bin runs `src/watcher.mjs` directly — oxlint + check:arch apply; no tsc/type-aware lint by design). Raw broadcaster construction (`new TeranodeBroadcaster`/`BsvBroadcaster`) is composition-root only (mcp runtime, chain regtest/testkit) — every funded spend sits behind `SpendGuard`; new construction sites fail check:arch.
- Tests: `pnpm --filter <pkg> test` (mcp suite runs `dist/test/*.test.js` — rebuild first)
- MCP + HTTP server: `NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=regtest node packages/mcp/dist/src/bin.js` (regtest is the default; needs `NEKTAR_REGTEST_WIF` + rpc pass from `.env` — `pnpm stack:up` brings the stack up)
- Demo + client wiring: `pnpm demo` (verified answer in <60s on the regtest stack), `pnpm setup:claude` / `pnpm setup:codex` (merge hooks + instructions, non-clobbering). Public shard packs: `GET /v1/pack/{shard}` + `/.well-known/nektar` (on by default; `NEKTAR_PACK_SHARE=0` opts out)
- Regtest e2e: `node packages/e2e/src/e2e-regtest.ts` and siblings (needs the local Teranode stack)
- Groth16 spike: `tools/groth16-spike/README.md` (Python 3.12 venv inside the zkscript checkout; Rust circuit builds inside Docker on Windows)

## Key custody (`NEKTAR_WALLET`)

- **`client`** — `WalletClient('auto')` over an external BRC-100 substrate (BSV/Metanet Desktop). The app holds **zero** private keys: fund, log, shard-owner and automaton signatures go through `createSignature`/`getPublicKey`; fragment encryption uses `createHmac`; `keys.json` is only _read_ for legacy shard owners + old records. `fundKey`/`fee.key.json`/`NEKTAR_FUND_WIF`/`NEKTAR_FUND_MNEMONIC` are unnecessary on this path.
- **`toolbox` (default)** — embedded `@bsv/wallet-toolbox` (`wallet.sqlite`); the keyDeriver root still needs `NEKTAR_FUND_WIF`/`NEKTAR_FUND_MNEMONIC` — the documented embedded-wallet exception. The wallet owns the funding-UTXO lifecycle (selection, spent marks, internalized change); `reserve.json` only seeds it once at boot. Signing goes through local BRC-42 derivation — toolbox's `createSignature` has no `hashToDirectlySign`, so remote signing would sign the wrong digest.
- **`legacy`** — `fee.key.json`/env WIF for the fee key + `keys.json` (scrypt passphrase `NEKTAR_KEY_PASSPHRASE`) for agent/log/shard keys + the file-backed `ReserveFunder`. Rollback during the toolbox rollout; the signing seam treats every key as `SignerLike`, so behavior is identical under any custody.
- Test-only treasuries (`testkit.ts`, `e2e`, `.state/ttn-wif.txt`, `agent-keys.json`) hold keys by design — they aren't app custody. Consumer-side helpers (`client-kit.ts`, `identity.signBinding`/`answerChallenge`, `reasoning.makeDelegation`, `graph.delegate`) take the _caller's_ key — custody there belongs to the caller.

## Regtest stack gotchas (learned the hard way)

- **Teranode JSON-RPC drops requests with no `id`** — treated as notifications:
  connection closes with _no_ response ("empty reply from server", nothing in
  the server log). Always include `"id":1` in diagnostic curl/nc calls; a
  "server is dead" symptom with silent drops is usually this.
- **`rpc_max_clients` defaults to 3** in the docker.m settings — keep-alive
  callers (bstack hosts, agent trackers, parallel test files) saturate it and
  new RPC calls are refused while the healthcheck stays green
  (`Max RPC clients exceeded` in `docker logs rpc`). Pinned to 128 in
  `compose/docker-teranode.yml` (`environment:` wins over env_file, so stale
  .env files are covered), `setup.sh`, and `.env.example`.
- **Docker Desktop port proxies go stale** after WSL/container restarts — a
  published port accepts then drops connections while the container is fine
  internally. `docker restart <service>` re-registers the binding.
- Container-local verification: `docker exec rpc` + `nc`/`wget` bypasses the
  Windows proxy entirely — always isolate host-proxy vs in-process before
  assuming a service is wedged.
- **bstack sandbox hosts race their db on daemon restart** — `depends_on` only
  applies at `compose up`, and the `nektar-bstack-regtest-*` containers ship
  with no restart policy, so a WSL/docker restart leaves them dead or
  crash-looping on a cold db. `restart: unless-stopped` is set in the local
  `compose.regtest.yaml` (and live via `docker update`) — the restart-retry
  loop is what beats the race. The sandbox clone is gitignored; reapply after
  a fresh checkout.
- **Kafka `txmeta` has ~60s retention** — txmeta records are written by the
  utxo-persister consuming live Kafka. Coinbases mined while the persister is
  still initializing (a `clean.sh --data-only` reset fast-seals hundreds of
  blocks in seconds) never land: the utxo set knows the outpoints but parent
  fetch returns `TX_MISSING_PARENT` forever — the funds exist on-chain and are
  unspendable, and every treasury built on them silently drains. Same ghost-
  parent failure after a reorg orphans the validator-side records. Symptom:
  `TX_MISSING_PARENT` on an outpoint that `listunspent`/utxo-status says is
  unspent → the metadata is gone, not the money. Recovery: there is none —
  reset the stack (`deploy/teranode-quickstart/clean.sh --data-only`) and
  refund treasuries, or harvest only the live-window coinbases (post-persister
  heights; binary-probe a 1-coinbase spend at increasing heights to find the
  lower boundary).
- **Subsidy collapses fast on this chain** — ~halving every ~150 blocks:
  early blocks pay ~5e9 sats, by ~block 400 it's ~1.5e8, by ~3000 it's ~37
  sats/block. Funding a treasury by mining is hopeless once the chain ages;
  keep `regtest-fund.mjs` output or swept agent leftovers (`sweep-agents.mjs`)
  topped up instead.
- **Long-running node evals die with `0xC0000409` on Windows/Node 24** — the
  libuv teardown assert kills processes holding sockets at exit. Agents are
  respawned by the scenario, but bench/harness evals aren't — split scans
  into phases writing checkpoint files, and use `connection: close` so no
  sockets linger.

Honesty rule: proofs establish recorded + unaltered + derivable — never
that a memory's content is true, and the model manifest is _declared_, not
zkML-proven.

# nektar receipts (snippet — merge into your project CLAUDE.md)

When answering a factual question that could come from memory:

1. Call `recall` on the relevant shard(s) first.
2. Route the final answer through `groundedAnswer` — every claim must cite a
   verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

4. If no receipt comes back (nothing grounded), say the answer is
   ungrounded — never fake a footer; the Stop hook verifies real tokens only.

The footer is the auditable part of the answer: anyone can hit the verify
URL (or scan the QR on that page) to confirm the answer was committed to
anchored, provenance-signed memory — not that it is _true_.
