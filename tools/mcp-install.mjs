// pnpm mcp:install [client ...] — build the nektar image, add it to a Docker
// MCP Toolkit profile ("nektar") and connect AI clients to that profile.
//   pnpm mcp:install                     → profile only (connect clients later)
//   pnpm mcp:install claude-desktop cursor vscode
// Clients supported by `docker mcp client connect`: claude-code claude-desktop
// cline codex continue crush cursor gemini goose gordon kiro lmstudio opencode
// sema4 vscode zed.
import { execFileSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const run = (args, opts = {}) => execFileSync('docker', args, { stdio: 'inherit', ...opts })
const out = args => execFileSync('docker', args, { encoding: 'utf8' })
const PROFILE = process.env.NEKTAR_PROFILE ?? 'nektar'
// Docker Desktop ≥ 4.97 only accepts file:// servers inside ~/.docker/mcp/catalogs
const { mkdirSync, copyFileSync } = await import('node:fs')
const { homedir } = await import('node:os')
const { join } = await import('node:path')
const catDir = join(homedir(), '.docker', 'mcp', 'catalogs', 'nektar')
mkdirSync(catDir, { recursive: true })
const staged = name => { const dst = join(catDir, name); copyFileSync(fileURLToPath(new URL(`../docker-mcp/${name}`, import.meta.url)), dst); return dst }
// --ghcr: use the published image (ghcr.io/nedzof/nektar:latest), no local build
const GHCR = process.argv.includes('--ghcr')
const yaml = staged(GHCR ? 'nektar.ghcr.yaml' : 'nektar.yaml')

try { out(['mcp', 'version']) } catch { console.error('Docker MCP Toolkit not found — enable it in Docker Desktop (Settings → Beta features / MCP Toolkit), Docker Desktop ≥ 4.62.'); process.exit(1) }
if (GHCR) run(['pull', 'ghcr.io/nedzof/nektar:latest'])
else run(['build', '-t', 'nektar:local', fileURLToPath(new URL('..', import.meta.url))])
if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(PROFILE)) run(['mcp', 'profile', 'create', '--name', PROFILE])
const listed = out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${PROFILE}`])
// re-register so the profile snapshot matches the chosen image (local vs GHCR)
if (/\bnektar\b/.test(listed)) run(['mcp', 'profile', 'server', 'remove', PROFILE, '--name', 'nektar'])
run(['mcp', 'profile', 'server', 'add', PROFILE, '--server', `file://${yaml}`])
const args = process.argv.slice(2)
if (args.includes('--regtest')) {
  // secrets from the gitignored env files → Docker MCP keychain via stdin (never printed)
  const { readFileSync, existsSync } = await import('node:fs')
  const env = p => existsSync(p) ? Object.fromEntries(readFileSync(p, 'utf8').split(/\r?\n/).map(l => l.match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)).filter(Boolean).map(m => [m[1], m[2].trim()])) : {}
  const local = env(fileURLToPath(new URL('../.env', import.meta.url)))
  const qs = env(fileURLToPath(new URL('../infra/teranode-quickstart/.env', import.meta.url)))
  const wif = process.env.NEKTAR_REGTEST_WIF ?? local.NEKTAR_REGTEST_WIF
  const pass = process.env.NEKTAR_RPC_PASS ?? qs.rpc_pass
  if (!wif || !pass) { console.error('--regtest: NEKTAR_REGTEST_WIF (nektar/.env) and rpc_pass (infra/teranode-quickstart/.env) are required'); process.exit(1) }
  execFileSync('docker', ['mcp', 'secret', 'set', 'nektar-regtest.fund_wif'], { input: wif, stdio: ['pipe', 'ignore', 'inherit'] })
  execFileSync('docker', ['mcp', 'secret', 'set', 'nektar-regtest.rpc_pass'], { input: pass, stdio: ['pipe', 'ignore', 'inherit'] })
  // own profile: the regtest server exposes the same tool names as the mock one
  const RP = `${PROFILE}_regtest` // profile IDs are slugs (no "-")
  const ryaml = staged('nektar-regtest.yaml')
  if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(RP)) run(['mcp', 'profile', 'create', '--name', RP, '--id', RP])
  // re-register so profile snapshots always reflect the current server definition
  if (/\bnektar-regtest\b/.test(out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${RP}`]))) run(['mcp', 'profile', 'server', 'remove', RP, '--name', 'nektar-regtest'])
  run(['mcp', 'profile', 'server', 'add', RP, '--server', `file://${ryaml}`])
  if (process.env.NEKTAR_CLEARANCE_AUTHORITIES) run(['mcp', 'profile', 'config', RP, '--set', `nektar-regtest.clearance_authorities=${process.env.NEKTAR_CLEARANCE_AUTHORITIES}`])
  run(['mcp', 'profile', 'config', RP, '--set', `nektar-regtest.rpc_user=${qs.rpc_user ?? 'teranode'}`, '--set', 'nektar-regtest.automine=1', '--set', `nektar-regtest.model_id=${process.env.NEKTAR_MODEL_ID ?? 'unspecified'}`])
  console.log(`profile "${RP}": nektar on regtest (secrets in the Docker MCP keychain; Teranode via host.docker.internal)`)
}
if (args.includes('--main')) {
  // MAINNET — real BSV. Only registers the profile; secrets + the explicit
  // NEKTAR_MAINNET_CONFIRM config are operator actions, never defaults.
  const MP = `${PROFILE}_main`
  const myaml = staged('nektar-main.yaml')
  if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(MP)) run(['mcp', 'profile', 'create', '--name', MP, '--id', MP])
  if (/\bnektar-main\b/.test(out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${MP}`]))) run(['mcp', 'profile', 'server', 'remove', MP, '--name', 'nektar-main'])
  run(['mcp', 'profile', 'server', 'add', MP, '--server', `file://${myaml}`])
  console.log(`profile "${MP}": nektar on MAINNET.`)
  console.log(`  required next steps (operator):`)
  console.log(`    docker mcp secret set nektar-main.key_passphrase`)
  console.log(`    docker mcp profile config ${MP} --set nektar-main.mainnet_confirm=I_UNDERSTAND`)
  console.log(`    (optional) docker mcp secret set nektar-main.arc_api_key / nektar-main.fund_wif`)
  console.log(`  the fee-key address is printed to stderr on first start — fund it deliberately.`)
}
if (args.includes('--ttn')) {
  // Teranode Test Net — test coins only; secrets + config are operator actions.
  const TP = `${PROFILE}_ttn`
  const tyaml = staged('nektar-ttn.yaml')
  if (!out(['mcp', 'profile', 'list']).split(/\s+/).includes(TP)) run(['mcp', 'profile', 'create', '--name', TP, '--id', TP])
  if (/\bnektar-ttn\b/.test(out(['mcp', 'profile', 'server', 'ls', '--filter', `profile=${TP}`]))) run(['mcp', 'profile', 'server', 'remove', TP, '--name', 'nektar-ttn'])
  run(['mcp', 'profile', 'server', 'add', TP, '--server', `file://${tyaml}`])
  console.log(`profile "${TP}": nektar on Teranode Test Net (TTN test coins).`)
  console.log(`  required next step: docker mcp secret set nektar-ttn.key_passphrase`)
  console.log(`  the fee-key address is printed to stderr on first start — fund it with TTN coins.`)
}
const target = args.includes('--regtest') ? `${PROFILE}_regtest` : args.includes('--main') ? `${PROFILE}_main` : args.includes('--ttn') ? `${PROFILE}_ttn` : PROFILE
for (const client of args.filter(a => !a.startsWith('--'))) run(['mcp', 'client', 'connect', client, '--profile', target])
console.log(`\nnektar is in Docker MCP profile "${PROFILE}".`)
console.log(`Any client: command "docker", args ["mcp","gateway","run","--profile","${PROFILE}"]`)
