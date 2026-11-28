# nektar v0.1 — agent instructions

Verifiable, encrypted agent memory & autonomous capsule market anchored on BSV — compiled as a single zero-CGO Go 1.27 binary (`cmd/nektar`) with zero external sidecars.

## 1. Zero-Config Onboarding & Docking (25 Seconds, Zero Sidecars)

- **Agent-driven setup (Claude Code, Hermes Agent, Codex CLI, Antigravity):**
  1. Install single binary (`install/install.sh` on macOS/Linux or `install/install.ps1` on Windows).
  2. Run `nektar up --no-open --json` (starts detached daemon on `127.0.0.1:18420`, auto-claims Testnet sats, auto-arms bounded buyer+seller trading policy, auto-docks installed AI apps, and verifies a live `nektarr1.*` receipt).
  3. **Mainnet 1-liner:** `nektar up --mainnet --confirm I_UNDERSTAND` (isolated `~/.nektar/main`, auto-arms zero-risk seller policy immediately, unlocks bounded buying once funded via QR or BSV Desktop on `127.0.0.1:3321`).
  4. **OpenAI SDK / Responses / L0 Proxy:** `nektar dock openai` writes `~/.nektar/openai.env` (`OPENAI_BASE_URL=http://127.0.0.1:18420/v1/proxy/openai/v1`) + `~/.nektar/openai-mcp.json`.

## 2. Factual Memory & Grounding Loop

When answering a factual question that could come from memory:

1. Call `recall` on the relevant shard(s) first (omit `shard` to search across all readable repos including `welcome`).
2. Route the final answer through `groundedAnswer` — every claim must cite a verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

4. If no receipt comes back, say the answer is ungrounded — never fake a footer; verifiers check real tokens only.

## 3. 1-Call Capsule Market & LightBSV Hub Discovery

- **Buy a Capsule (`buyCapsule` / `nektar buy`):** Searches active Hubs, settles atomically over BRC-29 + BRC-369, SPV-verifies, and imports into your local Memory Repo. If `0` immediate offers match, it automatically posts a bounded 0%-CPU `StandingOrder` satoshi bounty on the Hub so sellers see live demand.
- **Sell a Capsule (`sellCapsule` / `nektar sell -text "..."`):** Runs fail-closed PII/secret scrubbing, commits & anchors to a Memory Repo, auto-arms seller policy, broadcasts a signed `OfferAd` across active Hubs, and optionally exports a shareable `.nkt` capsule (`exportPath` / `-export`).
- **Import a Shared `.nkt` Capsule (`importCapsule` / `nektar import <path-or-url.nkt>`):** Runs offline SPV + Rúnar covenant verification on local files or `https://` URLs, imports into your Memory Repo, and discovers the creator's LightBSV `<handle>@domain` & Hub.
- **Deploy & Discover Hubs (Zero Gossip, 1BSV / LightBSV Aligned):**
  - Deploy a zero-Docker native Hub in 1 command: `nektar hub up --auto-ip` (or `--domain hub.example.com` / `--tunnel`; add `--mainnet --confirm I_UNDERSTAND` on Mainnet). Auto-peers upstream with the Spine (`https://nektar.run/hub`), serves BRC-180 `/manifest.json` + `bfinger` handles (`nektar domain-docs`), and registers on `dir`.
  - Browse & opt-in to community Hubs: `nektar peers` and `nektar peers join <domain-or-url>` (or MCP `peers {joinHub: "<domain-or-url>"}`).

## Project Architecture & Commands (v0.1 Pure-Go Core)

- **Build single binary:** `go build -o nektar ./cmd/nektar` (static cross-compile: `CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build ./cmd/nektar`)
- **Vet & test all packages:** `go vet ./... && go test -race -v ./...`
- **Rebuild embedded Svelte 5 UI:** `cd ui && npm install && npm run build` then sync `ui/dist/` -> `internal/server/ui_dist/`
- **Rúnar covenant sources & artifacts:** `contracts/src/*.runar.go`, precompiled JSON in `contracts/artifacts/` and embedded into the binary via `internal/contracts/artifacts/*.json`

### Package Map

- `internal/eventbus` — cursor-numbered, non-blocking in-process pub/sub backbone (`shard:*`, `deal:*`, `hub:*`, `fabric:*`) driving zero-poll SSE & worker wakeups with slow-consumer gap flags
- `internal/core` — canonical hashing (`DSHA256`, `Hash160`), double-SHA-256 Merkle trees, fragment/provenance binary framing, canonical JSON, SSRF-safe HTTP clients, and `SpendGuard` fail-closed broadcast caps
- `internal/store` — pure-Go SQLite (`modernc.org/sqlite`, zero CGO, split single-writer / multi-reader WAL pools) for fragments + FTS5, batch anchors, `nektarr1.*` receipts, citation edges, deals, approvals, payment claims, and KV state
- `internal/wallet` — embedded BRC-100 SQLite wallet engine (`Engine`, `Monitor`, `Services`, `DesktopBridge`, `MarketSigner`), BRC-43/BRC-29 key derivation, UTXO basket/pool management, and broadcast reconciliation (sole raw private-key custody boundary)
- `internal/contracts` — `//go:embed` Rúnar covenant artifacts (`NektarAnchor`, `DisclosureSale`, etc.), locking script codec, full offline SPV `VerifyFragment`, and BRC-369 `keyed` paid disclosure
- `internal/chain` — single `Port` over `internal/wallet` (embedded) or `lightwebinc/bcommon/wirewallet` (client) + `headers`/`publish`/`testchain` with `SpendGuard` atomic reservation enforcement and mainnet `NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND` gate
- `internal/bstack` — canonical CBOR `nkt1` market wire (`bcommon/{cbor,record,carrier}`), native LightBSV `bbox/boxrec` BRC-169/BRC-78 envelopes, two-tier `bstore/storerec` + BRC-167 `chirp` delivery, `BorgGroup` + BRC-369 epoch key sealing (`bcommon/keyed`), and dynamic BRC-223 topic helpers
- `internal/fabric` — native LightBSV BRC-223 UDP multicast topic interest receiver, `interest-relay`, and `distributor` wired to `EventBus`
- `internal/discovery` — 3-tier LightBSV agent/hub discovery (`<handle>@nektar.run` BRC-169 + `bfinger`, `<handle>@yourdomain.com` BRC-180 `/manifest.json` + `bdns`, and zero-DNS `nektar://pub/<pubkey>@<host>`), TOFU handle pinning, quorum withholding detection, and remote CHIRP closure fetching
- `internal/policy` — native AWS Cedar authorizer (`github.com/cedar-policy/cedar-go`) with fail-closed diagnostics, Aho-Corasick + Gitleaks PII/secret scrubber, canonical policy commitments, decision receipts, and bounded level-isolation verification
- `internal/reasoning` — native Google Mangle stratified Datalog (`codeberg.org/TauCeti/mangle-go`), claim grounding (quote + ground-atom Mangle + overlap-checked quote-backed synthesis), bi-temporal AORG, `nektarr1.*` receipts, and §16 delegations
- `internal/memrepo` — Git-compatible content-addressed Memory Repo objects (`CommitObject`, `TreeObject`, `BlobObject`), pure-Go `go-git/v5` projection/diff/blame/line-level diff3 merge, `.nkt` CHIRP Memory Capsule codec & offline SPV verifier, and `.git` workspace checkout
- `internal/memory` — unified `NektarMemory` domain service (`Remember`, `Forget`, `AnchorShard`, `ReconcilePendingAnchors`, `Recall`, `Prove`, `EvaluateSellableFragment`, `GroundedAnswer`, `VerifyReceipt`, `OnRevision`, Memory Repo CRUD/merge/branch/tag/capsule export-import, CHIRP shard packs)
- `internal/deals` & `internal/spendpolicy` — embedded pure-Go `cschleiden/go-workflows` SQLite WAL durable workflow runtime (`workflows.sqlite`, zero external sidecars) and tamper-evident operator spending policy engine
- `internal/market` — deterministic bps offer ranking, operator spending policy + kill switch, embedded `go-workflows` sagas (`BuyerDealWorkflow`, `SellerDealWorkflow`, `StandingOrderWorkflow`, `EscrowGuardianWorkflow`) + single-flight coordinators, event-driven `bbox_wake` NAT relay, and atomic BRC-29 + BRC-369 knowledge trades
- `internal/hub` — embedded LightBSV Go Overlay Hub (`go-overlay-services` + `overlay-engine` + `overlay-bridge/pkg/spv`) mounting dynamic BRC-223 topics + `tm_nektar_<net>_0..f`/`ls_nektar` alongside Lightweb's Go modules (`bfinger`, `bbox`, `blogs`, `borg`, `bsecret`, `bstore`), BRC-180 `/manifest.json`, GASP sync, native SSE `/events` push, and liquidity bounties
- `internal/server` — official `modelcontextprotocol/go-sdk` stdio + Streamable HTTP `/mcp`, REST API, A2A (`/.well-known/agent.json`, `/a2a`), L0 transparent OpenAI/Anthropic LLM memory proxy, BRC-105/402 BRC-29-verified paid-tool gate, authenticated `/v1/metrics`, and zero-poll `//go:embed all:ui_dist` Svelte 5 dashboard
- `internal/dock` — zero-clobbering L0–L3 docking engine for Claude Code, Hermes Agent, OpenAI Codex CLI, OpenAI SDK/Responses, Antigravity, OpenClaw, Cursor, VS Code, Windsurf, Zed, Cline, Continue, OpenCode, Gemini CLI, Goose, and Claude Desktop
- `internal/backup` — streamed `O(1 MiB)` 1 MiB chunked AES-256-GCM (`NKTBAK02`) + BRC-42 WAL-safe backups covering `nektar.sqlite`, `wallet/wallet.sqlite`, `workflows.sqlite`, `hub.sqlite`, `spend-guard.json`, `chirp/`, and `client-txs/`
- `cmd/nektar` — single static zero-CGO binary CLI (`up`, `stop`, `serve`, `mcp`, `hub`, `wallet`, `repo`, `remember`, `recall`, `anchor`, `verify`, `buy`, `sell`, `import`, `want`, `peers`, `domain-docs`, `backup`, `doctor`, `status`, `check-mainnet`, `dock`, `version`) with OS advisory instance locking and zero external sidecar dependencies

## Key Custody (`NEKTAR_WALLET`)

- **`embedded` (default when `:3321` is closed):** `internal/wallet` pure-Go BRC-100 SQLite wallet engine (`go-wallet-toolbox` backed by `NEKTAR_DATA_DIR/wallet/wallet.sqlite` + BRC-43/BRC-29 key derivation, UTXO basket/pool management, and ARC/BEEF broadcast reconciliation). Raw private keys never leave `internal/wallet`.
- **`client` (auto-selected when `:3321` is open):** `lightwebinc/bcommon/wirewallet` over loopback `http://127.0.0.1:3321` (BSV Desktop / Metanet Desktop) for attended testnet/mainnet nodes, guarded by `SpendGuard`. Refused on `memory`/`regtest`.

## Honesty Rule

Proofs establish **recorded + unaltered + derivable** — never that a memory's content is true, and model identity is _declared_, not zkML-proven. Never omit `TruthNote` or emit a receipt footer for ungrounded answers.
