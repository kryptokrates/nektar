import { test } from 'node:test'
import assert from 'node:assert/strict'
import { readdirSync, readFileSync, mkdtempSync, rmSync } from 'node:fs'
import { tmpdir } from 'node:os'
import { join, dirname } from 'node:path'
import { fileURLToPath } from 'node:url'
import { PrivateKey, ProtoWallet, Utils, type WalletInterface } from '@bsv/sdk'
import { openToolboxWallet, type ToolboxHandle } from '../src/toolbox.js'
import type { sdk } from '@bsv/wallet-toolbox'

// BRC-100 conformance — the official ts-stack vectors
// (conformance/vectors/wallet/brc100/, pinned at ts-stack 389d5d8) run
// against OUR wallet assemblies, mirroring the docs' TypeScript runner:
// the nine key/crypto methods go to ProtoWallet — the exact class our
// held-key custody path now uses — and the output/action/certificate
// methods go to a real openToolboxWallet on sqlite storage. The toolbox
// wallet also gets the crypto files as a second target: every failure it
// shows must be a `privileged` vector, the documented gap a wallet
// without a PrivilegedKeyManager has (ProtoWallet derives those from
// root, which is exactly what the vectors expect).

interface Vector {
  id: string
  description: string
  parity_class?: string
  skip_reason?: string
  input: {
    root_key?: string
    args: Record<string, unknown>
    originator?: string
    _scenario?: string
  }
  expected: { error?: boolean; code?: string; message?: string } & Record<string, unknown>
}

const DIR = join(dirname(fileURLToPath(import.meta.url)), '..', '..', 'test', 'vectors', 'brc100')
const KEY1 = '0000000000000000000000000000000000000000000000000000000000000001'

const METHOD: Record<string, keyof WalletInterface> = {
  abortaction: 'abortAction',
  acquirecertificate: 'acquireCertificate',
  createaction: 'createAction',
  createhmac: 'createHmac',
  createsignature: 'createSignature',
  decrypt: 'decrypt',
  discoverbyattributes: 'discoverByAttributes',
  discoverbyidentitykey: 'discoverByIdentityKey',
  encrypt: 'encrypt',
  getheaderforheight: 'getHeaderForHeight',
  getheight: 'getHeight',
  getnetwork: 'getNetwork',
  getpublickey: 'getPublicKey',
  getversion: 'getVersion',
  internalizeaction: 'internalizeAction',
  isauthenticated: 'isAuthenticated',
  listactions: 'listActions',
  listcertificates: 'listCertificates',
  listoutputs: 'listOutputs',
  provecertificate: 'proveCertificate',
  relinquishcertificate: 'relinquishCertificate',
  relinquishoutput: 'relinquishOutput',
  revealcounterpartykeylinkage: 'revealCounterpartyKeyLinkage',
  revealspecifickeylinkage: 'revealSpecificKeyLinkage',
  signaction: 'signAction',
  verifyhmac: 'verifyHmac',
  verifysignature: 'verifySignature',
  waitforauthentication: 'waitForAuthentication'
}

const CRYPTO = new Set(['getpublickey', 'createhmac', 'verifyhmac', 'createsignature', 'verifysignature', 'encrypt', 'decrypt', 'revealcounterpartykeylinkage', 'revealspecifickeylinkage'])
const STATE = new Set(['getnetwork', 'getheight', 'getheaderforheight', 'getversion', 'isauthenticated', 'waitforauthentication'])

/** vector `data` is a utf8 string in the JSON; SDK byte fields want arrays.
 *  encrypt reads `plaintext`, the vectors spell it `data`. */
const fixArgs = (file: string, args: Record<string, unknown>): Record<string, unknown> => {
  const a = { ...args }
  if (file === 'encrypt' && a.data !== undefined) {
    a.plaintext = a.data
    delete a.data
  }
  for (const k of ['data', 'plaintext', 'ciphertext', 'hmac', 'signature']) if (typeof a[k] === 'string') a[k] = Array.from(Utils.toArray(a[k] as string, 'utf8'))
  return a
}

/** state-method vectors expect the dispatcher's fixed service values — a
 *  wallet relays its services verbatim, so the stub answers exactly what
 *  the vector expects and the wallet must pass it through unchanged. */
const stubServices = (over: Record<string, unknown> = {}): sdk.WalletServices =>
  ({
    getHeight: async () => 1,
    getHeaderForHeight: async () => Array.from(Utils.toArray('0'.repeat(160), 'hex')),
    getRawTx: async () => ({ name: 'stub', status: 'error' }),
    getMerklePath: async () => ({ name: 'stub', status: 'error' }),
    postBeef: async (_b: unknown, txids: string[]) => [{ name: 'stub', status: 'success', txidResults: txids.map((txid) => ({ txid, status: 'success' })) }],
    getUtxoStatus: async () => ({ name: 'stub', status: 'success', isUtxo: false, details: [] }),
    getStatusForTxids: async () => ({ name: 'stub', status: 'success', results: [] }),
    getScriptHashHistory: async () => ({ name: 'stub', status: 'success', history: [] }),
    getBsvExchangeRate: async () => 0,
    getFiatExchangeRate: async () => 0,
    nLockTimeIsFinal: async () => true,
    getBeefForTxid: async () => {
      throw new Error('no beef')
    },
    isValidRootForHeight: async () => true,
    getChainTracker: async () => ({ currentHeight: async () => 1, isValidRootForHeight: async () => true }),
    chain: 'test',
    hashOutputScript: (s: string) => s,
    isUtxo: async () => false,
    hashToHeader: async () => {
      throw new Error('no hashToHeader')
    },
    getServicesCallHistory: () => ({}) as sdk.ServicesCallHistory,
    ...over
  }) as unknown as sdk.WalletServices

type Call = (args: Record<string, unknown>, originator?: string) => Promise<Record<string, unknown>>
const call = (w: WalletInterface, method: keyof WalletInterface): Call => {
  const fn = (w as unknown as Record<string, unknown>)[method]
  return async (args, originator) => (await (fn as Call).call(w, args, originator)) as Record<string, unknown>
}

/** compare each expected key — extra result fields don't matter (BRC-100
 *  results are additive). */
const diffExpected = (result: Record<string, unknown>, expected: Record<string, unknown>): string | undefined => {
  for (const [k, v] of Object.entries(expected)) {
    if (k === 'error' || k === 'code' || k === 'message') continue
    const r = result[k]
    if (JSON.stringify(r ?? null) !== JSON.stringify(v ?? null)) return `${k}: got ${JSON.stringify(r ?? null).slice(0, 120)} want ${JSON.stringify(v ?? null).slice(0, 120)}`
  }
  return undefined
}

const runVectors = async (file: string, vectors: Vector[], walletFor: (v: Vector) => Promise<{ w: WalletInterface; invoke?: Call } | undefined>): Promise<string[]> => {
  const method = METHOD[file]
  const fails: string[] = []
  for (const v of vectors) {
    if (v.parity_class === 'intended') continue
    const h = await walletFor(v)
    if (h === undefined) {
      fails.push(`${v.id}: no wallet`)
      continue
    }
    const invoke = h.invoke ?? call(h.w, method)
    const args = fixArgs(file, v.input.args)
    try {
      const result = (await invoke(args, v.input.originator)) as Record<string, unknown>
      if (v.expected.error === true) {
        fails.push(`${v.id}: expected error, got ${JSON.stringify(result).slice(0, 120)}`)
        continue
      }
      if (file === 'getheaderforheight' && typeof v.expected.header === 'string' && Array.isArray(result.header)) result.header = Buffer.from(result.header).toString('hex')
      const d = diffExpected(result, v.expected)
      if (d !== undefined) fails.push(`${v.id}: ${d}`)
    } catch (e) {
      if (v.expected.error !== true) {
        fails.push(`${v.id}: threw ${(e as Error).message.slice(0, 140)}`)
        continue
      }
      // code assertions match the vector's spec code when one is given —
      // the reference impl emits the WERR_ family, so a mismatch is real
      if (v.expected.code !== undefined) {
        const s = `${(e as { code?: string }).code ?? ''} ${(e as Error).name} ${(e as Error).message}`
        if (!s.includes(v.expected.code)) fails.push(`${v.id}: error code — got ${s.slice(0, 120)} want ${v.expected.code}`)
      }
    }
  }
  return fails
}

const load = (file: string): Vector[] => (JSON.parse(readFileSync(join(DIR, `${file}.json`), 'utf8')) as { vectors: Vector[] }).vectors
const files = readdirSync(DIR)
  .filter((f) => f.endsWith('.json'))
  .map((f) => f.replace(/\.json$/, ''))

// ── crypto files → ProtoWallet (our held-key signer) — must be fully
// conformant; this is the exact class custody calls. ────────────────────
for (const file of files.filter((f) => CRYPTO.has(f))) {
  test(`brc100 ${file}: ProtoWallet passes every required vector`, async () => {
    const fails = await runVectors(file, load(file), async (v) => ({
      w: new ProtoWallet(PrivateKey.fromHex(v.input.root_key ?? KEY1)) as unknown as WalletInterface
    }))
    assert.deepEqual(fails.slice(0, 8), [])
    assert.equal(fails.length, 0, `${fails.length} failures`)
  })
}

// ── crypto files → the toolbox Wallet — every failure must be a
// privileged vector (no PrivilegedKeyManager wired — documented gap, and
// the count quantifies our distance from serving the full surface). ─────
const toolboxCryptoFails: string[] = []
test(`brc100 crypto files: toolbox wallet failures are privileged-only`, async () => {
  const handles: ToolboxHandle[] = []
  const dirs: string[] = []
  const wallets = new Map<string, WalletInterface>()
  try {
    for (const file of files.filter((f) => CRYPTO.has(f))) {
      const fails = await runVectors(file, load(file), async (v) => {
        const key = v.input.root_key ?? KEY1
        let w = wallets.get(key)
        if (w === undefined) {
          const dir = mkdtempSync(join(tmpdir(), 'brc100-tb-'))
          dirs.push(dir)
          const tbh = await openToolboxWallet({ fundKey: PrivateKey.fromHex(key), dataDir: dir, chain: 'test', services: stubServices() })
          handles.push(tbh)
          w = tbh.wallet as unknown as WalletInterface
          wallets.set(key, w)
        }
        return { w }
      })
      for (const f of fails) toolboxCryptoFails.push(`${file}: ${f}`)
    }
  } finally {
    for (const h of handles) await h.close()
    for (const d of dirs) rmSync(d, { recursive: true, force: true })
  }
  const nonPrivileged = toolboxCryptoFails.filter((f) => {
    // every legitimately-failing vector has privileged:true in its args
    const [file, id] = f.split(': ')
    const vec = load(file).find((v) => v.id === id.trim())
    return vec?.input.args.privileged !== true
  })
  if (toolboxCryptoFails.length > 0) console.log(`brc100 toolbox crypto: ${toolboxCryptoFails.length} privileged-boundary failures (expected — no PrivilegedKeyManager)`)
  assert.deepEqual(nonPrivileged.slice(0, 8), [])
})

// ── stateful + state files → a real openToolboxWallet. Stateful files
// share one wallet per file (empty-wallet arg coverage); state files get
// a fresh wallet per vector because the services stub answers the
// vector's own fixed values. ────────────────────────────────────────────
for (const file of files.filter((f) => !CRYPTO.has(f))) {
  const vectors = load(file)
  const required = vectors.filter((v) => v.parity_class !== 'intended')
  if (required.length === 0) continue
  test(`brc100 ${file}: openToolboxWallet passes required vectors (${required.length})`, async () => {
    const dirs: string[] = []
    const handles: ToolboxHandle[] = []
    let cached: ToolboxHandle | undefined
    try {
      const fails = await runVectors(file, vectors, async (v) => {
        if (STATE.has(file)) {
          const chain = v.input._scenario === 'mainnet wallet' ? 'main' : 'test'
          const over: Record<string, unknown> = {}
          if (file === 'getheight') over.getHeight = async () => v.expected.height
          if (file === 'getheaderforheight') over.getHeaderForHeight = async () => Array.from(Utils.toArray(v.expected.header as string, 'hex'))
          const dir = mkdtempSync(join(tmpdir(), 'brc100-'))
          dirs.push(dir)
          const tbh = await openToolboxWallet({ fundKey: PrivateKey.fromHex(v.input.root_key ?? KEY1), dataDir: dir, chain, services: stubServices(over) })
          handles.push(tbh)
          // getversion vectors carry the dispatcher's own fixed string —
          // every real wallet answers its own version; the vector's schema
          // note defines conformant shape as [vendor]-[major.minor.patch]
          if (file === 'getversion') {
            const invoke: Call = async (args, originator) => {
              const r = (await tbh.wallet.getVersion(args as never, originator as never)) as unknown as Record<string, unknown>
              return { version: typeof r.version === 'string' && /^[\w.+-]+-\d+\.\d+\.\d+/.test(r.version) ? v.expected.version : r.version }
            }
            return { w: tbh.wallet as unknown as WalletInterface, invoke }
          }
          return { w: tbh.wallet as unknown as WalletInterface }
        }
        if (cached === undefined) {
          const dir = mkdtempSync(join(tmpdir(), 'brc100-'))
          dirs.push(dir)
          cached = await openToolboxWallet({ fundKey: PrivateKey.fromHex(v.input.root_key ?? KEY1), dataDir: dir, chain: 'test', services: stubServices() })
          handles.push(cached)
        }
        return { w: cached.wallet as unknown as WalletInterface }
      })
      assert.deepEqual(fails.slice(0, 8), [])
      assert.equal(fails.length, 0, `${fails.length} failures`)
    } finally {
      for (const h of handles) await h.close()
      for (const d of dirs) rmSync(d, { recursive: true, force: true })
    }
  })
}
