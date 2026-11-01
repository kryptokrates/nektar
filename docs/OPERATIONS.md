# Operations guide (DOC-002)

## Prerequisites

- Node 24, pnpm 12 (`corepack enable`), Docker Desktop (Teranode stack, TLC, Cedar SymCC, Rust oracle, mermaid-cli).
- `pnpm install` (lockfile pinned; one `@bsv/sdk` 4.0.1 via `overrides`).

## Regtest stack

```sh
pnpm stack:up      # Teranode v0.16.0 quickstart (infra/teranode-quickstart)
pnpm stack:down
```

| Service | Port | Used for |
|---|---|---|
| propagation (HTTP) | 8833 | broadcasting txs (**not** 8084, which is internal gRPC) |
| asset API | 8090 (basic auth = RPC creds) | `/api/v1/block_legacy/:hash` for BUMPs |
| RPC | 9292 | headers (`getblockheader`), mining (`generatetoaddress`) |

Credentials: `infra/teranode-quickstart/.env` (`rpc_pass`) and `mamem/.env` (`MAMEM_WIF`). Both are gitignored and loaded automatically by the regtest e2e.

## Funding (treasury chain)

- One outpoint is persisted in `.state/treasury.json`. Each regtest run splits it into 120 fresh outputs; a per-run reserve (`.state/reserve-run.json`) starts empty.
- If the treasury is missing, it is bootstrapped from an early coinbase.
- The **funding key is separate from every owner key**: shards and automata have their own owner keys, and one funder pays fees.

## Gates (run in this order; each has a short, bounded runtime)

```sh
pnpm -r build                      # all packages (contracts recompile the Rúnar artifacts)
pnpm check:deps                    # every import is a declared dependency
pnpm -r test                       # unit · contract · integration · fuzz (≤10 s per test)
pnpm reproduce                     # vectors/core.json identical; pinned STARK program id verifies the committed real receipt
pnpm e2e                           # offline story + v2 + P10/P14/v4 + full-paper scenario (~11 s)
MAMEM_NETWORK=regtest pnpm e2e     # same on Teranode (~2 min; run 3× for sign-off)
pnpm formal                        # TLC model check of the AORG invariants (+ mutant caught)
pnpm cedar:symcc                   # SymCC + cvc5 proof of level isolation (+ mutant refuted)
pnpm smt:vectors > vectors/smt-rust.json   # regenerate the Rust SMT oracle vectors
node tools/check-mermaid.mjs docs/*.md README.md   # every diagram renders
pnpm bench                         # writes BENCHMARKS.md (measured)
docker build -t mamem:local .      # clean-checkout gate (install → build → reproduce)
```

## Running the MCP server

The MCP server now runs on the official SDK, with stdio and Streamable HTTP at `/mcp`. It has persistent `MAMEM_NETWORK=regtest` mode with a confirmer, and installs through Docker MCP Toolkit with `pnpm mcp:install` / `pnpm mcp:smoke`. Full guide: [INTEGRATION.md](INTEGRATION.md). Live check of the production runtime with a restart: `node packages/mcp/demo/regtest-runtime.mjs`.


```sh
node packages/mcp/dist/src/bin.js              # stdio; offline demo chain that mines per tx
node packages/mcp/demo/client.mjs              # scripted agent session
MAMEM_LLM_URL=http://localhost:11434/v1 MAMEM_LLM_MODEL=qwen2.5 node packages/mcp/demo/client.mjs   # model-driven
docker compose run --rm -i mamem-mcp           # containerised
```

Claude Desktop: copy `packages/mcp/demo/claude_desktop_config.example.json` into your Claude config and set `MAMEM_MODEL_ID` to the exact provider release id (AGT-011).

## HTTP API, metrics, alerts

```ts
import { createHttpApi, FileSecurityStore, MamemMemory } from '@mamem/mcp'
const mem = new MamemMemory(chain, funder, fundKey, {
  securityStore: new FileSecurityStore('.state/security.json'),   // replay protection survives restarts
  requireSignedProvenance: true, finalityDepth: 6, maxMergeDelayMs: 600_000,
  batchPolicy: { maxLeaves: 1024, maxAgeMs: 60_000, maxBytes: 4_000_000 }
})
createHttpApi(mem).listen(8080)
setInterval(() => mem.flushDue(), 5_000)                          // interval/byte/cost batch closing
```

| Route | Purpose |
|---|---|
| `GET /v1/health` | liveness |
| `GET /v1/openapi.json` | OpenAPI 3.1 |
| `GET /v1/metrics` | Prometheus: anchors, anchor failures, unconfirmed anchors, anchor lag, promise violations, proof latency, withheld reads by reason |
| `GET /v1/tools`, `POST /v1/tools/{name}` | same tools as MCP |
| `GET /v1/proof/{shard}/{seq}` | public proof bundle + result |
| `GET /v1/checkpoint/{shard}` | latest signed checkpoint + log pubkey |

Alert rules are in `ops/alerts.yml`. Supply-chain artefacts: `pnpm sbom` → `sbom.cdx.json`. Cross-implementation vectors: `pnpm smt:vectors`, `pnpm curve:vectors`.

## Troubleshooting

| Symptom | Cause | Fix |
|---|---|---|
| `TX_MISSING_PARENT` | propagation race in a burst | handled: identical re-broadcast with backoff; persistent → check that the parent was accepted |
| `no Merkle proof at height` | asset API indexes a just-mined block with lag | handled: bounded retry; the mined height is read from the block header, not the tracker |
| `reserve dry: no funding UTXO available` | treasury exhausted | re-run (the treasury re-splits) or delete `.state/treasury.json` to re-bootstrap |
| `script[serializedBytes] is not a function` | two `@bsv/sdk` versions in the graph | `pnpm why @bsv/sdk` must show one version; keep the `overrides` in `pnpm-workspace.yaml` |
| `SCRIPT_INVALID … OP_EQUALVERIFY` on a new Rúnar method | runar 0.4.6 miscompiles (see migration-log) | distinct `const`s instead of reassigned `let`; every method continues state |
| Verification `NotMined` for an older anchor | missing BUMP for that specific tx | attach each anchor's own BUMP (never a descendant's) |
| finality stuck at 1 confirmation on regtest | `getblockchaininfo.blocks` lags `generatetoaddress` | the tracker's `tipHeight` uses the mined header height |
| Long-running command | a bug, not slowness | stop, inspect, fix; never extend timeouts |

## Backups

- `.state/` (treasury, reserve) must be backed up per environment; it holds funded outpoints.
- Shard stores: `MemoryShardStore` is in-process. For persistence use the file/sqlite stores (`packages/log`) and back them up with the overlay sqlite files. The anchored roots let you detect any restore that lost or altered records.
- Revocations, the challenge registry and issued ZK membership queries persist via `FileSecurityStore` in the runtime (`MAMEM_DATA_DIR`). The in-process demo server keeps them in memory.
- ZK proving needs the `mamem-stark-build` image (`pnpm stark:image`, ~4.6 GB) and the cargo target volume (`pnpm stark:build`), or `MAMEM_STARK_PROVER` pointing at a native `mamem-stark` binary. Verification needs neither (WASM in Node).
