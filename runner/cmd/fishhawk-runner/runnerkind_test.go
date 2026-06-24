package main

import "testing"

// TestDetectRunnerKind covers the full env matrix (#1346 / ADR-045).
// The load-bearing case is "CI=true only ⇒ local" (binding condition #1):
// CI must NEVER on its own resolve github_actions, or a local dev shell
// that exports CI=true would re-create the #1344 phantom-Actions-runner
// wedge. The github_actions decision keys ONLY off the GITHUB_* signals.
func TestDetectRunnerKind(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "github_actions true",
			env:  map[string]string{"GITHUB_ACTIONS": "true"},
			want: runnerKindGitHubActions,
		},
		{
			name: "github_actions true uppercase",
			env:  map[string]string{"GITHUB_ACTIONS": "TRUE"},
			want: runnerKindGitHubActions,
		},
		{
			name: "github_run_id set",
			env:  map[string]string{"GITHUB_RUN_ID": "123456789"},
			want: runnerKindGitHubActions,
		},
		{
			name: "github_run_id set, github_actions empty",
			env:  map[string]string{"GITHUB_ACTIONS": "", "GITHUB_RUN_ID": "42"},
			want: runnerKindGitHubActions,
		},
		{
			// LOAD-BEARING (binding condition #1): CI alone is NOT a
			// github_actions signal. A local dev shell exporting CI=true
			// must resolve local.
			name: "ci true only",
			env:  map[string]string{"CI": "true"},
			want: runnerKindLocal,
		},
		{
			// CI=true alongside an explicit GITHUB_ACTIONS=false (no run id)
			// stays local — the GITHUB_* signals are authoritative and both
			// are absent/false.
			name: "ci true with github_actions false",
			env:  map[string]string{"CI": "true", "GITHUB_ACTIONS": "false"},
			want: runnerKindLocal,
		},
		{
			name: "github_actions false only",
			env:  map[string]string{"GITHUB_ACTIONS": "false"},
			want: runnerKindLocal,
		},
		{
			name: "nothing set",
			env:  map[string]string{},
			want: runnerKindLocal,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			getenv := func(k string) string { return tc.env[k] }
			if got := detectRunnerKind(getenv); got != tc.want {
				t.Errorf("detectRunnerKind(%v) = %q, want %q", tc.env, got, tc.want)
			}
		})
	}
}
