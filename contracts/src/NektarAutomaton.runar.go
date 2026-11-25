// NektarAutomaton — AUT-005 full mode. Stateful Rúnar contract holding q of a
// deterministic finite automaton; each step spends the state UTXO and enforces
// delta on-chain (AUT-007: depth-4 Merkle membership over 24B transition leaves).
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarAutomaton struct {
	runar.StatefulSmartContract
	Q         runar.Bigint
	DeltaRoot runar.Sha256Digest // Merkle root over 24B leaf entries num2bin8(from)||num2bin8(sym)||num2bin8(to)
	OwnerPkh  runar.ByteString   // 20B = hash160(owner pubkey)
	N         runar.Bigint       // step counter
	LastOut   runar.ByteString   // 32B hash of last output event, empty at genesis
	FinalRoot runar.Sha256Digest // Merkle root over 8B leaves num2bin8(q) for q in F, padded to 16
	Halted    runar.Bigint       // 0 = running, 1 = halted in a final state
}

// fold computes one level of a depth-4 Merkle path; dir=1 -> sibling on left, dir=0 -> right.
func (c *NektarAutomaton) fold(h runar.ByteString, sib runar.ByteString, dir runar.Bigint) runar.ByteString {
	if dir == 1 {
		return runar.Hash256(runar.Cat(sib, h))
	}
	return runar.Hash256(runar.Cat(h, sib))
}

func (c *NektarAutomaton) Step(sig runar.Sig, pubKey runar.PubKey,
	from runar.Bigint, sym runar.Bigint, to runar.Bigint, outHash runar.ByteString,
	sib0 runar.ByteString, sib1 runar.ByteString, sib2 runar.ByteString, sib3 runar.ByteString,
	dir runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Halted == 0)
	runar.Assert(runar.Len(outHash) == 32)
	runar.Assert(from == c.Q)
	h0 := runar.Cat(runar.Num2Bin(from, 8), runar.Cat(runar.Num2Bin(sym, 8), runar.Num2Bin(to, 8)))
	h1 := c.fold(h0, sib0, dir%2)
	h2 := c.fold(h1, sib1, (dir/2)%2)
	h3 := c.fold(h2, sib2, (dir/4)%2)
	h4 := c.fold(h3, sib3, (dir/8)%2)
	runar.Assert(h4 == c.DeltaRoot)
	c.AddOutput(1, to, c.DeltaRoot, c.OwnerPkh, c.N+1, outHash, c.FinalRoot, 0)
}

// Halt verifies q in F by a depth-4 Merkle path of the 8-byte leaf num2bin(q) against finalRoot.
func (c *NektarAutomaton) Halt(sig runar.Sig, pubKey runar.PubKey,
	sib0 runar.ByteString, sib1 runar.ByteString, sib2 runar.ByteString, sib3 runar.ByteString, dir runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Halted == 0)
	f0 := runar.Num2Bin(c.Q, 8)
	f1 := c.fold(f0, sib0, dir%2)
	f2 := c.fold(f1, sib1, (dir/2)%2)
	f3 := c.fold(f2, sib2, (dir/4)%2)
	f4 := c.fold(f3, sib3, (dir/8)%2)
	runar.Assert(f4 == c.FinalRoot)
	c.AddOutput(1, c.Q, c.DeltaRoot, c.OwnerPkh, c.N+1, c.LastOut, c.FinalRoot, 1)
}
