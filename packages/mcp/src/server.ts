// Tool definitions (JSON Schema) + their single implementation (callTool),
// shared by the MCP server (mcp-server.ts, official SDK) and the HTTP API.
import { sha256, utf8 } from '@mamem/core'
import type { MamemMemory } from './memory.js'

export const TOOLS = [
  {
    name: 'remember',
    description: 'Append a fragment to a shard (off-chain write; anchored at next batch close).',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        data: { type: 'string', description: 'the fact to store, verbatim' },
        level: { type: 'number', description: 'confidentiality level; omit (0 = public) unless the user asks for restricted access — level > 0 records need a ZK clearance to read' },
        source: { type: 'string', description: 'where the fact comes from (document, URL, person)' },
        modality: {
          type: 'object',
          description: '§15 modal qualifier committed into the provenance hash, e.g. {kind:"temporal",qualifier:"as of Block 105432, Reg X includes Clause Y"}',
          properties: { kind: { type: 'string', enum: ['deontic', 'epistemic', 'temporal'] }, qualifier: { type: 'string' } },
          required: ['kind', 'qualifier']
        }
      },
      required: ['shard', 'data']
    }
  },
  {
    name: 'recall',
    description: 'Recall fragments for a shard; decrypts when the agent seed is held. Returns an answer receipt.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'memory namespace' },
        seq: { type: 'number', description: 'one record (the seq returned by remember); omit to recall the whole shard' },
        identity: { type: 'object', description: 'verified caller (supplied by the client wallet, not the model): {pubkey, binding, challengeNonce, challengeSig} answering a nonce from the challenge tool' },
        zk: { type: 'object', description: 'clearance presentation for level>0 records (client wallet): {commitment, proof, transcript:{queryId,fragmentId,nonce(hex)}, requiredLevel(decimal string)}' }
      },
      required: ['shard']
    }
  },
  {
    name: 'prove',
    description: 'Build and verify a proof bundle for one record; result says what the proof does/does not show.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' } },
      required: ['shard', 'seq']
    }
  },
  {
    name: 'anchor',
    description: 'Force-close the pending batch of a shard and broadcast the anchor tx.',
    inputSchema: { type: 'object', properties: { shard: { type: 'string' } }, required: ['shard'] }
  },
  {
    name: 'zkQuery',
    description: 'Verifier side: issue a single-use ZK membership query (queryId) that a holder must bind into its proof.',
    inputSchema: { type: 'object', properties: { verifierId: { type: 'string', description: 'who verifies (bound into the queryId)' } }, required: ['verifierId'] }
  },
  {
    name: 'proveMembership',
    description: 'Holder side: ZK-STARK (transparent, no trusted setup) that a committed record is in the SHA-256 batch tree anchored on BSV, bound to a verifier-issued queryId → L_ZKMem v2 hex. Takes ~35-50 s on CPU.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, queryId: { type: 'string', description: '32-byte hex from the verifier\'s zkQuery' } },
      required: ['shard', 'seq', 'queryId']
    }
  },
  {
    name: 'verifyMembership',
    description: 'Verifier side: verify an L_ZKMem v2 proof — pinned program, STARK valid, proven root == R of an SPV-verified anchor of this shard on its canonical chain, queryId issued here and unused. Proves membership only: not which record, not content, not truth.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number', description: 'locates the batch anchor (the proof itself does not reveal the record)' }, bytes: { type: 'string' } },
      required: ['shard', 'seq', 'bytes']
    }
  },
  {
    name: 'proveSubset',
    description: 'Holder side (DIS-002 proof sharding): selective proof of existence for a SUBSET of one batch\'s records — per-leaf shard subpaths over the same anchored tree (64-leaf subtrees) + one shared top multiproof → L_ZKDis v2 hex. Reveals leaf positions + hashes, not contents. Gated by MAMEM_PROOF_SHARDING (default on).',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        seqs: { type: 'array', items: { type: 'number' }, description: 'record seqs to disclose — all must belong to the SAME anchored batch' }
      },
      required: ['shard', 'seqs']
    }
  },
  {
    name: 'verifySubset',
    description: 'Verifier side: verify an L_ZKDis v2 subset proof — strict canonical codec, every disclosed leaf folds to its shard root, the shard roots fold through the shared multiproof to R of an SPV-verified anchor of this shard on its canonical chain, anchored size. Proves the listed leaf hashes are in the batch: not their content, not truth.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, bytes: { type: 'string', description: 'L_ZKDis v2 hex from proveSubset' } },
      required: ['shard', 'bytes']
    }
  },
  {
    name: 'proveDerivation',
    description: 'Holder side (Wright §14): ZK-STARK that a committed derivation path to pred(args) exists under the anchored policy DAG — each step applies a rule node Merkle-proven against the policy root. Bound to a verifier-issued queryId → L_ZKDer v3 hex. Reveals neither the path nor any fact.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'supplies the instance fragment(<shard>/<seq>) EDB facts' },
        query: { type: 'object', description: '{pred, args, edb?} — the conclusion to prove entailed; edb facts are private context premises' },
        queryId: { type: 'string', description: '32-byte hex from the verifier\'s zkQuery' }
      },
      required: ['shard', 'query', 'queryId']
    }
  },
  {
    name: 'verifyDerivation',
    description: 'Verifier side: verify an L_ZKDer v3 proof — pinned derivation program, STARK valid, proven policyRoot == this verifier\'s anchored policy DAG root, queryId issued here and unused. Proves a derivation exists: not which conclusion, not the premises, not truth.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string', description: 'shard carrying the anchored dagroot:<root> record (unless anchoredPolicyRoot is configured)' }, bytes: { type: 'string' } },
      required: ['shard', 'bytes']
    }
  },
  {
    name: 'attestMembership',
    description: '§13 cheap on-chain commitment: produce the L_ZKMem v2 STARK membership proof AND anchor sha256(proof) in an OP_FALSE OP_RETURN data tx funded by the fee key (a timestamped commitment, not a shard anchor; off-chain verification stays authoritative).',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, queryId: { type: 'string', description: '32-byte hex from the verifier\'s zkQuery' } },
      required: ['shard', 'seq', 'queryId']
    }
  },
  {
    name: 'verifyAttestation',
    description: 'Verifier side: fetch the attestation tx, extract the OP_RETURN commitment and check it equals sha256(supplied proof bytes). Only attestations broadcast by this instance are resolvable.',
    inputSchema: {
      type: 'object',
      properties: { txid: { type: 'string' }, bytes: { type: 'string', description: 'L_ZKMem v2 hex — the proof whose sha256 should be committed on-chain' } },
      required: ['txid', 'bytes']
    }
  },
  {
    name: 'rotate',
    description: 'Rotate a shard\'s owner key on-chain; emits a matching rotation cert leaf.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, newOwnerWif: { type: 'string' } },
      required: ['shard', 'newOwnerWif']
    }
  },
  {
    name: 'revoke',
    description: 'Mark a pubkey revoked in the revocation SMT and anchor the event.',
    inputSchema: {
      type: 'object',
      properties: { pkHex: { type: 'string' } },
      required: ['pkHex']
    }
  },
  {
    name: 'disclose',
    description: 'Verify an anchored disclosure envelope (signature + verifier + expiry + field path + anchor).',
    inputSchema: {
      type: 'object',
      properties: {
        envelope: { type: 'object' }, ownerPub: { type: 'string' }, verifierPub: { type: 'string' },
        fieldCount: { type: 'number' }, fieldRoot: { type: 'string' }, siblings: { type: 'array' },
        atTime: { type: 'number' }, shard: { type: 'string' }, seq: { type: 'number' }
      },
      required: ['envelope', 'ownerPub', 'verifierPub', 'fieldCount', 'fieldRoot', 'siblings', 'atTime', 'shard', 'seq']
    }
  },
  {
    name: 'challenge',
    description: 'Issue a fresh identity challenge nonce (5 min). Callers prove their key by signing it; only nonces issued here are accepted.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'authorize',
    description: 'Evaluate a Cedar action against the configured policy; principal id comes from a verified identity and level from a verified ZK proof.',
    inputSchema: {
      type: 'object',
      properties: {
        action: { type: 'string' }, identity: { type: 'object' }, zk: { type: 'object' },
        resource: { type: 'object' }
      },
      required: ['action']
    }
  },
  {
    name: 'automaton',
    description: 'Full-mode Merkle Automaton (Rúnar contract): deploy a committed δ, step, halt, or read state. The contract proves δ-conformance of each transition, not correctness of the output content.',
    inputSchema: {
      type: 'object',
      properties: {
        op: { type: 'string', enum: ['deploy', 'step', 'halt', 'state'] },
        id: { type: 'string' }, def: { type: 'object' },
        sym: { type: 'number' }, to: { type: 'number' }, outHash: { type: 'string' }
      },
      required: ['op', 'id']
    }
  }
] as const

const AUTOMATON_NOTE = 'On-chain proof that the transition (q, σ, q′) is in the committed δ and was authorized by the owner; it says nothing about whether the output content is true.'
const automatonView = (h: import('@mamem/automaton').AutomatonHead) =>
  ({ txid: h.tx.id('hex'), q: h.state.q, n: h.state.n, halted: h.state.halted, deltaRoot: h.state.deltaRoot, statement: AUTOMATON_NOTE })

export async function callTool (mem: MamemMemory, name: string, args: any): Promise<any> {
  switch (name) {
    case 'remember':
      return mem.remember(args.shard, args.data, { source: args.source, modality: args.modality }, args.level ?? 0)
    case 'recall': {
      const { decodePresentation } = await import('@mamem/identity')
      const r = await mem.recall(args.shard, { seq: args.seq, identity: args.identity, zk: args.zk === undefined ? undefined : decodePresentation(args.zk) })
      // tell the model how to recover from a wrong seq instead of an empty answer
      if (args.seq !== undefined && r.items.length === 0) {
        const seqs = (await mem.records(args.shard)).map(x => x.seq)
        return { ...r, hint: seqs.length ? `no record with seq ${args.seq} in shard "${args.shard}"; existing seqs: ${seqs.join(', ')} — or omit seq to recall all` : `shard "${args.shard}" is empty` }
      }
      return r
    }
    case 'prove':
      return mem.prove(args.shard, args.seq)
    case 'anchor':
      return mem.anchorShard(args.shard)
    case 'zkQuery':
      return { ...mem.issueZkQuery(args.verifierId), statement: 'single use, expires in 10 minutes; the holder binds queryId into its STARK' }
    case 'proveMembership': {
      const { Buffer } = await import('node:buffer')
      const { bytes, ms } = await mem.proveMembership(args.shard, args.seq, args.queryId)
      return { lzkmem: Buffer.from(bytes).toString('hex'), bytes: bytes.length, proveMs: ms, statement: 'ZK-STARK: some record is in the anchored SHA-256 batch tree; reveals no leaf, index or path' }
    }
    case 'verifyMembership': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyMembership(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'attestMembership': {
      const { Buffer } = await import('node:buffer')
      const { bytes, ms, txid, commitment } = await mem.attestMembership(args.shard, args.seq, args.queryId)
      return { lzkmem: Buffer.from(bytes).toString('hex'), bytes: bytes.length, proveMs: ms, txid, commitment, statement: 'OP_FALSE OP_RETURN sha256(proof) anchored on-chain: a timestamped commitment to the STARK\'s existence; the WASM verifier stays authoritative — this is not an anchor and proves nothing about truth' }
    }
    case 'verifyAttestation': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyAttestation(args.txid, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'proveSubset': {
      const { Buffer } = await import('node:buffer')
      const r = await mem.proveSubset(args.shard, args.seqs)
      if (!r.ok) return { ok: false, reason: r.reason }
      return { lzkdis: Buffer.from(r.bytes).toString('hex'), bytes: r.bytes.length, batch: r.batchIndex, size: r.size, indices: r.indices, statement: 'subset inclusion proof: the listed leaf hashes are in this anchored batch (per-shard subpaths + shared top path); reveals positions and leaf hashes, not record contents' }
    }
    case 'verifySubset': {
      const { Buffer } = await import('node:buffer')
      return mem.verifySubset(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'proveDerivation': {
      const { Buffer } = await import('node:buffer')
      const { bytes, ms } = await mem.proveDerivation(args.shard, args.query, args.queryId)
      return { lzkder: Buffer.from(bytes).toString('hex'), bytes: bytes.length, proveMs: ms, statement: 'ZK-STARK: a derivation path to the conclusion exists under the anchored policy DAG; reveals no rule, premise or fact' }
    }
    case 'verifyDerivation': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyDerivation(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'rotate': {
      const { PrivateKey } = await import('@bsv/sdk')
      const r = await mem.rotateShard(args.shard, PrivateKey.fromWif(args.newOwnerWif))
      return { ...r, receipt: { decision: 'allow', action: 'rotate' } }
    }
    case 'automaton': {
      if (args.op === 'deploy') return automatonView(await mem.automatonDeploy(args.id, args.def))
      const ac = mem.automaton(args.id)
      if (args.op === 'step') return automatonView(await ac.step(args.sym, args.to, args.outHash))
      if (args.op === 'halt') return automatonView(await ac.halt())
      if (args.op === 'state') { const h = ac.getHead(); return h ? automatonView(h) : null }
      throw new Error(`automaton: unknown op ${args.op}`)
    }
    case 'revoke': {
      // key = derive_key(pubkey) (idattr-smt registry convention), value = H("revoked"); persisted
      const root = mem.revoke(String(args.pkHex))
      await mem.remember('__revocation__', `revoke:${args.pkHex}:root=${Buffer.from(root).toString('hex')}`)
      const a = await mem.anchorShard('__revocation__')
      return { revoked: true, smtRoot: Buffer.from(root).toString('hex'), anchorTxid: a?.txid ?? null, receipt: { decision: 'allow', action: 'revoke' } }
    }
    case 'disclose': {
      const idm = await import('@mamem/identity')
      const verifyAnchor = async () => (await mem.prove(args.shard, args.seq)).result.ok === true
      const r = await idm.verifyDisclosureAnchored(
        args.envelope, args.ownerPub, args.verifierPub,
        args.fieldCount, Buffer.from(args.fieldRoot, 'hex'),
        (args.siblings as string[]).map(s => Buffer.from(s, 'hex')),
        args.atTime, verifyAnchor)
      return { verified: r.ok, reason: r.reason ?? null, receipt: { decision: r.ok ? 'allow' : 'deny', action: 'disclose' } }
    }
    case 'authorize':
    {
      const { decodePresentation } = await import('@mamem/identity')
      return { receipt: { ...(await mem.authorizeAction(args.action, { identity: args.identity, zk: args.zk === undefined ? undefined : decodePresentation(args.zk), resource: args.resource })), action: args.action } }
    }
    case 'challenge':
      return { nonce: mem.issueChallenge(), statement: 'sign with answerChallenge(key, nonce) within 5 minutes; nonces from anywhere else are refused' }
    default:
      throw new Error(`unknown tool ${name}`)
  }
}

/** MCP over stdio (official SDK) */
export async function serveStdio (mem: MamemMemory): Promise<void> {
  const { createMcpServer } = await import('./mcp-server.js')
  const { StdioServerTransport } = await import('@modelcontextprotocol/sdk/server/stdio.js')
  await createMcpServer(mem).connect(new StdioServerTransport())
}
