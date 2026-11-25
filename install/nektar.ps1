# nektar.ps1 — thin GHCR-backed Windows host CLI launcher.
# Zero Node.js/WSL required: runs one shared ghcr.io/kryptokrates/nektar container
# ("one brain") and docks every local Windows AI app cleanly ("many mouths").
[CmdletBinding()]
param(
  [Parameter(Position = 0)]
  [string]$Command = 'help',
  [Parameter(Position = 1, ValueFromRemainingArguments = $true)]
  [string[]]$Rest = @()
)

$ErrorActionPreference = 'Stop'
$Image = if ($env:NEKTAR_IMAGE) { $env:NEKTAR_IMAGE } elseif ($env:NEKTAR_IMAGE_TAG) { "ghcr.io/kryptokrates/nektar:$($env:NEKTAR_IMAGE_TAG)" } else { 'ghcr.io/kryptokrates/nektar:latest' }
$Container = if ($env:NEKTAR_CONTAINER_NAME) { $env:NEKTAR_CONTAINER_NAME } else { 'nektar' }
$Volume = if ($env:NEKTAR_VOLUME) { $env:NEKTAR_VOLUME } else { 'nektar-data' }
$Port = if ($env:NEKTAR_HTTP_PORT) { $env:NEKTAR_HTTP_PORT } else { '18420' }
$Network = if ($env:NEKTAR_NETWORK) { $env:NEKTAR_NETWORK } else { 'testnet' }
$StateDir = if ($env:NEKTAR_HOST_DIR) { $env:NEKTAR_HOST_DIR } else { Join-Path $HOME '.nektar' }
$DaemonJson = Join-Path $StateDir 'daemon.json'
$BaseUrl = "http://127.0.0.1:$Port"

function Show-Usage {
@"
nektar — verifiable AI agent memory & knowledge market (GHCR launcher)

Usage:
  nektar up                     Start (or adopt) your local nektar container on :$Port
  nektar open                   Open the Dashboard in your browser (authenticated)
  nektar dock [auto|<app>|proxy] Dock Claude, Cursor, Hermes, OpenClaw, Codex, VS Code, or L0 Proxy
  nektar status                 Check daemon health, shards, balance & next hints
  nektar logs [-f]              Show container logs
  nektar update                 Pull latest GHCR image and restart (keeps all data)
  nektar down                   Stop the container (keeps nektar-data volume)
  nektar policy ...             Approve/list/revoke trading policies inside the container
  nektar hub up|status|down|template  Deploy or manage a 1-click Hub Node
  nektar exec <args...>         Run any nektar subcommand inside the container
"@ | Write-Output
}

function Assert-Docker {
  if (-not (Get-Command docker -ErrorAction SilentlyContinue)) {
    throw 'nektar: Docker Desktop is required — install from https://www.docker.com/products/docker-desktop/'
  }
  $null = docker info 2>$null
  if ($LASTEXITCODE -ne 0) {
    throw 'nektar: Docker daemon is not running — start Docker Desktop and retry.'
  }
}

function Test-ContainerRunning {
  $names = docker ps --format '{{.Names}}' 2>$null
  return ($names -contains $Container)
}

function Test-ContainerExists {
  $names = docker ps -a --format '{{.Names}}' 2>$null
  return ($names -contains $Container)
}

function Sync-DaemonJson {
  New-Item -ItemType Directory -Force -Path $StateDir | Out-Null
  $tok = (docker exec $Container cat /data/http-token 2>$null)
  if ($tok) { $tok = $tok.Trim() }
  $opTok = (docker exec $Container cat /data/operator-token 2>$null)
  if ($opTok) { $opTok = $opTok.Trim() }
  $obj = @{ url = $BaseUrl; network = $Network; container = $Container }
  if ($tok) { $obj.token = $tok; $obj.authToken = $tok }
  if ($opTok) { $obj.operatorToken = $opTok }
  $obj | ConvertTo-Json -Depth 4 | Set-Content -Encoding UTF8 -Path $DaemonJson
}

function Get-LocalToken {
  if ($env:NEKTAR_HTTP_TOKEN) { return $env:NEKTAR_HTTP_TOKEN }
  if (Test-Path $DaemonJson) {
    try { return (Get-Content -Raw $DaemonJson | ConvertFrom-Json).token } catch {}
  }
  return $null
}

function Invoke-NektarUp {
  Assert-Docker
  if (Test-ContainerRunning) {
    Sync-DaemonJson
    Write-Output "nektar: already running at $BaseUrl"
    return
  }
  if (Test-ContainerExists) {
    $null = docker start $Container
  } else {
    Write-Output "nektar: pulling $Image ..."
    $null = docker pull $Image
    $auto = if ($env:NEKTAR_AUTOMATION) { $env:NEKTAR_AUTOMATION } else { '0' }
    $model = if ($env:NEKTAR_MODEL_ID) { $env:NEKTAR_MODEL_ID } else { 'unspecified' }
    $null = docker run -d --name $Container --restart unless-stopped `
      -p "127.0.0.1:${Port}:18420" -v "${Volume}:/data" `
      -e "NEKTAR_NETWORK=$Network" -e "NEKTAR_HTTP_PORT=18420" -e "NEKTAR_AUTOMATION=$auto" -e "NEKTAR_MODEL_ID=$model" `
      $Image serve -listen :18420
  }
  Write-Host -NoNewline 'nektar: waiting for ready '
  for ($i = 0; $i -lt 45; $i++) {
    try {
      $r = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$BaseUrl/v1/health"
      if ($r.StatusCode -eq 200) {
        Write-Host ' ok!'
        Sync-DaemonJson
        $logs = (docker logs --tail 25 $Container 2>&1) -join "`n"
        if ($logs -match '(?s)(╭─ nektar.*?╰─[^\n]*)') { Write-Output $Matches[1] }
        return
      }
    } catch {}
    Write-Host -NoNewline '.'
    Start-Sleep 1
  }
  throw 'nektar: container did not become healthy within 45s — run: nektar logs'
}

function Invoke-NektarDock([string[]]$DockArgs) {
  try { $null = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$BaseUrl/v1/health" } catch { Invoke-NektarUp }
  $tok = Get-LocalToken
  $mcpUrl = "$BaseUrl/mcp"
  $uninstall = $DockArgs -contains '--uninstall'
  $dry = $DockArgs -contains '--dry'
  $targets = @($DockArgs | Where-Object { -not $_.StartsWith('-') } | ForEach-Object { $_.ToLowerInvariant() })

  $hdr = @{ 'Content-Type' = 'application/json' }
  if ($tok) { $hdr['Authorization'] = "Bearer $tok" }

  function ConvertTo-HashtableCompat($Obj) {
    if ($null -eq $Obj) { return @{} }
    if ($Obj -is [System.Collections.IDictionary]) { return $Obj }
    $ht = @{}
    foreach ($p in $Obj.PSObject.Properties) {
      if ($p.Value -is [PSCustomObject]) { $ht[$p.Name] = ConvertTo-HashtableCompat $p.Value }
      else { $ht[$p.Name] = $p.Value }
    }
    return $ht
  }

  function Merge-JsonMcp([string]$Path, [string]$RootKey, [hashtable]$Entry) {
    $cfg = @{}
    if (Test-Path $Path) {
      try { $cfg = ConvertTo-HashtableCompat (Get-Content -Raw $Path | ConvertFrom-Json) } catch { $cfg = @{} }
    }
    if (-not $cfg.ContainsKey($RootKey)) { $cfg[$RootKey] = @{} }
    $bag = $cfg[$RootKey]
    if ($uninstall) {
      if ($bag.ContainsKey('nektar')) {
        $bag.Remove('nektar')
        if (-not $dry) { New-Item -ItemType Directory -Force -Path (Split-Path $Path) | Out-Null; $cfg | ConvertTo-Json -Depth 10 | Set-Content -Encoding UTF8 -Path $Path }
        return $true
      }
      return $false
    }
    if (-not $bag.ContainsKey('nektar')) {
      $bag['nektar'] = $Entry
      if (-not $dry) { New-Item -ItemType Directory -Force -Path (Split-Path $Path) | Out-Null; $cfg | ConvertTo-Json -Depth 10 | Set-Content -Encoding UTF8 -Path $Path }
      return $true
    }
    return $false
  }

  function Install-Bundle([string]$Name, [string]$Dest) {
    try {
      $b = Invoke-RestMethod -Uri "$BaseUrl/v1/adapters/bundle/$Name" -TimeoutSec 4
      foreach ($f in $b.files) {
        if ($f.path -match '\.\.' -or [System.IO.Path]::IsPathRooted($f.path)) { continue }
        $outPath = Join-Path $Dest ($f.path -replace '/', '\')
        if (-not $dry) {
          New-Item -ItemType Directory -Force -Path (Split-Path $outPath) | Out-Null
          Set-Content -Encoding UTF8 -Path $outPath -Value $f.content
        }
      }
      return $true
    } catch { return $false }
  }

  $httpEntry = @{ url = $mcpUrl }
  if ($tok) { $httpEntry['headers'] = @{ Authorization = "Bearer $tok" } }

  $detected = @()
  if ((Test-Path (Join-Path $HOME '.hermes')) -or (Test-Path (Join-Path $env:LOCALAPPDATA 'hermes'))) { $detected += 'hermes' }
  if (Test-Path (Join-Path $HOME '.openclaw')) { $detected += 'openclaw' }
  if ((Test-Path (Join-Path $HOME '.claude')) -or (Test-Path (Join-Path (Get-Location) '.claude'))) { $detected += 'claude' }
  if (Test-Path (Join-Path $HOME '.codex')) { $detected += 'codex' }
  if (Test-Path (Join-Path $HOME '.cursor')) { $detected += 'cursor' }
  if (Test-Path (Join-Path $HOME '.codeium\windsurf')) { $detected += 'windsurf' }
  if (Test-Path (Join-Path $env:APPDATA 'Code\User')) { $detected += 'vscode' }
  if (Test-Path (Join-Path $HOME '.gemini')) { $detected += 'gemini' }

  $selected = if ($targets.Count -eq 0 -or ($targets -contains 'auto')) { $detected } else { $targets }
  Write-Output "nektar dock$(if($dry){' [dry]'}) — endpoint $mcpUrl"
  if ($selected.Count -eq 0) {
    Write-Output '  no local AI apps auto-detected — run `nektar dock proxy` for any running agent or specify a client (claude, cursor, vscode, hermes, openclaw, codex)'
  }
  foreach ($c in $selected) {
    switch ($c) {
      'proxy' {
        Write-Output "`nL0 Universal LLM Memory Proxy (zero code changes):"
        Write-Output "  `$env:OPENAI_BASE_URL = `"$BaseUrl/v1/proxy/openai/v1`""
        if ($tok) { Write-Output "  `$env:OPENAI_API_KEY  = `"nkt:${tok}:`$env:OPENAI_API_KEY`"" }
        Write-Output "  `$env:ANTHROPIC_BASE_URL = `"$BaseUrl/v1/proxy/anthropic`""
        if ($tok) { Write-Output "  `$env:ANTHROPIC_API_KEY  = `"nkt:${tok}:`$env:ANTHROPIC_API_KEY`"" }
      }
      'cursor' { $ch = Merge-JsonMcp (Join-Path $HOME '.cursor\mcp.json') 'mcpServers' $httpEntry; Write-Output "  [L1] Cursor ($(if($ch){'updated'}else{'unchanged'}))" }
      'windsurf' { $ch = Merge-JsonMcp (Join-Path $HOME '.codeium\windsurf\mcp_config.json') 'mcpServers' $httpEntry; Write-Output "  [L1] Windsurf ($(if($ch){'updated'}else{'unchanged'}))" }
      'vscode' { $ch = Merge-JsonMcp (Join-Path $env:APPDATA 'Code\User\mcp.json') 'servers' (@{ type = 'http' } + $httpEntry); Write-Output "  [L1] VS Code ($(if($ch){'updated'}else{'unchanged'}))" }
      'gemini' { $ch = Merge-JsonMcp (Join-Path $HOME '.gemini\settings.json') 'mcpServers' (@{ httpUrl = $mcpUrl } + $(if($tok){@{ headers = @{ Authorization = "Bearer $tok" } }}else{@{} })); Write-Output "  [L1] Gemini / Antigravity ($(if($ch){'updated'}else{'unchanged'}))" }
      'claude' { $ch = Merge-JsonMcp (Join-Path (Get-Location) '.mcp.json') 'mcpServers' (@{ type = 'http' } + $httpEntry); Write-Output "  [L3] Claude Code ($(if($ch){'updated'}else{'unchanged'}))" }
      'hermes' { $ok = Install-Bundle 'hermes' (Join-Path $HOME '.hermes\plugins\memory\nektar'); Write-Output "  [L3] Hermes Agent ($(if($ok){'installed native MemoryProvider'}else{'checked'}))" }
      'openclaw' { $ok = Install-Bundle 'openclaw' (Join-Path $HOME '.openclaw\extensions\nektar'); Write-Output "  [L3] OpenClaw ($(if($ok){'installed native memory plugin'}else{'checked'}))" }
      default { Write-Output "  [info] use MCP URL $mcpUrl for $c" }
    }
  }
  if (-not $uninstall) {
    try {
      $rec = Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/tools/recall" -Headers $hdr -Body '{}' -TimeoutSec 3
      Write-Output "`n  live probe: ok — recall {} returned $($rec.items.Count) item(s) on [$($rec.shards -join ', ')]"
    } catch { Write-Output "`n  live probe: $_" }
  }
}

switch ($Command.ToLowerInvariant()) {
  { $_ -in @('up', 'start', 'serve') } { Invoke-NektarUp }
  { $_ -in @('down', 'stop') } { Assert-Docker; $null = docker rm -f $Container 2>$null; Write-Output "nektar: stopped (volume $Volume preserved)" }
  'restart' { Assert-Docker; $null = docker restart $Container; Write-Output 'nektar: restarted' }
  'logs' { Assert-Docker; & docker logs @Rest $Container }
  'open' {
    try { $null = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$BaseUrl/v1/health" } catch { Invoke-NektarUp }
    $logs = (docker logs --tail 40 $Container 2>&1) -join "`n"
    $target = if ($logs -match '(http://127\.0\.0\.1:\d+/pair\?code=[a-z0-9-]+)') { $Matches[1] } else { $BaseUrl }
    Start-Process $target
  }
  'status' {
    try {
      $null = Invoke-WebRequest -UseBasicParsing -TimeoutSec 2 "$BaseUrl/v1/health"
      Write-Output "nektar status: UP at $BaseUrl ($Network)"
      $tok = Get-LocalToken
      $hdr = @{ 'Content-Type' = 'application/json' }
      if ($tok) { $hdr['Authorization'] = "Bearer $tok" }
      $doc = Invoke-RestMethod -Method Post -Uri "$BaseUrl/v1/tools/doctor" -Headers $hdr -Body '{}' -TimeoutSec 3
      Write-Output "  memory : $($doc.records) record(s) across $($doc.shards) shard(s) [$($doc.shardNames -join ', ')]"
    } catch {
      Write-Output 'nektar status: STOPPED — run `nektar up` to start'
      exit 1
    }
  }
  'update' {
    Assert-Docker
    Write-Output "nektar: pulling latest $Image ..."
    & docker pull $Image
    $null = docker rm -f $Container 2>$null
    Invoke-NektarUp
  }
  'dock' { Invoke-NektarDock $Rest }
  'hub' {
    Assert-Docker
    if (Test-ContainerRunning) { & docker exec $Container nektar hub @Rest }
    else { & docker run --rm $Image hub @Rest }
  }
  { $_ -in @('policy', 'doctor', 'check-mainnet', 'wallet', 'remember', 'recall', 'anchor', 'verify', 'version') } {
    Assert-Docker
    if (Test-ContainerRunning) { & docker exec -i $Container nektar $Command @Rest }
    else { & docker run --rm -i -v "${Volume}:/data" -e "NEKTAR_NETWORK=$Network" $Image $Command @Rest }
  }
  'exec' { Assert-Docker; & docker exec -i $Container nektar @Rest }
  { $_ -in @('help', '-h', '--help') } { Show-Usage }
  default { Write-Error "nektar: unknown command '$Command'"; Show-Usage; exit 2 }
}
