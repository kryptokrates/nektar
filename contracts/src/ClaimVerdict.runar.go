// ClaimVerdict — GOAL-v9 §4.5 hardened consumer hook.
// F-21: Authorize() requires PayeePkh signature so third parties cannot destroy a
// pinned ClaimVerdict UTXO standalone for 1 satoshi and DoS DisclosureSale.BuyGated.
package contracts

import runar "github.com/icellan/runar/packages/runar-go"

type ClaimVerdict struct {
	runar.StatefulSmartContract
	ReceiptHash runar.ByteString
	JournalRoot runar.ByteString
	QueryId     runar.ByteString
	PayeePkh    runar.ByteString
}

func (c *ClaimVerdict) Authorize(sig runar.Sig, pubKey runar.PubKey, paySats runar.Bigint) {
	runar.Assert(runar.Hash160(pubKey) == c.PayeePkh)
	runar.Assert(runar.CheckSig(sig, pubKey))
	runar.Assert(paySats >= 1)
	c.AddRawOutput(paySats, runar.Cat(runar.HexToBytes("76a914"), runar.Cat(c.PayeePkh, runar.HexToBytes("88ac"))))
}
