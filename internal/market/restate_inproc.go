package market

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/store"
)

type inprocBuyerRun struct {
	mu      sync.Mutex
	state   DealState
	done    chan struct{}
	claimCh chan ClaimSignal
}

// AttachTestRestate attaches a lightweight in-process Restate Ingress HTTP server
// that executes the canonical BuyerDeal steps (policy eval, Awakeable approval gate,
// offer collection, deterministic + semantic ranking, Top-K failover, BRC-29/escrow
// payment, DeliverBuyerKey wakeup, and zero-trust verify+commit) over the real
// RestateClient HTTP surface. Intended for hermetic in-memory tests without requiring
// an external restate-server binary.
func AttachTestRestate(broker *Broker) func() {
	if broker == nil || broker.engine == nil {
		return func() {}
	}
	eng := broker.engine
	ctx, cancel := context.WithCancel(context.Background())

	var mu sync.Mutex
	runs := make(map[string]*inprocBuyerRun)
	awakeables := make(map[string]chan bool)

	getOrCreateAwakeable := func(id string) chan bool {
		mu.Lock()
		defer mu.Unlock()
		ch, ok := awakeables[id]
		if !ok {
			ch = make(chan bool, 1)
			awakeables[id] = ch
		}
		return ch
	}

	runBuyerDeal := func(dealID string, in BuyerDealInput, run *inprocBuyerRun) {
		defer close(run.done)
		want := in.Want
		if want.WantID == "" {
			want.WantID = dealID
		}
		setState := func(st DealState) {
			run.mu.Lock()
			run.state = st
			run.mu.Unlock()
		}

		eval, err := eng.buyerEvalPolicy(dealID, want, in.ApprovedOverrideID)
		if err != nil {
			setState(DealState{DealID: dealID, Role: "buyer", Status: "failed", Error: err.Error(), UpdatedAt: time.Now().Unix()})
			return
		}
		st := DealState{DealID: dealID, Role: "buyer", Status: "open", Want: want, UpdatedAt: eval.NowUnix}
		if eval.KillSwitch {
			st.Status = "failed"
			st.Error = "market: operator kill switch is engaged"
			setState(st)
			return
		}
		setState(st)

		bypassPolicy := eval.BypassPolicy
		if eval.NeedsApproval {
			awkID := "awk-" + dealID + "-" + randIDHex()
			awkCh := getOrCreateAwakeable(awkID)
			appID := "appr-" + randIDHex()
			wantJSON, _ := json.Marshal(want)
			if eng.mem != nil && eng.mem.DB() != nil {
				_ = eng.mem.DB().SaveApproval(store.ApprovalRecord{
					ID:           appID,
					DealID:       dealID,
					Reason:       eval.ApprovalReason,
					MaxExtraSats: want.MaxSats,
					Status:       "pending",
					AwakeableID:  awkID,
					ExpiresAt:    time.Now().Add(15 * time.Minute).Unix(),
				})
				eng.persistDealRecord(store.DealRecord{
					DealID:    dealID,
					Role:      "buyer",
					Status:    "approval_pending",
					WantJSON:  wantJSON,
					PriceSats: want.MaxSats,
					Error:     eval.ApprovalReason + " [approvalId=" + appID + "]",
				})
			}
			broker.notify("approval_pending", map[string]any{"approvalId": appID, "dealId": dealID, "reason": eval.ApprovalReason})
			st.Status = "approval_pending"
			st.Error = eval.ApprovalReason + " [approvalId=" + appID + "]"
			setState(st)

			waitDur := in.ApprovalTimeout
			if waitDur <= 0 {
				waitDur = 15 * time.Minute
			}
			var approved bool
			select {
			case approved = <-awkCh:
			case <-time.After(waitDur):
				approved = false
			case <-ctx.Done():
				return
			}
			if !approved {
				eng.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  "denied by operator",
				})
				st.Status = "failed"
				st.Error = "denied by operator"
				setState(st)
				return
			}
			bypassPolicy = true
		}

		collectRes, err := eng.buyerCollectOffers(ctx, dealID, want, in)
		if err != nil {
			st.Status = "failed"
			st.Error = err.Error()
			setState(st)
			return
		}
		if collectRes.NoOffers {
			st.Status = "expired"
			st.Error = collectRes.Error
			st.UpdatedAt = collectRes.NowUnix
			setState(st)
			return
		}
		if collectRes.PolicyRejected || len(collectRes.Offers) == 0 {
			st.Status = "failed"
			st.Error = collectRes.Error
			st.UpdatedAt = collectRes.NowUnix
			setState(st)
			return
		}

		rankedRes, err := eng.buyerRankAndMatch(ctx, dealID, want, collectRes, bypassPolicy)
		if err != nil {
			st.Status = "failed"
			st.Error = err.Error()
			setState(st)
			return
		}
		if rankedRes.NoOffers {
			st.Status = "expired"
			st.Error = rankedRes.Error
			st.UpdatedAt = rankedRes.NowUnix
			setState(st)
			return
		}
		if rankedRes.PolicyRejected || rankedRes.Chosen == nil {
			st.Status = "failed"
			st.MatchAudit = rankedRes.MatchAudit
			st.Error = rankedRes.Error
			st.UpdatedAt = rankedRes.NowUnix
			setState(st)
			return
		}

		candidates := rankedRes.Candidates
		if len(candidates) == 0 {
			candidates = []bstack.OfferAd{*rankedRes.Chosen}
		}

		for idx, cand := range candidates {
			hasNext := idx+1 < len(candidates)
			chosen := cand
			st.ChosenOffer = &chosen
			st.MatchAudit = rankedRes.MatchAudit
			st.ChirpURL = chosen.ChirpURL
			st.Status = "offered"
			st.Error = ""
			setState(st)

			if chosen.PriceSats > 0 && !bypassPolicy {
				if err := eng.policy.CanSpendForDomains(chosen.Pub, want.Domains, chosen.PriceSats); err != nil {
					if hasNext && !strings.Contains(strings.ToLower(err.Error()), "kill switch") {
						continue
					}
					eng.persistDealRecord(store.DealRecord{
						DealID: dealID,
						Role:   "buyer",
						Status: "failed",
						Error:  "custody reservation denied: " + err.Error(),
					})
					st.Status = "failed"
					st.Error = "custody reservation denied: " + err.Error()
					setState(st)
					return
				}
			}

			var immediateClaim *ClaimSignal
			if chosen.PayAddress != "" || chosen.PayIdentityKey != "" {
				payRes, pErr := eng.buyerPayAndDeliver(ctx, dealID, chosen)
				if pErr != nil {
					st.Status = "failed"
					st.Error = pErr.Error()
					setState(st)
					return
				}
				if payRes.PreBroadcastErr != "" {
					if hasNext {
						continue
					}
					eng.persistDealRecord(store.DealRecord{
						DealID: dealID,
						Role:   "buyer",
						Status: "failed",
						Error:  "payment failed: " + payRes.PreBroadcastErr,
					})
					st.Status = "failed"
					st.Error = "payment failed: " + payRes.PreBroadcastErr
					setState(st)
					return
				}
				if payRes.TxID != "" {
					eng.policy.RecordSpend(chosen.PriceSats)
					st.PaymentTxID = payRes.TxID
					st.Status = "paid_awaiting_reveal"
					st.UpdatedAt = payRes.NowUnix
					if payRes.EscrowTxID != "" {
						st.EscrowTxID = payRes.EscrowTxID
					}
					setState(st)
				}
				if payRes.DeliveryErr != "" && payRes.ImmediateClaim == nil {
					if eng.mem != nil && eng.mem.DB() != nil {
						_ = eng.mem.DB().RecordReputation(chosen.Pub, false, 0)
					}
					eng.persistDealRecord(store.DealRecord{
						DealID: dealID,
						Role:   "buyer",
						Status: "failed",
						Error:  "purchase delivery failed: " + payRes.DeliveryErr,
					})
					st.Status = "failed"
					st.Error = "purchase delivery failed: " + payRes.DeliveryErr
					setState(st)
					return
				}
				immediateClaim = payRes.ImmediateClaim
			}

			var sig ClaimSignal
			if immediateClaim != nil {
				sig = *immediateClaim
			} else {
				timeout := in.WaitTimeout
				if timeout <= 0 {
					timeout = 30 * time.Second
				}
				select {
				case sig = <-run.claimCh:
				case <-time.After(timeout):
					if st.PaymentTxID != "" && eng.mem != nil && eng.mem.DB() != nil {
						_ = eng.mem.DB().RecordReputation(chosen.Pub, false, 0)
					}
					eng.persistDealRecord(store.DealRecord{
						DealID: dealID,
						Role:   "buyer",
						Status: "expired",
						Error:  "timed out waiting for seller claim reveal",
					})
					st.Status = "expired"
					st.Error = "timed out waiting for seller claim reveal"
					setState(st)
					return
				case <-ctx.Done():
					return
				}
			}

			verRes, err := eng.buyerVerifyAndCommit(ctx, dealID, want, in.TargetShard, chosen, st.PaymentTxID, st.MatchAudit, sig)
			if err != nil {
				st.Status = "failed"
				st.Error = err.Error()
				setState(st)
				return
			}
			if verRes.FailedReason != "" {
				if st.PaymentTxID == "" && hasNext {
					continue
				}
				if eng.mem != nil && eng.mem.DB() != nil && chosen.Pub != "" {
					_ = eng.mem.DB().RecordReputation(chosen.Pub, false, 0)
				}
				eng.persistDealRecord(store.DealRecord{
					DealID: dealID,
					Role:   "buyer",
					Status: "failed",
					Error:  verRes.FailedReason,
				})
				st.Status = "failed"
				st.Error = verRes.FailedReason
				setState(st)
				return
			}
			st.ClaimTxID = sig.ClaimTxID
			st.Shard = verRes.TargetShard
			st.DeliveredTxt = verRes.DeliveredText
			st.DealReceipt = verRes.DealReceipt
			st.Status = "settled"
			st.Error = ""
			st.UpdatedAt = verRes.NowUnix
			setState(st)
			return
		}
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /BuyerDeal/{id}/run/send", func(w http.ResponseWriter, r *http.Request) {
		dealID := r.PathValue("id")
		var in BuyerDealInput
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		run, exists := runs[dealID]
		if !exists {
			run = &inprocBuyerRun{
				done:    make(chan struct{}),
				claimCh: make(chan ClaimSignal, 1),
			}
			runs[dealID] = run
			go runBuyerDeal(dealID, in, run)
		}
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"invocationId": "inv-" + dealID,
			"status":       "Accepted",
		})
	})
	mux.HandleFunc("GET /restate/workflow/BuyerDeal/{id}/attach", func(w http.ResponseWriter, r *http.Request) {
		dealID := r.PathValue("id")
		mu.Lock()
		run, ok := runs[dealID]
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown workflow "+dealID, http.StatusNotFound)
			return
		}
		select {
		case <-run.done:
		case <-r.Context().Done():
			return
		case <-ctx.Done():
			return
		}
		run.mu.Lock()
		st := run.state
		run.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /BuyerDeal/{id}/deliverKey", func(w http.ResponseWriter, r *http.Request) {
		dealID := r.PathValue("id")
		var sig ClaimSignal
		if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&sig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		run, ok := runs[dealID]
		mu.Unlock()
		if ok {
			select {
			case run.claimCh <- sig:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(true)
	})
	mux.HandleFunc("POST /BuyerDeal/{id}/status", func(w http.ResponseWriter, r *http.Request) {
		dealID := r.PathValue("id")
		mu.Lock()
		run, ok := runs[dealID]
		mu.Unlock()
		if !ok {
			http.Error(w, "unknown workflow "+dealID, http.StatusNotFound)
			return
		}
		run.mu.Lock()
		st := run.state
		run.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(st)
	})
	mux.HandleFunc("POST /restate/awakeables/{id}/resolve", func(w http.ResponseWriter, r *http.Request) {
		awkID := r.PathValue("id")
		raw, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil {
			http.Error(w, fmt.Sprintf("read awakeable payload: %v", err), http.StatusBadRequest)
			return
		}
		var approved bool
		var dec ApprovalDecision
		if json.Unmarshal(raw, &dec) == nil {
			approved = dec.Approved
		} else if err := json.Unmarshal(raw, &approved); err != nil {
			http.Error(w, fmt.Sprintf("decode awakeable payload: %v", err), http.StatusBadRequest)
			return
		}
		ch := getOrCreateAwakeable(awkID)
		select {
		case ch <- approved:
		default:
		}
		w.WriteHeader(http.StatusAccepted)
	})

	ts := httptest.NewServer(mux)
	rc := NewRestateClient(ts.URL, ts.URL)
	broker.SetRestateClient(rc)
	return func() {
		cancel()
		ts.CloseClientConnections()
		ts.Close()
	}
}
