// Tool definitions (JSON Schema) + their single implementation (callTool),
// shared by the MCP server (mcp-server.ts, official SDK) and the HTTP API.
import { nektarEnv } from '@nektar/core'
import { receiptToken } from './receipt.js'
import { PAID_TOOLS, paymentsEnabled, paymentRequired, priceFor, settlePayment } from './pay.js'
import type { NektarMemory } from './memory.js'

/** ready-to-paste share surface for a receipt — the model drops `markdown`
 *  straight into PRs/docs; every pasted badge is a verify-page entry point */
const shareBlock = (token: string) => {
  const verifyUrl = `${verifyBase()}/v1/verify?r=${token}`
  const badgeUrl = `${verifyBase()}/v1/badge?r=${token}`
  return { token, verifyUrl, badgeUrl, markdown: `[![verified answer](${badgeUrl})](${verifyUrl})` }
}

/** public base of /v1/verify for footers/QRs — NEKTAR_VERIFY_BASE for hosted
 *  deployments (https://host or ngrok), else the local HTTP port. */
const verifyBase = (): string => nektarEnv('NEKTAR_VERIFY_BASE') ?? `http://localhost:${nektarEnv('NEKTAR_HTTP_PORT') ?? 8080}`

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
const SALE_OPS = new Set(['offer', 'offerMemory', 'escrow', 'complete', 'refund', 'claim'])
const CLAIM_OPS = new Set(['deploy', 'status', 'settle', 'deploySale', 'buySale'])
const paidSaleEnabled = () => /^(1|true|yes)$/i.test(nektarEnv('NEKTAR_PAID_DISCLOSURE') ?? '')
/** tools visible to clients right now — paid-disclosure tools are gated by env */
const AGENT_TOOLS = new Set(['remember', 'recall', 'groundedAnswer', 'verifyReceipt', 'want', 'sell', 'deals', 'fund', 'doctor', 'automationDoctor'])
const toolProfile = (): 'agent' | 'operator' => ((nektarEnv('NEKTAR_TOOL_PROFILE') ?? 'agent').toLowerCase() === 'operator' ? 'operator' : 'agent')
export const visibleTools = () =>
  TOOLS.filter((t) => (toolProfile() === 'operator' || AGENT_TOOLS.has(t.name)) && (!PAID_SALE_TOOLS.has(t.name) || paidSaleEnabled()))

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

/** live-network runtime surface handed in by the MCP/HTTP transport layer —
 *  absent under stdio-less callers (tests, hooks), in which case the funding
 *  tools fall back to mem's own reserve view */
export interface ToolCtx {
  /** true for unauthenticated external callers (HTTP without bearer); owner
   *  channels (stdio/MCP, bearer-authed) leave it unset — priced-content
   *  gates charge externals only */
  external?: boolean
  ops?: {
    wallet?: () => Promise<unknown>
    costs?: () => unknown
    importFunding?: (txid: string) => Promise<unknown>
    refreshFunding?: () => Promise<unknown>
  }
}

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
  'fundingRefresh'
])

function settleEscrow(mem: NektarMemory, funding: Array<{ tx: import('@bsv/sdk').Transaction; vout: number }>, tx: import('@bsv/sdk').Transaction): void {
  for (const inp of funding) mem.markSpentOut(inp.tx.id('hex'), inp.vout)
  if (tx.outputs[1]) mem.addOwnedOut(tx, 1)
}

export async function callTool(mem: NektarMemory, name: string, args: any, ctx: ToolCtx = {}): Promise<any> {
  const r = await dispatchTool(mem, name, args, ctx)
  if (MUTATING_TOOLS.has(name) || name.includes('#')) mem.events.emit('change', name)
  return r
}

/** the single non-revoked policy carrying `role` for this agent+network —
 *  exactly-one is an operator decision: none means "approve one first",
 *  several means "revoke down to one" — the agent never picks silently */
async function singlePolicy(au: NonNullable<NektarMemory['automation']>, role: 'buyer' | 'seller', tool: string): Promise<string> {
  const found: string[] = []
  for (const r of await au.store.policies()) {
    if (r.revokedAt !== undefined) continue
    const p = await au.store.activePolicy(r.policyHash)
    if (p && p.roles.includes(role) && p.agentPub === au.engine.agentPub && p.network === au.engine.network) found.push(r.policyHash)
  }
  if (found.length !== 1) throw new Error(`${tool}: operator must approve exactly one ${role} policy (pnpm automation policy approve …)`)
  return found[0]
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
  switch (name) {
    case 'payQuote': {
      if (!PAID_TOOLS.has(String(args?.tool))) throw new Error(`payQuote: unknown or unpaid tool "${args?.tool}"`)
      if (!paymentsEnabled()) throw new Error('payments are disabled — set NEKTAR_PAYMENTS=1 (and NEKTAR_OPERATOR_WIF)')
      const { invoice, required } = await paymentRequired(mem, String(args.tool))
      return {
        invoice: {
          invoiceId: invoice.invoiceId,
          tool: invoice.tool,
          amount: invoice.amount,
          payTo: invoice.payTo,
          lockingScriptHex: invoice.lockingScriptHex,
          opReturn: invoice.opReturn,
          expires: new Date(invoice.expires).toISOString(),
          expiresMs: invoice.expires,
          pub: invoice.pub,
          quoteSig: invoice.quoteSig
        },
        paymentRequired: required
      }
    }
    case 'payInvoice': {
      // F3 — a grantId scopes this payment to the cumulative ledger; the
      // lifetime cap enforces, not just this call's amount
      const gid = typeof args?.grantId === 'string' ? args.grantId : undefined
      if (gid && mem.budget) {
        const r = mem.budget.reserve(gid, `payInvoice:${args.invoiceId}`, Number(args.amount))
        if (r === 'exists') throw new Error(`payInvoice: action already recorded for grant ${gid} � idempotent replay refused`)
        if (r !== 'ok') throw new Error(`payInvoice: grant ${gid} budget ${r}`)
        try {
          const out = await mem.payInvoice({
            invoiceId: String(args.invoiceId),
            amount: Number(args.amount),
            lockingScriptHex: String(args.lockingScriptHex),
            opReturn: String(args.opReturn)
          })
          mem.budget.commit(gid, `payInvoice:${args.invoiceId}`)
          return out
        } catch (e) {
          mem.budget.release(gid, `payInvoice:${args.invoiceId}`)
          throw e
        }
      }
      return await mem.payInvoice({
        invoiceId: String(args.invoiceId),
        amount: Number(args.amount),
        lockingScriptHex: String(args.lockingScriptHex),
        opReturn: String(args.opReturn)
      })
    }
    case 'sendSats': {
      return await mem.sendSats({ amount: Number(args.amount), address: args.address, scriptHex: args.scriptHex, opReturn: args.opReturn })
    }
    case 'sale': {
      const op = String(args?.op ?? '')
      if (!SALE_OPS.has(op)) throw new Error('sale: op required — offer|offerMemory|escrow|escrowBuild|complete|refund|claim')
      // F3 — escrow funding under a grant goes through the cumulative ledger;
      // the sale's price is the all-in debit reserved before funding
      const gid = typeof args?.grantId === 'string' ? args.grantId : undefined
      if (op === 'escrow' && gid && mem.budget) {
        const actionId = `sale:escrow:${args.cfg ? JSON.stringify(args.cfg).slice(0, 60) : Date.now()}`
        const amount = Number(args.cfg?.price ?? args.amount ?? 0)
        const r = mem.budget.reserve(gid, actionId, amount)
        if (r === 'exists') throw new Error(`sale: action already recorded for grant ${gid} � idempotent replay refused`)
        if (r !== 'ok') throw new Error(`sale: grant ${gid} budget ${r}`)
        try {
          const out = await callTool(mem, `sale#${op}`, args, ctx)
          mem.budget.commit(gid, actionId)
          return out
        } catch (e) {
          mem.budget.release(gid, actionId)
          throw e
        }
      }
      return await callTool(mem, `sale#${op}`, args, ctx)
    }
    case 'sale#offer': {
      const { offerDisclosure } = await import('@nektar/identity')
      const { COVENANT_ID } = await import('@nektar/anchor')
      const r = await offerDisclosure({
        sellerKey: mem.wrapCustody(),
        value: new Uint8Array(Buffer.from(args.value, 'hex')),
        price: BigInt(args.price),
        refundAfter: BigInt(args.refundAfter),
        maxFee: BigInt(args.maxFee),
        buyerPub: args.buyerPub,
        expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined
      })
      // JSON-RPC cannot carry BigInt — cfg crosses the wire as plain numbers
      return {
        cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
        wrapped: r.wrapped,
        sellerPub: r.sellerPub,
        expiresAt: Number(r.expiresAt ?? 0n),
        offerSig: r.offerSig,
        artifact: COVENANT_ID
      }
    }
    case 'sale#offerMemory': {
      const { offerDisclosure, encodeKeySale } = await import('@nektar/identity')
      const { COVENANT_ID } = await import('@nektar/anchor')
      const { serializeFragment, computeLeaf, toHex } = await import('@nektar/core')
      const rec = (await mem.records(args.shard)).find((r) => r.seq === args.seq)
      if (!rec) throw new Error(`no record seq ${args.seq} in shard "${args.shard}"`)
      // Paper f_i = {c_i, H(d_i), π_i}: an encrypted record is sold as its
      // FRAGMENT KEY (the §7 decryption capability) + ciphertext + provenance;
      // a plaintext record is sold as its canonical serialized fragment.
      let payload: Uint8Array
      let mode: 'key' | 'fragment'
      if (rec.fragment.E && rec.fragment.E.length && !rec.fragment.d.length) {
        payload = encodeKeySale(await mem.fragmentKeyFor(rec), rec.fragment.E, rec.fragment.p)
        mode = 'key'
      } else {
        payload = serializeFragment(rec.fragment)
        mode = 'fragment'
      }
      const leafHex = toHex(rec.leaf ?? computeLeaf(rec.fragment))
      // π_i — the record's inclusion proof — goes into the commitment so the
      // buyer verifies leaf∈anchored-batch BEFORE funding (ZKCP verify-before-pay).
      const { bundle } = await mem.prove(args.shard, args.seq)
      const r = await offerDisclosure({
        sellerKey: mem.wrapCustody(),
        value: payload,
        price: BigInt(args.price),
        refundAfter: BigInt(args.refundAfter),
        maxFee: BigInt(args.maxFee),
        buyerPub: args.buyerPub,
        leaf: leafHex,
        expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined,
        direct: args.direct === true
      })
      return {
        cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
        wrapped: r.wrapped,
        sellerPub: r.sellerPub,
        expiresAt: Number(r.expiresAt ?? 0n),
        offerSig: r.offerSig,
        artifact: COVENANT_ID,
        ...(r.direct === true ? { direct: true } : {}),
        mode,
        commitment: {
          leaf: leafHex,
          anchorTxid: rec.anchorTxid ?? null,
          hdp: toHex(rec.hdp),
          shard: args.shard,
          seq: args.seq,
          authorPub: rec.authorPub ?? null,
          level: rec.level ?? 0,
          batchR: bundle?.anchor.R ?? null,
          proof: bundle ? { path: bundle.path, leafIndex: bundle.leafIndex, n: bundle.n, size: bundle.size } : null
        }
      }
    }
    case 'sale#escrow':
    case 'sale#escrowBuild': {
      const isBuild = name === 'sale#escrowBuild'
      const { DisclosureSaleHost } = await import('@nektar/anchor')
      // §16/17 delegation-sig check: refuse to fund an escrow for a cfg the
      // seller never signed — blocks MITM cfg-swap griefing (deposit lockup).
      if (args.offer) {
        const { verifyOfferSig } = await import('@nektar/identity')
        const { reconstructRoot, toHex: th, fromHex: fh } = await import('@nektar/core')
        const buyerPub = mem.operatorKey().toPublicKey().toString()
        const leaf = args.offer.commitment?.leaf ?? ''
        // the sig binds whatever buyerPub the seller offered to — a public
        // offer signed with '' verifies against '', a private one against the
        // operator's own pub. Accept either; the sig covers the choice.
        if (
          !verifyOfferSig(
            { cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt },
            args.offer.buyerPub ?? buyerPub,
            leaf
          ) &&
          !verifyOfferSig({ cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt }, '', leaf)
        )
          throw new Error('sale op=escrow: offerSig does not verify — cfg may have been substituted')
        // π_i pre-funding check: if the commitment carries a path, the leaf
        // must rebuild the committed batch root — catch a bogus leaf early.
        const proof = args.offer.commitment?.proof
        if (proof?.path && args.offer.commitment?.batchR) {
          const root = reconstructRoot(fh(leaf), { index: proof.leafIndex, siblings: proof.path.map((h: string) => fh(h)) })
          if (th(root) !== args.offer.commitment.batchR) throw new Error('sale op=escrow: leaf path does not reach the committed batch root')
        }
        // §19 horizon: refuse to fund an offer that has lapsed — a signed
        // offer is only good until expiresAt (0 = no expiry).
        const exp = BigInt(args.offer.expiresAt ?? 0)
        if (exp > 0n && BigInt(await mem.chainHeight()) >= exp) throw new Error(`sale op=escrow: offer expired at height ${exp}`)
      }
      // §19 sanity: the escrow must not be instantly refundable — refundAfter
      // is a locktime gate (height < 500M ⇒ block height, else timestamp).
      {
        const ra = BigInt(args.cfg.refundAfter)
        const future = ra < 500_000_000n ? ra > BigInt(await mem.chainHeight()) : ra > BigInt(Math.floor(Date.now() / 1000))
        if (!future) throw new Error('sale op=escrow: cfg.refundAfter is not in the future — the escrow would be instantly refundable')
      }
      const depositorPkh = Buffer.from(mem.operatorKey().toPublicKey().toHash()).toString('hex')
      const host = new DisclosureSaleHost({
        ...args.cfg,
        depositorPkh,
        price: BigInt(args.cfg.price),
        refundAfter: BigInt(args.cfg.refundAfter),
        maxFee: BigInt(args.cfg.maxFee)
      })
      const funding = await mem.fundingOuts(args.amount + 500)
      const { tx, utxo, feeSats } = await host.buildEscrow(funding, mem.operatorKey(), args.amount, undefined, mem.feeRateSatPerKb)
      if (isBuild) return { tx, txHex: tx.toHex(), utxo: { ...utxo, txid: tx.id('hex') }, escrow: { ...args.cfg, depositorPkh }, feeSats }
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      settleEscrow(mem, funding, tx)
      return { accepted: true, txid: tx.id('hex'), utxo: { ...utxo, txid: tx.id('hex') }, escrow: { ...args.cfg, depositorPkh } }
    }
    case 'sale#pay':
    case 'sale#payBuild': {
      // the direct rail: an outright P2PKH payment to the quote's sellerPkh —
      // payment-final, so the same delegation-sig guard applies (a swapped
      // cfg would point the payment at a substituted key)
      const isBuild = name === 'sale#payBuild'
      if (args.offer) {
        const { verifyOfferSig } = await import('@nektar/identity')
        const buyerPub = mem.operatorKey().toPublicKey().toString()
        const leaf = args.offer.commitment?.leaf ?? ''
        if (
          !verifyOfferSig(
            { cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt },
            args.offer.buyerPub ?? buyerPub,
            leaf
          ) &&
          !verifyOfferSig({ cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt }, '', leaf)
        )
          throw new Error('sale op=pay: offerSig does not verify — cfg may have been substituted')
        const exp = BigInt(args.offer.expiresAt ?? 0)
        if (exp > 0n && BigInt(await mem.chainHeight()) >= exp) throw new Error(`sale op=pay: offer expired at height ${exp}`)
      }
      const pr = await mem.sendSats({ amount: args.amount, scriptHex: args.scriptHex, buildOnly: isBuild })
      if (isBuild) return { tx: pr.tx, txHex: pr.txHex, utxo: pr.utxo }
      return { accepted: true, txid: pr.txid, utxo: pr.utxo, txHex: pr.txHex }
    }
    case 'sale#complete': {
      const { DisclosureSaleHost } = await import('@nektar/anchor')
      // depositorPkh is always the LOCAL depositor (operator key) — the offer's
      // cfg never carries it; fill it here so callers can pass the raw offer cfg.
      const depositorPkh = args.cfg.depositorPkh ?? Buffer.from(mem.operatorKey().toPublicKey().toHash()).toString('hex')
      const host = new DisclosureSaleHost({
        ...args.cfg,
        depositorPkh,
        price: BigInt(args.cfg.price),
        refundAfter: BigInt(args.cfg.refundAfter),
        maxFee: BigInt(args.cfg.maxFee)
      })
      // covenant bound: utxo − price − change ≤ maxFee → change ≥ utxo − price − maxFee.
      // Default the change so the spend pays ~350 sats (≈mainnet relay for ~1.5KB),
      // never more than the bound allows.
      const changeSats =
        args.changeSats !== undefined
          ? BigInt(args.changeSats)
          : BigInt(args.utxo.satoshis) - BigInt(args.cfg.price) - BigInt(Math.min(Number(args.cfg.maxFee) - 2, 350))
      // the remembered escrow is the reveal's real parent — with it attached
      // the tx broadcasts as proper EF and the buyer receives BEEF/EF, not a
      // synthetic stub that cannot carry ancestry
      const { tx } = host.buildBuy(args.utxo, args.wrapped, changeSats, await mem.fetchTx(args.utxo.txid))
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      return { accepted: true, txid: tx.id('hex'), sellerSats: tx.outputs[0].satoshis, txHex: tx.toHex() }
    }
    case 'sale#refund': {
      const { DisclosureSaleHost } = await import('@nektar/anchor')
      const depositorPkh = args.cfg.depositorPkh ?? Buffer.from(mem.operatorKey().toPublicKey().toHash()).toString('hex')
      const host = new DisclosureSaleHost({
        ...args.cfg,
        depositorPkh,
        price: BigInt(args.cfg.price),
        refundAfter: BigInt(args.cfg.refundAfter),
        maxFee: BigInt(args.cfg.maxFee)
      })
      const changeSats = args.changeSats !== undefined ? BigInt(args.changeSats) : BigInt(args.utxo.satoshis) - BigInt(Math.min(Number(args.cfg.maxFee), 350))
      const { tx } = await host.buildRefund(
        args.utxo,
        mem.operatorKey(),
        args.lockTime ?? Number(args.cfg.refundAfter),
        changeSats,
        await mem.fetchTx(args.utxo.txid)
      )
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      return { accepted: true, txid: tx.id('hex'), refundSats: tx.outputs[0].satoshis }
    }
    case 'sale#claim': {
      const { Transaction } = await import('@bsv/sdk')
      const { claimDisclosure, claimValue } = await import('@nektar/identity')
      const { fromHex: fh2 } = await import('@nektar/core')
      // the direct rail delivers `wrapped` inside the envelope — no covenant
      // carrier to parse; verification is the same sha256→hWrapped→unwrap path
      const r =
        args.valueHex !== undefined
          ? await claimValue(fh2(args.valueHex), { hWrapped: args.hWrapped, buyerKey: mem.wrapCustody(), sellerPub: args.sellerPub })
          : await claimDisclosure(Transaction.fromHex(args.txHex), { hWrapped: args.hWrapped, buyerKey: mem.wrapCustody(), sellerPub: args.sellerPub })
      const out: any = { mode: r.mode, valueHex: Buffer.from(r.value).toString('hex') }
      // memory-bound payload? verify the bought bytes hash to the anchored leaf
      if (args.commitment?.leaf) {
        const { parseFragment, computeLeaf, toHex, fromHex } = await import('@nektar/core')
        try {
          if (args.offerMode === 'key') {
            // key-sale: payload carries the fragment key — decrypt E, rebuild
            // the fragment, then the same leaf check proves inclusion.
            const { claimKeySale } = await import('@nektar/identity')
            const { verifyProvenance } = await import('@nektar/log')
            const { d, p, E } = claimKeySale(r.value)
            const frag = { d, p, hdp: args.commitment.hdp ? fromHex(args.commitment.hdp) : undefined, E }
            out.verified = toHex(computeLeaf(frag)) === args.commitment.leaf
            if (out.verified && args.commitment.authorPub && p.signature.length) {
              out.authorVerified = verifyProvenance(args.commitment.authorPub, d, p)
            }
            out.valueHex = Buffer.from(d).toString('hex')
            out.decrypted = true
          } else {
            const frag = parseFragment(r.value)
            out.verified = toHex(computeLeaf(frag)) === args.commitment.leaf
            // §16/19: a bought memory must carry verifiable authorship —
            // check the provenance σ against the recorded author key.
            if (out.verified && args.commitment.authorPub && frag.p.signature.length) {
              const { verifyProvenance } = await import('@nektar/log')
              out.authorVerified = verifyProvenance(args.commitment.authorPub, frag.d, frag.p)
            }
          }
          if (out.verified && args.commitment.anchorTxid) out.anchorTxid = args.commitment.anchorTxid
          if (!out.verified) out.reason = 'computeLeaf(payload) != committed leaf'
        } catch (e) {
          out.verified = false
          out.reason = `payload verification failed: ${(e as Error).message}`
        }
      }
      return out
    }
    case 'remember': {
      if (args.private === false && mem.shardPrivate(args.shard)) throw new Error(`shard "${args.shard}" is private — the flag is monotone and cannot be unset`)
      const r = await mem.remember(args.shard, args.data, { source: args.source, modality: args.modality }, args.level ?? 0, { premiseRefs: args.premiseRefs })
      if (args.private === true) mem.setShardPrivate(args.shard)
      return { ...r, private: mem.shardPrivate(args.shard) || undefined }
    }
    case 'recall': {
      const { decodePresentation } = await import('@nektar/identity')
      const r = await mem.recall(args.shard, { seq: args.seq, identity: args.identity, zk: args.zk === undefined ? undefined : decodePresentation(args.zk) })
      // tell the model how to recover from a wrong seq instead of an empty answer
      if (args.seq !== undefined && r.items.length === 0) {
        const seqs = (await mem.records(args.shard)).map((x) => x.seq)
        return {
          ...r,
          hint: seqs.length
            ? `no record with seq ${args.seq} in shard "${args.shard}"; existing seqs: ${seqs.join(', ')} — or omit seq to recall all`
            : `shard "${args.shard}" is empty`
        }
      }
      return { ...r, shardPrivate: mem.shardPrivate(args.shard) || undefined }
    }
    case 'prove':
      // fail-closed: a private shard's plain merkle citation is a leak —
      // the same verification goal is met by proveMembership (ZK).
      if (mem.shardPrivate(args.shard))
        return {
          ok: false,
          reason: `shard "${args.shard}" is private — plain citations refused; cite via a ZK membership proof (zkQuery → proveMembership → verifyMembership)`
        }
      return mem.prove(args.shard, args.seq)
    case 'anchor': {
      const r = await mem.anchorShard(args.shard)
      return paid ? { ...r, paid } : r
    }
    case 'zkQuery':
      return { ...mem.issueZkQuery(args.verifierId), statement: 'single use, expires in 10 minutes; the holder binds queryId into its STARK' }
    case 'proveMembership': {
      const { Buffer } = await import('node:buffer')
      const { bytes, ms } = await mem.proveMembership(args.shard, args.seq, args.queryId)
      return {
        lzkmem: Buffer.from(bytes).toString('hex'),
        bytes: bytes.length,
        proveMs: ms,
        statement: 'ZK-STARK: some record is in the anchored SHA-256 batch tree; reveals no leaf, index or path'
      }
    }
    case 'verifyMembership': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyMembership(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'verifyCombined': {
      const { Buffer } = await import('node:buffer')
      const { decodePresentation } = await import('@nektar/identity')
      return mem.verifyCombined(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')), decodePresentation(args.zk), {
        scope: args.scope,
        derivationBytes: args.derivationBytes === undefined ? undefined : new Uint8Array(Buffer.from(args.derivationBytes, 'hex')),
        callerPubkey: args.callerPubkey
      })
    }
    case 'attestMembership': {
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
    }
    case 'verifyAttestation': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyAttestation(args.txid, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'proveSubset': {
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
    }
    case 'verifySubset': {
      const { Buffer } = await import('node:buffer')
      return mem.verifySubset(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'proveDerivation': {
      const { Buffer } = await import('node:buffer')
      const { bytes, ms } = await mem.proveDerivation(args.shard, args.query, args.queryId)
      return {
        lzkder: Buffer.from(bytes).toString('hex'),
        bytes: bytes.length,
        proveMs: ms,
        statement: 'ZK-STARK: a derivation path to the conclusion exists under the anchored policy DAG; reveals no rule, premise or fact'
      }
    }
    case 'verifyDerivation': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyDerivation(args.shard, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'rotate': {
      const keyId = args.newOwnerKeyId === undefined ? undefined : String(args.newOwnerKeyId)
      const wif = args.newOwnerWif === undefined ? undefined : String(args.newOwnerWif)
      if ((keyId === undefined) === (wif === undefined)) throw new Error('rotate needs exactly one of newOwnerKeyId (wallet) or newOwnerWif (legacy)')
      const { PrivateKey } = await import('@bsv/sdk')
      const newKey = keyId !== undefined ? mem.walletKeySigner(keyId) : PrivateKey.fromWif(wif!)
      const r = await mem.rotateShard(args.shard, newKey)
      return { ...r, receipt: { decision: 'allow', action: 'rotate' } }
    }
    case 'automaton': {
      if (args.op === 'deploy') return automatonView(await mem.automatonDeploy(args.id, args.def))
      const ac = mem.automaton(args.id)
      if (args.op === 'step') return automatonView(await ac.step(args.sym, args.to, args.outHash))
      if (args.op === 'halt') return automatonView(await ac.halt())
      if (args.op === 'state') {
        const h = ac.getHead()
        return h ? automatonView(h) : null
      }
      throw new Error(`automaton: unknown op ${args.op}`)
    }
    case 'revoke': {
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
    }
    case 'revocationProof': {
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
    }
    case 'revocationStatus': {
      return await mem.revocationStatus(String(args.pkHex))
    }
    case 'bondDeploy': {
      const r = await mem.bondDeploy(args.sats, args.releaseHeight === undefined ? undefined : BigInt(args.releaseHeight), args.force === true)
      return { ...r, releaseHeight: r.releaseHeight.toString(), receipt: { decision: 'allow', action: 'bondDeploy' } }
    }
    case 'bondStatus': {
      const s = mem.bondStatus()
      return s ? { ...s, releaseHeight: s.releaseHeight.toString(), deadline: s.deadline.toString() } : { deployed: false }
    }
    case 'bondSeize': {
      const bond = args.bond
        ? {
            ...args.bond,
            cfg: {
              ...args.bond.cfg,
              bondReleaseHeight: BigInt(args.bond.cfg.bondReleaseHeight),
              maxFee: BigInt(args.bond.cfg.maxFee),
              epochLen: BigInt(args.bond.cfg.epochLen)
            }
          }
        : undefined
      const r = await mem.bondSeize(args.payToScript, bond)
      return r ? { ...r, receipt: { decision: 'allow', action: 'bondSeize' } } : { deployed: false }
    }
    case 'bondCommit': {
      const r = await mem.bondCommit(args.root)
      return r ? { ...r, n: r.n.toString(), receipt: { decision: 'allow', action: 'bondCommit' } } : { deployed: false }
    }
    case 'bondClose': {
      const r = await mem.bondClose()
      return r ? { ...r, receipt: { decision: 'allow', action: 'bondClose' } } : { deployed: false }
    }
    case 'disclose': {
      const idm = await import('@nektar/identity')
      const verifyAnchor = async () => (await mem.prove(args.shard, args.seq)).result.ok === true
      const r = await idm.verifyDisclosureAnchored(
        args.envelope,
        args.ownerPub,
        args.verifierPub,
        args.fieldCount,
        Buffer.from(args.fieldRoot, 'hex'),
        (args.siblings as string[]).map((s) => Buffer.from(s, 'hex')),
        args.atTime,
        verifyAnchor
      )
      return { verified: r.ok, reason: r.reason ?? null, receipt: { decision: r.ok ? 'allow' : 'deny', action: 'disclose' } }
    }
    case 'authorize': {
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
    }
    case 'challenge':
      return { nonce: mem.issueChallenge(), statement: 'sign with answerChallenge(key, nonce) within 5 minutes; nonces from anywhere else are refused' }
    case 'claim': {
      const op = String(args?.op ?? '')
      if (!CLAIM_OPS.has(op)) throw new Error('claim: op required — deploy|status|settle|deploySale|buySale')
      return await callTool(mem, `claim#${op}`, args, ctx)
    }
    case 'claim#deploy': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      const { Buffer } = await import('node:buffer')
      return await mem.claims.deploy(Buffer.from(args.lzkmem, 'hex').toString('hex'), {
        bondSats: args.bondSats,
        deadline: args.deadline,
        anchorOutpoint: args.anchorOutpoint
      })
    }
    case 'claim#status': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.status(args.txid, args.vout ?? 0)
    }
    case 'claim#settle': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.settle(args.txid, args.vout ?? 0, args.payoutSats ?? 4980, args.verdictSats ?? 10)
    }
    case 'claim#deploySale': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.deploySale(args.verdictOutpoint, args.priceSats, args.wrapped)
    }
    case 'claim#buySale': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.buyGated(args.saleTxid, args.verdictOutpoint, args.wrapped, args.priceSats, args.changeSats ?? 900)
    }
    case 'modelManifest':
      return mem.modelManifest(args)
    case 'groundedAnswer': {
      const { ground, commitReceipt } = await import('@nektar/reasoning')
      const { Buffer } = await import('node:buffer')
      const fetch: import('@nektar/reasoning').FragmentFetch = async (ref) => {
        const r = await mem.recall(ref.shard, { seq: ref.seq })
        const it = r.items[0]
        return { verified: !!it && !it.unverifiable, text: it?.text }
      }
      // private shards (remember …, private:true): a plain cite leaks the shard
      // name into the public receipt — the record must be cited via a ZK
      // membership proof. Caller may supply {zk:{queryId,proof}} from an
      // externally-issued zkQuery; otherwise the service AUTO-PROVES (self-issue
      // → prove → verify) — a real proof either way: a verifier re-runs it via
      // proofCommit. Prover absent/failed → flagged and stripped — fail-closed.
      const privRefused: any[] = []
      const claimsForGround: any[] = []
      const proofCommits = new Map<string, { commit: string; store?: { endpoint: string; bucket: string } }>() // `${shard}:${seq}` → proof commit + store locator
      for (const c of args.claims ?? []) {
        const priv = (c.cites ?? []).filter((r: any) => mem.shardPrivate(r.shard))
        if (priv.length === 0) {
          claimsForGround.push(c)
          continue
        }
        let zkok = true
        for (const r of priv) {
          const zk = r.zk ?? c.zk
          let bytes: Uint8Array
          if (typeof zk?.queryId === 'string' && typeof zk?.proof === 'string') {
            bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
          } else {
            try {
              const q = mem.issueZkQuery('nektar:auto-cite')
              bytes = (await mem.proveMembership(r.shard, r.seq, q.queryId)).bytes
            } catch {
              zkok = false
              break
            }
          }
          const v = await mem.verifyMembership(r.shard, r.seq, bytes)
          if (!v.ok) {
            zkok = false
            break
          }
          // the proof becomes a public artifact: committed in the receipt and
          // fetchable at /v1/proof — a verifier re-runs it, no trust in us.
          const pc = mem.putProof(bytes)
          proofCommits.set(`${r.shard}:${r.seq}`, { commit: pc, store: mem.proofStoreRef(pc) })
        }
        if (zkok) claimsForGround.push(c)
        else privRefused.push(c)
      }
      const report = await ground(claimsForGround, fetch, { mode: args.mode === 'flag' ? 'flag' : 'remove' })
      for (const c of privRefused)
        report.flagged.push({
          ...c,
          grounded: false,
          reason:
            'cite targets a private shard — the ZK membership proof could not be produced (record unanchored or prover unavailable); supply {zk:{queryId,proof}} to cite explicitly'
        })
      const answer = report.kept.map((c) => c.text).join(' ')
      if (report.kept.length === 0) {
        return {
          answer: '',
          committed: false,
          kept: [],
          flagged: report.flagged,
          grounding: report,
          checkResult: 'failed',
          note: 'no claim survived grounding — nothing committed (an underivable answer is invalid by construction)'
        }
      }
      // the receipt commits only cites of KEPT claims — and private cites are
      // redacted to {private, hdp, anchorTxid}: the shard name never enters
      // the anchored record.
      const refs: any[] = []
      for (const c of report.kept)
        for (const r of c.cites ?? []) {
          refs.push(
            mem.shardPrivate(r.shard)
              ? {
                  private: true,
                  proofCommit: proofCommits.get(`${r.shard}:${r.seq}`)?.commit,
                  ...(proofCommits.get(`${r.shard}:${r.seq}`)?.store !== undefined ? { proofStore: proofCommits.get(`${r.shard}:${r.seq}`)!.store } : {}),
                  ...((await mem.fragmentRef(r.shard, r.seq)) ?? { hdp: '' })
                }
              : r
          )
        }
      // modelAttestation (from attestInference): the statement MUST re-derive
      // from (input‖output‖modelPoseidon) before it enters the anchored record —
      // fail closed, never commit an unverifiable attestation.
      let attestation = args.modelAttestation
      if (args.attestInference !== undefined) {
        if (attestation !== undefined) throw new Error('pass attestInference OR modelAttestation, not both')
        const { attestInference } = await import('./inference.js')
        attestation = (await attestInference(mem, args.attestInference ?? {})).attestation
      }
      if (attestation !== undefined) {
        const { validateAttestation } = await import('@nektar/zk')
        const v = validateAttestation(attestation)
        if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
      }
      const { receipt, receiptHash, anchor } = await commitReceipt(
        {
          answer,
          fragmentRefs: refs,
          checkResult: 'verified',
          timestamp: Date.now(),
          modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
          groundingReportHash: report.reportHash,
          ...(attestation ? { modelAttestation: attestation } : {}),
          ...(paid ? { payment: paid } : {}),
          ...(typeof args?.caller === 'string' ? { caller: args.caller } : {}),
          ...(typeof args?.delegationId === 'string' ? { delegationId: args.delegationId } : {}),
          ...(typeof (args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST')) === 'string'
            ? { manifestId: args?.manifestId ?? nektarEnv('NEKTAR_LAST_MANIFEST') }
            : {})
        },
        mem
      )
      const token = receiptToken(receiptHash)
      return {
        answer,
        committed: true,
        kept: report.kept,
        flagged: report.flagged,
        grounding: report,
        receipt,
        receiptHash,
        receiptToken: token,
        verifyUrl: `${verifyBase()}/v1/verify?r=${token}`,
        share: shareBlock(token),
        anchor,
        checkResult: 'verified'
      }
    }
    case 'commitReceipt': {
      const { commitReceipt } = await import('@nektar/reasoning')
      const { Buffer } = await import('node:buffer')
      // private shards: same fail-closed rule as groundedAnswer — a private
      // cite needs a ZK membership proof the service verifies, then commits
      // redacted with the proof's sha256 so a verifier can re-run it.
      const refs: any[] = []
      for (const r of args.fragmentRefs ?? []) {
        if (!mem.shardPrivate(r.shard)) {
          refs.push(r)
          continue
        }
        const zk = r.zk
        let bytes: Uint8Array
        if (typeof zk?.queryId === 'string' && typeof zk?.proof === 'string') {
          bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
        } else {
          // auto-prove: self-issue → prove → verify (same artifact, no caller choreography)
          let q: { queryId: string }
          try {
            q = mem.issueZkQuery('nektar:auto-cite')
            bytes = (await mem.proveMembership(r.shard, r.seq, q.queryId)).bytes
          } catch (e) {
            throw new Error(`private cite for "${r.shard}" could not be auto-proven (${(e as Error).message}) — supply zk:{queryId,proof} to cite explicitly`, {
              cause: e
            })
          }
        }
        const v = await mem.verifyMembership(r.shard, r.seq, bytes)
        if (!v.ok) throw new Error(`ZK membership proof for private cite failed: ${v.reason}`)
        const pc = mem.putProof(bytes)
        refs.push({
          private: true,
          proofCommit: pc,
          ...(mem.proofStoreRef(pc) !== undefined ? { proofStore: mem.proofStoreRef(pc) } : {}),
          ...((await mem.fragmentRef(r.shard, r.seq)) ?? { hdp: '' })
        })
      }
      let attestation = args.modelAttestation
      if (args.attestInference !== undefined) {
        if (attestation !== undefined) throw new Error('pass attestInference OR modelAttestation, not both')
        const { attestInference } = await import('./inference.js')
        attestation = (await attestInference(mem, args.attestInference ?? {})).attestation
      }
      if (attestation !== undefined) {
        const { validateAttestation } = await import('@nektar/zk')
        const v = validateAttestation(attestation)
        if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
      }
      const out = await commitReceipt(
        {
          answer: args.answer,
          fragmentRefs: refs,
          checkResult: args.checkResult,
          timestamp: Date.now(),
          modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
          groundingReportHash: args.groundingReportHash,
          ...(attestation ? { modelAttestation: attestation } : {}),
          ...(paid ? { payment: paid } : {})
        },
        mem
      )
      const token = receiptToken(out.receiptHash)
      return { ...out, receiptToken: token, verifyUrl: `${verifyBase()}/v1/verify?r=${token}`, share: shareBlock(token) }
    }
    case 'attestInference': {
      const { attestInference } = await import('./inference.js')
      return await attestInference(mem, args)
    }
    case 'verifyReceipt': {
      const { verifyReceipt } = await import('./receipt.js')
      return await verifyReceipt(mem, args)
    }
    case 'fundingStatus': {
      // prefer the runtime's live wallet view (spend caps, runway, correct
      // net prefix); fall back to the reserve mem tracks itself
      if (ctx.ops?.wallet) {
        const w = (await ctx.ops.wallet()) as Record<string, unknown>
        return { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
      }
      return await mem.fundingStatus()
    }
    case 'fundingImport': {
      if (!ctx.ops?.importFunding) throw new Error('fundingImport: live network only (regtest funds via the test harness)')
      if (typeof args?.txid !== 'string' || args.txid.length < 8) throw new Error('fundingImport: txid required')
      return await ctx.ops.importFunding(args.txid)
    }
    case 'fundingRefresh': {
      if (!ctx.ops?.refreshFunding) throw new Error('fundingRefresh: live network only')
      return await ctx.ops.refreshFunding()
    }
    case 'exportShard': {
      if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('exportShard: shard required')
      const pack = await mem.exportShardPack(args.shard)
      // the serve endpoint makes the pack a shareable URL — an invite the
      // agent can hand to another operator ('importShard from this URL').
      // The store ref is preferred: it resolves origin-free and re-stores
      // when the shard changed since the last PUT (the ref pins sha256).
      const store = await mem.storePack(args.shard, pack).catch(() => undefined)
      return {
        ...pack,
        ...(store !== undefined ? { store } : {}),
        packUrl: `${verifyBase()}/v1/pack/${encodeURIComponent(args.shard)}`
      }
    }
    case 'importShard': {
      let pack = args?.pack
      // object-store ref — fetch through getVerified (digest-checked) before
      // the pack's own re-verification decides what lands
      if (pack === undefined && typeof args?.store === 'object' && args.store !== null) {
        const r = args.store as { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string }
        const keyed = typeof r.key === 'string' && typeof r.sha256 === 'string'
        if (!keyed && typeof r.chirp !== 'string') throw new Error('importShard: store ref needs {key, sha256} or {chirp} (bucket/size/endpoint optional)')
        if (r.endpoint !== undefined && !/^https?:\/\//i.test(r.endpoint)) throw new Error('importShard: store endpoint must be http(s)')
        pack = await mem.fetchStoredPack(r)
      }
      if (pack === undefined) {
        const url =
          typeof args?.packUrl === 'string'
            ? args.packUrl
            : typeof args?.from === 'string' && typeof args?.shard === 'string'
              ? `${args.from.replace(/\/+$/, '')}/v1/pack/${encodeURIComponent(args.shard)}`
              : undefined
        if (!url) throw new Error('importShard: give {pack}, {from,shard}, {packUrl}, or {store}')
        if (!/^https?:\/\//i.test(url)) throw new Error('importShard: pack URL must be http(s)')
        const r = await fetch(url, { signal: AbortSignal.timeout(15_000) })
        if (!r.ok) throw new Error(`importShard: fetch ${url} → ${r.status}`)
        pack = await r.json()
      }
      if (typeof pack !== 'object' || pack === null) throw new Error('importShard: pack object required (ShardPack JSON from exportShard or a pack URL)')
      return await mem.importShardPack(pack)
    }
    case 'evidence': {
      // stored trade-evidence bundles — operator profile only. Fetches the
      // bundle through getVerified (digest/size checked against the ref) and
      // runs the evidence verifier; redacted bundles report which checks the
      // withheld question/disclosure fields make unavailable.
      const { objectStoreFromEnv } = await import('@nektar/store')
      const { fetchObjectByRef } = await import('@nektar/store')
      const { verifyTradeEvidence } = await import('@nektar/automation')
      const st = mem.objectStore ?? mem.automation?.objectStore ?? objectStoreFromEnv()
      let ref = args?.ref as { bucket?: string; key?: string; sha256?: string; size?: number; chirp?: string; endpoint?: string } | undefined
      if (ref === undefined && typeof args?.operationId === 'string') {
        const au = mem.automation
        if (!au) throw new Error('evidence: operationId lookup needs automation (NEKTAR_AUTOMATION=1)')
        ref = (await au.store.operation(args.operationId))?.data.evidenceRef as typeof ref
        if (ref === undefined) throw new Error(`evidence: no stored evidence for ${args.operationId}`)
      }
      if (ref === undefined) throw new Error('evidence: give {ref} or {operationId}')
      // the shared store-ref chain: chirp signed → chirp unsigned → keyed
      // signed → keyed unsigned — evidence resolves with no credentials when
      // the seller's gateway serves public reads
      const bytes = await fetchObjectByRef(st, ref)
      return verifyTradeEvidence(JSON.parse(new TextDecoder().decode(bytes)))
    }
    case 'doctor': {
      const d = await mem.doctor()
      if (ctx.ops?.wallet) {
        const w = (await ctx.ops.wallet()) as Record<string, unknown>
        d.funding = { ...w, funded: Number(w.balanceSats ?? 0) > 0 }
      }
      // B3 — bootstrap state: 'sealed' = the agent publishes its bfinger
      // record and is discoverable; 'unsealed' = it still needs a configured
      // host set for counterparties to reach it
      const out: Record<string, unknown> = { ...d, bootstrap: mem.automation?.bootstrap?.() ?? 'unsealed' }
      if (args?.fix) return { ...out, repair: await mem.doctorRepair() }
      return out
    }
    case 'describeShard': {
      if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('describeShard: shard required')
      if (!Array.isArray(args?.domains)) throw new Error('describeShard: domains[] required — the tags peers search by')
      return await mem.describeShard(args.shard, { domains: args.domains, desc: args.desc, offers: args.offers })
    }
    case 'importRecord': {
      // land a record bought via sale#claim — caller passes the verified
      // claimed payload; lineage (from shard:seq, leaf, settlement txid)
      // commits on __imports__
      if (typeof args?.data !== 'string') throw new Error('importRecord: data required (the verified claim payload)')
      return await mem.importRecord({
        data: args.data,
        provenance: args.provenance,
        sourceShard: args.sourceShard,
        sourceSeq: args.sourceSeq,
        leaf: args.leaf,
        saleTxid: args.saleTxid,
        shard: args.shard
      })
    }
    case 'want': {
      const au = mem.automation
      if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
      const ph = await singlePolicy(au, 'buyer', 'want')
      const r = await au.engine.publishWant(
        {
          domains: (args?.domains ?? []).map(String),
          ...(args?.question ? { question: String(args.question).slice(0, 560) } : {}),
          maxSats: Number(args?.maxSats)
        },
        ph
      )
      void au.cycle()
      return { wantId: r.wantId, operationId: r.operationId }
    }
    case 'sell': {
      const au = mem.automation
      if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
      if (typeof args?.shard !== 'string' || args.shard.length === 0) throw new Error('sell: shard required')
      if (!Array.isArray(args?.domains)) throw new Error('sell: domains[] required')
      const priceSats = Number(args?.priceSats)
      if (!Number.isSafeInteger(priceSats) || priceSats <= 0) throw new Error('sell: priceSats must be a positive integer')
      await mem.describeShard(args.shard, { domains: args.domains, desc: args.desc, offers: [{ kind: 'record', priceSats }] })
      const ph = await singlePolicy(au, 'seller', 'sell')
      await au.engine.enableSeller(ph)
      void au.cycle()
      return { shard: args.shard, domains: args.domains, priceSats, policyHash: ph }
    }
    case 'automationDoctor': {
      const au = mem.automation
      if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
      return await au.preflight()
    }
    case 'deals': {
      const au = mem.automation
      if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
      const entry = (o: import('@nektar/automation').Operation): Record<string, unknown> => {
        const d = o.data
        const selKey = typeof (d.selected as { key?: string } | undefined)?.key === 'string' ? (d.selected as { key: string }).key : undefined
        return {
          operationId: o.operationId,
          role: o.role,
          kind: o.kind,
          state: o.state,
          wantId: (d.want as { want?: { wantId?: string } } | undefined)?.want?.wantId,
          priceSats: (d.amounts as { purchaseSats?: number } | undefined)?.purchaseSats ?? d.priceSats,
          counterparty: o.role === 'seller' ? (d.want as { want?: { pub?: string } } | undefined)?.want?.pub : selKey?.split(':')[0],
          escrowTxid: d.escrowTxid,
          disclosureTxid: (d.result as { disclosureTxid?: string } | undefined)?.disclosureTxid,
          refundTxid: d.refundTxid,
          ...(d.evidenceRef !== undefined ? { evidence: { ref: d.evidenceRef, stored: true } } : {}),
          updatedAt: o.updatedAt
        }
      }
      const detail = (o: import('@nektar/automation').Operation, events: unknown): Record<string, unknown> => {
        const e: Record<string, unknown> = { ...entry(o), data: o.data, events, ...(o.lastError ? { lastError: o.lastError } : {}) }
        if (o.role === 'buyer' && (o.state === 'disclosure_verified' || o.state === 'useful')) {
          const r = o.data.result as { text?: string; taint?: string } | undefined
          // D4 — the taint tag rides the consumer surface: purchased content
          // reaches the caller TAGGED, never bare
          if (r?.text !== undefined) e.result = { text: r.text, taint: r.taint ?? 'untrusted-purchased-content' }
        }
        return e
      }
      if (args?.operationId) {
        const st = await au.engine.status(String(args.operationId))
        if (!st) throw new Error(`deals: unknown operationId ${args.operationId}`)
        const e = detail(st.operation, st.events)
        const j = await au.dealJournal?.(st.operation)
        if (j !== undefined) e.journal = j
        return e
      }
      const ops = (await au.store.operations())
        .filter((o) => o.kind === 'purchase' || o.kind === 'sale')
        .sort((a, b) => b.updatedAt - a.updatedAt)
        .slice(0, 50)
        .map(entry)
      return { deals: ops }
    }
    case 'fund': {
      const au = mem.automation
      if (!au) throw new Error('automation is off (NEKTAR_AUTOMATION=1)')
      const w = ctx.ops?.wallet ? ((await ctx.ops.wallet()) as Record<string, unknown>) : ((await mem.fundingStatus()) as unknown as Record<string, unknown>)
      const slots = await au.marketSlots()
      const live = (await au.store.policies()).filter((x) => x.revokedAt === undefined)
      return {
        address: w.address,
        network: au.engine.network,
        spendableSats: Number(w.balanceSats ?? 0),
        utxos: Number(w.utxos ?? 0),
        marketSlots: slots,
        ...(live.length === 1 ? { budget: await au.store.budgetStatus(live[0].policyHash) } : {})
      }
    }
    case 'automation': {
      const au = mem.automation
      if (!au) throw new Error('automation: not enabled — start the instance with NEKTAR_AUTOMATION=1')
      const op = String(args?.op ?? '')
      if (op === 'publishWant') {
        const r = await au.engine.publishWant(
          {
            domains: (args?.domains ?? []).map(String),
            ...(args?.question ? { question: String(args.question).slice(0, 560) } : {}),
            maxSats: Number(args?.maxSats),
            ...(args?.wantId ? { wantId: String(args.wantId) } : {})
          },
          String(args?.policyHash ?? '')
        )
        void au.cycle()
        return { ok: true, ...r }
      }
      if (op === 'enableSeller') {
        await au.engine.enableSeller(String(args?.policyHash ?? ''))
        void au.cycle()
        return { ok: true }
      }
      if (op === 'status') {
        const st = await au.engine.status(String(args?.operationId ?? ''))
        if (!st) throw new Error('automation: unknown operationId')
        return st
      }
      if (op === 'operations')
        return {
          operations: (await au.store.operations()).map((o) => ({
            operationId: o.operationId,
            role: o.role,
            kind: o.kind,
            state: o.state,
            updatedAt: o.updatedAt,
            ...(o.lastError ? { lastError: o.lastError } : {})
          }))
        }
      if (op === 'approvals') return { approvals: await au.store.approvals('pending') }
      if (op === 'budget') return await au.store.budgetStatus(String(args?.policyHash ?? ''))
      if (op === 'policies') return { policies: await au.store.policies() }
      throw new Error('automation: op required — publishWant|enableSeller|status|operations|approvals|budget|policies')
    }
    default:
      throw new Error(`unknown tool ${name}`)
  }
}

/** MCP over stdio lives in mcp-server.ts (it owns the SDK wiring — server.ts
 *  stays dependency-free of it; bin.ts + index.ts import from there) */
