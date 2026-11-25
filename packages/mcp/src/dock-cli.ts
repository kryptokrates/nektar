// dock-cli.ts — universal zero-config AI-client auto-wirer (`nektar dock`).
// Detects installed AI harnesses (Hermes, OpenClaw, Claude Code, Claude
// Desktop, Codex, Cursor, Windsurf, OpenCode, Goose), writes non-clobbering
// idempotent configs + portable hooks + agentskills.io SKILL.md, and probes
// the live nektar endpoint with `recall {}` to measure real TTFC/TTFV.
import { cpSync, existsSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs'
import { homedir, platform } from 'node:os'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { nektarEnv, parseDotEnv, sparse, writeErr, writeOut, type EnvSource } from '@nektar/core'
import { resolveBundledRoot, loadSkillMarkdown } from './adapters-export.js'
import { DEFAULT_DAEMON_PORT, discoverDaemon } from './daemon.js'
import { detectExtraClients, dockExtraClient, ensureHttpHookScripts, formatProxyDockInstructions } from './dock-hooks.js'

type DockClientId =
  | 'hermes'
  | 'openclaw'
  | 'claude'
  | 'claude-desktop'
  | 'codex'
  | 'cursor'
  | 'windsurf'
  | 'opencode'
  | 'goose'
  | 'vscode'
  | 'zed'
  | 'cline'
  | 'continue'
  | 'gemini'
  | 'proxy'

export interface DockIo {
  out?: (s: string) => void
  err?: (s: string) => void
  fetchImpl?: typeof fetch
  env?: EnvSource
  homeDir?: string
  cwd?: string
  osPlatform?: NodeJS.Platform
  binPath?: string
  repoRoot?: string
}

interface DockActionResult {
  client: DockClientId
  label: string
  level: 'L1' | 'L2' | 'L3'
  changed: boolean
  files: string[]
  notes: string[]
}

const ALL_CLIENTS: readonly DockClientId[] = [
  'hermes',
  'openclaw',
  'claude',
  'claude-desktop',
  'codex',
  'cursor',
  'windsurf',
  'opencode',
  'goose',
  'vscode',
  'zed',
  'cline',
  'continue',
  'gemini',
  'proxy'
]

const RECEIPT_SNIPPET = `# nektar receipts (snippet — merge into your project CLAUDE.md)

When answering a factual question that could come from memory:

1. Call \`recall\` on the relevant shard(s) first.
2. Route the final answer through \`groundedAnswer\` — every claim must cite a
   verified fragment; whatever can't be grounded is stripped by the tool.
3. Always end the answer with the receipt footer the tool returns:

   \`\`\`
   receipt: nektarr1.<token>
   verify: <verifyUrl>
   \`\`\`

4. If no receipt comes back (nothing grounded), say the answer is
   ungrounded — never fake a footer; the Stop hook verifies real tokens only.

The footer is the auditable part of the answer: anyone can hit the verify
URL (or scan the QR on that page) to confirm the answer was committed to
anchored, provenance-signed memory — not that it is _true_.
`

const USAGE = `nektar dock — zero-config docking into installed AI apps (non-clobbering)

  nektar dock [auto | <client>...] [options]

Clients:
  hermes          Hermes Agent (native MemoryProvider + SKILL.md + mcp_servers)
  openclaw        OpenClaw (plugins.slots.memory + hooks + native tools)
  claude          Claude Code (.mcp.json + portable hooks + SKILL.md + CLAUDE.md)
  claude-desktop  Claude Desktop (claude_desktop_config.json)
  codex           Codex CLI (config.toml + portable notify hook + AGENTS.md)
  cursor          Cursor (~/.cursor/mcp.json)
  windsurf        Windsurf (~/.codeium/windsurf/mcp_config.json)
  opencode        OpenCode (~/.config/opencode/opencode.json)
  goose           Goose (~/.config/goose/config.yaml)
  vscode          VS Code Copilot MCP (User/mcp.json)
  zed             Zed Editor (settings.json context_servers)
  cline           Cline (cline_mcp_settings.json)
  continue        Continue (~/.continue/config.yaml)
  gemini          Gemini CLI / Antigravity (~/.gemini/settings.json)
  proxy           L0 Transparent LLM Proxy env exports (any OpenAI/Anthropic agent)

Options:
  --url <url>     Explicit nektar HTTP base URL (default: discovered or http://127.0.0.1:18420)
  --token <tok>   Bearer token if NEKTAR_HTTP_TOKEN is set on the server
  --hook-mode <m> Hook driver: node (default) or http (zero-Node curl/PowerShell scripts for GHCR)
  --shard <name>  Default shard hint for \`dock proxy\` (default: agent)
  --docker        Configure stdio clients to spawn ghcr.io/kryptokrates/nektar:latest
  --dir <path>    Project directory for Claude Code / Codex project-scoped files (default: cwd)
  --dry           Preview all changes without writing any files
  --uninstall     Cleanly remove nektar entries while preserving sibling configs
  --json          Machine-readable output
`

const readFlag = (args: string[], name: string): string | undefined => {
  const i = args.indexOf(name)
  if (i !== -1 && i + 1 < args.length) return args[i + 1]
  const eq = args.find((a) => a.startsWith(`${name}=`))
  return eq?.slice(name.length + 1)
}

function resolveHermesHome(home: string, env: EnvSource, osPlatform: NodeJS.Platform): string {
  const explicit = env['HERMES_HOME']
  if (explicit) return resolve(explicit)
  const candidates =
    osPlatform === 'win32'
      ? [join(home, '.hermes'), join(env['LOCALAPPDATA'] ?? join(home, 'AppData', 'Local'), 'hermes')]
      : [join(home, '.hermes'), join(home, '.local', 'share', 'hermes')]
  for (const c of candidates) if (existsSync(c)) return c
  return candidates[0]
}

function claudeDesktopConfigPath(home: string, env: EnvSource, osPlatform: NodeJS.Platform): string {
  if (osPlatform === 'darwin') return join(home, 'Library', 'Application Support', 'Claude', 'claude_desktop_config.json')
  if (osPlatform === 'win32') return join(env['APPDATA'] ?? join(home, 'AppData', 'Roaming'), 'Claude', 'claude_desktop_config.json')
  return join(home, '.config', 'Claude', 'claude_desktop_config.json')
}

function detectInstalledClients(home: string, cwd: string, env: EnvSource, osPlatform: NodeJS.Platform): DockClientId[] {
  const found: DockClientId[] = []
  const hermesCandidates = [
    ...(env['HERMES_HOME'] ? [resolve(env['HERMES_HOME'])] : []),
    join(home, '.hermes'),
    join(home, '.local', 'share', 'hermes'),
    ...(osPlatform === 'win32' ? [join(env['LOCALAPPDATA'] ?? join(home, 'AppData', 'Local'), 'hermes')] : [])
  ]
  if (hermesCandidates.some((p) => existsSync(p))) found.push('hermes')
  if (existsSync(join(home, '.openclaw'))) found.push('openclaw')
  if (existsSync(join(home, '.claude')) || existsSync(join(cwd, '.claude'))) found.push('claude')
  if (existsSync(dirname(claudeDesktopConfigPath(home, env, osPlatform)))) found.push('claude-desktop')
  if (existsSync(join(home, '.codex')) || existsSync(join(cwd, '.codex'))) found.push('codex')
  if (existsSync(join(home, '.cursor'))) found.push('cursor')
  if (existsSync(join(home, '.codeium', 'windsurf'))) found.push('windsurf')
  if (existsSync(join(home, '.config', 'opencode'))) found.push('opencode')
  if (existsSync(join(home, '.config', 'goose'))) found.push('goose')
  for (const extra of detectExtraClients(home, env, osPlatform)) found.push(extra)
  return found
}

const asRecord = (v: unknown): Record<string, unknown> => (v !== null && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : {})

/** Safely quote a scalar for TOML/YAML double-quoted strings, stripping CR/LF. */
const quoteScalar = (s: string): string => JSON.stringify(s.replace(/[\r\n]+/g, ''))

function readJsonObj(path: string): Record<string, unknown> {
  if (!existsSync(path)) return {}
  try {
    return asRecord(JSON.parse(readFileSync(path, 'utf8')))
  } catch {
    return {}
  }
}

function writeIfLive(dry: boolean, path: string, content: string): void {
  if (dry) return
  mkdirSync(dirname(path), { recursive: true })
  writeFileSync(path, content)
}

function skillMarkdown(repoRoot: string | undefined): string {
  return loadSkillMarkdown(repoRoot)
}

interface DockCtx {
  mcpUrl: string
  baseUrl: string
  token?: string
  docker: boolean
  hookMode: 'node' | 'http'
  shard: string
  dry: boolean
  uninstall: boolean
  home: string
  cwd: string
  projectDir: string
  env: EnvSource
  osPlatform: NodeJS.Platform
  binPath: string
  repoRoot?: string
}

function stdioServerSpec(ctx: DockCtx): { command: string; args: string[] } {
  if (ctx.docker) {
    return {
      command: 'docker',
      args: ['run', '-i', '--rm', '-v', 'nektar-data:/data', '-e', 'NEKTAR_NETWORK=testnet', 'ghcr.io/kryptokrates/nektar:latest']
    }
  }
  return { command: 'node', args: [ctx.binPath] }
}

function resolveHookCommands(ctx: DockCtx): { sessionStart: string; prefetch: string; verifyReceipt: string; verifyCodexArgs: string[] } {
  if (ctx.hookMode === 'http') {
    return ensureHttpHookScripts({ home: ctx.home, baseUrl: ctx.baseUrl, token: ctx.token, osPlatform: ctx.osPlatform, dry: ctx.dry })
  }
  return {
    sessionStart: `node "${ctx.binPath}" hook session-start`,
    prefetch: `node "${ctx.binPath}" hook prefetch`,
    verifyReceipt: `node "${ctx.binPath}" hook verify-receipt`,
    verifyCodexArgs: ['node', ctx.binPath, 'hook', 'verify-receipt', '--codex']
  }
}

function mergeJsonMcpClient(
  ctx: DockCtx,
  client: DockClientId,
  label: string,
  configPath: string,
  mode: 'http-or-stdio' | 'stdio-only' | 'opencode'
): DockActionResult {
  const cfg = readJsonObj(configPath)
  const notes: string[] = []
  let changed = false

  if (mode === 'opencode') {
    const mcp = asRecord(cfg.mcp)
    cfg.mcp = mcp
    if (ctx.uninstall) {
      if (Object.hasOwn(mcp, 'nektar')) {
        delete mcp.nektar
        changed = true
        notes.push('removed mcp.nektar')
      } else notes.push('nektar not present')
    } else if (!Object.hasOwn(mcp, 'nektar')) {
      mcp.nektar = {
        type: 'remote',
        url: ctx.mcpUrl,
        ...(ctx.token ? { headers: { Authorization: `Bearer ${ctx.token}` } } : {})
      }
      changed = true
      notes.push(`registered remote MCP → ${ctx.mcpUrl}`)
    } else notes.push('mcp.nektar already present — kept')
  } else {
    const servers = asRecord(cfg.mcpServers)
    cfg.mcpServers = servers
    if (ctx.uninstall) {
      if (Object.hasOwn(servers, 'nektar')) {
        delete servers.nektar
        changed = true
        notes.push('removed mcpServers.nektar')
      } else notes.push('nektar not present')
    } else if (!Object.hasOwn(servers, 'nektar')) {
      servers.nektar =
        mode === 'stdio-only' || ctx.docker
          ? stdioServerSpec(ctx)
          : {
              url: ctx.mcpUrl,
              ...(ctx.token ? { headers: { Authorization: `Bearer ${ctx.token}` } } : {})
            }
      changed = true
      notes.push(mode === 'stdio-only' || ctx.docker ? `registered stdio MCP (${stdioServerSpec(ctx).command})` : `registered HTTP MCP → ${ctx.mcpUrl}`)
    } else notes.push('mcpServers.nektar already present — kept')
  }

  if (changed) writeIfLive(ctx.dry, configPath, JSON.stringify(cfg, null, 2) + '\n')
  return { client, label, level: 'L1', changed, files: [configPath], notes }
}

function dockClaude(ctx: DockCtx): DockActionResult {
  const mcpPath = join(ctx.projectDir, '.mcp.json')
  const settingsPath = join(ctx.projectDir, '.claude', 'settings.json')
  const claudeMdPath = join(ctx.projectDir, 'CLAUDE.md')
  const skillPath = join(ctx.projectDir, '.claude', 'skills', 'nektar-memory', 'SKILL.md')
  const files = [mcpPath, settingsPath, claudeMdPath, skillPath]
  const notes: string[] = []
  let changed = false

  const mcpCfg = readJsonObj(mcpPath)
  const servers = asRecord(mcpCfg.mcpServers)
  mcpCfg.mcpServers = servers

  const settings = readJsonObj(settingsPath)
  type HookGroup = { hooks?: Array<{ type?: string; command?: string }> }
  const hooks = asRecord(settings.hooks) as Record<string, HookGroup[]>
  settings.hooks = hooks

  const hookCmds = resolveHookCommands(ctx)
  const sessionCmd = hookCmds.sessionStart
  const prefetchCmd = hookCmds.prefetch
  const stopCmd = hookCmds.verifyReceipt

  if (ctx.uninstall) {
    if (Object.hasOwn(servers, 'nektar')) {
      delete servers.nektar
      changed = true
      notes.push('removed .mcp.json nektar entry')
    }
    for (const [ev, marker] of [
      ['SessionStart', 'session-start'],
      ['UserPromptSubmit', 'prefetch'],
      ['Stop', 'verify-receipt']
    ] as const) {
      const list = hooks[ev]
      if (!Array.isArray(list)) continue
      const next = list.filter((g) => !(g.hooks ?? []).some((h) => (h.command ?? '').includes(`hook ${marker}`) || (h.command ?? '').includes(`${marker}.mjs`)))
      if (next.length !== list.length) {
        hooks[ev] = next
        changed = true
        notes.push(`removed ${ev} hook`)
      }
    }
    if (changed) {
      writeIfLive(ctx.dry, mcpPath, JSON.stringify(mcpCfg, null, 2) + '\n')
      writeIfLive(ctx.dry, settingsPath, JSON.stringify(settings, null, 2) + '\n')
    }
    if (!ctx.dry && existsSync(skillPath)) {
      rmSync(dirname(skillPath), { recursive: true, force: true })
      changed = true
      notes.push('removed .claude/skills/nektar-memory')
    }
    return { client: 'claude', label: 'Claude Code', level: 'L3', changed, files, notes }
  }

  if (!Object.hasOwn(servers, 'nektar')) {
    servers.nektar = ctx.docker
      ? stdioServerSpec(ctx)
      : {
          type: 'http',
          url: ctx.mcpUrl,
          ...(ctx.token ? { headers: { Authorization: `Bearer ${ctx.token}` } } : {})
        }
    changed = true
    notes.push(ctx.docker ? 'registered Docker stdio MCP in .mcp.json' : `registered HTTP MCP → ${ctx.mcpUrl}`)
  } else notes.push('.mcp.json nektar already present — kept')

  for (const [ev, cmd, marker] of [
    ['SessionStart', sessionCmd, 'session-start'],
    ['UserPromptSubmit', prefetchCmd, 'prefetch'],
    ['Stop', stopCmd, 'verify-receipt']
  ] as const) {
    const list = (hooks[ev] ??= [])
    const has = list.some((g) => (g.hooks ?? []).some((h) => (h.command ?? '').includes(`hook ${marker}`) || (h.command ?? '').includes(`${marker}.mjs`)))
    if (!has) {
      list.push({ hooks: [{ type: 'command', command: cmd }] })
      changed = true
      notes.push(`added portable ${ev} hook`)
    }
  }

  const claudeMd = existsSync(claudeMdPath) ? readFileSync(claudeMdPath, 'utf8') : ''
  const claudeMdOut = claudeMd.includes('nektar receipts') ? claudeMd : claudeMd.trimEnd() ? claudeMd.trimEnd() + '\n\n' + RECEIPT_SNIPPET : RECEIPT_SNIPPET
  if (claudeMdOut !== claudeMd) {
    changed = true
    notes.push('appended receipt discipline to CLAUDE.md')
  }

  if (!existsSync(skillPath)) {
    changed = true
    notes.push('installed agentskills.io SKILL.md')
  }

  if (changed) {
    writeIfLive(ctx.dry, mcpPath, JSON.stringify(mcpCfg, null, 2) + '\n')
    writeIfLive(ctx.dry, settingsPath, JSON.stringify(settings, null, 2) + '\n')
    writeIfLive(ctx.dry, claudeMdPath, claudeMdOut)
    writeIfLive(ctx.dry, skillPath, skillMarkdown(ctx.repoRoot))
  }
  return { client: 'claude', label: 'Claude Code', level: 'L3', changed, files, notes }
}

function dockCodex(ctx: DockCtx): DockActionResult {
  const configPath = join(ctx.projectDir !== ctx.cwd ? join(ctx.projectDir, '.codex') : join(ctx.home, '.codex'), 'config.toml')
  const agentsMdPath = join(ctx.projectDir, 'AGENTS.md')
  const skillPath = join(ctx.home, '.codex', 'skills', 'nektar-memory', 'SKILL.md')
  const files = [configPath, agentsMdPath, skillPath]
  const notes: string[] = []
  const toml = existsSync(configPath) ? readFileSync(configPath, 'utf8') : ''
  let out = toml
  let changed = false

  if (ctx.uninstall) {
    const cleaned = out.replace(/\n?\[mcp_servers\.nektar\][\s\S]*?(?=\n\[|$)/, '').trimEnd()
    if (cleaned !== out.trimEnd()) {
      out = cleaned ? cleaned + '\n' : ''
      changed = true
      notes.push('removed [mcp_servers.nektar]')
    }
    if (changed) writeIfLive(ctx.dry, configPath, out)
    if (!ctx.dry && existsSync(skillPath)) {
      rmSync(dirname(skillPath), { recursive: true, force: true })
      changed = true
      notes.push('removed Codex nektar-memory skill')
    }
    return { client: 'codex', label: 'Codex CLI', level: 'L2', changed, files, notes }
  }

  const hookCmds = resolveHookCommands(ctx)
  const notifyLine = `notify = [${hookCmds.verifyCodexArgs.map((a) => quoteScalar(a)).join(', ')}]`
  if (/^\s*notify\s*=/m.test(out)) {
    notes.push('notify already set — left as-is (chain with --chain if desired)')
  } else {
    out = notifyLine + '\n\n' + out
    changed = true
    notes.push('added portable notify hook (verify-receipt --codex)')
  }
  if (/\[mcp_servers\.nektar\]/.test(out)) {
    notes.push('[mcp_servers.nektar] already present — kept')
  } else {
    const block = ctx.docker
      ? `\n[mcp_servers.nektar]\ncommand = "docker"\nargs = ["run","-i","--rm","-v","nektar-data:/data","-e","NEKTAR_NETWORK=testnet","ghcr.io/kryptokrates/nektar:latest"]\n`
      : `\n[mcp_servers.nektar]\nurl = ${quoteScalar(ctx.mcpUrl)}\n${ctx.token ? `http_headers = { "Authorization" = ${quoteScalar(`Bearer ${ctx.token}`)} }\n` : ''}`
    out = out.trimEnd() + '\n' + block
    changed = true
    notes.push(ctx.docker ? 'added [mcp_servers.nektar] (docker stdio)' : `added [mcp_servers.nektar] → ${ctx.mcpUrl}`)
  }

  const agentsMd = existsSync(agentsMdPath) ? readFileSync(agentsMdPath, 'utf8') : ''
  const agentsOut = agentsMd.includes('nektar receipts') ? agentsMd : agentsMd.trimEnd() ? agentsMd.trimEnd() + '\n\n' + RECEIPT_SNIPPET : RECEIPT_SNIPPET
  if (agentsOut !== agentsMd) {
    changed = true
    notes.push('appended receipt discipline to AGENTS.md')
  }
  if (!existsSync(skillPath)) {
    changed = true
    notes.push('installed SKILL.md into ~/.codex/skills/nektar-memory')
  }
  if (changed) {
    writeIfLive(ctx.dry, configPath, out)
    writeIfLive(ctx.dry, agentsMdPath, agentsOut)
    writeIfLive(ctx.dry, skillPath, skillMarkdown(ctx.repoRoot))
  }
  return { client: 'codex', label: 'Codex CLI', level: 'L2', changed, files, notes }
}

function dockHermes(ctx: DockCtx): DockActionResult {
  const hermesHome = resolveHermesHome(ctx.home, ctx.env, ctx.osPlatform)
  const cfgPath = join(hermesHome, 'config.yaml')
  const pluginDst = join(hermesHome, 'plugins', 'memory', 'nektar')
  const skillPath = join(hermesHome, 'skills', 'nektar-memory', 'SKILL.md')
  const files = [cfgPath, pluginDst, skillPath]
  const notes: string[] = []
  let changed = false

  const existingYaml = existsSync(cfgPath) ? readFileSync(cfgPath, 'utf8') : ''

  if (ctx.uninstall) {
    if (!ctx.dry && existsSync(pluginDst)) {
      rmSync(pluginDst, { recursive: true, force: true })
      changed = true
      notes.push(`removed ${pluginDst}`)
    }
    if (!ctx.dry && existsSync(dirname(skillPath))) {
      rmSync(dirname(skillPath), { recursive: true, force: true })
      changed = true
      notes.push(`removed ${dirname(skillPath)}`)
    }
    if (existingYaml.includes('# nektar-dock-start')) {
      const cleaned = existingYaml.replace(/\n?# nektar-dock-start[\s\S]*?# nektar-dock-end\n?/g, '\n').trimEnd() + '\n'
      writeIfLive(ctx.dry, cfgPath, cleaned)
      changed = true
      notes.push('removed nektar MCP stanza from config.yaml')
    }
    return { client: 'hermes', label: 'Hermes Agent', level: 'L3', changed, files, notes }
  }

  // 1. Copy native Hermes MemoryProvider plugin if available in checkout
  const pluginSrc = ctx.repoRoot ? join(ctx.repoRoot, 'integrations', 'hermes') : undefined
  if (pluginSrc && existsSync(join(pluginSrc, 'plugin.yaml'))) {
    if (!ctx.dry) {
      mkdirSync(pluginDst, { recursive: true })
      cpSync(pluginSrc, pluginDst, { recursive: true, force: true })
    }
    changed = true
    notes.push(`installed native MemoryProvider plugin → ${pluginDst} (enable via \`hermes memory setup\` → nektar)`)
  } else {
    notes.push('native MemoryProvider plugin not bundled in standalone tree — wired MCP + SKILL.md')
  }

  // 2. Install agentskills.io SKILL.md
  if (!existsSync(skillPath)) {
    writeIfLive(ctx.dry, skillPath, skillMarkdown(ctx.repoRoot))
    changed = true
    notes.push(`installed skill → ${skillPath}`)
  }

  // 3. Merge mcp_servers.nektar into config.yaml (non-clobbering)
  if (!/\bnektar:\s*\n/.test(existingYaml) && !existingYaml.includes('# nektar-dock-start')) {
    const authLines = ctx.token ? `      headers:\n        Authorization: ${quoteScalar(`Bearer ${ctx.token}`)}\n` : ''
    const nektarEntry = ctx.docker
      ? `  nektar:\n    command: "docker"\n    args: ["run", "-i", "--rm", "-v", "nektar-data:/data", "-e", "NEKTAR_NETWORK=testnet", "ghcr.io/kryptokrates/nektar:latest"]\n    connect_timeout: 90\n`
      : `  nektar:\n    url: ${quoteScalar(ctx.mcpUrl)}\n${authLines}    timeout: 60\n`
    let nextYaml: string
    if (/^mcp_servers:\s*$/m.test(existingYaml)) {
      nextYaml = existingYaml.replace(/^mcp_servers:\s*$/m, `mcp_servers:\n# nektar-dock-start\n${nektarEntry}# nektar-dock-end`)
    } else {
      nextYaml = (existingYaml.trimEnd() ? existingYaml.trimEnd() + '\n\n' : '') + `# nektar-dock-start\nmcp_servers:\n${nektarEntry}# nektar-dock-end\n`
    }
    writeIfLive(ctx.dry, cfgPath, nextYaml)
    changed = true
    notes.push(`registered mcp_servers.nektar in ${cfgPath}`)
  } else {
    notes.push('config.yaml already has nektar — kept')
  }

  return { client: 'hermes', label: 'Hermes Agent', level: 'L3', changed, files, notes }
}

function dockOpenClaw(ctx: DockCtx): DockActionResult {
  const ocHome = join(ctx.home, '.openclaw')
  const cfgPath = join(ocHome, 'openclaw.json')
  const pluginDst = join(ocHome, 'extensions', 'nektar')
  const files = [cfgPath, pluginDst]
  const notes: string[] = []
  let changed = false

  const cfg = readJsonObj(cfgPath)
  const plugins = asRecord(cfg.plugins)
  const entries = asRecord(plugins.entries)
  const slots = asRecord(plugins.slots)
  const allow: string[] = Array.isArray(plugins.allow) ? [...(plugins.allow as string[])] : []

  if (ctx.uninstall) {
    if (Object.hasOwn(entries, 'nektar-memory')) {
      delete entries['nektar-memory']
      changed = true
      notes.push('removed plugins.entries.nektar-memory')
    }
    if (slots.memory === 'nektar-memory') {
      slots.memory = 'memory-core'
      changed = true
      notes.push('restored plugins.slots.memory → memory-core')
    }
    const idx = allow.indexOf('nektar-memory')
    if (idx >= 0) {
      allow.splice(idx, 1)
      plugins.allow = allow
      changed = true
    }
    if (changed) {
      plugins.entries = entries
      plugins.slots = slots
      cfg.plugins = plugins
      writeIfLive(ctx.dry, cfgPath, JSON.stringify(cfg, null, 2) + '\n')
    }
    if (!ctx.dry && existsSync(pluginDst)) {
      rmSync(pluginDst, { recursive: true, force: true })
      changed = true
      notes.push(`removed ${pluginDst}`)
    }
    return { client: 'openclaw', label: 'OpenClaw', level: 'L3', changed, files, notes }
  }

  const pluginSrc = ctx.repoRoot ? join(ctx.repoRoot, 'integrations', 'openclaw') : undefined
  if (pluginSrc && existsSync(join(pluginSrc, 'openclaw.plugin.json'))) {
    if (!ctx.dry) {
      mkdirSync(pluginDst, { recursive: true })
      cpSync(pluginSrc, pluginDst, { recursive: true, force: true })
    }
    changed = true
    notes.push(`installed OpenClaw memory plugin → ${pluginDst}`)
  }

  if (!Object.hasOwn(entries, 'nektar-memory')) {
    entries['nektar-memory'] = {
      enabled: true,
      config: {
        url: ctx.baseUrl,
        ...(ctx.token ? { token: ctx.token } : {}),
        autoRecall: true,
        verifyReceipts: true
      },
      hooks: { allowPromptInjection: true, allowConversationAccess: true }
    }
    changed = true
    notes.push(`configured plugins.entries.nektar-memory → ${ctx.baseUrl}`)
  } else notes.push('plugins.entries.nektar-memory already present — kept')

  if (slots.memory === undefined || slots.memory === 'memory-core') {
    slots.memory = 'nektar-memory'
    changed = true
    notes.push('assigned plugins.slots.memory = "nektar-memory"')
  } else if (slots.memory !== 'nektar-memory') {
    const owner = typeof slots.memory === 'string' ? slots.memory : JSON.stringify(slots.memory)
    notes.push(`plugins.slots.memory already owned by "${owner}" — left untouched`)
  }

  if (!allow.includes('nektar-memory')) {
    allow.push('nektar-memory')
    changed = true
  }

  if (changed) {
    plugins.entries = entries
    plugins.slots = slots
    plugins.allow = allow
    cfg.plugins = plugins
    writeIfLive(ctx.dry, cfgPath, JSON.stringify(cfg, null, 2) + '\n')
  }
  return { client: 'openclaw', label: 'OpenClaw', level: 'L3', changed, files, notes }
}

export async function runDockCli(argv: string[], io: DockIo = {}): Promise<number> {
  const out = io.out ?? writeOut
  const err = io.err ?? writeErr
  const env = io.env ?? process.env
  const fetchImpl = io.fetchImpl ?? fetch
  const home = io.homeDir ?? homedir()
  const cwd = io.cwd ?? process.cwd()
  const osPlatform = io.osPlatform ?? platform()

  if (argv.includes('--help') || argv.includes('-h') || argv[0] === 'help') {
    out(USAGE)
    return 0
  }

  const dry = argv.includes('--dry')
  const uninstall = argv.includes('--uninstall')
  const docker = argv.includes('--docker')
  const asJson = argv.includes('--json')
  const dirFlag = readFlag(argv, '--dir')
  const hookModeFlag = readFlag(argv, '--hook-mode')
  const shardFlag = readFlag(argv, '--shard') ?? 'agent'
  const projectDir = resolve(dirFlag ?? cwd)

  const valueFlags = new Set(['--url', '--token', '--dir', '--hook-mode', '--shard'])
  const targets: string[] = []
  for (let i = 0; i < argv.length; i++) {
    const a = argv[i]
    if (a.startsWith('--')) {
      if (valueFlags.has(a)) i++
      continue
    }
    targets.push(a.toLowerCase())
  }

  const defaultBin = fileURLToPath(new URL('./bin.js', import.meta.url))
  const binPath = io.binPath ?? defaultBin
  const hookMode: 'node' | 'http' = hookModeFlag === 'http' || (hookModeFlag !== 'node' && docker) ? 'http' : 'node'
  const repoRoot = io.repoRoot ?? resolveBundledRoot(import.meta.url)

  const dot: Record<string, string | undefined> = existsSync(join(cwd, '.env')) ? parseDotEnv(readFileSync(join(cwd, '.env'), 'utf8')) : {}
  const discovered = await discoverDaemon({ env, fetchImpl, timeoutMs: 400 })
  const explicitUrl = readFlag(argv, '--url')
  if (explicitUrl !== undefined && !/^https?:\/\/\S+$/i.test(explicitUrl)) {
    err(`nektar dock: --url must be an http(s) URL, got "${explicitUrl}"\n`)
    return 2
  }
  const rawBase =
    explicitUrl?.replace(/\/mcp\/?$/, '') ??
    discovered?.url ??
    `http://127.0.0.1:${nektarEnv('NEKTAR_HTTP_PORT', env) ?? sparse(dot['NEKTAR_HTTP_PORT']) ?? String(DEFAULT_DAEMON_PORT)}`
  const baseUrl = rawBase.replace(/\/+$/, '')
  const mcpUrl = `${baseUrl}/mcp`
  const token = readFlag(argv, '--token') ?? nektarEnv('NEKTAR_HTTP_TOKEN', env) ?? dot['NEKTAR_HTTP_TOKEN'] ?? discovered?.token

  let selected: DockClientId[]
  if (targets.length === 0 || targets.includes('auto')) {
    selected = detectInstalledClients(home, projectDir, env, osPlatform)
  } else {
    const invalid = targets.filter((t) => !(ALL_CLIENTS as readonly string[]).includes(t))
    if (invalid.length > 0) {
      err(`nektar dock: unknown client(s): ${invalid.join(', ')}\nSupported: ${ALL_CLIENTS.join(', ')}\n`)
      return 2
    }
    selected = [...new Set(targets)] as DockClientId[]
  }

  const ctx: DockCtx = {
    mcpUrl,
    baseUrl,
    token,
    docker,
    hookMode,
    shard: shardFlag,
    dry,
    uninstall,
    home,
    cwd,
    projectDir,
    env,
    osPlatform,
    binPath,
    repoRoot
  }

  const results: DockActionResult[] = []
  let showProxyBlock = false
  for (const client of selected) {
    if (client === 'hermes') results.push(dockHermes(ctx))
    else if (client === 'openclaw') results.push(dockOpenClaw(ctx))
    else if (client === 'claude') results.push(dockClaude(ctx))
    else if (client === 'codex') results.push(dockCodex(ctx))
    else if (client === 'claude-desktop')
      results.push(mergeJsonMcpClient(ctx, 'claude-desktop', 'Claude Desktop', claudeDesktopConfigPath(home, env, osPlatform), 'stdio-only'))
    else if (client === 'cursor') results.push(mergeJsonMcpClient(ctx, 'cursor', 'Cursor', join(home, '.cursor', 'mcp.json'), 'http-or-stdio'))
    else if (client === 'windsurf')
      results.push(mergeJsonMcpClient(ctx, 'windsurf', 'Windsurf', join(home, '.codeium', 'windsurf', 'mcp_config.json'), 'http-or-stdio'))
    else if (client === 'opencode')
      results.push(mergeJsonMcpClient(ctx, 'opencode', 'OpenCode', join(home, '.config', 'opencode', 'opencode.json'), 'opencode'))
    else if (client === 'proxy') {
      showProxyBlock = true
      results.push({
        client: 'proxy',
        label: 'L0 LLM Memory Proxy',
        level: 'L3',
        changed: false,
        files: [],
        notes: [`OpenAI endpoint: ${ctx.baseUrl}/v1/proxy/openai/v1`, `Anthropic endpoint: ${ctx.baseUrl}/v1/proxy/anthropic`]
      })
    } else {
      results.push(
        dockExtraClient(client, ctx, {
          readJson: readJsonObj,
          readText: (p) => (existsSync(p) ? readFileSync(p, 'utf8') : ''),
          writeLive: writeIfLive
        })
      )
    }
  }

  // Live probe (OpenClaw pattern: verify before you trust it)
  let probe: { ok: boolean; ttfcMs?: number; items?: number; shards?: string[]; note?: string } = {
    ok: false,
    note: `daemon not running at ${baseUrl} — start it with \`nektar --port ${DEFAULT_DAEMON_PORT}\``
  }
  if (!uninstall) {
    const t0 = Date.now()
    try {
      const headers: Record<string, string> = { 'content-type': 'application/json' }
      if (token) headers.authorization = `Bearer ${token}`
      const r = await fetchImpl(`${baseUrl}/v1/tools/recall`, {
        method: 'POST',
        headers,
        body: '{}',
        signal: AbortSignal.timeout(1_500)
      })
      if (r.ok) {
        const j = (await r.json()) as { shards?: string[]; items?: unknown[] }
        probe = { ok: true, ttfcMs: Date.now() - t0, items: (j.items ?? []).length, shards: j.shards ?? [] }
      } else {
        probe = { ok: false, note: `HTTP ${r.status} from ${baseUrl}/v1/tools/recall` }
      }
    } catch {
      /* daemon not up yet */
    }
  }

  if (asJson) {
    out(JSON.stringify({ dry, uninstall, baseUrl, mcpUrl, clients: results, probe }, null, 2) + '\n')
    return 0
  }

  out(`nektar dock${dry ? ' [dry]' : ''}${uninstall ? ' [uninstall]' : ''} — endpoint ${mcpUrl}\n`)
  if (results.length === 0) {
    out(`  no AI clients auto-detected in ${home} — pass explicit names: nektar dock hermes claude cursor\n`)
  } else {
    for (const r of results) {
      out(`  [${r.level}] ${r.label} (${r.changed ? (dry ? 'would update' : 'updated') : 'unchanged'})\n`)
      for (const n of r.notes) out(`        ${n}\n`)
    }
  }
  if (showProxyBlock) {
    out('\n' + formatProxyDockInstructions(baseUrl, token, shardFlag))
  }
  if (!uninstall) {
    out(
      probe.ok
        ? `\n  live probe: ✓ recall {} answered in ${probe.ttfcMs}ms (${probe.items} item(s) on [${(probe.shards ?? []).join(', ')}])\n`
        : `\n  live probe: · ${probe.note}\n`
    )
  }
  return 0
}
