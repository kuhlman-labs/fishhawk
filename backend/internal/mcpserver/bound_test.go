package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/failuresig"
)

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func strptr(s string) *string { return &s }

// domainPayloadBytes measures the response with the elisions METADATA excluded,
// so a tier's reduction is measured on the domain payload alone. The whole
// response is deliberately NOT size-monotonic (attaching a growing elisions
// block can enlarge a later measurement), so no test asserts otherwise.
func domainPayloadBytes(t *testing.T, out GetRunStatusOutput) int {
	t.Helper()
	shallow := out
	shallow.Elisions = nil
	return len(mustMarshal(t, shallow))
}

// maximalRunStatusOutput builds a response where EVERY tier's target is
// present, so the ladder can be driven end to end without the handler.
func maximalRunStatusOutput(runID string) GetRunStatusOutput {
	now := time.Unix(1700000000, 0).UTC()
	out := GetRunStatusOutput{
		Run: Run{
			ID: runID, Repo: "kuhlman-labs/fishhawk", WorkflowID: "feature_change",
			WorkflowSHA: strings.Repeat("f", 40), TriggerSource: "issue",
			TriggerRef: strptr("#2508"), State: "failed",
			PullRequestURL: strptr("https://example/pull/1"),
			RetryAttempt:   1, MaxRetriesSnapshot: 2, RunnerKind: "local",
			IssueContext: &IssueContext{
				Title: "an issue", Body: strings.Repeat("issue body text ", 400),
				URL: "https://example/issues/1", Number: 2508,
				Comments: []IssueComment{{Author: "a", Body: strings.Repeat("comment ", 400), CreatedAt: "t"}},
			},
			Concerns:       &RunConcerns{Open: 3, ByState: map[string]int{"raised": 2, "reopened": 1}},
			LiveValidation: &RunLiveValidation{PendingCriteriaCount: 2, WalkRef: "#2509"},
			WorkingDir:     "/tmp/checkout",
			CreatedAt:      now, UpdatedAt: now,
		},
		ImplementReviewMergeHint: "the implement review is still pending",
		Budget:                   &BudgetStatus{Tier: "ok"},
		CacheEfficiency:          &CacheEfficiency{CacheReadRatio: 0.5, ReuseFactor: 2},
		Cost:                     &RunCost{TotalCostUSD: 12.5},
		Latency:                  &RunLatency{TotalWaitOnHumanSeconds: 900},
		ReviewActionHint:         &ReviewActionHint{Concerns: 3, Message: "route the concerns back with fishhawk_fixup_stage"},
		DriveStatus:              &DriveStatus{Drive: true, DerivedStatus: "ci_failed"},
		NextActions: &NextActions{
			State: "implement_failed_category_a",
			// The failure-signature block (#1703) travels the full marshal /
			// bound ladder with the fixture, so the byte-budget assertions are
			// exercised with it present. It is classified tierNever, so no tier
			// elides it — which is only safe because it is constant-size.
			Signature: failuresig.Match(failuresig.Evidence{
				StageType:       "implement",
				StageState:      "failed",
				FailureCategory: "A",
				FailureReason:   "terminal external API error 529 (retries exhausted): exit status 1",
			}),
		},
		ChildrenStatus: &ChildrenStatus{IntegrationPhase: "running_children", Total: 4},
	}
	for i := 0; i < 12; i++ {
		out.Run.Concerns.Items = append(out.Run.Concerns.Items, RunConcernItem{
			ID: uuid.NewString(), StageKind: "implement", Severity: "high", Category: "correctness", State: "raised",
		})
	}
	for i := 0; i < 4; i++ {
		out.Run.ReviewAuthority = append(out.Run.ReviewAuthority, RunReviewAuthority{
			Stage: fmt.Sprintf("s%d", i), StageType: "implement", Authority: "advisory", Source: "derived",
		})
	}
	for i := 0; i < 8; i++ {
		reason := strings.Repeat("failure reason <prose> ", 300)
		out.Stages = append(out.Stages, Stage{
			ID: uuid.NewString(), RunID: runID, Sequence: i + 1, Type: "implement",
			Executor: StageExecutor{Kind: "agent", Ref: "claude"}, State: "failed",
			StartedAt: &now, EndedAt: &now,
			FailureCategory: strptr("category_a"), FailureReason: &reason,
			Progress:  &StageProgress{LastEvent: "assistant", TurnsThisAttempt: 9, TokensThisAttempt: 13402, ReportedAt: now},
			CreatedAt: now, UpdatedAt: now,
		})
	}
	for i := 0; i < 20; i++ {
		out.RecentAudit = append(out.RecentAudit, AuditEntry{
			ID: uuid.NewString(), Sequence: int64(20 - i), RunID: runID,
			Category: "cost_recorded", Payload: map[string]any{"detail": strings.Repeat("x", 200)},
		})
	}
	for i := 0; i < 6; i++ {
		out.ImplementReviews = append(out.ImplementReviews, PlanReview{
			ReviewerKind: "agent", Authority: "advisory", Verdict: "approve_with_concerns",
		})
	}
	for i := 0; i < 10; i++ {
		out.SecurityFindings = append(out.SecurityFindings, SecurityFinding{
			Number: i, RuleID: "go/sql-injection", Severity: "high",
		})
	}
	for i := 0; i < 12; i++ {
		out.DriveStatus.AutoAdvanced = append(out.DriveStatus.AutoAdvanced, RunAutoAdvance{Rule: "reviews_settled_gate", From: "a", To: "b", Timestamp: now})
	}
	for i := 0; i < 20; i++ {
		out.NextActions.Actions = append(out.NextActions.Actions, SuggestedAction{
			Action: "fishhawk_fixup_stage", Precondition: "p", Consumes: "fixup_budget", Reason: "r",
		})
	}
	for i := 0; i < 4; i++ {
		out.ChildrenStatus.Children = append(out.ChildrenStatus.Children, ChildStatus{RunID: uuid.NewString(), SliceIndex: i, State: "running"})
	}
	return out
}

// ---------------------------------------------------------------------------
// the cap primitive
// ---------------------------------------------------------------------------

// adversarialStrings is the differential table: every cost class the encoder
// treats specially, INCLUDING several distinct invalid UTF-8 byte sequences
// written as literal bytes (never produced by calling the code under test).
var adversarialStrings = []string{
	"",
	"plain ascii",
	`quo"te`,
	`back\slash`,
	"\x00\x01\x02\x1f",
	"tab\there\nnewline\rcr",
	"<script> & </script>",
	" line sep para sep",
	"日本語のテキスト",
	"emoji 🐟🦅",
	"\xff",           // lone invalid byte
	"\x80",           // lone continuation byte
	"a\xe2\x28\xa1b", // truncated multi-byte lead
	"\xc0\xaf",       // overlong encoding
	"\xed\xa0\x80",   // surrogate half
	"mix<\x00日\xffz \"\\",
}

func TestJSONEncodedLen_MatchesEncoder_Differential(t *testing.T) {
	for i, s := range adversarialStrings {
		want := len(mustMarshal(t, s))
		if got := jsonEncodedLen(s); got != want {
			t.Errorf("case %d %q: jsonEncodedLen = %d, encoder = %d", i, s, got, want)
		}
	}
}

func TestCapJSONString_EncodedLengthContract(t *testing.T) {
	for i, s := range adversarialStrings {
		for b := -1; b <= 64; b++ {
			got := capJSONString(s, b)
			limit := b
			if limit < 2 {
				limit = 2
			}
			if n := len(mustMarshal(t, got)); n > limit {
				t.Fatalf("case %d %q budget %d: encoded %d > max(budget,2)=%d (result %q)", i, s, b, n, limit, got)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("case %d %q budget %d: result is not valid UTF-8: %q", i, s, b, got)
			}
			if !strings.HasPrefix(s, got) {
				t.Fatalf("case %d %q budget %d: result %q is not a prefix of the input", i, s, b, got)
			}
		}
	}
}

// ---------------------------------------------------------------------------
// the tier ladder
// ---------------------------------------------------------------------------

func TestElisionTier_ReducesDomainPayload(t *testing.T) {
	runID := uuid.NewString()
	for k, tier := range runStatusTiers {
		out := maximalRunStatusOutput(runID)
		led := &elisionLedger{budget: 1}
		// Apply every earlier tier so tier k sees the state it really runs on.
		for j := 0; j < k; j++ {
			led.tier = runStatusTiers[j].name
			runStatusTiers[j].apply(&out, runID, led)
		}
		before := domainPayloadBytes(t, out)
		led.tier = tier.name
		tier.apply(&out, runID, led)
		after := domainPayloadBytes(t, out)
		if after >= before {
			t.Errorf("tier %s: domain payload %d -> %d, want a strict decrease", tier.name, before, after)
		}
	}
}

func TestBound_ConvergesAtAnyBudget(t *testing.T) {
	runID := uuid.NewString()
	budgets := []int{0, 1, 2, 64, minimalRunStatusMaxBytes - 1, minimalRunStatusMaxBytes,
		6 * 1024, 16 * 1024, mcpResponseByteBudgetDefault, 1 << 20}
	for _, b := range budgets {
		out, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(b))
		if err != nil {
			t.Fatalf("budget %d: %v", b, err)
		}
		limit := b
		if limit < minimalRunStatusMaxBytes {
			limit = minimalRunStatusMaxBytes
		}
		if n := len(mustMarshal(t, out)); n > limit {
			t.Errorf("budget %d: bounded response = %d bytes, want <= max(budget, %d) = %d", b, n, minimalRunStatusMaxBytes, limit)
		}
	}
}

// TestBound_UnderBudget_ReturnsTheInputBytesUnchanged is the PRE-bound vs
// POST-bound byte-identity proof, and the baseline is what makes it one: the
// wire bytes are captured from the INPUT, BEFORE boundRunStatusOutput runs.
//
// Re-running the ladder over an ALREADY-bounded response instead would assert
// only IDEMPOTENCE, which a first-pass mutation emitting no elisions satisfies
// — an unconditional cap of an already-capped string is byte-stable on the
// second pass. So the handler-level test cannot carry this claim (its output has
// already been through the ladder) and this one does.
//
// The budget is the fixture's own marshalled length, which also pins the
// comparison as `n <= budget` rather than `n < budget`.
func TestBound_UnderBudget_ReturnsTheInputBytesUnchanged(t *testing.T) {
	runID := uuid.NewString()
	in := maximalRunStatusOutput(runID)

	// Non-vacuity: the fixture must carry the targets every tier would mutate,
	// or "unchanged" would be trivially true.
	if in.Run.IssueContext == nil || in.Cost == nil || in.ChildrenStatus == nil ||
		len(in.RecentAudit) <= recentAuditTierCap || len(in.SecurityFindings) == 0 ||
		len(in.NextActions.Actions) <= nextActionsTierCap {
		t.Fatalf("the fixture does not carry every tier's target — an unchanged result would prove nothing: %+v", in)
	}
	var sawLongFailureReason bool
	for _, s := range in.Stages {
		if s.FailureReason != nil && jsonEncodedLen(*s.FailureReason) > failureReasonTierCap {
			sawLongFailureReason = true
		}
	}
	if !sawLongFailureReason {
		t.Fatal("the fixture carries no over-cap failure_reason — T7 would be a no-op regardless of the early return")
	}

	before := mustMarshal(t, in) // the genuine PRE-bound wire

	got, err := boundRunStatusOutput(in, runID, fixedBudget(len(before)))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if got.Elisions != nil {
		t.Errorf("an under-budget response must carry no elisions block, got %+v", got.Elisions)
	}
	if after := string(mustMarshal(t, got)); after != string(before) {
		t.Errorf("the ladder mutated an at-budget response: %d pre-bound bytes -> %d post-bound bytes", len(before), len(after))
	}
}

// TestFloor_IsConstantSizeUnderAdversarialInput pins the floor's own bound for
// adversarial inputs, and that the floor sits below the default budget.
func TestFloor_IsConstantSizeUnderAdversarialInput(t *testing.T) {
	if minimalRunStatusMaxBytes >= mcpResponseByteBudgetDefault {
		t.Fatalf("floor %d must be below the default budget %d", minimalRunStatusMaxBytes, mcpResponseByteBudgetDefault)
	}
	cases := []struct{ name, category, state string }{
		{"megabyte category", strings.Repeat("z", 1<<20), "failed"},
		{"invalid utf8", "\xff\xfe" + strings.Repeat("\x00", 4096), "failed"},
		{"html + separators", strings.Repeat("<&> ", 4096), "failed"},
	}
	for _, tc := range cases {
		out, err := minimalRunStatusFrom(uuid.NewString(), tc.state, "implement", tc.state, tc.category, true, fixedBudget(8))
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if n := len(mustMarshal(t, out)); n > minimalRunStatusMaxBytes {
			t.Errorf("%s: floor = %d bytes, want <= %d", tc.name, n, minimalRunStatusMaxBytes)
		}
	}
}

func TestBound_DeterministicAcrossRepeatedRuns(t *testing.T) {
	runID := uuid.NewString()
	first, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(6*1024))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	want := mustMarshal(t, first)
	for i := 0; i < 50; i++ {
		got, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(6*1024))
		if err != nil {
			t.Fatalf("bound %d: %v", i, err)
		}
		if string(mustMarshal(t, got)) != string(want) {
			t.Fatalf("iteration %d produced different bytes — map iteration order leaked into the elision", i)
		}
	}
}

// TestEveryResponsePathIsClassified is the NESTED-AWARE reflection pin. It
// walks GetRunStatusOutput and, recursively, the retained composites (Run,
// Stage, NextActions, RunConcerns) and fails when any field — top-level OR
// nested — is absent from runStatusPathTable.
func TestEveryResponsePathIsClassified(t *testing.T) {
	for _, path := range classifiedResponsePaths() {
		if _, ok := pathClassificationFor(path); !ok {
			t.Errorf("response path %q is not classified in runStatusPathTable — assign it a tier (or the %q marker) so it cannot silently bypass the byte budget", path, tierNever)
		}
	}
	// The walk must actually reach the nested levels, or the pin is vacuous.
	for _, must := range []string{"run.issue_context", "run.concerns.items", "stages[].executor", "next_actions.actions"} {
		found := false
		for _, p := range classifiedResponsePaths() {
			if p == must {
				found = true
			}
		}
		if !found {
			t.Errorf("the reflection walk did not reach nested path %q", must)
		}
	}
	// And the table must not carry a row for a composite the walk recurses
	// into (that would classify a container instead of its fields).
	for path := range retainedCompositeTypes() {
		if _, ok := pathClassificationFor(path); ok {
			t.Errorf("path %q is a retained composite; classify its FIELDS, not the container", path)
		}
	}
	_ = reflect.TypeOf(GetRunStatusOutput{})
}

// ---------------------------------------------------------------------------
// class / pointer invariants
// ---------------------------------------------------------------------------

// bandedElisions drives the whole matrix: every tier band from a generous
// budget down to the floor, returning each band's emitted elisions.
//
// The 13 KiB band is inside the diagnosis-skeleton fit window (#3043). Adding
// run.concerns.open_implement as an itemised skeleton omission (consistent with
// its siblings run.concerns.open / by_state, since skeletonRunStatus never
// copies Run.Concerns) grows the skeleton to a constant 12465 bytes, so it no
// longer fits the 12 KiB band and its fit window shifts to [~12465, ~16383].
// Without a band in that window no probe would engage the skeleton tier at all,
// and the skeleton-ONLY next_actions.actions computed elision — which
// TestElisions_ComputedCarryNoPointer requires — would vanish from the matrix.
// TestElisions_SkeletonBandEngagesSkeletonTier pins that this band actually
// engages the skeleton, so the coverage is restored, not merely the green.
func bandedElisions(t *testing.T, runID string) map[int]*Elisions {
	t.Helper()
	out := map[int]*Elisions{}
	for _, b := range []int{28 * 1024, 20 * 1024, 13 * 1024, 12 * 1024, 8 * 1024, 6 * 1024, 5 * 1024, minimalRunStatusMaxBytes} {
		bounded, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(b))
		if err != nil {
			t.Fatalf("budget %d: %v", b, err)
		}
		if bounded.Elisions == nil {
			t.Fatalf("budget %d produced no elisions block", b)
		}
		out[b] = bounded.Elisions
	}
	return out
}

// TestElisions_SkeletonBandEngagesSkeletonTier is the amendment's binding
// condition (#3043): the 13 KiB band added to bandedElisions must POSITIVELY
// engage the diagnosis-skeleton tier and carry the skeleton-only
// next_actions.actions computed elision — not merely make TestElisions_*
// pass by probing nothing. If a future field addition shifts the skeleton fit
// window past 13 KiB, this fails LOUDLY (naming the tier it landed on and the
// measured skeleton size to re-diagnose) instead of the coverage silently
// evaporating. The skeleton is a constant 12465 bytes; the fit window is
// [~12465, ~16383], so 13 KiB (13312) sits ~850 B above the floor and ~3 KiB
// below the T9 ceiling — comfortably inside, one band suffices.
func TestElisions_SkeletonBandEngagesSkeletonTier(t *testing.T) {
	runID := uuid.NewString()
	const band = 13 * 1024
	bounded, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(band))
	if err != nil {
		t.Fatalf("band %d: %v", band, err)
	}
	if bounded.Elisions == nil {
		t.Fatalf("band %d produced no elisions block", band)
	}
	if bounded.Elisions.Tier != "skeleton" {
		raw, _ := json.Marshal(bounded)
		t.Fatalf("band %d engaged tier %q, want \"skeleton\"; the skeleton fit window shifted (measured skeleton size ~12465B, serialized here %dB) — re-pick the band inside the new window",
			band, bounded.Elisions.Tier, len(raw))
	}
	found := false
	for _, f := range bounded.Elisions.Fields {
		if f.Field == "next_actions.actions" {
			found = true
			if f.Class != string(classComputed) {
				t.Errorf("next_actions.actions elision class = %q, want computed", f.Class)
			}
		}
	}
	if !found {
		t.Errorf("band %d skeleton carries no next_actions.actions computed elision — the coverage this band restores", band)
	}
}

func TestElisions_ComputedCarryNoPointer(t *testing.T) {
	runID := uuid.NewString()
	seen := map[string]bool{}
	for b, el := range bandedElisions(t, runID) {
		for _, f := range el.Fields {
			if f.Class != string(classComputed) {
				continue
			}
			seen[f.Field] = true
			if f.Pointer != "" {
				t.Errorf("budget %d: computed entry %q carries pointer %q — recomputation is not retrieval", b, f.Field, f.Pointer)
			}
		}
	}
	for _, must := range []string{"cost", "cache_efficiency", "latency", "budget", "children_status", "next_actions.actions"} {
		if !seen[must] {
			t.Errorf("expected a computed elision for %q across the tier bands", must)
		}
	}
}

func TestElisions_StoredCarriesRetrievingPointer(t *testing.T) {
	runID := uuid.NewString()
	for b, el := range bandedElisions(t, runID) {
		for _, f := range el.Fields {
			if f.Class != string(classStored) {
				continue
			}
			if f.Pointer == "" {
				t.Errorf("budget %d: stored entry %q carries no pointer", b, f.Field)
				continue
			}
			if strings.Contains(f.Pointer, "include_") {
				t.Errorf("budget %d: stored entry %q pointer %q names an include_* flag — repeating a flag is circular", b, f.Field, f.Pointer)
			}
			if !strings.Contains(f.Pointer, runID) {
				t.Errorf("budget %d: stored entry %q pointer %q does not embed the run under test", b, f.Field, f.Pointer)
			}
			for _, part := range strings.Split(f.Pointer, "; ") {
				switch {
				case strings.HasPrefix(part, "fishhawk_list_audit(run_id="),
					strings.HasPrefix(part, "fishhawk_get_gate_view(run_id="),
					strings.HasPrefix(part, "GET /v0/runs/"):
				default:
					t.Errorf("budget %d: stored entry %q pointer part %q is not one of the three constructor forms", b, f.Field, part)
				}
			}
		}
	}
}

func TestElisions_OversizedCapablePointsAtUnboundedSurface(t *testing.T) {
	runID := uuid.NewString()
	seen := 0
	for b, el := range bandedElisions(t, runID) {
		for _, f := range el.Fields {
			if f.Class != string(classOversizedCapable) {
				continue
			}
			seen++
			if !strings.HasPrefix(f.Pointer, "GET /v0/") {
				t.Errorf("budget %d: oversized_capable entry %q pointer %q is not a REST path", b, f.Field, f.Pointer)
			}
			if strings.Contains(f.Pointer, "fishhawk_") {
				t.Errorf("budget %d: oversized_capable entry %q points at a bounded MCP tool (%q) — that call caps the same value again", b, f.Field, f.Pointer)
			}
		}
	}
	if seen == 0 {
		t.Fatal("no oversized_capable elision was emitted across the tier bands")
	}
}

func TestElisions_DroppedSetCarriesCount(t *testing.T) {
	runID := uuid.NewString()
	fixture := maximalRunStatusOutput(runID)
	want := map[string]int{
		"security_findings":          len(fixture.SecurityFindings),
		"implement_reviews":          len(fixture.ImplementReviews),
		"run.concerns.items":         len(fixture.Run.Concerns.Items),
		"run.review_authority":       len(fixture.Run.ReviewAuthority),
		"drive_status.auto_advanced": len(fixture.DriveStatus.AutoAdvanced),
		"children_status":            len(fixture.ChildrenStatus.Children),
		"next_actions.actions":       len(fixture.NextActions.Actions) - nextActionsTierCap,
	}
	out := fixture
	led := &elisionLedger{budget: 1}
	for _, tier := range runStatusTiers {
		led.tier = tier.name
		tier.apply(&out, runID, led)
	}
	got := map[string]int{}
	for _, e := range led.entries {
		if _, tracked := want[e.field]; tracked {
			got[e.field] = e.omittedCount
		}
	}
	for field, n := range want {
		if got[field] != n {
			t.Errorf("elision %q omitted_count = %d, want the true dropped count %d", field, got[field], n)
		}
	}
	// recent_audit is dropped in two steps (T5 caps, T6 drops the rest); the
	// two counts must sum to the original length.
	sum := 0
	for _, e := range led.entries {
		if e.field == "recent_audit" {
			sum += e.omittedCount
		}
	}
	if sum != len(fixture.RecentAudit) {
		t.Errorf("recent_audit omitted counts sum to %d, want %d", sum, len(fixture.RecentAudit))
	}
}

// ---------------------------------------------------------------------------
// pointer retrieval semantics (the at-least promise), through the API seam
// ---------------------------------------------------------------------------

// followAuditPointer parses a rendered fishhawk_list_audit pointer and drives
// the REAL listAudit tool through it, paginating exhaustively.
func followAuditPointer(t *testing.T, r *runResolver, pointer string) []AuditEntry {
	t.Helper()
	inner := strings.TrimSuffix(strings.TrimPrefix(pointer, "fishhawk_list_audit("), ")")
	in := ListAuditInput{}
	for _, kv := range strings.Split(inner, ", ") {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "run_id":
			in.RunID = v
		case "category":
			in.Category = v
		case "since_sequence":
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				t.Fatalf("pointer %q: bad since_sequence: %v", pointer, err)
			}
			in.SinceSequence = n
		default:
			t.Fatalf("pointer %q: unrecognised parameter %q", pointer, k)
		}
	}
	var all []AuditEntry
	for {
		_, out, err := r.listAudit(context.Background(), nil, in)
		if err != nil {
			t.Fatalf("follow %q: %v", pointer, err)
		}
		all = append(all, out.Items...)
		if out.NextCursor == "" {
			break
		}
		in.Cursor = out.NextCursor
	}
	return all
}

func TestElisionPointers_ReturnAtLeastOmittedContent(t *testing.T) {
	fb, srv := newFakeBackend(t)
	runID := uuid.New()
	r := newResolver(srv, nil)

	// The stored chain the pointers must reach: 20 cost_recorded rows plus the
	// two implement-review categories.
	var chain []AuditEntry
	for i := 0; i < 20; i++ {
		chain = append(chain, AuditEntry{ID: uuid.NewString(), Sequence: int64(i + 1), RunID: runID.String(), Category: "cost_recorded"})
	}
	chain = append(chain,
		AuditEntry{ID: uuid.NewString(), Sequence: 21, RunID: runID.String(), Category: "implement_reviewed"},
		AuditEntry{ID: uuid.NewString(), Sequence: 22, RunID: runID.String(), Category: "implement_reviewed"},
		AuditEntry{ID: uuid.NewString(), Sequence: 23, RunID: runID.String(), Category: "implement_review_skipped"},
	)
	fb.perRunAuditByRun[runID] = chain

	fixture := maximalRunStatusOutput(runID.String())
	// Mirror the audit rows the response carries onto the stored chain's ids,
	// so containment is checkable by id.
	fixture.RecentAudit = nil
	for i := 19; i >= 0; i-- { // time-descending, as the handler returns
		fixture.RecentAudit = append(fixture.RecentAudit, chain[i])
	}

	// --- T4: the union of the two category-exact pointers covers every
	// dropped implement_reviews row (equality holds here, asserted as a bonus).
	out := fixture
	led := &elisionLedger{budget: 1, tier: "T4"}
	tierImplementReviews(&out, runID.String(), led)
	if len(led.entries) != 2 {
		t.Fatalf("T4 emitted %d entries, want one per originating category", len(led.entries))
	}
	union := map[string]bool{}
	for _, e := range led.entries {
		for _, row := range followAuditPointer(t, r, e.pointer) {
			union[row.ID] = true
		}
	}
	for _, want := range chain[20:] {
		if !union[want.ID] {
			t.Errorf("T4 pointer union misses stored implement-review row %s (%s)", want.ID, want.Category)
		}
	}
	if len(union) != 3 {
		t.Errorf("T4 pointer union returned %d rows, want exactly the 3 implement-review rows (equality bonus)", len(union))
	}

	// --- T6: the anchored pointer's exhaustively-paginated result contains
	// every dropped recent_audit entry.
	out6 := fixture
	led6 := &elisionLedger{budget: 1, tier: "T6"}
	tierRecentAuditDrop(&out6, runID.String(), led6)
	got6 := map[string]bool{}
	for _, row := range followAuditPointer(t, r, led6.entries[0].pointer) {
		got6[row.ID] = true
	}
	for _, want := range fixture.RecentAudit {
		if !got6[want.ID] {
			t.Errorf("T6 pointer misses dropped recent_audit entry %s", want.ID)
		}
	}

	// --- T5: superset-containment PLUS the explicit negative that the pointer
	// is NOT a bare newest-N — the returned set includes an entry OLDER than
	// the retained window, which a newest-N call would exclude.
	out5 := fixture
	led5 := &elisionLedger{budget: 1, tier: "T5"}
	tierRecentAuditCap(&out5, runID.String(), led5)
	retained := map[string]bool{}
	for _, e := range out5.RecentAudit {
		retained[e.ID] = true
	}
	var oldestRetainedSeq int64 = 1 << 62
	for _, e := range out5.RecentAudit {
		if e.Sequence < oldestRetainedSeq {
			oldestRetainedSeq = e.Sequence
		}
	}
	got5 := followAuditPointer(t, r, led5.entries[0].pointer)
	ids5 := map[string]bool{}
	sawOlder := false
	for _, row := range got5 {
		ids5[row.ID] = true
		if row.Sequence < oldestRetainedSeq {
			sawOlder = true
		}
	}
	for _, want := range fixture.RecentAudit {
		if !ids5[want.ID] {
			t.Errorf("T5 pointer is not a superset: misses %s", want.ID)
		}
	}
	if !sawOlder {
		t.Error("T5 pointer returned no entry older than the retained window — it behaves like a bare newest-N, which would exclude exactly the dropped set")
	}
}

// ---------------------------------------------------------------------------
// the floor's derived union
// ---------------------------------------------------------------------------

func TestFloor_StoredAggregateUnionCoversEverySurface(t *testing.T) {
	runID := uuid.NewString()

	// DERIVE the expected union: bound a maximal fixture through the tiers AND
	// the skeleton and collect the Pointer of every stored / oversized-capable
	// entry the ladder actually emits. Never a fixed list.
	want := map[string]bool{}
	collect := func(el *Elisions) {
		for _, f := range el.Fields {
			if f.Aggregate || f.Pointer == "" {
				continue
			}
			if f.Class == string(classStored) || f.Class == string(classOversizedCapable) {
				want[f.Pointer] = true
			}
		}
	}
	out := maximalRunStatusOutput(runID)
	led := &elisionLedger{budget: 1, source: sourceDefault}
	for _, tier := range runStatusTiers {
		led.tier = tier.name
		tier.apply(&out, runID, led)
	}
	tiered, err := led.wire()
	if err != nil {
		t.Fatalf("tier wire: %v", err)
	}
	collect(tiered)
	sk, _, err := skeletonRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(1))
	if err != nil {
		t.Fatalf("skeleton: %v", err)
	}
	collect(sk.Elisions)
	if len(want) == 0 {
		t.Fatal("derived no surfaces from the ladder — the derivation itself is vacuous")
	}

	floor, err := minimalRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(minimalRunStatusMaxBytes))
	if err != nil {
		t.Fatalf("floor: %v", err)
	}
	if floor.Elisions == nil || len(floor.Elisions.Fields) != 2 {
		t.Fatalf("floor must emit EXACTLY two aggregate entries, got %#v", floor.Elisions)
	}
	var stored, computed *ElidedField
	for i := range floor.Elisions.Fields {
		f := &floor.Elisions.Fields[i]
		if !f.Aggregate {
			t.Errorf("floor entry %q is not marked aggregate", f.Field)
		}
		switch f.Class {
		case string(classStored):
			stored = f
		case string(classComputed):
			computed = f
		}
	}
	if stored == nil || computed == nil {
		t.Fatalf("floor must emit one stored aggregate (with a pointer) and one computed aggregate (without): %#v", floor.Elisions.Fields)
	}
	if computed.Pointer != "" {
		t.Errorf("floor computed aggregate carries pointer %q", computed.Pointer)
	}
	have := map[string]bool{}
	for _, part := range strings.Split(stored.Pointer, "; ") {
		have[part] = true
	}
	for surface := range want {
		if !have[surface] {
			t.Errorf("floor stored aggregate union omits surface %q that a tier/skeleton entry named", surface)
		}
	}
	// The gate-view surface specifically: this is the one the prior
	// hand-written floor union omitted while T9 assigned it.
	if !have[pointerGateView(runID).String()] {
		t.Errorf("floor stored aggregate union omits the gate-view surface %q that T9's run.concerns pointer uses", pointerGateView(runID).String())
	}
}

// ---------------------------------------------------------------------------
// the skeleton
// ---------------------------------------------------------------------------

func TestSkeleton_ItemisesNestedOmissions(t *testing.T) {
	runID := uuid.NewString()
	sk, _, err := skeletonRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(1))
	if err != nil {
		t.Fatalf("skeleton: %v", err)
	}
	byField := map[string]ElidedField{}
	for _, f := range sk.Elisions.Fields {
		if f.Aggregate {
			t.Errorf("skeleton entry %q is an aggregate — aggregates are the floor tier's exception, not the skeleton's", f.Field)
		}
		byField[f.Field] = f
	}
	// Each nested omission inside a RETAINED composite gets its OWN entry with
	// its own class and pointer, never one opaque line.
	for _, path := range []string{
		"run.issue_context", "run.concerns.items", "run.review_authority",
		"run.live_validation", "stages[].executor", "next_actions.actions",
	} {
		f, ok := byField[path]
		if !ok {
			t.Errorf("skeleton did not itemise nested omission %q", path)
			continue
		}
		row := mustPath(path)
		if f.Class != string(row.Class) {
			t.Errorf("skeleton entry %q class = %q, want the table's %q", path, f.Class, row.Class)
		}
		if row.Class == classComputed && f.Pointer != "" {
			t.Errorf("skeleton entry %q is computed but carries pointer %q", path, f.Pointer)
		}
		if row.Class != classComputed && f.Pointer == "" {
			t.Errorf("skeleton entry %q is %s but carries no pointer", path, row.Class)
		}
	}
	// The diagnosis core survives.
	if sk.Run.ID == "" || sk.Run.State == "" || len(sk.Stages) == 0 || sk.NextActions == nil {
		t.Errorf("skeleton dropped the diagnosis core: %#v", sk)
	}
}

// TestSkeletonTier_IsReachedAndItemisesPerField is C2's vehicle. Convergence
// CANNOT falsify the skeleton (the floor alone still converges and still
// satisfies the final-size assertion), so the skeleton's counterfactual asserts
// it is REACHED at a mid-band budget and itemises PER FIELD.
func TestSkeletonTier_IsReachedAndItemisesPerField(t *testing.T) {
	runID := uuid.NewString()
	// The band is DERIVED, not guessed: exactly the skeleton's own size. It is
	// necessarily above the floor and below anything T1..T9 can reach on the
	// maximal fixture (the skeleton is a strict subset of the T9 residue).
	sk, _, err := skeletonRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(1<<30))
	if err != nil {
		t.Fatalf("skeleton: %v", err)
	}
	band := len(mustMarshal(t, sk))
	if band <= minimalRunStatusMaxBytes {
		t.Fatalf("derived band %d is not above the floor %d", band, minimalRunStatusMaxBytes)
	}
	out, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(band))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if out.Elisions == nil {
		t.Fatal("no elisions block at the skeleton band")
	}
	if out.Elisions.Tier != "skeleton" {
		t.Fatalf("tier at budget %d = %q, want skeleton (the ladder must not fall straight through to the floor)", band, out.Elisions.Tier)
	}
	if len(out.Elisions.Fields) <= 2 {
		t.Fatalf("skeleton emitted %d entries, want strictly more than the floor's two aggregates", len(out.Elisions.Fields))
	}
	for _, f := range out.Elisions.Fields {
		if f.Aggregate {
			t.Errorf("skeleton entry %q is aggregated rather than itemised", f.Field)
		}
	}
}

// ---------------------------------------------------------------------------
// Q2-REVISED: the projection's validation pass and its sole-producer discipline
// ---------------------------------------------------------------------------

// TestWireElisions_ValidationRejectsMalformed drives every named invariant
// THROUGH wire() — the projection path — by seeding the internal accumulator
// with a raw elidedField composite literal (legal in-package, and exactly the
// residual hole the validation pass closes, since no constructor can spell
// these). Deleting the validateWireElisions call from wire() turns every case
// red, which a test calling validateWireElisions directly could not show.
func TestWireElisions_ValidationRejectsMalformed(t *testing.T) {
	cases := []struct {
		name      string
		tier      string
		entry     elidedField
		wantInErr []string
	}{
		{
			name: "computed with a pointer", tier: "T1",
			entry:     elidedField{field: "cost", reason: "r", class: classComputed, pointer: "GET /v0/runs/x"},
			wantInErr: []string{"cost", "computed"},
		},
		{
			name: "stored without a pointer", tier: "T3",
			entry:     elidedField{field: "security_findings", reason: "r", class: classStored},
			wantInErr: []string{"security_findings", "stored"},
		},
		{
			name: "oversized_capable pointing at a fishhawk_* tool", tier: "T8",
			entry:     elidedField{field: "run.issue_context", reason: "r", class: classOversizedCapable, pointer: "fishhawk_get_run_status(run_id=x)"},
			wantInErr: []string{"run.issue_context", "oversized_capable"},
		},
		{
			name: "pointer naming an include_ flag", tier: "T5",
			entry:     elidedField{field: "recent_audit", reason: "r", class: classStored, pointer: "fishhawk_get_run_status(include_audit_hashes=true)"},
			wantInErr: []string{"recent_audit", "include_"},
		},
		{
			name: "unrecognised class", tier: "T2",
			entry:     elidedField{field: "children_status", reason: "r", class: elisionClass("summarised")},
			wantInErr: []string{"children_status", "summarised"},
		},
		{
			name: "negative omitted_count", tier: "T3",
			entry:     elidedField{field: "security_findings", reason: "r", class: classStored, pointer: "GET /v0/runs/x", omittedCount: -1},
			wantInErr: []string{"security_findings", "omitted_count"},
		},
		{
			name: "aggregate above the floor", tier: "T9",
			entry:     elidedField{field: "*", reason: "r", class: classComputed, aggregate: true},
			wantInErr: []string{"aggregate", "T9"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			led := &elisionLedger{budget: 1024, source: sourceDefault, tier: tc.tier}
			led.add(tc.entry)
			got, err := led.wire()
			if err == nil {
				t.Fatalf("wire() accepted a malformed DTO (%s) and returned %#v", tc.name, got)
			}
			// REJECTED, not silently normalised: no repaired DTO comes back.
			if got != nil {
				t.Errorf("wire() returned a value alongside the error — the malformed DTO must be rejected, not normalised: %#v", got)
			}
			for _, want := range tc.wantInErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q — the error must identify the offending field and the invariant it broke", err, want)
				}
			}
		})
	}
	// The well-formed control: the same path accepts a constructor-built entry.
	ok := &elisionLedger{budget: 1024, source: sourceDefault, tier: "T3"}
	ok.add(newStoredElision("security_findings", "r", pointerListAudit("run", "implement_security_findings", 0), 3))
	if _, err := ok.wire(); err != nil {
		t.Fatalf("wire() rejected a well-formed ledger: %v", err)
	}
}

// TestWireElisions_RejectsBadBudgetSource pins the #2509 arm of
// validateWireElisions: a DTO must not ship a budget_source it cannot name. An
// empty or unrecognised source is rejected, and every legitimate source is
// accepted. The bad inputs are seeded BY CONSTRUCTION (a ledger with an empty /
// bogus source carrying an otherwise well-formed entry), so the RED lands on the
// source assertion, not on fixture setup. Deleting the validBudgetSource arm from
// validateWireElisions turns the reject cases green.
func TestWireElisions_RejectsBadBudgetSource(t *testing.T) {
	entry := newStoredElision("security_findings", "r", pointerListAudit("run", "implement_security_findings", 0), 3)

	bad := []struct {
		name   string
		source budgetSource
	}{
		{"empty", budgetSource("")},
		{"unrecognised", budgetSource("guessed")},
	}
	for _, tc := range bad {
		t.Run("rejects "+tc.name, func(t *testing.T) {
			led := &elisionLedger{budget: 1024, source: tc.source, tier: "T3"}
			led.add(entry)
			got, err := led.wire()
			if err == nil {
				t.Fatalf("wire() accepted budget_source %q and returned %#v", tc.source, got)
			}
			if got != nil {
				t.Errorf("wire() returned a value alongside the error — a bad source must be rejected, not normalised: %#v", got)
			}
			if !strings.Contains(err.Error(), "budget_source") {
				t.Errorf("error %q does not name budget_source", err)
			}
		})
	}

	for _, src := range []budgetSource{sourceAdvertised, sourceAdvertisedBelowFloor, sourceConfigured, sourceDefault} {
		t.Run("accepts "+string(src), func(t *testing.T) {
			led := &elisionLedger{budget: 1024, source: src, tier: "T3"}
			led.add(entry)
			if _, err := led.wire(); err != nil {
				t.Fatalf("wire() rejected valid budget_source %q: %v", src, err)
			}
		})
	}
}

// TestElisions_ReportsBudgetSource is the DONE-MEANS pin (#2509): the source
// string is a config-shaped value the compiler cannot enforce, so a comment-only
// or no-op touch of the resolver would pass a scope-presence check but fail here.
// It drives the real run-status ladder for each source and asserts the LITERAL
// shipped string on the marshalled wire.
func TestElisions_ReportsBudgetSource(t *testing.T) {
	runID := uuid.NewString()
	cases := []struct {
		budget responseBudget
		want   string
	}{
		{responseBudget{bytes: 16384, source: sourceAdvertised}, `"budget_source":"advertised"`},
		{responseBudget{bytes: 20000, source: sourceConfigured}, `"budget_source":"configured"`},
		{responseBudget{bytes: 16384, source: sourceDefault}, `"budget_source":"default"`},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			out, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, tc.budget)
			if err != nil {
				t.Fatalf("bound: %v", err)
			}
			if out.Elisions == nil {
				t.Fatalf("the fixture did not exceed %d bytes — no elisions block to carry the source", tc.budget.bytes)
			}
			raw := string(mustMarshal(t, out))
			if !strings.Contains(raw, tc.want) {
				t.Errorf("marshalled wire does not carry %s: %s", tc.want, raw)
			}
		})
	}
}

// TestElisions_BelowFloorAdvertisement_ReportsCouldNotHonour is BINDING
// CONDITION 2's wire-level pin: when a client advertises a limit below the
// convergence floor, the response emits the floor but SAYS on the wire that the
// smaller advertisement could not be honoured (source advertised_below_floor,
// never advertised). The budget is resolved through the REAL ladder from an
// advertised-below-floor capability, so the whole path is exercised.
func TestElisions_BelowFloorAdvertisement_ReportsCouldNotHonour(t *testing.T) {
	runID := uuid.NewString()
	budget := resolveResponseBudget(extCaps(float64(2048)), noEnv)
	out, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, budget)
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if out.Elisions == nil {
		t.Fatal("the fixture did not exceed the floor budget — no elisions block")
	}
	raw := string(mustMarshal(t, out))
	if !strings.Contains(raw, `"budget_source":"advertised_below_floor"`) {
		t.Errorf("wire must say the advertised limit could not be honoured (advertised_below_floor): %s", raw)
	}
	if strings.Contains(raw, `"budget_source":"advertised"`) {
		t.Errorf("wire must NOT present the floor as honouring the smaller advertisement: %s", raw)
	}
	if out.Elisions.Budget != mcpConvergenceFloorBytes {
		t.Errorf("effective budget = %d, want the floor %d", out.Elisions.Budget, mcpConvergenceFloorBytes)
	}
}

// wireDTOLiteral is one detected wire-DTO composite-literal construction: the
// exported name it builds (Elisions or ElidedField) and the position of the
// literal that builds it.
type wireDTOLiteral struct {
	Name string
	Pos  token.Pos
}

// wireDTOTypeIdent returns the *ast.Ident naming Elisions or ElidedField when
// expr is a type expression that DENOTES one of those wire DTOs, unwrapping the
// compound type forms that still denote it — *ast.ArrayType (its Elt, covering
// []T, [N]T and [...]T alike, since the array length lives in ArrayType.Len and
// never on Elt), *ast.StarExpr (its X) and *ast.ParenExpr (its X) — recursively.
// It returns nil for every other shape.
//
// It deliberately does NOT unwrap *ast.MapType: a map type has two independent
// positions (key and value) that must be attributed separately, so the walk
// resolves Key and Value with their own wireDTOTypeIdent calls rather than
// collapsing them here. It also does NOT match *ast.SelectorExpr: a package
// cannot import itself, so a qualified `mcpserver.Elisions` cannot occur in this
// package's own production files, and matching a bare Sel name would fire on an
// unrelated package's identically-named type.
//
// The name comparison is an EXACT match against "Elisions" / "ElidedField", so
// the unexported accumulator `elidedField` never matches.
func wireDTOTypeIdent(expr ast.Expr) *ast.Ident {
	switch t := expr.(type) {
	case *ast.Ident:
		if t.Name == "Elisions" || t.Name == "ElidedField" {
			return t
		}
		return nil
	case *ast.ArrayType:
		return wireDTOTypeIdent(t.Elt)
	case *ast.StarExpr:
		return wireDTOTypeIdent(t.X)
	case *ast.ParenExpr:
		return wireDTOTypeIdent(t.X)
	default:
		return nil
	}
}

// wireDTOLiterals walks n and returns one finding per wire-DTO construction:
// every CompositeLit whose OWN Type denotes Elisions / ElidedField (via
// wireDTOTypeIdent), PLUS every IMPLICITLY-typed (nil-Type) inner element whose
// position inside a matched container resolves to the DTO. An explicitly-typed
// inner element (`[]ElidedField{ElidedField{...}}`) is emitted EXACTLY ONCE, by
// its own standalone visit; parent-driven emission is restricted to nil-Type
// children, so no literal is double-counted. A nil-Type literal never matches
// the standalone path, so it is only ever reached through its parent's type.
func wireDTOLiterals(n ast.Node) []wireDTOLiteral {
	var found []wireDTOLiteral
	ast.Inspect(n, func(node ast.Node) bool {
		lit, ok := node.(*ast.CompositeLit)
		if !ok {
			return true
		}
		// Standalone match: the literal's own spelled type denotes the DTO.
		if id := wireDTOTypeIdent(lit.Type); id != nil {
			found = append(found, wireDTOLiteral{Name: id.Name, Pos: lit.Pos()})
		}
		// Parent-driven emission for this literal's implicitly-typed elements,
		// resolved against its own spelled container type. A nil-Type literal
		// carries no container type here, so its elements are attributed when
		// its PARENT walks it (below), never twice.
		if lit.Type != nil {
			found = append(found, wireDTOImplicitElements(lit, lit.Type)...)
		}
		return true
	})
	return found
}

// wireDTOImplicitElements emits a finding for every implicitly-typed (nil-Type)
// element of lit whose position resolves to the DTO, given lit's resolved
// container type typ (its spelled type at the top level, or the element/key/
// value type handed down by a parent during recursion). It recurses into a
// nil-Type element that is itself a container so a nested
// `[][]ElidedField{{{...}}}` — or a pointer-wrapped one, `[]*[]ElidedField{{{...}}}`,
// since unwrapContainerType strips the pointer — is covered.
func wireDTOImplicitElements(lit *ast.CompositeLit, typ ast.Expr) []wireDTOLiteral {
	var out []wireDTOLiteral
	switch t := unwrapContainerType(typ).(type) {
	case *ast.ArrayType:
		for _, el := range lit.Elts {
			e := el
			// An indexed element (`[]T{0: {...}}`) is a KeyValueExpr whose Key
			// is the index, not a DTO position; the Value is the element.
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				e = kv.Value
			}
			out = append(out, resolveImplicitElement(e, t.Elt)...)
		}
	case *ast.MapType:
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue // a well-formed map literal element is always a KeyValueExpr
			}
			out = append(out, resolveImplicitElement(kv.Key, t.Key)...)
			out = append(out, resolveImplicitElement(kv.Value, t.Value)...)
		}
	}
	return out
}

// resolveImplicitElement attributes one container element, occupying a position
// of type elemType, to the DTO — but ONLY when it is an implicitly-typed
// (nil-Type) composite literal. An explicitly-typed element is left to its own
// standalone visit (so it is not double-counted), and a non-composite element
// (a BasicLit map key, an identifier) is not a construction at all.
func resolveImplicitElement(expr ast.Expr, elemType ast.Expr) []wireDTOLiteral {
	cl, ok := expr.(*ast.CompositeLit)
	if !ok || cl.Type != nil {
		return nil
	}
	var out []wireDTOLiteral
	if id := wireDTOTypeIdent(elemType); id != nil {
		out = append(out, wireDTOLiteral{Name: id.Name, Pos: cl.Pos()})
	}
	// The element's resolved type is elemType; descend into ITS implicitly-typed
	// elements against elemType's element/key/value positions.
	out = append(out, wireDTOImplicitElements(cl, elemType)...)
	return out
}

// unwrapContainerType strips enclosing parentheses AND pointer indirections from
// a type expression, so a `([]ElidedField)` or a `*[]ElidedField` container is
// resolved to the underlying array/map like the bare form. The pointer unwrap
// mirrors wireDTOTypeIdent's *ast.StarExpr handling, so parent-driven recursion
// descends THROUGH a pointer-wrapped container — `[]*[]ElidedField{{{...}}}`,
// whose element type is `*[]ElidedField` — into its implicitly-typed inner
// elements instead of stopping at the pointer and leaving the inner literal
// undetected (#2652 fix-up).
func unwrapContainerType(expr ast.Expr) ast.Expr {
	for {
		switch t := expr.(type) {
		case *ast.ParenExpr:
			expr = t.X
		case *ast.StarExpr:
			expr = t.X
		default:
			return expr
		}
	}
}

// TestProjectionIsSoleProducerOfWireDTO parses the package's own .go files and
// asserts every composite literal that constructs Elisions / ElidedField outside
// the test files occurs inside wire() / wireField(). A second construction path
// bypasses the validation pass, so it must fail the package tests.
//
// The enclosing scope is DERIVED from the declaration being walked — each
// *ast.FuncDecl is inspected under its own name and every other declaration
// under an explicit package-scope sentinel — never carried across the walk in a
// mutable variable. The earlier shape remembered the last FuncDecl name and
// never restored it, so a package-level literal placed lexically AFTER wire()
// or wireField() inherited an allowed producer's name and escaped the pin
// outright, while one placed after any other function was reported against the
// wrong scope (#2514). Deriving the scope per declaration makes the verdict
// independent of source position.
//
// What this pin matches (widened in #2652 from the earlier Ident-only form):
// via wireDTOLiterals it now catches an Ident-typed literal (`Elisions{...}`,
// `ElidedField{...}`, and the address-of `&Elisions{...}` — the & is a
// UnaryExpr wrapping a still Ident-typed literal), AND a COMPOUND-typed
// construction that denotes the DTO — a slice or array `[]ElidedField{...}` /
// `[2]ElidedField{...}` / `[...]ElidedField{...}` (all reached through the
// *ast.ArrayType.Elt traversal), a pointer element `[]*Elisions{...}`, and a
// map whose key or value is the DTO — PLUS each of those literals'
// implicitly-typed inner elements (`{{Field: "f"}}`, which carry no Type node),
// resolved from the PARENT literal's element/key/value type rather than guessed.
// The map key/value positions are resolved independently, so
// `map[ElidedField]otherType{...}` attributes the key and NOT the value.
//
// What it still does NOT establish: it does not resolve a NAMED container type
// (`type Fields []ElidedField; Fields{{...}}`), a type-aliased or cross-package
// spelling, or a literal built through reflection. So the guarantee is "a
// syntactically-denoted construction is caught at any source position", not "no
// second construction path exists". This remains defence in depth behind
// validateWireElisions, which runs inside wire() and is the actual runtime
// enforcement — not a substitute for it.
func TestProjectionIsSoleProducerOfWireDTO(t *testing.T) {
	// Enumerate the package DIRECTORY, not a hard-coded file list, or a newly
	// added production file would escape the discipline. _test.go files are
	// excluded: this file's malformed-DTO cases build the wire type by
	// composite literal ON PURPOSE.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("ReadDir: %v", err)
	}
	fset := token.NewFileSet()
	allowedIn := map[string]bool{"wire": true, "wireField": true}
	// packageScope is the scope reported for a literal in a non-function
	// declaration. Its spelling is deliberately not a valid Go identifier, so it
	// can never equal an *ast.Ident name and can never collide its way back into
	// allowedIn.
	const packageScope = "package scope (no enclosing function)"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		// reportDecl flags every wire-DTO literal under decl, attributed to
		// enclosing. The scope is a PARAMETER — supplied by the caller from the
		// declaration it is about to walk — so it cannot leak from one
		// declaration into the next.
		reportDecl := func(decl ast.Node, enclosing string) {
			if allowedIn[enclosing] {
				return
			}
			for _, lit := range wireDTOLiterals(decl) {
				t.Errorf("%s:%d: %s composite literal in %q — the wire DTO has exactly one producer, (elisionLedger).wire()/wireField(), so every entry passes validateWireElisions",
					name, fset.Position(lit.Pos).Line, lit.Name, enclosing)
			}
		}
		for _, decl := range file.Decls {
			// Inspect the whole FuncDecl, not just its Body: a nil body (an
			// assembly or external declaration) is skipped by ast.Walk, and
			// receivers, signatures and nested func literals stay attributed to
			// the named function that owns them.
			if fn, ok := decl.(*ast.FuncDecl); ok {
				reportDecl(fn, fn.Name.Name)
				continue
			}
			reportDecl(decl, packageScope)
		}
	}
}

// TestWireDTOLiteralMatch is the committed pin for the widened matcher: it
// parses a small snippet per row and asserts the EXACT set of findings (name +
// line), not a non-zero count. It is the counterfactual vehicle for #2652 —
// deleting the compound-type unwrapping reddens the slice/array/map/pointer
// rows, deleting the nil-Type parent-resolution branch reddens the
// implicit-inner-element rows — and it carries the positive control as negative
// rows that must return ZERO findings, so an over-matching widening reddens here
// as well as on the live package walk.
func TestWireDTOLiteralMatch(t *testing.T) {
	fset := token.NewFileSet()
	findings := func(t *testing.T, src string) []string {
		t.Helper()
		f, err := parser.ParseFile(fset, "snippet.go", src, 0)
		if err != nil {
			t.Fatalf("parse: %v\nsource:\n%s", err, src)
		}
		var out []string
		for _, lit := range wireDTOLiterals(f) {
			out = append(out, fmt.Sprintf("%s:%d", lit.Name, fset.Position(lit.Pos).Line))
		}
		sort.Strings(out)
		return out
	}
	cases := []struct {
		name string
		src  string
		want []string
	}{
		// --- positive: each widened spelling as its OWN row --------------------
		{
			name: "bare Elisions",
			src:  "package p\nvar _ = Elisions{}\n",
			want: []string{"Elisions:2"},
		},
		{
			name: "address-of Elisions",
			src:  "package p\nvar _ = &Elisions{}\n",
			want: []string{"Elisions:2"},
		},
		{
			name: "slice: outer plus implicit inner",
			src:  "package p\nvar _ = []ElidedField{\n\t{Field: \"f\"},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3"},
		},
		{
			// Condition 2: an EXPLICITLY-typed inner element is reported EXACTLY
			// ONCE — by its own standalone visit, never also via the parent walk.
			// A third finding here would be the double-count defect.
			name: "slice: explicitly-typed inner element reported exactly once",
			src:  "package p\nvar _ = []ElidedField{\n\tElidedField{Field: \"f\"},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3"},
		},
		{
			name: "fixed array: outer plus two implicit inners",
			src:  "package p\nvar _ = [2]ElidedField{\n\t{},\n\t{},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3", "ElidedField:4"},
		},
		{
			// Condition 1: [...]T is a fixed array whose Ellipsis is ArrayType.Len,
			// NOT its Elt — so it is covered by the SAME ArrayType.Elt traversal as
			// []T and [N]T, not by any *ast.Ellipsis branch (there is none).
			name: "ellipsis-length array: covered by the Elt traversal",
			src:  "package p\nvar _ = [...]ElidedField{\n\t{Field: \"f\"},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3"},
		},
		{
			name: "pointer element slice",
			src:  "package p\nvar _ = []*Elisions{\n\t{Tier: \"x\"},\n}\n",
			want: []string{"Elisions:2", "Elisions:3"},
		},
		{
			name: "map value: outer not reported, value resolves",
			src:  "package p\nvar _ = map[string]ElidedField{\n\t\"a\": {Field: \"f\"},\n}\n",
			want: []string{"ElidedField:3"},
		},
		{
			name: "map key resolves",
			src:  "package p\nvar _ = map[ElidedField]string{\n\t{Field: \"f\"}: \"x\",\n}\n",
			want: []string{"ElidedField:3"},
		},
		{
			name: "nested slice recursion",
			src:  "package p\nvar _ = [][]ElidedField{\n\t{\n\t\t{Field: \"f\"},\n\t},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3", "ElidedField:4"},
		},
		{
			// Pins the KeyValueExpr branch of the ArrayType arm: an INDEXED element
			// (`[]T{0: {...}}`) is a KeyValueExpr whose Key is the integer index (not
			// a DTO position) and whose Value is the element. Dropping the kv.Value
			// unwrap, or resolving kv.Key against the element type, reddens this row.
			name: "indexed slice element resolves via KeyValueExpr value",
			src:  "package p\nvar _ = []ElidedField{\n\t0: {Field: \"f\"},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3"},
		},
		{
			// Pins unwrapContainerType's ParenExpr stripping on the RECURSION path:
			// the outer Elt is a parenthesized `([]ElidedField)`, so descending into
			// the middle literal's own element requires stripping the paren. Without
			// it the innermost `{Field: "f"}` (line 4) goes undetected.
			name: "parenthesized element type recurses",
			src:  "package p\nvar _ = []([]ElidedField){\n\t{\n\t\t{Field: \"f\"},\n\t},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3", "ElidedField:4"},
		},
		{
			// Pins the #2652 fix-up: the outer Elt is a pointer-wrapped container
			// `*[]ElidedField`, so parent-driven recursion must strip the pointer to
			// reach the innermost `{Field: "f"}` (line 4). Without the StarExpr unwrap
			// in unwrapContainerType the recursion stops at the pointer and drops it.
			name: "pointer-wrapped container recurses",
			src:  "package p\nvar _ = []*[]ElidedField{\n\t{\n\t\t{Field: \"f\"},\n\t},\n}\n",
			want: []string{"ElidedField:2", "ElidedField:3", "ElidedField:4"},
		},
		// --- negative: the positive control (must return ZERO findings) --------
		{
			// The unexported accumulator — production carries five of these. The
			// exact-case name comparison is what separates it from ElidedField.
			name: "unexported accumulator elidedField",
			src:  "package p\nvar _ = []elidedField{\n\t{field: \"f\"},\n}\n",
			want: nil,
		},
		{
			name: "unrelated string slice",
			src:  "package p\nvar _ = []string{\"a\"}\n",
			want: nil,
		},
		{
			name: "unrelated empty-struct map",
			src:  "package p\nvar _ = map[string]struct{}{}\n",
			want: nil,
		},
		{
			name: "unrelated reflect.Type map",
			src:  "package p\nvar _ = map[string]reflect.Type{}\n",
			want: nil,
		},
		{
			name: "unrelated named-struct slice",
			src:  "package p\nvar _ = []otherType{\n\t{},\n}\n",
			want: nil,
		},
		{
			// Precision row: the KEY position is the DTO and fires; the VALUE
			// position is unrelated and must NOT — the resolution is per position,
			// not "any implicit literal under a DTO-mentioning container".
			name: "map key fires, value does not",
			src:  "package p\nvar _ = map[ElidedField]otherType{\n\t{Field: \"f\"}: {},\n}\n",
			want: []string{"ElidedField:3"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := findings(t, tc.src)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("wireDTOLiterals findings = %v, want %v\nsource:\n%s", got, tc.want, tc.src)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// the budget override contract
// ---------------------------------------------------------------------------

func TestMCPResponseByteBudget_OverrideBranches(t *testing.T) {
	// The table is keyed by the getenv FUNC, not by an env map, so the
	// nil-getenv guard gets its own NAMED branch case. A resolver built without
	// an environment seam reaches it, and envFuncFromMap(nil) — a non-nil func
	// over an empty map — does NOT: it lands on "absent" instead, leaving the
	// nil branch covered only incidentally.
	cases := []struct {
		name   string
		getenv func(string) string
		want   int
	}{
		{"nil getenv (no environment seam at all)", nil, mcpResponseByteBudgetDefault},
		{"absent", envFuncFromMap(nil), mcpResponseByteBudgetDefault},
		{"unparseable", envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "not-a-number"}), mcpResponseByteBudgetDefault},
		{"non-positive", envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "0"}), mcpResponseByteBudgetDefault},
		{"negative", envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "-4096"}), mcpResponseByteBudgetDefault},
		{"honoured above the floor", envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "8192"}), 8192},
		{"clamped below the floor", envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "1024"}), minimalRunStatusMaxBytes},
		// The GENERAL var (#2510) and its precedence over the LEGACY spelling.
		// Both are operator-observable behaviour, so each gets its own named
		// branch rather than riding on the legacy cases above.
		{"general var honoured", envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "8192"}), 8192},
		{"general var clamped below the floor", envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "1024"}), mcpConvergenceFloorBytes},
		{"general var unparseable", envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "not-a-number"}), mcpResponseByteBudgetDefault},
		{"general var non-positive", envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: "0"}), mcpResponseByteBudgetDefault},
		{"both set: the general var WINS", envFuncFromMap(map[string]string{
			mcpResponseBudgetEnvVar: "8192", runStatusBudgetEnvVar: "20000"}), 8192},
		{"general var present but invalid does NOT fall through to the legacy var", envFuncFromMap(map[string]string{
			mcpResponseBudgetEnvVar: "not-a-number", runStatusBudgetEnvVar: "20000"}), mcpResponseByteBudgetDefault},
		{"legacy var alone still honoured (no operator override breaks)", envFuncFromMap(map[string]string{
			runStatusBudgetEnvVar: "20000"}), 20000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := mcpResponseByteBudget(tc.getenv); got != tc.want {
				t.Fatalf("mcpResponseByteBudget = %d, want %d", got, tc.want)
			}
		})
	}

	// The clamp is REPORTED, not merely applied: the elisions block's Budget
	// field carries the EFFECTIVE (post-clamp) value, so the wire never claims
	// a bound the ladder did not honour.
	runID := uuid.NewString()
	effective := mcpResponseByteBudget(envFuncFromMap(map[string]string{runStatusBudgetEnvVar: "1024"}))
	out, err := boundRunStatusOutput(maximalRunStatusOutput(runID), runID, fixedBudget(effective))
	if err != nil {
		t.Fatalf("bound: %v", err)
	}
	if out.Elisions == nil {
		t.Fatal("no elisions block at the clamped budget")
	}
	if out.Elisions.Budget != minimalRunStatusMaxBytes {
		t.Errorf("elisions.budget = %d, want the EFFECTIVE clamped budget %d (never the requested 1024)", out.Elisions.Budget, minimalRunStatusMaxBytes)
	}
}

// TestFloorSurfaceUnion_IsSortedAndDeduplicated pins the fold's determinism.
func TestFloorSurfaceUnion_IsSortedAndDeduplicated(t *testing.T) {
	u := floorStoredSurfaceUnion("run-1")
	if !sort.StringsAreSorted(u) {
		t.Errorf("floor union is not sorted: %v", u)
	}
	seen := map[string]bool{}
	for _, s := range u {
		if seen[s] {
			t.Errorf("floor union carries duplicate surface %q", s)
		}
		seen[s] = true
	}
}

// TestNextActionsSignature_RidesTheLadderConstantSize pins the byte-budget
// coupling of the failure-signature block (#1703).
//
// The block is classified tierNever, so no tier elides it — which is safe only
// because it cannot grow with run data. Three assertions:
//
//   - the maximal fixture really carries a block (otherwise every other
//     assertion here is vacuous);
//   - the DIAGNOSIS SKELETON and the CONSTANT-SIZE FLOOR both rebuild
//     next_actions without it, so neither is enlarged by it;
//   - the floor stays under its existing cap with the block present upstream.
func TestNextActionsSignature_RidesTheLadderConstantSize(t *testing.T) {
	runID := uuid.NewString()
	in := maximalRunStatusOutput(runID)
	if in.NextActions == nil || in.NextActions.Signature == nil {
		t.Fatal("the maximal fixture carries no signature block — the ladder assertions below would be vacuous")
	}

	sk, _, err := skeletonRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(1))
	if err != nil {
		t.Fatalf("skeletonRunStatus: %v", err)
	}
	if sk.NextActions == nil {
		t.Fatal("the skeleton dropped the next_actions presence marker")
	}
	if sk.NextActions.Signature != nil {
		t.Fatalf("the skeleton retained the signature block: %+v", sk.NextActions.Signature)
	}

	floor, err := minimalRunStatus(maximalRunStatusOutput(runID), runID, fixedBudget(minimalRunStatusMaxBytes))
	if err != nil {
		t.Fatalf("minimalRunStatus: %v", err)
	}
	if floor.NextActions != nil && floor.NextActions.Signature != nil {
		t.Fatalf("the floor retained the signature block: %+v", floor.NextActions.Signature)
	}
	if n := len(mustMarshal(t, floor)); n > minimalRunStatusMaxBytes {
		t.Fatalf("floor is %d bytes with a signature present upstream, cap is %d", n, minimalRunStatusMaxBytes)
	}
}

// TestNextActionsSignature_SurvivesEveryTier pins that no tier elides the
// block: it is tierNever, exactly like next_actions.state.
func TestNextActionsSignature_SurvivesEveryTier(t *testing.T) {
	runID := uuid.NewString()
	out := maximalRunStatusOutput(runID)
	led := &elisionLedger{budget: 1}
	for _, tier := range runStatusTiers {
		led.tier = tier.name
		tier.apply(&out, runID, led)
		if out.NextActions == nil || out.NextActions.Signature == nil {
			t.Fatalf("tier %s elided the signature block", tier.name)
		}
	}
}
