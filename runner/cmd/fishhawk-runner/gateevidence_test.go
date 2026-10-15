package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// diffOf builds a constraint.Diff of modified files for makeGitDiffEvent.
func diffOf(paths ...string) constraint.Diff {
	var d constraint.Diff
	for _, p := range paths {
		d.ChangedFiles = append(d.ChangedFiles, constraint.ChangedFile{Path: p, Status: "M"})
	}
	return d
}

// decodeEvidence unpacks a composed gate_evidence event for assertions.
func decodeEvidence(t *testing.T, ev *agent.Event) gateEvidencePayload {
	t.Helper()
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil, want event")
	}
	if ev.Kind != "gate_evidence" {
		t.Fatalf("kind = %q, want gate_evidence", ev.Kind)
	}
	var p gateEvidencePayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("payload decode: %v", err)
	}
	return p
}

func TestComposeGateEvidence_NilWhenNoGateRan(t *testing.T) {
	// Agent chatter and even a git_diff alone are not gates: only
	// verify_run / verify_summary / policy_event mean a gate ran.
	events := []agent.Event{
		{Kind: "system.init", Payload: json.RawMessage(`{}`)},
		{Kind: "raw", Payload: json.RawMessage(`{"text":"hello"}`)},
		makeGitDiffEvent("origin/main", diffOf("a.go"), "", false),
	}
	if ev := composeGateEvidence(events, 3); ev != nil {
		t.Fatalf("expected nil when no gate ran, got %s", ev.Payload)
	}
	if ev := composeGateEvidence(nil, 0); ev != nil {
		t.Fatalf("expected nil for empty events, got %s", ev.Payload)
	}
}

func TestComposeGateEvidence_FoldsBindingAssertions(t *testing.T) {
	// A binding_assertion event (#1171) is a gate on its own, and its
	// per-assertion satisfied verdicts fold into gate_evidence.
	events := []agent.Event{
		bindingAssertionEvidenceEvent([]gitops.BindingAssertionResult{
			{Type: "file_contains", Path: "docs/api/v0.md", Literal: "binding_assertions", Satisfied: true},
			{Type: "test_asserts", Path: "x_test.go", Literal: "TestX", Satisfied: false},
		}),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 2))
	if len(p.BindingAssertions) != 2 {
		t.Fatalf("binding_assertions = %d, want 2", len(p.BindingAssertions))
	}
	if p.BindingAssertions[0] != (bindingAssertionEvidence{Type: "file_contains", Path: "docs/api/v0.md", Literal: "binding_assertions", Satisfied: true}) {
		t.Errorf("assertion[0] = %+v", p.BindingAssertions[0])
	}
	if p.BindingAssertions[1].Satisfied {
		t.Errorf("assertion[1] satisfied = true, want false: %+v", p.BindingAssertions[1])
	}
}

func TestComposeGateEvidence_FoldsScopeExemptions(t *testing.T) {
	// A scope_files_exempted event (#1153) is a gate on its own, and its
	// validated path/reason entries fold into gate_evidence.ScopeExemptions.
	// A SINGLE such event folds to exactly its entries — no duplication — so
	// the run()-side single emission cannot double-count (binding condition 1).
	cfg := config{runID: "r1", stageID: "s1"}
	events := []agent.Event{
		scopeFilesExemptedEvent(cfg, []scopeExemption{
			{Path: "a.go", Reason: "already correct"},
			{Path: "b.go", Reason: "no change needed"},
		}),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 2))
	if len(p.ScopeExemptions) != 2 {
		t.Fatalf("scope_exemptions = %d, want 2 (single event folds once)", len(p.ScopeExemptions))
	}
	if p.ScopeExemptions[0] != (scopeExemptionEvidence{Path: "a.go", Reason: "already correct"}) {
		t.Errorf("exemption[0] = %+v", p.ScopeExemptions[0])
	}
	if p.ScopeExemptions[1] != (scopeExemptionEvidence{Path: "b.go", Reason: "no change needed"}) {
		t.Errorf("exemption[1] = %+v", p.ScopeExemptions[1])
	}
}

func TestComposeGateEvidence_FoldsFixupSelfReportDivergence(t *testing.T) {
	// A fixup_selfreport_divergence event (#1210) is a gate on its own, and its
	// claimed/actual statuses fold into gate_evidence.FixupSelfReportDivergence.
	cfg := config{runID: "r1", stageID: "s1"}
	events := []agent.Event{
		fixupSelfReportDivergenceEvent(cfg, "passed", "failed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.FixupSelfReportDivergence == nil {
		t.Fatal("FixupSelfReportDivergence = nil, want folded event")
	}
	if *p.FixupSelfReportDivergence != (fixupSelfReportDivergenceEvidence{ClaimedVerifyStatus: "passed", ActualVerifyStatus: "failed"}) {
		t.Errorf("FixupSelfReportDivergence = %+v", *p.FixupSelfReportDivergence)
	}
}

func TestComposeGateEvidence_NoFixupSelfReportDivergenceField(t *testing.T) {
	// A stage with a verify gate but no divergence event leaves the field nil —
	// byte-identical to before #1210.
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.FixupSelfReportDivergence != nil {
		t.Errorf("FixupSelfReportDivergence = %+v, want nil when no event", p.FixupSelfReportDivergence)
	}
}

func TestComposeGateEvidence_NoBindingAssertionsField(t *testing.T) {
	// A stage with a verify gate but no binding_assertion event adds no
	// binding_assertions field — byte-identical to before #1171.
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.BindingAssertions != nil {
		t.Errorf("BindingAssertions = %+v, want nil when no event", p.BindingAssertions)
	}
}

func TestComposeGateEvidence_BoundsTailToLastLines(t *testing.T) {
	var b strings.Builder
	for i := 1; i <= 100; i++ {
		fmt.Fprintf(&b, "line-%03d\n", i)
	}
	events := []agent.Event{
		verifyRunEvent("scripts/test", "abc123", "def456", 1, b.String(), "failed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 2))

	if len(p.VerifyRuns) != 1 {
		t.Fatalf("verify_runs = %d, want 1", len(p.VerifyRuns))
	}
	vr := p.VerifyRuns[0]
	if vr.Command != "scripts/test" || vr.ExitCode != 1 || vr.Outcome != "failed" {
		t.Errorf("run fields = %+v", vr)
	}
	if vr.HeadSHA != "abc123" || vr.TreeSHA != "def456" {
		t.Errorf("sha fields = %+v", vr)
	}
	lines := strings.Split(vr.OutputTail, "\n")
	if len(lines) != gateEvidenceTailLines {
		t.Fatalf("tail has %d lines, want %d", len(lines), gateEvidenceTailLines)
	}
	if lines[0] != "line-071" || lines[len(lines)-1] != "line-100" {
		t.Errorf("tail window wrong: first=%q last=%q", lines[0], lines[len(lines)-1])
	}
	if !vr.TailTruncated {
		t.Error("TailTruncated = false, want true")
	}
}

func TestComposeGateEvidence_BoundsTailToByteCap(t *testing.T) {
	// One enormous line defeats the line bound; the byte cap must hold
	// and keep the SUFFIX (failure summaries end the output).
	output := strings.Repeat("x", 3*gateEvidenceTailBytes) + "FINAL"
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 2, output, "failed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))

	tail := p.VerifyRuns[0].OutputTail
	if len(tail) > gateEvidenceTailBytes {
		t.Fatalf("tail is %d bytes, cap is %d", len(tail), gateEvidenceTailBytes)
	}
	if !strings.HasSuffix(tail, "FINAL") {
		t.Errorf("tail lost the suffix: ...%q", tail[len(tail)-10:])
	}
	if !p.VerifyRuns[0].TailTruncated {
		t.Error("TailTruncated = false, want true")
	}
}

func TestComposeGateEvidence_ShortOutputNotTruncated(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\nall passed\n", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	vr := p.VerifyRuns[0]
	if vr.OutputTail != "ok\nall passed" {
		t.Errorf("tail = %q", vr.OutputTail)
	}
	if vr.TailTruncated {
		t.Error("TailTruncated = true for short output")
	}
}

func TestComposeGateEvidence_RedactsTailAndDetail(t *testing.T) {
	secret := "ghp_" + strings.Repeat("z", 36)
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 1, "FAIL: leaked "+secret+" in output", "failed"),
		{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{
			"outcome": "failed", "iterations": 2, "max_iterations": 3,
			"detail": "abort after " + secret,
		})},
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))

	if got := p.VerifyRuns[0].OutputTail; strings.Contains(got, secret) ||
		!strings.Contains(got, "[REDACTED:github-pat-classic]") {
		t.Errorf("tail not redacted: %q", got)
	}
	if p.VerifySummary == nil {
		t.Fatal("verify_summary missing")
	}
	if got := p.VerifySummary.Detail; strings.Contains(got, secret) ||
		!strings.Contains(got, "[REDACTED:github-pat-classic]") {
		t.Errorf("detail not redacted: %q", got)
	}
	if p.VerifySummary.Outcome != "failed" || p.VerifySummary.Iterations != 2 ||
		p.VerifySummary.MaxIterations != 3 {
		t.Errorf("summary fields = %+v", p.VerifySummary)
	}
}

func TestComposeGateEvidence_RedactsBeforeBounding(t *testing.T) {
	// A secret sitting exactly across the byte-cap cut must be redacted
	// from the FULL text first — bounding first would slice it in half
	// and the fragment would no longer match any pattern.
	secret := "ghp_" + strings.Repeat("q", 36)
	output := strings.Repeat("x", gateEvidenceTailBytes-20) + secret + strings.Repeat("y", 40)
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 1, output, "failed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	tail := p.VerifyRuns[0].OutputTail
	if strings.Contains(tail, "ghp_") {
		t.Errorf("credential fragment leaked into tail: %q", tail)
	}
}

func TestComposeGateEvidence_ReRedactionIsNoOp(t *testing.T) {
	// The redacted bundle variant re-runs RedactDefault over the whole
	// composed payload (redactEvents). That second pass must be a no-op:
	// the markers don't match any credential pattern and the payload's
	// own field names don't trip the json-password-field rule.
	secret := "sk-ant-api03-" + strings.Repeat("k", 40)
	events := []agent.Event{
		verifyRunEvent("scripts/test", "h", "t", 1, "boom "+secret, "failed"),
		{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{
			"outcome": "failed", "iterations": 1, "max_iterations": 1, "detail": secret,
		})},
		{Kind: "policy_event", Payload: agent.MakePayload(map[string]any{
			"check": "constraints", "outcome": "violation",
			"constraint": "forbidden_paths", "detail": "touched " + secret,
			"files": []string{".github/workflows/ci.yml"},
		})},
	}
	ev := composeGateEvidence(events, 1)
	if ev == nil {
		t.Fatal("nil evidence")
	}
	once, hits := redaction.RedactDefault([]byte(ev.Payload))
	if len(hits) != 0 {
		t.Errorf("composed payload still had redactable content: %+v", hits)
	}
	twice, _ := redaction.RedactDefault(once)
	if !bytes.Equal(once, twice) {
		t.Errorf("re-redaction not idempotent:\nonce:  %s\ntwice: %s", once, twice)
	}
	if !bytes.Equal([]byte(ev.Payload), once) {
		t.Errorf("payload was not already fully redacted:\nraw:  %s\nonce: %s", ev.Payload, once)
	}
}

func TestComposeGateEvidence_SkipClassificationPassthrough(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", -1, "worktree_tmp: mkdir: no space left", "skipped"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	vr := p.VerifyRuns[0]
	if vr.Outcome != "skipped" || vr.ExitCode != -1 {
		t.Errorf("skip fields = %+v", vr)
	}
	if !strings.Contains(vr.OutputTail, "worktree_tmp: mkdir: no space left") {
		t.Errorf("skip reason lost: %q", vr.OutputTail)
	}
}

func TestComposeGateEvidence_ScopeFactsAndViolations(t *testing.T) {
	events := []agent.Event{
		{Kind: "policy_event", Payload: agent.MakePayload(map[string]any{
			"check": "scope_drift", "outcome": "excluded",
			"undeclared": []string{"stray/file.go", "other/thing.md"},
			"undeclared_categorized": []driftPathEvidence{
				{Path: "stray/file.go", Category: "A", Disposition: "excluded_from_commit"},
				{Path: "other/thing.md", Category: "B", Disposition: "would_fail_loud"},
			},
		})},
		{Kind: "policy_event", Payload: agent.MakePayload(map[string]any{
			"check": "constraints", "outcome": "violation",
			"constraint": "max_files_changed", "detail": "47 > 45",
			"files": []string{"a.go"},
		})},
		// Pre-fix-loop diff says 5 files; the #870 re-emit says 4 —
		// last-write-wins, matching the backend's ExtractDiff.
		makeGitDiffEvent("origin/main", diffOf("a.go", "b.go", "c.go", "d.go", "e.go"), "", false),
		makeGitDiffEvent("origin/main", diffOf("a.go", "b.go", "c.go", "d.go"), "", false),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 7))

	sf := p.ScopeFacts
	if sf == nil {
		t.Fatal("scope_facts missing")
	}
	if sf.DeclaredFiles != 7 {
		t.Errorf("declared_files = %d, want 7", sf.DeclaredFiles)
	}
	if sf.StagedFiles == nil || *sf.StagedFiles != 4 {
		t.Errorf("staged_files = %v, want 4 (last git_diff wins)", sf.StagedFiles)
	}
	wantDrift := []string{"stray/file.go", "other/thing.md"}
	if len(sf.UndeclaredPaths) != len(wantDrift) {
		t.Fatalf("undeclared_paths = %v, want %v", sf.UndeclaredPaths, wantDrift)
	}
	for i, w := range wantDrift {
		if sf.UndeclaredPaths[i] != w {
			t.Errorf("undeclared_paths[%d] = %q, want %q", i, sf.UndeclaredPaths[i], w)
		}
	}
	wantCategorized := []driftPathEvidence{
		{Path: "stray/file.go", Category: "A", Disposition: "excluded_from_commit"},
		{Path: "other/thing.md", Category: "B", Disposition: "would_fail_loud"},
	}
	if len(sf.UndeclaredCategorized) != len(wantCategorized) {
		t.Fatalf("undeclared_categorized = %+v, want %+v", sf.UndeclaredCategorized, wantCategorized)
	}
	for i, w := range wantCategorized {
		if sf.UndeclaredCategorized[i] != w {
			t.Errorf("undeclared_categorized[%d] = %+v, want %+v", i, sf.UndeclaredCategorized[i], w)
		}
	}

	if len(p.PolicyViolations) != 1 {
		t.Fatalf("policy_violations = %d, want 1", len(p.PolicyViolations))
	}
	v := p.PolicyViolations[0]
	if v.Check != "constraints" || v.Constraint != "max_files_changed" ||
		v.Detail != "47 > 45" || len(v.Files) != 1 || v.Files[0] != "a.go" {
		t.Errorf("violation = %+v", v)
	}
	// Non-violation policy_events (constraints valid, plan_validation
	// valid) count as "a gate ran" but produce no violation entries.
	valid := []agent.Event{
		{Kind: "policy_event", Payload: agent.MakePayload(map[string]any{
			"check": "constraints", "outcome": "valid", "files_checked": 3,
		})},
	}
	pv := decodeEvidence(t, composeGateEvidence(valid, 3))
	if len(pv.PolicyViolations) != 0 {
		t.Errorf("valid policy_event produced violations: %+v", pv.PolicyViolations)
	}
}

func TestComposeGateEvidence_UncategorizedDriftStaysNil(t *testing.T) {
	// A scope_drift event without the additive undeclared_categorized
	// key (an older emitter, or the categorize-failed degradation path)
	// must still surface the undeclared list while leaving the
	// categorized slice nil — the tolerant-decode contract (#991).
	events := []agent.Event{
		{Kind: "policy_event", Payload: agent.MakePayload(map[string]any{
			"check": "scope_drift", "outcome": "excluded",
			"undeclared": []string{"stray/file.go"},
		})},
	}
	p := decodeEvidence(t, composeGateEvidence(events, 2))
	sf := p.ScopeFacts
	if sf == nil {
		t.Fatal("scope_facts missing")
	}
	if len(sf.UndeclaredPaths) != 1 || sf.UndeclaredPaths[0] != "stray/file.go" {
		t.Errorf("undeclared_paths = %v, want [stray/file.go]", sf.UndeclaredPaths)
	}
	if sf.UndeclaredCategorized != nil {
		t.Errorf("undeclared_categorized = %+v, want nil", sf.UndeclaredCategorized)
	}
}

func TestComposeGateEvidence_CountsFlakeRetries(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "h1", "t1", 1, "context deadline exceeded", "failed"),
		{Kind: "verify_infra_flake_retry", Payload: agent.MakePayload(map[string]any{
			"iteration": 1, "detail": "testcontainers start timeout",
		})},
		verifyRunEvent("scripts/test", "h1", "t1", 0, "ok", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))
	if p.FlakeRetries != 1 {
		t.Errorf("flake_retries = %d, want 1", p.FlakeRetries)
	}
	if len(p.VerifyRuns) != 2 {
		t.Errorf("verify_runs = %d, want 2", len(p.VerifyRuns))
	}
}

func TestComposeGateEvidence_AbsorbedRunMarkedSuperseded(t *testing.T) {
	// Absorbed-then-passed: the verify-fix loop ran the gate, it failed, the
	// agent fixed the tree, and the re-run passed (#1205). The first
	// (absorbed) run operated on a stale tree → Superseded; the LAST run is
	// the terminal/authoritative attempt matching verify_summary → NOT marked,
	// so an absorbed iteration is not surfaced as a committed-tree blocker.
	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "h1", "t1", 1, "[build failed]", "failed"),
		verifyRunEvent("scripts/test verify", "h2", "t2", 0, "ok", "passed"),
		{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{
			"outcome": "passed", "iterations": 2, "max_iterations": 3,
		})},
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))
	if len(p.VerifyRuns) != 2 {
		t.Fatalf("verify_runs = %d, want 2", len(p.VerifyRuns))
	}
	if !p.VerifyRuns[0].Superseded {
		t.Error("absorbed (first) run Superseded = false, want true")
	}
	if p.VerifyRuns[1].Superseded {
		t.Error("terminal (last) run Superseded = true, want false")
	}
	if p.VerifyRuns[1].Outcome != "passed" {
		t.Errorf("terminal outcome = %q, want passed", p.VerifyRuns[1].Outcome)
	}
}

func TestComposeGateEvidence_TerminalFailNotSuperseded(t *testing.T) {
	// Budget-exhausted [fail,fail]: every iteration failed and the loop
	// terminated still red (#1205). The earlier run is marked superseded, but
	// the LAST run is the genuine terminal failure and MUST stay unmarked so
	// it remains a committed-tree HIGH blocker (the Superseded marking must
	// never mask a real terminal failure).
	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "h1", "t1", 1, "[build failed] first", "failed"),
		verifyRunEvent("scripts/test verify", "h2", "t2", 1, "[build failed] last", "failed"),
		{Kind: "verify_summary", Payload: agent.MakePayload(map[string]any{
			"outcome": "failed", "iterations": 2, "max_iterations": 2,
		})},
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))
	if len(p.VerifyRuns) != 2 {
		t.Fatalf("verify_runs = %d, want 2", len(p.VerifyRuns))
	}
	if !p.VerifyRuns[0].Superseded {
		t.Error("first run Superseded = false, want true")
	}
	if p.VerifyRuns[1].Superseded {
		t.Error("terminal failing run Superseded = true, want false — would mask a real committed-tree failure")
	}
	if p.VerifySummary == nil || p.VerifySummary.Outcome != "failed" {
		t.Errorf("verify_summary outcome = %+v, want failed", p.VerifySummary)
	}
}

func TestComposeGateEvidence_SingleRunNeverSuperseded(t *testing.T) {
	// A lone verify_run is the terminal run by definition (len == 1), so it is
	// never marked superseded — the omitempty field stays false (back-compat
	// with the pre-#1205 single-iteration rendering).
	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "h1", "t1", 0, "ok", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))
	if len(p.VerifyRuns) != 1 {
		t.Fatalf("verify_runs = %d, want 1", len(p.VerifyRuns))
	}
	if p.VerifyRuns[0].Superseded {
		t.Error("single run Superseded = true, want false")
	}
}

func TestComposeGateEvidence_SkipsUndecodablePayloadsBestEffort(t *testing.T) {
	events := []agent.Event{
		{Kind: "verify_run", Payload: json.RawMessage(`{not json`)},
		verifyRunEvent("scripts/test", "", "", 0, "ok", "passed"),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if len(p.VerifyRuns) != 1 || p.VerifyRuns[0].Outcome != "passed" {
		t.Errorf("verify_runs = %+v, want only the decodable run", p.VerifyRuns)
	}
}

func TestBoundEvidenceTail_TrimsPartialRune(t *testing.T) {
	// Force the byte cut to land inside a multi-byte rune; the bound
	// must advance to the next rune start instead of emitting invalid
	// UTF-8 into a JSON payload.
	s := strings.Repeat("é", gateEvidenceTailBytes) // 2 bytes each
	out, truncated := boundEvidenceTail(s)
	if !truncated {
		t.Fatal("expected truncation")
	}
	if len(out) > gateEvidenceTailBytes {
		t.Fatalf("len = %d, cap %d", len(out), gateEvidenceTailBytes)
	}
	if !strings.HasPrefix(out, "é") {
		t.Errorf("leading partial rune not trimmed: %q...", out[:4])
	}
}

// TestComposeGateEvidence_FoldsDiffCoverage confirms a diff_coverage
// event (#1888) folds into gate_evidence.DiffCoverage with its measured
// counts intact, and that the event is itself a gate — a stage whose ONLY
// gate is the coverage measurement still produces a gate_evidence event,
// so the backend never sees the measurement as absent.
func TestComposeGateEvidence_FoldsDiffCoverage(t *testing.T) {
	events := []agent.Event{
		diffCoverageEvent(diffCoverageEvidence{
			Outcome:         "measured",
			Command:         "make coverage",
			ExitCode:        0,
			ReportPath:      "coverage.lcov",
			BaseRef:         "main",
			NewLines:        4,
			CoveredNewLines: 3,
			Percent:         75,
			UncoveredFiles:  []string{"src/app.go"},
			Reason:          "3 of 4 new lines covered",
		}),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.DiffCoverage == nil {
		t.Fatal("DiffCoverage = nil, want the folded measurement")
	}
	got := p.DiffCoverage
	if got.Outcome != "measured" || got.NewLines != 4 || got.CoveredNewLines != 3 || got.Percent != 75 {
		t.Errorf("DiffCoverage = %+v", *got)
	}
	if got.Command != "make coverage" || got.ReportPath != "coverage.lcov" || got.BaseRef != "main" {
		t.Errorf("DiffCoverage identity fields = %+v", *got)
	}
	if len(got.UncoveredFiles) != 1 || got.UncoveredFiles[0] != "src/app.go" {
		t.Errorf("UncoveredFiles = %v", got.UncoveredFiles)
	}
}

// TestComposeGateEvidence_FoldsDiffCoverageZeroMeasurement pins the
// vacuous case end to end: a measured-with-zero result is carried as an
// explicit signal, NOT dropped. Absence would read as a violation, so
// dropping it here would fail a legitimately vacuous stage.
func TestComposeGateEvidence_FoldsDiffCoverageZeroMeasurement(t *testing.T) {
	events := []agent.Event{
		diffCoverageEvent(diffCoverageEvidence{
			Outcome: "measured",
			Command: "make coverage",
			BaseRef: "main",
			Reason:  `no added lines against "main"; nothing to measure`,
		}),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.DiffCoverage == nil {
		t.Fatal("DiffCoverage = nil — a measured-zero signal must survive the fold")
	}
	if p.DiffCoverage.Outcome != "measured" || p.DiffCoverage.NewLines != 0 {
		t.Errorf("DiffCoverage = %+v, want measured with zero new lines", *p.DiffCoverage)
	}
	// The zero counts must be present on the wire, not dropped by
	// omitempty — the backend distinguishes measured-zero from absent.
	raw := string(composeGateEvidence(events, 0).Payload)
	for _, want := range []string{`"new_lines":0`, `"covered_new_lines":0`} {
		if !strings.Contains(raw, want) {
			t.Errorf("payload %s omits %s", raw, want)
		}
	}
}

// TestComposeGateEvidence_DiffCoverageBounded pins the two bounding
// rules: the uncovered-file sample is capped, and the free-text reason is
// pre-redacted (the raw bundle dispatches the implement review, so a
// credential echoed by a coverage command's stderr must not reach it).
func TestComposeGateEvidence_DiffCoverageBounded(t *testing.T) {
	var many []string
	for i := 0; i < diffCoverageMaxUncovered*3; i++ {
		many = append(many, fmt.Sprintf("src/f%02d.go", i))
	}
	events := []agent.Event{
		diffCoverageEvent(diffCoverageEvidence{
			Outcome:        "failed",
			Command:        "make coverage",
			ExitCode:       1,
			Reason:         "coverage command exited 1: token ghp_0123456789abcdefghijklmnopqrstuvwxyz",
			UncoveredFiles: many,
		}),
	}
	ev := composeGateEvidence(events, 0)
	p := decodeEvidence(t, ev)
	if got := len(p.DiffCoverage.UncoveredFiles); got != diffCoverageMaxUncovered {
		t.Errorf("UncoveredFiles len = %d, want the %d cap", got, diffCoverageMaxUncovered)
	}
	if strings.Contains(p.DiffCoverage.Reason, "ghp_0123456789abcdefghijklmnopqrstuvwxyz") {
		t.Errorf("Reason %q still carries an unredacted credential", p.DiffCoverage.Reason)
	}
	// The redaction pass over the redacted bundle variant is a no-op on an
	// already-redacted payload — the same property every sibling field has.
	red, _ := redaction.RedactDefault(ev.Payload)
	if !bytes.Equal(red, ev.Payload) {
		t.Errorf("re-redaction changed the payload:\n got %s\nwant %s", red, ev.Payload)
	}
}

// TestComposeGateEvidence_NoDiffCoverageField pins the opt-in default: a
// stage with a verify gate but no diff_coverage event leaves the field
// absent — byte-identical to before #1888.
func TestComposeGateEvidence_NoDiffCoverageField(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
	}
	ev := composeGateEvidence(events, 0)
	p := decodeEvidence(t, ev)
	if p.DiffCoverage != nil {
		t.Errorf("DiffCoverage = %+v, want nil when no event", p.DiffCoverage)
	}
	if strings.Contains(string(ev.Payload), "diff_coverage") {
		t.Errorf("payload %s carries a diff_coverage member, want it omitted", ev.Payload)
	}
}

// fixupReportingObligationsWireFixture is the SHARED literal JSON both modules
// pin the runner↔backend gate-evidence wire contract against (#2737). The
// runner asserts composeGateEvidence emits exactly this for a validated report
// set; the backend asserts bundle.GateEvidence → gateEvidenceForReview decodes
// exactly this (backend/internal/server/trace_test.go's
// TestGateEvidenceForReview_DecodesFixupReportingObligations). No import can
// cross the module seam, so a one-sided json-tag edit fails on the other side —
// the same lockstep defense this file already applies to
// fixup_selfreport_divergence.
const fixupReportingObligationsWireFixture = `"fixup_reporting_obligations":[` +
	`{"id":"ob-1","status":"met"},` +
	`{"id":"ob-2","status":"declined"}]`

// TestComposeGateEvidence_FixupReportingObligationsWireShape is the runner half
// of the lockstep pair: the composer emits the shared literal byte-for-byte.
func TestComposeGateEvidence_FixupReportingObligationsWireShape(t *testing.T) {
	cfg := config{runID: "run-cccc", stageID: "stage-dddd"}
	events := []agent.Event{
		fixupReportingObligationsEvent(cfg, []fixupReportingObligationEvidence{
			{ID: "ob-1", Status: "met"},
			{ID: "ob-2", Status: "declined"},
		}),
	}
	ev := composeGateEvidence(events, 0)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil; a fixup_reporting_obligations event must count as a gate")
	}
	raw := string(ev.Payload)
	if !strings.Contains(raw, fixupReportingObligationsWireFixture) {
		t.Errorf("gate_evidence payload does not carry the shared wire fixture.\ngot:  %s\nwant substring: %s",
			raw, fixupReportingObligationsWireFixture)
	}
	p := decodeEvidence(t, ev)
	if len(p.FixupReportingObligations) != 2 {
		t.Fatalf("FixupReportingObligations = %+v, want 2 entries", p.FixupReportingObligations)
	}
	// The wire shape carries the join key and the status literal and NOTHING
	// else (#2737 security fix-up), so an agent-authored attestation or decline
	// reason has no field to ride out on.
	for _, k := range []string{"record", "reason", "detail", "text"} {
		if strings.Contains(raw, `"`+k+`":`) {
			t.Errorf("gate_evidence payload carries a %q member — agent-authored text must not cross the "+
				"upload boundary:\n%s", k, raw)
		}
	}
}

// TestComposeGateEvidence_NoFixupReportingObligationsField: without the event
// the field is absent, so an ordinary stage's gate_evidence stays byte-identical.
func TestComposeGateEvidence_NoFixupReportingObligationsField(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
	}
	ev := composeGateEvidence(events, 0)
	p := decodeEvidence(t, ev)
	if p.FixupReportingObligations != nil {
		t.Errorf("FixupReportingObligations = %+v, want nil when no event", p.FixupReportingObligations)
	}
	if strings.Contains(string(ev.Payload), "fixup_reporting_obligations") {
		t.Errorf("omitempty must keep the key off the wire:\n%s", ev.Payload)
	}
}

// TestComposeGateEvidence_FixupReportingObligationsMalformedPayloadSkipped: an
// undecodable event payload is skipped, exactly like every sibling fold.
func TestComposeGateEvidence_FixupReportingObligationsMalformedPayloadSkipped(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
		{Kind: "fixup_reporting_obligations", Payload: json.RawMessage(`{"obligations":"not-an-array"}`)},
	}
	p := decodeEvidence(t, composeGateEvidence(events, 0))
	if p.FixupReportingObligations != nil {
		t.Errorf("a malformed fixup_reporting_obligations payload must contribute nothing, got %+v",
			p.FixupReportingObligations)
	}
}

// fixupCounterfactualsWireFixture is the SHARED literal JSON every module pins
// the runner↔backend gate-evidence wire contract for #3042 against. Byte-for-byte
// twins live in backend/internal/bundle/bundle_test.go (asserted to DECODE it)
// and backend/internal/server/trace_test.go (asserted to RENDER it into the
// reviewer prompt). No import can cross the module seam, so a one-sided json-tag
// edit fails on the other side — the same lockstep defense this file already
// applies to fixup_reporting_obligations.
// The second entry's path is a `_test.go` path ON PURPOSE (#3107): the shared
// literal itself then exercises BOTH runner-derived kinds end to end — the runner
// half asserts the real composer EMITS `kind`, the bundle half asserts it
// DECODES, the server half asserts it RENDERS. The member order (control_path,
// kind, observed, restored) is the exact order encoding/json emits from the
// struct field order.
const fixupCounterfactualsWireFixture = `"fixup_counterfactuals":[` +
	`{"control_path":"runner/cmd/fishhawk-runner/main.go","kind":"production","observed":"red","restored":true},` +
	`{"control_path":"backend/internal/prompt/prompt_test.go","kind":"test","observed":"green","restored":false}]`

// TestFixupCounterfactuals_SelfReportToWire_EndToEnd is the RUNNER half of the
// #3042 cross-boundary span (binding condition 2). It starts from the agent's
// raw self-report JSON on disk and drives the REAL chain — loadFixupSelfReport
// (whole-sidecar rules + validateFixupCounterfactuals) -> fixupCounterfactualsEvent
// -> the REAL composeGateEvidence -> the emitted bundle JSON — then asserts the
// emitted gate_evidence payload carries the shared wire fixture BYTE-FOR-BYTE.
// Nothing here hand-writes the evidence shape: the fixture is what the real
// composer produced. The backend half consumes that exact literal.
func TestFixupCounterfactuals_SelfReportToWire_EndToEnd(t *testing.T) {
	cfg := config{runID: "run-cccc", stageID: "stage-dddd"}
	scope := []string{"runner/cmd/fishhawk-runner/main.go", "backend/internal/prompt/prompt_test.go"}

	dir := t.TempDir()
	orig := fixupSelfReportDir
	fixupSelfReportDir = dir
	t.Cleanup(func() { fixupSelfReportDir = orig })

	// The agent's sidecar, verbatim JSON — including a record narrative that
	// must NOT survive the boundary, an out-of-scope entry that must be dropped,
	// and an explicit `"restored": false` that must be RETAINED.
	const sidecar = `{
	  "run_id": "run-cccc",
	  "stage_id": "stage-dddd",
	  "verify_status": "passed",
	  "counterfactuals": [
	    {"control_path":"runner/cmd/fishhawk-runner/main.go","observed":"red","restored":true,
	     "kind":"i-tried-to-declare-test","record":"deleted the scope-membership check; the drop test went red; restored"},
	    {"control_path":"backend/internal/prompt/prompt_test.go","observed":"green","restored":false,
	     "record":"deleted the render branch; the prompt test still passed"},
	    {"control_path":"never/declared/elsewhere.go","observed":"red","restored":true,
	     "record":"out of scope, must be dropped"}
	  ]
	}`
	if err := os.WriteFile(fixupSelfReportPath(cfg.runID, cfg.stageID), []byte(sidecar), 0o600); err != nil {
		t.Fatal(err)
	}

	var logSink strings.Builder
	report := loadFixupSelfReport(cfg, scope, &logSink)
	if len(report.counterfactuals) != 2 {
		t.Fatalf("validated counterfactuals = %+v, want the 2 in-scope entries (log %q)",
			report.counterfactuals, logSink.String())
	}
	ev := composeGateEvidence([]agent.Event{fixupCounterfactualsEvent(cfg, report.counterfactuals)}, 0)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil; a fixup_counterfactuals event must count as a gate")
	}
	raw := string(ev.Payload)
	if !strings.Contains(raw, fixupCounterfactualsWireFixture) {
		t.Errorf("gate_evidence payload does not carry the shared wire fixture.\ngot:  %s\nwant substring: %s",
			raw, fixupCounterfactualsWireFixture)
	}
	// The wire shape carries the scope-member path, the observed literal and the
	// restored bool and NOTHING else, so the agent's record narrative has no
	// field to ride out on.
	for _, k := range []string{"record", "reason", "detail", "text", "test"} {
		if strings.Contains(raw, `"`+k+`":`) {
			t.Errorf("gate_evidence payload carries a %q member — agent-authored text must not cross the "+
				"upload boundary:\n%s", k, raw)
		}
	}
	p := decodeEvidence(t, ev)
	if len(p.FixupCounterfactuals) != 2 ||
		p.FixupCounterfactuals[0] != (fixupCounterfactualEvidence{ControlPath: "runner/cmd/fishhawk-runner/main.go", Kind: "production", Observed: "red", Restored: true}) ||
		p.FixupCounterfactuals[1] != (fixupCounterfactualEvidence{ControlPath: "backend/internal/prompt/prompt_test.go", Kind: "test", Observed: "green", Restored: false}) {
		t.Errorf("FixupCounterfactuals = %+v, want the two validated quads with runner-derived kind", p.FixupCounterfactuals)
	}
	// Derived-not-declared, proven end to end: the first sidecar entry tried to
	// declare its own `"kind"` on a PRODUCTION path, yet the emitted evidence
	// reads `kind:production` — the agent-supplied value was ignored.
	if p.FixupCounterfactuals[0].Kind != "production" {
		t.Errorf("agent-supplied kind must be ignored; got %q, want the runner-derived production",
			p.FixupCounterfactuals[0].Kind)
	}
}

// TestComposeGateEvidence_NoFixupCounterfactualsField: the additive/omitempty
// proof — a stage with no counterfactual event emits no member at all, so every
// existing bundle stays byte-identical.
func TestComposeGateEvidence_NoFixupCounterfactualsField(t *testing.T) {
	ev := composeGateEvidence([]agent.Event{
		verifyRunEvent("scripts/test", "", "", 0, "ok\n", "passed"),
	}, 0)
	if strings.Contains(string(ev.Payload), "fixup_counterfactuals") {
		t.Errorf("payload %s carries a fixup_counterfactuals member, want it omitted", ev.Payload)
	}
}

// approvalConditionResponsesWireFixture is the SHARED literal JSON pinning the
// runner↔backend gate-evidence wire contract for #3400. A byte-for-byte twin
// lives in backend/internal/bundle/bundle_test.go (asserted to DECODE it into
// GateEvidence.ApprovalConditionResponses with every field populated). No
// import can cross the module seam, so a one-sided json-tag edit fails on the
// other side — the same lockstep defense fixup_counterfactuals uses. The
// member order (source, text, truncated) is the order encoding/json emits from
// the struct field order.
const approvalConditionResponsesWireFixture = `"approval_condition_responses":` +
	`{"source":"implement_commitmsg","text":"- condition 1: added the drift test\n- condition 2: no action required because the path is a permission","truncated":true}`

// TestComposeGateEvidence_ApprovalConditionResponses drives the REAL composer
// with the event peekApprovalConditionResponses emits and asserts the packed
// gate_evidence payload carries the shared wire fixture BYTE-FOR-BYTE; the
// event-less compose omits the member entirely (byte-identical default).
func TestComposeGateEvidence_ApprovalConditionResponses(t *testing.T) {
	ev := composeGateEvidence([]agent.Event{{
		Kind: "approval_condition_responses",
		Payload: agent.MakePayload(map[string]any{
			"run_id":    "r",
			"stage_id":  "s",
			"source":    "implement_commitmsg",
			"text":      "- condition 1: added the drift test\n- condition 2: no action required because the path is a permission",
			"truncated": true,
		}),
	}}, 1)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil; an approval_condition_responses event must count as a gate")
	}
	raw := string(ev.Payload)
	if !strings.Contains(raw, approvalConditionResponsesWireFixture) {
		t.Errorf("gate_evidence payload does not carry the shared wire fixture.\ngot:  %s\nwant substring: %s",
			raw, approvalConditionResponsesWireFixture)
	}
	p := decodeEvidence(t, ev)
	if p.ApprovalConditionResponses == nil || p.ApprovalConditionResponses.Source != "implement_commitmsg" ||
		!p.ApprovalConditionResponses.Truncated {
		t.Errorf("ApprovalConditionResponses = %+v", p.ApprovalConditionResponses)
	}

	// Event-less compose (a verify_run alone) omits the member.
	plain := composeGateEvidence([]agent.Event{{
		Kind:    "verify_run",
		Payload: agent.MakePayload(map[string]any{"command": "x", "exit_code": 0, "outcome": "passed"}),
	}}, 1)
	if strings.Contains(string(plain.Payload), "approval_condition_responses") {
		t.Errorf("event-less compose must omit the member: %s", plain.Payload)
	}
}

// ---------------------------------------------------------------------------
// gate_isolation member (#2135)
// ---------------------------------------------------------------------------

// Fixed image identities the gate_container golden members (E51.3 / #2136)
// carry: a registry digest, a runtime-local image id and a build content
// digest, each a distinct realistic sha256.
const (
	goldenImageDigest   = "sha256:9b2e6d1c4a7f30e8b5c2d9f4a1e6b3c8d7f0a2e5b9c4d1f6a3e8b2c7d0f5a9e4"
	goldenImageID       = "sha256:3c1f8a6e2d9b4f7a0e5c8d3b6f1a9e4c7d2b5f8a1e6c9d4b7f0a3e8c2d5b9f6a"
	goldenContextDigest = "sha256:e7a4c1f9d6b3e0a8c5f2d9b6e3a0c7f4d1b8e5a2c9f6d3b0e7a4c1f8d5b2e9a6"
)

// gateIsolationGoldenSelections are the six fixed selections the SHARED golden
// testdata/wire/gate_isolation_evidence.json pins: one per class (#2135), plus
// a declared pulled image, a declared in-repo build and a declared image that
// ran on the host fallback (E51.3 / #2136). Each is produced by the real
// gateiso.Select (and, for the warning, the real EvaluateImagePolicy), so the
// golden tracks their text; the runner fills ResolvedImage and
// DistinctImagesCount after resolution, exactly as recordedSelection does, and
// stamps Credentials on every container-path selection (E51.26 / #4046) — the
// three container members carry both postures, the three non-container ones
// none.
func gateIsolationGoldenSelections() map[string]gateiso.Selection {
	const image = "ghcr.io/kuhlman-labs/fishhawk-gate:main"
	const podSock = "/run/user/1000/podman/podman.sock"
	const dockSock = "/var/run/docker.sock"
	dockerSafe := gateiso.Runtime{Kind: gateiso.KindDocker, Safe: true, Version: "27.3.1",
		Reason:     "docker 27.3.1 over local unix socket " + dockSock,
		Endpoint:   gateiso.Endpoint{Raw: "unix://" + dockSock, Scheme: "unix", Path: dockSock, Local: true},
		SocketPath: dockSock}

	const tagged = "ghcr.io/acme/gate:1.4"
	taggedReq := gateiso.ImageRequest{Source: gateiso.ImageSourceStage, Image: tagged}
	declared := gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeAuto, Profile: gateiso.ProfileSelfHosted, Image: tagged,
		Runtime: dockerSafe, Sandbox: gateiso.SandboxProbe{Available: true},
		ImageSource:   gateiso.ImageSourceStage,
		PolicyWarning: gateiso.EvaluateImagePolicy(gateiso.ProfileSelfHosted, taggedReq, nil, true).Warning})
	declared.ResolvedImage = &gateiso.ResolvedImage{Ref: "ghcr.io/acme/gate@" + goldenImageDigest, Digest: goldenImageDigest, ImageID: goldenImageID}
	declared.Credentials = gateiso.CredentialsOperatorConfig

	built := gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeAuto, Profile: gateiso.ProfileLocal,
		Runtime: dockerSafe, Sandbox: gateiso.SandboxProbe{Reason: "unshare unavailable on darwin (ADR-063 gap)"},
		Build: true, ImageSource: gateiso.ImageSourceWorkflow})
	built.ResolvedImage = &gateiso.ResolvedImage{Ref: gateiso.BuildTag(goldenContextDigest), ImageID: goldenImageID,
		BuildDockerfile: "build/gate/Dockerfile", BuildContext: "build/gate", ContextDigest: goldenContextDigest}
	built.DistinctImagesCount = 2
	built.Credentials = gateiso.CredentialsAnonymous

	container := gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeContainer, Profile: gateiso.ProfileHosted, Image: image,
		Runtime: gateiso.Runtime{Kind: gateiso.KindPodman, Safe: true, Rootless: true, Version: "5.2.1",
			Reason:     "podman 5.2.1 over local unix socket " + podSock,
			Endpoint:   gateiso.Endpoint{Raw: "unix://" + podSock, Scheme: "unix", Path: podSock, Local: true},
			SocketPath: podSock},
		Sandbox: gateiso.SandboxProbe{Available: true}})
	container.Credentials = gateiso.CredentialsAnonymous

	unhonored := gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeAuto, Profile: gateiso.ProfileLocal,
		Image:   "ghcr.io/acme/gate@" + goldenImageDigest,
		Runtime: gateiso.Runtime{Kind: gateiso.KindNone, Reason: "no container runtime on PATH (docker or podman)"},
		Sandbox: gateiso.SandboxProbe{Reason: "unshare unavailable on darwin (ADR-063 gap)"}, ImageSource: gateiso.ImageSourceStage})

	return map[string]gateiso.Selection{
		"container_declared":          declared,
		"container_build":             built,
		"fallback_declared_unhonored": unhonored,
		"fallback": gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeAuto, Profile: gateiso.ProfileLocal,
			Runtime: gateiso.Runtime{Kind: gateiso.KindNone, Reason: "no container runtime on PATH (docker or podman)"},
			Sandbox: gateiso.SandboxProbe{Reason: "unshare unavailable on darwin (ADR-063 gap)"}}),
		"refused": gateiso.Select(gateiso.Inputs{Mode: gateiso.ModeAuto, Profile: gateiso.ProfileHosted, Image: image,
			Runtime: gateiso.Runtime{Kind: gateiso.KindDocker, Reason: "docker endpoint from DOCKER_HOST is not a local unix socket: scheme tcp",
				Endpoint: gateiso.Endpoint{Raw: "tcp://10.0.0.5:2376", Scheme: "tcp"}},
			Sandbox: gateiso.SandboxProbe{Available: true}}),
		"container": container,
	}
}

// gateIsolationGolden reads the shared golden's per-class member bytes.
func gateIsolationGolden(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "gate_isolation_evidence.json"))
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode shared golden: %v", err)
	}
	return m
}

// gateIsolationGoldenClasses maps every shared-golden member to its class.
var gateIsolationGoldenClasses = map[string]string{
	"fallback":                    "fallback",
	"refused":                     "refused",
	"container":                   "container",
	"container_declared":          "container",
	"container_build":             "container",
	"fallback_declared_unhonored": "fallback",
}

// TestGateIsolationEvidence_MatchesSharedWireGolden pins the runner half of the
// cross-module contract on FIXED BYTES for all six members: the flattened
// member marshals byte-equal to the shared golden, and the composed
// gate_evidence payload carries it verbatim under gate_isolation. The backend's
// bundle_test decodes the same file.
func TestGateIsolationEvidence_MatchesSharedWireGolden(t *testing.T) {
	golden := gateIsolationGolden(t)
	sels := gateIsolationGoldenSelections()
	if len(golden) != len(gateIsolationGoldenClasses) || len(sels) != len(gateIsolationGoldenClasses) {
		t.Fatalf("golden has %d members, selections %d, want %d each", len(golden), len(sels), len(gateIsolationGoldenClasses))
	}
	for member, class := range gateIsolationGoldenClasses {
		t.Run(member, func(t *testing.T) {
			want, ok := golden[member]
			if !ok {
				t.Fatalf("shared golden has no %q variant", member)
			}
			iso := newGateIsolationEvidence(sels[member])
			got, err := json.Marshal(iso)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("member bytes drifted from the shared golden:\n got %s\nwant %s", got, want)
			}
			if iso.Class != class {
				t.Errorf("class = %q, want %q", iso.Class, class)
			}
			ev := composeGateEvidence(nil, 0, iso)
			if ev == nil {
				t.Fatal("composeGateEvidence returned nil for an isolation record")
			}
			if !bytes.Contains(ev.Payload, append([]byte(`"gate_isolation":`), want...)) {
				t.Errorf("gate_evidence payload does not carry the golden member verbatim:\n%s", ev.Payload)
			}
		})
	}
	if bytes.Contains(golden["refused"], []byte("10.0.0.5")) {
		t.Errorf("the runtime endpoint must not be carried: %s", golden["refused"])
	}
}

// TestGateIsolationEvidence_CarriesCredentialPosture (E51.26 / #4046) drives
// the real exec seam end to end: the posture the runner stamps on its recorded
// container-path selection reaches the composed gate_evidence payload as
// gate_isolation.credentials (anonymous by default, operator_config under
// FISHHAWK_GATE_DOCKER_CONFIG), and a refused or fallback selection carries no
// credentials key at all.
func TestGateIsolationEvidence_CarriesCredentialPosture(t *testing.T) {
	opCfg := operatorDockerConfig(t, `{"auths":{}}`)
	rows := []struct {
		name string
		st   func(io.Writer) *gateIsolationState
		want string
	}{
		{"container anonymous", func(w io.Writer) *gateIsolationState { return containerState("img:1", declSock, w) }, "anonymous"},
		{"container operator config", func(w io.Writer) *gateIsolationState {
			return containerStateEnv(t, "img:1", declSock, w, map[string]string{gateDockerConfigEnvVar: opCfg})
		}, "operator_config"},
		{"refused", refusedState, ""},
		{"fallback", func(io.Writer) *gateIsolationState { return fallbackState() }, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			st := r.st(io.Discard)
			installGateState(t, st)
			captureHostExec(t, false, 0)
			scriptAuxExec(t, nil)
			stubSeed(t, func(string) error { return nil })
			_, _, _ = runBoundedGateCommandDisposed(context.Background(), "true", t.TempDir(), filepath.Join(t.TempDir(), "lc"), time.Minute)
			iso := gateIsolationEvidenceFor(st)
			if iso == nil {
				t.Fatal("no gate_isolation evidence after a gate reached the seam")
			}
			if iso.Credentials != r.want {
				t.Fatalf("evidence credentials = %q, want %q", iso.Credentials, r.want)
			}
			ev := composeGateEvidence(nil, 0, iso)
			if ev == nil {
				t.Fatal("composeGateEvidence returned nil for an isolation record")
			}
			var p struct {
				GateIsolation map[string]json.RawMessage `json:"gate_isolation"`
			}
			if err := json.Unmarshal(ev.Payload, &p); err != nil {
				t.Fatalf("decode payload: %v", err)
			}
			got, present := p.GateIsolation["credentials"]
			switch {
			case r.want == "" && present:
				t.Errorf("a %s selection carries credentials %s; want the key absent", r.name, got)
			case r.want != "" && string(got) != `"`+r.want+`"`:
				t.Errorf("payload gate_isolation.credentials = %s, want %q", got, r.want)
			}
		})
	}
}

// TestGateIsolationEvidence_AbsentWhenNoGateRan: with no recorded selection the
// payload is BYTE-IDENTICAL to the pre-#2135 composition, and a stage with no
// gate events and no record composes nothing.
func TestGateIsolationEvidence_AbsentWhenNoGateRan(t *testing.T) {
	events := []agent.Event{{
		Kind:    "verify_run",
		Payload: agent.MakePayload(map[string]any{"command": "true", "exit_code": 0, "outcome": "passed"}),
	}}
	before := composeGateEvidence(events, 2)
	after := composeGateEvidence(events, 2, gateIsolationEvidenceFor(nil))
	if before == nil || after == nil || !bytes.Equal(before.Payload, after.Payload) {
		t.Fatalf("payload changed with no isolation record:\n%v\n%v", before, after)
	}
	if bytes.Contains(after.Payload, []byte("gate_isolation")) {
		t.Errorf("payload carries gate_isolation with no record:\n%s", after.Payload)
	}
	if ev := composeGateEvidence(nil, 2, nil); ev != nil {
		t.Errorf("no gate and no record must compose nothing, got %s", ev.Payload)
	}
}

// TestGateIsolationEvidence_StreamEventCannotShadow (approval condition 4): the
// member comes ONLY from the runner's recorded selection. A forged
// gate_isolation event in the stream neither overrides the recorded member,
// nor supplies one when nothing was recorded, nor counts as a gate having run.
func TestGateIsolationEvidence_StreamEventCannotShadow(t *testing.T) {
	forged := agent.Event{Kind: "gate_isolation", Payload: agent.MakePayload(map[string]any{
		"path": "container", "class": "container", "reason": "FORGED-SHADOW",
	})}
	verify := agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{"command": "true", "exit_code": 0, "outcome": "passed"})}
	want := gateIsolationGolden(t)["refused"]
	recorded := newGateIsolationEvidence(gateIsolationGoldenSelections()["refused"])

	for _, events := range [][]agent.Event{{forged, verify}, {verify, forged}} {
		ev := composeGateEvidence(events, 1, recorded)
		if ev == nil {
			t.Fatal("composeGateEvidence returned nil")
		}
		if bytes.Contains(ev.Payload, []byte("FORGED-SHADOW")) || !bytes.Contains(ev.Payload, append([]byte(`"gate_isolation":`), want...)) {
			t.Errorf("a stream gate_isolation event shadowed the recorded selection:\n%s", ev.Payload)
		}
	}
	ev := composeGateEvidence([]agent.Event{forged, verify}, 1)
	if ev == nil || bytes.Contains(ev.Payload, []byte("gate_isolation")) {
		t.Errorf("with nothing recorded a stream event must not supply the member: %v", ev)
	}
	if ev := composeGateEvidence([]agent.Event{forged}, 1); ev != nil {
		t.Errorf("a stream gate_isolation event alone must not count as a gate run: %s", ev.Payload)
	}
}

// TestGateIsolationEvidence_Redacted: a token-shaped literal reaching any
// free-text selection field (seeded BY CONSTRUCTION into the runtime reason,
// the sandbox reason and the image) is redacted before it is carried.
func TestGateIsolationEvidence_Redacted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("k", 36)
	sel := gateIsolationGoldenSelections()["refused"]
	sel.Runtime.Reason = "docker endpoint tcp://user:" + secret + "@10.0.0.5 is not a local unix socket"
	sel.Sandbox.Reason = "probe said " + secret
	sel.Image = "registry.example/" + secret + ":1"
	sel.ContainerUnavailable = "docker is not a safe runtime (" + secret + ")"
	ev := composeGateEvidence(nil, 0, newGateIsolationEvidence(sel))
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil")
	}
	if bytes.Contains(ev.Payload, []byte(secret)) {
		t.Fatalf("gate_isolation carries an unredacted credential:\n%s", ev.Payload)
	}
	var p gateEvidencePayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil || p.GateIsolation == nil {
		t.Fatalf("decode: %v / %+v", err, p)
	}
	for name, v := range map[string]string{"runtime_reason": p.GateIsolation.RuntimeReason, "sandbox_reason": p.GateIsolation.SandboxReason, "image": p.GateIsolation.Image, "container_unavailable": p.GateIsolation.ContainerUnavailable} {
		if !strings.Contains(v, "REDACTED") {
			t.Errorf("%s = %q, want a redaction marker", name, v)
		}
	}
}

// TestGateIsolationEvidence_IsolationOnlyCarriesNoGateVerdict (approval
// condition 5, runner half): a refusal where no other gate event exists
// composes a payload carrying the isolation member and NO verify run, verify
// summary or policy fact — nothing a consumer could read as "gates ran and
// passed". The backend half decodes the same shape in bundle_test.
func TestGateIsolationEvidence_IsolationOnlyCarriesNoGateVerdict(t *testing.T) {
	ev := composeGateEvidence(nil, 3, newGateIsolationEvidence(gateIsolationGoldenSelections()["refused"]))
	if ev == nil {
		t.Fatal("an isolation record alone must still compose the evidence")
	}
	var p gateEvidencePayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p.GateIsolation == nil || p.GateIsolation.Class != "refused" {
		t.Fatalf("gate_isolation = %+v, want class refused", p.GateIsolation)
	}
	if len(p.VerifyRuns) != 0 || p.VerifySummary != nil || len(p.PolicyViolations) != 0 || p.DiffCoverage != nil {
		t.Errorf("an isolation-only payload must carry no gate verdict: %s", ev.Payload)
	}
}

// TestGateIsolationEvidence_FinalDecidingGateImage (approval condition 4,
// evidence half): when a stage's gates resolved two different images, the
// flattened evidence carries the FINAL DECIDING verify gate's image — not the
// later non-deciding gate's — plus distinct_images_count. A deciding gate
// whose resolution FAILED leaves no image fields, never an earlier gate's.
func TestGateIsolationEvidence_FinalDecidingGateImage(t *testing.T) {
	const digestB = "sha256:" + "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"
	st := &gateIsolationState{sel: gateIsolationGoldenSelections()["container_declared"]}
	st.sel.ResolvedImage, st.sel.DistinctImagesCount = nil, 0
	st.seamReached.Store(true)

	st.noteDeclaredGate(true)
	st.recordResolved(gateiso.ResolvedImage{Ref: "ghcr.io/acme/gate@" + goldenImageDigest, Digest: goldenImageDigest, ImageID: goldenImageID}, true)
	st.recordResolved(gateiso.ResolvedImage{Ref: "ghcr.io/acme/gate@" + digestB, Digest: digestB, ImageID: "sha256:" + strings.Repeat("2", 64)}, false)
	ev := gateIsolationEvidenceFor(st)
	if ev == nil || ev.ImageDigest != goldenImageDigest || ev.ImageID != goldenImageID || ev.DistinctImagesCount != 2 {
		t.Fatalf("evidence = %+v, want the deciding gate's digest/id and distinct_images_count 2", ev)
	}

	st.noteDeclaredGate(true) // a later deciding gate whose resolution fails
	ev = gateIsolationEvidenceFor(st)
	if ev == nil || ev.ImageDigest != "" || ev.ImageID != "" || ev.BuildContextDigest != "" || ev.DistinctImagesCount != 2 {
		t.Errorf("evidence = %+v, want no image fields after a failed deciding resolution (count kept)", ev)
	}
}

// TestGateIsolationEvidence_DeclaredFieldsRedacted: a token-shaped literal
// reaching any new free-text field (seeded BY CONSTRUCTION) is redacted before
// it is carried.
func TestGateIsolationEvidence_DeclaredFieldsRedacted(t *testing.T) {
	secret := "ghp_" + strings.Repeat("q", 36)
	sel := gateIsolationGoldenSelections()["container_build"]
	sel.PolicyWarning = "warning " + secret
	sel.DeclaredUnhonored = "unhonored " + secret
	sel.ResolvedImage.BuildDockerfile = "ci/" + secret + "/Dockerfile"
	sel.ResolvedImage.BuildContext = "ci/" + secret
	ev := newGateIsolationEvidence(sel)
	b, _ := json.Marshal(ev)
	if bytes.Contains(b, []byte(secret)) {
		t.Fatalf("gate_isolation carries an unredacted credential:\n%s", b)
	}
	for name, v := range map[string]string{"policy_warning": ev.PolicyWarning, "declared_unhonored": ev.DeclaredUnhonored, "build_dockerfile": ev.BuildDockerfile, "build_context": ev.BuildContext} {
		if !strings.Contains(v, "REDACTED") {
			t.Errorf("%s = %q, want a redaction marker", name, v)
		}
	}
}

// gateContainerPromptGolden decodes one member of the SHARED prompt-wire golden
// testdata/wire/gate_container_prompt.json (E51.3 / #2136) — the bytes the
// backend's prompt handler emits as gate_container.
func gateContainerPromptGolden(t *testing.T, member string) *upload.GateContainerConfig {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "gate_container_prompt.json"))
	if err != nil {
		t.Fatalf("read shared prompt golden: %v", err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("decode shared prompt golden: %v", err)
	}
	var gc upload.GateContainerConfig
	if err := json.Unmarshal(m[member], &gc); err != nil || gc.Source == "" {
		t.Fatalf("decode member %q: %v / %+v", member, err, gc)
	}
	return &gc
}

// TestRun_DeclaredGateContainerReachesGateEvidence is the cross-boundary
// end-to-end pin (E51.3 / #2136): the backend's prompt golden (stage_image)
// is served by the fake prompt fetch, run() declares it, the committed-tree
// verify gate's selection records it, and the RAW bundle shipped to the
// backend carries image_source=stage and the declared_unhonored marker
// (FISHHAWK_GATE_ISOLATION=clone, so the declared image is not honoured).
// Not t.Parallel: it uses t.Setenv.
func TestRun_DeclaredGateContainerReachesGateEvidence(t *testing.T) {
	for _, k := range []string{gateImageEnvVar, deploymentProfileEnvVar, gateServicesEnvVar, gatePostgresImageEnvVar, gateImageAllowlistEnvVar, gateBuildEnvVar} {
		t.Setenv(k, "")
	}
	t.Setenv(gateIsolationModeEnvVar, "clone")
	repo := verifyFixBaseRepo(t)
	mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
	withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}})
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{
		StageID:       verifyFixStageID,
		StageType:     "implement",
		Prompt:        "implement",
		PromptHash:    "h",
		VerifyCommand: "true",
		ScopeFiles:    []upload.ScopeFile{{Path: "mod/reg.go", Operation: "modify"}},
		GateContainer: gateContainerPromptGolden(t, "stage_image"),
	}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
		t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
	}
	var rawBundle []byte
	for _, c := range fu.gotShipCalls {
		if c.Variant == "raw" {
			rawBundle = c.Bundle
		}
	}
	if rawBundle == nil {
		t.Fatalf("no raw bundle shipped (calls=%d):\n%s", len(fu.gotShipCalls), stderr.String())
	}
	p, raw := gateEvidencePayloadFromBundle(t, rawBundle)
	gi := p.GateIsolation
	if gi == nil {
		t.Fatalf("raw bundle gate_evidence carries no gate_isolation member:\n%s", raw)
	}
	if gi.Path != "clone" || gi.ImageSource != gateiso.ImageSourceStage ||
		!strings.Contains(gi.Image, "ghcr.io/kuhlman-labs/fishhawk-gate@sha256:") ||
		!strings.Contains(gi.DeclaredUnhonored, "gate_container declared (stage) but not honoured") {
		t.Errorf("gate_isolation = %+v, want path clone, image_source stage, the declared image and a declared_unhonored marker", gi)
	}
}

// TestRun_PlanCounterfactualReachesGateEvidence (fix-up B, concern bfd4c997)
// drives the REAL run() for an implement pass and a fix-up pass: one delivered
// spec (mutate mod/reg.go's seeding so TestGet fails), a passing committed-tree
// verify gate (VerifyCommand "true"), and the RAW bundle shipped to the backend
// must carry gate_evidence.counterfactual_observations[0] observed RED — not
// error/no_verified_tree — at the committed-tree gate's verified head. The
// phase runs BEFORE the push (its observations ride the signed bundle), so the
// pushed commit's SHA does not exist yet; what binds the observation to the
// push is that its head IS the gate's verify_run head and its tree IS the tree
// the push commits (the scope files over HEAD, captured at CommitAndPush).
// COUNTERFACTUALS: delete the runPlanCounterfactuals call in run() → no
// member → RED; pass "" for verifiedTreeSHA → error/no_verified_tree → RED.
// Not t.Parallel: it uses t.Setenv.
func TestRun_PlanCounterfactualReachesGateEvidence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fixup bool
	}{{"implement", false}, {"fixup", true}} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{gateImageEnvVar, deploymentProfileEnvVar, gateServicesEnvVar, gatePostgresImageEnvVar, gateImageAllowlistEnvVar, gateBuildEnvVar} {
				t.Setenv(k, "")
			}
			t.Setenv(gateIsolationModeEnvVar, "clone")
			repo := verifyFixBaseRepo(t)
			mustWrite(t, filepath.Join(repo, "mod", "reg.go"), regGetFixed)
			mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
			withFakeInvoker(t, &fakeInvoker{mirrorWorkingTreeFrom: repo, canned: agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}}})
			implementEnv(t, "kuhlman-labs/fishhawk", "main")
			fu := newFakeUploader(t)
			fu.promptResp = &upload.FetchedPrompt{
				StageID:       verifyFixStageID,
				StageType:     "implement",
				Prompt:        "implement",
				PromptHash:    "h",
				VerifyCommand: "true",
				ScopeFiles:    []upload.ScopeFile{{Path: "mod/reg.go", Operation: "create"}, {Path: "mod/reg_test.go", Operation: "create"}},
				Counterfactuals: []upload.CounterfactualSpec{{
					Index: 0, ControlPath: "mod/reg.go", MutationKind: upload.CounterfactualMutationReplace,
					Find: `registry["x"] = 42`, Replace: `registry["x"] = 41`,
					TestRun: "^TestGet$", TestPackage: "./mod", Expect: "red",
				}},
			}
			const fixupBranch = "fishhawk/run-11111111/stage-22222222"
			if tc.fixup {
				fu.promptResp.Fixup = true
				fu.promptResp.FixupBranch = fixupBranch
			}
			withFakeUploader(t, fu)
			var pushedTree string
			fp := &fakePusher{onCommit: func(args gitops.CommitAndPushArgs) {
				pushedTree = scopeTreeOverHead(t, args.RepoDir, "mod/reg.go", "mod/reg_test.go")
			}}
			withFakeGitOps(t, fp, &fakePROpener{})

			bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
			var stderr strings.Builder
			if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
				t.Fatalf("run = %d, want exitOK:\n%s", got, stderr.String())
			}
			if fp.gotArgs == nil || pushedTree == "" {
				t.Fatalf("CommitAndPush was not reached:\n%s", stderr.String())
			}
			if tc.fixup && fp.gotArgs.Branch != fixupBranch {
				t.Fatalf("pushed branch = %q, want the fix-up branch %q (the fix-up path was not taken)", fp.gotArgs.Branch, fixupBranch)
			}
			var rawBundle []byte
			for _, c := range fu.gotShipCalls {
				if c.Variant == "raw" {
					rawBundle = c.Bundle
				}
			}
			if rawBundle == nil {
				t.Fatalf("no raw bundle shipped:\n%s", stderr.String())
			}
			p, raw := gateEvidencePayloadFromBundle(t, rawBundle)
			if len(p.CounterfactualObservations) != 1 {
				t.Fatalf("counterfactual_observations = %d, want 1:\n%s", len(p.CounterfactualObservations), raw)
			}
			o := p.CounterfactualObservations[0]
			if o.Observed != cfOutcomeRed || o.Reason != "" || o.Baseline != cfOutcomeGreen {
				t.Fatalf("observation = %s/%s (baseline %s), want red with a green baseline", o.Observed, o.Reason, o.Baseline)
			}
			_, events, _, err := openBundleForTest(rawBundle)
			if err != nil {
				t.Fatalf("open bundle: %v", err)
			}
			if gateHead := lastBundleVerifyRunHead(t, events); o.HeadSHA == "" || o.HeadSHA != gateHead {
				t.Fatalf("observation head = %q, want the committed-tree gate's verify_run head %q", o.HeadSHA, gateHead)
			}
			if o.TreeSHA != pushedTree {
				t.Fatalf("observation tree = %q, want the pushed tree %q", o.TreeSHA, pushedTree)
			}
			if got := strings.TrimSpace(gitIn(t, repo, nil, "rev-parse", o.HeadSHA+"^{tree}")); got != pushedTree {
				t.Fatalf("observation head %s carries tree %s, want the pushed tree %s", o.HeadSHA, got, pushedTree)
			}
		})
	}
}

// scopeTreeOverHead is the tree a scope-only commit of paths over HEAD would
// carry — what CommitAndPush pushes — computed in a throwaway index so the
// checkout under test is not touched.
func scopeTreeOverHead(t *testing.T, repo string, paths ...string) string {
	t.Helper()
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(t.TempDir(), "index")}
	gitIn(t, repo, env, "read-tree", "HEAD")
	gitIn(t, repo, env, append([]string{"add", "--"}, paths...)...)
	return strings.TrimSpace(gitIn(t, repo, env, "write-tree"))
}

func gitIn(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v", args, err)
	}
	return string(out)
}

// counterfactualGoldenObservations are the four fixed observations the SHARED
// golden testdata/wire/counterfactual_observation.json pins (E85.3 / #4143):
// one green, one red, one timeout and one error/build_failed, all off the same
// verified head. The backend's bundle_test decodes the same file.
func counterfactualGoldenObservations() []counterfactualObservationEvidence {
	const head = "3f9a1c7e5b2d8046a1f3c9e7b5d2a8f6c4e1b9d7"
	const tree = "a7c4e1f9b6d3a0c8e5f2b9d6a3c0e7f4b1d8a5c2"
	return []counterfactualObservationEvidence{
		{Index: 0, ControlPath: "backend/internal/plan/validate.go", TestRun: "^TestValidate_Counterfactuals$", TestPackage: "./backend/internal/plan",
			HeadSHA: head, TreeSHA: tree, Baseline: "green", Observed: "green", DurationMS: 4210, TimeoutSeconds: 300,
			MutationSHA256: "5d41402abc4b2a76b9719d911017c592ae2b6c8e1f4a7d0c3b6e9f2a5d8c1b4e"},
		{Index: 1, ControlPath: "backend/internal/server/prompt.go", TestRun: "^TestPromptResponse_CarriesCounterfactuals$", TestPackage: "./backend/internal/server",
			HeadSHA: head, TreeSHA: tree, Baseline: "green", Observed: "red", DurationMS: 18733, TimeoutSeconds: 300,
			MutationSHA256: "9e107d9d372bb6826bd81d3542a419d6c4e7f1a0b3d6c9e2f5a8b1d4e7c0f3a6"},
		{Index: 2, ControlPath: "runner/internal/upload/upload.go", TestRun: "TestFetchPrompt", TestPackage: "./runner/internal/upload/...",
			HeadSHA: head, TreeSHA: tree, Baseline: "green", Observed: "timeout", DurationMS: 302114, TimeoutSeconds: 300,
			MutationSHA256: "e4d909c290d0fb1ca068ffaddf22cbd0b7c2e5f8a1d4c7b0e3f6a9c2d5b8e1f4"},
		{Index: 3, ControlPath: "backend/internal/plan/plan.go", TestRun: "^TestParse$", TestPackage: "./backend/internal/plan",
			HeadSHA: head, TreeSHA: tree, Baseline: "green", Observed: "error", Reason: "build_failed", DurationMS: 2954, TimeoutSeconds: 300,
			MutationSHA256: "c3fcd3d76192e4007dfb496cca67e13b7d0a3c6f9e2b5d8a1c4f7e0b3d6a9c2f"},
	}
}

// counterfactualObservedEvents builds one counterfactual_observed event per
// observation through the production constructor.
func counterfactualObservedEvents(obs []counterfactualObservationEvidence) []agent.Event {
	cfg := config{runID: "run-cf", stageID: "stage-cf"}
	out := make([]agent.Event, 0, len(obs))
	for _, o := range obs {
		out = append(out, counterfactualObservedEvent(cfg, o))
	}
	return out
}

// TestCounterfactualObservations_MatchSharedWireGolden pins the runner half of
// the counterfactual_observation_evidence contract on FIXED BYTES: four
// runner observations compose, in order, into a gate_evidence
// counterfactual_observations member byte-equal to the shared golden.
func TestCounterfactualObservations_MatchSharedWireGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "counterfactual_observation.json"))
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	want := bytes.TrimSuffix(raw, []byte("\n"))

	verify := agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{
		"command": "scripts/test verify", "exit_code": 0, "outcome": "passed",
	})}
	ev := composeGateEvidence([]agent.Event{verify}, 1, runnerCounterfactualObservations(counterfactualGoldenObservations()))
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil")
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &all); err != nil {
		t.Fatalf("decode gate_evidence: %v", err)
	}
	got, err := json.Marshal(map[string]json.RawMessage{"counterfactual_observations": all["counterfactual_observations"]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("counterfactual_observations drifted from the shared golden:\n got %s\nwant %s", got, want)
	}
}

// TestCounterfactualObservedEvent_CarriesRunStageAndObservation pins the event
// constructor's shape: the counterfactual_observed kind, the run and stage ids,
// and the observation verbatim under one `observation` key.
func TestCounterfactualObservedEvent_CarriesRunStageAndObservation(t *testing.T) {
	obs := counterfactualGoldenObservations()[1]
	ev := counterfactualObservedEvent(config{runID: "run-cf", stageID: "stage-cf"}, obs)
	if ev.Kind != "counterfactual_observed" {
		t.Fatalf("kind = %q, want counterfactual_observed", ev.Kind)
	}
	var w struct {
		RunID       string                            `json:"run_id"`
		StageID     string                            `json:"stage_id"`
		Observation counterfactualObservationEvidence `json:"observation"`
	}
	if err := json.Unmarshal(ev.Payload, &w); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if w.RunID != "run-cf" || w.StageID != "stage-cf" || w.Observation != obs {
		t.Fatalf("payload = %+v, want run-cf/stage-cf/%+v", w, obs)
	}
}

// TestComposeGateEvidence_CounterfactualObservationsCapped: 20 observations
// compose to the first gateEvidenceCounterfactualCap (8), in spec order, so
// an over-long slice cannot grow the signed bundle's evidence without bound.
func TestComposeGateEvidence_CounterfactualObservationsCapped(t *testing.T) {
	obs := make([]counterfactualObservationEvidence, 20)
	for i := range obs {
		obs[i] = counterfactualObservationEvidence{Index: i, ControlPath: "a/b.go", TestRun: "TestX", TestPackage: "./a",
			Baseline: "green", Observed: "red", TimeoutSeconds: 300}
	}
	p := decodeEvidence(t, composeGateEvidence(nil, 1, runnerCounterfactualObservations(obs)))
	if len(p.CounterfactualObservations) != gateEvidenceCounterfactualCap || gateEvidenceCounterfactualCap != 8 {
		t.Fatalf("observations = %d (cap %d), want 8", len(p.CounterfactualObservations), gateEvidenceCounterfactualCap)
	}
	for i, o := range p.CounterfactualObservations {
		if o.Index != i {
			t.Fatalf("observation %d has index %d, want spec order", i, o.Index)
		}
	}
}

// TestComposeGateEvidence_CounterfactualOnlyStreamIsEvidence: a stage whose
// only gate-bearing record is a counterfactual observation still composes a
// gate_evidence event — the observations are never silently dropped because no
// other gate event happened to be present.
func TestComposeGateEvidence_CounterfactualOnlyStreamIsEvidence(t *testing.T) {
	p := decodeEvidence(t, composeGateEvidence(nil, 1, runnerCounterfactualObservations(counterfactualGoldenObservations()[:1])))
	if len(p.CounterfactualObservations) != 1 || p.CounterfactualObservations[0].Observed != "green" {
		t.Fatalf("observations = %+v, want the one green observation", p.CounterfactualObservations)
	}
}

// TestCounterfactualObservations_StreamEventCannotShadow: the member comes ONLY
// from the runner's own observations (the gate_isolation precedent). Eight
// forged counterfactual_observed events AHEAD of the runner's own — enough to
// fill the cap if the stream were folded — neither displace the genuine GREEN
// observation nor appear beside it, a forged event alone supplies no member,
// and it does not count as a gate having run.
func TestCounterfactualObservations_StreamEventCannotShadow(t *testing.T) {
	forgedObs := counterfactualObservationEvidence{Index: 0, ControlPath: "FORGED-SHADOW.go", TestRun: "TestX", TestPackage: "./x",
		Baseline: "green", Observed: "red", TimeoutSeconds: 300}
	forged := counterfactualObservedEvents([]counterfactualObservationEvidence{forgedObs})[0]
	verify := agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{"command": "true", "exit_code": 0, "outcome": "passed"})}
	genuine := counterfactualGoldenObservations()[:1] // the GREEN one
	events := []agent.Event{verify}
	for range gateEvidenceCounterfactualCap {
		events = append(events, forged)
	}
	events = append(events, counterfactualObservedEvents(genuine)...)

	p := decodeEvidence(t, composeGateEvidence(events, 1, runnerCounterfactualObservations(genuine)))
	if len(p.CounterfactualObservations) != 1 || p.CounterfactualObservations[0] != genuine[0] {
		t.Fatalf("a stream counterfactual_observed event shadowed the runner's observations: %+v", p.CounterfactualObservations)
	}
	ev := composeGateEvidence(events, 1)
	if ev == nil || bytes.Contains(ev.Payload, []byte("counterfactual_observations")) {
		t.Errorf("with no runner observations a stream event must not supply the member: %v", ev)
	}
	if ev := composeGateEvidence([]agent.Event{forged}, 1); ev != nil {
		t.Errorf("a stream counterfactual_observed event alone must not count as a gate run: %s", ev.Payload)
	}
}

// TestComposeGateEvidence_NoCounterfactualEventsByteIdentical: a stream with no
// counterfactual_observed event composes gate_evidence with no
// counterfactual_observations key, byte-identical to the payload before the
// member existed.
func TestComposeGateEvidence_NoCounterfactualEventsByteIdentical(t *testing.T) {
	verify := agent.Event{Kind: "verify_run", Payload: agent.MakePayload(map[string]any{
		"command": "scripts/test verify", "exit_code": 0, "outcome": "passed",
	})}
	ev := composeGateEvidence([]agent.Event{verify}, 2)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil")
	}
	const want = `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed"}],"scope_facts":{"declared_files":2}}`
	if string(ev.Payload) != want {
		t.Fatalf("payload = %s\nwant %s", ev.Payload, want)
	}
}

// flakeAbsorbedGolden is the evidence the shared golden
// testdata/wire/verify_flake_absorbed_evidence.json pins (#4229). The
// backend's bundle_test decodes the same file.
func flakeAbsorbedGolden() flakeAbsorbedEvidence {
	return flakeAbsorbedEvidence{
		Iteration: 2,
		TreeSHA:   "c1e8a4f7b2d9e6a3c0f5b8d1e4a7c2f9b6d3e0a5",
		Packages: []string{
			"github.com/kuhlman-labs/fishhawk/backend/internal/run",
			"github.com/kuhlman-labs/fishhawk/runner/internal/upload",
		},
		PackageDirs:   []string{"backend/internal/run", "runner/internal/upload"},
		Tests:         []string{"TestDispatchLiveness_HeartbeatWindow", "TestShipTrace_TerminalEgressBudget"},
		FailedModules: []string{"backend", "runner"},
		PatchCoverage: "pass",
	}
}

// TestFlakeAbsorbedEvidence_MarshalsSharedGolden pins the runner half of the
// verify_flake_absorbed_evidence contract on FIXED BYTES: an event built by the
// production constructor folds, through composeGateEvidence, into a
// gate_evidence flake_absorbed member byte-equal to the shared golden.
func TestFlakeAbsorbedEvidence_MarshalsSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "verify_flake_absorbed_evidence.json"))
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	want := bytes.TrimSuffix(raw, []byte("\n"))

	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "", "", 0, "ok", "passed"),
		flakeAbsorbedEvent(flakeAbsorbedGolden()),
	}
	ev := composeGateEvidence(events, 1)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil")
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(ev.Payload, &all); err != nil {
		t.Fatalf("decode gate_evidence: %v", err)
	}
	got, err := json.Marshal(map[string]json.RawMessage{"flake_absorbed": all["flake_absorbed"]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("flake_absorbed drifted from the shared golden:\n got %s\nwant %s", got, want)
	}
}

// TestFlakeAbsorbedEvent_PayloadIsEvidence pins the constructor: the
// verify_flake_absorbed kind, and a payload that decodes back to the evidence
// struct it was built from.
func TestFlakeAbsorbedEvent_PayloadIsEvidence(t *testing.T) {
	ev := flakeAbsorbedEvent(flakeAbsorbedGolden())
	if ev.Kind != "verify_flake_absorbed" {
		t.Fatalf("kind = %q, want verify_flake_absorbed", ev.Kind)
	}
	var got flakeAbsorbedEvidence
	if err := json.Unmarshal(ev.Payload, &got); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if fmt.Sprintf("%+v", got) != fmt.Sprintf("%+v", flakeAbsorbedGolden()) {
		t.Fatalf("payload = %+v, want %+v", got, flakeAbsorbedGolden())
	}
}

// TestComposeGateEvidence_FoldsFlakeAbsorbed: the LAST verify_flake_absorbed
// event folds into gate_evidence.flake_absorbed, and every string entry is
// redacted — a token-shaped test name never reaches the signed bundle.
func TestComposeGateEvidence_FoldsFlakeAbsorbed(t *testing.T) {
	secret := "ghp_" + strings.Repeat("q", 36)
	earlier := flakeAbsorbedGolden()
	earlier.Iteration = 1
	earlier.Tests = []string{"TestEarlier"}
	last := flakeAbsorbedGolden()
	last.Tests = []string{"TestLeaks_" + secret, "TestClean"}
	last.PackageDirs = []string{"pkg/" + secret}
	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "", "", 1, "FAIL", "failed"),
		flakeAbsorbedEvent(earlier),
		verifyRunEvent("scripts/test verify", "", "", 0, "ok", "passed"),
		flakeAbsorbedEvent(last),
	}
	p := decodeEvidence(t, composeGateEvidence(events, 1))
	fa := p.FlakeAbsorbed
	if fa == nil {
		t.Fatal("flake_absorbed missing")
	}
	if fa.Iteration != 2 || fa.TreeSHA != last.TreeSHA || fa.PatchCoverage != "pass" ||
		len(fa.Packages) != 2 || len(fa.FailedModules) != 2 {
		t.Errorf("flake_absorbed = %+v, want the LAST event's fields", fa)
	}
	if len(fa.Tests) != 2 || fa.Tests[1] != "TestClean" {
		t.Fatalf("tests = %q, want the last event's two names", fa.Tests)
	}
	for _, s := range append(append([]string(nil), fa.Tests...), fa.PackageDirs...) {
		if strings.Contains(s, secret) {
			t.Errorf("unredacted token in flake_absorbed entry %q", s)
		}
	}
	if !strings.Contains(fa.Tests[0], "[REDACTED:github-pat-classic]") ||
		!strings.Contains(fa.PackageDirs[0], "[REDACTED:github-pat-classic]") {
		t.Errorf("entries not redacted: tests=%q dirs=%q", fa.Tests, fa.PackageDirs)
	}
}

// TestComposeGateEvidence_FlakeAbsorbedAloneIsNotAGate: a verify_flake_absorbed
// event with no verify_run / verify_summary beside it produces no gate_evidence
// at all — it is a fold source, never a gate on its own.
func TestComposeGateEvidence_FlakeAbsorbedAloneIsNotAGate(t *testing.T) {
	if ev := composeGateEvidence([]agent.Event{flakeAbsorbedEvent(flakeAbsorbedGolden())}, 1); ev != nil {
		t.Fatalf("lone verify_flake_absorbed composed gate_evidence %s, want nil", ev.Payload)
	}
}

// TestComposeGateEvidence_NoFlakeAbsorbedField: without the event the payload
// carries no flake_absorbed key, and an undecodable one is skipped.
func TestComposeGateEvidence_NoFlakeAbsorbedField(t *testing.T) {
	events := []agent.Event{
		verifyRunEvent("scripts/test verify", "", "", 0, "ok", "passed"),
		{Kind: "verify_flake_absorbed", Payload: json.RawMessage(`{"iteration":"not-an-int"}`)},
	}
	ev := composeGateEvidence(events, 1)
	if ev == nil {
		t.Fatal("composeGateEvidence returned nil")
	}
	if strings.Contains(string(ev.Payload), "flake_absorbed") {
		t.Fatalf("payload carries flake_absorbed without a decodable event: %s", ev.Payload)
	}
}

// TestFixupCommitClaims_MarshalsSharedGolden pins the runner half of the #4337
// fixup_commit_claims contract on FIXED BYTES: the incident's real commit
// message, checked against its real changed files by the production
// cross-check, folds through composeGateEvidence into a gate_evidence member
// byte-equal to testdata/wire/fixup_commit_claims.json. The backend's
// bundle_test decodes the same file.
func TestFixupCommitClaims_MarshalsSharedGolden(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "fixup_commit_claims.json"))
	if err != nil {
		t.Fatalf("read shared golden: %v", err)
	}
	want := bytes.TrimSuffix(raw, []byte("\n"))

	claims, truncated := checkFixupCommitClaims(incident4337Body(t), incident4337Changed, "", false)
	ev := fixupCommitClaimsEvidence{Truncated: truncated}
	for _, c := range claims {
		ev.Claims = append(ev.Claims, fixupCommitClaimEvidence(c))
	}
	composed := composeGateEvidence([]agent.Event{{Kind: fixupCommitClaimsUnbackedEvent, Payload: agent.MakePayload(ev)}}, 1)
	if composed == nil {
		t.Fatal("composeGateEvidence returned nil: the claims event must count as a gate having run")
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(composed.Payload, &all); err != nil {
		t.Fatalf("decode gate_evidence: %v", err)
	}
	got, err := json.Marshal(map[string]json.RawMessage{"fixup_commit_claims_unbacked": all["fixup_commit_claims_unbacked"]})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("fixup_commit_claims_unbacked drifted from the shared golden:\n got %s\nwant %s", got, want)
	}
}

// TestComposeGateEvidence_NoFixupCommitClaims_ByteIdentical: without the event
// the member is absent and gate_evidence is byte-identical to the pre-#4337
// composition; the last of two events wins.
func TestComposeGateEvidence_NoFixupCommitClaims_ByteIdentical(t *testing.T) {
	base := []agent.Event{verifyRunEvent("scripts/test verify", "", "", 0, "ok", "passed")}
	without := composeGateEvidence(base, 1)
	if without == nil || strings.Contains(string(without.Payload), "fixup_commit_claims_unbacked") {
		t.Fatalf("member present without the event: %s", without.Payload)
	}
	if want := `{"verify_runs":[{"command":"scripts/test verify","exit_code":0,"outcome":"passed","output_tail":"ok"}],"scope_facts":{"declared_files":1}}`; string(without.Payload) != want {
		t.Errorf("gate_evidence = %s\nwant byte-identical %s", without.Payload, want)
	}
	first := fixupCommitClaimsEvidence{Claims: []fixupCommitClaimEvidence{{Marker: "Item 1", FileRefs: []string{"a.go"}}}}
	last := fixupCommitClaimsEvidence{Claims: []fixupCommitClaimEvidence{{Marker: "Item 2", TestNames: []string{"TestB"}}}, Truncated: true}
	with := composeGateEvidence(append(base,
		agent.Event{Kind: fixupCommitClaimsUnbackedEvent, Payload: agent.MakePayload(first)},
		agent.Event{Kind: fixupCommitClaimsUnbackedEvent, Payload: json.RawMessage(`{"claims":"undecodable"}`)},
		agent.Event{Kind: fixupCommitClaimsUnbackedEvent, Payload: agent.MakePayload(last)},
	), 1)
	if !strings.Contains(string(with.Payload), `"fixup_commit_claims_unbacked":{"claims":[{"marker":"Item 2","test_names":["TestB"]}],"truncated":true}`) {
		t.Errorf("last-write-wins member missing: %s", with.Payload)
	}
}
