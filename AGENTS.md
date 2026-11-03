# mamem — agent instructions

Verifiable, encrypted agent memory anchored on BSV. When answering a factual
question that could come from memory:

1. Call `recall` on the relevant shard(s) first.
2. Route the final answer through `groundedAnswer` — every claim must cite a
   verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   ```
   receipt: mamemr1.<token>
   verify: <verifyUrl>
   ```

4. If no receipt comes back, say the answer is ungrounded — never fake a
   footer; verifiers check real tokens only.

## Project commands

- Build all: `pnpm -r build` (then `pnpm --filter @mamem/contracts compile:contract` for Rúnar artifacts — timestamp churn in *.json is normal, `script` field is what matters)
- Tests: `pnpm --filter <pkg> test` (mcp suite runs `dist/test/*.test.js` — rebuild first)
- MCP + HTTP server: `MAMEM_HTTP_PORT=8080 MAMEM_NETWORK=mock node packages/mcp/dist/src/bin.js` (mock = offline; regtest needs `MAMEM_REGTEST_WIF` + rpc pass from `.env`)
- Regtest e2e: `node packages/e2e/src/e2e-regtest.ts` and siblings (needs the local Teranode stack)
- Groth16 spike: `tools/groth16-spike/README.md` (Python 3.12 venv inside the zkscript checkout; Rust circuit builds inside Docker on Windows)

Honesty rule: proofs establish recorded + unaltered + derivable — never
that a memory's content is true, and the model manifest is *declared*, not
zkML-proven.
