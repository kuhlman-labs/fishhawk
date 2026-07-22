package server

import (
	"errors"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
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
		fc, err := s.cfg.GitHub.GetWorkflowSpec(r.Context(), forge.FromGitHubInstallationID(installationID), repoRef, "")
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

// regionPinDeps carries the two dependencies the cell-side region-pin handler
// needs: the RegionPinner (the accounts store plus THIS cell's home-region
// tag) and the secret shared with the global directory.
//
// SCOPE NOTE (E44.7 / #1831): these two belong on server.Config as plain
// fields, alongside AccountRoles. Config is declared in server.go, which this
// decomposed slice does not own — the scope amendment requesting it went
// undecided past the stage cap — so they are carried in a per-instance side
// table declared here instead. The registration API and the handler's reads
// are written so collapsing this into two Config fields is a mechanical
// refactor with no behavior change: replace ConfigureRegionPin with the
// fields and regionPin() with a struct read.
type regionPinDeps struct {
	pinner *account.RegionPinner
	secret []byte
}

// regionPinRegistry maps *Server → regionPinDeps. Keyed PER INSTANCE (not a
// package-level singleton) so concurrent tests and any multi-server process
// cannot see each other's wiring. An unregistered server reads the zero
// value, which makes the endpoint fail closed with 503.
var regionPinRegistry sync.Map

// ConfigureRegionPin wires the cell-side region-pin dependencies onto this
// server: the account store bound to this cell's home region, and the handoff
// secret shared with the global directory. Calling it with a nil pinner or an
// empty secret leaves the endpoint failing closed, which is the correct
// posture for a deployment that is not part of a regional topology.
func (s *Server) ConfigureRegionPin(pinner *account.RegionPinner, handoffSecret []byte) {
	regionPinRegistry.Store(s, regionPinDeps{pinner: pinner, secret: handoffSecret})
}

// regionPin reads this server's region-pin dependencies, returning the zero
// value (both absent) when none were wired.
func (s *Server) regionPin() regionPinDeps {
	v, ok := regionPinRegistry.Load(s)
	if !ok {
		return regionPinDeps{}
	}
	deps, _ := v.(regionPinDeps)
	return deps
}

// regionPinResponse is the cell's acknowledgement of a directory-issued region
// pin: the forge-neutral account identity, the region now recorded on
// accounts.home_region, and the account row id. It is deliberately the same
// shape whether the pin created the row or re-confirmed an existing one: the
// first-write-wins bound makes a repeated pin idempotent, so a replay is
// indistinguishable from the original by design.
type regionPinResponse struct {
	Provider   string `json:"provider"`
	AccountKey string `json:"account_key"`
	HomeRegion string `json:"home_region"`
	AccountID  string `json:"account_id"`
}

// handleOnboardingRegionPin implements GET /v0/onboarding/region-pin (ADR-062,
// E44.7 / #1831) — the cell side of directory-first onboarding.
//
// Onboarding is directory-first: the GLOBAL directory decides an account's home
// region, records it, and 302s the caller into that region's cell with a signed
// handoff appended to the original request URL. This handler is where that
// redirect lands. It never derives a region itself, and it never writes back to
// the directory — the data flow is strictly directory → cell.
//
// It is deliberately UNAUTHENTICATED in the session sense: it is reached by a
// browser redirect during onboarding, before any session exists. The HMAC over
// the handoff parameters IS the authentication, and it is checked first. Three
// independent gates must all pass before anything is persisted:
//
//  1. handoff.Verify — the pin was issued by the directory holding the shared
//     secret and has not expired (rejects absent, unsigned, forged, tampered,
//     and expired pins).
//  2. the RESIDENCY invariant — the pinned region equals THIS cell's own
//     configured home region, so an EU pin landing on a US cell fails closed
//     (account.ErrRegionForeign, 421 Misdirected Request).
//  3. the REPLAY bound — home_region is first-write-wins, so a replayed pin is
//     idempotent and can never move an account between regions
//     (account.ErrRegionConflict, 409).
//
// Method is GET by construction: a 302 must be followable by a browser, and the
// routed onboarding surfaces carry no request body.
func (s *Server) handleOnboardingRegionPin(w http.ResponseWriter, r *http.Request) {
	deps := s.regionPin()
	if deps.pinner == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "region_pin_unavailable",
			"this deployment has no account store wired; region pins cannot be recorded", nil)
		return
	}

	pin, err := handoff.Verify(r.URL.Query(), string(deps.secret), time.Now())
	if err != nil {
		status, code := http.StatusForbidden, "region_pin_rejected"
		switch {
		case errors.Is(err, handoff.ErrNoSecret):
			// No shared secret on this cell: no pin can be authenticated, so
			// none is accepted. Fail closed, never trust the parameters.
			status, code = http.StatusServiceUnavailable, "region_pin_unavailable"
		case errors.Is(err, handoff.ErrMissing), errors.Is(err, handoff.ErrMalformed):
			status, code = http.StatusBadRequest, "validation_failed"
		}
		s.cfg.Logger.Warn("onboarding region pin rejected", "error", err.Error(), "code", code)
		s.writeError(w, r, status, code, err.Error(), nil)
		return
	}

	acct, err := deps.pinner.Pin(r.Context(), account.PinParams{
		Provider:   pin.Provider,
		AccountKey: pin.AccountKey,
		Region:     pin.HomeRegion,
	})
	if err != nil {
		switch {
		case errors.Is(err, account.ErrRegionForeign):
			// 421 Misdirected Request (RFC 9110 §15.5.20): this request was
			// routed to a cell that cannot serve it. Exactly the residency
			// fault, and it tells the caller to re-resolve, not to retry.
			s.writeError(w, r, http.StatusMisdirectedRequest, "region_pin_misdirected", err.Error(),
				map[string]any{"cell_home_region": deps.pinner.HomeRegion(), "pin_home_region": pin.HomeRegion})
		case errors.Is(err, account.ErrRegionConflict):
			s.writeError(w, r, http.StatusConflict, "region_pin_conflict", err.Error(), nil)
		case errors.Is(err, account.ErrRegionUnsupported):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
				map[string]any{"field": handoff.ParamHomeRegion, "supported": account.SupportedRegions})
		case errors.Is(err, account.ErrRegionUnavailable):
			s.writeError(w, r, http.StatusServiceUnavailable, "region_pin_unavailable", err.Error(), nil)
		default:
			s.cfg.Logger.Error("onboarding region pin: persist failed",
				"provider", pin.Provider, "account_key", pin.AccountKey, "error", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"failed to record the account's home region", nil)
		}
		return
	}

	region := ""
	if acct.HomeRegion != nil {
		region = *acct.HomeRegion
	}
	s.writeJSON(w, r, http.StatusOK, regionPinResponse{
		Provider:   acct.Provider,
		AccountKey: acct.AccountKey,
		HomeRegion: region,
		AccountID:  acct.ID.String(),
	})
}
