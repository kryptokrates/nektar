package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

const WelcomeShard = "welcome"

// EnsureWelcome seeds a concise, verifiable starter `welcome` shard on a brand-new empty node
// so first-run `recall {}` and `groundedAnswer` succeed immediately in <50ms even offline
// (cold-start & friction J2: zero empty-room first session).
func (s *Service) EnsureWelcome(ctx context.Context) (bool, error) {
	shards, err := s.db.ListShards()
	if err != nil {
		return false, err
	}
	for _, sh := range shards {
		if sh != ReceiptsShard {
			return false, nil // already populated
		}
	}
	seeds := []string{
		"Nektar stores verifiable, encrypted AI agent memory anchored on BSV with SPV proofs, AWS Cedar policy isolation, and Google Mangle Datalog grounding.",
		"Always call recall first, route factual answers through groundedAnswer citing verified fragments, and preserve the nektarr1.* receipt footer verbatim.",
		"Agents discover peers through the signed dir lane and bfinger handles, buy knowledge with want {domains, question, maxSats}, and earn sats with sell {shard, domains, priceSats} or Hub bounties.",
	}
	for _, txt := range seeds {
		if _, err := s.Remember(ctx, RememberInput{
			Shard:  WelcomeShard,
			Text:   txt,
			Source: "nektar:welcome",
			Class:  "guide",
		}); err != nil {
			return false, fmt.Errorf("welcome seed: %w", err)
		}
	}
	return true, nil
}

// ListUserShards returns all non-system shard names (excluding __receipts__).
func (s *Service) ListUserShards() ([]string, error) {
	all, err := s.db.ListShards()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(all))
	for _, sh := range all {
		if sh == ReceiptsShard {
			continue
		}
		out = append(out, sh)
	}
	return out, nil
}

// RecallAllShards recalls across every readable non-system shard when RecallInput.Shard is omitted,
// returning both the discovered shard names and matching fragments (eliminating wrong-shard dead ends).
func (s *Service) RecallAllShards(ctx context.Context, in RecallInput) ([]string, []RecalledFragment, error) {
	shards, err := s.ListUserShards()
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.Shard) != "" {
		items, err := s.Recall(ctx, in)
		return shards, items, err
	}
	var combined []RecalledFragment
	remaining := in.Limit
	for _, sh := range shards {
		sub := in
		sub.Shard = sh
		if remaining > 0 {
			sub.Limit = remaining
		}
		items, err := s.Recall(ctx, sub)
		if err != nil {
			return shards, nil, err
		}
		combined = append(combined, items...)
		if in.Limit > 0 {
			remaining = in.Limit - len(combined)
			if remaining <= 0 {
				break
			}
		}
	}
	return shards, combined, nil
}

// ImportShardPackBytes verifies and imports a raw ShardPack JSON payload (used after CHIRP resolution).
func (s *Service) ImportShardPackBytes(raw []byte, targetShard string) (int, string, error) {
	var pack ShardPack
	if err := json.Unmarshal(raw, &pack); err != nil {
		return 0, "", fmt.Errorf("memory: decode shard pack: %w", err)
	}
	if targetShard == "" {
		targetShard = pack.Shard
	}
	imported := 0
	for _, f := range pack.Fragments {
		f.Shard = targetShard
		f.BatchN = -1
		f.LeafIndex = -1
		if _, dedup, err := s.db.AppendFragment(f); err == nil && !dedup {
			imported++
		}
	}
	return imported, targetShard, nil
}
