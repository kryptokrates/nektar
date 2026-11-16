// Tool definitions (JSON Schema) + their single implementation (callTool),
// shared by the MCP server (mcp-server.ts, official SDK) and the HTTP API.
// Handler bodies live in src/tools/*.ts — this file owns the schemas, the
// visibility/payment gates and the dispatch orchestration.
import { nektarEnv } from '@nektar/core'
import { PAID_TOOLS, paymentsEnabled, paymentRequired, priceFor, settlePayment } from './pay.js'
import type { NektarMemory } from './memory.js'
import type { ToolCtx, ToolHandler } from './tools/ctx.js'
import { PAY_HANDLERS } from './tools/pay.js'
import { SALE_HANDLERS } from './tools/sale.js'
import { RECORD_HANDLERS } from './tools/records.js'
import { ZK_HANDLERS } from './tools/zk.js'
import { IDENTITY_HANDLERS } from './tools/identity.js'
import { BOND_HANDLERS } from './tools/bonds.js'
import { CLAIM_HANDLERS } from './tools/claims.js'
import { ANSWER_HANDLERS } from './tools/answers.js'
import { PACK_HANDLERS } from './tools/packs.js'
import { AUTOMATION_HANDLERS } from './tools/automation.js'

export const TOOLS = [
  {
    name: 'remember',
    description: 'Append a fragment to a shard (off-chain write; anchored at next batch close).',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        data: { type: 'string', description: 'the fact to store, verbatim' },
        level: {
          type: 'number',
          description: 'confidentiality level; omit (0 = public) unless the user asks for restricted access — level > 0 records need a ZK clearance to read'
        },
        private: {
          type: 'boolean',
          description:
            'declare this shard PRIVATE (monotone — cannot be unset): records on it refuse plain merkle citations (prove/groundedAnswer fragmentRefs) and may only be cited via a ZK membership proof; the shard name is withheld from receipts'
        },
        premiseRefs: {
          type: 'array',
          items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] },
          description: '§AppE: records this fact is derived from — the new level is floored at their max so derived content cannot leak downward'
        },
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
        identity: {
          type: 'object',
          description:
            'verified caller (supplied by the client wallet, not the model): {pubkey, binding, challengeNonce, challengeSig} answering a nonce from the challenge tool'
        },
        zk: {
          type: 'object',
          description:
            'clearance presentation for level>0 records (client wallet): {commitment, proof, transcript:{queryId,fragmentId,nonce(hex)}, requiredLevel(decimal string)}'
        }
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
    description:
      'Force-close the pending batch of a shard and broadcast the anchor tx. With NEKTAR_PAYMENTS=1 this costs sats — call payQuote, then pass payment.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        payment: {
          type: 'object',
          description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote',
          properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } }
        }
      },
      required: ['shard']
    }
  },
  {
    name: 'zkQuery',
    description: 'Verifier side: issue a single-use ZK membership query (queryId) that a holder must bind into its proof.',
    inputSchema: {
      type: 'object',
      properties: { verifierId: { type: 'string', description: 'who verifies (bound into the queryId)' } },
      required: ['verifierId']
    }
  },
  {
    name: 'proveMembership',
    description:
      'Holder side: ZK-STARK (transparent, no trusted setup) that a committed record is in the SHA-256 batch tree anchored on BSV, bound to a verifier-issued queryId → L_ZKMem v2 hex. Takes ~35-50 s on CPU.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, queryId: { type: 'string', description: "32-byte hex from the verifier's zkQuery" } },
      required: ['shard', 'seq', 'queryId']
    }
  },
  {
    name: 'verifyMembership',
    description:
      'Verifier side: verify an L_ZKMem v2 proof — pinned program, STARK valid, proven root == R of an SPV-verified anchor of this shard on its canonical chain, queryId issued here and unused. Proves membership only: not which record, not content, not truth.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        seq: { type: 'number', description: 'locates the batch anchor (the proof itself does not reveal the record)' },
        bytes: { type: 'string' }
      },
      required: ['shard', 'seq', 'bytes']
    }
  },
  {
    name: 'verifyCombined',
    description:
      'Verifier side (Wright §13 Π_j = (π_j, ZKAccess)): verify the DUAL commitment as one object — the L_ZKMem membership STARK, the ZKAccess clearance and (optionally) an L_ZKDer derivation STARK must all carry the SAME verifier-issued queryId and the clearance transcript must carry the issued nonce. Rejects proofs paired with an unrelated clearance.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        seq: { type: 'number', description: 'locates the batch anchor (the proof itself does not reveal the record)' },
        bytes: { type: 'string', description: 'L_ZKMem v2 hex from proveMembership' },
        zk: {
          type: 'object',
          description: 'clearance presentation bound to the same queryId — {commitment, proof, transcript:{queryId,fragmentId,nonce(hex)}, requiredLevel}'
        },
        scope: { type: 'string', description: 'transcript fragmentId to enforce (default shard/seq)' },
        derivationBytes: { type: 'string', description: 'optional L_ZKDer v3 hex bound to the same queryId' },
        callerPubkey: { type: 'string', description: 'optional: require the clearance holder to be this pubkey (else artifact-validity only)' }
      },
      required: ['shard', 'seq', 'bytes', 'zk']
    }
  },
  {
    name: 'proveSubset',
    description:
      "Holder side (DIS-002 proof sharding): selective proof of existence for a SUBSET of one batch's records — per-leaf shard subpaths over the same anchored tree (64-leaf subtrees) + one shared top multiproof → L_ZKDis v2 hex. Reveals leaf positions + hashes, not contents. Gated by NEKTAR_PROOF_SHARDING (default on).",
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
    description:
      'Verifier side: verify an L_ZKDis v2 subset proof — strict canonical codec, every disclosed leaf folds to its shard root, the shard roots fold through the shared multiproof to R of an SPV-verified anchor of this shard on its canonical chain, anchored size. Proves the listed leaf hashes are in the batch: not their content, not truth.',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, bytes: { type: 'string', description: 'L_ZKDis v2 hex from proveSubset' } },
      required: ['shard', 'bytes']
    }
  },
  {
    name: 'proveDerivation',
    description:
      'Holder side (Wright §14): ZK-STARK that a committed derivation path to pred(args) exists under the anchored policy DAG — each step applies a rule node Merkle-proven against the policy root. Bound to a verifier-issued queryId → L_ZKDer v3 hex. Reveals neither the path nor any fact.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'supplies the instance fragment(<shard>/<seq>) EDB facts' },
        query: { type: 'object', description: '{pred, args, edb?} — the conclusion to prove entailed; edb facts are private context premises' },
        queryId: { type: 'string', description: "32-byte hex from the verifier's zkQuery" }
      },
      required: ['shard', 'query', 'queryId']
    }
  },
  {
    name: 'verifyDerivation',
    description:
      "Verifier side: verify an L_ZKDer v3 proof — pinned derivation program, STARK valid, proven policyRoot == this verifier's anchored policy DAG root, queryId issued here and unused. Proves a derivation exists: not which conclusion, not the premises, not truth.",
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string', description: 'shard carrying the anchored dagroot:<root> record (unless anchoredPolicyRoot is configured)' },
        bytes: { type: 'string' }
      },
      required: ['shard', 'bytes']
    }
  },
  {
    name: 'attestMembership',
    description:
      '§13 cheap on-chain commitment: produce the L_ZKMem v2 STARK membership proof AND anchor sha256(proof) in an OP_FALSE OP_RETURN data tx funded by the fee key (a timestamped commitment, not a shard anchor; off-chain verification stays authoritative).',
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, seq: { type: 'number' }, queryId: { type: 'string', description: "32-byte hex from the verifier's zkQuery" } },
      required: ['shard', 'seq', 'queryId']
    }
  },
  {
    name: 'verifyAttestation',
    description:
      'Verifier side: fetch the attestation tx, extract the OP_RETURN commitment and check it equals sha256(supplied proof bytes). Only attestations broadcast by this instance are resolvable.',
    inputSchema: {
      type: 'object',
      properties: { txid: { type: 'string' }, bytes: { type: 'string', description: 'L_ZKMem v2 hex — the proof whose sha256 should be committed on-chain' } },
      required: ['txid', 'bytes']
    }
  },
  {
    name: 'rotate',
    description:
      "Rotate a shard's owner key on-chain; emits a matching rotation cert leaf. New owner: newOwnerWif (legacy key custody) or newOwnerKeyId (wallet-derived keyID, e.g. nektar-shard:payments:2 — wallet custody, no key material crosses the API).",
    inputSchema: {
      type: 'object',
      properties: { shard: { type: 'string' }, newOwnerWif: { type: 'string' }, newOwnerKeyId: { type: 'string' } },
      required: ['shard']
    }
  },
  {
    name: 'revoke',
    description:
      'Mark a pubkey revoked in the revocation SMT and anchor the event. The anchored record embeds the SMT membership proof (§19 CRL) — third-party verifiable without operator state.',
    inputSchema: {
      type: 'object',
      properties: { pkHex: { type: 'string' } },
      required: ['pkHex']
    }
  },
  {
    name: 'revocationProof',
    description:
      'Third-party §19 CRL evidence: the anchored __revocation__ record for a pubkey with its embedded SMT membership proof + SPV inclusion bundle. verified=true means the record alone convicts.',
    inputSchema: {
      type: 'object',
      properties: { pkHex: { type: 'string' } },
      required: ['pkHex']
    }
  },
  {
    name: 'revocationStatus',
    description:
      'Is a pubkey revoked? Returns the latest anchored SMT root plus a fresh proof — membership (revoked) or non-membership — verifiable via verifyRevocationStatus without trusting this operator.',
    inputSchema: {
      type: 'object',
      properties: { pkHex: { type: 'string' } },
      required: ['pkHex']
    }
  },
  {
    name: 'bondDeploy',
    description:
      'Deploy the §12 checkpoint-bond covenant: lock sats so every anchor commits its checkpoint digest to an unforkable on-chain chain. A missed commit inside the SLA window is publicly provable.',
    inputSchema: {
      type: 'object',
      properties: { sats: { type: 'number' }, releaseHeight: { type: 'number' } }
    }
  },
  {
    name: 'bondStatus',
    description: 'Checkpoint-bond state: commits so far (n), last committed checkpoint digest, head txid, locked sats, release height, seize deadline.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'bondSeize',
    description:
      'Permissionless §12 slashing: claim a checkpoint bond whose commit deadline passed. Any observer may call this — the payout goes to payToScript (default: operator).',
    inputSchema: {
      type: 'object',
      properties: { payToScript: { type: 'string' } }
    }
  },
  {
    name: 'bondCommit',
    description: 'Commit a checkpoint root into the bond — the covenant continuation keeps the bond alive and pushes the deadline forward by epochLen.',
    inputSchema: {
      type: 'object',
      properties: { root: { type: 'string', description: '32-byte checkpoint root, hex' } },
      required: ['root']
    }
  },
  {
    name: 'bondClose',
    description: 'Close the bond after its release height — the operator reclaims the funds (whole bond minus a bounded fee).',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'disclose',
    description: 'Verify an anchored disclosure envelope (signature + verifier + expiry + field path + anchor).',
    inputSchema: {
      type: 'object',
      properties: {
        envelope: { type: 'object' },
        ownerPub: { type: 'string' },
        verifierPub: { type: 'string' },
        fieldCount: { type: 'number' },
        fieldRoot: { type: 'string' },
        siblings: { type: 'array' },
        atTime: { type: 'number' },
        shard: { type: 'string' },
        seq: { type: 'number' }
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
        action: { type: 'string' },
        identity: { type: 'object' },
        zk: { type: 'object' },
        resource: { type: 'object' }
      },
      required: ['action']
    }
  },
  {
    name: 'automaton',
    description:
      'Full-mode Merkle Automaton (Rúnar contract): deploy a committed δ, step, halt, or read state. The contract proves δ-conformance of each transition, not correctness of the output content.',
    inputSchema: {
      type: 'object',
      properties: {
        op: { type: 'string', enum: ['deploy', 'step', 'halt', 'state'] },
        id: { type: 'string' },
        def: { type: 'object' },
        sym: { type: 'number' },
        to: { type: 'number' },
        outHash: { type: 'string' }
      },
      required: ['op', 'id']
    }
  },
  {
    name: 'sale',
    description:
      'V3 DisclosureSale (GOAL-v7, BIP-199 escrow) — paid disclosure as one state machine. op=offer {value,price,refundAfter,maxFee,buyerPub?,expiresAt?}: seller wraps a payload (BRC-42 to buyerPub, or public) → {cfg,wrapped,sellerPub,offerSig}. op=offerMemory {shard,seq,price,refundAfter,maxFee,buyerPub?,expiresAt?}: sell one memory record — encrypted goes as its fragment key + ciphertext, plaintext as the canonical fragment; returns commitment {leaf,anchorTxid,hdp,authorPub,proof} so the buyer verifies inclusion BEFORE funding. op=escrow {cfg,amount,offer?}: buyer funds; pass offer={sellerPub,offerSig,expiresAt,commitment} for pre-funding sig + leaf-path + expiry checks. op=complete {cfg,utxo,wrapped,changeSats}: the buy tx — reveals wrapped on-chain, pays the seller price, returns change. op=refund {cfg,utxo,lockTime,changeSats}: depositor reclaim after cfg.refundAfter (locktime-gated). op=claim {txHex,hWrapped,sellerPub?,offerMode?,commitment?}: buyer extracts + verifies the payload (leaf-commitment + provenance check when commitment given). Requires NEKTAR_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        op: { type: 'string', enum: ['offer', 'offerMemory', 'escrow', 'complete', 'refund', 'claim'] },
        value: { type: 'string', description: 'offer: hex payload (envelope wrapped bytes or a {field,salt,path} JSON)' },
        shard: { type: 'string' },
        seq: { type: 'number' },
        price: { type: 'number' },
        refundAfter: { type: 'number' },
        maxFee: { type: 'number' },
        buyerPub: { type: 'string', description: 'recipient pubkey — omit for public disclosure' },
        expiresAt: { type: 'number', description: 'block height at which the signed offer lapses' },
        cfg: { type: 'object', description: 'escrow cfg (offer fields + depositorPkh)' },
        amount: { type: 'number', description: 'escrow deposit — price + maxFee + change margin' },
        offer: { type: 'object', description: 'escrow: {sellerPub, offerSig, expiresAt, commitment} — verified pre-funding' },
        utxo: { type: 'object', description: '{txid, vout, satoshis, script} of the funded escrow' },
        wrapped: { type: 'string', description: 'committed wrapped hex (sha256(wrapped) == cfg.hWrapped)' },
        changeSats: { type: 'number' },
        lockTime: { type: 'number' },
        txHex: { type: 'string' },
        hWrapped: { type: 'string' },
        sellerPub: { type: 'string', description: 'claim: needed to unwrap private (BRC-42) payloads' },
        offerMode: { type: 'string', description: 'claim: "key" for encrypted-record key sales' },
        commitment: { type: 'object', description: 'claim: {leaf,hdp,authorPub,anchorTxid} — verifies leaf inclusion + provenance σ' }
      },
      required: ['op']
    }
  },
  {
    name: 'claim',
    description:
      "Optimistic-claim rail (§4.5) + pay-iff-proven gated sale — regtest only. op=deploy {lzkmem,bondSats?,deadline?,anchorOutpoint?}: deploy a NektarZkClaim covenant bound to an L_ZKMem bundle — journal root/size/queryId committed behind the prover's bond; the watcher re-verifies the RISC Zero receipt off-chain and auto-challenges fraud. op=status {txid,vout?}: claim state open|challenged|revealed|bisecting, deadline, journalRoot, bond. op=settle {txid,vout?,payoutSats?,verdictSats?}: settle an open claim after deadline → ClaimVerdict UTXO + bond payout (unanchored only — anchored claims settle via the anchor co-spend). op=deploySale {verdictOutpoint,priceSats,wrapped}: DisclosureSale bound to a settled verdict — the price pays only in a tx that also consumes it (atomic pay-iff-settled). op=buySale {saleTxid,verdictOutpoint,wrapped,priceSats,changeSats?}: spends [sale,verdict] — consensus refuses without the verdict input.",
    inputSchema: {
      type: 'object',
      properties: {
        op: { type: 'string', enum: ['deploy', 'status', 'settle', 'deploySale', 'buySale'] },
        lzkmem: { type: 'string', description: 'deploy: L_ZKMem v2 hex from proveMembership' },
        bondSats: { type: 'number' },
        deadline: { type: 'number', description: 'deploy: locktime when the claim becomes settleable; 0 = immediately' },
        anchorOutpoint: { type: 'string', description: 'deploy: 36-byte anchor outpoint hex to bind journalRoot == anchor.R; omit for unanchored' },
        txid: { type: 'string' },
        vout: { type: 'number' },
        payoutSats: { type: 'number' },
        verdictSats: { type: 'number' },
        verdictOutpoint: { type: 'string', description: '36-byte outpoint hex of the settled verdict' },
        priceSats: { type: 'number' },
        wrapped: { type: 'string', description: 'payload to reveal on buy (hex)' },
        saleTxid: { type: 'string' },
        changeSats: { type: 'number' }
      },
      required: ['op']
    }
  },
  {
    name: 'modelManifest',
    description:
      'AGT-011: declare + anchor which model this instance runs — local weights sha256 or API provider+release. Anchored on __model__ and bound into subsequent receipts. DECLARED, not proven: says which model the operator says answered, not that the inference ran those weights.',
    inputSchema: {
      type: 'object',
      properties: {
        kind: { type: 'string', description: '"local" or "api"' },
        weightsPath: { type: 'string', description: 'local: path to the weights file (hashed)' },
        name: { type: 'string' },
        provider: { type: 'string', description: 'api: e.g. openai' },
        release: { type: 'string', description: 'api: e.g. gpt-5.2-2026-10' }
      },
      required: ['kind']
    }
  },
  {
    name: 'groundedAnswer',
    description:
      "Wright §8 (∀φ∈Rq ∃Ki: VerifyFragment(Ki)∧φ⊢di): takes the caller's draft claims, checks every cite resolves to a VERIFIED fragment and the claim is quoted or Datalog-entailed, STRIPS everything that fails, then anchors an answer receipt. Ungrounded statements are removed — the receipt is the per-answer proof, not an optional add-on.",
    inputSchema: {
      type: 'object',
      properties: {
        claims: {
          type: 'array',
          items: {
            type: 'object',
            properties: {
              text: { type: 'string' },
              literal: { type: 'string', description: 'optional Datalog literal for entailment checking' },
              cites: {
                type: 'array',
                items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] },
                description: 'private-shard cites are auto-proved (ZK membership) when the prover is available; may also carry explicit zk:{queryId,proof}'
              }
            },
            required: ['text', 'cites']
          }
        },
        mode: { type: 'string', description: '"remove" (default — strip ungrounded) or "flag"' },
        modelAttestation: {
          type: 'object',
          description: 'optional inference attestation (from attestInference): the statement is re-derived before being committed'
        },
        attestInference: {
          type: 'object',
          description:
            'auto-attest in one call: {input?, output?, onchain?} — runs attestInference and embeds the validated modelAttestation. Mutually exclusive with modelAttestation.'
        },
        payment: {
          type: 'object',
          description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote',
          properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } }
        }
      },
      required: ['claims']
    }
  },
  {
    name: 'commitReceipt',
    description:
      'AGT-012: commit an answer receipt {answer, fragmentRefs, checkResult} — hashed and anchored on __receipts__. Anchored "what I said" — proves the answer was committed, not that it is true.',
    inputSchema: {
      type: 'object',
      properties: {
        answer: { type: 'string' },
        fragmentRefs: {
          type: 'array',
          items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] },
          description: 'private-shard refs are auto-proved (ZK membership) when the prover is available; explicit zk:{queryId,proof} still accepted'
        },
        checkResult: { type: 'string', description: 'verified | unverifiable | failed' },
        model: { type: 'object', description: 'optional ModelIdentity override; defaults to the manifest set by modelManifest' },
        modelAttestation: {
          type: 'object',
          description: 'optional inference attestation (from attestInference): the statement is re-derived before being committed'
        },
        attestInference: {
          type: 'object',
          description:
            'auto-attest in one call: {input?, output?, onchain?} — runs attestInference and embeds the validated modelAttestation. Mutually exclusive with modelAttestation.'
        },
        groundingReportHash: { type: 'string' },
        payment: {
          type: 'object',
          description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote',
          properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } }
        }
      },
      required: ['answer', 'fragmentRefs', 'checkResult']
    }
  },
  {
    name: 'payQuote',
    description:
      'Issue a payment invoice for a paid tool (NEKTAR_PAYMENTS=1): returns the operator address, amount, and the OP_RETURN invoice tag. Pay it with a signed raw tx (output to payTo ≥ amount + OP_RETURN output carrying the tag), then retry the tool with payment:{invoiceId, transaction}. Settlement is atomic — the tx IS the payment.',
    inputSchema: {
      type: 'object',
      properties: { tool: { type: 'string', enum: ['anchor', 'groundedAnswer', 'commitReceipt'], description: 'the paid tool you intend to call' } },
      required: ['tool']
    }
  },
  {
    name: 'attestInference',
    description:
      'Model-execution attestation (nChain ai_inference-style): re-derives the Groth16 public statement poseidon(input‖output‖poseidon(model)) for the shipped 784×16×10 quantized MLP. DEFAULT PATH: anchors sha256(proof) in a ~300B OP_RETURN attestation tx and publishes the proof at /v1/zkproof/<commit> — cheap, one tx, verifiable by anyone. onchain:true instead deploys the ~398KB in-script verifier and spends it (Wright §18 "executable on-chain if necessary" — the dispute/showcase rail, ~440KB). Returns a modelAttestation record to embed in commitReceipt/groundedAnswer. Only the canned demo vector has a proof today; LLM inference stays declared-only.',
    inputSchema: {
      type: 'object',
      properties: {
        input: { type: 'array', items: { type: 'number' }, description: '784-element input — defaults to the demo vector' },
        output: { type: 'number', description: 'expected argmax — defaults to the demo vector (9)' },
        onchain: {
          type: 'boolean',
          description:
            'true = deploy+spend the in-script verifier under consensus (the heavy rail). Default anchors sha256(proof) in a small attestation tx instead.'
        },
        payment: {
          type: 'object',
          description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote',
          properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } }
        }
      }
    }
  },
  {
    name: 'verifyReceipt',
    description:
      'Scan a receipt token (nektarr1.…) or receiptHash and run the traffic-light checks: the receipt exists on __receipts__, its body re-hashes, the originator signature verifies, it is anchored, and every cited fragment is anchored. Proves the answer was committed + derivable — never that it was true.',
    inputSchema: {
      type: 'object',
      properties: {
        token: { type: 'string', description: 'receipt token "nektarr1.<base64url hash>"' },
        receiptHash: { type: 'string', description: 'or the bare 64-hex receipt hash' }
      }
    }
  },
  {
    name: 'fundingStatus',
    description:
      'Operator fee-wallet status: funding address (send a few sats here), tracked UTXOs, balance, and — on live networks — spend caps, avg fee per anchor and estimated runway. Anchoring fails when the reserve is empty.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'payInvoice',
    description:
      "Pay an invoice (from payQuote — buyer passes invoiceId/amount/lockingScriptHex/opReturn): builds + signs the payment tx from this instance's fund key — output ≥ amount to lockingScriptHex + OP_FALSE OP_RETURN tag. Returns payment:{invoiceId, transaction:<BEEF/EF hex>} ready to retry the paid call with. §16: settlement is an anchored tx — the audit trail is the receipt itself.",
    inputSchema: {
      type: 'object',
      required: ['invoiceId', 'amount', 'lockingScriptHex', 'opReturn'],
      properties: {
        invoiceId: { type: 'string' },
        amount: { type: 'number', description: 'sats to pay, from the invoice' },
        lockingScriptHex: { type: 'string', description: 'payee script from the invoice' },
        opReturn: { type: 'string', description: 'ascii invoice tag, e.g. nektarpay:<id>' }
      }
    }
  },
  {
    name: 'sendSats',
    description:
      "Send sats to an address or locking script — builds, signs and BROADCASTS a plain P2PKH payment from the fee reserve (unlike payInvoice, which returns an unsigned-for-settlement BEEF). Use this to fund another agent's fee address.",
    inputSchema: {
      type: 'object',
      required: ['amount'],
      properties: {
        amount: { type: 'number', description: 'sats to send' },
        address: { type: 'string', description: 'payee base58 address (alternative to scriptHex)' },
        scriptHex: { type: 'string', description: 'payee locking script hex (alternative to address)' },
        opReturn: { type: 'string', description: 'optional ascii OP_RETURN tag' }
      }
    }
  },
  {
    name: 'fundingRefresh',
    description:
      'Reconcile the fee reserve against chain truth: clears spent-marks left by dead unbroadcast txs (phantom entries), marks entries actually spent on-chain, and imports new outputs discovered at the fee address. Fixes "no funding UTXO" / dust-pick / insufficient-fee failures without a restart. Live networks only.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'fundingImport',
    description:
      'Import a funding transaction paying the operator fee address (a txid, or "hex:<rawtx>" where supported) into the tracked reserve — use after sending sats to the fundingStatus address. Live networks only.',
    inputSchema: {
      type: 'object',
      required: ['txid'],
      properties: { txid: { type: 'string', description: 'funding transaction id (or hex:<rawtx>)' } }
    }
  },
  {
    name: 'provisionPlan',
    description:
      'List the lane-locked outputs a provisioning transaction should carry — one entry per lane with its funding lock, per-slot value and a whole tree of slots. Hand the plan to provisionPay on the paying instance, then the block confirming the money also proves the first lane stock.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'provisionPay',
    description:
      'Build, sign and BROADCAST one provisioning transaction from the fee reserve: the lane-locked outputs from a provisionPlan plus a P2PKH payment to the recipient funding address — same guarded path as sendSats.',
    inputSchema: {
      type: 'object',
      required: ['plan', 'address', 'fundsSats'],
      properties: {
        plan: { type: 'object', description: 'provisionPlan result: {outputs: [{kind, lockingScriptHex, satoshis, count}]}' },
        address: { type: 'string', description: 'recipient funding address (from provisionPlan)' },
        fundsSats: { type: 'number', description: 'plain funding sats to include (>=546)' }
      }
    }
  },
  {
    name: 'exportShard',
    description:
      "Export a PUBLIC shard as a portable pack: every record's plaintext + provenance + its anchor proof bundle. Private shards and level>0 records refuse — a pack never carries gated content. importShard on another instance re-verifies everything.",
    inputSchema: {
      type: 'object',
      required: ['shard'],
      properties: { shard: { type: 'string' } }
    }
  },
  {
    name: 'importShard',
    description:
      'Import a shard pack (from exportShard or a remote operator): records re-land preserving provenance, every proof bundle re-verifies against the live chain (zero trust in the exporter — a hostile pack yields unverifiable entries, never trusted records), and the import event itself is recorded on __imports__. Target shard must be empty. Same network only — a regtest pack cannot anchor-verify on mainnet. Give EITHER {pack} (ShardPack JSON), OR {from, shard} (an operator base URL + shard — fetches {from}/v1/pack/{shard}), OR {packUrl} (a full pack URL), OR {store} (a bstore ObjectRef — {key, sha256} or {chirp}, plus optional bucket/endpoint; a chirp:// ref resolves content-addressed objects, unsigned when the gateway allows it — no credentials needed).',
    inputSchema: {
      type: 'object',
      properties: {
        pack: { type: 'object', description: 'the ShardPack JSON returned by exportShard' },
        from: { type: 'string', description: 'operator base URL — fetches {from}/v1/pack/{shard}' },
        shard: { type: 'string', description: 'shard name when importing via {from}' },
        packUrl: { type: 'string', description: 'full pack URL (e.g. from exportShard packUrl or /.well-known/nektar)' },
        store: {
          type: 'object',
          description: 'bstore object ref — {key, sha256} or {chirp} (chirp:// content address), optional bucket/endpoint/size'
        }
      }
    }
  },
  {
    name: 'doctor',
    description:
      'One-call diagnostic: network, funding (address, balance, utxos), shard/record/anchor counts, unproven anchors, payment and prover status. The "is everything working" check. fix:true also repairs what is safe — stale/corrupt instance.lock, missing data dir, missing VERSION marker; anything needing a spend or a judgment call is reported as refused, never attempted silently.',
    inputSchema: {
      type: 'object',
      properties: { fix: { type: 'boolean', description: 'apply safe repairs (stale/corrupt lock, missing dir, VERSION baseline)' } }
    }
  },
  {
    name: 'describeShard',
    description:
      'Commit a shard manifest to __catalog__: the domain tags other agents discover this shard by ("who knows about soccer"), a free-text description, and what is for sale. Anchored + timestamped like any memory — the claim is attributable; re-describing appends a newer manifest. Served via /.well-known/nektar.',
    inputSchema: {
      type: 'object',
      required: ['shard', 'domains'],
      properties: {
        shard: { type: 'string' },
        domains: {
          type: 'array',
          items: { type: 'string' },
          description: 'topic tags this shard covers, e.g. ["soccer","transfer-rumours"] — lowercase a-z0-9._-, up to 32 tags'
        },
        desc: { type: 'string', description: "one-paragraph description of the shard's coverage (≤560 chars)" },
        offers: {
          type: 'array',
          items: {
            type: 'object',
            properties: { kind: { type: 'string', enum: ['answer', 'pack', 'record'] }, priceSats: { type: 'number' }, seq: { type: 'number' } },
            required: ['kind']
          },
          description: 'saleable access modes: answer = paid groundedAnswer, pack = public shard pack, record = one record via sale escrow (seq required)'
        }
      }
    }
  },
  {
    name: 'importRecord',
    description:
      'Land a record purchased via sale#claim as local memory — pass the VERIFIED claim payload (data + provenance). An __imports__ audit record commits the acquisition (source shard:seq, leaf, settlement txid).',
    inputSchema: {
      type: 'object',
      required: ['data'],
      properties: {
        data: { type: 'string', description: 'the verified claim payload' },
        provenance: { type: 'object', description: '{sourcePub,hash,timestamp,sig} from the claim — preserved on the record' },
        sourceShard: { type: 'string' },
        sourceSeq: { type: 'number' },
        leaf: { type: 'string' },
        saleTxid: { type: 'string' },
        shard: { type: 'string', description: 'local shard to land it on (default "bought")' }
      }
    }
  },
  {
    name: 'automation',
    description:
      'Autonomous marketplace under an operator-approved policy. op=publishWant {domains, question, maxSats, policyHash} publishes a signed want and the runtime collects offers, scores them, funds escrow, verifies the disclosure and refunds on timeout without further calls. op=enableSeller {policyHash} answers matching wants from the sellable shards named in the policy. op=status {operationId} | operations | approvals | budget {policyHash} | policies are read-only. Approving policies and exceptions is operator-only (pnpm automation ...), never available here.',
    inputSchema: {
      type: 'object',
      required: ['op'],
      properties: {
        op: { type: 'string', enum: ['publishWant', 'enableSeller', 'status', 'operations', 'approvals', 'budget', 'policies'] },
        policyHash: { type: 'string' },
        operationId: { type: 'string' },
        domains: { type: 'array', items: { type: 'string' } },
        question: { type: 'string' },
        maxSats: { type: 'number' },
        wantId: { type: 'string' }
      }
    }
  },
  {
    name: 'want',
    description:
      "Publish a signed purchase want under the agent's single active buyer policy — the engine collects offers, selects, escrows and settles autonomously.",
    inputSchema: {
      type: 'object',
      properties: {
        domains: { type: 'array', items: { type: 'string' }, description: 'domain tags sellers watch' },
        question: { type: 'string', description: 'what the buyer needs (<=560 chars)' },
        maxSats: { type: 'number', description: 'price ceiling in sats' }
      },
      required: ['domains', 'maxSats']
    }
  },
  {
    name: 'sell',
    description:
      'List a shard for sale and enable autonomous selling: commits the manifest (describeShard with a record offer at priceSats) and arms the seller under the single active seller policy.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        domains: { type: 'array', items: { type: 'string' } },
        priceSats: { type: 'number' },
        desc: { type: 'string', description: 'free-text manifest description' }
      },
      required: ['shard', 'domains', 'priceSats']
    }
  },
  {
    name: 'automationDoctor',
    description:
      'Automation preflight — the readiness gate for the market agent. Re-runs every dependency check now (chain, broadcaster, hosts, store read-back, payments, policy, funds, watch liveness, spendguard dry-run, kill switch, conformance token) and reports {check,pass,detail,fix}[] with a deterministic verdict. Sensitive details (reserves, host lists, policy) stay on this trusted channel — the public HTTP surface reports pass only.',
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'deals',
    description:
      "List this agent's market operations (newest first, max 50) or detail one: state machine, event trail, sealed seller wraps, and the disclosed result once verified.",
    inputSchema: {
      type: 'object',
      properties: { operationId: { type: 'string' } }
    }
  },
  {
    name: 'fund',
    description:
      "Funding overview: receive address, spendable balance, tracked UTXOs, free market/bbox funding-tree slots, and the single active policy's budget when there is one.",
    inputSchema: { type: 'object', properties: {} }
  },
  {
    name: 'evidence',
    description:
      "Fetch a stored trade-evidence bundle from the bstore object store and re-verify it: {ref} ({bucket?, key, sha256, size?}) or {operationId} to resolve the operation's stored ref. Returns {ok, checks} — redacted bundles name the checks the withheld question/disclosure fields make unavailable.",
    inputSchema: {
      type: 'object',
      properties: {
        ref: {
          type: 'object',
          properties: { bucket: { type: 'string' }, key: { type: 'string' }, sha256: { type: 'string' }, size: { type: 'number' } },
          required: ['key', 'sha256']
        },
        operationId: { type: 'string' }
      }
    }
  }
] as const

// consolidated state machines: op → internal case label (never listed in
// TOOLS — reachable only through the 'sale'/'claim' dispatchers, which keep
// the paid-disclosure gate on BOTH the tool and the resolved op)
const PAID_SALE_TOOLS = new Set([
  'sale',
  'sale#offer',
  'sale#offerMemory',
  'sale#escrow',
  'sale#escrowBuild',
  'sale#complete',
  'sale#refund',
  'sale#claim',
  'sale#pay',
  'sale#payBuild'
])
const paidSaleEnabled = () => /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAID_DISCLOSURE') ?? '')
/** tools visible to clients right now — paid-disclosure tools are gated by env */
const AGENT_TOOLS = new Set(['remember', 'recall', 'groundedAnswer', 'verifyReceipt', 'want', 'sell', 'deals', 'fund', 'doctor', 'automationDoctor'])
const toolProfile = (): 'agent' | 'operator' => ((nektarEnv('NEKTAR_TOOL_PROFILE') ?? 'agent').toLowerCase() === 'operator' ? 'operator' : 'agent')
export const visibleTools = () =>
  TOOLS.filter((t) => (toolProfile() === 'operator' || AGENT_TOOLS.has(t.name)) && (!PAID_SALE_TOOLS.has(t.name) || paidSaleEnabled()))

export type { ToolCtx }

/** tools that change instance state — a successful call emits 'change' on
 *  mem.events so live surfaces (dashboard SSE) refresh instantly */
const MUTATING_TOOLS = new Set([
  'automation',
  'remember',
  'describeShard',
  'groundedAnswer',
  'commitReceipt',
  'importRecord',
  'importShard',
  'disclose',
  'rotate',
  'revoke',
  'payInvoice',
  'payQuote',
  'authorize',
  'challenge',
  'bondDeploy',
  'bondSeize',
  'bondCommit',
  'bondClose',
  'anchor',
  'automaton',
  'claim',
  'sale',
  'attestInference',
  'fundingImport',
  'sendSats',
  'fundingRefresh',
  'provisionPay'
])

export async function callTool(mem: NektarMemory, name: string, args: any, ctx: ToolCtx = {}): Promise<any> {
  const r = await dispatchTool(mem, name, args, ctx)
  if (MUTATING_TOOLS.has(name) || name.includes('#')) mem.events.emit('change', name)
  return r
}

/** every tool name → its group handler. Built once from the tools/* modules;
 *  internal '<group>#<op>' names live here too — never listed in TOOLS, only
 *  reachable when a group dispatcher re-enters through callTool */
const HANDLERS: Record<string, ToolHandler> = {
  ...PAY_HANDLERS,
  ...SALE_HANDLERS,
  ...RECORD_HANDLERS,
  ...ZK_HANDLERS,
  ...IDENTITY_HANDLERS,
  ...BOND_HANDLERS,
  ...CLAIM_HANDLERS,
  ...ANSWER_HANDLERS,
  ...PACK_HANDLERS,
  ...AUTOMATION_HANDLERS
}

async function dispatchTool(mem: NektarMemory, name: string, args: any, ctx: ToolCtx = {}): Promise<any> {
  if (PAID_SALE_TOOLS.has(name) && !paidSaleEnabled()) throw new Error(`${name}: paid disclosure is disabled — set NEKTAR_PAID_DISCLOSURE=1`)
  // NEKTAR_PAYMENTS=1 (BRC-166-style): anchor/groundedAnswer/commitReceipt need a
  // settled payment tx before they run — atomic, one tx per call, publicly
  // auditable. No payment → return the invoice (the 402 challenge) as data.
  let paid: import('./pay.js').PaymentRecord | undefined
  // paid content paths: a manifest's offers price recall ('answer' → read
  // sats) and pack downloads ('pack' sats) for EXTERNAL callers. Priced
  // shards join the PAID_TOOLS gate with the manifest's amount.
  const pricedTool = async (): Promise<{ tool: string; amount: number } | undefined> => {
    if (!paymentsEnabled() || !ctx.external) return undefined
    if (name === 'recall' && args?.shard) {
      const p = await mem.contentPrice(String(args.shard), 'read').catch(() => 0)
      if (p > 0) return { tool: 'recall', amount: p }
    }
    if (name === 'exportShard' && args?.shard) {
      const p = await mem.contentPrice(String(args.shard), 'pack').catch(() => 0)
      if (p > 0) return { tool: 'exportShard', amount: p }
    }
    return undefined
  }
  const gated = paymentsEnabled() ? (PAID_TOOLS.has(name) ? { tool: name, amount: priceFor(name) } : await pricedTool()) : undefined
  if (gated) {
    if (typeof args?.payment?.transaction !== 'string') {
      const { required } = await paymentRequired(mem, gated.tool, gated.amount)
      return { ok: false, error: `payment required — ${gated.tool} costs ${gated.amount} sats`, ...required }
    }
    paid = await settlePayment(mem, gated.tool, args.payment)
  }
  const handler = HANDLERS[name]
  if (handler === undefined) throw new Error(`unknown tool ${name}`)
  return await handler({ mem, name, args, ctx, paid, callTool })
}

/** MCP over stdio lives in mcp-server.ts (it owns the SDK wiring — server.ts
 *  stays dependency-free of it; bin.ts + index.ts import from there) */
