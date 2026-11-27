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
        self.explicit_token = (token or os.environ.get("NEKTAR_HTTP_TOKEN") or "").strip() or None
        self.token: str | None = self.explicit_token
        self.data_dir = data_dir or os.environ.get("NEKTAR_DATA_DIR")
        self.base_url: str | None = self.explicit_url
        self.card: dict[str, Any] | None = None

    def _headers(self, json_body: bool = False, token_override: str | None = None) -> dict[str, str]:
        h: dict[str, str] = {"Accept": "application/json"}
        if json_body:
            h["Content-Type"] = "application/json"
        tok = token_override if token_override is not None else self.token
        if tok:
            h["Authorization"] = f"Bearer {tok}"
        return h

    def _candidates(self) -> list[tuple[str, str | None]]:
        by_url: dict[str, int] = {}
        out: list[tuple[str, str | None]] = []

        def add(u: str | None, tok: str | None = None) -> None:
            if not u:
                return
            norm = u.strip().rstrip("/")
            if not norm:
                return
            resolved_tok = self.explicit_token or (tok.strip() if isinstance(tok, str) and tok.strip() else None)
            if norm in by_url:
                idx = by_url[norm]
                if out[idx][1] is None and resolved_tok is not None:
                    out[idx] = (norm, resolved_tok)
                return
            by_url[norm] = len(out)
            out.append((norm, resolved_tok))

        def add_daemon(path: Path) -> None:
            u, tok = _read_daemon_json(path)
            add(u, tok)

        add(self.explicit_url, self.explicit_token)
        add(os.environ.get("NEKTAR_URL"), self.explicit_token)
        add(os.environ.get("NEKTAR_DAEMON_URL"), self.explicit_token)
        if self.data_dir:
            add_daemon(Path(self.data_dir) / "daemon.json")
        add_daemon(Path.cwd() / ".nektar" / "daemon.json")
        add_daemon(Path.home() / ".nektar" / "active-daemon.json")
        add_daemon(Path.home() / ".nektar" / "main" / "daemon.json")
        add_daemon(Path.home() / ".nektar" / "daemon.json")
        env_port = os.environ.get("NEKTAR_HTTP_PORT")
        if env_port:
            add(f"http://127.0.0.1:{env_port}", self.explicit_token)
        for p in DEFAULT_PORTS:
            add(f"http://127.0.0.1:{p}", self.explicit_token)
        return out

    def _candidate_urls(self) -> list[str]:
        return [u for u, _ in self._candidates()]

    def discover(self, timeout_s: float = 0.6) -> str | None:
        for base, cand_token in self._candidates():
            try:
                req = request.Request(
                    f"{base}/.well-known/agent.json",
                    headers=self._headers(token_override=cand_token or ""),
                )
                with request.urlopen(req, timeout=timeout_s) as resp:
                    if resp.status != 200:
                        continue
                    card = json.loads(resp.read().decode("utf-8"))
                    if isinstance(card, dict) and card.get("name") == "nektar":
                        self.base_url = base
                        self.token = self.explicit_token or cand_token
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

        def _do_request(target_base: str) -> dict[str, Any]:
            req = request.Request(
                f"{target_base}/v1/tools/{parse.quote(name, safe='')}",
                data=payload,
                headers=self._headers(json_body=True),
                method="POST",
            )
            with request.urlopen(req, timeout=timeout_s) as resp:
                raw = resp.read().decode("utf-8")
                return json.loads(raw) if raw else {}

        try:
            return _do_request(base)
        except error.HTTPError as exc:
            if exc.code == 401 and not self.explicit_token:
                # Re-discover active-daemon.json once in case daemon restarted with a new authToken.
                prev_tok = self.token
                refreshed = self.discover(timeout_s=min(timeout_s, 0.5))
                if refreshed and (refreshed != base or self.token != prev_tok):
                    try:
                        return _do_request(refreshed)
                    except error.HTTPError as exc2:
                        body2 = exc2.read().decode("utf-8", errors="replace")
                        raise RuntimeError(f"nektar {name} failed (HTTP {exc2.code}): {body2[:200]}") from exc2
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
