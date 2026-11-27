package server

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/kryptokrates/nektar/internal/memory"
	"github.com/kryptokrates/nektar/internal/memrepo"
	"github.com/kryptokrates/nektar/internal/policy"
)

func (a *App) registerRepoRoutes(mux *http.ServeMux) {
	mux.Handle("GET /v1/repos", a.withAuth(http.HandlerFunc(a.handleRepoList)))
	mux.Handle("GET /v1/repos/{repo}", a.withAuth(http.HandlerFunc(a.handleRepoInspect)))
	mux.Handle("GET /v1/repos/{repo}/log", a.withAuth(http.HandlerFunc(a.handleRepoLog)))
	mux.Handle("GET /v1/repos/{repo}/tree", a.withAuth(http.HandlerFunc(a.handleRepoTree)))
	mux.Handle("GET /v1/repos/{repo}/diff", a.withAuth(http.HandlerFunc(a.handleRepoDiff)))
	mux.Handle("GET /v1/repos/{repo}/blame", a.withAuth(http.HandlerFunc(a.handleRepoBlame)))
	mux.Handle("POST /v1/repos/{repo}/commit", a.withAuth(http.HandlerFunc(a.handleRepoCommit)))
	mux.Handle("DELETE /v1/repos/{repo}/file", a.withAuth(http.HandlerFunc(a.handleRepoDeleteFile)))
	mux.Handle("POST /v1/repos/{repo}/branches", a.withOperatorAuth(http.HandlerFunc(a.handleRepoBranch)))
	mux.Handle("POST /v1/repos/{repo}/merge", a.withOperatorAuth(http.HandlerFunc(a.handleRepoMerge)))
	mux.Handle("POST /v1/repos/{repo}/revert", a.withOperatorAuth(http.HandlerFunc(a.handleRepoRevert)))
	mux.Handle("POST /v1/repos/{repo}/cherry-pick", a.withOperatorAuth(http.HandlerFunc(a.handleRepoCherryPick)))
	mux.Handle("POST /v1/repos/{repo}/fork", a.withOperatorAuth(http.HandlerFunc(a.handleRepoFork)))
	mux.Handle("GET /v1/repos/{repo}/tags", a.withAuth(http.HandlerFunc(a.handleRepoListTags)))
	mux.Handle("POST /v1/repos/{repo}/tags", a.withOperatorAuth(http.HandlerFunc(a.handleRepoCreateTag)))
	mux.Handle("POST /v1/repos/{repo}/prove-fields", a.withAuth(http.HandlerFunc(a.handleRepoProveFields)))
	mux.Handle("POST /v1/repos/{repo}/rotate", a.withOperatorAuth(http.HandlerFunc(a.handleRepoRotate)))
	mux.Handle("GET /v1/repos/{repo}/capsule", a.withOperatorAuth(http.HandlerFunc(a.handleRepoExportCapsule)))
	mux.Handle("POST /v1/repos/inspect-capsule", a.withAuth(http.HandlerFunc(a.handleRepoInspectCapsule)))
	mux.Handle("POST /v1/repos/import", a.withOperatorAuth(http.HandlerFunc(a.handleRepoImportCapsule)))
	mux.Handle("POST /v1/repos/{repo}/checkout", a.withOperatorAuth(http.HandlerFunc(a.handleRepoCheckout)))
}

func (a *App) handleRepoList(w http.ResponseWriter, r *http.Request) {
	cc := CallerContextFor(r.Context(), policy.Principal{})
	repos, err := a.mem.ListReposAuthorized(r.Context(), cc.Principal)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repos": repos})
}

func (a *App) handleRepoInspect(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	if branch == "" {
		branch = memrepo.DefaultBranch
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	sum, err := a.mem.GetRepoSummaryAuthorized(r.Context(), repo, cc.Principal)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	commits, _ := a.mem.RepoLogAuthorized(r.Context(), repo, branch, 50, cc.Principal)
	if len(commits) == 0 && branch == memrepo.DefaultBranch {
		commits, _ = a.mem.RepoLogAuthorized(r.Context(), repo, "", 50, cc.Principal)
	}
	_, tree, blobs, _ := a.mem.RepoTreeAuthorized(r.Context(), repo, branch, "", cc.Principal, cc.DisallowBorg)
	writeJSON(w, http.StatusOK, map[string]any{
		"summary": sum,
		"commits": commits,
		"tree":    tree,
		"blobs":   blobs,
	})
}

func (a *App) handleRepoLog(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	limit := 50
	if l := strings.TrimSpace(r.URL.Query().Get("limit")); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 500 {
			limit = n
		}
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	commits, err := a.mem.RepoLogAuthorized(r.Context(), repo, branch, limit, cc.Principal)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "branch": branch, "commits": commits})
}

func (a *App) handleRepoTree(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	commitHash := strings.TrimSpace(r.URL.Query().Get("commit"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	c, tree, blobs, err := a.mem.RepoTreeAuthorized(r.Context(), repo, branch, commitHash, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commit": c, "tree": tree, "blobs": blobs})
}

func (a *App) handleRepoDiff(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	fromC := strings.TrimSpace(r.URL.Query().Get("from"))
	toC := strings.TrimSpace(r.URL.Query().Get("to"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	diff, err := a.mem.RepoDiffAuthorized(r.Context(), repo, fromC, toC, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	patch, _ := a.mem.RepoDiffPatchAuthorized(r.Context(), repo, fromC, toC, cc.Principal, cc.DisallowBorg)
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "from": fromC, "to": toC, "changes": diff, "patch": patch})
}

func (a *App) handleRepoBlame(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	branch := strings.TrimSpace(r.URL.Query().Get("branch"))
	commitRef := strings.TrimSpace(r.URL.Query().Get("commit"))
	relPath := strings.TrimSpace(r.URL.Query().Get("path"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	lines, err := a.mem.RepoBlameAuthorized(r.Context(), repo, branch, commitRef, relPath, cc.Principal, cc.DisallowBorg)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "path": relPath, "lines": lines})
}

func (a *App) handleRepoCommit(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Branch        string `json:"branch,omitempty"`
		Path          string `json:"path,omitempty"`
		MediaType     string `json:"mediaType,omitempty"`
		Text          string `json:"text,omitempty"`
		BlobBase64    string `json:"blobBase64,omitempty"`
		ChirpURL      string `json:"chirpUrl,omitempty"`
		CommitMessage string `json:"commitMessage,omitempty"`
		BorgGroup     string `json:"borgGroup,omitempty"`
		Level         int    `json:"level,omitempty"`
		Class         string `json:"class,omitempty"`
		AutoAnchor    bool   `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 12<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	var blob []byte
	if strings.TrimSpace(in.BlobBase64) != "" {
		dec, err := base64.StdEncoding.DecodeString(strings.TrimSpace(in.BlobBase64))
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid blobBase64: " + err.Error()})
			return
		}
		blob = dec
	}
	remIn := memory.RememberInput{
		Shard:         repo,
		Branch:        in.Branch,
		Path:          in.Path,
		MediaType:     in.MediaType,
		Text:          in.Text,
		Blob:          blob,
		ChirpURL:      in.ChirpURL,
		CommitMessage: in.CommitMessage,
		BorgGroup:     in.BorgGroup,
		Level:         in.Level,
		Class:         in.Class,
		AutoAnchor:    in.AutoAnchor,
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	if cc.FromHTTP {
		p := cc.Principal
		remIn.Principal = &p
		remIn.CallerPub = ""
		if cc.DisallowBorg && remIn.BorgGroup != "" {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for Borg group writes"})
			return
		}
		if !cc.IsOperator && remIn.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	res, err := a.mem.Remember(r.Context(), remIn)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("remember")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoDeleteFile(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Branch     string `json:"branch,omitempty"`
		Path       string `json:"path"`
		Message    string `json:"message,omitempty"`
		AutoAnchor bool   `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 8192)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
		if !cc.IsOperator && in.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	c, err := a.mem.DeleteFile(r.Context(), repo, in.Branch, in.Path, in.Message, in.AutoAnchor, principalPtr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("remember")
	writeJSON(w, http.StatusOK, c)
}

func (a *App) handleRepoBranch(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Branch string `json:"branch"`
		From   string `json:"from,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}
	br, err := a.mem.CreateBranchAuthorized(r.Context(), repo, in.Branch, in.From, principalPtr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("repo_branch")
	writeJSON(w, http.StatusOK, br)
}

func (a *App) handleRepoMerge(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		SourceBranch string            `json:"sourceBranch"`
		TargetBranch string            `json:"targetBranch,omitempty"`
		Message      string            `json:"message,omitempty"`
		Strategy     string            `json:"strategy,omitempty"`
		Resolutions  map[string]string `json:"resolutions,omitempty"`
		AutoAnchor   bool              `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<20)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
		if !cc.IsOperator && in.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	res, err := a.mem.MergeBranchWithOptions(r.Context(), memory.MergeBranchInput{
		Repo:         repo,
		SourceBranch: in.SourceBranch,
		TargetBranch: in.TargetBranch,
		Message:      in.Message,
		Strategy:     in.Strategy,
		Resolutions:  in.Resolutions,
		AutoAnchor:   in.AutoAnchor,
		Principal:    principalPtr,
	})
	if err != nil {
		if errors.Is(err, memory.ErrMergeConflict) && res != nil {
			writeJSON(w, http.StatusConflict, map[string]any{
				"error":          err.Error(),
				"conflicts":      res.Conflicts,
				"baseCommitHash": res.BaseCommitHash,
				"autoMerged":     res.AutoMerged,
			})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("repo_merge")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoRevert(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Branch     string `json:"branch,omitempty"`
		Commit     string `json:"commit"`
		AutoAnchor bool   `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
		if !cc.IsOperator && in.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	res, err := a.mem.RevertCommit(r.Context(), repo, in.Branch, in.Commit, in.AutoAnchor, principalPtr)
	if err != nil {
		if errors.Is(err, memory.ErrMergeConflict) && res != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflicts": res.Conflicts})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("remember")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoCherryPick(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		TargetBranch string `json:"targetBranch,omitempty"`
		Commit       string `json:"commit"`
		AutoAnchor   bool   `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
		if !cc.IsOperator && in.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	res, err := a.mem.CherryPickCommit(r.Context(), repo, in.TargetBranch, in.Commit, in.AutoAnchor, principalPtr)
	if err != nil {
		if errors.Is(err, memory.ErrMergeConflict) && res != nil {
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "conflicts": res.Conflicts})
			return
		}
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("remember")
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleRepoFork(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Branch     string `json:"branch,omitempty"`
		NewRepo    string `json:"newRepo"`
		AutoAnchor bool   `json:"autoAnchor,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
		if !cc.IsOperator && in.AutoAnchor {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "operator token required for autoAnchor"})
			return
		}
	}
	sum, err := a.mem.ForkRepo(r.Context(), repo, in.Branch, in.NewRepo, in.AutoAnchor, principalPtr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("repo_branch")
	writeJSON(w, http.StatusOK, sum)
}

func (a *App) handleRepoListTags(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	cc := CallerContextFor(r.Context(), policy.Principal{})
	tags, err := a.mem.ListTagsAuthorized(repo, cc.Principal)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"repo": repo, "tags": tags})
}

func (a *App) handleRepoCreateTag(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimSpace(r.PathValue("repo"))
	var in struct {
		Tag     string `json:"tag"`
		Commit  string `json:"commit,omitempty"`
		Message string `json:"message,omitempty"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	cc := CallerContextFor(r.Context(), policy.Principal{})
	var principalPtr *policy.Principal
	if cc.FromHTTP {
		p := cc.Principal
		principalPtr = &p
	}
	tg, err := a.mem.CreateTagAuthorized(r.Context(), repo, in.Tag, in.Commit, in.Message, principalPtr)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	a.BroadcastDashboardEvent("repo_branch")
	writeJSON(w, http.StatusOK, tg)
}
