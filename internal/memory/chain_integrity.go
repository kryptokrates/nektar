package memory

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/bsv-blockchain/go-sdk/transaction"
	"github.com/lightwebinc/bcommon/guard"

	"github.com/kryptokrates/nektar/internal/contracts"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/store"
)

// ShardChainReport summarizes local hash-chain, leaf-hash, automaton-transition, and anchor-chain integrity for one shard.
type ShardChainReport struct {
	Shard           string `json:"shard"`
	OK              bool   `json:"ok"`
	FragmentCount   int    `json:"fragmentCount"`
	VerifiedCount   int    `json:"verifiedCount"`
	TransitionCount int    `json:"transitionCount,omitempty"`
	HeadStateQ      string `json:"headStateQ,omitempty"`
	HeadMmrRoot     string `json:"headMmrRoot,omitempty"`
	BrokenSeq       int64  `json:"brokenSeq,omitempty"`
	Reason          string `json:"reason,omitempty"`
	HeadChainHash   string `json:"headChainHash,omitempty"`
	AnchorCount     int    `json:"anchorCount"`
	AnchorsOK       bool   `json:"anchorsOk"`
}

// VerifyShardChain walks every fragment, transition, and anchor in shard in ascending order and verifies:
//  1. Unbroken seq continuity (catches silent SQLite deletion/insertion/reordering before anchoring),
//  2. PrevLeaf and ChainHash linkage via store.ComputeFragmentChainHash,
//  3. Unified L1-bound core.ComputeLeaf recomputation (binding HDP, H(E), MPERoot, ContentSHA256, CommitHash, TransitionHash)
//     and framed v2 HDP verification on non-forgotten plaintext fragments (plus plaintext-injection detection on CLKA rows),
//  4. Canonical CBOR Merkle Automaton transition chain continuity (prev_q -> next_q),
//  5. Strict NektarAnchor singleton UTXO spend lineage (tx_{n-1}:0 -> tx_n:0), R' chaining, StateHash (Q_n), and MMR root continuity.
func (s *Service) VerifyShardChain(shard string) (*ShardChainReport, error) {
	return s.VerifyShardChainAuthorized(shard, ownerPrincipal())
}

// VerifyShardChainAuthorized enforces system-shard denial for non-operators before verifying shard integrity.
func (s *Service) VerifyShardChainAuthorized(shard string, principal policy.Principal) (*ShardChainReport, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("memory: store not initialized")
	}
	if err := ValidateShardName(shard, isOperatorPrincipal(principal)); err != nil {
		return nil, err
	}
	frags, err := s.db.ListFragments(shard)
	if err != nil {
		return nil, err
	}
	rep := &ShardChainReport{
		Shard:         shard,
		OK:            true,
		FragmentCount: len(frags),
		AnchorsOK:     true,
	}

	var prevLeaf, prevChain string
	var expectedSeq int64 = 1
	for _, f := range frags {
		if f.Seq != expectedSeq {
			rep.OK = false
			rep.BrokenSeq = expectedSeq
			rep.Reason = fmt.Sprintf("sequence gap or reorder: expected seq %d, found %d", expectedSeq, f.Seq)
			return rep, nil
		}
		if f.ChainHash == "" {
			rep.OK = false
			rep.BrokenSeq = f.Seq
			rep.Reason = fmt.Sprintf("missing chain_hash at seq %d", f.Seq)
			return rep, nil
		}
		if f.Seq > 1 && f.PrevLeaf != prevLeaf {
			rep.OK = false
			rep.BrokenSeq = f.Seq
			rep.Reason = fmt.Sprintf("prev_leaf broken at seq %d: got %s want %s", f.Seq, f.PrevLeaf, prevLeaf)
			return rep, nil
		}
		wantChain := store.ComputeFragmentChainHash(prevChain, f.Leaf)
		if f.ChainHash != wantChain {
			rep.OK = false
			rep.BrokenSeq = f.Seq
			rep.Reason = fmt.Sprintf("chain_hash mismatch at seq %d: got %s want %s", f.Seq, f.ChainHash, wantChain)
			return rep, nil
		}
		prevChain = f.ChainHash

		// Verify local plaintext/ciphertext unified L1-bound leaf commitment on non-forgotten rows.
		if !f.Forgotten {
			hdpBytes, hErr := hex.DecodeString(f.HDP)
			if hErr != nil || len(hdpBytes) != 32 {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("invalid 32-byte HDP hex at seq %d", f.Seq)
				return rep, nil
			}
			if f.LatticeEpoch > 0 && len(f.Data) > 0 {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("unexpected plaintext data injected into CLKA-encrypted fragment at seq %d", f.Seq)
				return rep, nil
			}
			if f.BorgGroup == "" && f.LatticeEpoch == 0 && len(f.Data) > 0 && len(f.Provenance) > 0 {
				if !core.VerifyHDP(f.Data, f.Provenance, hdpBytes) {
					rep.OK = false
					rep.BrokenSeq = f.Seq
					rep.Reason = fmt.Sprintf("plaintext/provenance HDP tamper detected at seq %d", f.Seq)
					return rep, nil
				}
			}
			mpeBytes, mErr := hex.DecodeString(strings.TrimSpace(f.MPERoot))
			if mErr != nil || len(mpeBytes) != 32 {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("missing 32-byte L1 MPE root at seq %d", f.Seq)
				return rep, nil
			}
			var contentBytes, commitBytes, transBytes []byte
			if f.ContentSHA256 != "" {
				contentBytes, _ = hex.DecodeString(f.ContentSHA256)
			}
			if f.CommitHash != "" {
				commitBytes, _ = hex.DecodeString(f.CommitHash)
			}
			if f.TransitionHash != "" {
				transBytes, _ = hex.DecodeString(f.TransitionHash)
			}
			recomputedLeaf := core.ToHex(core.ComputeLeaf(core.Fragment{
				Data:           f.Data,
				HDP:            hdpBytes,
				Ciphertext:     f.Ciphertext,
				MPERoot:        mpeBytes,
				ContentSHA256:  contentBytes,
				CommitHash:     commitBytes,
				TransitionHash: transBytes,
			}))
			if recomputedLeaf != f.Leaf {
				rep.OK = false
				rep.BrokenSeq = f.Seq
				rep.Reason = fmt.Sprintf("leaf hash tamper detected at seq %d", f.Seq)
				return rep, nil
			}
		}

		prevLeaf = f.Leaf
		rep.VerifiedCount++
		expectedSeq++
	}
	rep.HeadChainHash = prevChain

	// Verify Merkle Automaton transitions chain if present.
	if transitions, tErr := s.db.ListTransitions(shard, len(frags)+16); tErr == nil && len(transitions) > 0 {
		rep.TransitionCount = len(transitions)
		expectedPrevQ := core.ToHex(core.Zero32Bytes())
		for _, tr := range transitions {
			if !strings.EqualFold(tr.PrevQ, expectedPrevQ) {
				rep.OK = false
				rep.BrokenSeq = tr.Seq
				rep.Reason = fmt.Sprintf("automaton transition prev_q broken at seq %d", tr.Seq)
				return rep, nil
			}
			dec, dErr := core.DecodeTransitionCBOR(tr.BodyCBOR)
			if dErr != nil {
				rep.OK = false
				rep.BrokenSeq = tr.Seq
				rep.Reason = fmt.Sprintf("corrupt transition CBOR at seq %d: %v", tr.Seq, dErr)
				return rep, nil
			}
			if !strings.EqualFold(core.ToHex(dec.Hash), tr.Hash) {
				rep.OK = false
				rep.BrokenSeq = tr.Seq
				rep.Reason = fmt.Sprintf("transition hash mismatch at seq %d", tr.Seq)
				return rep, nil
			}
			expectedPrevQ = tr.NextQ
		}
		rep.HeadStateQ = expectedPrevQ
	}

	anchors, err := s.db.ListAnchors(shard)
	if err != nil {
		return nil, err
	}
	rep.AnchorCount = len(anchors)
	prevRp := core.Zero32Bytes()
	mmr, _ := core.NewMMR(nil)
	var prevAnchorTx *transaction.Transaction
	var genesisShard, genesisOwnerPub string
	for i, a := range anchors {
		if a.N != int64(i) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor batch gap at n=%d (found n=%d)", i, a.N)
			return rep, nil
		}
		rBytes, err1 := hex.DecodeString(a.RootHex)
		rpBytes, err2 := hex.DecodeString(a.ChainedHex)
		if err1 != nil || err2 != nil || len(rBytes) != 32 || len(rpBytes) != 32 {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("invalid anchor R/Rp hex at n=%d", a.N)
			return rep, nil
		}
		wantRp := core.ChainRp(prevRp, rBytes, uint64(a.Size))
		if !core.EqualBytes(rpBytes, wantRp) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor Rp chain mismatch at n=%d", a.N)
			return rep, nil
		}
		if _, mErr := mmr.Append(rBytes); mErr != nil {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor MMR append error at n=%d: %v", a.N, mErr)
			return rep, nil
		}
		wantMmr := mmr.Root()

		if len(a.Beef) == 0 {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d missing AtomicBEEF for UTXO lineage check", a.N)
			return rep, nil
		}
		curBeef, curTx, txHash, pErr := guard.ParseBEEF(a.Beef, guard.DefaultBound)
		if pErr != nil || curTx == nil || txHash == nil || txHash.String() != a.TxID {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d invalid BEEF or txid mismatch", a.N)
			return rep, nil
		}
		if len(curTx.Outputs) == 0 || curTx.Outputs[0].LockingScript == nil {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d missing NektarAnchor output 0", a.N)
			return rep, nil
		}
		st, dErr := contracts.DecodeNektarAnchor(*curTx.Outputs[0].LockingScript)
		if dErr != nil {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d invalid spendable META NektarAnchor: %v", a.N, dErr)
			return rep, nil
		}
		if st.N != uint64(a.N) || st.Size != uint64(a.Size) ||
			!core.EqualBytes(st.R, rBytes) || !core.EqualBytes(st.Rp, rpBytes) ||
			!core.EqualBytes(st.MmrRoot, wantMmr) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d locking script state mismatches persisted batch anchor or MMR root", a.N)
			return rep, nil
		}
		if a.StateHash != "" && !strings.EqualFold(core.ToHex(st.StateHash), a.StateHash) {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d on-chain StateHash Q_n mismatches persisted anchor", a.N)
			return rep, nil
		}
		if genesisShard == "" {
			genesisShard = st.Shard
			genesisOwnerPub = st.OwnerPub
		} else if st.Shard != genesisShard || st.OwnerPub != genesisOwnerPub {
			rep.OK = false
			rep.AnchorsOK = false
			rep.Reason = fmt.Sprintf("anchor n=%d mutated immutable genesis shard/ownerPub", a.N)
			return rep, nil
		}
		if prevAnchorTx != nil {
			if lErr := contracts.VerifyAnchorUTXOLineageWithBEEF(prevAnchorTx, curTx, curBeef); lErr != nil {
				rep.OK = false
				rep.AnchorsOK = false
				rep.Reason = fmt.Sprintf("anchor n=%d UTXO spend lineage broken: %v", a.N, lErr)
				return rep, nil
			}
		}
		prevAnchorTx = curTx
		prevRp = rpBytes
		rep.HeadMmrRoot = core.ToHex(wantMmr)
		if len(st.StateHash) == 32 {
			rep.HeadStateQ = core.ToHex(st.StateHash)
		}
	}

	return rep, nil
}

// VerifyAllShardsIntegrity verifies every non-system shard in the local store.
func (s *Service) VerifyAllShardsIntegrity() ([]ShardChainReport, error) {
	shards, err := s.ListUserShards()
	if err != nil {
		return nil, err
	}
	out := make([]ShardChainReport, 0, len(shards))
	for _, sh := range shards {
		r, err := s.VerifyShardChain(sh)
		if err != nil {
			return nil, err
		}
		out = append(out, *r)
	}
	return out, nil
}

// FragmentFieldProof is an L1-bound selective-disclosure Merkle Proof Entity (MPE) proving chosen
// metadata fields of a fragment (e.g. source, time, modelId, class, level, hdp, path, chirpUrl)
// directly against its BSV NektarAnchor UTXO ProofBundle without revealing plaintext.
type FragmentFieldProof struct {
	Shard          string                         `json:"shard"`
	Seq            int64                          `json:"seq"`
	Leaf           string                         `json:"leaf"`
	HDP            string                         `json:"hdp"`
	CiphertextHash string                         `json:"ciphertextHash"`
	ContentSHA256  string                         `json:"contentSha256,omitempty"`
	CommitHash     string                         `json:"commitHash,omitempty"`
	TransitionHash string                         `json:"transitionHash,omitempty"`
	FieldsRoot     string                         `json:"fieldsRoot"`
	Anchored       bool                           `json:"anchored"`
	Proof          *core.SelectiveDisclosureProof `json:"proof"`
	ProofBundle    *contracts.ProofBundle         `json:"proofBundle"`
}

// ProveFragmentFields builds an L1-bound MPE proof over fragment (shard, seq) metadata and discloses
// ONLY the requested fields, strictly requiring an anchored NektarAnchor UTXO ProofBundle.
func (s *Service) ProveFragmentFields(ctx context.Context, shard string, seq int64, fields []string) (*FragmentFieldProof, error) {
	return s.ProveFragmentFieldsAuthorized(ctx, shard, seq, fields, ownerPrincipal())
}

// ProveFragmentFieldsAuthorized enforces system-shard denial and Cedar read authorization before proving fields.
func (s *Service) ProveFragmentFieldsAuthorized(ctx context.Context, shard string, seq int64, fields []string, principal policy.Principal) (*FragmentFieldProof, error) {
	isOp := isOperatorPrincipal(principal)
	if err := ValidateShardName(shard, isOp); err != nil {
		return nil, err
	}
	f, err := s.db.GetFragment(shard, seq)
	if err != nil {
		return nil, err
	}
	if !isOp {
		if dec, aErr := s.AuthorizeAction(principal, "read", policy.Resource{
			Type:  "Fragment",
			ID:    fmt.Sprintf("%s#%d", f.Shard, f.Seq),
			Level: int64(f.Level),
			Shard: f.Shard,
			Class: f.Class,
		}); aErr != nil || !dec.Allow {
			if aErr != nil {
				return nil, fmt.Errorf("memory: cedar prove-fields policy error: %w", aErr)
			}
			return nil, fmt.Errorf("memory: cedar policy denied prove-fields on %s#%d", f.Shard, f.Seq)
		}
	}
	if f.Forgotten {
		return nil, errors.New("memory: cannot prove fields of a forgotten fragment")
	}
	if f.MPERoot == "" {
		return nil, errors.New("memory: fragment lacks an L1-committed MPE root")
	}
	if f.BatchN < 0 {
		return nil, errors.New("memory: fragment must be anchored in a NektarAnchor UTXO before proving fields")
	}
	prov, err := core.ParseProvenance(f.Provenance)
	if err != nil {
		return nil, fmt.Errorf("memory: parse provenance: %w", err)
	}
	dataHash := ""
	if f.ContentSHA256 != "" {
		dataHash = f.ContentSHA256
	} else if len(f.Data) > 0 {
		dataHash = hex.EncodeToString(core.SHA256(f.Data))
	} else if f.LatticeEpoch > 0 || f.BorgGroup != "" {
		if pt, ok := s.openFragmentPlaintext(ctx, f, principal, s.port.IdentityPubHex(), nil, false, false); ok {
			dataHash = hex.EncodeToString(core.SHA256(pt))
		}
	}

	ontID, edgesRoot, _ := s.db.GetFragmentGraphMeta(f.Shard, f.Seq)
	mpe, err := buildCommittedFragmentMPEWithGraph(f.Shard, f.HDP, prov, f.Level, f.Class, f.Path, f.MediaType, f.ChirpURL, dataHash, ontID, edgesRoot)
	if err != nil {
		return nil, err
	}
	if !strings.EqualFold(mpe.RootHex, f.MPERoot) {
		return nil, fmt.Errorf("memory: committed MPE root mismatch (%s != %s)", mpe.RootHex, f.MPERoot)
	}
	sdp, err := mpe.ProveFields(fields)
	if err != nil {
		return nil, err
	}
	pb, vr, pErr := s.Prove(ctx, shard, seq)
	if pErr != nil || pb == nil {
		return nil, fmt.Errorf("memory: anchor UTXO proof unavailable for %s#%d: %v", shard, seq, pErr)
	}
	if vr != nil && !vr.OK && vr.Reason != "Unsettled" && vr.Reason != "InsufficientDepth" && vr.Reason != "TrackerUnavailable" {
		return nil, fmt.Errorf("memory: anchor UTXO proof failed (%s: %s)", vr.Reason, vr.Detail)
	}
	return &FragmentFieldProof{
		Shard:          f.Shard,
		Seq:            f.Seq,
		Leaf:           f.Leaf,
		HDP:            f.HDP,
		CiphertextHash: hex.EncodeToString(core.ComputeEH(f.Ciphertext)),
		ContentSHA256:  f.ContentSHA256,
		CommitHash:     f.CommitHash,
		TransitionHash: f.TransitionHash,
		FieldsRoot:     mpe.RootHex,
		Anchored:       true,
		Proof:          sdp,
		ProofBundle:    pb,
	}, nil
}
