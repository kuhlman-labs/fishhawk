package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// requiredRunScopes is the run-driving subset of operatorDefaultScopes
// (the canonical operator token scope set, backend/cmd/fishhawkd/token.go)
// that a caller's token must carry to drive a repo's first run end to end:
// read the run + its audit chain, and write the run/approval/stage
// transitions the plan → implement → review loop performs. It deliberately
// EXCLUDES write:campaigns, write:deploy, and read:audit-export — those gate
// the campaign primitive, the deploy stage, and the bulk compliance-export
// surfaces (E9.5/#1608), none of which is exercised on a repo's first
// feature_change run, so requiring them here would over-report a scope
// gap for exactly the onboarding caller this endpoint serves. Keep this in
// lockstep with operatorDefaultScopes if the run-drive contract changes.
var requiredRunScopes = []string{
	"read:runs", "read:audit", "write:runs", "write:approvals", "write:stages",
}

// onboardingReadinessResponse aggregates the four server-side-only checks
// `fishhawk doctor` (E29.5) needs before a repo's first run: GitHub App
// installation, the committed workflow spec's parse/validate state, per
// reviewer availability on this deployment, and the caller token's scope
// adequacy. The checks cascade — a not-installed repo yields an unavailable
// spec and empty reviewers — each with an explanatory note.
type onboardingReadinessResponse struct {
	Repo      string              `json:"repo"`
	App       appInstallReadiness `json:"app"`
	Spec      specReadiness       `json:"spec"`
	Reviewers []reviewerReadiness `json:"reviewers"`
	Scopes    scopeReadiness      `json:"scopes"`
}

// appInstallReadiness reports whether the GitHub App is installed on the
// target repo. Reason carries the human-readable explanation when it is not
// (or when the client could not resolve the installation).
type appInstallReadiness struct {
	Installed      bool   `json:"installed"`
	InstallationID int64  `json:"installation_id,omitempty"`
	Reason         string `json:"reason,omitempty"`
}

// specReadiness reports the committed workflow spec's fetch + parse + validate
// state. Source is "fetched" when the spec was read from the repo, else
// "unavailable" (with Note explaining why). Valid is only meaningful when
// Source == "fetched"; Error carries the parse or validation failure.
type specReadiness struct {
	Source string `json:"source"`
	Valid  bool   `json:"valid"`
	Error  string `json:"error,omitempty"`
	Note   string `json:"note,omitempty"`
}

// reviewerReadiness reports one spec-declared reviewer's availability on this
// deployment. Available mirrors the ReviewerSet.For probe the run-create path
// performs; MissingHint carries the adapter's missing-env-var hint when the
// provider cannot be resolved.
type reviewerReadiness struct {
	Provider        string `json:"provider"`
	Model           string `json:"model,omitempty"`
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	Available       bool   `json:"available"`
	MissingHint     string `json:"missing_hint,omitempty"`
}

// scopeReadiness reports whether the caller token holds the run-driving scope
// subset. Missing lists the required scopes the caller lacks (empty when
// adequate). Cookie-session callers bypass scope enforcement and are adequate
// by construction, recorded in Note.
type scopeReadiness struct {
	Adequate bool     `json:"adequate"`
	Required []string `json:"required"`
	Missing  []string `json:"missing"`
	Note     string   `json:"note,omitempty"`
}

// handleGetOnboardingReadiness implements GET /v0/onboarding/readiness?repo=owner/name
// (E29.4). It aggregates the server-side-only readiness probes a first run
// needs, reusing the exact classification the run-create path performs. It is
// read-only and gates on AUTHENTICATION only (401 for anonymous) — not a write
// scope — because scope adequacy is itself a reported field: a write-scope gate
// would lock out precisely the callers who need to discover their gap. Mirrors
// /v0/auth/me.
func (s *Server) handleGetOnboardingReadiness(w http.ResponseWriter, r *http.Request) {
	ident := IdentityFrom(r.Context())
	if ident.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token or session is required", nil)
		return
	}

	repo := r.URL.Query().Get("repo")
	owner, name, ok := strings.Cut(repo, "/")
	// strings.Cut splits on the FIRST "/", so a value like "owner/name/extra"
	// would otherwise pass with name == "name/extra". Reject any residual
	// slash: the contract is exactly one owner/name pair.
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name format",
			map[string]any{"field": "repo", "got": repo})
		return
	}
	repoRef := githubclient.RepoRef{Owner: owner, Name: name}

	resp := onboardingReadinessResponse{
		Repo:      repo,
		Reviewers: []reviewerReadiness{},
	}

	// (1) GitHub App installation. Reuse the runs.go run-create switch:
	// nil → installed, ErrNotInstalled → not-installed with reason, any
	// other error → not-installed with the error as reason + a WARN. Never
	// 500 the whole endpoint on a transient installation-resolve error.
	var installationID int64
	if s.cfg.GitHub == nil {
		resp.App.Reason = "github client not configured on this deployment"
	} else {
		id, err := s.cfg.GitHub.GetRepoInstallation(r.Context(), repoRef)
		switch {
		case err == nil:
			resp.App.Installed = true
			resp.App.InstallationID = id
			installationID = id
		case errors.Is(err, githubclient.ErrNotInstalled):
			resp.App.Reason = "GitHub App is not installed on the target repository"
		default:
			resp.App.Reason = err.Error()
			s.cfg.Logger.Warn("onboarding readiness: resolve repo installation failed",
				"repo", repo, "error", err.Error())
		}
	}

	// (2) Workflow spec fetch + parse + validate. Only meaningful once the
	// App is installed (the fetch needs an installation token). Empty ref
	// resolves the repo's default branch, matching run-create (runs.go).
	var parsedSpec *spec.Spec
	switch {
	case !resp.App.Installed:
		resp.Spec.Source = "unavailable"
		resp.Spec.Note = "GitHub App is not installed on the target repository; cannot fetch the workflow spec"
	default:
		fc, err := s.cfg.GitHub.GetWorkflowSpec(r.Context(), installationID, repoRef, "")
		switch {
		case err == nil:
			resp.Spec.Source = "fetched"
			p, perr := spec.ParseBytes(fc.Content)
			switch {
			case perr != nil:
				resp.Spec.Valid = false
				resp.Spec.Error = perr.Error()
			default:
				if verr := spec.Validate(p); verr != nil {
					resp.Spec.Valid = false
					resp.Spec.Error = verr.Error()
				} else {
					resp.Spec.Valid = true
					parsedSpec = p
				}
			}
		case errors.Is(err, githubclient.ErrNotFound):
			resp.Spec.Source = "unavailable"
			resp.Spec.Note = "no workflow spec found on the repository's default branch"
		default:
			resp.Spec.Source = "unavailable"
			resp.Spec.Note = err.Error()
			s.cfg.Logger.Warn("onboarding readiness: fetch workflow spec failed",
				"repo", repo, "error", err.Error())
		}
	}

	// (3) Per-reviewer availability. Only when the spec parsed + validated
	// cleanly. Reuse the ReviewerSet.For probe unavailableSpecReviewers
	// performs and surface the adapter's missing-env-var hint verbatim.
	if parsedSpec != nil {
		for _, rv := range collectSpecReviewers(parsedSpec) {
			out := reviewerReadiness{
				Provider:        rv.Provider,
				Model:           rv.Model,
				ReasoningEffort: rv.ReasoningEffort,
			}
			if s.cfg.PlanReviewers == nil {
				out.MissingHint = "no reviewer backend is wired on this deployment; set FISHHAWKD_ANTHROPIC_API_KEY, FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER, or FISHHAWKD_ENABLE_CODEX_REVIEWER"
			} else if _, err := s.cfg.PlanReviewers.For(rv.Provider, rv.Model, rv.ReasoningEffort); err != nil {
				out.MissingHint = err.Error()
			} else {
				out.Available = true
			}
			resp.Reviewers = append(resp.Reviewers, out)
		}
	}

	// (4) Caller-token scope adequacy against the run-driving subset. Cookie
	// -session callers (TokenID == "") authenticate via OAuth, carry no
	// explicit scope list, and bypass scope enforcement (requireWriteScope),
	// so they are adequate by construction.
	resp.Scopes.Required = requiredRunScopes
	resp.Scopes.Missing = []string{}
	if ident.TokenID == "" {
		resp.Scopes.Adequate = true
		resp.Scopes.Note = "cookie-session caller: scope enforcement is bypassed for OAuth sessions"
	} else {
		for _, want := range requiredRunScopes {
			if !hasScope(ident, want) {
				resp.Scopes.Missing = append(resp.Scopes.Missing, want)
			}
		}
		resp.Scopes.Adequate = len(resp.Scopes.Missing) == 0
	}

	s.writeJSON(w, r, http.StatusOK, resp)
}

// collectSpecReviewers enumerates the distinct (provider, model,
// reasoning_effort) reviewer tuples declared across every stage's
// reviewers.agents list in the spec, de-duped by that composite key — the
// same tuple identity unavailableSpecReviewers (runs.go) probes. Results are
// sorted by the composite key so the readiness response is deterministic
// regardless of Go's map-iteration order over sp.Workflows.
func collectSpecReviewers(sp *spec.Spec) []spec.AgentReviewer {
	seen := make(map[string]struct{})
	var out []spec.AgentReviewer
	for _, wf := range sp.Workflows {
		for _, st := range wf.Stages {
			if st.Reviewers == nil {
				continue
			}
			for _, a := range st.Reviewers.Agents {
				key := a.Provider + "\x00" + a.Model + "\x00" + a.ReasoningEffort
				if _, ok := seen[key]; ok {
					continue
				}
				seen[key] = struct{}{}
				out = append(out, a)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Provider != out[j].Provider {
			return out[i].Provider < out[j].Provider
		}
		if out[i].Model != out[j].Model {
			return out[i].Model < out[j].Model
		}
		return out[i].ReasoningEffort < out[j].ReasoningEffort
	})
	return out
}
