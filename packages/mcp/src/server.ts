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
        data: { type: 'string' },
        level: { type: 'number' },
        source: { type: 'string' }
      },
      required: ['shard', 'data']
    }
  },
  {
    name: 'recall',
    description: 'Recall fragments for a shard; decrypts when the agent seed is held. Returns an answer receipt.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' } },
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
    name: 'proveMembership',
    description: 'Holder side: build a Groth16 membership proof for a committed fragment → L_ZKMem hex.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, nonce: { type: 'string' }, verifierId: { type: 'string' } },
      required: ['shard', 'seq', 'nonce', 'verifierId']
    }
  },
  {
    name: 'verifyMembership',
    description: 'Verifier side: verify an L_ZKMem proof against a live-anchored batch (SPV + Groth16 + nullifier dedup).',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, bytes: { type: 'string' }, nonce: { type: 'string' }, verifierId: { type: 'string' } },
      required: ['shard', 'seq', 'bytes', 'nonce', 'verifierId']
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
      return mem.remember(args.shard, args.data, { source: args.source }, args.level ?? 0)
    case 'recall':
      return mem.recall(args.shard, { seq: args.seq })
    case 'prove':
      return mem.prove(args.shard, args.seq)
    case 'anchor':
      return mem.anchorShard(args.shard)
    case 'proveMembership': {
      const { Buffer } = await import('node:buffer')
      const bytes = await mem.proveMembership(args.shard, args.seq, { nonce: new Uint8Array(Buffer.from(args.nonce, 'hex')), verifierId: args.verifierId }, mem.zkArtifactsDir())
      return { lzkmem: Buffer.from(bytes).toString('hex'), statement: 'membership proof for a batch-anchored fragment' }
    }
    case 'verifyMembership': {
      const { Buffer } = await import('node:buffer')
      const res = await mem.verifyMembership(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')), { nonce: new Uint8Array(Buffer.from(args.nonce, 'hex')), verifierId: args.verifierId })
      return res
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
      return { receipt: { ...(await mem.authorizeAction(args.action, { identity: args.identity, zk: args.zk, resource: args.resource })), action: args.action } }
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
