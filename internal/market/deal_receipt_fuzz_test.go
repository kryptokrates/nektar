package market

import "testing"

func FuzzDecodeDealReceiptToken(f *testing.F) {
	f.Add("")
	f.Add("nektardr1.")
	f.Add("nektardr1.oQ==")
	f.Add("invalid.token")
	f.Fuzz(func(t *testing.T, tok string) {
		dr, err := DecodeDealReceiptToken(tok)
		if err == nil && dr != nil {
			_ = VerifyDealReceipt(*dr, false)
		}
	})
}
