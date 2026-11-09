#!/usr/bin/env node
// tools/service.mjs — always-on install, native + dependency-free.
//   node tools/service.mjs install     write + enable the unit for this OS
//   node tools/service.mjs uninstall   remove
//   node tools/service.mjs status      is it installed/running + health probe
//   node tools/service.mjs start|stop
//   node tools/service.mjs logs        where output goes
//
// Linux  → systemd --user unit (~/.config/systemd/user/nektar.service)
// Windows→ schtasks onlogon task running tools/service-run.mjs
// macOS  → launchd agent (~/Library/LaunchAgents/run.nektar.plist)
import { existsSync, mkdirSync, writeFileSync, readFileSync, rmSync } from 'node:fs'
import { join, resolve } from 'node:path'
import { homedir, userInfo, platform } from 'node:os'
import { execFileSync, spawnSync } from 'node:child_process'
import { fileURLToPath } from 'node:url'

const root = resolve(fileURLToPath(new URL('..', import.meta.url)))
const cmd = process.argv[2] ?? 'status'
const node = process.execPath
const runner = join(root, 'tools/service-run.mjs')
const dataDir = process.env.NEKTAR_DATA_DIR ?? join(root, '.nektar')
const logFile = join(dataDir, 'service.log')
const os = platform() // linux | win32 | darwin
const run = (c, a, quiet = false) => { const r = spawnSync(c, a, { stdio: quiet ? 'pipe' : 'inherit', encoding: 'utf8' }); return { ok: r.status === 0, out: (r.stdout ?? '') + (r.stderr ?? '') } }
const envExists = existsSync(join(root, '.env'))

// ─── preflight ────────────────────────────────────────────────────────────
const preflight = () => {
  const bad = []
  if (!existsSync(join(root, 'packages/mcp/dist/src/bin.js'))) bad.push('no build — run pnpm -r build first')
  if (!envExists) bad.push('no .env — run pnpm init:nektar first')
  if (envExists) {
    const env = readFileSync(join(root, '.env'), 'utf8')
    if (!/^NEKTAR_HTTP_PORT=/m.test(env) && !process.env.NEKTAR_HTTP_PORT) bad.push('no NEKTAR_HTTP_PORT — service would serve stdio only; pnpm config:nektar set NEKTAR_HTTP_PORT 8080')
  }
  return bad
}

// ─── platform drivers ─────────────────────────────────────────────────────
const linux = {
  unit: join(homedir(), '.config/systemd/user/nektar.service'),
  install () {
    mkdirSync(join(homedir(), '.config/systemd/user'), { recursive: true })
    writeFileSync(this.unit, `[Unit]\nDescription=nektar agent (verifiable memory + market)\nAfter=network-online.target\n\n[Service]\nType=simple\nWorkingDirectory=${root}\nEnvironmentFile=${join(root, '.env').replace(/%/g, '%%')}\nExecStart=${node} ${runner}\nRestart=on-failure\nRestartSec=10\n\n[Install]\nWantedBy=default.target\n`)
    console.log(`wrote ${this.unit}`)
    run('systemctl', ['--user', 'daemon-reload'])
    run('systemctl', ['--user', 'enable', '--now', 'nektar.service'])
    console.log('installed + started. For boot persistence without login:\n  sudo loginctl enable-linger ' + userInfo().username)
  },
  uninstall () {
    run('systemctl', ['--user', 'disable', '--now', 'nektar.service'])
    if (existsSync(this.unit)) rmSync(this.unit)
    run('systemctl', ['--user', 'daemon-reload'])
    console.log('removed')
  },
  status () {
    const r = run('systemctl', ['--user', 'is-active', 'nektar.service'], true)
    console.log(`systemd: ${r.out.trim() || 'not installed'}`)
  },
  start () { run('systemctl', ['--user', 'start', 'nektar.service']) },
  stop () { run('systemctl', ['--user', 'stop', 'nektar.service']) },
  logs () { console.log('journalctl --user -u nektar.service -f') }
}

const win = {
  // Startup-folder launcher — no Task Scheduler dependency, works where
  // schtasks /create is denied. .vbs wrapper runs the .cmd hidden at logon.
  cmdFile: join(dataDir, 'nektar-service.cmd'),
  vbs: join(homedir(), 'AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/nektar-service.vbs'),
  install () {
    mkdirSync(dataDir, { recursive: true })
    writeFileSync(this.cmdFile, `@echo off\r\ncd /d "${root}"\r\n"${node}" "${runner}" >> "${logFile}" 2>&1\r\n`)
    writeFileSync(this.vbs, `CreateObject("Wscript.Shell").Run "cmd /c """ & "${this.cmdFile.replace(/"/g, '""')}" & """", 0, False\r\n`)
    console.log(`installed — runs at logon via Startup folder (${this.vbs}).\n  runner: ${this.cmdFile}\n  logs → ${logFile}`)
  },
  uninstall () {
    if (existsSync(this.vbs)) rmSync(this.vbs)
    if (existsSync(this.cmdFile)) rmSync(this.cmdFile)
    console.log('removed')
  },
  status () { console.log(existsSync(this.vbs) ? `startup launcher: installed (${this.vbs})` : 'startup launcher: not installed') },
  start () { existsSync(this.cmdFile) ? console.log(run('cmd', ['/c', 'start', '/min', this.cmdFile]).ok ? 'started' : 'start failed') : console.error('not installed') },
  stop () { run('powershell', ['-NoProfile', '-Command', `Get-CimInstance Win32_Process -Filter "name='node.exe'" | Where-Object { $_.CommandLine -match 'service-run' } | ForEach-Object { Stop-Process -Id $_.ProcessId -Force }`]) },
  logs () { console.log(`log file: ${logFile}`) }
}

const darwin = {
  plist: join(homedir(), 'Library/LaunchAgents/run.nektar.plist'),
  install () {
    mkdirSync(join(homedir(), 'Library/LaunchAgents'), { recursive: true })
    mkdirSync(dataDir, { recursive: true })
    writeFileSync(this.plist, `<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict>\n<key>Label</key><string>run.nektar</string>\n<key>ProgramArguments</key><array><string>${node}</string><string>${runner}</string></array>\n<key>WorkingDirectory</key><string>${root}</string>\n<key>KeepAlive</key><true/>\n<key>StandardOutPath</key><string>${logFile}</string>\n<key>StandardErrorPath</key><string>${logFile}</string>\n</dict></plist>\n`)
    console.log(`wrote ${this.plist}`)
    run('launchctl', ['bootstrap', `gui/${process.getuid()}`, this.plist])
    console.log('installed + started')
  },
  uninstall () {
    run('launchctl', ['bootout', `gui/${process.getuid()}/${'run.nektar'}`])
    if (existsSync(this.plist)) rmSync(this.plist)
    console.log('removed')
  },
  status () {
    const r = run('launchctl', ['print', `gui/${process.getuid()}/run.nektar`], true)
    console.log(r.ok ? 'launchd: installed' : 'launchd: not installed')
  },
  start () { run('launchctl', ['kickstart', `gui/${process.getuid()}/run.nektar`]) },
  stop () { run('launchctl', ['kill', 'SIGTERM', `gui/${process.getuid()}/run.nektar`]) },
  logs () { console.log(`log file: ${logFile}`) }
}

const drv = { linux, win32: win, darwin }[os]
if (!drv) { console.error(`unsupported platform ${os}`); process.exit(1) }

if (cmd === 'install') {
  const bad = preflight()
  if (bad.length) { for (const b of bad) console.error(`  ! ${b}`); console.error('\nfix the above, then re-run install'); process.exit(1) }
}
if (!drv[cmd]) { console.error(`usage: node tools/service.mjs install|uninstall|start|stop|status|logs`); process.exit(2) }
drv[cmd]()
if (cmd === 'status') {
  const port = process.env.NEKTAR_HTTP_PORT
  if (port) fetch(`http://127.0.0.1:${port}/v1/health`, { signal: AbortSignal.timeout(2000) }).then(r => console.log(`health: ${r.ok ? 'up' : r.status}`)).catch(() => console.log('health: down'))
}
