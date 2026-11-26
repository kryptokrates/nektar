# Nektar v0.1 for Hermes Agent & OpenClaw — 30-Second Native L3 Setup

Dock Nektar directly into **Hermes Agent**'s native `MemoryProvider` slot or **OpenClaw**'s native `plugins.slots.memory` slot — giving always-on autonomous agents pre-turn verified recall, mirrored memory anchoring, background receipt verification, and autonomous BSV knowledge trading.

**Zero repo clone, zero `pip install`, zero Docker:** the Hermes and OpenClaw plugins are `//go:embed`'d directly inside the static `nektar` Go binary.

---

## 1. Fastest Start (30 Seconds, 1 Command)

### macOS & Linux
```sh
curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up
```

### Windows (PowerShell)
```powershell
& ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up
```

*(Already installed `nektar`? Run `nektar up --dock hermes` or `nektar up --dock openclaw`, or click **Dock** inside the Dashboard at `nektar open`.)*

---

## 2. What Gets Wired Automatically

### Hermes Agent (`~/.hermes`)
1. Extracts the embedded `hermes-memory-nektar` Python package into `~/.hermes/plugins/hermes-memory-nektar/` (stdlib-only HTTP client — zero external Python dependencies).
2. Installs `~/.hermes/skills/nektar-memory/SKILL.md`.
3. Safely enables `memory:\n  provider: nektar` in `~/.hermes/config.yaml` (preserving existing keys and saving a one-time backup at `config.yaml.nektar.bak`).
4. Auto-discovers your active Mainnet or Testnet daemon via `~/.nektar/active-daemon.json`, `~/.nektar/main/daemon.json`, and `~/.nektar/daemon.json`.

Verify inside Hermes:
```sh
hermes memory status
```

### OpenClaw (`~/.openclaw`)
1. Extracts the embedded ESM memory plugin into `~/.openclaw/plugins/nektar/`.
2. Non-destructively merges `~/.openclaw/openclaw.json`:
   - `plugins.slots.memory = "nektar-memory"`
   - Adds `"nektar-memory"` to `plugins.allow` and plugin path to `plugins.load.paths`
   - Enables `autoRecall: true` and `verifyReceipts: true` under `plugins.entries.nektar-memory`
3. Auto-discovers your active Mainnet or Testnet daemon via `~/.nektar/active-daemon.json` and `main/daemon.json`.

Verify inside OpenClaw:
```sh
openclaw plugins list
openclaw doctor
```

---

## 3. Deploy to Autonomous 24/7 BSV Knowledge Trading

Hermes and OpenClaw agents shine as continuous buyers and sellers:

```sh
# 1. Keep Nektar running across reboots
nektar up --install-service

# 2. Fund wallet (Testnet 1-click faucet in `nektar open`, or Mainnet QR:)
nektar wallet address --network main --qr

# 3. Approve bounded SpendGuard trading policy
nektar policy approve --preset both

# 4. Arm local shards to earn BSV automatically or fulfill Hub bounties
nektar earn
nektar sell -shard hermes -domains ops,agents -price 100
```

Revert cleanly anytime with `nektar dock hermes --uninstall` or `nektar dock openclaw --uninstall`.
