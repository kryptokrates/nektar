package chain

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/testchain"
	"github.com/lightwebinc/bcommon/wirewallet"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func TestMemoryPortBroadcastAnchorAndSPVVerify(t *testing.T) {
	ctx := context.Background()
	port, err := Open(ctx, Config{
		Network: "memory",
		DataDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Open memory port: %v", err)
	}
	if port.BalanceSats() == 0 {
		t.Fatal("expected seeded coinbase balance in embedded bwallet pool")
	}

	// Build and broadcast a real NektarAnchor tx onto bcommon/testchain, then SPV-verify a fragment!
	pubBytes := port.Signer().IdentityKey().Compressed()
	leaf := core.ComputeLeaf(core.Fragment{
		Data:       []byte("first anchored Go fact"),
		Provenance: core.Provenance{Source: "go-test", Time: 1730000000, ModelID: "test"},
	})
	root, _ := core.MerkleRoot([][]byte{leaf})
	rp := core.ChainRp(core.Zero32, root, 1)
	state := contracts.AnchorState{
		Shard:    core.ShardIDFromPubkey(pubBytes),
		OwnerPub: port.IdentityPubHex(),
		OwnerPKH: core.Hash160(pubBytes),
		N:        0,
		Size:     1,
		R:        root,
		Rp:       rp,
		Tag:      core.TagAnchorTag,
		Metanet:  true,
	}
	lockScript, err := contracts.EncodeAnchorLockingScript(state)
	if err != nil {
		t.Fatalf("EncodeAnchorLockingScript: %v", err)
	}
	tx, fee, spend, err := port.BuildFundedTx(ctx, []*transaction.TransactionOutput{AnchorOutput(lockScript)})
	if err != nil {
		t.Fatalf("BuildFundedTx: %v", err)
	}
	txid, err := port.Broadcast(ctx, tx, fee, spend)
	if err != nil {
		t.Fatalf("Broadcast: %v", err)
	}
	if err := port.AdoptChange(tx); err != nil {
		t.Fatalf("AdoptChange: %v", err)
	}

	beef, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}
	link := contracts.AnchorLink{
		TxID:        txid,
		OutputIndex: 0,
		N:           0,
		Size:        1,
		R:           core.ToHex(root),
		Rp:          core.ToHex(rp),
	}
	bundle := contracts.ProofBundle{
		Version:    1,
		Shard:      state.Shard,
		N:          0,
		Size:       1,
		Leaf:       core.ToHex(leaf),
		LeafIndex:  0,
		Path:       nil,
		Anchor:     link,
		ChainTail:  []contracts.AnchorLink{link},
		AnchorBEEF: beef,
	}
	res := contracts.VerifyFragment(bundle, port.Tracker())
	if !res.OK || !res.Settled || res.AnchorState != "mined" {
		t.Fatalf("expected mined SPV verification, got: %+v", res)
	}

	// Reopening the same dataDir in memory mode must preserve identity and reconcile pool cleanly!
	port2, err := Open(ctx, Config{Network: "memory", DataDir: port.dataDir})
	if err != nil {
		t.Fatalf("reopen memory port on same dataDir: %v", err)
	}
	if port2.IdentityPubHex() != port.IdentityPubHex() {
		t.Fatalf("identity pub changed across reopen: %s vs %s", port2.IdentityPubHex(), port.IdentityPubHex())
	}
	if port2.BalanceSats() == 0 {
		t.Fatal("reopened memory port has zero balance")
	}
	_ = port.Close()
	_ = port2.Close()
}

func TestClientWalletWireRoundTrip(t *testing.T) {
	ctx := context.Background()
	tc := testchain.New(1)
	var tcMu sync.Mutex

	// Simulate an external BSV Desktop / go-wallet-toolbox instance over standard BRC-100 binary wire!
	desktop, err := nwallet.Open(nwallet.Config{
		Dir:        t.TempDir(),
		Mainnet:    false,
		Originator: "bsv-desktop",
		Tracker:    tc,
		Broadcast: func(_ context.Context, tx *transaction.Transaction) error {
			tcMu.Lock()
			defer tcMu.Unlock()
			if err := tc.Send(tx); err != nil {
				return err
			}
			if mp, _, ok := tc.Proof(tx.TxID().String()); ok {
				tx.MerklePath = mp
			}
			return nil
		},
		ProofLookup: func(_ context.Context, txid string) (*transaction.MerklePath, uint32, bool, error) {
			tcMu.Lock()
			defer tcMu.Unlock()
			mp, h, ok := tc.Proof(txid)
			return mp, h, ok, nil
		},
	})
	if err != nil {
		t.Fatalf("open simulated desktop wallet: %v", err)
	}
	defer desktop.Close()

	// Seed mature coinbases into the simulated BSV Desktop wallet.
	if _, err := tc.Generate(testchain.Maturity+3, desktop.FundAddress()); err != nil {
		t.Fatalf("tc.Generate: %v", err)
	}
	for _, txid := range tc.Txids() {
		mp, h, ok := tc.Proof(txid)
		if !ok || h+testchain.Maturity > tc.Height() {
			continue
		}
		if err := desktop.SeedCoinbaseOutput(tc.Tx(txid), 0, mp, h); err != nil {
			t.Fatalf("SeedCoinbaseOutput: %v", err)
		}
	}

	wireSrv := httptest.NewServer(wirewallet.Serve(desktop))
	defer wireSrv.Close()

	// Open chain.Port in NEKTAR_WALLET=client mode pointing at the simulated BSV Desktop wire endpoint!
	clientPort, err := Open(ctx, Config{
		Network:       "testnet",
		DataDir:       t.TempDir(),
		WalletMode:    "client",
		WalletWireURL: wireSrv.URL,
	})
	if err != nil {
		t.Fatalf("Open client port: %v", err)
	}
	defer clientPort.Close()

	// 1. Identity, address, height, balance, and UTXO count MUST work in client mode!
	if clientPort.IdentityPubHex() != desktop.IdentityPubHex() {
		t.Fatalf("client identity mismatch: %s vs %s", clientPort.IdentityPubHex(), desktop.IdentityPubHex())
	}
	if addr, err := clientPort.FundAddress(); err != nil || addr != desktop.FundAddress() {
		t.Fatalf("client FundAddress = %q, err=%v; want %q", addr, err, desktop.FundAddress())
	}
	if bal := clientPort.BalanceSats(); bal == 0 {
		t.Fatal("client BalanceSats() returned 0; expected non-zero balance via BRC-100 ListOutputs")
	}
	if cnt := clientPort.UTXOCount(); cnt == 0 {
		t.Fatal("client UTXOCount() returned 0; expected > 0")
	}
	if h, err := clientPort.Height(ctx); err != nil || h < testchain.Maturity {
		t.Fatalf("client Height = %d, err=%v", h, err)
	}

	// 2. BuildFundedTx + Broadcast (previously failed with "embedded bwallet pool required"!) MUST succeed!
	peer, err := nwallet.Open(nwallet.Config{
		Dir:        t.TempDir(),
		Mainnet:    false,
		Originator: "peer",
		Tracker:    tc,
	})
	if err != nil {
		t.Fatalf("open peer wallet: %v", err)
	}
	defer peer.Close()

	tx, fee, spend, err := clientPort.BuildFundedTx(ctx, []*transaction.TransactionOutput{
		{Satoshis: 1500, LockingScript: peer.FundScript()},
	})
	if err != nil || tx == nil {
		t.Fatalf("client BuildFundedTx failed: %v", err)
	}
	txid, err := clientPort.Broadcast(ctx, tx, fee, spend)
	if err != nil || txid == "" {
		t.Fatalf("client Broadcast failed: %v", err)
	}

	// 3. SendToAddress + BRC-29 Send + InternalizeBRC29 across the wire!
	sendRes, err := clientPort.Send(ctx, nwallet.SendArgs{
		ToIdentityKey: peer.IdentityPubHex(),
		Satoshis:      2500,
		Description:   "client-mode BRC-29 payment",
	})
	if err != nil || len(sendRes.BEEF) == 0 {
		t.Fatalf("client Send BRC-29 failed: %v", err)
	}
	// Peer internalizes the BRC-29 payment sent by clientPort!
	peerPort := &Port{wallet: peer, engine: peer, signer: peer.Signer()}
	credited, err := peerPort.InternalizeBRC29(
		ctx, sendRes.BEEF, sendRes.OutputIndex,
		sendRes.DerivationPrefix, sendRes.DerivationSuffix, sendRes.SenderIdentityKey,
		"received from client port", []string{"client-test"},
	)
	if err != nil || credited != 2500 {
		t.Fatalf("peer InternalizeBRC29 = %d, err=%v; want 2500", credited, err)
	}
}
