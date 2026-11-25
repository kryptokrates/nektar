"""Zero-dependency stdlib HTTP client + loopback auto-discovery for nektar."""

from __future__ import annotations

import json
import os
from pathlib import Path
from typing import Any
from urllib import error, parse, request

DEFAULT_PORTS = (18420, 8080)


def _read_daemon_json(path: Path) -> tuple[str | None, str | None]:
    try:
        if not path.is_file():
            return None, None
        data = json.loads(path.read_text(encoding="utf-8"))
        url = data.get("url")
        tok = data.get("authToken") or data.get("token")
        tok_s = tok.strip() if isinstance(tok, str) and tok.strip() else None
        if isinstance(url, str) and url.startswith(("http://", "https://")):
            return url.rstrip("/"), tok_s
    except Exception:
        return None, None
    return None, None


class NektarClient:
    """Thin stdlib client talking to nektar's /v1 REST + discovery surface."""

    def __init__(
        self,
        url: str | None = None,
        token: str | None = None,
        data_dir: str | None = None,
    ) -> None:
        self.explicit_url = (url or "").strip().rstrip("/") or None
        self.token = (token or os.environ.get("NEKTAR_HTTP_TOKEN") or "").strip() or None
        self.data_dir = data_dir or os.environ.get("NEKTAR_DATA_DIR")
        self.base_url: str | None = self.explicit_url
        self.card: dict[str, Any] | None = None

    def _headers(self, json_body: bool = False) -> dict[str, str]:
        h: dict[str, str] = {"Accept": "application/json"}
        if json_body:
            h["Content-Type"] = "application/json"
        if self.token:
            h["Authorization"] = f"Bearer {self.token}"
        return h

    def _candidate_urls(self) -> list[str]:
        seen: set[str] = set()
        out: list[str] = []

        def add(u: str | None) -> None:
            if not u:
                return
            norm = u.strip().rstrip("/")
            if norm and norm not in seen:
                seen.add(norm)
                out.append(norm)

        def add_daemon(path: Path) -> None:
            u, tok = _read_daemon_json(path)
            if tok and not self.token:
                self.token = tok
            add(u)

        add(self.explicit_url)
        add(os.environ.get("NEKTAR_URL"))
        add(os.environ.get("NEKTAR_DAEMON_URL"))
        if self.data_dir:
            add_daemon(Path(self.data_dir) / "daemon.json")
        add_daemon(Path.cwd() / ".nektar" / "daemon.json")
        add_daemon(Path.home() / ".nektar" / "daemon.json")
        env_port = os.environ.get("NEKTAR_HTTP_PORT")
        if env_port:
            add(f"http://127.0.0.1:{env_port}")
        for p in DEFAULT_PORTS:
            add(f"http://127.0.0.1:{p}")
        return out

    def discover(self, timeout_s: float = 0.6) -> str | None:
        for base in self._candidate_urls():
            try:
                req = request.Request(f"{base}/.well-known/agent.json", headers=self._headers())
                with request.urlopen(req, timeout=timeout_s) as resp:
                    if resp.status != 200:
                        continue
                    card = json.loads(resp.read().decode("utf-8"))
                    if isinstance(card, dict) and card.get("name") == "nektar":
                        self.base_url = base
                        self.card = card
                        return base
            except Exception:
                continue
        return None

    def ensure_url(self, timeout_s: float = 0.5) -> str | None:
        if self.base_url:
            return self.base_url
        return self.discover(timeout_s=timeout_s)

    def call_tool(self, name: str, args: dict[str, Any] | None = None, timeout_s: float = 2.0) -> dict[str, Any]:
        base = self.ensure_url(timeout_s=min(timeout_s, 0.5))
        if not base:
            raise ConnectionError(
                "nektar daemon not reachable — start it with `nektar serve` or set NEKTAR_URL"
            )
        payload = json.dumps(args or {}).encode("utf-8")
        req = request.Request(
            f"{base}/v1/tools/{parse.quote(name, safe='')}",
            data=payload,
            headers=self._headers(json_body=True),
            method="POST",
        )
        try:
            with request.urlopen(req, timeout=timeout_s) as resp:
                raw = resp.read().decode("utf-8")
                return json.loads(raw) if raw else {}
        except error.HTTPError as exc:
            body = exc.read().decode("utf-8", errors="replace")
            raise RuntimeError(f"nektar {name} failed (HTTP {exc.code}): {body[:200]}") from exc

    def verify_receipt(self, token: str, base_override: str | None = None, timeout_s: float = 2.5) -> dict[str, Any]:
        override = (base_override or "").rstrip("/") or None
        base = override or self.ensure_url(timeout_s=0.5)
        if not base:
            raise ConnectionError("nektar verify endpoint unreachable")
        # Never attach our operator bearer token to a foreign verify override
        headers = self._headers() if (override is None or override == self.base_url) else {"Accept": "application/json"}
        req = request.Request(
            f"{base}/v1/verify?r={parse.quote(token, safe='')}",
            headers=headers,
            method="GET",
        )
        with request.urlopen(req, timeout=timeout_s) as resp:
            return json.loads(resp.read().decode("utf-8"))
