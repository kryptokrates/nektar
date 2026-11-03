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

Credentials: `infra/teranode-quickstart/.env` (`rpc_pass`) and `mamem/.env` (`MAMEM_REGTEST_WIF`). Both are gitignored and loaded automatically by the regtest e2e.

Bootstrap the operator wallet with `node packages/mcp/dist/src/init.js` — it writes `MAMEM_REGTEST_WIF` to `.env` (never prints it) and shows the regtest address. `--network=regtest` additionally verifies reserve funding on the running stack and prints the exact `generatetoaddress` funding command if empty; `--force` rotates (old-key UTXOs do not follow — rotate only on a quiet chain); `MAMEM_ENV_FILE` overrides the target env file.

## Funding (treasury chain)

- One outpoint is persisted in `.state/treasury.json`. Each regtest run splits it into 120 fresh outputs; a per-run reserve (`.state/reserve-run.json`) starts empty.
- If the treasury is missing, it is bootstrapped from an early coinbase.
- The **funding key is separate from every owner key**: shards and automata have their own owner keys, and one funder pays fees.

### Tracked-set model (no indexer dependency)

Mamem deliberately does **not** scan the chain or query address indexes —
both scale with total network volume. Instead it only ever spends outpoints
it already knows: outputs **it created** (splits, change — recorded at
construction) or outputs **handed to it** (`MAMEM_FUND_TXIDS=<txid>` /
`hex:<rawtx>`, `init.js --fund-txid=<txid>`). This is the SPV model the BSV
Association's own SPV Wallet specifies: counterparties exchange transactions
directly (BEEF/BRC-62, BUMP/BRC-74), verifiable against block headers —
global address indexes are explicitly excluded for scale reasons.

Per-network primitives, all O(1) per known outpoint/txid:

| need | regtest (Teranode) | mainnet |
|---|---|---|
| tx mined? | `asset /merkle_proof/<txid>` (404 = pending) | BSV Broadcaster `GET /tx/<txid>` → `merklePath` (BRC-74 BUMP) |
| outpoint spent? | `asset /utxo/<txid>?vout=N` (binary record; 0x01 = spent + spending txid) — wired into `takeReserve` | our ledger: we're the only spender — a spend is spent iff *our* spend tx mined (broadcaster status) |
| funding | `init --fund-txid` → `getrawtransaction` → record outputs paying our key | `MAMEM_FUND_TXIDS=<txid>` or `hex:<rawtx>` (fully offline) |

`MAMEM_INDEXER_DISCOVERY=1` re-enables WoC address-index lookup as an opt-in
convenience — off by default. Broadcasting is the one unavoidable external
service (public BSV Broadcaster endpoints are no-key: `arcade-*-1.bsvblockchain.tech`,
`arcade.gorillapool.io`); running your own Teranode (e.g. a Hetzner box) makes
the regtest path *literally* the self-sovereign mainnet path — same endpoints,
same code.

## Gates (run in this order; each has a short, bounded runtime)

```sh
pnpm -r build                      # all packages (contracts recompile the Rúnar artifacts)
pnpm check:deps                    # every import is a declared dependency
pnpm -r test                       # unit · contract · integration · fuzz (≤10 s per test)
pnpm reproduce                     # vectors/core.json identical; pinned STARK program id verifies the committed real receipt
pnpm e2e                           # offline story + v2 + P10/P14/v4 + full-paper scenario (~11 s)
MAMEM_NETWORK=regtest pnpm e2e     # same on Teranode (~2 min; run 3× for sign-off)
node packages/e2e/dist/src/e2e-vault-regtest.js      # V1 FeeVault covenant on Teranode (fund→spend→continuation, over-cap refused)
node packages/e2e/dist/src/e2e-covenants-regtest.js  # V2 CheckpointBond + V3 DisclosureSale covenants on Teranode
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
| Record proof reports `settled:false` / `anchorState:'broadcast'` | anchor broadcast but no BUMP attached yet | normal at 0-conf — the record is *anchored* (locally verified); `settled` upgrades when the confirmer attaches the BUMP. A `NotMined`/`BEEFInvalid` result from the STARK-anchored verifiers still means a genuinely missing or bogus BUMP |
| finality stuck at 1 confirmation on regtest | `getblockchaininfo.blocks` lags `generatetoaddress` | the tracker's `tipHeight` uses the mined header height |
| Long-running command | a bug, not slowness | stop, inspect, fix; never extend timeouts |

## Backups

- `.state/` (treasury, reserve) must be backed up per environment; it holds funded outpoints.
- Shard stores: `MemoryShardStore` is in-process. For persistence use the file/sqlite stores (`packages/log`) and back them up with the overlay sqlite files. The anchored roots let you detect any restore that lost or altered records.
- Revocations, the challenge registry and issued ZK membership queries persist via `FileSecurityStore` in the runtime (`MAMEM_DATA_DIR`). The in-process demo server keeps them in memory.
- **Mainnet (MVP) runbook.** The operator pays all anchor fees from a capped fee key; readers and verifiers pay nothing.
  1. Start:
     ```sh
     MAMEM_NETWORK=main MAMEM_MAINNET_CONFIRM=I_UNDERSTAND \
     MAMEM_KEY_PASSPHRASE_FILE=/run/secrets/pass MAMEM_DATA_DIR=/data \
     node packages/mcp/dist/src/bin.js --http 8080
     ```
     Without the confirm flag, or without a passphrase, it refuses to start. Keys (`keys.json`, `fee.key.json`) are scrypt + AES-GCM encrypted, and plaintext keys are refused.
  2. Send a small float to the address printed on stderr (`GET /v1/wallet` shows it too). UTXOs are discovered via WhatsOnChain.
  3. Broadcasting goes to **BSV Broadcaster** (the ARC successor, formerly Arcade), GorillaPool instance `arcade.gorillapool.io`:
     - `POST /tx`; Extended Format when every parent is known, raw otherwise;
     - falls back to WhatsOnChain only on network/5xx errors;
     - fee rate `max(100 sat/kB, broadcaster policy)`;
     - headers and SPV from WhatsOnChain; BUMPs from the broadcaster, else the WoC TSC proof; finality depth 6;
     - a 200/202 means accepted. The anchor chain continues immediately, and the BUMP is attached in the background once the transaction is mined.
  4. Hard caps, enforced before every broadcast and fail-closed:
     - `MAMEM_MAX_FEE_SATS_PER_TX` (1000)
     - `MAMEM_MAX_SPEND_SATS_PER_DAY` (20000)
     - `MAMEM_MIN_BROADCAST_INTERVAL_MS` (10000)
  4b. **Consensus-enforced caps (GOAL-v7 V1, optional):** `MAMEM_FEE_VAULT`
     routes anchor funding through the `MamemFeeVault` covenant — caps are
     enforced by node script eval, not just the local SpendGuard. Value is
     inline JSON or `@/path/to.json`:
     `{"perTxCap":1000,"windowCap":20000,"windowBlocks":144,"maxFee":10,"fundSats":500,"vaultSats":50000}`.
     On first start it deploys the vault (one tx, `vaultSats` locked) from the
     reserve; each anchor then mints a `fundSats`-capped P2PKH funding out.
     The vault utxo + minted outs persist in `MAMEM_DATA_DIR/vault-utxo.json`
     (back it up with `.state/`). A covenant spend that violates per-tx /
     rolling-window / fee caps is refused by the network (TX_INVALID) —
     independent of local config. Reserve outs the vault did not mint are
     never used for anchoring while this is set.
  5. Measured at 100 sat/kB: genesis 1,143 B / 115 sats, append 3,315 B / 332 sats, rotate 2,904 B / 291 sats.
  6. Monitor `GET /v1/wallet` (balance, runway), `GET /v1/costs`, and the alerts `MamemFeeBalanceLow`, `MamemSpendCapHit` and `MamemBroadcastFailures`.
  7. Back up `MAMEM_DATA_DIR` (keys, state, reserve, spend ledger, records) **and** the passphrase. Losing either loses the shard ownership keys.
  8. Smoke without spending: `pnpm main:readonly`.
- **Paid disclosure (GOAL-v7 V3, opt-in, BIP-199 escrow):**
  `MAMEM_PAID_DISCLOSURE=1` exposes `saleOffer`/`saleOfferMemory`/
  `saleEscrow`/`saleComplete`/`saleRefund`/`saleClaim` (SDK tool list and
  HTTP `/v1/tools` both honour the gate; the dispatcher refuses the calls
  when unset). The escrow is **buyer-funded** — the seller can never
  take a deposit without revealing:
  1. `saleOffer` (seller): wraps a value to a buyer pubkey (BRC-42; omit
     `buyerPub` for public plaintext) and returns the covenant cfg +
     committed `wrapped` + `sellerPub`/`offerSig` — the seller's signed
     offer attestation (§16/17 delegation signature; the buyer verifies
     it before funding). `saleOfferMemory` is the memory-bound variant —
     plaintext records sell `serializeFragment` bytes (`mode:"fragment"`);
     **encrypted records sell the fragment key** (`mode:"key"` — the §7
     decryption capability + ciphertext + provenance). The returned
     `commitment {leaf, hdp, anchorTxid, authorPub, level, batchR, proof}`
     carries the inclusion evidence π_i.
  2. `saleEscrow` (buyer): pass `offer {sellerPub, offerSig, commitment}`
     to verify the signed offer (refuses MITM-swapped cfgs) and the
     leaf's inclusion path before funding; then completes the cfg with
     its own `depositorPkh` and funds
     `DisclosureSaleHost(cfg).lockingScript()` from the operator key.
     Deposit should be `price + maxFee + change margin`.
  3. `saleComplete` (seller): spends the escrow via `buy` — `wrapped` is
     revealed on-chain in the same tx that pays the seller exactly
     `price` and returns change to the depositor. The covenant pins the
     change destination and bounds the fee (`maxFee`), so the completer
     cannot reroute or burn the deposit.
  4. `saleClaim` (buyer): re-parses that buy tx, re-checks
     `sha256(wrapped)==hWrapped` and unwraps to the operator key. For
     `mode:"fragment"` it recomputes `computeLeaf(parseFragment(payload))`;
     for `mode:"key"` it decrypts with the sold key and recomputes
     `computeLeaf({d,p,hdp,E})`. Both verify the leaf against the
     commitment and check the provenance σ (`authorVerified`) when the
     record carries an author key + signature.
  5. `saleRefund` (buyer): if the seller never completes, the depositor
     reclaims the escrow after `refundAfter` — depositor-signed,
     locktime-gated.
  GOAL-v8 additions: offers carry `expiresAt` (inside the signature —
  expired offers are refused at verification AND before escrow funding),
  and `saleEscrow` refuses a `refundAfter` that isn't safely in the
  future (`> currentHeight + margin`) before any deposit is made.
  Threat note: `wrapped` is public on-chain — private mode relies on
  BRC-42 ECDH confidentiality; the covenant binds payment↔reveal but
  cannot prevent the buyer reselling the plaintext afterwards, nor a
  third party front-running a *public* payload they saw in the mempool.
- **Paid service (opt-in, BRC-166-style atomic settlement):**
  `MAMEM_PAYMENTS=1` + `MAMEM_OPERATOR_WIF` gates `anchor`,
  `groundedAnswer` and `commitReceipt` behind a per-call P2PKH payment —
  the x402 `exact` profile, no wallet ecosystem: the caller signs a whole
  raw tx and hands it over, we verify + broadcast. **Startup is
  fail-closed**: `openRuntime` refuses to boot with `MAMEM_PAYMENTS=1`
  unless `MAMEM_OPERATOR_WIF` is set and parses as a valid WIF — an
  invoice to a missing address can never settle. The WIF is used for its
  ADDRESS only (never signs, never leaves the box); startup logs
  `mamem: payments on — settling to <addr>`. Prices:
  `MAMEM_PRICE_ANCHOR_SATS` (default 100),
  `MAMEM_PRICE_GROUNDEDANSWER_SATS` / `MAMEM_PRICE_COMMITRECEIPT_SATS`
  (default 250), or `MAMEM_PRICE_SATS` for all.
  Flow: unpaid call → `402`-shaped `{paymentRequired}` invoice (also
  `GET /v1/pay/quote?tool=…` → real HTTP 402 + `PAYMENT-REQUIRED`
  header; `GET /.well-known/x402` → the priced-tool catalog for
  discovery). The caller's tx must (a) pay `lockingScriptHex`/`payTo` ≥
  amount and (b) carry `OP_FALSE OP_RETURN mamempay:<invoiceId>` — the
  tag binds the payment to THAT call, publicly auditable from the tx
  alone. Retry with `payment:{invoiceId,transaction}` → the tx is
  verified + broadcast and the **receipt commits the settlement txid** —
  the "signed usage receipt" names what paid for it; `/v1/verify` shows a
  `paid` check line (receipts minted before enabling payments will show
  unpaid → failed while the flag is on). One tx pays for exactly one
  call: consumed invoices and txids are refused (`<dataDir>/payments.json`).
  `payment.transaction` accepts raw hex **or BEEF** (AtomicBEEF too) —
  BEEF is the right choice: funding parents travel inline, which mock
  `verifyScripts` and Teranode Extended Format both want.
  `payclient.ts` (`buildPaymentTx`) is the reference client: payer WIF +
  spendable utxos in → signed BEEF hex out; any WIF-capable signer works
  the same way.
  Verified on real chains by `packages/e2e/src/e2e-pay.mjs`:
  `MAMEM_NETWORK=regtest node packages/e2e/src/e2e-pay.mjs` (local
  Teranode) and `MAMEM_NETWORK=ttn MAMEM_DATA_DIR=<funded dir>
  MAMEM_KEY_PASSPHRASE=… node packages/e2e/src/e2e-pay.mjs` (public
  testnet) — payer funding tx, 402 invoice, signed settlement broadcast
  by real consensus, receipt commits the txid, `paid` check green,
  replay rejected.
  `packages/e2e/src/e2e-pay-remote.mjs` drives a DEPLOYED server over
  the public wire (`MAMEM_REMOTE_URL` + `MAMEM_HTTP_TOKEN`): MCP
  remember → auto-anchor → 402 → locally signed BEEF settlement →
  public `/v1/verify` → tx visible via Arcade.
  Hosted demo: the Hetzner box (`167.233.174.91:8080`) runs
  `MAMEM_NETWORK=ttn` with a funded `fee.key.json` reserve (3M sats,
  `MAMEM_FUND_TXIDS=hex:<rawtx>` offline import — the tx wasn't indexed
  at the lookup endpoints yet) and `MAMEM_PAYMENTS=1`. Anchors are real
  public testnet txs; payments settle to the operator address for real.
  Mock network caveat: settlement is recorded by the mock chain, not
  consensus-validated — real payment semantics need regtest/ttn/main.
- **Access audit log (GOAL-v8 D6, opt-in):** `MAMEM_ACCESS_LOG=1`
  appends timestamped `{event:'decrypt'|'traverse', shard, seq, caller}`
  records to the `__access__` shard, which anchors like any other shard.
  **Off by default** — the log is plaintext and records who read what;
  enable only where that tradeoff is acceptable. `logAccess()` is the
  public entry for callers that traverse outside `memory` (graph walks).
- **Anchored identity ledger (GOAL-v8 D4):** every `rotate` now also
  writes the `RotationCert` to the `__identity__` shard and anchors it.
  `rotationChainProof(shard)` returns the genesis owner + per-cert
  inclusion evidence; `verifyAnchoredRotationChain` (reasoning)
  replays the chain for third parties — no state-dir trust needed.
- **Anchor authorship σ_n (GOAL-v8 D5):**
  `extractAnchorAuthorship(spendTx, spentAnchorScript)` (contracts)
  pulls `{ownerPub, sig, preimage}` out of an anchor spend and verifies
  `sig` over `sha256(preimage)` — the discrete Appendix-D artifact.
- **Derived-content level floor (GOAL-v8 D2):** `remember` accepts
  `premiseRefs` — the stored level is floored at `max(cited levels)`.
  `revise` inherits the base's level automatically.
- **Receipt model binding (GOAL-v8 D7):** configure
  `model:{id, params, weightsHash}` on the instance; receipts carry it
  (plus per-call `modelParams`) inside the signed digest.
- TTN (Teranode Test Net — test coins, `MAMEM_NETWORK=ttn`):
  - Endpoints: Arcade (ARC-compatible) `https://arcade-v2-ttn-us-1.bsvblockchain.tech`, WoC-TTN `https://api.woc-ttn.bsvblockchain.tech/v1/bsv/test`, optional ChainTracks for the tip. Same live code path as `main` (`openLiveRuntime`) — SpendGuard caps, encrypted keys, WoC merkle paths.
  - Differences from main: **no** `MAMEM_MAINNET_CONFIRM` needed; fee floor is 1 sat/kB (Arcade policy is 0 — we never send zero-fee txs); testnet address/WIF encoding (fee key prints as `m…`/`n…`); no broadcast fallback beyond WoC `/tx/raw`; confirm interval 30 s.
  - No faucet on TTN — coins come from the community/bsv.dev testcoin channel. The fee-key address is printed to stderr at startup.
  - Read-only smoke (no spend): `pnpm ttn:readonly` — Arcade policy, WoC tip, header root check, and a real merkle path for a recent coinbase verified with `merklePath.verify(txid, tracker)`.
  - Funded pilot: `MAMEM_DATA_DIR=.ttn-pilot MAMEM_KEY_PASSPHRASE=… MAMEM_NETWORK=ttn pnpm ttn:pilot` — remember→anchor→BUMP→prove→append→rotate→restart-no-fork, with explorer links and per-tx fees from `ops.costs()`.
  - Docker MCP profile: `pnpm mcp:install --ttn` registers `mamem_ttn`.
- ZK proving:
  - **The mamem Docker image ships the prover** (`/usr/local/bin/mamem-stark` + `r0vm`, `MAMEM_STARK_PROVER` preset). Its build stage fails if the rebuilt program id differs from the pinned one.
  - Outside Docker, use the `mamem-stark-build` image (`pnpm stark:image` + `pnpm stark:build`) or point `MAMEM_STARK_PROVER` at a native binary.
  - Verification needs none of this (WASM in Node).
