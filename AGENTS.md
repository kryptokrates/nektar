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
- Layout notes: market automation lives in `@nektar/automation` (reaches the app only through `AutomationHost` in `src/host.ts`); the memory domain (NektarMemory layer chain + the ops port interfaces it consumes) lives in `@nektar/memory` — mcp keeps the concrete *Ops impls and the `NektarMemory` composition in `src/memory.ts`; shared object/chirp/pack storage in `@nektar/store`; `bstack/src/market-wire.ts` is the wire envelope (vs automation's `market.ts` decision engine). `NEKTAR_*` env reads go through `nektarEnv`/`setNektarEnv`/`nektarEnvNum`/`nektarEnvFlag` in `@nektar/core` (`src/env.ts`) — injected `env = process.env` params are the testable seam; raw `process.env.NEKTAR_*` outside env.ts fails check:arch. Diagnostic log lines go through `mkLog()`/`dbg()`/`writeErr()` in `@nektar/core` (`src/log.ts`) — every line is ISO-8601-stamped at the sink, `now`/`write` are the test seam; raw `console.*`/`process.std(err|out).write` in `packages/*/src` fails check:arch (mcp `init.ts` is the allowlisted setup-CLI exception). `packages/watcher` is the `.mjs` executable tier (bin runs `src/watcher.mjs` directly — oxlint + check:arch apply; no tsc/type-aware lint by design). Raw broadcaster construction (`new TeranodeBroadcaster`/`BsvBroadcaster`) is composition-root only (`chain/src/port.ts`, regtest/testkit) — every funded spend sits behind `SpendGuard`; new construction sites fail check:arch. One startup path: `openRuntime(cfg, ports)` in `mcp/src/runtime.ts` assembles every network — chain answers come from a `ChainPort` (`chain/src/port.ts`: `memoryPort`/`regtestPort`/`livePort` — broadcaster, witnessed tracker, proof + raw-tx lookups, status-hint stream, height, network label), custody/funding from `mcp/src/funding.ts`, boot plumbing from `mcp/src/rtsupport.ts`. Header verification is two-source strict by default (`NEKTAR_STRICT_HEADERS=0` opts out): `HeaderUnavailableError` means "no verdict — retry", never a negative, so catches around proof verification must rethrow it (`isHeaderUnavailable`). Tests inject `{chain: memoryPort(chain), funds}`; there is no second assembly path (`openLiveRuntime` is gone).
- Tests: `pnpm --filter <pkg> test` (mcp suite runs `dist/test/*.test.js` — rebuild first). Three tiers via `NEKTAR_TEST_TIER`: `memory` (default — in-process chain, no Docker), `stack` (`pnpm test:stack` — the real regtest node; `pnpm test:smoke` is the stack-tier subset), `testnet` (mcp market-testnet only — spends real coins)
- MCP + HTTP server: `NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=regtest node packages/mcp/dist/src/bin.js` (regtest is the default; needs `NEKTAR_REGTEST_WIF` + rpc pass from `.env` — `pnpm stack:up` brings the stack up)
- Demo + client wiring: `pnpm demo` (verified answer in <60s on the regtest stack), `pnpm setup:claude` / `pnpm setup:codex` (merge hooks + instructions, non-clobbering). Public shard packs: `GET /v1/pack/{shard}` + `/.well-known/nektar` (on by default; `NEKTAR_PACK_SHARE=0` opts out)
- No-build release artifact: `pnpm pack:deploy` → `dist-deploy/` (self-contained host build — `pnpm deploy --prod --node-linker hoisted` gives a flat node_modules; sqlite3 is native so release.yml builds one per OS. Host runs are REQUIRED for `NEKTAR_WALLET=client` — the BRC-100 substrate binds loopback-only). The release workflow extracts the packed artifact to a clean dir and boot-smokes it: resolution must not escape into the workspace store
- Regtest e2e: `node packages/e2e/src/e2e-regtest.ts` and siblings (needs the local Teranode stack)
- Groth16 spike: `tools/groth16-spike/README.md` (Python 3.12 venv inside the zkscript checkout; Rust circuit builds inside Docker on Windows)
- Wait-site contract (event plane): every recurring wait names its producer signal — `scheduler.wake`/`dueIn`, `store.outboxReady`/`workReady`/`dueAtArmed`, `mem.onTx*`, `events.poke`/`note` — or is classified `probe-tier`/`floor-only` in `automation/test/wake-coverage.test.ts`; a raw `setInterval` outside the map fails CI. Ceilings stay as leak guards, never the mechanism.
- Durable executor (Restate-only — no local mode, no env switch): `automation/src/rt/` is the execution plane — `exec.ts` (the `Executor` send/call seam: `RestateExecutor` posts to the `NEKTAR_RESTATE_URL` ingress (default `http://127.0.0.1:8080`); `executor()`/`setExecutor` is the process seam; `inRtScope`/`rtScopeKey` the same-key-deadlock guard; `EXEC_SVC` the service names), `emit.ts` (`txEmit(exec, …)` routes every `emitTxEvent` — the executor arg is mandatory), `services.ts` (keyed service bodies wrapped in `ctx.run` — tx VO per actionId, custody serialized on `CUSTODY_KEY`), `endpoint.ts` (in-process HTTP/2 endpoint + deployment auto-registration — production always boots one), `dispatch.ts` (the test seam: `cfg.exec: new Dispatcher()` gives each in-process runtime its own executor — `isLocalDispatch` wires the same bodies onto it, no endpoint/port/server; `cfg.rtDefs` throws under it). Every `emitTxEvent` call site goes through `txEmit(exec, …)` with the runtime-scoped exec (`rt.exec`, `this.d.executor`, `deps.exec`, watcher `opts.exec`) preferred over ambient `executor()` — a new direct call bypasses the executor and breaks journal coverage; a bare `executor()` inside runtime code misroutes under an injected Dispatcher. Deal-object sends (`deal.advance`/`due`/`event`/`reconcile`) key by the want correlation id — `dealOpKey`/`dealEventKey`, `ev:<wantId>` (wantless ops fall back to their operationId) — and carry the operationId in the `{op}` arg; keying by operationId splits event/advance legs onto different objects and their concurrent writes lose to the op's revision CAS. The release artifact ships restate-server in `restate/` and `bin.ts` runs `ensureRestate` (`src/restatesidecar.ts`) before `openRuntime`: `NEKTAR_RESTATE_URL` wins → a live admin is adopted (:19070 then :9070) → the bundled server spawns (native on unix, `wsl.exe` on win32 — no win32 build exists; the endpoint URI is pinned to the WSL gateway so it can call back) → anything else degrades to memory-only. Cleanup is `pkill -f` inside WSL, not `child.kill()` — wsl.exe's death does not propagate to the linux process.

## Key custody (`NEKTAR_WALLET`)

- **`client`** — `WalletClient('auto')` over an external BRC-100 substrate (BSV/Metanet Desktop). The app holds **zero** private keys: fund, log, shard-owner and automaton signatures go through `createSignature`/`getPublicKey`; fragment encryption uses `createHmac`; `keys.json` is only _read_ for legacy shard owners + old records. `fundKey`/`fee.key.json`/`NEKTAR_FUND_WIF`/`NEKTAR_FUND_MNEMONIC` are unnecessary on this path.
  - **Boot order: unlock BSV Desktop before booting nektar.** `openCustody` probes `http://localhost:3321/getVersion` (`probeWalletSubstrate` in `rtsupport.ts`) before constructing `WalletClient`, because the SDK flattens every substrate failure into "Install a BSV wallet today!". Locked vault → `WALLET_LOCKED` → boot fails with "BSV Desktop is running but the vault is LOCKED"; nothing on :3321 → "no BRC-100 wallet substrate". Raw JSON-API calls need an `Origin` header or get HTTP 400.
  - **Derived, not generated**: reserve locks to `nektar-fund` (protocol `[2,'nektar']`, counterparty `self`) — same address every boot, per network. `nektar signer/identity` and `nektar-shard:<name>` derive the same way. Verified live: `getPublicKey`, `createSignature` w/ `hashToDirectlySign` (P2PKH + covenant), `createHmac`, `encrypt`/`decrypt`.
  - **Toolbox→client migration is manual and ordered** — `tools/custody.mjs` prints the inventory + checklist. `utxos` rows carry no lock — a single-owner book — so toolbox-locked rows stay `live` and `take()` will pick coins the wallet can't sign (spend fails at broadcast). Sweep the toolbox reserve to the derived address **while still on toolbox** (SqlFunds marks the spent rows), then flip `NEKTAR_WALLET=client`. `reserve.json` is the legacy tracker — its live entries do not self-migrate either.
  - Client custody is for attended testnet/main boxes — every signature stalls when the wallet closes. Regtest refuses client outright; headless stays toolbox.
- **`toolbox` (default)** — the keyDeriver root still needs `NEKTAR_FUND_WIF`/`NEKTAR_FUND_MNEMONIC` — the documented embedded-wallet exception. **SqlFunds owns the funding-UTXO lifecycle** (`utxos` table in records.sqlite — see below); `wallet.sqlite` is legacy storage only — a first-boot basket sweep migrates its spendable rows into `utxos` and renames the file `.migrated` (`NEKTAR_SKIP_BASKET_SWEEP=1` opts out). Signing goes through local BRC-42 derivation — toolbox's `createSignature` has no `hashToDirectlySign`, so remote signing would sign the wrong digest.
- Test-only treasuries (`testkit.ts`, `e2e`, `.state/testnet-wif.txt`, `agent-keys.json`) hold keys by design — they aren't app custody. Consumer-side helpers (`client-kit.ts`, `identity.signBinding`/`answerChallenge`, `reasoning.makeDelegation`, `graph.delegate`) take the _caller's_ key — custody there belongs to the caller.
- **Funds (`@nektar/chain` `funds.ts`) is the one coin book; `SqlFunds` (`automation/sqlfunds.ts`) is its store.** The `utxos` table in records.sqlite is the bookkeeping truth — rows are `(txid, vout, satoshis, state)`; tx bytes/proofs stay in `TxStore`. Both custody modes share it — `client`'s external substrate never sees the coin set (it cannot double-spend ours). `take`/`spent`/`exported`/`died`/`adopt`/`burn` cover the lifecycle; `fundsOf` adapts test funder shapes. Callers never mark outpoints themselves — check:arch fails on `markSpent`/`addOwned`/`takeReserve` outside the impls (`regtest.ts`, `memchain.ts`, `testkit.ts`, `automation/toolbox.ts`, `funds.ts`). Networks are `regtest|testnet|main` (ttn removed); `NEKTAR_WALLET` is `toolbox|client` (legacy removed — starting with it throws). The old basket-backed store (`automation/wallettfunds.ts`) is deleted — everything goes through SqlFunds only.

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

## Windows/MSYS toolchain gotchas (learned the hard way)

- **`rg -r <str>` is replace-mode, not recursive** — ripgrep is recursive by
  default; `-rn` makes every match print as the literal `n`. Use `rg -n`.
- **Git Bash mangles leading-slash args** — `docker run -w /repo` becomes
  `C:/Program Files/Git/repo` and the run fails with "working directory
  invalid". Double-slash defeats MSYS conversion: `-w //repo`, `-v //host`
  (or `MSYS_NO_PATHCONV=1`).
- **`netstat`/`ps`/service output is locale-dependent** — a German box
  prints `ABHÖREN`, not `LISTENING`. Grep for PIDs/ports/process names,
  never for English state words.
- **mcp tests run compiled output** — `pnpm --filter @nektar/mcp test`
  executes `dist/test/*.test.js`; a red/green result on a stale `dist` is
  fiction. Rebuild before reading results. Same for `dist/` instrumentation:
  patch compiled files only for diagnosis, never as the fix.
- **Root `tsc -b` floods ambient `@types/node` errors** unrelated to the
  edit — the only build/verify path is `pnpm -r build` (or
  `pnpm --filter <pkg> build` for a scoped check).
- **`assert.deepEqual(arr, [])` narrows `arr` to `never[]`** under strict TS
  — later `arr.push(...)` becomes a type error. Assert `arr.length === 0`.
- **`rg --glob` and brace quoting differ from Unix** — prefer the built-in
  search tools / `fd` for file ops; keep shell greps simple and quoted.
- **Bulk type-aware lint autofix is not semantics-safe** — `oxlint --fix` /
  `--fix-suggestions` and regex/AST codemods over `require-await` removed
  non-null assertions that `noUncheckedIndexedAccess` needs, stripped
  `async` from Promise-required signatures without wrapping returns, and
  garbled text on colliding edit offsets. The advisory warning classes
  (`require-await`, `no-unnecessary-*`, `no-deprecated`) are advisory by
  design — fix sites when already touching the file, never repo-wide
  without a per-package `pnpm -r build` between every batch.

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
