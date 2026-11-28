package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

type cachedTrustEvalEntry struct {
	eval      *reputation.TrustEvaluation
	vecs      map[string]reputation.Vector6D
	epoch     uint64
	expiresAt time.Time
}

var (
	trustCacheMu    sync.RWMutex
	trustCacheEpoch atomic.Uint64
	trustEvalCache  = make(map[string]cachedTrustEvalEntry, 64)
)

// VerifyDealReceiptForReputation bridges market.VerifyDealReceipt into internal/reputation
// without introducing a circular import between internal/reputation and internal/market.
func VerifyDealReceiptForReputation(token string) (*reputation.DealReceiptBinding, error) {
	dr, err := DecodeDealReceiptToken(token)
	if err != nil {
		return nil, err
	}
	vr := VerifyDealReceipt(*dr, true)
	if !vr.OK || !vr.Bilateral {
		return nil, fmt.Errorf("deal receipt not bilaterally verified: %s", vr.Error)
	}
	digest, err := DealReceiptDigest(*dr)
	if err != nil {
		return nil, err
	}
	return &reputation.DealReceiptBinding{
		DealID:      dr.DealID,
		ReceiptHash: hex.EncodeToString(digest),
		BuyerPub:    strings.ToLower(strings.TrimSpace(dr.BuyerPub)),
		SellerPub:   strings.ToLower(strings.TrimSpace(dr.SellerPub)),
		PriceSats:   dr.PriceSats,
		SettledAt:   dr.SettledAt,
	}, nil
}

// RecomputeReputationForOntology runs deterministic Personalized EigenTrust over all persisted
// verified bilateral attestations for ontologyID and writes updated reputation_6d rows to SQLite.
func RecomputeReputationForOntology(db *store.DB, selfPub, ontologyID string, pinnedPeers map[string]bool) (map[string]reputation.Vector6D, error) {
	if db == nil {
		return nil, errors.New("market: db required")
	}
	ont := reputation.NormalizeOntologyID(ontologyID)
	rows, err := db.ListReputationAttestations(ont, 2000)
	if err != nil {
		return nil, err
	}
	atts := make([]reputation.Attestation, 0, len(rows))
	for _, r := range rows {
		atts = append(atts, reputation.Attestation{
			Version:         1,
			Role:            r.Role,
			IssuerPub:       r.IssuerPub,
			SubjectPub:      r.SubjectPub,
			OntologyID:      r.OntologyID,
			DealID:          r.DealID,
			DealReceiptHash: r.DealReceiptHash,
			Scores: reputation.Vector6D{
				SubjectPub:  r.SubjectPub,
				OntologyID:  r.OntologyID,
				AccBP:       r.AccBP,
				ConsBP:      r.ConsBP,
				DispBP:      r.DispBP,
				PredBP:      r.PredBP,
				CollabBP:    r.CollabBP,
				ProvBP:      r.ProvBP,
				CompositeBP: r.CompositeBP,
			},
			Timestamp:  r.CreatedAt,
			AttestHash: r.AttestHash,
		})
	}
	now := time.Now().Unix()
	vecs := reputation.ComputeOntologyVectors(selfPub, ont, atts, pinnedPeers, now, reputation.DefaultHalfLifeSeconds)
	batch := make([]store.Reputation6DRow, 0, len(vecs))
	for _, v := range vecs {
		batch = append(batch, store.Reputation6DRow{
			SubjectPub:   v.SubjectPub,
			OntologyID:   ont,
			AccBP:        v.AccBP,
			ConsBP:       v.ConsBP,
			DispBP:       v.DispBP,
			PredBP:       v.PredBP,
			CollabBP:     v.CollabBP,
			ProvBP:       v.ProvBP,
			EigenTrustBP: v.EigenTrustBP,
			CompositeBP:  v.CompositeBP,
			DirectDeals:  v.DirectDeals,
			AttestCount:  v.AttestCount,
			UpdatedAt:    now,
		})
	}
	_ = db.UpsertReputation6DBatch(batch)
	trustCacheEpoch.Add(1)
	return vecs, nil
}

// SaveVerifiedAttestation verifies and persists a signed nektarrep1.* attestation without running
// synchronous EigenTrust recomputation (used by high-TPS SSE intake paired with coalesced recompute).
func SaveVerifiedAttestation(
	db *store.DB,
	att *reputation.Attestation,
	knownBinding *reputation.DealReceiptBinding,
) (string, error) {
	if db == nil || att == nil {
		return "", errors.New("market: db and attestation required")
	}
	if _, err := reputation.VerifyAttestation(att, VerifyDealReceiptForReputation, knownBinding); err != nil {
		return "", err
	}
	tok, err := reputation.EncodeAttestationToken(*att)
	if err != nil {
		return "", err
	}
	ont := reputation.NormalizeOntologyID(att.OntologyID)
	row := store.ReputationAttestationRow{
		AttestHash:      att.AttestHash,
		Role:            att.Role,
		IssuerPub:       att.IssuerPub,
		SubjectPub:      att.SubjectPub,
		OntologyID:      ont,
		DealID:          att.DealID,
		DealReceiptHash: att.DealReceiptHash,
		Token:           tok,
		EvidenceHash:    att.EvidenceHash,
		AccBP:           att.Scores.AccBP,
		ConsBP:          att.Scores.ConsBP,
		DispBP:          att.Scores.DispBP,
		PredBP:          att.Scores.PredBP,
		CollabBP:        att.Scores.CollabBP,
		ProvBP:          att.Scores.ProvBP,
		CompositeBP:     att.Scores.CompositeBP,
		CreatedAt:       att.Timestamp,
	}
	if err := db.SaveReputationAttestation(row); err != nil {
		return "", err
	}
	return ont, nil
}

func (b *Broker) scheduleReputationRecompute(ontologyID string) {
	if b == nil {
		return
	}
	ont := reputation.NormalizeOntologyID(ontologyID)
	b.repMu.Lock()
	if b.dirtyRepOnts == nil {
		b.dirtyRepOnts = make(map[string]struct{})
	}
	b.dirtyRepOnts[ont] = struct{}{}
	if ont != reputation.GlobalOntologyID {
		b.dirtyRepOnts[reputation.GlobalOntologyID] = struct{}{}
	}
	b.repMu.Unlock()

	b.startMu.Lock()
	running := b.started
	b.startMu.Unlock()
	if !running {
		b.flushDirtyReputation()
		return
	}
	if b.repWakeCh != nil {
		select {
		case b.repWakeCh <- struct{}{}:
		default:
		}
	}
}

func (b *Broker) flushDirtyReputation() {
	if b == nil || b.mem == nil || b.mem.DB() == nil {
		return
	}
	b.repMu.Lock()
	if len(b.dirtyRepOnts) == 0 {
		b.repMu.Unlock()
		return
	}
	onts := make([]string, 0, len(b.dirtyRepOnts))
	for ont := range b.dirtyRepOnts {
		onts = append(onts, ont)
	}
	clear(b.dirtyRepOnts)
	b.repMu.Unlock()

	var pinned map[string]bool
	if b.engine != nil && b.engine.Policy() != nil {
		pinned = b.engine.Policy().PreferredSellersMap()
	}
	for _, ont := range onts {
		_, _ = RecomputeReputationForOntology(b.mem.DB(), b.PubHex(), ont, pinned)
	}
}

func (b *Broker) runStateFlusher(ctx context.Context) {
	cursorTick := time.NewTicker(1 * time.Second)
	defer cursorTick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-cursorTick.C:
			b.flushDirtyCursors()
			b.flushDirtyReputation()
		case <-b.repWakeCh:
			select {
			case <-ctx.Done():
				return
			case <-time.After(50 * time.Millisecond):
			}
			b.flushDirtyReputation()
		}
	}
}

// RecordAndRecomputeAttestation verifies a signed nektarrep1.* attestation against its bilateral
// nektardr1.* deal receipt, persists it in SQLite, and recomputes ontology-scoped EigenTrust + 6D vectors.
func RecordAndRecomputeAttestation(
	db *store.DB,
	selfPub string,
	att *reputation.Attestation,
	knownBinding *reputation.DealReceiptBinding,
	pinnedPeers map[string]bool,
) error {
	if db == nil || att == nil {
		return errors.New("market: db and attestation required")
	}
	if _, err := reputation.VerifyAttestation(att, VerifyDealReceiptForReputation, knownBinding); err != nil {
		return err
	}
	tok, err := reputation.EncodeAttestationToken(*att)
	if err != nil {
		return err
	}
	ont := reputation.NormalizeOntologyID(att.OntologyID)
	row := store.ReputationAttestationRow{
		AttestHash:      att.AttestHash,
		Role:            att.Role,
		IssuerPub:       att.IssuerPub,
		SubjectPub:      att.SubjectPub,
		OntologyID:      ont,
		DealID:          att.DealID,
		DealReceiptHash: att.DealReceiptHash,
		Token:           tok,
		EvidenceHash:    att.EvidenceHash,
		AccBP:           att.Scores.AccBP,
		ConsBP:          att.Scores.ConsBP,
		DispBP:          att.Scores.DispBP,
		PredBP:          att.Scores.PredBP,
		CollabBP:        att.Scores.CollabBP,
		ProvBP:          att.Scores.ProvBP,
		CompositeBP:     att.Scores.CompositeBP,
		CreatedAt:       att.Timestamp,
	}
	if err := db.SaveReputationAttestation(row); err != nil {
		return err
	}
	if _, err := RecomputeReputationForOntology(db, selfPub, ont, pinnedPeers); err != nil {
		return err
	}
	if ont != reputation.GlobalOntologyID {
		_, _ = RecomputeReputationForOntology(db, selfPub, reputation.GlobalOntologyID, pinnedPeers)
	}
	return nil
}

// EvaluateTrustForOntology runs Google Mangle stratified Datalog trust evaluation over local 6D vectors,
// active checkpoint bonds, and operator pin/block preferences.
func EvaluateTrustForOntology(
	db *store.DB,
	selfPub string,
	ontologyID string,
	pol *Policy,
	bonds []reputation.PeerBondInfo,
	contradictions []reputation.ContradictionRecord,
) (*reputation.TrustEvaluation, map[string]reputation.Vector6D, error) {
	ont := reputation.NormalizeOntologyID(ontologyID)
	preferred, blocked, pinnedMap := pol.SellerLists()

	// Fast-path cache when bonds & contradictions are empty (common quote/rank case) and reputation epoch is unchanged.
	canCache := db != nil && len(bonds) == 0 && len(contradictions) == 0
	var cacheKey string
	curEpoch := trustCacheEpoch.Load()
	if canCache {
		pSorted := append([]string(nil), preferred...)
		bSorted := append([]string(nil), blocked...)
		sort.Strings(pSorted)
		sort.Strings(bSorted)
		cacheKey = fmt.Sprintf("%p|%s|%s|%s|%s", db, selfPub, ont, strings.Join(pSorted, ","), strings.Join(bSorted, ","))
		now := time.Now()
		trustCacheMu.RLock()
		if hit, ok := trustEvalCache[cacheKey]; ok && hit.epoch == curEpoch && now.Before(hit.expiresAt) {
			vecsCopy := make(map[string]reputation.Vector6D, len(hit.vecs))
			for k, v := range hit.vecs {
				vecsCopy[k] = v
			}
			evalCopy := hit.eval
			trustCacheMu.RUnlock()
			return evalCopy, vecsCopy, nil
		}
		trustCacheMu.RUnlock()
	}

	vecs := make(map[string]reputation.Vector6D)
	if db != nil {
		rows, err := db.ListReputation6D(ont, 500)
		if err == nil && len(rows) == 0 {
			// Hydrate on demand from stored attestations if 6D table is not yet populated.
			vecs, _ = RecomputeReputationForOntology(db, selfPub, ont, pinnedMap)
			curEpoch = trustCacheEpoch.Load()
		} else if err == nil {
			for _, r := range rows {
				vecs[r.SubjectPub] = reputation.Vector6D{
					SubjectPub:   r.SubjectPub,
					OntologyID:   r.OntologyID,
					AccBP:        r.AccBP,
					ConsBP:       r.ConsBP,
					DispBP:       r.DispBP,
					PredBP:       r.PredBP,
					CollabBP:     r.CollabBP,
					ProvBP:       r.ProvBP,
					EigenTrustBP: r.EigenTrustBP,
					CompositeBP:  r.CompositeBP,
					DirectDeals:  r.DirectDeals,
					AttestCount:  r.AttestCount,
					UpdatedAt:    r.UpdatedAt,
				}
			}
		}
	}

	vecSlice := make([]reputation.Vector6D, 0, len(vecs))
	for _, v := range vecs {
		vecSlice = append(vecSlice, v)
	}
	eval, err := reputation.EvaluateMangleTrust(ont, vecSlice, bonds, contradictions, preferred, blocked, reputation.TrustThresholds{})
	if err != nil {
		return nil, vecs, err
	}
	if canCache && eval != nil {
		vecsSnap := make(map[string]reputation.Vector6D, len(vecs))
		for k, v := range vecs {
			vecsSnap[k] = v
		}
		trustCacheMu.Lock()
		if len(trustEvalCache) >= 512 {
			clear(trustEvalCache)
		}
		trustEvalCache[cacheKey] = cachedTrustEvalEntry{
			eval:      eval,
			vecs:      vecsSnap,
			epoch:     curEpoch,
			expiresAt: time.Now().Add(10 * time.Second),
		}
		trustCacheMu.Unlock()
	}
	return eval, vecs, nil
}

// PublishAttestation broadcasts a signed `nektarrep1.*` attestation onto the 16-lane overlay (`nkt1` kind=4).
func (b *Broker) PublishAttestation(ctx context.Context, att reputation.Attestation, domains []string) error {
	if b == nil || b.disc == nil || b.engine == nil || b.engine.signer == nil {
		return nil
	}
	tok, err := reputation.EncodeAttestationToken(att)
	if err != nil {
		return err
	}
	cleanDoms := make([]string, 0, len(domains))
	for _, d := range domains {
		if s := strings.ToLower(strings.TrimSpace(d)); s != "" {
			cleanDoms = append(cleanDoms, s)
			if len(cleanDoms) >= bstack.MaxDomains {
				break
			}
		}
	}
	ad := bstack.AttestationAd{
		AttestHash:      att.AttestHash,
		Role:            att.Role,
		Pub:             att.IssuerPub,
		SubjectPub:      att.SubjectPub,
		Net:             b.mem.Chain().Network(),
		Domains:         cleanDoms,
		OntologyID:      att.OntologyID,
		DealID:          att.DealID,
		DealReceiptHash: att.DealReceiptHash,
		Token:           tok,
		CompositeBP:     att.Scores.CompositeBP,
		Exp:             time.Now().Add(bstack.MaxLifetime).UnixMilli(),
	}
	sig, err := bstack.SignArtifactWithSigner(b.engine.signer, ad)
	if err != nil {
		return err
	}
	payload, err := bstack.NewAttestationPayload(bstack.SignedAttestation{Attestation: ad, Sig: sig})
	if err != nil {
		return err
	}
	topics := []string{bstack.LaneOf(ad.Net, bstack.DirDomain)}
	if len(cleanDoms) > 0 {
		topics = lanesForDomains(ad.Net, cleanDoms)
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil
	}
	_, err = b.disc.BuildAndSubmitMarketAd(ctx, b.engine.signer, payload, hosts, topics)
	return err
}

// EmitPostDealAttestation signs, persists, recomputes EigenTrust, and publishes a bilateral nektarrep1.* attestation.
func (e *Engine) EmitPostDealAttestation(
	ctx context.Context,
	role string,
	dr DealReceipt,
	ontologyID string,
	scores reputation.Vector6D,
	domains []string,
) (string, error) {
	if e == nil || e.mem == nil || e.mem.DB() == nil {
		return "", errors.New("market: memory db unavailable")
	}
	digest, err := DealReceiptDigest(dr)
	if err != nil {
		return "", err
	}
	drHash := hex.EncodeToString(digest)
	drTok, _ := EncodeDealReceiptToken(dr)

	issuerPub := strings.ToLower(strings.TrimSpace(e.salePubHex()))
	subjectPub := strings.ToLower(strings.TrimSpace(dr.SellerPub))
	if role == reputation.RoleSellerRatesBuyer {
		subjectPub = strings.ToLower(strings.TrimSpace(dr.BuyerPub))
	}
	if issuerPub == "" || subjectPub == "" || issuerPub == subjectPub {
		return "", errors.New("market: invalid bilateral issuer/subject for attestation")
	}

	ont := reputation.NormalizeOntologyID(ontologyID)
	att := reputation.Attestation{
		Version:          1,
		Role:             role,
		IssuerPub:        issuerPub,
		SubjectPub:       subjectPub,
		OntologyID:       ont,
		DealID:           dr.DealID,
		DealReceiptHash:  drHash,
		DealReceiptToken: drTok,
		Scores:           scores,
		AdvisoryNote:     reputation.AdvisoryReputationNote,
		Timestamp:        time.Now().Unix(),
	}
	if err := reputation.SignAttestation(&att, issuerPub, func(d []byte) ([]byte, error) {
		return e.signSaleDigest(ctx, d)
	}); err != nil {
		return "", err
	}
	binding := &reputation.DealReceiptBinding{
		DealID:      dr.DealID,
		ReceiptHash: drHash,
		BuyerPub:    strings.ToLower(strings.TrimSpace(dr.BuyerPub)),
		SellerPub:   strings.ToLower(strings.TrimSpace(dr.SellerPub)),
		OntologyID:  ont,
		PriceSats:   dr.PriceSats,
		SettledAt:   dr.SettledAt,
	}
	var pinned map[string]bool
	if e.policy != nil {
		pinned = e.policy.PreferredSellersMap()
	}
	if err := RecordAndRecomputeAttestation(e.mem.DB(), issuerPub, &att, binding, pinned); err != nil {
		return "", err
	}
	tok, err := reputation.EncodeAttestationToken(att)
	if err != nil {
		return "", err
	}
	if br := e.getBroker(); br != nil {
		br.spawnTracked(10*time.Second, func(pCtx context.Context) {
			_ = br.PublishAttestation(pCtx, att, domains)
		})
	}
	return tok, nil
}

// RecordTaskOutcomeAndReRate applies a downstream task pass/fail signal (`nektar hook outcome`) to
// the latest (or specified) settled buyer deal, updates r_pred, persists the outcome, and emits an updated nektarrep1.* rating.
func (e *Engine) RecordTaskOutcomeAndReRate(ctx context.Context, dealID string, exitCode int, note string) (*store.Reputation6DRow, error) {
	if e == nil || e.mem == nil || e.mem.DB() == nil {
		return nil, errors.New("market: memory db unavailable")
	}
	var rec store.DealRecord
	var err error
	if strings.TrimSpace(dealID) != "" {
		rec, err = e.mem.DB().GetDeal(strings.TrimSpace(dealID))
		if err != nil {
			return nil, fmt.Errorf("market: deal %q not found: %w", dealID, err)
		}
	} else {
		deals, lErr := e.mem.DB().ListDeals(50)
		if lErr != nil {
			return nil, lErr
		}
		found := false
		for _, d := range deals {
			if d.Role == "buyer" && d.Status == "settled" {
				rec = d
				found = true
				break
			}
		}
		if !found {
			return nil, errors.New("market: no settled buyer deal found to attribute task outcome")
		}
	}

	var dr DealReceipt
	if len(rec.DealReceiptJSON) > 0 {
		_ = json.Unmarshal(rec.DealReceiptJSON, &dr)
	}
	var chosen bstack.OfferAd
	if len(rec.ChosenOfferJSON) > 0 {
		_ = json.Unmarshal(rec.ChosenOfferJSON, &chosen)
	}
	ontID := reputation.NormalizeOntologyID(chosen.OntologyID)
	subjectPub := strings.ToLower(strings.TrimSpace(rec.CounterpartyPub))
	if subjectPub == "" {
		subjectPub = strings.ToLower(strings.TrimSpace(dr.SellerPub))
	}
	if subjectPub == "" {
		return nil, errors.New("market: deal has no seller counterparty pubkey")
	}
	_ = e.mem.DB().RecordDealOutcome(store.DealOutcomeRow{
		DealID:     rec.DealID,
		SubjectPub: subjectPub,
		OntologyID: ontID,
		ExitCode:   exitCode,
		Note:       strings.TrimSpace(note),
		RecordedAt: time.Now().Unix(),
	})

	prevRow, _ := e.mem.DB().GetReputation6D(subjectPub, ontID)
	prevVec := reputation.Vector6D{
		SubjectPub: subjectPub,
		OntologyID: ontID,
		AccBP:      prevRow.AccBP,
		ConsBP:     prevRow.ConsBP,
		DispBP:     prevRow.DispBP,
		PredBP:     prevRow.PredBP,
		CollabBP:   prevRow.CollabBP,
		ProvBP:     prevRow.ProvBP,
	}
	if prevVec.AccBP == 0 && prevVec.DispBP == 0 {
		prevVec = reputation.EvaluatePurchasedFragment(reputation.BuyerEvalInput{
			SellerPub: subjectPub, OntologyID: ontID, SPVVerified: true, MPEVerified: true,
		})
	}
	updated := reputation.ApplyTaskOutcome(prevVec, exitCode)
	if dr.DealID != "" && dr.SellerPub != "" && dr.BuyerPub != "" {
		_, _ = e.EmitPostDealAttestation(ctx, reputation.RoleBuyerRatesSeller, dr, ontID, updated, dr.Domains)
	} else {
		_ = e.mem.DB().UpsertReputation6D(store.Reputation6DRow{
			SubjectPub:   subjectPub,
			OntologyID:   ontID,
			AccBP:        updated.AccBP,
			ConsBP:       updated.ConsBP,
			DispBP:       updated.DispBP,
			PredBP:       updated.PredBP,
			CollabBP:     updated.CollabBP,
			ProvBP:       updated.ProvBP,
			EigenTrustBP: prevRow.EigenTrustBP,
			CompositeBP:  updated.CompositeBP,
			DirectDeals:  prevRow.DirectDeals,
			AttestCount:  prevRow.AttestCount + 1,
			UpdatedAt:    time.Now().Unix(),
		})
	}
	row, err := e.mem.DB().GetReputation6D(subjectPub, ontID)
	if err != nil {
		return nil, err
	}
	return &row, nil
}
