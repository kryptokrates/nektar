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

func (a *App) handleRepoExportCapsule(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	since := strings.TrimSpace(r.URL.Query().Get("since"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	res, _, raw, err := a.mem.ExportCapsuleAuthorized(r.Context(), repo, branch, since, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if r.URL.Query().Get("download") == "1" {
		shortHead := "empty"
		if len(res.HeadCommit) >= 8 {
			shortHead = res.HeadCommit[:8]
		}
		safeRepo := strings.ReplaceAll(repo, "/", "_")
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s-%s.nkt"`, safeRepo, shortHead))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(raw)
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoInspectCapsule(w http.ResponseWriter, r *http.Request) {
	ct := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Type")))
	var raw []byte
	targetRepo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if strings.Contains(ct, "application/octet-stream") {
		var err error
		raw, err = io.ReadAll(io.LimitReader(r.Body, memrepo.MaxCapsuleDecodedBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
	} else {
		var in struct {
			CapsuleBase64 string `json:"capsuleBase64,omitempty"`
			ChirpURL      string `json:"chirpUrl,omitempty"`
			TargetRepo    string `json:"targetRepo,omitempty"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, memrepo.MaxCapsuleDecodedBytes)).Decode(&in); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		if in.TargetRepo != "" {
			targetRepo = in.TargetRepo
		}
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
	}
	if len(raw) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "empty capsule payload"})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
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
	if strings.Contains(ct, "application/octet-stream") {
		raw, err := io.ReadAll(io.LimitReader(r.Body, memrepo.MaxCapsuleDecodedBytes))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		targetRepo := strings.TrimSpace(r.URL.Query().Get("repo"))
		targetBranch := strings.TrimSpace(r.URL.Query().Get("branch"))
		merge := r.URL.Query().Get("merge") != "0"
		res, err := a.mem.ImportCapsuleAuthorized(r.Context(), raw, "", targetRepo, targetBranch, merge, principalPtr)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		a.BroadcastDashboardEvent("repo_import")
		writeJSON(w, http.StatusOK, res)
		return
	}
	var in struct {
		ChirpURL      string `json:"chirpUrl,omitempty"`
		CapsuleBase64 string `json:"capsuleBase64,omitempty"`
		TargetRepo    string `json:"targetRepo,omitempty"`
		TargetBranch  string `json:"targetBranch,omitempty"`
		MergeIntoMain *bool  `json:"mergeIntoMain,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, memrepo.MaxCapsuleDecodedBytes)).Decode(&in); err != nil {
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
