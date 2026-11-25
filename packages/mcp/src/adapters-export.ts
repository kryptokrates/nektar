// adapters-export.ts — serves bundled L3 native plugins (Hermes, OpenClaw) and
// agentskills.io SKILL.md over HTTP (/v1/adapters/*) so the thin GHCR host CLI
// can wire full L3 integrations onto the user's machine with zero git checkout.
import { existsSync, readdirSync, readFileSync, statSync } from 'node:fs'
import type { IncomingMessage, ServerResponse } from 'node:http'
import { dirname, join, relative, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const SKIP_DIRS = new Set(['__pycache__', 'node_modules', '.pytest_cache', '.git', 'tests'])
const SKIP_EXTS = new Set(['.pyc', '.pyo'])

const FALLBACK_SKILL_MD = `---
name: nektar-memory
description: >
  Use nektar verifiable, encrypted agent memory anchored on BSV. Recall
  saved knowledge before answering factual questions, persist durable facts
  with provenance, route verifiable answers through groundedAnswer with
  receipt footers, and optionally buy/sell knowledge under approved policies.
---

# nektar — Verifiable Agent Memory

1. **Recall first:** Before answering factual questions that may be in memory,
   call \`recall\` (omit \`shard\` to discover readable shards such as \`welcome\`).
2. **Ground & cite:** Route verifiable answers through \`groundedAnswer\` so
   every claim cites a verified fragment and receives a checkable receipt.
3. **Keep the footer:** End grounded answers with the returned \`receipt:\`
   and \`verify:\` lines — never fabricate tokens.
4. **Remember with provenance:** When the user shares durable facts, call
   \`remember\` with \`shard\`, \`data\`, and \`source\`.
5. **Honest boundary:** Receipts prove content was *recorded, unaltered, and
   derivable* — never that it is *true*.
`

/** Locate bundled integrations/ directory in either the GHCR/deploy root
 *  (../../integrations) or a source repo checkout (../../../../integrations). */
export function resolveBundledRoot(moduleUrl: string = import.meta.url): string | undefined {
  const here = dirname(fileURLToPath(moduleUrl))
  for (const rel of ['../..', '../../../..']) {
    const cand = resolve(here, rel)
    if (existsSync(join(cand, 'integrations', 'hermes')) || existsSync(join(cand, 'integrations', 'openclaw'))) return cand
  }
  return undefined
}

interface BundleFile {
  path: string
  content: string
}

function collectFiles(rootDir: string, curDir = rootDir, out: BundleFile[] = []): BundleFile[] {
  for (const name of readdirSync(curDir)) {
    if (SKIP_DIRS.has(name)) continue
    const full = join(curDir, name)
    const st = statSync(full)
    if (st.isDirectory()) {
      collectFiles(rootDir, full, out)
    } else if (st.isFile()) {
      const dot = name.lastIndexOf('.')
      if (dot >= 0 && SKIP_EXTS.has(name.slice(dot))) continue
      const relPath = relative(rootDir, full).replace(/\\/g, '/')
      out.push({ path: relPath, content: readFileSync(full, 'utf8') })
    }
  }
  return out
}

function loadAdapterBundle(name: 'hermes' | 'openclaw', rootDir?: string): BundleFile[] | undefined {
  const root = rootDir ?? resolveBundledRoot()
  if (!root) return undefined
  const dir = join(root, 'integrations', name)
  if (!existsSync(dir)) return undefined
  return collectFiles(dir)
}

export function loadSkillMarkdown(rootDir?: string): string {
  const root = rootDir ?? resolveBundledRoot()
  if (root) {
    const p = join(root, 'skills', 'nektar-memory', 'SKILL.md')
    if (existsSync(p)) return readFileSync(p, 'utf8')
  }
  return FALLBACK_SKILL_MD
}

export function handleAdaptersRoute(req: IncomingMessage, res: ServerResponse, url: URL, rootDir?: string): boolean {
  if (req.method !== 'GET' || !url.pathname.startsWith('/v1/adapters/')) return false
  const sendJson = (code: number, body: unknown) => {
    res.writeHead(code, { 'content-type': 'application/json' })
    res.end(JSON.stringify(body))
  }

  if (url.pathname === '/v1/adapters/manifest') {
    const hermes = loadAdapterBundle('hermes', rootDir)
    const openclaw = loadAdapterBundle('openclaw', rootDir)
    sendJson(200, {
      v: 1,
      adapters: {
        hermes: { available: hermes !== undefined, files: hermes?.map((f) => f.path) ?? [] },
        openclaw: { available: openclaw !== undefined, files: openclaw?.map((f) => f.path) ?? [] },
        skill: { available: true, path: '/v1/adapters/skill.md' }
      }
    })
    return true
  }

  if (url.pathname === '/v1/adapters/skill.md') {
    res.writeHead(200, { 'content-type': 'text/markdown; charset=utf-8' })
    res.end(loadSkillMarkdown(rootDir))
    return true
  }

  const m = /^\/v1\/adapters\/bundle\/(hermes|openclaw)$/.exec(url.pathname)
  if (m) {
    const name = m[1] as 'hermes' | 'openclaw'
    const files = loadAdapterBundle(name, rootDir)
    if (!files) {
      sendJson(404, { error: `adapter bundle "${name}" not packaged in this build` })
      return true
    }
    sendJson(200, { name, files })
    return true
  }

  sendJson(404, { error: 'unknown adapter route' })
  return true
}
