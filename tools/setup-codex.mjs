#!/usr/bin/env node
// tools/setup-codex.mjs — wire the Codex integration into a config + project.
// Merges, never clobbers — same contract as setup-claude.mjs.
//
//   node tools/setup-codex.mjs [--dir <project>] [--url <mcpUrl>] [--port <n>] [--token <bearer>] [--dry]
//
// Writes into <config> (~/.codex/config.toml by default, <dir>/.codex/
// config.toml with --dir): the [mcp_servers.nektar] table and a top-level
// `notify` hook. Appends the receipt-discipline snippet to <dir or cwd>/
// AGENTS.md if absent.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { homedir } from 'node:os'
import { parseDotEnv } from './dotenv.mjs'

const args = process.argv.slice(2)
const flag = (k) => {
  const i = args.indexOf(k)
  if (i >= 0 && i + 1 < args.length) return args[i + 1]
  const eq = args.find((a) => a.startsWith(`${k}=`))
  return eq?.slice(k.length + 1)
}
const dry = args.includes('--dry')
const dirArg = flag('--dir')
const dir = dirArg ? resolve(dirArg) : null
const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..')
const dot = existsSync(join(root, '.env')) ? parseDotEnv(readFileSync(join(root, '.env'), 'utf8')) : {}
const port = flag('--port') ?? process.env.NEKTAR_HTTP_PORT ?? dot.NEKTAR_HTTP_PORT ?? '8080'
const mcpUrl = flag('--url') ?? `http://localhost:${port}/mcp`
const token = flag('--token') ?? process.env.NEKTAR_HTTP_TOKEN ?? dot.NEKTAR_HTTP_TOKEN

const configPath = join(dir ? join(dir, '.codex') : join(homedir(), '.codex'), 'config.toml')
const agentsMdPath = join(dir ?? process.cwd(), 'AGENTS.md')
const NOTIFY_LINE = `notify = ["node", "${join(root, 'integrations/codex/verify-notify.mjs').replace(/\\/g, '\\\\')}"]`
const headerLine = token ? `http_headers = { "Authorization" = "Bearer ${token.replace(/\\/g, '\\\\').replace(/"/g, '\\"')}" }\n` : ''
const MCP_BLOCK = `\n[mcp_servers.nektar]\nurl = "${mcpUrl}"\n${headerLine}`
const SNIPPET = readFileSync(join(root, 'integrations/claude-code/CLAUDE.md'), 'utf8')
const MARKER = 'nektar receipts'

const toml = existsSync(configPath) ? readFileSync(configPath, 'utf8') : ''
const notes = []
let out = toml
// `notify` is a top-level key — it must precede any [table]; prepend when absent
if (/^\s*notify\s*=/m.test(toml)) notes.push('notify already set — chain ours: notify = ["node", "<verify-notify.mjs>", "--chain", "<your notify>"]')
else {
  out = NOTIFY_LINE + '\n\n' + out
  notes.push('notify hook added (receipt check on turn end)')
}
if (/\[mcp_servers\.nektar\]/.test(toml)) notes.push('[mcp_servers.nektar] already present — left as-is')
else {
  out = out.trimEnd() + '\n' + MCP_BLOCK
  notes.push(`[mcp_servers.nektar] added → ${mcpUrl}${token ? ' (with bearer)' : ''}`)
}

const agentsMd = existsSync(agentsMdPath) ? readFileSync(agentsMdPath, 'utf8') : ''
const agentsOut = agentsMd.includes(MARKER) ? agentsMd : agentsMd.trimEnd() ? agentsMd.trimEnd() + '\n\n' + SNIPPET : SNIPPET

if (dry) {
  console.log(`[dry] ${configPath}:`)
  console.log(out)
  console.log(`[dry] ${agentsMdPath}: ${agentsOut === agentsMd ? 'unchanged (snippet present)' : 'snippet appended'}`)
  process.exit(0)
}
mkdirSync(dirname(configPath), { recursive: true })
writeFileSync(configPath, out)
writeFileSync(agentsMdPath, agentsOut)
console.log(`nektar → Codex wired (${configPath})`)
for (const n of notes) console.log('  ' + n)
console.log(`  AGENTS.md: ${agentsMd.includes(MARKER) ? 'already had the snippet' : 'receipt discipline appended'} (${agentsMdPath})`)
console.log(`  needs the instance running: NEKTAR_HTTP_PORT=${port} … node packages/mcp/dist/src/bin.js`)
