// MamemAutomatonZk — measured variant, DOCUMENTED-NOT-RUN.
// Reason: the compiled locking script is 883,419 bytes — 8.6× the 100 KB
// production cap — so it is kept OUT of all production paths and its
// Spend-eval is not exercised (per-handoff: over the cap ⇒ measure + compile
// result only, mark tests documented-not-run). The artifact exists at
// packages/contracts/MamemAutomatonZk.json for reference.
import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
import path from 'node:path'

const artifact = JSON.parse(readFileSync(path.join(fileURLToPath(import.meta.url), '..', '..', '..', '..', 'contracts', 'MamemAutomatonZk.json'), 'utf8'))

test('MamemAutomatonZk: compiled + size recorded (production gate: >100 KB, variant only)', () => {
  assert.ok(artifact.script.length > 0, 'artifact compiled')
  assert.equal(artifact.script.length / 2, 883419, 'measured 883,419 B — over the 100 KB cap → kept out of production paths (documented)')
})
