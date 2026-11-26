# Nektar + Claude Code (v0.1 Pure-Go)

One command wires Claude Code to Nektar (MCP stdio auto-bridged to your running daemon + portable `SessionStart`/`UserPromptSubmit`/`Stop` hooks + `SKILL.md`):

```sh
nektar serve &
nektar dock claude
```

- **Preview without writing:** `nektar dock claude --dry`
- **Clean removal:** `nektar dock claude --uninstall`
- **Plugin marketplace bundle:** `.claude-plugin/plugin.json` (uses `ghcr.io/kryptokrates/nektar:latest mcp`)

## Manual MCP Stanza (`~/.claude.json`)

```json
{
  "mcpServers": {
    "nektar": {
      "command": "nektar",
      "args": ["mcp"]
    }
  }
}
```

Every factual answer grounded via `groundedAnswer` ends with a verifiable `nektarr1.*` receipt footer; the `Stop` hook (`nektar hook verify-receipt`) verifies receipts automatically and blocks once if any receipt fails verification.
