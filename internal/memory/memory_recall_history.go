package memory

import (
	"context"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/policy"
)

// recallAtCommit performs epistemic time-travel recall against the immutable TreeObject & Blobs
// at in.AtCommit (commit SHA/prefix or tag) or in.Branch, enforcing Cedar authorization and CLKA/Borg decryption.
func (s *Service) recallAtCommit(ctx context.Context, in RecallInput) ([]RecalledFragment, error) {
	ref := strings.TrimSpace(in.AtCommit)
	if ref == "" {
		ref = strings.TrimSpace(in.Branch)
	}
	c, tr, blobs, err := s.RepoTree(ctx, in.Shard, in.Branch, ref)
	if err != nil {
		return nil, err
	}
	if c == nil || tr == nil {
		return nil, nil
	}
	principal := in.Principal
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	action := strings.TrimSpace(in.Action)
	if action == "" {
		action = "read"
	}
	callerPub := in.CallerPub
	if callerPub == "" && !in.DisallowBorg {
		callerPub = s.port.IdentityPubHex()
	}
	s.mu.RLock()
	polText := s.policyText
	s.mu.RUnlock()

	qLower := strings.ToLower(strings.TrimSpace(in.Query))
	anchorState := "unanchored"
	if c.AnchorTxID != "" {
		anchorState = "broadcast"
		if anchors, aErr := s.db.ListAnchorSummaries(in.Shard); aErr == nil {
			for _, a := range anchors {
				if a.TxID == c.AnchorTxID && a.Settled {
					anchorState = "mined"
					break
				}
			}
		}
	}

	out := make([]RecalledFragment, 0, len(tr.Entries))
	for idx, e := range tr.Entries {
		b, ok := blobs[e.BlobHash]
		if !ok || b.Forgotten {
			continue
		}
		lvl := e.Level
		if b.Level > lvl {
			lvl = b.Level
		}
		cls := e.Class
		if cls == "" {
			cls = b.Class
		}
		if cls == "" {
			cls = "repo"
		}
		dec, aErr := policy.Authorize(polText, principal, action, policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s@%s:%s", in.Shard, c.CommitHash[:min(7, len(c.CommitHash))], e.Path),
			Level: int64(lvl),
			Shard: in.Shard,
			Class: cls,
		})
		if aErr != nil || !dec.Allow {
			continue
		}
		plaintext, opened := s.openBlobPlaintext(ctx, b, principal, callerPub, in.Clearance, in.DisallowBorg, in.DisallowPrivate)
		if !opened {
			if !b.Encrypted && b.ChirpURL != "" {
				plaintext = []byte(fmt.Sprintf("[chirp:%s %d bytes]", b.ChirpURL, b.Size))
			} else {
				continue
			}
		}
		txt := string(plaintext)
		if qLower != "" && !strings.Contains(strings.ToLower(txt), qLower) && !strings.Contains(strings.ToLower(e.Path), qLower) {
			continue
		}
		out = append(out, RecalledFragment{
			Shard:        in.Shard,
			Seq:          int64(idx + 1),
			Path:         e.Path,
			CommitHash:   c.CommitHash,
			Text:         txt,
			HDP:          b.ContentSHA256,
			Leaf:         c.FragmentLeaf,
			Level:        lvl,
			Class:        cls,
			Public:       !b.Encrypted,
			LatticeEpoch: b.LatticeEpoch,
			BorgGroup:    b.BorgGroup,
			BorgEpoch:    b.BorgEpoch,
			AnchorTxID:   c.AnchorTxID,
			AnchorState:  anchorState,
			CreatedAt:    c.Timestamp,
		})
		if in.Limit > 0 && len(out) >= in.Limit {
			break
		}
	}
	return out, nil
}
