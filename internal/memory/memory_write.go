package memory

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/core"
	"github.com/kryptokrates/nektar/internal/jevgraph"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/nmu"
	"github.com/kryptokrates/nektar/internal/policy"
	"github.com/kryptokrates/nektar/internal/reasoning"
	"github.com/kryptokrates/nektar/internal/store"
)

// RememberInput describes a fragment (text fact, semantic triples, or volumetric file/blob) to commit into a Memory Repo.
// Private-by-default: every non-system fragment is CLKA-encrypted at rest unless Public: true is explicitly set
// (or BorgGroup is set for multi-party group epoch encryption).
type RememberInput struct {
	Shard         string                `json:"shard"`
	Branch        string                `json:"branch,omitempty"`
	Path          string                `json:"path,omitempty"`
	MediaType     string                `json:"mediaType,omitempty"`
	Text          string                `json:"text"`
	Blob          []byte                `json:"blob,omitempty"`
	ChirpURL      string                `json:"chirpUrl,omitempty"`
	CommitMessage string                `json:"commitMessage,omitempty"`
	ExtraParents  []memrepo.ParentRef   `json:"extraParents,omitempty"`
	Source        string                `json:"source,omitempty"`
	Level         int                   `json:"level,omitempty"`
	Class         string                `json:"class,omitempty"`
	Public        bool                  `json:"public,omitempty"`    // explicit opt-out of CLKA encryption-at-rest
	BorgGroup     string                `json:"borgGroup,omitempty"` // if set, seals via BRC-369 keyed under group epoch
	Modality      *core.Modality        `json:"modality,omitempty"`
	Refines       []core.RefinementEdge `json:"refines,omitempty"`
	Triples       []reasoning.Triple    `json:"triples,omitempty"`
	CallerPub     string                `json:"callerPub,omitempty"`
	Principal     *policy.Principal     `json:"principal,omitempty"`
	Supersedes        int64                    `json:"supersedes,omitempty"`
	Tainted           bool                     `json:"tainted,omitempty"`
	Distilled         bool                     `json:"distilled,omitempty"` // true iff passed fail-closed PII/secret distillation gate
	AutoAnchor        bool                     `json:"autoAnchor,omitempty"`
	Extra             map[string]string        `json:"extra,omitempty"`
	Ontology          string                   `json:"ontology,omitempty"`       // domain ontology repo name (ont-<domain>) or 64-hex OntologyID
	OntologyStrict    bool                     `json:"ontologyStrict,omitempty"` // fail closed if Jev evaluator is offline
	EntityHints       []jevgraph.EntityEntry   `json:"entityHints,omitempty"`
	PreExtractedEdges []jevgraph.ExtractedEdge `json:"preExtractedEdges,omitempty"`
	DistilledEdges    []jevgraph.ExtractedEdge `json:"distilledEdges,omitempty"` // verbatim-span edges produced by SleepCycle distillation
	NMU               *nmu.Spec                `json:"nmu,omitempty"`            // optional NMU-1 commodity metadata
	FreshnessClass    string                   `json:"freshnessClass,omitempty"`
	ValidUntilUnix    int64                    `json:"validUntilUnix,omitempty"`
	SubjectVersion    string                   `json:"subjectVersion,omitempty"`
	UsageRights       string                   `json:"usageRights,omitempty"`
	GradeMismatch     bool                     `json:"gradeMismatch,omitempty"`   // true when buyer post-reveal verification flagged downgrade
	QuarantineEdges   bool                     `json:"quarantineEdges,omitempty"` // when true, skip indexing untrusted edges into knowledge_edges
}

// RememberResult summarizes a persisted, Git-committed, automaton-transitioned fragment.
type RememberResult struct {
	Shard            string                     `json:"shard"`
	Branch           string                     `json:"branch,omitempty"`
	Path             string                     `json:"path,omitempty"`
	Seq              int64                      `json:"seq"`
	HDP              string                     `json:"hdp"`
	Leaf             string                     `json:"leaf"`
	MPERoot          string                     `json:"mpeRoot,omitempty"`
	CommitHash       string                     `json:"commitHash,omitempty"`
	TreeHash         string                     `json:"treeHash,omitempty"`
	TransitionHash   string                     `json:"transitionHash,omitempty"`
	StateQ           string                     `json:"stateQ,omitempty"`
	ChirpURL         string                     `json:"chirpUrl,omitempty"`
	Encrypted        bool                       `json:"encrypted"`
	Public           bool                       `json:"public,omitempty"`
	Distilled        bool                       `json:"distilled,omitempty"`
	LatticeEpoch     uint64                     `json:"latticeEpoch,omitempty"`
	BorgGroup        string                     `json:"borgGroup,omitempty"`
	BorgEpoch        int64                      `json:"borgEpoch,omitempty"`
	Refines          []core.RefinementEdge      `json:"refines,omitempty"`
	Dedup            bool                       `json:"dedup"`
	AnchorTxID       string                     `json:"anchorTxid,omitempty"`
	ImpactedReceipts []string                   `json:"impactedReceipts,omitempty"`
	OntologyID       string                     `json:"ontologyId,omitempty"`
	EdgesRoot        string                     `json:"edgesRoot,omitempty"`
	Extraction       *jevgraph.ExtractionResult `json:"extraction,omitempty"`
	NMU              *nmu.Spec                  `json:"nmu,omitempty"`
}

// Remember commits a provenance-signed fragment (text, semantic triples, or volumetric blob) into a
// Memory Repo with private-by-default CLKA encryption, cryptographic Knowledge DAG refinement edges,
// an atomic Git/Metanet CommitObject + Merkle Automaton TransitionRecord + unified core.ComputeLeaf,
// and optional immediate BSV NektarAnchor.Append().
func (s *Service) Remember(ctx context.Context, in RememberInput) (*RememberResult, error) {
	s.TouchActivity()
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

	// Canonicalize any structured RDF/semantic triples into verifiable Mangle atoms appended to payload text.
	if len(in.Triples) > 0 {
		var atomLines []string
		for i, tr := range in.Triples {
			fin, tErr := reasoning.FinalizeTriple(tr)
			if tErr != nil {
				return nil, fmt.Errorf("memory: triple %d invalid: %w", i, tErr)
			}
			atoms, aErr := fin.ToMangleAtoms()
			if aErr != nil {
				return nil, fmt.Errorf("memory: triple %d mangle encoding: %w", i, aErr)
			}
			atomLines = append(atomLines, atoms...)
			if in.Modality == nil && fin.Modality != nil {
				cp := *fin.Modality
				in.Modality = &cp
			}
		}
		joined := strings.Join(atomLines, "\n")
		if strings.TrimSpace(in.Text) == "" {
			in.Text = joined
		} else {
			in.Text = strings.TrimRight(in.Text, "\n") + "\n" + joined
		}
	}

	if in.Text == "" && len(in.Blob) == 0 && strings.TrimSpace(in.ChirpURL) == "" {
		return nil, errors.New("memory: text, triples, blob, or chirpUrl is required")
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

	// Validate & resolve cryptographic Knowledge DAG refinement edges (auto-promoting Supersedes into a signed edge).
	rawRefines := append([]core.RefinementEdge(nil), in.Refines...)
	if in.Supersedes > 0 {
		hasSup := false
		for _, e := range rawRefines {
			if e.Kind == core.RefinementSupersedes && e.TargetSeq == in.Supersedes && (e.TargetShard == "" || e.TargetShard == in.Shard) {
				hasSup = true
				break
			}
		}
		if !hasSup {
			supEdge, sErr := s.ResolveSupersedesEdge(in.Shard, in.Supersedes)
			if sErr != nil {
				return nil, sErr
			}
			if supEdge != nil {
				rawRefines = append(rawRefines, *supEdge)
			}
		}
	}
	resolvedRefines, err := s.ResolveAndValidateRefinements(in.Shard, rawRefines)
	if err != nil {
		return nil, err
	}

	src := in.Source
	if src == "" {
		src = "agent"
	}
	class := in.Class
	if class == "" {
		class = "general"
	}
	useCLKA := !in.Public && in.BorgGroup == "" && !IsSystemShard(in.Shard)
	isEncrypted := in.BorgGroup != "" || useCLKA

	now := time.Now().Unix()
	prov := core.Provenance{
		Source:   src,
		Time:     now,
		ModelID:  s.modelID.Provider + "/" + s.modelID.Release,
		Modality: in.Modality,
		Refines:  resolvedRefines,
	}

	// Support both text facts and volumetric binary blobs (stored in BRC-167 CHIRP).
	payloadBytes := []byte(in.Text)
	var contentSHA []byte
	chirpURL := strings.TrimSpace(in.ChirpURL)
	mediaType := strings.TrimSpace(in.MediaType)

	if len(in.Blob) > 0 {
		sum := sha256.Sum256(in.Blob)
		contentSHA = sum[:]
		if !isEncrypted && chirpURL == "" {
			if u, _, _, cErr := bstack.StoreChirpWithHashes(s.objects, in.Blob); cErr == nil {
				chirpURL = u
			}
		}
		if isEncrypted {
			// For encrypted binary blobs (Borg or CLKA), HDP MUST commit to the exact plaintext bytes
			// sealed inside the envelope so Recall's VerifyHDP(opened, ...) succeeds cleanly.
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
		Public:        !isEncrypted,
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
	} else if useCLKA {
		// Private-by-default Cryptographic Lattice Key Assignment (CLKA): zero plaintext in SQLite/FTS/CHIRP!
		var contentID [32]byte
		copy(contentID[:], hdp)
		sealTarget := data
		if len(in.Blob) > 0 {
			sealTarget = in.Blob
		}
		env, contentKey, err := s.lattice.SealFragment(ctx, in.Shard, in.Level, class, contentID, sealTarget)
		core.Zero32Array(&contentKey)
		if err != nil {
			return nil, fmt.Errorf("memory: lattice seal fragment: %w", err)
		}
		metaBytes, err := json.Marshal(env)
		if err != nil {
			return nil, err
		}
		rec.Data = nil // zero plaintext at rest
		rec.Ciphertext, err = hex.DecodeString(env.Ciphertext)
		if err != nil {
			return nil, fmt.Errorf("memory: decode lattice ciphertext: %w", err)
		}
		if len(in.Blob) > 0 && chirpURL == "" && s.objects != nil {
			// Store ONLY encrypted ciphertext bytes in CHIRP for volumetric blobs (never plaintext).
			if u, _, _, cErr := bstack.StoreChirpWithHashes(s.objects, rec.Ciphertext); cErr == nil {
				chirpURL = u
				rec.ChirpURL = u
			}
		}
		rec.KeyedMeta = metaBytes
		rec.LatticeEpoch = env.Epoch
	}

	// Build L1 MPE commitment over fragment metadata (including final chirpURL and optional JevGraph commitment).
	var (
		ontologyID        string
		edgesRoot         string
		extractionRes     *jevgraph.ExtractionResult
		allowedPredicates map[string]struct{}
	)
	if strings.TrimSpace(in.Ontology) != "" {
		compiledOnt, _, oErr := s.LoadDomainOntology(ctx, in.Ontology)
		if oErr != nil {
			return nil, fmt.Errorf("memory: load domain ontology %q: %w", in.Ontology, oErr)
		}
		ontologyID = compiledOnt.ID
		allowedPredicates = compiledOnt.PredicateSet()
		existingTriples := s.activeShardTriples(in.Shard, ontologyID)

		if len(in.PreExtractedEdges) > 0 {
			res := &jevgraph.ExtractionResult{
				OntologyID:      ontologyID,
				OntologyName:    compiledOnt.Ontology.Name,
				ExtractorStatus: "pre_extracted",
				AdvisoryNote:    jevgraph.AdvisoryExtractionNote,
			}
			running := append([]reasoning.Triple(nil), existingTriples...)
			var accepted []jevgraph.ExtractedEdge
			for _, pe := range in.PreExtractedEdges {
				pe.OntologyID = ontologyID
				valEdge, vErr := jevgraph.ValidateEdgeAgainstOntology(compiledOnt, in.Text, pe, running, nil)
				if vErr != nil {
					pe.Status = jevgraph.EdgeRejected
					pe.Reason = vErr.Error()
					res.Edges = append(res.Edges, pe)
					res.RejectedCount++
					continue
				}
				valEdge.Status = jevgraph.EdgeAccepted
				valEdge.Reason = "accepted"
				res.Edges = append(res.Edges, valEdge)
				accepted = append(accepted, valEdge)
				res.AcceptedCount++
				running = append(running, valEdge.Triple)
			}
			er, rErr := jevgraph.ComputeEdgesRoot(accepted)
			if rErr != nil {
				return nil, fmt.Errorf("memory: compute edges root: %w", rErr)
			}
			res.EdgesRoot = er
			extractionRes = res
			edgesRoot = er
		} else if strings.TrimSpace(in.Text) != "" {
			eval := s.JevEvaluator()
			if eval != nil && in.BorgGroup != "" {
				// Borg-encrypted secrets never leave the node to an external matcher unless explicitly permitted by Cedar.
				if dec, aErr := s.AuthorizeAction(policy.JevMatcherPrincipal, "disclose", policy.Resource{
					Type:  "Fragment",
					ID:    in.Shard + "#extract",
					Level: int64(in.Level),
					Shard: in.Shard,
					Class: class,
				}); aErr != nil || !dec.Allow {
					eval = nil
				}
			}
			res, eErr := jevgraph.Extract(ctx, eval, compiledOnt, in.Text, existingTriples, jevgraph.ExtractOptions{
				Strict:      in.OntologyStrict,
				EntityHints: in.EntityHints,
			})
			if eErr != nil {
				return nil, fmt.Errorf("memory: ontology extraction: %w", eErr)
			}
			extractionRes = res
			edgesRoot = res.EdgesRoot
		}
	} else if len(in.DistilledEdges) > 0 {
		// Bridge SleepCycle distilled triples + verbatim spans into unified L1 MPE EdgesRoot & knowledge_edges.
		if in.Extra != nil && strings.TrimSpace(in.Extra["ontologyId"]) != "" {
			ontologyID = strings.ToLower(strings.TrimSpace(in.Extra["ontologyId"]))
		}
		var accepted []jevgraph.ExtractedEdge
		for _, de := range in.DistilledEdges {
			if ontologyID == "" && de.OntologyID != "" {
				ontologyID = strings.ToLower(strings.TrimSpace(de.OntologyID))
			}
			fin, tErr := reasoning.FinalizeTriple(de.Triple)
			if tErr != nil {
				continue
			}
			de.Triple = fin
			if de.OntologyID == "" {
				de.OntologyID = ontologyID
			}
			if de.Status == "" {
				de.Status = jevgraph.EdgeAccepted
			}
			if de.Status == jevgraph.EdgeAccepted {
				accepted = append(accepted, de)
			}
		}
		if len(accepted) > 0 {
			er, rErr := jevgraph.ComputeEdgesRoot(accepted)
			if rErr != nil {
				return nil, fmt.Errorf("memory: compute distilled edges root: %w", rErr)
			}
			edgesRoot = er
			extractionRes = &jevgraph.ExtractionResult{
				OntologyID:      ontologyID,
				ExtractorStatus: "distilled",
				Edges:           accepted,
				AcceptedCount:   len(accepted),
				EdgesRoot:       er,
				AdvisoryNote:    jevgraph.AdvisoryExtractionNote,
			}
		}
	}

	dataHashHex := hex.EncodeToString(core.SHA256(data))
	if len(contentSHA) == 32 {
		dataHashHex = hex.EncodeToString(contentSHA)
	}
	mpe, mErr := buildCommittedFragmentMPEWithGraph(in.Shard, hdpHex, prov, in.Level, class, normPath, mediaType, chirpURL, dataHashHex, ontologyID, edgesRoot)
	if mErr != nil || mpe == nil {
		return nil, fmt.Errorf("memory: build L1 MPE commitment: %w", mErr)
	}
	mpeRootBytes := mpe.Root
	rec.MPERoot = mpe.RootHex
	rec.Distilled = in.Distilled
	if ontologyID != "" {
		rec.OntologyID = ontologyID
	} else if in.Extra != nil && strings.TrimSpace(in.Extra["ontologyId"]) != "" {
		rec.OntologyID = strings.ToLower(strings.TrimSpace(in.Extra["ontologyId"]))
	}
	if len(in.Triples) > 0 {
		if rawTriples, tErr := json.Marshal(in.Triples); tErr == nil {
			rec.TriplesJSON = rawTriples
		}
	} else if extractionRes != nil && len(extractionRes.Edges) > 0 && !in.QuarantineEdges {
		var acceptedTriples []reasoning.Triple
		for _, e := range extractionRes.Edges {
			if e.Status == jevgraph.EdgeAccepted {
				acceptedTriples = append(acceptedTriples, e.Triple)
			}
		}
		if len(acceptedTriples) > 0 {
			if rawTriples, tErr := json.Marshal(acceptedTriples); tErr == nil {
				rec.TriplesJSON = rawTriples
			}
		}
	}

	// Resolve optional NMU-1 commodity metadata (validate enums, compute verified triple count & local grade).
	var computedNMU *nmu.Spec
	nmuInput := in.NMU
	if nmuInput == nil && (strings.TrimSpace(in.FreshnessClass) != "" || in.ValidUntilUnix > 0 || strings.TrimSpace(in.SubjectVersion) != "" || strings.TrimSpace(in.UsageRights) != "" || in.GradeMismatch) {
		nmuInput = &nmu.Spec{
			Version:        nmu.SpecVersion,
			FreshnessClass: nmu.NormalizeFreshness(in.FreshnessClass),
			ValidUntilUnix: in.ValidUntilUnix,
			SubjectVersion: strings.TrimSpace(in.SubjectVersion),
			UsageRights:    nmu.NormalizeRights(in.UsageRights),
		}
	}
	if nmuInput != nil {
		specCopy := *nmuInput
		if specCopy.FreshnessClass == "" && strings.TrimSpace(in.FreshnessClass) != "" {
			specCopy.FreshnessClass = nmu.NormalizeFreshness(in.FreshnessClass)
		}
		if specCopy.ValidUntilUnix == 0 && in.ValidUntilUnix > 0 {
			specCopy.ValidUntilUnix = in.ValidUntilUnix
		}
		if specCopy.SubjectVersion == "" && strings.TrimSpace(in.SubjectVersion) != "" {
			specCopy.SubjectVersion = strings.TrimSpace(in.SubjectVersion)
		}
		if specCopy.UsageRights == "" && strings.TrimSpace(in.UsageRights) != "" {
			specCopy.UsageRights = nmu.NormalizeRights(in.UsageRights)
		}
		if specCopy.FreshnessClass != "" && !nmu.ValidFreshness(nmu.NormalizeFreshness(string(specCopy.FreshnessClass))) {
			return nil, fmt.Errorf("memory: invalid nmu freshnessClass %q", specCopy.FreshnessClass)
		}
		if specCopy.UsageRights != "" && !nmu.ValidRights(nmu.NormalizeRights(string(specCopy.UsageRights))) {
			return nil, fmt.Errorf("memory: invalid nmu usageRights %q", specCopy.UsageRights)
		}
		if len(specCopy.SubjectVersion) > nmu.MaxSubjectVersionLen {
			return nil, fmt.Errorf("memory: nmu subjectVersion exceeds %d bytes", nmu.MaxSubjectVersionLen)
		}
		specCopy.Version = nmu.SpecVersion
		specCopy.UsageRights = nmu.EffectiveRights(specCopy.UsageRights)
		specCopy.AdvisoryNote = nmu.AdvisoryNMUNote
		computedNMU = &specCopy
	}
	if !IsSystemShard(in.Shard) && (computedNMU != nil || in.Distilled || extractionRes != nil || len(in.Triples) > 0) {
		if computedNMU == nil {
			computedNMU = &nmu.Spec{
				Version:      nmu.SpecVersion,
				UsageRights:  nmu.RightsInferenceOnly,
				AdvisoryNote: nmu.AdvisoryNMUNote,
			}
		}
		var allTriples []reasoning.Triple
		if len(in.Triples) > 0 {
			allTriples = append(allTriples, in.Triples...)
		}
		if extractionRes != nil {
			for _, e := range extractionRes.Edges {
				if e.Status == jevgraph.EdgeAccepted {
					allTriples = append(allTriples, e.Triple)
				}
			}
		}
		if allowedPredicates == nil && rec.OntologyID != "" {
			allowedPredicates = s.ResolveOntologyPredicateSet(ctx, rec.OntologyID)
		}
		if computedNMU.Grade == "" || computedNMU.UnitCount == 0 {
			cls := nmu.ClassifyFragmentGrade(nmu.FragmentGradeInput{
				HasCleanText:           strings.TrimSpace(in.Text) != "" || len(data) > 0,
				Distilled:              in.Distilled || extractionRes != nil,
				OntologyID:             rec.OntologyID,
				Triples:                allTriples,
				AllowedPredicates:      allowedPredicates,
				MangleConsistent:       !in.GradeMismatch,
				EdgesRoot:              edgesRoot,
				MPEHasOntologyAndEdges: rec.OntologyID != "" && edgesRoot != "",
			})
			if computedNMU.Grade == "" {
				computedNMU.Grade = cls.Grade
			}
			if computedNMU.UnitCount == 0 {
				computedNMU.UnitCount = cls.UnitCount
			}
		}
	}

	// Prepare repo genesis & commit metadata BEFORE the atomic SQLite transaction.
	var (
		preBuiltBlob *memrepo.BlobObject
		commitMsg    string
		genPub       string
		genID        string
	)
	if !IsSystemShard(in.Shard) {
		if br, bErr := s.db.GetMemBranch(in.Shard, "main"); bErr == nil && br.GenesisPub != "" && br.GenesisShardID != "" {
			genPub, genID = br.GenesisPub, br.GenesisShardID
		} else {
			anchors, _ := s.db.ListAnchors(in.Shard)
			if gp, gid, gErr := s.resolveRepoGenesis(ctx, in.Shard, anchors); gErr == nil {
				genPub, genID = gp, gid
			}
		}
		commitMsg = strings.TrimSpace(in.CommitMessage)
		if normPath != "" {
			b, bErr := s.buildFragmentBlob(normPath, mediaType, data, in.Blob, chirpURL, rec.ContentSHA256, rec.BorgGroup, rec.BorgEpoch, rec.LatticeEpoch, rec.KeyedMeta, in.Level, class)
			if bErr != nil {
				return nil, fmt.Errorf("memory: build repo blob: %w", bErr)
			}
			preBuiltBlob = b
		}
	}

	policyHashHex := ""
	if pc := s.PolicyCommit(); pc != nil {
		policyHashHex = pc.Hash
	}
	ontologyHashHex := s.OntologyHead()
	authorPubHex := s.commitAuthorPubHex(ctx)

	var (
		stored         store.FragmentRecord
		dedup          bool
		commitHash     string
		treeHash       string
		transitionHash string
		stateQHex      string
		finalPath      string
	)

	unlockShard := s.lockShard(in.Shard)
	txErr := s.db.RunRepoTx(func(rtx *store.RepoTx) error {
		// 1. Check idempotency / dedup before minting any CommitObject or TransitionRecord.
		if existing, gErr := rtx.GetFragmentByHDP(in.Shard, hdpHex); gErr == nil {
			stored = existing
			dedup = true
			commitHash = existing.CommitHash
			transitionHash = existing.TransitionHash
			finalPath = existing.Path
			return nil
		} else if !errors.Is(gErr, sql.ErrNoRows) {
			return gErr
		}

		nextSeq, sErr := rtx.NextFragmentSeq(in.Shard)
		if sErr != nil {
			return sErr
		}
		prevQHex, qErr := rtx.LatestStateQ(in.Shard)
		if qErr != nil {
			return qErr
		}
		prevQBytes := core.Zero32Bytes()
		if prevQHex != "" {
			if b, dErr := hex.DecodeString(prevQHex); dErr == nil && len(b) == 32 {
				prevQBytes = b
			}
		}

		if IsSystemShard(in.Shard) {
			leaf := core.ComputeLeaf(core.Fragment{
				Data:          data,
				Provenance:    prov,
				HDP:           hdp,
				Ciphertext:    rec.Ciphertext,
				Path:          normPath,
				MediaType:     mediaType,
				ChirpURL:      chirpURL,
				ContentSHA256: contentSHA,
				MPERoot:       mpeRootBytes,
			})
			rec.Leaf = core.ToHex(leaf)
			var aErr error
			stored, dedup, aErr = rtx.AppendFragment(rec)
			finalPath = stored.Path
			return aErr
		}

		// 2. Unified Epistemic Write (collapses split-brain):
		//    Blob -> Tree -> Signed CommitObject -> TransitionRecord (prevQ -> nextQ) -> unified core.ComputeLeaf -> Persist ALL atomically!
		relPath := normPath
		if relPath == "" {
			relPath = fmt.Sprintf("facts/%04d.md", nextSeq)
		}
		finalPath = relPath

		blobObj := preBuiltBlob
		if blobObj == nil {
			var bErr error
			blobObj, bErr = s.buildFragmentBlob(relPath, mediaType, data, in.Blob, chirpURL, rec.ContentSHA256, rec.BorgGroup, rec.BorgEpoch, rec.LatticeEpoch, rec.KeyedMeta, rec.Level, rec.Class)
			if bErr != nil {
				return fmt.Errorf("memory: build repo blob: %w", bErr)
			}
		}

		msg := commitMsg
		if msg == "" {
			switch {
			case rec.BorgGroup != "":
				msg = fmt.Sprintf("borg: encrypted commit (%s)", relPath)
			case rec.LatticeEpoch > 0:
				msg = fmt.Sprintf("lattice: encrypted commit (%s)", relPath)
			default:
				msg = summarizeCommitText(in.Text, relPath)
			}
		}

		var extraBlobs []*memrepo.BlobObject
		// Only write plaintext graph sidecar blobs into the Git tree for explicitly Public fragments;
		// encrypted (BorgGroup or private-by-default CLKA) fragments NEVER leak plaintext into mem_objects/capsules.
		if extractionRes != nil && !isEncrypted {
			sidecar := jevgraph.BuildSidecar(relPath, data, extractionRes)
			sidecarBytes, scErr := core.CanonicalJSON(sidecar)
			if scErr != nil {
				return fmt.Errorf("memory: marshal graph sidecar: %w", scErr)
			}
			sidecarPath := "graph/" + relPath + ".edges.json"
			sb, _, sbErr := memrepo.BuildBlobWithMeta(s.objects, sidecarPath, "application/json", sidecarBytes, false, rec.Level, rec.Class)
			if sbErr != nil {
				return fmt.Errorf("memory: build graph sidecar blob: %w", sbErr)
			}
			extraBlobs = append(extraBlobs, sb)
		}

		rec.BlobHash = blobObj.BlobHash
		cObj, expectedHead, cErr := s.prepareFragmentCommitWithExtrasTx(ctx, rtx, in.Shard, nextSeq, now, blobObj, extraBlobs, branch, msg, in.ExtraParents, authorPubHex, policyHashHex, ontologyHashHex)
		if cErr != nil {
			return cErr
		}
		commitHash = cObj.CommitHash
		treeHash = cObj.TreeHash

		// 3. Build deterministic CBOR Merkle Automaton TransitionRecord (q_i -> q_{i+1}).
		nextTrSeq, trSeqErr := rtx.NextTransitionSeq(in.Shard)
		if trSeqErr != nil {
			return trSeqErr
		}
		commitHashBytes, _ := hex.DecodeString(commitHash)
		treeHashBytes, _ := hex.DecodeString(treeHash)
		var polBytes, ontBytes []byte
		if policyHashHex != "" {
			polBytes, _ = hex.DecodeString(policyHashHex)
		}
		if ontologyHashHex != "" {
			ontBytes, _ = hex.DecodeString(ontologyHashHex)
		}
		nextQBytes, qnErr := core.NextStepStateQ(prevQBytes, hdp, commitHashBytes, treeHashBytes, polBytes, ontBytes, now)
		if qnErr != nil {
			return fmt.Errorf("memory: compute next stateQ: %w", qnErr)
		}
		trRec := core.TransitionRecord{
			Version:      1,
			Shard:        in.Shard,
			Seq:          nextTrSeq,
			PrevStateQ:   prevQBytes,
			InputDigest:  hdp,
			NextStateQ:   nextQBytes,
			CommitHash:   commitHashBytes,
			TreeHash:     treeHashBytes,
			PolicyHash:   polBytes,
			OntologyHash: ontBytes,
			Timestamp:    now,
		}
		trHashBytes, trErr := core.ComputeTransitionHash(trRec)
		if trErr != nil {
			return fmt.Errorf("memory: compute transition hash: %w", trErr)
		}
		trRec.Hash = trHashBytes
		trCBOR, trErr := core.EncodeTransitionCBOR(trRec)
		if trErr != nil {
			return fmt.Errorf("memory: encode transition CBOR: %w", trErr)
		}
		transitionHash = core.ToHex(trHashBytes)
		stateQHex = core.ToHex(nextQBytes)

		// 4. Compute unified leaf committing (HDP, H(E), MPERoot, ContentSHA256, CommitHash, TransitionHash).
		leaf := core.ComputeLeaf(core.Fragment{
			Data:           data,
			Provenance:     prov,
			HDP:            hdp,
			Ciphertext:     rec.Ciphertext,
			Path:           normPath,
			MediaType:      mediaType,
			ChirpURL:       chirpURL,
			ContentSHA256:  contentSHA,
			MPERoot:        mpeRootBytes,
			CommitHash:     commitHashBytes,
			TransitionHash: trHashBytes,
		})
		rec.Leaf = core.ToHex(leaf)
		rec.CommitHash = commitHash
		rec.TransitionHash = transitionHash
		cObj.FragmentLeaf = rec.Leaf

		// 5. Persist CommitObject, Branch CAS, TransitionRow, and FragmentRecord atomically.
		if err := rtx.SaveMemCommit(rowFromCommit(cObj)); err != nil {
			return err
		}
		if err := rtx.CompareAndSwapMemBranch(store.MemBranchRow{
			Repo:           cObj.Repo,
			Branch:         cObj.Branch,
			HeadCommit:     cObj.CommitHash,
			GenesisPub:     genPub,
			GenesisShardID: genID,
			UpdatedAt:      cObj.Timestamp,
		}, expectedHead); err != nil {
			return err
		}
		if err := rtx.SaveTransition(store.TransitionRow{
			Shard:        in.Shard,
			Seq:          nextTrSeq,
			Hash:         transitionHash,
			PrevQ:        core.ToHex(prevQBytes),
			NextQ:        stateQHex,
			CommitHash:   commitHash,
			TreeHash:     treeHash,
			PolicyHash:   policyHashHex,
			OntologyHash: ontologyHashHex,
			BodyCBOR:     trCBOR,
			CreatedAt:    now,
		}); err != nil {
			return err
		}
		var aErr error
		stored, dedup, aErr = rtx.AppendFragment(rec)
		if aErr != nil {
			return aErr
		}
		if !dedup && ontologyID != "" {
			if mErr := rtx.SaveFragmentGraphMeta(stored.Shard, stored.Seq, ontologyID, edgesRoot); mErr != nil {
				return mErr
			}
			// Never persist plaintext extracted triples into SQLite for Borg-group secrets OR quarantined mismatched purchases!
			if extractionRes != nil && in.BorgGroup == "" && !in.QuarantineEdges && len(extractionRes.Edges) > 0 {
				rows := make([]store.KnowledgeEdgeRow, 0, len(extractionRes.Edges))
				for _, e := range extractionRes.Edges {
					rows = append(rows, edgeToStoreRow(stored.Shard, stored.Seq, now, e))
				}
				if kErr := rtx.SaveKnowledgeEdges(rows); kErr != nil {
					return kErr
				}
			}
		}
		if !dedup && computedNMU != nil {
			if nErr := rtx.SaveFragmentNMUMeta(store.FragmentNMUMeta{
				Shard:          stored.Shard,
				Seq:            stored.Seq,
				Grade:          string(computedNMU.Grade),
				UnitCount:      computedNMU.UnitCount,
				FreshnessClass: string(computedNMU.FreshnessClass),
				ValidUntilUnix: computedNMU.ValidUntilUnix,
				SubjectVersion: computedNMU.SubjectVersion,
				UsageRights:    string(nmu.EffectiveRights(computedNMU.UsageRights)),
				GradeMismatch:  in.GradeMismatch,
				UpdatedAt:      now,
			}); nErr != nil {
				return nErr
			}
		}
		return nil
	})
	unlockShard()
	if txErr != nil {
		return nil, txErr
	}

	var impacted []string
	if !dedup {
		// Notify OnRevision, EventBus, AND Grade S live SSE OnSupersede hooks for legacy Supersedes & RefinementSupersedes edges.
		s.mu.RLock()
		supHook := s.onSupersede
		bus := s.bus
		s.mu.RUnlock()
		if bus != nil && !IsSystemShard(in.Shard) {
			bus.Publish("shard:"+in.Shard, "fragment_committed", map[string]any{
				"shard":     stored.Shard,
				"seq":       stored.Seq,
				"leaf":      stored.Leaf,
				"hdp":       stored.HDP,
				"distilled": stored.Distilled,
			})
		}
		fireSup := func(targetShard string, targetSeq int64, reason string) {
			if IsSystemShard(targetShard) || (supHook == nil && bus == nil) {
				return
			}
			ev := SupersessionEvent{
				Shard:         targetShard,
				NewSeq:        stored.Seq,
				NewLeaf:       stored.Leaf,
				NewHDP:        stored.HDP,
				SupersededSeq: targetSeq,
				OntologyID:    rec.OntologyID,
				EdgesRoot:     edgesRoot,
				Reason:        reason,
				Timestamp:     now,
			}
			if bus != nil {
				bus.Publish("shard:"+targetShard, "fragment_superseded", ev)
			}
			if supHook != nil {
				s.hookWG.Add(1)
				go func(e SupersessionEvent) {
					defer s.hookWG.Done()
					supHook(e)
				}(ev)
			}
		}
		seenSup := make(map[string]bool)
		if in.Supersedes > 0 {
			seenSup[fmt.Sprintf("%s#%d", in.Shard, in.Supersedes)] = true
			if imp, err := s.OnRevision(ctx, in.Shard, in.Supersedes); err == nil {
				impacted = append(impacted, imp...)
			}
			fireSup(in.Shard, in.Supersedes, "superseded")
		}
		for _, e := range resolvedRefines {
			if e.Kind == core.RefinementSupersedes && e.TargetSeq > 0 {
				k := fmt.Sprintf("%s#%d", e.TargetShard, e.TargetSeq)
				if !seenSup[k] {
					seenSup[k] = true
					if imp, err := s.OnRevision(ctx, e.TargetShard, e.TargetSeq); err == nil {
						impacted = append(impacted, imp...)
					}
					fireSup(e.TargetShard, e.TargetSeq, e.Reason)
				}
			}
		}
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
		TransitionHash:   transitionHash,
		StateQ:           stateQHex,
		ChirpURL:         stored.ChirpURL,
		Encrypted:        stored.BorgGroup != "" || stored.LatticeEpoch > 0,
		Public:           stored.Public,
		Distilled:        stored.Distilled,
		LatticeEpoch:     stored.LatticeEpoch,
		BorgGroup:        stored.BorgGroup,
		BorgEpoch:        stored.BorgEpoch,
		Refines:          resolvedRefines,
		Dedup:            dedup,
		AnchorTxID:       anchorTxID,
		ImpactedReceipts: impacted,
		OntologyID:       rec.OntologyID,
		EdgesRoot:        edgesRoot,
		Extraction:       extractionRes,
		NMU:              computedNMU,
	}, nil
}
