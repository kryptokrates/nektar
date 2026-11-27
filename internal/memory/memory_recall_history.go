package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
)

// recallAtCommit performs epistemic time-travel recall against the immutable TreeObject & Blobs
// at in.AtCommit (commit SHA/prefix or tag) or in.Branch, enforcing Cedar authorization and Borg decryption.
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
		if anchors, aErr := s.db.ListAnchors(in.Shard); aErr == nil {
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
		var plaintext []byte
		if b.Encrypted {
			if in.DisallowBorg || callerPub == "" || len(b.KeyedMeta) == 0 {
				continue
			}
			var env bstack.SealedEnvelope
			if jErr := json.Unmarshal(b.KeyedMeta, &env); jErr != nil {
				continue
			}
			// Verify sealed envelope group/epoch binding matches BlobObject metadata (MED-12).
			if (b.BorgGroup != "" && env.GroupID != b.BorgGroup) || env.Epoch != b.BorgEpoch {
				continue
			}
			epochSym, kErr := s.groups.EpochKey(b.BorgGroup, callerPub, b.BorgEpoch)
			if kErr != nil {
				continue
			}
			opened, oErr := bstack.OpenWithEpoch(&env, epochSym)
			core.Zero32Array(&epochSym)
			if oErr != nil {
				continue
			}
			if b.ContentSHA256 != "" {
				sum := sha256.Sum256(opened)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
					continue
				}
			}
			plaintext = opened
		} else if len(b.Inline) > 0 {
			if b.ContentSHA256 != "" {
				sum := sha256.Sum256(b.Inline)
				if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
					continue
				}
			}
			plaintext = b.Inline
		} else if b.ChirpURL != "" {
			if resolved, rErr := bstack.ResolveChirp(s.objects, b.ChirpURL); rErr == nil {
				if b.ContentSHA256 != "" {
					sum := sha256.Sum256(resolved)
					if !strings.EqualFold(hex.EncodeToString(sum[:]), b.ContentSHA256) {
						continue
					}
				}
				plaintext = resolved
			} else {
				plaintext = []byte(fmt.Sprintf("[chirp:%s %d bytes]", b.ChirpURL, b.Size))
			}
		}
		txt := string(plaintext)
		if qLower != "" && !strings.Contains(strings.ToLower(txt), qLower) && !strings.Contains(strings.ToLower(e.Path), qLower) {
			continue
		}
		out = append(out, RecalledFragment{
			Shard:       in.Shard,
			Seq:         int64(idx + 1),
			Path:        e.Path,
			CommitHash:  c.CommitHash,
			Text:        txt,
			HDP:         b.ContentSHA256,
			Leaf:        c.FragmentLeaf,
			Level:       lvl,
			Class:       cls,
			BorgGroup:   b.BorgGroup,
			BorgEpoch:   b.BorgEpoch,
			AnchorTxID:  c.AnchorTxID,
			AnchorState: anchorState,
			CreatedAt:   c.Timestamp,
		})
		if in.Limit > 0 && len(out) >= in.Limit {
			break
		}
	}
	return out, nil
}
