package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
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
}

// checkOnboardingReadiness probes GET {backendURL}/v0/onboarding/readiness
// (E29.4) for the target repo and expands the aggregated server-side-only
// payload into one checkResult per precondition: GitHub App installation,
// per-reviewer availability, caller-token scope adequacy, and the committed
// workflow spec's validity. Each failing precondition carries an actionable
// remediation. A repo that could not be resolved, or any transport / non-200
// response, degrades to a single WARN — it never crashes the doctor.
func checkOnboardingReadiness(backendURL, token, repo string) []checkResult {
	const label = "onboarding readiness"
	if repo == "" {
		return []checkResult{{
			label: label, detail: "repo not determined", status: "warn",
			remediate: "pass --repo owner/name (git origin auto-detect found no github.com remote)",
		}}
	}

	endpoint := backendURL + "/v0/onboarding/readiness?repo=" + url.QueryEscape(repo)
	req, err := http.NewRequest(http.MethodGet, endpoint, nil)
	if err != nil {
		return []checkResult{{
			label: label, detail: err.Error(), status: "warn",
			remediate: "check --backend-url or $FISHHAWK_BACKEND_URL",
		}}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := doctorHTTPDo(req)
	if err != nil {
		return []checkResult{{
			label: label, detail: "readiness endpoint unreachable", status: "warn",
			remediate: "backend must be reachable for onboarding readiness checks",
		}}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return []checkResult{{
			label: label, detail: fmt.Sprintf("HTTP %d", resp.StatusCode), status: "warn",
			remediate: "onboarding readiness probe returned non-200; check the token and fishhawkd logs",
		}}
	}
	var body onboardingReadiness
	if decErr := json.NewDecoder(resp.Body).Decode(&body); decErr != nil {
		return []checkResult{{
			label: label, detail: "unparseable readiness response", status: "warn",
			remediate: "upgrade fishhawkd to a build that serves /v0/onboarding/readiness",
		}}
	}

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

	return out
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
	if err := yaml.Unmarshal(ds.Contents, &parsed); err != nil {
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
