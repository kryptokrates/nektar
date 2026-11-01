// KEY-012: emits ECDH / HKDF / RFC 6979 ECDSA vectors from TEA's Rust
// `bsvcurve` (JSON on stdout); packages/crypto/test/curve-rust.test.ts must
// reproduce them byte for byte with @bsv/sdk + node:crypto.
use tee_bsvcurve::{ecdh_shared_x, ecdsa_sign_prehash, hkdf_expand_one_block, hkdf_extract, sha256, BsvScalar};

fn scalar(i: u8) -> BsvScalar {
    let mut b = [0u8; 32];
    b[31] = i; b[0] = 0x11; b[15] = i.wrapping_mul(7);
    BsvScalar::from_bytes(&b).expect("valid scalar")
}

fn main() {
    let mut cases = vec![];
    for i in 1u8..=6 {
        let (a, b) = (scalar(i), scalar(i + 100));
        let pa = a.mul_base(); let pb = b.mul_base();
        let shared = ecdh_shared_x(&a, &pb);
        assert_eq!(shared, ecdh_shared_x(&b, &pa));
        let salt = sha256(format!("mamem:salt:{i}").as_bytes());
        let prk = hkdf_extract(&salt, &shared);
        let okm = hkdf_expand_one_block(&prk, format!("mamem:info:{i}").as_bytes());
        let prehash = sha256(format!("message-{i}").as_bytes());
        let sig = ecdsa_sign_prehash(&a, &prehash);
        cases.push(format!(
            "{{\"skA\":\"{}\",\"pubA\":\"{}\",\"skB\":\"{}\",\"pubB\":\"{}\",\"ecdhX\":\"{}\",\"salt\":\"{}\",\"prk\":\"{}\",\"info\":\"mamem:info:{i}\",\"okm\":\"{}\",\"prehash\":\"{}\",\"sigRS\":\"{}\"}}",
            hex::encode(a.to_bytes()), hex::encode(pa.to_compressed()), hex::encode(b.to_bytes()), hex::encode(pb.to_compressed()),
            hex::encode(shared), hex::encode(salt), hex::encode(prk), hex::encode(okm), hex::encode(prehash), hex::encode(sig)
        ));
    }
    println!("{{\"source\":\"triple-entry-evidence-bsv/crates/bsvcurve\",\"cases\":[{}]}}", cases.join(","));
}
