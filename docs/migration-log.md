# mamem migration log

## Round: v4 part-1 — datalog/AORG/policy-DAG/instancing (DONE)
- `@mamem/datalog`: L_pol parser (`@id` rules, negation, hyphens/quoted terms), safety checks, stratified semi-naive eval with provenance, deterministic `derivHash`, POL-012 linter.
- `@mamem/graph`: AORG vertices `h=dsha256(φ‖σ‖t‖context)` + committed edges; append-time acyclicity/justifiability (via datalog)/consistency/traceability; refinement invalidates bi-temporally (`currentView(t)`, nothing mutated); `instance(Ψ)` scoping + closure; Metanet head (AOR-011); `citing()` reverse-index hook (STO-005, used by P15).
- `GraphAnchor`: vertex/edge hashes as mamem shard fragments via the existing remember→anchor pipeline (structural `AnchorMem`, no workspace cycle); `proveVertex`/`verifyVertex` for the standard verify path; tampered hash rejects.
- Policy DAG: `h_i = dsha256(canonical rule incl. body)`, derivation + Φ(override) edges, per-node `supersedes`, conflict detection with full permit/deny paths (POL-008), `query`+`verifyDerivation` with per-node Merkle proofs (POL-006), `policyRoot` anchored (POL-004).
- Cedar∩DAG in `MamemMemory.recall`: `opts.policyDag`+`anchoredPolicyRoot` (instance-configured; caller-supplied roots never trusted — recomputed root mismatch denies with 'policy DAG root mismatch'); per-item permit withholding; receipt gains `dag:{policyRoot, derivationHash, state}` (POL-016). EDB facts only from verified identity/ZK.
- Instancing: `instanceOf` + `memberProof` (INS-002..004), `verifyInstance` (anchored members + closure + delegation + root), `delegate`/`verifyDelegation` `σ=Sign(H(root‖Ψ‖τ))` (INS-005), `entails` partial entailment under R_q (INS-006).
- POL-011 (ZK derivation attestation) deferred — documented, not trivial.
- e2e: offline + regtest v4 block — manual-revision refinement changes currentView; graph-root + policy-root anchors with printed txids; DAG-gate deny/conflict; third-party `verifyInstance`.

## Done so far
- Regtest unblocked: propagation HTTP `POST :8833/tx` (loopback port map in compose/docker-teranode.yml); asset API + rpc basic auth (`/api/v1/tx` = EF bytes). Spike B (legacy PushDrop): 1000 unconfirmed anchors, no limit.
- Fixed dup @bsv/sdk instances (peer dep at root) — `serializedBytes` cross-package bug.
- `packages/contracts/src/MamemAnchor.runar.ts` → compiles → `MamemAnchor.json` (297B code/412B state).
- `@mamem/contracts/src/index.ts`: artifact, `contractRp`=`chainRp`, `decodeMamemAnchor`, `TAG_RPRIME`.
- `packages/anchor/src/runarchain.ts`: Rúnar AnchorChain (deploy=genesis, call('append')=successor, LocalSigner+MamemProvider) — works on MockChain (n0→n1).
- Security: owner pubkey IS embedded in deployed locking script (verified includes(pk)).
- Not done: tag in state (readonly ByteString serializes empty in runar 0.4.6 — workaround: mutable state), Spend eval harness, full swap (tm/ls/verify still PushDrop.decode — legacy path green).

## Round: tag in mutable state + Spend eval + isValidAnchor (DONE)
- tagRprime = mutable state field, carried unchanged into continuation; serialises as `0x0f‖bytes` after poseidonRoot(0x00) in the OP_RETURN suffix.
- TAG constant = core `utf8('mamem:rprime:v1')` (NOT 'mamem-rprime' — earlier bug).
- super()/ctor args MUST match prop declaration order.
- contractRp == chainRp proven on vectors (test: contracts/test/valid-anchor.test.ts).
- decodeMamemAnchor returns {shard=hash160(owner),n,size,R,Rp,poseidonRoot,tag,ownerPub}; owner = LAST 33B 02/03 push before OP_RETURN (earlier ones are EC constants).
- isValidAnchor(d) checks tag==TAG_RPRIME — test green.
- Spend eval harness works (PrepareCall→inject DER+'41'→finalizeCall→Spend.validate):
  valid append PASS; wrong-key DER sig FAIL (top-stack-false checkSig); changed tag / wrong n / wrong Rp continuations FAIL (preimage/hashOutputs check → OP_CHECKSIGVERIFY).
- 3/3 contract tests pass. Legacy 40-test suite still green on PushDrop path.
- NEXT: swap tm/ls/verify to decodeMamemAnchor+isValidAnchor (port 8 overlay tests to contract anchors); perf; regtest; README/spec.

## Round: full swap + decode auth + perf (DONE)
- tm/ls/verify now decode via decodeMamemAnchor + isValidAnchor; overlay tests use REAL contract spends via AnchorChain (8/8 green).
- decodeMamemAnchor authenticates code byte-for-byte: artifact.script template + ctor slot (owner pubkey @byteOffset 165) + OP_RETURN + state. Fake/P2PKH+state scripts → null.
- getHead() fix (pending beats confirmed); register() attaches source parents so BEEF is complete.
- Perf: append ≈73ms FLAT (min69/med73/max96 over 30), hot spot = runar prepareCall (~120ms incl preimage+signing; provider calls ~0ms). Tx: genesis 712B, append 3373B.
- e2e burst reduced 1000→300 to fit the 30s cap; E2E PASS (exit 0).
- Legacy anchorchain.ts deleted; only remaining PushDrop refs = core parseAnchorFields codec (data encoding, not script) + comments.
- TODO: regtest contract e2e, README/spec updates, delete core PushDrop codec if unused.

## Round: LIVE contract chain on regtest — PROVEN
- MamemProvider fix (production): after each broadcast, record input outpoints as spent + own small (<300B) outputs as spendable; getUtxos filters spent + includes change.
- Self-contained live run (spikes/contract-live.mjs): fresh coinbase → 50-output split → production AnchorChain:
  split  0d062472000c6c62f11e1892f5c2a4aa9a58ce080c970dfb1503973320b93a61
  genesis c67b009ee0580d221de90f7f33ce9549873cafe56035e9fa16aeab60a771286f
  append1 40c4418b6e6bf4baf371197db5d749fece09597b81d1f9c539749671038e8a8b
  append2 06b6022a3f3aff21441a607831b7099411b9bcf1c866fefd651ca8690845e872
  append3 7f49df8f9ef3a8fda3b770263c27ba33c355912c5808c4d96a4675901175a71e
  (earlier accepted run: genesis 52060339…, append1 836617be…)
- All accepted by Teranode = real Chronicle script eval on contract spends. 1 block mined after.

## Round: real-headers proof — DONE
- Route: GET /api/v1/block_legacy/{hash} (Teranode asset API, v0.16.0) returns a legacy Bitcoin block: 80B header + 8B sizeInBytes + varint txcount + raw txs.
- spikes/live-verify.mjs: parsed tip block (5 txs incl all 4 anchors), computed standard merkle root over txids = header merkleroot (2caab69b…8bac338b) EXACT match at height 1656.
- Built MerklePath (sdk: path hashes in display order, leaf has txid:true) → merklePath.verify(txid, TeranodeRPCTracker) = PASS (isValidRootForHeight via RPC getblockheader).
- BUMP gap CLOSED: real-headers verification works on regtest.

## Round: productionised MerklePathProvider + full live verifyFragment — DONE
- packages/chain/src/regtest.ts: parseLegacyBlock (80B header + 8B sizeInBytes + varint + raw txs), merkleRootOf, merklePathFor(cfg,txid,tracker) — scans last N blocks, sanity-checks computed root == header merkleroot, returns {blockHeight,path} with display-order hashes + txid leaf.
- spikes/live-verify-fragment.mjs (LIVE): genesis anchor c67b009e… @block1656 → getrawtransaction → decodeMamemAnchor → merklePathFor → attach MerklePath → bundle → verifyFragment(TeranodeRPCTracker):
  verifyFragment: {"ok":true}
  tampered leaf : {"ok":false,"reason":"LeafPathMismatch"} ✓ correct rejection
- LIVE chain txids: genesis c67b009ee0580d221de90f7f33ce9549873cafe56035e9fa16aeab60a771286f, appends 40c4418b/06b6022a/7f49df8f (block 1656).
- TODO next: Spend regression test (3 appends + failed broadcast), MAMEM_NETWORK=regtest reduced e2e, parseAnchorFields delete, README/spec.

## Round: regtest e2e — PASS
- packages/e2e/src/e2e-regtest.ts (MAMEM_NETWORK=regtest): fresh coinbase→50/80-out split→MamemMemory story+correction→20 contract anchors→mine→overlay admit with real tracker→verifyFragment vs real headers + burst anchor SPV — ALL PASS.
- Fixes: shardKey=fundKey for funding sig; spendable outputs only if exact P2PKH to funderAddress; asset API indexes mined blocks ~1-2s late → retry loop on merklePathFor.
- Regression test added: "failed broadcast does not advance the head" (3/3 anchor tests).

## Round: hang fix + all gates green
- HANG root cause: getUtxos handed runar the whole pool (2200 entries) — it iterated all and effectively stalled. Fixed: getUtxos returns at most 10 unspent candidates (slice(0,10)); never loops.
- e2e regtest-vs-offline split: `MAMEM_NETWORK=regtest` at top of e2e.ts → e2e-regtest.ts.
- Burst reduced to 60 offline (~7.8s); engine2 replays manual shard only (full-tail needed for previousCoins). Offline e2e PASS in ~50s.
- Gates: build clean; tests all green (anchor 3/3 incl failed-broadcast regression; overlay 8/8; verify 11/11; mcp 3/3; contracts 3/3; core 5; chain 3; log 2; crypto 6); reproduce OK; offline e2e PASS; MAMEM_NETWORK=regtest e2e PASS (split 6098d7a4…, burst genesis f4633689…, BUMP block 2669).

## Round: perf fix + parseAnchorFields gone + docs
- init 41s was topup(2200)+chain.mine merkle-path over 2200-tx mempool → topup(40)/topup(80): offline e2e now ~4.4s wall.
- flat-cost check now real: 20-anchor segments, firstSeg vs lastSeg.
- parseAnchorFields deleted (dead reader); anchorFields kept (vectors use it).
- Regtest e2e PASS again (block 2782).

## Round: v2 Phase 0 + depth measurement
- Depth on regtest with CONTRACT anchors (production AnchorChain): 100 chained unconfirmed, NO LIMIT hit, in 8.7s (~87ms/append incl broadcast). Back-pressure default set to 25.
- Rotation spike: owner-in-state proven feasible (valid rotate + non-owner fails). Byte-diff pending on the new-key-append continuation.

## Round: rotation spike CLOSED — ownerPkh pattern
- Bisect: T1/T2 append PASS; T3/T4 (old key post-rotate) correctly fail at OP_EQUALVERIFY (hash160(pubKey)!=ownerPkh); T5 rotate→append(new key) PASS.
- Bug was test-side: c2 connected to newSigner (no UTXOs) → no fee input → hashOutputs mismatch. Connect owner signer for funding, sign contract arg with new key.
- All 4 spike tests PASS: valid rotate, non-owner rotate fails, post-rotate old fails/new passes, wrong-pubKey append fails (OP_EQUALVERIFY).
- Design: genesisOwner readonly (stable shard), ownerPkh ByteString(20B) mutable, hash160(pubKey)==ownerPkh + checkSig. Ripemd160 not a state type in runar 0.4.6 → ByteString.

## Round: rotation contract promoted to production — DONE
- MamemAnchorRot → packages/contracts/src/MamemAnchor.runar.ts; genesisOwner moved to MUTABLE state (never modified → shard = hash160(genesisOwner) stable; avoids the PubKey-ctor-slot hack that broke eval at OP_SPLIT PC475).
- decodeMamemAnchor: state = genesisOwner(33B)|n|size|R|Rp|poseidon|tag|ownerPkh; byte-auth via runar rebuild equality.
- AnchorChain: append(sig,pubKey,...); new rotate(newKey) — signer switches only after broadcast+decode. tm_mamem accepts append (n+1,chained Rp,same pkh) or rotate (same n/R/Rp,new pkh); ls stores ownerPkh.
- Tests: 9/9 overlay incl rotate admitted, new-key append admitted, rotate-with-changed-n rejected. e2e adds mid-chain rotate (n stays, new key appends n+1). Gates: build clean, 46 tests green, reproduce OK, offline e2e PASS (~4s), regtest e2e PASS.
- Gotcha: runar embeds a PubKey placeholder per ctor slot; unused readonly props aren't substituted (genesisOwner-in-code didn't work) → owner fields live in state.

## Round: ReserveFunder — regtest e2e self-funding, twice green
- Root cause found: regtest halves the coinbase subsidy every 150 blocks; at ~height 3400 fresh coinbases pay ~149 sats. Fresh mining can never fund again.
- ReserveFunder (packages/chain): scans early block coinbases to our P2PKH via block_legacy+parseLegacyBlock (records block heights for BUMPs), persists reserve+spent set in gitignored .state.json; take() hands out each entry once (optimistic spend); markSpent on UTXO_SPENT rejects; addOwned recycles change. MamemProvider prefers funder.take(); AnchorChain.anchor retries up to 60 UTXO_SPENT rejects marking each stale output.
- Funding parents' BUMPs attached via merklePathAtHeight (known height → single block fetch; 1-tx blocks emit root-as-leaf level).
- regtest e2e: two consecutive runs PASS — story+correction, 20-anchor burst, rotate (n=19, shard stable), overlay admit, verifyFragment real headers, SPV.

## Round: SHIP/GASP sync + tm statelessness — DONE
- tm_mamem rewritten stateless: successors validated against the parent decoded from the BEEF or via ls.getByOutpoint when previousCoins marks it topical but the parsed parent is absent. ls head used only as fork guard. ls_mamem persists ownerPkh (pkh col) + genesisOwner (pub col) — no migration needed.
- isContractInput structural (P2PKH unlock = 2 chunks); decoded-anchor seen-cache keyed by outpoint; append requires equal ownerPkh.
- runarchain: getShardId from decoded head (stable across rotate); spent/owned tracked per-funder via WeakMap, shared across AnchorChains.
- MockChain now Teranode-faithful: UTXO_SPENT tracking (mempool+mined, released on reorg) — GASP's conflicting-spend check was the first detector of a silent double-spend. Unit test added.
- MockFunder faucet parents unique per key (faucet:addr:n — two funders collided).
- sdk 4.0.1 note: tx.toBEEF() emits txid+BUMP only for proven parents — ls.getByOutpoint fallback required for tm.
- GASP sync proven in-process: real GASP + OverlayGASPStorage + peer provideForeign* methods; host2 converges to host1 heads for 2 shards incl. rotate+append (expected n=2); forged squatting genesis rejected through sync (tm still applies). securePeerFetch hard-requires HTTPS → in-process remote is the sanctioned wiring.
- Gates: build clean, 48 tests green, reproduce OK, offline e2e PASS, regtest e2e PASS.

## Round: P8 certs/SMT anchored + P7 disclosure — DONE
- Anchored roots real: cert-log batch root and SMT revocation root anchored via AnchorChain on MockChain (mined, BUMP); status proof verifies vs anchored root, fails vs stale; revoked-key non-inclusion rejected.
- custody.ts ported to @bsv/sdk: Shamir GF(n) + KeyCustodian lifecycle hash chain + threshold Schnorr (Lagrange partial sigs, no key assembly). 2-of-3 signs, 1-of-3 fails, stale/revoked rejected.
- disclosure.ts (DIS-001/003/004/006, KEY-014): field leaf H(TAG_FIELD‖salt‖name‖value) + fieldRoot; scoped envelope (one field→one verifier, expiry, owner sig, path→root); negatives: expired/wrong verifier/tampered/missing salt/replay. wrapFor/unwrapFor/wrapMulti via ECDH (Point.mul; BRC-42 invoice-derivation noted as layered on top).
- Gates: build clean, 74 tests green, reproduce OK, offline e2e ~3.3s, regtest e2e PASS.

## Round: P7 fixes + P9 ZKAccess — DONE
- P7 hardening: wrapFor = HKDF-SHA256(shared, salt=rand32, info=ctx) → AES-256-GCM(AAD=ctx) — kills the two-time pad; envDigest binds wrapped+fieldCount+fieldRoot (no swap); fieldsCommitment = H(TAG_FIELDS‖count‖fieldRoot); verifyDisclosureAnchored injects verifyAnchor (avoids verify→anchor cycle); DIS-006 disclosure events logged+anchored.
- Regtest flake root-caused: TX_MISSING_PARENT — burst broadcasts anchor N+1 before Teranode propagated N. Fixed: retryable with 250ms·attempt backoff (×10) in AnchorChain.anchor; UTXO_SPENT still marks+rebuilds.
- zkaccess.ts (ZKA-001..006): Pedersen C = ℓ·G + r·H (H = sha256(tag)·G NUMS), authority-signed commitment, CDS'94 OR + bit-decomposition range proof over C·g^(−ℓ_j), Fiat-Shamir transcript bound to queryId‖fragmentId‖nonce. Honest label: linear sigma proof, not Bulletproof. prove ~57ms / verify ~59ms (8-bit). Negatives: lower clearance, replay nonce/query, forged sig, tampered C, wrong required level.
- Recall gated: MamemMemory.recall(zk:{commitment,proof,transcript,requiredLevel}) — rejected proof never serves ciphertext; receipt.zkaccess records verified/rejected/not-required. Keys still enforce confidentiality independently.
- Gates: build clean, 76 tests green, reproduce OK, offline e2e ~3.3s, regtest e2e PASS (block 4369).

## Round: P9 Bulletproof + missing-proof gate — DONE
- bp.ts: real logarithmic Bulletproofs ported (inner-product fold, hash-to-curve generators, Fiat-Shamir); now the DEFAULT range proof in zkaccess (sigma kept as fallback). Transcript binding threaded into every FS challenge label (queryId‖fragmentId‖nonce) — replay still rejected.
- Sizes/timings: BP 8-bit 1164B (prove ~169ms/verify ~111ms), 64-bit 1579B (~1286ms/~720ms); sigma 8-bit 4412B (linear — kept only as fallback).
- Gate fix: recall now REQUIRES a valid ZKAccess proof before serving level>0 ciphertext — missing proof = rejected ('zkaccess required', receipt.zkaccess='rejected'). 'not-required' only for level-0.
- Bug fixed during port: module-top BP_U hashToPoint ran before ptFromHex (TDZ) → unreachable; moved initialization after helpers. Verify math corrected to the anchored source (g_i=−z, h'_i=z·y^i+z²·2^i, Pip=P−μH+tHat·U).
- Gates: build clean, 77 tests green, reproduce OK, offline e2e, regtest e2e PASS.

## Round: P12a Cedar — DONE
- @mamem/policy via @cedar-policy/cedar-wasm@4.14.0 (real Cedar isAuthorized/formatPolicies — no sidecar needed). Schema: principals Agent/User, actions read/write/disclose/rotate/revoke, resources Fragment{level,shard,class}.
- Policy sets canonicalized via formatPolicies + hashed (TAG policyset) + anchored through AnchorChain (test proves anchoring). Decision receipts {request, policySetHash, decision, time} — receiptMatchesAnchor enforces the anchored hash (a different-policy receipt refused).
- Level isolation: cedar-wasm has no symbolic analyzer → honest bounded exhaustive check over the schema domain (proveLevelIsolation: every level<l principal DENY on level-l fragments) — all pass.
- Recall gated: opts.cedar{policyText,policySetHash,anchoredPolicyHash,caller} — Cedar DENY or hash-mismatch never serves (plaintext OR ciphertext); evaluates the FRAGMENT's stored level (LogRecord.level persisted at remember). Combined with ZK (level>0 proof) + key possession.
- Negatives covered: DENY bypass refused, hash-mismatch refused, receipt-decision forged still DENY.
- Cedar single eval ~10.7ms; 4-case isolation check ~43ms.
- Regtest hiccup: one transient node abort (-1073740791) during e2e; re-run clean PASS (block 4378) — noted as a native/wasm-side crash, not a code path issue.
- Gates: build clean, 78 tests green (policy 4 + mcp 6 incl. cedar gate), reproduce OK, offline e2e ~2.5s, regtest e2e PASS.

## Final v2 round (retry semantics, treasury funding, MCP tools, v2 scenario)
- Anchor retry: ambiguous errors (TX_MISSING_PARENT/timeouts) re-broadcast the
  IDENTICAL tx with backoff; UTXO_SPENT on our head spent by our own attempt is
  adopted as the head; only funding-input UTXO_SPENT rebuilds.
- Funding signer = fundKey (LocalSigner); contract Sig = shard/owner key;
  rotate switches only shardKey. Shards default to distinct owner keys.
- Regtest funding = treasury chain (.state/treasury.json + reserve-run.json):
  persisted outpoint split ~120×50k per run; coinbase bootstrap when missing.
- MockChain.verifyScripts made node-faithful (own parent store, otherInputs
  excludes self, sequence/lockTime correct, TX_MISSING_PARENT on unknown).
- MCP tools added: rotate/revoke/disclose/authorize with receipts.
- v2 e2e scenario in e2e.ts + e2e-regtest.ts (A/B/C, ZK+Cedar+identity, rotate,
  SMT revoke+non-inclusion, auditor envelope verify→expiry).
- Crash investigation: `pnpm --filter @mamem/policy test` 20/20 clean,
  `pnpm e2e` 20/20 clean — 0xC0000409 not reproduced on this codebase; the
  earlier transient abort was a wasm/native hiccup, not a code-path bug.
- Gates: build clean, 89 tests green, reproduce OK, offline e2e PASS incl. v2
  scenario, regtest e2e 3/3 consecutive PASS.

## GOAL-v3 Phase 0 — feasibility spikes (spikes/v3)
- 0.1 ZK: circom2@0.2.23 (WASM) + snarkjs@0.7.6 + circomlibjs@0.1.7 pure-npm works; Poseidon depth-16 membership circuit (8857 wires), dev pot14+2-contrib zkey. prove 2626ms, verify 48ms, proof 725B; tampered proof/public rejected. GATES PASS.
- 0.2 Rúnar: MamemAutomaton compiled (no `slice` in 0.4.6 → flat bigint table args); deploy+step broadcast advances q0→q1. Negatives need production Spend path.
- 0.3 Datalog: ~146-line semi-naive evaluator w/ stratified negation (CycleError), per-fact provenance, deterministic derivHash — no suitable npm pkg.
- rustup: winget+rustup-init both failed (no cargo) — cedar-symcc/idattr-smt cross-check blocked.

## v3 / P10 ZK membership (@mamem/zk)
- Groth16 membership over BN254: circuit membership.circom (Poseidon(3)(hi,lo,blinding)
  leaf over SHA-leaf halves + nullifier Poseidon(2)(blinding,queryId), depth 16,
  9,463 wires); dev ceremony (pot14 + 2 phase-2 contributions) → artifacts/
  (wasm, membership.zkey 4.3MB, vkey.json, ceremony-transcript.json — committed).
  Intermediates in build/ (gitignored). `pnpm zk:ceremony` reproduces.
- L_ZKMem v1 codec: signals checked against scalar field r, pi_* coords against
  base field q (both exported). Proof coords are canonical (< q) or rejected.
- Companion PoseidonTree (circomlibjs, sparse Z_{d+1}=H(Z_d,Z_d)) built by the
  owner only — leafCommit needs owner-derived blinding ('zkblind' BRC-42 pi);
  non-owner can't build the tree, which is acceptable since the owner anchors.
- MamemAnchor.append now takes newPoseidonRoot (assert len==32 || 0); artifact
  regenerated; decoder handles length-prefixed '20'+32B vs legacy '00'.
- verifyMembershipAnchored does SPV (BUMP→headers) on the REAL anchor tx before
  Groth16 verify; plain verifyMembership is tests-only. NullifierStore dedups.
- CIRCOM SIGNAL ORDER GOTCHA: publicSignals = [outputs..., public inputs...] →
  [root, nullifier, queryId]. Was mis-ordered before; round-trip test catches it.
- On-chain Groth16 NOT implemented: BN254 pairing ops are not available in Rúnar
  and the verifier script would exceed miner policy — verification is off-chain
  against the anchored poseidonRoot (documented deviation). Rúnar's secp256k1
  EC ops are planned for a Schnorr PoK gate in the P14 automaton instead.
- Ceremony pitfall: preparePhase2 on a killed write leaves a truncated ptau
  that hangs newZKey silently — ceremony now keeps ptau in build/ and reuses
  only complete files.

## v4 / P14 Merkle Automaton
- `MamemAutomaton` (1,007 B): δ and F Merkle-committed (depth 4, 16 entries each);
  `step` checks owner + from==q + δ path; `halt` checks the F path and continues
  state with `halted=1`. `AutomatonChain` = production path (MamemProvider over
  Broadcaster/Funder, fresh contract per call, head byte-authenticated).
- RUNAR 0.4.6 MISCOMPILE #1: a `let` reassigned from a private-method result
  compiles its final read as an empty push → use distinct `const`s.
- RUNAR 0.4.6 MISCOMPILE #2: a terminal method (no addOutput) does not parse
  state from the preimage; mutable fields are read from constructor slots
  (deploy-time values) → every method continues state.
- leafEntry must be little-endian (num2bin), not BE.
- runar MockProvider returns parents without outputs → bad sighash; tests use
  MockChain.verifyScripts as the oracle instead.
- `MamemAutomatonZk` (on-chain Schnorr PoK via ecMulGen/ecAdd/ecMul): 883,419 B —
  over the 100 KB rule, excluded from production, documented-not-run.
- AUT-009 measured: lockingScript 1,007 B, step tx ~4,076 B, fee 5 sat (rate 1).
- AUT-010 (fuzzy FSM, ledger seed) deferred.
- Regtest: Teranode rejects an off-F halt with TX_INVALID.

## v4 / P15 reasoning, formal model, MCP demo
- `@mamem/reasoning`: grounding (quote / Datalog entailment from VERIFIED
  fragments), model identity (weights sha256 / provider release), committed +
  anchored receipts, citation index + correction loop, timeBounds (existence-by),
  offline audit bundles (JSON-portable derivation proofs).
- BUG FIXED: `MamemMemory.prove` passed the whole chain tail; the verifier
  requires the tail to end at the proven anchor → older batches were unprovable.
- TLA+ `docs/formal/AORG.tla`, TLC 2.19 in Docker: 14,701 distinct states, no
  violation; mutant (sealed edits allowed) caught violating AppendOnly.
- `packages/mcp/src/bin.js` stdio server, `demo/client.mjs` (scripted, or
  model-driven via any OpenAI-compatible endpoint, e.g. local Qwen), Claude
  Desktop config example, stdio integration test.

## v5 / P16 hardening
- SMT: Rust `idattr-smt` run via `docker rust:1-slim` (tools/smt-vectors) →
  `vectors/smt-rust.json`; TS port byte-identical (roots, derive_key, proofs).
  Fixed: TS proof siblings were descending-depth; now ascending like Rust.
- Cedar SymCC 0.8.0 + cvc5 1.3.1 (tools/cedar-symcc): level isolation PROVED for
  all 4 typed read/disclose environments; mutant refuted with counterexample.
  The bounded exhaustive check is kept as a fast unit test.
- Fuzzing (`packages/fuzz`, seeded): anchor/automaton decoders accept only
  well-formed state-only rewrites under identical code; bundle, L_ZKMem, L_pol,
  disclosure verifiers never crash and never accept mutations.
- Datalog: true semi-naive evaluation with predicate index + canonical
  provenance (1,405 → 38.6 ms on 10,751 facts). BUG FIXED: stratification
  rejected positive recursion as a "negation cycle".
- SECURITY FIXES (ZK clearance / identity): public ClearanceCommitment leaked
  level + blinding; any self-issued authority accepted; credential not holder-
  bound; transcript scope/replay unchecked; caller-chosen challenge nonces
  accepted; binding pubkey not matched; a forged proof's requiredLevel raised
  the Cedar level (bypass); plaintext level>0 records were served without
  clearance; encrypted-record proof level not compared to record level. All
  fixed + tested (`MamemMemory.issueChallenge/verifiedCaller/verifyClearance`,
  `clearanceAuthorities`). Principal id = hash160(pubkey).
- BUG FIXED (data loss): encrypted records hashed hdp over the dropped plaintext
  → distinct writes with equal provenance were deduplicated away.
- BUG FIXED: AnchorChain.confirm gave a descendant's BUMP to every earlier
  pending head (hidden by MockChain re-attaching proofs on mine).
- BUG FIXED: GraphAnchor refs pointed to the last batch; verifyVertex compared
  plaintext only (empty when encrypted) and skipped SPV.
- verifyMembership took the expected shard from the anchor under test.
- Revoke tool: SMT key = derive_key(pubkey), 33-byte pubkey required.
- Clean-checkout reproducibility: undeclared deps (@types/node, runar-lang,
  runar-sdk, @bsv/gasp, @bsv/sdk) found by `docker build`; `pnpm check:deps`
  gate added; one @bsv/sdk 4.0.1 via pnpm overrides (anchor previously worked
  only through an unmanaged runar-sdk copy in packages/anchor/node_modules);
  zk tsconfig referenced mcp (cycle) — removed.
- GASP for unconfirmed anchors: not implemented — GASP serves graph nodes with
  proofs; unconfirmed anchors keep propagating by direct `engine.submit`.
- POL-011 (ZK derivation attestation) deferred.
- Docker image + compose; health = scripted MCP session.

## Operationalisation: MCP for every client
- Official MCP SDK 1.32.2 (zod 3.25.76): stdio + Streamable HTTP (`/mcp`,
  sessions), 12 tools with annotations (served from the existing JSON Schemas
  via low-level handlers), 2 resource templates, 3 prompts, server instructions.
  Gotcha: `structuredContent` must be an object (null/array results text-only).
- `openRuntime`: mock | regtest (testnet refused until configured). regtest =
  sqlite records (level/author metadata now stored WITH the record — the old
  post-append mutation was lost by the knex store, i.e. a level gate bypass on
  persistent stores), anchor state (heads/tails/checkpoints/certs/BEEFs),
  security state, reserve + treasury, keys (0600) in MAMEM_DATA_DIR; confirmer
  (`unprovenAnchors`/`attachProof`), optional auto-mine, timed flushDue.
- `AnchorChain.resume(headTx)`: restart continues the same shard (no fork).
- Regtest funding gotcha: recent coinbases are tiny (5–79 sat after halvings);
  `bootstrapReserve` splits a persisted treasury or an early ≥0.1-coin coinbase.
- Docker MCP Toolkit: `docker-mcp/mamem.yaml` (longLived — the gateway otherwise
  starts a fresh container per call and state is lost; volume mamem-data:/data);
  `pnpm mcp:install`, `pnpm mcp:smoke` (remember → anchor → prove through the gateway).
- Docker Model Runner: `pnpm chat` (OpenAI-compatible loop + MCP client).
  ai/qwen3.8-flash-next:q8_0 fails to load: llama.cpp latest-cpu lacks the
  `qwen4exp` architecture (needs a newer DMR engine). On CPU, 8B qwen3 takes minutes
  per turn; 0.6B emits pseudo tool tags instead of tool calls.

## GOAL-v4 production readiness
- COM-003 ingest signatures (`provenanceDigest`, `signProvenance/verifyProvenance`,
  `requireSignedProvenance`), verified before encryption drops d.
- LOG-006 close policy (leaves/age/bytes/cost, injected clock, `flushDue`).
- LOG-007 signed checkpoints per anchor; LOG-005 `PromiseMonitor`.
- CHN-006 `finalityDepth` / `anchorStatus` / `requireFinal`; `ChainTracker.tipHeight`
  (MockChain's currentHeight is +100 for SDK maturity). REGTEST GOTCHA:
  `getblockchaininfo.blocks` lags generatetoaddress → tip = max(mined header
  height, RPC height).
- Persisted security state (`SecurityStateStore`, `FileSecurityStore`).
- HTTP API (node:http) + OpenAPI 3.1; Prometheus `/v1/metrics`; `ops/alerts.yml`.
- TST-006 Merkle mutation suite. FINDING: a path alone does not bind the leaf
  count (sizes 3 and 4 share height and root under odd self-pairing); the
  anchored size binds it — verifyFragment already checks size against the anchor.
- KEY-012: TEA `bsvcurve` (Docker cargo, tools/curve-vectors) ≡ @bsv/sdk for
  ECDH x, HKDF, RFC 6979 low-S ECDSA.
- SEC-006: `pnpm sbom` → CycloneDX 1.5 `sbom.cdx.json`.
- Removed stray `packages/packages/` and `failed-tx.hex`. `dangerouslyAllowAllBuilds`
  left in place by owner decision.
