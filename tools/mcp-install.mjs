// pnpm mcp:install [client ...] — build the mamem image, add it to a Docker
// MCP Toolkit profile ("mamem") and connect AI clients to that profile.
//   pnpm mcp:install                     → profile only (connect clients later)
//   pnpm mcp:install claude-desktop cursor vscode
// Clients supported by `docker mcp client connect`: claude-code claude-desktop
// cline codex continue crush cursor gemini goose gordon kiro lmstudio opencode
// sema4 vscode zed.
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const run = (args, opts = {}) => execFileSync('docker', args, { stdio: 'inherit', ...opts })
const out = args => execFileSync('docker', args, { encoding: 'utf8' })
const PROFILE = process.env.MAMEM_PROFILE ?? 'mamem'
const yaml = fileURLToPath(new URL('../docker-mcp/mamem.yaml', import.meta.url))

try { out(['mcp', 'version']) } catch { console.error('Docker MCP Toolkit not found — enable it in Docker Desktop (Settings → Beta features / MCP Toolkit), Docker Desktop ≥ 4.62.'); process.exit(1) }
run(['build', '-t', 'mamem:local', fileURLToPath(new URL('..', import.meta.url))])
if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(PROFILE)) run(['mcp', 'profile', 'create', '--name', PROFILE])
const listed = out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${PROFILE}`])
if (!/\bmamem\b/.test(listed)) run(['mcp', 'profile', 'server', 'add', PROFILE, '--server', `file://${yaml}`])
for (const client of process.argv.slice(2)) run(['mcp', 'client', 'connect', client, '--profile', PROFILE])
console.log(`\nmamem is in Docker MCP profile "${PROFILE}".`)
console.log(`Any client: command "docker", args ["mcp","gateway","run","--profile","${PROFILE}"]`)
