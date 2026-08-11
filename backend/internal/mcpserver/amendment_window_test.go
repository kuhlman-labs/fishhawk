package mcpserver

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// operatorFacingSources lists the package files the retired-wording guard walks
// — the OPERATOR-FACING surfaces the #2617 done-means covers: non-test .go
// files plus README.md. Two definitional files are deliberately EXCLUDED
// because they must contain the retired literals to do their job and are not
// themselves shown to an operator:
//
//   - *_test.go: a test asserts the retired wording is ABSENT, so it carries
//     the literal as a negative-assertion string (e.g. await_stage_test.go's
//     `Contains(..., "as if denied")` guard). Scoping the walk to non-test
//     files is what stops the guard flagging its own sibling tests (the
//     operator's CONDITION 1).
//   - amendment_window.go: the guard's OWN source, whose search needles ARE the
//     retired phrases ("as if denied", "proceed-as-denied"); it cannot detect a
//     literal it is forbidden to name.
//
// go test runs with the package directory as the working directory, so a plain
// ReadDir(".") enumerates the source files and README beside them.
func operatorFacingSources(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case name == "README.md":
			files = append(files, name)
		case strings.HasSuffix(name, "_test.go"):
			continue // a _test.go is not an operator-facing surface (see doc).
		case name == "amendment_window.go":
			continue // the guard's own definitional source (see doc).
		case strings.HasSuffix(name, ".go"):
			files = append(files, name)
		}
	}
	return files
}

type wordingFinding struct {
	file   string
	lineNo int
	line   string
	name   string
}

// walkAmendmentWordingFindings runs staleAmendmentWordingFindings over every
// line of every operator-facing source and collects the findings. Shared by the
// two done-means guard tests so each reports its own failure mode independently.
func walkAmendmentWordingFindings(t *testing.T) []wordingFinding {
	t.Helper()
	var out []wordingFinding
	for _, f := range operatorFacingSources(t) {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, name := range staleAmendmentWordingFindings(line) {
				out = append(out, wordingFinding{file: f, lineNo: i + 1, line: line, name: name})
			}
		}
	}
	return out
}

// TestNoRetiredProceedAsDeniedWording is a #2617 done-means guard: no
// operator-facing mcpserver surface may carry the retired proceed-as-denied
// framing. It asserts the SHIPPED text of the tool descriptions, doc comments,
// and README — so a comment-only / no-op touch that leaves the retired wording
// in place fails here, which the scope-completeness gate cannot catch.
func TestNoRetiredProceedAsDeniedWording(t *testing.T) {
	for _, f := range walkAmendmentWordingFindings(t) {
		if f.name == findingProceedAsDenied {
			t.Errorf("%s:%d carries retired proceed-as-denied wording: %q", f.file, f.lineNo, strings.TrimSpace(f.line))
		}
	}
}

// TestNoStaleAmendmentWindowFigure is the companion done-means guard for the
// stale ~5-minute figure. Separate from the wording guard so each failure mode
// reports independently.
func TestNoStaleAmendmentWindowFigure(t *testing.T) {
	for _, f := range walkAmendmentWordingFindings(t) {
		if f.name == findingStaleFigure {
			t.Errorf("%s:%d carries a stale ~5-minute amendment window figure: %q", f.file, f.lineNo, strings.TrimSpace(f.line))
		}
	}
}

// TestStaleWordingHistoricalExemptionIsNarrow tables the pure line-level helper,
// proving the ONE exemption (a line carrying BOTH #2601 and a historical
// marker) fires only there — and that the figure regexp does not false-positive
// on stage_wait.go's "~5.5 minutes" nor on a bare "5 minute" (condition 4).
func TestStaleWordingHistoricalExemptionIsNarrow(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		wantSet []string // findings expected, order-independent
	}{
		{
			name:    "stale phrasing, no marker -> flagged",
			line:    "the agent then proceeds as if denied when the window elapses",
			wantSet: []string{findingProceedAsDenied},
		},
		{
			name:    "hyphenated proceed-as-denied, no marker -> flagged",
			line:    "// the child's long-poll times out to proceed-as-denied here",
			wantSet: []string{findingProceedAsDenied},
		},
		{
			name:    "#2601 but no historical marker -> STILL flagged",
			line:    "see #2601 — the agent proceeds as if denied",
			wantSet: []string{findingProceedAsDenied},
		},
		{
			name:    "historical marker but no #2601 -> STILL flagged",
			line:    "formerly the agent proceeded as if denied",
			wantSet: []string{findingProceedAsDenied},
		},
		{
			name:    "real scope_amendment.go:196 correction record -> exempt",
			line:    "this doc said ~5 minutes and understated the window threefold, #2601).",
			wantSet: nil,
		},
		{
			name:    "benign stage_wait.go ~5.5 minutes -> NOT a finding",
			line:    "//     sitting at 30s: a stage 22 minutes in advertises ~5.5 minutes.",
			wantSet: nil,
		},
		{
			name:    "bare ~5-minute figure -> flagged (figure only)",
			line:    "Decide before the runner's ~5-minute amendment window elapses.",
			wantSet: []string{findingStaleFigure},
		},
		{
			name:    "bare 5 minute with no tilde -> flagged (condition 4)",
			line:    "the agent gives up after 5 minutes and stops",
			wantSet: []string{findingStaleFigure},
		},
		{
			name:    "five-minute spelled out -> flagged",
			line:    "a five-minute poll window",
			wantSet: []string{findingStaleFigure},
		},
		{
			name:    "current ~15 minutes figure -> NOT a finding",
			line:    "the agent polls ~15 minutes per request",
			wantSet: nil,
		},
		{
			name:    "both retired forms on one line -> both findings",
			line:    "proceeds as if denied when its ~5-minute window elapses",
			wantSet: []string{findingProceedAsDenied, findingStaleFigure},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := staleAmendmentWordingFindings(tc.line)
			if !sameStringSet(got, tc.wantSet) {
				t.Errorf("staleAmendmentWordingFindings(%q) = %v, want %v", tc.line, got, tc.wantSet)
			}
		})
	}
}

func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]int{}
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
	}
	for _, v := range seen {
		if v != 0 {
			return false
		}
	}
	return true
}

// TestAmendmentPollWindowMatchesPromptFigure pins the constant and its two
// rendered forms against the figure writeScopeAmendments (backend/internal/
// prompt/prompt.go) gives the agent — the number the mcpserver surfaces mirror.
func TestAmendmentPollWindowMatchesPromptFigure(t *testing.T) {
	if amendmentPollWindowMinutes != 15 {
		t.Fatalf("amendmentPollWindowMinutes = %d, want 15 (the figure writeScopeAmendments gives the agent)", amendmentPollWindowMinutes)
	}
	if got := amendmentPollWindowText(); got != "~15 minutes" {
		t.Errorf("amendmentPollWindowText() = %q, want ~15 minutes", got)
	}
	if got := amendmentPollWindowAdjective(); got != "~15-minute" {
		t.Errorf("amendmentPollWindowAdjective() = %q, want ~15-minute", got)
	}
}

// TestAwaitStageAmendmentMessageRendersWindowConstant proves the amendment_pending
// Message and next_step Reason RENDER the window figure from the constant rather
// than hardcoding it. It discriminates by MUTATING the constant to 17 and
// asserting the rendered text follows to "~17 minutes" (condition 2): a hardcoded
// "~15 minutes" literal would not move and the "~17 minutes" assertion would go
// red. The absence assertion catches a stale hardcoded 15 directly.
func TestAwaitStageAmendmentMessageRendersWindowConstant(t *testing.T) {
	orig := amendmentPollWindowMinutes
	t.Cleanup(func() { amendmentPollWindowMinutes = orig })
	amendmentPollWindowMinutes = 17

	runID := uuid.New()
	item := &ScopeAmendmentItem{
		ID:      uuid.New().String(),
		RunID:   runID.String(),
		StageID: uuid.New().String(),
		Paths:   []ScopeAmendmentPath{{Path: "backend/internal/mcpserver/tools_test.go", Operation: "modify"}},
		Reason:  "the coupled registration table must change too",
		Status:  "pending",
	}
	out := awaitStageAmendmentPendingOutput("implement", item.StageID, runID, "running", item, time.Now(), false, 600)

	if !strings.Contains(out.Message, "~17 minutes") {
		t.Errorf("Message must render the window figure from the constant (~17 minutes); got %q", out.Message)
	}
	if out.NextStep == nil || !strings.Contains(out.NextStep.Reason, "~17 minutes") {
		t.Errorf("next_step Reason must render the window figure from the constant (~17 minutes); got %+v", out.NextStep)
	}
	if strings.Contains(out.Message, "~15 minutes") {
		t.Errorf("Message still contains a hardcoded ~15 minutes after the constant moved to 17: %q", out.Message)
	}
}

// TestToolDescriptionsRenderWindowConstant is the companion discrimination test
// for the REGISTER-TIME tool descriptions — the surface #2627 correctly names as
// uncovered. The three descriptions that state the amendment poll window
// (fishhawk_await_stage, fishhawk_dispatch_stage, fishhawk_decide_scope_amendment)
// build their text INSIDE registerAwaitStage / registerDispatchStage /
// registerDecideScopeAmendment by concatenating amendmentPollWindowAdjective() /
// amendmentPollWindowText() — evaluated on EVERY registration call, NOT at
// package init (correcting #2627's package-init premise). So mutating the var and
// re-registering the tool set through the real ListTools round-trip observes the
// mutated figure, exactly as the runtime-Message discrimination test above does.
//
// The gap this closes: the pre-existing guards (TestNoRetiredProceedAsDeniedWording,
// TestNoStaleAmendmentWindowFigure) trip only on the retired proceed-as-denied
// wording and the retired ~5-minute figure, so a hardcoded CURRENT figure
// (e.g. "~15-minute" written as a literal instead of rendered from the constant)
// passes every existing test. This test catches that: it moves the var and
// asserts each description FOLLOWS.
//
// Two independently reporting arms:
//   - POSITIVE: each of the three named descriptions must contain the MUTATED
//     rendering (a description that hardcodes the figure would not move, red here).
//   - GENERIC NEGATIVE: NO registered description at all — not just the three —
//     may still carry the PRE-mutation rendering after the mutation, so a future
//     fourth description that hardcodes the current figure trips it without anyone
//     remembering to extend a list.
//
// Per condition 2 the mutation value is derived (orig+2), never a hardcoded 17:
// a literal target would make both arms vacuous if the window were ever
// legitimately set to that value. The var is restored via t.Cleanup and the
// package uses no t.Parallel(), so the mutation cannot leak across tests.
func TestToolDescriptionsRenderWindowConstant(t *testing.T) {
	orig := amendmentPollWindowMinutes
	t.Cleanup(func() { amendmentPollWindowMinutes = orig })

	// Derive both the pre-mutation (stale) and mutated renderings from orig so the
	// test carries no hardcoded figure and stays correct if the window ever moves.
	staleText := fmt.Sprintf("~%d minutes", orig)
	staleAdj := fmt.Sprintf("~%d-minute", orig)
	mutated := orig + 2
	mutatedText := fmt.Sprintf("~%d minutes", mutated)
	mutatedAdj := fmt.Sprintf("~%d-minute", mutated)

	amendmentPollWindowMinutes = mutated
	descByName := listToolDescriptions(t)

	// POSITIVE arm: the three descriptions that render the figure must follow the
	// mutated value. await_stage renders BOTH forms (the adjectival poll window and
	// the per-request phrasing); dispatch_stage renders the adjectival amendment
	// window; decide_scope_amendment renders the per-request phrasing.
	positive := []struct {
		tool string
		want []string
	}{
		{"fishhawk_await_stage", []string{mutatedAdj, mutatedText}},
		{"fishhawk_dispatch_stage", []string{mutatedAdj}},
		{"fishhawk_decide_scope_amendment", []string{mutatedText}},
	}
	for _, p := range positive {
		desc, ok := descByName[p.tool]
		if !ok {
			// Fail loud on a missing entry (e.g. a rename) so the assertion cannot
			// pass vacuously against an empty string.
			t.Fatalf("%s not registered: no description in ListTools", p.tool)
		}
		for _, want := range p.want {
			if !strings.Contains(desc, want) {
				t.Errorf("%s description must render the window figure from the constant (contain %q); a hardcoded figure would not move. got:\n%s", p.tool, want, desc)
			}
		}
	}

	// GENERIC NEGATIVE arm: after the mutation, NO registered description may still
	// carry the pre-mutation rendering — this extends the guard past the three
	// known sites to any description that hardcodes the current figure. t.Errorf
	// (not Fatalf) so every offending description is named. Only "~N minutes" /
	// "~N-minute" match; a "15s" heartbeat cadence in another description does not.
	for tool, desc := range descByName {
		if strings.Contains(desc, staleText) {
			t.Errorf("%s description still contains the pre-mutation figure %q after the constant moved to %d — it hardcodes the figure instead of rendering it", tool, staleText, mutated)
		}
		if strings.Contains(desc, staleAdj) {
			t.Errorf("%s description still contains the pre-mutation figure %q after the constant moved to %d — it hardcodes the figure instead of rendering it", tool, staleAdj, mutated)
		}
	}
}
