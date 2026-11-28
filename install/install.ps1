# nektar v0.1 zero-Docker native Windows installer:
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up
#   & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up -Mainnet -Confirm I_UNDERSTAND
#   irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1 | iex
param(
  [switch]$Up,
  [switch]$Mainnet,
  [string]$Confirm = '',
  [string]$Wallet = '',
  [switch]$InstallService,
  [switch]$Dock,
  [switch]$Docker,
  [switch]$Native
)

$ErrorActionPreference = 'Stop'
$RawBase = if ($env:NEKTAR_RAW_BASE) { $env:NEKTAR_RAW_BASE } else { 'https://raw.githubusercontent.com/kryptokrates/nektar/master' }
$InstallDir = if ($env:NEKTAR_BIN_DIR) { $env:NEKTAR_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'nektar' }

if ($Mainnet) {
  $conf = if ($Confirm) { $Confirm } else { $env:NEKTAR_MAINNET_CONFIRM }
  if ($conf -ne 'I_UNDERSTAND') {
    [Console]::Error.WriteLine('ERROR: -Mainnet spends REAL BSV and requires explicit -Confirm I_UNDERSTAND (or $env:NEKTAR_MAINNET_CONFIRM="I_UNDERSTAND").')
    exit 2
  }
}

New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

if ($Docker) {
  Write-Output "`n== nektar v0.1 install (Docker / GHCR opt-in, Windows) =="
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw 'Docker Desktop is required for -Docker mode. Re-run without -Docker for the zero-dependency native Go binary.'
  }
  Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.ps1" -OutFile (Join-Path $InstallDir 'nektar.ps1')
  Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.cmd" -OutFile (Join-Path $InstallDir 'nektar.cmd')
} else {
  Write-Output "`n== nektar v0.1 install (native single static Go binary — zero Docker, zero Node.js) =="
  $zip = Join-Path $env:TEMP 'nektar-windows-amd64.zip'
  $sums = Join-Path $env:TEMP 'nektar-SHA256SUMS'
  $baseDl = 'https://github.com/kryptokrates/nektar/releases/latest/download'
  Write-Output '-- downloading nektar-windows-amd64.zip and SHA256SUMS from latest GitHub release'
  Invoke-WebRequest -UseBasicParsing -Uri "$baseDl/nektar-windows-amd64.zip" -OutFile $zip
  try {
    Invoke-WebRequest -UseBasicParsing -Uri "$baseDl/SHA256SUMS" -OutFile $sums
    $line = Get-Content $sums | Where-Object { $_ -match 'nektar-windows-amd64\.zip$' } | Select-Object -First 1
    if (-not $line) {
      throw 'nektar-windows-amd64.zip missing from SHA256SUMS — refusing unverified binary'
    }
    $want = ($line -split '\s+')[0].ToLowerInvariant()
    if (-not $want -or $want.Length -ne 64) {
      throw 'Invalid SHA-256 entry in SHA256SUMS — refusing unverified binary'
    }
    $got = (Get-FileHash -Algorithm SHA256 -Path $zip).Hash.ToLowerInvariant()
    if ($got -ne $want) {
      throw "SHA-256 mismatch for nektar-windows-amd64.zip (got $got, want $want)"
    }
    Write-Output '✓ verified SHA-256 checksum for nektar-windows-amd64.zip'
  } finally {
    Remove-Item $sums -Force -ErrorAction SilentlyContinue
  }
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

if ($Up) {
  $upArgs = @('up')
  if ($Mainnet) {
    $conf = if ($Confirm) { $Confirm } else { $env:NEKTAR_MAINNET_CONFIRM }
    if ($conf -ne 'I_UNDERSTAND') {
      [Console]::Error.WriteLine('ERROR: -Mainnet spends REAL BSV and requires explicit -Confirm I_UNDERSTAND (or $env:NEKTAR_MAINNET_CONFIRM="I_UNDERSTAND").')
      exit 2
    }
    $upArgs += @('--network', 'main', '--confirm', $conf)
  }
  if ($Wallet) {
    $upArgs += @('--wallet', $Wallet)
  }
  if ($InstallService) {
    $upArgs += '--install-service'
  }
  & $exe @upArgs
} elseif ($Dock) {
  & $exe dock auto
} else {
  Write-Output "`nNext (25 seconds to live docked agents — zero Docker, zero sidecars):"
  Write-Output "  nektar up              # start background daemon + auto-dock Claude/Hermes/Codex/Antigravity/Cursor"
  Write-Output "  nektar up --mainnet --confirm I_UNDERSTAND   # go live on BSV Mainnet"
  Write-Output "  nektar hub up --auto-ip                      # launch a zero-Docker LightBSV Hub"
  Write-Output "  nektar open            # open Dashboard (1-click recall, capsules, wallet & Hubs)"
  Write-Output "  nektar doctor          # check wallet, durable workflows, policies, Hubs & cohort health`n"
}
