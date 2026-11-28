package reputation

import "testing"

func FuzzDecodeAttestationToken(f *testing.F) {
	f.Add("")
	f.Add("nektaratt1.")
	f.Add("nektaratt1.oQ==")
	f.Add("invalid.token")
	f.Fuzz(func(t *testing.T, tok string) {
		att, err := DecodeAttestationToken(tok)
		if err == nil && att != nil {
			_, _ = VerifyAttestation(att, nil, nil)
		}
	})
}
