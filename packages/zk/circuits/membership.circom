pragma circom 2.0.0;

include "poseidon.circom";

// P10 ZK membership, v1:
//   leaf = Poseidon(3)(hi, lo, blinding)  — hi/lo are the 128-bit halves of the
//   fragment's SHA-256 leaf hash (as field elements); blinding is a per-fragment
//   secret (derivable only from the owner key).
//   nullifier = Poseidon(2)(blinding, queryId) — bound to the proven leaf.
//   public: queryId;  outputs: root, nullifier.
template Membership(depth) {
    signal input hi;                   // private
    signal input lo;                   // private
    signal input blinding;             // private
    signal input queryId;              // public
    signal input pathElements[depth];  // private
    signal input pathBits[depth];      // private (0 = leaf left, 1 = leaf right)
    signal output root;
    signal output nullifier;

    component lf = Poseidon(3);
    lf.inputs[0] <== hi;
    lf.inputs[1] <== lo;
    lf.inputs[2] <== blinding;
    var leaf = lf.out;

    component nul = Poseidon(2);
    nul.inputs[0] <== blinding;
    nul.inputs[1] <== queryId;
    nullifier <== nul.out;

    var cur = leaf;
    component hasher[depth];
    var left, right;
    for (var i = 0; i < depth; i++) {
        pathBits[i] * (1 - pathBits[i]) === 0;
        hasher[i] = Poseidon(2);
        left  = cur + pathBits[i] * (pathElements[i] - cur);
        right = pathElements[i] + pathBits[i] * (cur - pathElements[i]);
        hasher[i].inputs[0] <== left;
        hasher[i].inputs[1] <== right;
        cur = hasher[i].out;
    }
    root <== cur;
}

component main { public [ queryId ] } = Membership(16);
