// nektar derivation guest (RISC Zero). Wright §14: a committed derivation path
// π = {v₀…vₖ} under the anchored policy DAG — nodes h_i = dsha256(serialise(v_i)),
// edges e = dsha256(h_in1 ‖ … ‖ h_ink ‖ τ ‖ h_out) where τ is the applied rule,
// itself a committed DAG node proven against the policy root with the SAME
// double-SHA-256 Merkle fold as the batch tree (odd self-pairing).
// PRIVATE: the path — every step's rule, premise facts and output fact, plus the
// rule-node Merkle paths. PUBLIC (journal): version, policyRoot, conclusionHash,
// queryId. Premises marked as context leaves are uncommitted (the attestation
// proves faithful rule application, not truth of the caller's EDB); premises may
// instead reference an earlier step's output (fromStep) or a fact node committed
// in the DAG (committedIndex + siblings).
#![no_main]
use risc0_zkvm::guest::env;
use risc0_zkvm::sha::{Impl, Sha256};
use serde::Deserialize;
use std::collections::BTreeMap;

risc0_zkvm::guest::entry!(main);

#[derive(Deserialize)]
struct Fact { pred: String, args: Vec<String> }
#[derive(Deserialize)]
struct Lit { pred: String, args: Vec<String>, neg: bool }
#[derive(Deserialize)]
struct RuleSpec { id: String, layer: String, head: Lit, body: Vec<Lit> }
#[derive(Deserialize)]
struct StepInput {
    fact: Fact,
    /// Some(i): this premise is the output of step i (must be < this step)
    from_step: Option<u32>,
    /// Some(i): this premise is a committed DAG fact node at index i
    committed_index: Option<u32>,
    committed_siblings: Vec<[u8; 32]>,
}
#[derive(Deserialize)]
struct Step {
    rule: RuleSpec,
    rule_index: u32,
    rule_siblings: Vec<[u8; 32]>,
    inputs: Vec<StepInput>,
    output: Fact,
}
#[derive(Deserialize)]
struct DerivationInput {
    node_count: u32, // total committed DAG nodes the policy root covers
    policy_root: [u8; 32],
    query_id: [u8; 32],
    steps: Vec<Step>,
}

fn dsha(data: &[u8]) -> [u8; 32] {
    let h1 = Impl::hash_bytes(data);
    let h2 = Impl::hash_bytes(h1.as_bytes());
    let mut out = [0u8; 32];
    out.copy_from_slice(h2.as_bytes());
    out
}

fn height(size: u32) -> usize {
    let (mut h, mut s) = (0usize, 1u64);
    while s < size as u64 { s *= 2; h += 1; }
    h
}

/// identical to the membership guest's fold: odd levels self-pair
fn fold_root(leaf: [u8; 32], index: u32, size: u32, siblings: &[[u8; 32]]) -> [u8; 32] {
    assert!(size >= 1 && index < size, "node index out of range");
    assert!(siblings.len() == height(size), "path length != height(nodeCount)");
    let (mut cur, mut idx, mut width) = (leaf, index, size);
    for sib in siblings.iter() {
        let is_last_odd = idx % 2 == 0 && idx + 1 == width;
        if is_last_odd { assert!(*sib == cur, "odd node must self-pair"); }
        let mut buf = [0u8; 64];
        if idx % 2 == 0 { buf[..32].copy_from_slice(&cur); buf[32..].copy_from_slice(sib); }
        else { buf[..32].copy_from_slice(sib); buf[32..].copy_from_slice(&cur); }
        cur = dsha(&buf);
        idx /= 2;
        width = (width + 1) / 2;
    }
    cur
}

/// JSON.stringify-compatible string escaping (only what JSON.stringify emits:
/// `"`, `\`, the named C0 escapes and \u00xx for the rest below 0x20)
fn json_escape(s: &str) -> String {
    let mut o = String::with_capacity(s.len() + 2);
    for c in s.chars() {
        match c {
            '"' => o.push_str("\\\""),
            '\\' => o.push_str("\\\\"),
            '\u{8}' => o.push_str("\\b"),
            '\u{c}' => o.push_str("\\f"),
            '\n' => o.push_str("\\n"),
            '\r' => o.push_str("\\r"),
            '\t' => o.push_str("\\t"),
            c if (c as u32) < 0x20 => o.push_str(&format!("\\u{:04x}", c as u32)),
            c => o.push(c),
        }
    }
    o
}

fn lit_str(l: &Lit) -> String { format!("{}({})", l.pred, l.args.join(",")) }

/// byte-identical to graph/policy.ts ruleCanonical(): JSON.stringify of
/// {id, layer, version: 1, head, body} in that key order
fn rule_canonical(r: &RuleSpec) -> String {
    let body = r.body.iter()
        .map(|l| format!("{}{}", if l.neg { "not " } else { "" }, lit_str(l)))
        .collect::<Vec<_>>().join(" ");
    format!(
        "{{\"id\":\"{}\",\"layer\":\"{}\",\"version\":1,\"head\":\"{}\",\"body\":\"{}\"}}",
        json_escape(&r.id), json_escape(&r.layer), json_escape(&lit_str(&r.head)), json_escape(&body)
    )
}

/// byte-identical to datalog/path.ts factCanonical(): `fact:pred(args)`
fn fact_canonical(f: &Fact) -> String { format!("fact:{}({})", f.pred, f.args.join(",")) }

fn node_hash(f: &Fact) -> [u8; 32] { dsha(fact_canonical(f).as_bytes()) }

fn is_var(a: &str) -> bool { matches!(a.chars().next(), Some('A'..='Z') | Some('_')) }

fn main() {
    let inp: DerivationInput = env::read();
    assert!(!inp.steps.is_empty(), "a derivation needs at least one step");
    assert!(inp.steps.len() <= 256, "path too long");

    let mut out_hashes: Vec<[u8; 32]> = Vec::with_capacity(inp.steps.len());
    let mut path_digest = [0u8; 32];

    for (i, step) in inp.steps.iter().enumerate() {
        // τ: the applied rule is a committed policy-DAG node (POL-003 canonical,
        // incl. body + negations) proven against the anchored policy root.
        let tau = dsha(rule_canonical(&step.rule).as_bytes());
        assert!(fold_root(tau, step.rule_index, inp.node_count, &step.rule_siblings) == inp.policy_root,
            "rule node not committed under the policy root");

        // ⊢_R: negation is not ZK-provable (absence can't be shown pointwise);
        // every body literal must be positive and match one premise fact.
        assert!(step.inputs.len() == step.rule.body.len(), "premise count != rule body arity");
        let mut bind: BTreeMap<String, String> = BTreeMap::new();
        let mut in_hashes: Vec<[u8; 32]> = Vec::with_capacity(step.inputs.len());
        for (lit, input) in step.rule.body.iter().zip(step.inputs.iter()) {
            assert!(!lit.neg, "negated body literal is not provable");
            assert!(lit.pred == input.fact.pred, "premise predicate mismatch");
            assert!(lit.args.len() == input.fact.args.len(), "premise arity mismatch");
            for (a, v) in lit.args.iter().zip(input.fact.args.iter()) {
                if is_var(a) {
                    match bind.get(a) {
                        Some(prev) => assert!(prev == v, "inconsistent variable binding"),
                        None => { bind.insert(a.clone(), v.clone()); }
                    }
                } else {
                    assert!(a == v, "premise constant mismatch");
                }
            }
            let h = node_hash(&input.fact);
            match (input.from_step, input.committed_index) {
                // premise produced by an earlier step: its hash must equal that
                // step's output hash (no skipped or reordered steps)
                (Some(src), None) => {
                    assert!((src as usize) < i, "premise references a later step");
                    assert!(out_hashes[src as usize] == h, "premise is not the referenced step's output");
                }
                // premise committed as a DAG fact node under the same root
                (None, Some(ci)) => {
                    assert!(fold_root(h, ci, inp.node_count, &input.committed_siblings) == inp.policy_root,
                        "fact node not committed under the policy root");
                }
                (None, None) => {} // context (EDB) premise — private, uncommitted
                (Some(_), Some(_)) => panic!("premise cannot be both derived and committed"),
            }
            in_hashes.push(h);
        }

        // the head must instantiate to the claimed output under the bindings
        assert!(step.output.pred == step.rule.head.pred, "output predicate != rule head");
        assert!(step.output.args.len() == step.rule.head.args.len(), "output arity != rule head");
        for (a, v) in step.rule.head.args.iter().zip(step.output.args.iter()) {
            if is_var(a) {
                assert!(bind.get(a) == Some(v), "output does not match the bound head");
            } else {
                assert!(a == v, "output constant mismatch");
            }
        }
        let out_h = node_hash(&step.output);

        // §14 edge commitment e = dsha256(h_in1 ‖ … ‖ h_ink ‖ τ ‖ h_out),
        // folded into the path digest
        let mut ebuf = Vec::with_capacity(32 * (in_hashes.len() + 2));
        for h in &in_hashes { ebuf.extend_from_slice(h); }
        ebuf.extend_from_slice(&tau);
        ebuf.extend_from_slice(&out_h);
        let edge = dsha(&ebuf);
        let mut pbuf = [0u8; 64];
        pbuf[..32].copy_from_slice(&path_digest);
        pbuf[32..].copy_from_slice(&edge);
        path_digest = dsha(&pbuf);

        out_hashes.push(out_h);
    }

    let conclusion_hash = *out_hashes.last().unwrap();
    env::commit(&(1u8, inp.policy_root, conclusion_hash, inp.query_id));
}
