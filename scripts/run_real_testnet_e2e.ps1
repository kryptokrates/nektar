<#
.SYNOPSIS
  Nektar v0.4 — LIVE BSV Testnet Heterogeneous 4-Agent Real-World E2E Suite!
.DESCRIPTION
  Runs 5 live compiled `nektar.exe` processes on REAL BSV Testnet (`woc:test` + GorillaPool Arcade),
  driving each of the 4 agents through a DIFFERENT REAL-WORLD INTEGRATION HARNESS:
    - Hub   (:18620) : Durable SQLite Overlay Hub + Curated Shards + Bounties + Faucet + Svelte 5 Dashboard
    - Alpha (:18621) : REAL Python Hermes Agent (`integrations/hermes/hermes_memory_nektar/provider.py` via `uv run python`)
    - Beta  (:18622) : REAL Claude Code / Codex CLI stdio MCP client (`nektar.exe mcp`) + native Go `nektar hook` (session-start, prefetch, verify-receipt)
    - Gamma (:18623) : REAL Cursor / OpenAI / Anthropic Streamable HTTP MCP (`/mcp`) + L0 Transparent OpenAI & Anthropic Proxy + Borg BRC-369
    - Delta (:18624) : REAL NAT-only A2A Agent (`/.well-known/agent.json` + JSON-RPC 2.0 `POST /a2a`, ZERO open inbound ports -> Hub `/bbox` relay)
.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\run_real_testnet_e2e.ps1
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\run_real_testnet_e2e.ps1 -Interactive
#>
param(
    [ValidateSet("testnet", "memory")]
    [string]$Network = "testnet",
    [string]$DataRoot = "",
    [switch]$Interactive,
    [switch]$WaitMine,
    [switch]$SkipBuild
)

$ErrorActionPreference = "Stop"
$env:PATH = "$env:USERPROFILE\.local\go\bin;$env:USERPROFILE\.local\bin;$env:PATH"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$exePath  = Join-Path $repoRoot "nektar.exe"
$hermesPy = Join-Path $repoRoot "scripts\hermes_agent_runner.py"
$agentsJS = Join-Path $repoRoot "scripts\testnet_real_agents_e2e.mjs"

if (-not $SkipBuild) {
    Write-Host "=== Building zero-CGO production nektar.exe ==="
    $env:CGO_ENABLED = "0"
    Push-Location $repoRoot
    go build -trimpath -ldflags="-s -w" -o $exePath ./cmd/nektar
    Pop-Location
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}

if (-not $DataRoot) {
    if ($Network -eq "testnet") {
        $DataRoot = Join-Path $env:USERPROFILE ".nektar\testnet-4agent-e2e"
    } else {
        $DataRoot = Join-Path $env:TEMP ("nektar-real-agents-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
    }
}

$dirs = @{
    hub   = Join-Path $DataRoot "hub"
    alpha = Join-Path $DataRoot "alpha"
    beta  = Join-Path $DataRoot "beta"
    gamma = Join-Path $DataRoot "gamma"
    delta = Join-Path $DataRoot "delta-nat"
}
foreach ($d in $dirs.Values) { New-Item -ItemType Directory -Force -Path $d | Out-Null }

$urls = @{
    hub   = "http://127.0.0.1:18620"
    alpha = "http://127.0.0.1:18621"
    beta  = "http://127.0.0.1:18622"
    gamma = "http://127.0.0.1:18623"
    delta = "http://127.0.0.1:18624"
}
$hubEndpoint = "$($urls.hub)/hub"
$mockLLMPort = 18699
$procs = @{}
$txList = New-Object System.Collections.Generic.List[object]
$tradeList = New-Object System.Collections.Generic.List[object]

function Start-NektarNode($key, $argList, $extraEnv = @{}) {
    $outLog = Join-Path $dirs[$key] "stdout.log"
    $errLog = Join-Path $dirs[$key] "stderr.log"
    $saved = @{}
    foreach ($ek in $extraEnv.Keys) {
        $saved[$ek] = [Environment]::GetEnvironmentVariable($ek, "Process")
        [Environment]::SetEnvironmentVariable($ek, $extraEnv[$ek], "Process")
    }
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $p = Start-Process -FilePath $exePath -ArgumentList $argList -PassThru -NoNewWindow -RedirectStandardOutput $outLog -RedirectStandardError $errLog
    foreach ($ek in $extraEnv.Keys) {
        [Environment]::SetEnvironmentVariable($ek, $saved[$ek], "Process")
    }
    $procs[$key] = $p
    for ($i = 0; $i -lt 80; $i++) {
        try {
            $h = Invoke-RestMethod -Uri "$($urls[$key])/v1/health" -TimeoutSec 2
            if ($h.ok) {
                $sw.Stop()
                Write-Host ("[OK] {0,-10} healthy at {1} (PID={2}, boot={3}ms)" -f $key, $urls[$key], $p.Id, $sw.ElapsedMilliseconds)
                return
            }
        } catch {}
        Start-Sleep -Milliseconds 150
    }
    $errTail = if (Test-Path $errLog) { Get-Content $errLog -Tail 25 | Out-String } else { "" }
    throw "$key failed to boot at $($urls[$key]):`n$errTail"
}

function Record-Tx($label, $txid) {
    if (-not $txid) { return }
    $entry = [pscustomobject]@{
        Label  = $label
        TxID   = $txid
        WoC    = if ($Network -eq "testnet") { "https://test.whatsonchain.com/tx/$txid" } else { "(memory)" }
        Banana = if ($Network -eq "testnet") { "https://test.bananablocks.com/tx/$txid" } else { "(memory)" }
    }
    $txList.Add($entry)
    if ($Network -eq "testnet") {
        $seen = $false
        for ($i = 0; $i -lt 4; $i++) {
            try {
                $tx = Invoke-RestMethod -Uri "https://api.whatsonchain.com/v1/bsv/test/tx/hash/$txid" -TimeoutSec 8
                if ($tx.txid -eq $txid) {
                    Write-Host ("     [ON-CHAIN TESTNET VERIFIED] {0} ({1}B) -> https://test.bananablocks.com/tx/{2}" -f $label, $tx.size, $txid) -ForegroundColor Cyan
                    $seen = $true
                    break
                }
            } catch {}
            Start-Sleep -Milliseconds 1200
        }
        if (-not $seen) {
            Write-Host ("     [ARCADE BROADCAST] {0} -> https://test.bananablocks.com/tx/{1}" -f $label, $txid) -ForegroundColor Yellow
        }
    }
}

function Sync-Wallet($key) {
    try { return Invoke-RestMethod -Method Post -Uri "$($urls[$key])/v1/wallet/sync" -TimeoutSec 30 }
    catch { return Invoke-RestMethod -Uri "$($urls[$key])/v1/wallet" -TimeoutSec 10 }
}

function Run-Hermes($phase, $tag) {
    $out = (& uv run --no-project python $hermesPy $phase $dirs.alpha $tag) -join "`n"
    if ($LASTEXITCODE -ne 0) { throw "Hermes runner ($phase) failed: $out" }
    return ($out | ConvertFrom-Json)
}

function Run-NodeAgent($role, $phase, $argList) {
    $allArgs = @($agentsJS, $role, $phase) + $argList
    $out = (& node @allArgs) -join "`n"
    if ($LASTEXITCODE -ne 0) { throw "Node agent $role ($phase) failed: $out" }
    return ($out | ConvertFrom-Json)
}

try {
    Write-Host "`n========================================================================="
    Write-Host " NEKTAR v0.4 — LIVE $($Network.ToUpper()) HETEROGENEOUS 4-AGENT REAL-WORLD E2E"
    Write-Host "   Hub   (:18620) = Durable SQLite Overlay Hub + Bounties + Svelte 5 UI"
    Write-Host "   Alpha (:18621) = REAL Python Hermes Agent (NektarMemoryProvider)"
    Write-Host "   Beta  (:18622) = REAL Claude Code / Codex stdio MCP + native hooks"
    Write-Host "   Gamma (:18623) = REAL Cursor Streamable HTTP MCP + L0 OpenAI/Anthropic Proxy"
    Write-Host "   Delta (:18624) = REAL NAT-only A2A JSON-RPC Agent (Hub /bbox relay)"
    Write-Host "=========================================================================`n"

    # Start local mock LLM upstream for Gamma's real L0 OpenAI & Anthropic proxy tests!
    $mockLLM = Start-Process -FilePath "node" -ArgumentList @($agentsJS, "mock-llm", "$mockLLMPort") -PassThru -NoNewWindow
    $procs["mockllm"] = $mockLLM
    Start-Sleep -Milliseconds 350

    # Launch all 5 real nektar.exe processes!
    Start-NektarNode "hub"   @("serve","--hub","--listen","127.0.0.1:18620","--public-url",$urls.hub,"--data-dir",$dirs.hub,"--network",$Network)
    Start-NektarNode "alpha" @("serve","--listen","127.0.0.1:18621","--public-url",$urls.alpha,"--hosts",$hubEndpoint,"--data-dir",$dirs.alpha,"--network",$Network)
    Start-NektarNode "beta"  @("serve","--listen","127.0.0.1:18622","--public-url",$urls.beta,"--hosts",$hubEndpoint,"--data-dir",$dirs.beta,"--network",$Network)
    Start-NektarNode "gamma" @("serve","--listen","127.0.0.1:18623","--public-url",$urls.gamma,"--hosts",$hubEndpoint,"--data-dir",$dirs.gamma,"--network",$Network) @{
        NEKTAR_OPENAI_UPSTREAM    = "http://127.0.0.1:$mockLLMPort"
        NEKTAR_ANTHROPIC_UPSTREAM = "http://127.0.0.1:$mockLLMPort"
    }
    # Delta is NAT-only (NO --public-url)!
    Start-NektarNode "delta" @("serve","--listen","127.0.0.1:18624","--hosts",$hubEndpoint,"--data-dir",$dirs.delta,"--network",$Network)

    Start-Sleep -Milliseconds 800

    # Stage 0: Sync Hub & Auto-Fund All 4 Heterogeneous Agents from Hub Faucet on Real BSV Testnet!
    Write-Host "`n=== Stage 0: Live $Network Wallet Sync & Hub -> 4-Agent Auto-Funding ==="
    $w = @{}
    foreach ($k in @("hub","alpha","beta","gamma","delta")) {
        $null = Sync-Wallet $k
        $w[$k] = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
        Write-Host ("  {0,-6} addr={1}  balance={2,9} sats  utxos={3}" -f $k, $w[$k].fundAddress, $w[$k].balanceSats, $w[$k].utxos)
    }
    if ($Network -eq "testnet") {
        if ($w["hub"].balanceSats -lt 20000) {
            throw "Hub needs >= 20,000 tBSV at $($w['hub'].fundAddress) (currently $($w['hub'].balanceSats) sats)."
        }
        foreach ($k in @("alpha","beta","gamma","delta")) {
            if ($w[$k].balanceSats -lt 3500) {
                $hd = Invoke-RestMethod -Method Post -Uri "$($urls.hub)/v1/faucet" -ContentType "application/json" -Body (@{address=$w[$k].fundAddress} | ConvertTo-Json) -TimeoutSec 30
                Write-Host ("  [Hub -> {0}] funded +{1} tBSV sats" -f $k, $hd.sats) -ForegroundColor Green
                Record-Tx "fund-$k" $hd.txid
                Start-Sleep -Milliseconds 700
            }
        }
        Start-Sleep -Seconds 2
        foreach ($k in @("hub","alpha","beta","gamma","delta")) {
            $null = Sync-Wallet $k
            $w[$k] = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
            Write-Host ("  [synced] {0,-6} balance={1,9} sats  utxos={2}" -f $k, $w[$k].balanceSats, $w[$k].utxos)
        }
    }

    # Approve operator buyer+seller policies on all 4 agents (simulating Dashboard 1-click policy preset)
    foreach ($k in @("alpha","beta","gamma","delta")) {
        $null = Invoke-RestMethod -Method Post -Uri "$($urls[$k])/v1/onboard/policy-preset" -ContentType "application/json" -Body '{"preset":"both"}'
    }

    $runTag = [guid]::NewGuid().ToString("N").Substring(0, 8)

    # Stage 1: Real-World Heterogeneous Agent Setup (Anchoring + Listing via 4 Different Client Stacks!)
    Write-Host "`n=== Stage 1: 4 Heterogeneous Real-World Agent Stacks Boot, Anchor & List ==="
    $alphaSetup = Run-Hermes "setup" $runTag
    Write-Host "[OK] Agent 1 (HERMES Python NektarMemoryProvider) discovered $($alphaSetup.discoveredUrl), mirrored memory via on_memory_write, anchored & listed ops @ 70 sats" -ForegroundColor Green
    Record-Tx "alpha-hermes-anchor" $alphaSetup.anchorTxid

    $betaSetup = Run-NodeAgent "beta" "setup" @($exePath, $dirs.beta, $runTag)
    Write-Host "[OK] Agent 2 (CLAUDE/CODEX stdio MCP + native SessionStart hook) exposed $($betaSetup.mcpToolsCount) tools, anchored & listed ops @ 55 sats + bsv @ 65 sats" -ForegroundColor Green
    Record-Tx "beta-codex-ops-anchor" $betaSetup.opsAnchorTxid
    Record-Tx "beta-codex-bsv-anchor" $betaSetup.bsvAnchorTxid

    $gammaSetup = Run-NodeAgent "gamma" "setup" @($urls.gamma, $runTag, $w["alpha"].identityPub)
    Write-Host "[OK] Agent 3 (CURSOR Streamable HTTP /mcp + L0 Proxy) anchored & listed agents @ 60 sats" -ForegroundColor Green
    Record-Tx "gamma-httpmcp-anchor" $gammaSetup.anchorTxid

    $deltaSetup = Run-NodeAgent "delta" "setup" @($urls.delta, $runTag)
    Write-Host "[OK] Agent 4 (A2A JSON-RPC behind NAT, zero open ports) verified A2A card, anchored & listed nat @ 50 sats" -ForegroundColor Green
    Record-Tx "delta-a2a-nat-anchor" $deltaSetup.anchorTxid

    Start-Sleep -Milliseconds 800

    # Stage 2: Full 4-Agent Cross-Protocol Trading Mesh on Live BSV Testnet!
    Write-Host "`n=== Stage 2: Cross-Protocol 4-Agent Trading Mesh + Hub Bounties + Verification ==="

    # 2a. Gamma (Streamable HTTP MCP) buys ops -> Beta (Codex MCP, 55 sats) beats Alpha (Hermes, 70 sats) & Hub (90 sats) + L0 OpenAI/Anthropic Proxy + Borg Revocation!
    $gammaTrade = Run-NodeAgent "gamma" "trade" @($urls.gamma, $runTag, $w["alpha"].identityPub)
    $tradeList.Add([pscustomobject]@{ Route="Gamma(HTTP-MCP) <- Beta(Codex-MCP) [competed vs Alpha]"; Sats=$gammaTrade.deal.priceSats; FirstOfferMs=$gammaTrade.deal.firstOfferMs; TotalMs=$gammaTrade.deal.totalMs; TxID=$gammaTrade.deal.paymentTxid })
    Write-Host ("[OK] Gamma(HTTP-MCP) <- Beta(Codex-MCP) settled={0} sats | firstOffer={1}ms | total={2}ms | L0 OpenAI+Anthropic Proxy=PASS | Borg epoch={3}" -f $gammaTrade.deal.priceSats, $gammaTrade.deal.firstOfferMs, $gammaTrade.deal.totalMs, $gammaTrade.borgRevokedEpoch) -ForegroundColor Green
    Record-Tx "trade-gamma-from-beta" $gammaTrade.deal.paymentTxid

    # 2b. Alpha (Hermes Python Provider) buys agents from Gamma + sells to Hub bounty-ops + verifies receipt via Hermes sync_turn()!
    $alphaTrade = Run-Hermes "trade" $runTag
    $tradeList.Add([pscustomobject]@{ Route="Alpha(Hermes-Py) <- Gamma(HTTP-MCP)"; Sats=$alphaTrade.deal.priceSats; FirstOfferMs=$alphaTrade.deal.firstOfferMs; TotalMs=$alphaTrade.deal.totalMs; TxID=$alphaTrade.deal.paymentTxid })
    Write-Host ("[OK] Alpha(Hermes-Py) <- Gamma(HTTP-MCP) settled={0} sats | firstOffer={1}ms | total={2}ms | Hermes sync_turn receipt=PASS" -f $alphaTrade.deal.priceSats, $alphaTrade.deal.firstOfferMs, $alphaTrade.deal.totalMs) -ForegroundColor Green
    Record-Tx "trade-alpha-from-gamma" $alphaTrade.deal.paymentTxid
    Write-Host ("[OK] Alpha(Hermes-Py) -> Hub bounty-ops PAID +{0} sats (curated/{1} seq={2})" -f $alphaTrade.bounty.rewardSats, $alphaTrade.bounty.shard, $alphaTrade.bounty.seq) -ForegroundColor Green
    Record-Tx "bounty-alpha-to-hub" $alphaTrade.bounty.txid

    # 2c. Beta (Codex/Claude stdio MCP + native hooks) buys nat from NAT-only Delta (A2A) purely over Hub /bbox!
    $betaTrade = Run-NodeAgent "beta" "trade" @($exePath, $dirs.beta, $runTag)
    $tradeList.Add([pscustomobject]@{ Route="Beta(Codex-MCP) <- Delta(A2A-NAT /bbox)"; Sats=$betaTrade.deal.priceSats; FirstOfferMs=$betaTrade.deal.firstOfferMs; TotalMs=$betaTrade.deal.totalMs; TxID=$betaTrade.deal.paymentTxid })
    Write-Host ("[OK] Beta(Codex-MCP) <- Delta(A2A-NAT) via /bbox settled={0} sats | firstOffer={1}ms | total={2}ms | hooks prefetch+verify={3}" -f $betaTrade.deal.priceSats, $betaTrade.deal.firstOfferMs, $betaTrade.deal.totalMs, $betaTrade.hookVerdict) -ForegroundColor Green
    Record-Tx "trade-beta-from-delta-nat" $betaTrade.deal.paymentTxid

    # 2d. Delta (NAT-only A2A JSON-RPC) discovers >=5 peers, buys bsv from Beta, produces & verifies receipt via pure /a2a!
    $deltaTrade = Run-NodeAgent "delta" "trade" @($urls.delta, $runTag)
    $tradeList.Add([pscustomobject]@{ Route="Delta(A2A-NAT) <- Beta(Codex-MCP)"; Sats=$deltaTrade.deal.priceSats; FirstOfferMs=$deltaTrade.deal.firstOfferMs; TotalMs=$deltaTrade.deal.totalMs; TxID=$deltaTrade.deal.paymentTxid })
    Write-Host ("[OK] Delta(A2A-NAT) <- Beta(Codex-MCP) settled={0} sats | firstOffer={1}ms | total={2}ms | A2A peers={3} | receipt={4}" -f $deltaTrade.deal.priceSats, $deltaTrade.deal.firstOfferMs, $deltaTrade.deal.totalMs, $deltaTrade.peersDiscovered, $deltaTrade.receiptVerdict) -ForegroundColor Green
    Record-Tx "trade-delta-from-beta" $deltaTrade.deal.paymentTxid

    # Stage 3: Chaos — Kill & Restart Live Hub OS Process Mid-Flight + Verify Hermes <-> A2A NAT Trade Recovers!
    Write-Host "`n=== Stage 3: Chaos Test — Kill & Restart Hub OS Process Mid-Flight ==="
    Stop-Process -Id $procs["hub"].Id -Force
    Start-Sleep -Milliseconds 500
    Start-NektarNode "hub" @("serve","--hub","--listen","127.0.0.1:18620","--public-url",$urls.hub,"--data-dir",$dirs.hub,"--network",$Network)
    Start-Sleep -Milliseconds 1000
    $postRestart = Run-Hermes "post-restart" $runTag
    $tradeList.Add([pscustomobject]@{ Route="[POST-HUB-KILL] Alpha(Hermes) <- Delta(A2A-NAT)"; Sats=$postRestart.priceSats; FirstOfferMs=$postRestart.firstOfferMs; TotalMs=$postRestart.totalMs; TxID=$postRestart.paymentTxid })
    Write-Host ("[OK] Post-Hub-Kill Alpha(Hermes) <- Delta(A2A-NAT) settled={0} sats | firstOffer={1}ms | total={2}ms!" -f $postRestart.priceSats, $postRestart.firstOfferMs, $postRestart.totalMs) -ForegroundColor Green
    Record-Tx "post-hub-kill-trade" $postRestart.paymentTxid

    # Stage 4: Check Dashboard /state + `nektar doctor` + `nektar dock` + Resource/Speed Scorecard!
    Write-Host "`n=== Stage 4: Dashboard /state, nektar doctor, nektar dock & Scorecard ==="
    $hubState = Invoke-RestMethod -Uri "$($urls.hub)/state"
    Write-Host ("[OK] Hub Svelte Dashboard /state: network={0} peers={1} bounties={2} treasury={3} sats" -f $hubState.network, $hubState.net.peers.Count, $hubState.net.bounties.Count, $hubState.treasury.sats)
    $dockOut = (& $exePath dock -data-dir $dirs.alpha) -join "`n"
    if ($dockOut -notmatch "Live probe: .*PASS") { throw "nektar dock probe failed" }
    Write-Host "[OK] 'nektar dock' live recall probe PASSED!"

    Write-Host "`n========================================================================="
    Write-Host " LIVE $($Network.ToUpper()) HETEROGENEOUS 4-AGENT SCORECARD"
    Write-Host "========================================================================="
    $tradeList | Format-Table -AutoSize

    Write-Host "Live Process RAM Footprints:"
    foreach ($k in @("hub","alpha","beta","gamma","delta")) {
        $gp = Get-Process -Id $procs[$k].Id -ErrorAction SilentlyContinue
        if ($gp) {
            Write-Host ("  {0,-8} PID={1,-6} RAM={2,6} MB   Dashboard: {3}" -f $k, $gp.Id, [math]::Round($gp.WorkingSet64 / 1MB, 1), $urls[$k])
        }
    }
    if ($txList.Count -gt 0) {
        Write-Host "`nLive BSV Testnet Transactions Broadcast ($($txList.Count) real on-chain txs):"
        $txList | Format-Table Label, TxID, Banana -AutoSize
    }

    Write-Host "`nALL REAL-WORLD HETEROGENEOUS 4-AGENT $($Network.ToUpper()) CHECKS PASSED!" -ForegroundColor Green

    if ($Interactive) {
        Write-Host "`n[INTERACTIVE MODE] All 5 live Testnet daemons are STILL RUNNING!" -ForegroundColor Cyan
        Write-Host "  Hub Dashboard   : $($urls.hub)"
        Write-Host "  Alpha (Hermes)  : $($urls.alpha)"
        Write-Host "  Beta  (Codex)   : $($urls.beta)"
        Write-Host "  Gamma (HTTP-MCP): $($urls.gamma)"
        Write-Host "  Delta (A2A NAT) : $($urls.delta)"
        Read-Host "`nPress ENTER to stop all 5 processes cleanly"
    }
}
finally {
    foreach ($p in $procs.Values) {
        if ($p -and -not $p.HasExited) {
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
    }
}
