// NektarFeeVault — hardened rolling-window rate-limited fee vault.
// Enforces extractSequence < 0xffffffff and block-height locktime domain (< 500_000_000)
// so windowCap resets cannot be spoofed via SEQUENCE_FINAL or timestamp locktimes.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type NektarFeeVault struct {
	runar.StatefulSmartContract
	DestPkh       runar.ByteString
	OwnerPkh      runar.ByteString
	PerTxCap      runar.Bigint
	WindowCap     runar.Bigint
	WindowBlocks  runar.Bigint
	MaxFee        runar.Bigint
	SpentInWindow runar.Bigint
	WindowStart   runar.Bigint
}

func (c *NektarFeeVault) Init() {
	c.SpentInWindow = 0
	c.WindowStart = 0
}

func (c *NektarFeeVault) p2pkh(pkh runar.ByteString) runar.ByteString {
	return runar.Cat(runar.HexToBytes("76a914"), runar.Cat(pkh, runar.HexToBytes("88ac")))
}

func (c *NektarFeeVault) Spend(sig runar.Sig, pubKey runar.PubKey, payAmount runar.Bigint, newSpent runar.Bigint, newStart runar.Bigint, contSats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.OwnerPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	lt := runar.ExtractLocktime(c.TxPreimage)
	runar.Assert(runar.ExtractSequence(c.TxPreimage) < 4294967295)
	runar.Assert(lt < 500000000)
	runar.Assert(payAmount >= 1 && contSats >= 1)
	expired := lt >= c.WindowStart+c.WindowBlocks
	runar.Assert((expired && newSpent == payAmount && newStart == lt) ||
		(!expired && newSpent == c.SpentInWindow+payAmount && newStart == c.WindowStart))
	c.SpentInWindow = newSpent
	c.WindowStart = newStart
	runar.Assert(c.SpentInWindow <= c.WindowCap)
	runar.Assert(payAmount <= c.PerTxCap)
	runar.Assert(runar.ExtractAmount(c.TxPreimage)-contSats-payAmount <= c.MaxFee+1)
	c.AddOutput(contSats, c.DestPkh, c.OwnerPkh, c.PerTxCap, c.WindowCap, c.WindowBlocks, c.MaxFee, c.SpentInWindow, c.WindowStart)
	c.AddRawOutput(payAmount, c.p2pkh(c.DestPkh))
}
