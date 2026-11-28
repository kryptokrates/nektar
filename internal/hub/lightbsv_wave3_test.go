package hub_test

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"strings"
	"testing"
	"time"

	ec "github.com/bsv-blockchain/go-sdk/primitives/ec"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bbox/boxrec"
	"github.com/lightwebinc/bcommon/resolve"
	"github.com/lightwebinc/bstore/storerec"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/chain"
	"github.com/kryptokrates/nektar/internal/discovery"
	"github.com/kryptokrates/nektar/internal/eventbus"
	"github.com/kryptokrates/nektar/internal/fabric"
	"github.com/kryptokrates/nektar/internal/hub"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

func buildTestWantBEEF(t *testing.T, key *ec.PrivateKey, net, wantID, domain, ontID string) []byte {
	t.Helper()
	want := bstack.WantAd{
		WantID:     wantID,
		Pub:        hex.EncodeToString(key.PubKey().Compressed()),
		Net:        net,
		Domains:    []string{domain},
		OntologyID: ontID,
		Query:      "LightBSV Wave 3 dynamic topic verification",
		MaxSats:    500,
		Revision:   1,
		Exp:        time.Now().Add(time.Hour).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(key, want)
	if err != nil {
		t.Fatalf("SignArtifact: %v", err)
	}
	pl, err := bstack.NewWantPayload(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		t.Fatalf("NewWantPayload: %v", err)
	}
	cborBytes, err := bstack.EncodePayload(pl)
	if err != nil {
		t.Fatalf("EncodePayload: %v", err)
	}
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData(cborBytes)
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	raw, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}
	return raw
}

func TestLightBSVWave3ManifestFileBlobsDynamicTopicsAndBRC223Fabric(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	shared := chain.NewSharedMemoryChain()
	hubPort := openMemoryPort(t, shared)
	bus := eventbus.New()
	dbPath := filepath.Join(t.TempDir(), "wave3_hub.sqlite")

	node, err := hub.NewNode(hub.Config{
		Network:      "memory",
		DBPath:       dbPath,
		EnableFinger: true,
		EnableBbox:   true,
		EnableBstore: true,
		EnableFabric: true,
		EventBus:     bus,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
		HandleResolver: func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"name": r.URL.Query().Get("name")})
		},
	})
	if err != nil {
		t.Fatalf("NewNode: %v", err)
	}
	defer node.Close()

	// 1. Zero-config default bbox office & bstore bucket must satisfy Lightweb's strict grammar.
	if err := boxrec.CheckOffice(node.DefaultBboxOffice()); err != nil {
		t.Fatalf("DefaultBboxOffice %q failed boxrec.CheckOffice: %v", node.DefaultBboxOffice(), err)
	}
	if err := storerec.CheckBucket(node.DefaultBstoreBucket()); err != nil {
		t.Fatalf("DefaultBstoreBucket %q failed storerec.CheckBucket: %v", node.DefaultBstoreBucket(), err)
	}

	ts := httptest.NewServer(node.Handler())
	defer ts.Close()

	// 2. BRC-180 /manifest.json must parse cleanly through Lightweb's `bcommon/resolve.ParseManifest`.
	mResp, err := http.Get(ts.URL + "/manifest.json")
	if err != nil || mResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /manifest.json failed: err=%v resp=%+v", err, mResp)
	}
	mBody, _ := io.ReadAll(mResp.Body)
	mResp.Body.Close()
	manifest, err := resolve.ParseManifest(mBody)
	if err != nil {
		t.Fatalf("resolve.ParseManifest: %v (raw=%s)", err, string(mBody))
	}
	for _, svc := range []string{"tm_finger", "ls_finger", "ls_bbox", "ls_bstore", "ls_nektar", "tm_bbox_" + node.DefaultBboxOffice(), "tm_bstore_" + node.DefaultBstoreBucket()} {
		if _, ok := manifest.Overlay(svc); !ok {
			t.Fatalf("manifest missing overlay %q: %+v", svc, manifest.Overlays)
		}
	}
	if manifest.Handles == nil || manifest.Handles.Messagebox != node.DefaultBboxOffice() {
		t.Fatalf("manifest handles invalid: %+v", manifest.Handles)
	}
	if manifest.Trust == nil || manifest.Trust.PublicKey == "" {
		t.Fatalf("manifest trust missing publicKey: %+v", manifest.Trust)
	}

	// 3. Subscribe a BRC-223 `fabric.Subscriber` ONLY to a dynamic semantic domain topic (`tm_nektar_memory_dom_golang`)
	// and verify zero-poll push delivery + EventBus wake + PERF-5 fileblobs offloading!
	domTopic := bstack.DomainTopic("memory", "Golang")
	ontID := strings.Repeat("c3", 32)
	ontTopic := bstack.OntologyTopic("memory", ontID)

	deliveredCh := make(chan fabric.DeliveryEvent, 4)
	sub, err := fabric.StartSubscriber(ctx, fabric.SubscriberConfig{
		Label:     "wave3-sub",
		Upstreams: []netip.AddrPort{node.FabricServer().UDPAddr()},
		Bus:       bus,
		OnObject: func(_ context.Context, ev fabric.DeliveryEvent) {
			select {
			case deliveredCh <- ev:
			default:
			}
		},
	})
	if err != nil {
		t.Fatalf("StartSubscriber: %v", err)
	}
	defer sub.Close()
	sub.SetTopics([]string{domTopic})

	// Wait briefly for UDP BRC-223 interest datagram to register on loopback distributor.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if node.FabricServer().DistributorStats().Routes >= 1 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}

	admitSub := bus.Subscribe([]string{"hub:admit:" + domTopic}, 4)
	defer admitSub.Close()

	buyerKey, _ := ec.NewPrivateKey()
	beef := buildTestWantBEEF(t, buyerKey, "memory", "dyn-want-1", "golang", ontID)
	steak, err := node.Submit(ctx, beef, []string{domTopic, ontTopic})
	if err != nil {
		t.Fatalf("Submit to dynamic semantic & ontology topics: %v", err)
	}
	if len(steak[domTopic].OutputsToAdmit) != 1 || len(steak[ontTopic].OutputsToAdmit) != 1 {
		t.Fatalf("expected 1 admitted output on both dynamic topics, got %+v", steak)
	}

	select {
	case <-admitSub.Events():
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for instant EventBus wake on hub:admit")
	}

	select {
	case ev := <-deliveredCh:
		if ev.Topic != domTopic || !bytes.Equal(ev.BEEF, beef) {
			t.Fatalf("unexpected BRC-223 fabric delivery: topic=%q beefMatch=%v", ev.Topic, bytes.Equal(ev.BEEF, beef))
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for BRC-223 / BRC-149 push delivery")
	}

	// 4. PERF-5 verification: raw SQLite `hub_events.beef` column MUST be NULL when disk-backed,
	// while `EventsSince` hydrates the exact BEEF bytes from `overlay-engine` fileblobs!
	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("sql.Open raw hub sqlite: %v", err)
	}
	defer rawDB.Close()
	var nullCount int
	if err := rawDB.QueryRow(`SELECT COUNT(*) FROM hub_events WHERE beef IS NULL`).Scan(&nullCount); err != nil {
		t.Fatalf("query null beef count: %v", err)
	}
	if nullCount == 0 {
		t.Fatal("PERF-5 regression: expected disk-backed hub_events.beef column to be NULL (offloaded to fileblobs)")
	}
	hydrated, err := node.Store().EventsSince(0, []string{"golang"}, 10)
	if err != nil || len(hydrated) == 0 || !bytes.Equal(hydrated[0].BEEF, beef) {
		t.Fatalf("PERF-5 hydration failed: err=%v len=%d", err, len(hydrated))
	}
}

func TestLightBSVWave3BatchedEgressRefundPoisonSkipAndFastFeeSweep(t *testing.T) {
	ctx := context.Background()
	shared := chain.NewSharedMemoryChain()
	hubPort := openMemoryPort(t, shared)
	payerPort := openMemoryPort(t, shared)

	fs := hub.FeeSchedule{
		Role:                   hub.RoleSpine,
		EgressSatsPer100Events: 100, // 1000 msats/event
		EgressMinTopupSats:     2,
		AcceptWindow:           time.Minute,
	}
	spine, err := hub.NewNode(hub.Config{
		Network:      "memory",
		Fees:         fs,
		FeeCollector: hub.NewPortFeeCollector(hubPort),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer spine.Close()
	spineTS := httptest.NewServer(spine.Handler())
	defer spineTS.Close()

	// Seed 5 market events on Spine.
	adKey, _ := ec.NewPrivateKey()
	lane := bstack.LaneOf("memory", "ops")
	for i := 1; i <= 5; i++ {
		if _, err := spine.Submit(ctx, buildTestWantBEEF(t, adKey, "memory", "batch-w-"+string(rune('0'+i)), "ops", ""), []string{lane}); err != nil {
			t.Fatal(err)
		}
	}

	// Top up 25 sats (= 25,000 msats = 25 events; batch reserves 20 upfront).
	subKey, _ := ec.NewPrivateKey()
	subPub := hex.EncodeToString(subKey.PubKey().Compressed())
	terms := fetchTermsHTTP(t, spineTS.URL, "/v1/terms")
	payHdr, _, err := discovery.BuildStandaloneHubPayment(ctx, payerPort, terms.PayeeIdentityKey, terms.Prefixes[hub.ActionEgressTopup], hub.ActionEgressTopup, 25)
	if err != nil {
		t.Fatal(err)
	}
	topBody, _ := json.Marshal(map[string]string{"payerPub": subPub})
	topReq, _ := http.NewRequest(http.MethodPost, spineTS.URL+"/v1/credits/topup", bytes.NewReader(topBody))
	topReq.Header.Set("Content-Type", "application/json")
	topReq.Header.Set("X-BSV-Payment", payHdr)
	topResp, err := http.DefaultClient.Do(topReq)
	if err != nil || topResp.StatusCode != http.StatusOK {
		t.Fatalf("topup 25 sats failed: err=%v resp=%+v", err, topResp)
	}
	topResp.Body.Close()

	// Open SSE `/events?cursor=0`, read all 5 backlog events, then disconnect early so 15 unused batch events refund!
	sseCtx, sseCancel := context.WithCancel(ctx)
	aud := strings.TrimPrefix(spineTS.URL, "http://")
	pubH, tsH, nonceH, sigH, _ := discovery.SignEgressAuthHeaders(nwallet.NewMarketSigner(subKey), "hub_events", aud, 0, "")
	sseReq, _ := http.NewRequestWithContext(sseCtx, http.MethodGet, spineTS.URL+"/events?cursor=0", nil)
	sseReq.Header.Set("X-Nektar-Pub", pubH)
	sseReq.Header.Set("X-Nektar-Ts", tsH)
	sseReq.Header.Set("X-Nektar-Nonce", nonceH)
	sseReq.Header.Set("X-Nektar-Sig", sigH)
	sseResp, err := http.DefaultClient.Do(sseReq)
	if err != nil || sseResp.StatusCode != http.StatusOK {
		t.Fatalf("SSE open failed: err=%v resp=%+v", err, sseResp)
	}
	sc := bufio.NewScanner(sseResp.Body)
	readEvents := 0
	for sc.Scan() {
		if strings.HasPrefix(sc.Text(), "data: ") {
			readEvents++
			if readEvents == 5 {
				break
			}
		}
	}
	sseCancel()
	sseResp.Body.Close()

	// Wait briefly for server handler defer RefundUnusedEgress to execute.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		cred, _ := spine.Store().GetEgressCredit(subPub)
		if cred.BalanceMilliSats == 20_000 && cred.TotalEventsDelivered == 5 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	cred, err := spine.Store().GetEgressCredit(subPub)
	if err != nil || cred.BalanceMilliSats != 20_000 || cred.TotalEventsDelivered != 5 {
		t.Fatalf("PERF-11 refund mismatch: expected 20000 msats & 5 delivered, got %+v err=%v", cred, err)
	}

	// STATE-9: unmined fast-path fee older than acceptance window must sweep to 'lost' and flag payer!
	stalePayer := strings.Repeat("02", 1) + strings.Repeat("aa", 32)
	if err := spine.Store().RecordFeeLedger(hub.FeeLedgerEntry{
		TxID:      strings.Repeat("ef", 32),
		Action:    "submit:want",
		PayerPub:  stalePayer,
		Sats:      12,
		Decision:  "fast",
		Status:    "unmined",
		CreatedAt: time.Now().Add(-2 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}
	swept, err := spine.SweepUnsettleableFees(1800)
	if err != nil || swept != 1 {
		t.Fatalf("STATE-9 SweepUnsettleableFees expected 1, got %d err=%v", swept, err)
	}
	flagged, _, err := spine.Store().IsPayerFlagged(stalePayer)
	if err != nil || !flagged {
		t.Fatalf("STATE-9 expected stale fast-fee payer to be flagged, got flagged=%v err=%v", flagged, err)
	}

	// ARCH-F5: inject a corrupted poison BEEF row on Spine's DB followed by a valid frame, and verify
	// a Leaf Hub on StartPlaneRelay skips & counts the poison frame and still receives the valid frame!
	_, _, _ = spine.Store().AppendEvent(hub.StoredEvent{
		Outpoint:   strings.Repeat("11", 32) + ".0",
		Topic:      lane,
		TxID:       strings.Repeat("11", 32),
		Vout:       0,
		Kind:       bstack.KindWant,
		Pub:        subPub,
		Domains:    []string{"ops"},
		BEEF:       []byte("corrupt-poison-beef-bytes"),
		Exp:        time.Now().Add(time.Hour).UnixMilli(),
		AdmittedAt: time.Now().UnixMilli(),
	})
	if _, err := spine.Submit(ctx, buildTestWantBEEF(t, adKey, "memory", "after-poison-valid", "ops", ""), []string{lane}); err != nil {
		t.Fatal(err)
	}
	leafKey, _ := ec.NewPrivateKey()
	leafPub := hex.EncodeToString(leafKey.PubKey().Compressed())
	spine.AddSpinePeerPub(leafPub)

	leaf, err := hub.NewNode(hub.Config{Network: "memory"})
	if err != nil {
		t.Fatal(err)
	}
	defer leaf.Close()
	leaf.SetEgressAuth(func(action, aud string, cursor int64, domainsCSV string) (string, string, string, string, error) {
		return discovery.SignEgressAuthHeaders(nwallet.NewMarketSigner(leafKey), action, aud, cursor, domainsCSV)
	}, nil)
	relCtx, relCancel := context.WithCancel(ctx)
	defer relCancel()
	leaf.StartPlaneRelay(relCtx, []string{spineTS.URL})

	gotAfterPoison := false
	deadline = time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		evs, _ := leaf.Store().EventsSince(0, nil, 20)
		for _, ev := range evs {
			if sw, dErr := bstack.DecodeWantPayload(ev.Payload.Body); dErr == nil && sw.Want.WantID == "after-poison-valid" {
				gotAfterPoison = true
				break
			}
		}
		if gotAfterPoison {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}
	if !gotAfterPoison || leaf.PoisonFramesSkipped() == 0 {
		t.Fatalf("ARCH-F5 poison skip failed: gotAfterPoison=%v poisonSkipped=%d", gotAfterPoison, leaf.PoisonFramesSkipped())
	}
}
