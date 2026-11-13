// Signing seam — every "key.sign(digest)" and "P2PKH().unlock(key)" site in
// the codebase routes through this abstraction so the same tx-building code
// runs against either a local PrivateKey (legacy custody) or a BRC-100
// wallet (WalletInterface subset). ECDSA over a given digest is identical
// regardless of who holds the key material, so on-chain scripts, covenant
// unlocking formats and receipt signatures are unchanged — only custody
// differs.
//
// Contract notes:
// - DigestSigner.sign(digest) mimics PrivateKey.sign: ECDSA over
//   sha256(digest) — sdk 4.x PrivateKey.sign HASHES its input, and callers
//   pass single-hashed material (sighash preimages, attestation digests) so
//   the pair lands on the consensus double-hash. Wallet impls therefore
//   hand hashToDirectlySign sha256(digest): createSignature signs that value
//   raw, matching PrivateKey byte-for-byte.
// - unlockP2PKH produces the same unlocking script the SDK's
//   P2PKH().unlock(privateKey,...) template produces: <sig+sighash> <pubkey>,
//   and self-verifies the pair — a wallet substrate whose createSignature
//   derives a key that does not match getPublicKey throws instead of
//   emitting a script that can never evaluate on-chain.

import {
  Hash,
  PrivateKey,
  PublicKey,
  Script,
  Signature,
  Transaction,
  TransactionSignature,
  UnlockingScript,
  Utils,
  type LockingScript,
  type WalletInterface
} from '@bsv/sdk'

/** Anything that can sign a digest and expose its public key. */
export interface DigestSigner {
  /** ECDSA over sha256(digest) — same contract as PrivateKey.sign. */
  sign(digest: number[] | Uint8Array): Promise<Signature>
  /** compressed public key (hex). */
  pubKeyHex(): Promise<string>
  /** deterministic SECRET bytes keyed to this signer — replaces
   *  `sha256(pre + key.toWif() + post)` derivations. Key impls hash the raw
   *  key inline (byte-identical to the old formula); wallet impls use
   *  createHmac over `pre + post` with the wallet-held key — still secret
   *  and stable across restarts, but not byte-identical to key custody. */
  deriveSecret?(pre: string, post?: string): Promise<Uint8Array>
}

export type SignerLike = PrivateKey | DigestSigner

/** Narrow wallet surface the seam needs — satisfied by Wallet, WalletClient,
 *  ProtoWallet and any BRC-100 substrate client. */
export type SignerWallet = Pick<WalletInterface, 'createSignature' | 'getPublicKey' | 'createHmac'>

export function isDigestSigner(x: unknown): x is DigestSigner {
  return (
    typeof x === 'object' &&
    x !== null &&
    typeof (x as DigestSigner).sign === 'function' &&
    typeof (x as DigestSigner).pubKeyHex === 'function' &&
    !(x instanceof PrivateKey)
  )
}

export function keySigner(key: PrivateKey): DigestSigner {
  return {
    sign: async (d) => key.sign(Array.from(d)),
    pubKeyHex: async () => key.toPublicKey().toString(),
    deriveSecret: async (pre, post) => new Uint8Array(Hash.sha256(Utils.toArray(pre + key.toWif() + (post ?? ''), 'utf8')))
  }
}

/** Wallet-backed signer. `identityKey: true` signs with the wallet's root
 *  identity key (toolbox: the fund key; WalletClient: the external wallet's
 *  root). Passing WalletEncryptionArgs instead derives per protocolID/keyID. */
export function walletSigner(
  wallet: SignerWallet,
  keyArgs: { identityKey: true } | { protocolID: [number, string]; keyID: string; counterparty?: string; forSelf?: boolean } = { identityKey: true }
): DigestSigner {
  let cachedPub: string | undefined
  // One explicit derivation tuple for BOTH calls — conformant substrates
  // (ProtoWallet and every wallet built on it) ignore `identityKey` inside
  // createSignature and derive by protocolID/keyID/counterparty, with
  // counterparty defaulting to 'anyone' there but 'self' in getPublicKey:
  // passing the flag produced a sig under a key nothing could name. The
  // identity surface is therefore the self-derived 'nektar signer/identity'
  // keyID — a deterministic pub/sig pair under any BRC-100 wallet. Held-key
  // custody (toolbox) never reaches this path: custodySigner intercepts to
  // keySigner(root), which yields the literal identity pub.
  const args: { protocolID: [number, string]; keyID: string; counterparty: string; forSelf?: boolean } =
    'identityKey' in keyArgs
      ? { protocolID: [0, 'nektar signer'], keyID: 'identity', counterparty: 'self' }
      : { ...keyArgs, counterparty: keyArgs.counterparty ?? 'self' }
  return {
    sign: async (d) => {
      // hashToDirectlySign is signed raw (no internal hash), so we hand the
      // wallet sha256(d) — PrivateKey.sign semantics: ECDSA over
      // sha256(digest), the construction every caller verifies against.
      const r = await wallet.createSignature({ ...args, hashToDirectlySign: Array.from(Hash.sha256(Array.from(d))) } as never)
      return Signature.fromDER(Array.from(r.signature))
    },
    pubKeyHex: async () => {
      if (!cachedPub) cachedPub = (await wallet.getPublicKey(args as never)).publicKey
      return cachedPub
    },
    deriveSecret: async (pre, post) => {
      const r = await wallet.createHmac({ ...args, data: Array.from(Utils.toArray(pre + (post ?? ''), 'utf8')) } as never)
      return new Uint8Array(r.hmac)
    }
  }
}

/** Derivation the app uses for funding spends under external wallet custody
 *  (NEKTAR_WALLET=client): a stable self-derived keyID so the reserve's pkh
 *  and the signing pair come from one BRC-42 derivation. Deterministic
 *  across restarts for the same wallet. */
export const FUND_SIGN_ARGS: { protocolID: [number, string]; keyID: string; counterparty: string } = {
  protocolID: [2, 'nektar'],
  keyID: 'nektar-fund',
  counterparty: 'self'
}

/** Best ECDSA-capable signer a wallet surface can offer for a given
 *  derivation. If the wallet holds a keyDeriver (toolbox custody — the app
 *  literally holds the root key) the signer is a keySigner over that root
 *  key: byte-exact legacy semantics, pub = identity. External custody
 *  (WalletClient, no key material in-process) falls back to a walletSigner
 *  over keyArgs — a consistent derived pub/sig pair on spec-conformant
 *  substrates; unlockP2PKH/runarSigner self-verify catches substrates that
 *  cannot honor pub↔sig parity (e.g. sdk-4 ProtoWallet). */
/** the wallet's held root key, re-keyed into THIS sdk build — toolbox's
 *  keyDeriver carries the CJS copy's PrivateKey, which fails instanceof on
 *  real key material (WIF round-trip keeps every downstream object one
 *  class set). undefined under genuine external custody (WalletClient). */
export function heldRootKey(wallet: SignerWallet): PrivateKey | undefined {
  const root = (wallet as { keyDeriver?: { rootKey?: unknown } }).keyDeriver?.rootKey
  if (root instanceof PrivateKey) return root
  if (typeof (root as PrivateKey | undefined)?.toWif === 'function') {
    try {
      return PrivateKey.fromWif((root as PrivateKey).toWif())
    } catch {
      return undefined
    }
  }
  return undefined
}

export function custodySigner(
  wallet: SignerWallet,
  keyArgs: { identityKey: true } | { protocolID: [number, string]; keyID: string; counterparty?: string; forSelf?: boolean }
): DigestSigner {
  // Held-key custody (toolbox): the app literally holds the root key, so every
  // signer is a local keySigner — exact legacy semantics AND correct digest
  // semantics. The remote path can't be used anyway: toolbox's createSignature
  // doesn't implement hashToDirectlySign (it would sign sha256(digest), which
  // no on-chain verifier accepts). Derived args take the same BRC-42
  // derivation the wallet's own signing uses, so the pub is byte-identical
  // to what getPublicKey(keyArgs) returns.
  const root = heldRootKey(wallet)
  if (root !== undefined) {
    if ('identityKey' in keyArgs) return keySigner(root)
    const derive = (wallet as { keyDeriver?: { derivePrivateKey?: (p: [number, string], k: string, c: unknown) => { toWif(): string } } }).keyDeriver
      ?.derivePrivateKey
    if (typeof derive === 'function') {
      try {
        return keySigner(PrivateKey.fromWif(derive(keyArgs.protocolID, keyArgs.keyID, keyArgs.counterparty ?? 'self').toWif()))
      } catch {
        /* derivation unsupported — fall through to the remote signer */
      }
    }
  }
  return walletSigner(wallet, keyArgs)
}

/** Accept either custody form — legacy call sites pass PrivateKey, wallet-mode
 *  call sites pass a DigestSigner. */
export function toSigner(x: SignerLike): DigestSigner {
  return isDigestSigner(x) ? x : keySigner(x)
}

/** Resolve a pubkey hex from either form — replaces `key.toPublicKey().toString()`. */
export async function signerPubHex(x: SignerLike): Promise<string> {
  return isDigestSigner(x) ? x.pubKeyHex() : x.toPublicKey().toString()
}

/** Resolve a pubkey hash (20B) from either form — replaces
 *  `key.toPublicKey().toHash()` / toAddress derivations. */
export async function signerPubKeyHash(x: SignerLike): Promise<number[]> {
  const pk = PublicKey.fromString(await signerPubHex(x))
  const h = pk.toHash()
  return typeof h === 'string' ? Array.from(Utils.toArray(h, 'hex')) : Array.from(h)
}

/** Resolve a base58 address from either form — replaces
 *  `key.toPublicKey().toAddress(prefix)`. */
export async function signerAddress(x: SignerLike, prefix: number[] | string = [0x00]): Promise<string> {
  const pk = PublicKey.fromString(await signerPubHex(x))
  return pk.toAddress(prefix)
}

const SIGHASH_FORKID = 0x40
const scopeFor = (signOutputs: 'all' | 'none' | 'single', anyoneCanPay: boolean): number => {
  const base = signOutputs === 'all' ? 0x01 : signOutputs === 'none' ? 0x02 : 0x03
  return base | SIGHASH_FORKID | (anyoneCanPay ? 0x80 : 0)
}

/** Unlocking-script template equivalent to the SDK's
 *  `new P2PKH().unlock(privateKey, ...)` — byte-identical output — but signing
 *  through a DigestSigner so a wallet can produce it without the app holding
 *  key material. Requires each input's sourceTransaction (all our call sites
 *  already attach it). */
export function unlockP2PKH(
  signer: SignerLike,
  opts: {
    signOutputs?: 'all' | 'none' | 'single'
    anyoneCanPay?: boolean
    lockingScript?: LockingScript
  } = {}
): { sign: (tx: Transaction, inputIndex: number) => Promise<UnlockingScript>; estimateLength: (tx: Transaction, inputIndex: number) => Promise<number> } {
  const s = toSigner(signer)
  const signOutputs = opts.signOutputs ?? 'all'
  const anyoneCanPay = opts.anyoneCanPay ?? false
  const signatureScope = scopeFor(signOutputs, anyoneCanPay)
  return {
    sign: async (tx, inputIndex) => {
      const input = tx.inputs[inputIndex]
      const lockingScript = opts.lockingScript ?? input.sourceTransaction!.outputs[input.sourceOutputIndex].lockingScript
      const preimage = tx.preimage(inputIndex, signatureScope, lockingScript)
      const preimageHash = Hash.sha256(preimage)
      const raw = await s.sign(preimageHash)
      const pub = PublicKey.fromString(await s.pubKeyHex())
      if (!pub.verify(Array.from(preimageHash), raw))
        throw new Error(
          'unlockP2PKH: signer produced a signature that does not verify under its public key — wallet-custody signers must return a matching pub/sig pair (BRC-100 derived keys on a conformant substrate, or a held key)'
        )
      const txsig = new TransactionSignature(raw.r, raw.s, signatureScope)
      const sigForScript = txsig.toChecksigFormat()
      const enc = pub.encode(true)
      const pubkeyForScript = typeof enc === 'string' ? Utils.toArray(enc, 'hex') : enc
      return new UnlockingScript([
        { op: sigForScript.length, data: sigForScript },
        { op: pubkeyForScript.length, data: pubkeyForScript }
      ])
    },
    estimateLength: async () => 108
  }
}

/** Structural adapter to runar-sdk's `Signer` interface (the same shape
 *  `LocalSigner`/`ExternalSigner` implement) backed by a DigestSigner, so
 *  RunarContract funding/covenant calls can run on wallet custody. Replicates
 *  LocalSigner.sign exactly: BIP-143 preimage via TransactionSignature.format,
 *  sha256(preimage) into signer.sign, DER + sighash byte. Self-verifies the
 *  pub/sig pair like unlockP2PKH — covenant sigs are the same risk class.
 *  Typed structurally — core does not depend on runar-sdk. */
export function runarSigner(
  signer: SignerLike,
  opts: { addressPrefix?: number[] } = {}
): {
  getPublicKey: () => Promise<string>
  getAddress: () => Promise<string>
  sign: (txHex: string, inputIndex: number, subscript: string, satoshis: number, sigHashType?: number) => Promise<string>
} {
  const s = toSigner(signer)
  const prefix = opts.addressPrefix ?? [0x00]
  return {
    getPublicKey: () => s.pubKeyHex(),
    getAddress: () => signerAddress(s, prefix),
    sign: async (txHex, inputIndex, subscript, satoshis, sigHashType = 0x41) => {
      const tx = Transaction.fromHex(txHex)
      const input = tx.inputs[inputIndex]
      const otherInputs = tx.inputs
        .filter((_inp, i) => i !== inputIndex)
        .map((inp) => ({ sourceTXID: inp.sourceTXID, sourceOutputIndex: inp.sourceOutputIndex, sequence: inp.sequence }))
      const outputs = tx.outputs.map((o) => ({ satoshis: o.satoshis, lockingScript: o.lockingScript }))
      const preimage = TransactionSignature.format({
        sourceTXID: input.sourceTXID ?? '',
        sourceOutputIndex: input.sourceOutputIndex,
        sourceSatoshis: satoshis,
        transactionVersion: tx.version,
        otherInputs,
        outputs,
        inputIndex,
        subscript: Script.fromHex(subscript),
        inputSequence: input.sequence ?? 0xffffffff,
        lockTime: tx.lockTime,
        scope: sigHashType
      })
      const sighash = Hash.sha256(preimage)
      const signature = await s.sign(sighash)
      if (!PublicKey.fromString(await s.pubKeyHex()).verify(Array.from(sighash), signature))
        throw new Error(
          'runarSigner: signer produced a signature that does not verify under its public key — wallet-custody signers must return a matching pub/sig pair'
        )
      const der = signature.toDER('hex') as string
      return der + sigHashType.toString(16).padStart(2, '0')
    }
  }
}
