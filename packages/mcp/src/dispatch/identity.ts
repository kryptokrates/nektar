// key rotation, automaton, revocation, authz + claim ops — extracted verbatim from the server.ts dispatchTool switch
import type { ToolHandler } from './shared.js'

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

const CLAIM_OPS = new Set(['deploy', 'status', 'settle', 'deploySale', 'buySale'])

export const handlers: Record<string, ToolHandler> = {
  rotate: async (mem, args) => {
    const keyId = args.newOwnerKeyId === undefined ? undefined : String(args.newOwnerKeyId)
    const wif = args.newOwnerWif === undefined ? undefined : String(args.newOwnerWif)
    if ((keyId === undefined) === (wif === undefined)) throw new Error('rotate needs exactly one of newOwnerKeyId (wallet) or newOwnerWif (legacy)')
    // owner keyIDs are never persisted — the resumed shard signer re-derives
    // 'nektar-shard:<shard>' under wallet custody (or reads keys.json under
    // held-key). Any other keyID can never be re-derived: the next boot's
    // 'head owner is not this shard key' would brick the shard.
    if (keyId !== undefined && keyId !== `nektar-shard:${String(args.shard)}`)
      throw new Error(`rotate: keyID '${keyId}' cannot be re-derived after restart — only 'nektar-shard:<shard>' survives (owner keyIDs are not persisted)`)
    const { PrivateKey } = await import('@bsv/sdk')
    const newKey = keyId !== undefined ? mem.walletKeySigner(keyId) : PrivateKey.fromWif(wif!)
    const r = await mem.rotateShard(args.shard, newKey)
    return { ...r, receipt: { decision: 'allow', action: 'rotate' } }
  },
  automaton: async (mem, args) => {
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
  revoke: async (mem, args) => {
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
  revocationProof: async (mem, args) => {
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
  revocationStatus: async (mem, args) => {
    return await mem.revocationStatus(String(args.pkHex))
  },
  authorize: async (mem, args) => {
    const { decodePresentation } = await import('@nektar/identity')
    return {
      receipt: {
        ...(await mem.authorizeAction(args.action, {
          identity: args.identity,
          zk: args.zk === undefined ? undefined : decodePresentation(args.zk),
          resource: args.resource
        })),
        action: args.action
      }
    }
  },
  challenge: (mem) => {
    return Promise.resolve({
      nonce: mem.issueChallenge(),
      statement: 'sign with answerChallenge(key, nonce) within 5 minutes; nonces from anywhere else are refused'
    })
  },
  claim: async (mem, args, d) => {
    const callTool = d.call
    const op = String(args?.op ?? '')
    if (!CLAIM_OPS.has(op)) throw new Error('claim: op required — deploy|status|settle|deploySale|buySale')
    return await callTool(mem, `claim#${op}`, args, d.ctx)
  },
  'claim#deploy': async (mem, args) => {
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    const { Buffer } = await import('node:buffer')
    return await mem.claims.deploy(Buffer.from(args.lzkmem, 'hex').toString('hex'), {
      bondSats: args.bondSats,
      deadline: args.deadline,
      anchorOutpoint: args.anchorOutpoint
    })
  },
  'claim#status': async (mem, args) => {
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.status(args.txid, args.vout ?? 0)
  },
  'claim#settle': async (mem, args) => {
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.settle(args.txid, args.vout ?? 0, args.payoutSats ?? 4980, args.verdictSats ?? 10)
  },
  'claim#deploySale': async (mem, args) => {
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.deploySale(args.verdictOutpoint, args.priceSats, args.wrapped)
  },
  'claim#buySale': async (mem, args) => {
    if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
    return await mem.claims.buyGated(args.saleTxid, args.verdictOutpoint, args.wrapped, args.priceSats, args.changeSats ?? 900)
  }
}
