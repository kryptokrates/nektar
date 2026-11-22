// Revert `Promise.resolve(x)` where it was written as a bare object-literal
// member (stack-top === '{') — those should be plain shorthand `x`.
import fs from 'fs'

for (const f of process.argv.slice(2)) {
  const lines = fs.readFileSync(f, 'utf8').split('\n')
  let cnt = 0
  const out = lines
    .map((line) => {
      let res = ''
      const stack = []
      let str = null
      let i = 0
      while (i < line.length) {
        const c = line[i]
        if (str !== null) {
          res += c
          if (c === '\\' && i + 1 < line.length) {
            res += line[i + 1]
            i += 2
            continue
          }
          if (c === str) str = null
          i++
          continue
        }
        if (c === '"' || c === "'" || c === '`') {
          str = c
          res += c
          i++
          continue
        }
        const m = /^Promise\.resolve\(([A-Za-z_$][\w$]*)\)/.exec(line.slice(i))
        if (m && stack[stack.length - 1] === '{' && /^\s*[,}]/.test(line.slice(i + m[0].length))) {
          res += m[1]
          i += m[0].length
          cnt++
          continue
        }
        if (c === '(' || c === '[' || c === '{') stack.push(c)
        else if (c === ')' || c === ']' || c === '}') stack.pop()
        res += c
        i++
      }
      return res
    })
    .join('\n')
  fs.writeFileSync(f, out)
  console.log(f, cnt)
}
