// dock-hooks.ts — host-portable zero-Node HTTP hook scripts (for GHCR Docker
// installs), L0 LLM proxy env snippets, and extended IDE client config paths.
import { chmodSync, existsSync, mkdirSync, writeFileSync } from 'node:fs'
import { join } from 'node:path'
import type { EnvSource } from '@nektar/core'

export interface PortableHookCommands {
  sessionStart: string
  prefetch: string
  verifyReceipt: string
  verifyCodexArgs: string[]
  files: string[]
}

/** Generate zero-Node host hook scripts under ~/.nektar/hooks/ that talk to the
 *  running GHCR/local HTTP daemon directly via curl (Unix) or PowerShell (Windows).
 *  Fixes the container-path problem where /app/dist/src/bin.js does not exist on host. */
export function ensureHttpHookScripts(opts: {
  home: string
  baseUrl: string
  token?: string
  osPlatform: NodeJS.Platform
  dry: boolean
}): PortableHookCommands {
  const dir = join(opts.home, '.nektar', 'hooks')
  const base = opts.baseUrl.replace(/\/+$/, '')
  if (opts.osPlatform === 'win32') {
    const ss = join(dir, 'session-start.ps1')
    const pf = join(dir, 'prefetch.ps1')
    const vr = join(dir, 'verify-receipt.ps1')
    const authHdr = opts.token
      ? `@{ 'Authorization' = 'Bearer ${opts.token.replace(/'/g, "''")}'; 'Content-Type' = 'application/json' }`
      : `@{ 'Content-Type' = 'application/json' }`
    if (!opts.dry) {
      mkdirSync(dir, { recursive: true })
      writeFileSync(
        ss,
        `$ErrorActionPreference='SilentlyContinue'\ntry { $d = Invoke-RestMethod -Method Post -Uri '${base}/v1/tools/doctor' -Headers ${authHdr} -Body '{}' -TimeoutSec 2; if ($d) { Write-Output ("nektar ready (${base}): $($d.records) record(s) on $($d.shards) shard(s) ($($d.network)) - recall first, route verifiable answers through groundedAnswer.") } } catch {}\n`
      )
      writeFileSync(
        pf,
        `$ErrorActionPreference='SilentlyContinue'\n$raw = [Console]::In.ReadToEnd()\ntry { $j = $raw | ConvertFrom-Json; $q = if ($j.prompt) { $j.prompt } elseif ($j.user_prompt) { $j.user_prompt } else { '' } } catch { $q = $raw }\n$body = @{ limit = 5; query = $q } | ConvertTo-Json -Compress\ntry { $r = Invoke-RestMethod -Method Post -Uri '${base}/v1/tools/recall' -Headers ${authHdr} -Body $body -TimeoutSec 2; if ($r.items.Count -gt 0) { Write-Output '[nektar verified memory - cite via groundedAnswer; recorded != true]'; foreach ($it in ($r.items | Select-Object -First 5)) { if ($it.text) { Write-Output ("- [$($it.shard):$($it.seq)] " + ($it.text -replace '\\s+',' ').Substring(0, [Math]::Min(240, ($it.text -replace '\\s+',' ').Length))) } } } } catch {}\n`
      )
      writeFileSync(
        vr,
        `param([switch]$Codex, [string]$Payload = '')\n$ErrorActionPreference='SilentlyContinue'\n$raw = if ($Codex) { $Payload } else { [Console]::In.ReadToEnd() }\n$m = [regex]::Match($raw, 'nektarr1\\.[A-Za-z0-9_-]{40,}')\nif (-not $m.Success) { exit 0 }\n$tok = $m.Value\ntry { $v = Invoke-RestMethod -Uri ("${base}/v1/verify?r=" + [uri]::EscapeDataString($tok)) -TimeoutSec 3; if ($v.ok -ne $true -and -not $Codex) { @{ decision = 'block'; reason = "nektar receipt verification FAILED for $tok" } | ConvertTo-Json -Compress | Write-Output } } catch {}\n`
      )
    }
    return {
      sessionStart: `powershell -NoProfile -ExecutionPolicy Bypass -File "${ss}"`,
      prefetch: `powershell -NoProfile -ExecutionPolicy Bypass -File "${pf}"`,
      verifyReceipt: `powershell -NoProfile -ExecutionPolicy Bypass -File "${vr}"`,
      verifyCodexArgs: ['powershell', '-NoProfile', '-ExecutionPolicy', 'Bypass', '-File', vr, '-Codex'],
      files: [ss, pf, vr]
    }
  }

  const ss = join(dir, 'session-start.sh')
  const pf = join(dir, 'prefetch.sh')
  const vr = join(dir, 'verify-receipt.sh')
  const authArg = opts.token ? `-H "Authorization: Bearer ${opts.token.replace(/"/g, '\\"')}"` : ''
  if (!opts.dry) {
    mkdirSync(dir, { recursive: true })
    writeFileSync(
      ss,
      `#!/bin/sh\nresp="$(curl -fsS --max-time 2 -X POST "${base}/v1/tools/doctor" -H "Content-Type: application/json" ${authArg} -d '{}' 2>/dev/null)" || exit 0\nprintf 'nektar ready (%s) — recall first, route verifiable answers through groundedAnswer; keep the receipt footer.\\n' "${base}"\n`
    )
    writeFileSync(
      pf,
      `#!/bin/sh\nraw="$(cat 2>/dev/null || true)"\nresp="$(curl -fsS --max-time 2 -X POST "${base}/v1/tools/recall" -H "Content-Type: application/json" ${authArg} -d '{"limit":5}' 2>/dev/null)" || exit 0\ncase "$resp" in *'"text"'*) printf '[nektar verified memory — call recall/groundedAnswer; recorded != true]\\n' ;; esac\n`
    )
    writeFileSync(
      vr,
      `#!/bin/sh\nif [ "$1" = "--codex" ]; then raw="$2"; codex=1; else raw="$(cat 2>/dev/null || true)"; codex=0; fi\ntok="$(printf '%s' "$raw" | grep -oE 'nektarr1\\.[A-Za-z0-9_-]{40,}' | head -n1)"\n[ -n "$tok" ] || exit 0\nver="$(curl -fsS --max-time 3 "${base}/v1/verify?r=$tok" 2>/dev/null)" || exit 0\ncase "$ver" in *'"ok":true'*) exit 0 ;; esac\n[ "$codex" = "1" ] && exit 0\nprintf '{"decision":"block","reason":"nektar receipt verification FAILED for %s"}\\n' "$tok"\n`
    )
    for (const f of [ss, pf, vr]) {
      try {
        chmodSync(f, 0o755)
      } catch {
        /* ignore */
      }
    }
  }
  return {
    sessionStart: `sh "${ss}"`,
    prefetch: `sh "${pf}"`,
    verifyReceipt: `sh "${vr}"`,
    verifyCodexArgs: ['sh', vr, '--codex'],
    files: [ss, pf, vr]
  }
}

export function formatProxyDockInstructions(baseUrl: string, token: string | undefined, shard = 'agent'): string {
  const base = baseUrl.replace(/\/+$/, '')
  const oaiKey = token ? `nkt:${token}:$OPENAI_API_KEY` : '$OPENAI_API_KEY'
  const antKey = token ? `nkt:${token}:$ANTHROPIC_API_KEY` : '$ANTHROPIC_API_KEY'
  return [
    `L0 Universal LLM Memory Proxy — dock ANY running agent with ZERO code changes:`,
    ``,
    `  # OpenAI-compatible agents (LangGraph, CrewAI, n8n, Open WebUI, Aider, AutoGen, custom):`,
    `  export OPENAI_BASE_URL="${base}/v1/proxy/openai/v1"`,
    ...(token ? [`  export OPENAI_API_KEY="${oaiKey}"   # or send header: X-Nektar-Token: ${token}`] : []),
    ``,
    `  # Anthropic-compatible agents:`,
    `  export ANTHROPIC_BASE_URL="${base}/v1/proxy/anthropic"`,
    ...(token ? [`  export ANTHROPIC_API_KEY="${antKey}"`] : []),
    ``,
    `  # Optional per-agent headers:`,
    `  #   X-Nektar-Shard: ${shard}      (scope memory to one shard)`,
    `  #   X-Nektar-Capture: 1           (opt-in passive turn capture)`,
    ``
  ].join('\n')
}

function vscodeMcpPath(home: string, env: EnvSource, osPlatform: NodeJS.Platform): string {
  if (osPlatform === 'darwin') return join(home, 'Library', 'Application Support', 'Code', 'User', 'mcp.json')
  if (osPlatform === 'win32') return join(env['APPDATA'] ?? join(home, 'AppData', 'Roaming'), 'Code', 'User', 'mcp.json')
  return join(home, '.config', 'Code', 'User', 'mcp.json')
}

function zedSettingsPath(home: string, env: EnvSource, osPlatform: NodeJS.Platform): string {
  if (osPlatform === 'win32') return join(env['APPDATA'] ?? join(home, 'AppData', 'Roaming'), 'Zed', 'settings.json')
  return join(home, '.config', 'zed', 'settings.json')
}

function clineMcpPath(home: string, env: EnvSource, osPlatform: NodeJS.Platform): string {
  const base =
    osPlatform === 'darwin'
      ? join(home, 'Library', 'Application Support', 'Code', 'User', 'globalStorage', 'saoudrizwan.claude-dev', 'settings')
      : osPlatform === 'win32'
        ? join(env['APPDATA'] ?? join(home, 'AppData', 'Roaming'), 'Code', 'User', 'globalStorage', 'saoudrizwan.claude-dev', 'settings')
        : join(home, '.config', 'Code', 'User', 'globalStorage', 'saoudrizwan.claude-dev', 'settings')
  return join(base, 'cline_mcp_settings.json')
}

export function detectExtraClients(home: string, env: EnvSource, osPlatform: NodeJS.Platform): Array<'vscode' | 'zed' | 'cline' | 'continue' | 'gemini'> {
  const found: Array<'vscode' | 'zed' | 'cline' | 'continue' | 'gemini'> = []
  if (existsSync(join(vscodeMcpPath(home, env, osPlatform), '..'))) found.push('vscode')
  if (existsSync(join(zedSettingsPath(home, env, osPlatform), '..'))) found.push('zed')
  if (existsSync(join(clineMcpPath(home, env, osPlatform), '..'))) found.push('cline')
  if (existsSync(join(home, '.continue'))) found.push('continue')
  if (existsSync(join(home, '.gemini'))) found.push('gemini')
  return found
}

export interface ExtraDockInput {
  mcpUrl: string
  token?: string
  dry: boolean
  uninstall: boolean
  home: string
  env: EnvSource
  osPlatform: NodeJS.Platform
}

export interface ExtraDockResult {
  client: 'vscode' | 'zed' | 'cline' | 'continue' | 'gemini' | 'goose'
  label: string
  level: 'L1'
  changed: boolean
  files: string[]
  notes: string[]
}

const quoteYaml = (s: string): string => JSON.stringify(s.replace(/[\r\n]+/g, ''))
const asObj = (v: unknown): Record<string, unknown> => (v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {})

function mergeKey(
  input: ExtraDockInput,
  client: ExtraDockResult['client'],
  label: string,
  configPath: string,
  rootKey: 'servers' | 'context_servers' | 'mcpServers',
  entry: Record<string, unknown>,
  readJson: (p: string) => Record<string, unknown>,
  writeLive: (dry: boolean, p: string, content: string) => void
): ExtraDockResult {
  const cfg = readJson(configPath)
  const bag = asObj(cfg[rootKey])
  cfg[rootKey] = bag
  const notes: string[] = []
  let changed = false
  if (input.uninstall) {
    if (Object.hasOwn(bag, 'nektar')) {
      delete bag.nektar
      changed = true
      notes.push(`removed ${rootKey}.nektar`)
    } else notes.push('nektar not present')
  } else if (!Object.hasOwn(bag, 'nektar')) {
    bag.nektar = entry
    changed = true
    notes.push(`registered ${rootKey}.nektar → ${input.mcpUrl}`)
  } else notes.push(`${rootKey}.nektar already present — kept`)
  if (changed) writeLive(input.dry, configPath, JSON.stringify(cfg, null, 2) + '\n')
  return { client, label, level: 'L1', changed, files: [configPath], notes }
}

export function dockExtraClient(
  client: 'vscode' | 'zed' | 'cline' | 'continue' | 'gemini' | 'goose',
  input: ExtraDockInput,
  io: {
    readJson: (p: string) => Record<string, unknown>
    readText: (p: string) => string
    writeLive: (dry: boolean, p: string, content: string) => void
  }
): ExtraDockResult {
  const auth = input.token ? { headers: { Authorization: `Bearer ${input.token}` } } : {}
  if (client === 'vscode') {
    return mergeKey(
      input,
      'vscode',
      'VS Code',
      vscodeMcpPath(input.home, input.env, input.osPlatform),
      'servers',
      { type: 'http', url: input.mcpUrl, ...auth },
      io.readJson,
      io.writeLive
    )
  }
  if (client === 'zed') {
    return mergeKey(
      input,
      'zed',
      'Zed',
      zedSettingsPath(input.home, input.env, input.osPlatform),
      'context_servers',
      { url: input.mcpUrl, ...auth },
      io.readJson,
      io.writeLive
    )
  }
  if (client === 'cline') {
    return mergeKey(
      input,
      'cline',
      'Cline',
      clineMcpPath(input.home, input.env, input.osPlatform),
      'mcpServers',
      { url: input.mcpUrl, ...auth },
      io.readJson,
      io.writeLive
    )
  }
  if (client === 'gemini') {
    return mergeKey(
      input,
      'gemini',
      'Gemini / Antigravity',
      join(input.home, '.gemini', 'settings.json'),
      'mcpServers',
      { httpUrl: input.mcpUrl, ...auth },
      io.readJson,
      io.writeLive
    )
  }
  if (client === 'continue') {
    const cfgPath = join(input.home, '.continue', 'config.yaml')
    const existing = io.readText(cfgPath)
    const notes: string[] = []
    let changed = false
    if (input.uninstall) {
      if (existing.includes('# nektar-dock-start')) {
        io.writeLive(input.dry, cfgPath, existing.replace(/\n?# nektar-dock-start[\s\S]*?# nektar-dock-end\n?/g, '\n').trimEnd() + '\n')
        changed = true
        notes.push('removed nektar MCP entry from Continue config.yaml')
      }
      return { client: 'continue', label: 'Continue', level: 'L1', changed, files: [cfgPath], notes }
    }
    if (!existing.includes('# nektar-dock-start') && !/name:\s*nektar\b/.test(existing)) {
      const hdr = input.token ? `    headers:\n      Authorization: ${quoteYaml(`Bearer ${input.token}`)}\n` : ''
      const block = `  - name: nektar\n    type: streamable-http\n    url: ${quoteYaml(input.mcpUrl)}\n${hdr}`
      const next = /^mcpServers:\s*$/m.test(existing)
        ? existing.replace(/^mcpServers:\s*$/m, `mcpServers:\n# nektar-dock-start\n${block}# nektar-dock-end`)
        : (existing.trimEnd() ? existing.trimEnd() + '\n\n' : '') + `# nektar-dock-start\nmcpServers:\n${block}# nektar-dock-end\n`
      io.writeLive(input.dry, cfgPath, next)
      changed = true
      notes.push(`registered Continue streamable-http MCP → ${input.mcpUrl}`)
    } else notes.push('Continue config.yaml already has nektar — kept')
    return { client: 'continue', label: 'Continue', level: 'L1', changed, files: [cfgPath], notes }
  }
  const cfgPath = join(input.home, '.config', 'goose', 'config.yaml')
  const existing = io.readText(cfgPath)
  const notes: string[] = []
  let changed = false
  if (input.uninstall) {
    if (existing.includes('# nektar-dock-start')) {
      io.writeLive(input.dry, cfgPath, existing.replace(/\n?# nektar-dock-start[\s\S]*?# nektar-dock-end\n?/g, '\n').trimEnd() + '\n')
      changed = true
      notes.push('removed nektar extension from Goose config.yaml')
    }
    return { client: 'goose', label: 'Goose', level: 'L1', changed, files: [cfgPath], notes }
  }
  if (!/\bnektar:\s*\n/.test(existing) && !existing.includes('# nektar-dock-start')) {
    const entry = `  nektar:\n    name: nektar\n    type: streamable_http\n    uri: ${quoteYaml(input.mcpUrl)}\n    enabled: true\n`
    const next = /^extensions:\s*$/m.test(existing)
      ? existing.replace(/^extensions:\s*$/m, `extensions:\n# nektar-dock-start\n${entry}# nektar-dock-end`)
      : (existing.trimEnd() ? existing.trimEnd() + '\n\n' : '') + `# nektar-dock-start\nextensions:\n${entry}# nektar-dock-end\n`
    io.writeLive(input.dry, cfgPath, next)
    changed = true
    notes.push(`registered Goose streamable_http extension → ${input.mcpUrl}`)
  } else notes.push('Goose config.yaml already has nektar — kept')
  return { client: 'goose', label: 'Goose', level: 'L1', changed, files: [cfgPath], notes }
}
