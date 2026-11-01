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
// Docker Desktop ≥ 4.97 only accepts file:// servers inside ~/.docker/mcp/catalogs
const { mkdirSync, copyFileSync } = await import('node:fs')
const { homedir } = await import('node:os')
const { join } = await import('node:path')
const catDir = join(homedir(), '.docker', 'mcp', 'catalogs', 'mamem')
mkdirSync(catDir, { recursive: true })
const staged = name => { const dst = join(catDir, name); copyFileSync(fileURLToPath(new URL(`../docker-mcp/${name}`, import.meta.url)), dst); return dst }
const yaml = staged('mamem.yaml')

try { out(['mcp', 'version']) } catch { console.error('Docker MCP Toolkit not found — enable it in Docker Desktop (Settings → Beta features / MCP Toolkit), Docker Desktop ≥ 4.62.'); process.exit(1) }
run(['build', '-t', 'mamem:local', fileURLToPath(new URL('..', import.meta.url))])
if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(PROFILE)) run(['mcp', 'profile', 'create', '--name', PROFILE])
const listed = out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${PROFILE}`])
if (!/\bmamem\b/.test(listed)) run(['mcp', 'profile', 'server', 'add', PROFILE, '--server', `file://${yaml}`])
const args = process.argv.slice(2)
if (args.includes('--regtest')) {
  // secrets from the gitignored env files → Docker MCP keychain via stdin (never printed)
  const { readFileSync, existsSync } = await import('node:fs')
  const env = p => existsSync(p) ? Object.fromEntries(readFileSync(p, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()])) : {}
  const local = env(fileURLToPath(new URL('../.env', import.meta.url)))
  const qs = env(fileURLToPath(new URL('../infra/teranode-quickstart/.env', import.meta.url)))
  const wif = process.env.MAMEM_REGTEST_WIF ?? local.MAMEM_REGTEST_WIF
  const pass = process.env.MAMEM_RPC_PASS ?? qs.rpc_pass
  if (!wif || !pass) { console.error('--regtest: MAMEM_REGTEST_WIF (mamem/.env) and rpc_pass (infra/teranode-quickstart/.env) are required'); process.exit(1) }
  execFileSync('docker', ['mcp', 'secret', 'set', 'mamem-regtest.fund_wif'], { input: wif, stdio: ['pipe', 'ignore', 'inherit'] })
  execFileSync('docker', ['mcp', 'secret', 'set', 'mamem-regtest.rpc_pass'], { input: pass, stdio: ['pipe', 'ignore', 'inherit'] })
  // own profile: the regtest server exposes the same tool names as the mock one
  const RP = `${PROFILE}_regtest` // profile IDs are slugs (no "-")
  const ryaml = staged('mamem-regtest.yaml')
  if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(RP)) run(['mcp', 'profile', 'create', '--name', RP, '--id', RP])
  if (!/\bmamem-regtest\b/.test(out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${RP}`]))) run(['mcp', 'profile', 'server', 'add', RP, '--server', `file://${ryaml}`])
  run(['mcp', 'profile', 'config', RP, '--set', `mamem-regtest.rpc_user=${qs.rpc_user ?? 'teranode'}`, '--set', 'mamem-regtest.automine=1', '--set', `mamem-regtest.model_id=${process.env.MAMEM_MODEL_ID ?? 'unspecified'}`])
  console.log(`profile "${RP}": mamem on regtest (secrets in the Docker MCP keychain; Teranode via host.docker.internal)`)
}
const target = args.includes('--regtest') ? `${PROFILE}_regtest` : PROFILE
for (const client of args.filter(a => !a.startsWith('--'))) run(['mcp', 'client', 'connect', client, '--profile', target])
console.log(`\nmamem is in Docker MCP profile "${PROFILE}".`)
console.log(`Any client: command "docker", args ["mcp","gateway","run","--profile","${PROFILE}"]`)
