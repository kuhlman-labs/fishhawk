package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergegate"
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

// onboardingReadinessResponse aggregates the five server-side-only checks
// `fishhawk doctor` (E29.5) needs before a repo's first run: GitHub App
// installation, the committed workflow spec's parse/validate state, per
// reviewer availability on this deployment, the caller token's scope
// adequacy, and — since #3161 — whether the `fishhawk_audit_complete` check
// Fishhawk publishes is actually REQUIRED by the repo's branch protection.
// The checks cascade — a not-installed repo yields an unavailable spec, empty
// reviewers and an `unknown` merge gate — each with an explanatory note.
type onboardingReadinessResponse struct {
	Repo      string              `json:"repo"`
	App       appInstallReadiness `json:"app"`
	Spec      specReadiness       `json:"spec"`
	Reviewers []reviewerReadiness `json:"reviewers"`
	Scopes    scopeReadiness      `json:"scopes"`
	MergeGate mergeGateReadiness  `json:"merge_gate"`
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

// mergeGateReadiness reports whether the `fishhawk_audit_complete` Check Run
// Fishhawk PUBLISHES is actually REQUIRED by the repo's protection
// configuration on its default branch (#3161), reconciled by
// backend/internal/mergegate.
//
// The invariant this field exists to carry: Status is `not_required` ONLY on a
// fully authoritative read of BOTH protection surfaces. Every degrade — no
// GitHub client, App not installed, default-branch lookup failure, a 403 from
// a missing `administration: read`, a rulesets endpoint that 404s, a ref_name
// token the v0 matcher cannot evaluate, a transport error or a probe timeout —
// resolves to `unknown` with a naming Reason. A surface that was never read
// must never render as "this repo requires nothing".
//
// It is REPORTING only: it gates no run, no merge and no exit code, and it is
// a point-in-time read — a ruleset edited after the probe is not reflected.
// Long-form contract: backend/internal/server/README.md.
type mergeGateReadiness struct {
	// Status is "required", "not_required" or "unknown".
	Status string `json:"status"`
	// Check is the context name that was probed.
	Check string `json:"check"`
	// Branch is the repo's REAL default branch, the branch the probe
	// evaluated. Empty when the probe never got far enough to resolve it.
	Branch string `json:"branch,omitempty"`
	// Sources are the protection surfaces observed requiring Check, each
	// with its OWN bypass posture. Never aggregated across sources: each
	// source enforces independently, so a merger must bypass every one of
	// them.
	Sources []mergeGateSource `json:"sources,omitempty"`
	// Bypassable is the conjunction over Sources — true only when EVERY
	// requiring source is individually bypassable. False when Sources is
	// empty.
	Bypassable bool `json:"bypassable"`
	// Authoritative is true only when both protection surfaces answered
	// definitively for the probed branch.
	Authoritative bool `json:"authoritative"`
	// Reason is a machine code naming why the evaluation did not settle (or
	// settled only partially). Non-empty whenever Status is "unknown".
	Reason string `json:"reason,omitempty"`
	// Detail is the human sentence for Reason.
	Detail string `json:"detail,omitempty"`
	// Remediation is the operator's next step, when there is one.
	Remediation string `json:"remediation,omitempty"`
	// RequiredContexts is the union of every context the evaluated sources
	// require — what IS required, when the probed check is not.
	RequiredContexts []string `json:"required_contexts,omitempty"`
}

// mergeGateSource is one protection surface requiring the probed check, with
// its own bypass detail.
//
// BypassEntries counts a ruleset's `bypass_actors` ENTRIES — each a role, team,
// app or integration that may cover many people or none. It is never a
// headcount, and the classic source's admin exemption is NOT coerced into a
// count of 1: that is carried by EnforceAdmins as its own named condition.
type mergeGateSource struct {
	// Identity is "branch_protection" or "ruleset:<id>".
	Identity string `json:"identity"`
	// Classic is true for the classic branch-protection source.
	Classic bool `json:"classic,omitempty"`
	// BypassEntries is the number of entries in THIS ruleset's bypass_actors
	// array. Always 0 for the classic source.
	BypassEntries int `json:"bypass_entries"`
	// EnforceAdmins mirrors classic protection's enforce_admins.enabled;
	// meaningful only when Classic is true. False means repository admins are
	// exempt from this source.
	EnforceAdmins bool `json:"enforce_admins,omitempty"`
	// Bypassable is whether THIS source alone can be bypassed.
	Bypassable bool `json:"bypassable"`
}

// Reason codes for the degrades that never reach mergegate.Reconcile — the
// preconditions the server itself cannot satisfy. The forge-level codes
// (rulesets_unqueryable, non_authoritative, administration_read_missing,
// transport_error) come from the mergegate package and are surfaced verbatim.
const (
	// mergeGateReasonNoGitHubClient — this deployment has no GitHub client
	// wired, so no protection surface can be read at all.
	mergeGateReasonNoGitHubClient = "github_client_unconfigured"
	// mergeGateReasonAppNotInstalled — both protection reads need an
	// installation token, which an uninstalled App cannot mint.
	mergeGateReasonAppNotInstalled = "app_not_installed"
	// mergeGateReasonDefaultBranch — the repo's REAL default branch could not
	// be resolved. The probe never guesses "main": a `~DEFAULT_BRANCH`
	// ruleset evaluated against the wrong branch would silently mis-answer.
	mergeGateReasonDefaultBranch = "default_branch_unresolved"
	// mergeGateReasonProbeFailed — Reconcile rejected the call itself (a
	// caller mistake such as an empty branch). Forge failures never land
	// here; they resolve to a StatusUnknown reconciliation instead.
	mergeGateReasonProbeFailed = "probe_failed"
)

// mergeGateRepository resolves the target repo's metadata (its REAL default
// branch). It is a package var so a test can drive probeMergeGate's
// nil/empty-default-branch guard: githubclient.GetRepository never returns
// (nil, nil) — it errors when `default_branch` is absent — so that guard is
// unreachable through the concrete client, yet removing it would leave a nil
// dereference one interface change away.
var mergeGateRepository = func(ctx context.Context, gh *githubclient.Client, scope forge.CredentialScope,
	repo githubclient.RepoRef) (*githubclient.Repository, error) {
	return gh.GetRepository(ctx, scope, repo)
}

// mergeGateReconcile is the reconciliation seam, a package var for the same
// reason: mergegate.Reconcile reserves its error return for CALLER mistakes
// (nil api, empty branch, empty check), all three of which probeMergeGate has
// already excluded by the time it calls — so the error branch is unreachable
// in production but must still fail closed rather than render a verdict.
var mergeGateReconcile = mergegate.Reconcile

// mergeGateProbeTimeout bounds the whole merge-gate probe — the default-branch
// resolve plus both protection reads. Same 10s order as
// requiredChecksCaptureTimeout (required_checks_capture.go): a readiness report
// must not hang on a slow forge. Declared as a var so a test can shrink it.
var mergeGateProbeTimeout = 10 * time.Second

// probeMergeGate runs readiness check (5). It reads the repo's REAL default
// branch and hands it to mergegate.Reconcile, which answers whether the
// published check gates merges on it.
//
// Every failure path returns StatusUnknown with a naming Reason — the
// fail-closed contract. `not_required` is reachable ONLY through a
// Reconciliation that said so on an authoritative read.
func (s *Server) probeMergeGate(ctx context.Context, repo string, repoRef githubclient.RepoRef,
	installed bool, installationID int64) mergeGateReadiness {
	out := mergeGateReadiness{
		Status: string(mergegate.StatusUnknown),
		Check:  auditcheckpublisher.CheckName,
	}
	switch {
	case s.cfg.GitHub == nil:
		out.Reason = mergeGateReasonNoGitHubClient
		out.Detail = "github client not configured on this deployment; the repository's branch protection could not be read"
		return out
	case !installed:
		out.Reason = mergeGateReasonAppNotInstalled
		out.Detail = "GitHub App is not installed on the target repository; reading branch protection needs an installation token"
		out.Remediation = "Install the Fishhawk GitHub App on " + repo + ", then re-run the check."
		return out
	}

	scope := forge.FromGitHubInstallationID(installationID)
	pctx, cancel := context.WithTimeout(ctx, mergeGateProbeTimeout)
	defer cancel()

	// The REAL default branch, never a guessed "main": the ruleset matcher
	// evaluates `~DEFAULT_BRANCH` against it, so a repo defaulting to `trunk`
	// resolves its rulesets only when the true name is supplied (#2506).
	meta, err := mergeGateRepository(pctx, s.cfg.GitHub, scope, repoRef)
	switch {
	case err != nil:
		out.Reason = mergeGateReasonDefaultBranch
		out.Detail = "could not resolve the repository's default branch: " + err.Error()
		out.Remediation = "Re-run the check once the forge is reachable."
		s.cfg.Logger.Warn("onboarding readiness: merge gate default-branch resolve failed",
			"repo", repo, "error", err.Error())
		return out
	case meta == nil || meta.DefaultBranch == "":
		out.Reason = mergeGateReasonDefaultBranch
		out.Detail = "the repository response carried no default branch, so the protection surfaces could not be evaluated"
		out.Remediation = "Re-run the check once the forge is reachable."
		return out
	}

	branch := meta.DefaultBranch
	out.Branch = branch

	rec, err := mergeGateReconcile(pctx, s.cfg.GitHub, scope, repoRef, branch, branch, auditcheckpublisher.CheckName)
	if err != nil {
		// Reserved for caller mistakes; forge failures come back as a
		// StatusUnknown reconciliation with a nil error.
		out.Reason = mergeGateReasonProbeFailed
		out.Detail = "the merge-gate probe could not run: " + err.Error()
		s.cfg.Logger.Warn("onboarding readiness: merge gate probe failed",
			"repo", repo, "error", err.Error())
		return out
	}

	out.Status = string(rec.Status)
	out.Check = rec.Check
	out.Bypassable = rec.Bypassable
	out.Authoritative = rec.Authoritative
	out.Reason = rec.Reason
	out.Detail = rec.Detail
	out.Remediation = rec.Remediation
	out.RequiredContexts = rec.RequiredContexts
	for _, src := range rec.Sources {
		out.Sources = append(out.Sources, mergeGateSource{
			Identity:      src.Identity,
			Classic:       src.Classic,
			BypassEntries: src.BypassEntries,
			EnforceAdmins: src.EnforceAdmins,
			Bypassable:    src.Bypassable,
		})
	}
	return out
}

// handleGetOnboardingReadiness implements GET /v0/onboarding/readiness?repo=owner/name
// (E29.4). It aggregates the server-side-only readiness probes a first run
// needs, reusing the exact classification the run-create path performs.
//
// Two-part gate:
//
//   - It does NOT gate on a write scope — scope adequacy is itself a reported
//     field, and a write-scope gate would lock out precisely the callers who
//     need to discover their gap.
//   - It DOES gate on repo read-visibility: a NON-ADMIN cookie-session caller
//     who lacks forge `read` on the queried repo gets 403 repo_forbidden BEFORE
//     any installation resolve or spec fetch (ADR-057 Amendment A2 / #2071,
//     issue #1512). The gate reuses enforceRepoVisibility, so the endpoint
//     inherits the whole #2071 point-read contract: the three unfiltered
//     postures (bearer/MCP token identities, workspace admins — INCLUDING admin
//     cookie sessions, which bypass via the RoleAdmin branch of repoFilterFor
//     and never see the 403 — and deployments with no repo-ACL mirror wired),
//     403 on a deny, 503 on a mirror-store / provider-resolution / role-
//     resolution fault, and the cross-forge / prefixless-subject fail-closed
//     denies.
//
// The ordering invariant is load-bearing: 401 anonymous → 400 malformed repo →
// visibility. Anonymous is rejected before any filter resolve (an
// unauthenticated caller must not learn a repo exists), the repo string is
// validated to a well-formed owner/name before the filter is handed it, and
// only then does the visibility gate run — so a denied caller reaches ZERO
// forge calls, ZERO spec fetches, and receives no spec.Error text at all.
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

	// Repo read-visibility gate (#1512, ADR-057 Amendment A2 / #2071). Runs
	// AFTER the 401/400 checks above and BEFORE any forge call below, so a
	// denied caller learns nothing about the repo's installation or spec state.
	if !s.enforceRepoVisibility(w, r, repo) {
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

	// (5) Merge gate: is the check Fishhawk publishes actually required by the
	// repo's protection on its REAL default branch (#3161)? Needs an
	// installation token, so it runs only once the App is installed; every
	// other posture degrades to `unknown` with a naming reason rather than to
	// `not_required`.
	resp.MergeGate = s.probeMergeGate(r.Context(), repo, repoRef, resp.App.Installed, installationID)

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
