// Shared regtest helpers for nektar Phase-0 spikes.
// Reads secrets from nektar/.env (gitignored) and RPC creds from the
// teranode-quickstart .env. Never prints key material.
import fs from 'node:fs'
import path from 'node:path'
import { fileURLToPath } from 'node:url'

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..', '..')
const QUICKSTART = path.join(ROOT, 'infra', 'teranode-quickstart')

function loadEnvFile(p) {
  const out = {}
  if (!fs.existsSync(p)) return out
  for (const line of fs.readFileSync(p, 'utf8').split('\n')) {
    const m = line.trim().match(/^([A-Za-z_][A-Za-z0-9_]*)=(.*)$/)
    if (m) out[m[1]] = m[2].trim()
  }
  return out
}

const local = loadEnvFile(path.join(ROOT, '.env'))
const qs = loadEnvFile(path.join(QUICKSTART, '.env'))

export const WIF = local.NEKTAR_REGTEST_WIF
export const RPC = {
  url: 'http://localhost:9292/',
  user: qs.rpc_user || 'teranode',
  pass: qs.rpc_pass || ''
}
export const PROPAGATION_URL = 'http://localhost:8833/tx'
export const STATE_FILE = path.join(ROOT, 'spikes', '.state.json')

if (!WIF) throw new Error('NEKTAR_REGTEST_WIF missing in nektar/.env')
if (!RPC.pass) throw new Error('rpc_pass missing in teranode-quickstart/.env')

export async function rpc(method, ...params) {
  const res = await fetch(RPC.url, {
    method: 'POST',
    headers: {
      'Content-Type': 'application/json',
      Authorization: 'Basic ' + Buffer.from(`${RPC.user}:${RPC.pass}`).toString('base64')
    },
    body: JSON.stringify({ jsonrpc: '1.0', id: 'nektar', method, params })
  })
  const j = await res.json()
  if (j.error) throw new Error(`rpc ${method}: ${JSON.stringify(j.error)}`)
  return j.result
}

export async function broadcastEF(tx) {
  const res = await fetch(PROPAGATION_URL, {
    method: 'POST',
    headers: { 'Content-Type': 'application/octet-stream' },
    body: Buffer.from(tx.toHexEF ? tx.toHexEF() : tx.toHex(), 'hex')
  })
  const text = await res.text()
  return { status: res.status, body: text }
}

export function loadState() {
  return fs.existsSync(STATE_FILE) ? JSON.parse(fs.readFileSync(STATE_FILE, 'utf8')) : {}
}

export function saveState(s) {
  fs.writeFileSync(STATE_FILE, JSON.stringify(s, null, 2))
}
