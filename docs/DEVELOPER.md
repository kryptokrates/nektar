# Developer guide (DOC-002)

## Hard rules

1. **Never hand-write Bitcoin Script.** Custom locking logic is a Rúnar contract in `packages/contracts/src/*.runar.ts`. SDK P2PKH is used only for fee funding.
2. **Use the file tools for every file read and edit**, never `sed`/`cat` or `node -e` rewrites. The shell is for running builds, tests and programs only.
3. **Short timeouts.** Each unit test ≤10 s; the offline e2e ≤60 s; the regtest e2e ~2 min. A long wait means a bug.
4. **Errors are never swallowed in tests.** Negative tests assert the specific reason.
5. **Production and tests use the same code path.** No `as any` reach-ins: add a public accessor instead.
6. **Every new cryptographic structure** needs vectors, negative tests and an entry in `PROOFS.md`.
7. **Every import is declared** (`pnpm check:deps`). New dependency versions must be ≥7 days old and pinned.

## Package map

| Package | Responsibility |
|---|---|
| `core` | canonical serialization, domain tags, double-SHA-256 Merkle, R' chaining, anchor codec |
| `chain` | Broadcaster/ChainTracker/Funder interfaces; `MockChain` (script-evaluating, reorgs, double-spend checks); Teranode adapters |
| `log` | append-only BatchLog, dedup by H(d‖p), signed inclusion promises, batch policy, stores |
| `contracts` | Rúnar `NektarAnchor`, `NektarAutomaton` (+ measured `NektarAutomatonZk`), artifacts, byte-authenticated decoders |
| `anchor` | `AnchorChain` (one head UTXO per shard, retry and adoption, BEEF normalization), `NektarProvider` |
| `overlay` | `tm_nektar` topic manager, `ls_nektar` lookup, GASP sync |
| `verify` | proof bundles, `verifyFragment` |
| `crypto` | per-fragment HKDF keys, AES-256-GCM, level keyspaces, shredding |
| `identity` | bindings, challenges, rotation certs, SMT (TS ≡ Rust), custody, disclosure, ZK clearance |
| `zk` | Wright §13 membership: L_ZKMem v2 codec, WASM STARK verifier (`stark/`), RISC Zero prover adapter, anchored verifier with issued single-use queryIds. Guest + host + WASM sources live in `tools/stark/` (`pnpm stark:image`, `pnpm stark:build`, `pnpm stark:vectors`) |
| `datalog` | L_pol parser, linter, stratified semi-naive engine with canonical provenance |
| `graph` | AORG, GraphAnchor, PolicyDAG (layers, conflicts, derivation proofs), instancing/delegation |
| `policy` | Cedar authorize, canonical policy sets, receipts |
| `automaton` | automaton definition, δ/F tables, `AutomatonChain`, light-mode trace verification |
| `mcp` | `NektarMemory` service, MCP tools, stdio server (`bin.js`), demo client |
| `reasoning` | grounding, model identity, receipts, citation index and correction loop, consensus time, audit bundles |
| `fuzz` | seeded fuzzing of every decoder and verifier |
| `e2e` | offline and regtest e2e, the full-paper scenario, benchmarks |

## How to…

- **Add a Rúnar contract method.** Declare state in `super()` order and keep "constants" as mutable state. Use distinct `const`s, never reassign a `let` from a private method. Every method must `addOutput` (a terminal method reads deploy-time state). Then run `pnpm --filter @nektar/contracts build`. Add tests that go through a chain class on `MockChain.verifyScripts = true` (the script-evaluating mock is the oracle) and include a regtest step.
- **Add an MCP tool.** Add the schema to `TOOLS` and a case in `callTool` that calls a **public** `NektarMemory` method. Add a test in `mcp/test` and, where it adds I/O, a stdio test.
- **Add a policy rule.** Write L_pol with an `@id`; run `lint()`; anchor the new DAG root and configure `anchoredPolicyRoot`; add a derivation test with `verifyDerivation`.
- **Add an automaton.** Write the `AutomatonDef` (≤16 transitions, deterministic, F non-empty), then call `AutomatonChain.deploy`; steps are δ-checked on-chain.
- **Add a topic manager or lookup service.** Follow `overlay/src/tm.ts`: decode with the byte-authenticated decoder and admit successors only.

## CI and releases

- `.github/workflows/ci.yml` runs on every push and PR. It builds, runs `check:deps`, all unit/contract/integration/fuzz tests, `reproduce` and the offline e2e, plus a Docker clean build with an in-image MCP session.
- `.github/workflows/release.yml` pushes `ghcr.io/kryptokrates/nektar`: `edge` on master; `<version>` and `latest` on `v*` tags. Each push is smoke-tested.
- These stay manual because they need local infrastructure: `NEKTAR_NETWORK=regtest pnpm e2e`, `pnpm e2e:mcp`, `pnpm formal`, `pnpm cedar:symcc`, `pnpm smt:vectors`, `pnpm curve:vectors`.
- To release: bump versions, then `git tag v0.x.y && git push --tags`.

## Test conventions

- `node:test` + `node:assert/strict`. Each package has `test/*.test.ts`, compiled to `dist/test`.
- Use the script-evaluating `MockChain` (`verifyScripts = true`) for anything that spends a contract.
- Close every worker and handle (`shutdownZk()`, `knex.destroy()`); never `process.exit` in tests.
- Fuzz with seeds: `NEKTAR_FUZZ_SEED`, `NEKTAR_FUZZ_N`.
