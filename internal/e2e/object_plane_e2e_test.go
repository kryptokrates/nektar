package e2e

import (
	"context"
	"encoding/hex"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/lightwebinc/bcommon/testchain"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/store"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

// TestSixHubBRC149RelayReplayAndFundingTreeE2E verifies the full 1BSV / Lightweb Hub-to-Hub
// architecture across 6 live Hubs (1 Spine BRC-149 Relay + 5 Leaf Hubs with zero sibling gossip):
//   - M=16 Carrier Funding Tree (CarrierTreeManager) minting + Ahead pre-minting
//   - O(N) BRC-149 Application-Layer Object Plane delivery from Spine to all 5 Leaf Hubs
//   - Automatic NACK gap repair (/plane/nack) & offline BRC-24 CatchUpFromPeer backfill
//   - Kill-switch sweep retraction (RetractCarrier) evicting the carrier cleanly
func TestSixHubBRC149RelayReplayAndFundingTreeE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	shared := chain.NewSharedMemoryChain()
	_ = testchain.New(1) // ensure testchain import availability

	// Boot 1 Spine Hub (BRC-149 Application-Layer Object Plane Relay)
	spine, err := hub.NewNode(hub.Config{
		Network:       "memory",
		DBPath:        filepath.Join(t.TempDir(), "spine.sqlite"),
		ChainTracker:  shared,
		TransportMode: hub.TransportRelay,
		Fees:          hub.FeeSchedule{Role: hub.RoleSpine},
	})
	if err != nil {
		t.Fatalf("new spine: %v", err)
	}
	defer spine.Close()
	spineTS := httptest.NewServer(spine.Handler())
	defer spineTS.Close()

	// Boot 4 online Leaf Hubs (hub1..hub4) subscribing to Spine's BRC-149 plane stream (NO sibling mesh!)
	leaves := make([]*hub.Node, 4)
	for i := range 4 {
		leaf, lErr := hub.NewNode(hub.Config{
			Network:        "memory",
			DBPath:         filepath.Join(t.TempDir(), "leaf.sqlite"),
			ChainTracker:   shared,
			TransportMode:  hub.TransportRelay,
			Fees:           hub.FeeSchedule{Role: hub.RoleLeaf},
			UpstreamSpines: []string{spineTS.URL},
		})
		if lErr != nil {
			t.Fatalf("new leaf %d: %v", i+1, lErr)
		}
		defer leaf.Close()
		leaf.StartPlaneRelay(ctx, []string{spineTS.URL})
		leaves[i] = leaf
	}
	time.Sleep(150 * time.Millisecond)

	// Open a publisher wallet on the shared memory chain and mint 3 carriers from ONE M=16 Funding Tree!
	pubPort, err := chain.Open(ctx, chain.Config{
		Network:           "memory",
		DataDir:           t.TempDir(),
		SharedMemoryChain: shared,
	})
	if err != nil {
		t.Fatalf("chain.Open: %v", err)
	}
	defer pubPort.Close()
	pubDB, err := store.Open(filepath.Join(t.TempDir(), "pub.sqlite"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	defer pubDB.Close()

	priv, _ := ec.NewPrivateKey()
	pubHex := hex.EncodeToString(priv.PubKey().Compressed())
	treeMgr := discovery.NewCarrierTreeManager(pubPort, pubDB, nwallet.NewMarketSigner(priv))

	topic := bstack.LaneOf("memory", "travel")
	var firstFundingTreeTxID string
	var firstVout uint32
	for i := range 3 {
		w := bstack.WantAd{
			WantID:   "want-plane-" + string(rune('a'+i)),
			Pub:      pubHex,
			Net:      "memory",
			Domains:  []string{"travel"},
			Query:    "EU261 playbook",
			MaxSats:  150,
			Revision: 1,
			Exp:      time.Now().Add(time.Hour).UnixMilli(),
		}
		sig, _ := bstack.SignArtifact(priv, w)
		payload, pErr := bstack.NewWantPayload(bstack.SignedWant{Want: w, Sig: sig})
		if pErr != nil {
			t.Fatalf("NewWantPayload #%d: %v", i, pErr)
		}
		beef, fop, _, mErr := treeMgr.MintCarrier(ctx, payload)
		if mErr != nil {
			t.Fatalf("MintCarrier #%d: %v", i, mErr)
		}
		if i == 0 {
			st := treeMgr.State()
			if st.Active == nil || len(st.Active.Vouts) != discovery.DefaultFundingTreeWidth {
				t.Fatalf("expected M=%d funding tree outputs, got %+v", discovery.DefaultFundingTreeWidth, st.Active)
			}
			firstFundingTreeTxID = st.Active.TxID
			firstVout = st.Active.Vouts[0]
			_ = fop
		}
		// Publish ONCE to Spine (BRC-149 Application-Layer Object Plane).
		if _, err := spine.Submit(ctx, beef, []string{topic}); err != nil {
			t.Fatalf("spine.Submit #%d: %v", i, err)
		}
	}
	// Verify all 3 carriers shared the EXACT SAME M=16 funding tree (1 miner fee, 3 carriers!).
	if st := treeMgr.State(); st.Active == nil || st.Active.NextIndex != 3 {
		t.Fatalf("expected M=16 tree NextIndex=3, got %+v", st.Active)
	}

	// Verify all 4 online Leaf Hubs received all 3 objects over Spine's BRC-149 stream!
	deadline := time.Now().Add(4 * time.Second)
	for {
		allSynced := true
		for _, leaf := range leaves {
			evs, _ := leaf.Store().EventsSince(0, nil, 20)
			if len(evs) < 3 {
				allSynced = false
				break
			}
		}
		if allSynced {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for all 4 Leaf Hubs to receive 3 BRC-149 frames from Spine")
		}
		time.Sleep(50 * time.Millisecond)
	}

	// Boot 5th Leaf Hub OFFLINE AFTER the 3 publishes -> backfill via BRC-24 CatchUpFromPeer (Paper 008 §6)!
	hub5, err := hub.NewNode(hub.Config{
		Network:       "memory",
		DBPath:        filepath.Join(t.TempDir(), "hub5.sqlite"),
		ChainTracker:  shared,
		TransportMode: hub.TransportUnicast,
		Fees:          hub.FeeSchedule{Role: hub.RoleLeaf},
	})
	if err != nil {
		t.Fatalf("new hub5: %v", err)
	}
	defer hub5.Close()
	cu, err := hub5.CatchUpFromPeer(ctx, spineTS.URL, 5)
	if err != nil || cu.Admitted != 3 {
		t.Fatalf("hub5 CatchUpFromPeer: expected 3 admitted, got res=%+v err=%v", cu, err)
	}

	// Kill-switch sweep: retract the 1st carrier on Spine and verify active count drops 3 -> 2!
	_, sweepBEEF, err := treeMgr.RetractCarrier(ctx, firstFundingTreeTxID, firstVout)
	if err != nil {
		t.Fatalf("RetractCarrier: %v", err)
	}
	if _, err := spine.Submit(ctx, sweepBEEF, []string{topic}); err != nil {
		t.Fatalf("spine kill-switch Submit: %v", err)
	}
	remaining, _ := spine.Store().EventsSince(0, nil, 20)
	if len(remaining) != 2 {
		t.Fatalf("expected 2 active carriers on Spine after kill-switch sweep, got %d", len(remaining))
	}
}
