#!/usr/bin/env node
// tools/setup-claude.mjs — wire the Claude Code integration into a project.
// Merges, never clobbers: existing hooks/settings/CLAUDE.md content survives.
//
//   node tools/setup-claude.mjs [--dir <project>] [--dry]
//
// Writes into <project>/.claude/settings.json the Stop + SessionStart hooks
// (absolute paths to THIS checkout's integration scripts), and appends the
// receipt discipline snippet to <project>/CLAUDE.md if absent.
import { existsSync, mkdirSync, readFileSync, writeFileSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const args = process.argv.slice(2)
const dir = resolve(args.includes('--dir') ? args[args.indexOf('--dir') + 1] : process.cwd())
const dry = args.includes('--dry')
const here = dirname(fileURLToPath(import.meta.url))
const root = resolve(here, '..') // nektar checkout root

const STOP_CMD = `node ${join(root, 'integrations/claude-code/verify-receipt.mjs')}`
const SESSION_CMD = `node ${join(root, 'integrations/claude-code/session-start.mjs')}`
const CLAUDE_SNIPPET = readFileSync(join(root, 'integrations/claude-code/CLAUDE.md'), 'utf8')
const SNIPPET_MARKER = 'nektar receipts'

const settingsPath = join(dir, '.claude', 'settings.json')
const claudeMdPath = join(dir, 'CLAUDE.md')

// — settings.json: read or create, then merge hook entries without dupes ———
const settings = existsSync(settingsPath) ? JSON.parse(readFileSync(settingsPath, 'utf8')) : {}
settings.hooks ??= {}
for (const [hook, command] of [['Stop', STOP_CMD], ['SessionStart', SESSION_CMD]]) {
  const list = settings.hooks[hook] ??= []
  const already = list.some(g => g?.hooks?.some(h => h?.command === command || h?.command?.includes(hook === 'Stop' ? 'verify-receipt.mjs' : 'session-start.mjs')))
  if (!already) list.push({ hooks: [{ type: 'command', command }] })
}
const claudeMd = existsSync(claudeMdPath) ? readFileSync(claudeMdPath, 'utf8') : ''
const claudeMdOut = claudeMd.includes(SNIPPET_MARKER) ? claudeMd
  : (claudeMd.trimEnd() ? claudeMd.trimEnd() + '\n\n' + CLAUDE_SNIPPET : CLAUDE_SNIPPET)

if (dry) {
  console.log(`[dry] ${settingsPath}:`); console.log(JSON.stringify(settings, null, 2))
  console.log(`[dry] ${claudeMdPath}: ${claudeMdOut === claudeMd ? 'unchanged (snippet already present)' : 'snippet appended'}`)
  process.exit(0)
}
mkdirSync(dirname(settingsPath), { recursive: true })
writeFileSync(settingsPath, JSON.stringify(settings, null, 2) + '\n')
writeFileSync(claudeMdPath, claudeMdOut)
console.log(`nektar → Claude Code wired in ${dir}`)
console.log('  hooks: Stop (receipt verify) + SessionStart (memory status) added, existing entries kept')
console.log(`  CLAUDE.md: ${claudeMd.includes(SNIPPET_MARKER) ? 'already had the snippet' : 'receipt discipline appended'}`)
console.log('  needs the instance running: NEKTAR_HTTP_PORT=8080 … node packages/mcp/dist/src/bin.js')
