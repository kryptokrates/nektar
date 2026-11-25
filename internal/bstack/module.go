package bstack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/bsv-blockchain/go-sdk/chainhash"
	"github.com/bsv-blockchain/go-sdk/overlay"
	"github.com/bsv-blockchain/go-sdk/overlay/lookup"
	"github.com/bsv-blockchain/go-sdk/script"
	"github.com/bsv-blockchain/go-sdk/transaction"
	sdkpushdrop "github.com/bsv-blockchain/go-sdk/transaction/template/pushdrop"
	"github.com/lightwebinc/bcommon/module"
)

// MarketEvent is emitted whenever the Nektar overlay LookupService admits a verified market artifact.
// The embedded Hub assigns monotonic Seq numbers and fans these out over cursored SSE (/events).
type MarketEvent struct {
	Seq        int64         `json:"seq,omitempty"`
	Topic      string        `json:"topic"`
	TxID       string        `json:"txid"`
	Vout       uint32        `json:"vout"`
	AdmittedAt int64         `json:"admittedAt"`
	Payload    MarketPayload `json:"payload"`
}

// MarketTopicManager implements lightwebinc/bcommon/module.TopicManager for one `tm_nektar_<net>_<nibble>` lane.
type MarketTopicManager struct {
	Net    string
	Topic  string
	Nibble string
	Now    func() time.Time
}

func (m *MarketTopicManager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// ExtractNkt1Payload extracts the nkt1 canonical CBOR payload from a PushDrop locking script or OP_RETURN data push.
func ExtractNkt1Payload(s *script.Script) (MarketPayload, bool, error) {
	if s == nil {
		return MarketPayload{}, false, nil
	}
	// 1. Canonical PushDrop carrier (Lightweb native format).
	if pd := sdkpushdrop.Decode(s); pd != nil && len(pd.Fields) > 0 {
		p, decErr := DecodePayload(pd.Fields[0])
		if decErr == nil {
			return p, true, nil
		}
	}
	// 2. Also inspect raw data pushes (OP_RETURN / script pushes carrying nkt1 CBOR).
	ops, err := s.ParseOps()
	if err != nil {
		return MarketPayload{}, false, nil
	}
	for _, op := range ops {
		if len(op.Data) >= 6 {
			if p, decErr := DecodePayload(op.Data); decErr == nil {
				return p, true, nil
			}
		}
	}
	return MarketPayload{}, false, nil
}

// ValidateMarketPayload verifies the signed body, network tag, domain bounds, lane routing, and expiry.
func ValidateMarketPayload(p MarketPayload, expectedNet, expectedTopic string, now time.Time) error {
	nowMs := now.UnixMilli()
	maxExp := now.Add(MaxLifetime + 5*time.Minute).UnixMilli()
	switch p.Kind {
	case KindWant:
		var sw SignedWant
		if err := json.Unmarshal(p.Body, &sw); err != nil {
			return fmt.Errorf("invalid want JSON: %w", err)
		}
		if sw.Want.WantID == "" || sw.Want.Pub == "" {
			return errors.New("want missing wantId or pub")
		}
		if expectedNet != "" && sw.Want.Net != expectedNet {
			return fmt.Errorf("want net %q != %q", sw.Want.Net, expectedNet)
		}
		if len(sw.Want.Domains) == 0 || len(sw.Want.Domains) > MaxDomains {
			return errors.New("want domains out of range")
		}
		if sw.Want.Exp > 0 && (sw.Want.Exp < nowMs || sw.Want.Exp > maxExp) {
			return errors.New("want expired or lifetime too long")
		}
		if err := VerifyArtifact(sw.Want.Pub, sw.Sig, sw.Want); err != nil {
			return err
		}
		if expectedTopic != "" && !topicMatchesDomains(expectedNet, expectedTopic, sw.Want.Domains) {
			return errors.New("want domain does not match topic lane")
		}
		return nil

	case KindOffer:
		var so SignedOffer
		if err := json.Unmarshal(p.Body, &so); err != nil {
			return fmt.Errorf("invalid offer JSON: %w", err)
		}
		if so.Offer.OfferID == "" || so.Offer.WantID == "" || so.Offer.Pub == "" {
			return errors.New("offer missing required ids or pub")
		}
		if expectedNet != "" && so.Offer.Net != expectedNet {
			return fmt.Errorf("offer net %q != %q", so.Offer.Net, expectedNet)
		}
		if len(so.Offer.Domains) == 0 || len(so.Offer.Domains) > MaxDomains {
			return errors.New("offer domains out of range")
		}
		if so.Offer.Exp > 0 && (so.Offer.Exp < nowMs || so.Offer.Exp > maxExp) {
			return errors.New("offer expired or lifetime too long")
		}
		if err := VerifyArtifact(so.Offer.Pub, so.Sig, so.Offer); err != nil {
			return err
		}
		if expectedTopic != "" && !topicMatchesDomains(expectedNet, expectedTopic, so.Offer.Domains) {
			return errors.New("offer domain does not match topic lane")
		}
		return nil

	case KindAnnounce:
		var sa SignedAnnounce
		if err := json.Unmarshal(p.Body, &sa); err != nil {
			return fmt.Errorf("invalid announce JSON: %w", err)
		}
		if sa.Announce.Pub == "" || len(sa.Announce.Hosts) == 0 {
			return errors.New("announce missing pub or hosts")
		}
		if expectedNet != "" && sa.Announce.Net != expectedNet {
			return fmt.Errorf("announce net %q != %q", sa.Announce.Net, expectedNet)
		}
		if sa.Announce.Exp > 0 && (sa.Announce.Exp < nowMs || sa.Announce.Exp > maxExp) {
			return errors.New("announce expired or lifetime too long")
		}
		if err := VerifyArtifact(sa.Announce.Pub, sa.Sig, sa.Announce); err != nil {
			return err
		}
		if expectedTopic != "" && expectedTopic != LaneOf(expectedNet, DirDomain) {
			return errors.New("announce only allowed on dir lane")
		}
		return nil

	default:
		return fmt.Errorf("unsupported market kind %q", p.Kind)
	}
}

func topicMatchesDomains(net, topic string, domains []string) bool {
	for _, d := range domains {
		if LaneOf(net, d) == topic {
			return true
		}
	}
	return false
}

func (m *MarketTopicManager) IdentifyAdmissibleOutputs(_ context.Context, beef *transaction.Beef, txid *chainhash.Hash, _ []uint32) (overlay.AdmittanceInstructions, error) {
	var out overlay.AdmittanceInstructions
	if beef == nil || txid == nil {
		return out, nil
	}
	tx := beef.FindTransaction(txid.String())
	if tx == nil {
		return out, nil
	}
	now := m.now()
	for i, txOut := range tx.Outputs {
		p, ok, _ := ExtractNkt1Payload(txOut.LockingScript)
		if !ok {
			continue
		}
		if err := ValidateMarketPayload(p, m.Net, m.Topic, now); err == nil {
			out.OutputsToAdmit = append(out.OutputsToAdmit, uint32(i))
		}
	}
	return out, nil
}

func (m *MarketTopicManager) GetDocumentation() string {
	return "Nektar v0.4 market lane topic manager (`tm_nektar_<net>_<nibble>`) validating signed canonical CBOR `nkt1` wants, offers, and directory announcements."
}

func (m *MarketTopicManager) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{
		Name:        m.Topic,
		Description: "Nektar v0.4 signed market lane",
		Version:     "0.4.0",
	}
}

// MarketLookupService implements lightwebinc/bcommon/module.LookupService + SpendListener + Restorer
// for `ls_nektar`.
type MarketLookupService struct {
	mu      sync.RWMutex
	records map[string]indexedRecord // outpoint string -> record
	onAdmit func(MarketEvent)
	host    module.Host
}

type indexedRecord struct {
	seq        int64
	outpoint   transaction.Outpoint
	topic      string
	payload    MarketPayload
	beef       []byte
	admittedAt int64
	wantID     string
	offerID    string
	pub        string
	buyerPub   string
	domains    []string
	exp        int64
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
		beef:       beef,
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
		rec.exp = so.Offer.Exp
	case KindAnnounce:
		var sa SignedAnnounce
		if err := json.Unmarshal(p.Body, &sa); err != nil {
			return err
		}
		rec.pub = sa.Announce.Pub
		rec.domains = append([]string{DirDomain}, sa.Announce.Domains...)
		rec.exp = sa.Announce.Exp
	}
	key := op.String()
	s.mu.Lock()
	if existing, ok := s.records[key]; ok {
		rec.seq = existing.seq
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
	Kind     ArtifactKind `json:"kind,omitempty"`
	WantID   string       `json:"wantId,omitempty"`
	OfferID  string       `json:"offerId,omitempty"`
	Domain   string       `json:"domain,omitempty"`
	Pub      string       `json:"pub,omitempty"`
	BuyerPub string       `json:"buyerPub,omitempty"`
	SinceMs  int64        `json:"sinceMs,omitempty"`
	SinceSeq int64        `json:"sinceSeq,omitempty"`
	Limit    int          `json:"limit,omitempty"`
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

	s.mu.RLock()
	var results []MarketEvent
	var formulas []lookup.LookupFormula
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
		if q.SinceMs > 0 && rec.admittedAt < q.SinceMs {
			continue
		}
		if q.SinceSeq > 0 && rec.seq <= q.SinceSeq {
			continue
		}
		if domLower != "" && !hasDomain(rec.domains, domLower) {
			continue
		}
		results = append(results, MarketEvent{
			Seq:        rec.seq,
			Topic:      rec.topic,
			TxID:       rec.outpoint.Txid.String(),
			Vout:       rec.outpoint.Index,
			AdmittedAt: rec.admittedAt,
			Payload:    rec.payload,
		})
		opCopy := rec.outpoint
		formulas = append(formulas, lookup.LookupFormula{Outpoint: &opCopy})
		if len(results) >= q.Limit {
			break
		}
	}
	s.mu.RUnlock()

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

func (s *MarketLookupService) GetDocumentation() string {
	return "Nektar v0.4 market lookup service (`ls_nektar`) indexing wants, offers, and directory announcements across all 16 nibble lanes."
}

func (s *MarketLookupService) GetMetaData() *overlay.MetaData {
	return &overlay.MetaData{
		Name:        LookupService,
		Description: "Nektar v0.4 market lookup service",
		Version:     "0.4.0",
	}
}

// NewMarketModule returns a Lightweb-compatible module.Factory registering all 16 nibble topics
// (`tm_nektar_<net>_0..f`) and `ls_nektar`.
func NewMarketModule(net string, onAdmit func(MarketEvent)) module.Factory {
	return func(h module.Host) (module.Module, error) {
		topics := make(map[string]module.TopicManager, 16)
		for i := range 16 {
			nibble := fmt.Sprintf("%x", i)
			name := fmt.Sprintf("tm_nektar_%s_%s", net, nibble)
			topics[name] = &MarketTopicManager{
				Net:    net,
				Topic:  name,
				Nibble: nibble,
			}
		}
		return module.Module{
			Topics: topics,
			Lookups: map[string]module.LookupService{
				LookupService: NewMarketLookupService(h, onAdmit),
			},
		}, nil
	}
}
