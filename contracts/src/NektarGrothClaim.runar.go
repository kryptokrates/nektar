// NektarGrothClaim — GOAL-v9 §0e: hardened VALIDITY-rail claim covenant.
// Enforces:
//   - Exact 2-input co-spend pinning verifierOutpoint (36B) via hashPrevouts.
//   - F-19: strict length bounds on proverPkh/queryId/journalRoot/receiptHash before 1B num2bin.
//   - F-25: no unconstrained caller verifierSats on RHS of maxFee check; verdictSats >= 1 && payoutSats >= 1.
//   - F-18: paired with Go VerifyGrothClaimSettlement checking verifier VK script hash & doc-push == journalRoot.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarGrothClaim struct {
	runar.StatefulSmartContract
	ReceiptHash      runar.Sha256Digest
	JournalRoot      runar.ByteString
	JournalSize      runar.Bigint
	QueryId          runar.ByteString
	ProverPkh        runar.ByteString
	VerifierOutpoint runar.ByteString
	MaxFee           runar.Bigint
	VerdictCode      runar.ByteString
}

func (c *NektarGrothClaim) SettleValid(sig runar.Sig, pubKey runar.PubKey, prevouts runar.ByteString, verdictSats runar.Bigint, payoutSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(runar.Len(c.VerifierOutpoint) == 36)
	runar.Assert(runar.Hash256(prevouts) == runar.ExtractHashPrevouts(c.TxPreimage))
	runar.Assert(prevouts == runar.Cat(runar.ExtractOutpoint(c.TxPreimage), c.VerifierOutpoint))
	runar.Assert(runar.Len(c.ProverPkh) == 20)
	runar.Assert(runar.Len(c.QueryId) <= 75)
	runar.Assert(runar.Len(c.JournalRoot) == 32)
	runar.Assert(runar.Len(c.ReceiptHash) == 32)
	runar.Assert(verdictSats >= 1 && payoutSats >= 1)
	e1 := runar.Cat(runar.Num2Bin(runar.Len(c.ProverPkh), 1), c.ProverPkh)
	e2 := runar.Cat(runar.Num2Bin(runar.Len(c.QueryId), 1), c.QueryId)
	e3 := runar.Cat(runar.Num2Bin(runar.Len(c.JournalRoot), 1), c.JournalRoot)
	e4 := runar.Cat(runar.Num2Bin(runar.Len(c.ReceiptHash), 1), c.ReceiptHash)
	vstate := runar.Cat(e4, runar.Cat(e3, runar.Cat(e2, e1)))
	c.AddRawOutput(verdictSats, runar.Cat(c.VerdictCode, runar.Cat(runar.HexToBytes("6a"), vstate)))
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats-verdictSats <= c.MaxFee+1)
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.ProverPkh, runar.HexToBytes("88ac"))))
}
