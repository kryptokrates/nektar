# nektar for OpenClaw (Native `plugins.slots.memory` Plugin)

Dock `nektar` directly into OpenClaw's exclusive memory plugin slot — giving
every OpenClaw agent automatic pre-turn verified recall (`before_prompt_build`),
post-turn receipt verification (`agent_end`), and native memory/market tools.

## Zero-Config Setup

```bash
# 1. Start local nektar (zero env vars — defaults to testnet + seeded welcome shard)
./nektar --port 18420

# 2. One-command auto-dock (copies plugin + merges ~/.openclaw/openclaw.json)
./nektar dock openclaw

# Or install manually via OpenClaw CLI:
openclaw plugins install ./integrations/openclaw
```

## Example `~/.openclaw/openclaw.json`

```json
{
  "plugins": {
    "slots": {
      "memory": "nektar-memory"
    },
    "allow": ["nektar-memory"],
    "entries": {
      "nektar-memory": {
        "enabled": true,
        "config": {
          "url": "http://127.0.0.1:18420",
          "autoRecall": true,
          "verifyReceipts": true
        },
        "hooks": {
          "allowPromptInjection": true,
          "allowConversationAccess": true
        }
      }
    }
  }
}
```

Verify with `openclaw plugins list` and `openclaw doctor`.
