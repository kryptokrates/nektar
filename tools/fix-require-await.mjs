// Strip `async` from functions flagged require-await (they contain no await,
// so dropping async cannot strand an await). Sites whose signatures require a
// Promise return are then wrapped in Promise.resolve in a second, build-driven
// pass.
import { readFileSync, writeFileSync } from 'node:fs'
import ts from 'typescript'

const diags = JSON.parse(readFileSync('.lint-ta.json', 'utf8')).diagnostics
const sites = new Map()
for (const d of diags) {
  if (!(d.code || '').includes('require-await')) continue
  const s = d.labels?.[0]?.span ?? d.span
  if (!s) continue
  const arr = sites.get(d.filename) ?? []
  arr.push({ start: s.offset, end: s.offset + s.length })
  sites.set(d.filename, arr)
}

const isFn = (n) => ts.isFunctionDeclaration(n) || ts.isFunctionExpression(n) || ts.isArrowFunction(n) || ts.isMethodDeclaration(n)

let done = 0
const misses = []
for (const [file, spans] of sites) {
  const buf = readFileSync(file)
  const src = buf.toString('utf8')
  // byte offsets -> char positions
  const spansC = spans.map((s) => ({
    start: buf.toString('utf8', 0, s.start).length,
    end: buf.toString('utf8', 0, s.end).length
  }))
  const sf = ts.createSourceFile(file, src, ts.ScriptTarget.Latest, true)
  const edits = []
  const seen = new Set()
  // collect every async fn containing each span; the innermost (smallest range)
  // is the flagged one — pre-order would hit the OUTER function first otherwise
  const fnSpans = []
  const collect = (n) => {
    if (isFn(n) && n.modifiers?.some((m) => m.kind === ts.SyntaxKind.AsyncKeyword)) {
      fnSpans.push(n)
    }
    ts.forEachChild(n, collect)
  }
  collect(sf)
  for (const s of spansC) {
    const cands = fnSpans.filter((n) => n.getStart(sf) <= s.start && s.end <= n.end)
    cands.sort((a, b) => a.end - a.getStart(sf) - (b.end - b.getStart(sf)))
    const n = cands[0]
    if (!n) {
      misses.push(`${file}@${s.start}`)
      continue
    }
    if (seen.has(n.getStart(sf))) continue
    seen.add(n.getStart(sf))
    const asyncMod = n.modifiers.find((m) => m.kind === ts.SyntaxKind.AsyncKeyword)
    let e = asyncMod.end
    while (e < src.length && /\s/.test(src[e])) e++
    edits.push([asyncMod.getStart(sf), e])
  }
  edits.sort((a, b) => b[0] - a[0])
  let out = src
  for (const [s, e] of edits) out = out.slice(0, s) + out.slice(e)
  if (edits.length) writeFileSync(file, out)
  done += edits.length
}
console.log('stripped ' + done)
if (misses.length) console.log('MISSES ' + misses.length + ':\n' + misses.slice(0, 30).join('\n'))
