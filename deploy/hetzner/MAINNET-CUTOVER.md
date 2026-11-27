# `nektar.run` — Hetzner Mainnet Spine Hub Cutover & Rollback Runbook

This runbook brings up the **Mainnet Spine Hub** on `https://nektar.run` (`127.0.0.1:18430`) side-by-side with the existing Testnet Hub on `https://testnet.nektar.run` (`127.0.0.1:18420`) and fleet hubs (`18421..18425`), using strictly isolated Docker volumes (`hub-mainnet-data`, `hub-mainnet-backups`, `restate-mainnet-data`) and zero changes to live Testnet CI until cutover is verified.

---

## 1. Create Box-Local Secrets (`/opt/nektar/.env.mainnet`)

On the Hetzner host (never commit this file to Git):

```bash
umask 077
cat >/opt/nektar/.env.mainnet <<EOF
NEKTAR_MAINNET_CONFIRM=I_UNDERSTAND
NEKTAR_HTTP_TOKEN=$(openssl rand -hex 32)
NEKTAR_OPERATOR_TOKEN=$(openssl rand -hex 32)
NEKTAR_OPERATOR_PUBKEY=02<your-66-hex-cold-operator-pubkey>

# Optional off-site encrypted backups (Cloudflare R2 / Backblaze B2 / AWS S3)
# NEKTAR_BACKUP_S3_BUCKET=nektar-mainnet-backups
# NEKTAR_BACKUP_S3_ENDPOINT=https://<account-id>.r2.cloudflarestorage.com
# NEKTAR_BACKUP_S3_REGION=auto
# NEKTAR_BACKUP_S3_PREFIX=nektar-mainnet-spine
# NEKTAR_BACKUP_S3_ACCESS_KEY=<access-key>
# NEKTAR_BACKUP_S3_SECRET_KEY=<secret-key>
EOF
chmod 600 /opt/nektar/.env.mainnet
```

---

## 2. Pre-Flight Gate (`nektar check-mainnet`)

Verify the exact Mainnet env passes all fail-closed safety gates (confirmation, 2-of-3 non-circular PoW header witnesses, US/EU broadcaster spec, and SpendGuard):

```bash
cd /opt/nektar
docker compose \
  --env-file deploy/hetzner/nektar-mainnet.env \
  --env-file /opt/nektar/.env.mainnet \
  -f deploy/hetzner/compose.mainnet.yaml \
  run --rm --no-deps hub check-mainnet
```

Expected exit `0`:
`mainnet gates OK: confirmation, >=2 independent PoW header witnesses (non-circular), settlement spec, and SpendGuard configured`

---

## 3. Boot Mainnet Spine Hub + Restate v1.8.1 Sidecar

```bash
cd /opt/nektar
docker compose \
  --env-file deploy/hetzner/nektar-mainnet.env \
  --env-file /opt/nektar/.env.mainnet \
  -f deploy/hetzner/compose.mainnet.yaml pull hub restate

docker compose \
  --env-file deploy/hetzner/nektar-mainnet.env \
  --env-file /opt/nektar/.env.mainnet \
  -f deploy/hetzner/compose.mainnet.yaml up -d
```

---

## 4. Verify Local Loopback Before Switching Caddy

```bash
set -a && source /opt/nektar/.env.mainnet && set +a

# 1. Network == main & healthy
curl -fsS http://127.0.0.1:18430/v1/health
curl -fsS http://127.0.0.1:18430/hub/v1/health

# 2. Spine role + hybrid BRC-105 fee schedule active
curl -fsS http://127.0.0.1:18430/hub/v1/terms

# 3. Anti-freerider HTTP 402 gate active on /hub/sync
curl -i http://127.0.0.1:18430/hub/sync?cursor=0
# Expect: HTTP/1.1 402 Payment Required + X-BSV-Payment-Satoshis-Required

# 4. Doctor all green (restate, clock_skew_guard, autonomy_watchdog, encrypted_backup)
curl -fsS -X POST http://127.0.0.1:18430/v1/tools/doctor \
  -H "Authorization: Bearer $NEKTAR_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" -d '{}'

# 5. Get Mainnet funding address & send 5,000-10,000 real canary sats
curl -fsS http://127.0.0.1:18430/v1/wallet \
  -H "Authorization: Bearer $NEKTAR_OPERATOR_TOKEN"
```

---

## 5. Cut Over Caddy (`nektar.run` -> Mainnet, `testnet.nektar.run` -> Testnet)

```bash
sudo cp /etc/caddy/Caddyfile /etc/caddy/Caddyfile.pre-mainnet.bak
sudo cp /opt/nektar/deploy/hetzner/Caddyfile.mainnet /etc/caddy/Caddyfile
sudo caddy validate --config /etc/caddy/Caddyfile
sudo systemctl reload caddy

# Public smoke test
curl -fsS https://nektar.run/v1/health          # expect "network":"main"
curl -fsS https://testnet.nektar.run/v1/health  # expect "network":"testnet"
```

---

## 6. Instant Rollback

If anything looks wrong, revert Caddy to Testnet in 2 seconds without losing Mainnet data:

```bash
sudo cp /etc/caddy/Caddyfile.pre-mainnet.bak /etc/caddy/Caddyfile
sudo systemctl reload caddy
# Optional: engage kill switch on Mainnet container immediately
curl -X POST http://127.0.0.1:18430/v1/automation/kill \
  -H "Authorization: Bearer $NEKTAR_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" -d '{"enabled":true}'
```
