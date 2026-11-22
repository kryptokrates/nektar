// ZK membership/subset/derivation proof tools — extracted verbatim from the server.ts dispatchTool switch
import type { ToolHandler } from './shared.js'

export const handlers: Record<string, ToolHandler> = {
  zkQuery: (mem, args) => {
    return Promise.resolve({ ...mem.issueZkQuery(args.verifierId), statement: 'single use, expires in 10 minutes; the holder binds queryId into its STARK' })
  },
  proveMembership: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    const { bytes, ms } = await mem.proveMembership(args.shard, args.seq, args.queryId)
    return {
      lzkmem: Buffer.from(bytes).toString('hex'),
      bytes: bytes.length,
      proveMs: ms,
      statement: 'ZK-STARK: some record is in the anchored SHA-256 batch tree; reveals no leaf, index or path'
    }
  },
  verifyMembership: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    return mem.verifyMembership(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')))
  },
  verifyCombined: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    const { decodePresentation } = await import('@nektar/identity')
    return mem.verifyCombined(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')), decodePresentation(args.zk), {
      scope: args.scope,
      derivationBytes: args.derivationBytes === undefined ? undefined : new Uint8Array(Buffer.from(args.derivationBytes, 'hex')),
      callerPubkey: args.callerPubkey
    })
  },
  attestMembership: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    const { bytes, ms, txid, commitment } = await mem.attestMembership(args.shard, args.seq, args.queryId)
    return {
      lzkmem: Buffer.from(bytes).toString('hex'),
      bytes: bytes.length,
      proveMs: ms,
      txid,
      commitment,
      statement:
        "OP_FALSE OP_RETURN sha256(proof) anchored on-chain: a timestamped commitment to the STARK's existence; the WASM verifier stays authoritative — this is not an anchor and proves nothing about truth"
    }
  },
  verifyAttestation: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    return mem.verifyAttestation(args.txid, new Uint8Array(Buffer.from(args.bytes, 'hex')))
  },
  proveSubset: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    const r = await mem.proveSubset(args.shard, args.seqs)
    if (!r.ok) return { ok: false, reason: r.reason }
    return {
      lzkdis: Buffer.from(r.bytes).toString('hex'),
      bytes: r.bytes.length,
      batch: r.batchIndex,
      size: r.size,
      indices: r.indices,
      statement:
        'subset inclusion proof: the listed leaf hashes are in this anchored batch (per-shard subpaths + shared top path); reveals positions and leaf hashes, not record contents'
    }
  },
  verifySubset: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    return mem.verifySubset(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
  },
  proveDerivation: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    const { bytes, ms } = await mem.proveDerivation(args.shard, args.query, args.queryId)
    return {
      lzkder: Buffer.from(bytes).toString('hex'),
      bytes: bytes.length,
      proveMs: ms,
      statement: 'ZK-STARK: a derivation path to the conclusion exists under the anchored policy DAG; reveals no rule, premise or fact'
    }
  },
  verifyDerivation: async (mem, args) => {
    const { Buffer } = await import('node:buffer')
    return mem.verifyDerivation(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
  },
  modelManifest: async (mem, args) => {
    return mem.modelManifest(args)
  }
}
