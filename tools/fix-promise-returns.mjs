// Wrap expressions at tsc error sites in Promise.resolve(...) — for the
// require-await strip fallout: sync impls of Promise-typed members.
// Usage: node tools/fix-promise-returns.mjs <pkg> <dir> [--apply]
// Dry-run by default: prints what would wrap + sites queued for manual work.
import fs from 'fs'
import cp from 'child_process'

const pkg = process.argv[2]
const dir = process.argv[3]
const APPLY = process.argv.includes('--apply')

const out = cp.execSync(`pnpm --filter ${pkg} build 2>&1 || true`, { maxBuffer: 1 << 24 }).toString()
// match only errors for THIS package: "packages/<pkg> build: <path>(l,c)" —
// never '../other/...' paths from tsc -b project references
const re = /^((?:src|test)\/[\w./-]+)\((\d+),(\d+)\): error (TS\d+):/gm
const errs = [...out.matchAll(re)].map((m) => ({
  f: m[1],
  ln: +m[2],
  col: +m[3],
  code: m[4]
}))

const files = {}
for (const e of errs) (files[e.f] ??= []).push(e)

const OPEN = new Set(['(', '[', '{'])
const CLOSE = new Set([')', ']', '}'])

// Scan an expression starting at index i; return its end index (exclusive),
// or -1 when it spans lines / starts with a block body.
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
      if (depth === 0) return j // unmatched closer terminates the expr
      depth--
      j++
      continue // keep scanning: operators may continue the expression
    }
    if (depth === 0 && (c === ',' || c === ';')) return j
    j++
  }
  return depth === 0 ? j : -1
}

const manual = []
let applied = 0
for (const [f, es] of Object.entries(files)) {
  const p = `${dir}/${f}`
  if (!fs.existsSync(p)) {
    manual.push(`${f} NOFILE`)
    continue
  }
  const lines = fs.readFileSync(p, 'utf8').split('\n')
  es.sort((a, b) => b.ln - a.ln || b.col - a.col) // bottom-up: positions stay valid
  for (const e of es) {
    const li = e.ln - 1
    const line = lines.at(li)
    if (line === undefined) {
      manual.push(`${f}:${e.ln} BADLINE`)
      continue
    }
    const seg = line.slice(e.col - 1)
    const arrowM = /^[\w\s,()[\].'":?|<>-]*=>/.exec(seg)
    if (arrowM) {
      const body = seg.slice(arrowM[0].length)
      const ws = body.match(/^\s*/)[0].length
      if (body[ws] === '{') {
        manual.push(`${f}:${e.ln} BLOCK ${line.trim().slice(0, 80)}`)
        continue
      }
      const bs = e.col - 1 + arrowM[0].length + ws
      const be = exprEnd(line, bs)
      if (be === -1) {
        manual.push(`${f}:${e.ln} ML ${line.trim().slice(0, 80)}`)
        continue
      }
      lines[li] = line.slice(0, bs) + 'Promise.resolve(' + line.slice(bs, be) + ')' + line.slice(be)
      applied++
      continue
    }
    let i = e.col - 1
    while (i < line.length && line[i] === ' ') i++
    if (line.startsWith('return', i)) {
      manual.push(`${f}:${e.ln} RET ${line.trim().slice(0, 80)}`)
      continue
    }
    // column on an object-literal key — the wrap belongs on the whole
    // literal (usually a multiline object upstream): not line-local
    if (/^[A-Za-z_$][\w$]*\s*:/.test(line.slice(i))) {
      manual.push(`${f}:${e.ln} KEY ${line.trim().slice(0, 80)}`)
      continue
    }
    // shorthand object member `{ a, b }` — the member is brace-scoped, so the
    // honest expansion is `b: Promise.resolve(b)`, not `Promise.resolve(b)`
    {
      const stack = []
      let str = null
      for (let k = 0; k < i; k++) {
        const c = line[k]
        if (str !== null) {
          if (c === '\\') k++
          else if (c === str) str = null
          continue
        }
        if (c === '"' || c === "'" || c === '`') {
          str = c
          continue
        }
        if (c === '(' || c === '[' || c === '{') stack.push(c)
        else if (c === ')' || c === ']' || c === '}') stack.pop()
      }
      const top = stack[stack.length - 1]
      const idM = /^[A-Za-z_$][\w$]*/.exec(line.slice(i))
      const afterArrow = /=>\s*$/.test(line.slice(0, i))
      if (top === '{' && idM && !afterArrow && /^\s*[,}]/.test(line.slice(i + idM[0].length))) {
        const id = idM[0]
        lines[li] = line.slice(0, i) + `${id}: Promise.resolve(${id})` + line.slice(i + id.length)
        applied++
        continue
      }
    }
    // column on an assignment target — the wrap belongs on the RHS
    // (typically an arrow fn or expression), not on `x = y` itself
    const assignM = /^[.\w$[\]'"]+\s*=(?![=>=])/.exec(line.slice(i))
    if (assignM) {
      i += assignM[0].length
      while (i < line.length && line[i] === ' ') i++
      const rhs = line.slice(i)
      const rhsArrow = /^[\w\s,()[\].'":?|<>-]*=>/.exec(rhs)
      if (rhsArrow) {
        const body = rhs.slice(rhsArrow[0].length)
        const ws = body.match(/^\s*/)[0].length
        if (body[ws] === '{') {
          manual.push(`${f}:${e.ln} BLOCK ${line.trim().slice(0, 80)}`)
          continue
        }
        const bs = i + rhsArrow[0].length + ws
        const be2 = exprEnd(line, bs)
        if (be2 === -1) {
          manual.push(`${f}:${e.ln} ML ${line.trim().slice(0, 80)}`)
          continue
        }
        lines[li] = line.slice(0, bs) + 'Promise.resolve(' + line.slice(bs, be2) + ')' + line.slice(be2)
        applied++
        continue
      }
    }
    const be = exprEnd(line, i)
    if (be === -1 || be === i) {
      manual.push(`${f}:${e.ln} ${be === -1 ? 'ML' : 'EMPTY'} ${line.trim().slice(0, 80)}`)
      continue
    }
    lines[li] = line.slice(0, i) + 'Promise.resolve(' + line.slice(i, be) + ')' + line.slice(be)
    applied++
  }
  if (APPLY) fs.writeFileSync(p, lines.join('\n'))
}
console.log('applied', applied, 'manual', manual.length)
manual.forEach((m) => console.log(' ', m))
