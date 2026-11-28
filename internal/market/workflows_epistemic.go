package market

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/kryptokrates/nektar/internal/memory"
)

// EpistemicShardState tracks the durable Merkle Automaton head state of a single shard.
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

// RuntimeEpistemicTransitionInput routes a shard transition.
type RuntimeEpistemicTransitionInput struct {
	Shard string                   `json:"shard"`
	Input EpistemicTransitionInput `json:"input"`
}

var localEpistemicLocks sync.Map // shard string -> *sync.Mutex

func lockLocalEpistemicShard(shard string) func() {
	v, _ := localEpistemicLocks.LoadOrStore(shard, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
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
	if anchors, err := e.mem.DB().ListAnchorSummaries(shard); err == nil && len(anchors) > 0 {
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

// ExecuteEpistemicTransition executes a serialized Merkle Automaton transition per shard,
// guaranteeing contiguous NektarAnchor.Append() UTXO chains with zero UTXO double-spend contention.
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
			_, _ = e.EmitTxEventLocal(ctx, b.TxID, TxEventInput{
				Kind:       "broadcast",
				Shard:      sh,
				ObservedAt: time.Now().Unix(),
			})
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

// ExecuteEpistemicAnchor exclusively anchors all unanchored transitions on shard.
func (e *Engine) ExecuteEpistemicAnchor(ctx context.Context, shard string) (*EpistemicShardState, error) {
	sh := strings.TrimSpace(shard)
	if sh == "" {
		sh = "default"
	}
	unlock := lockLocalEpistemicShard(sh)
	defer unlock()

	b, err := e.mem.AnchorShard(ctx, sh)
	if err != nil {
		return nil, err
	}
	if b != nil && b.TxID != "" {
		_, _ = e.EmitTxEventLocal(ctx, b.TxID, TxEventInput{
			Kind:       "broadcast",
			Shard:      sh,
			ObservedAt: time.Now().Unix(),
		})
	}
	st := e.loadEpistemicStateFromDB(sh)
	return &st, nil
}
