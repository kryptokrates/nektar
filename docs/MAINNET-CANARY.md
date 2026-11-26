# Nektar v0.1 — Small-Sats Mainnet Canary Runbook

> **Honesty Rule:** Nektar proofs establish **recorded + unaltered + derivable by block time** — never that a memory's content is true, and model identity is *declared*, not zkML-proven.

This runbook takes a v0.1 Pure-Go Nektar node or Spine-and-Leaf Hub from Testnet verification to a controlled **5,000–10,000 satoshi** live BSV Mainnet canary with strict fail-closed custody, multi-witness PoW header validation, and Restate v1.8.1 durable execution.

---

## 1. Cold / Hot Key Separation & Vault Custody

Never keep treasury funds on an unattended hot agent node.

1. **Pin an off-box Operator Pubkey** (hardware wallet / air-gapped key / BSV Desktop key):
   ```bash
   export NEKTAR_OPERATOR_PUBKEY="02<66-hex-compressed-cold-pubkey>"
   ```
   When set, every operator spending policy record must carry a valid signature from `NEKTAR_OPERATOR_PUBKEY`; the hot agent key cannot weaken its own spend caps.

2. **Deploy a two-key on-chain `NektarAgentVault`** (optional for hot-agent treasury):
   ```bash
   nektar vault deploy --operator-pub "$NEKTAR_OPERATOR_PUBKEY" --sats 10000
   nektar vault status
   ```
   The hot agent key can only draw within policy limits; the cold operator key can `pause`, `rotate-agent`, or `sweep` at any time.

3. **Keep hot canary funding small:** fund the canary wallet with **5,000–10,000 sats** (`~$0.002–$0.005`) — enough for anchors, FuelKeeper splits, Hub fees, and several `DisclosureSale` trades, with negligible blast radius.

---

## 2. Mainnet Safety Gate (`nektar check-mainnet`)

Mainnet nodes **fail closed** at startup unless all four safety gates are explicitly configured:

```bash
export NEKTAR_NETWORK="main"
export NEKTAR_MAINNET_CONFIRM="I_UNDERSTAND"

# >= 2 distinct-origin, non-circular PoW header witnesses (zero-PoW Native header endpoints are forbidden!)
# Default 2-of-2 live Mainnet quorum: WhatsOnChain + Babbage Chaintracks v2 (built into bcommon/headers)
export NEKTAR_HEADERS_SOURCE="woc:main,chaintracks:https://mainnet-chaintracks.babbage.systems/v2"
export NEKTAR_CHAIN_SPEC="woc:main"
export NEKTAR_SETTLE_SPEC="arcade:https://arcade-v2-us-1.bsvblockchain.tech"

# Conservative canary SpendGuard caps
export NEKTAR_MAX_FEE_SATS_PER_TX=2000
export NEKTAR_MAX_SPEND_SATS_PER_DAY=10000
export NEKTAR_WALLET_PROFILE="standard"
```

Verify the gate before starting any daemon:

```bash
nektar check-mainnet
# Expected exit 0:
# mainnet gates OK: confirmation, >=2 independent PoW header witnesses (non-circular), settlement spec, and SpendGuard configured
```

---

## 3. Production Restate v1.8.1 Durable Execution

Live-network trades (`testnet` and `main`) **require** an active Restate v1.8.1 durable execution plane (`BuyerDeal`, `SellerDeal`, `Custody`, `Tx`, `AnchorCadence`, `NektarRuntime`). There is no local-broker bypass on live networks.

- **Bare-metal Linux / macOS (auto-supervised loopback):**
  ```bash
  nektar restate-up        # downloads & SHA-256 verifies pinned restate-server v1.8.1 -> ~/.nektar/bin/
  export NEKTAR_RESTATE_REQUIRED=1
  ```
- **Docker Compose / Kubernetes sidecar:**
  Use `deploy/hub/compose.yaml` (runs `docker.io/restatedev/restate:1.8.1` on an internal bridge network) and set:
  ```bash
  export NEKTAR_RESTATE_URL="http://restate:8080"
  export NEKTAR_RESTATE_ADMIN="http://restate:9070"
  export NEKTAR_RESTATE_ENDPOINT_URI="http://agent:9080"
  export NEKTAR_RESTATE_REQUIRED=1
  # Recommended for multi-host networks:
  # export NEKTAR_RESTATE_IDENTITY_KEY="publickeyv1_..."
  ```

---

## 4. Spine-and-Leaf Hub Launch (Optional Hub Canary)

To deploy a Mainnet Leaf or Spine Hub with BRC-105 fee enforcement:

```bash
# 1-command Docker Compose + Caddy Auto-TLS deployment
nektar hub up \
  --domain hub.example.com \
  --network main \
  --confirm I_UNDERSTAND \
  --hub-role leaf \
  --fee-preset hybrid
```

Confirm live terms and anti-freerider enforcement immediately:

```bash
curl -s https://hub.example.com/hub/v1/terms
# Verify role=leaf|spine, preset=hybrid|lightbsv_egress, and non-zero fee schedule.

curl -i https://hub.example.com/hub/sync?cursor=0
# Expected: HTTP/1.1 402 Payment Required with X-BSV-Payment-Satoshis-Required header.
```

---

## 5. Step-by-Step Mainnet Canary Sequence (5,000–10,000 sats)

### Step A — Boot & Fund

```bash
nektar serve \
  --listen 127.0.0.1:18420 \
  --network main \
  --hosts https://hub.example.com/hub

nektar wallet address        # send 5,000–10,000 real BSV sats to this address
nektar wallet sync           # verify confirmed balance & UTXOs
nektar wallet fuel           # split into parallel fuel UTXOs (optional for >1 concurrent tx)
nektar doctor                # confirm all green (Restate, PoW witnesses, SpendGuard, wallet)
```

### Step B — Anchor & Offline SPV Verification (1 Real Block)

```bash
nektar remember -shard canary -text "Mainnet canary fragment #1: verified BSV anchor." -anchor
```

Wait for **1 mined Mainnet block** (~10 minutes), then refresh and verify offline SPV proof + receipt:

```bash
nektar wallet sync
nektar recall -shard canary -spv
# Confirm anchorState == "mined" and spvVerified == true.
```

### Step C — 1 Small-Sats Live `DisclosureSale` Trade

On **Node B (Seller)** with a mined fragment in `canary`:

```bash
nektar policy approve --preset seller --domains canary --confirm I_UNDERSTAND
nektar sell -shard canary -domains canary -price 50 -desc "Mainnet canary knowledge fragment"
```

On **Node A (Buyer)** funded with 5,000 sats:

```bash
nektar policy approve --preset buyer --domains canary --confirm I_UNDERSTAND
nektar want -domains canary -question "Mainnet canary fragment verified BSV anchor" -max 100 -shard bought/canary
```

Verify all four artifacts after settlement:
1. Buyer `nektar recall -shard bought/canary -spv` returns the decrypted fragment with valid SPV proof.
2. `nektar verify -token <nektardr1...>` returns verified `DealReceipt`.
3. Payment txid appears on independent block explorers (`whatsonchain.com/tx/<txid>`).
4. Hub `GET /hub/v1/hub/earnings` records collected Valve 1/2/3 satoshis with zero double-spend claims.

### Step D — Emergency Kill-Switch, Pause & Sweep Drill

Always rehearse the emergency controls while the balance is small:

```bash
# 1. Engage operator kill switch (immediately blocks all automated market spends)
curl -X POST http://127.0.0.1:18420/v1/automation/kill \
  -H "Content-Type: application/json" \
  -d '{"enabled":true}'
# Or drop a filesystem sentinel: touch ~/.nektar/KILL

# 2. If using NektarAgentVault, test on-chain pause & sweep
nektar vault pause
nektar vault sweep --to <cold-bsv-address>

# 3. Or sweep hot embedded wallet back to cold address
nektar wallet sweep -to <cold-bsv-address> -confirm I_UNDERSTAND
```

---

## 6. Go / No-Go Mainnet Checklist

- [ ] `go test -count=1 ./...` 16/16 packages green
- [ ] `nektar check-mainnet` exits `0` with `>=2` distinct-origin PoW witnesses
- [ ] `restate-server v1.8.1` connected and registered (`NEKTAR_RESTATE_REQUIRED=1`)
- [ ] `NEKTAR_OPERATOR_PUBKEY` pinned to off-box cold key
- [ ] SpendGuard caps conservative (`NEKTAR_MAX_FEE_SATS_PER_TX <= 2000`, `NEKTAR_MAX_SPEND_SATS_PER_DAY <= 10000`)
- [ ] Canary anchor mined & `nektar recall -spv` green
- [ ] Canary trade settled & `nektar verify` green
- [ ] Kill-switch & sweep drill verified end-to-end
