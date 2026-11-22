// Remove flagged unnecessary `x as T` / `x!` assertions via the TS parser.
// A flagged span ends exactly at its AsExpression/NonNullExpression node end,
// so matching on e === n.end is unambiguous (nested `a as T as U` chains are
// distinct nodes with distinct ends).
import { readFileSync, writeFileSync } from 'node:fs'
import ts from 'typescript'

const diags = JSON.parse(readFileSync('.lint-ta.json', 'utf8')).diagnostics
const sites = new Map()
for (const d of diags) {
  if (!(d.code || '').includes('no-unnecessary-type-assertion')) continue
  const s = d.labels?.[0]?.span ?? d.span
  if (!s) continue
  const arr = sites.get(d.filename) ?? []
  arr.push(s.offset + s.length)
  sites.set(d.filename, arr)
}

let done = 0
const misses = []
for (const [file, ends] of sites) {
  const buf = readFileSync(file)
  // diagnostics give byte offsets; the TS AST uses JS string (UTF-16 code-unit)
  // positions — convert each byte end to a char position
  const endsC = ends.map((e) => buf.toString('utf8', 0, e).length)
  const src = buf.toString('utf8')
  const sf = ts.createSourceFile(file, src, ts.ScriptTarget.Latest, true)
  const edits = []
  const endSet = new Set(endsC)
  const matched = new Set()
  const visit = (n) => {
    if (ts.isAsExpression(n) || ts.isNonNullExpression(n)) {
      // span end is the node end, or just past it when closers ()/;/, follow
      for (const e of endSet) {
        if (matched.has(e)) continue
        if (n.end <= e && e - n.end <= 3 && /^[\s;,)]*$/.test(src.slice(n.end, e))) {
          // the node's own start must precede the flagged span start — the
          // span covers expr+assertion; require the gap to be only closers
          edits.push([n.expression.end, n.end])
          matched.add(e)
          break
        }
      }
    }
    ts.forEachChild(n, visit)
  }
  visit(sf)
  for (const e of endsC) if (!matched.has(e)) misses.push(`${file}@${e}`)
  edits.sort((a, b) => b[0] - a[0])
  let out = src
  for (const [s, e] of edits) out = out.slice(0, s) + out.slice(e)
  if (edits.length) writeFileSync(file, out)
  done += edits.length
}
console.log('removed ' + done)
if (misses.length) console.log('MISSES:\n' + misses.join('\n'))
