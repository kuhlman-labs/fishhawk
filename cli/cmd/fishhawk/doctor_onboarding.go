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

	// (b) Per-reviewer availability — one rung per declared reviewer.
	for _, rv := range body.Reviewers {
		rvLabel := "reviewer available: " + rv.Provider
		if rv.Available {
			detail := "available"
			if rv.Model != "" {
				detail = rv.Model
			}
			out = append(out, checkResult{label: rvLabel, detail: detail, status: "ok"})
			continue
		}
		remediate := rv.MissingHint
		if remediate == "" {
			remediate = "configure the " + rv.Provider + " reviewer backend on this deployment"
		}
		out = append(out, checkResult{
			label: rvLabel, detail: "unavailable", status: "fail", remediate: remediate,
		})
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
