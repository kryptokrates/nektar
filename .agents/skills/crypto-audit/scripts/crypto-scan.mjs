#!/usr/bin/env node
// crypto-scan.mjs — Zero-dependency, strictly read-only static pre-scanner
// for Go cryptographic packages and Rúnar Bitcoin Script ZK covenants.
//
// Usage:
//   node scripts/crypto-scan.mjs [target-dir] [--include-tests] [--no-probe]

import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import { fileURLToPath } from 'node:url';

const SKIP_DIRS = new Set([
  '.git',
  '.state',
  '.tools',
  'node_modules',
  'dist',
  'ui',
  'target',
  'scratch',
  'infra',
  'artifacts',
  'deploy',
  'site',
]);

const GO_CRYPTO_IMPORT_RE =
  /"(crypto\/[^"]+|golang\.org\/x\/crypto\/[^"]+|github\.com\/bsv-blockchain\/go-sdk\/primitives\/(?:ec|hash)|github\.com\/lightwebinc\/bcommon\/(?:keyed|cbor|record|carrier|payee|bwallet)|github\.com\/consensys\/gnark[^"]*|github\.com\/cloudflare\/circl[^"]*|github\.com\/tink-crypto\/tink-go[^"]*|github\.com\/awnumar\/memguard[^"]*|github\.com\/cedar-policy\/cedar-go[^"]*)"/g;

const RUNAR_PRIMITIVE_RE =
  /\b(?:runar\.)?(ecMulGen|EcMulGen|ecMul|EcMul|ecAdd|EcAdd|ecOnCurve|EcOnCurve|checkPreimage|CheckPreimage|extractHashPrevouts|ExtractHashPrevouts|extractOutputHash|ExtractOutputHash|extractLocktime|ExtractLocktime|extractSequence|ExtractSequence|checkSig|CheckSig|hash256|Hash256|sha256|Sha256|hash160|Hash160|2013265921n?)\b/g;

function normalizeRel(rootDir, fullPath) {
  return path.relative(rootDir, fullPath).split(path.sep).join('/');
}

function lineNumberAt(content, index) {
  let line = 1;
  for (let i = 0; i < index; i++) {
    if (content.charCodeAt(i) === 10) line++;
  }
  return line;
}

function lineTextAt(lines, lineNum) {
  return (lines[lineNum - 1] || '').trim();
}

function addLead(leads, { rule, severity, file, line, snippet, detail }) {
  leads.push({
    rule,
    severity,
    where: `${file}:${line}`,
    file,
    line,
    snippet: snippet.slice(0, 180),
    detail,
  });
}

/**
 * Scans a single Go source file for cryptographic inventory & static leads.
 */
export function scanGoSource(relPath, content) {
  const lines = content.split(/\r?\n/);
  const imports = new Set();
  for (const m of content.matchAll(GO_CRYPTO_IMPORT_RE)) {
    imports.add(m[1]);
  }

  const leads = [];

  // 1. Weak primitives / TLS bypass
  const weakChecks = [
    { re: /"crypto\/md5"/g, detail: 'MD5 imported — verify it is never used for security/integrity' },
    { re: /"crypto\/sha1"/g, detail: 'SHA-1 imported — verify it is never used for signatures/commitments' },
    { re: /"crypto\/rc4"/g, detail: 'RC4 cipher is broken and forbidden' },
    { re: /"crypto\/des"/g, detail: 'DES/3DES cipher is deprecated and weak' },
    { re: /InsecureSkipVerify\s*:\s*true/g, detail: 'TLS certificate verification disabled (MITM)' },
  ];
  for (const chk of weakChecks) {
    for (const m of content.matchAll(chk.re)) {
      const ln = lineNumberAt(content, m.index);
      addLead(leads, {
        rule: 'crypto/weak-primitive',
        severity: 'HIGH',
        file: relPath,
        line: ln,
        snippet: lineTextAt(lines, ln),
        detail: chk.detail,
      });
    }
  }

  // 2. Unchecked crypto/rand.Read
  for (const m of content.matchAll(/(?:_\s*,\s*_\s*=|\b_\s*=)\s*(?:crypto\/)?rand\.Read\s*\(/g)) {
    const ln = lineNumberAt(content, m.index);
    addLead(leads, {
      rule: 'key/unchecked-rng',
      severity: 'HIGH',
      file: relPath,
      line: ln,
      snippet: lineTextAt(lines, ln),
      detail: 'rand.Read error discarded — handle CSPRNG failure explicitly instead of continuing with zero bytes',
    });
  }

  // 3. math/rand inside a file that also touches crypto/auth/tokens
  if (/"math\/rand(?:\/v2)?"/.test(content) && (imports.size > 0 || /\b(token|nonce|secret|key|salt|csrf)\b/i.test(content))) {
    for (const m of content.matchAll(/"math\/rand(?:\/v2)?"/g)) {
      const ln = lineNumberAt(content, m.index);
      addLead(leads, {
        rule: 'key/unchecked-rng',
        severity: 'HIGH',
        file: relPath,
        line: ln,
        snippet: lineTextAt(lines, ln),
        detail: 'math/rand imported in file with crypto/auth symbols — verify it is never used for secrets/nonces/tokens',
      });
    }
  }

  // 4. Ad-hoc SHA-256 KDF (derive*Key using sha256 without hkdf)
  const kdfFuncRe = /func\s+(?:\([^)]+\)\s*)?([A-Za-z0-9_]*(?:derive|kdf)[A-Za-z0-9_]*key[A-Za-z0-9_]*)\s*\([^)]*\)/gi;
  for (const m of content.matchAll(kdfFuncRe)) {
    const start = m.index;
    const bodySlice = content.slice(start, Math.min(content.length, start + 1200));
    if (/sha256\.(?:New|Sum256)/.test(bodySlice) && !/\bhkdf\b/i.test(bodySlice)) {
      const ln = lineNumberAt(content, start);
      addLead(leads, {
        rule: 'crypto/adhoc-kdf',
        severity: 'MED',
        file: relPath,
        line: ln,
        snippet: lineTextAt(lines, ln),
        detail: `Function ${m[1]} derives key material via raw SHA-256 instead of RFC 5869 crypto/hkdf`,
      });
    }
  }

  // 5. Delimiter-joined AAD
  for (const m of content.matchAll(/func\s+[A-Za-z0-9_]*aad[A-Za-z0-9_]*\s*\([^)]*\)[^{]*\{[\s\S]{0,400}?"\|"/gi)) {
    const ln = lineNumberAt(content, m.index);
    addLead(leads, {
      rule: 'crypto/aad-ambiguity',
      severity: 'MED',
      file: relPath,
      line: ln,
      snippet: lineTextAt(lines, ln),
      detail: 'AAD constructed with "|" delimiter concatenation — check for delimiter ambiguity across variable-length fields',
    });
  }

function extractBalancedParens(content, openParenIdx) {
  let depth = 0;
  for (let i = openParenIdx; i < content.length && i < openParenIdx + 1000; i++) {
    const ch = content[i];
    if (ch === '(') depth++;
    else if (ch === ')') {
      depth--;
      if (depth === 0) return content.slice(openParenIdx + 1, i);
    }
  }
  return '';
}

  // 6. Unframed Concat with multiple raw []byte(...) casts in digest/hash expressions
  for (const m of content.matchAll(/(?:DSHA256|SHA256)\s*\(\s*(?:core\.)?Concat\s*\(/g)) {
    const openIdx = m.index + m[0].length - 1;
    const args = extractBalancedParens(content, openIdx);
    const rawByteCasts = args.match(/\[\]byte\s*\([^)]+\)/g) || [];
    if (rawByteCasts.length >= 2) {
      const ln = lineNumberAt(content, m.index);
      addLead(leads, {
        rule: 'hash/unframed-concat',
        severity: 'HIGH',
        file: relPath,
        line: ln,
        snippet: lineTextAt(lines, ln),
        detail: 'Hash over Concat with multiple unframed []byte(...) fields — length-prefix variable fields to prevent boundary collisions',
      });
    }
  }

  // 7. Delimiter-formatted string passed to hash (fmt.Sprintf("...|...") + DSHA256/SHA256)
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/fmt\.Sprintf\s*\([^)]*\|/.test(line)) {
      const window = lines.slice(i, Math.min(lines.length, i + 4)).join('\n');
      if (/(?:DSHA256|SHA256|sha256\.)/.test(window)) {
        addLead(leads, {
          rule: 'hash/delimiter-collision',
          severity: 'MED',
          file: relPath,
          line: i + 1,
          snippet: line.trim(),
          detail: 'Hash computed over "|"-delimited fmt.Sprintf string — verify inputs cannot contain delimiter',
        });
      }
    }
  }

  // 8. Variable-time comparison on secret/auth/token/mac/key identifiers
  const secretIdentRe = /\b[a-zA-Z0-9_.]*(?:secret|token|tok|mac|hmac|csrf|verifier|privkey|apikey|password|passwd)[a-zA-Z0-9_]*\b/i;
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^\s*\/\//.test(line) || /ConstantTimeCompare/.test(line)) continue;
    const eqCall = line.match(/\b(?:bytes\.Equal|slices\.Equal|EqualBytes)\s*\(([^)]+)\)/);
    if (eqCall && secretIdentRe.test(eqCall[1])) {
      addLead(leads, {
        rule: 'ct/variable-time-secret-cmp',
        severity: 'HIGH',
        file: relPath,
        line: i + 1,
        snippet: line.trim(),
        detail: 'Variable-time byte comparison involving secret/auth/token identifier — prefer crypto/subtle.ConstantTimeCompare',
      });
    }
  }

  // 9. VerifyFragmentStructural called outside test files (and outside its own func declaration)
  if (!relPath.endsWith('_test.go')) {
    for (let i = 0; i < lines.length; i++) {
      const line = lines[i];
      if (/^\s*\/\//.test(line)) continue;
      if (/\bVerifyFragmentStructural\s*\(/.test(line) && !/^\s*func\s+VerifyFragmentStructural\b/.test(line)) {
        addLead(leads, {
          rule: 'spv/structural-verify-in-prod',
          severity: 'BLOCKER',
          file: relPath,
          line: i + 1,
          snippet: line.trim(),
          detail: 'VerifyFragmentStructural called in non-test code — live networks must verify BUMP against a real PoW ChainTracker',
        });
      }
    }
  }

  const inventory =
    imports.size > 0
      ? {
          kind: 'go',
          file: relPath,
          primitives: [...imports].sort(),
        }
      : null;

  return { inventory, leads };
}

/**
 * Scans a single Rúnar covenant file (.runar.go or .runar.ts) for ZK/crypto inventory & static leads.
 */
export function scanRunarSource(relPath, content) {
  const lines = content.split(/\r?\n/);
  const primitives = new Set();
  for (const m of content.matchAll(RUNAR_PRIMITIVE_RE)) {
    primitives.add(m[1]);
  }

  const leads = [];

  // 1. BabyBear modular arithmetic (% 2013265921[n]) without explicit input range guard
  const bbMod = content.match(/%\s*2013265921n?\b/);
  if (bbMod) {
    const hasRangeGuard = /<\s*2013265921n?\b/.test(content) && />=\s*0n?\b/.test(content);
    if (!hasRangeGuard) {
      const ln = lineNumberAt(content, bbMod.index);
      addLead(leads, {
        rule: 'script/signed-mod-neg',
        severity: 'HIGH',
        file: relPath,
        line: ln,
        snippet: lineTextAt(lines, ln),
        detail: 'BabyBear OP_MOD (% 2013265921) without visible 0 <= x < p input range assertions — Bitcoin Script OP_MOD preserves negative signs',
      });
    }
  }

  // 2. Manual 1-byte length prefix num2bin/Num2Bin(len/Len(...), 1[n])
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/^\s*\/\//.test(line)) continue;
    if (/(?:runar\.)?(?:num2bin|Num2Bin)\s*\(\s*(?:(?:runar\.)?(?:len|Len)\s*\([^)]+\)|sl)\s*,\s*1n?\s*\)/.test(line)) {
      const hasBound = /(?:(?:runar\.)?(?:len|Len)\s*\([^)]+\)|sl)\s*(?:<=?\s*(?:75|127|128)n?|===?\s*(?:20|32|36)n?)\b/.test(content);
      if (!hasBound) {
        addLead(leads, {
          rule: 'script/length-prefix-overflow',
          severity: 'MED',
          file: relPath,
          line: i + 1,
          snippet: line.trim(),
          detail: '1-byte signed num2bin length prefix without <= 75 / < 128 guard — signed Bitcoin Script numbers flip sign at >= 128',
        });
      }
    }
  }

  // 3. Unpinned stage/verifier TODO inside ZK covenant
  for (let i = 0; i < lines.length; i++) {
    const line = lines[i];
    if (/TODO.*(?:bind|pin|template|verifier|stage)/i.test(line)) {
      addLead(leads, {
        rule: 'zk/unpinned-vk-or-stage',
        severity: 'BLOCKER',
        file: relPath,
        line: i + 1,
        snippet: line.trim(),
        detail: 'Unfinished TODO for on-chain script/verifier/stage pinning in ZK covenant',
      });
    }
  }

  // 4. Schnorr-PoK Fiat-Shamir completeness check (ecMulGen/EcMulGen + sha256/Sha256 challenge)
  const schnorrMatch =
    content.match(/public\s+(\w+)\s*\(([^)]*)\)\s*\{([\s\S]*?)ecMulGen\s*\(/) ||
    content.match(/func\s+\([^)]+\)\s+([A-Z]\w*)\s*\(([^)]*)\)\s*\{([\s\S]*?)(?:runar\.)?EcMulGen\s*\(/);
  if (schnorrMatch) {
    const methodName = schnorrMatch[1];
    const params = schnorrMatch[2];
    const preBody = schnorrMatch[3];
    const challengeMatch = preBody.match(/(?:(?:const|let)\s+\w+\s*(?::\s*ByteString)?\s*=|\w+\s*:=)\s*(?:runar\.)?(?:sha256|Sha256)\s*\(([^\n;]+)\)/);
    if (challengeMatch) {
      const challengeIdx = preBody.indexOf(challengeMatch[0]);
      const challengeWindow = preBody.slice(0, challengeIdx + challengeMatch[0].length);
      const missing = [];
      for (const field of ['sym', 'outHash', 'from']) {
        if (new RegExp(`\\b${field}\\b`).test(params) && !new RegExp(`\\b${field}\\b`).test(challengeWindow)) {
          missing.push(field);
        }
      }
      if (missing.length > 0) {
        const ln = lineNumberAt(content, schnorrMatch.index + schnorrMatch[0].indexOf(challengeMatch[0]));
        addLead(leads, {
          rule: 'zk/fiat-shamir-incomplete',
          severity: 'BLOCKER',
          file: relPath,
          line: ln,
          snippet: lineTextAt(lines, ln),
          detail: `Schnorr-PoK method ${methodName} Fiat-Shamir challenge omits method transition parameter(s): ${missing.join(', ')}`,
        });
      }
    }
  }

  // 5. Stateless SmartContract using txPreimage/TxPreimage without checkPreimage/CheckPreimage
  const isStateless = /extends\s+SmartContract\b/.test(content) || /\brunar\.SmartContract\b/.test(content);
  if (isStateless && /\b(?:txPreimage|TxPreimage)\b/.test(content) && !/\b(?:checkPreimage|CheckPreimage)\s*\(/.test(content)) {
    const idx = content.search(/\b(?:txPreimage|TxPreimage)\b/);
    const ln = lineNumberAt(content, idx);
    addLead(leads, {
      rule: 'script/unpinned-cospend',
      severity: 'BLOCKER',
      file: relPath,
      line: ln,
      snippet: lineTextAt(lines, ln),
      detail: 'Stateless SmartContract reads txPreimage without calling checkPreimage/CheckPreimage',
    });
  }

  // 6. Locktime extraction without nSequence finality guard (< 0xffffffff)
  const ltMatch = content.match(/\b(?:runar\.)?(?:extractLocktime|ExtractLocktime)\s*\(/);
  if (ltMatch && !/\b(?:extractSequence|ExtractSequence)\s*\(/.test(content)) {
    const ln = lineNumberAt(content, ltMatch.index);
    addLead(leads, {
      rule: 'script/locktime-sequence-bypass',
      severity: 'BLOCKER',
      file: relPath,
      line: ln,
      snippet: lineTextAt(lines, ln),
      detail: 'Covenant reads extractLocktime/ExtractLocktime without enforcing extractSequence < 0xffffffff (SEQUENCE_FINAL disables consensus nLockTime)',
    });
  }

  const inventory =
    primitives.size > 0
      ? {
          kind: 'runar',
          file: relPath,
          primitives: [...primitives].sort(),
        }
      : null;

  return { inventory, leads };
}

function walkFiles(dir, rootDir, includeTests, out) {
  let entries;
  try {
    entries = fs.readdirSync(dir, { withFileTypes: true });
  } catch {
    return;
  }
  entries.sort((a, b) => a.name.localeCompare(b.name));
  for (const ent of entries) {
    if (ent.isSymbolicLink()) continue;
    const full = path.join(dir, ent.name);
    if (ent.isDirectory()) {
      if (SKIP_DIRS.has(ent.name) || ent.name.startsWith('.nektar')) continue;
      walkFiles(full, rootDir, includeTests, out);
    } else if (ent.isFile()) {
      if (ent.name.endsWith('.runar.go') || ent.name.endsWith('.runar.ts')) {
        out.runar.push(full);
      } else if (ent.name.endsWith('.go')) {
        if (!includeTests && ent.name.endsWith('_test.go')) continue;
        out.go.push(full);
      }
    }
  }
}

export function probeTools() {
  const names = ['go', 'govulncheck', 'gosec', 'staticcheck', 'golangci-lint', 'semgrep', 'gitleaks', 'node'];
  const result = {};
  const finder = process.platform === 'win32' ? 'where' : 'which';
  for (const name of names) {
    try {
      const out = execFileSync(finder, [name], { stdio: ['ignore', 'pipe', 'ignore'], timeout: 2000 })
        .toString()
        .trim()
        .split(/\r?\n/)[0];
      result[name] = { available: true, path: out };
    } catch {
      result[name] = { available: false, path: null };
    }
  }
  return result;
}

export function scanDirectory(targetDir, opts = {}) {
  const rootDir = path.resolve(targetDir || '.');
  const includeTests = Boolean(opts.includeTests);
  const doProbe = opts.probe !== false;

  const files = { go: [], runar: [] };
  walkFiles(rootDir, rootDir, includeTests, files);

  const inventory = [];
  const leads = [];

  for (const f of files.go) {
    const rel = normalizeRel(rootDir, f);
    const content = fs.readFileSync(f, 'utf8');
    const res = scanGoSource(rel, content);
    if (res.inventory) inventory.push(res.inventory);
    leads.push(...res.leads);
  }

  for (const f of files.runar) {
    const rel = normalizeRel(rootDir, f);
    const content = fs.readFileSync(f, 'utf8');
    const res = scanRunarSource(rel, content);
    if (res.inventory) inventory.push(res.inventory);
    leads.push(...res.leads);
  }

  const sevRank = { BLOCKER: 0, HIGH: 1, MED: 2, LOW: 3 };
  leads.sort((a, b) => (sevRank[a.severity] ?? 9) - (sevRank[b.severity] ?? 9) || a.where.localeCompare(b.where));

  return {
    schemaVersion: 1,
    target: rootDir,
    scannedFiles: {
      go: files.go.length,
      runar: files.runar.length,
    },
    inventory,
    leadCount: leads.length,
    leads,
    availableTools: doProbe ? probeTools() : undefined,
  };
}

const isMain = process.argv[1] && path.resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  const args = process.argv.slice(2);
  let targetDir = '.';
  let includeTests = false;
  let probe = true;

  for (const arg of args) {
    if (arg === '--include-tests') includeTests = true;
    else if (arg === '--no-probe') probe = false;
    else if (!arg.startsWith('--')) targetDir = arg;
  }

  const report = scanDirectory(targetDir, { includeTests, probe });
  process.stdout.write(JSON.stringify(report, null, 2) + '\n');
}
