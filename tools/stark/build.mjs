// pnpm stark:build — reproducible build of the membership program, prover CLI and
// WASM verifier inside mamem-stark-build (pnpm stark:image). Writes
// packages/zk/stark/{verifier.js,verifier_bg.wasm,image-id.json}.
import { execFileSync } from 'node:child_process'
import { readFileSync, writeFileSync } from 'node:fs'
import { fileURLToPath } from 'node:url'
const here = fileURLToPath(new URL('.', import.meta.url)).replace(/\\/g, '/').replace(/\/$/, '')
const out = fileURLToPath(new URL('../../packages/zk/stark', import.meta.url)).replace(/\\/g, '/')
const dockerEnv = { ...process.env, MSYS_NO_PATHCONV: '1' }
const run = (args) => execFileSync('docker', ['run', '--rm', '-v', `${here}:/work`, '-v', `${out}:/out`,
  '-v', 'mamem-stark-target:/work/target', '-v', 'mamem-stark-wtarget:/work/verifier-wasm/target',
  '-v', 'mamem-stark-cargo:/usr/local/cargo/registry', '-v', 'mamem-stark-wb:/opt/wb', 'mamem-stark-build', 'sh', '-c', args], { env: dockerEnv, encoding: 'utf8', stdio: ['ignore', 'pipe', 'inherit'] })
run('cargo build --release -p mamem-stark >&2')
const imageId = run('/work/target/release/mamem-stark image-id').trim()
run('rustup target add wasm32-unknown-unknown >&2 && cd verifier-wasm && cargo build --release --target wasm32-unknown-unknown >&2 && (test -x /opt/wb/bin/wasm-bindgen || cargo install --root /opt/wb wasm-bindgen-cli --version 0.2.100 >&2) && /opt/wb/bin/wasm-bindgen --target nodejs --out-dir /out --out-name verifier target/wasm32-unknown-unknown/release/mamem_stark_verifier.wasm')
const f = fileURLToPath(new URL('../../packages/zk/stark/image-id.json', import.meta.url))
const meta = JSON.parse(readFileSync(f, 'utf8'))
if (meta.imageId !== imageId) console.log(`image id CHANGED: ${meta.imageId} → ${imageId} (re-pin: every existing proof becomes foreign)`)
writeFileSync(f, JSON.stringify({ ...meta, imageId }, null, 2) + '\n')
console.log(`stark:build OK — image id ${imageId}`)
