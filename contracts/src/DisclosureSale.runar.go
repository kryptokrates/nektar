// DisclosureSale — GOAL-v7 V4: hardened atomic payment-for-disclosure escrow.
// Security invariants enforced under consensus:
//   - F-05: Buy/BuyGated verify native BRC-369 keyed.Commitment(wrapped):
//           len(wrapped) == 32 AND sha256("metanet keyed content commitment v1" || wrapped) == hWrapped
//           so hWrapped == KeyCommitment directly under Bitcoin Script consensus.
//   - F-06: Buy() asserts len(verdictOutpoint) == 0 so verdict-gated escrows cannot bypass BuyGated().
//   - F-02: Refund() enforces extractSequence < 0xffffffff AND matching locktime domain (< or >= 500_000_000).
//   - F-19: unsigned varint length encoding in out() + 20-byte PKH and positive changeSats checks.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type DisclosureSale struct {
	runar.SmartContract
	SellerPkh       runar.ByteString `runar:"readonly"`
	Price           runar.Bigint     `runar:"readonly"`
	HWrapped        runar.Sha256Digest `runar:"readonly"`
	RefundAfter     runar.Bigint     `runar:"readonly"`
	MaxFee          runar.Bigint     `runar:"readonly"`
	DepositorPkh    runar.ByteString `runar:"readonly"`
	VerdictOutpoint runar.ByteString `runar:"readonly"`
}

func (c *DisclosureSale) p2pkh(pkh runar.ByteString) runar.ByteString {
	return runar.Cat(runar.HexToBytes("76a914"), runar.Cat(pkh, runar.HexToBytes("88ac")))
}

func (c *DisclosureSale) out(sats runar.Bigint, script runar.ByteString) runar.ByteString {
	sl := runar.Len(script)
	var slB runar.ByteString = ""
	if sl < 253 {
		slB = runar.Left(runar.Num2Bin(sl, 2), 1)
	} else {
		slB = runar.Cat(runar.HexToBytes("fd"), runar.Left(runar.Num2Bin(sl, 4), 2))
	}
	return runar.Cat(runar.Num2Bin(sats, 8), runar.Cat(slB, script))
}

// Buy performs atomic ungated reveal of the 32-byte BRC-369 content key `wrapped`
// whose BRC-369 keyed.Commitment(wrapped) equals `HWrapped`. Refused if VerdictOutpoint is set.
func (c *DisclosureSale) Buy(wrapped runar.ByteString, changeSats runar.Bigint, txPreimage runar.SigHashPreimage) {
	runar.Assert(runar.CheckPreimage(txPreimage))
	runar.Assert(runar.Len(c.VerdictOutpoint) == 0)
	runar.Assert(runar.Len(c.SellerPkh) == 20 && runar.Len(c.DepositorPkh) == 20)
	runar.Assert(runar.Len(wrapped) == 32)
	runar.Assert(runar.Sha256(runar.Cat(runar.HexToBytes("6d6574616e6574206b6579656420636f6e74656e7420636f6d6d69746d656e74207631"), wrapped)) == c.HWrapped)
	runar.Assert(changeSats >= 1)
	runar.Assert(runar.ExtractAmount(txPreimage)-c.Price-changeSats <= c.MaxFee)
	runar.Assert(runar.Hash256(runar.Cat(
		c.out(c.Price, c.p2pkh(c.SellerPkh)),
		c.out(changeSats, c.p2pkh(c.DepositorPkh)))) == runar.ExtractOutputHash(txPreimage))
}

// BuyGated performs atomic ZK-verdict-gated reveal: requires input1 == committed ClaimVerdict outpoint.
func (c *DisclosureSale) BuyGated(wrapped runar.ByteString, changeSats runar.Bigint, prevouts runar.ByteString, txPreimage runar.SigHashPreimage) {
	runar.Assert(runar.CheckPreimage(txPreimage))
	runar.Assert(runar.Len(c.VerdictOutpoint) == 36)
	runar.Assert(runar.Len(c.SellerPkh) == 20 && runar.Len(c.DepositorPkh) == 20)
	runar.Assert(runar.Len(wrapped) == 32)
	runar.Assert(runar.Sha256(runar.Cat(runar.HexToBytes("6d6574616e6574206b6579656420636f6e74656e7420636f6d6d69746d656e74207631"), wrapped)) == c.HWrapped)
	runar.Assert(runar.Hash256(prevouts) == runar.ExtractHashPrevouts(txPreimage))
	runar.Assert(prevouts == runar.Cat(runar.ExtractOutpoint(txPreimage), c.VerdictOutpoint))
	runar.Assert(changeSats >= 1)
	runar.Assert(runar.ExtractAmount(txPreimage)-c.Price-changeSats <= c.MaxFee)
	runar.Assert(runar.Hash256(runar.Cat(
		c.out(c.Price, c.p2pkh(c.SellerPkh)),
		c.out(changeSats, c.p2pkh(c.DepositorPkh)))) == runar.ExtractOutputHash(txPreimage))
}

// Refund allows the depositor to reclaim an unrevealed escrow after RefundAfter (consensus locktime enforced).
func (c *DisclosureSale) Refund(sig runar.Sig, pubKey runar.PubKey, changeSats runar.Bigint, txPreimage runar.SigHashPreimage) {
	runar.Assert(runar.CheckPreimage(txPreimage))
	runar.Assert(runar.Len(c.DepositorPkh) == 20)
	runar.Assert(runar.Hash160(pubKey) == c.DepositorPkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	lt := runar.ExtractLocktime(txPreimage)
	runar.Assert(runar.ExtractSequence(txPreimage) < 4294967295)
	if c.RefundAfter < 500000000 {
		runar.Assert(lt < 500000000)
		runar.Assert(lt >= c.RefundAfter)
	} else {
		runar.Assert(lt >= 500000000)
		runar.Assert(lt >= c.RefundAfter)
	}
	runar.Assert(changeSats >= 1)
	runar.Assert(runar.ExtractAmount(txPreimage)-changeSats <= c.MaxFee)
	runar.Assert(runar.Hash256(c.out(changeSats, c.p2pkh(c.DepositorPkh))) == runar.ExtractOutputHash(txPreimage))
}
