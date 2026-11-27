package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

// RepoToolInput is the unified input schema for the `repo` tool across MCP, HTTP /v1/tools/repo, and A2A.
type RepoToolInput struct {
	Action        string            `json:"action"` // list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate
	Repo          string            `json:"repo,omitempty"`
	Shard         string            `json:"shard,omitempty"` // backward-compatible alias for repo
	Branch        string            `json:"branch,omitempty"`
	Path          string            `json:"path,omitempty"`
	Text          string            `json:"text,omitempty"`
	BlobBase64    string            `json:"blobBase64,omitempty"`
	MediaType     string            `json:"mediaType,omitempty"`
	Commit        string            `json:"commit,omitempty"`
	From          string            `json:"from,omitempty"`
	To            string            `json:"to,omitempty"`
	SourceBranch  string            `json:"sourceBranch,omitempty"`
	TargetBranch  string            `json:"targetBranch,omitempty"`
	NewRepo       string            `json:"newRepo,omitempty"`
	Tag           string            `json:"tag,omitempty"`
	Message       string            `json:"message,omitempty"`
	Strategy      string            `json:"strategy,omitempty"`
	Resolutions   map[string]string `json:"resolutions,omitempty"`
	Since         string            `json:"since,omitempty"`
	ChirpURL      string            `json:"chirpUrl,omitempty"`
	CapsuleBase64 string            `json:"capsuleBase64,omitempty"`
	MergeIntoMain *bool             `json:"mergeIntoMain,omitempty"`
	Seq           int64             `json:"seq,omitempty"`
	BatchN        int64             `json:"batchN,omitempty"`
	Seqs          []int64           `json:"seqs,omitempty"`
	Fields        []string          `json:"fields,omitempty"`
	NewOwner       string            `json:"newOwner,omitempty"`
	BorgGroup      string            `json:"borgGroup,omitempty"`
	Public         bool              `json:"public,omitempty"`
	Ontology       string            `json:"ontology,omitempty"`
	OntologyStrict bool              `json:"ontologyStrict,omitempty"`
	Level          int               `json:"level,omitempty"`
	AutoAnchor     bool              `json:"autoAnchor,omitempty"`
	Limit          int               `json:"limit,omitempty"`
}

// ExecRepo executes every Git-native & SingularGrit Memory Repo operation from one shared entry point.
func (tb *Toolbox) ExecRepo(ctx context.Context, in RepoToolInput) (any, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(in.Shard)
	}
	cc := CallerContextFor(ctx, policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}

	switch strings.ToLower(strings.TrimSpace(in.Action)) {
	case "", "list":
		repos, err := tb.mem.ListReposAuthorized(ctx, cc.Principal)
		if err != nil {
			return nil, err
		}
		return map[string]any{"repos": repos, "count": len(repos)}, nil

	case "log":
		commits, err := tb.mem.RepoLogAuthorized(ctx, repo, in.Branch, in.Limit, cc.Principal)
		if err != nil {
			return nil, err
		}
		return map[string]any{"repo": repo, "branch": in.Branch, "commits": commits, "count": len(commits)}, nil

	case "show":
		ref := strings.TrimSpace(in.Commit)
		if ref == "" {
			ref = strings.TrimSpace(in.To)
		}
		if ref == "" {
			ref = strings.TrimSpace(in.Branch)
		}
		c, err := tb.mem.ResolveCommitRefAuthorized(ctx, repo, ref, cc.Principal)
		if err != nil {
			return nil, err
		}
		patch, _ := tb.mem.RepoDiffPatchAuthorized(ctx, repo, "", c.CommitHash, cc.Principal, cc.DisallowBorg)
		return map[string]any{"repo": repo, "commit": c, "patch": patch}, nil

	case "tree":
		ref := strings.TrimSpace(in.Commit)
		if ref == "" {
			ref = strings.TrimSpace(in.To)
		}
		c, tr, _, err := tb.mem.RepoTreeAuthorized(ctx, repo, in.Branch, ref, cc.Principal, cc.DisallowBorg)
		if err != nil {
			return nil, err
		}
		return map[string]any{"repo": repo, "commit": c, "tree": tr}, nil

	case "read":
		normPath, err := memrepo.NormalizePath(in.Path)
		if err != nil {
			return nil, err
		}
		c, tr, blobs, err := tb.mem.RepoTreeAuthorized(ctx, repo, in.Branch, in.Commit, cc.Principal, cc.DisallowBorg)
		if err != nil {
			return nil, err
		}
		if tr == nil {
			return nil, fmt.Errorf("file %q not found in %s", normPath, repo)
		}
		for _, e := range tr.Entries {
			if e.Path != normPath {
				continue
			}
			b, ok := blobs[e.BlobHash]
			if !ok {
				return nil, fmt.Errorf("blob %s missing for %s", e.BlobHash, normPath)
			}
			out := map[string]any{
				"repo":          repo,
				"path":          e.Path,
				"mediaType":     e.MediaType,
				"size":          e.Size,
				"contentSha256": e.ContentSHA256,
				"encrypted":     e.Encrypted,
				"chirpUrl":      e.ChirpURL,
			}
			if c != nil {
				out["commitHash"] = c.CommitHash
				out["anchorTxid"] = c.AnchorTxID
			}
			if !b.Encrypted {
				raw := memrepo.ResolveBlobBytes(b, tb.mem.Objects())
				out["text"] = string(raw)
			} else if b.BorgGroup == "" && b.LatticeEpoch > 0 {
				out["latticeEpoch"] = b.LatticeEpoch
				if pt, ok := tb.mem.OpenBlobAuthorized(ctx, b, cc.Principal, cc.DisallowBorg); ok {
					out["text"] = string(pt)
				}
			} else {
				out["borgGroup"] = b.BorgGroup
				out["borgEpoch"] = b.BorgEpoch
			}
			return out, nil
		}
		return nil, fmt.Errorf("file %q not found in %s", normPath, repo)

	case "diff":
		changes, err := tb.mem.RepoDiffAuthorized(ctx, repo, in.From, in.To, cc.Principal, cc.DisallowBorg)
		if err != nil {
			return nil, err
		}
		patch, _ := tb.mem.RepoDiffPatchAuthorized(ctx, repo, in.From, in.To, cc.Principal, cc.DisallowBorg)
		return map[string]any{"repo": repo, "from": in.From, "to": in.To, "changes": changes, "patch": patch}, nil

	case "blame":
		lines, err := tb.mem.RepoBlameAuthorized(ctx, repo, in.Branch, in.Commit, in.Path, cc.Principal, cc.DisallowBorg)
		if err != nil {
			return nil, err
		}
		return map[string]any{"repo": repo, "path": in.Path, "lines": lines}, nil

	case "verify":
		// Verify either a .nkt capsule (capsuleBase64 or chirpUrl) OR a local repo's UTXO spend lineage chain!
		if strings.TrimSpace(in.CapsuleBase64) != "" || strings.TrimSpace(in.ChirpURL) != "" {
			var raw []byte
			if strings.TrimSpace(in.CapsuleBase64) != "" {
				dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.CapsuleBase64))
				if err != nil {
					return nil, fmt.Errorf("invalid capsuleBase64: %w", err)
				}
				raw = dec
			} else {
				var err error
				raw, err = bstack.ResolveChirp(tb.mem.Objects(), strings.TrimSpace(in.ChirpURL))
				if err != nil {
					return nil, err
				}
			}
			return tb.mem.InspectCapsuleAuthorized(ctx, raw, repo, cc.Principal, cc.DisallowBorg)
		}
		return tb.mem.VerifyShardChainAuthorized(repo, cc.Principal)

	case "commit":
		var blob []byte
		if strings.TrimSpace(in.BlobBase64) != "" {
			dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.BlobBase64))
			if err != nil {
				return nil, fmt.Errorf("invalid blobBase64: %w", err)
			}
			blob = dec
		}
		if cc.DisallowBorg && in.BorgGroup != "" {
			return nil, errors.New("operator token required for Borg group writes")
		}
		if cc.FromHTTP && !cc.IsOperator && in.AutoAnchor {
			return nil, errors.New("operator token required for autoAnchor")
		}
		return tb.mem.Remember(ctx, memory.RememberInput{
			Shard:          repo,
			Branch:         in.Branch,
			Path:           in.Path,
			MediaType:      in.MediaType,
			Text:           in.Text,
			Blob:           blob,
			ChirpURL:       in.ChirpURL,
			CommitMessage:  in.Message,
			BorgGroup:      in.BorgGroup,
			Public:         in.Public,
			Ontology:       in.Ontology,
			OntologyStrict: in.OntologyStrict,
			Level:          in.Level,
			Principal:      principalPtr,
			AutoAnchor:     in.AutoAnchor,
		})

	case "rm", "delete":
		if cc.FromHTTP && !cc.IsOperator && in.AutoAnchor {
			return nil, errors.New("operator token required for autoAnchor")
		}
		return tb.mem.DeleteFile(ctx, repo, in.Branch, in.Path, in.Message, in.AutoAnchor, principalPtr)

	case "branch":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo branch")
		}
		return tb.mem.CreateBranchAuthorized(ctx, repo, in.Branch, in.From, principalPtr)

	case "fork":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo fork")
		}
		return tb.mem.ForkRepo(ctx, repo, in.Branch, in.NewRepo, in.AutoAnchor, principalPtr)

	case "merge":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo merge")
		}
		res, err := tb.mem.MergeBranchWithOptions(ctx, memory.MergeBranchInput{
			Repo:         repo,
			SourceBranch: in.SourceBranch,
			TargetBranch: in.TargetBranch,
			Message:      in.Message,
			Strategy:     in.Strategy,
			Resolutions:  in.Resolutions,
			AutoAnchor:   in.AutoAnchor,
			Principal:    principalPtr,
		})
		if err != nil && errors.Is(err, memory.ErrMergeConflict) && res != nil {
			return map[string]any{
				"ok":             false,
				"conflict":       true,
				"error":          err.Error(),
				"baseCommitHash": res.BaseCommitHash,
				"conflicts":      res.Conflicts,
			}, nil
		}
		return res, err

	case "cherrypick", "cherry-pick", "cherry_pick":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo cherryPick")
		}
		ref := strings.TrimSpace(in.Commit)
		if ref == "" {
			ref = strings.TrimSpace(in.From)
		}
		return tb.mem.CherryPickCommit(ctx, repo, in.TargetBranch, ref, in.AutoAnchor, principalPtr)

	case "revert":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo revert")
		}
		ref := strings.TrimSpace(in.Commit)
		if ref == "" {
			ref = strings.TrimSpace(in.From)
		}
		return tb.mem.RevertCommit(ctx, repo, in.Branch, ref, in.AutoAnchor, principalPtr)

	case "tag", "tags":
		if strings.TrimSpace(in.Tag) == "" {
			tags, err := tb.mem.ListTagsAuthorized(repo, cc.Principal)
			if err != nil {
				return nil, err
			}
			return map[string]any{"repo": repo, "tags": tags}, nil
		}
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo tag")
		}
		return tb.mem.CreateTagAuthorized(ctx, repo, in.Tag, in.Commit, in.Message, principalPtr)

	case "export":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo export")
		}
		res, _, _, err := tb.mem.ExportCapsuleAuthorized(ctx, repo, in.Branch, in.Since, cc.Principal, cc.DisallowBorg)
		return res, err

	case "import":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for repo import")
		}
		var raw []byte
		if strings.TrimSpace(in.CapsuleBase64) != "" {
			dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.CapsuleBase64))
			if err != nil {
				return nil, fmt.Errorf("invalid capsuleBase64: %w", err)
			}
			raw = dec
		}
		merge := true
		if in.MergeIntoMain != nil {
			merge = *in.MergeIntoMain
		}
		return tb.mem.ImportCapsuleAuthorized(ctx, raw, in.ChirpURL, repo, in.TargetBranch, merge, principalPtr)

	case "prove":
		// Support BOTH L1-bound MPE selective field disclosure AND US 12,730,827 B2 level-6 subset proof!
		if len(in.Seqs) > 0 {
			return tb.mem.ProveSubsetAuthorized(ctx, repo, in.BatchN, in.Seqs, cc.Principal)
		}
		seq := in.Seq
		if seq <= 0 && strings.TrimSpace(in.Commit) != "" {
			c, err := tb.mem.ResolveCommitRefAuthorized(ctx, repo, in.Commit, cc.Principal)
			if err != nil {
				return nil, err
			}
			seq = c.Seq
		}
		if seq <= 0 {
			return nil, errors.New("repo prove requires seq (or commit) with fields[], or batchN + seqs[]")
		}
		fields := in.Fields
		if len(fields) == 0 {
			fields = []string{"source", "modelId", "time", "path"}
		}
		return tb.mem.ProveFragmentFieldsAuthorized(ctx, repo, seq, fields, cc.Principal)

	case "rotate":
		if !cc.IsOperator {
			return nil, errors.New("operator token required for NektarAnchor.Rotate() owner key rotation")
		}
		if strings.TrimSpace(in.NewOwner) == "" {
			return nil, errors.New("newOwner (33-byte compressed pubkey hex or 20-byte PKH hex) is required")
		}
		return tb.mem.RotateShardOwner(ctx, repo, in.NewOwner)

	default:
		return nil, fmt.Errorf("unknown repo action %q (use list|log|show|tree|read|diff|blame|verify|commit|rm|branch|fork|merge|cherryPick|revert|tag|export|import|prove|rotate)", in.Action)
	}
}

// marshalPrettyJSON formats an MCP resource payload cleanly.
func marshalPrettyJSON(v any) string {
	b, _ := json.MarshalIndent(v, "", "  ")
	return string(b)
}
