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
- Hygiene: `pnpm check:hygiene` = `check:lint` (oxlint — correctness rules fail; warnings advisory, `oxlint --fix-suggestions` clears most) + `check:lint:types` (oxlint `--type-aware` via oxlint-tsgolint — floating/misused promises, await-on-sync leftovers and dead casts fail in src; `require-await`/`no-unnecessary-*`/`no-deprecated` are warnings so `@deprecated` marks track migration cleanup without blocking) + `check:format` (prettier; `pnpm format` to apply) + `check:syncpack` (same dep version everywhere — `@bsv/sdk`-style pin drift is a real incident class) + deps/cruise/knip + `check:dupes` (jscpd — copy-paste leftovers; fails >5% duplicated lines, `.jscpd.json` scopes src+tools and skips tests/demos/generated) + `check:leftovers` (debt ratchet — counts `as any`, `@ts-ignore`, TODO markers, console.* and commented-out code per package against `tools/.leftover-baseline.json`; counts may only shrink, regenerate deliberately with `--update`) + `check:secrets` (gitleaks dir-scan of the working tree — binary if on PATH, else the pinned docker image; `.gitleaks.toml` suppresses hex-only crypto material + gitignored local keystores). `pnpm report:cold` ranks source files by last-commit age — the legacy-review inventory. CI hygiene job additionally gates shellcheck, actionlint, hadolint, zizmor (Actions security) and a full-history gitleaks scan.
- Tests: `pnpm --filter <pkg> test` (mcp suite runs `dist/test/*.test.js` — rebuild first)
- MCP + HTTP server: `NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=regtest node packages/mcp/dist/src/bin.js` (regtest is the default; needs `NEKTAR_REGTEST_WIF` + rpc pass from `.env` — `pnpm stack:up` brings the stack up)
- Demo + client wiring: `pnpm demo` (verified answer in <60s on the regtest stack), `pnpm setup:claude` / `pnpm setup:codex` (merge hooks + instructions, non-clobbering). Public shard packs: `GET /v1/pack/{shard}` + `/.well-known/nektar` (on by default; `NEKTAR_PACK_SHARE=0` opts out)
- Regtest e2e: `node packages/e2e/src/e2e-regtest.ts` and siblings (needs the local Teranode stack)
- Groth16 spike: `tools/groth16-spike/README.md` (Python 3.12 venv inside the zkscript checkout; Rust circuit builds inside Docker on Windows)

## Regtest stack gotchas (learned the hard way)

- **Teranode JSON-RPC drops requests with no `id`** — treated as notifications:
  connection closes with *no* response ("empty reply from server", nothing in
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
