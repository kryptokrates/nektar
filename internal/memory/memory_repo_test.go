package memory

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/store"
)

func TestPerRepoGenesisIsolationAndL1BoundMPE(t *testing.T) {
	ctx := context.Background()
	svc, port := newTestMemoryService(t)

	// 1. Commit to two distinct Memory Repos (L1-bound MPE is now unconditional on every fragment!).
	rOps, err := svc.Remember(ctx, RememberInput{
		Shard:         "ops",
		Path:          "runbooks/restart.md",
		Text:          "Always reconcile pending anchors before rotating owner keys.",
		CommitMessage: "Add anchor restart runbook",
	})
	if err != nil {
		t.Fatalf("Remember(ops): %v", err)
	}
	if rOps.MPERoot == "" || rOps.CommitHash == "" || rOps.TreeHash == "" {
		t.Fatalf("expected MPERoot, CommitHash, TreeHash; got %+v", rOps)
	}

	rBSV, err := svc.Remember(ctx, RememberInput{
		Shard:         "bsv",
		Path:          "specs/chronicle.md",
		Text:          "NektarAnchor uses OP_PUSH_TX singleton continuation.",
		CommitMessage: "Add Chronicle spec",
	})
	if err != nil {
		t.Fatalf("Remember(bsv): %v", err)
	}

	// Anchor both repos and verify they derive DISTINCT on-chain GenesisOwner ShardIDs (fixing Gap 1!).
	aOps0, err := svc.AnchorShard(ctx, "ops")
	if err != nil {
		t.Fatalf("AnchorShard(ops, 0): %v", err)
	}
	aBSV0, err := svc.AnchorShard(ctx, "bsv")
	if err != nil {
		t.Fatalf("AnchorShard(bsv, 0): %v", err)
	}
	if aOps0.ShardID == aBSV0.ShardID {
		t.Fatalf("GAP 1 regression: ops and bsv collided on identical on-chain ShardID %s", aOps0.ShardID)
	}

	// Second commit & anchor on "ops" MUST spend aOps0 via NektarAnchor.Append() and keep the exact same ShardID!
	if _, err := svc.Remember(ctx, RememberInput{
		Shard: "ops",
		Path:  "runbooks/backup.md",
		Text:  "Export .nkt CHIRP capsules hourly.",
	}); err != nil {
		t.Fatalf("Remember(ops #2): %v", err)
	}
	aOps1, err := svc.AnchorShard(ctx, "ops")
	if err != nil {
		t.Fatalf("AnchorShard(ops, 1): %v", err)
	}
	if aOps1.ShardID != aOps0.ShardID || aOps1.N != 1 {
		t.Fatalf("ops append anchor mismatch: n=%d shardId=%s want=%s", aOps1.N, aOps1.ShardID, aOps0.ShardID)
	}

	// 2. Verify L1-bound MPE selective disclosure offline (fixing Gap 2!).
	fp, err := svc.ProveFragmentFields(ctx, "ops", rOps.Seq, []string{"source", "path", "modelId"})
	if err != nil {
		t.Fatalf("ProveFragmentFields: %v", err)
	}
	if !fp.Anchored || fp.ProofBundle == nil {
		t.Fatalf("expected L1-anchored MPE proof with ProofBundle, got anchored=%v bundle=%v", fp.Anchored, fp.ProofBundle != nil)
	}
	vr := contracts.VerifyAnchoredMPEProof(ctx, fp.HDP, fp.CiphertextHash, fp.ContentSHA256, fp.FieldsRoot, fp.CommitHash, fp.TransitionHash, fp.Proof, *fp.ProofBundle, port.Tracker())
	if !vr.OK || !vr.Settled {
		t.Fatalf("VerifyAnchoredMPEProof failed: %+v", vr)
	}
	// Tampering any disclosed field MUST fail closed!
	tampered := *fp.Proof
	tamperedFields := append([]core.DisclosedField(nil), fp.Proof.Fields...)
	tamperedFields[0].Value = []byte("forged")
	tampered.Fields = tamperedFields
	if badVR := contracts.VerifyAnchoredMPEProof(ctx, fp.HDP, fp.CiphertextHash, fp.ContentSHA256, fp.FieldsRoot, fp.CommitHash, fp.TransitionHash, &tampered, *fp.ProofBundle, port.Tracker()); badVR.OK {
		t.Fatal("VerifyAnchoredMPEProof accepted tampered disclosed field!")
	}

	// VerifyShardChain must pass cleanly on v2 MPE-bound leaves!
	rep, err := svc.VerifyShardChain("ops")
	if err != nil || !rep.OK {
		t.Fatalf("VerifyShardChain(ops) failed: err=%v rep=%+v", err, rep)
	}
	_ = rBSV
}

func TestVolumetricChirpBranchMergeCapsuleAndCrossNodeFork(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()

	openNode := func(name string) *Service {
		t.Helper()
		dir := t.TempDir()
		port, err := chain.Open(ctx, chain.Config{
			Network:           "memory",
			DataDir:           dir,
			SharedMemoryChain: shared,
		})
		if err != nil {
			t.Fatalf("open port %s: %v", name, err)
		}
		t.Cleanup(func() { _ = port.Close() })
		db, err := store.Open(filepath.Join(dir, "nektar.sqlite"))
		if err != nil {
			t.Fatalf("open db %s: %v", name, err)
		}
		t.Cleanup(func() { _ = db.Close() })
		svc, err := New(db, port, bstack.NewGroupRegistry(), bstack.NewMemoryObjectStore(), Config{})
		if err != nil {
			t.Fatalf("new memory %s: %v", name, err)
		}
		return svc
	}

	seller := openNode("seller")
	buyer := openNode("buyer")

	// 1. Seller commits a text fact + a volumetric >64 KiB binary PDF file, anchors on BSV!
	c1, err := seller.Remember(ctx, RememberInput{
		Shard:         "research",
		Path:          "facts/spv-notes.md",
		Text:          "SPV proofs verify inclusion against block headers in O(log N).",
		CommitMessage: "Add SPV notes",
	})
	if err != nil {
		t.Fatalf("seller Remember text: %v", err)
	}
	pdfBytes := bytes.Repeat([]byte("%PDF-1.7 BSV AnchorChain Volumetric Blob\n"), 2000)
	c2, err := seller.Remember(ctx, RememberInput{
		Shard:         "research",
		Path:          "papers/anchorchain.pdf",
		MediaType:     "application/pdf",
		Blob:          pdfBytes,
		CommitMessage: "Attach AnchorChain PDF paper",
	})
	if err != nil {
		t.Fatalf("seller Remember blob: %v", err)
	}
	if c2.ChirpURL == "" {
		t.Fatal("expected volumetric CHIRP URL for binary PDF")
	}
	if _, err := seller.AnchorShard(ctx, "research"); err != nil {
		t.Fatalf("seller AnchorShard: %v", err)
	}

	// 2. Fork branch "agent/codex", commit a new finding, diff, and 2-parent merge into "main"!
	br, err := seller.CreateBranch(ctx, "research", "agent/codex", "main")
	if err != nil || br.Branch != "agent/codex" {
		t.Fatalf("CreateBranch: err=%v br=%+v", err, br)
	}
	c3, err := seller.Remember(ctx, RememberInput{
		Shard:         "research",
		Branch:        "agent/codex",
		Path:          "facts/codex-finding.md",
		Text:          "Proof-sharding at level 6 deduplicates upper Merkle siblings.",
		CommitMessage: "Codex finding on proof-sharding",
	})
	if err != nil {
		t.Fatalf("seller branch Remember: %v", err)
	}
	merged, err := seller.MergeBranch(ctx, "research", "agent/codex", "main", "Merge Codex proof-sharding findings")
	if err != nil {
		t.Fatalf("MergeBranch: %v", err)
	}
	if len(merged.Parents) != 2 {
		t.Fatalf("expected 2-parent merge commit, got %d parents", len(merged.Parents))
	}
	diff, err := seller.RepoDiff(ctx, "research", c1.CommitHash, merged.CommitHash)
	if err != nil || len(diff) != 2 {
		t.Fatalf("expected 2 added files in diff, got err=%v diff=%+v", err, diff)
	}

	// 3. Verify US 12,730,827 B2 subset proof over seller's anchored batch!
	subProof, err := seller.ProveSubset(ctx, "research", 0, []int64{c1.Seq})
	if err != nil {
		t.Fatalf("ProveSubset: %v", err)
	}
	anchors, _ := seller.DB().ListAnchors("research")
	if err := memrepo.VerifySubsetProof(anchors[0].RootHex, anchors[0].Size, subProof); err != nil {
		t.Fatalf("VerifySubsetProof: %v", err)
	}

	// 4. Export full .nkt Capsule AND incremental delta Capsule since c1!
	expFull, _, rawFull, err := seller.ExportCapsule(ctx, "research", "main", "")
	if err != nil || expFull.CommitCount < 3 || expFull.AnchorCount != 1 {
		t.Fatalf("ExportCapsule full: err=%v res=%+v", err, expFull)
	}
	expDelta, capDelta, _, err := seller.ExportCapsule(ctx, "research", "main", c1.CommitHash)
	if err != nil || expDelta.CommitCount >= expFull.CommitCount || capDelta.BaseCommit != c1.CommitHash {
		t.Fatalf("ExportCapsule delta: err=%v res=%+v", err, expDelta)
	}

	// 5. Buyer imports Seller's .nkt Capsule with mergeIntoMain=true -> creates fork branch + 2-parent Merge Commit citing Seller's UTXO!
	if _, err := buyer.Remember(ctx, RememberInput{
		Shard: "my-brain",
		Path:  "facts/local.md",
		Text:  "Buyer's own preexisting local knowledge.",
	}); err != nil {
		t.Fatalf("buyer Remember: %v", err)
	}
	imp, err := buyer.ImportCapsule(ctx, rawFull, "", "my-brain", "main", true)
	if err != nil || imp.Verification == nil || !imp.Verification.OK || imp.MergeCommitHash == "" {
		t.Fatalf("buyer ImportCapsule: err=%v imp=%+v", err, imp)
	}
	buyerLog, err := buyer.RepoLog(ctx, "my-brain", "main", 10)
	if err != nil || len(buyerLog) < 2 {
		t.Fatalf("buyer RepoLog: err=%v len=%d", err, len(buyerLog))
	}
	head := buyerLog[0]
	if head.CommitHash != imp.MergeCommitHash || len(head.Parents) != 2 {
		t.Fatalf("expected buyer head to be 2-parent merge commit, got %+v", head)
	}
	if head.Parents[1].AnchorTxID != anchors[0].TxID || head.Parents[1].RepoGenesis != anchors[0].ShardID {
		t.Fatalf("buyer merge commit missing seller UTXO lineage: got %+v want txid=%s genesis=%s",
			head.Parents[1], anchors[0].TxID, anchors[0].ShardID)
	}

	// 6. Checkout real .git workspace on disk!
	checkoutDir := filepath.Join(t.TempDir(), "checkout")
	gitRes, err := seller.CheckoutGitWorkspace(ctx, "research", "main", checkoutDir)
	if err != nil || gitRes.GitHeadOID == "" {
		t.Fatalf("CheckoutGitWorkspace: err=%v res=%+v", err, gitRes)
	}
	if _, err := os.Stat(filepath.Join(checkoutDir, "facts", "codex-finding.md")); err != nil {
		t.Fatalf("expected merged file in checked-out git workspace: %v", err)
	}
	_ = c3
}

// TestStrictUTXOLineageFailClosed verifies that NektarAnchor singleton UTXO spend lineage
// (tx_{n-1}:0 -> tx_n:0) is the SOLE accepted provenance path across AnchorShard, VerifyFragment,
// VerifyCapsule, and VerifyShardChain — disconnected UTXO anchors are rejected everywhere!
func TestStrictUTXOLineageFailClosed(t *testing.T) {
	ctx := context.Background()
	svc, port := newTestMemoryService(t)

	// 1. Anchor batch n=0 legitimately.
	if _, err := svc.Remember(ctx, RememberInput{Shard: "strict", Text: "batch 0 fact"}); err != nil {
		t.Fatal(err)
	}
	a0, err := svc.AnchorShard(ctx, "strict")
	if err != nil {
		t.Fatalf("AnchorShard(0): %v", err)
	}

	// 2. Construct a forged DISCONNECTED n=1 anchor tx that preserves valid ChainRp & ShardID
	//    but does NOT spend a0.TxID:0 (funded directly from wallet UTXOs instead of NektarAnchor.Append)!
	f2, err := svc.Remember(ctx, RememberInput{Shard: "strict", Text: "batch 1 forged disconnected fact"})
	if err != nil {
		t.Fatal(err)
	}
	leaf1, _ := hex.DecodeString(f2.Leaf)
	root1, _ := core.MerkleRoot([][]byte{leaf1})
	prevRp, _ := hex.DecodeString(a0.ChainedHex)
	rp1 := core.ChainRp(prevRp, root1, 1)
	genPub, err := port.DeriveRepoGenesisPubKey(ctx, "strict", "main")
	if err != nil {
		t.Fatalf("DeriveRepoGenesisPubKey: %v", err)
	}
	ownerPubBytes := port.Signer().IdentityKey().Compressed()
	forgedState := contracts.AnchorState{
		Shard:     a0.ShardID,
		OwnerPub:  hex.EncodeToString(genPub.Compressed()),
		OwnerPKH:  core.Hash160(ownerPubBytes),
		N:         1,
		Size:      1,
		R:         root1,
		Rp:        rp1,
		StateHash: core.Zero32Bytes(),
		MmrRoot:   core.Zero32Bytes(),
		Tag:       core.TagRPrime,
		Metanet:   true,
	}
	forgedScript, err := contracts.EncodeAnchorLockingScript(forgedState)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript: %v", err)
	}
	forgedTx, fee, spend, err := port.BuildFundedTx(ctx, []*transaction.TransactionOutput{chain.AnchorOutput(forgedScript)})
	if err != nil {
		t.Fatalf("BuildFundedTx: %v", err)
	}
	forgedTxID, err := port.Broadcast(ctx, forgedTx, fee, spend)
	if err != nil {
		t.Fatalf("Broadcast forged disconnected anchor: %v", err)
	}
	forgedBEEF, err := forgedTx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}

	// 2a. contracts.VerifyFragment MUST reject the disconnected ProofBundle with UTXOLineageBroken!
	link0 := contracts.AnchorLink{
		TxID: a0.TxID, OutputIndex: 0, N: 0, Size: uint64(a0.Size),
		R: a0.RootHex, Rp: a0.ChainedHex, AnchorBEEF: a0.Beef,
	}
	link1 := contracts.AnchorLink{
		TxID: forgedTxID, OutputIndex: 0, N: 1, Size: 1,
		R: hex.EncodeToString(root1), Rp: hex.EncodeToString(rp1), AnchorBEEF: forgedBEEF,
	}
	bundle := contracts.ProofBundle{
		Version:    1,
		Shard:      a0.ShardID,
		N:          1,
		Size:       1,
		Leaf:       f2.Leaf,
		LeafIndex:  0,
		Anchor:     link1,
		ChainTail:  []contracts.AnchorLink{link0, link1},
		AnchorBEEF: forgedBEEF,
	}
	if vr := contracts.VerifyFragment(bundle, port.Tracker()); vr.OK || vr.Reason != "UTXOLineageBroken" {
		t.Fatalf("expected VerifyFragment to reject disconnected n=1 anchor with UTXOLineageBroken, got %+v", vr)
	}

	// 2b. memrepo.VerifyCapsule MUST reject a capsule carrying the disconnected n=1 anchor!
	_, capObj, _, err := svc.ExportCapsule(ctx, "strict", "main", "")
	if err != nil {
		t.Fatalf("ExportCapsule: %v", err)
	}
	capObj.Anchors = append(capObj.Anchors, store.BatchAnchor{
		Shard: "strict", ShardID: a0.ShardID, N: 1, Size: 1,
		RootHex: hex.EncodeToString(root1), ChainedHex: hex.EncodeToString(rp1),
		TxID: forgedTxID, Beef: forgedBEEF, Settled: true,
	})
	if ver, err := memrepo.VerifyCapsule(ctx, capObj, svc.Objects(), port.Tracker(), true); err != nil || ver.OK {
		t.Fatalf("expected VerifyCapsule to reject disconnected n=1 anchor, got ver=%+v err=%v", ver, err)
	}

	// 2c. Persist the forged disconnected anchor into SQLite -> VerifyShardChain MUST catch broken UTXO lineage!
	if err := svc.DB().SaveBatchAnchorStrict(store.BatchAnchor{
		Shard: "strict", ShardID: a0.ShardID, N: 1, Size: 1,
		RootHex: hex.EncodeToString(root1), ChainedHex: hex.EncodeToString(rp1),
		TxID: forgedTxID, Beef: forgedBEEF, Settled: true,
	}); err != nil {
		t.Fatal(err)
	}
	rep, err := svc.VerifyShardChain("strict")
	if err != nil || rep.OK || rep.AnchorsOK {
		t.Fatalf("expected VerifyShardChain to reject disconnected n=1 anchor, got %+v err=%v", rep, err)
	}

	// 3. AnchorShard on a repo whose previous anchor UTXO cannot be resolved/spent MUST fail closed (never mint a disconnected UTXO)!
	if _, err := svc.Remember(ctx, RememberInput{Shard: "broken-prev", Text: "genesis fact"}); err != nil {
		t.Fatal(err)
	}
	missingTxID := core.ToHex(core.DSHA256([]byte("missing-prev-anchor-utxo")))
	if err := svc.DB().SaveBatchAnchorStrict(store.BatchAnchor{
		Shard:      "broken-prev",
		ShardID:    a0.ShardID,
		N:          0,
		Size:       1,
		RootHex:    a0.RootHex,
		ChainedHex: a0.ChainedHex,
		TxID:       missingTxID,
		Beef:       nil, // missing prev UTXO -> cannot spend n=0:0!
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.AnchorShard(ctx, "broken-prev"); err == nil {
		t.Fatal("expected AnchorShard to fail closed when previous anchor UTXO is unavailable (no disconnected fallback!)")
	}
}
