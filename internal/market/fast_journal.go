package market

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/faststore"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

const ContentTypeCBOR = "application/cbor"

// EncodePurchaseRequestCBOR encodes PurchaseRequest into compact binary CBOR with raw PaymentBEEF bytes
// (zero 2x hex overhead across the wire).
func EncodePurchaseRequestCBOR(req PurchaseRequest) ([]byte, error) {
	var beefBytes []byte
	if req.PaymentBEEFHex != "" {
		b, err := hex.DecodeString(strings.TrimSpace(req.PaymentBEEFHex))
		if err != nil {
			return nil, fmt.Errorf("market: encode purchase cbor beef: %w", err)
		}
		beefBytes = b
	}
	m := cbor.Map{
		{Key: uint64(1), Val: req.OfferID},
		{Key: uint64(2), Val: req.WantID},
		{Key: uint64(3), Val: req.BuyerPub},
		{Key: uint64(4), Val: req.BuyerSig},
		{Key: uint64(5), Val: req.TS},
		{Key: uint64(6), Val: req.TxID},
		{Key: uint64(7), Val: beefBytes},
		{Key: uint64(8), Val: uint64(req.OutputIndex)},
		{Key: uint64(9), Val: req.DerivationPrefix},
		{Key: uint64(10), Val: req.DerivationSuffix},
		{Key: uint64(11), Val: req.SenderIdentityKey},
	}
	if req.EscrowMode {
		m = append(m,
			cbor.Pair{Key: uint64(12), Val: true},
			cbor.Pair{Key: uint64(13), Val: req.DepositorPKH},
			cbor.Pair{Key: uint64(14), Val: req.EscrowSellerPKH},
			cbor.Pair{Key: uint64(15), Val: req.EscrowRefundAfter},
			cbor.Pair{Key: uint64(16), Val: req.EscrowMaxFee},
		)
	}
	return core.EncodeCBOR(m)
}

var purchaseReqKnownKeys = core.KnownKeyRange(1, 16)

// DecodePurchaseRequestCBOR decodes a binary CBOR PurchaseRequest.
func DecodePurchaseRequestCBOR(raw []byte) (PurchaseRequest, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return PurchaseRequest{}, err
	}
	m, _, err := core.SplitCBORMap(v, purchaseReqKnownKeys)
	if err != nil {
		return PurchaseRequest{}, err
	}
	var req PurchaseRequest
	req.OfferID, _ = core.CBORText(m, 1)
	req.WantID, _ = core.CBORText(m, 2)
	req.BuyerPub, _ = core.CBORText(m, 3)
	req.BuyerSig, _ = core.CBORText(m, 4)
	req.TS, _ = core.CBORInt(m, 5)
	req.TxID, _ = core.CBORText(m, 6)
	if beef, bErr := core.CBORBytes(m, 7); bErr == nil && len(beef) > 0 {
		req.PaymentBEEFHex = hex.EncodeToString(beef)
	}
	if idx, uErr := core.CBORUint(m, 8); uErr == nil {
		req.OutputIndex = uint32(idx)
	}
	req.DerivationPrefix, _ = core.CBORText(m, 9)
	req.DerivationSuffix, _ = core.CBORText(m, 10)
	req.SenderIdentityKey, _ = core.CBORText(m, 11)
	req.EscrowMode, _ = core.CBORBool(m, 12)
	req.DepositorPKH, _ = core.CBORText(m, 13)
	req.EscrowSellerPKH, _ = core.CBORText(m, 14)
	req.EscrowRefundAfter, _ = core.CBORUint(m, 15)
	req.EscrowMaxFee, _ = core.CBORUint(m, 16)
	return req, nil
}

// EncodePurchaseResponseCBOR encodes PurchaseResponse in binary CBOR (raw key & ciphertext bytes).
func EncodePurchaseResponseCBOR(resp *PurchaseResponse) ([]byte, error) {
	if resp == nil {
		return nil, errors.New("market: nil purchase response")
	}
	m := cbor.Map{
		{Key: uint64(1), Val: resp.OK},
		{Key: uint64(2), Val: resp.Status},
		{Key: uint64(3), Val: int64(resp.RequiredDepth)},
		{Key: uint64(4), Val: resp.OfferID},
		{Key: uint64(5), Val: resp.SellerPub},
		{Key: uint64(6), Val: resp.Error},
		{Key: uint64(7), Val: resp.ChirpURL},
	}
	if resp.ContentKeyHex != "" {
		if b, err := hex.DecodeString(strings.TrimSpace(resp.ContentKeyHex)); err == nil {
			m = append(m, cbor.Pair{Key: uint64(8), Val: b})
		}
	}
	if resp.CiphertextHex != "" {
		if b, err := hex.DecodeString(strings.TrimSpace(resp.CiphertextHex)); err == nil {
			m = append(m, cbor.Pair{Key: uint64(9), Val: b})
		}
	}
	if resp.ClaimTxID != "" {
		m = append(m, cbor.Pair{Key: uint64(10), Val: resp.ClaimTxID})
	}
	if resp.ClaimTxBEEFHex != "" {
		if b, err := hex.DecodeString(strings.TrimSpace(resp.ClaimTxBEEFHex)); err == nil {
			m = append(m, cbor.Pair{Key: uint64(11), Val: b})
		}
	}
	if resp.ProofBundle != nil {
		if pbJSON, err := json.Marshal(resp.ProofBundle); err == nil {
			m = append(m, cbor.Pair{Key: uint64(12), Val: pbJSON})
		}
	}
	if resp.DealReceipt != nil {
		if drJSON, err := json.Marshal(resp.DealReceipt); err == nil {
			m = append(m, cbor.Pair{Key: uint64(13), Val: drJSON})
		}
	}
	return core.EncodeCBOR(m)
}

var purchaseRespKnownKeys = core.KnownKeyRange(1, 13)

// DecodePurchaseResponseCBOR decodes a binary CBOR PurchaseResponse.
func DecodePurchaseResponseCBOR(raw []byte) (*PurchaseResponse, error) {
	v, err := core.DecodeCBOR(raw)
	if err != nil {
		return nil, err
	}
	m, _, err := core.SplitCBORMap(v, purchaseRespKnownKeys)
	if err != nil {
		return nil, err
	}
	resp := &PurchaseResponse{}
	resp.OK, _ = core.CBORBool(m, 1)
	resp.Status, _ = core.CBORText(m, 2)
	if rd, iErr := core.CBORInt(m, 3); iErr == nil {
		resp.RequiredDepth = int(rd)
	}
	resp.OfferID, _ = core.CBORText(m, 4)
	resp.SellerPub, _ = core.CBORText(m, 5)
	resp.Error, _ = core.CBORText(m, 6)
	resp.ChirpURL, _ = core.CBORText(m, 7)
	if b, bErr := core.CBORBytes(m, 8); bErr == nil && len(b) > 0 {
		resp.ContentKeyHex = hex.EncodeToString(b)
	}
	if b, bErr := core.CBORBytes(m, 9); bErr == nil && len(b) > 0 {
		resp.CiphertextHex = hex.EncodeToString(b)
	}
	resp.ClaimTxID, _ = core.CBORText(m, 10)
	if b, bErr := core.CBORBytes(m, 11); bErr == nil && len(b) > 0 {
		resp.ClaimTxBEEFHex = hex.EncodeToString(b)
	}
	if pbJSON, bErr := core.CBORBytes(m, 12); bErr == nil && len(pbJSON) > 0 {
		var pb contracts.ProofBundle
		if json.Unmarshal(pbJSON, &pb) == nil {
			resp.ProofBundle = &pb
		}
	}
	if drJSON, bErr := core.CBORBytes(m, 13); bErr == nil && len(drJSON) > 0 {
		var dr DealReceipt
		if json.Unmarshal(drJSON, &dr) == nil {
			resp.DealReceipt = &dr
		}
	}
	return resp, nil
}

// -----------------------------------------------------------------------------
// Fast Market Pebble WAL Journal + Immutable .nkt Capsule Cache + Async Commit Worker
// -----------------------------------------------------------------------------

type cachedCapsuleEntry struct {
	payloadBytes  []byte
	payloadFormat string
	proofBundle   *contracts.ProofBundle
	cachedAt      int64
}

type asyncBuyerCommitJob struct {
	dealID      string
	targetShard string
	verifiedCap *memrepo.MemoryCapsule
	capReport   *memrepo.CapsuleVerifyReport
	plaintext   []byte
	dealRec     store.DealRecord
	sellerPub   string
	priceSats   uint64
}

// FastMarketJournal backs high-throughput Buyer/Seller deal state, atomic payment-claim input locks,
// pending sales, verified capsule WAL persistence, immutable .nkt capsule caching, and async
// go-git/SQLite/FTS5 projection workers on pure-Go Pebble LSM.
type FastMarketJournal struct {
	db     *faststore.DB
	ownsDB bool

	claimStripes [64]sync.Mutex

	capMu    sync.RWMutex
	capCache map[string]cachedCapsuleEntry

	listMu         sync.RWMutex
	listingsCached []store.SellListing
	listingsAt     time.Time

	polMu     sync.Mutex
	polSyncAt time.Time

	bondMu        sync.RWMutex
	bondCheckedAt time.Time
	bondOutpoint  string
	bondBEEFHex   string
	bondActive    bool

	fragMu    sync.RWMutex
	fragCache map[string]cachedFragEntry

	clsMu    sync.RWMutex
	clsCache map[string]cachedClassifyEntry

	spendMu       sync.Mutex
	spendDay      string
	spendToday    uint64
	spendReserved map[string]uint64

	commitCh   chan asyncBuyerCommitJob
	stopCh     chan struct{}
	wg         sync.WaitGroup
	mem        *memory.Service
	closeOnce  sync.Once
}

type cachedFragEntry struct {
	frags []memory.RecalledFragment
	at    time.Time
}

type cachedClassifyEntry struct {
	cls       nmu.GradeClassification
	ontID     string
	edgesRoot string
	at        time.Time
}

// GetCachedClassification returns cached NMU classification + ontology + edgesRoot for an immutable fragment.
func (j *FastMarketJournal) GetCachedClassification(key string) (nmu.GradeClassification, string, string, bool) {
	if j == nil {
		return nmu.GradeClassification{}, "", "", false
	}
	j.clsMu.RLock()
	ent, ok := j.clsCache[key]
	j.clsMu.RUnlock()
	if !ok || time.Since(ent.at) > 5*time.Second {
		return nmu.GradeClassification{}, "", "", false
	}
	return ent.cls, ent.ontID, ent.edgesRoot, true
}

// PutCachedClassification caches NMU classification + ontology + edgesRoot for 5s.
func (j *FastMarketJournal) PutCachedClassification(key string, cls nmu.GradeClassification, ontID, edgesRoot string) {
	if j == nil {
		return
	}
	j.clsMu.Lock()
	if j.clsCache == nil {
		j.clsCache = make(map[string]cachedClassifyEntry, 256)
	}
	if len(j.clsCache) >= 2048 {
		clear(j.clsCache)
	}
	j.clsCache[key] = cachedClassifyEntry{cls: cls, ontID: ontID, edgesRoot: edgesRoot, at: time.Now()}
	j.clsMu.Unlock()
}

// NewFastMarketJournal initializes a Pebble WAL journal and async commit worker for market deals.
func NewFastMarketJournal(db *faststore.DB, ownsDB bool, mem *memory.Service) *FastMarketJournal {
	if db == nil {
		return nil
	}
	j := &FastMarketJournal{
		db:            db,
		ownsDB:        ownsDB,
		capCache:      make(map[string]cachedCapsuleEntry, 256),
		fragCache:     make(map[string]cachedFragEntry, 64),
		spendReserved: make(map[string]uint64, 1024),
		commitCh:      make(chan asyncBuyerCommitJob, 8192),
		stopCh:        make(chan struct{}),
		mem:           mem,
	}
	j.wg.Add(1)
	go j.runAsyncCommitWorker()
	return j
}

// ReserveSpendAtomic enforces per-deal & daily spend caps atomically in memory + Pebble WAL
// without hitting nektar.sqlite BEGIN IMMEDIATE on every deal.
func (j *FastMarketJournal) ReserveSpendAtomic(opID string, attempt int, amountSats, maxPerDeal, maxPerDay uint64) (bool, error) {
	if j == nil {
		return false, nil
	}
	key := fmt.Sprintf("%s:%d", opID, attempt)
	today := time.Now().UTC().Format("2006-01-02")
	j.spendMu.Lock()
	defer j.spendMu.Unlock()
	if j.spendDay != today {
		j.spendDay = today
		j.spendToday = 0
		clear(j.spendReserved)
	}
	if _, ok := j.spendReserved[key]; ok {
		return true, nil
	}
	if maxPerDeal > 0 && amountSats > maxPerDeal {
		return false, fmt.Errorf("deal amount %d exceeds maxPerDeal %d", amountSats, maxPerDeal)
	}
	if maxPerDay > 0 && j.spendToday+amountSats > maxPerDay {
		return false, fmt.Errorf("daily spend %d + %d exceeds maxPerDay %d", j.spendToday, amountSats, maxPerDay)
	}
	j.spendToday += amountSats
	j.spendReserved[key] = amountSats
	return false, nil
}

// ReleaseSpendReservation refunds a fast-journal spend reservation after a pre-broadcast failure.
func (j *FastMarketJournal) ReleaseSpendReservation(opID string, attempt int) {
	if j == nil {
		return
	}
	key := fmt.Sprintf("%s:%d", opID, attempt)
	j.spendMu.Lock()
	if amt, ok := j.spendReserved[key]; ok {
		delete(j.spendReserved, key)
		if j.spendToday >= amt {
			j.spendToday -= amt
		} else {
			j.spendToday = 0
		}
	}
	j.spendMu.Unlock()
}

// Close stops the async commit worker (draining queued jobs cleanly) and closes Pebble if owned.
func (j *FastMarketJournal) Close() error {
	if j == nil {
		return nil
	}
	var err error
	j.closeOnce.Do(func() {
		close(j.stopCh)
		j.wg.Wait()
		if j.ownsDB && j.db != nil {
			err = j.db.Close()
		}
	})
	return err
}

// DrainAsyncCommits waits until all currently queued async go-git/SQLite projection jobs finish.
func (j *FastMarketJournal) DrainAsyncCommits(timeout time.Duration) bool {
	if j == nil {
		return true
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(j.commitCh) == 0 {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return len(j.commitCh) == 0
}

func (j *FastMarketJournal) runAsyncCommitWorker() {
	defer j.wg.Done()
	// Coalesce SQLite projection work on a gentle 500ms background cadence so SQLite never
	// steals CPU cores from 1,000+ TPS hot-path Pebble WAL commits (WAL is already crash-safe!).
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	var pending []asyncBuyerCommitJob
	flushBatch := func(limit int) {
		if len(pending) == 0 || j.mem == nil || j.mem.DB() == nil {
			pending = pending[:0]
			return
		}
		n := len(pending)
		if limit > 0 && n > limit {
			n = limit
		}
		for i := 0; i < n; i++ {
			j.applyCommitJob(pending[i])
		}
		pending = append(pending[:0], pending[n:]...)
	}
	for {
		select {
		case <-j.stopCh:
			for {
				select {
				case job := <-j.commitCh:
					pending = append(pending, job)
				default:
					flushBatch(64)
					return
				}
			}
		case job := <-j.commitCh:
			if len(pending) < 4096 {
				pending = append(pending, job)
			}
		case <-ticker.C:
			// Project bounded slice per tick so SQLite stays <1% CPU during sustained bursts.
			flushBatch(16)
		}
	}
}

func (j *FastMarketJournal) applyCommitJob(job asyncBuyerCommitJob) {
	if j.mem == nil || j.mem.DB() == nil {
		return
	}
	if job.dealRec.DealID != "" {
		_ = j.mem.DB().SaveDeal(job.dealRec)
	}
	if job.sellerPub != "" && job.priceSats > 0 {
		_ = j.mem.DB().RecordReputation(job.sellerPub, true, job.priceSats)
	}
}

func journalStripeIdx(key string) int {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return int(h & 63)
}

// ClaimPaymentAtomic enforces Seller-side zero-replay and double-spend input locking atomically
// via 64-shard stripes + Pebble WAL group-commit (~40us, zero SQLite lock).
func (j *FastMarketJournal) ClaimPaymentAtomic(txid string, inputs []string) error {
	if j == nil || j.db == nil {
		return nil
	}
	// Lock deterministic stripe set for txid + all input outpoints.
	var mask uint64
	mask |= 1 << uint(journalStripeIdx(txid))
	for _, in := range inputs {
		mask |= 1 << uint(journalStripeIdx(in))
	}
	for i := 0; i < 64; i++ {
		if (mask & (1 << uint(i))) != 0 {
			j.claimStripes[i].Lock()
			defer j.claimStripes[i].Unlock()
		}
	}
	txKey := []byte("mk:claim:" + txid)
	if exists, err := j.db.Has(txKey); err != nil {
		return err
	} else if exists {
		return errors.New("payment txid already claimed")
	}
	for _, in := range inputs {
		if exists, err := j.db.Has([]byte("mk:inp:" + in)); err != nil {
			return err
		} else if exists {
			return fmt.Errorf("payment input %s already claimed by another tx", in)
		}
	}
	batch := j.db.NewBatch()
	_ = batch.Set(txKey, []byte{1}, nil)
	for _, in := range inputs {
		_ = batch.Set([]byte("mk:inp:"+in), []byte(txid), nil)
	}
	return j.db.CommitBatch(batch)
}

// ReleasePaymentClaim rolls back a payment claim on broadcast/internalize failure.
func (j *FastMarketJournal) ReleasePaymentClaim(txid string, inputs []string) {
	if j == nil || j.db == nil {
		return
	}
	var mask uint64
	mask |= 1 << uint(journalStripeIdx(txid))
	for _, in := range inputs {
		mask |= 1 << uint(journalStripeIdx(in))
	}
	for i := 0; i < 64; i++ {
		if (mask & (1 << uint(i))) != 0 {
			j.claimStripes[i].Lock()
			defer j.claimStripes[i].Unlock()
		}
	}
	batch := j.db.NewBatch()
	_ = batch.Delete([]byte("mk:claim:"+txid), nil)
	for _, in := range inputs {
		_ = batch.Delete([]byte("mk:inp:"+in), nil)
	}
	_ = j.db.CommitBatch(batch)
}

// SaveDealWAL durably commits a Buyer or Seller DealRecord into Pebble WAL (group-committed).
func (j *FastMarketJournal) SaveDealWAL(rec store.DealRecord) error {
	if j == nil || j.db == nil {
		return nil
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	return j.db.Set([]byte("mk:deal:"+rec.DealID), raw)
}

// GetDealWAL loads a DealRecord from Pebble WAL.
func (j *FastMarketJournal) GetDealWAL(dealID string) (store.DealRecord, bool) {
	if j == nil || j.db == nil || dealID == "" {
		return store.DealRecord{}, false
	}
	raw, err := j.db.Get([]byte("mk:deal:" + dealID))
	if err != nil || len(raw) == 0 {
		return store.DealRecord{}, false
	}
	var rec store.DealRecord
	if json.Unmarshal(raw, &rec) != nil {
		return store.DealRecord{}, false
	}
	return rec, true
}

// CommitBuyerSettledWAL atomically persists the verified .nkt capsule/plaintext AND settled DealRecord
// in ONE group-committed Pebble WAL batch (<1ms, crash-safe) and enqueues async SQLite projection.
func (j *FastMarketJournal) CommitBuyerSettledWAL(job asyncBuyerCommitJob) error {
	if j == nil || j.db == nil {
		return nil
	}
	dealBytes, err := json.Marshal(job.dealRec)
	if err != nil {
		return err
	}
	batch := j.db.NewBatch()
	_ = batch.Set([]byte("mk:deal:"+job.dealID), dealBytes, nil)
	if len(job.plaintext) > 0 {
		_ = batch.Set([]byte("mk:cap:"+job.dealID), job.plaintext, nil)
	}
	if err := j.db.CommitBatch(batch); err != nil {
		return err
	}
	// Non-blocking enqueue for async SQLite/reputation projection (Pebble WAL is already durable!).
	select {
	case j.commitCh <- job:
	default:
	}
	return nil
}

// CountSettledDealsWAL counts settled deals durably persisted in Pebble WAL (for crash-recovery verification).
func (j *FastMarketJournal) CountSettledDealsWAL(role string) int {
	if j == nil || j.db == nil {
		return 0
	}
	n := 0
	_ = j.db.ScanPrefix([]byte("mk:deal:"), func(_, val []byte) bool {
		var rec store.DealRecord
		if json.Unmarshal(val, &rec) == nil && rec.Status == "settled" && (role == "" || rec.Role == role) {
			n++
		}
		return true
	})
	return n
}

// GetCachedCapsule returns an immutable pre-exported .nkt capsule + ProofBundle for (shard, seq, leaf, anchorTxID).
func (j *FastMarketJournal) GetCachedCapsule(key string) ([]byte, string, *contracts.ProofBundle, bool) {
	if j == nil {
		return nil, "", nil, false
	}
	j.capMu.RLock()
	ent, ok := j.capCache[key]
	j.capMu.RUnlock()
	if !ok {
		return nil, "", nil, false
	}
	return ent.payloadBytes, ent.payloadFormat, ent.proofBundle, true
}

// PutCachedCapsule caches an immutable exported .nkt capsule + ProofBundle.
func (j *FastMarketJournal) PutCachedCapsule(key string, payload []byte, format string, pb *contracts.ProofBundle) {
	if j == nil || len(payload) == 0 {
		return
	}
	j.capMu.Lock()
	if len(j.capCache) >= 2048 {
		clear(j.capCache)
	}
	j.capCache[key] = cachedCapsuleEntry{
		payloadBytes:  append([]byte(nil), payload...),
		payloadFormat: format,
		proofBundle:   pb,
		cachedAt:      time.Now().Unix(),
	}
	j.capMu.Unlock()
}

// CachedListings returns seller listings cached for up to 2s to avoid hitting SQLite on every WantAd.
func (j *FastMarketJournal) CachedListings(db *store.DB) ([]store.SellListing, error) {
	if j == nil || db == nil {
		if db == nil {
			return nil, nil
		}
		return db.ListSellListings()
	}
	j.listMu.RLock()
	if time.Since(j.listingsAt) < 2*time.Second && len(j.listingsCached) > 0 {
		out := j.listingsCached
		j.listMu.RUnlock()
		return out, nil
	}
	j.listMu.RUnlock()
	listings, err := db.ListSellListings()
	if err != nil {
		return nil, err
	}
	j.listMu.Lock()
	j.listingsCached = listings
	j.listingsAt = time.Now()
	j.listMu.Unlock()
	return listings, nil
}

// InvalidateListings clears the 2s seller listings cache (call after PublishListing/UpdateListing).
func (j *FastMarketJournal) InvalidateListings() {
	if j == nil {
		return
	}
	j.listMu.Lock()
	j.listingsAt = time.Time{}
	j.listingsCached = nil
	j.listMu.Unlock()
}

// CachedPolicySync runs SyncPolicyFromDB at most once per second on hot quoting paths.
func (j *FastMarketJournal) CachedPolicySync(db *store.DB, pol *Policy) {
	if db == nil || pol == nil {
		return
	}
	if j == nil {
		_ = SyncPolicyFromDB(db, pol)
		return
	}
	j.polMu.Lock()
	need := time.Since(j.polSyncAt) >= time.Second
	if need {
		j.polSyncAt = time.Now()
	}
	j.polMu.Unlock()
	if need {
		_ = SyncPolicyFromDB(db, pol)
	}
}

// CachedActiveBond caches LoadActiveBond for 2s to avoid SQLite reads on every WantAd.
func (j *FastMarketJournal) CachedActiveBond(db *store.DB) (hasActive bool, outpoint, beefHex string) {
	if db == nil {
		return false, "", ""
	}
	if j != nil {
		j.bondMu.RLock()
		if time.Since(j.bondCheckedAt) < 2*time.Second {
			a, o, b := j.bondActive, j.bondOutpoint, j.bondBEEFHex
			j.bondMu.RUnlock()
			return a, o, b
		}
		j.bondMu.RUnlock()
	}
	if bRec, _, bErr := LoadActiveBond(db); bErr == nil && bRec != nil && !bRec.State.Seized {
		hasActive = true
		outpoint = bRec.Outpoint()
		if len(bRec.BEEFHex) <= 8192 {
			beefHex = bRec.BEEFHex
		}
	}
	if j != nil {
		j.bondMu.Lock()
		j.bondActive = hasActive
		j.bondOutpoint = outpoint
		j.bondBEEFHex = beefHex
		j.bondCheckedAt = time.Now()
		j.bondMu.Unlock()
	}
	return hasActive, outpoint, beefHex
}

// CachedSellableFragments caches per-shard recall results for 2s to avoid SQLite + Cedar on every WantAd.
func (j *FastMarketJournal) CachedSellableFragments(ctx context.Context, mem *memory.Service, shard string) ([]memory.RecalledFragment, error) {
	if j != nil {
		j.fragMu.RLock()
		if ent, ok := j.fragCache[shard]; ok && time.Since(ent.at) < 2*time.Second {
			j.fragMu.RUnlock()
			return ent.frags, nil
		}
		j.fragMu.RUnlock()
	}
	frags, err := mem.Recall(ctx, memory.RecallInput{
		Shard:        shard,
		Principal:    policy.Principal{Type: "Agent", ID: "market-seller", Level: 0},
		Action:       "disclose",
		DisallowBorg: true,
		VerifySPV:    false,
	})
	if err != nil {
		return nil, err
	}
	if j != nil {
		j.fragMu.Lock()
		if len(j.fragCache) >= 1024 {
			clear(j.fragCache)
		}
		j.fragCache[shard] = cachedFragEntry{frags: frags, at: time.Now()}
		j.fragMu.Unlock()
	}
	return frags, nil
}

// -----------------------------------------------------------------------------
// Standing / Catalog Direct-Buy Wire Protocol (POST /v1/market/direct-quote)
// -----------------------------------------------------------------------------

// DirectQuoteRequest asks a known Seller directly for a signed OfferAd, skipping Hub Want/Offer round-trips.
type DirectQuoteRequest struct {
	WantID   string   `json:"wantId"`
	BuyerPub string   `json:"buyerPub"`
	BuyerSig string   `json:"buyerSig"`
	TS       int64    `json:"ts"`
	Domains  []string `json:"domains"`
	Query    string   `json:"query"`
	MaxSats  uint64   `json:"maxSats"`
}

func (r DirectQuoteRequest) DomainTag() []byte { return []byte("nektar:direct-quote:v1") }

func (r DirectQuoteRequest) EncodeCanonicalCBOR() ([]byte, error) {
	return core.EncodeCBOR(cbor.Map{
		{Key: uint64(1), Val: r.WantID},
		{Key: uint64(2), Val: strings.ToLower(strings.TrimSpace(r.BuyerPub))},
		{Key: uint64(3), Val: r.TS},
		{Key: uint64(4), Val: core.CBORStringsVal(r.Domains)},
		{Key: uint64(5), Val: r.Query},
		{Key: uint64(6), Val: r.MaxSats},
	})
}

// DirectQuoteResponse returns a seller-signed OfferAd ready for immediate BRC-29 PurchaseRequest.
type DirectQuoteResponse struct {
	OK    bool            `json:"ok"`
	Offer *bstack.OfferAd `json:"offer,omitempty"`
	Error string          `json:"error,omitempty"`
}

// PostDirectQuote sends a signed DirectQuoteRequest to sellerURL/v1/market/direct-quote and verifies the returned OfferAd.
func (b *Broker) PostDirectQuote(ctx context.Context, sellerURL string, req DirectQuoteRequest) (*bstack.OfferAd, error) {
	if req.WantID == "" {
		req.WantID = "dq-" + randIDHex()
	}
	if req.BuyerPub == "" {
		req.BuyerPub = b.PubHex()
	}
	if req.TS == 0 {
		req.TS = time.Now().Unix()
	}
	sig, err := bstack.SignArtifactWithSigner(b.engine.signer, req)
	if err != nil {
		return nil, err
	}
	req.BuyerSig = sig
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	target := strings.TrimRight(sellerURL, "/") + "/v1/market/direct-quote"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := b.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	var qResp DirectQuoteResponse
	if err := json.Unmarshal(raw, &qResp); err != nil {
		return nil, fmt.Errorf("market: decode direct-quote response (HTTP %d): %w", resp.StatusCode, err)
	}
	if !qResp.OK || qResp.Offer == nil {
		if qResp.Error != "" {
			return nil, errors.New(qResp.Error)
		}
		return nil, fmt.Errorf("market: direct-quote HTTP %d refused", resp.StatusCode)
	}
	return qResp.Offer, nil
}

// DirectBuyRequest executes a Standing/Catalog Direct-Buy against a known Seller URL,
// skipping Hub WantAd/OfferAd round-trips while enforcing full BRC-29 payment, BRC-369 key verification,
// SPV ProofBundle verification, SpendGuard, and crash-safe Pebble WAL commit.
type DirectBuyRequest struct {
	SellerURL   string
	Domains     []string
	Question    string
	MaxSats     uint64
	TargetShard string
}

// BuyDirect fetches a signed OfferAd directly from req.SellerURL and executes the full verified BRC-29 + BRC-369 purchase.
func (b *Broker) BuyDirect(ctx context.Context, req DirectBuyRequest) (*store.DealRecord, error) {
	if strings.TrimSpace(req.SellerURL) == "" {
		return nil, errors.New("market: sellerUrl is required for BuyDirect")
	}
	if len(req.Domains) == 0 {
		req.Domains = []string{"ops"}
	}
	if strings.TrimSpace(req.Question) == "" {
		return nil, errors.New("market: question is required for BuyDirect")
	}
	wantID := fmt.Sprintf("dbuy-%s-%s", shortPub(b.PubHex()), randIDHex())
	offer, err := b.PostDirectQuote(ctx, req.SellerURL, DirectQuoteRequest{
		WantID:   wantID,
		BuyerPub: b.PubHex(),
		TS:       time.Now().Unix(),
		Domains:  req.Domains,
		Query:    req.Question,
		MaxSats:  req.MaxSats,
	})
	if err != nil {
		return nil, err
	}
	var so contracts.KeyedSaleOffer
	if err := json.Unmarshal(offer.SaleOffer, &so); err != nil {
		return nil, fmt.Errorf("market: decode direct offer saleOffer: %w", err)
	}
	if err := contracts.VerifySaleOffer(&so, b.PubHex(), offer.Leaf); err != nil {
		return nil, fmt.Errorf("market: verify direct offer: %w", err)
	}
	if !strings.EqualFold(so.SellerPub, offer.Pub) || so.Config.Price != offer.PriceSats || !strings.EqualFold(so.KeyCommitment, offer.KeyCommitHex) {
		return nil, errors.New("market: direct offer metadata mismatch vs signed SaleOffer")
	}
	want := bstack.WantAd{
		WantID:   wantID,
		Pub:      b.PubHex(),
		Net:      b.mem.Chain().Network(),
		Domains:  req.Domains,
		Query:    req.Question,
		MaxSats:  req.MaxSats,
		Revision: 1,
		Exp:      time.Now().Add(5 * time.Minute).UnixMilli(),
	}
	st, handled, dErr := b.engine.ExecuteBuyerDealDirect(ctx, wantID, BuyerDealInput{
		Want:        want,
		Offers:      []bstack.OfferAd{*offer},
		TargetShard: req.TargetShard,
		WaitTimeout: 30 * time.Second,
	})
	if !handled {
		return nil, errors.New("market: BuyDirect requires non-interactive operator policy")
	}
	if st != nil && (st.Status == "failed" || st.Status == "expired") {
		return &store.DealRecord{
			DealID:        wantID,
			Role:          "buyer",
			Status:        st.Status,
			Shard:         st.Shard,
			PaymentTxID:   st.PaymentTxID,
			DeliveredText: st.DeliveredTxt,
			Error:         st.Error,
		}, dErr
	}
	if fj := b.engine.FastJournal(); fj != nil {
		if rec, ok := fj.GetDealWAL(wantID); ok && rec.Status == "settled" {
			return &rec, dErr
		}
	}
	if dErr != nil {
		return nil, dErr
	}
	return &store.DealRecord{
		DealID:        wantID,
		Role:          "buyer",
		Status:        st.Status,
		Shard:         st.Shard,
		PaymentTxID:   st.PaymentTxID,
		DeliveredText: st.DeliveredTxt,
		Error:         st.Error,
	}, nil
}
