package memrepo

import (
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/lightwebinc/bcommon/cbor"

	"github.com/kryptokrates/nektar/internal/core"
)

// BuildCommitMPE constructs the deterministic Merkle Proof Entity over a commit's metadata fields
// so any single field (author, modelId, policyCommit, treeHash, parents, message, timestamp) can be
// selectively disclosed and verified directly against the L1-anchored commit leaf.
func BuildCommitMPE(c CommitObject) (*core.MPE, error) {
	parentBytes, err := core.EncodeCBOR(parentsToCBOR(c.Parents))
	if err != nil {
		return nil, err
	}
	fields := map[string][]byte{
		"repo":           []byte(c.Repo),
		"branch":         []byte(c.Branch),
		"treeHash":       []byte(c.TreeHash),
		"parents":        parentBytes,
		"authorPub":      []byte(c.AuthorPub),
		"modelId":        []byte(c.ModelID),
		"policyCommit":   []byte(c.PolicyCommit),
		"ontologyCommit": []byte(c.OntologyCommit),
		"message":        []byte(c.Message),
		"timestamp":      []byte(strconv.FormatInt(c.Timestamp, 10)),
	}
	if c.CommitKind != 0 {
		fields["commitKind"] = []byte(strconv.Itoa(c.CommitKind))
	}
	return core.BuildMPE(fields)
}

// ComputeCommitDigest returns the 32-byte double-SHA-256 CBOR digest signed by AuthorPub.
func ComputeCommitDigest(c CommitObject) ([]byte, error) {
	return core.HashCBOR(core.TagCommitV1, commitSignableCBOR(c))
}

// ComputeCommitHash returns the canonical 32-byte hex DSHA-256 CommitHash covering the signed CBOR commit envelope.
func ComputeCommitHash(c CommitObject) (string, error) {
	m := append(commitSignableCBOR(c), cbor.Pair{Key: uint64(14), Val: c.Signature})
	h, err := core.HashCBOR(core.TagCommitV1, m)
	if err != nil {
		return "", err
	}
	return core.ToHex(h), nil
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
	digest, err := ComputeCommitDigest(c)
	if err != nil {
		return err
	}
	if err := core.FastVerifyStrictHex(c.AuthorPub, c.Signature, digest); err != nil {
		return fmt.Errorf("memrepo: commit signature verification failed: %w", err)
	}
	return nil
}
