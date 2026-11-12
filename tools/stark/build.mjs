// pnpm stark:build — reproducible build of the membership + derivation programs,
// prover CLI and WASM verifier inside nektar-stark-build (pnpm stark:image). Writes
// packages/zk/stark/{verifier.js,verifier_bg.wasm,image-id.json}.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
const here = fileURLToPath(new URL('.', import.meta.url))
  .replace(/\\/g, '/')
  .replace(/\/$/, '')
const out = fileURLToPath(new URL('../../packages/zk/stark', import.meta.url)).replace(/\\/g, '/')
const dockerEnv = { ...process.env, MSYS_NO_PATHCONV: '1' }
const run = (args) =>
  execFileSync(
    'docker',
    [
      'run',
      '--rm',
      '-v',
      `${here}:/work`,
      '-v',
      `${out}:/out`,
      '-v',
      'nektar-stark-target:/work/target',
      '-v',
      'nektar-stark-wtarget:/work/verifier-wasm/target',
      '-v',
      'nektar-stark-cargo:/usr/local/cargo/registry',
      '-v',
      'nektar-stark-wb:/opt/wb',
      'nektar-stark-build',
      'sh',
      '-c',
      args
    ],
    { env: dockerEnv, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] }
  )
run('cargo build --release -p nektar-stark >&2')
const imageId = run('/work/target/release/nektar-stark image-id').trim()
const datalogImageId = run('/work/target/release/nektar-stark image-id-der').trim()
run(
  'rustup target add wasm32-unknown-unknown >&2 && cd verifier-wasm && cargo build --release --target wasm32-unknown-unknown >&2 && (test -x /opt/wb/bin/wasm-bindgen || cargo install --root /opt/wb wasm-bindgen-cli --version 0.2.100 >&2) && /opt/wb/bin/wasm-bindgen --target nodejs --out-dir /out --out-name verifier target/wasm32-unknown-unknown/release/nektar_stark_verifier.wasm'
)
const f = fileURLToPath(new URL('../../packages/zk/stark/image-id.json', import.meta.url))
const meta = JSON.parse(readFileSync(f, 'utf8'))
if (meta.imageId !== imageId) console.log(`image id CHANGED: ${meta.imageId} → ${imageId} (re-pin: every existing proof becomes foreign)`)
if (meta.datalogImageId !== datalogImageId) console.log(`datalog image id CHANGED: ${meta.datalogImageId} → ${datalogImageId}`)
writeFileSync(f, JSON.stringify({ ...meta, imageId, datalogImageId }, null, 2) + '\n')
console.log(`stark:build OK — membership ${imageId}, derivation ${datalogImageId}`)
