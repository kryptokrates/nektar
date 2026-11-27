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
	"time"

	restate "github.com/restatedev/sdk-go"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/distill"
	"github.com/kryptokrates/nektar/internal/hub"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/store"
)

const (
	DefaultMicroEarnerFloatSats uint64 = 2000
	DefaultMicroEarnerMaxTasks         = 4
	MicroEarnerObjectKeyDefault        = "default"
	microEarnerStateKVKey              = "micro_earner_state_v1"
)

// MicroEarnerInput configures a zero-sat background earning pass.
type MicroEarnerInput struct {
	TargetFloatSats uint64   `json:"targetFloatSats,omitempty"`
	MaxTasks        int      `json:"maxTasks,omitempty"`
	HubURLs         []string `json:"hubUrls,omitempty"`
	AllowDesktop    bool     `json:"allowDesktop,omitempty"`
}

// MicroEarnerState tracks durable zero-sat float earning status.
type MicroEarnerState struct {
	Enabled           bool   `json:"enabled"`
	Status            string `json:"status"` // funded_idle | earning | disabled | error
	TargetFloatSats   uint64 `json:"targetFloatSats"`
	BalanceSats       uint64 `json:"balanceSats"`
	EarnedSatsTotal   uint64 `json:"earnedSatsTotal"`
	TasksCompleted    int64  `json:"tasksCompleted"`
	BountiesSubmitted int64  `json:"bountiesSubmitted"`
	LastRunAt         int64  `json:"lastRunAt"`
	LastError         string `json:"lastError,omitempty"`
}

// MicroEarnerToggleInput enables or disables background MicroEarner.
type MicroEarnerToggleInput struct {
	Enabled         bool   `json:"enabled"`
	TargetFloatSats uint64 `json:"targetFloatSats,omitempty"`
}

// RunMicroEarnerCycle runs one bounded 0-sat cold-start earning cycle across active Hubs:
//   - Track A: verifies & submits 0-sat Hub work bounties (header_witness_audit / spv_pack_audit)
//   - Track B: submits local PII-clean distilled facts to Hub-sponsored 0-sat knowledge bounties
//
// Immediately SPV-imports returned AtomicBEEF into the local wallet and parks at funded_idle once
// SpendableSats >= targetFloatSats.
func (e *Engine) RunMicroEarnerCycle(ctx context.Context, in MicroEarnerInput) (*MicroEarnerState, error) {
	now := time.Now().Unix()
	target := in.TargetFloatSats
	if target == 0 {
		target = DefaultMicroEarnerFloatSats
	}
	maxTasks := in.MaxTasks
	if maxTasks <= 0 {
		maxTasks = DefaultMicroEarnerMaxTasks
	}
	st := &MicroEarnerState{
		Enabled:         true,
		TargetFloatSats: target,
		LastRunAt:       now,
	}
	if e == nil || e.mem == nil || e.mem.Chain() == nil {
		st.Status = "error"
		st.LastError = "chain port unavailable"
		return st, errors.New(st.LastError)
	}
	if e.mem.DB() != nil {
		var saved MicroEarnerState
		if ok, _ := e.mem.DB().GetJSON(microEarnerStateKVKey, &saved); ok {
			st.EarnedSatsTotal = saved.EarnedSatsTotal
			st.TasksCompleted = saved.TasksCompleted
			st.BountiesSubmitted = saved.BountiesSubmitted
			if !saved.Enabled && saved.LastRunAt > 0 {
				st.Enabled = false
				st.Status = "disabled"
				st.BalanceSats = e.mem.Chain().BalanceSats()
				return st, nil
			}
		}
	}
	if !in.AllowDesktop && e.mem.Chain().WalletMode() == "client" {
		st.Status = "disabled"
		st.BalanceSats = e.mem.Chain().BalanceSats()
		return st, nil
	}

	bal := e.mem.Chain().BalanceSats()
	st.BalanceSats = bal
	if bal >= target {
		st.Status = "funded_idle"
		if e.mem.DB() != nil {
			_ = e.mem.DB().SetJSON(microEarnerStateKVKey, st)
		}
		return st, nil
	}

	hubs := in.HubURLs
	if len(hubs) == 0 {
		if br := e.getBroker(); br != nil && br.Discovery() != nil {
			hubs = br.Discovery().ActiveHosts()
		}
	}
	if len(hubs) == 0 {
		st.Status = "earning"
		st.LastError = "no active hub configured"
		if e.mem.DB() != nil {
			_ = e.mem.DB().SetJSON(microEarnerStateKVKey, st)
		}
		return st, nil
	}

	addr, err := e.mem.Chain().FundAddress()
	if err != nil || strings.TrimSpace(addr) == "" {
		st.Status = "error"
		st.LastError = fmt.Sprintf("fund address: %v", err)
		return st, err
	}
	netName := e.mem.Chain().Network()
	hc := core.SafeHTTPClientForNetwork(netName, 15*time.Second)
	workerPub := e.salePubHex()

	completed := 0
	for _, rawHub := range hubs {
		if bal >= target || completed >= maxTasks {
			break
		}
		hubURL := strings.TrimRight(strings.TrimSpace(rawHub), "/")
		if hubURL == "" {
			continue
		}
		if _, vErr := core.ValidateOutboundURL(hubURL, core.SafeHTTPOpts{
			AllowPrivate: netName == "memory" || netName == "regtest" || netName == "",
			RequireHTTPS: netName == "main",
		}); vErr != nil {
			continue
		}

		// Track A: Complete 0-sat verifiable Hub work bounties.
		tasks, tErr := fetchHubWorkTasks(ctx, hc, hubURL)
		if tErr == nil {
			for _, task := range tasks {
				if bal >= target || completed >= maxTasks {
					break
				}
				digest, count, dErr := hub.ComputeWorkAuditDigest(task.TaskID, task.Kind, task.ChallengeHex, task.Payload)
				if dErr != nil {
					continue
				}
				wSub := hub.WorkSubmission{
					TaskID:        task.TaskID,
					WorkerPub:     workerPub,
					WorkerAddress: addr,
					AuditDigest:   digest,
					VerifiedCount: count,
					TS:            time.Now().Unix(),
				}
				sig, sErr := bstack.SignArtifact(e.saleKey, wSub.Signable())
				if sErr != nil {
					continue
				}
				wSub.Sig = sig
				payout, pErr := submitHubWorkTask(ctx, hc, hubURL, wSub)
				if pErr != nil || payout == nil || !payout.Accepted {
					continue
				}
				if payout.BEEFHex != "" {
					if beefBytes, hErr := hex.DecodeString(payout.BEEFHex); hErr == nil && len(beefBytes) > 0 {
						_, _ = e.mem.Chain().ImportBEEF(ctx, beefBytes)
					}
				}
				completed++
				st.TasksCompleted++
				st.EarnedSatsTotal += payout.RewardSats
				if e.mem.DB() != nil {
					_ = e.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
						Kind:      "work_earn",
						SatsDelta: int64(payout.RewardSats),
						DealID:    task.TaskID,
						Provider:  hubURL,
						CreatedAt: time.Now().Unix(),
					})
				}
				bal = e.mem.Chain().BalanceSats()
				if bal < st.BalanceSats+payout.RewardSats {
					bal += payout.RewardSats
				}
			}
		}

		// Track B: If still below target float, submit local PII-clean distilled facts to sponsored 0-sat bounties.
		if bal < target && completed < maxTasks {
			bounties, bErr := fetchHubBounties(ctx, hc, hubURL)
			if bErr == nil {
				for _, b := range bounties {
					if !b.Sponsored || !b.Active || b.Filled >= b.MaxFills || bal >= target || completed >= maxTasks {
						continue
					}
					payout, sErr := e.submitSponsoredDistilledBounty(ctx, hc, hubURL, b, addr, workerPub)
					if sErr != nil || payout == nil || !payout.Accepted {
						continue
					}
					if payout.BEEFHex != "" {
						if beefBytes, hErr := hex.DecodeString(payout.BEEFHex); hErr == nil && len(beefBytes) > 0 {
							_, _ = e.mem.Chain().ImportBEEF(ctx, beefBytes)
						}
					}
					completed++
					st.BountiesSubmitted++
					st.EarnedSatsTotal += payout.RewardSats
					if e.mem.DB() != nil {
						_ = e.mem.DB().RecordEconomicsEvent(store.EconomicsEventRow{
							Kind:      "bounty_earn",
							SatsDelta: int64(payout.RewardSats),
							Shard:     payout.Shard,
							DealID:    b.ID,
							Provider:  hubURL,
							CreatedAt: time.Now().Unix(),
						})
					}
					bal = e.mem.Chain().BalanceSats()
					if bal < st.BalanceSats+payout.RewardSats {
						bal += payout.RewardSats
					}
				}
			}
		}
	}

	st.BalanceSats = e.mem.Chain().BalanceSats()
	if st.BalanceSats >= target {
		st.Status = "funded_idle"
	} else {
		st.Status = "earning"
	}
	if e.mem.DB() != nil {
		_ = e.mem.DB().SetJSON(microEarnerStateKVKey, st)
	}
	return st, nil
}

func (e *Engine) submitSponsoredDistilledBounty(
	ctx context.Context,
	hc *http.Client,
	hubURL string,
	b hub.Bounty,
	sellerAddr string,
	sellerPub string,
) (*hub.BountyPayoutResult, error) {
	if e.mem == nil || e.mem.DB() == nil {
		return nil, errors.New("memory unavailable")
	}
	candShards := []string{"market-" + strings.ToLower(strings.TrimSpace(b.Domain))}
	if all, err := e.mem.DB().ListShards(); err == nil {
		for _, sh := range all {
			if strings.HasPrefix(sh, "market-") && sh != candShards[0] {
				candShards = append(candShards, sh)
			}
		}
	}
	provPub, err := e.mem.Chain().SigningPubKey(ctx)
	if err != nil {
		return nil, err
	}
	provPubHex := hex.EncodeToString(provPub.Compressed())

	for _, sh := range candShards {
		frags, fErr := e.mem.DB().ListFragments(sh)
		if fErr != nil {
			continue
		}
		for i := len(frags) - 1; i >= 0; i-- {
			f := frags[i]
			if f.Forgotten || !f.Distilled || !f.Public || f.Level > 0 || f.BorgGroup != "" || len(f.Data) < 12 {
				continue
			}
			txt := string(f.Data)
			if sr := distill.Scrub(txt); sr.Verdict == distill.ScrubBlocked || sr.Verdict == distill.ScrubDroppedLowSignal {
				continue
			}
			hdp := strings.ToLower(strings.TrimSpace(f.HDP))
			sub := hub.BountySubmission{
				BountyID:       b.ID,
				SellerPub:      sellerPub,
				ProvenancePub:  provPubHex,
				SellerAddress:  sellerAddr,
				Shard:          f.Shard,
				Seq:            f.Seq,
				HDP:            hdp,
				MPERoot:        f.MPERoot,
				ContentSHA256:  f.ContentSHA256,
				CommitHash:     f.CommitHash,
				TransitionHash: f.TransitionHash,
				Text:           txt,
				ProvenanceHex:  hex.EncodeToString(f.Provenance),
				TS:             time.Now().Unix(),
			}
			signable := sub.Signable(hdp)
			sig, sErr := bstack.SignArtifact(e.saleKey, signable)
			if sErr != nil {
				continue
			}
			sub.Sig = sig
			canon, cErr := core.CanonicalJSON(signable)
			if cErr != nil {
				continue
			}
			oSig, oErr := e.mem.Chain().SignDigest(ctx, core.SHA256(canon))
			if oErr != nil {
				continue
			}
			sub.OwnerSig = hex.EncodeToString(oSig)

			body, _ := json.Marshal(sub)
			req, rErr := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/bounties/submit", bytes.NewReader(body))
			if rErr != nil {
				continue
			}
			req.Header.Set("Content-Type", "application/json")
			resp, dErr := hc.Do(req)
			if dErr != nil {
				continue
			}
			var out hub.BountyPayoutResult
			decErr := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out)
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK && decErr == nil && out.Accepted {
				return &out, nil
			}
		}
	}
	return nil, errors.New("no eligible distilled fragment for sponsored bounty")
}

func fetchHubWorkTasks(ctx context.Context, hc *http.Client, hubURL string) ([]hub.WorkTask, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/work-bounties", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("work-bounties HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Tasks []hub.WorkTask `json:"tasks"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Tasks, nil
}

func submitHubWorkTask(ctx context.Context, hc *http.Client, hubURL string, sub hub.WorkSubmission) (*hub.WorkPayoutResult, error) {
	body, _ := json.Marshal(sub)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, hubURL+"/work-bounties/submit", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("work-bounties submit HTTP %d", resp.StatusCode)
	}
	var out hub.WorkPayoutResult
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

func fetchHubBounties(ctx context.Context, hc *http.Client, hubURL string) ([]hub.Bounty, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, hubURL+"/bounties", nil)
	if err != nil {
		return nil, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("bounties HTTP %d", resp.StatusCode)
	}
	var payload struct {
		Bounties []hub.Bounty `json:"bounties"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 2<<20)).Decode(&payload); err != nil {
		return nil, err
	}
	return payload.Bounties, nil
}

// LoadMicroEarnerState returns the latest persisted MicroEarner state for dashboard & CLI inspection.
func LoadMicroEarnerState(mem *memory.Service) MicroEarnerState {
	st := MicroEarnerState{
		Enabled:         true,
		Status:          "earning",
		TargetFloatSats: DefaultMicroEarnerFloatSats,
	}
	if mem == nil {
		return st
	}
	if mem.DB() != nil {
		_, _ = mem.DB().GetJSON(microEarnerStateKVKey, &st)
		if st.TargetFloatSats == 0 {
			st.TargetFloatSats = DefaultMicroEarnerFloatSats
		}
	}
	if mem.Chain() != nil {
		st.BalanceSats = mem.Chain().BalanceSats()
		if !st.Enabled {
			st.Status = "disabled"
		} else if st.BalanceSats >= st.TargetFloatSats {
			st.Status = "funded_idle"
		} else if st.Status == "" {
			st.Status = "earning"
		}
	}
	return st
}

// SetMicroEarnerEnabled toggles autonomous zero-sat MicroEarner persistence.
func SetMicroEarnerEnabled(mem *memory.Service, enabled bool, targetFloatSats uint64) MicroEarnerState {
	st := LoadMicroEarnerState(mem)
	st.Enabled = enabled
	if targetFloatSats > 0 {
		st.TargetFloatSats = targetFloatSats
	}
	if !enabled {
		st.Status = "disabled"
	} else if st.BalanceSats >= st.TargetFloatSats {
		st.Status = "funded_idle"
	} else {
		st.Status = "earning"
	}
	st.LastRunAt = time.Now().Unix()
	if mem != nil && mem.DB() != nil {
		_ = mem.DB().SetJSON(microEarnerStateKVKey, st)
	}
	return st
}

// MicroEarner is registered WithIngressPrivate(true) and invoked only via the authenticated NektarRuntime gateway
// (Restate strips caller HTTP headers on internal service-to-service Object calls, matching AnchorCadence/Tx/EpistemicShard).
func (e *Engine) restateMicroEarnerRun(ctx restate.ObjectContext, in MicroEarnerInput) (MicroEarnerState, error) {
	prev, _ := restate.Get[MicroEarnerState](ctx, "state")
	if prev.LastRunAt > 0 && !prev.Enabled {
		prev.Status = "disabled"
		return prev, nil
	}
	res, err := restate.Run(ctx, func(rctx restate.RunContext) (MicroEarnerState, error) {
		out, rErr := e.RunMicroEarnerCycle(rctx, in)
		if out == nil {
			return MicroEarnerState{Status: "error"}, rErr
		}
		return *out, rErr
	}, restate.WithName("microearner_cycle"))
	if err != nil {
		prev.Status = "error"
		prev.LastError = err.Error()
		restate.Set(ctx, "state", prev)
		return prev, err
	}
	restate.Set(ctx, "state", res)
	return res, nil
}

func (e *Engine) restateMicroEarnerToggle(ctx restate.ObjectContext, in MicroEarnerToggleInput) (MicroEarnerState, error) {
	st, err := restate.Run(ctx, func(_ restate.RunContext) (MicroEarnerState, error) {
		return SetMicroEarnerEnabled(e.mem, in.Enabled, in.TargetFloatSats), nil
	}, restate.WithName("microearner_toggle"))
	if err != nil {
		return MicroEarnerState{}, err
	}
	restate.Set(ctx, "state", st)
	return st, nil
}

func (e *Engine) restateMicroEarnerStatus(ctx restate.ObjectSharedContext, _ restate.Void) (MicroEarnerState, error) {
	return restate.Get[MicroEarnerState](ctx, "state")
}
