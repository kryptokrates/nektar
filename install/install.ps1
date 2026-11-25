# nektar v0.4 zero-Docker native Windows installer:
#   irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Dock
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Docker
param(
  [switch]$Dock,
  [switch]$Docker,
  [switch]$Native
)

$ErrorActionPreference = 'Stop'
$RawBase = if ($env:NEKTAR_RAW_BASE) { $env:NEKTAR_RAW_BASE } else { 'https://raw.githubusercontent.com/kryptokrates/nektar/master' }
$InstallDir = if ($env:NEKTAR_BIN_DIR) { $env:NEKTAR_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'nektar' }

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

if ($Docker) {
  Write-Output "`n== nektar v0.4 install (Docker / GHCR opt-in, Windows) =="
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw 'Docker Desktop is required for -Docker mode. Re-run without -Docker for the zero-dependency native Go binary.'
  }
  Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.ps1" -OutFile (Join-Path $InstallDir 'nektar.ps1')
  Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.cmd" -OutFile (Join-Path $InstallDir 'nektar.cmd')
} else {
  Write-Output "`n== nektar v0.4 install (native single static Go binary — zero Docker, zero Node.js) =="
  $zip = Join-Path $env:TEMP 'nektar-windows-amd64.zip'
  Write-Output '-- downloading nektar-windows-amd64.zip from latest GitHub release'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/kryptokrates/nektar/releases/latest/download/nektar-windows-amd64.zip' -OutFile $zip
  Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
  Remove-Item $zip -Force -ErrorAction SilentlyContinue
  Write-Output "✓ installed native nektar.exe -> $InstallDir\nektar.exe"
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath -or ($userPath -split ';' | Where-Object { $_ -ieq $InstallDir }).Count -eq 0) {
  $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  $env:Path = "$env:Path;$InstallDir"
  Write-Output "Added $InstallDir to User PATH"
}

$exe = if ($Docker) { Join-Path $InstallDir 'nektar.cmd' } else { Join-Path $InstallDir 'nektar.exe' }

if ($Dock) {
  & $exe dock auto
} else {
  Write-Output "`nNext (zero Docker, starts in <50ms on http://127.0.0.1:18420):"
  Write-Output "  nektar serve           # start unified agent daemon + Svelte Dashboard + MCP + Market Broker"
  Write-Output "  nektar serve --hub     # start agent + embedded durable SQLite Overlay Hub + Bounties"
  Write-Output "  nektar dock            # auto-wire Claude Code, Cursor, VS Code, Windsurf, Codex, Hermes, OpenClaw..."
  Write-Output "  nektar open            # open Dashboard (1-click recall, faucet, policy presets & peers)"
  Write-Output "  nektar doctor          # check wallet, policies, peers & SSE health`n"
}
