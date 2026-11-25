# nektar v0.4 — agent instructions

Verifiable, encrypted agent memory anchored on BSV — compiled as a single zero-CGO Go 1.27 binary (`cmd/nektar`).

When answering a factual question that could come from memory:

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

## Project Architecture & Commands (v0.4 Pure-Go Core)

- **Build single binary:** `go build -o nektar ./cmd/nektar` (static cross-compile: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/nektar`)
- **Vet & test all packages:** `go vet ./... && go test -race -v ./...`
- **Rebuild embedded Svelte 5 UI:** `cd ui && npm install && npm run build` then sync `ui/dist/` -> `internal/server/ui_dist/`
- **Rúnar covenant sources & artifacts:** `contracts/src/*.runar.ts`, precompiled JSON in `contracts/artifacts/` and embedded into the binary via `internal/contracts/artifacts/*.json`

### Package Map

- `internal/core` — canonical hashing (`DSHA256`, `Hash160`), double-SHA-256 Merkle trees, fragment/provenance binary framing, canonical JSON, SSRF-safe HTTP clients, and `SpendGuard` fail-closed broadcast caps
- `internal/store` — pure-Go SQLite (`modernc.org/sqlite`, zero CGO) for fragments + FTS5, batch anchors, `nektarr1.*` receipts, citation edges, deals, approvals, payment claims, and KV state
- `internal/wallet` — embedded BRC-100 SQLite wallet engine (`Engine`, `Monitor`, `Services`, `DesktopBridge`), BRC-43/BRC-29 key derivation, UTXO basket/pool management, and broadcast reconciliation (sole raw private-key custody boundary)
- `internal/contracts` — `//go:embed` Rúnar covenant artifacts (`NektarAnchor`, `DisclosureSale`, etc.), locking script codec, full offline SPV `VerifyFragment`, and BRC-369 `keyed` paid disclosure
- `internal/chain` — single `Port` over `internal/wallet` (embedded) or `lightwebinc/bcommon/wirewallet` (client) + `headers`/`publish`/`testchain` with `SpendGuard` atomic reservation enforcement and mainnet `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` gate
- `internal/bstack` — canonical CBOR `nkt1` market wire (`bcommon/{cbor,record,carrier}`), `BorgGroup` + BRC-369 epoch key sealing/revocation rotation (`bcommon/keyed`), BRC-167 `chirp` object storage (`bcommon/chirp`), and Nektar's 16-lane Go overlay module (`bcommon/module`)
- `internal/discovery` — verified peer directory announcements, multi-host quorum withholding detection, bfinger handle resolution, and remote CHIRP closure fetching
- `internal/policy` — native AWS Cedar authorizer (`github.com/cedar-policy/cedar-go`) with fail-closed diagnostics, canonical policy commitments, decision receipts, and bounded level-isolation verification
- `internal/reasoning` — native Google Mangle stratified Datalog (`codeberg.org/TauCeti/mangle-go`), claim grounding (quote + ground-atom Mangle + overlap-checked quote-backed synthesis), bi-temporal AORG, `nektarr1.*` receipts, and §16 delegations
- `internal/memory` — unified `NektarMemory` domain service (`Remember`, `Forget`, `AnchorShard`, `ReconcilePendingAnchors`, `Recall`, `Prove`, `GroundedAnswer`, `VerifyReceipt`, `OnRevision`, CHIRP shard packs)
- `internal/market` — deterministic bps offer ranking, operator spending policy + kill switch, Restate Go SDK durable workflows (`BuyerDeal`, `SellerDeal`, `AnchorCadence`, `Custody`) as the sole live-network deal engine, and atomic BRC-29 + BRC-369 knowledge trades
- `internal/hub` — embedded Go Overlay Hub mounting `tm_nektar_<net>_0..f`/`ls_nektar` alongside Lightweb's Go modules (`bfinger`, `bbox`, `blogs`, `borg`, `bsecret`, `bstore`) with native SSE `/events` push and liquidity bounties
- `internal/server` — official `modelcontextprotocol/go-sdk` stdio + Streamable HTTP `/mcp`, REST API, A2A (`/.well-known/agent.json`, `/a2a`), L0 transparent OpenAI/Anthropic LLM memory proxy, BRC-105/402 BRC-29-verified paid-tool gate, `/v1/metrics`, and `//go:embed all:ui_dist` Svelte 5 dashboard
- `cmd/nektar` — single-binary CLI (`serve`, `mcp`, `hub`, `wallet`, `remember`, `recall`, `anchor`, `verify`, `status`, `check-mainnet`, `dock`, `version`) with OS advisory instance locking and managed `restate-server` sidecar

## Key Custody (`NEKTAR_WALLET`)

- **`embedded` (default):** `lightwebinc/bcommon/bwallet` BRC-100 embedded wallet (`identity.json` + atomic `wallet.json` pool under `NEKTAR_DATA_DIR/wallet`). Keys are derived internally under BRC-43; raw private keys never leave `bwallet`.
- **`client`:** `lightwebinc/bcommon/wirewallet` over loopback `http://127.0.0.1:3321` (BSV Desktop / Metanet Desktop) for attended testnet/mainnet nodes. Refused on `memory`/`regtest`.

## Honesty Rule

Proofs establish **recorded + unaltered + derivable** — never that a memory's content is true, and model identity is _declared_, not zkML-proven. Never omit `TruthNote` or emit a receipt footer for ungrounded answers.
