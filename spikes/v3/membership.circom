pragma circom 2.0.0;

include "poseidon.circom";

// depth-16 Poseidon Merkle membership:
// private: leaf value + path elements + path bits
// public: root (bound), nullifier = Poseidon(secret, queryId)
template Membership(depth) {
    signal input leaf;                 // private
    signal input secret;               // private (drives the nullifier)
    signal input queryId;              // public — binds the nullifier to a query
    signal input pathElements[depth];  // private
    signal input pathBits[depth];      // private (0 = leaf on left, 1 = leaf on right)
    signal output root;
    signal output nullifier;

    component h = Poseidon(2);
    h.inputs[0] <== secret;
    h.inputs[1] <== queryId;
    nullifier <== h.out;

    var cur = leaf;
    component hasher[depth];
    var left, right;
    for (var i = 0; i < depth; i++) {
        pathBits[i] * (1 - pathBits[i]) === 0; // bit check
        hasher[i] = Poseidon(2);
        // bit=0 → H(cur, sib); bit=1 → H(sib, cur)
        left  = cur + pathBits[i] * (pathElements[i] - cur);
        right = pathElements[i] + pathBits[i] * (cur - pathElements[i]);
        hasher[i].inputs[0] <== left;
        hasher[i].inputs[1] <== right;
        cur = hasher[i].out;
    }
    root <== cur;
}

component main { public [ queryId ] } = Membership(16);
