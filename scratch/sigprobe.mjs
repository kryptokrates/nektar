import { compile } from 'runar-compiler'
import fs from 'fs'
const r = compile(fs.readFileSync('C:/tmp/sigonly.runar.ts', 'utf8'))
fs.writeFileSync('C:/tmp/sigonly.json', JSON.stringify(r.artifact))
console.log('script:', r.artifact.script)
