# Codex CLI — receipts + auto-check

Same outcome as the Claude Code integration, different mechanics — Codex has
`mcp_servers` (tool surface) and `notify` (turn-complete hook) instead of
hooks that can block.

## Setup

```sh
# 1. one server: MCP + verify endpoint on :8080
NEKTAR_HTTP_PORT=8080 NEKTAR_NETWORK=mock node packages/mcp/dist/src/bin.js
#    mock = offline zero-setup, anchors auto-mine → receipts fully verifiable
#    regtest = real chain (needs NEKTAR_REGTEST_WIF + rpc pass in .env)

# 2. ~/.codex/config.toml — merge config-snippet.toml:
#    [mcp_servers.nektar] url = "http://localhost:8080/mcp"
#    notify = ["node", "integrations/codex/verify-notify.mjs", "--chain", "<your existing notify if any>"]

# 3. run codex from the repo root so it picks up AGENTS.md (receipt rules):
cd nektar && codex
```

Ask something factual → the agent recalls, grounds the answer, and ends with:

```
receipt: nektarr1.…   verify: http://localhost:8080/v1/verify?r=nektarr1.…
```

## The three ways a receipt gets checked

1. **You scan it** — the `verify:` URL (or the QR on `?format=html`) runs the
   live checks: exists → re-hash → provenance → anchored → cites anchored.
2. **The agent checks it** — instructed to call `verifyReceipt` after
   grounding; the result is visible inline in the session.
3. **The notify hook checks it** — on every completed turn,
   `verify-notify.mjs` greps the last message for tokens, hits the endpoint,
   and appends to `codex-receipts.log` (`tail -f codex-receipts.log`).

## Honest boundary vs the Claude Code version

Codex `notify` is **fire-and-forget** — it can flag a FAILED receipt in the
log but cannot block the turn or feed back to the model (the Claude Code
Stop hook can). If the agent never calls `groundedAnswer`, no token appears
and the hook reports nothing — "no receipt" is itself the signal. And as
always: a verified receipt proves committed + derivable, never *true*.
