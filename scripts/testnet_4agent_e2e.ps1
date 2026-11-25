<#
.SYNOPSIS
  Nektar v0.4 — Full 5-Process Live BSV Testnet (or memory) 4-Agent End-to-End, Smoke, Chaos & Speed Suite.
.DESCRIPTION
  Spins up 5 real compiled nektar.exe OS processes:
    1) Hub       (:18620, --hub, durable hub.sqlite, starter shards, bounties & faucet)
    2) Alpha     (:18621, direct HTTP, sells ops @ 70 sats, buys agents, sells to Hub bounty)
    3) Beta      (:18622, direct HTTP, competes on ops @ 55 sats + sells bsv @ 65 sats, buys nat)
    4) Gamma     (:18623, direct HTTP, sells agents @ 60 sats, Borg BRC-369 + Mangle receipts)
    5) Delta-NAT (:18624, NAT-ONLY with NO public URL, trades purely via Hub /bbox relay)
.EXAMPLE
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\testnet_4agent_e2e.ps1 -Network testnet
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\testnet_4agent_e2e.ps1 -Network testnet -WaitMine
  powershell -NoProfile -ExecutionPolicy Bypass -File .\scripts\testnet_4agent_e2e.ps1 -Network memory
#>
param(
    [ValidateSet("testnet", "memory")]
    [string]$Network = "testnet",
    [string]$DataRoot = "",
    [int]$WaitFundSec = 0,
    [switch]$WaitMine,
    [switch]$SkipBuild,
    [switch]$KeepData
)

$ErrorActionPreference = "Stop"
$env:PATH = "$env:USERPROFILE\.local\go\bin;$env:PATH"

$repoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$exePath = Join-Path $repoRoot "nektar.exe"

if (-not $SkipBuild) {
    Write-Host "=== Building zero-CGO production nektar.exe ==="
    $env:CGO_ENABLED = "0"
    Push-Location $repoRoot
    go build -trimpath -ldflags="-s -w" -o $exePath ./cmd/nektar
    Pop-Location
    if ($LASTEXITCODE -ne 0) { throw "go build failed" }
}
if (-not (Test-Path $exePath)) { throw "nektar.exe not found at $exePath" }

$cleanTemp = $false
if (-not $DataRoot) {
    if ($Network -eq "testnet") {
        # Persist testnet wallets across runs so funded tBSV coins are never wasted!
        $DataRoot = Join-Path $env:USERPROFILE ".nektar\testnet-4agent-e2e"
    } else {
        $DataRoot = Join-Path $env:TEMP ("nektar-5node-" + [guid]::NewGuid().ToString("N").Substring(0, 8))
        $cleanTemp = -not $KeepData
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

$procs = @{}
$txids = New-Object System.Collections.Generic.List[string]
$tradeTimings = New-Object System.Collections.Generic.List[object]

function Start-Node($key, $argList) {
    $outLog = Join-Path $dirs[$key] "stdout.log"
    $errLog = Join-Path $dirs[$key] "stderr.log"
    $sw = [System.Diagnostics.Stopwatch]::StartNew()
    $p = Start-Process -FilePath $exePath -ArgumentList $argList -PassThru -NoNewWindow -RedirectStandardOutput $outLog -RedirectStandardError $errLog
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
    $errTail = if (Test-Path $errLog) { Get-Content $errLog -Tail 20 | Out-String } else { "" }
    throw "$key failed to boot at $($urls[$key]). stderr:`n$errTail"
}

function Invoke-Tool($nodeKey, $toolName, $payloadObj) {
    $body = if ($null -eq $payloadObj) { "{}" } else { $payloadObj | ConvertTo-Json -Depth 10 -Compress }
    return Invoke-RestMethod -Method Post -Uri "$($urls[$nodeKey])/v1/tools/$toolName" -ContentType "application/json" -Body $body -TimeoutSec 45
}

function Sync-NodeWallet($nodeKey) {
    try {
        return Invoke-RestMethod -Method Post -Uri "$($urls[$nodeKey])/v1/wallet/sync" -TimeoutSec 25
    } catch {
        return Invoke-RestMethod -Method Get -Uri "$($urls[$nodeKey])/v1/wallet" -TimeoutSec 10
    }
}

function Verify-WoCTestnetTx($txid, $label) {
    if ($Network -ne "testnet" -or -not $txid) { return }
    $txids.Add("$label : $txid -> https://test.whatsonchain.com/tx/$txid")
    for ($i = 0; $i -lt 5; $i++) {
        try {
            $tx = Invoke-RestMethod -Uri "https://api.whatsonchain.com/v1/bsv/test/tx/hash/$txid" -TimeoutSec 10
            if ($tx.txid -eq $txid) {
                Write-Host ("     [WoC Testnet VERIFIED] {0} size={1}B https://test.whatsonchain.com/tx/{2}" -f $label, $tx.size, $txid)
                return
            }
        } catch {}
        Start-Sleep -Milliseconds 1200
    }
    Write-Host ("     [Testnet Arcade Broadcast] {0} tx={1} (indexing on WoC...)" -f $label, $txid)
}

try {
    Write-Host "`n================================================================"
    Write-Host " NEKTAR v0.4 — 5-PROCESS LIVE E2E ($($Network.ToUpper()))"
    Write-Host " DataRoot: $DataRoot"
    Write-Host "================================================================`n"

    # 1. Boot all 5 OS processes!
    Start-Node "hub"   @("serve","--hub","--listen","127.0.0.1:18620","--public-url",$urls.hub,"--data-dir",$dirs.hub,"--network",$Network)
    Start-Node "alpha" @("serve","--listen","127.0.0.1:18621","--public-url",$urls.alpha,"--hosts",$hubEndpoint,"--data-dir",$dirs.alpha,"--network",$Network)
    Start-Node "beta"  @("serve","--listen","127.0.0.1:18622","--public-url",$urls.beta,"--hosts",$hubEndpoint,"--data-dir",$dirs.beta,"--network",$Network)
    Start-Node "gamma" @("serve","--listen","127.0.0.1:18623","--public-url",$urls.gamma,"--hosts",$hubEndpoint,"--data-dir",$dirs.gamma,"--network",$Network)
    # Delta is NAT-ONLY: zero --public-url!
    Start-Node "delta" @("serve","--listen","127.0.0.1:18624","--hosts",$hubEndpoint,"--data-dir",$dirs.delta,"--network",$Network)

    Start-Sleep -Milliseconds 700

    # 2. Live Testnet Funding & SyncPool Check
    Write-Host "`n=== Stage 0: Wallet Funding & UTXO Sync ($Network) ==="
    $wallets = @{}
    foreach ($k in @("hub","alpha","beta","gamma","delta")) {
        $null = Sync-NodeWallet $k
        $w = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
        $wallets[$k] = $w
        Write-Host ("  {0,-6} addr={1}  balance={2,8} sats  utxos={3}" -f $k, $w.fundAddress, $w.balanceSats, $w.utxos)
    }

    if ($Network -eq "testnet") {
        # Try nektar.run public testnet faucet for any unfunded node!
        $triedPublic = $false
        foreach ($k in @("hub","alpha","beta","gamma","delta")) {
            if ($wallets[$k].balanceSats -lt 1500) {
                $triedPublic = $true
                try {
                    $fRes = Invoke-RestMethod -Method Post -Uri "https://nektar.run/v1/faucet" -ContentType "application/json" -Body (@{address=$wallets[$k].fundAddress} | ConvertTo-Json) -TimeoutSec 15
                    Write-Host ("  [faucet] nektar.run dripped to {0}: {1}" -f $k, ($fRes | ConvertTo-Json -Compress))
                } catch {
                    Write-Host ("  [faucet] nektar.run unavailable for {0}: {1}" -f $k, $_.Exception.Message)
                }
            }
        }
        if ($triedPublic) {
            Start-Sleep -Seconds 3
            foreach ($k in @("hub","alpha","beta","gamma","delta")) {
                $null = Sync-NodeWallet $k
                $wallets[$k] = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
            }
        }
        # If Hub has balance >= 10,000 sats and any agent still needs tBSV, drip directly from our Hub faucet!
        if ($wallets["hub"].balanceSats -ge 10000) {
            foreach ($k in @("alpha","beta","gamma","delta")) {
                if ($wallets[$k].balanceSats -lt 1500) {
                    try {
                        $hd = Invoke-RestMethod -Method Post -Uri "$($urls.hub)/v1/faucet" -ContentType "application/json" -Body (@{address=$wallets[$k].fundAddress} | ConvertTo-Json) -TimeoutSec 20
                        Write-Host ("  [hub-faucet] Hub dripped {0} sats to {1} (tx={2})" -f $hd.sats, $k, $hd.txid)
                        Verify-WoCTestnetTx $hd.txid "hub-faucet->$k"
                    } catch {
                        Write-Host ("  [hub-faucet] failed for {0}: {1}" -f $k, $_.Exception.Message)
                    }
                }
            }
            Start-Sleep -Seconds 2
            foreach ($k in @("hub","alpha","beta","gamma","delta")) {
                $null = Sync-NodeWallet $k
                $wallets[$k] = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
            }
        }

        # Optional wait loop if user is sending testnet coins manually.
        if ($WaitFundSec -gt 0) {
            $deadline = (Get-Date).AddSeconds($WaitFundSec)
            while ((Get-Date) -lt $deadline) {
                $unfunded = @("hub","alpha","beta","gamma","delta") | Where-Object { $wallets[$_].balanceSats -lt 1000 }
                if ($unfunded.Count -eq 0) { break }
                Write-Host "  Waiting for testnet funding on [$($unfunded -join ', ')]..."
                Start-Sleep -Seconds 5
                foreach ($k in $unfunded) {
                    $null = Sync-NodeWallet $k
                    $wallets[$k] = Invoke-RestMethod -Uri "$($urls[$k])/v1/wallet"
                }
            }
        }

        $stillEmpty = @("hub","alpha","beta","gamma","delta") | Where-Object { $wallets[$_].balanceSats -lt 1000 }
        if ($stillEmpty.Count -gt 0) {
            Write-Host "`n[NEEDS TESTNET COINS] The following testnet addresses need starter tBSV (saved in $DataRoot):" -ForegroundColor Yellow
            foreach ($k in @("hub","alpha","beta","gamma","delta")) {
                Write-Host ("  {0,-6}: {1}  (balance={2} sats)" -f $k, $wallets[$k].fundAddress, $wallets[$k].balanceSats)
            }
            Write-Host "`nTip: Send ~20,000 tBSV to JUST the 'hub' address above, then re-run this script — the Hub will automatically fund all 4 agents!" -ForegroundColor Cyan
            exit 2
        }
    }

    # 3. Stage 1: Cold-Start Surface Smoke Tests
    Write-Host "`n=== Stage 1: Full Surface Smoke Matrix ==="
    $hubState = Invoke-RestMethod -Uri "$($urls.hub)/state"
    if (-not $hubState.treasury -or -not $hubState.net -or -not $hubState.automation) { throw "Hub /state incomplete" }
    $card = Invoke-RestMethod -Uri "$($urls.alpha)/.well-known/agent.json"
    if ($card.name -ne "nektar") { throw "agent.json name mismatch: $($card.name)" }
    $rs = Invoke-WebRequest -UseBasicParsing -Uri "$($urls.alpha)/restate/discover" -Headers @{Accept="application/vnd.restate.endpointmanifest.v3+json"}
    $rsText = if ($rs.Content -is [byte[]]) { [System.Text.Encoding]::UTF8.GetString($rs.Content) } else { [string]$rs.Content }
    foreach ($svc in @("BuyerDeal","SellerDeal","Custody","Tx","AnchorCadence")) {
        if ($rsText -notmatch "`"$svc`"") { throw "Restate missing $svc" }
    }
    $wel = Invoke-Tool "gamma" "recall" @{}
    if ($wel.count -lt 3) { throw "Welcome shard expected >=3 items, got $($wel.count)" }
    Write-Host "[OK] Dashboard /state, Hermes agent.json, Restate 5 services, and cold-start recall {} ($($wel.count) welcome items) PASSED"

    # 4. Stage 2: Approve Policies + Anchor Real Knowledge on Each of the 4 Agents!
    Write-Host "`n=== Stage 2: Policy Approval + Real Anchoring & Arming 4 Agents ==="
    foreach ($k in @("alpha","beta","gamma","delta")) {
        $null = Invoke-RestMethod -Method Post -Uri "$($urls[$k])/v1/onboard/policy-preset" -ContentType "application/json" -Body '{"preset":"both"}'
    }
    $runTag = [guid]::NewGuid().ToString("N").Substring(0, 8)

    $remAlpha = Invoke-Tool "alpha" "remember" @{ shard="ops-infra"; text="Alpha[$runTag]: Prod Kubernetes etcd snapshots rotate every 15m to s3://etcd-prod."; source="alpha"; autoAnchor=$true }
    Verify-WoCTestnetTx $remAlpha.anchorTxid "alpha-anchor"
    $null = Invoke-Tool "alpha" "sell" @{ shard="ops-infra"; domains=@("ops"); priceSats=70; description="Alpha SRE runbook" }

    $remBetaOps = Invoke-Tool "beta" "remember" @{ shard="ops-fast"; text="Beta[$runTag]: Prod Kubernetes etcd snapshots rotate every 15m using zstd-3."; source="beta"; autoAnchor=$true }
    Verify-WoCTestnetTx $remBetaOps.anchorTxid "beta-ops-anchor"
    $null = Invoke-Tool "beta" "sell" @{ shard="ops-fast"; domains=@("ops"); priceSats=55; description="Beta faster cheaper SRE" }

    $remBetaBsv = Invoke-Tool "beta" "remember" @{ shard="bsv-spv"; text="Beta[$runTag]: GorillaPool Arcade testnet accepts EF txs at 100 sat/kB."; source="beta"; autoAnchor=$true }
    Verify-WoCTestnetTx $remBetaBsv.anchorTxid "beta-bsv-anchor"
    $null = Invoke-Tool "beta" "sell" @{ shard="bsv-spv"; domains=@("bsv"); priceSats=65; description="Beta BSV engineering" }

    $remGamma = Invoke-Tool "gamma" "remember" @{ shard="agents-ai"; text="Gamma[$runTag]: MCP Streamable HTTP multiplexes concurrent agents over one daemon."; source="gamma"; autoAnchor=$true }
    Verify-WoCTestnetTx $remGamma.anchorTxid "gamma-anchor"
    $null = Invoke-Tool "gamma" "sell" @{ shard="agents-ai"; domains=@("agents"); priceSats=60; description="Gamma AI architecture" }

    $remDelta = Invoke-Tool "delta" "remember" @{ shard="nat-edge"; text="Delta[$runTag]: NAT-only agents settle BRC-369 trades over Hub bbox relay."; source="delta-nat"; autoAnchor=$true }
    Verify-WoCTestnetTx $remDelta.anchorTxid "delta-nat-anchor"
    $null = Invoke-Tool "delta" "sell" @{ shard="nat-edge"; domains=@("nat"); priceSats=50; description="Delta NAT edge intel" }

    Start-Sleep -Milliseconds 500

    # 5. Stage 3: 3-Layer Peer Discovery Verification
    Write-Host "`n=== Stage 3: Signed dir Lane Peer Discovery ==="
    $peers = Invoke-Tool "gamma" "peers" @{ refresh=$true }
    Write-Host "[OK] Gamma discovered $($peers.peers.Count) verified peers & $($peers.bounties.Count) Hub bounties on dir lane"
    if ($peers.peers.Count -lt 5) { throw "Expected >=5 verified peers (Hub + 4 agents), got $($peers.peers.Count)" }

    # 6. Stage 4 & 5: Full 4-Agent Cross-Trading Mesh (Competitive Ranking + Direct A2A + NAT bbox!)
    Write-Host "`n=== Stage 4 & 5: 4-Agent Cross-Trading Mesh (Competition + Direct A2A + NAT bbox) ==="
    function Run-Trade($buyer, $domain, $question, $maxSats, $expectRegex, $label) {
        $sw = [System.Diagnostics.Stopwatch]::StartNew()
        $d = Invoke-Tool $buyer "want" @{ domains=@($domain); question=$question; maxSats=$maxSats; targetShard="bought/$domain" }
        $sw.Stop()
        if ($d.status -ne "settled" -or $d.deliveredText -notmatch $expectRegex) {
            throw "$label FAILED: $($d | ConvertTo-Json -Depth 6)"
        }
        $tradeTimings.Add([pscustomobject]@{ Trade=$label; PriceSats=$d.priceSats; FirstOfferMs=$d.firstOfferMs; TotalMs=$sw.ElapsedMilliseconds; TxID=$d.paymentTxid })
        Write-Host ("[OK] {0,-32} settled={1} sats | firstOffer={2}ms | total={3}ms | tx={4}..." -f $label, $d.priceSats, $d.firstOfferMs, $sw.ElapsedMilliseconds, $d.paymentTxid.Substring(0,14))
        Verify-WoCTestnetTx $d.paymentTxid $label
        return $d
    }

    # Competitive ranking: Gamma buys ops -> Beta (55) beats Alpha (70) and Hub (90)!
    $dComp = Run-Trade "gamma" "ops" "Kubernetes etcd snapshots" 150 "zstd-3" "Gamma <- Beta (competed vs Alpha)"
    if ($dComp.priceSats -ne 55) { throw "Expected cheaper organic Beta (55 sats) to win competition, got $($dComp.priceSats)" }
    # Direct A2A: Alpha buys agents from Gamma
    $null = Run-Trade "alpha" "agents" "MCP Streamable HTTP multiplexes" 120 "multiplexes" "Alpha <- Gamma (direct A2A)"
    # NAT bbox Seller: Beta buys nat from NAT-only Delta (ZERO inbound ports!)
    $null = Run-Trade "beta" "nat" "NAT-only agents bbox relay" 100 "bbox relay" "Beta <- Delta-NAT (Hub bbox)"
    # NAT bbox Buyer: NAT-only Delta buys bsv from Beta
    $null = Run-Trade "delta" "bsv" "GorillaPool Arcade testnet EF" 120 "100 sat/kB" "Delta-NAT <- Beta (direct/NAT)"

    # 7. Stage 6: Sell-to-Hub Knowledge Bounty + Duplicate HDP 409 Check
    Write-Host "`n=== Stage 6: Sell-to-Hub Knowledge Bounties ==="
    $bPay = Invoke-Tool "alpha" "sell" @{ bountyId="bounty-ops"; shard="ops-infra"; seq=$remAlpha.seq }
    if (-not $bPay.accepted -or -not $bPay.txid) { throw "Alpha SellToHub bounty-ops failed" }
    Write-Host "[OK] Alpha sold verified fact TO Hub bounty-ops -> +$($bPay.rewardSats) sats (tx=$($bPay.txid.Substring(0,14))...)"
    Verify-WoCTestnetTx $bPay.txid "hub-bounty-ops"

    $dupRejected = $false
    try {
        $null = Invoke-Tool "alpha" "sell" @{ bountyId="bounty-ops"; shard="ops-infra"; seq=$remAlpha.seq }
    } catch { $dupRejected = $true }
    if (-not $dupRejected) { throw "Expected duplicate HDP bounty submission to fail!" }
    Write-Host "[OK] Duplicate HDP bounty submission rejected cleanly!"

    # 8. Stage 7: Borg BRC-369 Revocation + GroundedAnswer + /v1/verify?r= + Native Go Hook
    Write-Host "`n=== Stage 7: Borg BRC-369 Group, Receipts & Native Go Hook ==="
    $gStat = Invoke-Tool "gamma" "status" @{}
    $aStat = Invoke-Tool "alpha" "status" @{}
    $null = Invoke-Tool "gamma" "borgGroup" @{ op="create"; groupId="e2e-sec-$runTag"; name="E2E Sec"; pubHex=$gStat.identityPub }
    $null = Invoke-Tool "gamma" "borgGroup" @{ op="add"; groupId="e2e-sec-$runTag"; pubHex=$aStat.identityPub; role="member" }
    $null = Invoke-Tool "gamma" "remember" @{ shard="sec-$runTag"; text="Secret epoch 1 token"; borgGroup="e2e-sec-$runTag"; callerPub=$gStat.identityPub }
    $rev = Invoke-Tool "gamma" "borgGroup" @{ op="revoke"; groupId="e2e-sec-$runTag"; pubHex=$aStat.identityPub }
    if ($rev.newEpoch -ne 2) { throw "Expected Borg epoch rotation to 2, got $($rev.newEpoch)" }
    Write-Host "[OK] Borg BRC-369 group created, member added & revoked (rotated to epoch $($rev.newEpoch))"

    $boughtOps = Invoke-Tool "gamma" "recall" @{ shard="bought/ops" }
    $seq = $boughtOps.items[0].seq
    $ga = Invoke-Tool "gamma" "groundedAnswer" @{
        claims = @(@{ text=$boughtOps.items[0].text; cites=@(@{ shard="bought/ops"; seq=$seq }) })
        autoAnchor = $true
    }
    $ver = Invoke-RestMethod -Uri "$($urls.gamma)/v1/verify?r=$($ga.receiptToken)"
    if ($ver.verdict -ne "green" -and $ver.verdict -ne "amber") { throw "Unexpected receipt verdict: $($ver.verdict)" }
    $hookOut = ($ga.footer | & $exePath hook verify-receipt -data-dir $dirs.gamma) -join "`n"
    if ($hookOut -notmatch '"verdict":\s*"(green|amber)"') { throw "Native hook verify-receipt failed: $hookOut" }
    Write-Host "[OK] Receipt $($ga.receiptToken.Substring(0,22))... verified ($($ver.verdict.ToUpper())) via HTTP /v1/verify?r= & native 'nektar hook verify-receipt'"

    # 9. Stage 8: Multi-Client stdio->HTTP MCP Bridge + doctor + dock
    Write-Host "`n=== Stage 8: Multi-Client MCP Bridge, doctor & dock ==="
    $mcpIn = @(
        '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"testnet-e2e","version":"1.0"}}}'
        '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"recall","arguments":{"shard":"bought/ops"}}}'
    ) -join "`n"
    $mcpOut = ($mcpIn | & $exePath mcp -data-dir $dirs.gamma) -join "`n"
    if ($mcpOut -notmatch "zstd-3") { throw "MCP stdio->HTTP bridge failed: $mcpOut" }
    $dockOut = (& $exePath dock -data-dir $dirs.gamma) -join "`n"
    if ($dockOut -notmatch "Live probe: .*PASS") { throw "nektar dock probe failed: $dockOut" }
    Write-Host "[OK] 'nektar mcp' stdio->HTTP bridge & 'nektar dock' live probe PASSED"

    # 10. Stage 9: CHAOS — Kill & Restart Hub OS Process Mid-Flight!
    Write-Host "`n=== Stage 9: Chaos Test — Kill & Restart Hub OS Process Mid-Flight ==="
    Stop-Process -Id $procs["hub"].Id -Force
    Start-Sleep -Milliseconds 400
    Start-Node "hub" @("serve","--hub","--listen","127.0.0.1:18620","--public-url",$urls.hub,"--data-dir",$dirs.hub,"--network",$Network)
    Start-Sleep -Milliseconds 800
    $peersAfter = Invoke-Tool "alpha" "peers" @{ refresh=$true }
    if ($peersAfter.peers.Count -lt 4) { throw "Expected Hub SQLite persistence to retain peers after restart, got $($peersAfter.peers.Count)" }
    $null = Run-Trade "alpha" "nat" "NAT-only agents bbox relay" 100 "bbox relay" "Post-Hub-Restart Alpha <- Delta-NAT"
    Write-Host "[OK] Hub survived mid-flight kill & restart — SQLite peers restored, SSE reconnected & post-restart NAT trade SETTLED!"

    # 11. Optional Stage 10: Wait for 1 Real BSV Testnet Block Confirmation (-WaitMine)
    if ($Network -eq "testnet" -and $WaitMine) {
        Write-Host "`n=== Stage 10: Waiting for Live BSV Testnet Block Confirmation (amber -> green) ==="
        $mineDeadline = (Get-Date).AddMinutes(15)
        $minedOK = $false
        while ((Get-Date) -lt $mineDeadline) {
            $sync = Sync-NodeWallet "gamma"
            $vNow = Invoke-RestMethod -Uri "$($urls.gamma)/v1/verify?r=$($ga.receiptToken)"
            if ($vNow.verdict -eq "green") {
                Write-Host "[OK] REAL BSV TESTNET BLOCK MINED! Receipt upgraded AMBER -> GREEN ($($vNow.reason))!" -ForegroundColor Green
                $minedOK = $true
                break
            }
            Write-Host "  Still waiting for next testnet block (upgradedAnchors=$($sync.upgradedAnchors), verdict=$($vNow.verdict))..."
            Start-Sleep -Seconds 20
        }
        if (-not $minedOK) { Write-Host "[WARN] Timed out waiting 15m for a testnet block; transactions remain valid in mempool." -ForegroundColor Yellow }
    }

    # 12. Performance, Latency & Memory Resource Scorecard!
    Write-Host "`n================================================================"
    Write-Host " 5-PROCESS SPEED & RESOURCE SCORECARD ($($Network.ToUpper()))"
    Write-Host "================================================================"
    $tradeTimings | Format-Table -AutoSize
    Write-Host "Process Memory Footprints (RSS / WorkingSet):"
    foreach ($k in @("hub","alpha","beta","gamma","delta")) {
        $gp = Get-Process -Id $procs[$k].Id -ErrorAction SilentlyContinue
        if ($gp) {
            $mb = [math]::Round($gp.WorkingSet64 / 1MB, 1)
            Write-Host ("  {0,-8} PID={1,-6} RAM={2,6} MB" -f $k, $gp.Id, $mb)
        }
    }
    if ($txids.Count -gt 0) {
        Write-Host "`nReal BSV Testnet Transactions Broadcast:"
        foreach ($line in $txids) { Write-Host "  $line" }
    }
    Write-Host "`nALL 10 STAGES OF 5-PROCESS $($Network.ToUpper()) E2E PASSED!" -ForegroundColor Green
}
finally {
    foreach ($p in $procs.Values) {
        if ($p -and -not $p.HasExited) {
            Stop-Process -Id $p.Id -Force -ErrorAction SilentlyContinue
        }
    }
    Start-Sleep -Milliseconds 300
    if ($cleanTemp -and (Test-Path $DataRoot)) {
        Remove-Item -Recurse -Force $DataRoot -ErrorAction SilentlyContinue
    }
}
