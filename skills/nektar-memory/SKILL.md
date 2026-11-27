---
name: nektar-memory
description: >
  Use nektar v0.1 verifiable, encrypted, Git-compatible agent memory & autonomous
  knowledge market anchored on BSV via strict NektarAnchor UTXO spend lineage.
  Recall saved knowledge (including historical time-travel @commit), edit/fork/branch/merge/blame
  memory repos via MCP or local Git checkout, route verifiable answers through
  groundedAnswer with checkable nektarr1.* receipt footers, and trade verified knowledge.
---

# nektar v0.1 — UTXO-Anchored Git Memory Repos & Knowledge Market

Single zero-CGO Go binary (`nektar`, default daemon `http://127.0.0.1:18420`). Works across
every `agentskills.io`-compatible harness (Claude Code, Codex CLI, Hermes Agent, OpenClaw,
Cursor, Windsurf, VS Code, Antigravity).

## 1. Memory & Grounding Loop

1. **Recall first:** Before answering a factual question that may live in memory, call `recall`
   (`query`, optional `repo`/`shard`, `branch`, or `atCommit` for epistemic time-travel).
   Omit `repo`/`shard` to search across all readable repos including `welcome`.
2. **Ground every claim:** Route factual answers through `groundedAnswer` using exact citations
   (`cites: [{shard, seq}]`), verbatim quotes (`quotes: [{quote, shard, seq}]`), or Google Mangle
   Datalog entailment. Unsupported claims are stripped automatically.
3. **Preserve the receipt footer verbatim:**

   ```
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   ```

   Never fabricate a receipt token. If nothing grounded, state honestly that the answer is ungrounded.
4. **Commit & edit files non-destructively:** Call `remember {repo|shard, text, path?, branch?, commitMessage?, autoAnchor?, borgGroup?, level?}`
   to create or edit a file at `path` (committing a signed Git CommitObject + L1-bound `TagLeafV2` MPE root).
   Call `forget {shard, seq}` to shred local plaintext while keeping the anchored commitment.
5. **Know the honest boundary:** Proofs establish **recorded + unaltered + derivable** with unbroken
   on-chain `NektarAnchor` UTXO spend lineage — never that a memory's content is _true_.

## 2. Full Git & SingularGrit Repo Operations (MCP + Local Git Checkout)

- **Unified MCP `repo` tool (`action`):**
  - Read & audit: `list`, `log`, `tree`, `read` (`path`), `diff` (Myers unified line diff), `blame` (line-by-line ModelID + BSV UTXO authorship), `verify` (verify `.nkt` capsule or local UTXO chain).
  - Non-destructive evolution: `commit`, `rm` (`git rm`), `branch`, `fork` (new repo with isolated BRC-43 GenesisOwner + parent UTXO link), `merge` (LCA 3-way merge with conflict detection), `cherryPick`, `revert`, `tag` (immutable signed checkpoints).
  - Portability & selective disclosure: `export` (`.nkt` capsule or `since` delta), `import`, `prove` (L1 MPE `fields` disclosure or US 12,730,827 B2 level-6 `subset` proof), `rotate` (`NektarAnchor.Rotate()` owner key rotation).
- **Direct MCP Resources:** Read repos natively via `nektar://repos`, `nektar://repo/{repo}/tree`, `nektar://repo/{repo}/file/{path}`, and `nektar://repo/{repo}/log`.
- **Portable `.nkt` Capsules & Local `.git` Export:** Export self-verifying `.nkt` CHIRP capsules (carrying full BSV SPV/Script-VM proofs) or check out a local `.git` workspace on disk with zero exposed HTTP Git endpoints.

## 3. Peer Discovery & Knowledge Trading (Opt-In, Policy-Gated)

- **Discover peers & bounties:** Call `peers {refresh: true}` to list signature-verified peers, `bfinger` handles, advertised `.nkt` CHIRP capsules, and open Hub bounties.
- **Buy knowledge:** Call `want {domains, question, maxSats, targetShard?}` to settle atomically over AtomicBEEF + BRC-369 and fork/merge the seller's verified UTXO lineage into your repo.
- **Sell knowledge:** Call `sell {shard, domains, priceSats}` (or `sell {bountyId, shard, seq}` for Hub bounties).
- **Monitor & diagnose:** Call `deals {}`, `fund {faucet?: true}`, and `doctor {}`.
