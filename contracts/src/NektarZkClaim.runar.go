// NektarZkClaim — GOAL-v9 §4.5: hardened optimistic ZK verdict claim+bond covenant.
// Security invariants enforced under consensus:
//   - F-01: responseDeadline uses the vault-arg pattern (newDeadline - lt == window)
//           so the compiler emits OP_SUB + OP_NUMEQUAL (never OP_CAT).
//   - F-02: every locktime check enforces extractSequence < 0xffffffff AND lt < 500_000_000.
//   - F-03: proveStage pins stageCodeHash (hash256(stageCode) == c.StageCodeHash)
//           and len(stageOutpoint) == 36.
//   - F-04: explicit turn states (0=open, 1=awaiting_reveal [prover], 2=awaiting_bisect
//           [challenger], 3=awaiting_leaf [prover]); ClaimTimeout sweeps on status==2,
//           SlashTimeout sweeps on status==1||3; Reveal resets responseDeadline and
//           rejects zero midCommit.
//   - F-19: fixed pushdata bounds before num2bin(len(x), 1).
//   - F-20: Challenge() locks 2*bondSats (matching challenger bond).
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarZkClaim struct {
	runar.StatefulSmartContract
	Status           runar.Bigint
	ReceiptHash      runar.Sha256Digest
	JournalRoot      runar.ByteString
	JournalSize      runar.Bigint
	QueryId          runar.ByteString
	BondSats         runar.Bigint
	Deadline         runar.Bigint
	ResponseDeadline runar.Bigint
	ChallengerPkh    runar.ByteString
	BisectLo         runar.Bigint
	BisectHi         runar.Bigint
	MidCommit        runar.Sha256Digest
	ProverPkh        runar.ByteString
	MaxFee           runar.Bigint
	Window           runar.Bigint
	Stages           runar.Bigint
	VerdictCode      runar.ByteString
	AnchorOutpoint   runar.ByteString
	StageCodeHash    runar.Sha256Digest
}

func (c *NektarZkClaim) Settle(sig runar.Sig, pubKey runar.PubKey, prevouts runar.ByteString, payoutSats runar.Bigint, verdictSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 0)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.Deadline)
	runar.Assert(runar.Len(c.AnchorOutpoint) == 0)
	runar.Assert(runar.Hash256(prevouts) == runar.ExtractHashPrevouts(c.TxPreimage))
	runar.Assert(prevouts == runar.ExtractOutpoint(c.TxPreimage))
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

func (c *NektarZkClaim) SettleAnchored(sig runar.Sig, pubKey runar.PubKey, prevouts runar.ByteString, anchorSats runar.Bigint, anchorScript runar.ByteString, payoutSats runar.Bigint, verdictSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 0)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.Deadline)
	runar.Assert(runar.Len(c.AnchorOutpoint) == 36)
	runar.Assert(runar.Hash256(prevouts) == runar.ExtractHashPrevouts(c.TxPreimage))
	runar.Assert(prevouts == runar.Cat(runar.ExtractOutpoint(c.TxPreimage), c.AnchorOutpoint))
	runar.Assert(runar.Len(c.ProverPkh) == 20)
	runar.Assert(runar.Len(c.QueryId) <= 75)
	runar.Assert(runar.Len(c.JournalRoot) == 32)
	runar.Assert(runar.Len(c.ReceiptHash) == 32)
	runar.Assert(anchorSats >= 1 && verdictSats >= 1 && payoutSats >= 1)
	c.AddRawOutput(anchorSats, anchorScript)
	e1 := runar.Cat(runar.Num2Bin(runar.Len(c.ProverPkh), 1), c.ProverPkh)
	e2 := runar.Cat(runar.Num2Bin(runar.Len(c.QueryId), 1), c.QueryId)
	e3 := runar.Cat(runar.Num2Bin(runar.Len(c.JournalRoot), 1), c.JournalRoot)
	e4 := runar.Cat(runar.Num2Bin(runar.Len(c.ReceiptHash), 1), c.ReceiptHash)
	vstate := runar.Cat(e4, runar.Cat(e3, runar.Cat(e2, e1)))
	c.AddRawOutput(verdictSats, runar.Cat(c.VerdictCode, runar.Cat(runar.HexToBytes("6a"), vstate)))
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats-verdictSats-anchorSats <= c.MaxFee+1)
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.ProverPkh, runar.HexToBytes("88ac"))))
}

func (c *NektarZkClaim) Challenge(sig runar.Sig, pubKey runar.PubKey, newDeadline runar.Bigint) {
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 0)
	runar.Assert(c.Stages >= 1)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt < c.Deadline)
	runar.Assert(newDeadline-lt == c.Window)
	runar.Assert(newDeadline < 500000000)
	if c.Stages > 1 {
		c.Status = 1
	} else {
		c.Status = 3
	}
	c.ChallengerPkh = runar.Hash160(pubKey)
	c.BisectLo = 0
	c.BisectHi = c.Stages
	c.ResponseDeadline = newDeadline
	c.AddOutput(c.BondSats*2, c.Status, c.ReceiptHash, c.JournalRoot, c.JournalSize, c.QueryId, c.BondSats, c.Deadline, c.ResponseDeadline, c.ChallengerPkh, c.BisectLo, c.BisectHi, c.MidCommit, c.ProverPkh, c.MaxFee, c.Window, c.Stages, c.VerdictCode, c.AnchorOutpoint, c.StageCodeHash)
}

func (c *NektarZkClaim) Reveal(sig runar.Sig, pubKey runar.PubKey, mid runar.Sha256Digest, newDeadline runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 1)
	runar.Assert(c.BisectHi-c.BisectLo > 1)
	runar.Assert(mid != runar.HexToBytes("0000000000000000000000000000000000000000000000000000000000000000"))
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(newDeadline-lt == c.Window)
	runar.Assert(newDeadline < 500000000)
	c.Status = 2
	c.MidCommit = mid
	c.ResponseDeadline = newDeadline
	c.AddOutput(c.BondSats*2, c.Status, c.ReceiptHash, c.JournalRoot, c.JournalSize, c.QueryId, c.BondSats, c.Deadline, c.ResponseDeadline, c.ChallengerPkh, c.BisectLo, c.BisectHi, c.MidCommit, c.ProverPkh, c.MaxFee, c.Window, c.Stages, c.VerdictCode, c.AnchorOutpoint, c.StageCodeHash)
}

func (c *NektarZkClaim) Bisect(sig runar.Sig, pubKey runar.PubKey, takeUpper runar.Bigint, newDeadline runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ChallengerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 2)
	runar.Assert(c.MidCommit != runar.HexToBytes("0000000000000000000000000000000000000000000000000000000000000000"))
	runar.Assert(takeUpper == 0 || takeUpper == 1)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(newDeadline-lt == c.Window)
	runar.Assert(newDeadline < 500000000)
	lo0 := c.BisectLo
	hi0 := c.BisectHi
	mid := (lo0 + hi0) / 2
	rem := (mid - lo0) + takeUpper*(hi0+lo0-2*mid)
	c.BisectLo = lo0 + takeUpper*(mid-lo0)
	c.BisectHi = mid + takeUpper*(hi0-mid)
	if rem > 1 {
		c.Status = 1
	} else {
		c.Status = 3
	}
	c.MidCommit = runar.HexToBytes("0000000000000000000000000000000000000000000000000000000000000000")
	c.ResponseDeadline = newDeadline
	c.AddOutput(c.BondSats*2, c.Status, c.ReceiptHash, c.JournalRoot, c.JournalSize, c.QueryId, c.BondSats, c.Deadline, c.ResponseDeadline, c.ChallengerPkh, c.BisectLo, c.BisectHi, c.MidCommit, c.ProverPkh, c.MaxFee, c.Window, c.Stages, c.VerdictCode, c.AnchorOutpoint, c.StageCodeHash)
}

func (c *NektarZkClaim) ProveStage(sig runar.Sig, pubKey runar.PubKey, prevouts runar.ByteString, stageOutpoint runar.ByteString, payoutSats runar.Bigint, stageSats runar.Bigint, stageCode runar.ByteString, stageState runar.ByteString) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 3)
	runar.Assert(c.BisectHi-c.BisectLo <= 1)
	runar.Assert(runar.Len(stageOutpoint) == 36)
	runar.Assert(runar.Hash256(stageCode) == c.StageCodeHash)
	runar.Assert(runar.Hash256(prevouts) == runar.ExtractHashPrevouts(c.TxPreimage))
	runar.Assert(prevouts == runar.Cat(runar.ExtractOutpoint(c.TxPreimage), stageOutpoint))
	runar.Assert(payoutSats >= 1 && stageSats >= 1)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats <= c.MaxFee+1)
	c.AddRawOutput(stageSats, runar.Cat(stageCode, runar.Cat(runar.HexToBytes("6a"), stageState)))
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.ProverPkh, runar.HexToBytes("88ac"))))
}

// ClaimTimeout lets the prover sweep both bonds if the challenger goes silent on their bisect turn (status == 2).
func (c *NektarZkClaim) ClaimTimeout(sig runar.Sig, pubKey runar.PubKey, payoutSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ProverPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 2)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.ResponseDeadline)
	runar.Assert(payoutSats >= 1)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats <= c.MaxFee+1)
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.ProverPkh, runar.HexToBytes("88ac"))))
}

// SlashTimeout lets the challenger sweep both bonds if the prover goes silent on reveal (1) or proveStage (3).
func (c *NektarZkClaim) SlashTimeout(sig runar.Sig, pubKey runar.PubKey, payoutSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.ChallengerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Status == 1 || c.Status == 3)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.ResponseDeadline)
	runar.Assert(payoutSats >= 1)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats <= c.MaxFee+1)
	c.AddRawOutput(payoutSats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.ChallengerPkh, runar.HexToBytes("88ac"))))
}
