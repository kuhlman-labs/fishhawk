package stub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// gitlabDefaultPerPage mirrors GitLab's default page size when a client
// sends no per_page.
const gitlabDefaultPerPage = 20

// GitLabHandler serves the GitLab v4 REST subset the product calls against
// the stub's state. Behaviour is ported from splitParentGitLab
// (backend/internal/server/split_parent_close_test.go):
//
//	GET /api/v4/projects/{url-encoded path}          -> {id, path_with_namespace, web_url}
//	GET /api/v4/projects/{id}/issues/{iid}            -> {iid, title, description, state, labels, web_url}
//	PUT /api/v4/projects/{id}/issues/{iid}            -> state changes ONLY via state_event close|reopen
//	GET /api/v4/projects/{id}/issues/{iid}/notes      -> per_page/page paginated, RFC 8288 rel="next" Link
//	POST /api/v4/projects/{id}/issues/{iid}/notes
//	GET /api/v4/projects/{id}/merge_requests/{iid}    -> {iid, state, sha, merge_commit_sha, merged_at, ...}
//
// A PUT carrying a bare `state` field is IGNORED, as the real API ignores
// it: the edit-issue endpoint moves state only through state_event. The
// Link header is rooted at GitLabBaseURL because the real adapter follows
// a next link only when it targets its own scheme+host. Every other path
// answers a JSON 404 and never panics.
func (f *Forge) GitLabHandler() http.Handler {
	return http.HandlerFunc(f.serveGitLab)
}

func (f *Forge) serveGitLab(w http.ResponseWriter, r *http.Request) {
	// EscapedPath keeps a %2F-encoded namespaced project path as ONE
	// segment; URL.Path would already have decoded it into several.
	parts := strings.Split(strings.Trim(r.URL.EscapedPath(), "/"), "/")
	if len(parts) < 4 || parts[0] != "api" || parts[1] != "v4" || parts[2] != "projects" {
		writeGitLabNotFound(w)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	pid, err := strconv.Atoi(parts[3])
	if err != nil {
		// Not numeric: a namespaced path lookup.
		if len(parts) != 4 || r.Method != http.MethodGet {
			writeGitLabNotFound(w)
			return
		}
		path, uerr := url.PathUnescape(parts[3])
		if uerr != nil {
			writeGitLabNotFound(w)
			return
		}
		f.recordLocked(OpGitLabGetProject, "gitlab:"+path)
		if f.faultedLocked(OpGitLabGetProject) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabGetProject})
			return
		}
		id, ok := f.projects[path]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, gitlabProjectJSON(id, path))
		return
	}
	if len(parts) == 4 && r.Method == http.MethodGet {
		f.recordLocked(OpGitLabGetProject, "gitlab:"+strconv.Itoa(pid))
		if f.faultedLocked(OpGitLabGetProject) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabGetProject})
			return
		}
		path, ok := f.projectPathLocked(pid)
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, gitlabProjectJSON(pid, path))
		return
	}
	if len(parts) < 6 {
		writeGitLabNotFound(w)
		return
	}
	iid, err := strconv.Atoi(parts[5])
	if err != nil || iid <= 0 {
		writeGitLabNotFound(w)
		return
	}
	key := recordKey(ForgeGitLab, "", pid, iid)

	switch {
	case parts[4] == "merge_requests" && len(parts) == 6 && r.Method == http.MethodGet:
		f.recordLocked(OpGitLabGetMergeRequest, key)
		if f.faultedLocked(OpGitLabGetMergeRequest) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabGetMergeRequest})
			return
		}
		mr, ok := f.pulls[key]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, gitlabMergeRequestJSON(mr))
	case parts[4] == "issues" && len(parts) == 6 && r.Method == http.MethodGet:
		f.recordLocked(OpGitLabGetIssue, key)
		if f.faultedLocked(OpGitLabGetIssue) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabGetIssue})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		writeJSON(w, http.StatusOK, gitlabIssueJSON(is))
	case parts[4] == "issues" && len(parts) == 6 && r.Method == http.MethodPut:
		f.recordLocked(OpGitLabPutIssue, key)
		if f.faultedLocked(OpGitLabPutIssue) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabPutIssue})
			return
		}
		var body struct {
			StateEvent  string  `json:"state_event"`
			Title       *string `json:"title"`
			Description *string `json:"description"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "stub: undecodable put body: " + err.Error()})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		// The real API moves state ONLY through state_event; a bare `state`
		// field in the body is not decoded and so cannot change anything.
		switch body.StateEvent {
		case "close":
			is.State = "closed"
		case "reopen":
			is.State = "opened"
		case "":
			// no state change requested
		default:
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": fmt.Sprintf("stub: state_event %q is not close|reopen", body.StateEvent)})
			return
		}
		if body.Title != nil {
			is.Title = *body.Title
		}
		if body.Description != nil {
			is.Body = *body.Description
		}
		writeJSON(w, http.StatusOK, gitlabIssueJSON(is))
	case parts[4] == "issues" && len(parts) == 7 && parts[6] == "notes" && r.Method == http.MethodGet:
		f.recordLocked(OpGitLabListNotes, key)
		if f.faultedLocked(OpGitLabListNotes) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabListNotes})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		f.writeGitLabNotesPage(w, r, pid, iid, is.Comments)
	case parts[4] == "issues" && len(parts) == 7 && parts[6] == "notes" && r.Method == http.MethodPost:
		f.recordLocked(OpGitLabPostNote, key)
		if f.faultedLocked(OpGitLabPostNote) {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "stub fault: " + OpGitLabPostNote})
			return
		}
		var body struct {
			Body string `json:"body"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"message": "stub: undecodable note body: " + err.Error()})
			return
		}
		is, ok := f.issues[key]
		if !ok {
			writeGitLabNotFound(w)
			return
		}
		is.Comments = append(is.Comments, body.Body)
		writeJSON(w, http.StatusCreated, gitlabNoteJSON(len(is.Comments), body.Body))
	default:
		writeGitLabNotFound(w)
	}
}

// writeGitLabNotesPage answers one page of notes. The page size is the
// SetNotesPageSize override when set, else the request's per_page, else
// GitLab's default. When more notes remain it emits an RFC 8288
// rel="next" Link rooted at GitLabBaseURL, which the real adapter follows
// to exhaustion; a Link on any other host would be refused (the client
// never lets its PRIVATE-TOKEN leave the configured origin).
func (f *Forge) writeGitLabNotesPage(w http.ResponseWriter, r *http.Request, pid, iid int, all []string) {
	q := r.URL.Query()
	perPage := f.notesPageSize
	if perPage <= 0 {
		perPage, _ = strconv.Atoi(q.Get("per_page"))
	}
	if perPage <= 0 {
		perPage = gitlabDefaultPerPage
	}
	page, _ := strconv.Atoi(q.Get("page"))
	if page <= 0 {
		page = 1
	}
	from := (page - 1) * perPage
	if from > len(all) {
		from = len(all)
	}
	to := from + perPage
	if to > len(all) {
		to = len(all)
	}
	if to < len(all) {
		w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/projects/%d/issues/%d/notes?per_page=%d&page=%d>; rel="next"`,
			GitLabBaseURL, pid, iid, perPage, page+1))
	}
	out := []map[string]any{}
	for i := from; i < to; i++ {
		out = append(out, gitlabNoteJSON(i+1, all[i]))
	}
	writeJSON(w, http.StatusOK, out)
}

// projectPathLocked reverse-looks-up a registered namespaced path for a
// project id; ok is false when no path is registered for it.
func (f *Forge) projectPathLocked(pid int) (string, bool) {
	for path, id := range f.projects {
		if id == pid {
			return path, true
		}
	}
	return "", false
}

func gitlabProjectJSON(id int, path string) map[string]any {
	return map[string]any{
		"id": id, "path_with_namespace": path,
		"web_url": GitLabBaseURL + "/" + path, "default_branch": "main",
	}
}

func gitlabIssueJSON(is *Issue) map[string]any {
	return map[string]any{
		"iid": is.Number, "project_id": is.ProjectID, "title": is.Title, "description": is.Body,
		"state": is.State, "labels": []string{},
		"web_url": fmt.Sprintf("%s/%s/-/issues/%d", GitLabBaseURL, is.Repo, is.Number),
	}
}

func gitlabNoteJSON(id int, body string) map[string]any {
	return map[string]any{
		"id": id, "body": body, "system": false,
		"created_at": "2026-08-01T12:00:00Z",
		"author":     map[string]any{"username": "fishhawk"},
	}
}

func gitlabMergeRequestJSON(mr *PullRequest) map[string]any {
	var mergedAt any
	if mr.MergedAt != nil {
		mergedAt = mr.MergedAt.UTC().Format(time.RFC3339)
	}
	return map[string]any{
		"id": stableID(recordKey(ForgeGitLab, "", mr.ProjectID, mr.Number)), "iid": mr.Number, "project_id": mr.ProjectID,
		"title": mr.Title, "description": mr.Body, "state": mr.State,
		"source_branch": mr.HeadRef, "target_branch": mr.BaseRef, "sha": mr.HeadSHA,
		"merge_commit_sha": mr.MergeCommitSHA, "merged_at": mergedAt,
		"web_url": fmt.Sprintf("%s/%s/-/merge_requests/%d", GitLabBaseURL, mr.Repo, mr.Number),
	}
}

func writeGitLabNotFound(w http.ResponseWriter) {
	writeJSON(w, http.StatusNotFound, map[string]any{"message": "404 Not Found"})
}
