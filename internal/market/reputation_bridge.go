package market

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
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
	for _, v := range vecs {
		_ = db.UpsertReputation6D(store.Reputation6DRow{
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
	return vecs, nil
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
	var preferred, blocked []string
	pinnedMap := make(map[string]bool)
	if pol != nil {
		pol.mu.RLock()
		for p, ok := range pol.PreferredSellers {
			if ok {
				lp := strings.ToLower(strings.TrimSpace(p))
				preferred = append(preferred, lp)
				pinnedMap[lp] = true
			}
		}
		for p, ok := range pol.BlockedSellers {
			if ok {
				blocked = append(blocked, strings.ToLower(strings.TrimSpace(p)))
			}
		}
		pol.mu.RUnlock()
	}

	vecs := make(map[string]reputation.Vector6D)
	if db != nil {
		rows, err := db.ListReputation6D(ont, 500)
		if err == nil && len(rows) == 0 {
			// Hydrate on demand from stored attestations if 6D table is not yet populated.
			vecs, _ = RecomputeReputationForOntology(db, selfPub, ont, pinnedMap)
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
	return eval, vecs, nil
}

// PublishAttestation broadcasts a signed `nektarrep1.*` attestation onto the 16-lane overlay (`nkt1` kind=4).
func (b *Broker) PublishAttestation(ctx context.Context, att reputation.Attestation, domains []string) error {
	if b == nil || b.disc == nil || b.engine == nil || b.engine.saleKey == nil {
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
	sig, err := bstack.SignArtifact(b.engine.saleKey, ad)
	if err != nil {
		return err
	}
	body, err := json.Marshal(bstack.SignedAttestation{Attestation: ad, Sig: sig})
	if err != nil {
		return err
	}
	payload := bstack.MarketPayload{Kind: bstack.KindAttestation, Body: body}
	topics := []string{bstack.LaneOf(ad.Net, bstack.DirDomain)}
	if len(cleanDoms) > 0 {
		topics = lanesForDomains(ad.Net, cleanDoms)
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil
	}
	_, err = b.disc.BuildAndSubmitMarketAd(ctx, b.engine.saleKey, payload, hosts, topics)
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
		e.policy.mu.RLock()
		pinned = make(map[string]bool, len(e.policy.PreferredSellers))
		for p, ok := range e.policy.PreferredSellers {
			if ok {
				pinned[strings.ToLower(strings.TrimSpace(p))] = true
			}
		}
		e.policy.mu.RUnlock()
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
