package memory

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestNektarMemoryFullEndToEndLifecycle(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	port, err := chain.Open(ctx, chain.Config{Network: "memory", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{
		DatalogRules: []string{
			`reachable(X, Y) :- trusts(X, Y).`,
			`reachable(X, Z) :- trusts(X, Y), reachable(Y, Z).`,
		},
		VerifyBaseURL: "https://nektar.local/v1/verify",
	})
	if err != nil {
		t.Fatal(err)
	}

	// 1. Remember plain fragments + anchor on testchain + SPV verify!
	r1, err := svc.Remember(ctx, RememberInput{
		Shard: "ops",
		Text:  "BSV Chronicle activated at block 943816.",
		Level: 0,
	})
	if err != nil {
		t.Fatalf("Remember r1: %v", err)
	}
	r2, err := svc.Remember(ctx, RememberInput{
		Shard: "ops",
		Text:  `trusts("alice", "bob")` + "\n" + `trusts("bob", "carol")`,
		Level: 2,
	})
	if err != nil {
		t.Fatalf("Remember r2: %v", err)
	}
	anchor, err := svc.AnchorShard(ctx, "ops")
	if err != nil || anchor == nil || !anchor.Settled {
		t.Fatalf("AnchorShard failed: anchor=%+v err=%v", anchor, err)
	}

	// 2. Cedar clearance isolation: level-1 agent sees r1 (level 0) but NOT r2 (level 2)!
	lowRec, err := svc.Recall(ctx, RecallInput{
		Shard:     "ops",
		Principal: policy.Principal{Type: "Agent", ID: "intern", Level: 1},
		VerifySPV: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(lowRec) != 1 || lowRec[0].Seq != r1.Seq || lowRec[0].SPV == nil || !lowRec[0].SPV.OK || !lowRec[0].SPV.Settled {
		t.Fatalf("expected 1 SPV-verified level-0 fragment, got %+v", lowRec)
	}

	// 3. Borg group + BRC-369 encryption + revocation epoch rotation!
	bobKey, _ := ec.NewPrivateKey()
	bobPub := hex.EncodeToString(bobKey.PubKey().Compressed())
	ownerPub := port.IdentityPubHex()
	if _, err := svc.Groups().CreateGroup("sec-team", "Security Team", ownerPub); err != nil {
		t.Fatal(err)
	}
	if err := svc.Groups().AddMember("sec-team", bobPub, "reader"); err != nil {
		t.Fatal(err)
	}
	sec1, err := svc.Remember(ctx, RememberInput{
		Shard:     "vault",
		Text:      "Epoch 1 secret key rotation schedule.",
		BorgGroup: "sec-team",
	})
	if err != nil || !sec1.Encrypted || sec1.BorgEpoch != 1 {
		t.Fatalf("expected epoch 1 encrypted fragment, got %+v err=%v", sec1, err)
	}
	// Bob can read epoch 1.
	bobRec1, _ := svc.Recall(ctx, RecallInput{Shard: "vault", CallerPub: bobPub})
	if len(bobRec1) != 1 || bobRec1[0].Text != "Epoch 1 secret key rotation schedule." {
		t.Fatalf("expected bob to decrypt epoch 1, got %+v", bobRec1)
	}
	// Revoke Bob -> epoch 2!
	newEp, err := svc.Groups().RevokeMember("sec-team", bobPub)
	if err != nil || newEp != 2 {
		t.Fatalf("RevokeMember: ep=%d err=%v", newEp, err)
	}
	sec2, err := svc.Remember(ctx, RememberInput{
		Shard:     "vault",
		Text:      "Epoch 2 post-revocation secret.",
		BorgGroup: "sec-team",
	})
	if err != nil || sec2.BorgEpoch != 2 {
		t.Fatalf("expected epoch 2 encrypted fragment, got %+v err=%v", sec2, err)
	}
	// Revoked Bob must NOT decrypt any group fragments (neither epoch 1 nor epoch 2); owner sees both!
	bobRec2, _ := svc.Recall(ctx, RecallInput{Shard: "vault", CallerPub: bobPub})
	if len(bobRec2) != 0 {
		t.Fatalf("revoked member must not decrypt any fragments after revocation, got: %+v", bobRec2)
	}
	ownerRec, _ := svc.Recall(ctx, RecallInput{Shard: "vault", CallerPub: ownerPub})
	if len(ownerRec) != 2 {
		t.Fatalf("owner expected 2 vault fragments, got %d", len(ownerRec))
	}

	// 4. GroundedAnswer (quote + Mangle + hallucination stripping) -> nektarr1.* receipt -> GREEN verification!
	ga, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{
			{Text: "BSV Chronicle activated at block 943816", Cites: []reasoning.FragmentRef{{Shard: "ops", Seq: r1.Seq}}},
			{Text: "Alice reaches Carol", Literal: `reachable("alice", "carol")`, Cites: []reasoning.FragmentRef{{Shard: "ops", Seq: r2.Seq}}},
			{Text: "Hallucinated unsupported claim", Cites: nil},
		},
		AutoAnchor: true,
	})
	if err != nil {
		t.Fatalf("GroundedAnswer: %v", err)
	}
	if ga.Ungrounded || !strings.HasPrefix(ga.ReceiptToken, "nektarr1.") || !strings.Contains(ga.Footer, "verify: https://nektar.local/v1/verify/") {
		t.Fatalf("unexpected GroundedAnswer result: %+v", ga)
	}
	if strings.Contains(ga.Answer, "Hallucinated") {
		t.Fatalf("hallucination was not stripped: %q", ga.Answer)
	}

	ver, err := svc.VerifyReceipt(ctx, ga.ReceiptToken)
	if err != nil {
		t.Fatal(err)
	}
	if ver.Verdict != "green" || ver.CitationsMined != 2 || !ver.ReceiptAnchored {
		t.Fatalf("expected green anchored verification with 2 mined citations, got %+v", ver)
	}

	// 5. Honesty check: all-uncited claims produce Ungrounded=true and NO receipt token.
	ungr, err := svc.GroundedAnswer(ctx, GroundedAnswerInput{
		Claims: []reasoning.Claim{{Text: "Pure hallucination"}},
	})
	if err != nil || !ungr.Ungrounded || ungr.ReceiptToken != "" || ungr.Footer != "" {
		t.Fatalf("expected ungrounded result with empty footer, got %+v err=%v", ungr, err)
	}

	// 6. Revision correction loop (AGT-006): superseding r1 flags our receipt!
	r3, err := svc.Remember(ctx, RememberInput{
		Shard:      "ops",
		Text:       "BSV Chronicle activated at block 943816 (revised note).",
		Supersedes: r1.Seq,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(r3.ImpactedReceipts) != 1 || r3.ImpactedReceipts[0] != ga.ReceiptHash {
		t.Fatalf("expected revision to flag receipt %s, got %+v", ga.ReceiptHash, r3.ImpactedReceipts)
	}

	// 7. CHIRP BRC-167 shard pack export -> import!
	pack, err := svc.ExportShardPack(ctx, "ops")
	if err != nil || !strings.HasPrefix(pack.ChirpURL, "chirp://") {
		t.Fatalf("ExportShardPack: %+v err=%v", pack, err)
	}
	imported, err := svc.ImportShardPack(ctx, pack.ChirpURL, "ops-mirror")
	if err != nil || imported != 3 {
		t.Fatalf("ImportShardPack: imported=%d err=%v", imported, err)
	}
}
