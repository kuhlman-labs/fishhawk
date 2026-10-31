package concern

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// TestTransition_FullMatrix pins every edge of the lifecycle machine.
// Anything not explicitly allowed must fail with InvalidTransitionError.
func TestTransition_FullMatrix(t *testing.T) {
	states := []State{
		StateRaised, StateAddressedPending, StateAddressed,
		StateReopened, StateWaived, StateSuperseded, StateDeferred,
		StateAddressedByCondition,
	}
	allowed := map[State][]State{
		StateRaised:           {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateAddressedPending: {StateAddressed, StateReopened, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateAddressed:        {StateReopened},
		StateReopened:         {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateWaived:           {},
		// E45.83 / #3618: the ONE edge out of a closed state. superseded is
		// closed (TestStateIsOpen still pins IsOpen()==false for it) but an
		// operator fix-up may re-route a retry-discarded concern back into the
		// open set. Every other exit from superseded is refused — the matrix
		// loop below asserts that for addressed / reopened / waived / deferred /
		// addressed_by_condition, and
		// TestTransition_SupersededExitsAreNarrow names each explicitly.
		StateSuperseded: {
			StateAddressedPending,
		},
		StateDeferred:             {},
		StateAddressedByCondition: {},
	}
	for _, from := range states {
		want := map[State]bool{}
		for _, to := range allowed[from] {
			want[to] = true
		}
		for _, to := range states {
			err := Transition(from, to)
			if want[to] && err != nil {
				t.Errorf("Transition(%s, %s) = %v, want allowed", from, to, err)
			}
			if !want[to] {
				if err == nil {
					t.Errorf("Transition(%s, %s) = nil, want InvalidTransitionError", from, to)
					continue
				}
				var inv InvalidTransitionError
				if !errors.As(err, &inv) {
					t.Errorf("Transition(%s, %s) error type = %T, want InvalidTransitionError", from, to, err)
				} else if inv.From != from || inv.To != to {
					t.Errorf("InvalidTransitionError = %s -> %s, want %s -> %s", inv.From, inv.To, from, to)
				}
			}
		}
	}
}

// TestTransition_UnknownState ensures a state outside the enum (the
// tolerant TEXT column admits anything) has no outgoing edges.
func TestTransition_UnknownState(t *testing.T) {
	if err := Transition(State("bogus"), StateAddressed); err == nil {
		t.Fatal("Transition from unknown state succeeded, want error")
	}
}

// TestTransition_ReopenWinsOverConfirm pins the precedence rule in BOTH
// arrival orders (#964): heterogeneous reviewers emitting conflicting
// resolutions for the same concern must resolve to reopened
// deterministically, order-independently.
func TestTransition_ReopenWinsOverConfirm(t *testing.T) {
	// Confirm landed first, then a reopen: the reopen APPLIES.
	state := StateAddressedPending
	if err := Transition(state, StateAddressed); err != nil {
		t.Fatalf("confirm from addressed_pending: %v", err)
	}
	state = StateAddressed
	if err := Transition(state, StateReopened); err != nil {
		t.Fatalf("reopen after confirm must apply (reopen wins): %v", err)
	}
	state = StateReopened

	// Reopen landed first, then a late confirm: the confirm is REJECTED
	// with a loggable error — never a silent downgrade.
	err := Transition(state, StateAddressed)
	if err == nil {
		t.Fatal("confirm after reopen succeeded, want InvalidTransitionError (reopen wins)")
	}
	var inv InvalidTransitionError
	if !errors.As(err, &inv) {
		t.Fatalf("error type = %T, want InvalidTransitionError", err)
	}
	// The concern stays reopened.
	if !state.IsOpen() {
		t.Fatal("reopened concern must remain open after the rejected late confirm")
	}
}

// TestTransition_SupersededExitsAreNarrow pins the E45.83 / #3618 edge's
// NARROWNESS one state at a time, independently of the full matrix: the single
// admitted exit from superseded is addressed_pending, and each of the five
// other enum states is refused with InvalidTransitionError carrying the
// from/to pair. Deleting the StateSuperseded -> StateAddressedPending edge
// reddens the first assertion; WIDENING the row (adding any of the five)
// reddens the corresponding table row.
func TestTransition_SupersededExitsAreNarrow(t *testing.T) {
	if err := Transition(StateSuperseded, StateAddressedPending); err != nil {
		t.Fatalf("Transition(superseded, addressed_pending) = %v, want allowed — an operator fix-up must be able to re-route a retry-discarded concern", err)
	}
	for _, to := range []State{
		StateAddressed, StateReopened, StateWaived, StateDeferred, StateAddressedByCondition,
	} {
		err := Transition(StateSuperseded, to)
		if err == nil {
			t.Errorf("Transition(superseded, %s) = nil, want InvalidTransitionError — addressed_pending is the ONLY exit", to)
			continue
		}
		var inv InvalidTransitionError
		if !errors.As(err, &inv) {
			t.Errorf("Transition(superseded, %s) error type = %T, want InvalidTransitionError", to, err)
			continue
		}
		if inv.From != StateSuperseded || inv.To != to {
			t.Errorf("InvalidTransitionError = %s -> %s, want superseded -> %s", inv.From, inv.To, to)
		}
	}
	// superseded re-entering the open set stays OPEN once routed: the edge is
	// only useful if addressed_pending is an open state.
	if !StateAddressedPending.IsOpen() {
		t.Error("addressed_pending.IsOpen() = false — the re-route edge would land a concern outside the open set")
	}
}

// TestTransition_TerminalStatesAdmitNoExit pins that the genuinely terminal
// states stayed terminal when #3618 opened superseded: waived, deferred and
// addressed_by_condition admit NO outgoing edge at all. Widening any of them
// would make a settled operator disposition re-routable, which #3618
// deliberately did not do.
func TestTransition_TerminalStatesAdmitNoExit(t *testing.T) {
	every := []State{
		StateRaised, StateAddressedPending, StateAddressed, StateReopened,
		StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition,
	}
	for _, from := range []State{StateWaived, StateDeferred, StateAddressedByCondition} {
		for _, to := range every {
			if err := Transition(from, to); err == nil {
				t.Errorf("Transition(%s, %s) = nil, want InvalidTransitionError — %s is terminal", from, to, from)
			}
		}
	}
}

func TestStateIsOpen(t *testing.T) {
	open := []State{StateRaised, StateAddressedPending, StateReopened}
	// superseded stays in the CLOSED list after E45.83 / #3618 gave it one
	// outgoing edge: it is closed-and-recoverable, not open. Widening IsOpen
	// would put every retry-discarded concern back on the open-concern
	// surfaces and into the merge-gate count.
	closed := []State{StateAddressed, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition, State("bogus")}
	for _, s := range open {
		if !s.IsOpen() {
			t.Errorf("%s.IsOpen() = false, want true", s)
		}
	}
	for _, s := range closed {
		if s.IsOpen() {
			t.Errorf("%s.IsOpen() = true, want false", s)
		}
	}
}

// ptr returns a pointer to s, for the *string ReviewerModel field.
func ptr(s string) *string { return &s }

// TestDisplayNote_NonBlankPassthrough pins the no-op path: an authored note is
// returned BYTE-IDENTICAL, never rewritten or decorated.
func TestDisplayNote_NonBlankPassthrough(t *testing.T) {
	const note = "the fixup never ran gofmt; backend/internal/server/trace.go is unformatted at HEAD"
	c := Concern{
		Note:                 note,
		StageKind:            StageKindImplement,
		ReviewerModel:        ptr("claude-opus-4-8"),
		OriginReviewSequence: 41,
	}
	if got := c.DisplayNote(); got != note {
		t.Errorf("DisplayNote() = %q, want the stored note byte-identical (%q)", got, note)
	}
}

// TestDisplayNote_BlankReturnsPointer is the read-side control: a LEGACY row
// minted before the write-side backfill landed carries an empty note, and every
// surface reading it rendered an empty field. The blank fixture is seeded BY
// CONSTRUCTION (a literal ""), so deleting the blank branch reddens this
// assertion and not the setup.
func TestDisplayNote_BlankReturnsPointer(t *testing.T) {
	c := Concern{
		Note:                 "",
		StageKind:            StageKindImplement,
		ReviewerModel:        ptr("gpt-5-codex"),
		OriginReviewSequence: 77,
	}
	got := c.DisplayNote()
	if got == "" {
		t.Fatal("DisplayNote() = \"\" for a blank-note row — the surface renders nothing actionable")
	}
	for _, want := range []string{MissingNoteMarker, "gpt-5-codex", StageKindImplement, "77"} {
		if !strings.Contains(got, want) {
			t.Errorf("DisplayNote() = %q, want it to name %q so the operator can find the originating review", got, want)
		}
	}
}

// TestDisplayNote_WhitespaceOnlyReturnsPointer: a note of only whitespace is as
// unactionable as an empty one and takes the same branch.
func TestDisplayNote_WhitespaceOnlyReturnsPointer(t *testing.T) {
	c := Concern{
		Note:                 "   \n\t  ",
		StageKind:            StageKindPlan,
		ReviewerModel:        ptr("claude-opus-4-8"),
		OriginReviewSequence: 12,
	}
	got := c.DisplayNote()
	if !strings.Contains(got, MissingNoteMarker) {
		t.Errorf("DisplayNote() = %q for a whitespace-only note, want the %q stand-in", got, MissingNoteMarker)
	}
	if !strings.Contains(got, StageKindPlan) {
		t.Errorf("DisplayNote() = %q, want it to name the plan stage kind", got)
	}
}

// TestDisplayNote_NilReviewerModelRendersUnknown pins the nil-pointer branch:
// ReviewerModel is a *string stored NULL for a row minted with no model, so a
// naive deref would panic and a naive format would print "%!s(*string=<nil>)".
func TestDisplayNote_NilReviewerModelRendersUnknown(t *testing.T) {
	for name, c := range map[string]Concern{
		"nil_model":   {StageKind: StageKindImplement, OriginReviewSequence: 5},
		"blank_model": {StageKind: StageKindImplement, ReviewerModel: ptr("  "), OriginReviewSequence: 5},
	} {
		t.Run(name, func(t *testing.T) {
			got := c.DisplayNote()
			if !strings.Contains(got, missingNoteUnknownReviewer) {
				t.Errorf("DisplayNote() = %q, want the %q rendering", got, missingNoteUnknownReviewer)
			}
			if strings.Contains(got, "%!") {
				t.Errorf("DisplayNote() = %q, want no format-verb error artifact", got)
			}
		})
	}
}

// TestMissingNotePointer_BlankStageKind covers the last defensive branch: a row
// whose stage kind is somehow empty still renders a legible pointer.
func TestMissingNotePointer_BlankStageKind(t *testing.T) {
	got := MissingNotePointer("", "m", 9)
	if !strings.Contains(got, "unknown-stage") {
		t.Errorf("MissingNotePointer(\"\", …) = %q, want it to render an unknown stage kind", got)
	}
}

// TestConcern_IsServerCheck pins the provenance predicate the human-only
// clearing guards key on (E80.3 / #3760): exactly ProvenanceServerCheck is a
// server check; the legacy empty value and any other value are not. The comparison is
// exact — a near-miss spelling must NOT read as a server check, and the
// constant's value is pinned because it is persisted on every such row.
func TestConcern_IsServerCheck(t *testing.T) {
	cases := []struct {
		provenance string
		want       bool
	}{
		{ProvenanceServerCheck, true},
		{"", false},
		{"reviewer", false},
		{"Server_Check", false},
		{" server_check", false},
	}
	for _, tc := range cases {
		if got := (Concern{Provenance: tc.provenance}).IsServerCheck(); got != tc.want {
			t.Errorf("Concern{Provenance: %q}.IsServerCheck() = %v, want %v", tc.provenance, got, tc.want)
		}
	}
	if ProvenanceServerCheck != "server_check" {
		t.Errorf("ProvenanceServerCheck = %q, want %q (persisted on every server-check row)", ProvenanceServerCheck, "server_check")
	}
}

// TestRolledUpStateReason_NamesChildRunAndConcern pins the copy's state_reason
// (E94.1 / #4692): it names both the child run and the child concern.
func TestRolledUpStateReason_NamesChildRunAndConcern(t *testing.T) {
	src := &Concern{ID: uuid.New(), RunID: uuid.New()}
	got := RolledUpStateReason(src)
	if !strings.Contains(got, src.RunID.String()) || !strings.Contains(got, src.ID.String()) {
		t.Errorf("RolledUpStateReason = %q, want it to name run %s and concern %s", got, src.RunID, src.ID)
	}
	sup := RolledUpSupersededReason(src, "superseded by stage retry")
	if !strings.Contains(sup, src.ID.String()) || !strings.Contains(sup, "superseded by stage retry") {
		t.Errorf("RolledUpSupersededReason = %q, want the child concern id and the child reason", sup)
	}
	if RolledUpStateReason(nil) == "" || RolledUpSupersededReason(nil, "r") == "" {
		t.Error("nil source must still yield a non-empty reason")
	}
}

// TestNormalizeJudgement: each closed-set value reads as itself; blank and
// anything outside the set read unlabelled (the tolerant read of the CHECK-less
// column, E94.2 / #4693).
func TestNormalizeJudgement(t *testing.T) {
	for _, j := range []Judgement{
		JudgementDefect, JudgementDefectPreExisting, JudgementNoDefect, JudgementTradeoff,
		JudgementPreference, JudgementStale, JudgementInsufficientEvidence, JudgementUnlabelled,
	} {
		if got := NormalizeJudgement(string(j)); got != j {
			t.Errorf("NormalizeJudgement(%q) = %q, want itself", j, got)
		}
	}
	for _, in := range []string{"", "bogus", "Defect", " defect"} {
		if got := NormalizeJudgement(in); got != JudgementUnlabelled {
			t.Errorf("NormalizeJudgement(%q) = %q, want unlabelled", in, got)
		}
	}
}

// TestEffectiveOutcome: a duplicate reports duplicate_of:<target>; every other
// row its stored outcome.
func TestEffectiveOutcome(t *testing.T) {
	target := uuid.New()
	if got := (Concern{Outcome: OutcomeFixed, DuplicateOf: &target}).EffectiveOutcome(); got != "duplicate_of:"+target.String() {
		t.Errorf("duplicate EffectiveOutcome = %q, want duplicate_of:%s", got, target)
	}
	if got := (Concern{Outcome: OutcomeFixed}).EffectiveOutcome(); got != "fixed" {
		t.Errorf("EffectiveOutcome = %q, want fixed", got)
	}
	if got := (Concern{}).EffectiveOutcome(); got != "" {
		t.Errorf("EffectiveOutcome with none recorded = %q, want empty", got)
	}
}

// TestResolveJudgements pins every duplicate-chain mode (E94.2 / #4693,
// ADR-117 D1). Each resolution runs in a goroutine bounded by a scaled
// deadline, so a walk that fails to terminate fails the test instead of
// hanging the package.
func TestResolveJudgements(t *testing.T) {
	mk := func(j Judgement, dup *uuid.UUID) *Concern {
		return &Concern{ID: uuid.New(), Judgement: j, DuplicateOf: dup}
	}
	idp := func(c *Concern) *uuid.UUID { id := c.ID; return &id }
	resolve := func(t *testing.T, rows []*Concern) map[uuid.UUID]EffectiveJudgement {
		t.Helper()
		done := make(chan map[uuid.UUID]EffectiveJudgement, 1)
		go func() { done <- ResolveJudgements(rows) }()
		select {
		case got := <-done:
			return got
		case <-time.After(timescale.D(2 * time.Second)):
			t.Fatal("ResolveJudgements did not return: the duplicate walk failed to terminate")
			return nil
		}
	}
	type want struct {
		j    Judgement
		from *uuid.UUID
	}
	check := func(t *testing.T, got map[uuid.UUID]EffectiveJudgement, c *Concern, w want) {
		t.Helper()
		e, ok := got[c.ID]
		if !ok {
			t.Fatalf("row %s missing from the resolution", c.ID)
		}
		if e.Judgement != w.j {
			t.Errorf("judgement = %q, want %q", e.Judgement, w.j)
		}
		switch {
		case w.from == nil && e.InheritedFrom != nil:
			t.Errorf("inherited_from = %s, want nil", e.InheritedFrom)
		case w.from != nil && (e.InheritedFrom == nil || *e.InheritedFrom != *w.from):
			t.Errorf("inherited_from = %v, want %s", e.InheritedFrom, w.from)
		}
	}

	t.Run("non-duplicate reads its own normalized judgement", func(t *testing.T) {
		a, blank := mk(JudgementPreference, nil), mk("", nil)
		got := resolve(t, []*Concern{a, blank})
		check(t, got, a, want{j: JudgementPreference})
		check(t, got, blank, want{j: JudgementUnlabelled})
	})
	t.Run("direct duplicate inherits its root", func(t *testing.T) {
		root := mk(JudgementDefect, nil)
		dup := mk(JudgementNoDefect, idp(root))
		got := resolve(t, []*Concern{root, dup})
		check(t, got, dup, want{j: JudgementDefect, from: idp(root)})
		check(t, got, root, want{j: JudgementDefect})
	})
	t.Run("chain A<-B<-C inherits from A", func(t *testing.T) {
		a := mk(JudgementStale, nil)
		b := mk(JudgementNoDefect, idp(a))
		c := mk(JudgementNoDefect, idp(b))
		got := resolve(t, []*Concern{c, b, a})
		check(t, got, c, want{j: JudgementStale, from: idp(a)})
		check(t, got, b, want{j: JudgementStale, from: idp(a)})
	})
	t.Run("chain ending at an unlabelled root", func(t *testing.T) {
		root := mk(JudgementUnlabelled, nil)
		dup := mk(JudgementDefect, idp(root))
		check(t, resolve(t, []*Concern{root, dup}), dup, want{j: JudgementUnlabelled, from: idp(root)})
	})
	t.Run("root with a blank stored judgement", func(t *testing.T) {
		root := mk("", nil)
		dup := mk(JudgementDefect, idp(root))
		check(t, resolve(t, []*Concern{root, dup}), dup, want{j: JudgementUnlabelled, from: idp(root)})
	})
	t.Run("dangling target reads unlabelled", func(t *testing.T) {
		missing := uuid.New()
		dup := mk(JudgementDefect, &missing)
		check(t, resolve(t, []*Concern{dup}), dup, want{j: JudgementUnlabelled})
	})
	t.Run("two-node cycle reads unlabelled", func(t *testing.T) {
		a, b := mk(JudgementDefect, nil), mk(JudgementDefect, nil)
		a.DuplicateOf, b.DuplicateOf = idp(b), idp(a)
		got := resolve(t, []*Concern{a, b})
		check(t, got, a, want{j: JudgementUnlabelled})
		check(t, got, b, want{j: JudgementUnlabelled})
	})
	t.Run("self-link reads unlabelled", func(t *testing.T) {
		a := mk(JudgementDefect, nil)
		a.DuplicateOf = idp(a)
		check(t, resolve(t, []*Concern{a}), a, want{j: JudgementUnlabelled})
	})
	t.Run("a chain into a cycle reads unlabelled", func(t *testing.T) {
		a, b := mk(JudgementDefect, nil), mk(JudgementDefect, nil)
		a.DuplicateOf, b.DuplicateOf = idp(b), idp(a)
		tail := mk(JudgementDefect, idp(a))
		check(t, resolve(t, []*Concern{tail, a, b}), tail, want{j: JudgementUnlabelled})
	})
	t.Run("nil rows are skipped", func(t *testing.T) {
		a := mk(JudgementDefect, nil)
		got := resolve(t, []*Concern{nil, a})
		if len(got) != 1 {
			t.Errorf("resolutions = %d, want 1", len(got))
		}
	})
}

// TestHumanJudgements pins the human set: the closed vocabulary minus
// unlabelled, and a fresh slice per call.
func TestHumanJudgements(t *testing.T) {
	got := HumanJudgements()
	want := []Judgement{JudgementDefect, JudgementDefectPreExisting, JudgementNoDefect, JudgementTradeoff,
		JudgementPreference, JudgementStale, JudgementInsufficientEvidence}
	if len(got) != len(want) {
		t.Fatalf("HumanJudgements() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("HumanJudgements()[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	got[0] = "mutated"
	if HumanJudgements()[0] != JudgementDefect {
		t.Error("HumanJudgements returned a shared slice")
	}
}

// TestJudgementFromReasonPrefix pins the three interim prefixes (FO ruling
// D1): exact, lowercase, after leading whitespace only.
func TestJudgementFromReasonPrefix(t *testing.T) {
	for _, tc := range []struct {
		reason string
		want   Judgement
		ok     bool
	}{
		{"false_positive: the guard exists", JudgementNoDefect, true},
		{"  \n\taccepted_tradeoff: slower", JudgementTradeoff, true},
		{"preference: naming", JudgementPreference, true},
		{"False_Positive: upper case", "", false},
		{"false_positive no colon", "", false},
		{"see false_positive: mid-string", "", false},
		{"", "", false},
	} {
		got, ok := JudgementFromReasonPrefix(tc.reason)
		if got != tc.want || ok != tc.ok {
			t.Errorf("JudgementFromReasonPrefix(%q) = %q, %v; want %q, %v", tc.reason, got, ok, tc.want, tc.ok)
		}
	}
}

// TestResolveHumanJudgement is the validator table (E94.7 / #4840): one row
// per refusal rule and one per FO ruling D1 rule (a)-(d). CF6 (the tradeoff
// rule) and CF9 (the prefix-conflict check) are declared against it.
func TestResolveHumanJudgement(t *testing.T) {
	for _, tc := range []struct {
		name                       string
		judgement, cost, prefixSrc string
		want                       Judgement
		wantRule, wantField        string
		wantPrefix                 string
		wantMsg                    []string
	}{
		{name: "required", wantRule: JudgementRuleRequired, wantField: "judgement"},
		{name: "required with an unprefixed reason", prefixSrc: "looks fine", wantRule: JudgementRuleRequired, wantField: "judgement"},
		{name: "unknown case variant", judgement: "No_Defect", wantRule: JudgementRuleUnknown, wantField: "judgement"},
		{name: "unknown bogus", judgement: "bogus", wantRule: JudgementRuleUnknown, wantField: "judgement"},
		{name: "unknown beats a prefix conflict", judgement: "bogus", prefixSrc: "false_positive: x", wantRule: JudgementRuleUnknown, wantField: "judgement"},
		{name: "unlabelled", judgement: "unlabelled", wantRule: JudgementRuleUnlabelled, wantField: "judgement"},
		{name: "tradeoff blank cost", judgement: "tradeoff", wantRule: JudgementRuleTradeoffCostRequired, wantField: "tradeoff_cost"},
		{name: "tradeoff whitespace cost", judgement: "tradeoff", cost: " \t", wantRule: JudgementRuleTradeoffCostRequired, wantField: "tradeoff_cost"},
		{name: "prefix-derived tradeoff blank cost", prefixSrc: "accepted_tradeoff: slower", wantRule: JudgementRuleTradeoffCostRequired, wantField: "tradeoff_cost"},
		{name: "cost without tradeoff", judgement: "defect", cost: "c", wantRule: JudgementRuleTradeoffCostWithoutTradeoff, wantField: "tradeoff_cost"},
		{name: "tradeoff with cost", judgement: "tradeoff", cost: "slower", want: JudgementTradeoff},
		{name: "(a) explicit + matching prefix", judgement: "no_defect", prefixSrc: "false_positive: the guard exists", want: JudgementNoDefect},
		{name: "(b) explicit + no prefix", judgement: "defect", prefixSrc: "real but minor", want: JudgementDefect},
		{name: "(b) explicit, no prefix source", judgement: "stale", want: JudgementStale},
		{name: "(c) false_positive: alone", prefixSrc: "false_positive: the guard exists", want: JudgementNoDefect},
		{name: "(c) accepted_tradeoff: alone", prefixSrc: "accepted_tradeoff: slower", cost: "slower", want: JudgementTradeoff},
		{name: "(c) preference: alone", prefixSrc: "preference: naming", want: JudgementPreference},
		{
			name: "(d) explicit contradicts prefix", judgement: "defect", prefixSrc: "false_positive: the guard exists",
			wantRule: JudgementRuleConflictsReasonPrefix, wantField: "judgement", wantPrefix: "false_positive:",
			wantMsg: []string{`"defect"`, `"false_positive:"`, `"no_defect"`},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ResolveHumanJudgement(tc.judgement, tc.cost, tc.prefixSrc)
			if tc.wantRule == "" {
				if err != nil || got != tc.want {
					t.Fatalf("ResolveHumanJudgement = %q, %v; want %q, nil", got, err, tc.want)
				}
				return
			}
			var je JudgementError
			if !errors.As(err, &je) {
				t.Fatalf("ResolveHumanJudgement = %q, %v; want a JudgementError rule %s", got, err, tc.wantRule)
			}
			if got != "" || je.Rule != tc.wantRule || je.Field != tc.wantField || je.Prefix != tc.wantPrefix {
				t.Errorf("got %q, JudgementError{Field %q Rule %q Prefix %q}; want \"\", {%q %q %q}",
					got, je.Field, je.Rule, je.Prefix, tc.wantField, tc.wantRule, tc.wantPrefix)
			}
			for _, sub := range tc.wantMsg {
				if !strings.Contains(je.Message, sub) {
					t.Errorf("message %q does not name %s", je.Message, sub)
				}
			}
		})
	}
}

// TestResolveHumanJudgement_RefusesTradeoffCostWithoutTradeoff pins the
// INFERRED rule tradeoff_cost_without_tradeoff (approval condition T1): a cost
// sent with any judgement other than tradeoff is refused rather than silently
// dropped — explicit, prefix-derived and on the store's Validate alike.
func TestResolveHumanJudgement_RefusesTradeoffCostWithoutTradeoff(t *testing.T) {
	for _, tc := range []struct{ judgement, prefixSrc string }{
		{"defect", ""},
		{"no_defect", ""},
		{"", "preference: naming"},
	} {
		_, err := ResolveHumanJudgement(tc.judgement, "a stated cost", tc.prefixSrc)
		var je JudgementError
		if !errors.As(err, &je) || je.Rule != JudgementRuleTradeoffCostWithoutTradeoff || je.Field != "tradeoff_cost" {
			t.Errorf("ResolveHumanJudgement(%q, cost, %q) err = %v, want rule %s on tradeoff_cost",
				tc.judgement, tc.prefixSrc, err, JudgementRuleTradeoffCostWithoutTradeoff)
		}
	}
	err := HumanDisposition{Outcome: OutcomeWaived, Judgement: JudgementDefect, ActorKind: "user", TradeoffCost: "c"}.Validate()
	var je JudgementError
	if !errors.As(err, &je) || je.Rule != JudgementRuleTradeoffCostWithoutTradeoff {
		t.Errorf("Validate with a cost on defect = %v, want rule %s", err, JudgementRuleTradeoffCostWithoutTradeoff)
	}
}

// TestHumanDispositionValidate pins every refusal of the store-side guard
// (E94.7 / #4840) and the three accepted outcomes.
func TestHumanDispositionValidate(t *testing.T) {
	ok := HumanDisposition{Outcome: OutcomeWaived, Judgement: JudgementNoDefect, ActorKind: "user"}
	for _, o := range []Outcome{OutcomeWaived, OutcomeFixed, OutcomeDeferred} {
		d := ok
		d.Outcome = o
		if err := d.Validate(); err != nil {
			t.Errorf("Validate(outcome %s) = %v, want nil", o, err)
		}
	}
	if err := (HumanDisposition{Outcome: OutcomeWaived, Judgement: JudgementTradeoff, ActorKind: "agent", TradeoffCost: "c"}).Validate(); err != nil {
		t.Errorf("Validate(tradeoff with cost) = %v, want nil", err)
	}
	for _, tc := range []struct {
		name     string
		mutate   func(*HumanDisposition)
		wantRule string // "" = a plain error, not a JudgementError
	}{
		{"outcome carried", func(d *HumanDisposition) { d.Outcome = OutcomeCarried }, ""},
		{"outcome blank", func(d *HumanDisposition) { d.Outcome = "" }, ""},
		{"actor kind blank", func(d *HumanDisposition) { d.ActorKind = " " }, ""},
		{"judgement blank", func(d *HumanDisposition) { d.Judgement = "" }, JudgementRuleRequired},
		{"judgement unlabelled", func(d *HumanDisposition) { d.Judgement = JudgementUnlabelled }, JudgementRuleUnlabelled},
		{"judgement unknown", func(d *HumanDisposition) { d.Judgement = "bogus" }, JudgementRuleUnknown},
		{"tradeoff without cost", func(d *HumanDisposition) { d.Judgement = JudgementTradeoff }, JudgementRuleTradeoffCostRequired},
	} {
		d := ok
		tc.mutate(&d)
		err := d.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want a refusal", tc.name)
			continue
		}
		var je JudgementError
		isJE := errors.As(err, &je)
		if tc.wantRule == "" && isJE {
			t.Errorf("%s: Validate = JudgementError %s, want a plain error", tc.name, je.Rule)
		}
		if tc.wantRule != "" && (!isJE || je.Rule != tc.wantRule) {
			t.Errorf("%s: Validate = %v, want JudgementError rule %s", tc.name, err, tc.wantRule)
		}
	}
}

// ledgerRow builds a hand-made row at a ledger position (E94.8 / #4841).
func ledgerRow(runID uuid.UUID, seq int64, created time.Time, id uuid.UUID) *Concern {
	return &Concern{ID: id, RunID: runID, OriginReviewSequence: seq, CreatedAt: created}
}

// TestLedgerBefore pins each tie-break level of the run ledger order
// (origin_review_sequence, created_at, id bytes) and its strictness: a row
// never precedes itself and no two rows precede each other.
func TestLedgerBefore(t *testing.T) {
	run := uuid.New()
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	lowID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	highID := uuid.MustParse("ff000000-0000-0000-0000-000000000000")
	for _, tc := range []struct {
		name string
		a, b *Concern
		want bool
	}{
		{"lower sequence first, despite later created_at and higher id", ledgerRow(run, 1, t0.Add(time.Hour), highID), ledgerRow(run, 2, t0, lowID), true},
		{"higher sequence after", ledgerRow(run, 2, t0, lowID), ledgerRow(run, 1, t0.Add(time.Hour), highID), false},
		{"same sequence, earlier created_at first, despite higher id", ledgerRow(run, 1, t0, highID), ledgerRow(run, 1, t0.Add(time.Microsecond), lowID), true},
		{"same sequence, later created_at after", ledgerRow(run, 1, t0.Add(time.Microsecond), lowID), ledgerRow(run, 1, t0, highID), false},
		{"same sequence and created_at, lower id bytes first", ledgerRow(run, 1, t0, lowID), ledgerRow(run, 1, t0, highID), true},
		{"same sequence and created_at, higher id bytes after", ledgerRow(run, 1, t0, highID), ledgerRow(run, 1, t0, lowID), false},
		{"a row never precedes itself", ledgerRow(run, 1, t0, lowID), ledgerRow(run, 1, t0, lowID), false},
	} {
		if got := LedgerBefore(tc.a, tc.b); got != tc.want {
			t.Errorf("%s: LedgerBefore = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestCheckDuplicateTarget pins the ONE duplicate_of guard (E94.8 / #4841).
// The cross-run row puts the target at a LOWER sequence than the labelled
// concern, so the ordering rule alone would ACCEPT it: the run-id check is the
// only thing in that path (C1). Approval condition M1: a missing target and a
// cross-run target return the IDENTICAL error under ONE rule, and nothing in
// it names the target's run.
func TestCheckDuplicateTarget(t *testing.T) {
	run, foreignRun := uuid.New(), uuid.New()
	t0 := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	c := ledgerRow(run, 5, t0, uuid.New())
	earlier := ledgerRow(run, 2, t0.Add(time.Hour), uuid.New())
	later := ledgerRow(run, 9, t0, uuid.New())
	sameSeqEarlierCreated := ledgerRow(run, 5, t0.Add(-time.Second), uuid.New())
	crossRun := ledgerRow(foreignRun, 1, t0.Add(-time.Hour), uuid.New())
	if !LedgerBefore(crossRun, c) {
		t.Fatal("fixture: the cross-run target must sort before c so only the run-id check refuses it")
	}

	for _, tc := range []struct {
		name   string
		target *Concern
		rule   string
	}{
		{"same-run earlier sequence accepted", earlier, ""},
		{"same sequence, earlier created_at accepted", sameSeqEarlierCreated, ""},
		{"cross-run refused", crossRun, JudgementRuleDuplicateOfNotFound},
		{"missing target refused", nil, JudgementRuleDuplicateOfNotFound},
		{"self refused", c, JudgementRuleDuplicateOfNotEarlier},
		{"later refused", later, JudgementRuleDuplicateOfNotEarlier},
	} {
		err := CheckDuplicateTarget(c, tc.target)
		if tc.rule == "" {
			if err != nil {
				t.Errorf("%s: err = %v, want nil", tc.name, err)
			}
			continue
		}
		var je JudgementError
		if !errors.As(err, &je) || je.Rule != tc.rule || je.Field != "duplicate_of" {
			t.Errorf("%s: err = %#v, want JudgementError{Field: duplicate_of, Rule: %s}", tc.name, err, tc.rule)
		}
		if got := errors.Is(err, ErrDuplicateTargetNotInRun); got != (tc.rule == JudgementRuleDuplicateOfNotFound) {
			t.Errorf("%s: errors.Is(ErrDuplicateTargetNotInRun) = %v", tc.name, got)
		}
		if errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err matches ErrNotFound; a duplicate_of refusal must never read as the labelled concern's 404", tc.name)
		}
	}

	// M1: identical refusal, naming only c's own run.
	crossErr, missingErr := CheckDuplicateTarget(c, crossRun), CheckDuplicateTarget(c, nil)
	if crossErr == nil || missingErr == nil {
		t.Fatalf("cross-run err %v / missing err %v, want both refused", crossErr, missingErr)
	}
	if crossErr != missingErr || crossErr.Error() != missingErr.Error() {
		t.Errorf("cross-run and missing refusals differ:\n cross:   %#v\n missing: %#v", crossErr, missingErr)
	}
	var je JudgementError
	errors.As(crossErr, &je)
	for name, s := range map[string]string{"Error()": crossErr.Error(), "Message": je.Message, "Field": je.Field, "Rule": je.Rule, "Prefix": je.Prefix} {
		if strings.Contains(s, foreignRun.String()) || strings.Contains(s, crossRun.ID.String()) {
			t.Errorf("cross-run refusal %s %q names the foreign run or target; M1 allows only the labelled concern's own run", name, s)
		}
	}
	if !strings.Contains(crossErr.Error(), run.String()) {
		t.Errorf("cross-run refusal %q does not name the labelled concern's own run %s", crossErr, run)
	}
	if err := CheckDuplicateTarget(nil, earlier); err == nil {
		t.Error("nil labelled concern: err = nil, want a refusal")
	}
}

// TestLabelValidate pins Label.Validate, one row per rule (E94.8 / #4841).
func TestLabelValidate(t *testing.T) {
	target := uuid.New()
	for _, tc := range []struct {
		name         string
		l            Label
		field, rule  string
		plainRefusal bool // a non-JudgementError refusal
	}{
		{"judgement and duplicate_of", Label{Judgement: JudgementDefect, DuplicateOf: &target, ActorKind: "user"}, "duplicate_of", JudgementRuleDuplicateOfExcludesJudgement, false},
		{"tradeoff_cost with duplicate_of", Label{TradeoffCost: "c", DuplicateOf: &target, ActorKind: "user"}, "tradeoff_cost", JudgementRuleDuplicateOfExcludesJudgement, false},
		{"neither", Label{ActorKind: "user"}, "judgement", JudgementRuleRequired, false},
		{"unlabelled", Label{Judgement: JudgementUnlabelled, ActorKind: "user"}, "judgement", JudgementRuleUnlabelled, false},
		{"unknown", Label{Judgement: "bogus", ActorKind: "user"}, "judgement", JudgementRuleUnknown, false},
		{"tradeoff without cost", Label{Judgement: JudgementTradeoff, ActorKind: "user"}, "tradeoff_cost", JudgementRuleTradeoffCostRequired, false},
		{"cost without tradeoff", Label{Judgement: JudgementDefect, TradeoffCost: "c", ActorKind: "user"}, "tradeoff_cost", JudgementRuleTradeoffCostWithoutTradeoff, false},
		{"blank actor kind on a judgement", Label{Judgement: JudgementDefect, ActorKind: " "}, "", "", true},
		{"blank actor kind on a duplicate", Label{DuplicateOf: &target}, "", "", true},
	} {
		err := tc.l.Validate()
		if err == nil {
			t.Errorf("%s: Validate = nil, want a refusal", tc.name)
			continue
		}
		var je JudgementError
		isJE := errors.As(err, &je)
		if tc.plainRefusal {
			if isJE {
				t.Errorf("%s: err = %#v, want a plain refusal", tc.name, err)
			}
			continue
		}
		if !isJE || je.Field != tc.field || je.Rule != tc.rule {
			t.Errorf("%s: err = %#v, want JudgementError{Field: %s, Rule: %s}", tc.name, err, tc.field, tc.rule)
		}
	}
	for name, l := range map[string]Label{
		"judgement":          {Judgement: JudgementNoDefect, ActorKind: "user"},
		"tradeoff with cost": {Judgement: JudgementTradeoff, TradeoffCost: "slower", ActorKind: "user", Backfill: true},
		"duplicate":          {DuplicateOf: &target, ActorKind: "user", Backfill: true},
	} {
		if err := l.Validate(); err != nil {
			t.Errorf("%s: Validate = %v, want nil", name, err)
		}
	}
}
