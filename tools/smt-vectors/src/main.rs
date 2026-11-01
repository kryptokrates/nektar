// Emits SMT cross-check vectors from the reference Rust crate (idattr-smt) as
// JSON on stdout. packages/identity/test/smt-rust.test.ts replays the same
// operations on the TypeScript port and requires byte-identical results.
use idattr_smt::{derive_key, empty_root, verify, LeafValue, Smt};
use sha2::{Digest, Sha256};

fn h(s: &str) -> [u8; 32] { Sha256::digest(s.as_bytes()).into() }

fn proof_json(p: &idattr_smt::Proof) -> String {
    let sibs: Vec<String> = p.siblings.iter().map(|(d, s)| format!("[{},\"{}\"]", d, hex::encode(s))).collect();
    format!("{{\"key\":\"{}\",\"siblings\":[{}]}}", hex::encode(p.key), sibs.join(","))
}

fn main() {
    let mut t = Smt::new();
    let mut roots = vec![hex::encode(empty_root())];
    let mut keys = vec![];
    for i in 0..8 {
        let k = derive_key(format!("id-{i}").as_bytes());
        keys.push(hex::encode(k));
        roots.push(hex::encode(t.insert(&k, &h(&format!("v-{i}")))));
    }
    let k3 = derive_key(b"id-3");
    let incl = t.prove(&k3);
    assert!(verify(&incl, LeafValue::Present(&h("v-3")), &t.root()));
    let absent = derive_key(b"absent");
    let non = t.prove(&absent);
    assert!(verify(&non, LeafValue::Absent, &t.root()));
    let after_remove = hex::encode(t.remove(&derive_key(b"id-2")));
    println!(
        "{{\"source\":\"identity-attribution/crates/idattr-smt\",\"keys\":[{}],\"rootsAfterEachInsert\":[{}],\"inclusion\":{},\"nonInclusion\":{},\"rootAfterRemoveId2\":\"{}\"}}",
        keys.iter().map(|k| format!("\"{k}\"")).collect::<Vec<_>>().join(","),
        roots.iter().map(|r| format!("\"{r}\"")).collect::<Vec<_>>().join(","),
        proof_json(&incl), proof_json(&non), after_remove
    );
}
