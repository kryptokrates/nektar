package bstack

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/module"

	"github.com/kryptokrates/nektar/internal/nmu"
)

// MarketLookupService implements lightwebinc/bcommon/module.LookupService + SpendListener + Restorer
// for `ls_nektar` with O(1) FIFO eviction and secondary inverted indexes for O(matches) queries.
type MarketLookupService struct {
	mu        sync.RWMutex
	records   map[string]indexedRecord // outpoint string -> record
	order     []string
	orderHead int
	byWantID  map[string]map[string]struct{}
	byOfferID map[string]map[string]struct{}
	byDomain  map[string]map[string]struct{}
	byPub     map[string]map[string]struct{}
	byKind    map[ArtifactKind]map[string]struct{}
	onAdmit   func(MarketEvent)
	host      module.Host
}

const maxIndexedMarketRecords = 262144

type indexedRecord struct {
	seq               int64
	outpoint          transaction.Outpoint
	topic             string
	payload           MarketPayload
	beef              []byte
	admittedAt        int64
	wantID            string
	offerID           string
	pub               string
	buyerPub          string
	subjectPub        string
	domains           []string
	ontologyIDs       []string
	targetOntologyIDs []string
	grades            []nmu.Grade
	pricingModes      []nmu.PricingMode
	usageRights       []nmu.UsageRights
	freshnessClasses  []nmu.FreshnessClass
	minSatsPer1kUnits uint64
	exp               int64
}

// NewMarketLookupService creates an ls_nektar lookup service with an optional live admission callback (for Hub SSE).
func NewMarketLookupService(host module.Host, onAdmit func(MarketEvent)) *MarketLookupService {
	return &MarketLookupService{
		records:   make(map[string]indexedRecord, 1024),
		byWantID:  make(map[string]map[string]struct{}),
		byOfferID: make(map[string]map[string]struct{}),
		byDomain:  make(map[string]map[string]struct{}),
		byPub:     make(map[string]map[string]struct{}),
		byKind:    make(map[ArtifactKind]map[string]struct{}),
		onAdmit:   onAdmit,
		host:      host,
	}
}

func addStrSet(idx map[string]map[string]struct{}, k, opKey string) {
	if k == "" {
		return
	}
	m := idx[k]
	if m == nil {
		m = make(map[string]struct{}, 4)
		idx[k] = m
	}
	m[opKey] = struct{}{}
}

func delStrSet(idx map[string]map[string]struct{}, k, opKey string) {
	if k == "" {
		return
	}
	if m := idx[k]; m != nil {
		delete(m, opKey)
		if len(m) == 0 {
			delete(idx, k)
		}
	}
}

func (s *MarketLookupService) addSecondaryLocked(opKey string, rec *indexedRecord) {
	if rec.wantID != "" {
		addStrSet(s.byWantID, rec.wantID, opKey)
	}
	if rec.offerID != "" {
		addStrSet(s.byOfferID, rec.offerID, opKey)
	}
	if rec.pub != "" {
		addStrSet(s.byPub, rec.pub, opKey)
	}
	for _, d := range rec.domains {
		if dl := strings.ToLower(strings.TrimSpace(d)); dl != "" {
			addStrSet(s.byDomain, dl, opKey)
		}
	}
	if rec.payload.Kind != "" {
		m := s.byKind[rec.payload.Kind]
		if m == nil {
			m = make(map[string]struct{}, 64)
			s.byKind[rec.payload.Kind] = m
		}
		m[opKey] = struct{}{}
	}
}

func (s *MarketLookupService) removeRecordLocked(opKey string) {
	rec, ok := s.records[opKey]
	if !ok {
		return
	}
	delete(s.records, opKey)
	delStrSet(s.byWantID, rec.wantID, opKey)
	delStrSet(s.byOfferID, rec.offerID, opKey)
	delStrSet(s.byPub, rec.pub, opKey)
	for _, d := range rec.domains {
		if dl := strings.ToLower(strings.TrimSpace(d)); dl != "" {
			delStrSet(s.byDomain, dl, opKey)
		}
	}
	if m := s.byKind[rec.payload.Kind]; m != nil {
		delete(m, opKey)
		if len(m) == 0 {
			delete(s.byKind, rec.payload.Kind)
		}
	}
}

func (s *MarketLookupService) AdmissionMode() module.AdmissionMode {
	return module.AdmitLockingScript
}

func (s *MarketLookupService) SpendNotificationMode() module.SpendNotificationMode {
	return module.SpendTxid
}

func (s *MarketLookupService) indexOutput(op transaction.Outpoint, topic string, lock *script.Script, beef []byte) error {
	p, ok, _ := ExtractNkt1Payload(lock)
	if !ok || p.IsUnknownExtensionKind() {
		return nil
	}
	rec := indexedRecord{
		outpoint:   op,
		topic:      topic,
		payload:    p,
		beef:       append([]byte(nil), beef...),
		admittedAt: time.Now().UnixMilli(),
	}
	switch p.Kind {
	case KindWant:
		sw, err := DecodeWantPayload(p.Body)
		if err != nil {
			return err
		}
		rec.wantID = sw.Want.WantID
		rec.pub = sw.Want.Pub
		rec.domains = sw.Want.Domains
		if oid := strings.ToLower(strings.TrimSpace(sw.Want.OntologyID)); oid != "" {
			rec.ontologyIDs = []string{oid}
		}
		if g := nmu.NormalizeGrade(sw.Want.MinGrade); g != "" {
			rec.grades = []nmu.Grade{g}
		}
		rec.pricingModes = []nmu.PricingMode{nmu.EffectivePricingMode(nmu.PricingMode(sw.Want.PricingMode))}
		rec.usageRights = []nmu.UsageRights{nmu.EffectiveRights(nmu.UsageRights(sw.Want.RequiredRights))}
		if fc := nmu.NormalizeFreshness(sw.Want.FreshnessClass); fc != "" {
			rec.freshnessClasses = []nmu.FreshnessClass{fc}
		}
		rec.minSatsPer1kUnits = sw.Want.MaxSatsPer1kUnits
		rec.exp = sw.Want.Exp
	case KindOffer:
		so, err := DecodeOfferPayload(p.Body)
		if err != nil {
			return err
		}
		rec.offerID = so.Offer.OfferID
		rec.wantID = so.Offer.WantID
		rec.pub = so.Offer.Pub
		rec.buyerPub = so.Offer.BuyerPub
		rec.domains = so.Offer.Domains
		if oid := strings.ToLower(strings.TrimSpace(so.Offer.OntologyID)); oid != "" {
			rec.ontologyIDs = []string{oid}
		}
		if g := nmu.NormalizeGrade(so.Offer.Grade); g != "" {
			rec.grades = []nmu.Grade{g}
		}
		rec.pricingModes = []nmu.PricingMode{nmu.EffectivePricingMode(nmu.PricingMode(so.Offer.PricingMode))}
		rec.usageRights = []nmu.UsageRights{nmu.EffectiveRights(nmu.UsageRights(so.Offer.UsageRights))}
		if fc := nmu.NormalizeFreshness(so.Offer.FreshnessClass); fc != "" {
			rec.freshnessClasses = []nmu.FreshnessClass{fc}
		}
		if so.Offer.SatsPer1kUnits > 0 {
			rec.minSatsPer1kUnits = so.Offer.SatsPer1kUnits
		} else if so.Offer.PriceSats > 0 {
			rec.minSatsPer1kUnits = nmu.ComputeSatsPer1kUnits(so.Offer.PriceSats, so.Offer.UnitCount)
		}
		rec.exp = so.Offer.Exp
	case KindAnnounce:
		sa, err := DecodeAnnouncePayload(p.Body)
		if err != nil {
			return err
		}
		rec.pub = sa.Announce.Pub
		rec.domains = append([]string{DirDomain}, sa.Announce.Domains...)
		for _, pk := range sa.Announce.Packs {
			if oid := strings.ToLower(strings.TrimSpace(pk.OntologyID)); oid != "" {
				rec.ontologyIDs = append(rec.ontologyIDs, oid)
			}
			if tid := strings.ToLower(strings.TrimSpace(pk.TargetOntologyID)); tid != "" {
				rec.targetOntologyIDs = append(rec.targetOntologyIDs, tid)
			}
			if g := nmu.NormalizeGrade(pk.Grade); g != "" {
				rec.grades = append(rec.grades, g)
			}
			if len(pk.EnabledModes) > 0 {
				for _, m := range pk.EnabledModes {
					if pm := nmu.NormalizePricingMode(m); nmu.ValidPricingMode(pm) {
						rec.pricingModes = append(rec.pricingModes, pm)
					}
				}
			} else if pk.PricingMode != "" {
				rec.pricingModes = append(rec.pricingModes, nmu.EffectivePricingMode(nmu.PricingMode(pk.PricingMode)))
			} else {
				rec.pricingModes = append(rec.pricingModes, nmu.ModeSpot)
			}
			rec.usageRights = append(rec.usageRights, nmu.EffectiveRights(nmu.UsageRights(pk.UsageRights)))
			if fc := nmu.NormalizeFreshness(pk.FreshnessClass); fc != "" {
				rec.freshnessClasses = append(rec.freshnessClasses, fc)
			}
			rate := pk.SatsPer1kUnits
			if rate == 0 && pk.PriceSats > 0 {
				rate = nmu.ComputeSatsPer1kUnits(pk.PriceSats, pk.UnitCount)
			}
			if rate > 0 && (rec.minSatsPer1kUnits == 0 || rate < rec.minSatsPer1kUnits) {
				rec.minSatsPer1kUnits = rate
			}
		}
		rec.exp = sa.Announce.Exp
	case KindAttestation:
		sat, err := DecodeAttestationPayload(p.Body)
		if err != nil {
			return err
		}
		rec.pub = sat.Attestation.Pub
		rec.subjectPub = sat.Attestation.SubjectPub
		rec.domains = append([]string{DirDomain}, sat.Attestation.Domains...)
		if oid := strings.ToLower(strings.TrimSpace(sat.Attestation.OntologyID)); oid != "" {
			rec.ontologyIDs = []string{oid}
		}
		rec.exp = sat.Attestation.Exp
	}
	key := op.String()
	s.mu.Lock()
	if existing, ok := s.records[key]; ok {
		rec.seq = existing.seq
		s.removeRecordLocked(key)
	} else {
		for len(s.records) >= maxIndexedMarketRecords && s.orderHead < len(s.order) {
			oldKey := s.order[s.orderHead]
			s.order[s.orderHead] = ""
			s.orderHead++
			s.removeRecordLocked(oldKey)
		}
		if s.orderHead > 0 && s.orderHead*2 >= len(s.order) {
			rem := copy(s.order, s.order[s.orderHead:])
			s.order = s.order[:rem]
			s.orderHead = 0
		}
		s.order = append(s.order, key)
	}
	s.records[key] = rec
	s.addSecondaryLocked(key, &rec)
	cb := s.onAdmit
	s.mu.Unlock()

	if s.host != nil {
		s.host.Metrics().Inc("nektar_market_admitted_total", map[string]string{"kind": string(p.Kind)}, 1)
	}
	if cb != nil {
		cb(MarketEvent{
			Seq:        rec.seq,
			Topic:      topic,
			TxID:       op.Txid.String(),
			Vout:       op.Index,
			AdmittedAt: rec.admittedAt,
			Payload:    p,
			BEEF:       append([]byte(nil), rec.beef...),
		})
	}
	return nil
}

// SetSeq stamps a durable Hub-assigned monotonic sequence number on an indexed outpoint.
func (s *MarketLookupService) SetSeq(outpointKey string, seq int64) {
	s.mu.Lock()
	if rec, ok := s.records[outpointKey]; ok {
		rec.seq = seq
		s.records[outpointKey] = rec
	}
	s.mu.Unlock()
}

// PruneExpired evicts all expired records from the in-memory lookup index to bound RAM usage.
func (s *MarketLookupService) PruneExpired(nowMs int64) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	pruned := 0
	for k, rec := range s.records {
		if rec.exp > 0 && rec.exp < nowMs {
			s.removeRecordLocked(k)
			pruned++
		}
	}
	return pruned
}

func (s *MarketLookupService) OutputAdmittedByTopic(_ context.Context, payload *module.OutputAdmittedByTopic) error {
	if payload == nil {
		return nil
	}
	return s.indexOutput(payload.Outpoint, payload.Topic, payload.LockingScript, payload.AtomicBEEF)
}

func (s *MarketLookupService) OutputSpent(_ context.Context, payload *module.OutputSpent) error {
	if payload == nil {
		return nil
	}
	s.mu.Lock()
	s.removeRecordLocked(payload.Outpoint.String())
	s.mu.Unlock()
	return nil
}

func (s *MarketLookupService) OutputEvicted(_ context.Context, outpoint *transaction.Outpoint) error {
	if outpoint == nil {
		return nil
	}
	s.mu.Lock()
	s.removeRecordLocked(outpoint.String())
	s.mu.Unlock()
	return nil
}

func (s *MarketLookupService) Restore(_ context.Context, outputs []module.Output, _ module.RestoreStorage) (int, error) {
	n := 0
	for _, o := range outputs {
		if o.Spent {
			continue
		}
		if err := s.indexOutput(o.Outpoint, o.Topic, o.OutputScript, o.Beef); err == nil {
			n++
		}
	}
	return n, nil
}

// MarketQuery is the structured JSON query accepted by `ls_nektar`.
type MarketQuery struct {
	Kind              ArtifactKind `json:"kind,omitempty"`
	WantID            string       `json:"wantId,omitempty"`
	OfferID           string       `json:"offerId,omitempty"`
	Domain            string       `json:"domain,omitempty"`
	OntologyID        string       `json:"ontologyId,omitempty"`
	TargetOntologyID  string       `json:"targetOntologyId,omitempty"`
	MinGrade          string       `json:"minGrade,omitempty"`
	PricingMode       string       `json:"pricingMode,omitempty"`
	UsageRights       string       `json:"usageRights,omitempty"`
	FreshnessClass    string       `json:"freshnessClass,omitempty"`
	MaxSatsPer1kUnits uint64       `json:"maxSatsPer1kUnits,omitempty"`
	Pub               string       `json:"pub,omitempty"`
	BuyerPub          string       `json:"buyerPub,omitempty"`
	SubjectPub        string       `json:"subjectPub,omitempty"`
	SinceMs           int64        `json:"sinceMs,omitempty"`
	SinceSeq          int64        `json:"sinceSeq,omitempty"`
	Limit             int          `json:"limit,omitempty"`
}

func (s *MarketLookupService) Lookup(_ context.Context, question *lookup.LookupQuestion) (*lookup.LookupAnswer, error) {
	var q MarketQuery
	if question != nil && len(question.Query) > 0 {
		if err := json.Unmarshal(question.Query, &q); err != nil {
			return nil, fmt.Errorf("ls_nektar: invalid query: %w", err)
		}
	}
	if q.Limit <= 0 {
		q.Limit = 100
	} else if q.Limit > 2000 {
		q.Limit = 2000
	}
	nowMs := time.Now().UnixMilli()
	domLower := strings.ToLower(strings.TrimSpace(q.Domain))
	ontLower := strings.ToLower(strings.TrimSpace(q.OntologyID))
	tgtOntLower := strings.ToLower(strings.TrimSpace(q.TargetOntologyID))
	minGrade := nmu.NormalizeGrade(q.MinGrade)
	wantMode := nmu.NormalizePricingMode(q.PricingMode)
	wantRights := nmu.NormalizeRights(q.UsageRights)
	wantFresh := nmu.NormalizeFreshness(q.FreshnessClass)

	needSort := q.SinceSeq > 0 || q.Kind == KindAnnounce

	s.mu.RLock()
	// Pick the smallest selective secondary index set when available (O(matches) vs O(N_records)!).
	var candidateSet map[string]struct{}
	hasCandidateFilter := false
	considerSet := func(m map[string]struct{}) {
		if !hasCandidateFilter || len(m) < len(candidateSet) {
			candidateSet = m
			hasCandidateFilter = true
		}
	}
	if q.OfferID != "" {
		considerSet(s.byOfferID[q.OfferID])
	}
	if q.WantID != "" {
		considerSet(s.byWantID[q.WantID])
	}
	if q.Pub != "" {
		considerSet(s.byPub[q.Pub])
	}
	if domLower != "" {
		considerSet(s.byDomain[domLower])
	}
	if q.Kind != "" {
		considerSet(s.byKind[q.Kind])
	}

	matchRec := func(rec indexedRecord) bool {
		if rec.exp > 0 && rec.exp < nowMs {
			return false
		}
		if q.Kind != "" && rec.payload.Kind != q.Kind {
			return false
		}
		if q.WantID != "" && rec.wantID != q.WantID {
			return false
		}
		if q.OfferID != "" && rec.offerID != q.OfferID {
			return false
		}
		if q.Pub != "" && rec.pub != q.Pub {
			return false
		}
		if q.BuyerPub != "" && rec.buyerPub != q.BuyerPub {
			return false
		}
		if q.SubjectPub != "" && !strings.EqualFold(rec.subjectPub, q.SubjectPub) {
			return false
		}
		if q.SinceMs > 0 && rec.admittedAt < q.SinceMs {
			return false
		}
		if q.SinceSeq > 0 && rec.seq <= q.SinceSeq {
			return false
		}
		if domLower != "" && !hasDomain(rec.domains, domLower) {
			return false
		}
		if ontLower != "" && !hasDomain(rec.ontologyIDs, ontLower) {
			return false
		}
		if tgtOntLower != "" && !hasDomain(rec.targetOntologyIDs, tgtOntLower) {
			return false
		}
		if minGrade != "" && !matchesMinGrade(rec.grades, minGrade) {
			return false
		}
		if wantMode != "" && !matchesPricingMode(rec.pricingModes, wantMode) {
			return false
		}
		if wantRights != "" && !matchesUsageRights(rec.usageRights, wantRights) {
			return false
		}
		if wantFresh != "" && !matchesFreshness(rec.freshnessClasses, wantFresh) {
			return false
		}
		if q.MaxSatsPer1kUnits > 0 && (rec.minSatsPer1kUnits == 0 || rec.minSatsPer1kUnits > q.MaxSatsPer1kUnits) {
			return false
		}
		return true
	}

	var matched []indexedRecord
	if hasCandidateFilter {
		for k := range candidateSet {
			rec, ok := s.records[k]
			if !ok || !matchRec(rec) {
				continue
			}
			matched = append(matched, rec)
			if !needSort && len(matched) >= q.Limit {
				break
			}
		}
	} else {
		for _, rec := range s.records {
			if !matchRec(rec) {
				continue
			}
			matched = append(matched, rec)
			if !needSort && len(matched) >= q.Limit {
				break
			}
		}
	}
	s.mu.RUnlock()

	if q.SinceSeq > 0 && len(matched) > 1 {
		sort.Slice(matched, func(i, j int) bool {
			if matched[i].seq != matched[j].seq {
				return matched[i].seq < matched[j].seq
			}
			return matched[i].outpoint.String() < matched[j].outpoint.String()
		})
	} else if q.Kind == KindAnnounce && len(matched) > 1 {
		// Deterministic ordering across sibling Hubs prevents non-deterministic map-iteration truncation
		// from causing false multi-host WithholdingWarning quarantines when >Limit peers exist.
		sort.Slice(matched, func(i, j int) bool {
			if matched[i].pub != matched[j].pub {
				return matched[i].pub < matched[j].pub
			}
			if matched[i].exp != matched[j].exp {
				return matched[i].exp > matched[j].exp
			}
			return matched[i].outpoint.String() < matched[j].outpoint.String()
		})
	}
	if len(matched) > q.Limit {
		matched = matched[:q.Limit]
	}

	results := make([]MarketEvent, 0, len(matched))
	formulas := make([]lookup.LookupFormula, 0, len(matched))
	for _, rec := range matched {
		results = append(results, MarketEvent{
			Seq:        rec.seq,
			Topic:      rec.topic,
			TxID:       rec.outpoint.Txid.String(),
			Vout:       rec.outpoint.Index,
			AdmittedAt: rec.admittedAt,
			Payload:    rec.payload,
			BEEF:       append([]byte(nil), rec.beef...),
		})
		opCopy := rec.outpoint
		formulas = append(formulas, lookup.LookupFormula{Outpoint: &opCopy})
	}

	return &lookup.LookupAnswer{
		Type:     lookup.AnswerTypeFreeform,
		Formulas: formulas,
		Result:   results,
	}, nil
}

func hasDomain(domains []string, want string) bool {
	for _, d := range domains {
		if strings.ToLower(strings.TrimSpace(d)) == want {
			return true
		}
	}
	return false
}

func matchesMinGrade(grades []nmu.Grade, min nmu.Grade) bool {
	for _, g := range grades {
		if nmu.MeetsMinGrade(g, min) {
			return true
		}
	}
	return false
}

func matchesPricingMode(modes []nmu.PricingMode, want nmu.PricingMode) bool {
	for _, m := range modes {
		if m == want {
			return true
		}
	}
	return false
}

func matchesUsageRights(rights []nmu.UsageRights, want nmu.UsageRights) bool {
	for _, r := range rights {
		if nmu.MeetsRequiredRights(r, want) {
			return true
		}
	}
	return false
}

func matchesFreshness(classes []nmu.FreshnessClass, want nmu.FreshnessClass) bool {
	for _, c := range classes {
		if c == want {
			return true
		}
	}
	return false
}

func (s *MarketLookupService) GetDocumentation() string {
	return "Nektar v0.1 market lookup service (`ls_nektar`) indexing wants, offers, and directory announcements across all 16 nibble lanes."
}

func (s *MarketLookupService) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{
		Name:        LookupService,
		Description: "Nektar v0.1 market lookup service",
		Version:     "0.1.0",
	}
}
