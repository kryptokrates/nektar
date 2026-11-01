// mamem-stark: prover/verifier CLI for the membership guest.
//   mamem-stark image-id                     → hex image id (pinned by mamem)
//   mamem-stark prove  < {"leaf","index","size","siblings":[..],"queryId"}  → {"receipt":hex,"journal":{..},"ms":n}
//   mamem-stark verify < {"receipt":hex,"imageId":hex}                       → {"ok":bool,"journal":{..}|null,"error":..}
// Succinct receipts only: a single ZK-STARK (composite receipts leak trace length).
use mamem_methods::{MAMEM_MEMBERSHIP_ELF, MAMEM_MEMBERSHIP_ID};
use risc0_zkvm::{default_prover, ExecutorEnv, ProverOpts, Receipt, Digest};
use serde::Deserialize;
use std::io::Read;

#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct ProveIn { leaf: String, index: u32, size: u32, siblings: Vec<String>, query_id: String }
#[derive(Deserialize)]
#[serde(rename_all = "camelCase")]
struct VerifyIn { receipt: String, image_id: String }

fn b32(h: &str) -> [u8; 32] {
    let v = hex::decode(h).expect("hex");
    assert!(v.len() == 32, "expected 32 bytes");
    let mut o = [0u8; 32]; o.copy_from_slice(&v); o
}
fn image_id_hex() -> String { hex::encode(Digest::from(MAMEM_MEMBERSHIP_ID).as_bytes()) }
fn journal_json(r: &Receipt) -> serde_json::Value {
    let (ver, root, size, qid): (u8, [u8; 32], u32, [u8; 32]) = r.journal.decode().expect("journal");
    serde_json::json!({ "version": ver, "root": hex::encode(root), "size": size, "queryId": hex::encode(qid) })
}

fn main() {
    let cmd = std::env::args().nth(1).unwrap_or_default();
    let mut input = String::new();
    if cmd != "image-id" { std::io::stdin().read_to_string(&mut input).expect("stdin"); }
    match cmd.as_str() {
        "image-id" => println!("{}", image_id_hex()),
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
        _ => { eprintln!("usage: mamem-stark image-id | prove | verify"); std::process::exit(2) }
    }
}
