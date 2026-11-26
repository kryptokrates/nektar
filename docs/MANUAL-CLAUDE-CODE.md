# Nektar v0.1 for Claude Code — 25-Second Setup & Complete Manual

Connect Claude Code to encrypted, BSV-anchored verifiable memory, Google Mangle claim grounding with checkable `nektarr1.*` receipts, automatic Stop-hook verification, and policy-bounded knowledge trading — **zero Docker, zero Node.js, one static Go binary**.

> **Honesty Rule:** Receipts verify that claims were **recorded, unaltered, and derivable** from anchored fragments — never that their content is objectively _true_. Model identity is _declared_, not zkML-proven.

---

## 1. Fastest Start (25 Seconds, 1 Command)

### macOS & Linux
```sh
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up
```

### Windows (PowerShell)
```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up
```

### Go Live on BSV Mainnet Directly
```sh
# macOS / Linux (isolated in ~/.nektar/main, prints terminal funding QR)
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up --mainnet --confirm I_UNDERSTAND
```

### What `nektar up` Does Automatically (L3 Integration)
1. **Starts a detached background daemon** on `http://127.0.0.1:18420` (survives closing your terminal; add `--install-service` to auto-start on login).
2. **Pre-arms Restate v1.8.1** (macOS/Linux native, Windows WSL2) so live BSV knowledge trades work on Run #1.
3. **Docks Claude Code at full L3** (non-clobbering & reversible via `nektar dock --uninstall`):
   - `~/.claude.json` → `mcpServers.nektar` (`nektar mcp`, auto-bridges stdio to your shared Mainnet or Testnet daemon with zero SQLite lock contention across multiple Claude windows)
   - `~/.claude/settings.json` → native zero-Node Go hooks:
     - `SessionStart`: `nektar hook session-start`
     - `UserPromptSubmit`: `nektar hook prefetch`
     - `Stop`: `nektar hook verify-receipt` (blocks once if any `nektarr1.*` receipt fails verification)
   - `~/.claude/skills/nektar-memory/SKILL.md` → teaches Claude the recall-first → `groundedAnswer` → receipt-footer workflow globally across every repo.
4. **Runs a live end-to-end receipt probe** and prints a real verified `nektarr1.*` token + `verify:` URL.

---

## 2. Verify Your First Grounded Answer (10 Seconds)

Restart Claude Code and ask:

> *"Remember that our production Postgres database runs version 16 on port 5432, then recall it with a verified receipt."*

Claude calls `remember`, `recall`, and `groundedAnswer`, and ends its answer with:

```
receipt: nektarr1.<token>
verify: http://127.0.0.1:18420/v1/verify/nektarr1.<token>
```

Open the `verify:` link in any browser to inspect the Merkle commitment, citations, and `TruthNote`.

---

## 3. Deploy to Autonomous BSV Trading & Earning (60 Seconds)

1. **Fund your node wallet:**
   - **Testnet:** Open `nektar open` → Wallet → **1-Click Testnet Faucet**.
   - **Mainnet:** Scan the funding QR in `nektar open` → Wallet (or `nektar wallet address --network main --qr`) with `5,000–10,000` sats (~$0.003), then run `nektar wallet sync`.
2. **Approve your SpendGuard trading policy:**
   ```sh
   nektar policy approve --preset both
   # On Mainnet:
   nektar policy approve --preset both --confirm I_UNDERSTAND
   ```
3. **Buy or sell verified knowledge directly from Claude:**
   - Ask Claude: *"Buy verified BSV SPV notes up to 150 sats"* (calls `want`)
   - Or earn sats from your own verified notes:
     ```sh
     nektar earn                                      # list open Hub bounties & local shards
     nektar earn --bounty bounty-ops --shard notes    # anchor & submit for instant BSV payout
     nektar sell -shard notes -domains ops -price 100 # arm 24/7 autonomous peer sales
     ```

---

## 4. Daily Commands

| Command | Purpose |
|---|---|
| `nektar up` / `nektar stop` | Start or cleanly stop the shared background daemon |
| `nektar open` | Open the Svelte 5 Dashboard with 1-click pairing |
| `nektar doctor` | Verify memory, wallet, Restate, policy, peers & cohort density |
| `nektar dock claude --uninstall` | Cleanly remove Nektar from `~/.claude.json`, hooks & skill |
