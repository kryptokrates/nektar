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
// for `ls_nektar`.
type MarketLookupService struct {
	mu      sync.RWMutex
	records map[string]indexedRecord // outpoint string -> record
	onAdmit func(MarketEvent)
	host    module.Host
}

const maxIndexedMarketRecords = 10000

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
		records: make(map[string]indexedRecord),
		onAdmit: onAdmit,
		host:    host,
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
	if !ok {
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
		var sw SignedWant
		if err := json.Unmarshal(p.Body, &sw); err != nil {
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
		var so SignedOffer
		if err := json.Unmarshal(p.Body, &so); err != nil {
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
		var sa SignedAnnounce
		if err := json.Unmarshal(p.Body, &sa); err != nil {
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
		var sat SignedAttestation
		if err := json.Unmarshal(p.Body, &sat); err != nil {
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
	nowMs := time.Now().UnixMilli()
	s.mu.Lock()
	if existing, ok := s.records[key]; ok {
		rec.seq = existing.seq
	} else if len(s.records) >= maxIndexedMarketRecords {
		// Evict expired first, then oldest admitted record to keep RAM strictly bounded.
		for k, r := range s.records {
			if r.exp <= 0 || r.exp < nowMs {
				delete(s.records, k)
			}
		}
		if len(s.records) >= maxIndexedMarketRecords {
			var oldestKey string
			var oldestAt int64
			for k, r := range s.records {
				if oldestKey == "" || r.admittedAt < oldestAt {
					oldestKey = k
					oldestAt = r.admittedAt
				}
			}
			if oldestKey != "" {
				delete(s.records, oldestKey)
			}
		}
	}
	s.records[key] = rec
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
			delete(s.records, k)
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
	delete(s.records, payload.Outpoint.String())
	s.mu.Unlock()
	return nil
}

func (s *MarketLookupService) OutputEvicted(_ context.Context, outpoint *transaction.Outpoint) error {
	if outpoint == nil {
		return nil
	}
	s.mu.Lock()
	delete(s.records, outpoint.String())
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
	if q.Limit <= 0 || q.Limit > 500 {
		q.Limit = 100
	}
	nowMs := time.Now().UnixMilli()
	domLower := strings.ToLower(strings.TrimSpace(q.Domain))
	ontLower := strings.ToLower(strings.TrimSpace(q.OntologyID))
	tgtOntLower := strings.ToLower(strings.TrimSpace(q.TargetOntologyID))
	minGrade := nmu.NormalizeGrade(q.MinGrade)
	wantMode := nmu.NormalizePricingMode(q.PricingMode)
	wantRights := nmu.NormalizeRights(q.UsageRights)
	wantFresh := nmu.NormalizeFreshness(q.FreshnessClass)

	s.mu.RLock()
	var matched []indexedRecord
	for _, rec := range s.records {
		if rec.exp > 0 && rec.exp < nowMs {
			continue
		}
		if q.Kind != "" && rec.payload.Kind != q.Kind {
			continue
		}
		if q.WantID != "" && rec.wantID != q.WantID {
			continue
		}
		if q.OfferID != "" && rec.offerID != q.OfferID {
			continue
		}
		if q.Pub != "" && rec.pub != q.Pub {
			continue
		}
		if q.BuyerPub != "" && rec.buyerPub != q.BuyerPub {
			continue
		}
		if q.SubjectPub != "" && !strings.EqualFold(rec.subjectPub, q.SubjectPub) {
			continue
		}
		if q.SinceMs > 0 && rec.admittedAt < q.SinceMs {
			continue
		}
		if q.SinceSeq > 0 && rec.seq <= q.SinceSeq {
			continue
		}
		if domLower != "" && !hasDomain(rec.domains, domLower) {
			continue
		}
		if ontLower != "" && !hasDomain(rec.ontologyIDs, ontLower) {
			continue
		}
		if tgtOntLower != "" && !hasDomain(rec.targetOntologyIDs, tgtOntLower) {
			continue
		}
		if minGrade != "" && !matchesMinGrade(rec.grades, minGrade) {
			continue
		}
		if wantMode != "" && !matchesPricingMode(rec.pricingModes, wantMode) {
			continue
		}
		if wantRights != "" && !matchesUsageRights(rec.usageRights, wantRights) {
			continue
		}
		if wantFresh != "" && !matchesFreshness(rec.freshnessClasses, wantFresh) {
			continue
		}
		if q.MaxSatsPer1kUnits > 0 && (rec.minSatsPer1kUnits == 0 || rec.minSatsPer1kUnits > q.MaxSatsPer1kUnits) {
			continue
		}
		matched = append(matched, rec)
		if q.SinceSeq <= 0 && len(matched) >= q.Limit {
			break
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
