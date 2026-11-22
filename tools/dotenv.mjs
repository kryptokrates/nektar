// tools/dotenv.mjs — the .env parser contract for pre-build scripts.
// tools/* run before any workspace build, so they cannot import
// @nektar/core's dist — this is the SAME contract as parseDotEnv in
// packages/core/src/env.ts, kept deliberately trivial to keep in sync:
// '#' comments only after whitespace outside quotes, surrounding quotes
// strip, 'export K=' accepted, whitespace around '=' tolerated.
// Any change here must land in env.ts too (and vice versa).
export const parseDotEnv = (text) => {
  const out = {}
  for (const raw of text.split(/\r?\n/)) {
    const line = raw.trim()
    if (line === '' || line.startsWith('#')) continue
    const m = line.match(/^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/)
    if (!m) continue
    let v = m[2].trim()
    if (v.startsWith('"') || v.startsWith("'")) {
      const q = v[0]
      const end = v.indexOf(q, 1)
      v = end === -1 ? v.slice(1) : v.slice(1, end)
    } else {
      const h = v.indexOf(' #')
      if (h !== -1) v = v.slice(0, h).trim()
    }
    out[m[1]] = v
  }
  return out
}
