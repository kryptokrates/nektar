package market

import (
	"strings"
	"testing"
)

func TestVerifyBRC105ChallengeIdentity_BlocksMITMOmittedSellerPub(t *testing.T) {
	dirSeller := "02" + strings.Repeat("aa", 32)
	attackerPayee := "03" + strings.Repeat("ff", 32)
	separatePayout := "02" + strings.Repeat("bb", 32)

	// 1. MITM omits optional JSON sellerPub and substitutes attackerPayee -> MUST fail closed!
	if err := verifyBRC105ChallengeIdentity(dirSeller, "", attackerPayee); err == nil {
		t.Fatal("expected MITM with omitted sellerPub and foreign payeePub to fail closed")
	}

	// 2. Omitted sellerPub with matching payeePub -> allowed.
	if err := verifyBRC105ChallengeIdentity(dirSeller, "", dirSeller); err != nil {
		t.Fatalf("expected matching payeePub with omitted sellerPub to pass: %v", err)
	}

	// 3. Declared sellerPub matches directory identity (even with separate wallet payout key) -> allowed.
	if err := verifyBRC105ChallengeIdentity(dirSeller, dirSeller, separatePayout); err != nil {
		t.Fatalf("expected matching declared sellerPub to pass: %v", err)
	}

	// 4. Declared sellerPub mismatches directory identity -> MUST fail closed!
	if err := verifyBRC105ChallengeIdentity(dirSeller, attackerPayee, dirSeller); err == nil {
		t.Fatal("expected mismatched declared sellerPub to fail closed")
	}

	// 5. Empty expectedSellerPub (unpinned direct call) -> allowed.
	if err := verifyBRC105ChallengeIdentity("", "", attackerPayee); err != nil {
		t.Fatalf("expected unpinned direct call to pass: %v", err)
	}
}
