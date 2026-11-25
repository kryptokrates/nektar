#!/usr/bin/env node
// Recompiles all production Rúnar 0.4.6 covenants from contracts/src/*.runar.ts
// into contracts/artifacts/*.json AND syncs them bit-for-bit to internal/contracts/artifacts/*.json.
import fs from 'node:fs';
import path from 'node:path';
import { createRequire } from 'node:module';
import { fileURLToPath } from 'node:url';

const __dirname = path.dirname(fileURLToPath(import.meta.url));
const repoRoot = path.resolve(__dirname, '..');

// Load runar-compiler from local workspace or baseline cache.
const candidatePaths = [
  path.join(repoRoot, 'node_modules', 'runar-compiler', 'dist', 'index.js'),
  path.join(repoRoot, '.state', 'baseline-src', 'node_modules', '.pnpm', 'runar-compiler@0.4.6', 'node_modules', 'runar-compiler', 'dist', 'index.js'),
];

let compile;
for (const p of candidatePaths) {
  if (fs.existsSync(p)) {
    const mod = await import('file:///' + p.replace(/\\/g, '/'));
    compile = mod.compile;
    break;
  }
}
if (!compile) {
  throw new Error('runar-compiler not found in node_modules or .state/baseline-src cache');
}

const PRODUCTION_CONTRACTS = [
  'NektarAnchor',
  'NektarAutomaton',
  'NektarFeeVault',
  'NektarCheckpointBond',
  'DisclosureSale',
  'NektarZkClaim',
  'NektarGrothClaim',
  'ClaimVerdict',
];

const artDir = path.join(repoRoot, 'contracts', 'artifacts');
const embedDir = path.join(repoRoot, 'internal', 'contracts', 'artifacts');
fs.mkdirSync(artDir, { recursive: true });
fs.mkdirSync(embedDir, { recursive: true });

for (const name of PRODUCTION_CONTRACTS) {
  const srcPath = path.join(repoRoot, 'contracts', 'src', `${name}.runar.ts`);
  const src = fs.readFileSync(srcPath, 'utf8');
  const res = compile(src);
  if (!res.success) {
    console.error(`COMPILE FAILED for ${name}:`, JSON.stringify(res.diagnostics, null, 2));
    process.exit(1);
  }
  const out = JSON.stringify(res.artifact, (k, v) => (typeof v === 'bigint' ? v.toString() : v), 2);
  fs.writeFileSync(path.join(artDir, `${name}.json`), out);
  fs.writeFileSync(path.join(embedDir, `${name}.json`), out);
  console.log(`compiled & synced: ${name}.json (${res.artifact.script.length / 2} bytes)`);
}
