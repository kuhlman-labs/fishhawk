package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// fakeMintClient is a mutex-guarded mcpTokenClient. Its OWN lock guards every
// counter so a -race verdict in the concurrency test is attributable to the
// source under test, never to the fake (#2586).
type fakeMintClient struct {
	mu        sync.Mutex
	calls     []string // ordered: "issue_key" / "fetch_mcp_token"
	issueErr  error
	fetchErr  error
	mints     int
	keyExpiry time.Time
	// tokenTTL is added to now() for each minted token's ExpiresAt. Zero makes
	// every minted token immediately due for refresh again.
	tokenTTL time.Duration
	now      func() time.Time
	// ctxErrs records ctx.Err() as observed inside each client call.
	ctxErrs []error
}

func (f *fakeMintClient) IssueKey(ctx context.Context, _ string, _ time.Duration) (*upload.IssuedKey, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "issue_key")
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.issueErr != nil {
		return nil, f.issueErr
	}
	return &upload.IssuedKey{PrivateKey: make([]byte, 64), ExpiresAt: f.keyExpiry}, nil
}

func (f *fakeMintClient) FetchMCPToken(ctx context.Context, _ upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, "fetch_mcp_token")
	f.ctxErrs = append(f.ctxErrs, ctx.Err())
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	f.mints++
	return &upload.FetchMCPTokenResult{
		Token:     fmt.Sprintf("fhm_refreshed_%d", f.mints),
		TokenID:   fmt.Sprintf("tid-%d", f.mints),
		ExpiresAt: f.now().Add(f.tokenTTL),
	}, nil
}

func (f *fakeMintClient) callLog() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func (f *fakeMintClient) count(kind string) int {
	n := 0
	for _, c := range f.callLog() {
		if c == kind {
			n++
		}
	}
	return n
}

var mintEpoch = time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)

// newTestTokenSource seeds a source at mintEpoch whose token expires at
// tokenExpiry and whose key expires at keyExpiry. Minted keys are fresh for an
// hour; minted tokens use tokenTTL.
func newTestTokenSource(token string, tokenExpiry, keyExpiry time.Time, tokenTTL time.Duration) (*mcpTokenSource, *fakeMintClient, *bytes.Buffer) {
	now := func() time.Time { return mintEpoch }
	fc := &fakeMintClient{keyExpiry: mintEpoch.Add(time.Hour), tokenTTL: tokenTTL, now: now}
	var log bytes.Buffer
	s := newMCPTokenSource(fc, "run-1", token, tokenExpiry,
		&upload.IssuedKey{PrivateKey: make([]byte, 64), ExpiresAt: keyExpiry}, &log)
	s.now = now
	return s, fc, &log
}

// newExpiredTokenSource is the consumer-seam fixture: the stored token is
// EXPIRED by construction and every mint returns a new, distinct bearer that is
// itself immediately due again — so successive bearer() calls yield
// fhm_refreshed_1, fhm_refreshed_2, … and a per-call read is observable.
func newExpiredTokenSource() (*mcpTokenSource, *fakeMintClient) {
	s, fc, _ := newTestTokenSource("fhm_expired", mintEpoch.Add(-time.Minute), mintEpoch.Add(time.Hour), 0)
	return s, fc
}

// (1) nil source / empty token → "" and zero client calls (ADR-050 posture).
func TestMCPTokenSource_NoTokenNeverMints(t *testing.T) {
	var nilSrc *mcpTokenSource
	if got := nilSrc.bearer(context.Background()); got != "" {
		t.Fatalf("nil source bearer = %q, want empty", got)
	}
	s, fc, log := newTestTokenSource("", mintEpoch.Add(-time.Hour), mintEpoch.Add(-time.Hour), time.Hour)
	if got := s.bearer(context.Background()); got != "" {
		t.Fatalf("empty-token bearer = %q, want empty", got)
	}
	if calls := fc.callLog(); len(calls) != 0 {
		t.Fatalf("empty-token source made client calls %v, want none", calls)
	}
	if log.Len() != 0 {
		t.Fatalf("empty-token source logged %q, want nothing", log.String())
	}
	// freshMCPToken on a config with no source returns the fallback verbatim.
	if got := (config{}).freshMCPToken(context.Background(), "fhm_orig"); got != "fhm_orig" {
		t.Fatalf("nil-source freshMCPToken = %q, want fhm_orig", got)
	}
}

// (2) token outside the skew window → unchanged, zero calls.
func TestMCPTokenSource_FreshTokenNoNetwork(t *testing.T) {
	s, fc, _ := newTestTokenSource("fhm_fresh", mintEpoch.Add(mcpTokenRefreshSkew+time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	for i := 0; i < 3; i++ {
		if got := s.bearer(context.Background()); got != "fhm_fresh" {
			t.Fatalf("bearer = %q, want fhm_fresh", got)
		}
	}
	if calls := fc.callLog(); len(calls) != 0 {
		t.Fatalf("fresh token made client calls %v, want none", calls)
	}
}

// (3) inside the skew window with a fresh key → exactly one FetchMCPToken,
// zero IssueKey, a DIFFERENT token, mcp_token_refreshed logged.
func TestMCPTokenSource_RefreshWithFreshKey(t *testing.T) {
	s, fc, log := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	got := s.bearer(context.Background())
	if got != "fhm_refreshed_1" {
		t.Fatalf("bearer = %q, want fhm_refreshed_1", got)
	}
	if n := fc.count("fetch_mcp_token"); n != 1 {
		t.Fatalf("FetchMCPToken calls = %d, want 1", n)
	}
	if n := fc.count("issue_key"); n != 0 {
		t.Fatalf("IssueKey calls = %d, want 0", n)
	}
	if !strings.Contains(log.String(), `"event":"mcp_token_refreshed"`) {
		t.Fatalf("log missing mcp_token_refreshed: %q", log.String())
	}
	// The refreshed token is now fresh: a second call costs nothing.
	if again := s.bearer(context.Background()); again != got {
		t.Fatalf("second bearer = %q, want %q", again, got)
	}
	if n := fc.count("fetch_mcp_token"); n != 1 {
		t.Fatalf("FetchMCPToken calls after second bearer = %d, want 1", n)
	}
}

// (4) key ALSO inside its window → IssueKey observed BEFORE FetchMCPToken.
func TestMCPTokenSource_StaleKeyReissuedFirst(t *testing.T) {
	s, fc, _ := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Minute), time.Hour)
	if got := s.bearer(context.Background()); got != "fhm_refreshed_1" {
		t.Fatalf("bearer = %q, want fhm_refreshed_1", got)
	}
	want := []string{"issue_key", "fetch_mcp_token"}
	if calls := fc.callLog(); strings.Join(calls, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", calls, want)
	}
}

// (5) IssueKey error → stale token, degraded logged, FetchMCPToken never called.
func TestMCPTokenSource_IssueKeyErrorDegrades(t *testing.T) {
	s, fc, log := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Minute), time.Hour)
	fc.issueErr = errors.New("backend down")
	if got := s.bearer(context.Background()); got != "fhm_old" {
		t.Fatalf("bearer = %q, want stale fhm_old", got)
	}
	if n := fc.count("fetch_mcp_token"); n != 0 {
		t.Fatalf("FetchMCPToken calls = %d, want 0", n)
	}
	if !strings.Contains(log.String(), `"event":"mcp_token_refresh_degraded"`) || !strings.Contains(log.String(), `"step":"issue_key"`) {
		t.Fatalf("log missing issue_key degrade: %q", log.String())
	}
}

// (6) FetchMCPToken error → stale token, degraded logged.
func TestMCPTokenSource_FetchErrorDegrades(t *testing.T) {
	s, fc, log := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	fc.fetchErr = errors.New("401 authentication_required")
	if got := s.bearer(context.Background()); got != "fhm_old" {
		t.Fatalf("bearer = %q, want stale fhm_old", got)
	}
	if !strings.Contains(log.String(), `"event":"mcp_token_refresh_degraded"`) || !strings.Contains(log.String(), `"step":"fetch_mcp_token"`) {
		t.Fatalf("log missing fetch degrade: %q", log.String())
	}
	if !s.nextAttempt.Equal(mintEpoch.Add(mcpTokenRefreshRetryInterval)) {
		t.Fatalf("nextAttempt = %v, want now+retry interval", s.nextAttempt)
	}
}

// (7) a second bearer() inside the retry interval after a failure → zero
// additional calls; once the interval passes, it retries.
func TestMCPTokenSource_BackoffAfterFailure(t *testing.T) {
	s, fc, _ := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	fc.fetchErr = errors.New("boom")
	_ = s.bearer(context.Background())
	before := len(fc.callLog())
	s.now = func() time.Time { return mintEpoch.Add(mcpTokenRefreshRetryInterval - time.Second) }
	if got := s.bearer(context.Background()); got != "fhm_old" {
		t.Fatalf("bearer in backoff = %q, want fhm_old", got)
	}
	if after := len(fc.callLog()); after != before {
		t.Fatalf("client calls inside backoff: %d → %d, want none", before, after)
	}
	fc.mu.Lock()
	fc.fetchErr = nil
	fc.mu.Unlock()
	s.now = func() time.Time { return mintEpoch.Add(mcpTokenRefreshRetryInterval) }
	if got := s.bearer(context.Background()); got != "fhm_refreshed_1" {
		t.Fatalf("bearer after backoff = %q, want fhm_refreshed_1", got)
	}
	if !s.nextAttempt.IsZero() {
		t.Fatalf("nextAttempt not cleared after success: %v", s.nextAttempt)
	}
}

// (8) 16 concurrent callers on a due token → exactly one mint, race-clean.
func TestMCPTokenSource_ConcurrentCallersMintOnce(t *testing.T) {
	s, fc, _ := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	var wg sync.WaitGroup
	start := make(chan struct{})
	results := make([]string, 16)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = s.bearer(context.Background())
		}(i)
	}
	close(start)
	wg.Wait()
	if n := fc.count("fetch_mcp_token"); n != 1 {
		t.Fatalf("FetchMCPToken calls = %d, want exactly 1", n)
	}
	for i, r := range results {
		if r != "fhm_refreshed_1" {
			t.Fatalf("caller %d got %q, want fhm_refreshed_1", i, r)
		}
	}
}

// Approval condition 2: a pre-cancelled CALLER context must not fail the
// refresh — the mint runs under a detached, bounded context — and no backoff
// is stamped.
func TestMCPTokenSource_CancelledCallerCtxStillRefreshes(t *testing.T) {
	s, fc, log := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Minute), time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := s.bearer(ctx); got != "fhm_refreshed_1" {
		t.Fatalf("bearer with cancelled caller ctx = %q, want fhm_refreshed_1 (log %q)", got, log.String())
	}
	if !s.nextAttempt.IsZero() {
		t.Fatalf("backoff stamped on a caller cancellation: nextAttempt = %v", s.nextAttempt)
	}
	fc.mu.Lock()
	defer fc.mu.Unlock()
	for i, err := range fc.ctxErrs {
		if err != nil {
			t.Fatalf("client call %d (%s) observed ctx.Err() = %v; the mint context must be detached", i, fc.calls[i], err)
		}
	}
}

// The mint context carries the named bound.
func TestMCPTokenSource_MintContextIsBounded(t *testing.T) {
	if mcpTokenRefreshTimeout != 15*time.Second {
		t.Fatalf("mcpTokenRefreshTimeout = %v, want 15s (approval condition 1)", mcpTokenRefreshTimeout)
	}
	s, _, _ := newTestTokenSource("fhm_old", mintEpoch.Add(time.Minute), mintEpoch.Add(time.Hour), time.Hour)
	dc := &deadlineCapturingClient{}
	s.client = dc
	_ = s.bearer(context.Background())
	if !dc.hadDeadline {
		t.Fatal("mint context carried no deadline; want mcpTokenRefreshTimeout bound")
	}
	if dc.remaining > mcpTokenRefreshTimeout {
		t.Fatalf("mint deadline %v exceeds mcpTokenRefreshTimeout", dc.remaining)
	}
}

type deadlineCapturingClient struct {
	hadDeadline bool
	remaining   time.Duration
}

func (d *deadlineCapturingClient) IssueKey(context.Context, string, time.Duration) (*upload.IssuedKey, error) {
	return nil, errors.New("unused")
}

func (d *deadlineCapturingClient) FetchMCPToken(ctx context.Context, _ upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error) {
	dl, ok := ctx.Deadline()
	d.hadDeadline = ok
	d.remaining = time.Until(dl)
	return &upload.FetchMCPTokenResult{Token: "fhm_new", ExpiresAt: mintEpoch.Add(time.Hour)}, nil
}
