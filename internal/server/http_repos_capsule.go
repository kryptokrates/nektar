package server

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/kryptokrates/nektar/internal/bstack"
	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

func (a *App) handleRepoProveFields(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Seq    int64    `json:"seq,omitempty"`
		Commit string   `json:"commit,omitempty"`
		Fields []string `json:"fields"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	seq := in.Seq
	if seq <= 0 && strings.TrimSpace(in.Commit) != "" {
		c, err := a.mem.ResolveCommitRefAuthorized(r.Context(), repo, in.Commit, cc.Principal)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		seq = c.Seq
	}
	if seq <= 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "seq or commit is required"})
		return
	}
	if len(in.Fields) == 0 {
		in.Fields = []string{"source", "modelId", "time", "path"}
	}
	fp, err := a.mem.ProveFragmentFieldsAuthorized(r.Context(), repo, seq, in.Fields, cc.Principal)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fp)
}

func (a *App) handleRepoRotate(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		NewOwner string `json:"newOwner"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	updated, err := a.mem.RotateShardOwner(r.Context(), repo, in.NewOwner)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("anchor")
	writeJSON(w, http.StatusOK, updated)
}

const capsuleContentType = "application/vnd.nektar.capsule+cbor"

func (a *App) handleRepoExportCapsule(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	accept := strings.ToLower(strings.TrimSpace(r.Header.Get("Accept")))
	if r.URL.Query().Get("download") == "1" || strings.Contains(accept, capsuleContentType) {
		selfContained := r.URL.Query().Get("selfContained") != "0"
		safeRepo := strings.ReplaceAll(repo, "/", "_")
		w.Header().Set("Content-Type", capsuleContentType)
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.nkt"`, safeRepo))
		w.WriteHeader(http.StatusOK)
		p := cc.Principal
		_, _ = a.mem.ExportCapsuleStream(r.Context(), w, repo, branch, since, &memory.CapsuleExportOpts{
			SelfContained: selfContained,
			SignExport:    true,
			Principal:     &p,
			DisallowBorg:  cc.DisallowBorg,
		})
		return
	}
	res, _, _, err := a.mem.ExportCapsuleAuthorized(r.Context(), repo, branch, since, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func isBinaryCapsuleContentType(ct string) bool {
	return strings.Contains(ct, capsuleContentType) || strings.Contains(ct, "application/octet-stream")
}

func (a *App) handleRepoInspectCapsule(w http.ResponseWriter, r *http.Request) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	targetRepo := strings.TrimSpace(r.URL.Query().Get("repo"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	if isBinaryCapsuleContentType(ct) {
		r.Body = http.MaxBytesReader(w, r.Body, memrepo.MaxCapsuleDecodedBytes)
		res, err := a.mem.InspectCapsuleStream(r.Context(), r.Body, targetRepo, cc.Principal, cc.DisallowBorg, nil)
		if err != nil {
			var maxErr *http.MaxBytesError
			if strings.Contains(err.Error(), "http: request body too large") || errorAs(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "capsule payload exceeds maximum allowed size"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, memrepo.MaxCapsuleDecodedBytes)
	var in struct {
		CapsuleBase64 string `json:"capsuleBase64,omitempty"`
		ChirpURL      string `json:"chirpUrl,omitempty"`
		TargetRepo    string `json:"targetRepo,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if strings.Contains(err.Error(), "http: request body too large") || errorAs(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "capsule payload exceeds maximum allowed size"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if in.TargetRepo != "" {
		targetRepo = in.TargetRepo
	}
	var raw []byte
	if strings.TrimSpace(in.CapsuleBase64) != "" {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.CapsuleBase64))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid capsuleBase64: " + err.Error()})
			return
		}
		raw = dec
	} else if strings.TrimSpace(in.ChirpURL) != "" {
		resolved, err := bstack.ResolveChirp(a.mem.Objects(), strings.TrimSpace(in.ChirpURL))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "resolve chirp: " + err.Error()})
			return
		}
		raw = resolved
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty capsule payload"})
		return
	}
	res, err := a.mem.InspectCapsuleAuthorized(r.Context(), raw, targetRepo, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoImportCapsule(w http.ResponseWriter, r *http.Request) {
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	if isBinaryCapsuleContentType(ct) {
		r.Body = http.MaxBytesReader(w, r.Body, memrepo.MaxCapsuleDecodedBytes)
		targetRepo := strings.TrimSpace(r.URL.Query().Get("repo"))
		targetBranch := strings.TrimSpace(r.URL.Query().Get("branch"))
		merge := r.URL.Query().Get("merge") != "0"
		res, err := a.mem.ImportCapsuleStream(r.Context(), r.Body, targetRepo, targetBranch, merge, principalPtr, nil)
		if err != nil {
			var maxErr *http.MaxBytesError
			if strings.Contains(err.Error(), "http: request body too large") || errorAs(err, &maxErr) {
				writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "capsule payload exceeds maximum allowed size"})
				return
			}
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("repo_import")
		writeJSON(w, http.StatusOK, res)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, memrepo.MaxCapsuleDecodedBytes)
	var in struct {
		ChirpURL      string `json:"chirpUrl,omitempty"`
		CapsuleBase64 string `json:"capsuleBase64,omitempty"`
		TargetRepo    string `json:"targetRepo,omitempty"`
		TargetBranch  string `json:"targetBranch,omitempty"`
		MergeIntoMain *bool  `json:"mergeIntoMain,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		var maxErr *http.MaxBytesError
		if strings.Contains(err.Error(), "http: request body too large") || errorAs(err, &maxErr) {
			writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "capsule payload exceeds maximum allowed size"})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var raw []byte
	if strings.TrimSpace(in.CapsuleBase64) != "" {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.CapsuleBase64))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid capsuleBase64: " + err.Error()})
			return
		}
		raw = dec
	}
	merge := true
	if in.MergeIntoMain != nil {
		merge = *in.MergeIntoMain
	}
	res, err := a.mem.ImportCapsuleAuthorized(r.Context(), raw, in.ChirpURL, in.TargetRepo, in.TargetBranch, merge, principalPtr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("repo_import")
	writeJSON(w, http.StatusOK, res)
}

func errorAs[T any](err error, target *T) bool {
	if err == nil {
		return false
	}
	type unwrapper interface{ Unwrap() error }
	for cur := err; cur != nil; {
		if v, ok := cur.(T); ok {
			*target = v
			return true
		}
		u, ok := cur.(unwrapper)
		if !ok {
			return false
		}
		cur = u.Unwrap()
	}
	return false
}

func (a *App) handleRepoCheckout(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	if err := memory.ValidateShardName(repo, false); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var in struct {
		Branch string `json:"branch,omitempty"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in)
	baseDir := strings.TrimSpace(a.cfg.DataDir)
	if baseDir == "" {
		if home, err := os.UserHomeDir(); err == nil {
			baseDir = filepath.Join(home, ".nektar")
		} else {
			baseDir = "."
		}
	}
	safeRepo := strings.ReplaceAll(repo, "/", "_")
	destDir := filepath.Join(baseDir, "repos", safeRepo)
	res, err := a.mem.CheckoutGitWorkspace(r.Context(), repo, in.Branch, destDir)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}
