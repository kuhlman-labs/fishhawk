package failuresig

import (
	"strings"
	"testing"
)

// failedEvidence is the common shape: a failed stage carrying a category and a
// reason. Tests vary only what they are pinning.
func failedEvidence(category, reason string) Evidence {
	return Evidence{
		StageType:       "implement",
		StageState:      "failed",
		FailureCategory: category,
		FailureReason:   reason,
	}
}

// matchFixture is one REALISTIC evidence per catalog entry — each reason is
// the literal shape its emitting site renders, not the bare anchor — paired
// with the id it must classify as.
type matchFixture struct {
	name string
	ev   Evidence
	want string
}

// matchFixtures is the shared production-path corpus. It is deliberately a
// helper rather than a table local to one test: every assertion about a
// PRODUCIBLE hint (the id, the version stamp, the marshalled size) must drive
// Match with evidence a caller could really supply, not with a hand-built
// Hint. TestMatch_EverySignature pins that it covers every catalog entry, so a
// new signature cannot land without one.
func matchFixtures() []matchFixture {
	return []matchFixture{
		{
			name: "external api incident",
			ev:   failedEvidence("A", "terminal external API error 529 (retries exhausted): exit status 1"),
			want: "external_api_incident",
		},
		{
			name: "model quota exhausted",
			ev:   failedEvidence("A", "could not obtain model quota (likely a usage/rate cap): agent exited with exit status 1 after 2s having made no model call (0 tokens)"),
			want: "model_quota_exhausted",
		},
		{
			name: "slice integration conflict",
			ev:   failedEvidence("B", "slice integration conflict: slice 2 (child run 8f3a) could not merge onto fishhawk/run-abc"),
			want: "slice_integration_conflict",
		},
		{
			name: "lineage lock contention",
			ev:   failedEvidence("C", "lineage_lock: another runner holds the lineage lock for run 8f3a"),
			want: "lineage_lock_contention",
		},
		{
			name: "zero exit strand",
			ev:   failedEvidence("D", "runner exited 0 without settling the stage (state=running)"),
			want: "zero_exit_strand",
		},
		{
			name: "runner died before reporting",
			ev:   failedEvidence("D", "runner exited 5 before reporting a terminal state"),
			want: "runner_died_before_reporting",
		},
		{
			name: "infra flake recurred",
			ev:   failedEvidence("A", "verify gate failed after verify_infra_flake_retry absorbed one flake"),
			want: "infra_flake_recurred",
		},
		{
			name: "agent no progress on a repeat attempt",
			ev: Evidence{
				StageType:        "implement",
				StageState:       "failed",
				FailureCategory:  "A",
				FailureReason:    "agent exited with exit status 1",
				RetryAttempt:     1,
				ProgressReported: true,
			},
			want: "agent_no_progress_repeat",
		},
	}
}

// TestMatch_EverySignature drives every fixture through Match and asserts the
// matched id by identity, and pins that the corpus covers the whole catalog.
func TestMatch_EverySignature(t *testing.T) {
	covered := map[string]struct{}{}
	for _, tc := range matchFixtures() {
		covered[tc.want] = struct{}{}
	}
	for _, sig := range Registry() {
		if _, ok := covered[sig.ID]; !ok {
			t.Errorf("no fixture drives %s through Match — every catalog entry needs one (README: Adding a signature, step 2)", sig.ID)
		}
	}
	for _, tc := range matchFixtures() {
		t.Run(tc.name, func(t *testing.T) {
			got := Match(tc.ev)
			if got == nil {
				t.Fatalf("Match returned nil, want id %q", tc.want)
			}
			if got.ID != tc.want {
				t.Fatalf("Match id = %q, want %q", got.ID, tc.want)
			}
			if got.RegistryVersion != RegistryVersion {
				t.Errorf("RegistryVersion = %q, want %q", got.RegistryVersion, RegistryVersion)
			}
			if got.Title == "" || got.Means == "" || len(got.Playbook) == 0 {
				t.Errorf("hint is incomplete: %+v", got)
			}
		})
	}
}

// TestMatch_ZeroEvidenceYieldsNil pins defensive mode 3: evidence carrying
// neither a category nor a reason names no failure and matches nothing.
func TestMatch_ZeroEvidenceYieldsNil(t *testing.T) {
	if got := Match(Evidence{}); got != nil {
		t.Fatalf("Match(zero evidence) = %+v, want nil", got)
	}
}

// TestMatch_UnrecognizedReasonYieldsNil pins the fail-open contract: a real
// failure whose reason matches no anchor returns nil rather than the nearest
// entry.
func TestMatch_UnrecognizedReasonYieldsNil(t *testing.T) {
	ev := failedEvidence("A", "the agent could not satisfy the binding assertion in step 4")
	if got := Match(ev); got != nil {
		t.Fatalf("Match(unrecognized reason) = %+v, want nil", got)
	}
}

// TestMatch_CategoryWithoutReasonYieldsNil pins that a category alone never
// fires a reason-anchored signature.
func TestMatch_CategoryWithoutReasonYieldsNil(t *testing.T) {
	for _, cat := range []string{"A", "B", "C", "D"} {
		if got := Match(failedEvidence(cat, "")); got != nil {
			t.Fatalf("Match(category %s, empty reason) = %+v, want nil", cat, got)
		}
	}
}

// TestMatch_HealthyEvidenceNeverMatches is the counterfactual vehicle for the
// describesFailure guard.
//
// The load-bearing case is a stage RETRIED IN PLACE: the row still carries the
// previous attempt's failure_reason while the stage is running again. Every
// field except StageState is byte-identical to a case TestMatch_EverySignature
// proves DOES match, so the only thing that can keep the walk from firing is
// the guard — delete it and this test goes RED on the behavioural assertion,
// not on a fixture-setup failure.
func TestMatch_HealthyEvidenceNeverMatches(t *testing.T) {
	cases := []struct {
		name string
		ev   Evidence
	}{
		{
			name: "running stage carrying the previous attempt's external-API reason",
			ev: Evidence{
				StageType:       "implement",
				StageState:      "running",
				FailureCategory: "A",
				FailureReason:   "terminal external API error 529 (retries exhausted): exit status 1",
			},
		},
		{
			name: "running stage carrying the previous attempt's lineage-lock reason",
			ev: Evidence{
				StageType:       "implement",
				StageState:      "running",
				FailureCategory: "C",
				FailureReason:   "lineage_lock: another runner holds the lineage lock for run 8f3a",
			},
		},
		{
			name: "succeeded stage whose counters satisfy the no-progress predicate",
			ev: Evidence{
				StageType:        "implement",
				StageState:       "succeeded",
				FailureCategory:  "A",
				FailureReason:    "verify_infra_flake_retry",
				RetryAttempt:     2,
				ProgressReported: true,
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Match(tc.ev); got != nil {
				t.Fatalf("Match(healthy evidence) = id %q, want nil — a non-failed stage must never carry a recovery playbook", got.ID)
			}
		})
	}
}

// TestMatch_FirstMatchWins is the counterfactual vehicle for the early return
// in the walk. The fixture satisfies external_api_incident AND
// infra_flake_recurred simultaneously; the assertion is by IDENTITY, so a
// last-match-wins walk names infra_flake_recurred and goes RED.
func TestMatch_FirstMatchWins(t *testing.T) {
	ev := failedEvidence("A", "terminal external API error 529 (retries exhausted): the verify gate had already absorbed one verify_infra_flake_retry")
	got := Match(ev)
	if got == nil {
		t.Fatal("Match returned nil, want external_api_incident")
	}
	if got.ID != "external_api_incident" {
		t.Fatalf("Match id = %q, want external_api_incident — precedence decides the recovery (back off vs retry immediately)", got.ID)
	}
}

// TestMatch_AnchorsAreSubstringContracts pins that an anchor embedded in a
// longer real line still matches — the runner never emits a bare anchor.
func TestMatch_AnchorsAreSubstringContracts(t *testing.T) {
	ev := failedEvidence("C", "stage refused to start: lineage_lock held by pid 4711 (run 8f3a, stage implement)")
	got := Match(ev)
	if got == nil || got.ID != "lineage_lock_contention" {
		t.Fatalf("Match = %+v, want lineage_lock_contention from an embedded anchor", got)
	}
}

// TestMatch_MalformedExternalAPIStatusFallsThrough pins defensive mode 5: the
// signature keys on the PHRASE, so a reason whose phrase is followed by a
// non-integer still classifies as the incident. (The mcpserver-side status
// parser independently declines to name a status; the two are decoupled on
// purpose.)
func TestMatch_MalformedExternalAPIStatusFallsThrough(t *testing.T) {
	ev := failedEvidence("A", "terminal external API error unknown (retries exhausted): exit status 1")
	got := Match(ev)
	if got == nil || got.ID != "external_api_incident" {
		t.Fatalf("Match = %+v, want external_api_incident on a malformed status", got)
	}
}

// TestMatch_AbsentProgressNeverMatchesNoProgressSignature pins defensive mode
// 6: an ABSENT heartbeat leaves the counters at zero, which must never be read
// as observed inactivity.
func TestMatch_AbsentProgressNeverMatchesNoProgressSignature(t *testing.T) {
	ev := Evidence{
		StageType:       "implement",
		StageState:      "failed",
		FailureCategory: "A",
		FailureReason:   "agent exited with exit status 1",
		RetryAttempt:    2,
		// ProgressReported deliberately false: no stage_progress heartbeat
		// arrived, so the zero counters carry no information.
	}
	if got := Match(ev); got != nil {
		t.Fatalf("Match = id %q, want nil — an absent heartbeat is not observed inactivity", got.ID)
	}
}

// TestMatch_NoProgressFirstAttemptDoesNotMatch pins defensive mode 7: the
// signature is about a REPEAT.
func TestMatch_NoProgressFirstAttemptDoesNotMatch(t *testing.T) {
	ev := Evidence{
		StageType:        "implement",
		StageState:       "failed",
		FailureCategory:  "A",
		FailureReason:    "agent exited with exit status 1",
		RetryAttempt:     0,
		ProgressReported: true,
	}
	if got := Match(ev); got != nil {
		t.Fatalf("Match = id %q, want nil on a first attempt", got.ID)
	}
}

// TestMatch_NoProgressRequiresBothCountersZero pins that real activity on the
// repeat attempt keeps the signature quiet.
func TestMatch_NoProgressRequiresBothCountersZero(t *testing.T) {
	base := Evidence{
		StageType:        "implement",
		StageState:       "failed",
		FailureCategory:  "A",
		FailureReason:    "agent exited with exit status 1",
		RetryAttempt:     1,
		ProgressReported: true,
	}
	withTurns := base
	withTurns.TurnsThisAttempt = 9
	if got := Match(withTurns); got != nil {
		t.Fatalf("Match(turns=9) = id %q, want nil", got.ID)
	}
	withTokens := base
	withTokens.TokensThisAttempt = 13402
	if got := Match(withTokens); got != nil {
		t.Fatalf("Match(tokens=13402) = id %q, want nil", got.ID)
	}
}

// TestMatch_LineageLockRequiresCategoryC pins the one category-qualified
// reason anchor: "lineage_lock" is a short token that could appear in an
// unrelated line, so the signature also requires the refusal category.
func TestMatch_LineageLockRequiresCategoryC(t *testing.T) {
	ev := failedEvidence("A", "the agent wrote a lineage_lock helper and the tests failed")
	if got := Match(ev); got != nil {
		t.Fatalf("Match = id %q, want nil — a non-C category must not classify as lineage-lock contention", got.ID)
	}
}

// TestMatch_ReturnsIndependentPlaybookCopies pins the BEHAVIOUR that a caller
// mutating a returned playbook cannot corrupt the catalog for the next caller.
//
// It does NOT discriminate the defensive copy in Match. The control that
// defends this property TODAY is Registry()'s per-call reconstruction — it
// rebuilds every Playbook from a fresh slice literal, so the second Match
// reads a brand-new backing array whether or not Match copies, and deleting
// the copy leaves this test green (observed, #1703 fix-up pass).
// TestRegistryReturnsFreshPlaybooksPerCall is the counterfactual vehicle for
// that reconstruction. Match's copy becomes load-bearing only if Registry() is
// ever changed to hand out a shared package-level catalog; this test is what
// would then hold the line, which is why it is kept rather than folded away.
func TestMatch_ReturnsIndependentPlaybookCopies(t *testing.T) {
	ev := failedEvidence("A", "terminal external API error 529 (retries exhausted)")
	first := Match(ev)
	if first == nil || len(first.Playbook) == 0 {
		t.Fatal("Match returned no playbook")
	}
	first.Playbook[0] = "MUTATED"
	second := Match(ev)
	if second == nil {
		t.Fatal("second Match returned nil")
	}
	if second.Playbook[0] == "MUTATED" {
		t.Fatal("a caller's mutation reached the catalog")
	}
	if strings.Contains(second.Playbook[0], "MUTATED") {
		t.Fatal("a caller's mutation reached the catalog")
	}
}
