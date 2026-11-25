# nektar one-line Windows installer:
#   irm https://nektar.run/install.ps1 | iex
#   & ([scriptblock]::Create((irm https://nektar.run/install.ps1))) -Dock
#   & ([scriptblock]::Create((irm https://nektar.run/install.ps1))) -Native   # host build for BSV Desktop
param(
  [switch]$Dock,
  [switch]$Native
)

$ErrorActionPreference = 'Stop'
$RawBase = if ($env:NEKTAR_RAW_BASE) { $env:NEKTAR_RAW_BASE } else { 'https://raw.githubusercontent.com/kryptokrates/nektar/master' }
$InstallDir = if ($env:NEKTAR_BIN_DIR) { $env:NEKTAR_BIN_DIR } else { Join-Path $env:LOCALAPPDATA 'nektar' }

Write-Output "`n== nektar install (GHCR-first, Windows) =="
New-Item -ItemType Directory -Force -Path $InstallDir | Out-Null

if ($Native) {
  $zip = Join-Path $env:TEMP 'nektar-mcp-win32-x64.zip'
  Write-Output '-- downloading nektar-mcp-win32-x64.zip from latest GitHub release'
  Invoke-WebRequest -UseBasicParsing -Uri 'https://github.com/kryptokrates/nektar/releases/latest/download/nektar-mcp-win32-x64.zip' -OutFile $zip
  Expand-Archive -Path $zip -DestinationPath $InstallDir -Force
  Remove-Item $zip -Force -ErrorAction SilentlyContinue
} else {
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw 'Docker Desktop is required for GHCR install: https://www.docker.com/products/docker-desktop/ (or re-run with -Native for the Node.js host build)'
  }
  $null = docker info 2>$null
  if ($LASTEXITCODE -ne 0) {
    throw 'Docker Desktop is installed but not running — start Docker Desktop and re-run.'
  }
  $localDir = if ($PSScriptRoot) { $PSScriptRoot } else { '' }
  if ($localDir -and (Test-Path (Join-Path $localDir 'nektar.ps1'))) {
    Copy-Item (Join-Path $localDir 'nektar.ps1') (Join-Path $InstallDir 'nektar.ps1') -Force
    Copy-Item (Join-Path $localDir 'nektar.cmd') (Join-Path $InstallDir 'nektar.cmd') -Force
  } else {
    Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.ps1" -OutFile (Join-Path $InstallDir 'nektar.ps1')
    Invoke-WebRequest -UseBasicParsing -Uri "$RawBase/install/nektar.cmd" -OutFile (Join-Path $InstallDir 'nektar.cmd')
  }
}

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
if (-not $userPath -or ($userPath -split ';' | Where-Object { $_ -ieq $InstallDir }).Count -eq 0) {
  $newPath = if ($userPath) { "$userPath;$InstallDir" } else { $InstallDir }
  [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
  $env:Path = "$env:Path;$InstallDir"
  Write-Output "Added $InstallDir to User PATH"
}

Write-Output "Installed nektar -> $InstallDir"

if (-not $Native) {
  & (Join-Path $InstallDir 'nektar.cmd') up
  if ($Dock) {
    & (Join-Path $InstallDir 'nektar.cmd') dock auto
  } else {
    Write-Output "`nNext:"
    Write-Output "  nektar open        # open Dashboard (1-click testnet faucet + live recall)"
    Write-Output "  nektar dock        # wire Claude Code, Cursor, VS Code, Hermes, OpenClaw, Codex..."
    Write-Output "  nektar dock proxy  # get OPENAI_BASE_URL for any existing running agent"
    Write-Output "  nektar hub up      # graduate to a public Hub Node when ready`n"
  }
}
