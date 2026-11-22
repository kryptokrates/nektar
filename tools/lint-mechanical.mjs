// Byte-accurate mechanical lint fixes from fresh oxlint JSON diagnostics.
// Rules handled: no-floating-promises (insert `void `), require-await
// (remove `async ` before the flagged `=>`), no-unused-vars (rename/`_`).
// Usage: node tools/lint-mechanical.mjs .lint-ta.json [--apply]
import fs from 'fs'

const dump = JSON.parse(fs.readFileSync(process.argv[2], 'utf8'))
const APPLY = process.argv.includes('--apply')
const diags = dump.diagnostics ?? dump

const edits = new Map() // file -> [{off,len,ins,del}]
const add = (f, e) => (edits.get(f) ?? edits.set(f, []).get(f)).push(e)
const misses = []

for (const d of diags) {
  const code = d.code ?? ''
  const span = d.labels?.[0]?.span
  if (span === undefined) continue
  const buf = fs.existsSync(d.filename) ? fs.readFileSync(d.filename) : null
  if (buf === null) continue

  if (code.includes('no-floating-promises')) {
    const at = span.offset
    // skip if already void-prefixed
    if (
      buf
        .subarray(Math.max(0, at - 5), at)
        .toString()
        .endsWith('void ')
    )
      continue
    add(d.filename, { off: at, len: 0, ins: 'void ' })
  } else if (code.includes('require-await')) {
    // span is ' =>' — backscan for 'async' before it on the same line
    const at = span.offset
    const lineStart = buf.lastIndexOf(10, at - 1) + 1
    const before = buf.subarray(lineStart, at).toString()
    const m = /\basync\b/g
    let last = null
    for (const x of before.matchAll(m)) last = x
    if (last === null) {
      misses.push(`${d.filename}:${span.line} require-await no-async`)
      continue
    }
    // remove `async` + one following space
    add(d.filename, { off: lineStart + last.index, len: last[0].length + (buf[lineStart + last.index + 5] === 32 ? 1 : 0), ins: '' })
  } else if (code.includes('no-unused-vars')) {
    const at = span.offset
    const name = buf.subarray(at, at + span.length).toString()
    // an import specifier: bounded scan for enclosing braces
    const seg = buf.subarray(Math.max(0, at - 400), at + 400).toString()
    const rel = at - Math.max(0, at - 400)
    let open = -1
    for (let i = rel - 1; i >= 0 && rel - i < 350; i--) {
      if (seg[i] === '{') {
        open = i
        break
      }
      if (seg[i] === '}' || seg[i] === ';' || seg[i] === '(') break
    }
    let isImport = false
    if (open >= 0) {
      const kwStart = seg.lastIndexOf('import', open)
      const close = seg.indexOf('}', rel)
      if (kwStart >= 0 && kwStart < open && close > rel && /from|import/.test(seg.slice(0, close + 1))) {
        // remove the specifier + one adjacent comma
        const absOpen = Math.max(0, at - 400) + open
        const absClose = Math.max(0, at - 400) + close
        // find comma before or after
        let cb = -1
        for (let i = at - 1; i > absOpen; i--) {
          if (buf[i] === 44) {
            cb = i
            break
          }
          if (buf[i] !== 32 && buf[i] !== 9 && buf[i] !== 10 && buf[i] !== 13) break
        }
        if (cb >= 0) add(d.filename, { off: cb, len: at + span.length - cb, ins: '' })
        else {
          let ca = -1
          for (let i = at + span.length; i < absClose; i++) {
            if (buf[i] === 44) {
              ca = i
              break
            }
            if (buf[i] !== 32 && buf[i] !== 9 && buf[i] !== 10 && buf[i] !== 13) break
          }
          add(d.filename, { off: at, len: ca >= 0 ? ca + 1 - at : span.length, ins: '' })
        }
        isImport = true
      }
    }
    if (!isImport) {
      // char before must not be a word char (mid-identifier guard)
      const prev = at > 0 ? String.fromCharCode(buf[at - 1]) : ' '
      if (/[\w$]/.test(prev)) {
        misses.push(`${d.filename}:${span.line} mid-ident '${name}'`)
        continue
      }
      // destructure shorthand? look behind for '{' or ', ' inside braces
      if (/[{,]\s*$/.test(beforeOf(buf, at)) && /^\s*[,}]/.test(buf.subarray(at + span.length, at + span.length + 2).toString())) {
        // check enclosing '{' vs '(' — shorthand lives in object patterns
        let pd = 0,
          bd = 0
        for (let k = Math.max(0, at - 300); k < at; k++) {
          if (buf[k] === 40) pd++
          else if (buf[k] === 41) pd--
          else if (buf[k] === 123) bd++
          else if (buf[k] === 125) bd--
        }
        if (bd > pd) {
          add(d.filename, { off: at, len: 0, ins: `${name}: _` })
          continue
        }
      }
      add(d.filename, { off: at, len: 0, ins: '_' })
    }
  }
}

function beforeOf(buf, at) {
  return buf.subarray(Math.max(0, at - 120), at).toString()
}

let applied = 0
for (const [f, es] of edits) {
  es.sort((a, b) => b.off - a.off)
  let s = fs.readFileSync(f, 'utf8')
  // byte->char offset map
  const raw = Buffer.from(s, 'utf8')
  const b2c = new Uint32Array(raw.length + 1)
  let ci = 0
  for (let i = 0; i < raw.length; i++) {
    b2c[i] = ci
    if ((raw[i] & 0xc0) !== 0x80) ci++
  }
  b2c[raw.length] = ci
  for (const e of es) {
    const o = b2c[e.off]
    const end = b2c[e.off + e.len]
    s = s.slice(0, o) + e.ins + s.slice(end)
    applied++
    // lengths shift after this edit only for offsets BELOW; we process
    // descending so earlier (larger) offsets were already applied — but the
    // map is stale for the edited string; recompute cheaply per edit
    // (edit ops are few) — recompute after each:
    const raw2 = Buffer.from(s, 'utf8')
    let ci2 = 0
    for (let i = 0; i < raw2.length && i <= e.off; i++) {
      b2c[i] = ci2
      if ((raw2[i] & 0xc0) !== 0x80) ci2++
    }
    for (let i = e.off; i < raw2.length; i++) {
      b2c[i] = ci2
      if ((raw2[i] & 0xc0) !== 0x80) ci2++
    }
    b2c[raw2.length] = ci2
  }
  if (APPLY) fs.writeFileSync(f, s)
}
console.log('applied', applied, 'misses', misses.length)
misses.forEach((m) => console.log(' ', m))
