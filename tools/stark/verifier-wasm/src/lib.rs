// WASM verifier for mamem membership receipts (Node, any OS).
// verify(receipt_bincode, image_id_32) → JSON {ok, journal|error}
// Only SUCCINCT receipts (a single ZK-STARK) are accepted.
use risc0_zkvm::{Digest, Receipt};
use wasm_bindgen::prelude::*;

#[wasm_bindgen]
pub fn verify(receipt: &[u8], image_id: &[u8]) -> String {
    let res = (|| -> Result<serde_json::Value, String> {
        let r: Receipt = bincode::deserialize(receipt).map_err(|e| format!("decode: {e}"))?;
        if r.inner.succinct().is_err() { return Err("not a succinct receipt".into()); }
        let id = Digest::try_from(image_id).map_err(|_| "image id must be 32 bytes".to_string())?;
        r.verify(id).map_err(|e| format!("verify: {e}"))?;
        // the journal deserializer does NOT check trailing bytes — the shapes
        // are discriminated by exact length. risc0 serde writes each element as
        // a u32 word: membership (u8‖[u8;32]‖u32‖[u8;32]) = 264 B, §14
        // derivation (u8‖[u8;32]‖[u8;32]‖[u8;32]) = 388 B.
        match r.journal.bytes.len() {
            264 => {
                let (ver, root, size, qid): (u8, [u8; 32], u32, [u8; 32]) = r.journal.decode().map_err(|e| format!("journal: {e}"))?;
                Ok(serde_json::json!({ "version": ver, "root": hex::encode(root), "size": size, "queryId": hex::encode(qid) }))
            }
            388 => {
                let (ver, proot, concl, qid): (u8, [u8; 32], [u8; 32], [u8; 32]) = r.journal.decode().map_err(|e| format!("journal: {e}"))?;
                Ok(serde_json::json!({ "version": ver, "policyRoot": hex::encode(proot), "conclusionHash": hex::encode(concl), "queryId": hex::encode(qid) }))
            }
            n => Err(format!("journal: unknown shape ({n} bytes)")),
        }
    })();
    match res {
        Ok(j) => serde_json::json!({ "ok": true, "journal": j }).to_string(),
        Err(e) => serde_json::json!({ "ok": false, "error": e }).to_string(),
    }
}
