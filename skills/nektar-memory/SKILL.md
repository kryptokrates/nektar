---
name: nektar-memory
description: >
  Use nektar v0.1 verifiable, encrypted, Git-compatible agent memory & autonomous
  capsule market anchored on BSV. Recall saved knowledge, ground factual answers
  with checkable nektarr1.* receipt footers, buy/sell/import SPV-verified .nkt
  memory capsules in 1 call, and discover LightBSV Hubs & handles.
---

# nektar v0.1 — Shared Brain, Verified Receipts & 1-Call `.nkt` Capsule Market

Single zero-CGO Go binary (`nektar`, default daemon `http://127.0.0.1:18420`, zero external sidecars).
Shared lock-free across Claude Code, Hermes Agent, Codex CLI, OpenAI SDKs, Antigravity, OpenClaw, Cursor, Windsurf, and VS Code.

## 1. Memory & Grounding Loop

1. **Recall first:** Before answering a factual question that may live in memory, call `recall`
   (`query`, optional `repo`/`shard`, `branch`, or `atCommit` for time-travel). Omit `shard` to search all readable repos including `welcome`.
2. **Ground every claim:** Route factual answers through `groundedAnswer` using citations (`cites: [{shard, seq}]`), verbatim `quotes`, or Google Mangle Datalog entailment. Unsupported claims are stripped automatically.
3. **Preserve the receipt footer verbatim:**

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

   Never fabricate a receipt token. If nothing grounded, state honestly that the answer is ungrounded.
4. **Save durable notes:** Call `remember {shard, text, path?, commitMessage?, public?}` (private CLKA-encrypted by default unless `public: true`). Call `forget {shard, seq}` to shred local plaintext while preserving Merkle lineage.
5. **Honest boundary:** Proofs establish **recorded + unaltered + derivable** with unbroken `NektarAnchor` UTXO lineage — never that a memory's content is objectively _true_.

## 2. 1-Call Capsule Market (Buy, Sell & Share `.nkt` Capsules)

Starter Atomic Network domains: `ops`, `debugging`, `mcp`, `api-quirks`, `bsv`.

- **Buy a Capsule (`buyCapsule`):**
  ```json
  {"question": "Go 1.27 SQLite WAL tuning runbook", "domains": ["ops"], "maxSats": 100}
  ```
  Searches active Hubs, settles atomically over BRC-29 + BRC-369 (including zero-port NAT `bbox`), SPV-verifies, and imports into your local Memory Repo. **Zero-State Bounty:** if 0 offers match immediately, `buyCapsule` automatically posts a bounded 0%-CPU `StandingOrder` satoshi bounty on the Hub (`mode: "bounty_posted"`) that auto-fills as soon as a seller answers.
- **Sell a Capsule (`sellCapsule`):**
  ```json
  {"text": "Verified fix: ...", "domains": ["debugging", "ops"], "priceSats": 50, "exportPath": "./fix.nkt"}
  ```
  Runs fail-closed PII/secret scrubbing, commits & anchors to a Memory Repo, auto-arms seller policy, broadcasts a signed `OfferAd` to active Hubs, and optionally exports a portable `.nkt` capsule stamped with your `<handle>` for GitHub/PRs/X.
- **Import a Shared `.nkt` Capsule (`importCapsule`):**
  ```json
  {"source": "https://example.com/capsules/fix.nkt", "joinCreatorHub": false}
  ```
  Offline SPV-verifies any local `.nkt` file, `https://` URL, or `chirp://` URI, merges into your local Memory Repo, and discovers the creator's LightBSV handle & Hub.

## 3. LightBSV Hub Discovery & Git Repo Power Tools

- **Discover & Join Hubs (`peers`):** Call `peers {refresh: true}` to inspect connected & community LightBSV Hubs (BRC-180 `/manifest.json`), `bfinger` handles, and open satoshi bounties. Call `peers {joinHub: "https://hub.example.com"}` to opt in to a community Hub.
- **Deploy Your Own Zero-Docker Hub (CLI):** `nektar hub up --auto-ip` (or `--domain hub.example.com` / `--tunnel`; add `--mainnet --confirm I_UNDERSTAND` on Mainnet).
- **Git Memory Repo Operations (`repo`):** `action` supports `list`, `log`, `show`, `tree`, `read`, `diff`, `blame`, `verify`, `commit`, `rm`, `branch`, `fork`, `merge`, `cherryPick`, `revert`, `tag`, `export`, `import`, `prove`, and `rotate`.
- **Diagnostics & Funding:** Call `doctor {}`, `fund {faucet?: true}`, or `deals {}`.
