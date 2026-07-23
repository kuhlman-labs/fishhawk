package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/constraint"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
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
