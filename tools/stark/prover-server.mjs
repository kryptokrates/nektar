#!/usr/bin/env node
// tools/stark/prover-server.mjs — HTTP shim over the `nektar-stark` prover
// binary for NEKTAR_STARK_PROVER_URL clients.
//
//   POST /prove      JSON body → `nektar-stark prove`    stdin → stdout JSON
//   POST /prove-der  JSON body → `nektar-stark prove-der` stdin → stdout JSON
//   GET  /health     → 200 {"ok":true,"bin":"<path>"}
//
// Env:
//   PROVER_PORT   listen port (default 8443)
//   PROVER_BIN    prover binary (default ./target/release/nektar-stark)
//   PROVER_DOCKER set to use `docker run -i nektar-stark-build` instead of the binary
//   PROVER_TOKEN  bearer token — REQUIRED to be set for any non-loopback bind;
//                 clients send `Authorization: Bearer <token>`
//   PROVER_HOST   bind address (default 127.0.0.1 — set 0.0.0.0 behind WireGuard/auth)
//
// WARNING: the prover sees private witnesses — restrict the port to trusted
// clients (bind loopback + SSH tunnel/WireGuard, or always set PROVER_TOKEN).
import { spawn } from 'node:child_process'
import { createServer } from 'node:http'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = dirname(fileURLToPath(import.meta.url))
const PORT = Number(process.env.PROVER_PORT ?? 8443)
const HOST = process.env.PROVER_HOST ?? '127.0.0.1'
const TOKEN = process.env.PROVER_TOKEN ?? ''
const TIMEOUT_MS = Number(process.env.PROVER_TIMEOUT_MS ?? 600_000)
const MAX_BODY = 64 * 1024 * 1024 // 64 MiB — derivation payloads can be large

function proverCommand(sub) {
  if (process.env.PROVER_DOCKER) {
    return [
      'docker',
      [
        'run',
        '--rm',
        '-i',
        '-v',
        `${process.env.NEKTAR_STARK_TARGET_VOLUME ?? 'nektar-stark-target'}:/work/target`,
        process.env.NEKTAR_STARK_IMAGE ?? 'nektar-stark-build',
        '/work/target/release/nektar-stark',
        sub
      ]
    ]
  }
  return [resolve(process.env.PROVER_BIN ?? `${ROOT}/target/release/nektar-stark`), [sub]]
}

function runProver(sub, body) {
  return new Promise((res, rej) => {
    const [cmd, args] = proverCommand(sub)
    const child = spawn(cmd, args, { stdio: ['pipe', 'pipe', 'pipe'], env: { ...process.env, MSYS_NO_PATHCONV: '1' } })
    let out = ''
    let err = ''
    const t = setTimeout(() => {
      child.kill('SIGKILL')
      rej(new Error(`prover timed out after ${TIMEOUT_MS} ms`))
    }, TIMEOUT_MS)
    child.stdout.on('data', (d) => {
      out += d
    })
    child.stderr.on('data', (d) => {
      err += d
    })
    child.on('error', (e) => {
      clearTimeout(t)
      rej(new Error(`prover failed to start: ${e.message}`))
    })
    child.on('close', (code) => {
      clearTimeout(t)
      if (code !== 0)
        return rej(
          new Error(
            `prover exited ${code}: ${
              err
                .split('\n')
                .filter((l) => /panicked|assert|error/i.test(l))
                .slice(-2)
                .join(' ')
                .slice(0, 300) || 'no detail'
            }`
          )
        )
      res(out)
    })
    child.stdin.end(body)
  })
}

const server = createServer(async (req, res) => {
  const reply = (code, obj) => {
    res.writeHead(code, { 'Content-Type': 'application/json' })
    res.end(JSON.stringify(obj))
  }
  if (req.method === 'GET' && req.url === '/health') return reply(200, { ok: true, bin: proverCommand('prove')[0] })
  if (req.method !== 'POST' || (req.url !== '/prove' && req.url !== '/prove-der')) return reply(404, { error: 'not found' })
  if (TOKEN) {
    const auth = req.headers.authorization ?? ''
    if (auth !== `Bearer ${TOKEN}`) return reply(401, { error: 'unauthorized' })
  } else if (HOST !== '127.0.0.1' && HOST !== '::1' && HOST !== 'localhost') {
    return reply(500, { error: 'PROVER_TOKEN is required when binding a non-loopback address' })
  }
  let body = ''
  req.on('data', (c) => {
    body += c
    if (body.length > MAX_BODY) {
      req.destroy()
    }
  })
  req.on('end', async () => {
    try {
      const out = await runProver(req.url.slice(1), body)
      res.writeHead(200, { 'Content-Type': 'application/json' })
      res.end(out)
    } catch (e) {
      reply(502, { error: e.message })
    }
  })
})

server.listen(PORT, HOST, () =>
  console.log(`nektar prover-server on ${HOST}:${PORT} (bin: ${proverCommand('prove')[0]}${TOKEN ? ', token auth on' : ', NO TOKEN'})`)
)
