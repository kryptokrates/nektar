package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/market"
	"github.com/kryptokrates/nektar/internal/reputation"
	"github.com/kryptokrates/nektar/internal/store"
)

func (a *App) registerAutonomousRoutes(mux *http.ServeMux) {
	mux.Handle("POST /v1/distill/run", a.withOperatorAuth(http.HandlerFunc(a.handleDistillRun)))
	mux.Handle("GET /v1/reputation", a.withAuth(http.HandlerFunc(a.handleReputationGet)))
	mux.Handle("POST /v1/reputation/rate", a.withOperatorAuth(http.HandlerFunc(a.handleReputationRate)))
	mux.Handle("POST /v1/reputation/outcome", a.withAuth(http.HandlerFunc(a.handleReputationOutcome)))
	mux.Handle("POST /v1/microearner/toggle", a.withOperatorAuth(http.HandlerFunc(a.handleMicroEarnerToggle)))
	mux.Handle("GET /v1/economics", a.withAuth(http.HandlerFunc(a.handleEconomicsGet)))
}

func (a *App) handleDistillRun(w http.ResponseWriter, r *http.Request) {
	var in market.SleepCycleInput
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in)
	}
	in.Force = true
	if rc := a.restateClient(); rc != nil {
		if st, err := rc.TriggerSleepCycle(r.Context(), in); err == nil && st != nil {
			a.BroadcastDashboardEvent("distill_complete")
			writeJSON(w, http.StatusOK, st)
			return
		}
	}
	st, err := a.mkt.RunSleepCycle(r.Context(), in)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("distill_complete")
	writeJSON(w, http.StatusOK, st)
}

func (a *App) handleReputationGet(w http.ResponseWriter, r *http.Request) {
	ontID := strings.TrimSpace(r.URL.Query().Get("ontologyId"))
	writeJSON(w, http.StatusOK, a.buildReputationPayload(ontID))
}

func (a *App) handleReputationRate(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DealID     string `json:"dealId"`
		SubjectPub string `json:"subjectPub"`
		OntologyID string `json:"ontologyId"`
		Role       string `json:"role"`
		AccBP      int64  `json:"accBp"`
		ConsBP     int64  `json:"consBp"`
		DispBP     int64  `json:"dispBp"`
		PredBP     int64  `json:"predBp"`
		CollabBP   int64  `json:"collabBp"`
		ProvBP     int64  `json:"provBp"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if strings.TrimSpace(in.DealID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "dealId is required"})
		return
	}
	dRec, err := a.mem.DB().GetDeal(strings.TrimSpace(in.DealID))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "deal not found: " + err.Error()})
		return
	}
	if len(dRec.DealReceiptJSON) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "deal has no verified bilateral deal receipt"})
		return
	}
	var dr market.DealReceipt
	if err := json.Unmarshal(dRec.DealReceiptJSON, &dr); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid deal receipt JSON"})
		return
	}
	role := strings.TrimSpace(in.Role)
	if role == "" {
		if dRec.Role == "seller" {
			role = reputation.RoleSellerRatesBuyer
		} else {
			role = reputation.RoleBuyerRatesSeller
		}
	}
	scores := reputation.Vector6D{
		AccBP:    reputation.ClampBP(in.AccBP),
		ConsBP:   reputation.ClampBP(in.ConsBP),
		DispBP:   reputation.ClampBP(in.DispBP),
		PredBP:   reputation.ClampBP(in.PredBP),
		CollabBP: reputation.ClampBP(in.CollabBP),
		ProvBP:   reputation.ClampBP(in.ProvBP),
	}
	if scores.AccBP == 0 && scores.DispBP == 0 && scores.CollabBP == 0 {
		scores = reputation.Vector6D{AccBP: 8500, ConsBP: 8500, DispBP: 10000, PredBP: 8500, CollabBP: 8500, ProvBP: 8500}
	}
	scores.CompositeBP = reputation.ComputeComposite6D(scores.AccBP, scores.ConsBP, scores.DispBP, scores.PredBP, scores.CollabBP, scores.ProvBP)
	tok, err := a.mkt.EmitPostDealAttestation(r.Context(), role, dr, in.OntologyID, scores, dr.Domains)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("reputation_updated")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"token":        tok,
		"dealId":       dr.DealID,
		"role":         role,
		"compositeBp":  scores.CompositeBP,
		"advisoryNote": reputation.AdvisoryReputationNote,
	})
}

func (a *App) handleReputationOutcome(w http.ResponseWriter, r *http.Request) {
	var in struct {
		DealID   string `json:"dealId"`
		ExitCode int    `json:"exitCode"`
		Note     string `json:"note"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	row, err := a.mkt.RecordTaskOutcomeAndReRate(r.Context(), in.DealID, in.ExitCode, in.Note)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("reputation_updated")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reputation": row})
}

func (a *App) handleMicroEarnerToggle(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Enabled         *bool  `json:"enabled,omitempty"`
		TargetFloatSats uint64 `json:"targetFloatSats,omitempty"`
		RunNow          bool   `json:"runNow,omitempty"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(io.LimitReader(r.Body, 32<<10)).Decode(&in)
	}
	var st market.MicroEarnerState
	if in.Enabled != nil {
		if rc := a.restateClient(); rc != nil {
			if rSt, err := rc.ToggleMicroEarner(r.Context(), market.MicroEarnerToggleInput{
				Enabled:         *in.Enabled,
				TargetFloatSats: in.TargetFloatSats,
			}); err == nil && rSt != nil {
				st = *rSt
			} else {
				st = market.SetMicroEarnerEnabled(a.mem, *in.Enabled, in.TargetFloatSats)
			}
		} else {
			st = market.SetMicroEarnerEnabled(a.mem, *in.Enabled, in.TargetFloatSats)
		}
	} else {
		st = market.LoadMicroEarnerState(a.mem)
	}
	if in.RunNow && st.Enabled {
		if rc := a.restateClient(); rc != nil {
			if rSt, err := rc.RunMicroEarner(r.Context(), market.MicroEarnerInput{
				TargetFloatSats: st.TargetFloatSats,
			}); err == nil && rSt != nil {
				st = *rSt
			}
		} else if a.mkt != nil {
			if lSt, err := a.mkt.RunMicroEarnerCycle(r.Context(), market.MicroEarnerInput{
				TargetFloatSats: st.TargetFloatSats,
			}); err == nil && lSt != nil {
				st = *lSt
			}
		}
	}
	a.BroadcastDashboardEvent("microearner_updated")
	writeJSON(w, http.StatusOK, st)
}

func (a *App) handleEconomicsGet(w http.ResponseWriter, _ *http.Request) {
	cutoff24h := time.Now().Add(-24 * time.Hour).Unix()
	today, _ := a.mem.DB().SummarizeEconomics(cutoff24h)
	allTime, _ := a.mem.DB().SummarizeEconomics(0)
	writeJSON(w, http.StatusOK, map[string]any{
		"today":   today,
		"allTime": allTime,
	})
}

func (a *App) buildReputationPayload(ontologyID string) map[string]any {
	selfPub := ""
	if a.broker != nil {
		selfPub = a.broker.PubHex()
	} else if a.mem != nil && a.mem.Chain() != nil {
		selfPub = a.mem.Chain().IdentityPubHex()
	}
	ont := reputation.NormalizeOntologyID(ontologyID)
	eval, _, _ := market.EvaluateTrustForOntology(a.mem.DB(), selfPub, ont, a.mkt.Policy(), nil, nil)
	rows, _ := a.mem.DB().ListReputation6D(ontologyID, 100)
	if rows == nil {
		rows = []store.Reputation6DRow{}
	}
	atts, _ := a.mem.DB().ListReputationAttestations(ontologyID, 50)
	if atts == nil {
		atts = []store.ReputationAttestationRow{}
	}
	outcomes, _ := a.mem.DB().ListDealOutcomes(25)
	if outcomes == nil {
		outcomes = []store.DealOutcomeRow{}
	}

	// Collect available ontology IDs present in reputation rows + committed domain ontologies.
	ontSeen := map[string]bool{reputation.GlobalOntologyID: true}
	ontologies := []string{reputation.GlobalOntologyID}
	for _, r := range rows {
		if r.OntologyID != "" && !ontSeen[r.OntologyID] {
			ontSeen[r.OntologyID] = true
			ontologies = append(ontologies, r.OntologyID)
		}
	}

	var trusted, priority, untrusted []string
	if eval != nil {
		for p, ok := range eval.TrustedSellers {
			if ok {
				trusted = append(trusted, p)
			}
		}
		for p, ok := range eval.PriorityBuyers {
			if ok {
				priority = append(priority, p)
			}
		}
		for p, ok := range eval.SybilOrUntrusted {
			if ok {
				untrusted = append(untrusted, p)
			}
		}
	}
	return map[string]any{
		"ontologyId":       ont,
		"ontologies":       ontologies,
		"vectors":          rows,
		"attestations":     atts,
		"outcomes":         outcomes,
		"trustedSellers":   trusted,
		"priorityBuyers":   priority,
		"sybilOrUntrusted": untrusted,
		"advisoryNote":     reputation.AdvisoryReputationNote,
	}
}

type dashboardAutonomousSections struct {
	economics   map[string]any
	reputation  map[string]any
	distill     map[string]any
	microEarner market.MicroEarnerState
}

func (a *App) buildDashboardAutonomous(_ context.Context, cutoff24h int64) dashboardAutonomousSections {
	todayEcon, _ := a.mem.DB().SummarizeEconomics(cutoff24h)
	allEcon, _ := a.mem.DB().SummarizeEconomics(0)

	quarantinedCount, _ := a.mem.DB().CountDistillQuarantine()
	quarantinedList, _ := a.mem.DB().ListDistillQuarantine(10)
	if quarantinedList == nil {
		quarantinedList = []store.DistillQuarantineRecord{}
	}
	distilledCount, _ := a.mem.DB().CountDistilledFragments()
	var lastSleep market.SleepCycleState
	_, _ = a.mem.DB().GetJSON("sleep_cycle_last_state_v1", &lastSleep)

	var marketShards []string
	if allShards, err := a.mem.DB().ListShards(); err == nil {
		for _, sh := range allShards {
			if strings.HasPrefix(sh, "market-") {
				marketShards = append(marketShards, sh)
			}
		}
	}
	if marketShards == nil {
		marketShards = []string{}
	}

	return dashboardAutonomousSections{
		economics: map[string]any{
			"today":   todayEcon,
			"allTime": allEcon,
		},
		reputation: a.buildReputationPayload(""),
		distill: map[string]any{
			"distilledCount":   distilledCount,
			"quarantinedCount": quarantinedCount,
			"quarantined":      quarantinedList,
			"marketShards":     marketShards,
			"idleSeconds":      a.mem.IdleSeconds(),
			"lastCycle":        lastSleep,
		},
		microEarner: market.LoadMicroEarnerState(a.mem),
	}
}
