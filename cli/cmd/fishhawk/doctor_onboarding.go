package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/cli/internal/spec"
)

// onboardingReadiness mirrors the backend's onboardingReadinessResponse
// (backend/internal/server/onboarding.go) — the E29.4 GET
// /v0/onboarding/readiness payload. Field names and JSON tags MUST stay
// byte-compatible with that struct; a divergence silently zero-values a
// field. The all-green end-to-end doctor test asserts a fully-populated
// render to catch drift.
type onboardingReadiness struct {
	Repo string `json:"repo"`
	App  struct {
		Installed      bool   `json:"installed"`
		InstallationID int64  `json:"installation_id"`
		Reason         string `json:"reason"`
	} `json:"app"`
	Spec struct {
		Source string `json:"source"`
		Valid  bool   `json:"valid"`
		Error  string `json:"error"`
		Note   string `json:"note"`
	} `json:"spec"`
	Reviewers []struct {
		Provider    string `json:"provider"`
		Model       string `json:"model"`
		Available   bool   `json:"available"`
		ModelStatus string `json:"model_status"`
		ModelHint   string `json:"model_hint"`
		Priced      *bool  `json:"priced"`
		MissingHint string `json:"missing_hint"`
	} `json:"reviewers"`
	Scopes struct {
		Adequate bool     `json:"adequate"`
		Required []string `json:"required"`
		Missing  []string `json:"missing"`
		Note     string   `json:"note"`
	} `json:"scopes"`
	// MergeGate is the #3161 merge-gate reconciliation. It is a POINTER on
	// purpose: an older fishhawkd serves no `merge_gate` key at all, and the
	// doctor must then emit NO rung rather than a bogus warning derived from a
	// zero value. nil means "the backend did not answer this question",
	// which is not the same claim as "unknown".
	MergeGate *mergeGateReadiness `json:"merge_gate"`
	// TraceStore is the DEPLOYMENT-scoped trace-store rung (E45.75 / #3600).
	// A POINTER for the same reason as MergeGate: a pre-#3600 fishhawkd serves
	// no `trace_store` key, and the doctor must then emit NO rung — a zero
	// value would read as configured:false with an out-of-enum kind "", a
	// verdict the payload never made.
	TraceStore *traceStoreReadiness `json:"trace_store"`
	// ReviewGrounding is the DEPLOYMENT-scoped review-grounding rung (E45.90
	// / #3625). A POINTER for the same reason as TraceStore: a pre-#3625
	// fishhawkd serves no `review_grounding` key, and the doctor must then
	// emit NO rung — a zero value would read as enabled:false, a verdict
	// about the deployment's posture the payload never made.
	ReviewGrounding *reviewGroundingReadiness `json:"review_grounding"`
	// WorkItemProvider is the HYBRID-scoped work-item-provider rung (E45.94 /
	// #3646). A POINTER for the same reason as TraceStore: a pre-#3646
	// fishhawkd serves no `work_item_provider` key, and the doctor must then
	// emit NO rung — a zero value would read as an out-of-enum empty status,
	// a verdict the payload never made, and would FAIL the command on a
	// backend that simply cannot answer.
	WorkItemProvider *workItemProviderReadiness `json:"work_item_provider"`
}

// workItemProviderReadiness mirrors the backend workItemProviderReadiness
// sub-object (backend/internal/server/onboarding.go, #3646): is the work-item
// provider this repo's conventions RESOLVE to actually REGISTERED on this
// deployment? Status is a closed three-value vocabulary —
// registered | unregistered | unknown. Unlike review_grounding, `unregistered`
// is a hard FAILURE that moves the doctor's aggregate outcome and exit code:
// every campaign, `fishhawk_file_issue` and the grooming loop respond 501
// provider_unimplemented on such a deployment.
type workItemProviderReadiness struct {
	Status      string   `json:"status"`
	Provider    string   `json:"provider"`
	Registered  []string `json:"registered"`
	Reason      string   `json:"reason"`
	Note        string   `json:"note"`
	MissingHint string   `json:"missing_hint"`
}

// reviewGroundingReadiness mirrors the backend reviewGroundingReadiness
// sub-object (backend/internal/server/onboarding.go, #3625): whether this
// deployment grounds its review agents against an exported read-only tree, or
// leaves them DIFF-ONLY. Enabled false is the SUPPORTED DEFAULT (grounding
// ships dormant behind FISHHAWKD_REVIEW_GROUNDING), so the rung renders it as
// ok-with-a-hint and never degrades the doctor's aggregate outcome.
type reviewGroundingReadiness struct {
	Enabled     bool                          `json:"enabled"`
	Adapters    []reviewGroundingAdapterBound `json:"adapters"`
	Note        string                        `json:"note"`
	Remediation string                        `json:"remediation"`
}

// reviewGroundingAdapterBound mirrors one backend adapter row. Bound is a
// closed two-value vocabulary whose members are NOT the same strength:
// "confined" (codex — OS-level deny-by-default allowlist) and "blocklist"
// (claude — a tool-layer deny-rule list, defence-in-depth). The rung renders
// both verbatim rather than collapsing them into one word.
type reviewGroundingAdapterBound struct {
	Adapter string `json:"adapter"`
	Bound   string `json:"bound"`
	Note    string `json:"note"`
}

// traceStoreReadiness mirrors the backend traceStoreReadiness sub-object
// (backend/internal/server/onboarding.go, #3600). Kind is one of four values:
// "s3", "memory" (the --dev-fixtures / --dev-trace-store in-memory store,
// EPHEMERAL), "none" (no
// store — every run's trace upload 503s after the agent was billed) or
// "other" (a non-S3, non-memory store, no durability claim).
type traceStoreReadiness struct {
	Configured  bool   `json:"configured"`
	Kind        string `json:"kind"`
	Note        string `json:"note"`
	Remediation string `json:"remediation"`
}

// mergeGateReadiness mirrors the backend mergeGateReadiness sub-object
// (backend/internal/server/onboarding.go, #3161): whether the
// fishhawk_audit_complete Check Run Fishhawk publishes is actually REQUIRED by
// the repo's protection on its REAL default branch.
//
// Read Status fail-closed: "unknown" means the question could not be settled,
// NOT that the check is unrequired. Reason names which degrade.
type mergeGateReadiness struct {
	Status           string            `json:"status"`
	Check            string            `json:"check"`
	Branch           string            `json:"branch"`
	Sources          []mergeGateSource `json:"sources"`
	Bypassable       bool              `json:"bypassable"`
	Authoritative    bool              `json:"authoritative"`
	Reason           string            `json:"reason"`
	Detail           string            `json:"detail"`
	Remediation      string            `json:"remediation"`
	RequiredContexts []string          `json:"required_contexts"`
}

// mergeGateSource mirrors one protection surface requiring the probed check.
//
// BypassEntries counts a ruleset's `bypass_actors` ENTRIES — roles, teams or
// apps, each of which may cover multiple people — never a headcount. The
// classic source's admin exemption is carried by EnforceAdmins as its own
// named condition and is never coerced into a count of 1.
type mergeGateSource struct {
	Identity      string `json:"identity"`
	Classic       bool   `json:"classic"`
	BypassEntries int    `json:"bypass_entries"`
	EnforceAdmins bool   `json:"enforce_admins"`
	Bypassable    bool   `json:"bypassable"`
}

// checkOnboardingReadiness probes GET {backendURL}/v0/onboarding/readiness
// (E29.4) for the target repo and expands the aggregated server-side-only
// payload into one checkResult per precondition: GitHub App installation,
// per-reviewer availability, caller-token scope adequacy, the committed
// workflow spec's validity, and — when the backend serves it — whether the
// published check is actually required by the repo's branch protection
// (#3161). Each failing precondition carries an actionable
// remediation. A repo that could not be resolved, or any transport / non-200
// response, degrades to a single WARN — it never crashes the doctor.
//
// It also returns a readinessOutcome carrying whether the endpoint answered an
// AUTHORITATIVE 200 (HTTP 200 AND a decodable body whose echoed `repo` is
// non-empty AND equals the requested repo) plus the server's own scope verdict,
// threaded into the token rung so a server-authenticated credential is not
// failed by the local /v0/runs heuristic. A decode that succeeds is NOT proof
// the body is a readiness verdict — `{}`, `null`, and a verdict for a DIFFERENT
// repo all decode cleanly — so answered is set ONLY on that positive repo-echo
// marker. Any non-answer (undecodable body, empty or mismatched repo) leaves
// answered false, so a broken or wrong-repo backend response can never suppress
// a genuine token failure (binding condition 1 / #2480).
func checkOnboardingReadiness(backendURL, token, repo string) ([]checkResult, readinessOutcome) {
	const label = "onboarding readiness"
	if repo == "" {
		return []checkResult{{
			label: label, detail: "repo not determined", status: "warn",
			remediate: "pass --repo owner/name (git origin auto-detect found no github.com remote)",
		}}, readinessOutcome{}
	}

	endpoint := backendURL + "/v0/onboarding/readiness?repo=" + url.QueryEscape(repo)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return []checkResult{{
			label: label, detail: err.Error(), status: "warn",
			remediate: "check --backend-url or $FISHHAWK_BACKEND_URL",
		}}, readinessOutcome{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return []checkResult{{
			label: label, detail: "readiness endpoint unreachable", status: "warn",
			remediate: "backend must be reachable for onboarding readiness checks",
		}}, readinessOutcome{}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return []checkResult{{
			label: label, detail: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "warn",
			remediate: "onboarding readiness probe returned non-200; check the token and fishhawkd logs",
		}}, readinessOutcome{}
	}
	var body onboardingReadiness
	// degradeToWarn returns the single non-authoritative warn rung with a zero
	// readinessOutcome (answered=false), so a non-answer can never suppress a
	// genuine token-rung failure (binding condition 1 / #2480).
	degradeToWarn := func() ([]checkResult, readinessOutcome) {
		return []checkResult{{
			label: label, detail: "unparseable readiness response", status: "warn",
			remediate: "upgrade fishhawkd to a build that serves /v0/onboarding/readiness",
		}}, readinessOutcome{}
	}
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		// A 200 with an undecodable body is NOT an authoritative answer.
		return degradeToWarn()
	}
	// A successful decode is NOT proof the body is a readiness verdict:
	// onboardingReadiness has no required fields, so `{}`, `{"unrelated":1}`,
	// and a verdict for a DIFFERENT repo all decode cleanly, and `null` decodes
	// to the zero value. The endpoint ALWAYS echoes the repo it answered for
	// (onboarding.go handleGetOnboardingReadiness sets Repo: repo), so require
	// that positive marker to be non-empty AND to equal the repo argument.
	// Anything else — empty marker, or a repo that does not match — is NOT
	// authoritative: degrade to the same single warn, with answered=false, so
	// the token rung can still fail (binding condition 1 / #2480).
	if body.Repo == "" || body.Repo != repo {
		return degradeToWarn()
	}

	// 200, a decodable body, AND an echoed repo that matches the request: this
	// is the authoritative answer.
	outcome := readinessOutcome{answered: true, scopesAdequate: body.Scopes.Adequate}

	var out []checkResult

	// (a) GitHub App installation.
	if body.App.Installed {
		detail := "installed"
		if body.App.InstallationID != 0 {
			detail = fmt.Sprintf("installed (installation %d)", body.App.InstallationID)
		}
		out = append(out, checkResult{label: "app installed", detail: detail, status: "ok"})
	} else {
		detail := body.App.Reason
		if detail == "" {
			detail = "not installed"
		}
		out = append(out, checkResult{
			label: "app installed", detail: detail, status: "fail",
			remediate: "install the Fishhawk GitHub App on " + repo +
				": https://github.com/apps/fishhawk/installations/new",
		})
	}

	// (b) Per-reviewer availability — one rung per declared reviewer. The
	// model-id honesty fields (#3578) refine an available reviewer's verdict:
	// an unverifiable or unpriced model is a WARN (not a hard ok), and a
	// rejected model surfaces the did-you-mean as the remediation.
	for _, rv := range body.Reviewers {
		rvLabel := "reviewer available: " + rv.Provider
		if !rv.Available {
			// Prefer the model_hint (did-you-mean) when the provider IS wired
			// but the resolved model was authoritatively rejected; otherwise the
			// provider-level missing_hint.
			remediate := rv.MissingHint
			if rv.ModelStatus == "rejected" && rv.ModelHint != "" {
				remediate = rv.ModelHint
			}
			if remediate == "" {
				remediate = "configure the " + rv.Provider + " reviewer backend on this deployment"
			}
			out = append(out, checkResult{
				label: rvLabel, detail: "unavailable", status: "fail", remediate: remediate,
			})
			continue
		}
		detail := "available"
		if rv.Model != "" {
			detail = rv.Model
		}
		// unverified: no authoritative snapshot to check against (a typo would
		// only fail at review time). unpriced: pricing table doesn't know the
		// family, so usage would book at $0. Either downgrades ok → warn. A nil
		// or true priced (and a verified/empty status) leaves the rung ok — an
		// OLDER backend that serves none of these fields renders exactly as before.
		unverified := rv.ModelStatus == "unverifiable"
		unpriced := rv.Priced != nil && !*rv.Priced
		if unverified || unpriced {
			var notes []string
			if unverified {
				notes = append(notes, "unverified")
			}
			if unpriced {
				notes = append(notes, "unpriced")
			}
			out = append(out, checkResult{
				label: rvLabel, detail: detail + " (" + strings.Join(notes, ", ") + ")",
				status: "warn", remediate: rv.ModelHint,
			})
			continue
		}
		out = append(out, checkResult{label: rvLabel, detail: detail, status: "ok"})
	}

	// (c) Caller-token scope adequacy.
	if body.Scopes.Adequate {
		detail := "adequate"
		if body.Scopes.Note != "" {
			detail = body.Scopes.Note
		}
		out = append(out, checkResult{label: "token scope adequate", detail: detail, status: "ok"})
	} else {
		out = append(out, checkResult{
			label:  "token scope adequate",
			detail: "missing: " + strings.Join(body.Scopes.Missing, ", "),
			status: "fail",
			remediate: "reissue the token with the missing scope(s) via `fishhawkd token issue --subject <login> --scopes " +
				strings.Join(body.Scopes.Missing, ",") + "`",
		})
	}

	// (d) Committed workflow spec validity (server-side fetch + parse + validate).
	specLabel := "workflow spec (committed) valid"
	switch body.Spec.Source {
	case "fetched":
		if body.Spec.Valid {
			out = append(out, checkResult{label: specLabel, detail: "valid", status: "ok"})
		} else {
			reason := body.Spec.Error
			if reason == "" {
				reason = body.Spec.Note
			}
			out = append(out, checkResult{
				label: specLabel, detail: "invalid", status: "fail",
				remediate: "run `fishhawk validate` for details: " + reason,
			})
		}
	default: // "unavailable" or any other non-fetched source.
		detail := body.Spec.Note
		if detail == "" {
			detail = "spec unavailable"
		}
		out = append(out, checkResult{
			label: specLabel, detail: detail, status: "warn",
			remediate: "install the App and commit .fishhawk/workflows.yaml so the spec can be fetched",
		})
	}

	// (e) Merge gate — is the check Fishhawk publishes actually required by the
	// repo's protection (#3161)? Absent entirely against an older fishhawkd,
	// which draws NO rung: silence beats a warning derived from a zero value.
	if rung, ok := mergeGateRung(body.MergeGate); ok {
		out = append(out, rung)
	}

	// (f) Trace store — will this deployment accept the run's trace bundle
	// (#3600)? Deployment-scoped; absent against an older fishhawkd, which
	// draws NO rung.
	if rung, ok := traceStoreRung(body.TraceStore); ok {
		out = append(out, rung)
	}

	// (g) Review grounding — are the review agents grounded against an
	// exported tree, or DIFF-ONLY (#3625)? Deployment-scoped; absent against
	// an older fishhawkd, which draws NO rung. It NEVER reports worse than
	// ok: off is the supported default, so this rung cannot move the
	// aggregate outcome or the doctor's exit code.
	if rung, ok := reviewGroundingRung(body.ReviewGrounding); ok {
		out = append(out, rung)
	}

	// (h) Work-item provider — can this deployment file work items for this
	// repo AT ALL (#3646)? Hybrid-scoped; absent against an older fishhawkd,
	// which draws NO rung. Unlike (g) this rung CAN report "fail": a resolved
	// but unregistered provider forecloses every campaign, work-item filing
	// and the grooming loop with a 501, which is a failure and not data.
	if rung, ok := workItemProviderRung(body.WorkItemProvider); ok {
		out = append(out, rung)
	}

	return out, outcome
}

// mergeGateRung renders the merge-gate readiness rung, or reports ok=false
// when there is no rung to render (#3161).
//
// Three states plus one absence:
//
//   - nil        — the backend served no `merge_gate` key (a pre-#3161
//     fishhawkd). NO rung: the doctor has not learned that the
//     check is unrequired, only that this backend cannot say.
//     A warning here would be a claim the payload does not make.
//   - required   — ok. The detail names each requiring source with ITS OWN
//     bypass condition; when EVERY requiring source is
//     bypassable the server's remediation rides along as a hint.
//   - not_required — warn. Fishhawk requires the check to be required: with it
//     unrequired, fishhawk_merge_run can queue an auto-merge that
//     fires with the review verdict still pending. Non-fatal to
//     the doctor's exit code (only "fail" counts), so an operator
//     who opts out deliberately is informed, not blocked.
//   - unknown    — warn naming the reason. NOT a claim the check is
//     unrequired — the question could not be settled.
func mergeGateRung(mg *mergeGateReadiness) (checkResult, bool) {
	if mg == nil {
		return checkResult{}, false
	}
	const label = "merge gate enforced"
	check := mg.Check
	if check == "" {
		check = "fishhawk_audit_complete"
	}

	switch mg.Status {
	case "required":
		detail := "required on " + mg.Branch
		if mg.Branch == "" {
			detail = "required"
		}
		if names := mergeGateSourceSummary(mg.Sources); names != "" {
			detail += " (" + names + ")"
		}
		return checkResult{
			label: label, detail: detail, status: "ok",
			// Non-empty only when every requiring source is bypassable — the
			// server leaves it empty when the gate genuinely holds.
			remediate: mg.Remediation,
		}, true
	case "not_required":
		detail := check + " is not a required status check on " + mg.Branch
		if mg.Branch == "" {
			detail = check + " is not a required status check"
		}
		return checkResult{
			label: label, detail: detail, status: "warn",
			remediate: mg.Remediation,
		}, true
	default:
		// "unknown", and any status this build does not recognise — both are
		// unsettled, and neither licenses reporting the check as unrequired.
		detail := "unknown"
		if mg.Reason != "" {
			detail = "unknown (" + mg.Reason + ")"
		}
		remediate := mg.Detail
		if mg.Remediation != "" {
			remediate = mg.Remediation
		}
		if remediate == "" {
			remediate = "could not read the repository's branch protection; this is not evidence the check is unrequired"
		}
		return checkResult{
			label: label, detail: detail, status: "warn", remediate: remediate,
		}, true
	}
}

// traceStoreRung renders the trace-store readiness rung, or reports ok=false
// when there is no rung to render (#3600).
//
//   - nil          — the backend served no `trace_store` key (a pre-#3600
//     fishhawkd). NO rung: absence means the backend cannot
//     answer, which is not the same claim as configured:false.
//   - none         — fail. Every run's trace upload responds 503 AFTER the
//     agent has run and been billed; remediate names
//     FISHHAWKD_S3_BUCKET and `make s3-init`.
//   - memory       — warn. Uploads succeed but the store is EPHEMERAL.
//   - s3 / other   — ok. Any kind this build does not recognise is rendered
//     from Configured: configured → ok, else fail.
func traceStoreRung(ts *traceStoreReadiness) (checkResult, bool) {
	if ts == nil {
		return checkResult{}, false
	}
	const label = "trace store configured"
	switch {
	case ts.Kind == "memory":
		remediate := ts.Note
		if remediate == "" {
			remediate = "the in-memory trace store (--dev-fixtures or --dev-trace-store) is EPHEMERAL: every bundle is lost when fishhawkd restarts"
		}
		return checkResult{
			label: label, detail: "memory (ephemeral)", status: "warn",
			remediate: remediate + "; set FISHHAWKD_S3_BUCKET for a durable store",
		}, true
	case ts.Kind == "none" || !ts.Configured:
		remediate := ts.Remediation
		if remediate == "" {
			remediate = "set FISHHAWKD_S3_BUCKET (see the trace-storage block in .env.example) and create the bucket with `make s3-init`, then restart fishhawkd"
		}
		return checkResult{
			label: label, detail: "no trace store: every run's trace upload will respond 503 after the agent is billed", status: "fail",
			remediate: remediate,
		}, true
	default:
		detail := ts.Kind
		if detail == "" {
			detail = "configured"
		}
		return checkResult{label: label, detail: detail, status: "ok"}, true
	}
}

// reviewGroundingRung renders the review-grounding readiness rung, or reports
// ok=false when there is no rung to render (#3625).
//
//   - nil      — the backend served no `review_grounding` key (a pre-#3625
//     fishhawkd). NO rung: absence means the backend cannot
//     answer, which is not the same claim as enabled:false.
//   - disabled — ok, detail "off (reviews are diff-only)", with a hint naming
//     FISHHAWKD_REVIEW_GROUNDING and the per-adapter asymmetry.
//     Status ok, NOT warn: off is the supported, recommended
//     default (grounding ships dormant, #2522), so a correctly
//     configured deployment is not nagged and the doctor's
//     aggregate outcome and exit code are unchanged.
//   - enabled  — ok, detail naming each adapter's bound, with the asymmetry
//     carried in the hint.
//
// `remediate` renders on ok rungs too (doctor.go prints `hint:` whenever it is
// non-empty), which is what lets an ok rung still surface the flag.
func reviewGroundingRung(rg *reviewGroundingReadiness) (checkResult, bool) {
	if rg == nil {
		return checkResult{}, false
	}
	const label = "review grounding"
	if !rg.Enabled {
		remediate := rg.Remediation
		if remediate == "" {
			remediate = "set FISHHAWKD_REVIEW_GROUNDING=true to ground reviews against an exported read-only tree; it is an opt-in posture for a single-tenant host you control, and the per-adapter read bounds are not equivalent (codex: OS-enforced confinement; claude: a tool-layer blocklist, defence-in-depth only)"
		}
		return checkResult{
			label: label, detail: "off (reviews are diff-only)", status: "ok",
			remediate: remediate,
		}, true
	}
	detail := "on"
	if bounds := reviewGroundingBoundSummary(rg.Adapters); bounds != "" {
		detail = "on (" + bounds + ")"
	}
	remediate := rg.Remediation
	if remediate == "" {
		remediate = "the per-adapter read bounds are not equivalent: codex gets OS-enforced confinement, claude gets a tool-layer blocklist that is defence-in-depth only"
	}
	return checkResult{label: label, detail: detail, status: "ok", remediate: remediate}, true
}

// reviewGroundingBoundSummary renders each adapter's bound IN ITS OWN TERMS —
// "codex: confined; claude: blocklist" — never one collapsed word for both.
// The asymmetry is the load-bearing fact (#2522) and flattening it into a
// single "bounded" would be exactly the over-claim the honest-label invariant
// forbids.
func reviewGroundingBoundSummary(adapters []reviewGroundingAdapterBound) string {
	if len(adapters) == 0 {
		return ""
	}
	parts := make([]string, 0, len(adapters))
	for _, a := range adapters {
		bound := a.Bound
		if bound == "" {
			bound = "unknown bound"
		}
		parts = append(parts, a.Adapter+": "+bound)
	}
	return strings.Join(parts, "; ")
}

// workItemProviderRung renders the work-item-provider readiness rung, or
// reports ok=false when there is no rung to render (#3646).
//
// Four states:
//
//   - nil          — the backend served no `work_item_provider` key (a
//     pre-#3646 fishhawkd). NO rung: absence means the backend
//     cannot answer, which is not the same claim as
//     unregistered — and a rung here would fail the command on
//     a backend that made no claim.
//   - registered   — ok, detail naming the resolved provider.
//   - unregistered — FAIL. This is the one rung state here that is a hard
//     failure: it moves the doctor's aggregate outcome and exit
//     code, because a deployment on which fishhawk_start_campaign,
//     fishhawk_file_issue and the grooming loop all respond 501
//     provider_unimplemented is not ready, and #3646 exists
//     precisely because it previously reported all-green.
//   - unknown      — warn (and NEVER a pass), naming the reason, with a
//     remediation that explicitly states this is not evidence
//     the provider is unregistered. Any status this build does
//     not recognise takes the same branch: an unsettled verdict
//     must never be rendered as a pass.
func workItemProviderRung(wp *workItemProviderReadiness) (checkResult, bool) {
	if wp == nil {
		return checkResult{}, false
	}
	const label = "work-item provider registered"
	switch wp.Status {
	case "registered":
		detail := wp.Provider
		if detail == "" {
			detail = "registered"
		}
		return checkResult{label: label, detail: detail, status: "ok"}, true
	case "unregistered":
		provider := wp.Provider
		if provider == "" {
			provider = "the repo's resolved provider"
		}
		detail := provider + " is not registered on this deployment"
		if len(wp.Registered) > 0 {
			detail += " (registered: " + strings.Join(wp.Registered, ", ") + ")"
		} else {
			detail += " (no work-item provider is registered at all)"
		}
		detail += "; campaigns, `fishhawk file-issue` and the grooming loop will respond 501 provider_unimplemented"
		remediate := wp.MissingHint
		if remediate == "" {
			remediate = "configure a work-item provider's credentials on the fishhawkd deployment and restart it; a provider registers only when its client is configured at startup"
		}
		return checkResult{label: label, detail: detail, status: "fail", remediate: remediate}, true
	default:
		// "unknown", and any status this build does not recognise — both are
		// unsettled, and neither licenses reporting the provider as either
		// registered or unregistered.
		detail := "unknown"
		if wp.Reason != "" {
			detail = "unknown (" + wp.Reason + ")"
		}
		remediate := wp.MissingHint
		if remediate == "" {
			remediate = "the repo's work-management conventions could not be resolved, so this rung makes no claim"
		}
		remediate += "; this is NOT evidence that the provider is unregistered"
		return checkResult{label: label, detail: detail, status: "warn", remediate: remediate}, true
	}
}

// mergeGateSourceSummary renders each requiring source's bypass posture IN ITS
// OWN TERMS.
//
// A ruleset bypass entry is a role, team, app or integration — it may cover
// many people or none — so it is reported as "N bypass entries (roles, teams
// or apps), each of which may cover multiple people", never as "bypassable by
// N actors". Classic protection has no such list: its exemption is
// enforce_admins:false, rendered as its own named condition ("repository
// admins are exempt") and never coerced into a count of 1.
func mergeGateSourceSummary(sources []mergeGateSource) string {
	if len(sources) == 0 {
		return ""
	}
	parts := make([]string, 0, len(sources))
	for _, src := range sources {
		switch {
		case src.Classic && !src.EnforceAdmins:
			parts = append(parts, src.Identity+": repository admins are exempt")
		case src.Classic:
			parts = append(parts, src.Identity)
		case src.BypassEntries > 0:
			parts = append(parts, fmt.Sprintf(
				"%s: %d bypass entries (roles, teams or apps), each of which may cover multiple people",
				src.Identity, src.BypassEntries))
		default:
			parts = append(parts, src.Identity)
		}
	}
	return strings.Join(parts, "; ")
}

// checkExecutionPath verifies that the committed workflow spec declares an
// executor for every stage — the client-side complement to the readiness
// probe. A stage with no executor is exactly a spec that "looks onboarded"
// but wedges on the first run when that stage dispatches.
//
// Per the E29.5 approval condition it reports "ok" ONLY when EVERY stage in
// the discovered spec declares a non-empty executor (agent, human, or a
// delegate). It FAILS — naming the offending stage(s) in its remediation — if
// ANY stage lacks one, so a mixed spec (some stages configured, at least one
// not) is flagged rather than passing. It warns when no spec is found;
// checkSpec is the authority on a missing / schema-invalid spec.
//
// The check runs on the RESOLVED document (spec.ResolveReuse), not the raw
// author bytes (#2340): a workflow-v2 stage may legitimately omit its own
// executor and inherit one from a file- or workflow-level `defaults` block or
// an `extends` base, and the product accepts such a stage because both Go
// validators resolve reuse before schema validation. Checking the raw bytes
// would false-fail that stage. A spec that cannot be resolved degrades to
// warn pointing at `fishhawk validate`, mirroring the parse-error rung —
// checkSpec remains the authority on a broken or schema-invalid spec, so a
// doctor rung must never be the thing that reports one as a hard fail.
func checkExecutionPath(workingDir string) checkResult {
	const label = "execution path configured"
	ds, err := discoverSpec(workingDir, "")
	if err != nil {
		return checkResult{label: label, detail: "spec read error", status: "warn",
			remediate: "fix the read error on .fishhawk/workflows.yaml"}
	}
	if ds == nil {
		return checkResult{label: label, detail: "no spec found", status: "warn",
			remediate: "create .fishhawk/workflows.yaml (see docs/spec/workflows-v0.md)"}
	}

	resolved, err := spec.ResolveReuse(ds.Contents)
	if err != nil {
		return checkResult{label: label, detail: "spec resolve error", status: "warn",
			remediate: "run `fishhawk validate` for details"}
	}

	var parsed struct {
		Workflows map[string]struct {
			Stages []struct {
				ID       string `yaml:"id"`
				Executor struct {
					Agent    string         `yaml:"agent"`
					Human    bool           `yaml:"human"`
					Delegate map[string]any `yaml:"delegate"`
				} `yaml:"executor"`
			} `yaml:"stages"`
		} `yaml:"workflows"`
	}
	if err := yaml.Unmarshal(resolved, &parsed); err != nil {
		return checkResult{label: label, detail: "spec parse error", status: "warn",
			remediate: "run `fishhawk validate` for details"}
	}

	total := 0
	var unconfigured []string
	for wfName, wf := range parsed.Workflows {
		for i, st := range wf.Stages {
			total++
			configured := st.Executor.Agent != "" || st.Executor.Human || len(st.Executor.Delegate) > 0
			if configured {
				continue
			}
			name := st.ID
			if name == "" {
				name = fmt.Sprintf("%s[%d]", wfName, i)
			}
			unconfigured = append(unconfigured, name)
		}
	}

	if total == 0 {
		return checkResult{label: label, detail: "no stages declared", status: "warn",
			remediate: "add at least one stage with an executor (see docs/spec/workflows-v0.md)"}
	}
	if len(unconfigured) > 0 {
		sort.Strings(unconfigured)
		return checkResult{
			label:  label,
			detail: fmt.Sprintf("%d of %d stage(s) without an executor", len(unconfigured), total),
			status: "fail",
			remediate: "add an executor to each stage (see docs/spec/workflows-v0.md); missing on: " +
				strings.Join(unconfigured, ", "),
		}
	}
	return checkResult{label: label, detail: fmt.Sprintf("%d stage(s) configured", total), status: "ok"}
}
