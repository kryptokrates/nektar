// authorshipops.ts — the authorship/rotation surface extracted from
// NektarMemory: owner rotation (rotateShard), the cached author-chain
// projection (genesis ownerPub + live certs), §19 authorship checks, and the
// __identity__ cert-chain proof export.
import type { Transaction } from '@bsv/sdk'
import { toHex, signerPubHex, type Bytes, type SignerLike, type Provenance } from '@nektar/core'
import * as idbind from '@nektar/identity'
import type { AnchorChain } from '@nektar/anchor'
import type { AnchorLink, ProofBundle, VerifyResult } from '@nektar/verify'
import type { LogRecord } from '@nektar/log'
import type { NektarMemoryOptions } from './types.js'

type AuthorChainInfo = { genesisPub: string; certs: idbind.RotationCert[] } | null

export interface AuthorshipOpsDeps {
  opts: NektarMemoryOptions
  anchors: Map<string, AnchorChain>
  tails: Map<string, AnchorLink[]>
  anchorTxs: Map<string, Transaction>
  txidShard: Map<string, string>
  /** the AnchorChain factory — creates on first access */
  anchorChain: (shard: string) => AnchorChain
  shardPubs: Map<string, string>
  storeAll: (shard: string) => Promise<LogRecord[]>
  storeBySeq: (shard: string, seq: number) => Promise<LogRecord | undefined>
  canEncrypt: () => boolean
  decryptRecord: (rec: LogRecord) => Promise<string | undefined>
  prove: (shard: string, seq: number) => Promise<{ bundle?: ProofBundle; result: VerifyResult | { ok: false; reason: string } }>
  remember: (shard: string, data: string) => Promise<{ seq: number }>
  anchorShard: (shard: string) => Promise<unknown>
  persistState: () => void
}

export class AuthorshipOps {
  /** author-chain projection per shard — genesis ownerPub (decoded off the
   *  genesis anchor's locking script) + live rotation certs */
  private authorChainCache = new Map<string, AuthorChainInfo>()

  constructor(private deps: AuthorshipOpsDeps) {}

  async rotateShard(shard: string, newKey: SignerLike): Promise<{ txid: string; n: number }> {
    const ac = this.deps.anchors.get(shard)
    if (!ac) throw new Error(`no anchor for shard ${shard}`)
    const r = await ac.rotate(newKey)
    this.deps.opts.shardKeys?.set(shard, newKey)
    this.deps.shardPubs.set(shard, await signerPubHex(newKey))
    this.deps.anchorTxs.set(r.tx.id('hex'), r.tx)
    this.deps.txidShard.set(r.tx.id('hex'), shard)
    this.authorChainCache.delete(shard)
    const cert = ac.rotationCerts.at(-1)
    if (cert) {
      await this.deps.remember('__identity__', JSON.stringify({ type: 'rotation-cert', shard, cert }))
      try {
        await this.deps.anchorShard('__identity__')
      } catch {
        /* best-effort: stays pending until the next anchor */
      }
    }
    this.deps.persistState()
    return { txid: r.tx.id('hex'), n: r.head.data.n }
  }

  /** §19 author chain: the shard's genesis ownerPub decoded off the genesis
   *  anchor's decoded ownerPub + live rotation certs. Cached per shard. */
  async authorChain(shard: string): Promise<AuthorChainInfo> {
    if (this.authorChainCache.has(shard)) return this.authorChainCache.get(shard)!
    let out: AuthorChainInfo = null
    const genesis = (this.deps.tails.get(shard) ?? []).find((l) => l.n === 0)
    const gtx = genesis ? this.deps.anchorTxs.get(genesis.txid) : undefined
    if (gtx) {
      const { decodeNektarAnchor } = await import('@nektar/contracts')
      const dec = decodeNektarAnchor(gtx.outputs[0].lockingScript.toHex())
      if (dec) out = { genesisPub: dec.ownerPub, certs: [...this.deps.anchorChain(shard).rotationCerts] }
    }
    this.authorChainCache.set(shard, out)
    return out
  }

  /** §19: was record seq written by the key of its recorded epoch, inside that key's window? */
  async authorship(shard: string, seq: number, genesisPub: string, certs: idbind.RotationCert[]) {
    const r = await this.deps.storeBySeq(shard, seq)
    if (!r?.authorPub || r.authorEpoch === undefined) return { ok: false, reason: 'record carries no verified author epoch' }
    return idbind.verifyAuthorship(r.authorPub, r.authorEpoch, r.fragment.p.time, genesisPub, certs)
  }

  /** the full cert-chain a verifier replays: genesis ownerPub plus every
   *  `rotation-cert` record in `__identity__` with its inclusion bundle. A
   *  verifier replays verifyCertChain over certs it can each prove were
   *  anchored; a withheld cert can only make the chain look older. */
  async rotationChainProof(shard: string): Promise<{
    genesisPub: string | null
    certs: Array<{
      cert: idbind.RotationCert
      seq: number
      data: string
      prov?: Provenance
      E?: Bytes
      leaf: string
      bundle?: ProofBundle
      anchored: boolean
    }>
  } | null> {
    if (!this.deps.anchors.get(shard)) return null
    const ac = await this.authorChain(shard)
    const certs: NonNullable<Awaited<ReturnType<AuthorshipOps['rotationChainProof']>>>['certs'] = []
    for (const rec of await this.deps.storeAll('__identity__')) {
      let data: string
      if (rec.fragment.d.length) data = Buffer.from(rec.fragment.d).toString('utf8')
      else if (rec.fragment.E?.length && this.deps.canEncrypt()) {
        const t = await this.deps.decryptRecord(rec)
        if (t === undefined) continue
        data = t
      } else continue
      let cert: idbind.RotationCert
      try {
        const j = JSON.parse(data)
        if (j.type !== 'rotation-cert' || j.shard !== shard) continue
        cert = j.cert
      } catch {
        continue
      }
      const { bundle, result } = await this.deps.prove('__identity__', rec.seq)
      certs.push({ cert, seq: rec.seq, data, prov: rec.fragment.p, E: rec.fragment.E, leaf: toHex(rec.leaf), bundle, anchored: result.ok === true })
    }
    certs.sort((a, b) => a.cert.index - b.cert.index)
    return { genesisPub: ac?.genesisPub ?? null, certs }
  }
}
