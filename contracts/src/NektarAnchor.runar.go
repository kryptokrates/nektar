// NektarAnchor — stateful Rúnar contract enforcing the nektar successor rule
// on-chain (OP_PUSH_TX continuation), v3: the state carries ONLY Wright's
// commitments (n, size, R_n, R'_n) plus ownership — no ZK companion root.
// ZK membership is proven over R_n itself (ADR-020, STARK over SHA-256).
//   shard = hash160(genesisOwner) — immutable, embedded in code.
//   ANC-006 (GOAL-v6 S6.2): the state payload's FIRST push is the Metanet
//   flag 0x4D455441 ("META"). Flagged scripts decode alongside flag-absent
//   v3 anchors (decodeNektarAnchor accepts both artifacts).
//   append(sig, pubKey, newR, newSize):
//     hash160(pubKey) == ownerPkh AND checkSig(sig, pubKey) AND continuation
//     carries n+1, size=newSize, R=newR, Rp' = hash256(TAG‖Rp‖newR‖size_le8),
//     same ownerPkh.
//   rotate(sig, pubKey, newOwnerPkh):
//     same checks; continuation changes ONLY ownerPkh -> newOwnerPkh.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarAnchor struct {
	runar.StatefulSmartContract
	MetaFlag     runar.ByteString
	GenesisOwner runar.PubKey
	N            runar.Bigint
	Size         runar.Bigint
	R            runar.Sha256Digest
	Rp           runar.Sha256Digest
	TagRprime    runar.ByteString
	OwnerPkh     runar.ByteString // 20B = hash160(current owner pubkey)
}

func (c *NektarAnchor) Append(sig runar.Sig, pubKey runar.PubKey, newR runar.Sha256Digest, newSize runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	newRp := runar.Hash256(runar.Cat(c.TagRprime, runar.Cat(c.Rp, runar.Cat(newR, runar.Num2Bin(newSize, 8)))))
	c.AddOutput(1, c.MetaFlag, c.GenesisOwner, c.N+1, newSize, newR, newRp, c.TagRprime, c.OwnerPkh)
}

func (c *NektarAnchor) Rotate(sig runar.Sig, pubKey runar.PubKey, newOwnerPkh runar.ByteString) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(runar.Len(newOwnerPkh) == 20)
	c.AddOutput(1, c.MetaFlag, c.GenesisOwner, c.N, c.Size, c.R, c.Rp, c.TagRprime, newOwnerPkh)
}

// WitnessSettle implements the §4.5 anchor witness co-spent as input1 of
// NektarZkClaim.settleAnchored. Re-emits our continuation first (same state),
// then [verdict, payout] with c.R embedded at the journalRoot field.
func (c *NektarAnchor) WitnessSettle(sig runar.Sig, pubKey runar.PubKey, receiptHash runar.ByteString, queryId runar.ByteString, payeePkh runar.ByteString, verdictCode runar.ByteString, verdictSats runar.Bigint, payoutSats runar.Bigint, payoutPkh runar.ByteString) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(runar.Len(payeePkh) == 20 && runar.Len(payoutPkh) == 20)
	runar.Assert(runar.Len(queryId) <= 75 && runar.Len(c.R) == 32 && runar.Len(receiptHash) == 32)
	runar.Assert(verdictSats >= 1 && payoutSats >= 1)
	e1 := runar.Cat(runar.Num2Bin(runar.Len(payeePkh), 1), payeePkh)
	e2 := runar.Cat(runar.Num2Bin(runar.Len(queryId), 1), queryId)
	e3 := runar.Cat(runar.Num2Bin(runar.Len(c.R), 1), c.R)
	e4 := runar.Cat(runar.Num2Bin(runar.Len(receiptHash), 1), receiptHash)
	vstate := runar.Cat(e4, runar.Cat(e3, runar.Cat(e2, e1)))
	c.AddOutput(1, c.MetaFlag, c.GenesisOwner, c.N, c.Size, c.R, c.Rp, c.TagRprime, c.OwnerPkh)
	c.AddRawOutput(verdictSats, runar.Cat(verdictCode, runar.Cat(runar.HexToBytes("6a"), vstate)))
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(payoutPkh, runar.HexToBytes("88ac"))))
}
