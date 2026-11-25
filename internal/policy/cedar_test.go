package policy

import "testing"

func TestCedarGoPolicyAndLevelIsolation(t *testing.T) {
	pc, err := Canonicalize("default", DefaultPolicySet)
	if err != nil || len(pc.Hash) != 64 {
		t.Fatalf("Canonicalize: pc=%+v err=%v", pc, err)
	}
	allow, err := Authorize(DefaultPolicySet, Principal{Type: "Agent", ID: "a1", Level: 2}, "read", Resource{
		Type: "Fragment", ID: "f1", Level: 2, Shard: "alpha", Class: "secret",
	})
	if err != nil || !allow.Allow {
		t.Fatalf("expected allow at equal level, got %+v err=%v", allow, err)
	}
	deny, err := Authorize(DefaultPolicySet, Principal{Type: "Agent", ID: "a1", Level: 1}, "read", Resource{
		Type: "Fragment", ID: "f1", Level: 2, Shard: "alpha", Class: "secret",
	})
	if err != nil || deny.Allow {
		t.Fatalf("expected deny below level, got %+v err=%v", deny, err)
	}
	ok, checked, violations, err := ProveLevelIsolation(DefaultPolicySet, 0, 5)
	if err != nil || !ok || checked != 10 || violations != 0 {
		t.Fatalf("ProveLevelIsolation: ok=%v checked=%d violations=%d err=%v", ok, checked, violations, err)
	}
}
