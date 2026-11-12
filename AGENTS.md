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
- Tests: `pnpm --filter <pkg> test` (mcp suite runs `dist/test/*.test.js` — rebuild first)
- MCP + HTTP server: `NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=mock node packages/mcp/dist/src/bin.js` (mock = offline; regtest needs `NEKTAR_REGTEST_WIF` + rpc pass from `.env`)
- Demo + client wiring: `pnpm demo` (verified answer in <60s, offline), `pnpm setup:claude` / `pnpm setup:codex` (merge hooks + instructions, non-clobbering). Public shard packs: `GET /v1/pack/{shard}` + `/.well-known/nektar` (on by default; `NEKTAR_PACK_SHARE=0` opts out)
- Regtest e2e: `node packages/e2e/src/e2e-regtest.ts` and siblings (needs the local Teranode stack)
- Groth16 spike: `tools/groth16-spike/README.md` (Python 3.12 venv inside the zkscript checkout; Rust circuit builds inside Docker on Windows)

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
