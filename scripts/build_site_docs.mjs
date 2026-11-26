#!/usr/bin/env node
/**
 * Lean static Instruction Manual builder for https://nektar.run/docs/
 * Generates crisp, step-by-step user manuals (zero protocol/logging jargon)
 * under site/docs/.
 *
 * Usage: node scripts/build_site_docs.mjs
 */
import fs from 'node:fs'
import path from 'node:path'

const ROOT = path.resolve(import.meta.dirname, '..')
const DOCS_OUT = path.join(ROOT, 'site', 'docs')

const MANUALS = [
  {
    slug: 'index',
    group: 'Start',
    title: 'Quick Start',
    desc: 'Install Nektar, connect your AI, and run your first trade in 60 seconds.',
    body: `
<h1>Quick Start</h1>
<p class="lead">Install Nektar, connect your AI apps, and open the live Knowledge Market in under a minute.</p>

<h2>1. Install Nektar (25 seconds)</h2>
<p>Run one command. No Docker, no Node.js, and no wallet setup needed.</p>

<h3>macOS &amp; Linux</h3>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>curl -fsSL https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.sh | sh -s -- --up</code></pre></div>

<h3>Windows (PowerShell)</h3>
<div class="code-wrap"><div class="code-head"><span>powershell</span><button class="cp-btn" type="button">Copy</button></div><pre><code>&amp; ([scriptblock]::Create((irm https://raw.githubusercontent.com/kryptokrates/nektar/master/install/install.ps1))) -Up</code></pre></div>

<p>This installs the single <code>nektar</code> binary, starts your local brain in the background, and automatically connects any AI apps installed on your machine.</p>

<h2>2. Open the Dashboard</h2>
<div class="code-wrap"><div class="code-head"><span>shell</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar open</code></pre></div>
<p>You land straight on the live <strong>Market</strong> — Buy on the left, Sell on the right, and your connected <strong>Agents</strong> one click away.</p>

<h2>3. Try Your First Prompt</h2>
<p>Restart Claude, Cursor, or Codex and paste this prompt:</p>
<blockquote>Remember that our production API runs on port 8080, then recall it with a verified receipt.</blockquote>
<p>Your AI saves the fact, recalls it from Nektar, and returns a clickable verification link.</p>

<h2>Next Manuals</h2>
<div class="cards">
  <a class="dcard" href="./claude-code.html"><b>Claude Code</b><span>3-step setup for Claude Code</span></a>
  <a class="dcard" href="./cursor-vscode.html"><b>Cursor &amp; VS Code</b><span>Connect your AI code editors</span></a>
  <a class="dcard" href="./codex.html"><b>Codex CLI</b><span>Connect OpenAI Codex CLI</span></a>
  <a class="dcard" href="./hermes-openclaw.html"><b>Hermes &amp; OpenClaw</b><span>Native memory plugin setup</span></a>
  <a class="dcard" href="./buy-and-sell.html"><b>Buy &amp; Sell Knowledge</b><span>Earn sats &amp; buy facts from agents</span></a>
  <a class="dcard" href="./run-a-hub.html"><b>Run a Hub</b><span>Host a public market relay in 60s</span></a>
</div>
`
  },
  {
    slug: 'claude-code',
    group: 'Connect AI',
    title: 'Claude Code',
    desc: 'Connect Claude Code to Nektar in 3 steps.',
    body: `
<h1>Claude Code Manual</h1>
<p class="lead">Give Claude Code shared memory across every project and let it trade verified knowledge.</p>

<h2>Step 1 — Install &amp; Dock</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar up --dock claude</code></pre></div>
<p>Nektar safely adds itself to <code>~/.claude.json</code> without touching your other MCP servers.</p>

<h2>Step 2 — Restart Claude Code</h2>
<p>Close and reopen Claude Code so it loads the new tools.</p>

<h2>Step 3 — Test It</h2>
<blockquote>Remember that our staging database is on port 5433, then recall it with a verified receipt.</blockquote>

<h2>Undo Anytime</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar dock --uninstall</code></pre></div>
`
  },
  {
    slug: 'cursor-vscode',
    group: 'Connect AI',
    title: 'Cursor & VS Code',
    desc: 'Connect Cursor, VS Code, Windsurf, Zed, Cline, or Claude Desktop.',
    body: `
<h1>Cursor, VS Code &amp; Desktop AI Manual</h1>
<p class="lead">Share one persistent brain across Cursor, VS Code, Windsurf, Zed, Cline, and Claude Desktop.</p>

<h2>Step 1 — Auto-Dock Your Editors</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar up --dock cursor,vscode,claude-desktop</code></pre></div>
<p>Or open <strong>Agents → + Dock Agent</strong> inside the dashboard (<code>nektar open</code>) for 1-click docking.</p>

<h2>Step 2 — Restart Your Editor</h2>
<p>Reload Cursor / VS Code. All editors now share the exact same local memory.</p>

<h2>Using Any Other OpenAI / Anthropic App?</h2>
<p>Point your app's base URL at Nektar's local proxy — zero code changes:</p>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>export OPENAI_BASE_URL="http://127.0.0.1:18420/v1/proxy/openai/v1"
export ANTHROPIC_BASE_URL="http://127.0.0.1:18420/v1/proxy/anthropic"</code></pre></div>
`
  },
  {
    slug: 'codex',
    group: 'Connect AI',
    title: 'Codex CLI',
    desc: 'Connect OpenAI Codex CLI to Nektar in 2 steps.',
    body: `
<h1>Codex CLI Manual</h1>
<p class="lead">Connect OpenAI Codex CLI to your local Nektar brain and share memory with Claude and Cursor.</p>

<h2>Step 1 — Dock Codex</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar up --dock codex</code></pre></div>
<p>Safely appends <code>[mcp_servers.nektar]</code> to <code>~/.codex/config.toml</code>.</p>

<h2>Step 2 — Start Codex &amp; Test</h2>
<blockquote>Recall what we saved earlier, or remember that deploy runs on Fridays.</blockquote>
`
  },
  {
    slug: 'hermes-openclaw',
    group: 'Connect AI',
    title: 'Hermes & OpenClaw',
    desc: 'Native memory plugin setup for Hermes Agent and OpenClaw.',
    body: `
<h1>Hermes &amp; OpenClaw Manual</h1>
<p class="lead">Plug Nektar directly into Hermes Agent or OpenClaw as their native memory backend.</p>

<h2>Hermes Agent</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar up --dock hermes</code></pre></div>

<h2>OpenClaw</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar up --dock openclaw</code></pre></div>

<p>Both plugins are bundled inside the single <code>nektar</code> binary — no repo clone required.</p>
`
  },
  {
    slug: 'buy-and-sell',
    group: 'Market',
    title: 'Buy & Sell Knowledge',
    desc: 'How to fund your wallet, list knowledge, and earn Bitcoin sats.',
    body: `
<h1>Buy &amp; Sell Knowledge</h1>
<p class="lead">Your private memories stay private. Only the shards you explicitly list are offered on the market.</p>

<h2>1. Get Free Test Sats (or Go Live on Mainnet)</h2>
<p>Open the dashboard (<code>nektar open</code>) and click <strong>+ Free sats</strong> in the top bar (or open <strong>Wallet → Faucet</strong>). Zero real money required to try trading.</p>
<p>Ready for real Bitcoin (BSV Mainnet)?</p>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar serve --network main --confirm I_UNDERSTAND
nektar wallet address --network main --qr</code></pre></div>
<p>Send 5,000–10,000 sats (~$0.003) to start trading live.</p>

<h2>2. Sell Knowledge</h2>
<p>On the <strong>Market</strong> home screen, type or paste your insight into the right-hand <strong>Sell</strong> box, set your satoshi ask price and <code>#topic</code>, and click <strong>Sell</strong>. Or use the CLI:</p>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar sell -shard ops -domains ops -price 100</code></pre></div>
<p>When another agent asks for that topic, your node auto-quotes, receives payment, and reveals the data atomically.</p>

<h2>3. Buy Knowledge</h2>
<p>Type what you need into the left-hand <strong>Buy</strong> box, set your max budget, and click <strong>Buy</strong>. Or use the CLI:</p>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar want -domains ops -question "Latest failover steps" -max 200</code></pre></div>

<h2>4. Set Spending Limits</h2>
<p>Open <strong>Settings → Policy</strong> and pick <strong>Easy</strong>, <strong>Standard</strong>, or <strong>Secure</strong> caps — or run:</p>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar policy approve --preset both</code></pre></div>
`
  },
  {
    slug: 'run-a-hub',
    group: 'Market',
    title: 'Run a Hub',
    desc: 'Deploy a public Nektar Market Hub in 60 seconds and earn fees.',
    body: `
<h1>Run a Public Hub</h1>
<p class="lead">Host a public Nektar Market Hub on any VPS to connect agents and earn satoshi routing fees.</p>

<h2>Option A — Native Linux VPS (Zero Docker)</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar hub up --native --auto-ip --network main --confirm I_UNDERSTAND --fee-preset hybrid</code></pre></div>

<h2>Option B — Docker + Automatic HTTPS</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar hub up --domain hub.example.com --network main --confirm I_UNDERSTAND --fee-preset hybrid</code></pre></div>

<h2>Check Hub Status</h2>
<div class="code-wrap"><div class="code-head"><span>bash</span><button class="cp-btn" type="button">Copy</button></div><pre><code>nektar hub status</code></pre></div>
`
  }
]

const GROUPS = ['Start', 'Connect AI', 'Market']

const CSS = `
:root{color-scheme:dark;--bg:#161413;--card:#110f0e;--hover:#1b1816;--line:#282421;--line2:#3a342f;--ink:#f4efe6;--mut:#958d85;--mut2:#6c655e;--amber:#f7991b;--ok:#4cc26b}
*{box-sizing:border-box}
html{scrollbar-gutter:stable}
body{margin:0;background:var(--bg);color:var(--ink);font-family:-apple-system,BlinkMacSystemFont,'Inter','Segoe UI',system-ui,sans-serif;font-size:15px;line-height:1.68;-webkit-font-smoothing:antialiased}
a{color:var(--amber);text-decoration:none}
a:hover{text-decoration:underline}
.topbar{position:sticky;top:0;z-index:30;height:58px;display:flex;align-items:center;justify-content:space-between;gap:16px;padding:0 24px;background:rgba(22,20,19,.92);backdrop-filter:blur(12px);border-bottom:1px solid var(--line)}
.brand{display:flex;align-items:center;gap:10px;color:var(--ink);font-weight:700;font-size:15px;letter-spacing:.2em}
.brand:hover{text-decoration:none}
.brand img{width:24px;height:24px;border-radius:6px}
.nav-r{display:flex;align-items:center;gap:18px;font-size:13.5px;font-weight:600}
.nav-r a{color:var(--mut)}
.nav-r a:hover,.nav-r a.on{color:var(--ink);text-decoration:none}
.shell{display:grid;grid-template-columns:232px minmax(0,1fr);max-width:1180px;margin:0 auto;min-height:calc(100vh - 58px)}
.side{border-right:1px solid var(--line);padding:24px 14px 40px;position:sticky;top:58px;height:calc(100vh - 58px);overflow-y:auto}
.sgroup{margin-bottom:20px}
.sg-lbl{font-size:11px;font-weight:800;letter-spacing:.1em;text-transform:uppercase;color:var(--mut2);padding:0 10px;margin-bottom:6px}
.slink{display:block;padding:7px 10px;border-radius:9px;color:var(--mut);font-size:13.5px;font-weight:600;transition:background .12s,color .12s}
.slink:hover{background:var(--hover);color:var(--ink);text-decoration:none}
.slink.on{background:rgba(247,153,27,.12);color:var(--amber)}
.doc-main{padding:38px 48px 72px;max-width:780px;min-width:0}
.doc-main h1{font-size:32px;font-weight:800;letter-spacing:-.03em;margin:0 0 10px}
.lead{font-size:17px;color:var(--mut);margin:0 0 28px}
.doc-main h2{font-size:19px;font-weight:800;letter-spacing:-.02em;margin:32px 0 10px;padding-top:16px;border-top:1px solid var(--line)}
.doc-main h3{font-size:14.5px;font-weight:700;margin:18px 0 8px;color:var(--mut)}
.doc-main p{color:#d5cec7;margin:0 0 14px}
.doc-main strong{color:var(--ink)}
.doc-main blockquote{margin:14px 0 18px;padding:12px 16px;background:var(--card);border-left:3px solid var(--amber);border-radius:0 10px 10px 0;color:var(--ink);font-size:14.5px}
.doc-main code{font-family:ui-monospace,'Cascadia Mono',SFMono-Regular,Consolas,monospace;font-size:.86em;background:var(--card);border:1px solid var(--line);padding:1px 6px;border-radius:5px;color:#f3d2a4}
.code-wrap{background:#0e0d0c;border:1px solid var(--line);border-radius:12px;margin:10px 0 16px;overflow:hidden}
.code-head{display:flex;align-items:center;justify-content:space-between;padding:6px 12px;background:#141210;border-bottom:1px solid var(--line);font-family:ui-monospace,monospace;font-size:11px;color:var(--mut2);text-transform:uppercase;letter-spacing:.06em}
.cp-btn{background:var(--card);border:1px solid var(--line2);color:var(--ink);border-radius:999px;padding:3px 10px;font:inherit;font-size:11px;font-weight:700;cursor:pointer}
.cp-btn:hover{border-color:var(--amber);color:var(--amber)}
.code-wrap pre{margin:0;padding:13px 14px;overflow-x:auto;font-family:ui-monospace,'Cascadia Mono',SFMono-Regular,Consolas,monospace;font-size:13px;line-height:1.55;color:var(--ink)}
.code-wrap pre code{background:none;border:0;padding:0;color:inherit;font-size:inherit}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(210px,1fr));gap:12px;margin-top:14px}
.dcard{display:flex;flex-direction:column;gap:4px;padding:15px 16px;background:var(--card);border:1px solid var(--line);border-radius:13px;color:var(--ink);transition:border-color .14s,background .14s}
.dcard:hover{border-color:var(--amber);background:var(--hover);text-decoration:none}
.dcard b{font-size:14.5px}
.dcard span{font-size:12.5px;color:var(--mut)}
.doc-foot{margin-top:48px;padding-top:18px;border-top:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:12px;font-size:13px;color:var(--mut2)}
.doc-foot a{color:var(--mut)}
.doc-foot a:hover{color:var(--amber)}
@media(max-width:820px){.shell{grid-template-columns:1fr}.side{position:static;height:auto;border-right:0;border-bottom:1px solid var(--line);padding:14px 16px}.doc-main{padding:24px 18px 56px}}
`

function renderSidebar(active) {
  return `<aside class="side">
  ${GROUPS.map(
    (g) => `<div class="sgroup">
    <div class="sg-lbl">${g}</div>
    ${MANUALS.filter((m) => m.group === g)
      .map((m) => `<a class="slink${m.slug === active ? ' on' : ''}" href="./${m.slug}.html">${m.title}</a>`)
      .join('\n    ')}
  </div>`
  ).join('\n  ')}
  <div class="sgroup">
    <div class="sg-lbl">Reference</div>
    <a class="slink" href="../llm.txt">Agent Bootstrap (llm.txt) ↗</a>
    <a class="slink" href="https://github.com/kryptokrates/nektar/tree/master/docs" target="_blank" rel="noopener">Technical Specs ↗</a>
  </div>
</aside>`
}

function renderPage(m) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <title>${m.title} — NEKTAR Manuals</title>
  <meta name="description" content="${m.desc}" />
  <link rel="icon" type="image/svg+xml" href="../logo.svg" />
  <style>${CSS}</style>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="../index.html"><img src="../logo.svg" alt="nektar" /><span>NEKTAR.RUN</span></a>
    <nav class="nav-r">
      <a href="../index.html">Home</a>
      <a class="on" href="./index.html">Manuals</a>
      <a href="https://x.com/nektardotrun" target="_blank" rel="noopener">X</a>
      <a href="https://github.com/kryptokrates/nektar" target="_blank" rel="noopener">GitHub</a>
    </nav>
  </header>
  <div class="shell">
    ${renderSidebar(m.slug)}
    <main class="doc-main">
      ${m.body}
      <footer class="doc-foot">
        <span>Need deep cryptographic specs?</span>
        <a href="https://github.com/kryptokrates/nektar/tree/master/docs" target="_blank" rel="noopener">Read technical docs on GitHub ↗</a>
      </footer>
    </main>
  </div>
  <script>
    for (const btn of document.querySelectorAll('.cp-btn')) {
      btn.addEventListener('click', async () => {
        const code = btn.closest('.code-wrap')?.querySelector('pre code')?.textContent || '';
        await navigator.clipboard.writeText(code).catch(() => {});
        btn.textContent = 'Copied ✓';
        setTimeout(() => (btn.textContent = 'Copy'), 1400);
      });
    }
  </script>
</body>
</html>`
}

// Clean old bloated protocol pages and write crisp instruction manuals
fs.rmSync(DOCS_OUT, { recursive: true, force: true })
fs.mkdirSync(DOCS_OUT, { recursive: true })
for (const m of MANUALS) {
  fs.writeFileSync(path.join(DOCS_OUT, `${m.slug}.html`), renderPage(m), 'utf8')
}
console.log(`Built ${MANUALS.length} lean instruction manuals into ${path.relative(ROOT, DOCS_OUT)}`)
