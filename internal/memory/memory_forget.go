package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

func (s *Service) buildFragmentBlob(
	relPath, mediaType string,
	data, rawBlob []byte,
	chirpURL, contentSHAHex string,
	borgGroup string,
	borgEpoch int64,
	keyedMeta []byte,
	level int,
	class string,
) (*memrepo.BlobObject, error) {
	payload := data
	if len(rawBlob) > 0 {
		payload = rawBlob
	}
	if borgGroup != "" {
		// Encrypted fragments MUST NEVER store plaintext inline or write plaintext to CHIRP.
		shaHex := contentSHAHex
		if shaHex == "" && len(payload) > 0 {
			sum := sha256.Sum256(payload)
			shaHex = hex.EncodeToString(sum[:])
		}
		b, err := memrepo.BuildEncryptedBlob(relPath, mediaType, int64(len(payload)), shaHex, borgGroup, borgEpoch, keyedMeta)
		if err != nil {
			return nil, err
		}
		b.Level = level
		b.Class = class
		return b, nil
	}
	b, _, err := memrepo.BuildBlob(s.objects, relPath, mediaType, payload, len(rawBlob) > 0)
	if err != nil {
		return nil, err
	}
	if chirpURL != "" && b.ChirpURL == "" {
		b.ChirpURL = chirpURL
		h, hErr := memrepo.ComputeBlobHash(*b)
		if hErr != nil {
			return nil, hErr
		}
		b.BlobHash = h
	}
	b.Level = level
	b.Class = class
	return b, nil
}

func buildCommittedFragmentMPE(shard, hdpHex string, prov core.Provenance, level int, class, relPath, mediaType, chirpURL, dataHashHex string) (*core.MPE, error) {
	return core.BuildMPE(map[string][]byte{
		"shard":      []byte(shard),
		"hdp":        []byte(hdpHex),
		"source":     []byte(prov.Source),
		"time":       []byte(strconv.FormatInt(prov.Time, 10)),
		"modelId":    []byte(prov.ModelID),
		"level":      []byte(strconv.Itoa(level)),
		"class":      []byte(class),
		"path":       []byte(relPath),
		"mediaType":  []byte(mediaType),
		"chirpUrl":   []byte(chirpURL),
		"signature":  []byte(hex.EncodeToString(prov.Signature)),
		"dataSha256": []byte(dataHashHex),
	})
}

// Forget shreds local plaintext/ciphertext of (shard, seq) while keeping the anchored commitment.
func (s *Service) Forget(ctx context.Context, shard string, seq int64) error {
	return s.ForgetAuthorized(ctx, shard, seq, policy.Principal{Type: "Agent", ID: "owner", Level: 100})
}

// ForgetAuthorized checks shard validity and Cedar write authorization before shredding (shard, seq),
// including its SQLite fragment/FTS/mem_objects/commit message AND any stored CHIRP objects/public allowlist.
func (s *Service) ForgetAuthorized(_ context.Context, shard string, seq int64, principal policy.Principal) error {
	if err := ValidateShardName(shard, false); err != nil {
		return err
	}
	unlock := s.lockShard(shard)
	defer unlock()

	rec, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return err
	}
	if principal.ID == "" {
		principal = policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	}
	if dec, aErr := s.AuthorizeAction(principal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    fmt.Sprintf("%s#%d", rec.Shard, rec.Seq),
		Level: int64(rec.Level),
		Shard: rec.Shard,
		Class: rec.Class,
	}); aErr != nil || !dec.Allow {
		if aErr != nil {
			return fmt.Errorf("memory: cedar forget policy error: %w", aErr)
		}
		return fmt.Errorf("memory: cedar policy denied forget on %s#%d (level %d > principal level %d)", rec.Shard, rec.Seq, rec.Level, principal.Level)
	}
	if rec.ChirpURL != "" && s.objects != nil {
		if hashes, _ := bstack.DeleteChirp(s.objects, rec.ChirpURL); len(hashes) > 0 {
			s.RevokePublicChirpHashes(hashes...)
		}
	}
	if err := s.db.ForgetFragment(shard, seq); err != nil {
		return err
	}
	s.projCacheMu.Lock()
	s.projCache = nil
	s.projCacheMu.Unlock()
	return nil
}
