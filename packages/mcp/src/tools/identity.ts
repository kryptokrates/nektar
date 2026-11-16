// identity.ts — key rotation, the on-chain automaton, and the SMT-backed
// revocation registry surface.
import type { ToolHandler } from './ctx.js'

const AUTOMATON_NOTE =
  'On-chain proof that the transition (q, σ, q′) is in the committed δ and was authorized by the owner; it says nothing about whether the output content is true.'
const automatonView = (h: import('@nektar/automaton').AutomatonHead) => ({
  txid: h.tx.id('hex'),
  q: h.state.q,
  n: h.state.n,
  halted: h.state.halted,
  deltaRoot: h.state.deltaRoot,
  statement: AUTOMATON_NOTE
})

export const IDENTITY_HANDLERS: Record<string, ToolHandler> = {
  rotate: async (d) => {
    const { mem, args } = d
    const keyId = args.newOwnerKeyId === undefined ? undefined : String(args.newOwnerKeyId)
    const wif = args.newOwnerWif === undefined ? undefined : String(args.newOwnerWif)
    if ((keyId === undefined) === (wif === undefined)) throw new Error('rotate needs exactly one of newOwnerKeyId (wallet) or newOwnerWif (legacy)')
    const { PrivateKey } = await import('@bsv/sdk')
    const newKey = keyId !== undefined ? mem.walletKeySigner(keyId) : PrivateKey.fromWif(wif!)
    const r = await mem.rotateShard(args.shard, newKey)
    return { ...r, receipt: { decision: 'allow', action: 'rotate' } }
  },
  automaton: async (d) => {
    const { mem, args } = d
    if (args.op === 'deploy') return automatonView(await mem.automatonDeploy(args.id, args.def))
    const ac = mem.automaton(args.id)
    if (args.op === 'step') return automatonView(await ac.step(args.sym, args.to, args.outHash))
    if (args.op === 'halt') return automatonView(await ac.halt())
    if (args.op === 'state') {
      const h = ac.getHead()
      return h ? automatonView(h) : null
    }
    throw new Error(`automaton: unknown op ${args.op}`)
  },
  revoke: async (d) => {
    const { mem, args } = d
    // key = derive_key(pubkey) (idattr-smt registry convention), value = H("revoked"); persisted.
    // The SMT membership proof is embedded in the record itself (§19 CRL):
    // the anchored text is then a self-contained conviction — a verifier
    // needs no operator state (idbind.verifyRevocationEntry).
    const idm = await import('@nektar/identity')
    const root = mem.revoke(String(args.pkHex))
    const proof = mem.revocations.prove(idm.revocationKey(String(args.pkHex)))
    await mem.remember('__revocation__', idm.makeRevocationRecord(String(args.pkHex), root, proof))
    const a = await mem.anchorShard('__revocation__')
    return { revoked: true, smtRoot: Buffer.from(root).toString('hex'), anchorTxid: a?.txid ?? null, receipt: { decision: 'allow', action: 'revoke' } }
  },
  revocationProof: async (d) => {
    const { mem, args } = d
    const p = await mem.revocationProof(String(args.pkHex))
    if (!p) return { found: false, note: 'no revoke record for this pubkey on __revocation__' }
    const idm = await import('@nektar/identity')
    const entry = idm.parseRevocationRecord(p.data)
    return {
      found: true,
      seq: p.seq,
      data: p.data,
      leaf: p.leaf,
      root: p.root,
      smtProof: p.smtProof,
      anchored: p.anchored,
      bundle: p.bundle,
      verified: entry ? idm.verifyRevocationEntry(entry) : false,
      verifier: 'verifyRevocationEntry(parseRevocationRecord(data)) binds derive_key(pk)+root; bundle anchors the record text'
    }
  },
  revocationStatus: async (d) => {
    const { mem, args } = d
    return await mem.revocationStatus(String(args.pkHex))
  }
}
