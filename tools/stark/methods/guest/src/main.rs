// nektar membership guest (RISC Zero). Wright §13: π ⊢ h_j ∈ M_n for the SAME
// double-SHA-256 batch tree whose root R_n is anchored on BSV (core/merkle.ts):
// node = dsha256(left ‖ right), odd level self-pairs, internal byte order.
// PRIVATE: leaf, index, siblings.  PUBLIC (journal): version, root, size, queryId.
#![no_main]
use risc0_zkvm::guest::env;
use risc0_zkvm::sha::{Impl, Sha256};

risc0_zkvm::guest::entry!(main);

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

fn main() {
    let leaf: [u8; 32] = env::read();
    let index: u32 = env::read();
    let size: u32 = env::read();
    let siblings: Vec<[u8; 32]> = env::read();
    let query_id: [u8; 32] = env::read();

    assert!(size >= 1 && index < size, "index out of range");
    assert!(siblings.len() == height(size), "path length != height(size)");

    // odd self-pairing: at every level where this node is the last odd node,
    // its sibling MUST be itself (prevents the duplicate-last-leaf ambiguity)
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
    env::commit(&(1u8, cur, size, query_id));
}
