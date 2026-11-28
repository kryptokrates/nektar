package fabric

import (
	"bytes"
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/shard-common/interest"

	"github.com/kryptokrates/nektar/internal/eventbus"
)

func makeSampleAtomicBEEF(t *testing.T, tag string) []byte {
	t.Helper()
	ls := &script.Script{}
	_ = ls.AppendOpcodes(script.OpFALSE, script.OpRETURN)
	_ = ls.AppendPushData([]byte(tag))
	tx := transaction.NewTransaction()
	tx.AddOutput(&transaction.TransactionOutput{Satoshis: 0, LockingScript: ls})
	raw, err := tx.AtomicBEEF(false)
	if err != nil {
		t.Fatalf("AtomicBEEF: %v", err)
	}
	return raw
}

func TestBRC223InterestAndBRC149ZeroPollDelivery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	srv, err := StartHubInterestServer(ctx, HubConfig{
		UDPListen: "127.0.0.1:0",
	})
	if err != nil {
		t.Fatalf("StartHubInterestServer: %v", err)
	}
	defer srv.Close()

	bus1 := eventbus.New()
	sub1, err := StartSubscriber(ctx, SubscriberConfig{
		Label:     "agent-bsv",
		Upstreams: []netip.AddrPort{srv.UDPAddr()},
		Bus:       bus1,
	})
	if err != nil {
		t.Fatalf("StartSubscriber 1: %v", err)
	}
	defer sub1.Close()

	bus2 := eventbus.New()
	sub2, err := StartSubscriber(ctx, SubscriberConfig{
		Label:     "agent-finance",
		Upstreams: []netip.AddrPort{srv.UDPAddr()},
		Bus:       bus2,
	})
	if err != nil {
		t.Fatalf("StartSubscriber 2: %v", err)
	}
	defer sub2.Close()

	topicBSV := "tm_nektar_regtest_dom_bsv"
	topicFin := "tm_nektar_regtest_dom_finance"
	sub1.SetTopics([]string{topicBSV})
	sub2.SetTopics([]string{topicFin})

	// Wait until BRC-223 UDP handshake acknowledges both topic routes (no busy-poll in production; test readiness check).
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if sub1.Client().Status(TopicID(topicBSV)) == interest.StatusOK &&
			sub2.Client().Status(TopicID(topicFin)) == interest.StatusOK &&
			srv.DistributorStats().Routes == 2 {
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if got := srv.DistributorStats().Routes; got != 2 {
		t.Fatalf("expected 2 routed BRC-223 topic subscriptions, got %d", got)
	}

	s1 := bus1.Subscribe([]string{"fabric:" + topicBSV}, 8)
	defer s1.Close()
	s2 := bus2.Subscribe([]string{"fabric:" + topicFin}, 8)
	defer s2.Close()

	beefBSV := makeSampleAtomicBEEF(t, "bsv-knowledge-1")
	copied := srv.Fanout([]string{topicBSV}, beefBSV, netip.Addr{})
	if copied != 1 {
		t.Fatalf("expected Fanout(topicBSV) copied to strictly 1 interested subscriber, got %d", copied)
	}

	select {
	case ev := <-s1.Events():
		deliv, ok := ev.Payload.(DeliveryEvent)
		if !ok || deliv.Topic != topicBSV || !bytes.Equal(deliv.BEEF, beefBSV) {
			t.Fatalf("unexpected delivery on sub1: %+v", ev.Payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for zero-poll BRC-149 push on sub1")
	}

	// sub2 did NOT subscribe to topicBSV -> must receive zero records!
	select {
	case unexpected := <-s2.Events():
		t.Fatalf("sub2 received unrequested topic event: %+v", unexpected)
	case <-time.After(80 * time.Millisecond):
	}
}
