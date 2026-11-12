#!/usr/bin/env node
import { readFileSync } from 'node:fs'
import { join } from 'node:path'
import { verifyTradeEvidence } from '../../../mcp/dist/src/automation/evidence.js'
const dir = process.argv[2]
if (!dir) {
  console.error('usage: pnpm scenarios:verify -- <evidence-dir>')
  process.exit(2)
}
const e = JSON.parse(readFileSync(join(dir, 's1-evidence.json'), 'utf8'))
const v = verifyTradeEvidence(e)
console.log(JSON.stringify(v, null, 1))
process.exit(v.ok ? 0 : 1)
