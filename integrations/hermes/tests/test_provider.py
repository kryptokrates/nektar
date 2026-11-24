"""Unit tests for the Hermes native nektar MemoryProvider plugin."""

from __future__ import annotations

import json
import sys
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any

# Ensure integrations/hermes is importable without editable pip install
sys.path.insert(0, str(Path(__file__).resolve().parents[1]))

from hermes_memory_nektar import NektarMemoryProvider, register
from hermes_memory_nektar.client import NektarClient


class _FakeNektarState:
    def __init__(self) -> None:
        self.remembered: list[dict[str, Any]] = []
        self.verify_calls: list[str] = []


def _start_fake_server(state: _FakeNektarState) -> tuple[HTTPServer, str]:
    class Handler(BaseHTTPRequestHandler):
        def log_message(self, format: str, *args: Any) -> None:
            _ = (format, args)

        def _send(self, code: int, body: dict[str, Any]) -> None:
            raw = json.dumps(body).encode("utf-8")
            self.send_response(code)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

        def do_GET(self) -> None:
            if self.path == "/.well-known/agent.json":
                self._send(
                    200,
                    {
                        "name": "nektar",
                        "network": "testnet",
                        "endpoints": {"mcp": f"http://127.0.0.1:{self.server.server_port}/mcp"},
                    },
                )
                return
            if self.path.startswith("/v1/verify?r="):
                tok = self.path.split("?r=", 1)[1]
                state.verify_calls.append(tok)
                self._send(200, {"ok": True, "checks": [{"name": "found", "ok": True}], "receipt": {"answer": "45 Nm"}})
                return
            self._send(404, {"error": "not found"})

        def do_POST(self) -> None:
            length = int(self.headers.get("Content-Length", "0"))
            body = json.loads(self.rfile.read(length).decode("utf-8") or "{}")
            if self.path == "/v1/tools/doctor":
                self._send(200, {"network": "testnet", "records": 2, "shards": 1, "shardNames": ["welcome"], "anchoredRecords": 1})
                return
            if self.path == "/v1/tools/recall":
                self._send(
                    200,
                    {
                        "shards": ["welcome"],
                        "items": [
                            {"shard": "welcome", "seq": 0, "text": "Drain port 4 torque is 45 Nm.", "check": {"verified": True}},
                            {"shard": "welcome", "seq": 1, "text": "Unrelated general greeting.", "check": {"verified": True}},
                        ],
                    },
                )
                return
            if self.path == "/v1/tools/remember":
                state.remembered.append(body)
                self._send(200, {"seq": len(state.remembered) - 1, "shard": body.get("shard")})
                return
            self._send(404, {"error": f"unknown route {self.path}"})

    srv = HTTPServer(("127.0.0.1", 0), Handler)
    port = srv.server_address[1]
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    return srv, f"http://127.0.0.1:{port}"


class TestNektarHermesProvider(unittest.TestCase):
    def test_register_binds_provider(self) -> None:
        registered: dict[str, Any] = {}

        class FakeCtx:
            def register_memory_provider(self, name: str, cls: Any) -> None:
                registered[name] = cls

        register(FakeCtx())
        self.assertIs(registered["nektar"], NektarMemoryProvider)

    def test_lifecycle_prefetch_mirror_and_receipt_check(self) -> None:
        state = _FakeNektarState()
        srv, url = _start_fake_server(state)
        try:
            provider = NektarMemoryProvider(config={"url": url, "default_shard": "hermes"})
            provider.initialize(session_id="s1")

            prompt = provider.system_prompt_block()
            self.assertIn("welcome", prompt)
            self.assertIn("never that it is true", prompt)

            prefetched = provider.prefetch("What torque does drain port 4 need?")
            self.assertIn("[welcome:0]", prefetched)
            self.assertIn("45 Nm", prefetched)

            provider.on_memory_write("append", "MEMORY.md", "User prefers dark mode and TypeScript.")
            tok = "nektarr1." + ("A" * 44)
            provider.sync_turn("torque?", f"Torque is 45 Nm.\nreceipt: {tok}\nverify: {url}/v1/verify?r={tok}")
            provider.on_session_end("s1")

            self.assertEqual(len(state.remembered), 1)
            self.assertEqual(state.remembered[0]["shard"], "hermes")
            self.assertEqual(state.remembered[0]["source"], "hermes:MEMORY.md:append")
            self.assertEqual(state.verify_calls, [tok])
            self.assertEqual(provider.last_receipt_checks[0]["ok"], True)

            schemas = provider.get_tool_schemas()
            names = {s["function"]["name"] for s in schemas}
            self.assertIn("nektar_grounded_answer", names)
            self.assertIn("nektar_want", names)

            out = json.loads(provider.handle_tool_call("nektar_recall", {}))
            self.assertEqual(out["shards"], ["welcome"])

            status = provider.get_status_config()
            self.assertEqual(status["status"], "connected")
            self.assertEqual(status["network"], "testnet")
        finally:
            srv.shutdown()
            srv.server_close()

    def test_offline_fails_open(self) -> None:
        client = NektarClient(url="http://127.0.0.1:1")
        provider = NektarMemoryProvider(config={"url": "http://127.0.0.1:1", "prefetch_timeout_ms": 150}, client=client)
        provider.initialize(session_id="offline")
        self.assertEqual(provider.system_prompt_block(), "")
        self.assertEqual(provider.prefetch("anything"), "")
        self.assertEqual(provider.get_status_config()["status"], "offline")

    def test_remote_verify_url_in_assistant_text_is_ignored(self) -> None:
        state = _FakeNektarState()
        srv, url = _start_fake_server(state)
        try:
            provider = NektarMemoryProvider(config={"url": url, "token": "secret-operator-tok"})
            provider.initialize(session_id="s-ssrf")
            tok = "nektarr1." + ("B" * 44)
            # Untrusted assistant text pointing verify: at a non-loopback origin
            # must fall back to the configured local daemon instead of SSRFing.
            provider.sync_turn("q", f"45 Nm\nreceipt: {tok}\nverify: https://evil.example/v1/verify?r={tok}")
            provider.on_session_end("s-ssrf")
            self.assertEqual(state.verify_calls, [tok])
            self.assertEqual(provider.last_receipt_checks[0]["ok"], True)
        finally:
            srv.shutdown()
            srv.server_close()


if __name__ == "__main__":
    unittest.main()
