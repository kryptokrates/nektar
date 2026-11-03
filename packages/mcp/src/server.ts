// Tool definitions (JSON Schema) + their single implementation (callTool),
// shared by the MCP server (mcp-server.ts, official SDK) and the HTTP API.
import { sha256, utf8 } from '@mamem/core'
import { receiptToken } from './receipt.js'
import { PAID_TOOLS, paymentsEnabled, paymentRequired, priceFor, settlePayment } from './pay.js'
import type { MamemMemory } from './memory.js'

/** public base of /v1/verify for footers/QRs — MAMEM_VERIFY_BASE for hosted
 *  deployments (https://host or ngrok), else the local HTTP port. */
const verifyBase = (): string => process.env.MAMEM_VERIFY_BASE ?? `http://localhost:${process.env.MAMEM_HTTP_PORT ?? 8080}`

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
        private: { type: 'boolean', description: 'declare this shard PRIVATE (monotone — cannot be unset): records on it refuse plain merkle citations (prove/groundedAnswer fragmentRefs) and may only be cited via a ZK membership proof; the shard name is withheld from receipts' },
        premiseRefs: { type: 'array', items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] }, description: '§AppE: records this fact is derived from — the new level is floored at their max so derived content cannot leak downward' },
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
    description: 'Force-close the pending batch of a shard and broadcast the anchor tx. With MAMEM_PAYMENTS=1 this costs sats — call payQuote, then pass payment.',
    inputSchema: { type: 'object', properties: { shard: { type: 'string' }, payment: { type: 'object', description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote', properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } } } }, required: ['shard'] }
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
    name: 'verifyCombined',
    description: 'Verifier side (Wright §13 Π_j = (π_j, ZKAccess)): verify the DUAL commitment as one object — the L_ZKMem membership STARK, the ZKAccess clearance and (optionally) an L_ZKDer derivation STARK must all carry the SAME verifier-issued queryId and the clearance transcript must carry the issued nonce. Rejects proofs paired with an unrelated clearance.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' },
        seq: { type: 'number', description: 'locates the batch anchor (the proof itself does not reveal the record)' },
        bytes: { type: 'string', description: 'L_ZKMem v2 hex from proveMembership' },
        zk: { type: 'object', description: 'clearance presentation bound to the same queryId — {commitment, proof, transcript:{queryId,fragmentId,nonce(hex)}, requiredLevel}' },
        scope: { type: 'string', description: 'transcript fragmentId to enforce (default shard/seq)' },
        derivationBytes: { type: 'string', description: 'optional L_ZKDer v3 hex bound to the same queryId' },
        callerPubkey: { type: 'string', description: 'optional: require the clearance holder to be this pubkey (else artifact-validity only)' }
      },
      required: ['shard', 'seq', 'bytes', 'zk']
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
  ,
  {
    name: 'saleOffer',
    description: 'V3 DisclosureSale (GOAL-v7, BIP-199 escrow): wrap a value to a buyer pubkey (BRC-42) and produce the seller offer {sellerPkh,price,hWrapped,refundAfter,maxFee} + committed wrapped hex. The BUYER then funds the escrow via saleEscrow. Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        value: { type: 'string', description: 'hex payload (envelope wrapped bytes or a {field,salt,path} JSON)' },
        price: { type: 'number' }, refundAfter: { type: 'number' }, maxFee: { type: 'number' },
        buyerPub: { type: 'string', description: 'recipient pubkey — omit for public disclosure' },
        expiresAt: { type: 'number', description: 'optional block height at which the signed offer lapses' }
      },
      required: ['value', 'price', 'refundAfter', 'maxFee']
    }
  },
  {
    name: 'saleOfferMemory',
    description: 'V3 DisclosureSale: sell one memory record — the payload is the record\'s canonical serialized fragment; the buyer recomputes its leaf and checks it against the anchored batch (commitment returned for a pre-funding check). Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        shard: { type: 'string' }, seq: { type: 'number' },
        price: { type: 'number' }, refundAfter: { type: 'number' }, maxFee: { type: 'number' },
        buyerPub: { type: 'string', description: 'recipient pubkey — omit for public disclosure' },
        expiresAt: { type: 'number', description: 'optional block height at which the signed offer lapses' }
      },
      required: ['shard', 'seq', 'price', 'refundAfter', 'maxFee']
    }
  },
  {
    name: 'saleEscrow',
    description: 'V3 DisclosureSale (buyer side): fund the escrow for a seller\'s cfg — deposits `amount` sats into DisclosureSale(cfg, depositorPkh=me) from the operator key. Pass `offer` (saleOffer result + commitment) to verify the seller\'s signed offer and the leaf\'s inclusion path BEFORE funding. Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        cfg: { type: 'object', description: 'the seller cfg returned by saleOffer' },
        amount: { type: 'number', description: 'escrow deposit — should be price + maxFee + change margin' },
        offer: { type: 'object', description: 'optional {sellerPub, offerSig, commitment} — verified pre-funding (sig + leaf path)' }
      },
      required: ['cfg', 'amount']
    }
  },
  {
    name: 'saleComplete',
    description: 'V3 DisclosureSale (seller side): complete a sale on a buyer-funded escrow — spend it via buy() revealing `wrapped` on-chain; the same tx pays the seller exactly `price` and returns change to the depositor. Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        cfg: { type: 'object', description: 'the full escrow cfg (offer fields + depositorPkh from the escrow announcement)' },
        utxo: { type: 'object', description: '{txid, vout, satoshis, script} of the funded escrow' },
        wrapped: { type: 'string', description: 'the committed wrapped hex (sha256(wrapped) must equal cfg.hWrapped)' },
        changeSats: { type: 'number' }
      },
      required: ['cfg', 'utxo', 'wrapped', 'changeSats']
    }
  },
  {
    name: 'saleRefund',
    description: 'V3 DisclosureSale (buyer side): reclaim an unrevealed escrow after cfg.refundAfter — depositor-signed, locktime-gated. Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        cfg: { type: 'object' }, utxo: { type: 'object' },
        lockTime: { type: 'number' }, changeSats: { type: 'number' }
      },
      required: ['cfg', 'utxo', 'lockTime', 'changeSats']
    }
  },
  {
    name: 'saleClaim',
    description: 'V3 DisclosureSale (buyer side): extract `wrapped` from a buy tx, verify sha256(wrapped)==hWrapped, unwrap to the operator key (private) or return the public value; for memory payloads also verifies the leaf commitment when `commitment` is given. Requires MAMEM_PAID_DISCLOSURE.',
    inputSchema: {
      type: 'object',
      properties: {
        txHex: { type: 'string' }, hWrapped: { type: 'string' },
        sellerPub: { type: 'string', description: 'needed to unwrap private (BRC-42) payloads' },
        offerMode: { type: 'string', description: '"key" for encrypted-record key sales, "fragment" or omit for plaintext fragment sales' },
        commitment: { type: 'object', description: 'optional {leaf, hdp, authorPub, anchorTxid} from saleOfferMemory — verifies leaf inclusion + provenance σ' }
      },
      required: ['txHex', 'hWrapped']
    }
  },
  {
    name: 'claimDeploy',
    description: 'Optimistic-claim layer (§4.5): deploy a MamemZkClaim covenant bound to an L_ZKMem proof bundle — the journal root/size/queryId are committed on-chain behind the prover\'s bond. Registered for the watcher, which re-verifies the RISC Zero receipt off-chain and auto-challenges fraud. Regtest only.',
    inputSchema: {
      type: 'object',
      properties: {
        lzkmem: { type: 'string', description: 'L_ZKMem v2 hex from proveMembership' },
        bondSats: { type: 'number' }, deadline: { type: 'number', description: 'locktime when the claim becomes settleable; 0 = immediately' },
        anchorOutpoint: { type: 'string', description: '36-byte anchor outpoint hex to bind journalRoot == anchor.R; omit for unanchored' }
      },
      required: ['lzkmem']
    }
  },
  {
    name: 'claimStatus',
    description: 'Read a deployed claim\'s covenant state: open | challenged | revealed | bisecting, deadline, journalRoot, bond.',
    inputSchema: { type: 'object', properties: { txid: { type: 'string' }, vout: { type: 'number' } }, required: ['txid'] }
  },
  {
    name: 'claimSettle',
    description: 'Settle an open claim after its deadline: emits the ClaimVerdict UTXO (spendable proof of acceptance) + bond payout. Unanchored claims only — anchored claims settle via the anchor co-spend.',
    inputSchema: { type: 'object', properties: { txid: { type: 'string' }, vout: { type: 'number' }, payoutSats: { type: 'number' }, verdictSats: { type: 'number' } }, required: ['txid'] }
  },
  {
    name: 'saleDeployGated',
    description: 'Deploy a DisclosureSale bound to a settled ClaimVerdict — the price can only be paid by a tx that also consumes the verdict (atomic pay-iff-settled). Regtest only.',
    inputSchema: {
      type: 'object',
      properties: {
        verdictOutpoint: { type: 'string', description: '36-byte outpoint hex from claimSettle' },
        priceSats: { type: 'number' }, wrapped: { type: 'string', description: 'payload to reveal on buy (hex)' }
      },
      required: ['verdictOutpoint', 'priceSats', 'wrapped']
    }
  },
  {
    name: 'saleBuyGated',
    description: 'Execute the gated buy: spends [sale, verdict] → pays the verdict\'s payee atomically. Consensus refuses without the verdict input.',
    inputSchema: {
      type: 'object',
      properties: {
        saleTxid: { type: 'string' }, verdictOutpoint: { type: 'string' }, wrapped: { type: 'string' }, priceSats: { type: 'number' }, changeSats: { type: 'number' }
      },
      required: ['saleTxid', 'verdictOutpoint', 'wrapped', 'priceSats']
    }
  },
  {
    name: 'modelManifest',
    description: 'AGT-011: declare + anchor which model this instance runs — local weights sha256 or API provider+release. Anchored on __model__ and bound into subsequent receipts. DECLARED, not proven: says which model the operator says answered, not that the inference ran those weights.',
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
    description: 'Wright §8 (∀φ∈Rq ∃Ki: VerifyFragment(Ki)∧φ⊢di): takes the caller\'s draft claims, checks every cite resolves to a VERIFIED fragment and the claim is quoted or Datalog-entailed, STRIPS everything that fails, then anchors an answer receipt. Ungrounded statements are removed — the receipt is the per-answer proof, not an optional add-on.',
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
              cites: { type: 'array', items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] } }
            },
            required: ['text', 'cites']
          }
        },
        mode: { type: 'string', description: '"remove" (default — strip ungrounded) or "flag"' },
        modelAttestation: { type: 'object', description: 'optional inference attestation (from attestInference): the statement is re-derived before being committed' },
        payment: { type: 'object', description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote', properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } } }
      },
      required: ['claims']
    }
  },
  {
    name: 'commitReceipt',
    description: 'AGT-012: commit an answer receipt {answer, fragmentRefs, checkResult} — hashed and anchored on __receipts__. Anchored "what I said" — proves the answer was committed, not that it is true.',
    inputSchema: {
      type: 'object',
      properties: {
        answer: { type: 'string' },
        fragmentRefs: { type: 'array', items: { type: 'object', properties: { shard: { type: 'string' }, seq: { type: 'number' } }, required: ['shard', 'seq'] } },
        checkResult: { type: 'string', description: 'verified | unverifiable | failed' },
        model: { type: 'object', description: 'optional ModelIdentity override; defaults to the manifest set by modelManifest' },
        modelAttestation: { type: 'object', description: 'optional inference attestation (from attestInference): the statement is re-derived before being committed' },
        groundingReportHash: { type: 'string' },
        payment: { type: 'object', description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote', properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } } }
      },
      required: ['answer', 'fragmentRefs', 'checkResult']
    }
  },
  {
    name: 'payQuote',
    description: 'Issue a payment invoice for a paid tool (MAMEM_PAYMENTS=1): returns the operator address, amount, and the OP_RETURN invoice tag. Pay it with a signed raw tx (output to payTo ≥ amount + OP_RETURN output carrying the tag), then retry the tool with payment:{invoiceId, transaction}. Settlement is atomic — the tx IS the payment.',
    inputSchema: {
      type: 'object',
      properties: { tool: { type: 'string', enum: ['anchor', 'groundedAnswer', 'commitReceipt'], description: 'the paid tool you intend to call' } },
      required: ['tool']
    }
  },
  {
    name: 'attestInference',
    description: 'Model-execution attestation (nChain ai_inference-style): re-derives the Groth16 public statement poseidon(input‖output‖poseidon(model)) for the shipped 784×16×10 quantized MLP and — when verifier artifacts + a funded chain exist — deploys the in-script verifier and spends it, so consensus itself checks the proof. Returns a modelAttestation record to embed in commitReceipt/groundedAnswer. Only the canned demo vector has a proof today; LLM inference stays declared-only.',
    inputSchema: {
      type: 'object',
      properties: {
        input: { type: 'array', items: { type: 'number' }, description: '784-element input — defaults to the demo vector' },
        output: { type: 'number', description: 'expected argmax — defaults to the demo vector (9)' },
        payment: { type: 'object', description: 'payment settlement for paid mode: {invoiceId, transaction:<signed raw tx hex>} — see payQuote', properties: { invoiceId: { type: 'string' }, transaction: { type: 'string' } } }
      }
    }
  },
  {
    name: 'verifyReceipt',
    description: 'Scan a receipt token (mamemr1.…) or receiptHash and run the traffic-light checks: the receipt exists on __receipts__, its body re-hashes, the originator signature verifies, it is anchored, and every cited fragment is anchored. Proves the answer was committed + derivable — never that it was true.',
    inputSchema: {
      type: 'object',
      properties: {
        token: { type: 'string', description: 'receipt token "mamemr1.<base64url hash>"' },
        receiptHash: { type: 'string', description: 'or the bare 64-hex receipt hash' }
      }
    }
  }
] as const

const PAID_SALE_TOOLS = new Set(['saleOffer', 'saleOfferMemory', 'saleEscrow', 'saleComplete', 'saleRefund', 'saleClaim'])
const paidSaleEnabled = () => /^(1|true|yes)$/i.test(process.env.MAMEM_PAID_DISCLOSURE ?? '')
/** tools visible to clients right now — paid-disclosure tools are gated by env */
export const visibleTools = () => TOOLS.filter(t => !PAID_SALE_TOOLS.has(t.name) || paidSaleEnabled())

const AUTOMATON_NOTE = 'On-chain proof that the transition (q, σ, q′) is in the committed δ and was authorized by the owner; it says nothing about whether the output content is true.'
const automatonView = (h: import('@mamem/automaton').AutomatonHead) =>
  ({ txid: h.tx.id('hex'), q: h.state.q, n: h.state.n, halted: h.state.halted, deltaRoot: h.state.deltaRoot, statement: AUTOMATON_NOTE })

export async function callTool (mem: MamemMemory, name: string, args: any): Promise<any> {
  if (PAID_SALE_TOOLS.has(name) && !paidSaleEnabled()) throw new Error(`${name}: paid disclosure is disabled — set MAMEM_PAID_DISCLOSURE=1`)
  // MAMEM_PAYMENTS=1 (BRC-166-style): anchor/groundedAnswer/commitReceipt need a
  // settled payment tx before they run — atomic, one tx per call, publicly
  // auditable. No payment → return the invoice (the 402 challenge) as data.
  let paid: import('./pay.js').PaymentRecord | undefined
  if (PAID_TOOLS.has(name) && paymentsEnabled()) {
    if (typeof args?.payment?.transaction !== 'string') {
      return { ok: false, error: `payment required — ${name} costs ${priceFor(name)} sats`, ...paymentRequired(name).required }
    }
    paid = await settlePayment(mem, name, args.payment)
  }
  switch (name) {
    case 'payQuote': {
      if (!PAID_TOOLS.has(String(args?.tool))) throw new Error(`payQuote: unknown or unpaid tool "${args?.tool}"`)
      if (!paymentsEnabled()) throw new Error('payments are disabled — set MAMEM_PAYMENTS=1 (and MAMEM_OPERATOR_WIF)')
      const { invoice, required } = paymentRequired(String(args.tool))
      return { invoice: { invoiceId: invoice.invoiceId, amount: invoice.amount, payTo: invoice.payTo, lockingScriptHex: invoice.lockingScriptHex, opReturn: invoice.opReturn, expires: new Date(invoice.expires).toISOString() }, paymentRequired: required }
    }
    case 'saleOffer': {
      const { offerDisclosure } = await import('@mamem/identity')
      const r = offerDisclosure({
        sellerKey: mem.operatorKey(), value: new Uint8Array(Buffer.from(args.value, 'hex')),
        price: BigInt(args.price), refundAfter: BigInt(args.refundAfter), maxFee: BigInt(args.maxFee),
        buyerPub: args.buyerPub, expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined
      })
      // JSON-RPC cannot carry BigInt — cfg crosses the wire as plain numbers
      return {
        cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
        wrapped: r.wrapped, sellerPub: r.sellerPub, expiresAt: Number(r.expiresAt ?? 0n), offerSig: r.offerSig
      }
    }
    case 'saleOfferMemory': {
      const { offerDisclosure, encodeKeySale } = await import('@mamem/identity')
      const { serializeFragment, computeLeaf, toHex } = await import('@mamem/core')
      const rec = (await mem.records(args.shard)).find(r => r.seq === args.seq)
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
      const r = offerDisclosure({
        sellerKey: mem.operatorKey(), value: payload,
        price: BigInt(args.price), refundAfter: BigInt(args.refundAfter), maxFee: BigInt(args.maxFee),
        buyerPub: args.buyerPub, leaf: leafHex,
        expiresAt: args.expiresAt !== undefined ? BigInt(args.expiresAt) : undefined
      })
      return {
        cfg: { ...r.cfg, price: Number(r.cfg.price), refundAfter: Number(r.cfg.refundAfter), maxFee: Number(r.cfg.maxFee) },
        wrapped: r.wrapped, sellerPub: r.sellerPub, expiresAt: Number(r.expiresAt ?? 0n), offerSig: r.offerSig, mode,
        commitment: {
          leaf: leafHex, anchorTxid: rec.anchorTxid ?? null,
          hdp: toHex(rec.hdp), shard: args.shard, seq: args.seq,
          authorPub: rec.authorPub ?? null, level: rec.level ?? 0,
          batchR: bundle?.anchor.R ?? null,
          proof: bundle ? { path: bundle.path, leafIndex: bundle.leafIndex, n: bundle.n, size: bundle.size } : null
        }
      }
    }
    case 'saleEscrow': {
      const { DisclosureSaleHost } = await import('@mamem/anchor')
      // §16/17 delegation-sig check: refuse to fund an escrow for a cfg the
      // seller never signed — blocks MITM cfg-swap griefing (deposit lockup).
      if (args.offer) {
        const { verifyOfferSig } = await import('@mamem/identity')
        const { reconstructRoot, toHex: th, fromHex: fh } = await import('@mamem/core')
        const buyerPub = mem.operatorKey().toPublicKey().toString()
        const leaf = args.offer.commitment?.leaf ?? ''
        if (!verifyOfferSig(
          { cfg: args.cfg, sellerPub: args.offer.sellerPub, offerSig: args.offer.offerSig, expiresAt: args.offer.expiresAt },
          buyerPub, leaf
        )) throw new Error('saleEscrow: offerSig does not verify — cfg may have been substituted')
        // π_i pre-funding check: if the commitment carries a path, the leaf
        // must rebuild the committed batch root — catch a bogus leaf early.
        const proof = args.offer.commitment?.proof
        if (proof?.path && args.offer.commitment?.batchR) {
          const root = reconstructRoot(fh(leaf), { index: proof.leafIndex, siblings: proof.path.map((h: string) => fh(h)) })
          if (th(root) !== args.offer.commitment.batchR) throw new Error('saleEscrow: leaf path does not reach the committed batch root')
        }
        // §19 horizon: refuse to fund an offer that has lapsed — a signed
        // offer is only good until expiresAt (0 = no expiry).
        const exp = BigInt(args.offer.expiresAt ?? 0)
        if (exp > 0n && BigInt(await mem.chainHeight()) >= exp) throw new Error(`saleEscrow: offer expired at height ${exp}`)
      }
      // §19 sanity: the escrow must not be instantly refundable — refundAfter
      // is a locktime gate (height < 500M ⇒ block height, else timestamp).
      {
        const ra = BigInt(args.cfg.refundAfter)
        const future = ra < 500_000_000n
          ? ra > BigInt(await mem.chainHeight())
          : ra > BigInt(Math.floor(Date.now() / 1000))
        if (!future) throw new Error('saleEscrow: cfg.refundAfter is not in the future — the escrow would be instantly refundable')
      }
      const depositorPkh = Buffer.from(mem.operatorKey().toPublicKey().toHash()).toString('hex')
      const host = new DisclosureSaleHost({
        ...args.cfg, depositorPkh,
        price: BigInt(args.cfg.price), refundAfter: BigInt(args.cfg.refundAfter), maxFee: BigInt(args.cfg.maxFee)
      })
      const funding = await mem.fundingOuts(args.amount + 500)
      const { tx, utxo } = await host.buildEscrow(funding, mem.operatorKey(), args.amount)
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      for (const inp of funding) mem.markSpentOut(inp.tx.id('hex'), inp.vout)
      if (tx.outputs[1]) mem.addOwnedOut(tx, 1) // the escrow change out
      return { accepted: true, txid: tx.id('hex'), utxo: { ...utxo, txid: tx.id('hex') }, escrow: { ...args.cfg, depositorPkh } }
    }
    case 'saleComplete': {
      const { DisclosureSaleHost } = await import('@mamem/anchor')
      const host = new DisclosureSaleHost({
        ...args.cfg,
        price: BigInt(args.cfg.price), refundAfter: BigInt(args.cfg.refundAfter), maxFee: BigInt(args.cfg.maxFee)
      })
      const { tx } = host.buildBuy(args.utxo, args.wrapped, BigInt(args.changeSats))
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      return { accepted: true, txid: tx.id('hex'), sellerSats: tx.outputs[0]!.satoshis }
    }
    case 'saleRefund': {
      const { DisclosureSaleHost } = await import('@mamem/anchor')
      const host = new DisclosureSaleHost({
        ...args.cfg,
        price: BigInt(args.cfg.price), refundAfter: BigInt(args.cfg.refundAfter), maxFee: BigInt(args.cfg.maxFee)
      })
      const { tx } = host.buildRefund(args.utxo, mem.operatorKey(), args.lockTime, BigInt(args.changeSats))
      const r = await mem.broadcastTx(tx)
      if (r.status !== 'success') return { accepted: false, reason: r.description }
      return { accepted: true, txid: tx.id('hex'), refundSats: tx.outputs[0]!.satoshis }
    }
    case 'saleClaim': {
      const { Transaction } = await import('@bsv/sdk')
      const { claimDisclosure } = await import('@mamem/identity')
      const tx = Transaction.fromHex(args.txHex)
      const r = claimDisclosure(tx, { hWrapped: args.hWrapped, buyerKey: mem.operatorKey(), sellerPub: args.sellerPub })
      const out: any = { mode: r.mode, valueHex: Buffer.from(r.value).toString('hex') }
      // memory-bound payload? verify the bought bytes hash to the anchored leaf
      if (args.commitment?.leaf) {
        const { parseFragment, computeLeaf, toHex, fromHex } = await import('@mamem/core')
        try {
          if (args.offerMode === 'key') {
            // key-sale: payload carries the fragment key — decrypt E, rebuild
            // the fragment, then the same leaf check proves inclusion.
            const { claimKeySale } = await import('@mamem/identity')
            const { verifyProvenance } = await import('@mamem/log')
            const { d, p, E } = claimKeySale(r.value)
            const frag = { d, p, hdp: args.commitment.hdp ? fromHex(args.commitment.hdp) : undefined, E }
            out.verified = toHex(computeLeaf(frag as any)) === args.commitment.leaf
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
              const { verifyProvenance } = await import('@mamem/log')
              out.authorVerified = verifyProvenance(args.commitment.authorPub, frag.d, frag.p)
            }
          }
          if (out.verified && args.commitment.anchorTxid) out.anchorTxid = args.commitment.anchorTxid
          if (!out.verified) out.reason = 'computeLeaf(payload) != committed leaf'
        } catch (e) { out.verified = false; out.reason = `payload verification failed: ${(e as Error).message}` }
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
      const { decodePresentation } = await import('@mamem/identity')
      const r = await mem.recall(args.shard, { seq: args.seq, identity: args.identity, zk: args.zk === undefined ? undefined : decodePresentation(args.zk) })
      // tell the model how to recover from a wrong seq instead of an empty answer
      if (args.seq !== undefined && r.items.length === 0) {
        const seqs = (await mem.records(args.shard)).map(x => x.seq)
        return { ...r, hint: seqs.length ? `no record with seq ${args.seq} in shard "${args.shard}"; existing seqs: ${seqs.join(', ')} — or omit seq to recall all` : `shard "${args.shard}" is empty` }
      }
      return { ...r, shardPrivate: mem.shardPrivate(args.shard) || undefined }
    }
    case 'prove':
      // fail-closed: a private shard's plain merkle citation is a leak —
      // the same verification goal is met by proveMembership (ZK).
      if (mem.shardPrivate(args.shard)) return { ok: false, reason: `shard "${args.shard}" is private — plain citations refused; cite via a ZK membership proof (zkQuery → proveMembership → verifyMembership)` }
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
      return { lzkmem: Buffer.from(bytes).toString('hex'), bytes: bytes.length, proveMs: ms, statement: 'ZK-STARK: some record is in the anchored SHA-256 batch tree; reveals no leaf, index or path' }
    }
    case 'verifyMembership': {
      const { Buffer } = await import('node:buffer')
      return mem.verifyMembership(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')))
    }
    case 'verifyCombined': {
      const { Buffer } = await import('node:buffer')
      const { decodePresentation } = await import('@mamem/identity')
      return mem.verifyCombined(args.shard, args.seq, new Uint8Array(Buffer.from(args.bytes, 'hex')), decodePresentation(args.zk), {
        scope: args.scope,
        derivationBytes: args.derivationBytes === undefined ? undefined : new Uint8Array(Buffer.from(args.derivationBytes, 'hex')),
        callerPubkey: args.callerPubkey
      })
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
    case 'claimDeploy': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      const { Buffer } = await import('node:buffer')
      return await mem.claims.deploy(Buffer.from(args.lzkmem, 'hex').toString('hex'), { bondSats: args.bondSats, deadline: args.deadline, anchorOutpoint: args.anchorOutpoint })
    }
    case 'claimStatus': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.status(args.txid, args.vout ?? 0)
    }
    case 'claimSettle': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.settle(args.txid, args.vout ?? 0, args.payoutSats ?? 4980, args.verdictSats ?? 10)
    }
    case 'saleDeployGated': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.deploySale(args.verdictOutpoint, args.priceSats, args.wrapped)
    }
    case 'saleBuyGated': {
      if (!mem.claims) throw new Error('claims: not available on this network (regtest only)')
      return await mem.claims.buyGated(args.saleTxid, args.verdictOutpoint, args.wrapped, args.priceSats, args.changeSats ?? 900)
    }
    case 'modelManifest':
      return mem.modelManifest(args)
    case 'groundedAnswer': {
      const { ground, commitReceipt } = await import('@mamem/reasoning')
      const { Buffer } = await import('node:buffer')
      const fetch: import('@mamem/reasoning').FragmentFetch = async (ref) => {
        const r = await mem.recall(ref.shard, { seq: ref.seq })
        const it = r.items[0]
        return { verified: !!it && !it.unverifiable, text: it?.text }
      }
      // private shards (remember …, private:true): a plain cite leaks the shard
      // name into the public receipt — the record must be cited via a ZK
      // membership proof ({shard, seq, zk:{queryId, proof}}); without one the
      // claim is flagged and stripped — fail-closed, the cite never leaves.
      const privRefused: any[] = []
      const claimsForGround: any[] = []
      const proofCommits = new Map<string, string>() // `${shard}:${seq}` → sha256(proof)
      for (const c of args.claims ?? []) {
        const priv = (c.cites ?? []).filter((r: any) => mem.shardPrivate(r.shard))
        if (priv.length === 0) { claimsForGround.push(c); continue }
        let zkok = true
        for (const r of priv) {
          const zk = r.zk ?? c.zk
          if (typeof zk?.queryId !== 'string' || typeof zk?.proof !== 'string') { zkok = false; break }
          const bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
          const v = await mem.verifyMembership(r.shard, r.seq, bytes)
          if (!v.ok) { zkok = false; break }
          // the proof becomes a public artifact: committed in the receipt and
          // fetchable at /v1/proof — a verifier re-runs it, no trust in us.
          proofCommits.set(`${r.shard}:${r.seq}`, mem.putProof(bytes))
        }
        if (zkok) claimsForGround.push(c)
        else privRefused.push(c)
      }
      const report = await ground(claimsForGround, fetch, { mode: args.mode === 'flag' ? 'flag' : 'remove' })
      for (const c of privRefused) report.flagged.push({ ...c, grounded: false, reason: 'cite targets a private shard — a ZK membership proof is required (zkQuery → proveMembership, then cite {shard,seq,zk:{queryId,proof}})' })
      const answer = report.kept.map(c => c.text).join(' ')
      if (report.kept.length === 0) {
        return { answer: '', committed: false, kept: [], flagged: report.flagged, grounding: report, checkResult: 'failed', note: 'no claim survived grounding — nothing committed (an underivable answer is invalid by construction)' }
      }
      // the receipt commits only cites of KEPT claims — and private cites are
      // redacted to {private, hdp, anchorTxid}: the shard name never enters
      // the anchored record.
      const refs: any[] = []
      for (const c of report.kept) for (const r of c.cites ?? []) {
        refs.push(mem.shardPrivate(r.shard) ? { private: true, proofCommit: proofCommits.get(`${r.shard}:${r.seq}`), ...(await mem.fragmentRef(r.shard, r.seq) ?? { hdp: '' }) } : r)
      }
      // modelAttestation (from attestInference): the statement MUST re-derive
      // from (input‖output‖modelPoseidon) before it enters the anchored record —
      // fail closed, never commit an unverifiable attestation.
      if (args.modelAttestation !== undefined) {
        const { validateAttestation } = await import('@mamem/zk')
        const v = validateAttestation(args.modelAttestation)
        if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
      }
      const { receipt, receiptHash, anchor } = await commitReceipt({
        answer,
        fragmentRefs: refs,
        checkResult: 'verified',
        timestamp: Date.now(),
        modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
        groundingReportHash: report.reportHash,
        ...(args.modelAttestation ? { modelAttestation: args.modelAttestation } : {}),
        ...(paid ? { payment: paid } : {})
      }, mem)
      const token = receiptToken(receiptHash)
      return { answer, committed: true, kept: report.kept, flagged: report.flagged, grounding: report, receipt, receiptHash, receiptToken: token, verifyUrl: `${verifyBase()}/v1/verify?r=${token}`, anchor, checkResult: 'verified' }
    }
    case 'commitReceipt': {
      const { commitReceipt } = await import('@mamem/reasoning')
      const { Buffer } = await import('node:buffer')
      // private shards: same fail-closed rule as groundedAnswer — a private
      // cite needs a ZK membership proof the service verifies, then commits
      // redacted with the proof's sha256 so a verifier can re-run it.
      const refs: any[] = []
      for (const r of args.fragmentRefs ?? []) {
        if (!mem.shardPrivate(r.shard)) { refs.push(r); continue }
        const zk = r.zk
        if (typeof zk?.queryId !== 'string' || typeof zk?.proof !== 'string') throw new Error(`fragmentRefs for private shard "${r.shard}" require zk:{queryId,proof} — plain cites are refused`)
        const bytes = new Uint8Array(Buffer.from(zk.proof, 'hex'))
        const v = await mem.verifyMembership(r.shard, r.seq, bytes)
        if (!v.ok) throw new Error(`ZK membership proof for private cite failed: ${v.reason}`)
        refs.push({ private: true, proofCommit: mem.putProof(bytes), ...(await mem.fragmentRef(r.shard, r.seq) ?? { hdp: '' }) })
      }
      if (args.modelAttestation !== undefined) {
        const { validateAttestation } = await import('@mamem/zk')
        const v = validateAttestation(args.modelAttestation)
        if (!v.ok) throw new Error(`modelAttestation rejected: ${v.reason}`)
      }
      const out = await commitReceipt({
        answer: args.answer,
        fragmentRefs: refs,
        checkResult: args.checkResult,
        timestamp: Date.now(),
        modelId: args.model ?? mem.declaredModel ?? { kind: 'api', provider: 'undeclared', release: 'undeclared' },
        groundingReportHash: args.groundingReportHash,
        ...(args.modelAttestation ? { modelAttestation: args.modelAttestation } : {}),
        ...(paid ? { payment: paid } : {})
      }, mem)
      const token = receiptToken(out.receiptHash)
      return { ...out, receiptToken: token, verifyUrl: `${verifyBase()}/v1/verify?r=${token}` }
    }
    case 'attestInference': {
      const { attestInference } = await import('./inference.js')
      return await attestInference(mem, args)
    }
    case 'verifyReceipt': {
      const { verifyReceipt } = await import('./receipt.js')
      return await verifyReceipt(mem, args)
    }
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
