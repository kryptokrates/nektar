<#
.SYNOPSIS
  Nektar v0.1 — Real BSV Testnet Spine-and-Leaf Heavy Load & Chaos Runner (WSL2 + Restate v1.8.1).
.DESCRIPTION
  Cross-compiles zero-CGO Linux `nektar` + `testnet_load_driver`, stages the funded local
  Testnet Spine & mined seller shards onto native WSL2 ext4 (avoiding 9p SQLite lock quirks),
  runs the full 9-process Spine-and-Leaf + Restate v1.8.1 Load & Chaos Test, and writes a JSON scorecard.
.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\run_testnet_load.ps1
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\run_testnet_load.ps1 -Network memory
#>
param(
    [ValidateSet("testnet", "main", "memory")]
    [string]$Network = "testnet",
    [string]$ConfirmMainnet = "",
    [int]$Remembers = 120,
    [int]$Anchors = 12,
    [int]$Verifies = 1000,
    [int]$Trades = 24,
    [int]$TradeStaggerMs = 0,
    [switch]$SweepBack,
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
if ($Network -eq "main" -and $ConfirmMainnet -ne "I_UNDERSTAND") {
    throw "Refusing -Network main without -ConfirmMainnet I_UNDERSTAND"
}
$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$goExe    = if (Test-Path "$env:USERPROFILE\.local\go\bin\go.exe") { "$env:USERPROFILE\.local\go\bin\go.exe" } else { "go" }
$reportWin = Join-Path $repoRoot "testnet-load-scorecard.json"

if (-not $SkipBuild) {
    Write-Host "=== Cross-compiling zero-CGO Linux nektar & load driver ==="
    $env:CGO_ENABLED = "0"
    $env:GOOS = "linux"
    $env:GOARCH = "amd64"
    Push-Location $repoRoot
    try {
        & $goExe build -trimpath -ldflags="-s -w" -o .\nektar-linux ./cmd/nektar
        if ($LASTEXITCODE -ne 0) { throw "build nektar-linux failed" }
        & $goExe build -tags testnetload -trimpath -ldflags="-s -w" -o .\testnet-load-linux .\scripts\testnet_load_driver.go
        if ($LASTEXITCODE -ne 0) { throw "build testnet-load-linux failed" }
    } finally {
        Pop-Location
        Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
    }
}

$wslRepo = "/mnt/" + $repoRoot.Substring(0,1).ToLower() + ($repoRoot.Substring(2) -replace '\\','/')
$winHome = $env:USERPROFILE
$wslSeed = "/mnt/" + $winHome.Substring(0,1).ToLower() + ($winHome.Substring(2) -replace '\\','/') + "/.nektar/testnet-4agent-e2e"
$wslMainSeed = "/mnt/" + $winHome.Substring(0,1).ToLower() + ($winHome.Substring(2) -replace '\\','/') + "/.nektar/mainnet"
$wslReport = "$wslRepo/testnet-load-scorecard.json"
$extraFlags = ""
if ($Network -eq "main") {
    $extraFlags += " -confirm-mainnet=I_UNDERSTAND"
}
if ($SweepBack) {
    $extraFlags += " -sweep-back=true"
}

$bash = @"
set -euo pipefail
pkill -9 -x nektar 2>/dev/null || true
pkill -9 -x restate-server 2>/dev/null || true
pkill -9 -x testnet-load 2>/dev/null || true
WORK_DIR="`$HOME/nektar-load-$Network"
if [ "$Network" = "memory" ]; then
  rm -rf "`$WORK_DIR"
  mkdir -p "`$WORK_DIR"
elif [ "$Network" = "main" ]; then
  mkdir -p "`$WORK_DIR"
  if [ ! -d "`$WORK_DIR/spine/wallet" ]; then
    echo "Staging funded Mainnet Spine wallet from $wslMainSeed into `$WORK_DIR/spine..."
    cp -a "$wslMainSeed" "`$WORK_DIR/spine"
    rm -f "`$WORK_DIR"/spine/daemon.json "`$WORK_DIR"/spine/*.lock "`$WORK_DIR"/spine/restate-server.pid "`$WORK_DIR"/spine/hub.sqlite*
  fi
else
  mkdir -p "`$WORK_DIR"
  if [ ! -d "`$WORK_DIR/spine/wallet" ]; then
    echo "Staging funded Testnet Spine & pre-mined seller shards into `$WORK_DIR..."
    cp -a "$wslSeed/hub"       "`$WORK_DIR/spine"
    cp -a "$wslSeed/alpha"     "`$WORK_DIR/alpha"
    cp -a "$wslSeed/beta"      "`$WORK_DIR/beta"
    cp -a "$wslSeed/gamma"     "`$WORK_DIR/gamma"
    cp -a "$wslSeed/delta-nat" "`$WORK_DIR/delta-nat"
    rm -f "`$WORK_DIR"/*/daemon.json "`$WORK_DIR"/*/*.lock "`$WORK_DIR"/*/restate-server.pid "`$WORK_DIR"/spine/hub.sqlite*
  fi
fi
cp "$wslRepo/nektar-linux" "`$WORK_DIR/nektar"
cp "$wslRepo/testnet-load-linux" "`$WORK_DIR/testnet-load"
chmod +x "`$WORK_DIR/nektar" "`$WORK_DIR/testnet-load"
"`$WORK_DIR/testnet-load" -bin "`$WORK_DIR/nektar" -network "$Network" -work-dir "`$WORK_DIR" -remembers $Remembers -anchors $Anchors -verifies $Verifies -trades $Trades -trade-stagger-ms $TradeStaggerMs -restate=true -report "$wslReport"$extraFlags
if [ "$Network" = "main" ] && [ -d "`$WORK_DIR/spine/wallet" ]; then
  echo "Syncing updated Mainnet Spine wallet back to $wslMainSeed/wallet..."
  mkdir -p "$wslMainSeed/wallet"
  cp -a "`$WORK_DIR/spine/wallet/." "$wslMainSeed/wallet/"
fi

"@

$bash -replace "`r`n", "`n" | wsl -d Ubuntu -- bash -s
if ($LASTEXITCODE -ne 0) {
    throw "Testnet load test exited with code $LASTEXITCODE"
}
Write-Host "`nScorecard saved to: $reportWin"
