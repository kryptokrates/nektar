#!/usr/bin/env node
/**
 * Zero-dependency static docs builder for https://nektar.run/docs/
 * Imports curated Markdown manuals directly from docs/*.md into clean,
 * ultra-lean, obsidian-styled HTML pages under site/docs/.
 *
 * Usage: node scripts/build_site_docs.mjs
 */
import fs from 'node:fs'
import path from 'node:path'

const ROOT = path.resolve(import.meta.dirname, '..')
const DOCS_SRC = path.join(ROOT, 'docs')
const DOCS_OUT = path.join(ROOT, 'site', 'docs')

const SECTIONS = [
  {
    group: 'Start Here',
    items: [
      { slug: 'claude-code', src: 'MANUAL-CLAUDE-CODE.md', title: 'Claude Code', desc: '25-second L3 setup with MCP, native hooks & receipt verification.' },
      { slug: 'codex', src: 'MANUAL-CODEX.md', title: 'Codex CLI', desc: 'Connect OpenAI Codex CLI via MCP + notify hook.' },
      { slug: 'hermes-openclaw', src: 'MANUAL-HERMES.md', title: 'Hermes & OpenClaw', desc: 'Native L3 memory provider plugin & memory slot.' },
      { slug: 'docking-and-hubs', src: 'DOCKING-AND-HUBS.md', title: 'Docking & Hubs', desc: 'Plain-English guide to docking AI apps and running a Hub.' },
      { slug: 'testnet', src: 'JOIN-TESTNET.md', title: 'Join Testnet', desc: 'Try autonomous buying & selling with free built-in test sats.' },
      { slug: 'mainnet', src: 'JOIN-MAINNET.md', title: 'Go Live on Mainnet', desc: 'Run on BSV Mainnet with SpendGuard caps & dual-witness PoW.' },
      { slug: 'demo', src: 'DEMO.md', title: '60-Second Demo', desc: 'End-to-end walkthrough from first fact to verified receipt.' }
    ]
  },
  {
    group: 'Knowledge Market',
    items: [
      { slug: 'market', src: 'MARKET.md', title: 'How the Market Works', desc: 'Signed wants, auto-quotes, atomic BRC-29 + BRC-369 settlement.' },
      { slug: 'money-path', src: 'MONEY-PATH.md', title: 'Money Path & Escrow', desc: 'Verify the proof first, then satoshis move.' },
      { slug: 'automation', src: 'AUTOMATION.md', title: 'Autonomous Trading', desc: 'Operator spending policies, kill switch & Restate durability.' },
      { slug: 'cold-start', src: 'COLD-START.md', title: 'Bounties & Liquidity', desc: 'Earn instant BSV payouts from open Hub bounties.' }
    ]
  },
  {
    group: 'Architecture & Proofs',
    items: [
      { slug: 'architecture', src: 'ARCHITECTURE.md', title: 'Architecture', desc: 'Single zero-CGO Go binary: SQLite, Cedar, Mangle, BSV & Restate.' },
      { slug: 'proofs', src: 'PROOFS.md', title: 'SPV Proofs & Receipts', desc: 'How nektarr1.* receipts prove origin, inclusion & derivability.' },
      { slug: 'threat-model', src: 'THREAT-MODEL.md', title: 'Threat Model', desc: 'Security boundaries, key custody & fail-closed SpendGuard.' },
      { slug: 'claims', src: 'CLAIMS.md', title: 'Honest Boundaries', desc: 'What cryptographic verification proves — and what it never claims.' },
      { slug: 'environment', src: 'ENVIRONMENT.md', title: 'Configuration & Env', desc: 'Environment variables, ports, networks & wallet modes.' }
    ]
  }
]

const ALL_ITEMS = SECTIONS.flatMap((s) => s.items)
const SRC_TO_SLUG = new Map(ALL_ITEMS.map((it) => [it.src.toLowerCase(), it.slug]))

function escHtml(s) {
  return String(s)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
}

function slugifyHeading(s) {
  return s
    .toLowerCase()
    .replace(/`[^`]*`/g, (m) => m.slice(1, -1))
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
}

function renderInline(raw) {
  let out = ''
  let i = 0
  while (i < raw.length) {
    // Inline code
    if (raw[i] === '`') {
      const end = raw.indexOf('`', i + 1)
      if (end !== -1) {
        out += `<code>${escHtml(raw.slice(i + 1, end))}</code>`
        i = end + 1
        continue
      }
    }
    // Markdown link [label](href)
    if (raw[i] === '[') {
      const closeBracket = raw.indexOf(']', i + 1)
      if (closeBracket !== -1 && raw[closeBracket + 1] === '(') {
        const closeParen = raw.indexOf(')', closeBracket + 2)
        if (closeParen !== -1) {
          const label = raw.slice(i + 1, closeBracket)
          let href = raw.slice(closeBracket + 2, closeParen).trim()
          const mdBase = href.replace(/^\.?\//, '').replace(/^docs\//, '').split('#')[0].toLowerCase()
          const hash = href.includes('#') ? '#' + href.split('#')[1] : ''
          if (SRC_TO_SLUG.has(mdBase)) {
            href = `./${SRC_TO_SLUG.get(mdBase)}.html${hash}`
          } else if (href.endsWith('.md') && !/^https?:\/\//i.test(href)) {
            href = `https://github.com/kryptokrates/nektar/blob/master/docs/${href.replace(/^\.?\//, '').replace(/^docs\//, '')}`
          }
          const ext = /^https?:\/\//i.test(href) ? ' target="_blank" rel="noopener"' : ''
          out += `<a href="${escHtml(href)}"${ext}>${renderInline(label)}</a>`
          i = closeParen + 1
          continue
        }
      }
    }
    // Bold **...**
    if (raw[i] === '*' && raw[i + 1] === '*') {
      const end = raw.indexOf('**', i + 2)
      if (end !== -1) {
        out += `<strong>${renderInline(raw.slice(i + 2, end))}</strong>`
        i = end + 2
        continue
      }
    }
    // Italic *...* or _..._
    if ((raw[i] === '*' || raw[i] === '_') && raw[i + 1] !== ' ' && (i === 0 || !/[a-zA-Z0-9]/.test(raw[i - 1]))) {
      const ch = raw[i]
      const end = raw.indexOf(ch, i + 1)
      if (end !== -1 && end > i + 1 && raw[end - 1] !== ' ') {
        out += `<em>${renderInline(raw.slice(i + 1, end))}</em>`
        i = end + 1
        continue
      }
    }
    out += escHtml(raw[i])
    i++
  }
  return out
}

function renderMarkdown(md) {
  const lines = md.replace(/\r\n/g, '\n').split('\n')
  const html = []
  let i = 0

  while (i < lines.length) {
    const line = lines[i]

    // Fenced code block (including indented blocks inside lists)
    const fence = line.match(/^\s*```(\w*)/)
    if (fence) {
      const lang = fence[1] || ''
      const indentLen = line.indexOf('```')
      const buf = []
      i++
      while (i < lines.length && !/^\s*```/.test(lines[i])) {
        const rawLine = lines[i]
        buf.push(indentLen > 0 && rawLine.startsWith(' '.repeat(indentLen)) ? rawLine.slice(indentLen) : rawLine)
        i++
      }
      i++ // skip closing fence
      const code = buf.join('\n')
      html.push(
        `<div class="code-wrap"><div class="code-head"><span>${escHtml(lang || 'text')}</span><button class="cp-btn" type="button">Copy</button></div><pre><code>${escHtml(code)}</code></pre></div>`
      )
      continue
    }

    // Blank line
    if (!line.trim()) {
      i++
      continue
    }

    // Horizontal rule
    if (/^(-{3,}|\*{3,}|_{3,})$/.test(line.trim())) {
      html.push('<hr />')
      i++
      continue
    }

    // Heading
    const hd = line.match(/^(#{1,4})\s+(.+)$/)
    if (hd) {
      const lvl = hd[1].length
      const id = slugifyHeading(hd[2])
      html.push(`<h${lvl} id="${escHtml(id)}">${renderInline(hd[2].trim())}</h${lvl}>`)
      i++
      continue
    }

    // Blockquote
    if (line.startsWith('>')) {
      const q = []
      while (i < lines.length && lines[i].startsWith('>')) {
        q.push(lines[i].replace(/^>\s?/, ''))
        i++
      }
      html.push(`<blockquote>${renderInline(q.join(' '))}</blockquote>`)
      continue
    }

    // Markdown table
    if (line.includes('|') && i + 1 < lines.length && /^\s*\|?[\s:-]+\|[\s|:-]*$/.test(lines[i + 1])) {
      const splitRow = (r) =>
        r
          .trim()
          .replace(/^\|/, '')
          .replace(/\|$/, '')
          .split('|')
          .map((c) => c.trim())
      const headers = splitRow(line)
      i += 2
      const rows = []
      while (i < lines.length && lines[i].includes('|') && lines[i].trim() !== '') {
        rows.push(splitRow(lines[i]))
        i++
      }
      html.push(
        `<div class="tbl-wrap"><table><thead><tr>${headers.map((h) => `<th>${renderInline(h)}</th>`).join('')}</tr></thead><tbody>${rows
          .map((r) => `<tr>${r.map((c) => `<td>${renderInline(c)}</td>`).join('')}</tr>`)
          .join('')}</tbody></table></div>`
      )
      continue
    }

    // Unordered list
    if (/^\s*[-*]\s+/.test(line)) {
      const items = []
      while (i < lines.length && /^\s*[-*]\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*[-*]\s+/, ''))
        i++
      }
      html.push(`<ul>${items.map((it) => `<li>${renderInline(it)}</li>`).join('')}</ul>`)
      continue
    }

    // Ordered list (preserves explicit start number across nested blocks)
    const olMatch = line.match(/^\s*(\d+)\.\s+/)
    if (olMatch) {
      const startNum = Number(olMatch[1])
      const items = []
      while (i < lines.length && /^\s*\d+\.\s+/.test(lines[i])) {
        items.push(lines[i].replace(/^\s*\d+\.\s+/, ''))
        i++
      }
      const startAttr = startNum > 1 ? ` start="${startNum}"` : ''
      html.push(`<ol${startAttr}>${items.map((it) => `<li>${renderInline(it)}</li>`).join('')}</ol>`)
      continue
    }

    // Paragraph
    const para = []
    while (
      i < lines.length &&
      lines[i].trim() !== '' &&
      !/^\s*```/.test(lines[i]) &&
      !lines[i].startsWith('#') &&
      !lines[i].startsWith('>') &&
      !/^\s*[-*]\s+/.test(lines[i]) &&
      !/^\s*\d+\.\s+/.test(lines[i]) &&
      !/^(-{3,}|\*{3,}|_{3,})$/.test(lines[i].trim())
    ) {
      para.push(lines[i].trim())
      i++
    }
    html.push(`<p>${renderInline(para.join(' '))}</p>`)
  }

  return html.join('\n')
}

const SHARED_CSS = `
:root{color-scheme:dark;--bg:#090807;--surf:#11100e;--hover:#171513;--line:#23201d;--line2:#342f2a;--ink:#f1ede9;--mut:#9a9087;--mut2:#6d655e;--amber:#f7991b;--ok:#4cc26b}
*{box-sizing:border-box}
html{scrollbar-gutter:stable}
body{margin:0;background:var(--bg);color:var(--ink);font-family:-apple-system,BlinkMacSystemFont,'Inter','Segoe UI',system-ui,sans-serif;font-size:15px;line-height:1.68;-webkit-font-smoothing:antialiased}
a{color:var(--amber);text-decoration:none}
a:hover{text-decoration:underline}
.topbar{position:sticky;top:0;z-index:30;height:56px;display:flex;align-items:center;justify-content:space-between;gap:16px;padding:0 20px;background:rgba(9,8,7,.9);backdrop-filter:blur(14px);border-bottom:1px solid var(--line)}
.brand{display:flex;align-items:center;gap:10px;color:var(--ink);font-weight:800;font-size:16px;letter-spacing:-.02em}
.brand:hover{text-decoration:none}
.brand img{width:26px;height:26px;border-radius:7px}
.nav-links{display:flex;align-items:center;gap:16px;font-size:13.5px}
.nav-links a{color:var(--mut);font-weight:600}
.nav-links a:hover,.nav-links a.on{color:var(--ink);text-decoration:none}
.shell{display:grid;grid-template-columns:248px minmax(0,1fr);max-width:1280px;margin:0 auto;min-height:calc(100vh - 56px)}
.side{border-right:1px solid var(--line);padding:20px 14px 40px;position:sticky;top:56px;height:calc(100vh - 56px);overflow-y:auto}
.ssearch{width:100%;background:var(--surf);border:1px solid var(--line);border-radius:999px;padding:7px 12px;color:var(--ink);font:inherit;font-size:13px;outline:none;margin-bottom:16px}
.ssearch:focus{border-color:var(--amber)}
.sgroup{margin-bottom:18px}
.sg-lbl{font-size:11px;font-weight:800;letter-spacing:.1em;text-transform:uppercase;color:var(--mut2);padding:0 10px;margin-bottom:6px}
.slink{display:block;padding:6px 10px;border-radius:8px;color:var(--mut);font-size:13.5px;font-weight:600;transition:background .12s,color .12s}
.slink:hover{background:var(--hover);color:var(--ink);text-decoration:none}
.slink.on{background:rgba(247,153,27,.12);color:var(--amber)}
.doc-main{padding:36px 44px 72px;max-width:860px;min-width:0}
.doc-main h1{font-size:30px;font-weight:800;letter-spacing:-.03em;line-height:1.2;margin:0 0 14px}
.doc-main h2{font-size:20px;font-weight:800;letter-spacing:-.02em;margin:34px 0 12px;padding-top:16px;border-top:1px solid var(--line)}
.doc-main h3{font-size:16px;font-weight:700;margin:24px 0 8px;color:var(--ink)}
.doc-main p{color:#d5cec7;margin:0 0 14px}
.doc-main ul,.doc-main ol{color:#d5cec7;padding-left:22px;margin:0 0 16px}
.doc-main li{margin:6px 0}
.doc-main strong{color:var(--ink)}
.doc-main hr{border:0;border-top:1px solid var(--line);margin:28px 0}
.doc-main blockquote{margin:16px 0;padding:12px 16px;background:rgba(247,153,27,.07);border-left:3px solid var(--amber);border-radius:0 10px 10px 0;color:#e8dfd5;font-size:14px}
.doc-main code{font-family:ui-monospace,'Cascadia Mono',SFMono-Regular,Consolas,monospace;font-size:.86em;background:var(--surf);border:1px solid var(--line);padding:1px 6px;border-radius:5px;color:#f3d2a4}
.code-wrap{background:#0e0d0b;border:1px solid var(--line);border-radius:12px;margin:14px 0 18px;overflow:hidden}
.code-head{display:flex;align-items:center;justify-content:space-between;padding:7px 12px;background:#141210;border-bottom:1px solid var(--line);font-family:ui-monospace,monospace;font-size:11px;color:var(--mut2);text-transform:uppercase;letter-spacing:.06em}
.cp-btn{background:var(--surf);border:1px solid var(--line2);color:var(--ink);border-radius:999px;padding:3px 10px;font:inherit;font-size:11px;font-weight:700;cursor:pointer}
.cp-btn:hover{border-color:var(--amber);color:var(--amber)}
.code-wrap pre{margin:0;padding:14px;overflow-x:auto;font-family:ui-monospace,'Cascadia Mono',SFMono-Regular,Consolas,monospace;font-size:13px;line-height:1.55;color:var(--ink)}
.code-wrap pre code{background:none;border:0;padding:0;color:inherit;font-size:inherit}
.tbl-wrap{overflow-x:auto;margin:16px 0 20px;border:1px solid var(--line);border-radius:12px}
table{width:100%;border-collapse:collapse;font-size:13.5px}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--line)}
th{background:#13110f;color:var(--mut);font-size:11.5px;text-transform:uppercase;letter-spacing:.06em;font-weight:700}
tr:last-child td{border-bottom:0}
.doc-foot{margin-top:44px;padding-top:18px;border-top:1px solid var(--line);display:flex;align-items:center;justify-content:space-between;flex-wrap:wrap;gap:12px;font-size:13px;color:var(--mut)}
.cards{display:grid;grid-template-columns:repeat(auto-fill,minmax(240px,1fr));gap:12px;margin:16px 0 28px}
.dcard{display:flex;flex-direction:column;gap:5px;padding:15px 16px;background:var(--surf);border:1px solid var(--line);border-radius:13px;color:var(--ink);transition:border-color .14s,background .14s}
.dcard:hover{border-color:var(--amber);background:var(--hover);text-decoration:none}
.dcard b{font-size:14.5px}
.dcard span{font-size:13px;color:var(--mut);line-height:1.45}
@media(max-width:860px){.shell{grid-template-columns:1fr}.side{position:static;height:auto;border-right:0;border-bottom:1px solid var(--line);padding:14px 16px}.doc-main{padding:24px 18px 56px}}
`

function sidebarHtml(activeSlug) {
  return `<aside class="side">
  <input class="ssearch" id="doc-filter" type="search" placeholder="Filter docs…" aria-label="Filter documentation" />
  <div class="sgroup">
    <a class="slink${activeSlug === 'index' ? ' on' : ''}" href="./index.html">Overview</a>
  </div>
  ${SECTIONS.map(
    (sec) => `<div class="sgroup" data-group>
    <div class="sg-lbl">${escHtml(sec.group)}</div>
    ${sec.items
      .map(
        (it) =>
          `<a class="slink${activeSlug === it.slug ? ' on' : ''}" data-item="${escHtml((it.title + ' ' + it.desc).toLowerCase())}" href="./${it.slug}.html">${escHtml(it.title)}</a>`
      )
      .join('\n    ')}
  </div>`
  ).join('\n  ')}
</aside>`
}

function pageShell({ title, desc, activeSlug, bodyHtml, sourceFile }) {
  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8" />
  <meta name="viewport" content="width=device-width,initial-scale=1" />
  <title>${escHtml(title)} — nektar docs</title>
  <meta name="description" content="${escHtml(desc)}" />
  <meta property="og:title" content="${escHtml(title)} — nektar docs" />
  <meta property="og:description" content="${escHtml(desc)}" />
  <meta property="og:image" content="https://nektar.run/og-card.png" />
  <meta name="twitter:card" content="summary_large_image" />
  <link rel="icon" type="image/svg+xml" href="../logo.svg" />
  <style>${SHARED_CSS}</style>
</head>
<body>
  <header class="topbar">
    <a class="brand" href="../index.html">
      <img src="../logo.svg" alt="nektar" />
      <span>nektar</span>
    </a>
    <nav class="nav-links" aria-label="Primary">
      <a href="../index.html">Home</a>
      <a class="on" href="./index.html">Docs</a>
      <a href="https://x.com/nektardotrun" target="_blank" rel="noopener">X</a>
      <a href="https://github.com/kryptokrates/nektar" target="_blank" rel="noopener">GitHub</a>
    </nav>
  </header>
  <div class="shell">
    ${sidebarHtml(activeSlug)}
    <main class="doc-main">
      ${bodyHtml}
      <footer class="doc-foot">
        <span>Proofs establish recorded, unaltered, and derivable — never truth.</span>
        ${sourceFile ? `<a href="https://github.com/kryptokrates/nektar/blob/master/docs/${escHtml(sourceFile)}" target="_blank" rel="noopener">Edit on GitHub ↗</a>` : `<a href="https://github.com/kryptokrates/nektar" target="_blank" rel="noopener">GitHub ↗</a>`}
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
    const filter = document.getElementById('doc-filter');
    if (filter) {
      filter.addEventListener('input', () => {
        const q = filter.value.trim().toLowerCase();
        for (const a of document.querySelectorAll('[data-item]')) {
          a.style.display = !q || a.dataset.item.includes(q) ? '' : 'none';
        }
      });
    }
  </script>
</body>
</html>`
}

fs.mkdirSync(DOCS_OUT, { recursive: true })

// 1. Build overview index page
const indexBody = `
<h1>Nektar Documentation</h1>
<p>Everything you need to connect your AI agents to one verifiable, BSV-anchored brain — from a 25-second newbie install to running a Spine-and-Leaf Hub.</p>
${SECTIONS.map(
  (sec) => `<h2>${escHtml(sec.group)}</h2>
<div class="cards">
  ${sec.items
    .map(
      (it) => `<a class="dcard" href="./${it.slug}.html"><b>${escHtml(it.title)}</b><span>${escHtml(it.desc)}</span></a>`
    )
    .join('\n  ')}
</div>`
).join('\n')}
`

fs.writeFileSync(
  path.join(DOCS_OUT, 'index.html'),
  pageShell({
    title: 'Overview',
    desc: 'Guides for Claude Code, Cursor, Codex CLI, Hermes, OpenClaw, BSV Mainnet, and the Nektar Knowledge Market.',
    activeSlug: 'index',
    bodyHtml: indexBody
  }),
  'utf8'
)

// 2. Build each curated doc page from docs/*.md
let built = 0
for (const item of ALL_ITEMS) {
  const srcPath = path.join(DOCS_SRC, item.src)
  if (!fs.existsSync(srcPath)) {
    console.warn(`skip missing: ${item.src}`)
    continue
  }
  const md = fs.readFileSync(srcPath, 'utf8')
  const html = pageShell({
    title: item.title,
    desc: item.desc,
    activeSlug: item.slug,
    bodyHtml: renderMarkdown(md),
    sourceFile: item.src
  })
  fs.writeFileSync(path.join(DOCS_OUT, `${item.slug}.html`), html, 'utf8')
  built++
}

console.log(`Built ${built} doc pages + index into ${path.relative(ROOT, DOCS_OUT)}`)
