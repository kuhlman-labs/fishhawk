package stub

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// GitHubHandler serves the GitHub REST subset the product calls against
// the stub's state. Behaviour is ported from splitParentGitHub
// (backend/internal/server/split_parent_close_test.go):
//
//	POST /app/installations/{id}/access_tokens   -> 201 {token, expires_at}
//	GET  /repos/{owner}/{name}                   -> {id, full_name, default_branch}
//	GET  /repos/{owner}/{name}/issues/{n}        -> {number, title, body, state, state_reason, labels}
//	PATCH /repos/{owner}/{name}/issues/{n}       -> state + state_reason written verbatim
//	GET  /repos/{owner}/{name}/issues/{n}/comments
//	POST /repos/{owner}/{name}/issues/{n}/comments
//	GET  /repos/{owner}/{name}/pulls/{n}         -> {node_id, state, merged, merged_at, merge_commit_sha, head, base}
//
// Every other path answers a JSON 404 and never panics: the board-sync
// reconciler's conventions fetch on the same issues.closed delivery hits a
// contents path the stub does not serve, and must simply fail its load.
func (f *Forge) GitHubHandler() http.Handler {
	return http.HandlerFunc(f.serveGitHub)
}

func (f *Forge) serveGitHub(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case len(parts) == 4 && parts[0] == "app" && parts[1] == "installations" && parts[3] == "access_tokens" && r.Method == http.MethodPost:
		f.mu.Lock()
		defer f.mu.Unlock()
		f.recordLocked(OpGitHubAccessToken, "github:installation/"+parts[2])
		if f.faultedLocked(OpGitHubAccessToken) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubAccessToken})
			return
		}
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":      InstallationToken,
			"expires_at": time.Now().UTC().Add(time.Hour).Format(time.RFC3339),
		})
		return
	case len(parts) >= 3 && parts[0] == "repos":
		f.serveGitHubRepo(w, r, parts[1]+"/"+parts[2], parts[3:])
		return
	}
	writeGitHubNotFound(w)
}

// serveGitHubRepo handles everything under /repos/{owner}/{name}; rest is
// the path after the repo segments.
func (f *Forge) serveGitHubRepo(w http.ResponseWriter, r *http.Request, repo string, rest []string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if len(rest) == 0 {
		if r.Method != http.MethodGet {
			writeGitHubNotFound(w)
			return
		}
		f.recordLocked(OpGitHubGetRepo, "github:"+repo)
		if f.faultedLocked(OpGitHubGetRepo) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubGetRepo})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"id": stableID(repo), "full_name": repo, "default_branch": "main",
		})
		return
	}
	if len(rest) < 2 {
		writeGitHubNotFound(w)
		return
	}
	num, err := strconv.Atoi(rest[1])
	if err != nil || num <= 0 {
		writeGitHubNotFound(w)
		return
	}
	key := recordKey(ForgeGitHub, repo, 0, num)

	switch {
	case rest[0] == "pulls" && len(rest) == 2 && r.Method == http.MethodGet:
		f.recordLocked(OpGitHubGetPull, key)
		if f.faultedLocked(OpGitHubGetPull) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubGetPull})
			return
		}
		pr, ok := f.pulls[key]
		if !ok {
			writeGitHubNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, githubPullJSON(pr))
	case rest[0] == "issues" && len(rest) == 2 && r.Method == http.MethodGet:
		f.recordLocked(OpGitHubGetIssue, key)
		if f.faultedLocked(OpGitHubGetIssue) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubGetIssue})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitHubNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, githubIssueJSON(is))
	case rest[0] == "issues" && len(rest) == 2 && r.Method == http.MethodPatch:
		f.recordLocked(OpGitHubPatchIssue, key)
		if f.faultedLocked(OpGitHubPatchIssue) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubPatchIssue})
			return
		}
		var body struct {
			State       *string `json:"state"`
			StateReason *string `json:"state_reason"`
			Title       *string `json:"title"`
			Body        *string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "stub: undecodable patch body: " + err.Error()})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitHubNotFound(w)
			return
		}
		// GitHub writes state and state_reason verbatim (unlike GitLab, which
		// moves state only through state_event — see gitlab.go).
		if body.State != nil {
			is.State = *body.State
		}
		if body.StateReason != nil {
			is.StateReason = *body.StateReason
		}
		if body.Title != nil {
			is.Title = *body.Title
		}
		if body.Body != nil {
			is.Body = *body.Body
		}
		writeJSON(w, http.StatusOK, githubIssueJSON(is))
	case rest[0] == "issues" && len(rest) == 3 && rest[2] == "comments" && r.Method == http.MethodGet:
		f.recordLocked(OpGitHubListComments, key)
		if f.faultedLocked(OpGitHubListComments) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubListComments})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitHubNotFound(w)
			return
		}
		out := []map[string]any{}
		for i, b := range is.Comments {
			out = append(out, map[string]any{
				"id": i + 1, "body": b, "created_at": "2026-08-01T12:00:00Z",
				"user": map[string]any{"login": "fishhawk"},
			})
		}
		writeJSON(w, http.StatusOK, out)
	case rest[0] == "issues" && len(rest) == 3 && rest[2] == "comments" && r.Method == http.MethodPost:
		f.recordLocked(OpGitHubPostComment, key)
		if f.faultedLocked(OpGitHubPostComment) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitHubPostComment})
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"message": "stub: undecodable comment body: " + err.Error()})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitHubNotFound(w)
			return
		}
		is.Comments = append(is.Comments, body.Body)
		writeJSON(w, http.StatusCreated, map[string]any{
			"id": len(is.Comments), "body": body.Body,
			"html_url": "https://github.com/" + repo + "/issues/" + strconv.Itoa(num) + "#issuecomment-" + strconv.Itoa(len(is.Comments)),
		})
	default:
		writeGitHubNotFound(w)
	}
}

func githubIssueJSON(is *Issue) map[string]any {
	return map[string]any{
		"number": is.Number, "title": is.Title, "body": is.Body,
		"state": is.State, "state_reason": is.StateReason, "labels": []string{},
	}
}

func githubPullJSON(pr *PullRequest) map[string]any {
	var mergedAt any
	if pr.MergedAt != nil {
		mergedAt = pr.MergedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"node_id":          "PR_stub_" + strings.ReplaceAll(pr.Repo, "/", "_") + "_" + strconv.Itoa(pr.Number),
		"number":           pr.Number,
		"title":            pr.Title,
		"body":             pr.Body,
		"state":            pr.State,
		"merged":           pr.Merged,
		"merged_at":        mergedAt,
		"merge_commit_sha": pr.MergeCommitSHA,
		"head":             map[string]any{"sha": pr.HeadSHA, "ref": pr.HeadRef},
		"base":             map[string]any{"ref": pr.BaseRef},
	}
}

func writeGitHubNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "Not Found"})
}

// writeJSON writes v as a JSON body with the given status.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// stableID derives a deterministic positive id from a name so a repeated
// read of the same repository or project reports the same id.
func stableID(name string) int {
	h := 0
	for _, c := range name {
		h = (h*31 + int(c)) % 1_000_000_007
	}
	return h%900_000 + 100_000
}
