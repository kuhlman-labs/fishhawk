package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

const (
	pmPrior  = "aaaa111111111111111111111111111111111111"
	pmMerge  = "bbbb222222222222222222222222222222222222"
	pmRacer  = "dddd444444444444444444444444444444444444"
	pmErrTag = "boom-read"
)

// pmRead is one scripted post-merge read: a head, or an error when err is set.
type pmRead struct {
	head string
	err  bool
}

// scriptedPostMergeReader returns reads[min(i, len-1)] on the i-th call and
// counts calls.
func scriptedPostMergeReader(reads []pmRead, calls *int) func(context.Context) (string, error) {
	return func(context.Context) (string, error) {
		i := *calls
		*calls++
		if i >= len(reads) {
			i = len(reads) - 1
		}
		if reads[i].err {
			return "", errors.New(pmErrTag)
		}
		return reads[i].head, nil
	}
}

// TestReadPostMergeHead_Classifies is one row per outcome of the #4199
// classifier, asserting Outcome, Observed and Attempts.
func TestReadPostMergeHead_Classifies(t *testing.T) {
	d := timescale.D(time.Millisecond)
	backoff := []time.Duration{d, d, d, d, d}
	full := 1 + len(backoff)

	cases := []struct {
		name         string
		mergeSHA     string
		reads        []pmRead
		wantOutcome  string
		wantObserved string
		wantAttempts int
		wantLastErr  bool
	}{
		{"converged on the first read", pmMerge, []pmRead{{head: pmMerge}},
			postMergeHeadReadConverged, pmMerge, 1, false},
		{"lag then converge", pmMerge, []pmRead{{head: pmPrior}, {head: pmPrior}, {head: pmMerge}},
			postMergeHeadReadConverged, pmMerge, 3, false},
		{"persistent lag", pmMerge, []pmRead{{head: pmPrior}},
			postMergeHeadReadReadAfterWriteLag, pmPrior, full, false},
		{"third sha is a concurrent push, no further reads", pmMerge, []pmRead{{head: pmPrior}, {head: pmRacer}, {head: pmMerge}},
			postMergeHeadReadConcurrentPush, pmRacer, 2, false},
		{"all errors are unreadable", pmMerge, []pmRead{{err: true}},
			postMergeHeadReadUnreadable, "", full, true},
		{"empty heads are unreadable", pmMerge, []pmRead{{head: ""}},
			postMergeHeadReadUnreadable, "", full, true},
		{"undecodable plus a third sha is read_back", "", []pmRead{{head: pmPrior}, {head: pmMerge}},
			postMergeHeadReadReadBack, pmMerge, 2, false},
		{"undecodable plus persistent prior is lag", "", []pmRead{{head: pmPrior}},
			postMergeHeadReadReadAfterWriteLag, pmPrior, full, false},
		{"mixed error and prior reads are lag", pmMerge, []pmRead{{err: true}, {head: pmPrior}, {err: true}},
			postMergeHeadReadReadAfterWriteLag, pmPrior, full, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			got := readPostMergeHead(context.Background(), scriptedPostMergeReader(tc.reads, &calls),
				pmPrior, tc.mergeSHA, backoff)
			if got.Outcome != tc.wantOutcome {
				t.Errorf("Outcome = %q, want %q", got.Outcome, tc.wantOutcome)
			}
			if got.Observed != tc.wantObserved {
				t.Errorf("Observed = %q, want %q", got.Observed, tc.wantObserved)
			}
			if got.Attempts != tc.wantAttempts || calls != tc.wantAttempts {
				t.Errorf("Attempts/calls = %d/%d, want %d", got.Attempts, calls, tc.wantAttempts)
			}
			if (got.LastErr != nil) != tc.wantLastErr {
				t.Errorf("LastErr = %v, want set=%v", got.LastErr, tc.wantLastErr)
			}
		})
	}
}

// TestReadPostMergeHead_StopsOnContextCancel: a cancelled context ends the
// backoff wait at once, so no further read is made and the helper returns
// well inside one backoff step.
func TestReadPostMergeHead_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	step := timescale.D(10 * time.Second)
	calls := 0
	read := func(context.Context) (string, error) {
		calls++
		cancel()
		return pmPrior, nil
	}
	start := time.Now()
	got := readPostMergeHead(ctx, read, pmPrior, pmMerge, []time.Duration{step, step})
	elapsed := time.Since(start)
	if got.Attempts != 1 || calls != 1 {
		t.Errorf("Attempts/calls = %d/%d, want 1 — a cancelled context must stop re-reading", got.Attempts, calls)
	}
	if bound := timescale.D(2 * time.Second); elapsed >= bound {
		t.Errorf("elapsed = %v, want < %v — the backoff wait must honour ctx.Done()", elapsed, bound)
	}
	if got.Outcome != postMergeHeadReadReadAfterWriteLag {
		t.Errorf("Outcome = %q, want %q (the one read saw the pre-merge head)", got.Outcome, postMergeHeadReadReadAfterWriteLag)
	}
}

// TestResolvePostMergeHead pins the provenance rule: a decoded merge sha is the
// head except on a genuine concurrent push; without one, only read_back
// supplies a head.
func TestResolvePostMergeHead(t *testing.T) {
	cases := []struct {
		outcome, observed, mergeSHA, want string
	}{
		{postMergeHeadReadConverged, pmMerge, pmMerge, pmMerge},
		{postMergeHeadReadReadAfterWriteLag, pmPrior, pmMerge, pmMerge},
		{postMergeHeadReadUnreadable, "", pmMerge, pmMerge},
		{postMergeHeadReadConcurrentPush, pmRacer, pmMerge, pmRacer},
		{postMergeHeadReadReadBack, pmRacer, "", pmRacer},
		{postMergeHeadReadReadAfterWriteLag, pmPrior, "", ""},
		{postMergeHeadReadUnreadable, "", "", ""},
	}
	for _, tc := range cases {
		got := resolvePostMergeHead(postMergeHeadRead{Outcome: tc.outcome, Observed: tc.observed}, tc.mergeSHA)
		if got != tc.want {
			t.Errorf("resolvePostMergeHead(%s, observed=%q, merge=%q) = %q, want %q",
				tc.outcome, tc.observed, tc.mergeSHA, got, tc.want)
		}
	}
}

// TestPostMergeHeadReadNote pins the degraded-outcome notes: each names the
// classification and what the head was anchored on; the clean outcomes carry
// none.
func TestPostMergeHeadReadNote(t *testing.T) {
	readErr := errors.New(pmErrTag)
	cases := []struct {
		name     string
		pm       postMergeHeadRead
		mergeSHA string
		want     []string
	}{
		{"lag with merge sha", postMergeHeadRead{Outcome: postMergeHeadReadReadAfterWriteLag, Observed: pmPrior, Attempts: 6}, pmMerge,
			[]string{"read_after_write_lag", pmPrior, pmMerge, "NOT a concurrent push", "no fishhawk_vouch_commit is needed", "6"}},
		{"lag without merge sha", postMergeHeadRead{Outcome: postMergeHeadReadReadAfterWriteLag, Observed: pmPrior, Attempts: 6}, "",
			[]string{"read_after_write_lag", pmPrior, "unresolved"}},
		{"unreadable with merge sha", postMergeHeadRead{Outcome: postMergeHeadReadUnreadable, Attempts: 6, LastErr: readErr}, pmMerge,
			[]string{"unreadable", pmErrTag, "divergence could not be checked", pmMerge}},
		{"unreadable without merge sha", postMergeHeadRead{Outcome: postMergeHeadReadUnreadable, Attempts: 6}, "",
			[]string{"unreadable", errPostMergeEmptyHead.Error(), "unresolved"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note := postMergeHeadReadNote(tc.pm, pmPrior, tc.mergeSHA)
			for _, w := range tc.want {
				if !strings.Contains(note, w) {
					t.Errorf("note must contain %q: %q", w, note)
				}
			}
		})
	}
	for _, outcome := range []string{postMergeHeadReadConverged, postMergeHeadReadConcurrentPush, postMergeHeadReadReadBack} {
		if note := postMergeHeadReadNote(postMergeHeadRead{Outcome: outcome}, pmPrior, pmMerge); note != "" {
			t.Errorf("%s note = %q, want empty", outcome, note)
		}
	}
}

// TestPostMergeHeadUnresolvedReason: the republish warning's reason names the
// lag, the last read error, or the empty-head fallback.
func TestPostMergeHeadUnresolvedReason(t *testing.T) {
	if got := postMergeHeadUnresolvedReason(postMergeHeadRead{Outcome: postMergeHeadReadReadAfterWriteLag, Attempts: 6}, pmPrior); !strings.Contains(got, "read-after-write lag") || !strings.Contains(got, pmPrior) {
		t.Errorf("lag reason = %q", got)
	}
	if got := postMergeHeadUnresolvedReason(postMergeHeadRead{Outcome: postMergeHeadReadUnreadable, LastErr: errors.New(pmErrTag)}, pmPrior); got != pmErrTag {
		t.Errorf("unreadable reason = %q, want the last read error", got)
	}
	if got := postMergeHeadUnresolvedReason(postMergeHeadRead{Outcome: postMergeHeadReadUnreadable}, pmPrior); got != errPostMergeEmptyHead.Error() {
		t.Errorf("fallback reason = %q", got)
	}
}

// TestPostMergeHeadReadSchedule: the production schedule is selected when no
// test seam is set — an initial read plus five re-reads, ~10s cumulative.
func TestPostMergeHeadReadSchedule(t *testing.T) {
	s := &Server{}
	got := s.postMergeHeadReadSchedule()
	if len(got) != 5 {
		t.Fatalf("production schedule = %v, want 5 re-read steps", got)
	}
	var total time.Duration
	for _, d := range got {
		total += d
	}
	if total != 10*time.Second {
		t.Errorf("production schedule total = %v, want 10s", total)
	}
	seam := []time.Duration{time.Millisecond}
	s.postMergeHeadReadBackoff = seam
	if got := s.postMergeHeadReadSchedule(); len(got) != 1 || got[0] != time.Millisecond {
		t.Errorf("seam schedule = %v, want %v", got, seam)
	}
}
