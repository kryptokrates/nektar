"""Configuration schema for `hermes memory setup` and Hermes dashboard."""

from __future__ import annotations

from typing import Any

CONFIG_SCHEMA: list[dict[str, Any]] = [
    {
        "key": "url",
        "label": "Nektar HTTP URL",
        "type": "string",
        "required": False,
        "default": "",
        "help": "Leave blank to auto-discover local nektar (daemon.json -> :18420 -> :8080).",
    },
    {
        "key": "token",
        "label": "Bearer Token (NEKTAR_HTTP_TOKEN)",
        "type": "secret",
        "required": False,
        "default": "",
        "help": "Only needed for remote or token-gated instances. Supports ${env:NEKTAR_HTTP_TOKEN}.",
    },
    {
        "key": "default_shard",
        "label": "Default Shard",
        "type": "string",
        "required": False,
        "default": "hermes",
        "help": "Shard where mirrored Hermes memories are saved.",
    },
    {
        "key": "mirror_memory_writes",
        "label": "Mirror MEMORY.md / USER.md writes into nektar",
        "type": "boolean",
        "required": False,
        "default": True,
        "help": "Automatically persists Hermes built-in memory updates to nektar (secret-scrubbed).",
    },
    {
        "key": "sync_turns",
        "label": "Capture raw turn summaries",
        "type": "boolean",
        "required": False,
        "default": False,
        "help": "Opt-in: off by default so only curated memory writes land on the ledger.",
    },
    {
        "key": "prefetch_timeout_ms",
        "label": "Prefetch timeout (ms)",
        "type": "integer",
        "required": False,
        "default": 800,
        "help": "Hard upper bound on pre-turn recall latency so Hermes never stalls.",
    },
    {
        "key": "prefetch_max_chars",
        "label": "Prefetch max chars",
        "type": "integer",
        "required": False,
        "default": 1600,
        "help": "Maximum context budget injected per turn.",
    },
]


def default_config() -> dict[str, Any]:
    return {field["key"]: field["default"] for field in CONFIG_SCHEMA}
