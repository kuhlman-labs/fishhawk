package main

import "strings"

// runner_kind literals. These DUPLICATE the backend's closed set
// (backend/internal/run.ValidRunnerKinds / RunnerKindGitHubActions /
// RunnerKindLocal): the runner module cannot import the backend module,
// and the value rides the SIGNED bundle manifest as a wire string, so the
// two sides move in lockstep (same duplication discipline as the bundle
// manifest fields). A drift here is caught by the backend's
// run.ValidRunnerKinds membership check in ResolveRunnerKind, which
// ignores an unrecognized self-report rather than persisting it.
const (
	runnerKindGitHubActions = "github_actions"
	runnerKindLocal         = "local"
)

// detectRunnerKind observes the execution channel from the process
// environment and returns the runner_kind to stamp into the bundle
// manifest (#1346 / ADR-045). The backend reconciles this self-report
// against the run's creation-time hint, so the runner — not the operator
// — is authoritative on which backend actually executed.
//
// LOAD-BEARING assumption: the GITHUB_* variables are the authoritative
// github_actions signal. GitHub Actions sets GITHUB_ACTIONS=true and a
// non-empty GITHUB_RUN_ID in every workflow job; a host-side
// fishhawk_dispatch_stage / run_stage spawn (the local dogfood loop)
// inherits NEITHER. We treat EITHER of those as proof of github_actions.
//
// CI is DELIBERATELY NOT consulted. A local dev shell commonly exports
// CI=true (test runners, pre-commit harnesses), and mis-locking such a
// local run to github_actions would re-create the phantom-Actions-runner
// wedge this change exists to fix (#1344 — the failure DIRECTION): the
// drive's plan-approval gate would wait forever on a GitHub-Actions runner
// that never dispatches. So CI=true ALONE (with no GITHUB_* var present)
// MUST resolve to local. The github_actions decision keys ONLY off the
// GITHUB_* signals.
func detectRunnerKind(getenv func(string) string) string {
	if strings.EqualFold(getenv("GITHUB_ACTIONS"), "true") || getenv("GITHUB_RUN_ID") != "" {
		return runnerKindGitHubActions
	}
	return runnerKindLocal
}
