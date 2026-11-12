// keystore.ts — encrypted key material at rest (scrypt + AES-256-GCM).
// Format: {v:1, kdf:'scrypt', salt, iv, ct, tag} hex fields.
// On `main` every key file MUST be encrypted — a plaintext keys.json or a
// missing passphrase refuses startup. Regtest only encrypts when a
// passphrase is set.
import { readFileSync, writeFileSync, chmodSync } from 'node:fs'
import { scryptSync, randomBytes, createCipheriv, createDecipheriv } from 'node:crypto'

const V = 1
const KDF = { N: 2 ** 15, r: 8, p: 1 }
const KEYLEN = 32
const MAXMEM = 256 * 1024 * 1024 // scrypt N=2^15,r=8 needs ~32MB+ above Node's default

export class KeystoreError extends Error {}

function enc(pass: string, plain: string): string {
  const salt = randomBytes(16)
  const iv = randomBytes(12)
  const key = scryptSync(pass, salt, KEYLEN, { ...KDF, maxmem: MAXMEM })
  const c = createCipheriv('aes-256-gcm', key, iv)
  const ct = Buffer.concat([c.update(plain, 'utf8'), c.final()])
  return JSON.stringify({
    v: V,
    kdf: 'scrypt',
    salt: salt.toString('hex'),
    iv: iv.toString('hex'),
    ct: ct.toString('hex'),
    tag: c.getAuthTag().toString('hex')
  })
}
function dec(pass: string, blob: string): string {
  const o = JSON.parse(blob)
  if (o.v !== V || o.kdf !== 'scrypt') throw new KeystoreError('unsupported key file format')
  const key = scryptSync(pass, Buffer.from(o.salt, 'hex'), KEYLEN, { ...KDF, maxmem: MAXMEM })
  const d = createDecipheriv('aes-256-gcm', key, Buffer.from(o.iv, 'hex'))
  d.setAuthTag(Buffer.from(o.tag, 'hex'))
  try {
    return Buffer.concat([d.update(Buffer.from(o.ct, 'hex')), d.final()]).toString('utf8')
  } catch {
    throw new KeystoreError('key file decryption failed (wrong passphrase or tampered)')
  }
}

export function passphraseFromEnv(): string | undefined {
  if (process.env.NEKTAR_KEY_PASSPHRASE) return process.env.NEKTAR_KEY_PASSPHRASE
  const f = process.env.NEKTAR_KEY_PASSPHRASE_FILE
  if (f) return readFileSync(f, 'utf8').trim()
  return undefined
}

export function isEncrypted(blob: string): boolean {
  try {
    const o = JSON.parse(blob)
    return o.kdf === 'scrypt' && typeof o.ct === 'string'
  } catch {
    return false
  }
}

export function readKeyFile(file: string, pass: string | undefined, requireEnc: boolean): string {
  const blob = readFileSync(file, 'utf8')
  if (isEncrypted(blob)) {
    if (!pass) throw new KeystoreError(`${file} is encrypted — set NEKTAR_KEY_PASSPHRASE`)
    return dec(pass, blob)
  }
  if (requireEnc) throw new KeystoreError(`${file} is plaintext — mainnet requires encrypted keys at rest`)
  return blob
}

export function writeKeyFile(file: string, plain: string, pass: string | undefined): void {
  writeFileSync(file, pass ? enc(pass, plain) : plain, { mode: 0o600 })
  try {
    chmodSync(file, 0o600)
  } catch {
    /* windows */
  }
}
