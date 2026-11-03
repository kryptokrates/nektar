// mamem membership circuit — "I know leaf L, index i and a sha256d Merkle
// path such that fold(L, path, i) == R", where R is BAKED as a circuit
// constant: the verifying key itself commits to the statement. A verifier
// UTXO carrying this vk is spendable only by a valid proof *for this R* —
// the claim covenant pins the outpoint, which is the full binding.
//
// parameters.json: {"root":"<hex32>", "leaf":"<hex32>", "siblings":["<hex32>"xD],
//                    "index": <u64>}
// Emits proof/{proof,verifying_key,public_inputs}.json in the uncompressed
// arkworks serialization zkscript's loader expects.
use std::{fs::File, io::{BufReader, Write}, marker::PhantomData};

use ark_bls12_381::{Bls12_381, Fr};
use ark_ff::PrimeField;
use ark_groth16::Groth16;
use ark_r1cs_std::{alloc::AllocVar, bits::ToBitsGadget, boolean::Boolean, eq::EqGadget,
                 fields::{fp::FpVar, FieldVar}, uint8::UInt8};
use ark_relations::r1cs::{ConstraintSynthesizer, ConstraintSystemRef};
use ark_serialize::{CanonicalSerialize, Compress};
use ark_snark::SNARK;
use ark_crypto_primitives::crh::{sha256::constraints::{DigestVar, Sha256Gadget}};
use rand_chacha::ChaChaRng;
use rand::SeedableRng;
use serde_json::{json, Value};

const DEPTH: usize = 4; // 16-leaf tree

#[derive(Clone)]
pub struct Membership<F: PrimeField> {
    pub root: [u8; 32],          // circuit constant — binds the statement
    pub leaf: [u8; 32],          // witness
    pub siblings: Vec<[u8; 32]>, // witness, DEPTH entries
    pub index: u64,              // witness + public input
    _f: PhantomData<F>,
}

// select each bit of a/b by `bit`: out = bit ? a : b
fn select_bytes<F: PrimeField>(
    bit: &Boolean<F>, a: &[UInt8<F>], b: &[UInt8<F>],
) -> ark_relations::r1cs::Result<Vec<UInt8<F>>> {
    let mut out = Vec::with_capacity(a.len());
    for (x, y) in a.iter().zip(b.iter()) {
        let xb = x.to_bits_le()?;
        let yb = y.to_bits_le()?;
        let mut sel = Vec::with_capacity(8);
        for k in 0..8 { sel.push(bit.select(&xb[k], &yb[k])?); }
        out.push(UInt8::<F>::from_bits_le(&sel));
    }
    Ok(out)
}

impl<F: PrimeField> ConstraintSynthesizer<F> for Membership<F> {
    fn generate_constraints(self, cs: ConstraintSystemRef<F>) -> ark_relations::r1cs::Result<()> {
        assert_eq!(self.siblings.len(), DEPTH);

        // public input: index as field element
        let pub_index = FpVar::<F>::new_input(cs.clone(), || Ok(F::from(self.index)))?;

        // witness: leaf + index bits (little-endian)
        let mut cur = UInt8::<F>::new_witness_vec(cs.clone(), &self.leaf)?;
        let mut pos_bits = Vec::with_capacity(DEPTH);
        for d in 0..DEPTH {
            pos_bits.push(Boolean::<F>::new_witness(cs.clone(), || Ok((self.index >> d) & 1 == 1))?);
        }
        // bind pub_index == sum bit_d * 2^d
        let mut acc = FpVar::<F>::zero();
        for (d, b) in pos_bits.iter().enumerate() {
            let bf = b.select(&FpVar::<F>::one(), &FpVar::<F>::zero())?;
            acc += bf * FpVar::<F>::constant(F::from(1u64 << d));
        }
        pub_index.enforce_equal(&acc)?;

        for (d, b) in pos_bits.iter().enumerate() {
            let sib = UInt8::<F>::new_witness_vec(cs.clone(), &self.siblings[d])?;
            // bit=1 → cur is right child (sib||cur); bit=0 → cur is left (cur||sib)
            let left = select_bytes(b, &sib, &cur)?;
            let right = select_bytes(b, &cur, &sib)?;
            let mut input = left;
            input.extend(right);
            let h1 = Sha256Gadget::<F>::digest(&input)?;
            let h2 = Sha256Gadget::<F>::digest(&h1.0)?; // double-sha256
            cur = h2.0;
        }

        // enforce computed root == baked constant root
        let expected = DigestVar::<F>(
            self.root.iter().map(|b| UInt8::<F>::constant(*b)).collect()
        );
        DigestVar::<F>(cur).enforce_equal(&expected)
    }
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let mut rng = ChaChaRng::seed_from_u64(0x4d414d454d); // deterministic dev ceremony — real deployment needs MPC setup

    let params = read_params("parameters.json")?;
    let circuit = Membership::<Fr> {
        root: params.root, leaf: params.leaf,
        siblings: params.siblings, index: params.index,
        _f: PhantomData,
    };

    let (pk, vk) = Groth16::<Bls12_381>::circuit_specific_setup(circuit.clone(), &mut rng)?;
    let proof = Groth16::<Bls12_381>::prove(&pk, circuit.clone(), &mut rng)?;
    let ok = Groth16::<Bls12_381>::verify(&vk, &[Fr::from(circuit.index)], &proof)?;
    assert!(ok, "Proof is invalid");
    println!("proof verifies (root {:02x}{:02x}.. index {})",
             circuit.root[0], circuit.root[1], circuit.index);

    save_to_file(&proof, "proof/proof.json", "proof")?;
    save_to_file(&vk, "proof/verifying_key.json", "verifying_key")?;
    save_to_file(&vec![Fr::from(circuit.index)], "proof/public_inputs.json", "public_inputs")?;
    Ok(())
}

struct Params { root: [u8;32], leaf: [u8;32], siblings: Vec<[u8;32]>, index: u64 }

fn read_params(path: &str) -> Result<Params, Box<dyn std::error::Error>> {
    let j: Value = serde_json::from_reader(BufReader::new(File::open(path)?))?;
    let d32 = |v: &Value| -> [u8;32] {
        let b = hex::decode(v.as_str().unwrap()).unwrap();
        b.try_into().unwrap()
    };
    Ok(Params {
        root: d32(&j["root"]),
        leaf: d32(&j["leaf"]),
        siblings: j["siblings"].as_array().unwrap().iter().map(d32).collect(),
        index: j["index"].as_u64().unwrap(),
    })
}

fn save_to_file<T: CanonicalSerialize>(item: &T, path: &str, key: &str)
    -> Result<(), Box<dyn std::error::Error>>
{
    let mut data = vec![0; item.serialized_size(Compress::No)];
    item.serialize_uncompressed(&mut data[..])?;
    File::create(path)?.write_all(serde_json::to_string_pretty(&json!({key: data}))?.as_bytes())?;
    Ok(())
}
