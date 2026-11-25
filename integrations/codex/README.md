# Codex CLI — receipts + auto-check (v0.4 Pure-Go)

Same outcome as the Claude Code integration, different mechanics — Codex has
`mcp_servers` (tool surface) and `notify` (turn-complete hook) instead of
hooks that can block. Zero Node.js required.

## Setup

```sh
# 1. Start the single Go binary (MCP + Dashboard + verify endpoint on :18420)
nektar serve

# 2. One-command auto-dock into ~/.codex/config.toml (or merge config-snippet.toml manually)
nektar dock codex

# 3. Run codex from the repo root so it picks up AGENTS.md (receipt rules)
cd nektar && codex
```

Ask something factual → the agent recalls, grounds the answer, and ends with:

```
receipt: nektarr1.…   verify: http://127.0.0.1:18420/v1/verify/nektarr1.…
```

## The three ways a receipt gets checked

1. **You scan it** — the `verify:` URL runs the live checks: exists → re-hash →
   provenance → anchored → cites anchored.
2. **The agent checks it** — instructed to call `verifyReceipt` after
   grounding; the result is visible inline in the session.
3. **The native Go notify hook checks it** — `nektar hook verify-receipt` runs
   on every completed turn and verifies any `nektarr1.*` tokens printed in the
   assistant reply.

## Honest boundary vs the Claude Code version

Codex `notify` is **fire-and-forget** — it can flag a FAILED receipt in the log
but cannot block the turn the way the Claude Code Stop hook can. The in-session
check (the agent calling `verifyReceipt` itself) is the visible one here. As
always: a verified receipt proves **committed + unaltered + derivable**, never
_true_.
