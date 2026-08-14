package concern

import (
	"errors"
	"strings"
	"testing"
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
		StateRaised:               {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateAddressedPending:     {StateAddressed, StateReopened, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateAddressed:            {StateReopened},
		StateReopened:             {StateAddressedPending, StateWaived, StateSuperseded, StateDeferred, StateAddressedByCondition},
		StateWaived:               {},
		StateSuperseded:           {},
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

func TestStateIsOpen(t *testing.T) {
	open := []State{StateRaised, StateAddressedPending, StateReopened}
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
