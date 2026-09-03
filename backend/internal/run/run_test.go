package run

import "testing"

// TestStageStateAwaitingHostDispatch_Classification pins the #1912 split of the
// conflated local 'dispatched' state. The new parked-for-host-dispatch state
// carries the exact wire value 'awaiting_host_dispatch' (audit log + JSON
// payloads carry it forever) and is classified settled-but-not-terminal — a
// parked judgment awaiting a host/operator spawn, mirroring awaiting_approval.
// These are behavioral done-means assertions (compilation does not enforce the
// classifier tables), companion to the exhaustive IsSettled/IsTerminal tables
// in transition_test.go.
func TestStageStateAwaitingHostDispatch_Classification(t *testing.T) {
	if got := string(StageStateAwaitingHostDispatch); got != "awaiting_host_dispatch" {
		t.Errorf("StageStateAwaitingHostDispatch = %q, want awaiting_host_dispatch", got)
	}
	if !StageStateAwaitingHostDispatch.IsSettled() {
		t.Error("awaiting_host_dispatch must be settled (parked for a host/operator spawn, #1912)")
	}
	if StageStateAwaitingHostDispatch.IsTerminal() {
		t.Error("awaiting_host_dispatch must NOT be terminal (a spawn/cancel still moves it forward)")
	}
	// It is distinct from 'dispatched', which now unambiguously means a spawn
	// attempt exists and stays in-flight (not settled).
	if StageStateAwaitingHostDispatch == StageStateDispatched {
		t.Error("awaiting_host_dispatch and dispatched must be distinct states")
	}
	if StageStateDispatched.IsSettled() {
		t.Error("dispatched must remain in-flight (not settled) after the #1912 split")
	}
}

// TestRunnerKindGitLabCI_Membership pins the additive gitlab_ci runner_kind
// member (ADR-058 / E45.8, #1861). The value carries the exact wire string
// 'gitlab_ci' (audit log + JSON payloads carry it forever) and is a member of
// the closed ValidRunnerKinds set alongside github_actions and local. This is
// the done-means assertion for the enum half of the plumbing slice; the
// migration 0054 CHECK widening that makes it persistable is pinned in
// postgres_test.go.
func TestRunnerKindGitLabCI_Membership(t *testing.T) {
	if RunnerKindGitLabCI != "gitlab_ci" {
		t.Errorf("RunnerKindGitLabCI = %q, want gitlab_ci", RunnerKindGitLabCI)
	}
	if _, ok := ValidRunnerKinds[RunnerKindGitLabCI]; !ok {
		t.Error("gitlab_ci must be a member of ValidRunnerKinds")
	}
	// The prior two kinds remain members (the widening is additive).
	if _, ok := ValidRunnerKinds[RunnerKindGitHubActions]; !ok {
		t.Error("github_actions must remain a member of ValidRunnerKinds")
	}
	if _, ok := ValidRunnerKinds[RunnerKindLocal]; !ok {
		t.Error("local must remain a member of ValidRunnerKinds")
	}
	// A bogus kind is not admitted (fail-closed membership).
	if _, ok := ValidRunnerKinds["gitlab_pipeline"]; ok {
		t.Error("ValidRunnerKinds must not admit an out-of-set kind")
	}
}

// TestValidTriggerSources_ClosedSet pins the accepted trigger-source set
// (E54.22 / #2826) as the SINGLE source of truth every consumer renders from:
// the server's POST /v0/runs membership check and its 400 message, and the MCP
// start_run mirror. Both the CONTENTS and the ORDER are asserted, and the
// LENGTH separately — so adding a fifth source without updating the consumers
// that iterate this accessor reddens HERE first, next to the declaration,
// rather than in a handler test a package away.
func TestValidTriggerSources_ClosedSet(t *testing.T) {
	want := []TriggerSource{TriggerGitHubIssue, TriggerCLI, TriggerUI, TriggerOnDemand}
	got := ValidTriggerSources()
	if len(got) != len(want) {
		t.Fatalf("ValidTriggerSources() has %d members %v, want %d %v — a new member must be reflected in every consumer that renders this set",
			len(got), got, len(want), want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ValidTriggerSources()[%d] = %q, want %q (declaration order is binding: the 400 message is rendered from it)", i, got[i], want[i])
		}
	}
	// The wire values are forever: they are persisted on the runs row and
	// enumerated by the runs_trigger_source_check CHECK constraint.
	if string(TriggerOnDemand) != "on_demand" {
		t.Errorf("TriggerOnDemand = %q, want on_demand", TriggerOnDemand)
	}
	// A caller must not be able to mutate the shared set through the accessor.
	ValidTriggerSources()[0] = "mutated"
	if ValidTriggerSources()[0] != TriggerGitHubIssue {
		t.Error("ValidTriggerSources() must return a fresh slice; a caller mutated the set")
	}
	// `scheduled` is deliberately NOT a member: no producer could mint it, and
	// an unmintable enum member is the dead surface #2826 exists to close.
	for _, ts := range ValidTriggerSources() {
		if ts == "scheduled" {
			t.Error("ValidTriggerSources() must not admit 'scheduled' — no producer can mint it")
		}
	}
}

// TestRunIsIssueAnchored pins the source-level predicate the issue_context
// coupling (server + MCP) and the six issuecomment suppression sites are
// written against (E54.22 / #2826). github_issue and on_demand are anchored;
// cli, ui, an unknown value and the empty value are not.
func TestRunIsIssueAnchored(t *testing.T) {
	cases := []struct {
		source TriggerSource
		want   bool
	}{
		{TriggerGitHubIssue, true},
		{TriggerOnDemand, true},
		{TriggerCLI, false},
		{TriggerUI, false},
		{TriggerSource("scheduled"), false},
		{TriggerSource("nonsense"), false},
		{TriggerSource(""), false},
	}
	for _, tc := range cases {
		r := &Run{TriggerSource: tc.source}
		if got := r.IsIssueAnchored(); got != tc.want {
			t.Errorf("Run{TriggerSource: %q}.IsIssueAnchored() = %v, want %v", tc.source, got, tc.want)
		}
	}
	// Nil-safe: the predicate is called on rows loaded from storage, and a
	// nil receiver must not panic a notifier path into a 500.
	var nilRun *Run
	if nilRun.IsIssueAnchored() {
		t.Error("(*Run)(nil).IsIssueAnchored() = true, want false")
	}
}

// TestStageStateSuperseded_Classification pins the merge-supersede terminal
// state (E64.2 / #3083). It carries the exact wire value 'superseded' (the
// audit log and every JSON payload carry it forever) and is classified TERMINAL
// — which is what makes Orchestrator.completeRun's #968 guard accept it and
// what makes transitionStage stamp ended_at. Terminal implies settled, so the
// two classifiers stay consistent. These are behavioral done-means assertions:
// compilation does not enforce the classifier tables.
func TestStageStateSuperseded_Classification(t *testing.T) {
	if got := string(StageStateSuperseded); got != "superseded" {
		t.Errorf("StageStateSuperseded = %q, want superseded", got)
	}
	if !StageStateSuperseded.IsTerminal() {
		t.Error("superseded must be terminal (the #968 completion guard must accept it, and ended_at must stamp)")
	}
	if !StageStateSuperseded.IsSettled() {
		t.Error("superseded must be settled (every terminal state is settled)")
	}
	// It is a DISTINCT fact from the two states an operator would otherwise be
	// forced to lie with — the whole point of #3083.
	if StageStateSuperseded == StageStateCancelled || StageStateSuperseded == StageStateFailed {
		t.Error("superseded must be distinct from cancelled and failed")
	}
	// The prior three terminal states remain terminal (the widening is additive).
	for _, s := range []StageState{StageStateSucceeded, StageStateFailed, StageStateCancelled} {
		if !s.IsTerminal() {
			t.Errorf("%q must remain terminal after the #3083 widening", s)
		}
	}
	// A non-terminal park must NOT have been swept into the terminal set.
	if StageStateAwaitingHostDispatch.IsTerminal() {
		t.Error("awaiting_host_dispatch must remain non-terminal")
	}
}
