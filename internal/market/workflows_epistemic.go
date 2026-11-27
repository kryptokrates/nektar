package market

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	restate "github.com/restatedev/sdk-go"
	"github.com/restatedev/sdk-go/ingress"

	"github.com/kryptokrates/nektar/internal/memory"
)

// EpistemicShardState tracks the durable Merkle Automaton head state of a single shard inside
// the Restate EpistemicShard Virtual Object.
type EpistemicShardState struct {
	Shard          string `json:"shard"`
	Seq            int64  `json:"seq"`
	StateQ         string `json:"stateQ,omitempty"`
	CommitHash     string `json:"commitHash,omitempty"`
	TransitionHash string `json:"transitionHash,omitempty"`
	AnchorN        int64  `json:"anchorN"`
	AnchorTxID     string `json:"anchorTxid,omitempty"`
	MmrRoot        string `json:"mmrRoot,omitempty"`
	UpdatedAt      int64  `json:"updatedAt"`
}

// EpistemicTransitionInput drives a serialized Merkle Automaton state transition on a shard.
type EpistemicTransitionInput struct {
	Remember memory.RememberInput `json:"remember"`
	Anchor   bool                 `json:"anchor,omitempty"`
}

// EpistemicTransitionResult summarizes a completed durable EpistemicShard transition.
type EpistemicTransitionResult struct {
	Remember   *memory.RememberResult `json:"remember"`
	AnchorTxID string                 `json:"anchorTxid,omitempty"`
	AnchorN    int64                  `json:"anchorN"`
	State      EpistemicShardState    `json:"state"`
}

// RuntimeEpistemicTransitionInput routes a shard transition through the authenticated NektarRuntime gateway.
type RuntimeEpistemicTransitionInput struct {
	Shard string                   `json:"shard"`
	Input EpistemicTransitionInput `json:"input"`
}

// restateEpistemicTransition executes Remember + optional AnchorShard exclusively per shard key,
// guaranteeing contiguous NektarAnchor.Append() UTXO chains with zero UTXO double-spend contention.
func (e *Engine) restateEpistemicTransition(ctx restate.ObjectContext, in EpistemicTransitionInput) (EpistemicTransitionResult, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[EpistemicShardState](ctx, "state")
	if err != nil {
		return EpistemicTransitionResult{}, err
	}
	if st.Shard == "" {
		st.Shard = shard
		st.AnchorN = -1
	}

	wantAnchor := in.Anchor || in.Remember.AutoAnchor
	remIn := in.Remember
	remIn.Shard = shard
	remIn.AutoAnchor = false

	remRes, err := restate.Run(ctx, func(rctx restate.RunContext) (*memory.RememberResult, error) {
		return e.mem.Remember(rctx, remIn)
	}, restate.WithName("epistemic_remember"))
	if err != nil {
		return EpistemicTransitionResult{}, err
	}
	if remRes != nil {
		st.Seq = remRes.Seq
		if remRes.StateQ != "" {
			st.StateQ = remRes.StateQ
		}
		if remRes.CommitHash != "" {
			st.CommitHash = remRes.CommitHash
		}
		if remRes.TransitionHash != "" {
			st.TransitionHash = remRes.TransitionHash
		}
	}

	var anchorTxID string
	anchorN := st.AnchorN
	if wantAnchor {
		type ancOut struct {
			TxID      string `json:"txid"`
			N         int64  `json:"n"`
			StateHash string `json:"stateHash"`
			MmrRoot   string `json:"mmrRoot"`
			NowUnix   int64  `json:"nowUnix"`
		}
		aRes, aErr := restate.Run(ctx, func(rctx restate.RunContext) (ancOut, error) {
			b, err := e.mem.AnchorShard(rctx, shard)
			if err != nil {
				return ancOut{}, err
			}
			if b == nil {
				return ancOut{N: -1, NowUnix: time.Now().Unix()}, nil
			}
			return ancOut{
				TxID:      b.TxID,
				N:         b.N,
				StateHash: b.StateHash,
				MmrRoot:   b.MmrRoot,
				NowUnix:   time.Now().Unix(),
			}, nil
		}, restate.WithName("epistemic_anchor"))
		if aErr != nil {
			return EpistemicTransitionResult{}, aErr
		}
		if aRes.TxID != "" {
			anchorTxID = aRes.TxID
			anchorN = aRes.N
			st.AnchorN = aRes.N
			st.AnchorTxID = aRes.TxID
			if aRes.StateHash != "" {
				st.StateQ = aRes.StateHash
			}
			if aRes.MmrRoot != "" {
				st.MmrRoot = aRes.MmrRoot
			}
			st.UpdatedAt = aRes.NowUnix
			restate.ObjectSend(ctx, "Tx", aRes.TxID, "emit").Send(
				TxEventInput{Kind: "broadcast", Shard: shard, ObservedAt: aRes.NowUnix},
				restate.WithIdempotencyKey("tx:"+aRes.TxID+":broadcast:0"),
			)
		}
	}

	if st.UpdatedAt == 0 {
		now, _ := restate.Run(ctx, func(_ restate.RunContext) (int64, error) {
			return time.Now().Unix(), nil
		}, restate.WithName("epistemic_ts"))
		st.UpdatedAt = now
	}
	restate.Set(ctx, "state", st)

	return EpistemicTransitionResult{
		Remember:   remRes,
		AnchorTxID: anchorTxID,
		AnchorN:    anchorN,
		State:      st,
	}, nil
}

// restateEpistemicAnchor exclusively anchors all unanchored transitions on shard.
func (e *Engine) restateEpistemicAnchor(ctx restate.ObjectContext, _ restate.Void) (EpistemicShardState, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[EpistemicShardState](ctx, "state")
	if err != nil {
		return EpistemicShardState{}, err
	}
	if st.Shard == "" {
		st.Shard = shard
		st.AnchorN = -1
	}
	type ancOut struct {
		TxID      string `json:"txid"`
		N         int64  `json:"n"`
		StateHash string `json:"stateHash"`
		MmrRoot   string `json:"mmrRoot"`
		NowUnix   int64  `json:"nowUnix"`
	}
	aRes, err := restate.Run(ctx, func(rctx restate.RunContext) (ancOut, error) {
		b, err := e.mem.AnchorShard(rctx, shard)
		if err != nil {
			return ancOut{}, err
		}
		if b == nil {
			return ancOut{N: -1, NowUnix: time.Now().Unix()}, nil
		}
		return ancOut{
			TxID:      b.TxID,
			N:         b.N,
			StateHash: b.StateHash,
			MmrRoot:   b.MmrRoot,
			NowUnix:   time.Now().Unix(),
		}, nil
	}, restate.WithName("epistemic_anchor_only"))
	if err != nil {
		return st, err
	}
	if aRes.TxID != "" {
		st.AnchorN = aRes.N
		st.AnchorTxID = aRes.TxID
		if aRes.StateHash != "" {
			st.StateQ = aRes.StateHash
		}
		if aRes.MmrRoot != "" {
			st.MmrRoot = aRes.MmrRoot
		}
		st.UpdatedAt = aRes.NowUnix
		restate.ObjectSend(ctx, "Tx", aRes.TxID, "emit").Send(
			TxEventInput{Kind: "broadcast", Shard: shard, ObservedAt: aRes.NowUnix},
			restate.WithIdempotencyKey("tx:"+aRes.TxID+":broadcast:0"),
		)
		restate.Set(ctx, "state", st)
	}
	return st, nil
}

// restateEpistemicState returns the current automaton head state of shard (hydrating from SQLite if empty).
func (e *Engine) restateEpistemicState(ctx restate.ObjectSharedContext, _ restate.Void) (EpistemicShardState, error) {
	shard := restate.Key(ctx)
	st, err := restate.Get[EpistemicShardState](ctx, "state")
	if err == nil && st.Shard != "" {
		return st, nil
	}
	return restate.Run(ctx, func(_ restate.RunContext) (EpistemicShardState, error) {
		return e.loadEpistemicStateFromDB(shard), nil
	}, restate.WithName("epistemic_state_hydrate"))
}

func (e *Engine) loadEpistemicStateFromDB(shard string) EpistemicShardState {
	st := EpistemicShardState{Shard: shard, AnchorN: -1}
	if e == nil || e.mem == nil || e.mem.DB() == nil {
		return st
	}
	if tr, err := e.mem.DB().GetLatestTransition(shard); err == nil {
		st.Seq = tr.Seq
		st.StateQ = tr.NextQ
		st.CommitHash = tr.CommitHash
		st.TransitionHash = tr.Hash
		st.UpdatedAt = tr.CreatedAt
	}
	if anchors, err := e.mem.DB().ListAnchors(shard); err == nil && len(anchors) > 0 {
		last := anchors[len(anchors)-1]
		st.AnchorN = last.N
		st.AnchorTxID = last.TxID
		if last.StateHash != "" {
			st.StateQ = last.StateHash
		}
		st.MmrRoot = last.MmrRoot
		if last.CreatedAt > st.UpdatedAt {
			st.UpdatedAt = last.CreatedAt
		}
	}
	return st
}

// --- NektarRuntime authenticated ingress gateway methods for EpistemicShard ---

func (e *Engine) restateRuntimeEpistemicTransition(ctx restate.Context, in RuntimeEpistemicTransitionInput) (EpistemicTransitionResult, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return EpistemicTransitionResult{}, err
	}
	sh := strings.TrimSpace(in.Shard)
	if sh == "" {
		sh = strings.TrimSpace(in.Input.Remember.Shard)
	}
	if sh == "" {
		return EpistemicTransitionResult{}, restate.TerminalErrorf("shard is required")
	}
	return restate.Object[EpistemicTransitionResult](ctx, "EpistemicShard", sh, "transition").Request(in.Input)
}

func (e *Engine) restateRuntimeEpistemicAnchor(ctx restate.Context, shard string) (EpistemicShardState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return EpistemicShardState{}, err
	}
	sh := strings.TrimSpace(shard)
	if sh == "" {
		return EpistemicShardState{}, restate.TerminalErrorf("shard is required")
	}
	return restate.Object[EpistemicShardState](ctx, "EpistemicShard", sh, "anchor").Request(restate.Void{})
}

func (e *Engine) restateRuntimeEpistemicState(ctx restate.Context, shard string) (EpistemicShardState, error) {
	if err := e.checkInternalAuth(ctx); err != nil {
		return EpistemicShardState{}, err
	}
	sh := strings.TrimSpace(shard)
	if sh == "" {
		return EpistemicShardState{}, restate.TerminalErrorf("shard is required")
	}
	return restate.Object[EpistemicShardState](ctx, "EpistemicShard", sh, "state").Request(restate.Void{})
}

// --- RestateClient helpers for EpistemicShard ---

// EpistemicTransition invokes EpistemicShard/transition exclusively via NektarRuntime.
func (c *RestateClient) EpistemicTransition(ctx context.Context, shard string, in EpistemicTransitionInput) (*EpistemicTransitionResult, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[RuntimeEpistemicTransitionInput, EpistemicTransitionResult](c.Ingress, "NektarRuntime", "epistemicTransition").
		Request(ctx, RuntimeEpistemicTransitionInput{Shard: shard, Input: in}, opts...)
	if err != nil {
		return nil, fmt.Errorf("restate: epistemicTransition %s: %w", shard, err)
	}
	return &out, nil
}

// EpistemicAnchor invokes EpistemicShard/anchor exclusively via NektarRuntime.
func (c *RestateClient) EpistemicAnchor(ctx context.Context, shard string) (*EpistemicShardState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[string, EpistemicShardState](c.Ingress, "NektarRuntime", "epistemicAnchor").
		Request(ctx, shard, opts...)
	if err != nil {
		return nil, fmt.Errorf("restate: epistemicAnchor %s: %w", shard, err)
	}
	return &out, nil
}

// EpistemicState reads EpistemicShard/state via NektarRuntime.
func (c *RestateClient) EpistemicState(ctx context.Context, shard string) (*EpistemicShardState, error) {
	var opts []ingress.RequestOption
	if hdrs := c.runtimeHeaders(); hdrs != nil {
		opts = append(opts, restate.WithHeaders(hdrs))
	}
	out, err := ingress.Service[string, EpistemicShardState](c.Ingress, "NektarRuntime", "epistemicState").
		Request(ctx, shard, opts...)
	if err != nil {
		return nil, fmt.Errorf("restate: epistemicState %s: %w", shard, err)
	}
	return &out, nil
}

// --- Local per-shard serialized fallback when Restate sidecar is not attached ---

var localEpistemicLocks sync.Map // shard string -> *sync.Mutex

func lockLocalEpistemicShard(shard string) func() {
	v, _ := localEpistemicLocks.LoadOrStore(shard, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

// ExecuteEpistemicTransition executes a serialized Merkle Automaton transition locally with
// exact EpistemicShard exclusivity semantics (used when Restate sidecar is absent or in hermetic tests).
func (e *Engine) ExecuteEpistemicTransition(ctx context.Context, shard string, in EpistemicTransitionInput) (*EpistemicTransitionResult, error) {
	sh := strings.TrimSpace(shard)
	if sh == "" {
		sh = strings.TrimSpace(in.Remember.Shard)
	}
	if sh == "" {
		sh = "default"
	}
	unlock := lockLocalEpistemicShard(sh)
	defer unlock()

	wantAnchor := in.Anchor || in.Remember.AutoAnchor
	remIn := in.Remember
	remIn.Shard = sh
	remIn.AutoAnchor = false

	remRes, err := e.mem.Remember(ctx, remIn)
	if err != nil {
		return nil, err
	}
	var anchorTxID string
	anchorN := int64(-1)
	if wantAnchor {
		b, aErr := e.mem.AnchorShard(ctx, sh)
		if aErr != nil {
			return nil, aErr
		}
		if b != nil {
			anchorTxID = b.TxID
			anchorN = b.N
		}
	}
	st := e.loadEpistemicStateFromDB(sh)
	if anchorN < 0 {
		anchorN = st.AnchorN
	}
	return &EpistemicTransitionResult{
		Remember:   remRes,
		AnchorTxID: anchorTxID,
		AnchorN:    anchorN,
		State:      st,
	}, nil
}
