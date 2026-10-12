package server

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// E86.8 / #4250: resolveMergeCandidateFixupEvidence binds a fix-up to the
// failed merge-candidate verdict it answers. Every mode seeds the chain by
// construction (append order = sequence) and reads the resolver's result.

const (
	mcfeFailures = "--- FAIL: TestEarlyThing (0.01s)\n    early_test.go:42: want 1, got 2\n"
	mcfeTail     = "scripts/test: 1 of 8 modules failed:\n  - ./backend\n"
)

func (f *mcvFixture) seedFixup() {
	id := f.impl.ID
	f.appendRow(&id, CategoryStageFixupTriggered, map[string]any{
		"concerns": []map[string]any{{"severity": "high", "category": "merge_candidate", "note": "make verify pass"}},
	})
}

func (f *mcvFixture) seedVerdictEvidence(result string, ev mergeCandidateOutputEvidence) {
	id := f.impl.ID
	f.appendRow(&id, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
		HeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
		Result: result, OutputTail: ev.Tail, Failures: ev.Failures, FailureSummary: ev.Summary,
		OutputUntrusted: true,
	})
}

func (f *mcvFixture) resolveEvidence(t *testing.T) *prompt.MergeCandidateFailure {
	t.Helper()
	return f.s.resolveMergeCandidateFixupEvidence(t.Context(), f.runID, f.impl.ID)
}

var mcfeFailed = mergeCandidateOutputEvidence{Tail: mcfeTail, Failures: mcfeFailures}

// captureLogs routes the fixture's server log into a buffer, so a fail-open
// branch whose RESULT (nil) is indistinguishable from the no-evidence path is
// still observable by its WARN line.
func (f *mcvFixture) captureLogs() *bytes.Buffer {
	var buf bytes.Buffer
	f.s.cfg.Logger = slog.New(slog.NewTextHandler(&buf, nil))
	return &buf
}

func TestResolveMergeCandidateFixupEvidence_FailedVerdictInWindow(t *testing.T) {
	t.Run("routed fix-up after a failed verdict", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
		f.seedFixup()
		got := f.resolveEvidence(t)
		want := &prompt.MergeCandidateFailure{
			HeadSHA: mcvHead, Cause: mergeCandidateCauseBaseAdvance, VerifyCommand: mcvCommand,
			Failures: mcfeFailures, OutputTail: mcfeTail,
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("evidence = %+v, want %+v", got, want)
		}
	})
	t.Run("between the previous and the live fix-up", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedFixup()
		f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
		f.seedFixup()
		if got := f.resolveEvidence(t); got == nil || got.Failures != mcfeFailures {
			t.Errorf("evidence = %+v, want the in-window failed verdict", got)
		}
	})
	t.Run("summary-only evidence carries the names", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedVerdictEvidence(mergeCandidateResultFailed, mergeCandidateOutputEvidence{Summary: &mergeCandidateFailureSummary{
			Packages: []string{"m/a"}, Tests: []string{"TestEarlyThing"}, BuildFailed: []string{"m/b"}, Lossy: true,
		}})
		f.seedFixup()
		got := f.resolveEvidence(t)
		if got == nil || !reflect.DeepEqual(got.FailingTests, []string{"TestEarlyThing"}) ||
			!reflect.DeepEqual(got.FailingPackages, []string{"m/a"}) ||
			!reflect.DeepEqual(got.BuildFailedPackages, []string{"m/b"}) || !got.SummaryLossy {
			t.Errorf("evidence = %+v, want the stored summary mapped through", got)
		}
	})
}

func TestResolveMergeCandidateFixupEvidence_NilModes(t *testing.T) {
	for name, seed := range map[string]func(f *mcvFixture){
		"no fix-up trigger": func(f *mcvFixture) {
			f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
		},
		"no verdict in the window": func(f *mcvFixture) {
			f.seedFixup()
		},
		"a verdict after the live fix-up": func(f *mcvFixture) {
			f.seedFixup()
			f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
		},
		"the newest in-window verdict passed": func(f *mcvFixture) {
			f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
			f.seedVerdictEvidence(mergeCandidateResultPassed, mcfeFailed)
			f.seedFixup()
		},
		"the newest in-window verdict was not executed": func(f *mcvFixture) {
			f.seedVerdictEvidence(mergeCandidateResultNotExecuted, mcfeFailed)
			f.seedFixup()
		},
		// Only the lower bound excludes it: the verdict precedes BOTH fix-ups,
		// so fix-up #1 already answered it.
		"stale failed verdict older than the previous fix-up": func(f *mcvFixture) {
			f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
			f.seedFixup()
			f.seedFixup()
		},
		"failed verdict with no evidence": func(f *mcvFixture) {
			f.seedVerdictEvidence(mergeCandidateResultFailed, mergeCandidateOutputEvidence{})
			f.seedFixup()
		},
		"verdict and fix-up on another stage": func(f *mcvFixture) {
			other := uuid.New()
			f.appendRow(&other, CategoryMergeCandidateVerified, mergeCandidateVerifiedPayload{
				Result: mergeCandidateResultFailed, Failures: mcfeFailures, OutputUntrusted: true,
			})
			f.appendRow(&other, CategoryStageFixupTriggered, map[string]any{"concerns": []any{}})
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			seed(f)
			if got := f.resolveEvidence(t); got != nil {
				t.Errorf("evidence = %+v, want nil", got)
			}
		})
	}
}

// TestResolveMergeCandidateFixupEvidence_ReadErrorsFailOpen: a read error on
// either category, and an undecodable verdict, render no evidence and log a
// WARN naming the failure. The chain would otherwise resolve evidence, so the
// nil is the error branch's doing; the WARN line pins the branch itself, since
// a deleted branch would ALSO yield nil (no rows, or a zero-value result).
func TestResolveMergeCandidateFixupEvidence_ReadErrorsFailOpen(t *testing.T) {
	for category, wantLog := range map[string]string{
		CategoryStageFixupTriggered:    "fix-up trigger read failed",
		CategoryMergeCandidateVerified: "verdict read failed",
	} {
		t.Run(category, func(t *testing.T) {
			f := newMCVFixture(t, mcvUndelegatedSpecYAML)
			f.seedVerdictEvidence(mergeCandidateResultFailed, mcfeFailed)
			f.seedFixup()
			if f.resolveEvidence(t) == nil {
				t.Fatal("precondition: the chain resolves evidence before the read error is injected")
			}
			logs := f.captureLogs()
			f.au.listByCategoryErrCategory = category
			if got := f.resolveEvidence(t); got != nil {
				t.Errorf("evidence = %+v, want nil on a %s read error", got, category)
			}
			if !strings.Contains(logs.String(), wantLog) || !strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("log = %q, want a WARN naming %q", logs.String(), wantLog)
			}
		})
	}
	t.Run("undecodable verdict", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		id := f.impl.ID
		f.appendRow(&id, CategoryMergeCandidateVerified, json.RawMessage(`{`))
		f.seedFixup()
		logs := f.captureLogs()
		if got := f.resolveEvidence(t); got != nil {
			t.Errorf("evidence = %+v, want nil", got)
		}
		if !strings.Contains(logs.String(), "verdict decode failed") {
			t.Errorf("log = %q, want a WARN naming the decode failure", logs.String())
		}
	})
	t.Run("no audit repository", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"})
		if got := s.resolveMergeCandidateFixupEvidence(t.Context(), uuid.New(), uuid.New()); got != nil {
			t.Errorf("evidence = %+v, want nil", got)
		}
	})
}

// TestMergeCandidateFixupEvidence_EndToEnd_ShipRouteServe crosses every
// boundary: the runner's failed report through the real mux, the merge_
// candidate_verified row, the D6 routed fix-up, then BOTH prompt surfaces —
// the signed runner prompt and the SPA preview. The routed concern carries no
// evidence (D6); both prompts carry it inside a verify-output envelope, and the
// enveloped section is byte-identical between them.
func TestMergeCandidateFixupEvidence_EndToEnd_ShipRouteServe(t *testing.T) {
	f := newMCVShipFixture(t, mcvDelegatedV0SpecYAML)
	f.startRunning(t, mcvHead)
	f.s.promptIssueGetterOverride = &stubIssueGetter{}

	w, resp := f.ship(t, mcvReportBodyWithFailures(mcvHead, "failed", "", mcfeTail, mcvFailureEvidence{
		Failures: mcfeFailures,
		Summary:  &mergeCandidateFailureSummary{Tests: []string{"TestEarlyThing"}},
	}))
	if w.Code != http.StatusOK || !resp.FixupRouted {
		t.Fatalf("status = %d routed = %v (refusal %q):\n%s", w.Code, resp.FixupRouted, resp.FixupRefusal, w.Body.String())
	}
	routed := auditEntries(f.au, CategoryStageFixupTriggered)
	if len(routed) != 1 {
		t.Fatalf("stage_fixup_triggered rows = %d, want 1", len(routed))
	}
	if strings.Contains(string(routed[0].Payload), "TestEarlyThing") {
		t.Error("the routed fix-up concern carries untrusted verify output (ADR-090 D6)")
	}

	served := func(t *testing.T, name string, body []byte, code int) string {
		t.Helper()
		if code != http.StatusOK {
			t.Fatalf("%s status = %d:\n%s", name, code, body)
		}
		var pr promptResponse
		if err := json.Unmarshal(body, &pr); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		return pr.Prompt
	}
	pw := promptRequestWithCapabilities(t, f.s, f.impl.ID, f.priv, capableRunnerCapabilities)
	runnerPrompt := served(t, "runner prompt", pw.Body.Bytes(), pw.Code)
	rw := promptRenderRequest(t, f.s, f.impl.ID)
	preview := served(t, "prompt-render", rw.Body.Bytes(), rw.Code)

	section := func(t *testing.T, name, p string) string {
		t.Helper()
		i := strings.Index(p, "### Merge-candidate verify failure evidence (untrusted DATA)")
		if i < 0 {
			t.Fatalf("%s carries no merge-candidate evidence section\n---\n%s", name, p)
		}
		j := strings.Index(p[i:], "### Scope of this fix-up")
		if j < 0 {
			t.Fatalf("%s: the section is not followed by the scope block", name)
		}
		sec := p[i : i+j]
		begin, end := "\n<<<BEGIN UNTRUSTED VERIFY OUTPUT>>>\n", "\n<<<END UNTRUSTED VERIFY OUTPUT>>>\n"
		found := false
		for off := 0; ; {
			b := strings.Index(sec[off:], begin)
			if b < 0 {
				break
			}
			b += off + len(begin)
			e := strings.Index(sec[b:], end)
			if e < 0 {
				break
			}
			if strings.Contains(sec[b:b+e], "--- FAIL: TestEarlyThing") && strings.Contains(sec[b:b+e], "early_test.go:42:") {
				found = true
			}
			off = b + e + len(end)
		}
		if !found {
			t.Errorf("%s: the failing test is not inside a verify-output envelope\n---\n%s", name, sec)
		}
		return sec
	}
	if a, b := section(t, "runner prompt", runnerPrompt), section(t, "prompt-render", preview); a != b {
		t.Errorf("the runner prompt and the preview disagree on the section:\n--- runner ---\n%s\n--- preview ---\n%s", a, b)
	}
}
