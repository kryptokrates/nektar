"""Real Hermes Agent MemoryProvider live BSV Testnet driver (Agent 1: Alpha).

Imports and drives the ACTUAL `integrations/hermes/hermes_memory_nektar` provider
against a running `nektar serve` daemon — no shortcuts, no mocks.
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path

repo_root = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(repo_root / "integrations" / "hermes"))

from hermes_memory_nektar.client import NektarClient  # noqa: E402
from hermes_memory_nektar.provider import NektarMemoryProvider  # noqa: E402


def main() -> None:
    if len(sys.argv) < 3:
        print("usage: hermes_agent_runner.py <phase> <data_dir> [run_tag]", file=sys.stderr)
        sys.exit(2)
    phase = sys.argv[1]
    data_dir = sys.argv[2]
    run_tag = sys.argv[3] if len(sys.argv) > 3 else "live"

    # Real Hermes auto-discovery via daemon.json + /.well-known/agent.json ("name" == "nektar")!
    client = NektarClient(data_dir=data_dir)
    provider = NektarMemoryProvider(
        config={"default_shard": "hermes-ops", "mirror_memory_writes": True, "sync_turns": False},
        client=client,
    )
    provider.initialize(session_id=f"hermes-testnet-{run_tag}")
    if not provider._connected:
        raise RuntimeError(f"Hermes NektarMemoryProvider failed to auto-discover daemon in {data_dir}")

    if phase == "setup":
        prompt_block = provider.system_prompt_block()
        assert "Verifiable Memory (nektar)" in prompt_block, f"bad system_prompt_block: {prompt_block}"
        pref = provider.prefetch("Nektar")
        assert "welcome" in pref, f"expected welcome shard in Hermes prefetch, got: {pref}"

        # Mirror a durable fact via real Hermes on_memory_write() + anchor it on BSV Testnet!
        fact = f"HermesAlpha[{run_tag}]: Prod Kubernetes etcd snapshots rotate every 15m to s3://etcd-prod."
        provider.on_memory_write("save", "ops", fact)
        provider.on_session_end(f"hermes-testnet-{run_tag}")

        # Anchor via NektarClient and list for sale via real Hermes handle_tool_call("nektar_sell")!
        anc = client.call_tool("anchor", {"shard": "hermes-ops"}, timeout_s=45.0)
        sell_raw = provider.handle_tool_call(
            "nektar_sell",
            {"shard": "hermes-ops", "domains": ["ops"], "priceSats": 70, "desc": "Hermes Alpha SRE runbook"},
        )
        sell_res = json.loads(sell_raw)
        rec = json.loads(provider.handle_tool_call("nektar_recall", {"shard": "hermes-ops"}))
        seq = rec["items"][-1]["seq"]

        status = provider.get_status_config()
        assert status["status"] == "connected", f"Hermes status failed: {status}"
        print(json.dumps({
            "ok": True,
            "discoveredUrl": client.base_url,
            "network": status["network"],
            "shards": status["shards"],
            "anchorTxid": anc.get("txid", ""),
            "seq": seq,
            "sell": sell_res,
        }))
        return

    if phase == "trade":
        # Buy "agents" knowledge from Gamma (Streamable HTTP MCP / L0 Proxy agent) via real Hermes nektar_want!
        t0 = time.perf_counter()
        want_raw = provider.handle_tool_call(
            "nektar_want",
            {"domains": ["agents"], "question": "MCP Streamable HTTP multiplexes", "maxSats": 150},
        )
        elapsed_ms = int((time.perf_counter() - t0) * 1000)
        deal = json.loads(want_raw)
        if deal.get("status") != "settled" or "multiplexes" not in str(deal.get("deliveredText", "")):
            raise RuntimeError(f"Hermes Alpha nektar_want failed: {deal}")

        # Also sell Hermes Alpha's anchored fact TO THE HUB bounty-ops!
        rec = json.loads(provider.handle_tool_call("nektar_recall", {"shard": "hermes-ops"}))
        seq = rec["items"][-1]["seq"]
        bounty = client.call_tool("sell", {"bountyId": "bounty-ops", "shard": "hermes-ops", "seq": seq}, timeout_s=45.0)
        if not bounty.get("accepted") or not bounty.get("txid"):
            raise RuntimeError(f"Hermes Alpha SellToHub bounty-ops failed: {bounty}")

        # Produce a grounded answer citing the purchased fact and verify it via Hermes sync_turn()!
        bought = json.loads(provider.handle_tool_call("nektar_recall", {"shard": "market/agents"}))
        bought_item = bought["items"][-1]
        ga_raw = provider.handle_tool_call(
            "nektar_grounded_answer",
            {
                "question": "How does MCP Streamable HTTP handle concurrent agents?",
                "claims": [{
                    "text": bought_item["text"],
                    "cites": [{"shard": bought_item["shard"], "seq": bought_item["seq"]}],
                }],
            },
        )
        ga = json.loads(ga_raw)
        assistant_msg = f"{ga['answer']}\n\n{ga['footer']}"
        provider.sync_turn("How does MCP Streamable HTTP work?", assistant_msg)
        provider.on_session_end(f"hermes-testnet-{run_tag}")
        checks = provider.last_receipt_checks
        if not checks or not checks[0].get("ok") or checks[0].get("drift"):
            raise RuntimeError(f"Hermes sync_turn receipt verification failed: {checks}")

        print(json.dumps({
            "ok": True,
            "deal": {
                "status": deal["status"],
                "priceSats": deal["priceSats"],
                "firstOfferMs": deal.get("firstOfferMs", 0),
                "totalMs": elapsed_ms,
                "paymentTxid": deal.get("paymentTxid", ""),
                "deliveredText": deal.get("deliveredText", ""),
            },
            "bounty": bounty,
            "receiptToken": ga["receiptToken"],
            "hermesSyncTurnCheck": checks[0],
        }))
        return

    if phase == "post-restart":
        # Verify Hermes Alpha can STILL trade with NAT-only Delta after Hub kill & restart!
        t0 = time.perf_counter()
        want_raw = provider.handle_tool_call(
            "nektar_want",
            {"domains": ["nat"], "question": "NAT-only A2A agents bbox relay", "maxSats": 150},
        )
        elapsed_ms = int((time.perf_counter() - t0) * 1000)
        deal = json.loads(want_raw)
        if deal.get("status") != "settled" or "bbox" not in str(deal.get("deliveredText", "")):
            raise RuntimeError(f"Hermes Alpha post-restart NAT trade failed: {deal}")
        print(json.dumps({
            "ok": True,
            "priceSats": deal["priceSats"],
            "firstOfferMs": deal.get("firstOfferMs", 0),
            "totalMs": elapsed_ms,
            "paymentTxid": deal.get("paymentTxid", ""),
        }))
        return

    raise SystemExit(f"unknown phase: {phase}")


if __name__ == "__main__":
    main()
