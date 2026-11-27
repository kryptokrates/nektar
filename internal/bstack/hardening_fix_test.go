package bstack

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/transaction"
)

func TestLookupSinceSeqSortsAscendingBeforeLimit(t *testing.T) {
	ls := NewMarketLookupService(nil, nil)
	nowMs := time.Now().UnixMilli()
	ls.mu.Lock()
	for i := int64(1); i <= 30; i++ {
		var h chainhash.Hash
		h[0] = byte(i)
		op := transaction.Outpoint{Txid: h, Index: 0}
		ls.records[op.String()] = indexedRecord{
			seq:        i,
			outpoint:   op,
			topic:      LaneOf("regtest", "ops"),
			payload:    MarketPayload{Kind: KindWant, Body: json.RawMessage(`{}`)},
			admittedAt: nowMs,
			wantID:     fmt.Sprintf("w-%d", i),
			exp:        nowMs + 60_000,
		}
	}
	ls.mu.Unlock()

	qBytes, _ := json.Marshal(MarketQuery{SinceSeq: 5, Limit: 5})
	for trial := 0; trial < 20; trial++ {
		ans, err := ls.Lookup(context.Background(), &lookup.LookupQuestion{
			Service: LookupService,
			Query:   qBytes,
		})
		if err != nil {
			t.Fatalf("Lookup: %v", err)
		}
		evs, ok := ans.Result.([]MarketEvent)
		if !ok || len(evs) != 5 {
			t.Fatalf("expected 5 events, got %T len=%d", ans.Result, len(evs))
		}
		for idx, wantSeq := range []int64{6, 7, 8, 9, 10} {
			if evs[idx].Seq != wantSeq {
				t.Fatalf("trial %d idx %d: expected seq %d, got %d", trial, idx, wantSeq, evs[idx].Seq)
			}
		}
	}
}

type failingGroupStore struct {
	failSave bool
}

func (f *failingGroupStore) LoadBorgGroups() ([]*BorgGroup, error) { return nil, nil }
func (f *failingGroupStore) SaveBorgGroup(*BorgGroup) error {
	if f.failSave {
		return errors.New("disk full")
	}
	return nil
}

func TestGroupRegistryPersistFailureDoesNotMutateInMemory(t *testing.T) {
	st := &failingGroupStore{}
	reg := NewGroupRegistry()
	if err := reg.SetPersistence(st); err != nil {
		t.Fatalf("SetPersistence: %v", err)
	}
	if _, err := reg.CreateGroup("g1", "team", "owner-pub"); err != nil {
		t.Fatalf("CreateGroup: %v", err)
	}
	st.failSave = true
	if err := reg.AddMember("g1", "alice-pub", "reader"); err == nil {
		t.Fatal("expected AddMember error when SaveBorgGroup fails")
	}
	g, ok := reg.GetGroup("g1")
	if !ok {
		t.Fatal("expected group g1")
	}
	if _, exists := g.Members["alice-pub"]; exists {
		t.Fatal("AddMember mutated in-memory Members despite SaveBorgGroup failure")
	}
	if _, err := reg.RevokeMember("g1", "owner-pub"); err == nil {
		t.Fatal("expected RevokeMember error when SaveBorgGroup fails")
	}
	g2, _ := reg.GetGroup("g1")
	if g2.CurrentEpoch != 1 {
		t.Fatalf("RevokeMember mutated in-memory CurrentEpoch to %d despite SaveBorgGroup failure", g2.CurrentEpoch)
	}
	if _, exists := g2.Members["owner-pub"]; !exists {
		t.Fatal("RevokeMember removed owner-pub in memory despite SaveBorgGroup failure")
	}
	if len(g2.Revoked) != 0 {
		t.Fatalf("RevokeMember populated Revoked in memory despite SaveBorgGroup failure: %+v", g2.Revoked)
	}
}

func TestFileObjectStoreTracksUsedBytesQuota(t *testing.T) {
	fs, err := NewFileObjectStore(t.TempDir())
	if err != nil {
		t.Fatalf("NewFileObjectStore: %v", err)
	}
	fs.maxBytes = 100

	b1 := make([]byte, 60)
	b1[0] = 1
	h1 := sha256.Sum256(b1)
	if err := fs.PutObject(h1, b1); err != nil {
		t.Fatalf("PutObject(b1): %v", err)
	}
	if fs.usedBytes != 60 {
		t.Fatalf("expected usedBytes=60, got %d", fs.usedBytes)
	}
	// Duplicate put must be idempotent and not double-count usedBytes.
	if err := fs.PutObject(h1, b1); err != nil {
		t.Fatalf("PutObject(dup b1): %v", err)
	}
	if fs.usedBytes != 60 {
		t.Fatalf("expected usedBytes=60 after duplicate put, got %d", fs.usedBytes)
	}
	b2 := make([]byte, 50)
	b2[0] = 2
	h2 := sha256.Sum256(b2)
	if err := fs.PutObject(h2, b2); err == nil {
		t.Fatal("expected quota exceeded error on 60+50 > 100")
	}
	if fs.usedBytes != 60 {
		t.Fatalf("expected usedBytes unchanged at 60 after rejected put, got %d", fs.usedBytes)
	}
}
