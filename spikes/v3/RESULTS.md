# GOAL-v3 Phase 0 — feasibility spikes

## Versions (pinned, all >7 days old)

- `circom2@0.2.23` (WASM circom 2.0), `snarkjs@0.7.6`, `circomlibjs@0.1.7`, `circomlib@2.0.5`
- `runar-compiler@0.4.6`, `runar-sdk@0.4.6`, `@bsv/sdk@4.0.1`

## Spike 0.1 — ZK membership toolchain: **PASS**

- Pure-npm path works — no cargo/circom binary needed.
- Circuit `membership.circom`: Poseidon(2) Merkle membership depth 16, private {leaf, secret, pathElements[16], pathBits[16]}, public {queryId}, outputs {root, nullifier=Poseidon(secret,queryId)}. Compiled: **8,857 wires** (~8.8k constraints).
- Dev ptau pot14 (2 contributions) + 2-contributor zkey, all local — no large ptau downloads.
- Sparse-tree convention `Z_{d+1}=H(Z_d,Z_d)` pads the 6-leaf witness to depth 16; root matches.
- **Measured**: prove **2,626 ms** · verify **48 ms** · proof **725 B** (JSON).
- Negatives: tampered proof rejected, tampered public signal rejected.
- **Gates**: verify <50 ms ✓ (48) · prove <10 s ✓ (2.6 s).

## Spike 0.2 — Rúnar automaton: **PASS (compile + valid broadcast)**

- `NektarAutomaton.runar.ts`: state `{q, deltaRoot, ownerPkh, n}`; `step(sig,pubKey, e0..e11, from,sym,to)` — rebuilds the 4-triple table via `num2bin`+`cat`, asserts `hash256(table)==deltaRoot`, `from==q`, unrolled 4-entry membership, `addOutput` continues with `q=to`.
- `slice` is **not** in Rúnar 0.4.6 — table passed as flat bigint args instead (compiled cleanly after the change).
- Artifact `NektarAutomaton.json` written; deploys + `step` calls broadcast on `MockProvider` (valid transition `q0 -a-> q1` advanced state to q1, n+1).
- Negatives designed (invalid triple, tampered table, wrong signer) — full Spend-eval of the negatives needs the production `finalizeCall` signature path (the placeholder-sig tx isn't spend-evaluable on MockProvider); production integration will reuse `packages/anchor`'s broadcast/Spend path.

## Spike 0.3 — Datalog: **PASS**

- No suitable npm stratified-Datalog-with-negation + provenance package found small enough → wrote a **~146-line semi-naive evaluator** (`datalog.cjs`): stratified negation (CycleError on neg cycle), per-fact provenance `{ruleId, premises}`, deterministic `derivHash` over sorted derivations.
- Test: clearance/delegation/level/revocation rules — `allowed(agent-b,frag-1)` derives with full premise tree; revoked principal correctly not derived; `p <- NOT p` throws `CycleError`.
- derivHash deterministic: `4b0d204526a7c47aa15b9cb225c5942a918308fc49f6f42aec54373d4f2479df`.

## rustup on Windows: **FAILED**

- `winget install Rustlang.Rustup` failed (exit 143); `rustup-init.exe -y` recovered a partial install then timed out mid-download — no `cargo` on PATH. cedar-policy-symcc / idattr-smt cross-check stay blocked on this machine until cargo is available another way.

## Gate verdicts

| Spike               | Verdict                                                                                     |
| ------------------- | ------------------------------------------------------------------------------------------- |
| 0.1 ZK toolchain    | **PASS** — prove 2626ms (<10s), verify 48ms (<50ms), proof 725B, negatives reject           |
| 0.2 Rúnar automaton | **PASS** — compiles + valid transition advances state; negatives need production Spend path |
| 0.3 Datalog         | **PASS** — stratified negation + provenance + deterministic hash, cycle throws              |
