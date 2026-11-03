// @nektar/zk — Wright §13 ZK membership: π ⊢ h_j ∈ M_n for the SAME
// double-SHA-256 batch tree whose root R_n is anchored on BSV. Transparent
// ZK-STARK (RISC Zero succinct receipt; Fiat–Shamir, FRI, no trusted setup),
// verified off-chain (WASM) against the SPV-verified anchor. See ADR-020.
export * from './stark/index.js'
export * from './poseidon-toy.js'
export * from './aiinf-demo.js'
