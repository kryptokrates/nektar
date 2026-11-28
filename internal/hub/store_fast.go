package hub

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/faststore"
)

type fastHubCarrierRec struct {
	CarrierTxID     string `json:"txid"`
	CarrierOutpoint string `json:"op"`
	Topic           string `json:"topic"`
	SweptBy         string `json:"sweptBy,omitempty"`
}

type fastHubOutpointRec struct {
	Seq         int64  `json:"seq"`
	Topic       string `json:"topic"`
	BlockHeight int64  `json:"h,omitempty"`
	Spent       bool   `json:"spent,omitempty"`
}

// fastEventStore backs Hub hot-path event admissions, carrier claims, and BEEF blobs on Pebble LSM
// with 64-shard outpoint locking and WAL group-commit (zero SQLite lock, zero NTFS temp+rename).
type fastEventStore struct {
	db      *faststore.DB
	ownsDB  bool
	nextSeq atomic.Int64
	stripes [64]sync.Mutex
}

func newFastEventStore(db *faststore.DB, ownsDB bool) *fastEventStore {
	if db == nil {
		return nil
	}
	f := &fastEventStore{db: db, ownsDB: ownsDB}
	// Recover highest persisted seq on startup.
	var maxSeq int64
	_ = db.ScanPrefix([]byte("hb:ev:"), func(k, _ []byte) bool {
		if len(k) == 6+8 {
			seq := int64(binary.BigEndian.Uint64(k[6:]))
			if seq > maxSeq {
				maxSeq = seq
			}
		}
		return true
	})
	f.nextSeq.Store(maxSeq)
	return f
}

func (f *fastEventStore) Close() error {
	if f == nil || !f.ownsDB || f.db == nil {
		return nil
	}
	return f.db.Close()
}

func fastStripeIdx(key string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h & 63)
}

func fastEventKey(seq int64) []byte {
	k := make([]byte, 6+8)
	copy(k, "hb:ev:")
	binary.BigEndian.PutUint64(k[6:], uint64(seq))
	return k
}

func (f *fastEventStore) LatestSeq() int64 {
	if f == nil {
		return 0
	}
	return f.nextSeq.Load()
}

func (f *fastEventStore) AppendEventWithCarrierClaim(ev StoredEvent, fundingOutpoint string) (int64, bool, error) {
	if ev.AdmittedAt == 0 {
		ev.AdmittedAt = time.Now().UnixMilli()
	}
	// Lock ONLY the relevant stripe(s) so all 16 lanes & distinct outpoints commit in full parallel!
	s1 := fastStripeIdx(ev.Outpoint)
	s2 := -1
	if fundingOutpoint != "" {
		s2 = fastStripeIdx(fundingOutpoint)
		if s2 == s1 {
			s2 = -1
		}
	}
	if s2 >= 0 {
		if s1 < s2 {
			f.stripes[s1].Lock()
			f.stripes[s2].Lock()
			defer f.stripes[s2].Unlock()
			defer f.stripes[s1].Unlock()
		} else {
			f.stripes[s2].Lock()
			f.stripes[s1].Lock()
			defer f.stripes[s1].Unlock()
			defer f.stripes[s2].Unlock()
		}
	} else {
		f.stripes[s1].Lock()
		defer f.stripes[s1].Unlock()
	}

	batch := f.db.NewBatch()
	if fundingOutpoint != "" && ev.TxID != "" {
		ck := []byte("hb:car:" + fundingOutpoint)
		if raw, err := f.db.Get(ck); err == nil && len(raw) > 0 {
			var cre fastHubCarrierRec
			if json.Unmarshal(raw, &cre) == nil {
				if cre.SweptBy != "" {
					batch.Close()
					return 0, false, ErrCarrierRetracted
				}
				if cre.CarrierTxID != ev.TxID {
					batch.Close()
					return 0, false, ErrCarrierFundingReused
				}
			}
		} else if err != nil && !errors.Is(err, faststore.ErrNotFound) {
			batch.Close()
			return 0, false, err
		} else {
			cb, _ := json.Marshal(fastHubCarrierRec{
				CarrierTxID:     ev.TxID,
				CarrierOutpoint: ev.Outpoint,
				Topic:           ev.Topic,
			})
			_ = batch.Set(ck, cb, nil)
		}
	}

	opKey := []byte("hb:op:" + ev.Outpoint)
	if raw, err := f.db.Get(opKey); err == nil && len(raw) > 0 {
		var orec fastHubOutpointRec
		if json.Unmarshal(raw, &orec) == nil && orec.Seq > 0 {
			if len(ev.BEEF) > 0 && ev.BlockHeight > orec.BlockHeight {
				orec.BlockHeight = ev.BlockHeight
				ob, _ := json.Marshal(orec)
				_ = batch.Set(opKey, ob, nil)
				if ev.TxID != "" {
					_ = batch.Set([]byte("hb:beef:"+ev.TxID), ev.BEEF, nil)
				}
				if cErr := f.db.CommitBatch(batch); cErr != nil {
					return orec.Seq, false, cErr
				}
				return orec.Seq, false, nil
			}
			batch.Close()
			return orec.Seq, false, nil
		}
	} else if err != nil && !errors.Is(err, faststore.ErrNotFound) {
		batch.Close()
		return 0, false, err
	}

	seq := f.nextSeq.Add(1)
	evCopy := ev
	evCopy.Seq = seq
	evCopy.BEEF = nil // stored once under hb:beef:<txid>
	evBytes, err := json.Marshal(evCopy)
	if err != nil {
		batch.Close()
		return 0, false, err
	}
	opBytes, _ := json.Marshal(fastHubOutpointRec{
		Seq:         seq,
		Topic:       ev.Topic,
		BlockHeight: ev.BlockHeight,
	})
	_ = batch.Set(fastEventKey(seq), evBytes, nil)
	_ = batch.Set(opKey, opBytes, nil)
	if len(ev.BEEF) > 0 && ev.TxID != "" {
		_ = batch.Set([]byte("hb:beef:"+ev.TxID), ev.BEEF, nil)
	}
	if err := f.db.CommitBatch(batch); err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func (f *fastEventStore) FindEventTopicByOutpoint(outpoint string) (string, bool, bool, error) {
	raw, err := f.db.Get([]byte("hb:op:" + outpoint))
	if err != nil {
		if errors.Is(err, faststore.ErrNotFound) {
			return "", false, false, nil
		}
		return "", false, false, err
	}
	var orec fastHubOutpointRec
	if err := json.Unmarshal(raw, &orec); err != nil {
		return "", false, false, err
	}
	return orec.Topic, orec.Spent, true, nil
}

func (f *fastEventStore) MarkOutpointSpent(outpoint string) (string, bool, error) {
	sh := &f.stripes[fastStripeIdx(outpoint)]
	sh.Lock()
	defer sh.Unlock()
	opKey := []byte("hb:op:" + outpoint)
	raw, err := f.db.Get(opKey)
	if err != nil {
		if errors.Is(err, faststore.ErrNotFound) {
			return "", false, nil
		}
		return "", false, err
	}
	var orec fastHubOutpointRec
	if err := json.Unmarshal(raw, &orec); err != nil {
		return "", false, err
	}
	if !orec.Spent {
		orec.Spent = true
		ob, _ := json.Marshal(orec)
		if err := f.db.Set(opKey, ob); err != nil {
			return "", false, err
		}
	}
	return orec.Topic, true, nil
}

func (f *fastEventStore) MarkCarrierFundingSwept(fundingOutpoint, spendingTxID string) (string, string, bool, error) {
	sh := &f.stripes[fastStripeIdx(fundingOutpoint)]
	sh.Lock()
	defer sh.Unlock()
	ck := []byte("hb:car:" + fundingOutpoint)
	raw, err := f.db.Get(ck)
	if err != nil {
		if errors.Is(err, faststore.ErrNotFound) {
			return "", "", false, nil
		}
		return "", "", false, err
	}
	var cre fastHubCarrierRec
	if err := json.Unmarshal(raw, &cre); err != nil {
		return "", "", false, err
	}
	if cre.SweptBy == "" {
		cre.SweptBy = spendingTxID
		cb, _ := json.Marshal(cre)
		_ = f.db.Set(ck, cb)
	}
	return cre.CarrierOutpoint, cre.Topic, true, nil
}

func (f *fastEventStore) ListEventsSince(afterSeq int64, domains []string, limit int) ([]bstack.MarketEvent, error) {
	if limit <= 0 {
		limit = 200
	}
	filter := make(map[string]bool, len(domains))
	for _, d := range domains {
		if d = strings.ToLower(strings.TrimSpace(d)); d != "" {
			filter[d] = true
		}
	}
	nowMs := time.Now().UnixMilli()
	out := make([]bstack.MarketEvent, 0, min(limit, 64))
	// Iterate from afterSeq+1 upward using Pebble key range scan.
	startKey := fastEventKey(afterSeq + 1)
	iter, err := f.db.Raw().NewIter(nil)
	if err != nil {
		return nil, err
	}
	defer iter.Close()
	for iter.SeekGE(startKey); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) != 14 || string(k[:6]) != "hb:ev:" {
			break
		}
		var ev StoredEvent
		if json.Unmarshal(iter.Value(), &ev) != nil {
			continue
		}
		if ev.Kind == "module" {
			continue
		}
		if ev.Exp > 0 && ev.Exp < nowMs {
			continue
		}
		if len(filter) > 0 && !matchesAnyDomain(ev.Domains, filter) {
			continue
		}
		if ev.TxID != "" {
			if raw, gErr := f.db.Get([]byte("hb:beef:" + ev.TxID)); gErr == nil && len(raw) > 0 {
				ev.BEEF = raw
			}
		}
		out = append(out, bstack.MarketEvent{
			Seq:        ev.Seq,
			Topic:      ev.Topic,
			TxID:       ev.TxID,
			Vout:       ev.Vout,
			AdmittedAt: ev.AdmittedAt,
			Payload:    ev.Payload,
			BEEF:       ev.BEEF,
		})
		if len(out) >= limit {
			break
		}
	}
	return out, iter.Error()
}

func (f *fastEventStore) EventsInRangeScanned(fromSeq, toSeq int64, topics []string, limit int) ([]StoredEvent, int64, int, error) {
	if fromSeq < 1 {
		fromSeq = 1
	}
	if toSeq < fromSeq {
		return nil, fromSeq - 1, 0, nil
	}
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	wantTopic := make(map[string]bool, len(topics))
	for _, t := range topics {
		if t = strings.TrimSpace(t); t != "" {
			wantTopic[t] = true
		}
	}
	nowMs := time.Now().UnixMilli()
	nextSeq := fromSeq - 1
	rawCount := 0
	out := make([]StoredEvent, 0, min(limit, 64))
	startKey := fastEventKey(fromSeq)
	endKey := fastEventKey(toSeq)
	iter, err := f.db.Raw().NewIter(nil)
	if err != nil {
		return nil, nextSeq, 0, err
	}
	defer iter.Close()
	for iter.SeekGE(startKey); iter.Valid(); iter.Next() {
		k := iter.Key()
		if len(k) != 14 || string(k[:6]) != "hb:ev:" || string(k) > string(endKey) {
			break
		}
		var ev StoredEvent
		if json.Unmarshal(iter.Value(), &ev) != nil {
			continue
		}
		rawCount++
		if ev.Seq > nextSeq {
			nextSeq = ev.Seq
		}
		if ev.Exp > 0 && ev.Exp < nowMs {
			continue
		}
		if opRaw, oErr := f.db.Get([]byte("hb:op:" + ev.Outpoint)); oErr == nil && len(opRaw) > 0 {
			var orec fastHubOutpointRec
			if json.Unmarshal(opRaw, &orec) == nil && orec.Spent {
				continue
			}
		}
		matchedTopic := storedEventMatchesTopicFilter(ev, wantTopic)
		if len(wantTopic) > 0 && matchedTopic == "" {
			continue
		}
		if matchedTopic != "" {
			ev.Topic = matchedTopic
		}
		if ev.TxID != "" {
			if raw, gErr := f.db.Get([]byte("hb:beef:" + ev.TxID)); gErr == nil && len(raw) > 0 {
				ev.BEEF = raw
			}
		}
		out = append(out, ev)
		if rawCount >= limit || len(out) >= limit {
			break
		}
	}
	return out, nextSeq, rawCount, iter.Error()
}
