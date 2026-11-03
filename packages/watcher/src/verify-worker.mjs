// nektar-watcher verify worker — runs the RISC Zero WASM verifier in a
// dedicated process so a wasm abort (Rust panic → fail-fast, observed as
// exit 0xC0000409 on Windows) kills THIS process, never the daemon.
// Usage: node verify-worker.mjs <proof-bundle-file>
// stdout: one JSON line, the StarkVerdict from verifyZkm2.
import { readFileSync } from 'node:fs'
import { verifyZkm2 } from '@nektar/zk'

const file = process.argv[2]
if (!file) { console.log(JSON.stringify({ ok: false, reason: 'usage: verify-worker <file>' })); process.exit(2) }
let bytes
try { bytes = new Uint8Array(readFileSync(file)) } catch (e) {
  console.log(JSON.stringify({ ok: false, reason: `read: ${e.message}` })); process.exit(0)
}
// verifyZkm2 may abort the process inside wasm — nothing to catch here;
// the parent classifies any nonzero/missing output as INCONCLUSIVE.
const v = verifyZkm2(bytes)
console.log(JSON.stringify(v.ok ? { ok: true, journal: v.journal } : v))
