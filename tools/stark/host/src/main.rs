// mamem-stark: prover/verifier CLI for the membership + derivation guests.
//   mamem-stark image-id                     → hex image id (membership, pinned by mamem)
//   mamem-stark image-id-der                 → hex image id (§14 derivation)
//   mamem-stark prove  < {"leaf","index","size","siblings":[..],"queryId"}  → {"receipt":hex,"journal":{..},"ms":n}
//   mamem-stark prove-der < DerIn JSON                                         → {"receipt":hex,"journal":{..},"ms":n}
//   mamem-stark execute | execute-der                                        → run without proving
//   mamem-stark verify < {"receipt":hex,"imageId":hex}                       → {"ok":bool,"journal":{..}|null,"error":..}
// Succinct receipts only: a single ZK-STARK (composite receipts leak trace length).
use mamem_methods::{MAMEM_MEMBERSHIP_ELF, MAMEM_MEMBERSHIP_ID, MAMEM_DATALOG_ELF, MAMEM_DATALOG_ID};
use risc0_zkvm::{default_prover, ExecutorEnv, ProverOpts, Receipt, Digest};
use serde::{Deserialize, Serialize};
use std::io::Read;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProveIn { leaf: String, index: u32, size: u32, siblings: Vec<String>, query_id: String }
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct VerifyIn { receipt: String, image_id: String }

// §14 derivation input — field order must match the guest's DerivationInput
// (the zkVM env serializer is positional, not field-name based).
#[derive(Deserialize, Serialize)]
struct DerFact { pred: String, args: Vec<String> }
#[derive(Deserialize, Serialize)]
struct DerLit { pred: String, args: Vec<String>, neg: bool }
#[derive(Deserialize, Serialize)]
struct DerRule { id: String, layer: String, head: DerLit, body: Vec<DerLit> }
#[derive(Deserialize, Serialize)]
#[serde(rename_all = "camelCase")]
struct DerStepInput {
    fact: DerFact,
    from_step: Option<u32>,
    committed_index: Option<u32>,
    #[serde(default)]
    committed_siblings: Vec<String>,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DerStepIn {
    rule: DerRule,
    rule_index: u32,
    rule_siblings: Vec<String>,
    inputs: Vec<DerStepInput>,
    output: DerFact,
}
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct DerIn { node_count: u32, policy_root: String, query_id: String, steps: Vec<DerStepIn> }

// guest-side mirrors (same field order as guest-datalog/src/main.rs)
#[derive(Serialize)]
struct GFact { pred: String, args: Vec<String> }
#[derive(Serialize)]
struct GLit { pred: String, args: Vec<String>, neg: bool }
#[derive(Serialize)]
struct GRule { id: String, layer: String, head: GLit, body: Vec<GLit> }
#[derive(Serialize)]
struct GStepInput { fact: GFact, from_step: Option<u32>, committed_index: Option<u32>, committed_siblings: Vec<[u8; 32]> }
#[derive(Serialize)]
struct GStep { rule: GRule, rule_index: u32, rule_siblings: Vec<[u8; 32]>, inputs: Vec<GStepInput>, output: GFact }
#[derive(Serialize)]
struct GIn { node_count: u32, policy_root: [u8; 32], query_id: [u8; 32], steps: Vec<GStep> }

fn b32(h: &str) -> [u8; 32] {
    let v = hex::decode(h).expect("hex");
    assert!(v.len() == 32, "expected 32 bytes");
    let mut o = [0u8; 32]; o.copy_from_slice(&v); o
}
fn image_id_hex() -> String { hex::encode(Digest::from(MAMEM_MEMBERSHIP_ID).as_bytes()) }
fn der_image_id_hex() -> String { hex::encode(Digest::from(MAMEM_DATALOG_ID).as_bytes()) }

fn ginput(p: &DerIn) -> GIn {
    let conv_fact = |f: &DerFact| GFact { pred: f.pred.clone(), args: f.args.clone() };
    let conv_lit = |l: &DerLit| GLit { pred: l.pred.clone(), args: l.args.clone(), neg: l.neg };
    GIn {
        node_count: p.node_count,
        policy_root: b32(&p.policy_root),
        query_id: b32(&p.query_id),
        steps: p.steps.iter().map(|s| GStep {
            rule: GRule {
                id: s.rule.id.clone(), layer: s.rule.layer.clone(),
                head: conv_lit(&s.rule.head), body: s.rule.body.iter().map(&conv_lit).collect(),
            },
            rule_index: s.rule_index,
            rule_siblings: s.rule_siblings.iter().map(|x| b32(x)).collect(),
            inputs: s.inputs.iter().map(|i| GStepInput {
                fact: conv_fact(&i.fact), from_step: i.from_step,
                committed_index: i.committed_index,
                committed_siblings: i.committed_siblings.iter().map(|x| b32(x)).collect(),
            }).collect(),
            output: conv_fact(&s.output),
        }).collect(),
    }
}

fn journal_json(r: &Receipt) -> serde_json::Value { journal_json_session(&r.journal) }

fn journal_json_session(j: &risc0_zkvm::Journal) -> serde_json::Value {
    // the deserializer does NOT check trailing bytes — discriminate by the raw
    // journal length. risc0 serde writes each element as a u32 word, so
    // membership (u8‖[u8;32]‖u32‖[u8;32]) = 4+128+4+128 = 264 B and §14
    // derivation (u8‖[u8;32]‖[u8;32]‖[u8;32]) = 4+128·3 = 388 B.
    match j.bytes.len() {
        264 => {
            let (ver, root, size, qid): (u8, [u8; 32], u32, [u8; 32]) = j.decode().expect("journal");
            serde_json::json!({ "version": ver, "root": hex::encode(root), "size": size, "queryId": hex::encode(qid) })
        }
        388 => {
            let (ver, proot, concl, qid): (u8, [u8; 32], [u8; 32], [u8; 32]) = j.decode().expect("journal");
            serde_json::json!({ "version": ver, "policyRoot": hex::encode(proot), "conclusionHash": hex::encode(concl), "queryId": hex::encode(qid) })
        }
        n => panic!("unknown journal shape ({n} bytes)"),
    }
}

fn main() {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    let mut input = String::new();
    if cmd != "image-id" && cmd != "image-id-der" { std::io::stdin().read_to_string(&mut input).expect("stdin"); }
    match cmd.as_str() {
        "image-id" => println!("{}", image_id_hex()),
        "image-id-der" => println!("{}", der_image_id_hex()),
        "execute-der" => {
            let p: DerIn = serde_json::from_str(&input).expect("execute-der input");
            let env = ExecutorEnv::builder().write(&ginput(&p)).unwrap().build().unwrap();
            match risc0_zkvm::default_executor().execute(env, MAMEM_DATALOG_ELF) {
                Ok(s) => println!("{}", serde_json::json!({ "ok": true, "journal": journal_json_session(&s.journal) })),
                Err(e) => println!("{}", serde_json::json!({ "ok": false, "error": e.to_string() })),
            }
        }
        "prove-der" => {
            let p: DerIn = serde_json::from_str(&input).expect("prove-der input");
            let env = ExecutorEnv::builder().write(&ginput(&p)).unwrap().build().unwrap();
            let t = std::time::Instant::now();
            let info = default_prover().prove_with_opts(env, MAMEM_DATALOG_ELF, &ProverOpts::succinct()).expect("prove");
            let receipt = info.receipt;
            let bytes = bincode::serialize(&receipt).unwrap();
            println!("{}", serde_json::json!({ "receipt": hex::encode(&bytes), "journal": journal_json(&receipt), "imageId": der_image_id_hex(), "ms": t.elapsed().as_millis() as u64 }));
        }
        "execute" => {
            // run the exact guest WITHOUT proving (fast convention/negative checks)
            let p: ProveIn = serde_json::from_str(&input).expect("execute input");
            let sibs: Vec<[u8; 32]> = p.siblings.iter().map(|s| b32(s)).collect();
            let env = ExecutorEnv::builder()
                .write(&b32(&p.leaf)).unwrap().write(&p.index).unwrap().write(&p.size).unwrap()
                .write(&sibs).unwrap().write(&b32(&p.query_id)).unwrap()
                .build().unwrap();
            match risc0_zkvm::default_executor().execute(env, MAMEM_MEMBERSHIP_ELF) {
                Ok(s) => {
                    let (ver, root, size, qid): (u8, [u8; 32], u32, [u8; 32]) = s.journal.decode().expect("journal");
                    println!("{}", serde_json::json!({ "ok": true, "journal": { "version": ver, "root": hex::encode(root), "size": size, "queryId": hex::encode(qid) } }))
                }
                Err(e) => println!("{}", serde_json::json!({ "ok": false, "error": e.to_string() })),
            }
        }
        "prove" => {
            let p: ProveIn = serde_json::from_str(&input).expect("prove input");
            let sibs: Vec<[u8; 32]> = p.siblings.iter().map(|s| b32(s)).collect();
            let env = ExecutorEnv::builder()
                .write(&b32(&p.leaf)).unwrap().write(&p.index).unwrap().write(&p.size).unwrap()
                .write(&sibs).unwrap().write(&b32(&p.query_id)).unwrap()
                .build().unwrap();
            let t = std::time::Instant::now();
            let info = default_prover().prove_with_opts(env, MAMEM_MEMBERSHIP_ELF, &ProverOpts::succinct()).expect("prove");
            let receipt = info.receipt;
            let bytes = bincode::serialize(&receipt).unwrap();
            println!("{}", serde_json::json!({ "receipt": hex::encode(&bytes), "journal": journal_json(&receipt), "imageId": image_id_hex(), "ms": t.elapsed().as_millis() as u64 }));
        }
        "verify" => {
            let v: VerifyIn = serde_json::from_str(&input).expect("verify input");
            let res = (|| -> Result<serde_json::Value, String> {
                let bytes = hex::decode(&v.receipt).map_err(|e| e.to_string())?;
                let receipt: Receipt = bincode::deserialize(&bytes).map_err(|e| format!("decode: {e}"))?;
                // only a single succinct ZK-STARK is acceptable
                if receipt.inner.succinct().is_err() { return Err("not a succinct receipt".into()); }
                let id = Digest::try_from(hex::decode(&v.image_id).map_err(|e| e.to_string())?.as_slice()).map_err(|_| "image id")?;
                receipt.verify(id).map_err(|e| format!("verify: {e}"))?;
                Ok(journal_json(&receipt))
            })();
            match res {
                Ok(j) => println!("{}", serde_json::json!({ "ok": true, "journal": j })),
                Err(e) => println!("{}", serde_json::json!({ "ok": false, "journal": null, "error": e })),
            }
        }
        _ => { eprintln!("usage: mamem-stark image-id | image-id-der | prove | prove-der | execute | execute-der | verify"); std::process::exit(2) }
    }
}
