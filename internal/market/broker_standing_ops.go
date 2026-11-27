package market

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/nmu"
)

// StartStandingOrder launches a durable 0%-CPU Restate StandingOrder workflow ("Limit-Order for Knowledge").
func (b *Broker) StartStandingOrder(ctx context.Context, in StandingOrderInput) (*StandingOrderState, error) {
	rc := b.RestateClient()
	if rc == nil {
		return nil, errors.New("market: Restate durable executor is required for StandingOrders (start without --no-restate)")
	}
	if len(in.Domains) == 0 {
		in.Domains = []string{"ops"}
	}
	if err := nmu.ValidateWantFilters(in.MinGrade, in.FreshnessClass, in.RequiredRights, in.PricingMode); err != nil {
		return nil, err
	}
	if in.OrderID == "" {
		in.OrderID = fmt.Sprintf("so-%s-%s", shortPub(b.PubHex()), randIDHex())
	}
	if in.MaxSats == 0 {
		in.MaxSats = 500
	}
	if in.TTL <= 0 && in.TTLSeconds <= 0 {
		in.TTLSeconds = 86400
	}
	invID, err := rc.StartStandingOrderAsync(ctx, in.OrderID, in)
	if err != nil {
		return nil, err
	}
	// Persist initial index entry immediately so fast SSE offers don't race the first restate.Run step.
	now := time.Now().Unix()
	ttlSec := in.TTLSeconds
	if in.TTL > 0 {
		ttlSec = int64(in.TTL.Seconds())
	}
	if ttlSec <= 0 {
		ttlSec = 86400
	}
	st := StandingOrderState{
		OrderID:           in.OrderID,
		InvocationID:      invID,
		WaitingSeq:        1,
		Status:            "watching",
		Domains:           in.Domains,
		Query:             strings.TrimSpace(in.Query),
		OntologyID:        strings.ToLower(strings.TrimSpace(in.OntologyID)),
		AllowBridge:       in.AllowBridge,
		MaxSats:           in.MaxSats,
		MinGrade:          string(nmu.NormalizeGrade(in.MinGrade)),
		MaxSatsPer1kUnits: in.MaxSatsPer1kUnits,
		FreshnessClass:    string(nmu.NormalizeFreshness(in.FreshnessClass)),
		RequiredRights:    string(nmu.NormalizeRights(in.RequiredRights)),
		PricingMode:       string(nmu.NormalizePricingMode(in.PricingMode)),
		TargetShard:       strings.TrimSpace(in.TargetShard),
		AgentID:           strings.TrimSpace(in.AgentID),
		ExpiresAtUnix:     now + ttlSec,
		CreatedAt:         now,
		UpdatedAt:         now,
	}
	b.engine.upsertStandingOrderIndex(st)
	b.notify("standing_order_created", map[string]any{"orderId": st.OrderID, "domains": st.Domains, "maxSats": st.MaxSats})
	return &st, nil
}

// ListStandingOrders returns all indexed StandingOrders, enriched with live Restate workflow state when available.
func (b *Broker) ListStandingOrders(ctx context.Context) []StandingOrderState {
	if b.mem == nil || b.mem.DB() == nil {
		return nil
	}
	entries := LoadStandingOrderIndex(b.mem.DB())
	rc := b.RestateClient()
	out := make([]StandingOrderState, 0, len(entries))
	for _, e := range entries {
		if rc != nil {
			qCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			st, err := rc.StandingOrderStatus(qCtx, e.OrderID)
			cancel()
			if err == nil && st != nil && st.OrderID != "" {
				out = append(out, *st)
				continue
			}
		}
		out = append(out, StandingOrderState{
			OrderID:           e.OrderID,
			Status:            e.Status,
			Domains:           e.Domains,
			Query:             e.Query,
			OntologyID:        e.OntologyID,
			AllowBridge:       e.AllowBridge,
			MaxSats:           e.MaxSats,
			MinGrade:          e.MinGrade,
			MaxSatsPer1kUnits: e.MaxSatsPer1kUnits,
			FreshnessClass:    e.FreshnessClass,
			RequiredRights:    e.RequiredRights,
			PricingMode:       e.PricingMode,
			TargetShard:       e.TargetShard,
			AgentID:           e.AgentID,
			ExpiresAtUnix:     e.ExpiresAtUnix,
			UpdatedAt:         e.UpdatedAt,
		})
	}
	return out
}

// SteerStandingOrder live-updates maxSats, pause state, or TTL on a running StandingOrder via restate.Signal.
func (b *Broker) SteerStandingOrder(ctx context.Context, orderID string, steer SteerStandingOrderInput) (bool, error) {
	rc := b.RestateClient()
	if rc == nil {
		return false, errors.New("market: Restate durable executor is unavailable")
	}
	ok, err := rc.SteerStandingOrder(ctx, orderID, steer)
	if err == nil && ok {
		b.notify("standing_order_steered", map[string]any{"orderId": orderID})
	}
	return ok, err
}

// CancelStandingOrder terminates a watching/paused StandingOrder cleanly via restate.Signal.
func (b *Broker) CancelStandingOrder(ctx context.Context, orderID, reason string) (bool, error) {
	rc := b.RestateClient()
	if rc == nil {
		return false, errors.New("market: Restate durable executor is unavailable")
	}
	ok, err := rc.CancelStandingOrder(ctx, orderID, reason)
	if err == nil && ok {
		b.notify("standing_order_cancelled", map[string]any{"orderId": orderID})
	}
	return ok, err
}

// PublishWantForStandingOrder signs and broadcasts a KindWant across active hubs so currently online
// or newly announced sellers quote OfferAds that wake the sleeping StandingOrder.
func (b *Broker) PublishWantForStandingOrder(ctx context.Context, orderID string, domains []string, query, ontologyID string, maxSats uint64) error {
	return b.PublishWantForStandingOrderNMU(ctx, StandingOrderState{
		OrderID:    orderID,
		Domains:    domains,
		Query:      query,
		OntologyID: ontologyID,
		MaxSats:    maxSats,
	})
}

// PublishWantForStandingOrderNMU broadcasts a StandingOrder WantAd carrying all NMU-1 commodity filters.
func (b *Broker) PublishWantForStandingOrderNMU(ctx context.Context, st StandingOrderState) error {
	if b == nil || b.disc == nil || b.mem == nil || b.mem.Chain() == nil || b.engine == nil || b.engine.saleKey == nil {
		return nil
	}
	hosts := b.disc.ActiveHosts()
	if len(hosts) == 0 {
		return nil
	}
	domains := st.Domains
	if len(domains) == 0 {
		domains = []string{"ops"}
	}
	q := strings.TrimSpace(st.Query)
	if q == "" {
		q = "standing order: " + strings.Join(domains, ",")
	}
	want := bstack.WantAd{
		WantID:            fmt.Sprintf("sow-%s-%s", shortPub(st.OrderID), randIDHex()[:8]),
		Pub:               b.PubHex(),
		Net:               b.mem.Chain().Network(),
		Domains:           domains,
		Query:             q,
		OntologyID:        strings.ToLower(strings.TrimSpace(st.OntologyID)),
		MaxSats:           st.MaxSats,
		MinGrade:          string(nmu.NormalizeGrade(st.MinGrade)),
		MaxSatsPer1kUnits: st.MaxSatsPer1kUnits,
		FreshnessClass:    string(nmu.NormalizeFreshness(st.FreshnessClass)),
		RequiredRights:    string(nmu.NormalizeRights(st.RequiredRights)),
		PricingMode:       string(nmu.NormalizePricingMode(st.PricingMode)),
		Revision:          1,
		Exp:               time.Now().Add(30 * time.Minute).UnixMilli(),
	}
	sig, err := bstack.SignArtifact(b.engine.saleKey, want)
	if err != nil {
		return err
	}
	body, err := json.Marshal(bstack.SignedWant{Want: want, Sig: sig})
	if err != nil {
		return err
	}
	topics := lanesForDomains(want.Net, want.Domains)
	_, err = b.disc.BuildAndSubmitMarketAd(ctx, b.engine.saleKey, bstack.MarketPayload{Kind: bstack.KindWant, Body: body}, hosts, topics)
	return err
}

func standingOrderNMUMatches(minGrade string, maxSatsPer1k uint64, freshness, reqRights, mode string, off bstack.OfferAd, nowUnix int64) bool {
	if nmu.IsExpired(nmu.FreshnessClass(off.FreshnessClass), off.ValidUntilUnix, nowUnix) {
		return false
	}
	if mg := nmu.NormalizeGrade(minGrade); mg != "" && !nmu.MeetsMinGrade(nmu.Grade(off.Grade), mg) {
		return false
	}
	if rr := nmu.NormalizeRights(reqRights); rr != "" && !nmu.MeetsRequiredRights(nmu.UsageRights(off.UsageRights), rr) {
		return false
	}
	if fc := nmu.NormalizeFreshness(freshness); fc != "" && nmu.NormalizeFreshness(off.FreshnessClass) != fc {
		return false
	}
	if pm := nmu.NormalizePricingMode(mode); pm != "" && nmu.EffectivePricingMode(nmu.PricingMode(off.PricingMode)) != pm {
		return false
	}
	if maxSatsPer1k > 0 && effectiveOfferUnitRate(off) > maxSatsPer1k {
		return false
	}
	return true
}
