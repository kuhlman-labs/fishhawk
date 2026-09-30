package server

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationconfirm"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// GET /v0/repos/{owner}/{name}/delegation (E76.1 / #3747) answers "what may
// the crew decide on its own in this repository?" from the workflow spec at a
// ref, WITHOUT a run.
//
// It is READ-ONLY in the strongest sense: it writes nothing, mints NO audit
// entry, and grants no authority. The projection is the DECLARATION, not an
// evaluation — see backend/internal/delegationview/README.md for the two
// contracts a reader must not misread (the matrix is the workflow-level one;
// ceiling_matrix is what a ceiling WOULD produce).
//
// TWO EXPLICIT SPEC SOURCES, never a silent switch:
//
//	source=ref        (default) fetch .fishhawk/workflows.yaml through the
//	                  forge at ?ref=; an empty ref serves the repository's
//	                  DEFAULT BRANCH head, which is the issue's stated default.
//	source=run_cache  project the spec cached on the repo's NEWEST run —
//	                  exactly the source GET /v0/repos/{owner}/{name}/posture
//	                  already reads.
//
// The resolved source is ALWAYS echoed on the response. An unconfigured forge
// under source=ref is a named 503 whose message points at run_cache; it is
// never a silent fallback, because a caller reading a delegation posture must
// know which spec they were shown.
//
// Long-form contract: backend/internal/server/README.md § "Delegation read".

// The two accepted `source` values.
const (
	delegationSourceRef      = "ref"
	delegationSourceRunCache = "run_cache"
)

// delegationResponse is the GET body: the pure view, plus — when the
// delegation-confirmation store is wired (E76.5 / #3768) — a per-workflow
// `confirmation` block and a view-level `confirmation` summary. The
// projection types are the wire types (delegationview), embedded so a
// json-tag change there is a deliberate API change rather than an accident
// of two mirrors. The outer Workflows field shadows View.Workflows by
// encoding/json's shallowest-field rule; View.Workflows is left empty.
type delegationResponse struct {
	delegationview.View
	Workflows    []delegationWorkflowResponse   `json:"workflows"`
	Confirmation *delegationConfirmationSummary `json:"confirmation,omitempty"`
}

// delegationWorkflowResponse is one workflow entry with its confirmation
// verdict. Confirmation is ABSENT (never a false confirmed) when the store is
// not wired or could not be read.
type delegationWorkflowResponse struct {
	delegationview.WorkflowDelegation
	Confirmation *delegationconfirm.WorkflowStatus `json:"confirmation,omitempty"`
}

// delegationConfirmationSummary is the view-level confirmation block:
// the captain in force and the retained workflows still awaiting
// confirmation (never-confirmed ones included). Unavailable carries the
// read failure instead when the chain could not be read.
type delegationConfirmationSummary struct {
	Captain              *string  `json:"captain"`
	SeatSequence         int64    `json:"seat_sequence"`
	UnconfirmedWorkflows []string `json:"unconfirmed_workflows"`
	Unavailable          string   `json:"unavailable,omitempty"`
}

// handleGetRepoDelegation implements GET /v0/repos/{owner}/{name}/delegation.
//
// SEVEN refusal modes, each with its own status AND named code: unknown source
// (400 validation_failed), forge unconfigured under source=ref (503
// github_unconfigured), spec not found (404 workflow_spec_not_found — three
// branches: no cached run, a forge 404, and a 200 whose spec content is EMPTY),
// a forge FAULT rather than a not-found on either forge call (502
// forge_unavailable), spec invalid (422 workflow_spec_invalid), unknown
// ?workflow (404 workflow_not_found), repo not visible (403 repo_forbidden,
// from the shared prelude).
func (s *Server) handleGetRepoDelegation(w http.ResponseWriter, r *http.Request) {
	// The SAME gate the four repo-dashboard rollups use: the run-repo 503, then
	// requestRepoFilter + repoVisibleOr403 — the point-read DENY convention.
	repo, _, ok := s.repoDashPrelude(w, r, false, false)
	if !ok {
		return
	}
	q := r.URL.Query()
	source := q.Get("source")
	switch source {
	case "":
		source = delegationSourceRef
	case delegationSourceRef, delegationSourceRunCache:
	default:
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("source must be %q or %q", delegationSourceRef, delegationSourceRunCache),
			map[string]any{"field": "source", "got": source,
				"accepted": []string{delegationSourceRef, delegationSourceRunCache}})
		return
	}
	ref := q.Get("ref")

	var specBytes []byte
	var workflowSHA string
	if source == delegationSourceRef {
		specBytes, workflowSHA, ok = s.delegationSpecFromRef(w, r, repo, ref)
	} else {
		specBytes, workflowSHA, ok = s.delegationSpecFromRunCache(w, r, repo)
	}
	if !ok {
		return
	}

	out := delegationview.View{
		Repo: repo, Source: source, Ref: ref, WorkflowSHA: workflowSHA,
		Workflows: []delegationview.WorkflowDelegation{},
	}
	// The declared version is read from the raw YAML independently of
	// validation — exactly as projectPosture reads it — so an INVALID spec can
	// still report which version it claimed.
	var raw map[string]any
	if yaml.Unmarshal(specBytes, &raw) == nil {
		if v, ok := raw["version"]; ok && v != nil {
			out.SpecVersion = fmt.Sprint(v)
		}
	}
	out.SchemaMajor = spec.VersionMajor(out.SpecVersion)

	parsed, err := spec.ParseBytes(specBytes)
	if err != nil {
		// FAIL CLOSED, and the `return` is the control: a partial matrix is
		// never emitted. A spec that decodes into a workflow map but fails
		// validation would project happily, so falling through here would
		// serve a 200 with a workflows array for a spec the backend refuses.
		s.writeError(w, r, http.StatusUnprocessableEntity, "workflow_spec_invalid",
			"the workflow spec at this source does not validate; no delegation matrix is projected for an invalid spec",
			map[string]any{"repo": repo, "source": source, "error": err.Error()})
		return
	}

	out.Workflows = delegationview.Project(parsed)
	// The per-workflow hashes are stamped by Project, BEFORE the filter, so a
	// filtered read reports the same per-workflow hash an unfiltered one does.
	// Only the view-level hash reflects the retained set.
	if id := q.Get("workflow"); id != "" {
		kept := make([]delegationview.WorkflowDelegation, 0, 1)
		for _, wf := range out.Workflows {
			if wf.ID == id {
				kept = append(kept, wf)
			}
		}
		if len(kept) == 0 {
			s.writeError(w, r, http.StatusNotFound, "workflow_not_found",
				fmt.Sprintf("the spec at this source declares no workflow %q", id),
				map[string]any{"field": "workflow", "got": id, "repo": repo, "source": source})
			return
		}
		out.Workflows = kept
	}
	out.ContentHash = delegationview.HashWorkflows(out.Workflows)
	s.writeJSON(w, r, http.StatusOK, s.withDelegationConfirmation(r, repo, out))
}

// withDelegationConfirmation wraps the view with the confirmation blocks,
// computed AFTER the ?workflow filter and after every hash is stamped, so the
// verdict's hash half compares against the hashes this very response reports.
// A nil store leaves every block absent; a read failure reports
// confirmation.unavailable and leaves the per-workflow blocks absent.
func (s *Server) withDelegationConfirmation(r *http.Request, repo string, v delegationview.View) delegationResponse {
	resp := delegationResponse{Workflows: make([]delegationWorkflowResponse, 0, len(v.Workflows))}
	for _, wf := range v.Workflows {
		resp.Workflows = append(resp.Workflows, delegationWorkflowResponse{WorkflowDelegation: wf})
	}
	ws := v.Workflows
	v.Workflows = nil
	resp.View = v
	if s.cfg.DelegationConfirmStore == nil {
		return resp
	}
	snap, err := s.cfg.DelegationConfirmStore.Read(r.Context(), identityAccountID(r.Context()), repo)
	if err != nil {
		// A fixed reason, not err.Error(): the block rides a 200 body, so a raw
		// chain-read error (which can carry connection/host/DSN detail) must not
		// reach a repo-visible caller — the captain surface's posture (captain.go
		// captainDelegationUnconfirmed).
		resp.Confirmation = &delegationConfirmationSummary{UnconfirmedWorkflows: []string{}, Unavailable: captainDelegationChainReadFailed}
		return resp
	}
	statuses := delegationStatuses(snap.State, ws)
	for i := range resp.Workflows {
		st := statuses[i]
		resp.Workflows[i].Confirmation = &st
	}
	resp.Confirmation = &delegationConfirmationSummary{
		Captain: captainPtr(snap.State), SeatSequence: snap.State.SeatSequence,
		UnconfirmedWorkflows: delegationconfirm.Unconfirmed(statuses),
	}
	return resp
}

// delegationSpecFromRef fetches the spec through the forge Contents API.
//
// An empty ref serves the repository's DEFAULT BRANCH head — githubclient's
// documented ref="" behaviour, relied on today by handleGetOnboardingReadiness
// and campaign_admission_screen.go — which is this endpoint's "base branch
// head" default.
func (s *Server) delegationSpecFromRef(w http.ResponseWriter, r *http.Request, repo, ref string) ([]byte, string, bool) {
	// FAIL CLOSED on an unconfigured forge, naming the alternative. Never a
	// silent fall-through to the cached spec: the caller asked which spec they
	// were shown by choosing a source, and answering with the other one would
	// make the echo a lie.
	if s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "github_unconfigured",
			fmt.Sprintf("source=%q requires a configured GitHub App; retry with source=%q to project the spec cached on this repository's newest run",
				delegationSourceRef, delegationSourceRunCache),
			map[string]any{"field": "source", "got": delegationSourceRef})
		return nil, "", false
	}
	owner, name := r.PathValue("owner"), r.PathValue("name")
	repoRef := forge.RepoRef{Owner: owner, Name: name}
	ctx := r.Context()
	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, repoRef)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "workflow_spec_not_found",
				"no GitHub App installation is visible for this repository",
				map[string]any{"repo": repo})
			return nil, "", false
		}
		s.writeError(w, r, http.StatusBadGateway, "forge_unavailable",
			"resolving the repository installation failed",
			map[string]any{"repo": repo, "error": err.Error()})
		return nil, "", false
	}
	file, err := s.cfg.GitHub.GetWorkflowSpec(ctx, forge.FromGitHubInstallationID(instID), repoRef, ref)
	if err != nil {
		if errors.Is(err, forge.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "workflow_spec_not_found",
				fmt.Sprintf("%s is not present at this ref", githubclient.WorkflowSpecPath),
				map[string]any{"repo": repo, "ref": ref})
			return nil, "", false
		}
		s.writeError(w, r, http.StatusBadGateway, "forge_unavailable",
			"fetching the workflow spec failed",
			map[string]any{"repo": repo, "ref": ref, "error": err.Error()})
		return nil, "", false
	}
	if len(bytes.TrimSpace(file.Content)) == 0 {
		s.writeError(w, r, http.StatusNotFound, "workflow_spec_not_found",
			fmt.Sprintf("%s is empty at this ref", githubclient.WorkflowSpecPath),
			map[string]any{"repo": repo, "ref": ref})
		return nil, "", false
	}
	return file.Content, file.SHA, true
}

// delegationSpecFromRunCache projects the spec cached on the repo's NEWEST run
// — the projectPosture ladder, same account narrowing.
func (s *Server) delegationSpecFromRunCache(w http.ResponseWriter, r *http.Request, repo string) ([]byte, string, bool) {
	rows, err := s.cfg.RunRepo.ListRuns(r.Context(), run.ListRunsFilter{
		Repo: repo, AccountID: IdentityFrom(r.Context()).AccountID, Limit: 1,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list runs failed", map[string]any{"error": err.Error()})
		return nil, "", false
	}
	if len(rows) == 0 || len(bytes.TrimSpace(rows[0].WorkflowSpec)) == 0 {
		s.writeError(w, r, http.StatusNotFound, "workflow_spec_not_found",
			fmt.Sprintf("no run of this repository carries a cached workflow spec; retry with source=%q to read it from the forge",
				delegationSourceRef),
			map[string]any{"repo": repo, "source": delegationSourceRunCache})
		return nil, "", false
	}
	return rows[0].WorkflowSpec, rows[0].WorkflowSHA, true
}
