package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// RememberInput describes a fragment (text fact or volumetric file/blob) to commit into a Memory Repo.
type RememberInput struct {
	Shard         string              `json:"shard"`
	Branch        string              `json:"branch,omitempty"`
	Path          string              `json:"path,omitempty"`
	MediaType     string              `json:"mediaType,omitempty"`
	Text          string              `json:"text"`
	Blob          []byte              `json:"blob,omitempty"`
	ChirpURL      string              `json:"chirpUrl,omitempty"`
	CommitMessage string              `json:"commitMessage,omitempty"`
	ExtraParents  []memrepo.ParentRef `json:"extraParents,omitempty"`
	Source        string              `json:"source,omitempty"`
	Level         int                 `json:"level,omitempty"`
	Class         string              `json:"class,omitempty"`
	BorgGroup     string              `json:"borgGroup,omitempty"` // if set, seals via BRC-369 keyed under group epoch
	CallerPub     string              `json:"callerPub,omitempty"`
	Principal     *policy.Principal   `json:"principal,omitempty"`
	Supersedes    int64               `json:"supersedes,omitempty"`
	Tainted       bool                `json:"tainted,omitempty"`
	AutoAnchor    bool                `json:"autoAnchor,omitempty"`
	Extra         map[string]string   `json:"extra,omitempty"`
}

// RememberResult summarizes a persisted, Git-committed fragment.
type RememberResult struct {
	Shard            string   `json:"shard"`
	Branch           string   `json:"branch,omitempty"`
	Path             string   `json:"path,omitempty"`
	Seq              int64    `json:"seq"`
	HDP              string   `json:"hdp"`
	Leaf             string   `json:"leaf"`
	MPERoot          string   `json:"mpeRoot,omitempty"`
	CommitHash       string   `json:"commitHash,omitempty"`
	TreeHash         string   `json:"treeHash,omitempty"`
	ChirpURL         string   `json:"chirpUrl,omitempty"`
	Encrypted        bool     `json:"encrypted"`
	BorgGroup        string   `json:"borgGroup,omitempty"`
	BorgEpoch        int64    `json:"borgEpoch,omitempty"`
	Dedup            bool     `json:"dedup"`
	AnchorTxID       string   `json:"anchorTxid,omitempty"`
	ImpactedReceipts []string `json:"impactedReceipts,omitempty"`
}

// Remember commits a provenance-signed fragment (or volumetric file blob) to a Memory Repo,
// optionally BRC-369 encrypting it to a Borg group epoch, recording a signed Git/Metanet CommitObject,
// and/or anchoring immediately on BSV via NektarAnchor.
func (s *Service) Remember(ctx context.Context, in RememberInput) (*RememberResult, error) {
	if strings.TrimSpace(in.Shard) == "" {
		in.Shard = "default"
	}
	if err := ValidateShardName(in.Shard, false); err != nil {
		return nil, err
	}
	branch, err := memrepo.NormalizeBranch(in.Branch)
	if err != nil {
		return nil, err
	}
	if in.Text == "" && len(in.Blob) == 0 && strings.TrimSpace(in.ChirpURL) == "" {
		return nil, errors.New("memory: text, blob, or chirpUrl is required")
	}
	var normPath string
	if strings.TrimSpace(in.Path) != "" {
		normPath, err = memrepo.NormalizePath(in.Path)
		if err != nil {
			return nil, err
		}
	}
	// Cedar write-gate enforcement (default owner level 100 when Principal omitted).
	writePrincipal := policy.Principal{Type: "Agent", ID: "owner", Level: 100}
	if in.Principal != nil && in.Principal.ID != "" {
		writePrincipal = *in.Principal
	}
	if dec, err := s.AuthorizeAction(writePrincipal, "write", policy.Resource{
		Type:  "Fragment",
		ID:    in.Shard + "#new",
		Level: int64(in.Level),
		Shard: in.Shard,
		Class: in.Class,
	}); err != nil || !dec.Allow {
		if err != nil {
			return nil, fmt.Errorf("memory: cedar write policy error: %w", err)
		}
		return nil, fmt.Errorf("memory: cedar policy denied write to shard %q (level %d > principal level %d)", in.Shard, in.Level, writePrincipal.Level)
	}
	src := in.Source
	if src == "" {
		src = "agent"
	}
	class := in.Class
	if class == "" {
		class = "general"
	}
	now := time.Now().Unix()
	prov := core.Provenance{
		Source:  src,
		Time:    now,
		ModelID: s.modelID.Provider + "/" + s.modelID.Release,
	}

	// Support both text facts and volumetric binary blobs (stored in BRC-167 CHIRP).
	payloadBytes := []byte(in.Text)
	var contentSHA []byte
	chirpURL := strings.TrimSpace(in.ChirpURL)
	mediaType := strings.TrimSpace(in.MediaType)

	if len(in.Blob) > 0 {
		sum := sha256.Sum256(in.Blob)
		contentSHA = sum[:]
		if in.BorgGroup == "" && chirpURL == "" {
			if u, _, _, cErr := bstack.StoreChirpWithHashes(s.objects, in.Blob); cErr == nil {
				chirpURL = u
			}
		}
		if in.BorgGroup != "" {
			// For Borg-encrypted binary blobs, HDP MUST commit to the exact plaintext bytes passed
			// to SealForEpoch so Recall's VerifyHDP(opened, ...) succeeds cleanly.
			payloadBytes = in.Blob
		} else if len(payloadBytes) == 0 {
			label := normPath
			if label == "" {
				label = "attachment.bin"
			}
			payloadBytes = []byte(fmt.Sprintf("[file:%s sha256:%s bytes:%d]", label, hex.EncodeToString(contentSHA)[:12], len(in.Blob)))
		}
	} else if chirpURL != "" {
		if resolved, rErr := bstack.ResolveChirp(s.objects, chirpURL); rErr == nil {
			sum := sha256.Sum256(resolved)
			contentSHA = sum[:]
		}
		if len(payloadBytes) == 0 {
			payloadBytes = []byte(fmt.Sprintf("[chirp:%s]", chirpURL))
		}
	}
	data := payloadBytes

	// Provenance signature via BRC-43 wallet key (zero raw key exposure).
	sigBytes, err := s.port.SignDigest(ctx, core.ProvenanceDigest(data, prov))
	if err != nil {
		return nil, fmt.Errorf("memory: sign provenance: %w", err)
	}
	prov.Signature = sigBytes
	provBytes := core.SerializeProvenance(prov)
	hdp := core.ComputeHDPV2(data, prov)
	hdpHex := core.ToHex(hdp)

	rec := store.FragmentRecord{
		Shard:         in.Shard,
		HDP:           hdpHex,
		Data:          data,
		Provenance:    provBytes,
		Level:         in.Level,
		Class:         class,
		Supersedes:    in.Supersedes,
		Tainted:       in.Tainted,
		CreatedAt:     now,
		Path:          normPath,
		MediaType:     mediaType,
		ChirpURL:      chirpURL,
		ContentSHA256: hex.EncodeToString(contentSHA),
	}

	if in.BorgGroup != "" {
		caller := in.CallerPub
		if caller == "" {
			caller = s.port.IdentityPubHex()
		}
		grp, ok := s.groups.GetGroup(in.BorgGroup)
		if !ok {
			grp, err = s.groups.CreateGroup(in.BorgGroup, in.BorgGroup, caller)
			if err != nil {
				return nil, err
			}
		}
		epoch := grp.CurrentEpoch
		epochSym, err := s.groups.EpochKey(in.BorgGroup, caller, epoch)
		if err != nil {
			return nil, fmt.Errorf("memory: borg encrypt access: %w", err)
		}
		defer core.Zero32Array(&epochSym)
		var contentID [32]byte
		copy(contentID[:], hdp)
		sealTarget := data
		if len(in.Blob) > 0 {
			sealTarget = in.Blob
		}
		env, contentKey, err := bstack.SealForEpoch(in.BorgGroup, epoch, epochSym, contentID, sealTarget)
		core.Zero32Array(&contentKey)
		if err != nil {
			return nil, err
		}
		metaBytes, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		rec.Data = nil // never store plaintext alongside BRC-369 ciphertext
		rec.Ciphertext, err = hex.DecodeString(env.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("memory: decode sealed ciphertext: %w", err)
		}
		rec.KeyedMeta = metaBytes
		rec.BorgGroup = in.BorgGroup
		rec.BorgEpoch = epoch
	}

	// ALWAYS bind L1 MPE root into TagLeafV2 on every committed fragment so selective-disclosure proofs
	// tie directly into the anchored BSV NektarAnchor UTXO Merkle root (no unanchored MPE fallback).
	dataHashHex := hex.EncodeToString(core.SHA256(data))
	if len(contentSHA) == 32 {
		dataHashHex = hex.EncodeToString(contentSHA)
	}
	mpe, mErr := buildCommittedFragmentMPE(in.Shard, hdpHex, prov, in.Level, class, normPath, mediaType, chirpURL, dataHashHex)
	if mErr != nil || mpe == nil {
		return nil, fmt.Errorf("memory: build L1 MPE commitment: %w", mErr)
	}
	mpeRootBytes := mpe.Root
	rec.MPERoot = mpe.RootHex

	// F-11 + LeafV2: compute leaf AFTER BorgGroup sealing & MPE root construction so on-chain R_n
	// ALWAYS commits to H(d|p), H(E), MPERoot (selective disclosure), and volumetric ContentSHA256!
	frag := core.Fragment{
		Data:          data,
		Provenance:    prov,
		HDP:           hdp,
		Ciphertext:    rec.Ciphertext,
		Path:          normPath,
		MediaType:     mediaType,
		ChirpURL:      chirpURL,
		ContentSHA256: contentSHA,
		MPERoot:       mpeRootBytes,
	}
	leaf := core.ComputeLeafV2(frag)
	rec.Leaf = core.ToHex(leaf)

	// Prepare content-addressed repo blob BEFORE starting the atomic SQLite transaction so
	// Borg-encrypted fragments NEVER pass plaintext to memrepo.BuildBlob or CHIRP s.objects.
	var (
		preBuiltBlob *memrepo.BlobObject
		commitMsg    string
		genPub       string
		genID        string
	)
	if !IsSystemShard(in.Shard) {
		anchors, _ := s.db.ListAnchors(in.Shard)
		if gp, gid, gErr := s.resolveRepoGenesis(ctx, in.Shard, anchors); gErr == nil {
			genPub, genID = gp, gid
		}
		commitMsg = strings.TrimSpace(in.CommitMessage)
		// If path is already known, we can build the blob now; otherwise build inside tx once seq is known.
		if normPath != "" {
			b, bErr := s.buildFragmentBlob(normPath, mediaType, data, in.Blob, chirpURL, rec.ContentSHA256, rec.BorgGroup, rec.BorgEpoch, rec.KeyedMeta, in.Level, class)
			if bErr != nil {
				return nil, fmt.Errorf("memory: build repo blob: %w", bErr)
			}
			preBuiltBlob = b
		}
	}

	var (
		stored     store.FragmentRecord
		dedup      bool
		commitHash string
		treeHash   string
		finalPath  string
	)
	unlockShard := s.lockShard(in.Shard)
	txErr := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		var aErr error
		stored, dedup, aErr = rtx.AppendFragment(rec)
		if aErr != nil {
			return aErr
		}
		if dedup || IsSystemShard(in.Shard) {
			commitHash = stored.CommitHash
			finalPath = stored.Path
			return nil
		}
		relPath := stored.Path
		if relPath == "" {
			relPath = fmt.Sprintf("facts/%04d.md", stored.Seq)
		}
		finalPath = relPath
		blobObj := preBuiltBlob
		if blobObj == nil {
			var bErr error
			blobObj, bErr = s.buildFragmentBlob(relPath, mediaType, data, in.Blob, chirpURL, stored.ContentSHA256, stored.BorgGroup, stored.BorgEpoch, stored.KeyedMeta, stored.Level, stored.Class)
			if bErr != nil {
				return fmt.Errorf("memory: build repo blob: %w", bErr)
			}
		}
		msg := commitMsg
		if msg == "" {
			if stored.BorgGroup != "" {
				// Never leak unencrypted Borg plaintext into mem_commits.message / git log!
				msg = fmt.Sprintf("borg: encrypted commit (%s)", relPath)
			} else {
				msg = summarizeCommitText(in.Text, relPath)
			}
		}
		cObj, cErr := s.recordFragmentCommitTx(ctx, rtx, stored, blobObj, branch, msg, in.ExtraParents, genPub, genID)
		if cErr != nil {
			return cErr
		}
		commitHash = cObj.CommitHash
		treeHash = cObj.TreeHash
		return nil
	})
	unlockShard()
	if txErr != nil {
		return nil, txErr
	}

	var impacted []string
	if in.Supersedes > 0 && !dedup {
		impacted, _ = s.OnRevision(ctx, in.Shard, in.Supersedes)
	}

	var anchorTxID string
	if in.AutoAnchor {
		if a, err := s.AnchorShard(ctx, in.Shard); err == nil && a != nil {
			anchorTxID = a.TxID
		} else if err != nil {
			return nil, err
		}
	} else if !dedup && !IsSystemShard(in.Shard) {
		s.mu.RLock()
		hook := s.onRemember
		s.mu.RUnlock()
		if hook != nil {
			s.hookWG.Add(1)
			go func(sh string) {
				defer s.hookWG.Done()
				hook(sh)
			}(in.Shard)
		}
	}

	return &RememberResult{
		Shard:            stored.Shard,
		Branch:           branch,
		Path:             finalPath,
		Seq:              stored.Seq,
		HDP:              stored.HDP,
		Leaf:             stored.Leaf,
		MPERoot:          stored.MPERoot,
		CommitHash:       commitHash,
		TreeHash:         treeHash,
		ChirpURL:         stored.ChirpURL,
		Encrypted:        stored.BorgGroup != "",
		BorgGroup:        stored.BorgGroup,
		BorgEpoch:        stored.BorgEpoch,
		Dedup:            dedup,
		AnchorTxID:       anchorTxID,
		ImpactedReceipts: impacted,
	}, nil
}
