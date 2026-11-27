package hub

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memory"
	nwallet "github.com/kryptokrates/nektar/internal/wallet"
)

const (
	// WorkKindHeaderWitnessAudit rewards a 0-sat node for auditing a batch of PoW/header witnesses.
	WorkKindHeaderWitnessAudit = "header_witness_audit"
	// WorkKindSPVPackAudit rewards a 0-sat node for auditing an anchored CHIRP/SPV pack digest.
	WorkKindSPVPackAudit = "spv_pack_audit"

	DefaultWorkPerPubDailyCapSats uint64 = 500
	DefaultWorkGlobalDailyCapSats uint64 = 20_000
	DefaultHeaderAuditRewardSats  uint64 = 35
	DefaultSPVPackAuditRewardSats uint64 = 60
)

var (
	ErrWorkAlreadyClaimed = errors.New("hub: work bounty task already claimed")
	ErrWorkCapExceeded    = errors.New("hub: work bounty 24h cap exceeded")
)

// WorkTask describes a zero-sat verifiable audit task offered by the Hub so fresh nodes can earn
// their initial BSV float without manual funding.
type WorkTask struct {
	TaskID         string          `json:"taskId"`
	Kind           string          `json:"kind"` // header_witness_audit | spv_pack_audit
	RewardSats     uint64          `json:"rewardSats"`
	ChallengeHex   string          `json:"challengeHex"`
	Payload        json.RawMessage `json:"payload"`
	ExpectedDigest string          `json:"-"` // never leaked on wire
	ExpiresAt      int64           `json:"expiresAt"`
	Claimed        bool            `json:"claimed,omitempty"`
}

// WorkSubmission is a signed agent proof completing one WorkTask for instant AtomicBEEF payout.
type WorkSubmission struct {
	TaskID        string `json:"taskId"`
	WorkerPub     string `json:"workerPub"`
	WorkerAddress string `json:"workerAddress"`
	AuditDigest   string `json:"auditDigest"`
	VerifiedCount int    `json:"verifiedCount"`
	TS            int64  `json:"ts"`
	Sig           string `json:"sig"`
}

// Signable returns the canonical signable fields for WorkSubmission.
func (w WorkSubmission) Signable() map[string]any {
	return map[string]any{
		"action":        "work_bounty_submit",
		"taskId":        w.TaskID,
		"workerPub":     w.WorkerPub,
		"workerAddress": w.WorkerAddress,
		"auditDigest":   strings.ToLower(strings.TrimSpace(w.AuditDigest)),
		"verifiedCount": w.VerifiedCount,
		"ts":            w.TS,
	}
}

// WorkPayoutResult reports the Hub's verified AtomicBEEF payout for a completed work bounty.
type WorkPayoutResult struct {
	Accepted   bool   `json:"accepted"`
	TaskID     string `json:"taskId"`
	Kind       string `json:"kind"`
	RewardSats uint64 `json:"rewardSats"`
	TxID       string `json:"txid,omitempty"`
	BEEFHex    string `json:"beefHex,omitempty"`
	Status     string `json:"status,omitempty"`
	Reason     string `json:"reason,omitempty"`
}

// WorkHandler executes SpendGuard-enforced AtomicBEEF payout for a verified work task.
type WorkHandler func(ctx context.Context, task WorkTask, sub WorkSubmission) (*WorkPayoutResult, error)

// WorkAuditPayload is the canonical verifiable audit batch carried inside WorkTask.Payload.
type WorkAuditPayload struct {
	Network string   `json:"network"`
	Height  int64    `json:"height"`
	Items   []string `json:"items"` // header hashes or pack merkle roots to verify & bind
}

// ComputeWorkAuditDigest deterministically parses and audits payload JSON and computes the canonical
// domain-separated work proof digest: hex(DSHA256(TagWorkBountyV1 || taskId || kind || challengeHex || itemsHash)).
// Both the Hub (task generator) and MicroEarner (0-sat worker) execute this exact verifier.
func ComputeWorkAuditDigest(taskID, kind, challengeHex string, payload []byte) (string, int, error) {
	var wp WorkAuditPayload
	if err := json.Unmarshal(payload, &wp); err != nil {
		return "", 0, fmt.Errorf("work-audit: invalid payload JSON: %w", err)
	}
	if len(wp.Items) == 0 {
		return "", 0, errors.New("work-audit: payload has zero audit items")
	}
	var itemConcat []byte
	for i, it := range wp.Items {
		s := strings.ToLower(strings.TrimSpace(it))
		if len(s) != 64 {
			return "", 0, fmt.Errorf("work-audit: item %d is not 64-char hex", i)
		}
		raw, err := hex.DecodeString(s)
		if err != nil {
			return "", 0, fmt.Errorf("work-audit: item %d hex: %w", i, err)
		}
		itemConcat = append(itemConcat, raw...)
	}
	itemsRoot := core.DSHA256(itemConcat)
	digest := core.DSHA256(core.Concat(
		core.TagWorkBountyV1,
		core.Field([]byte(strings.TrimSpace(taskID))),
		core.Field([]byte(strings.TrimSpace(kind))),
		core.Field([]byte(strings.ToLower(strings.TrimSpace(challengeHex)))),
		core.U64LE(uint64(wp.Height)),
		itemsRoot,
	))
	return hex.EncodeToString(digest), len(wp.Items), nil
}

// SetWorkHandler attaches the Hub's SpendGuard-enforced payout handler for 0-sat work bounties.
func (n *Node) SetWorkHandler(h WorkHandler) {
	n.mu.Lock()
	n.workHandler = h
	n.mu.Unlock()
}

// SetWorkCaps overrides per-pubkey and global 24h caps for work bounties.
func (n *Node) SetWorkCaps(perPubDailySats, globalDailySats uint64) {
	n.mu.Lock()
	if perPubDailySats > 0 {
		n.workPerPubCapSats = perPubDailySats
	}
	if globalDailySats > 0 {
		n.workGlobalCapSats = globalDailySats
	}
	n.mu.Unlock()
}

func (n *Node) workCaps() (perPub, global uint64) {
	n.mu.RLock()
	perPub = n.workPerPubCapSats
	global = n.workGlobalCapSats
	n.mu.RUnlock()
	if perPub == 0 {
		perPub = DefaultWorkPerPubDailyCapSats
	}
	if global == 0 {
		global = DefaultWorkGlobalDailyCapSats
	}
	return perPub, global
}

// ensureWorkTaskPool keeps a small bounded pool of live, unexpired work bounties available.
func (n *Node) ensureWorkTaskPool() []WorkTask {
	now := time.Now().Unix()
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.workTasks == nil {
		n.workTasks = make(map[string]WorkTask)
	}
	for id, t := range n.workTasks {
		if t.Claimed || t.ExpiresAt <= now {
			delete(n.workTasks, id)
		}
	}
	// Seed up to 4 open tasks (2 header_witness_audit + 2 spv_pack_audit) when Hub work handler is armed.
	if n.workHandler != nil {
		kinds := []struct {
			kind   string
			reward uint64
		}{
			{WorkKindHeaderWitnessAudit, DefaultHeaderAuditRewardSats},
			{WorkKindSPVPackAudit, DefaultSPVPackAuditRewardSats},
		}
		for len(n.workTasks) < 4 {
			spec := kinds[len(n.workTasks)%len(kinds)]
			var rnd [16]byte
			_, _ = crand.Read(rnd[:])
			taskID := fmt.Sprintf("wrk-%s-%s", spec.kind[:4], hex.EncodeToString(rnd[:6]))
			chal := hex.EncodeToString(core.SHA256(append([]byte(taskID), rnd[:]...)))
			items := make([]string, 3)
			for i := range items {
				items[i] = hex.EncodeToString(core.DSHA256(core.Concat([]byte(n.net), []byte(taskID), core.U64LE(uint64(i+1)), rnd[:])))
			}
			plBytes, _ := json.Marshal(WorkAuditPayload{
				Network: n.net,
				Height:  now / 600,
				Items:   items,
			})
			expDigest, _, _ := ComputeWorkAuditDigest(taskID, spec.kind, chal, plBytes)
			n.workTasks[taskID] = WorkTask{
				TaskID:         taskID,
				Kind:           spec.kind,
				RewardSats:     spec.reward,
				ChallengeHex:   chal,
				Payload:        plBytes,
				ExpectedDigest: expDigest,
				ExpiresAt:      now + 1800,
			}
		}
	}
	out := make([]WorkTask, 0, len(n.workTasks))
	for _, t := range n.workTasks {
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TaskID < out[j].TaskID })
	return out
}

func (n *Node) handleWorkBountiesList(w http.ResponseWriter, _ *http.Request) {
	tasks := n.ensureWorkTaskPool()
	writeJSON(w, http.StatusOK, map[string]any{"tasks": tasks})
}

func (n *Node) handleWorkBountiesSubmit(w http.ResponseWriter, r *http.Request) {
	var sub WorkSubmission
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&sub); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	sub.TaskID = strings.TrimSpace(sub.TaskID)
	sub.WorkerPub = strings.ToLower(strings.TrimSpace(sub.WorkerPub))
	sub.WorkerAddress = strings.TrimSpace(sub.WorkerAddress)
	sub.AuditDigest = strings.ToLower(strings.TrimSpace(sub.AuditDigest))
	if sub.TaskID == "" || sub.WorkerPub == "" || sub.WorkerAddress == "" || sub.AuditDigest == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "taskId, workerPub, workerAddress, and auditDigest are required"})
		return
	}
	now := time.Now().Unix()
	if sub.TS < now-bboxSkewSeconds || sub.TS > now+bboxSkewSeconds || sub.Sig == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "fresh signed work submission (ts + sig) required"})
		return
	}
	if err := bstack.VerifyArtifact(sub.WorkerPub, sub.Sig, sub.Signable()); err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid work submission signature"})
		return
	}
	if !n.allowPubRate(sub.WorkerPub) {
		writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "rate limit exceeded"})
		return
	}

	n.mu.RLock()
	task, exists := n.workTasks[sub.TaskID]
	wh := n.workHandler
	n.mu.RUnlock()
	if !exists || task.Claimed || task.ExpiresAt <= now {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "work bounty task not found, claimed, or expired"})
		return
	}
	if wh == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "hub work bounty payer is not armed"})
		return
	}
	if !strings.EqualFold(sub.AuditDigest, task.ExpectedDigest) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid work audit digest"})
		return
	}

	perPubCap, globalCap := n.workCaps()
	if err := n.store.ReserveWorkClaim(task.TaskID, task.Kind, sub.WorkerPub, task.RewardSats, perPubCap, globalCap); err != nil {
		if errors.Is(err, ErrWorkAlreadyClaimed) {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		if errors.Is(err, ErrWorkCapExceeded) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	res, err := wh(r.Context(), task, sub)
	if err != nil {
		if !errors.Is(err, nwallet.ErrBroadcastUnknown) {
			_ = n.store.CancelWorkClaim(task.TaskID)
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if res == nil || !res.Accepted {
		_ = n.store.CancelWorkClaim(task.TaskID)
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "work bounty payout declined"})
		return
	}
	_ = n.store.FinalizeWorkClaim(task.TaskID, res.TxID)
	n.mu.Lock()
	if t, ok := n.workTasks[task.TaskID]; ok {
		t.Claimed = true
		n.workTasks[task.TaskID] = t
	}
	n.mu.Unlock()
	res.Status = "paid"
	writeJSON(w, http.StatusOK, res)
}

// ReserveWorkClaim atomically enforces task uniqueness, per-pubkey 24h cap, and global 24h cap inside SQLite.
func (s *Store) ReserveWorkClaim(taskID, kind, workerPub string, rewardSats, perPubDailyCap, globalDailyCap uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.sql.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	cutoff := time.Now().Add(-24 * time.Hour).Unix()

	var pubSpent int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(reward_sats), 0) FROM hub_work_claims WHERE worker_pub = ? AND created_at >= ?`, workerPub, cutoff).Scan(&pubSpent); err != nil {
		return err
	}
	if perPubDailyCap > 0 && uint64(pubSpent)+rewardSats > perPubDailyCap {
		return fmt.Errorf("%w: pubkey 24h cap %d sats reached", ErrWorkCapExceeded, perPubDailyCap)
	}
	var globalSpent int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(reward_sats), 0) FROM hub_work_claims WHERE created_at >= ?`, cutoff).Scan(&globalSpent); err != nil {
		return err
	}
	if globalDailyCap > 0 && uint64(globalSpent)+rewardSats > globalDailyCap {
		return fmt.Errorf("%w: global 24h cap %d sats reached", ErrWorkCapExceeded, globalDailyCap)
	}
	if _, err := tx.Exec(`
		INSERT INTO hub_work_claims (task_id, kind, worker_pub, txid, reward_sats, status, created_at)
		VALUES (?, ?, ?, '', ?, 'pending', ?)`,
		taskID, kind, workerPub, rewardSats, time.Now().Unix()); err != nil {
		if isSQLiteConstraintErr(err) {
			return ErrWorkAlreadyClaimed
		}
		return err
	}
	return tx.Commit()
}

// FinalizeWorkClaim marks a work claim paid with its broadcast txid.
func (s *Store) FinalizeWorkClaim(taskID, txid string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`UPDATE hub_work_claims SET txid = ?, status = 'paid' WHERE task_id = ?`, txid, taskID)
	return err
}

// CancelWorkClaim removes an unbroadcast pending work claim.
func (s *Store) CancelWorkClaim(taskID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.sql.Exec(`DELETE FROM hub_work_claims WHERE task_id = ? AND status = 'pending'`, taskID)
	return err
}

func makeDefaultWorkHandler(mem *memory.Service) WorkHandler {
	return func(_ context.Context, task WorkTask, sub WorkSubmission) (*WorkPayoutResult, error) {
		payCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		sendRes, err := mem.Chain().Send(payCtx, nwallet.SendArgs{
			ToAddress:   sub.WorkerAddress,
			Satoshis:    task.RewardSats,
			Description: nwallet.ActionPayHubBounty("work:" + task.TaskID).Description,
		})
		if err != nil {
			return nil, fmt.Errorf("work-bounty: payout failed: %w", err)
		}
		return &WorkPayoutResult{
			Accepted:   true,
			TaskID:     task.TaskID,
			Kind:       task.Kind,
			RewardSats: task.RewardSats,
			TxID:       sendRes.TxID,
			BEEFHex:    sendRes.BEEFHex,
			Status:     "paid",
		}, nil
	}
}
