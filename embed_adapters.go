package nektar

import "embed"

// EmbeddedAdaptersFS bundles the native Hermes Agent MemoryProvider plugin,
// the OpenClaw memory-slot plugin, and the universal agentskills.io SKILL.md
// directly into the single static Go binary so `nektar dock` and
// `/v1/adapters/*` work offline with zero repo checkout required.
//
//go:embed skills/nektar-memory/SKILL.md
//go:embed integrations/hermes/plugin.yaml integrations/hermes/pyproject.toml integrations/hermes/__init__.py integrations/hermes/README.md integrations/hermes/hermes_memory_nektar/*.py
//go:embed integrations/openclaw/index.mjs integrations/openclaw/openclaw.plugin.json integrations/openclaw/package.json integrations/openclaw/README.md
var EmbeddedAdaptersFS embed.FS
