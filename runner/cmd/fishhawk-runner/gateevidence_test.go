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
