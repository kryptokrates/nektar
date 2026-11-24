#!/usr/bin/env node
// tools/setup-claude.mjs — wire the Claude Code integration into a project.
// Merges, never clobbers: existing hooks/settings/MCP servers/CLAUDE.md survive.
//
//   node tools/setup-claude.mjs [--dir <project>] [--url <mcpUrl>] [--port <n>] [--token <bearer>] [--dry]
//
// Writes into <project>/.mcp.json the `nektar` HTTP MCP server entry, into
// <project>/.claude/settings.json the Stop + SessionStart hooks (absolute
// paths to THIS checkout's integration scripts), and appends the receipt
// discipline snippet to <project>/CLAUDE.md if absent.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { parseDotEnv } from './dotenv.mjs'

const args = process.argv.slice(2)
const flag = (k) => {
  const i = args.indexOf(k)
  if (i >= 0 && i + 1 < args.length) return args[i + 1]
  const eq = args.find((a) => a.startsWith(`${k}=`))
  return eq?.slice(k.length + 1)
}
const dir = resolve(flag('--dir') ?? process.cwd())
const dry = args.includes('--dry')
const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..') // nektar checkout root
const dot = existsSync(join(root, '.env')) ? parseDotEnv(readFileSync(join(root, '.env'), 'utf8')) : {}
const port = flag('--port') ?? process.env.NEKTAR_HTTP_PORT ?? dot.NEKTAR_HTTP_PORT ?? '8080'
const mcpUrl = flag('--url') ?? `http://localhost:${port}/mcp`
const token = flag('--token') ?? process.env.NEKTAR_HTTP_TOKEN ?? dot.NEKTAR_HTTP_TOKEN

const STOP_CMD = `node ${join(root, 'integrations/claude-code/verify-receipt.mjs')}`
const SESSION_CMD = `node ${join(root, 'integrations/claude-code/session-start.mjs')}`
const CLAUDE_SNIPPET = readFileSync(join(root, 'integrations/claude-code/CLAUDE.md'), 'utf8')
const SNIPPET_MARKER = 'nektar receipts'

const settingsPath = join(dir, '.claude', 'settings.json')
const mcpJsonPath = join(dir, '.mcp.json')
const claudeMdPath = join(dir, 'CLAUDE.md')

// — .mcp.json: register the nektar MCP server without clobbering siblings —
const mcpCfg = existsSync(mcpJsonPath) ? JSON.parse(readFileSync(mcpJsonPath, 'utf8')) : {}
mcpCfg.mcpServers ??= {}
const hadMcp = Object.hasOwn(mcpCfg.mcpServers, 'nektar')
if (!hadMcp) {
  mcpCfg.mcpServers.nektar = {
    type: 'http',
    url: mcpUrl,
    ...(token ? { headers: { Authorization: `Bearer ${token}` } } : {})
  }
}

// — settings.json: read or create, then merge hook entries without dupes ———
const settings = existsSync(settingsPath) ? JSON.parse(readFileSync(settingsPath, 'utf8')) : {}
settings.hooks ??= {}
for (const [hook, command] of [
  ['Stop', STOP_CMD],
  ['SessionStart', SESSION_CMD]
]) {
  const list = (settings.hooks[hook] ??= [])
  const already = list.some((g) =>
    g?.hooks?.some((h) => h?.command === command || h?.command?.includes(hook === 'Stop' ? 'verify-receipt.mjs' : 'session-start.mjs'))
  )
  if (!already) list.push({ hooks: [{ type: 'command', command }] })
}
const claudeMd = existsSync(claudeMdPath) ? readFileSync(claudeMdPath, 'utf8') : ''
const claudeMdOut = claudeMd.includes(SNIPPET_MARKER) ? claudeMd : claudeMd.trimEnd() ? claudeMd.trimEnd() + '\n\n' + CLAUDE_SNIPPET : CLAUDE_SNIPPET

if (dry) {
  console.log(`[dry] ${mcpJsonPath}:`)
  console.log(JSON.stringify(mcpCfg, null, 2))
  console.log(`[dry] ${settingsPath}:`)
  console.log(JSON.stringify(settings, null, 2))
  console.log(`[dry] ${claudeMdPath}: ${claudeMdOut === claudeMd ? 'unchanged (snippet already present)' : 'snippet appended'}`)
  process.exit(0)
}
mkdirSync(dirname(settingsPath), { recursive: true })
writeFileSync(mcpJsonPath, JSON.stringify(mcpCfg, null, 2) + '\n')
writeFileSync(settingsPath, JSON.stringify(settings, null, 2) + '\n')
writeFileSync(claudeMdPath, claudeMdOut)
console.log(`nektar → Claude Code wired in ${dir}`)
console.log(`  MCP server: ${hadMcp ? 'already registered in .mcp.json — left as-is' : `registered → ${mcpUrl}${token ? ' (with bearer)' : ''}`}`)
console.log('  hooks: Stop (receipt verify) + SessionStart (memory status) added, existing entries kept')
console.log(`  CLAUDE.md: ${claudeMd.includes(SNIPPET_MARKER) ? 'already had the snippet' : 'receipt discipline appended'}`)
console.log(`  needs the instance running: NEKTAR_HTTP_PORT=${port} … node packages/mcp/dist/src/bin.js`)
