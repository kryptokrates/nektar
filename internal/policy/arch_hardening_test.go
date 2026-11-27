package policy

import "testing"

func TestAuthorizeFailsClosedOnDiagErrors(t *testing.T) {
	// Policy referencing a non-existent attribute triggers a Cedar evaluation diagnostic error.
	badAttrPolicy := `
permit (principal, action == Action::"read", resource)
when { principal.nonexistent_attr == 1 };
permit (principal, action == Action::"read", resource)
when { principal.level >= resource.level };
`
	dec, err := Authorize(badAttrPolicy, Principal{Type: "Agent", ID: "a", Level: 10}, "read", Resource{
		Type: "Fragment", ID: "f1", Level: 0, Shard: "s", Class: "c",
	})
	if err == nil || dec.Allow {
		t.Fatalf("expected fail-closed error on Cedar diag.Errors, got allow=%v err=%v", dec.Allow, err)
	}
}

func TestProveLevelIsolationRejectsInvalidRange(t *testing.T) {
	if ok, _, _, err := ProveLevelIsolation(DefaultPolicySet, 5, 5); err == nil || ok {
		t.Fatalf("expected error when minLevel == fragmentLevel, got ok=%v err=%v", ok, err)
	}
	if ok, _, _, err := ProveLevelIsolation(DefaultPolicySet, 0, maxLevelIsolationSpan+1); err == nil || ok {
		t.Fatalf("expected error when span exceeds maxLevelIsolationSpan, got ok=%v err=%v", ok, err)
	}
}
