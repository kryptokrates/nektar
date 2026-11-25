// NektarCheckpointBond — hardened SLA checkpoint bond covenant.
// Enforces extractSequence < 0xffffffff and block-height locktime domain (< 500_000_000)
// on commit, seize, and close so consensus nLockTime cannot be bypassed via SEQUENCE_FINAL
// or 1985 timestamp confusion.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarCheckpointBond struct {
	runar.StatefulSmartContract
	OperatorPkh       runar.ByteString
	BondReleaseHeight runar.Bigint
	MaxFee            runar.Bigint
	EpochLen          runar.Bigint
	N                 runar.Bigint
	LastRoot          runar.ByteString
	Deadline          runar.Bigint
	Seized            runar.Bigint
}

func (c *NektarCheckpointBond) Init() {
	c.N = 0
	c.LastRoot = ""
	c.Seized = 0
}

func (c *NektarCheckpointBond) p2pkh(pkh runar.ByteString) runar.ByteString {
	return runar.Cat(runar.HexToBytes("76a914"), runar.Cat(pkh, runar.HexToBytes("88ac")))
}

func (c *NektarCheckpointBond) Commit(sig runar.Sig, pubKey runar.PubKey, newRoot runar.ByteString, bondSats runar.Bigint, newDeadline runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OperatorPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(c.Seized == 0)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt < c.Deadline)
	runar.Assert(newDeadline-lt == c.EpochLen)
	runar.Assert(newDeadline < 500000000)
	runar.Assert(bondSats >= 1)
	c.N += 1
	c.LastRoot = newRoot
	c.Deadline = newDeadline
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-bondSats <= c.MaxFee+1)
	c.AddOutput(bondSats, c.OperatorPkh, c.BondReleaseHeight, c.MaxFee, c.EpochLen, c.N, c.LastRoot, c.Deadline, c.Seized)
}

func (c *NektarCheckpointBond) Seize(payTo runar.ByteString, payoutSats runar.Bigint) {
	runar.Assert(c.Seized == 0)
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.Deadline)
	runar.Assert(payoutSats >= 1)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats <= c.MaxFee+1)
	c.Seized = 1
	c.AddRawOutput(payoutSats, payTo)
	c.AddOutput(1, c.OperatorPkh, c.BondReleaseHeight, c.MaxFee, c.EpochLen, c.N, c.LastRoot, c.Deadline, c.Seized)
}

func (c *NektarCheckpointBond) Close(sig runar.Sig, pubKey runar.PubKey, payoutSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OperatorPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(lt >= c.BondReleaseHeight)
	runar.Assert(payoutSats >= 1)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-payoutSats <= c.MaxFee+1)
	c.AddRawOutput(payoutSats, c.p2pkh(c.OperatorPkh))
}
