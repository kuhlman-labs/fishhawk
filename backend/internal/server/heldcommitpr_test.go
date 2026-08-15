package server

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Held-commit PR-text resolution (#2570). heldCommitPRTitleBody is what stops a
// resumed implement stage opening `chore: fishhawk implement stage <id>` with no
// summary, no test plan and no `Closes #N` — the last of which means the trigger
// issue silently stays open on merge.

func heldCommitIssueRun(title string, number int) *run.Run {
	return &run.Run{IssueContext: &run.IssueContext{Title: title, Number: number}}
}

// TestHeldCommitPRTitleBody_RecoveredVerbatim is m1's backend half: recovered
// text is used exactly as captured, with only the auto-close directive added.
func TestHeldCommitPRTitleBody_RecoveredVerbatim(t *testing.T) {
	title, body := heldCommitPRTitleBody(
		heldCommitIssueRun("[E67.5] a held-commit resume opens a placeholder PR", 2570),
		"feat(server): carry agent PR text across a held-commit resume",
		"## Summary\n\n- capture at park time\n\n## Test plan\n\n- [ ] run it",
	)
	if title != "feat(server): carry agent PR text across a held-commit resume" {
		t.Errorf("recovered title must be used verbatim, got %q", title)
	}
	if !strings.HasPrefix(body, "## Summary\n\n- capture at park time") {
		t.Errorf("recovered body must be used verbatim, got %q", body)
	}
	if !strings.HasSuffix(body, "\n\nCloses #2570") {
		t.Errorf("a recovered body with no closing reference must get one appended, got %q", body)
	}
}

// TestHeldCommitPRTitleBody_IssueContextFallback is m2: no recovered text at all
// (every pre-#2570 park row) still yields a real, conventional, issue-closing PR.
func TestHeldCommitPRTitleBody_IssueContextFallback(t *testing.T) {
	t.Run("non-conventional issue title gets the chore prefix", func(t *testing.T) {
		title, body := heldCommitPRTitleBody(heldCommitIssueRun("[E67.5] resume opens a placeholder PR", 2570), "", "")
		if title != "chore: [E67.5] resume opens a placeholder PR" {
			t.Errorf("title = %q, want the chore-prefixed issue title", title)
		}
		if !strings.HasPrefix(body, "## Summary\n\n") {
			t.Errorf("fallback body must open with a Summary heading, got %q", body)
		}
		if !strings.Contains(body, "resume") {
			t.Errorf("fallback body must state that this was a resume, got %q", body)
		}
		if !strings.HasSuffix(body, "\n\nCloses #2570") {
			t.Errorf("fallback body must close the trigger issue, got %q", body)
		}
	})

	t.Run("already-conventional issue title is used verbatim", func(t *testing.T) {
		title, _ := heldCommitPRTitleBody(heldCommitIssueRun("fix(runner): stop dropping the PR text", 2570), "", "")
		if title != "fix(runner): stop dropping the PR text" {
			t.Errorf("title = %q, want the issue title verbatim (no double prefix)", title)
		}
	})

	t.Run("number but no title", func(t *testing.T) {
		title, body := heldCommitPRTitleBody(heldCommitIssueRun("", 2570), "", "")
		if title != "" {
			t.Errorf("with no issue title there is nothing to synthesize a title from, got %q", title)
		}
		if !strings.Contains(body, "#2570") || !strings.HasSuffix(body, "\n\nCloses #2570") {
			t.Errorf("body = %q, want a synthesized body closing #2570", body)
		}
	})
}

// TestHeldCommitPRTitleBody_NoContextReturnsEmpty is m3's backend half: with
// neither recovered text nor issue context, BOTH fields come back empty so the
// runner keeps today's placeholder verbatim. A resume never starts failing
// because there was nothing to synthesize from.
func TestHeldCommitPRTitleBody_NoContextReturnsEmpty(t *testing.T) {
	for name, r := range map[string]*run.Run{
		"nil run":           nil,
		"nil issue context": {},
		"empty context":     heldCommitIssueRun("", 0),
	} {
		t.Run(name, func(t *testing.T) {
			title, body := heldCommitPRTitleBody(r, "", "")
			if title != "" || body != "" {
				t.Errorf("want both empty so the runner keeps its placeholder, got (%q, %q)", title, body)
			}
		})
	}
}

// TestHeldCommitPRTitleBody_PartialRecovery pins binding condition 4: each field
// is used INDEPENDENTLY, so a recovered title is never discarded because the
// body was empty (or the reverse).
func TestHeldCommitPRTitleBody_PartialRecovery(t *testing.T) {
	t.Run("title only", func(t *testing.T) {
		title, body := heldCommitPRTitleBody(heldCommitIssueRun("[E67.5] a placeholder PR", 2570),
			"feat(server): recovered subject", "")
		if title != "feat(server): recovered subject" {
			t.Errorf("a recovered title must survive an empty body, got %q", title)
		}
		if !strings.HasPrefix(body, "## Summary") || !strings.HasSuffix(body, "\n\nCloses #2570") {
			t.Errorf("the MISSING field must be synthesized, got %q", body)
		}
	})

	t.Run("body only", func(t *testing.T) {
		title, body := heldCommitPRTitleBody(heldCommitIssueRun("[E67.5] a placeholder PR", 2570),
			"", "## Summary\n\n- recovered narrative")
		if title != "chore: [E67.5] a placeholder PR" {
			t.Errorf("the MISSING title must be synthesized, got %q", title)
		}
		if !strings.HasPrefix(body, "## Summary\n\n- recovered narrative") {
			t.Errorf("a recovered body must survive an empty title, got %q", body)
		}
	})

	t.Run("title only, no issue context", func(t *testing.T) {
		title, body := heldCommitPRTitleBody(nil, "feat(server): recovered subject", "")
		if title != "feat(server): recovered subject" {
			t.Errorf("title = %q, want the recovered one", title)
		}
		if body != "" {
			t.Errorf("with nothing to synthesize from, the body stays empty, got %q", body)
		}
	})
}

// TestHeldCommitPRTitleBody_ClosesNotDuplicated is m4 and the counterfactual
// vehicle for the closing-reference de-duplication guard.
//
// THE SET DISCRIMINATES IN BOTH DIRECTIONS. The suppress cases (a real closing
// reference already present) are paired with the append cases (a mention that is
// NOT a closing directive, or a closing-looking one that GitHub will not act on),
// so a guard that is too loose fails the append cases and a guard that is absent
// fails the suppress cases. A too-loose guard is the dangerous half: it silences
// the only auto-close directive and the issue stays open on merge.
func TestHeldCommitPRTitleBody_ClosesNotDuplicated(t *testing.T) {
	suppress := map[string]string{
		"canonical":     "## Summary\n\n- x\n\nCloses #2570",
		"lowercase":     "## Summary\n\n- x\n\ncloses #2570",
		"fixes keyword": "## Summary\n\n- x\n\nFixes #2570",
		"resolves":      "## Summary\n\n- x\n\nResolves #2570",
		"colon form":    "## Summary\n\n- x\n\nCloses: #2570",
		"mid-sentence":  "## Summary\n\nThis closes #2570 for good.",
		"trailing dot":  "## Summary\n\n- x\n\nCloses #2570.",
		// The elision must not be so blunt that it hides a REAL directive: an
		// inline span earlier in the body, and a fenced block closed by a run
		// LONGER than its opener (valid per CommonMark), both leave the trailing
		// directive active.
		"after an inline span":       "## Summary\n\nSee `the runbook` first.\n\nCloses #2570",
		"after a longer closing run": "## Summary\n\n```\nsome code\n`````\n\nCloses #2570",
	}
	for name, body := range suppress {
		t.Run("suppress/"+name, func(t *testing.T) {
			_, got := heldCommitPRTitleBody(heldCommitIssueRun("t", 2570), "feat: x", body)
			if n := strings.Count(got, "#2570"); n != 1 {
				t.Errorf("want exactly one reference to #2570, got %d:\n%s", n, got)
			}
			if got != body {
				t.Errorf("a body already closing the issue must be used verbatim:\n got: %q\nwant: %q", got, body)
			}
		})
	}

	// Each of these looks closing-ish but will NOT auto-close #2570, so the
	// directive MUST still be appended. `#2570foo` and `#25701` are the
	// trailing-garbage cases: a boundary that accepted "any non-digit" (or no
	// boundary at all) would let them suppress the real append.
	appendCases := map[string]string{
		"non-closing mention":  "## Summary\n\nRelated to #2570 which describes the defect.",
		"see reference":        "## Summary\n\nSee #2570.",
		"bare reference":       "## Summary\n\nContext: #2570",
		"trailing garbage":     "## Summary\n\nCloses #2570foo",
		"longer number":        "## Summary\n\nCloses #25701",
		"different issue":      "## Summary\n\nCloses #2571",
		"fenced code block":    "## Summary\n\nEnd the body with:\n\n```\nCloses #2570\n```\n",
		"tilde fenced block":   "## Summary\n\n~~~markdown\nCloses #2570\n~~~\n",
		"indented fence":       "## Summary\n\n  ```\n  Closes #2570\n  ```\n",
		"inline code span":     "## Summary\n\nThe body must end with `Closes #2570` on its own line.",
		"double-backtick span": "## Summary\n\nWrite ``Closes #2570`` at the end.",
		// An elided inline span must leave a BOUNDARY behind. Eliding to nothing
		// collapses this into the active directive "Closes  #2570" that the source
		// text does not contain, and GitHub closes nothing for it.
		"span between keyword and reference": "## Summary\n\nCloses `note` #2570",
		// Only a VALID closing fence closes a block. A shorter backtick run and an
		// info-string line are fence CONTENT; treating either as a closer exposes
		// the directive that is still inside the block.
		"shorter run inside a longer fence": "## Summary\n\n````\n```\nCloses #2570\n````\n",
		"info-string line inside a fence":   "## Summary\n\n```\n```go\nCloses #2570\n```\n",
	}
	for name, body := range appendCases {
		t.Run("append/"+name, func(t *testing.T) {
			_, got := heldCommitPRTitleBody(heldCommitIssueRun("t", 2570), "feat: x", body)
			if !strings.HasSuffix(got, "\n\nCloses #2570") {
				t.Errorf("this is not an ACTIVE closing directive, so one must be appended:\n%s", got)
			}
		})
	}
}

// TestHasClosingReference_ZeroIssueNumber: a run with no issue number has
// nothing to close, so nothing is ever suppressed OR appended.
func TestHasClosingReference_ZeroIssueNumber(t *testing.T) {
	if hasClosingReference("Closes #0", 0) {
		t.Error("issue number 0 is not a real issue")
	}
	_, body := heldCommitPRTitleBody(heldCommitIssueRun("t", 0), "feat: x", "## Summary\n\n- x")
	if strings.Contains(body, "Closes #") {
		t.Errorf("with no issue number there is nothing to close, got %q", body)
	}
}

// TestStripCodeContexts covers the elision helper's own branches: an
// unterminated inline run is literal text (not a span), an elided span leaves a
// boundary rather than joining its neighbours, and only a VALID closing fence
// (same character, at least as long as the opener, no info string) closes a
// block.
func TestStripCodeContexts(t *testing.T) {
	cases := map[string]struct{ in, wantContains, wantMissing string }{
		"inline span elided":       {"a `code` b", "a " + codeSpanElision + " b", "code"},
		"unterminated run literal": {"a ` b Closes #1", "Closes #1", ""},
		"fence elides its content": {"x\n```\nCloses #1\n```\ny", "y", "Closes #1"},
		"mismatched fence stays open": {
			"```\nCloses #1\n~~~\nCloses #2\n", "", "Closes #2",
		},
		// The elision leaves a boundary: the two sides must NOT join into text the
		// source never contained.
		"span leaves a boundary": {"Closes `note` #1", "", "Closes  #1"},
		// Closing-fence validation: neither a shorter run nor an info-string line
		// ends the block, so the directive inside stays elided.
		"shorter run does not close": {"````\n```\nCloses #1\n````\ny", "y", "Closes #1"},
		"info string does not close": {"```\n```go\nCloses #1\n```\ny", "y", "Closes #1"},
		// ...but a LONGER closing run is valid, so what follows it is active text.
		"longer run closes": {"```\nx\n`````\nCloses #1\n", "Closes #1", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := stripCodeContexts(tc.in)
			if tc.wantContains != "" && !strings.Contains(got, tc.wantContains) {
				t.Errorf("stripCodeContexts(%q) = %q, want it to contain %q", tc.in, got, tc.wantContains)
			}
			if tc.wantMissing != "" && strings.Contains(got, tc.wantMissing) {
				t.Errorf("stripCodeContexts(%q) = %q, want %q elided", tc.in, got, tc.wantMissing)
			}
		})
	}
}

// TestConventionalCommitHeaderRe_MatchesOrchestratorSource pins the THIRD copy
// of the conventional-commit-header pattern against the orchestrator's, which
// lives in the SAME Go module and is therefore directly readable. The var is
// unexported, so the pin is against that file's source literal: a drift in
// either copy fails here rather than silently changing whether a resumed PR
// title gets the `chore: ` prefix. (The runner's copy is in a separate module
// and can only be comment-guarded.)
func TestConventionalCommitHeaderRe_MatchesOrchestratorSource(t *testing.T) {
	src, err := os.ReadFile("../orchestrator/orchestrator.go")
	if err != nil {
		t.Fatalf("read the orchestrator source that owns the mirrored pattern: %v", err)
	}
	decl := regexp.MustCompile("var conventionalCommitHeaderRe = regexp\\.MustCompile\\(`([^`]*)`\\)")
	m := decl.FindSubmatch(src)
	if m == nil {
		t.Fatal("orchestrator.go no longer declares conventionalCommitHeaderRe as a raw-string MustCompile; " +
			"re-point this pin at wherever the mirrored pattern moved")
	}
	if got, want := conventionalCommitHeaderRe.String(), string(m[1]); got != want {
		t.Errorf("the conventional-commit-header pattern drifted between its two backend copies:\n"+
			" server: %s\n orchestrator: %s", got, want)
	}
}
