# Nektar v0.1 for Codex CLI — 25-Second Setup & Complete Manual

Give OpenAI Codex CLI encrypted, BSV-anchored memory, automatic turn-complete receipt verification, global `SKILL.md` grounding rules, and autonomous knowledge trading — **zero Docker, zero Node.js, one static Go binary**.

> **Honesty Rule:** A verified receipt proves a claim was **recorded, unaltered, and derivable** from anchored memory — never that it is objectively _true_.

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

*(Or if `nektar` is already installed: `nektar up --dock codex` — works identically on Mainnet with `--network main --confirm I_UNDERSTAND`.)*

### What `nektar up` Wires Automatically for Codex CLI (L2 Integration)
1. **`~/.codex/config.toml`** (properly escaped for macOS, Linux, and Windows paths, preserving all sibling servers & custom settings):
   - Top-level `notify = ["<nektar>", "hook", "verify-receipt", "--codex"]` (placed safely before any `[table]` header per TOML spec; skipped if you already have a custom `notify` hook)
   - `[mcp_servers.nektar]` running `nektar mcp` (transparently auto-bridges to your live Mainnet or Testnet background daemon via `~/.nektar/active-daemon.json` — zero SQLite lock contention across multiple Codex sessions!)
2. **Global Skills** (`~/.codex/skills/nektar-memory/SKILL.md` & `~/.agents/skills/nektar-memory/SKILL.md`):
   - Teaches Codex the recall-first → `groundedAnswer` → `nektarr1.*` receipt loop in **every** workspace, not just inside the `nektar` repository.
3. **Background Pure-Go Daemon (Zero Sidecars)**:
   - Survives terminal closes (`nektar stop` to shut down; `--install-service` to auto-start on login) and auto-arms Testnet sandbox capsule trading out of the box.

---

## 2. Verify It Works (10 Seconds)

Start `codex` in any project and ask:

> *"Remember that our staging API runs on port 9090, then recall it with a verified receipt."*

Codex calls `remember`, `recall`, and `groundedAnswer`, outputs a real `nektarr1.*` receipt footer, and the native Go `notify` hook (`nektar hook verify-receipt --codex`) verifies every emitted receipt automatically.

*Honest boundary vs. Claude Code:* Codex `notify` runs after a turn completes (fire-and-forget audit), whereas Claude Code's `Stop` hook can block a turn once if a receipt fails. Codex's in-session `verifyReceipt` call + `verify:` link are the visible inline checks.

---

## 3. Deploy to Autonomous BSV Trading & Earning (60 Seconds)

```sh
# 1. Fund wallet (Testnet 1-click faucet in `nektar open`, or Mainnet QR:)
nektar wallet address --network main --qr

# 2. Approve buyer + seller SpendGuard caps
nektar policy approve --preset both

# 3. Inspect Hub bounties or earn sats directly
nektar earn
nektar want -domains ops,bsv -question "How does BRC-369 disclosure verify?" -max 150
```

Revert Codex config cleanly anytime with `nektar dock codex --uninstall`.
