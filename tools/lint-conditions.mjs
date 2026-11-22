// no-unnecessary-condition mechanical fixes from fresh oxlint JSON:
//  - 'optional chain on non-nullish': remove the '?' inside the flagged span
//  - 'left-hand side of ??': delete ' ?? <rhs>' after the flagged lhs span
// Usage: node tools/lint-conditions.mjs .lint-ta.json [--apply]
import fs from 'fs'

const dump = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'))
const APPLY = process.argv.includes('--apply')
const diags = dump.diagnostics ?? dump

const OPEN = new Set(['(', '[', '{'])
const CLOSE = new Set([')', ']', '}'])
function exprEnd(line, i) {
  let depth = 0
  let str = null
  let j = i
  while (j < line.length) {
    const c = line[j]
    if (str !== null) {
      if (c === '\\') j += 2
      else if (c === str) {
        str = null
        j++
      } else j++
      continue
    }
    if (c === '"' || c === "'" || c === '`') {
      str = c
      j++
      continue
    }
    if (OPEN.has(c)) {
      depth++
      j++
      continue
    }
    if (CLOSE.has(c)) {
      if (depth === 0) return j
      depth--
      j++
      continue
    }
    if (depth === 0 && (c === ',' || c === ';' || c === '?' || c === ':' || (c === '=' && line[j + 1] === '='))) return j
    if (depth === 0 && c === '&' && line[j + 1] === '&') return j
    if (depth === 0 && c === '|' && line[j + 1] === '|') return j
    j++
  }
  return j
}

const edits = new Map()
const misses = []
const add = (f, e) => (edits.get(f) ?? edits.set(f, []).get(f)).push(e)

for (const d of diags) {
  if (!(d.code ?? '').includes('no-unnecessary-condition')) continue
  const span = d.labels?.[0]?.span
  const msg = d.message ?? ''
  if (span === undefined || !fs.existsSync(d.filename)) continue
  const buf = fs.readFileSync(d.filename)

  if (msg.includes('optional chain')) {
    // span covers 'm?.v' — the '?' sits at offset+1 for '?.', or inside for '?.['
    const seg = buf.subarray(span.offset, span.offset + span.length).toString()
    const qi = seg.indexOf('?.')
    if (qi < 0) {
      misses.push(`${d.filename}:${span.line} optchain`)
      continue
    }
    add(d.filename, { off: span.offset + qi, len: 1, ins: '' })
  } else if (msg.includes('left-hand side')) {
    // span = lhs; rhs begins after '??' — delete ' ?? rhs'
    const lineStart = buf.lastIndexOf(10, span.offset) + 1
    const lineEnd = buf.indexOf(10, span.offset)
    const line = buf.subarray(lineStart, lineEnd < 0 ? buf.length : lineEnd).toString()
    const rel = span.offset - lineStart
    const after = line.slice(rel + span.length)
    const m = /^\s*\?\?\s*/.exec(after)
    if (m === null) {
      misses.push(`${d.filename}:${span.line} noqq`)
      continue
    }
    const rhsStart = rel + span.length + m[0].length
    const rhsEnd = exprEnd(line, rhsStart)
    if (rhsEnd <= rhsStart) {
      misses.push(`${d.filename}:${span.line} rhs`)
      continue
    }
    add(d.filename, { off: span.offset + span.length, len: rhsEnd - rel - span.length, ins: '' })
  }
}

let applied = 0
for (const [f, es] of edits) {
  es.sort((a, b) => b.off - a.off)
  let s = fs.readFileSync(f, 'utf8')
  const raw = Buffer.from(s, 'utf8')
  const b2c = new Uint32Array(raw.length + 1)
  let ci = 0
  for (let i = 0; i < raw.length; i++) {
    b2c[i] = ci
    if ((raw[i] & 0xc0) !== 0x80) ci++
  }
  b2c[raw.length] = ci
  // recompute lazily per edit region (descending order keeps earlier offsets valid)
  for (const e of es) {
    s = s.slice(0, b2c[e.off]) + e.ins + s.slice(b2c[e.off + e.len])
    applied++
  }
  if (APPLY) fs.writeFileSync(f, s)
}
console.log('applied', applied, 'misses', misses.length)
misses.slice(0, 30).forEach((m) => console.log(' ', m))
