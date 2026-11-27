"""Native Hermes Agent MemoryProvider backed by nektar.

Implements Hermes's MemoryProvider lifecycle:
  - initialize(session_id, hermes_home, **kwargs)
  - system_prompt_block() -> frozen, cache-friendly grounding rules + shards
  - prefetch(query, *, session_id="") -> bounded verified memory recall
  - on_memory_write(action, target, content) -> non-blocking mirror to nektar
  - sync_turn(user, assistant, *, session_id="", messages=None) -> receipt check
    + optional opt-in turn capture
  - get_tool_schemas() / handle_tool_call(name, args) -> native nektar tools
  - get_status_config() -> feeds `hermes memory status`
  - on_session_end(session_id) -> bounded flush of background writes
"""

from __future__ import annotations

import json
import os
import re
import threading
from typing import Any

from .client import NektarClient
from .config_schema import CONFIG_SCHEMA, default_config

# Import Hermes's base class & context-preserving thread helper when running
# inside Hermes Agent; fall back cleanly for standalone testing/validation.
try:  # pragma: no cover - exercised inside live Hermes runtime
    from agent.memory_provider import MemoryProvider as _BaseMemoryProvider
    from agent.memory_provider import spawn_context_thread as _spawn_context_thread
except Exception:  # pragma: no cover - standalone fallback

    class _BaseMemoryProvider:  # type: ignore[no-redef]
        name: str = ""

    def _spawn_context_thread(target: Any, args: tuple[Any, ...] = (), kwargs: dict[str, Any] | None = None, **_: Any) -> threading.Thread:
        t = threading.Thread(target=target, args=args, kwargs=kwargs or {}, daemon=True)
        t.start()
        return t


_RECEIPT_RE = re.compile(r"nektarr1\.[A-Za-z0-9_-]{40,}")
_VERIFY_URL_RE = re.compile(r"verify:\s*(https?://\S+)", re.IGNORECASE)


def _norm(text: str | None) -> str:
    return re.sub(r"\s+", " ", (text or "").lower()).strip()


def _rank_items(items: list[dict[str, Any]], query: str, limit: int = 5) -> list[dict[str, Any]]:
    words = [w for w in re.split(r"[^a-z0-9]+", _norm(query)) if len(w) >= 3]
    verified = [
        it
        for it in items
        if isinstance(it.get("text"), str)
        and it["text"].strip()
        and (not isinstance(it.get("check"), dict) or it["check"].get("verified") is not False)
    ]
    if not words:
        return verified[:limit]
    scored = []
    for idx, it in enumerate(verified):
        low = _norm(it.get("text"))
        score = sum(1 for w in words if w in low)
        scored.append((score, -idx, it))
    scored.sort(key=lambda x: (x[0], x[1]), reverse=True)
    hits = [it for score, _, it in scored if score > 0]
    return (hits or verified)[:limit]


class NektarMemoryProvider(_BaseMemoryProvider):
    """Pluggable Hermes MemoryProvider for nektar verifiable memory + market."""

    name = "nektar"
    config_schema = CONFIG_SCHEMA

    def __init__(self, config: dict[str, Any] | None = None, client: NektarClient | None = None) -> None:
        merged = default_config()
        if config:
            merged.update(config)
        self.config = merged
        self.client = client or NektarClient(
            url=str(merged.get("url") or "") or None,
            token=str(merged.get("token") or "") or None,
        )
        self.session_id: str = ""
        self.hermes_home: str | None = None
        self._shards: list[str] = []
        self._network: str = "unknown"
        self._connected: bool = False
        self._threads: list[threading.Thread] = []
        self._lock = threading.Lock()
        self.last_receipt_checks: list[dict[str, Any]] = []

    def _track_thread(self, target: Any, *args: Any) -> None:
        t = _spawn_context_thread(target=target, args=args)
        with self._lock:
            self._threads = [th for th in self._threads if th.is_alive()]
            self._threads.append(t)

    def initialize(self, session_id: str = "", hermes_home: str | None = None, **_: Any) -> None:
        self.session_id = session_id
        self.hermes_home = hermes_home
        url = self.client.discover(timeout_s=0.8)
        if not url:
            self._connected = False
            return
        try:
            doc = self.client.call_tool("doctor", {}, timeout_s=1.5)
            self._connected = True
            self._network = str(doc.get("network") or "unknown")
            shards = doc.get("shardNames")
            self._shards = [str(s) for s in shards] if isinstance(shards, list) else []
        except Exception:
            self._connected = False

    def system_prompt_block(self) -> str:
        if not self._connected:
            return ""
        shard_list = ", ".join(self._shards) if self._shards else "welcome"
        return (
            "## Verifiable Memory (nektar)\n"
            f"- Connected to nektar ({self._network}); readable shards: {shard_list}.\n"
            "- Verified fragments prefetched below carry `[shard:seq]` citations.\n"
            "- For verifiable answers, call `nektar_grounded_answer` citing verified fragments "
            "and keep the returned `receipt:` and `verify:` footer verbatim.\n"
            "- Honest boundary: receipts prove a record was committed and unaltered — never that it is true."
        )

    def prefetch(self, query: str, *, session_id: str = "") -> str:
        _ = session_id
        timeout_s = max(0.1, float(self.config.get("prefetch_timeout_ms", 800)) / 1000.0)
        max_chars = int(self.config.get("prefetch_max_chars", 1600))
        payload: dict[str, Any] = {"limit": 5}
        if (query or "").strip():
            payload["query"] = query.strip()
        try:
            res = self.client.call_tool("recall", payload, timeout_s=timeout_s)
        except Exception:
            return ""
        items = res.get("items")
        if not isinstance(items, list) or not items:
            return ""
        picks = _rank_items(items, query, limit=5)
        if not picks:
            return ""
        header = "[nektar verified memory — cite via nektar_grounded_answer]"
        lines = [header]
        used = len(header)
        for it in picks:
            snippet = re.sub(r"\s+", " ", str(it.get("text") or "")).strip()[:280]
            line = f"- [{it.get('shard', '?')}:{it.get('seq', 0)}] {snippet}"
            if used + len(line) + 1 > max_chars:
                break
            lines.append(line)
            used += len(line) + 1
        return "\n".join(lines) if len(lines) > 1 else ""

    def on_memory_write(self, action: str, target: str, content: str, **_: Any) -> None:
        if not self.config.get("mirror_memory_writes", True):
            return
        text = (content or "").strip()
        if not text:
            return
        shard = str(self.config.get("default_shard") or os.environ.get("NEKTAR_SHARD") or "hermes")
        source = f"hermes:{target or 'memory'}:{action or 'write'}"

        def _worker() -> None:
            try:
                self.client.call_tool("remember", {"shard": shard, "data": text, "source": source}, timeout_s=3.0)
            except Exception:
                pass

        self._track_thread(_worker)

    def sync_turn(
        self,
        user: str,
        assistant: str,
        *,
        session_id: str = "",
        messages: list[dict[str, Any]] | None = None,
    ) -> None:
        _ = messages
        tokens = list(dict.fromkeys(_RECEIPT_RE.findall(assistant or "")))[:3]
        capture = bool(self.config.get("sync_turns", False))
        if not tokens and not capture:
            return

        def _worker() -> None:
            if tokens:
                base_override = None
                m = _VERIFY_URL_RE.search(assistant or "")
                if m:
                    from urllib.parse import urlparse

                    parsed = urlparse(m.group(1))
                    if parsed.scheme in ("http", "https") and parsed.netloc:
                        cand = f"{parsed.scheme}://{parsed.netloc}"
                        host = (parsed.hostname or "").lower()
                        # Only trust loopback or our own configured daemon origin
                        # from untrusted assistant text (prevents SSRF & spoofing).
                        if host in ("127.0.0.1", "localhost", "::1") or cand == self.client.base_url:
                            base_override = cand
                checks_out: list[dict[str, Any]] = []
                for tok in tokens:
                    try:
                        ver = self.client.verify_receipt(tok, base_override=base_override, timeout_s=2.5)
                        ans = (ver.get("receipt") or {}).get("answer")
                        drift = bool(ans) and _norm(ans) not in _norm(assistant)
                        checks_out.append({"token": tok, "ok": bool(ver.get("ok")) and not drift, "drift": drift})
                    except Exception as exc:
                        checks_out.append({"token": tok, "ok": None, "error": str(exc)})
                with self._lock:
                    self.last_receipt_checks = checks_out

            if capture and (user or "").strip() and (assistant or "").strip():
                shard = str(self.config.get("default_shard") or "hermes")
                summary = f"Q: {user.strip()[:300]}\nA: {assistant.strip()[:500]}"
                try:
                    self.client.call_tool(
                        "remember",
                        {"shard": shard, "data": summary, "source": f"hermes:turn:{session_id or self.session_id}"},
                        timeout_s=3.0,
                    )
                except Exception:
                    pass

        self._track_thread(_worker)

    def get_tool_schemas(self) -> list[dict[str, Any]]:
        return [
            {
                "type": "function",
                "function": {
                    "name": "nektar_recall",
                    "description": "Recall verified fragments from nektar memory (omit shard to list and read all readable shards).",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "shard": {"type": "string", "description": "Optional shard name (e.g. welcome, hermes)"},
                            "query": {"type": "string", "description": "Optional search query to rank relevant memories (FTS5 BM25)"},
                            "limit": {"type": "integer", "description": "Optional max results"},
                            "includeSuperseded": {"type": "boolean", "description": "Include superseded records"},
                        },
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_remember",
                    "description": "Persist a durable fact into nektar verifiable memory with provenance (provide `text` or `data`; optionally supersede an older seq).",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "shard": {"type": "string"},
                            "text": {"type": "string", "description": "Fact or file content to persist (alias: data)"},
                            "data": {"type": "string", "description": "Alias for text"},
                            "path": {"type": "string", "description": "Optional file path inside the memory repo"},
                            "source": {"type": "string"},
                            "supersedes": {"type": "integer", "description": "Optional prior fragment seq in this shard superseded by this fact"},
                        },
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_forget",
                    "description": "Append a tombstone hiding an outdated or wrong record from active recall views without deleting append-only history.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "shard": {"type": "string"},
                            "seq": {"type": "integer"},
                            "reason": {"type": "string"},
                        },
                        "required": ["shard", "seq"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_grounded_answer",
                    "description": "Produce a verifiable grounded answer citing verified fragments and returning a checkable receipt footer.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "question": {"type": "string"},
                            "claims": {
                                "type": "array",
                                "items": {
                                    "type": "object",
                                    "properties": {
                                        "text": {"type": "string"},
                                        "cites": {
                                            "type": "array",
                                            "items": {
                                                "type": "object",
                                                "properties": {"shard": {"type": "string"}, "seq": {"type": "integer"}},
                                                "required": ["shard", "seq"],
                                            },
                                        },
                                    },
                                    "required": ["text", "cites"],
                                },
                            },
                        },
                        "required": ["claims"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_verify_receipt",
                    "description": "Verify a nektarr1.<token> answer receipt.",
                    "parameters": {
                        "type": "object",
                        "properties": {"token": {"type": "string"}},
                        "required": ["token"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_want",
                    "description": "Publish a policy-bounded knowledge buy want on the nektar BSV market.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "domains": {"type": "array", "items": {"type": "string"}},
                            "question": {"type": "string"},
                            "maxSats": {"type": "integer"},
                        },
                        "required": ["domains", "maxSats"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_sell",
                    "description": "List a shard for sale under an operator-approved seller policy.",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "shard": {"type": "string"},
                            "domains": {"type": "array", "items": {"type": "string"}},
                            "priceSats": {"type": "integer"},
                            "desc": {"type": "string"},
                        },
                        "required": ["shard", "domains", "priceSats"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_deals",
                    "description": "Inspect active and completed nektar knowledge trades.",
                    "parameters": {
                        "type": "object",
                        "properties": {"operationId": {"type": "string"}},
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_repo",
                    "description": "Full Git + SingularGrit Memory Repo operations anchored on BSV (action: list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate).",
                    "parameters": {
                        "type": "object",
                        "properties": {
                            "action": {"type": "string", "description": "Repo action to execute"},
                            "repo": {"type": "string", "description": "Target Memory Repo name"},
                            "branch": {"type": "string", "description": "Branch name (default: main)"},
                            "path": {"type": "string", "description": "File path for read/commit/rm/blame"},
                            "text": {"type": "string", "description": "File content for commit"},
                            "commit": {"type": "string", "description": "Commit SHA"},
                            "from": {"type": "string", "description": "Base ref/branch for diff/branch"},
                            "to": {"type": "string", "description": "Target ref/branch for diff"},
                            "sourceBranch": {"type": "string", "description": "Source branch for merge"},
                            "targetBranch": {"type": "string", "description": "Target branch for merge"},
                            "newRepo": {"type": "string", "description": "Destination repo name for fork"},
                            "tag": {"type": "string", "description": "Tag name"},
                            "message": {"type": "string", "description": "Commit or tag message"},
                            "autoAnchor": {"type": "boolean", "description": "Anchor on BSV immediately if funded"},
                        },
                        "required": ["action"],
                    },
                },
            },
            {
                "type": "function",
                "function": {
                    "name": "nektar_doctor",
                    "description": "Check nektar instance health, shards, funding balance, and next-step hints.",
                    "parameters": {"type": "object", "properties": {}},
                },
            },
        ]

    def handle_tool_call(self, name: str, args: dict[str, Any] | None = None) -> str:
        mapping = {
            "nektar_recall": "recall",
            "nektar_remember": "remember",
            "nektar_forget": "forget",
            "nektar_grounded_answer": "groundedAnswer",
            "nektar_verify_receipt": "verifyReceipt",
            "nektar_repo": "repo",
            "nektar_want": "want",
            "nektar_sell": "sell",
            "nektar_deals": "deals",
            "nektar_doctor": "doctor",
        }
        tool = mapping.get(name)
        if not tool:
            return json.dumps({"ok": False, "error": f"unknown nektar tool: {name}"})
        payload = dict(args or {})
        if tool == "remember" and not payload.get("shard") and not payload.get("repo"):
            payload["shard"] = str(self.config.get("default_shard") or "hermes")
        timeout = 45.0 if tool in ("want", "sell", "remember", "groundedAnswer", "repo") else 15.0
        try:
            res = self.client.call_tool(tool, payload, timeout_s=timeout)
            return json.dumps(res)
        except Exception as exc:
            return json.dumps({"ok": False, "error": str(exc)})

    def get_status_config(self) -> dict[str, Any]:
        url = self.client.discover(timeout_s=0.8)
        if not url:
            return {
                "provider": "nektar",
                "status": "offline",
                "hint": "start local nektar (`nektar serve`) or set NEKTAR_URL",
            }
        try:
            doc = self.client.call_tool("doctor", {}, timeout_s=1.5)
            return {
                "provider": "nektar",
                "status": "connected",
                "url": url,
                "network": doc.get("network"),
                "records": doc.get("records"),
                "shards": doc.get("shardNames"),
                "anchoredRecords": doc.get("anchoredRecords"),
                "hints": doc.get("hints", []),
            }
        except Exception as exc:
            return {"provider": "nektar", "status": "error", "url": url, "error": str(exc)}

    def on_session_end(self, session_id: str = "") -> None:
        _ = session_id
        with self._lock:
            threads = list(self._threads)
        for t in threads:
            t.join(timeout=2.0)


def register(ctx: Any) -> None:
    """Hermes plugin entry point (`hermes_agent.plugins`)."""
    ctx.register_memory_provider("nektar", NektarMemoryProvider)
