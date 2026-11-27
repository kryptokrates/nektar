package memrepo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kryptokrates/nektar/internal/core"
)

// BuildCommitMPE constructs the deterministic Merkle Proof Entity over a commit's metadata fields
// so any single field (author, modelId, policyCommit, treeHash, parents, message, timestamp) can be
// selectively disclosed and verified directly against the L1-anchored commit leaf.
func BuildCommitMPE(c CommitObject) (*core.MPE, error) {
	parentBytes, err := core.CanonicalJSON(c.Parents)
	if err != nil {
		return nil, err
	}
	fields := map[string][]byte{
		"repo":         []byte(c.Repo),
		"branch":       []byte(c.Branch),
		"treeHash":     []byte(c.TreeHash),
		"parents":      parentBytes,
		"authorPub":    []byte(c.AuthorPub),
		"modelId":      []byte(c.ModelID),
		"policyCommit": []byte(c.PolicyCommit),
		"message":      []byte(c.Message),
		"timestamp":    []byte(strconv.FormatInt(c.Timestamp, 10)),
	}
	return core.BuildMPE(fields)
}

type commitSignable struct {
	Version      int         `json:"v"`
	Repo         string      `json:"repo"`
	Branch       string      `json:"branch"`
	Parents      []ParentRef `json:"parents,omitempty"`
	TreeHash     string      `json:"treeHash"`
	MPERoot      string      `json:"mpeRoot"`
	AuthorPub    string      `json:"authorPub"`
	ModelID      string      `json:"modelId"`
	PolicyCommit string      `json:"policyCommit,omitempty"`
	Message      string      `json:"message"`
	Timestamp    int64       `json:"timestamp"`
}

func toCommitSignable(c CommitObject) commitSignable {
	return commitSignable{
		Version:      c.Version,
		Repo:         c.Repo,
		Branch:       c.Branch,
		Parents:      c.Parents,
		TreeHash:     c.TreeHash,
		MPERoot:      c.MPERoot,
		AuthorPub:    c.AuthorPub,
		ModelID:      c.ModelID,
		PolicyCommit: c.PolicyCommit,
		Message:      c.Message,
		Timestamp:    c.Timestamp,
	}
}

// ComputeCommitDigest returns the 32-byte double-SHA-256 digest signed by AuthorPub.
func ComputeCommitDigest(c CommitObject) ([]byte, error) {
	raw, err := core.CanonicalJSON(toCommitSignable(c))
	if err != nil {
		return nil, err
	}
	return core.DSHA256(core.Concat(core.TagCommitV1, raw)), nil
}

// ComputeCommitHash returns the canonical 32-byte hex CommitHash covering the signed commit envelope.
func ComputeCommitHash(c CommitObject) (string, error) {
	type hashedCommit struct {
		commitSignable
		Signature string `json:"signature"`
	}
	raw, err := core.CanonicalJSON(hashedCommit{
		commitSignable: toCommitSignable(c),
		Signature:      c.Signature,
	})
	if err != nil {
		return "", err
	}
	return core.ToHex(core.SHA256(core.Concat(core.TagCommitV1, raw))), nil
}

// FinalizeCommit computes MPERoot, calls signFn over the commit digest, and computes CommitHash.
func FinalizeCommit(c CommitObject, signFn func(digest []byte) ([]byte, error)) (CommitObject, error) {
	if c.Version == 0 {
		c.Version = 1
	}
	if c.Branch == "" {
		c.Branch = DefaultBranch
	}
	if c.AnchorN == 0 && c.AnchorTxID == "" {
		c.AnchorN = -1
	}
	mpe, err := BuildCommitMPE(c)
	if err != nil {
		return CommitObject{}, fmt.Errorf("memrepo: build commit MPE: %w", err)
	}
	c.MPERoot = mpe.RootHex
	digest, err := ComputeCommitDigest(c)
	if err != nil {
		return CommitObject{}, err
	}
	if signFn != nil {
		sig, err := signFn(digest)
		if err != nil {
			return CommitObject{}, fmt.Errorf("memrepo: sign commit: %w", err)
		}
		c.Signature = hex.EncodeToString(sig)
	}
	ch, err := ComputeCommitHash(c)
	if err != nil {
		return CommitObject{}, err
	}
	c.CommitHash = ch
	return c, nil
}

// VerifyCommitObject verifies a CommitObject's MPERoot, CommitHash, and mandatory strict
// compressed-pubkey + low-S DER ECDSA signature (failing closed on unsigned commits).
func VerifyCommitObject(c CommitObject) error {
	if c.Version != 1 {
		return fmt.Errorf("memrepo: unsupported commit version %d", c.Version)
	}
	if c.Repo == "" || c.TreeHash == "" {
		return errors.New("memrepo: commit missing repo or treeHash")
	}
	if c.AuthorPub == "" || c.Signature == "" {
		return errors.New("memrepo: unsigned commit (missing authorPub or signature)")
	}
	mpe, err := BuildCommitMPE(c)
	if err != nil {
		return fmt.Errorf("memrepo: recompute commit MPE: %w", err)
	}
	if !strings.EqualFold(mpe.RootHex, c.MPERoot) {
		return fmt.Errorf("memrepo: commit MPE root mismatch (got %s want %s)", c.MPERoot, mpe.RootHex)
	}
	wantHash, err := ComputeCommitHash(c)
	if err != nil {
		return err
	}
	if !strings.EqualFold(wantHash, c.CommitHash) {
		return fmt.Errorf("memrepo: commit hash mismatch (got %s want %s)", c.CommitHash, wantHash)
	}
	pub, _, err := core.ParseStrictCompressedPubKey(c.AuthorPub)
	if err != nil {
		return fmt.Errorf("memrepo: invalid authorPub: %w", err)
	}
	sig, err := core.ParseStrictLowSDER(c.Signature)
	if err != nil {
		return fmt.Errorf("memrepo: invalid DER signature: %w", err)
	}
	digest, err := ComputeCommitDigest(c)
	if err != nil {
		return err
	}
	if !sig.Verify(digest, pub) {
		return errors.New("memrepo: commit signature verification failed")
	}
	return nil
}
