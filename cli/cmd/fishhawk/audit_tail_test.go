package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// stubAuditTailAPI is a programmable auditTailAPI that returns a
// pre-recorded slice of responses, one per poll, then short-circuits
// the runAuditTail loop by cancelling the context the test owns. Use
// `responses` for happy-path polls and `errs[i]` to inject a
// transport error on the i-th poll.
type stubAuditTailAPI struct {
	responses []httpclient.ListRunAuditResult
	errs      []error
	polled    atomic.Int32
}

func (s *stubAuditTailAPI) ListRunAudit(_ context.Context, _ uuid.UUID, _ httpclient.ListRunAuditFilter) (*httpclient.ListRunAuditResult, error) {
	idx := int(s.polled.Add(1)) - 1
	if idx < len(s.errs) && s.errs[idx] != nil {
		return nil, s.errs[idx]
	}
	if idx >= len(s.responses) {
		// Loop continued past what the test programmed; return an
		// empty page so the loop keeps spinning until ctx fires.
		return &httpclient.ListRunAuditResult{}, nil
	}
	out := s.responses[idx]
	return &out, nil
}

func tailEntry(seq int64, runID uuid.UUID, category string, payload map[string]any) httpclient.AuditEntry {
	body, _ := json.Marshal(payload)
	return httpclient.AuditEntry{
		ID: uuid.New(), Sequence: seq, RunID: runID,
		Timestamp: time.Date(2026, 5, 14, 12, 0, int(seq), 0, time.UTC),
		Category:  category,
		Payload:   body,
		EntryHash: "h",
	}
}

func TestAuditTail_PrintsCurrentPageThenNewEntries(t *testing.T) {
	runID := uuid.New()
	api := &stubAuditTailAPI{
		responses: []httpclient.ListRunAuditResult{
			// Poll 1: current page, two entries.
			{Items: []httpclient.AuditEntry{
				tailEntry(1, runID, "run_dispatched", map[string]any{"kind": "issue"}),
				tailEntry(2, runID, "plan_generated", map[string]any{"summary": "x"}),
			}},
			// Poll 2: one new entry beyond the high-water.
			{Items: []httpclient.AuditEntry{
				tailEntry(1, runID, "run_dispatched", map[string]any{"kind": "issue"}),
				tailEntry(2, runID, "plan_generated", map[string]any{"summary": "x"}),
				tailEntry(3, runID, "approval_submitted", map[string]any{"decision": "approve"}),
			}},
		},
	}

	var stdout strings.Builder
	rc := runAuditTail(context.Background(), api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "text",
		maxPolls:  2,
	}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("exit = %d, want exitOK", rc)
	}
	out := stdout.String()
	for _, want := range []string{"run_dispatched", "plan_generated", "approval_submitted"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q\n---\n%s", want, out)
		}
	}
	// Each category should appear exactly once even though poll 2
	// re-served them — the high-water filter must dedup.
	for _, cat := range []string{"run_dispatched", "plan_generated", "approval_submitted"} {
		if got := strings.Count(out, cat); got != 1 {
			t.Errorf("category %q appeared %d times; want 1 (dedup must filter)\n---\n%s",
				cat, got, out)
		}
	}
}

func TestAuditTail_NoDuplicates_AcrossManyPolls(t *testing.T) {
	// The server returns the same page on every poll; the loop
	// must emit each entry exactly once across many cycles.
	runID := uuid.New()
	page := httpclient.ListRunAuditResult{Items: []httpclient.AuditEntry{
		tailEntry(1, runID, "run_dispatched", nil),
		tailEntry(2, runID, "plan_generated", nil),
	}}
	api := &stubAuditTailAPI{
		responses: []httpclient.ListRunAuditResult{page, page, page, page},
	}

	var stdout strings.Builder
	rc := runAuditTail(context.Background(), api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "text",
		maxPolls:  4,
	}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("exit = %d", rc)
	}
	for _, cat := range []string{"run_dispatched", "plan_generated"} {
		if got := strings.Count(stdout.String(), cat); got != 1 {
			t.Errorf("%q appeared %d times; want 1\n%s", cat, got, stdout.String())
		}
	}
}

func TestAuditTail_TransientErrorWarnedAndContinued(t *testing.T) {
	// One transport error on poll 1; recovers on poll 2. Exit code
	// stays OK.
	runID := uuid.New()
	api := &stubAuditTailAPI{
		responses: []httpclient.ListRunAuditResult{
			{}, // ignored on poll 1 (errored)
			{Items: []httpclient.AuditEntry{tailEntry(1, runID, "run_dispatched", nil)}},
		},
		errs: []error{errors.New("connection refused"), nil},
	}

	var stdout, stderr strings.Builder
	rc := runAuditTail(context.Background(), api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "text",
		maxPolls:  2,
	}, &stdout, &stderr)
	if rc != exitOK {
		t.Errorf("exit = %d, want exitOK after transient recovery", rc)
	}
	if !strings.Contains(stderr.String(), "poll failed (1/5)") {
		t.Errorf("stderr missing transient-warning: %s", stderr.String())
	}
	if !strings.Contains(stdout.String(), "run_dispatched") {
		t.Errorf("entry after recovery should print: %s", stdout.String())
	}
}

func TestAuditTail_PersistentErrorBailsNonZero(t *testing.T) {
	// Five consecutive failures → exit failure. Sixth poll never
	// happens.
	runID := uuid.New()
	api := &stubAuditTailAPI{
		errs: []error{
			errors.New("e1"),
			errors.New("e2"),
			errors.New("e3"),
			errors.New("e4"),
			errors.New("e5"),
		},
	}

	var stderr strings.Builder
	rc := runAuditTail(context.Background(), api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "text",
		maxPolls:  10,
	}, io.Discard, &stderr)
	if rc != exitFailure {
		t.Errorf("exit = %d, want exitFailure", rc)
	}
	if !strings.Contains(stderr.String(), "consecutive poll failures") {
		t.Errorf("stderr missing bail message: %s", stderr.String())
	}
	if got := int(api.polled.Load()); got != 5 {
		t.Errorf("polled %d times; want 5 (bail at consecutive cap)", got)
	}
}

func TestAuditTail_ContextCancelExitsClean(t *testing.T) {
	// Caller cancels mid-loop; exit code should be 0.
	runID := uuid.New()
	api := &stubAuditTailAPI{
		responses: []httpclient.ListRunAuditResult{
			{Items: []httpclient.AuditEntry{tailEntry(1, runID, "run_dispatched", nil)}},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel immediately so the loop exits at the first ctx.Done
	// poll guard.
	cancel()

	var stdout strings.Builder
	rc := runAuditTail(ctx, api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "text",
		maxPolls:  0,
	}, &stdout, io.Discard)
	if rc != exitOK {
		t.Errorf("exit = %d, want exitOK on context cancel", rc)
	}
}

func TestAuditTail_JSONOutputIsNDJSON(t *testing.T) {
	runID := uuid.New()
	api := &stubAuditTailAPI{
		responses: []httpclient.ListRunAuditResult{
			{Items: []httpclient.AuditEntry{
				tailEntry(1, runID, "run_dispatched", map[string]any{"kind": "issue"}),
				tailEntry(2, runID, "plan_generated", map[string]any{"summary": "y"}),
			}},
		},
	}

	var stdout strings.Builder
	rc := runAuditTail(context.Background(), api, runID, auditTailOptions{
		interval:  time.Millisecond,
		outputFmt: "json",
		maxPolls:  1,
	}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("exit = %d", rc)
	}
	lines := strings.Split(strings.TrimSpace(stdout.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 NDJSON lines; got %d\n%s", len(lines), stdout.String())
	}
	for _, line := range lines {
		var e httpclient.AuditEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("not valid AuditEntry: %v\n%s", err, line)
		}
	}
}

// --- flag-parsing guards (these go through run([]string{...}) since
// the dispatch + parsing path is what they exercise) ---

func TestAuditTail_IntervalBelowMinimum_Refused(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	rc := run([]string{
		"audit", "tail", "--interval", "100ms", uuid.New().String(),
	}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("exit = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "500ms minimum") {
		t.Errorf("stderr missing minimum-interval diagnostic: %s", stderr.String())
	}
}

func TestAuditTail_BadUUID(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	rc := run([]string{"audit", "tail", "not-a-uuid"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("exit = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
}

func TestAuditTail_MissingArg(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	rc := run([]string{"audit", "tail"}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("exit = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "<run-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestAuditTail_BadOutputValue(t *testing.T) {
	_, srv := newFakeBackend(t)
	withBackend(t, srv)
	var stderr strings.Builder
	rc := run([]string{
		"audit", "tail", "--output", "xml", uuid.New().String(),
	}, io.Discard, &stderr)
	if rc != exitUsage {
		t.Errorf("exit = %d, want exitUsage", rc)
	}
	if !strings.Contains(stderr.String(), "invalid --output") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// TestAuditTail_EndToEnd_WithFakeBackend covers the full integration
// path: signal-cancelled context, fake httptest backend, bounded by
// --max-polls. The polling loop, network round-trip, dedup, and
// rendering are all live; the only fake is the backend itself.
func TestAuditTail_EndToEnd_WithFakeBackend(t *testing.T) {
	fb, srv := newFakeBackend(t)
	withBackend(t, srv)
	runID := uuid.New()
	fb.auditResp = httpclient.ListRunAuditResult{
		Items: []httpclient.AuditEntry{
			tailEntry(1, runID, "run_dispatched", nil),
			tailEntry(2, runID, "plan_generated", map[string]any{"summary": "y"}),
		},
	}

	var stdout strings.Builder
	rc := run([]string{
		"audit", "tail",
		"--interval", "500ms",
		"--max-polls", "1",
		runID.String(),
	}, &stdout, io.Discard)
	if rc != exitOK {
		t.Fatalf("exit = %d", rc)
	}
	for _, want := range []string{"run_dispatched", "plan_generated"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout missing %q\n---\n%s", want, stdout.String())
		}
	}
}
